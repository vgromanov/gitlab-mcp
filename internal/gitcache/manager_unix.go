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
)

// Root is one private cache directory. It is unreachable from production startup.
type Root struct {
	path     string
	helper   string
	quota    uint64
	slots    []Slot
	owned    map[int]bool
	meta     map[int]map[string]uint64
	access   uint64
	fs       *fsClient
	rootFile *os.File
	lockFile *os.File
	git      *exec.Cmd
	draining bool
	closed   bool
	mu       sync.Mutex
	domains  [64]sync.Mutex
}

// Open validates a private directory, takes the root lock, and loads the ledger.
// Non-committed slots stay frozen and keep a full reservation.
func Open(path, helper string, quota uint64) (*Root, error) {
	if err := QuotaOK(quota); err != nil {
		return nil, err
	}
	rf, err := walkRoot(path)
	if err != nil {
		return nil, err
	}
	lfd, err := openatNoFollow(int(rf.Fd()), "root.lock", syscall.O_RDWR|syscall.O_CREAT, 0600)
	if err != nil {
		rf.Close()
		return nil, ErrPath
	}
	if err := syscall.Flock(lfd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		syscall.Close(lfd)
		rf.Close()
		return nil, ErrBusy
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
		owned: map[int]bool{}, meta: map[int]map[string]uint64{},
		fs: fs, rootFile: rf, lockFile: lock,
	}
	if err := r.load(); err != nil {
		_ = fs.stop()
		lock.Close()
		rf.Close()
		return nil, err
	}
	return r, nil
}

func (r *Root) load() error {
	names, err := r.names(".")
	if err != nil {
		return err
	}
	haveLedger := false
	for _, n := range names {
		if !allowedRootName(n) {
			return ErrCorrupt
		}
		st, err := r.lstat(n)
		if err != nil {
			return ErrCorrupt
		}
		kind := st[0] & uint64(syscall.S_IFMT)
		if kind == uint64(syscall.S_IFLNK) {
			return ErrPath
		}
		if kind == uint64(syscall.S_IFREG) && st[1] != 1 {
			return ErrPath
		}
		if n == "ledger" {
			haveLedger = true
		}
		if hex32(n) {
			if kind != uint64(syscall.S_IFDIR) {
				return ErrPath
			}
		}
	}
	if !haveLedger {
		r.slots = make([]Slot, MaxSlots)
		return r.persist()
	}
	buf, err := r.readFile("ledger")
	if err != nil {
		return err
	}
	_, slots, err := parseLedger(buf)
	if err != nil {
		return err
	}
	r.slots = slots
	return nil
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
		r.slots[idx] = Slot{}
		return "", err
	}
	r.owned[idx] = true
	return id, nil
}

func (r *Root) Activate(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
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
	if err := r.writeAt(rel, r.slots[i].Pack, PackMax, data); err != nil {
		return err
	}
	r.slots[i].Pack += uint64(len(data))
	return r.persist()
}

