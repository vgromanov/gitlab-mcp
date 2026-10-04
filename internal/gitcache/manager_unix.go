//go:build linux || darwin

package gitcache

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Root is one private cache directory. It is unreachable from production startup.
type Root struct {
	path         string
	helper       string
	quota        uint64
	slots        []Slot
	owned        map[int]bool
	ownedPins    map[int]uint32
	meta         map[int]map[string]uint64
	gitRole      Role
	access       uint64
	fs           *fsClient
	rootFile     *os.File
	lockFile     *os.File
	git          *exec.Cmd
	gitGen       string
	gitDone      chan error
	readers      map[int]*slotReader
	readerFailed map[int]bool
	pinHold      bool
	gitHold      bool
	childSlot    int
	draining     bool
	closed       bool
	mu           sync.Mutex
	domains      [64]sync.Mutex
}

// roleWait bounds a normal Git role. Cancellation uses QuiesceGroup instead.
const roleWait = 20 * time.Second

// skipLaunchIdentity is a test seam. Production leaves it false.
var skipLaunchIdentity bool

// slotReader is the process holding one owned pin. Unpin waits for it.
type slotReader struct {
	cmd  *exec.Cmd
	done chan error
}

// Open validates a private directory, takes the root lock, and loads the ledger.
// Non-committed slots stay frozen and keep a full reservation.
func Open(path, helper string, quota uint64) (*Root, error) {
	if helper == "" || helper[0] != '/' {
		return nil, ErrPath
	}
	if err := QuotaOK(quota); err != nil {
		return nil, err
	}
	rf, err := walkRoot(path)
	if err != nil {
		return nil, err
	}
	var rootSt syscall.Stat_t
	if err := syscall.Fstat(int(rf.Fd()), &rootSt); err != nil {
		rf.Close()
		return nil, ErrPath
	}
	lfd, err := openRootLock(int(rf.Fd()), uint64(rootSt.Dev))
	if err != nil {
		rf.Close()
		return nil, err
	}
	lock := os.NewFile(uintptr(lfd), "root.lock")
	fs, err := startFS(helper, rf)
	if err != nil {
		lock.Close()
		rf.Close()
		return nil, err
	}
	r := &Root{
		path: path, helper: helper, quota: quota,
		owned: map[int]bool{}, ownedPins: map[int]uint32{}, readers: map[int]*slotReader{}, readerFailed: map[int]bool{}, meta: map[int]map[string]uint64{},
		childSlot: -1,
		fs:        fs, rootFile: rf, lockFile: lock,
	}
	if err := r.load(); err != nil {
		_ = fs.stop()
		lock.Close()
		rf.Close()
		return nil, err
	}
	return r, nil
}

func openRootLock(rootfd int, rootDev uint64) (int, error) {
	lfd, err := openatNoFollow(rootfd, "root.lock", syscall.O_RDWR|syscall.O_NONBLOCK, 0)
	created := false
	if errors.Is(err, syscall.ENOENT) {
		lfd, err = openatNoFollow(rootfd, "root.lock", syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NONBLOCK, 0600)
		created = true
	}
	if err != nil {
		return -1, ErrPath
	}
	if err := lockAttributes(lfd, rootDev, created); err != nil {
		syscall.Close(lfd)
		return -1, err
	}
	if err := syscall.Flock(lfd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		syscall.Close(lfd)
		return -1, ErrBusy
	}
	return lfd, nil
}

func lockAttributes(fd int, rootDev uint64, created bool) error {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return ErrPath
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Nlink != 1 || observedUID(&st) != uint32(os.Geteuid()) || uint64(st.Dev) != rootDev || st.Size != 0 {
		return ErrCorrupt
	}
	if st.Mode&0777 == 0600 {
		return nil
	}
	if !created {
		return ErrCorrupt
	}
	if err := syscall.Fchmod(fd, 0600); err != nil {
		return ErrPath
	}
	if err := syscall.Fstat(fd, &st); err != nil || st.Mode&0777 != 0600 {
		return ErrCorrupt
	}
	return nil
}

