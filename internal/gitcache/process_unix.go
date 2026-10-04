//go:build linux || darwin

package gitcache

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const (
	LimitAS     uint64 = 8 << 30
	LimitCPU    uint64 = 30
	LimitNOFILE uint64 = 64
	ackWait            = 2 * time.Second
)

// ApplyLimits sets soft=hard FSIZE, AS, CPU, CORE, and NOFILE, then reads
// them back. Any failure returns ErrLimit and the caller must not Exec.
func ApplyLimits(fsize uint64) error {
	want := []struct {
		res int
		cur uint64
	}{
		{syscall.RLIMIT_FSIZE, fsize},
		{syscall.RLIMIT_AS, LimitAS},
		{syscall.RLIMIT_CPU, LimitCPU},
		{syscall.RLIMIT_CORE, 0},
		{syscall.RLIMIT_NOFILE, LimitNOFILE},
	}
	for _, w := range want {
		lim := syscall.Rlimit{Cur: w.cur, Max: w.cur}
		if err := limitSet(w.res, &lim); err != nil {
			return ErrLimit
		}
		var got syscall.Rlimit
		if err := limitGet(w.res, &got); err != nil {
			return ErrLimit
		}
		if got.Cur != w.cur || got.Max != w.cur {
			if asNote != nil {
				asNote("get-mismatch")
			}
			return ErrLimit
		}
	}
	// getrlimit agreement is not the AS gate. The child must prove the
	// mapping before it is allowed to ACK or Exec.
	if err := asGate(LimitAS); err != nil {
		if asNote != nil {
			asNote("map")
		}
		return ErrLimit
	}
	return nil
}

var limitSet = func(res int, lim *syscall.Rlimit) error {
	return syscall.Setrlimit(res, lim)
}

var limitGet = func(res int, lim *syscall.Rlimit) error {
	return syscall.Getrlimit(res, lim)
}

var asGate = func(limit uint64) error { return enforceVirtualAS(limit) }

// asNote is nil in production. A test helper may set it to record which
// ApplyLimits check ran, without changing the limit values.
var asNote func(string)

func installASSeam(kind string) {
	saved := map[int]syscall.Rlimit{}
	limitSet = func(res int, lim *syscall.Rlimit) error {
		saved[res] = *lim
		return nil
	}
	limitGet = func(res int, lim *syscall.Rlimit) error {
		if kind == "get" && res == syscall.RLIMIT_AS {
			lim.Cur, lim.Max = 1, 1
			return nil
		}
		got, ok := saved[res]
		if !ok {
			return errors.New("unset")
		}
		*lim = got
		return nil
	}
	asGate = func(uint64) error {
		switch kind {
		case "over":
			return errASIneffective
		case "under":
			return ErrLimit
		case "probe":
			return errors.New("probe")
		default:
			return nil
		}
	}
	asNote = func(string) {
		_ = os.WriteFile("as-gate", []byte(kind), 0600)
	}
}

// enforceVirtualAS is the AS gate used by ApplyLimits. Tests may replace it
// only inside a helper process that has not reached Exec.
var enforceVirtualAS = EnforceVirtualAS

// ReadToken consumes a bounded frame only after RLIMIT_CORE is confirmed 0.
// A failed check returns before any read.
func ReadToken(r io.Reader) ([]byte, error) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_CORE, &lim); err != nil || lim.Cur != 0 {
		return nil, ErrLimit
	}
	var nbuf [4]byte
	if _, err := io.ReadFull(r, nbuf[:]); err != nil {
		return nil, ErrCorrupt
	}
	n := int(nbuf[0]) | int(nbuf[1])<<8 | int(nbuf[2])<<16 | int(nbuf[3])<<24
	if n < 0 || n > 256 {
		return nil, ErrQuota
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, ErrCorrupt
	}
	return buf, nil
}

// QuiesceGroup is positive quiescence for one created process group.
// Leader Wait alone is not enough: after SIGKILL and Wait, kill(-pgid, 0)
// must return ESRCH. A setsid descendant is outside this proof; audited
// argv must not call setsid. An unknown result fails closed.
// awaitClose is the group closure used by a role. Tests may replace it to
// observe an unknown Wait without changing production QuiesceGroup.
var awaitClose = QuiesceGroup

func QuiesceGroup(pgid int, wait func() error) error {
	if pgid <= 1 || wait == nil {
		return ErrNotQuiescent
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	done := make(chan error, 1)
	go func() { done <- wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-time.After(2 * time.Second):
		return ErrNotQuiescent
	}
	if !exitReceipt(waitErr) {
		return ErrNotQuiescent
	}
	if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.ESRCH) {
		return ErrNotQuiescent
	}
	return nil
}