func (r *Root) WriteMeta(id, name string, data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
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
	if err := r.writeAt(tmp, 0, metaCap(name), data); err != nil {
		return err
	}
	if _, err := r.fs.call(4, []byte(tmp+"\x00"+final)); err != nil {
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
	if err := r.writeAt(rel, 0, IndexMax, data); err != nil {
		return err
	}
	r.slots[i].Index = uint64(len(data))
	return r.persist()
}

func (r *Root) Quiesce(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	i, err := r.index(id)
	if err != nil {
		return err
	}
	if r.slots[i].Frozen || r.slots[i].State != stateActive {
		return ErrState
	}
	if r.git != nil {
		if err := QuiesceGroup(r.git.Process.Pid, r.git.Wait); err != nil {
			return err
		}
		r.git = nil
	}
	r.slots[i].State = stateQuiescent
	return r.persist()
}

func (r *Root) Verify(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
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
	i, err := r.index(id)
	if err != nil {
		return err
	}
	if r.slots[i].Frozen || r.slots[i].State != stateVerified || !hex32(dest) || dest == id {
		return ErrState
	}
	r.slots[i].DestID = dest
	if err := r.persist(); err != nil {
		r.slots[i].DestID = ""
		return err
	}
	if _, err := r.fs.call(4, []byte(id+"\x00"+dest)); err != nil {
		return err
	}
	r.slots[i].State = stateCommitted
	r.access++
	r.slots[i].Access = r.access
	if err := r.persist(); err != nil {
		return err
	}
	return nil
}

func (r *Root) Pin(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	i, err := r.index(id)
	if err != nil {
		return err
	}
	if r.slots[i].State != stateCommitted {
		return ErrState
	}
	var leases uint32
	for _, s := range r.slots {
		leases += s.ReadCount
	}
	if leases >= MaxLeases {
		return ErrBusy
	}
	r.slots[i].ReadCount++
	r.access++
	r.slots[i].Access = r.access
	if err := r.persist(); err != nil {
		r.slots[i].ReadCount--
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
	if r.slots[i].ReadCount == 0 {
		return ErrState
	}
	r.slots[i].ReadCount--
	if err := r.persist(); err != nil {
		r.slots[i].ReadCount++
		return err
	}
	return nil
}

func (r *Root) Launch(role Role, git, id, tip string, idn BuildIdentity, kernel string, darwinMajor int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := IdentityOK(idn, kernel, darwinMajor); err != nil {
		return err
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
	cmd, err := LaunchGit(role, git, id, tip, r.rootFile, r.lockFile)
	if err != nil {
		return err
	}
	r.git = cmd
	return nil
}

func (r *Root) EvictLRU() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
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
	if err := r.removeGen(dir); err != nil {
		return "", err
	}
	r.slots[best] = Slot{}
	delete(r.meta, best)
	if err := r.persist(); err != nil {
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
	for _, s := range r.slots {
		if s.ReadCount > 0 {
			return ErrPinned
		}
	}
	r.closed = true
	if r.git != nil {
		if err := QuiesceGroup(r.git.Process.Pid, r.git.Wait); err != nil {
			return err
		}
		r.git = nil
	}
	for i := range r.slots {
		if !r.owned[i] || r.slots[i].Frozen {
			continue
		}
		if r.slots[i].State == stateEmpty || r.slots[i].State == stateCommitted {
			continue
		}
		if err := r.removeGen(r.slots[i].ID); err != nil {
			return err
		}
		r.slots[i] = Slot{}
	}
	if err := r.persist(); err != nil {
		return err
	}
	err := r.fs.stop()
	r.lockFile.Close()
	r.rootFile.Close()
	return err
}

// CrashCut kills the helper group without cleanup. Durable reservations remain.
func (r *Root) CrashCut() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.git != nil {
		_ = QuiesceGroup(r.git.Process.Pid, r.git.Wait)
		r.git = nil
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
	if err := r.writeAt("ledger.tmp", 0, uint64(len(buf)), buf); err != nil {
		return err
	}
	if _, err := r.fs.call(4, []byte("ledger.tmp\x00ledger")); err != nil {
		return err
	}
	_, err = r.fs.call(8, nil)
	return err
}

func (r *Root) writeAt(rel string, off, max uint64, data []byte) error {
	if uint64(len(data)) > max || off > max-uint64(len(data)) {
		return ErrQuota
	}
	const chunk = 256 << 10
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
		if err != nil {
			return err
		}
		if len(body) != 4 || binary.LittleEndian.Uint32(body) != uint32(n) {
			return ErrQuota
		}
		data = data[n:]
		off += uint64(n)
	}
	return nil
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

func (r *Root) lstat(rel string) ([5]uint64, error) {
	buf, err := r.fs.call(6, []byte(rel))
	if err != nil {
		return [5]uint64{}, err
	}
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
		return nil
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
		if err != nil {
			continue
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
		return nil
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