func (r *Root) load() error {
	names, err := r.names(".")
	if err != nil {
		return err
	}
	haveLedger := false
	var gens []string
	lockOK := false
	for _, n := range names {
		if !allowedRootName(n) {
			return ErrCorrupt
		}
		st, err := r.lstat(n)
		if err != nil {
			return err
		}
		kind := st[0] & uint64(syscall.S_IFMT)
		mode := st[0] & 0777
		if kind == uint64(syscall.S_IFLNK) {
			return ErrPath
		}
		switch n {
		case "root.lock":
			if kind != uint64(syscall.S_IFREG) || st[1] != 1 || st[2] != 0 || mode != 0600 {
				return ErrCorrupt
			}
			lockOK = true
		case "ledger":
			if kind != uint64(syscall.S_IFREG) || st[1] != 1 || st[2] != LedgerLen || mode != 0600 {
				return ErrCorrupt
			}
			haveLedger = true
		case "ledger.tmp":
			if kind != uint64(syscall.S_IFREG) || st[1] != 1 || st[2] > LedgerLen || mode != 0600 {
				return ErrCorrupt
			}
		default:
			if !hex32(n) || kind != uint64(syscall.S_IFDIR) || mode != 0700 {
				return ErrCorrupt
			}
			gens = append(gens, n)
		}
	}
	if !lockOK {
		return ErrCorrupt
	}
	if !haveLedger {
		if len(gens) != 0 {
			return ErrCorrupt
		}
		for _, n := range names {
			if n != "root.lock" {
				return ErrCorrupt
			}
		}
		r.slots = make([]Slot, MaxSlots)
		return r.persist()
	}
	buf, err := r.readFile("ledger")
	if err != nil {
		return err
	}
	hdr, slots, err := parseLedger(buf)
	if err != nil {
		return err
	}
	if hdr.Quota != r.quota {
		return ErrQuota
	}
	if err := reconcileSlots(r, slots, gens); err != nil {
		return err
	}
	r.slots = slots
	return nil
}

func reconcileSlots(r *Root, slots []Slot, gens []string) error {
	seen := map[string]bool{}
	claim := func(name string) error {
		if name == "" || seen[name] {
			return ErrCorrupt
		}
		seen[name] = true
		return nil
	}
	for i := range slots {
		s := slots[i]
		if s.State == stateEmpty {
			continue
		}
		if err := claim(s.ID); err != nil {
			return err
		}
		if s.DestID != "" {
			if s.DestID == s.ID {
				return ErrCorrupt
			}
			if err := claim(s.DestID); err != nil {
				return err
			}
		}
		if s.State == stateCommitted {
			if s.DestID == "" {
				return ErrCorrupt
			}
			// The source directory must be gone. Do not delete it from here.
			_, err := r.lstat(s.ID)
			if err == nil {
				return ErrCorrupt
			}
			if !errors.Is(err, ErrAbsent) {
				return err
			}
			if err := r.matchCommitted(s); err != nil {
				return err
			}
			continue
		}
		// A non-committed record keeps full R. Source or intended dest may
		// exist, never both. The directory is not deleted or reclaimed.
		srcOK, err := dirHere(r, s.ID)
		if err != nil {
			return err
		}
		destOK := false
		if s.DestID != "" {
			destOK, err = dirHere(r, s.DestID)
			if err != nil {
				return err
			}
		}
		if srcOK && destOK {
			return ErrCorrupt
		}
		if !srcOK && !destOK {
			if s.State == stateReserved {
				continue
			}
			return ErrCorrupt
		}
		dir := s.ID
		if destOK {
			dir = s.DestID
		}
		if err := r.boundStage(dir); err != nil {
			return err
		}
	}
	for _, g := range gens {
		if !seen[g] {
			return ErrCorrupt
		}
	}
	return nil
}

func dirHere(r *Root, name string) (bool, error) {
	st, err := r.lstat(name)
	if errors.Is(err, ErrAbsent) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if st[0]&uint64(syscall.S_IFMT) != uint64(syscall.S_IFDIR) || st[0]&0777 != 0700 {
		return false, ErrCorrupt
	}
	return true, nil
}

func (r *Root) boundStage(id string) error {
	if err := r.auditGen(id); err != nil {
		return err
	}
	files := []struct {
		rel string
		cap uint64
		idx bool
	}{
		{id + "/objects/pack/input.pack", PackMax, false},
		{id + "/objects/pack/input.idx", IndexMax, true},
		{id + "/config", MetaConfig, false},
		{id + "/config.tmp", MetaConfig, false},
		{id + "/HEAD", MetaHEAD, false},
		{id + "/HEAD.tmp", MetaHEAD, false},
		{id + "/provenance", MetaProvenance, false},
		{id + "/provenance.tmp", MetaProvenance, false},
		{id + "/manifest", MetaManifest, false},
		{id + "/manifest.tmp", MetaManifest, false},
		{id + "/refs/heads/acquired", MetaRef, false},
		{id + "/refs/heads/acquired.tmp", MetaRef, false},
	}
	for _, f := range files {
		st, err := r.openStat(f.rel)
		if errors.Is(err, ErrAbsent) {
			continue
		}
		if err != nil {
			return err
		}
		if st[0]&uint64(syscall.S_IFMT) != uint64(syscall.S_IFREG) || st[1] != 1 || st[2] > f.cap {
			return ErrCorrupt
		}
		mode := st[0] & 0777
		if f.idx {
			if mode != 0600 && mode != 0444 {
				return ErrCorrupt
			}
			continue
		}
		if mode != 0600 {
			return ErrCorrupt
		}
	}
	return nil
}