// exitReceipt accepts one owned Wait result: a natural exit, or a signal.
// A nil function result is not a receipt. Callers that discarded the status
// and returned nil are rejected here; exit 0 is observed via ProcessState.
func exitReceipt(waitErr error) bool {
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		return false
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return false
	}
	return ws.Exited() || ws.Signaled()
}

// reapOwned waits for the command that LaunchGit started. A natural exit is
// the receipt. SIGKILL is only the bound when that wait does not finish.
func reapOwned(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 1 || cmd.ProcessState != nil {
		return ErrNotQuiescent
	}
	pid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-time.After(ackWait):
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		select {
		case waitErr = <-done:
		case <-time.After(ackWait):
			return ErrNotQuiescent
		}
	}
	if err := waitDrainsBounded(cmd, ackWait); err != nil {
		return ErrNotQuiescent
	}
	if cmd.ProcessState == nil {
		return ErrNotQuiescent
	}
	if waitErr == nil && !cmd.ProcessState.Success() {
		return ErrNotQuiescent
	}
	if waitErr != nil && !exitReceipt(waitErr) {
		return ErrNotQuiescent
	}
	if err := syscall.Kill(-pid, 0); !errors.Is(err, syscall.ESRCH) {
		return ErrNotQuiescent
	}
	if launchReceipt != nil {
		launchReceipt(cmd.ProcessState)
	}
	return nil
}

// Role selects the FSIZE cap applied before Exec.
type Role string

const (
	RoleIndex   Role = "index"
	RoleVersion Role = "version"
	RoleRevList Role = "rev-list"
	RoleCatFile Role = "cat-file"
)

func roleFSIZE(role Role) (uint64, error) {
	switch role {
	case RoleIndex:
		return IndexMax, nil
	case RoleVersion, RoleRevList, RoleCatFile:
		return 0, nil
	default:
		return 0, ErrUnsupported
	}
}

func roleArgv(role Role, git, tip string) ([]string, error) {
	switch role {
	case RoleIndex:
		if tip != "" {
			return nil, ErrPath
		}
		return IndexArgv(git)
	case RoleVersion:
		if tip != "" {
			return nil, ErrPath
		}
		return VersionArgv(git)
	case RoleRevList:
		return RevListArgv(git, tip)
	case RoleCatFile:
		if tip != "" {
			return nil, ErrPath
		}
		return CatFileArgv(git)
	default:
		return nil, ErrUnsupported
	}
}

// LaunchGit starts one audited Git role in a new process group.
// Limits are applied in the child before Exec. A failed limit writes "limit"
// on the ack pipe and does not Exec. The caller must Quiesce the returned
// process; discarding it without Wait leaves the reservation charged.
// gitLaunchArg is the helper argv. Production uses __gitcache_git.
// A test may select __gitcache_git_failas to force the AS gate closed.
var gitLaunchArg = "__gitcache_git"

// launchCapture is a test seam. Production leaves it nil.
// Writes are synchronized, and done is closed after the stdout drain returns.
var launchCapture *launchBuf

type launchBuf struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	once sync.Once
	done chan struct{}
}

func newLaunchBuf() *launchBuf {
	return &launchBuf{done: make(chan struct{})}
}

func (b *launchBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *launchBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *launchBuf) finish() {
	b.once.Do(func() { close(b.done) })
}

type drainPair struct {
	out chan struct{}
	err chan struct{}
}

var launchDrains sync.Map

// launchReaped, when set by a test, observes a group that reached ESRCH.
var launchReaped func(int)

// launchReceipt, when set by a test, observes the owned ProcessState.
var launchReceipt func(*os.ProcessState)

// launchClosure, nil in production, lets a test deny closure after the
// owned receipt. It does not change the failure lifecycle.
var launchClosure func(*exec.Cmd) error

func waitDrains(cmd *exec.Cmd) {
	_ = waitDrainsBounded(cmd, 0)
}

func waitDrainsBounded(cmd *exec.Cmd, bound time.Duration) error {
	v, ok := launchDrains.LoadAndDelete(cmd)
	if !ok {
		return nil
	}
	d := v.(drainPair)
	wait := func(ch chan struct{}) error {
		if bound <= 0 {
			<-ch
			return nil
		}
		select {
		case <-ch:
			return nil
		case <-time.After(bound):
			return ErrNotQuiescent
		}
	}
	if err := wait(d.out); err != nil {
		return err
	}
	return wait(d.err)
}

// reapLaunch collects the owned command. A natural exit closes the pipes.
// A helper that ignores the deadline is killed only after that bound.
func reapLaunch(cmd *exec.Cmd) error {
	if err := reapOwned(cmd); err != nil {
		return err
	}
	if launchClosure != nil {
		if err := launchClosure(cmd); err != nil {
			return err
		}
	}
	if launchReaped != nil && cmd.Process != nil {
		launchReaped(cmd.Process.Pid)
	}
	return nil
}

