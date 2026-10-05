//go:build linux || darwin

package gitcache

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"sync"
	"syscall"

	"github.com/go-git/go-git/v5/plumbing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/pack"
)

const (
	lockName   = "root.lock"
	ledgerName = "ledger.bin"
	ledgerTmp  = "ledger.tmp"
	gensDir    = "generations"
	stageDir   = "staging"
)

// Manager owns one private cache root for the process lifetime.
type Manager struct {
	root         *os.File
	lock         *os.File
	rootDev      uint64
	quota        uint64
	slots        []Slot
	mu           sync.Mutex
	closed       bool
	closing      bool
	closeMu      sync.Mutex
	acqBusy      bool
	readers      int
	cancel       context.CancelFunc
	workCtx      context.Context
	wg           sync.WaitGroup
	fault        *FaultPoints // test seams
	ledgerDigest [32]byte
	closeDone    chan struct{}
}

// FaultPoints are hermetic crash injection points. Nil in production.
type FaultPoints struct {
	AfterReserve func() error
	AfterStage   func() error
	AfterIndex   func() error
	AfterLedger  func() error
	AfterFsync   func() error
	AfterRename  func() error
	BeforeDelete func() error
}

// SetFaultPoints installs crash-injection seams for hermetic tests only.
func (m *Manager) SetFaultPoints(fp *FaultPoints) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fault = fp
}

// ChargedBytes returns the durable charged total under the manager lock.
func (m *Manager) ChargedBytes() (uint64, error) {
	if m == nil {
		return 0, ErrClosed
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return chargedTotal(m.slots)
}

// AmbiguousSlots reports how many slots are in SlotAmbiguous (crash retained).
func (m *Manager) AmbiguousSlots() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.slots {
		if s.State == SlotAmbiguous {
			n++
		}
	}
	return n
}

// Generation is an immutable published pack/index/manifest set.
type Generation struct {
	ID        string
	PackSize  int64
	IndexSize int64
	MetaSize  int64
	Objects   map[plumbing.Hash]pack.Object
	mgr       *Manager
	slot      int
	pins      int
}

// OpenManager walks path with nofollow descriptors, takes an exclusive lock,
// and recovers under that lock. quota is the logical disk budget.
func OpenManager(path string, quota int64) (*Manager, error) {
	if quota <= 0 {
		quota = bounds.DefaultQuotaBytes
	}
	if uint64(quota) < uint64(bounds.BrootBytes) {
		return nil, ErrQuota
	}
	root, dev, err := walkRoot(path)
	if err != nil {
		return nil, err
	}
	// flock follows the directory inode. Replacing root.lock creates a new file
	// inode and must not admit a second manager while this one is alive.
	if err := flockExclusive(int(root.Fd())); err != nil {
		_ = root.Close()
		return nil, err
	}
	lock, err := openRootLock(int(root.Fd()), dev)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if err := confirmLockFile(int(root.Fd()), int(lock.Fd())); err != nil {
		_ = lock.Close()
		_ = root.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		root:    root,
		lock:    lock,
		rootDev: dev,
		quota:   uint64(quota),
		slots:   make([]Slot, slotCount),
		workCtx: ctx,
		cancel:  cancel,
	}
	if err := m.loadOrInit(); err != nil {
		cancel()
		_ = lock.Close()
		_ = root.Close()
		return nil, err
	}
	if err := m.recoverLocked(); err != nil {
		cancel()
		_ = lock.Close()
		_ = root.Close()
		return nil, err
	}
	return m, nil
}

func openRootLock(rootfd int, rootDev uint64) (*os.File, error) {
	fd, err := openatNoFollow(rootfd, lockName, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, ErrPath
	}
	st, err := fstat(fd)
	if err != nil {
		syscall.Close(fd)
		return nil, ErrPath
	}
	if err := checkRegular(&st, rootDev); err != nil || st.Size != bounds.LockReserveBytes {
		syscall.Close(fd)
		return nil, ErrPath
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		syscall.Close(fd)
		return nil, ErrBusy
	}
	return os.NewFile(uintptr(fd), lockName), nil
}

func flockExclusive(fd int) error {
	err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return ErrBusy
	}
	return ErrPath
}