func (r *Root) matchCommitted(s Slot) error {
	return r.matchTree(s.DestID, s)
}

func (r *Root) matchTree(dir string, s Slot) error {
	if err := r.auditGen(dir); err != nil {
		return err
	}
	metas := []struct {
		name string
		cap  uint64
	}{
		{"config", MetaConfig},
		{"HEAD", MetaHEAD},
		{"provenance", MetaProvenance},
		{"manifest", MetaManifest},
		{"refs/heads/acquired", MetaRef},
	}
	for _, m := range metas {
		if err := r.metaRequired(dir+"/"+m.name, m.cap); err != nil {
			return err
		}
		if err := r.metaLen(dir+"/"+m.name+".tmp", m.cap); err != nil {
			return err
		}
	}
	pack, err := r.fileLen(dir+"/objects/pack/input.pack", s.Pack, 0600)
	if err != nil || pack != s.Pack {
		return ErrCorrupt
	}
	st, err := r.openStat(dir + "/objects/pack/input.idx")
	if err != nil {
		return ErrCorrupt
	}
	mode := st[0] & 0777
	if mode != 0600 && mode != 0444 {
		return ErrCorrupt
	}
	if st[1] != 1 || st[2] != s.Index {
		return ErrCorrupt
	}
	return nil
}

func (r *Root) metaRequired(rel string, cap uint64) error {
	st, err := r.openStat(rel)
	if err != nil {
		return ErrCorrupt
	}
	if st[0]&uint64(syscall.S_IFMT) != uint64(syscall.S_IFREG) || st[1] != 1 || st[0]&0777 != 0600 || st[2] == 0 || st[2] > cap {
		return ErrCorrupt
	}
	return nil
}

func (r *Root) metaLen(rel string, cap uint64) error {
	st, err := r.openStat(rel)
	if errors.Is(err, ErrAbsent) {
		return nil
	}
	if err != nil {
		return err
	}
	if st[0]&uint64(syscall.S_IFMT) != uint64(syscall.S_IFREG) || st[1] != 1 || st[0]&0777 != 0600 || st[2] > cap {
		return ErrCorrupt
	}
	return nil
}

func (r *Root) fileLen(rel string, want, mode uint64) (uint64, error) {
	st, err := r.openStat(rel)
	if errors.Is(err, ErrAbsent) {
		return 0, ErrCorrupt
	}
	if err != nil {
		return 0, err
	}
	if st[0]&uint64(syscall.S_IFMT) != uint64(syscall.S_IFREG) || st[1] != 1 || (st[0]&0777) != mode || st[2] != want {
		return 0, ErrCorrupt
	}
	return st[2], nil
}

func allowedRootName(name string) bool {
	switch name {
	case "ledger", "ledger.tmp", "root.lock":
		return true
	default:
		return hex32(name)
	}
}

func (r *Root) Used() (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return accounted(r.quota, r.slots)
}