func LaunchGit(role Role, helper, git, genPath, tip string, root *os.File, lock *os.File) (*exec.Cmd, error) {
	fsize, err := roleFSIZE(role)
	if err != nil {
		return nil, err
	}
	argv, err := roleArgv(role, git, tip)
	if err != nil {
		return nil, err
	}
	if genPath == "" || genPath[0] == '/' || helper == "" || helper[0] != '/' {
		return nil, ErrPath
	}
	reqR, reqW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	ackR, ackW, err := os.Pipe()
	if err != nil {
		reqR.Close()
		reqW.Close()
		return nil, err
	}
	cmd := exec.Command(helper, gitLaunchArg)
	cmd.Env = GitEnv()
	cmd.Stdin = nil
	cmd.ExtraFiles = []*os.File{root, lock, reqR, ackW}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		closePipes(reqR, reqW, ackR, ackW)
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		closePipes(reqR, reqW, ackR, ackW)
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		closePipes(reqR, reqW, ackR, ackW)
		return nil, err
	}
	reqR.Close()
	ackW.Close()
	outDone := make(chan struct{})
	errDone := make(chan struct{})
	launchDrains.Store(cmd, drainPair{out: outDone, err: errDone})
	go func() {
		drainLimit(stderr, cmd.Process.Pid, 64<<10)
		close(errDone)
	}()
	outLimit := 64 << 10
	if role == RoleCatFile {
		outLimit = CatFileStdoutMax
	}
	var out io.ReadCloser = stdout
	capBuf := launchCapture
	if capBuf != nil {
		out = closeReader{Reader: io.TeeReader(stdout, capBuf), Closer: stdout}
	}
	go func() {
		drainLimit(out, cmd.Process.Pid, outLimit)
		if capBuf != nil {
			capBuf.finish()
		}
		close(outDone)
	}()
	_ = fsize
	_ = argv // rebuilt in the child from the role, the absolute git path, and the tip
	payload := []byte(string(role) + "\x00" + git + "\x00" + genPath + "\x00" + tip)
	if err := writeFullDeadline(reqW, payload, ackWait); err != nil {
		_ = reqW.Close()
		ackR.Close()
		if qerr := reapLaunch(cmd); qerr != nil {
			return cmd, qerr
		}
		return nil, err
	}
	_ = reqW.Close()
	ack := make([]byte, 8)
	if err := readFullDeadline(ackR, ack, ackWait); err != nil {
		ackR.Close()
		if qerr := reapLaunch(cmd); qerr != nil {
			return cmd, qerr
		}
		return nil, ErrLimit
	}
	ackR.Close()
	if string(ack) != "OKAY\x00\x00\x00\x00" {
		if qerr := reapLaunch(cmd); qerr != nil {
			return cmd, qerr
		}
		return nil, ErrLimit
	}
	return cmd, nil
}

type closeReader struct {
	io.Reader
	io.Closer
}

func closePipes(fs ...*os.File) {
	for _, f := range fs {
		if f != nil {
			f.Close()
		}
	}
}

func heldRoot(fd int) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return 0, err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFDIR || st.Mode&0777 != 0700 || observedUID(&st) != uint32(os.Geteuid()) {
		return 0, ErrPath
	}
	return uint64(st.Dev), nil
}

func runGitLauncher() int {
	rootfd, reqfd, ackfd := 3, 5, 6
	// fd 4 is the root lock and must stay open across Exec.
	fail := func() int {
		_, _ = syscall.Write(ackfd, []byte("FAIL\x00\x00\x00\x00"))
		syscall.Close(ackfd)
		return 2
	}
	buf := make([]byte, 4096)
	n, err := syscall.Read(reqfd, buf)
	syscall.Close(reqfd)
	if err != nil || n < 4 {
		return fail()
	}
	parts := bytes.Split(buf[:n], []byte{0})
	if len(parts) != 4 {
		return fail()
	}
	role := Role(parts[0])
	git := string(parts[1])
	gen := string(parts[2])
	tip := string(parts[3])
	fsize, rerr := roleFSIZE(role)
	argv, aerr := roleArgv(role, git, tip)
	if rerr != nil || aerr != nil || !relativeGen(gen) {
		return fail()
	}
	rootDev, err := heldRoot(rootfd)
	if err != nil {
		return fail()
	}
	if err := syscall.Fchdir(rootfd); err != nil {
		return fail()
	}
	syscall.Close(rootfd)
	if err := enterGen(gen, rootDev); err != nil {
		return fail()
	}
	if err := ApplyLimits(fsize); err != nil {
		_, _ = syscall.Write(ackfd, []byte("LIMIT\x00\x00\x00"))
		syscall.Close(ackfd)
		return 3
	}
	if _, err := syscall.Write(ackfd, []byte("OKAY\x00\x00\x00\x00")); err != nil {
		syscall.Close(ackfd)
		return 2
	}
	syscall.Close(ackfd)
	devnull, err := syscall.Open("/dev/null", syscall.O_RDONLY, 0)
	if err == nil && devnull != 0 {
		_ = dupTo(devnull, 0)
		syscall.Close(devnull)
	}
	_ = syscall.Exec(argv[0], argv, GitEnv())
	return 127
}