// confirmLockFile fails closed when the held lock inode is no longer the name
// root.lock. A pathname stat by itself cannot exclude a manager that already
// opened a replacement inode.
func confirmLockFile(rootfd, lockfd int) error {
	held, err := fstat(lockfd)
	if err != nil {
		return ErrIntegrity
	}
	var current unix.Stat_t
	if err := unix.Fstatat(rootfd, lockName, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil || uint64(current.Ino) != uint64(held.Ino) || uint64(current.Dev) != uint64(held.Dev) {
		return ErrIntegrity
	}
	return nil
}

func (m *Manager) confirmLockIdentity() error {
	if m == nil || m.root == nil || m.lock == nil {
		return ErrClosed
	}
	return confirmLockFile(int(m.root.Fd()), int(m.lock.Fd()))
}

// Open a fresh directory description: Dup shares the enumeration offset.
func listNamesAt(dirfd, max int) ([]string, error) {
	fd, err := openatDir(dirfd, ".")
	if err != nil {
		return nil, ErrPath
	}
	f := os.NewFile(uintptr(fd), ".")
	defer f.Close()
	entries, err := f.ReadDir(max + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, ErrCorrupt
	}
	if len(entries) > max {
		return nil, ErrLimit
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}
func (m *Manager) listRootNames() ([]string, error) { return listNamesAt(int(m.root.Fd()), 5) }

func (m *Manager) assertInitableRoot() error {
	names, err := m.listRootNames()
	if err != nil {
		return err
	}
	for _, name := range names {
		switch name {
		case ".", "..", lockName:
			continue
		default:
			return ErrCorrupt
		}
	}
	return nil
}

func (m *Manager) inventoryExistingRoot() error {
	names, err := m.listRootNames()
	if err != nil {
		return err
	}
	allowed := map[string]bool{
		".": true, "..": true,
		lockName: true, ledgerName: true, ledgerTmp: true,
		gensDir: true, stageDir: true,
	}
	for _, name := range names {
		if !allowed[name] {
			return ErrCorrupt
		}
		switch name {
		case lockName, ledgerName, ledgerTmp:
			fd, err := openatNoFollow(int(m.root.Fd()), name, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				return ErrCorrupt
			}
			st, err := fstat(fd)
			syscall.Close(fd)
			if err != nil || checkRegular(&st, m.rootDev) != nil || st.Size < 0 {
				return ErrCorrupt
			}
			if name == lockName && st.Size != 0 {
				return ErrCorrupt
			}
			if name == ledgerName && st.Size != bounds.LedgerBytes {
				return ErrCorrupt
			}
			if name == ledgerTmp && st.Size > bounds.LedgerBytes {
				return ErrCorrupt
			}
		case gensDir, stageDir:
			fd, err := m.openPrivateDir(int(m.root.Fd()), name)
			if err != nil {
				return err
			}
			syscall.Close(fd)
		}
	}
	return nil
}

func (m *Manager) loadOrInit() error {
	fd, err := openatNoFollow(int(m.root.Fd()), ledgerName, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if !errors.Is(err, syscall.ENOENT) {
			return ErrCorrupt
		}
		if err := m.assertInitableRoot(); err != nil {
			return err
		}
		buf, err := emptyLedger(m.quota)
		if err != nil {
			return err
		}
		return m.writeLedger(buf)
	}
	f := os.NewFile(uintptr(fd), ledgerName)
	defer f.Close()
	st, err := fstat(fd)
	if err != nil {
		return ErrCorrupt
	}
	if err := checkRegular(&st, m.rootDev); err != nil {
		return err
	}
	if st.Size != int64(headerBytes+slotCount*slotBytes) {
		return ErrCorrupt
	}
	buf := make([]byte, st.Size)
	if _, err := io.ReadFull(f, buf); err != nil {
		return ErrCorrupt
	}
	h, slots, err := parseLedger(buf)
	if err != nil {
		return err
	}
	if err := validateLedgerSemantics(h, slots); err != nil {
		return err
	}
	if h.Quota != m.quota {
		return ErrQuota
	}
	if err := m.inventoryExistingRoot(); err != nil {
		return err
	}
	m.slots = slots
	m.ledgerDigest = sha256.Sum256(buf)
	return nil
}

func openFileAt(m *Manager, rel string, flags int, perm uint32) (*os.File, error) {
	parts, err := splitRel(rel)
	if err != nil {
		return nil, err
	}
	dirfd := int(m.root.Fd())
	owned := []int{}
	defer func() {
		for _, fd := range owned {
			syscall.Close(fd)
		}
	}()
	for i, name := range parts {
		if i == len(parts)-1 {
			fd, err := openatNoFollow(dirfd, name, flags, perm)
			if err != nil {
				return nil, err
			}
			return os.NewFile(uintptr(fd), rel), nil
		}
		next, err := openatDir(dirfd, name)
		if err != nil {
			return nil, err
		}
		st, err := fstat(next)
		if err != nil || checkDir(&st, m.rootDev) != nil {
			syscall.Close(next)
			return nil, ErrPath
		}
		owned = append(owned, next)
		dirfd = next
	}
	return nil, ErrPath
}

func (m *Manager) writeLedger(buf []byte) error {
	return m.writeLedgerContext(context.Background(), buf)
}
func (m *Manager) writeLedgerContext(ctx context.Context, buf []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h, slots, err := parseLedger(buf)
	if err != nil {
		return err
	}
	if err := validateLedgerSemantics(h, slots); err != nil {
		return err
	}
	if err := m.inventoryExistingRoot(); err != nil {
		return err
	}
	if m.ledgerDigest != ([32]byte{}) {
		old, err := readAllAtFDContext(ctx, int(m.root.Fd()), ledgerName, bounds.LedgerBytes, m.rootDev)
		if err != nil {
			return err
		}
		if sha256.Sum256(old) != m.ledgerDigest {
			return ErrIntegrity
		}
	}

	if m.fault != nil && m.fault.AfterLedger != nil {
		if err := m.fault.AfterLedger(); err != nil {
			return err
		}
	}
	// Open without O_TRUNC; validate private regular attrs, then truncate.
	fd, err := openatNoFollow(int(m.root.Fd()), ledgerTmp, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return ErrPath
	}
	st, err := fstat(fd)
	if err != nil {
		syscall.Close(fd)
		return ErrPath
	}
	if err := checkRegular(&st, m.rootDev); err != nil {
		syscall.Close(fd)
		return err
	}
	if st.Size < 0 || st.Size > bounds.LedgerBytes {
		syscall.Close(fd)
		return ErrCorrupt
	}
	if err := ctx.Err(); err != nil {
		syscall.Close(fd)
		return err
	}
	if st.Size > 0 {
		if st.Size != bounds.LedgerBytes {
			syscall.Close(fd)
			return ErrCorrupt
		}
		existing := os.NewFile(uintptr(fd), ledgerTmp)
		draft := make([]byte, bounds.LedgerBytes)
		_, readErr := existing.ReadAt(draft, 0)
		dh, ds, parseErr := parseLedger(draft)
		if readErr != nil || parseErr != nil || dh.Quota != m.quota || validateLedgerSemantics(dh, ds) != nil {
			existing.Close()
			return ErrCorrupt
		}
		// os.File owns fd below; duplicate before transferring ownership back.
		copyFD, dupErr := syscall.Dup(fd)
		if dupErr != nil {
			existing.Close()
			return ErrPath
		}
		existing.Close()
		fd = copyFD
	}
	if err := syscall.Ftruncate(fd, 0); err != nil {
		syscall.Close(fd)
		return ErrPath
	}
	f := os.NewFile(uintptr(fd), ledgerTmp)
	if _, err := f.Write(buf); err != nil {
		f.Close()
		return err
	}
	if err := ctx.Err(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if m.fault != nil && m.fault.AfterFsync != nil {
		if err := m.fault.AfterFsync(); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := renameat(int(m.root.Fd()), ledgerTmp, int(m.root.Fd()), ledgerName); err != nil {
		return ErrPath
	}
	m.ledgerDigest = sha256.Sum256(buf)
	if m.fault != nil && m.fault.AfterRename != nil {
		if err := m.fault.AfterRename(); err != nil {
			return err
		}
	}
	if err := syscall.Fsync(int(m.root.Fd())); err != nil {
		return err
	}
	if err := compareFileAt(ctx, int(m.root.Fd()), ledgerName, buf, m.rootDev); err != nil {
		return err
	}
	m.ledgerDigest = sha256.Sum256(buf)
	m.quota = h.Quota
	m.slots = slots
	return nil
}

func (m *Manager) persistSlots() error { return m.persistSlotsContext(context.Background()) }
func (m *Manager) persistSlotsContext(ctx context.Context) error {
	if err := m.confirmLockIdentity(); err != nil {
		return err
	}
	h := ledgerHeader{Quota: m.quota}
	buf, err := encodeLedger(h, m.slots)
	if err != nil {
		return err
	}
	return m.writeLedgerContext(ctx, buf)
}

func (m *Manager) ensureControlDir(name string) error {
	if err := mkdirat(int(m.root.Fd()), name, 0o700); err != nil && err != syscall.EEXIST {
		return ErrPath
	}
	fd, err := openatDir(int(m.root.Fd()), name)
	if err != nil {
		return ErrPath
	}
	defer syscall.Close(fd)
	st, err := fstat(fd)
	if err != nil {
		return ErrPath
	}
	return checkDir(&st, m.rootDev)
}

func (m *Manager) recoverLocked() error {
	if err := m.confirmLockIdentity(); err != nil {
		return err
	}
	if err := m.ensureControlDir(gensDir); err != nil {
		return err
	}
	if err := m.ensureControlDir(stageDir); err != nil {
		return err
	}
	// Clear reclaimable staging contents under the exclusive root lock.
	if err := m.reclaimStagingLocked(); err != nil {
		return err
	}
	if err := m.inventoryGenerationsLocked(); err != nil {
		return err
	}
	// Stale in-flight states retain conservative charge as ambiguous.
	for i := range m.slots {
		s := &m.slots[i]
		switch s.State {
		case SlotReserved, SlotStaging, SlotActive, SlotDeleting:
			s.State = SlotAmbiguous
			if s.Charge == 0 {
				s.Charge = uint64(bounds.GenerationCharge())
			}
			s.ReadCount = 0
		case SlotCommitted:
			s.ReadCount = 0
		case SlotAmbiguous:
			s.ReadCount = 0
		case SlotEmpty:
		default:
			return ErrCorrupt
		}
	}
	return m.persistSlots()
}

func (m *Manager) reclaimStagingLocked() error {
	fd, err := m.openPrivateDir(int(m.root.Fd()), stageDir)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	names, err := listNamesAt(fd, 0)
	if err != nil {
		return err
	}
	// Production writes directly into a uniquely reserved generation. No staging
	// children are owned by this format; arbitrary files must never be removed.
	if len(names) != 0 {
		return ErrCorrupt
	}
	return nil
}

func (m *Manager) reclaimReservedLocked(slot int) {
	if slot < 0 || slot >= len(m.slots) {
		return
	}
	m.slots[slot] = Slot{State: SlotEmpty}
}

// Close stops new work, cancels and joins outstanding work, then releases the lock.
// Pinned readers fail closed with ErrPinned and leave the manager open.
func (m *Manager) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// Immutable lifetime cancel may run without the state mutex, including while
	// publication/local validation holds it. Local checkpoints observe cancellation.
	if m.cancel != nil {
		m.cancel()
	}
	m.closeMu.Lock()
	defer m.closeMu.Unlock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closing = true
	if m.readers > 0 {
		m.mu.Unlock()
		return ErrPinned
	}
	m.mu.Unlock()
	if m.closeDone == nil {
		m.closeDone = make(chan struct{})
		go func() { m.wg.Wait(); close(m.closeDone) }()
	}
	done := m.closeDone
	select {
	case <-done:
	case <-ctx.Done():
		return ErrCanceled
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readers > 0 {
		return ErrPinned
	}
	// An uncertain join retains fds and closing state; a later Close retries it.
	// Close the root first: an error still leaves the lifetime lock held.
	// os.File.Close consumes its descriptor even when it reports an error, so a
	// later Close can retry the remaining lock release without using a stale fd.
	if m.root != nil {
		err := m.root.Close()
		m.root = nil
		if err != nil {
			return err
		}
	}
	if m.lock != nil {
		err := m.lock.Close()
		m.lock = nil
		m.closed = true
		if err != nil {
			return err
		}
	}
	m.closed = true
	return nil
}

// reserveLocked charges a full generation before any output write.
func (m *Manager) reserveLocked(ctx context.Context) (int, [32]byte, error) {
	if err := ctx.Err(); err != nil {
		return -1, [32]byte{}, err
	}
	if err := m.inventoryExistingRoot(); err != nil {
		return -1, [32]byte{}, err
	}
	if err := m.inventoryGenerationsLocked(); err != nil {
		return -1, [32]byte{}, err
	}
	if err := m.reclaimStagingLocked(); err != nil {
		return -1, [32]byte{}, err
	}
	total, err := chargedTotal(m.slots)
	if err != nil {
		return -1, [32]byte{}, err
	}
	need := uint64(bounds.GenerationCharge())
	broot := uint64(bounds.BrootBytes)
	charged := broot + total
	if charged < broot {
		return -1, [32]byte{}, ErrOverflow
	}
	withNeed := charged + need
	if withNeed < charged || withNeed > m.quota {
		return -1, [32]byte{}, ErrQuota
	}
	var id [32]byte
	if _, err := rand.Read(id[:]); err != nil {
		return -1, id, err
	}
	if id == ([32]byte{}) {
		return -1, id, ErrIntegrity
	}
	for _, s := range m.slots {
		if s.State != SlotEmpty && s.ID == id {
			return -1, id, ErrIntegrity
		}
	}
	for i := range m.slots {
		if m.slots[i].State == SlotEmpty {
			m.slots[i] = Slot{
				State:  SlotReserved,
				ID:     id,
				Charge: need,
			}
			if m.fault != nil && m.fault.AfterReserve != nil {
				if err := m.fault.AfterReserve(); err != nil {
					m.slots[i].State = SlotAmbiguous
					_ = m.persistSlotsContext(ctx)
					return -1, id, err
				}
			}
			if err := m.persistSlotsContext(ctx); err != nil {
				m.slots[i].State = SlotAmbiguous
				return -1, id, err
			}
			return i, id, nil
		}
	}
	return -1, [32]byte{}, ErrQuota
}

type genManifest struct {
	GrantFP string            `json:"grant_fp"`
	Head    string            `json:"head"`
	Base    string            `json:"base"`
	Start   string            `json:"start"`
	Objects map[string]string `json:"objects"` // hash -> type
	NS      string            `json:"ns"`
}

// PublishGeneration writes pack+index+manifest under a prior reservation.
// ip must already be the sole decode of the pack (DecodeIndexed). Index v2 is
// built from ip.Entries without a second decode, so input pack bytes and one
// Objects map are the only simultaneously retained pack payloads.
func (m *Manager) publishGeneration(ctx context.Context, grantFP, ns string, head, base, start plumbing.Hash, ip pack.IndexedPack) (*Generation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.closing {
		return nil, ErrClosed
	}
	// Whole-acquisition admission owns acqBusy (Acquire). Direct publish tests
	// serialize on the manager mutex alone.

	slot, id, err := m.reserveLocked(ctx)
	if err != nil {
		return nil, err
	}
	idHex := hex.EncodeToString(id[:])

	failPre := func(err error) (*Generation, error) {
		prior := m.slots[slot]
		m.reclaimReservedLocked(slot)
		if releaseErr := m.persistSlotsContext(ctx); releaseErr != nil {
			prior.State = SlotAmbiguous
			m.slots[slot] = prior
			return nil, errors.Join(err, releaseErr)
		}
		return nil, err
	}
	failPost := func(err error) (*Generation, error) {
		m.slots[slot].State = SlotAmbiguous
		m.slots[slot].Charge = uint64(bounds.GenerationCharge())
		if persistErr := m.persistSlotsContext(ctx); persistErr != nil {
			return nil, errors.Join(err, persistErr)
		}
		return nil, err
	}

	packBytes := ip.PackBytes
	objs := ip.Objects
	if packBytes == nil || objs == nil || len(ip.Entries) == 0 || len(objs) > bounds.MaxObjects || len(grantFP) > 256 || len(ns) > 256 {
		return failPre(ErrCorrupt)
	}
	if int64(len(packBytes)) > bounds.MaxPackBytes {
		return failPre(ErrLimit)
	}
	if out := ip.RetainedOutputBytes(); out > bounds.MaxRetainedOutput {
		return failPre(ErrLimit)
	}
	// Index from the existing IndexedPack only — no re-decode / second Objects map.
	idx, err := pack.BuildIndexV2(ctx, ip)
	if err != nil {
		return failPre(err)
	}
	if int64(len(idx)) > bounds.MaxIndexBytes {
		return failPre(ErrLimit)
	}
	metaObj := genManifest{
		GrantFP: grantFP,
		Head:    head.String(),
		Base:    base.String(),
		Start:   start.String(),
		Objects: map[string]string{},
		NS:      ns,
	}
	for h, o := range objs {
		if err := ctx.Err(); err != nil {
			return failPre(err)
		}
		if o.Hash != h || (o.Type != "commit" && o.Type != "tree" && o.Type != "blob" && o.Type != "tag") {
			return failPre(ErrCorrupt)
		}
		metaObj.Objects[h.String()] = o.Type
	}
	meta, err := json.Marshal(metaObj)
	if err != nil || int64(len(meta)) > bounds.MaxMetaBytes {
		return failPre(ErrLimit)
	}
	if err := ctx.Err(); err != nil {
		return failPre(err)
	}

	m.slots[slot].State = SlotStaging
	if err := m.persistSlotsContext(ctx); err != nil {
		m.slots[slot].State = SlotAmbiguous
		return nil, err
	}
	if m.fault != nil && m.fault.AfterStage != nil {
		if err := m.fault.AfterStage(); err != nil {
			return failPost(err)
		}
	}

	if err := m.ensureControlDir(gensDir); err != nil {
		return failPost(err)
	}
	gfd, err := m.openPrivateDir(int(m.root.Fd()), gensDir)
	if err != nil {
		return failPost(ErrPath)
	}
	if err := mkdirat(gfd, idHex, 0o700); err != nil {
		syscall.Close(gfd)
		return failPost(ErrPath)
	}
	dirfd, err := openatDir(gfd, idHex)
	if err != nil {
		syscall.Close(gfd)
		return failPost(ErrPath)
	}
	dst, err := fstat(dirfd)
	if err != nil || checkDir(&dst, m.rootDev) != nil {
		syscall.Close(dirfd)
		syscall.Close(gfd)
		return failPost(ErrPath)
	}

	if err := writeFileAtFDContext(ctx, dirfd, "pack.pack", packBytes, m); err != nil {
		syscall.Close(dirfd)
		syscall.Close(gfd)
		return failPost(err)
	}
	if m.fault != nil && m.fault.AfterIndex != nil {
		if err := m.fault.AfterIndex(); err != nil {
			syscall.Close(dirfd)
			syscall.Close(gfd)
			return failPost(err)
		}
	}
	if err := writeFileAtFDContext(ctx, dirfd, "pack.idx", idx, m); err != nil {
		syscall.Close(dirfd)
		syscall.Close(gfd)
		return failPost(err)
	}
	if err := writeFileAtFDContext(ctx, dirfd, "manifest.json", meta, m); err != nil {
		syscall.Close(dirfd)
		syscall.Close(gfd)
		return failPost(err)
	}
	// Durable directory ordering before ledger commit.
	if err := syscall.Fsync(dirfd); err != nil {
		syscall.Close(dirfd)
		syscall.Close(gfd)
		return failPost(err)
	}
	if err := syscall.Fsync(gfd); err != nil {
		syscall.Close(dirfd)
		syscall.Close(gfd)
		return failPost(err)
	}
	// Positive readback of pack/index/manifest before committing the ledger.
	if err := verifyGenerationFilesContext(ctx, dirfd, packBytes, idx, meta, m.rootDev); err != nil {
		syscall.Close(dirfd)
		syscall.Close(gfd)
		return failPost(err)
	}
	syscall.Close(dirfd)
	syscall.Close(gfd)

	m.slots[slot].ManifestDigest = sha256.Sum256(meta)
	m.slots[slot].State = SlotCommitted
	m.slots[slot].PackSize = uint64(len(packBytes))
	m.slots[slot].IndexSize = uint64(len(idx))
	m.slots[slot].MetaSize = uint64(len(meta))
	actual := m.slots[slot].PackSize + m.slots[slot].IndexSize + m.slots[slot].MetaSize
	if actual > 0 && actual < m.slots[slot].Charge {
		m.slots[slot].Charge = actual
	}
	if err := m.persistSlotsContext(ctx); err != nil {
		m.slots[slot].State = SlotAmbiguous
		m.slots[slot].Charge = uint64(bounds.GenerationCharge())
		return nil, err
	}
	return &Generation{
		ID:        idHex,
		PackSize:  int64(len(packBytes)),
		IndexSize: int64(len(idx)),
		MetaSize:  int64(len(meta)),
		Objects:   objs,
		mgr:       m,
		slot:      slot,
	}, nil
}

func verifyGenerationFiles(dirfd int, p, idx, meta []byte, dev uint64) error {
	return verifyGenerationFilesContext(context.Background(), dirfd, p, idx, meta, dev)
}
func verifyGenerationFilesContext(ctx context.Context, dirfd int, p, idx, meta []byte, dev uint64) error {
	for _, entry := range []struct {
		name string
		data []byte
	}{{"pack.pack", p}, {"pack.idx", idx}, {"manifest.json", meta}} {
		if err := compareFileAt(ctx, dirfd, entry.name, entry.data, dev); err != nil {
			return err
		}
	}
	return nil
}

// Stream readback avoids retaining another whole pack. Compare exact bytes,
// size and held descriptor metadata; cancellation remains typed.
func compareFileAt(ctx context.Context, dirfd int, name string, want []byte, dev uint64) error {
	fd, err := openatNoFollow(dirfd, name, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ErrIntegrity
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	st, err := fstat(fd)
	if err != nil || checkRegular(&st, dev) != nil || st.Size != int64(len(want)) {
		return ErrIntegrity
	}
	buf := make([]byte, 32768)
	for pos := 0; pos < len(want); {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := pos + len(buf)
		if end > len(want) {
			end = len(want)
		}
		if _, err := io.ReadFull(f, buf[:end-pos]); err != nil {
			return ErrIntegrity
		}
		if !bytes.Equal(buf[:end-pos], want[pos:end]) {
			return ErrIntegrity
		}
		pos = end
	}
	after, err := fstat(fd)
	if err != nil || checkRegular(&after, dev) != nil || st.Dev != after.Dev || st.Ino != after.Ino || st.Size != after.Size {
		return ErrIntegrity
	}
	return ctx.Err()
}
func writeFileAtFD(dirfd int, name string, body []byte, m *Manager) error {
	return writeFileAtFDContext(context.Background(), dirfd, name, body, m)
}
func writeFileAtFDContext(ctx context.Context, dirfd int, name string, body []byte, m *Manager) error {
	fd, err := openatNoFollow(dirfd, name, syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return ErrPath
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	st, err := fstat(fd)
	if err != nil || checkRegular(&st, m.rootDev) != nil {
		return ErrPath
	}
	for pos := 0; pos < len(body); {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := pos + 32768
		if end > len(body) {
			end = len(body)
		}
		n, err := f.Write(body[pos:end])
		if err != nil {
			return err
		}
		if n != end-pos {
			return io.ErrShortWrite
		}
		pos = end
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.Sync()
}

// Pin pins a committed generation against eviction. Each Pin requires a matching Unpin.
func (g *Generation) Pin() error {
	if g == nil || g.mgr == nil {
		return ErrClosed
	}
	m := g.mgr
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.closing {
		return ErrClosed
	}
	if m.readers >= bounds.MaxReaders {
		return ErrBusy
	}
	if g.slot < 0 || g.slot >= len(m.slots) || m.slots[g.slot].State != SlotCommitted || hex.EncodeToString(m.slots[g.slot].ID[:]) != g.ID {
		return ErrCorrupt
	}
	return m.pinLocked(context.Background(), g)
}

// Unpin releases one reader pin acquired by Pin.
func (g *Generation) Unpin() error {
	if g == nil || g.mgr == nil {
		return nil
	}
	m := g.mgr
	m.mu.Lock()
	defer m.mu.Unlock()
	if g.pins <= 0 {
		return nil
	}
	if g.slot < 0 || g.slot >= len(m.slots) || hex.EncodeToString(m.slots[g.slot].ID[:]) != g.ID {
		return ErrCorrupt
	}
	prior := m.slots[g.slot]
	g.pins--
	if m.slots[g.slot].ReadCount > 0 {
		m.slots[g.slot].ReadCount--
	}
	if m.readers > 0 {
		m.readers--
	}
	if err := m.persistSlots(); err != nil {
		m.slots[g.slot] = prior
		g.pins++
		m.readers++
		return err
	}
	return nil
}

// pinGeneration pins a committed generation by id without decoding objects.
func (m *Manager) pinGeneration(ctx context.Context, id string) (*Generation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, ErrUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.closing {
		return nil, ErrClosed
	}
	if m.readers >= bounds.MaxReaders {
		return nil, ErrBusy
	}
	for i, s := range m.slots {
		if s.State != SlotCommitted {
			continue
		}
		if hex.EncodeToString(s.ID[:]) != id {
			continue
		}
		g := &Generation{ID: id, PackSize: int64(s.PackSize), IndexSize: int64(s.IndexSize), MetaSize: int64(s.MetaSize), mgr: m, slot: i}
		if err := m.pinLocked(ctx, g); err != nil {
			return nil, err
		}
		return g, nil
	}
	return nil, ErrUnavailable
}

// lookupWarm finds a committed generation matching grant fingerprint and namespace.
// Content return is only via Acquire after a fresh ResolveGrant.
func (m *Manager) lookupWarm(ctx context.Context, grantFP, ns string) (*Generation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.closing {
		return nil, ErrClosed
	}
	if m.readers >= bounds.MaxReaders {
		return nil, ErrBusy
	}
	for i, s := range m.slots {
		if s.State != SlotCommitted {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := hex.EncodeToString(s.ID[:])
		meta, err := m.readManifestLocked(ctx, id, &s)
		if err != nil {
			return nil, preserveCtx(ctx, err)
		}
		if meta.GrantFP != grantFP || meta.NS != ns {
			continue
		}
		g := &Generation{ID: id, PackSize: int64(s.PackSize), IndexSize: int64(s.IndexSize), MetaSize: int64(s.MetaSize), mgr: m, slot: i}
		if err := m.pinLocked(ctx, g); err != nil {
			return nil, err
		}
		_, objs, err := m.readGenerationLocked(ctx, id, &s)
		if err != nil {
			m.readers--
			g.pins--
			m.slots[i].ReadCount--
			if pe := m.persistSlots(); pe != nil {
				m.slots[i].State = SlotAmbiguous
				return nil, errors.Join(preserveCtx(ctx, err), pe)
			}
			return nil, preserveCtx(ctx, err)
		}
		g.Objects = objs
		return g, nil
	}
	return nil, ErrUnavailable
}
func (m *Manager) pinLocked(ctx context.Context, g *Generation) error {
	prior := m.slots[g.slot]
	m.readers++
	m.slots[g.slot].ReadCount++
	g.pins++
	if err := m.persistSlotsContext(ctx); err != nil {
		m.slots[g.slot] = prior
		m.readers--
		g.pins--
		return err
	}
	return nil
}

func (m *Manager) readGenerationLocked(ctx context.Context, idHex string, slot *Slot) (genManifest, map[plumbing.Hash]pack.Object, error) {
	gfd, err := m.openPrivateDir(int(m.root.Fd()), gensDir)
	if err != nil {
		return genManifest{}, nil, preserveCtx(ctx, ErrCorrupt)
	}
	defer syscall.Close(gfd)
	gst, err := fstat(gfd)
	if err != nil || checkDir(&gst, m.rootDev) != nil {
		return genManifest{}, nil, preserveCtx(ctx, ErrCorrupt)
	}
	dirfd, err := openatDir(gfd, idHex)
	if err != nil {
		return genManifest{}, nil, preserveCtx(ctx, ErrCorrupt)
	}
	defer syscall.Close(dirfd)
	dst, err := fstat(dirfd)
	if err != nil || checkDir(&dst, m.rootDev) != nil {
		return genManifest{}, nil, preserveCtx(ctx, ErrCorrupt)
	}
	metaBytes, err := readAllAtFDContext(ctx, dirfd, "manifest.json", bounds.MaxMetaBytes, m.rootDev)
	if err != nil {
		return genManifest{}, nil, preserveCtx(ctx, ErrCorrupt)
	}
	var meta genManifest
	if err := decodeManifest(ctx, metaBytes, &meta); err != nil {
		return genManifest{}, nil, preserveCtx(ctx, ErrCorrupt)
	}
	if err := validateManifest(meta, metaBytes, slot); err != nil {
		return genManifest{}, nil, err
	}
	if meta.Head == "" || meta.Base == "" || meta.Start == "" || meta.Objects == nil {
		return genManifest{}, nil, preserveCtx(ctx, ErrCorrupt)
	}
	packBytes, err := readAllAtFDContext(ctx, dirfd, "pack.pack", bounds.MaxPackBytes, m.rootDev)
	if err != nil {
		return genManifest{}, nil, preserveCtx(ctx, ErrCorrupt)
	}
	idxBytes, err := readAllAtFDContext(ctx, dirfd, "pack.idx", bounds.MaxIndexBytes, m.rootDev)
	if err != nil {
		return genManifest{}, nil, preserveCtx(ctx, ErrCorrupt)
	}
	if slot != nil {
		if uint64(len(packBytes)) != slot.PackSize || uint64(len(idxBytes)) != slot.IndexSize || uint64(len(metaBytes)) != slot.MetaSize {
			return genManifest{}, nil, preserveCtx(ctx, ErrCorrupt)
		}
	}
	ip, err := pack.DecodeIndexed(ctx, packBytes)
	objs := ip.Objects
	if err != nil {
		return genManifest{}, nil, preserveCtx(ctx, ErrCorrupt)
	}
	expectedIdx, err := pack.BuildIndexV2(ctx, ip)
	if err != nil {
		return genManifest{}, nil, preserveCtx(ctx, err)
	}
	if !bytes.Equal(expectedIdx, idxBytes) {
		return genManifest{}, nil, ErrIntegrity
	}
	if len(meta.Objects) == 0 || len(meta.Objects) != len(objs) {
		return genManifest{}, nil, preserveCtx(ctx, ErrCorrupt)
	}
	for h, typ := range meta.Objects {
		if err := ctx.Err(); err != nil {
			return genManifest{}, nil, err
		}
		hh, err := ParseSHA(h)
		if err != nil {
			return genManifest{}, nil, ErrCorrupt
		}
		o, ok := objs[hh]
		if !ok || o.Type != typ || o.Hash != hh {
			return genManifest{}, nil, preserveCtx(ctx, ErrCorrupt)
		}
	}
	for h, o := range objs {
		typ, ok := meta.Objects[h.String()]
		if !ok || typ != o.Type {
			return genManifest{}, nil, preserveCtx(ctx, ErrCorrupt)
		}
	}
	return meta, objs, nil
}

func readAllAtFD(dirfd int, name string, max int64, rootDev uint64) ([]byte, error) {
	return readAllAtFDContext(context.Background(), dirfd, name, max, rootDev)
}
func readAllAtFDContext(ctx context.Context, dirfd int, name string, max int64, rootDev uint64) ([]byte, error) {
	fd, err := openatNoFollow(dirfd, name, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	st, err := fstat(fd)
	if err != nil {
		return nil, err
	}
	if err := checkRegular(&st, rootDev); err != nil {
		return nil, err
	}
	if st.Size > max {
		return nil, ErrLimit
	}
	b, err := io.ReadAll(io.LimitReader(contextFileReader{ctx, f}, max+1))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	after, err := fstat(fd)
	if err != nil || checkRegular(&after, rootDev) != nil || st.Ino != after.Ino || st.Dev != after.Dev || st.Size != after.Size || int64(len(b)) != st.Size {
		return nil, ErrIntegrity
	}
	if int64(len(b)) > max {
		return nil, ErrLimit
	}
	return b, nil
}

type contextFileReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextFileReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// Public direct publication joins the same single-acquisition lifetime contract.
func (m *Manager) PublishGeneration(ctx context.Context, fp, ns string, head, base, start plumbing.Hash, ip pack.IndexedPack) (*Generation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.closed || m.closing {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	if m.acqBusy {
		m.mu.Unlock()
		return nil, ErrBusy
	}
	m.acqBusy = true
	m.wg.Add(1)
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.acqBusy = false; m.mu.Unlock(); m.wg.Done() }()
	op, cancel := context.WithTimeout(ctx, bounds.Timeout)
	defer cancel()
	stop := context.AfterFunc(m.workCtx, cancel)
	defer stop()
	return m.publishGeneration(op, fp, ns, head, base, start, ip)
}
func (m *Manager) openPrivateDir(parent int, name string) (int, error) {
	fd, err := openatDir(parent, name)
	if err != nil {
		return -1, ErrPath
	}
	st, err := fstat(fd)
	if err != nil || checkDir(&st, m.rootDev) != nil {
		syscall.Close(fd)
		return -1, ErrPath
	}
	return fd, nil
}
func validateManifest(meta genManifest, data []byte, s *Slot) error {
	if len(meta.GrantFP) == 0 || len(meta.GrantFP) > 256 || len(meta.NS) == 0 || len(meta.NS) > 256 || len(meta.Objects) == 0 || len(meta.Objects) > bounds.MaxObjects {
		return ErrCorrupt
	}
	for _, v := range []string{meta.Head, meta.Base, meta.Start} {
		h, e := ParseSHA(v)
		if e != nil || h == plumbing.ZeroHash || h.String() != v {
			return ErrCorrupt
		}
	}
	if s != nil && (uint64(len(data)) != s.MetaSize || sha256.Sum256(data) != s.ManifestDigest) {
		return ErrIntegrity
	}
	return nil
}
func (m *Manager) readManifestLocked(ctx context.Context, id string, s *Slot) (genManifest, error) {
	parent, err := m.openPrivateDir(int(m.root.Fd()), gensDir)
	if err != nil {
		return genManifest{}, err
	}
	defer syscall.Close(parent)
	fd, err := m.openPrivateDir(parent, id)
	if err != nil {
		return genManifest{}, err
	}
	defer syscall.Close(fd)
	data, err := readAllAtFDContext(ctx, fd, "manifest.json", bounds.MaxMetaBytes, m.rootDev)
	if err != nil {
		return genManifest{}, err
	}
	var meta genManifest
	if err := decodeManifest(ctx, data, &meta); err != nil {
		return meta, ErrCorrupt
	}
	return meta, validateManifest(meta, data, s)
}
func (m *Manager) inventoryGenerationsLocked() error {
	parent, err := m.openPrivateDir(int(m.root.Fd()), gensDir)
	if err != nil {
		return err
	}
	defer syscall.Close(parent)
	names, err := listNamesAt(parent, bounds.MaxGenerations)
	if err != nil {
		return err
	}
	owned := make(map[string]int, len(m.slots))
	for i, s := range m.slots {
		if s.State != SlotEmpty {
			owned[hex.EncodeToString(s.ID[:])] = i
		}
	}
	present := make(map[string]bool, len(names))
	for _, name := range names {
		i, ok := owned[name]
		if !ok {
			return ErrCorrupt
		}
		present[name] = true
		fd, err := m.openPrivateDir(parent, name)
		if err != nil {
			return err
		}
		err = m.inventoryGenerationFiles(fd, &m.slots[i])
		syscall.Close(fd)
		if err != nil {
			return err
		}
	}
	for name, i := range owned {
		if m.slots[i].State == SlotCommitted && !present[name] {
			return ErrCorrupt
		}
	}
	return nil
}
func (m *Manager) inventoryGenerationFiles(fd int, s *Slot) error {
	names, err := listNamesAt(fd, 3)
	if err != nil {
		return err
	}
	max := map[string]int64{"pack.pack": bounds.MaxPackBytes, "pack.idx": bounds.MaxIndexBytes, "manifest.json": bounds.MaxMetaBytes}
	expected := map[string]uint64{"pack.pack": s.PackSize, "pack.idx": s.IndexSize, "manifest.json": s.MetaSize}
	var total uint64
	if s.State == SlotCommitted && len(names) != 3 {
		return ErrCorrupt
	}
	for _, name := range names {
		limit, ok := max[name]
		if !ok {
			return ErrCorrupt
		}
		child, e := openatNoFollow(fd, name, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
		if e != nil {
			return ErrPath
		}
		st, e := fstat(child)
		syscall.Close(child)
		if e != nil || checkRegular(&st, m.rootDev) != nil || st.Size < 0 || st.Size > limit {
			return ErrCorrupt
		}
		if s.State == SlotCommitted && uint64(st.Size) != expected[name] {
			return ErrIntegrity
		}
		total += uint64(st.Size)
	}
	if total > s.Charge {
		return ErrQuota
	}
	return nil
}

// EvictGeneration removes only a positively inventoried, ledger-owned, unpinned
// generation. A failure leaves durable deleting/ambiguous charge for recovery.
func (m *Manager) EvictGeneration(ctx context.Context, id string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	if m.closed || m.closing {
		m.mu.Unlock()
		return ErrClosed
	}
	if m.acqBusy {
		m.mu.Unlock()
		return ErrBusy
	}
	m.acqBusy = true
	m.wg.Add(1)
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.acqBusy = false; m.mu.Unlock(); m.wg.Done() }()
	op, cancel := context.WithTimeout(ctx, bounds.Timeout)
	defer cancel()
	stop := context.AfterFunc(m.workCtx, cancel)
	defer stop()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := op.Err(); err != nil {
		return err
	}
	for i, s := range m.slots {
		if s.State != SlotEmpty && hex.EncodeToString(s.ID[:]) == id {
			return m.deleteGenerationLocked(op, i)
		}
	}
	return ErrUnavailable
}

func (m *Manager) deleteGenerationLocked(ctx context.Context, i int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.confirmLockIdentity(); err != nil {
		return err
	}
	prior := m.slots[i]
	if prior.ReadCount != 0 {
		return ErrPinned
	}
	parent, err := m.openPrivateDir(int(m.root.Fd()), gensDir)
	if err != nil {
		return err
	}
	defer syscall.Close(parent)
	id := hex.EncodeToString(prior.ID[:])
	fd, err := m.openPrivateDir(parent, id)
	if err != nil {
		var missing unix.Stat_t
		if e := unix.Fstatat(parent, id, &missing, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(e, syscall.ENOENT) {
			return err
		}
		if prior.State == SlotCommitted {
			return ErrCorrupt
		}
		if err := syscall.Fsync(parent); err != nil {
			return err
		}
		m.slots[i] = Slot{}
		if err := m.persistSlots(); err != nil {
			prior.State = SlotAmbiguous
			m.slots[i] = prior
			return err
		}
		return nil
	}
	defer syscall.Close(fd)
	if err := m.inventoryGenerationFiles(fd, &prior); err != nil {
		return err
	}
	m.slots[i].State = SlotDeleting
	if err := m.persistSlots(); err != nil {
		m.slots[i].State = SlotAmbiguous
		return err
	}
	fail := func(err error) error { m.slots[i].State = SlotAmbiguous; _ = m.persistSlots(); return err }
	if m.fault != nil && m.fault.BeforeDelete != nil {
		if err := m.fault.BeforeDelete(); err != nil {
			return fail(err)
		}
	}
	names, err := listNamesAt(fd, 3)
	if err != nil {
		return fail(err)
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		child, err := openatNoFollow(fd, name, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return fail(err)
		}
		st, err := fstat(child)
		if err != nil || checkRegular(&st, m.rootDev) != nil {
			syscall.Close(child)
			return fail(ErrPath)
		}
		var current unix.Stat_t
		err = unix.Fstatat(fd, name, &current, unix.AT_SYMLINK_NOFOLLOW)
		if err != nil || uint64(current.Ino) != uint64(st.Ino) || uint64(current.Dev) != uint64(st.Dev) {
			syscall.Close(child)
			return fail(ErrIntegrity)
		}
		err = unlinkat(fd, name, 0)
		syscall.Close(child)
		if err != nil {
			return fail(err)
		}
	}
	if err := syscall.Fsync(fd); err != nil {
		return fail(err)
	}
	held, err := fstat(fd)
	var current unix.Stat_t
	if err != nil {
		return fail(err)
	}
	if err = unix.Fstatat(parent, id, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil || uint64(current.Ino) != uint64(held.Ino) || uint64(current.Dev) != uint64(held.Dev) {
		return fail(ErrIntegrity)
	}
	if err := unlinkat(parent, id, unix.AT_REMOVEDIR); err != nil {
		return fail(err)
	}
	if err := syscall.Fsync(parent); err != nil {
		return fail(err)
	}
	var check unix.Stat_t
	if err := unix.Fstatat(parent, id, &check, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(err, syscall.ENOENT) {
		return fail(ErrIntegrity)
	}
	m.slots[i] = Slot{}
	if err := m.persistSlots(); err != nil {
		prior.State = SlotAmbiguous
		m.slots[i] = prior
		return err
	}
	return nil
}

// The byte cap alone does not bound map entries: admit each key before inserting
// it. Reject duplicate/unknown fields and duplicate objects rather than silently
// accepting JSON overwrite semantics.
func decodeManifest(ctx context.Context, data []byte, out *genManifest) error {
	if int64(len(data)) > bounds.MaxMetaBytes {
		return ErrLimit
	}
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return ErrCorrupt
	}
	seen := make(map[string]bool, 6)
	for d.More() {
		if err := ctx.Err(); err != nil {
			return err
		}
		token, err = d.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return ErrCorrupt
		}
		seen[name] = true
		switch name {
		case "objects":
			token, err = d.Token()
			if err != nil || token != json.Delim('{') {
				return ErrCorrupt
			}
			out.Objects = make(map[string]string)
			for d.More() {
				if err := ctx.Err(); err != nil {
					return err
				}
				if len(out.Objects) >= bounds.MaxObjects {
					return ErrLimit
				}
				token, err = d.Token()
				key, ok := token.(string)
				if err != nil || !ok || len(key) != 40 {
					return ErrCorrupt
				}
				h, e := ParseSHA(key)
				if e != nil || h.String() != key {
					return ErrCorrupt
				}
				if _, exists := out.Objects[key]; exists {
					return ErrCorrupt
				}
				token, err = d.Token()
				typ, ok := token.(string)
				if err != nil || !ok || (typ != "commit" && typ != "tree" && typ != "blob" && typ != "tag") {
					return ErrCorrupt
				}
				out.Objects[key] = typ
			}
			token, err = d.Token()
			if err != nil || token != json.Delim('}') {
				return ErrCorrupt
			}
		case "grant_fp", "ns", "head", "base", "start":
			token, err = d.Token()
			value, ok := token.(string)
			if err != nil || !ok || len(value) > 256 {
				return ErrCorrupt
			}
			switch name {
			case "grant_fp":
				out.GrantFP = value
			case "ns":
				out.NS = value
			case "head":
				out.Head = value
			case "base":
				out.Base = value
			case "start":
				out.Start = value
			}
		default:
			return ErrCorrupt
		}
	}
	token, err = d.Token()
	if err != nil || token != json.Delim('}') || len(seen) != 6 {
		return ErrCorrupt
	}
	if _, err = d.Token(); !errors.Is(err, io.EOF) {
		return ErrCorrupt
	}
	return ctx.Err()
}