func (r *Root) Reserve(domain, tip string) (string, error) {
	if domain == "" || len(domain) > 32 || len(tip) > 40 || strings.IndexByte(domain, 0) >= 0 {
		return "", ErrPath
	}
	d := stripe(domain)
	r.domains[d].Lock()
	defer r.domains[d].Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.draining {
		return "", ErrClosed
	}
	idx := -1
	for i := range r.slots {
		if r.slots[i].State == stateEmpty {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", ErrBusy
	}
	used, err := accounted(r.quota, r.slots)
	if err != nil {
		return "", err
	}
	sum, err := Add(used, Reserve)
	if err != nil || sum > r.quota {
		return "", ErrQuota
	}
	id, err := newID()
	if err != nil {
		return "", err
	}
	r.slots[idx] = Slot{ID: id, Domain: domain, Tip: tip, State: stateReserved}
	if err := r.persist(); err != nil {
		r.slots[idx].Frozen = true
		return "", err
	}
	r.owned[idx] = true
	return id, nil
}

func (r *Root) Activate(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.admit(); err != nil {
		return err
	}
	i, err := r.index(id)
	if err != nil {
		return err
	}
	if r.slots[i].Frozen || r.slots[i].State != stateReserved {
		return ErrState
	}
	for _, dir := range []string{
		id,
		id + "/objects",
		id + "/objects/pack",
		id + "/objects/info",
		id + "/refs",
		id + "/refs/heads",
	} {
		if _, err := r.fs.call(3, []byte(dir)); err != nil {
			return err
		}
	}
	r.slots[i].State = stateActive
	r.meta[i] = map[string]uint64{}
	return r.persist()
}

func (r *Root) WritePack(id string, data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.admit(); err != nil {
		return err
	}
	i, err := r.index(id)
	if err != nil {
		return err
	}
	if r.slots[i].Frozen || r.slots[i].State != stateActive {
		return ErrState
	}
	if err := AdmitPack(r.slots[i].Pack, uint64(len(data))); err != nil {
		return err
	}
	rel := id + "/objects/pack/input.pack"
	n, werr := r.writeAt(rel, r.slots[i].Pack, PackMax, data)
	if n > 0 {
		r.slots[i].Pack += n
	}
	if werr != nil || n != uint64(len(data)) {
		r.slots[i].Frozen = true
		_ = r.persist()
		if werr != nil {
			return werr
		}
		return ErrQuota
	}
	if err := r.persist(); err != nil {
		r.slots[i].Frozen = true
		return err
	}
	return nil
}

func (r *Root) WriteMeta(id, name string, data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.admit(); err != nil {
		return err
	}
	i, err := r.index(id)
	if err != nil {
		return err
	}
	if r.slots[i].Frozen || r.slots[i].State != stateActive {
		return ErrState
	}
	cur := uint64(0)
	if r.meta[i] != nil {
		cur = r.meta[i][name]
	}
	sum := metaSum(r.meta[i]) + uint64(len(data))
	if err := AdmitMeta(name, uint64(len(data)), sum); err != nil {
		return err
	}
	if err := AdmitMetaBody(name, data); err != nil {
		return err
	}
	tmp := id + "/" + name + ".tmp"
	final := id + "/" + name
	n, werr := r.writeAt(tmp, 0, metaCap(name), data)
	if werr != nil || n != uint64(len(data)) {
		r.slots[i].Frozen = true
		_ = r.persist()
		if werr != nil {
			return werr
		}
		return ErrQuota
	}
	if _, err := r.fs.call(4, []byte(tmp+"\x00"+final)); err != nil {
		r.slots[i].Frozen = true
		_ = r.persist()
		return err
	}
	if r.meta[i] == nil {
		r.meta[i] = map[string]uint64{}
	}
	r.meta[i][name] = uint64(len(data))
	_ = cur
	return nil
}

func metaCap(name string) uint64 {
	switch name {
	case "config":
		return MetaConfig
	case "HEAD":
		return MetaHEAD
	case "refs/heads/acquired":
		return MetaRef
	case "provenance":
		return MetaProvenance
	case "manifest":
		return MetaManifest
	default:
		return 0
	}
}

func metaSum(m map[string]uint64) uint64 {
	var s uint64
	for _, n := range m {
		s += n
	}
	return s
}

func (r *Root) WriteIndex(id string, data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.admit(); err != nil {
		return err
	}
	i, err := r.index(id)
	if err != nil {
		return err
	}
	if r.slots[i].Frozen || r.slots[i].State != stateActive {
		return ErrState
	}
	if uint64(len(data)) > IndexMax {
		return ErrQuota
	}
	rel := id + "/objects/pack/input.idx"
	n, werr := r.writeAt(rel, 0, IndexMax, data)
	if werr != nil || n != uint64(len(data)) {
		if n > 0 {
			r.slots[i].Index = n
		}
		r.slots[i].Frozen = true
		_ = r.persist()
		if werr != nil {
			return werr
		}
		return ErrQuota
	}
	r.slots[i].Index = n
	if err := r.persist(); err != nil {
		r.slots[i].Frozen = true
		return err
	}
	return nil
}

func (r *Root) Quiesce(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.admit(); err != nil {
		return err
	}
	i, err := r.index(id)
	if err != nil {
		return err
	}
	if r.slots[i].Frozen || r.slots[i].State != stateActive {
		return ErrState
	}
	had := r.git != nil
	role := r.gitRole
	gen := r.gitGen
	if err := r.awaitGit(id); err != nil {
		r.slots[i].Frozen = true
		_ = r.persist()
		return err
	}
	if had && role == RoleIndex && gen == id {
		if err := r.measureIndex(i); err != nil {
			r.slots[i].Frozen = true
			_ = r.persist()
			return err
		}
	}
	r.slots[i].State = stateQuiescent
	if err := r.persist(); err != nil {
		r.slots[i].Frozen = true
		return err
	}
	return nil
}

func (r *Root) clearGit() {
	r.git = nil
	r.gitRole = ""
	r.gitGen = ""
	r.gitDone = nil
}

func (r *Root) awaitGit(id string) error {
	if r.git == nil {
		r.gitRole = ""
		r.gitGen = ""
		return nil
	}
	pid := r.git.Process.Pid
	done := r.gitDone
	wait := r.git.Wait
	if done != nil {
		wait = func() error { return <-done }
	}
	if r.gitGen != id {
		owner := r.gitGen
		if qerr := awaitClose(pid, wait); qerr != nil {
			r.gitHold = true
			if oi, err := r.index(owner); err == nil {
				r.slots[oi].Frozen = true
				_ = r.persist()
			}
			return qerr
		}
		r.clearGit()
		if oi, err := r.index(owner); err == nil {
			r.slots[oi].Frozen = true
			_ = r.persist()
		}
		return ErrState
	}
	if done == nil {
		done = make(chan error, 1)
		go func() { done <- wait() }()
		r.gitDone = done
	}
	var waitErr error
	select {
	case waitErr = <-done:
	case <-time.After(roleWait):
		qerr := awaitClose(pid, func() error {
			waitErr = <-done
			return waitErr
		})
		if qerr != nil {
			r.gitHold = true
			return qerr
		}
		r.clearGit()
		return ErrNotQuiescent
	}
	if !ownedWaitReceipt(r.git, waitErr) {
		r.holdGit(id)
		return ErrNotQuiescent
	}
	if kerr := syscall.Kill(-pid, 0); !errors.Is(kerr, syscall.ESRCH) {
		r.holdGit(id)
		return ErrNotQuiescent
	}
	r.clearGit()
	if waitErr != nil {
		return waitErr
	}
	return nil
}

func (r *Root) holdGit(id string) {
	r.gitHold = true
	if oi, err := r.index(id); err == nil {
		r.slots[oi].Frozen = true
		_ = r.persist()
	}
}

func (r *Root) measureIndex(i int) error {
	id := r.slots[i].ID
	pack, err := r.openStat(id + "/objects/pack/input.pack")
	if err != nil && !errors.Is(err, ErrAbsent) {
		return err
	}
	if err == nil {
		if pack[0]&uint64(syscall.S_IFMT) != uint64(syscall.S_IFREG) || pack[1] != 1 || pack[2] > PackMax {
			return ErrCorrupt
		}
		r.slots[i].Pack = pack[2]
	}
	idx, err := r.openStat(id + "/objects/pack/input.idx")
	if err != nil {
		return err
	}
	mode := idx[0] & 0777
	if idx[0]&uint64(syscall.S_IFMT) != uint64(syscall.S_IFREG) || idx[1] != 1 || (mode != 0444 && mode != 0600) || idx[2] > IndexMax {
		return ErrCorrupt
	}
	r.slots[i].Index = idx[2]
	return nil
}

func (r *Root) Verify(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.admit(); err != nil {
		return err
	}
	i, err := r.index(id)
	if err != nil {
		return err
	}
	if r.slots[i].Frozen || r.slots[i].State != stateQuiescent {
		return ErrState
	}
	if _, err := CommittedCharge(r.slots[i].Pack, r.slots[i].Index); err != nil {
		return err
	}
	r.slots[i].State = stateVerified
	return r.persist()
}

func (r *Root) Commit(id, dest string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.admit(); err != nil {
		return err
	}
	i, err := r.index(id)
	if err != nil {
		return err
	}
	if r.slots[i].Frozen || r.slots[i].State != stateVerified || !hex32(dest) || dest == id {
		return ErrState
	}
	if r.leaseCount() >= MaxLeases {
		return ErrBusy
	}
	if err := r.matchTree(id, r.slots[i]); err != nil {
		return err
	}
	r.slots[i].DestID = dest
	if err := r.persist(); err != nil {
		r.slots[i].DestID = ""
		return err
	}
	if _, err := r.fs.call(4, []byte(id+"\x00"+dest)); err != nil {
		r.slots[i].Frozen = true
		return err
	}
	r.slots[i].State = stateCommitted
	r.slots[i].ReadCount = 1
	r.access++
	r.slots[i].Access = r.access
	if err := r.persist(); err != nil {
		r.slots[i].State = stateVerified
		r.slots[i].ReadCount = 0
		r.slots[i].Frozen = true
		// The committed record may already have been renamed into place.
		// Put the frozen reservation back before returning so a restart
		// does not observe a reduced charge.
		_ = r.persist()
		return err
	}
	r.ownedPins[i] = 1
	return nil
}

func (r *Root) Pin(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.admit(); err != nil {
		return err
	}
	i, err := r.index(id)
	if err != nil {
		return err
	}
	if r.slots[i].State != stateCommitted || r.slots[i].Frozen {
		return ErrState
	}
	if r.leaseCount() >= MaxLeases {
		return ErrBusy
	}
	r.slots[i].ReadCount++
	r.ownedPins[i]++
	r.access++
	r.slots[i].Access = r.access
	if err := r.persist(); err != nil {
		r.slots[i].ReadCount--
		r.ownedPins[i]--
		r.slots[i].Frozen = true
		r.pinHold = true
		return err
	}
	r.slots[i].PinSync = true
	// Content is readable only after the pin record is durable.
	// A missing manifest is not a pin failure. A symlink or hardlink is.
	if _, err := r.readFile(r.dirOf(i) + "/manifest"); err != nil {
		if errors.Is(err, ErrPath) || errors.Is(err, ErrCorrupt) || errors.Is(err, ErrQuota) {
			return err
		}
	}
	return nil
}

func (r *Root) Unpin(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	i, err := r.index(id)
	if err != nil {
		return err
	}
	if r.pinHold || r.readerFailed[i] {
		return ErrNotQuiescent
	}
	if r.ownedPins[i] == 0 || r.slots[i].ReadCount == 0 {
		return ErrState
	}
	if err := r.readerFinished(i); err != nil {
		return err
	}
	if r.childSlot == i {
		r.childSlot = -1
	}
	r.slots[i].ReadCount--
	r.ownedPins[i]--
	if err := r.persist(); err != nil {
		r.slots[i].ReadCount++
		r.ownedPins[i]++
		if r.childSlot < 0 {
			r.childSlot = i
		}
		r.slots[i].Frozen = true
		r.pinHold = true
		return err
	}
	return nil
}

func (r *Root) leaseCount() uint32 {
	var n uint32
	for _, s := range r.slots {
		n += s.ReadCount
	}
	if r.childSlot >= 0 && n > 0 {
		n--
	}
	return n
}

// StartReader starts the process that holds an owned pin. The pin must
// already be durable. The child does not receive a generation path.
func (r *Root) StartReader(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.admit(); err != nil {
		return err
	}
	i, err := r.index(id)
	if err != nil {
		return err
	}
	if r.ownedPins[i] == 0 || r.readers[i] != nil || r.childSlot >= 0 {
		return ErrState
	}
	var pins uint32
	for _, s := range r.slots {
		pins += s.ReadCount
	}
	if pins >= MaxLeases+1 {
		return ErrBusy
	}
	// The additional child pin is durable before the process exists.
	r.slots[i].ReadCount++
	r.ownedPins[i]++
	r.childSlot = i
	if err := r.persist(); err != nil {
		r.slots[i].Frozen = true
		r.pinHold = true
		return err
	}
	cmd := exec.Command(r.helper, "__gitcache_reader")
	cmd.Env = AllowEnv()
	cmd.Dir = r.path
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		r.slots[i].Frozen = true
		r.pinHold = true
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	r.readers[i] = &slotReader{cmd: cmd, done: done}
	return nil
}

func (r *Root) readerFinished(i int) error {
	rd := r.readers[i]
	if rd == nil {
		return nil
	}
	select {
	case err := <-rd.done:
		if !ownedWaitReceipt(rd.cmd, err) {
			r.pinHold = true
			return ErrNotQuiescent
		}
		pid := rd.cmd.Process.Pid
		if kerr := syscall.Kill(-pid, 0); !errors.Is(kerr, syscall.ESRCH) {
			r.pinHold = true
			return ErrNotQuiescent
		}
		delete(r.readers, i)
		if err != nil {
			r.readerFailed[i] = true
			return err
		}
		return nil
	default:
		return ErrBusy
	}
}

func (r *Root) Launch(role Role, git, id, tip string, idn BuildIdentity, kernel string, darwinMajor int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.admit(); err != nil {
		return err
	}
	if !skipLaunchIdentity {
		if err := IdentityOK(idn, kernel, darwinMajor); err != nil {
			return err
		}
	}
	if r.git != nil {
		return ErrBusy
	}
	i, err := r.index(id)
	if err != nil {
		return err
	}
	if r.slots[i].Frozen || r.slots[i].State != stateActive {
		return ErrState
	}
	cmd, err := LaunchGit(role, r.helper, git, id, tip, r.rootFile, r.lockFile)
	if err != nil {
		if cmd != nil {
			r.retainGit(cmd, role, id)
			r.gitHold = true
			r.slots[i].Frozen = true
			_ = r.persist()
		}
		return err
	}
	done := make(chan error, 1)
	go func() {
		waitDrains(cmd)
		done <- cmd.Wait()
	}()
	r.git = cmd
	r.gitRole = role
	r.gitGen = id
	r.gitDone = done
	return nil
}

func (r *Root) EvictLRU() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.admit(); err != nil {
		return "", err
	}
	best := -1
	var bestA uint64
	for i := range r.slots {
		s := r.slots[i]
		if s.Frozen || s.State != stateCommitted || s.ReadCount != 0 {
			continue
		}
		if best < 0 || s.Access < bestA {
			best = i
			bestA = s.Access
		}
	}
	if best < 0 {
		return "", ErrBusy
	}
	id := r.slots[best].ID
	dir := r.dirOf(best)
	old := r.slots[best]
	if err := r.removeGen(dir); err != nil {
		return "", err
	}
	r.slots[best] = Slot{}
	delete(r.meta, best)
	delete(r.ownedPins, best)
	if err := r.persist(); err != nil {
		r.slots[best] = old
		return "", err
	}
	return id, nil
}