func enterGen(gen string, rootDev uint64) error {
	// Caller is at the cache root. Walk without following links.
	parts, err := splitRel(gen)
	if err != nil || len(parts) == 0 {
		return ErrPath
	}
	fd, err := syscall.Open(".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	for _, p := range parts {
		next, err := syscall.Open(p, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
		syscall.Close(fd)
		if err != nil {
			return err
		}
		var st syscall.Stat_t
		if err := syscall.Fstat(next, &st); err != nil || uint64(st.Dev) != rootDev || st.Mode&syscall.S_IFMT != syscall.S_IFDIR || st.Mode&0777 != 0700 || observedUID(&st) != uint32(os.Geteuid()) {
			syscall.Close(next)
			return ErrPath
		}
		if err := syscall.Fchdir(next); err != nil {
			syscall.Close(next)
			return err
		}
		fd = next
	}
	syscall.Close(fd)
	return nil
}

func relativeGen(gen string) bool {
	_, err := splitRel(gen)
	return err == nil
}

// EnforceFSIZE writes one byte past a fresh file-size limit in this process.
// It is the native evidence that FSIZE is enforced, not merely stored.
func EnforceFSIZE(limit uint64) error {
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		return ErrLimit
	}
	defer syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old)
	// Lower only the soft limit so the hard limit can be restored in-process.
	// The Git launcher still sets soft=hard before Exec.
	if old.Max < limit {
		return ErrLimit
	}
	lim := syscall.Rlimit{Cur: limit, Max: old.Max}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lim); err != nil {
		return ErrLimit
	}
	var got syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &got); err != nil || got.Cur != limit {
		return ErrLimit
	}
	f, err := os.CreateTemp("", "gitcache-fsize")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	defer f.Close()
	buf := make([]byte, limit+1)
	_, werr := f.Write(buf)
	if werr == nil {
		return ErrLimit
	}
	if !errors.Is(werr, syscall.EFBIG) && !isFileTooLarge(werr) {
		return ErrLimit
	}
	return nil
}

func isFileTooLarge(err error) bool {
	return err != nil && (errors.Is(err, syscall.EFBIG) || bytes.Contains([]byte(err.Error()), []byte("file too large")))
}

// errASIneffective means a virtual mapping larger than the cap succeeded.
var errASIneffective = errors.New("gitcache: as ineffective")

// EnforceVirtualAS maps anonymous PROT_NONE address space.
// A 1GiB map must succeed. A map of limit+4096 must fail with ENOMEM.
// The pages are never written, so this does not allocate physical RAM.
// getrlimit agreement alone is not success.
func EnforceVirtualAS(limit uint64) error {
	const under = 1 << 30
	if limit <= under+4096 {
		return ErrLimit
	}
	small, err := mapAnon(under)
	if err != nil {
		return ErrLimit
	}
	if err := unmapAnon(small, under); err != nil {
		return ErrLimit
	}
	over := uintptr(limit + 4096)
	huge, err := mapAnon(over)
	if err == nil {
		_ = unmapAnon(huge, over)
		return errASIneffective
	}
	if errors.Is(err, syscall.ENOMEM) {
		return nil
	}
	return ErrLimit
}

// EnforceNOFILE opens files until the kernel returns EMFILE.
func EnforceNOFILE(limit uint64) (int, error) {
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &old); err != nil {
		return 0, ErrLimit
	}
	defer syscall.Setrlimit(syscall.RLIMIT_NOFILE, &old)
	if old.Max < limit {
		return 0, ErrLimit
	}
	lim := syscall.Rlimit{Cur: limit, Max: old.Max}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0, ErrLimit
	}
	var files []*os.File
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	for i := 0; i < int(limit)+8; i++ {
		f, err := os.Open("/dev/null")
		if err != nil {
			if errors.Is(err, syscall.EMFILE) {
				return len(files), nil
			}
			return len(files), err
		}
		files = append(files, f)
	}
	return len(files), ErrLimit
}