func (r *Root) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	r.draining = true
	if r.pinHold || r.gitHold {
		return ErrNotQuiescent
	}
	if r.git != nil {
		pid := r.git.Process.Pid
		done := r.gitDone
		wait := r.git.Wait
		if done != nil {
			wait = func() error { return <-done }
		}
		if err := awaitClose(pid, wait); err != nil {
			r.gitHold = true
			return err
		}
		r.clearGit()
	}
	for i, rd := range r.readers {
		if err := QuiesceGroup(rd.cmd.Process.Pid, func() error { return <-rd.done }); err != nil {
			return err
		}
		delete(r.readers, i)
	}
	for i := range r.slots {
		if r.ownedPins[i] == 0 || r.slots[i].ReadCount == 0 {
			continue
		}
		if r.ownedPins[i] > r.slots[i].ReadCount {
			return ErrCorrupt
		}
		oldCount := r.slots[i].ReadCount
		oldOwned := r.ownedPins[i]
		r.slots[i].ReadCount -= r.ownedPins[i]
		r.ownedPins[i] = 0
		if err := r.persist(); err != nil {
			r.slots[i].ReadCount = oldCount
			r.ownedPins[i] = oldOwned
			return err
		}
	}
	for _, s := range r.slots {
		if s.ReadCount > 0 {
			return ErrPinned
		}
	}
	for i := range r.slots {
		if !r.owned[i] || r.slots[i].Frozen {
			continue
		}
		if r.slots[i].State == stateEmpty || r.slots[i].State == stateCommitted {
			continue
		}
		old := r.slots[i]
		if err := r.removeGen(r.slots[i].ID); err != nil {
			return err
		}
		r.slots[i] = Slot{}
		if err := r.persist(); err != nil {
			r.slots[i] = old
			return err
		}
	}
	if err := r.persist(); err != nil {
		return err
	}
	if err := r.fs.stop(); err != nil {
		return err
	}
	r.closed = true
	r.lockFile.Close()
	r.rootFile.Close()
	return nil
}

func (r *Root) retainGit(cmd *exec.Cmd, role Role, id string) {
	done := make(chan error, 1)
	go func() {
		waitDrains(cmd)
		done <- cmd.Wait()
	}()
	r.git = cmd
	r.gitRole = role
	r.gitGen = id
	r.gitDone = done
}

func (r *Root) admit() error {
	if r.pinHold || r.gitHold {
		return ErrNotQuiescent
	}
	if r.closed || r.draining {
		return ErrClosed
	}
	return nil
}

// CrashCut kills the helper group without cleanup. Durable reservations remain.
func (r *Root) CrashCut() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.git != nil {
		pid := r.git.Process.Pid
		done := r.gitDone
		wait := r.git.Wait
		if done != nil {
			wait = func() error { return <-done }
		}
		_ = QuiesceGroup(pid, wait)
		r.clearGit()
	}
	for _, rd := range r.readers {
		if rd == nil || rd.cmd == nil || rd.cmd.Process == nil {
			continue
		}
		_ = QuiesceGroup(rd.cmd.Process.Pid, func() error { return <-rd.done })
	}
	r.fs.crash()
	r.lockFile.Close()
	r.rootFile.Close()
}

func (r *Root) State(id string) (uint8, bool, uint32, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i, err := r.index(id)
	if err != nil {
		return 0, false, 0, err
	}
	s := r.slots[i]
	return s.State, s.Frozen, s.ReadCount, nil
}

func (r *Root) dirOf(i int) string {
	if r.slots[i].State == stateCommitted && r.slots[i].DestID != "" {
		return r.slots[i].DestID
	}
	return r.slots[i].ID
}

func (r *Root) index(id string) (int, error) {
	for i := range r.slots {
		if r.slots[i].ID == id && r.slots[i].State != stateEmpty {
			return i, nil
		}
	}
	return 0, ErrState
}

func (r *Root) persist() error {
	buf, err := writeLedger(r.quota, r.slots)
	if err != nil {
		return err
	}
	if _, err := r.writeAt("ledger.tmp", 0, uint64(len(buf)), buf); err != nil {
		return err
	}
	if _, err := r.fs.call(4, []byte("ledger.tmp\x00ledger")); err != nil {
		return err
	}
	_, err = r.fs.call(8, nil)
	return err
}

func (r *Root) writeAt(rel string, off, max uint64, data []byte) (uint64, error) {
	if uint64(len(data)) > max || off > max-uint64(len(data)) {
		return 0, ErrQuota
	}
	const chunk = 256 << 10
	var wrote uint64
	for len(data) > 0 {
		n := len(data)
		if n > chunk {
			n = chunk
		}
		payload := make([]byte, 0, len(rel)+1+16+n)
		payload = append(payload, rel...)
		payload = append(payload, 0)
		var num [16]byte
		binary.LittleEndian.PutUint64(num[0:8], max)
		binary.LittleEndian.PutUint64(num[8:16], off)
		payload = append(payload, num[:]...)
		payload = append(payload, data[:n]...)
		body, err := r.fs.call(1, payload)
		got := uint64(0)
		if len(body) >= 4 {
			got = uint64(binary.LittleEndian.Uint32(body[:4]))
		}
		wrote += got
		if err != nil {
			return wrote, err
		}
		if got != uint64(n) {
			return wrote, ErrQuota
		}
		data = data[n:]
		off += uint64(n)
	}
	return wrote, nil
}

func (r *Root) readFile(rel string) ([]byte, error) {
	return r.fs.call(2, []byte(rel))
}

func (r *Root) names(rel string) ([]string, error) {
	buf, err := r.fs.call(7, []byte(rel))
	if err != nil {
		return nil, err
	}
	if len(buf) == 0 {
		return nil, nil
	}
	parts := strings.Split(string(buf), "\n")
	out := parts[:0]
	for _, p := range parts {
		if p != "" && p != "." && p != ".." {
			out = append(out, p)
		}
	}
	return out, nil
}

func (r *Root) openStat(rel string) ([5]uint64, error) {
	buf, err := r.fs.call(9, []byte(rel))
	if err != nil {
		return [5]uint64{}, err
	}
	return parseStat(buf)
}

func (r *Root) lstat(rel string) ([5]uint64, error) {
	buf, err := r.fs.call(6, []byte(rel))
	if err != nil {
		return [5]uint64{}, err
	}
	return parseStat(buf)
}

func parseStat(buf []byte) ([5]uint64, error) {
	fields := strings.Fields(string(buf))
	if len(fields) != 5 {
		return [5]uint64{}, ErrCorrupt
	}
	var out [5]uint64
	for i, f := range fields {
		var n uint64
		for _, c := range f {
			if c < '0' || c > '9' {
				return [5]uint64{}, ErrCorrupt
			}
			n = n*10 + uint64(c-'0')
		}
		out[i] = n
	}
	return out, nil
}

func (r *Root) removeGen(id string) error {
	if _, err := r.lstat(id); err != nil {
		if errors.Is(err, ErrAbsent) {
			return nil
		}
		return err
	}
	if err := r.auditGen(id); err != nil {
		return err
	}
	files := []string{
		"config", "HEAD", "provenance", "manifest",
		"config.tmp", "HEAD.tmp", "provenance.tmp", "manifest.tmp",
		"refs/heads/acquired", "refs/heads/acquired.tmp",
		"objects/pack/input.pack", "objects/pack/input.idx",
	}
	for _, f := range files {
		if err := r.unlinkExisting(id + "/" + f); err != nil {
			return err
		}
	}
	for _, d := range []string{id + "/objects/pack", id + "/objects/info", id + "/objects", id + "/refs/heads", id + "/refs", id} {
		if err := r.unlinkExisting(d); err != nil {
			return err
		}
	}
	if _, err := r.fs.call(8, nil); err != nil {
		return err
	}
	return nil
}

func (r *Root) auditGen(id string) error {
	tops, err := r.names(id)
	if err != nil {
		return err
	}
	for _, n := range tops {
		if !knownGenName(n) {
			return ErrCorrupt
		}
	}
	checks := []struct {
		dir   string
		allow map[string]bool
	}{
		{id + "/objects", map[string]bool{"pack": true, "info": true}},
		{id + "/objects/pack", map[string]bool{"input.pack": true, "input.idx": true}},
		{id + "/objects/info", map[string]bool{}},
		{id + "/refs", map[string]bool{"heads": true}},
		{id + "/refs/heads", map[string]bool{"acquired": true, "acquired.tmp": true}},
	}
	for _, c := range checks {
		kids, err := r.names(c.dir)
		if errors.Is(err, ErrAbsent) {
			continue
		}
		if err != nil {
			return err
		}
		for _, n := range kids {
			if !c.allow[n] {
				return ErrCorrupt
			}
		}
	}
	return nil
}

func knownGenName(name string) bool {
	switch name {
	case "config", "HEAD", "provenance", "manifest",
		"config.tmp", "HEAD.tmp", "provenance.tmp", "manifest.tmp",
		"objects", "refs":
		return true
	default:
		return false
	}
}

func (r *Root) unlinkExisting(rel string) error {
	if _, err := r.lstat(rel); err != nil {
		if errors.Is(err, ErrAbsent) {
			return nil
		}
		return err
	}
	_, err := r.fs.call(5, []byte(rel))
	return err
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func stripe(domain string) int {
	sum := sha256.Sum256([]byte(domain))
	return int(sum[0] % 64)
}
