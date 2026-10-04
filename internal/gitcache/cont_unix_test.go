//go:build linux || darwin

package gitcache

import (
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestContDestOnlyKeepsReservation(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	dest := strings.Repeat("ab", 16)
	r, err := Open(root, h, QuotaMin+Reserve)
	if err != nil {
		t.Fatal(err)
	}
	id, err := r.Reserve("dom", "tip")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Activate(id); err != nil {
		t.Fatal(err)
	}
	if err := r.WritePack(id, []byte{1, 2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteMeta(id, "HEAD", []byte(AllowedHEAD)); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteMeta(id, "config", []byte(AllowedConfig)); err != nil {
		t.Fatal(err)
	}
	if err := fillProof(r, id); err != nil {
		t.Fatal(err)
	}
	if err := r.Quiesce(id); err != nil || r.Verify(id) != nil || r.Commit(id, dest) != nil {
		t.Fatal(err)
	}
	r.CrashCut()
	payload := filepath.Join(root, dest, "config")
	before, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	rewriteSlots(t, root, func(slots []Slot) {
		slots[0].State = stateVerified
	})
	r, err = Open(root, h, QuotaMin+Reserve)
	if err != nil {
		t.Fatalf("dest-only crash denied the root: %v", err)
	}
	st, frozen, _, err := r.State(id)
	if err != nil || !frozen || st == stateCommitted || st == stateEmpty {
		t.Fatalf("dest-only state %d frozen %v %v", st, frozen, err)
	}
	used, err := r.Used()
	if err != nil || used < Reserve {
		t.Fatalf("dest-only charge %d %v", used, err)
	}
	if _, err := r.Reserve("other", "tip"); err != nil {
		t.Fatalf("valid slot blocked by dest-only crash: %v", err)
	}
	r.CrashCut()
	if b, err := os.ReadFile(payload); err != nil || string(b) != string(before) {
		t.Fatalf("dest-only payload changed %q %v", b, err)
	}
}

func TestContStageMetaUncapped(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	r, err := Open(root, h, QuotaMin)
	if err != nil {
		t.Fatal(err)
	}
	id, err := r.Reserve("dom", "tip")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Activate(id); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(root, id, "config")
	huge := make([]byte, MetaConfig+1)
	if err := os.WriteFile(cfg, huge, 0600); err != nil {
		t.Fatal(err)
	}
	r.CrashCut()
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("oversized stage metadata admitted")
	}
	if st, err := os.Stat(cfg); err != nil || st.Size() != int64(MetaConfig)+1 {
		t.Fatalf("stage metadata mutated %+v %v", st, err)
	}
}

func TestContReservedSkipsBound(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	r, err := Open(root, h, QuotaMin)
	if err != nil {
		t.Fatal(err)
	}
	id, err := r.Reserve("dom", "tip")
	if err != nil {
		t.Fatal(err)
	}
	r.CrashCut()
	dir := filepath.Join(root, id)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config")
	if err := os.WriteFile(cfg, make([]byte, MetaConfig+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("reserved generation skipped metadata cap")
	}
	if st, err := os.Stat(cfg); err != nil || st.Size() != int64(MetaConfig)+1 {
		t.Fatalf("reserved metadata mutated %+v %v", st, err)
	}
}

func TestContCommittedOptionalPack(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	dest := strings.Repeat("cd", 16)
	r, _ := commitPacked(t, root, h, dest)
	r.CrashCut()
	pack := filepath.Join(root, dest, "objects", "pack", "input.pack")
	if err := os.Remove(pack); err != nil {
		t.Fatal(err)
	}
	rewriteSlots(t, root, func(slots []Slot) {
		slots[0].Pack = 0
		slots[0].Index = 0
	})
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("committed generation admitted without pack and index")
	}
}

func TestContQuiesceRetry(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	r, err := Open(root, h, QuotaMin)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	id, err := r.Reserve("dom", "tip")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Activate(id); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "exit 1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	r.git = cmd
	r.gitRole = RoleIndex
	r.gitGen = id
	r.gitDone = done
	if err := r.Quiesce(id); err == nil {
		t.Fatal("nonzero role quiesced")
	}
	if err := r.Quiesce(id); err == nil {
		t.Fatal("retry promoted a failed role")
	}
	st, frozen, _, err := r.State(id)
	if err != nil || !frozen || st == stateQuiescent || st == stateVerified || st == stateCommitted {
		t.Fatalf("failed role reusable state %d frozen %v %v", st, frozen, err)
	}
	if err := r.Verify(id); err == nil {
		t.Fatal("verify after failed role")
	}
}

func TestContNilWaitAccepted(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = cmd.Wait()
	if err := QuiesceGroup(pid, nil); err == nil {
		t.Fatal("nil wait accepted")
	}
	cmd = exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := QuiesceGroup(cmd.Process.Pid, func() error {
		_ = cmd.Wait()
		return nil
	}); err == nil {
		t.Fatal("nil wait result accepted")
	}
	cmd = exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := QuiesceGroup(cmd.Process.Pid, func() error {
		_ = cmd.Wait()
		return errors.New("unknown")
	}); err == nil {
		t.Fatal("unknown wait accepted")
	}
	cmd = exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := QuiesceGroup(cmd.Process.Pid, cmd.Wait); err != nil {
		t.Fatal(err)
	}
	if err := QuiesceGroup(cmd.Process.Pid, cmd.Wait); err == nil {
		t.Fatal("repeated wait accepted")
	}
}

func TestContReaderUnpinRetry(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	dest := strings.Repeat("ef", 16)
	r, id := commitPacked(t, root, h, dest)
	defer r.Close()
	i := -1
	for n := range r.slots {
		if r.slots[n].ID == id {
			i = n
		}
	}
	if i < 0 {
		t.Fatal("slot")
	}
	cmd := exec.Command("/bin/sh", "-c", "exit 1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	r.readers[i] = &slotReader{cmd: cmd, done: done}
	time.Sleep(100 * time.Millisecond)
	if err := r.Unpin(id); err == nil {
		t.Fatal("nonzero reader unpin succeeded")
	}
	if err := r.Unpin(id); err == nil {
		t.Fatal("retry unpin released a failed reader")
	}
	_, _, n, err := r.State(id)
	if err != nil || n != 1 {
		t.Fatalf("pin after retry %d %v", n, err)
	}
}

func TestContNinthPublication(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	r, err := Open(root, h, QuotaMin+Reserve)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var last string
	for n := 0; n < MaxLeases+1; n++ {
		id, err := r.Reserve("d"+string(rune('a'+n)), "tip")
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Activate(id); err != nil {
			t.Fatal(err)
		}
		if err := r.WritePack(id, []byte{1, 2, 3, 4}); err != nil {
			t.Fatal(err)
		}
		if err := r.WriteMeta(id, "HEAD", []byte(AllowedHEAD)); err != nil {
			t.Fatal(err)
		}
		if err := r.WriteMeta(id, "config", []byte(AllowedConfig)); err != nil {
			t.Fatal(err)
		}
		for _, meta := range []string{"provenance", "manifest", "refs/heads/acquired"} {
			if err := r.WriteMeta(id, meta, []byte("x")); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.WriteIndex(id, []byte("idx")); err != nil {
			t.Fatal(err)
		}
		if err := r.Quiesce(id); err != nil {
			t.Fatal(err)
		}
		if err := r.Verify(id); err != nil {
			t.Fatal(err)
		}
		dest := strings.Repeat("0123456789abcdef", 2)
		dest = dest[:31] + string("0123456789abcdef"[n])
		err = r.Commit(id, dest)
		if n == MaxLeases {
			if err == nil {
				t.Fatal("ninth publication pin accepted")
			}
			last = id
			break
		}
		if err != nil {
			t.Fatalf("commit %d: %v", n, err)
		}
	}
	if _, _, n, err := r.State(last); err != nil || n != 0 {
		t.Fatalf("rejected commit still pinned %d %v", n, err)
	}
}

func TestContPartialWrite(t *testing.T) {
	err := writeFullDeadline(shortOK{}, []byte("abcdef"), time.Second)
	if err == nil {
		t.Fatal("partial write accepted")
	}
}

type shortOK struct{}

func (shortOK) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return 1, nil
}

func (shortOK) SetWriteDeadline(time.Time) error { return nil }

func TestContShortSIGKILL(t *testing.T) {
	if cpuTerminationOK(0, "cpu-ready soft=1 hard=8\n", true, 10*time.Millisecond) {
		t.Fatal("short SIGKILL accepted")
	}
}

func TestContCoreNotAbort(t *testing.T) {
	cmd := exec.Command(helperBin(t), "__gitcache_core")
	cmd.Env = AllowEnv()
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("core %v %s", err, out)
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGABRT || !strings.Contains(string(out), "core-armed") {
		t.Fatalf("core exit is not SIGABRT: %v %s", err, out)
	}
}

func TestContFIFOBlocks(t *testing.T) {
	root := privateRoot(t)
	rf, err := walkRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(int(rf.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	h := &fsHelper{root: int(rf.Fd()), dev: uint64(st.Dev)}
	done := make(chan uint32, 1)
	go func() {
		code, _ := h.openStat([]byte("pipe"))
		done <- code
	}()
	select {
	case code := <-done:
		if code == 0 {
			t.Fatal("fifo stat succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("fifo open blocked before type check")
	}
}

func TestContRootModeAfterOpen(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	r, err := Open(root, h, QuotaMin)
	if err != nil {
		t.Fatal(err)
	}
	defer r.CrashCut()
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reserve("dom", "tip"); err == nil {
		t.Fatal("root mode change was ignored")
	}
}

func TestContASGateInsideLimits(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	r, err := Open(root, h, QuotaMin)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	id, err := r.Reserve("dom", "tip")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Activate(id); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "ran")
	script := filepath.Join(dir, "notgit")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+sentinel+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	old := gitLaunchArg
	t.Cleanup(func() { gitLaunchArg = old })
	for _, kind := range []string{"over", "get", "under", "probe"} {
		gitLaunchArg = "__gitcache_as_" + kind
		cmd, lerr := LaunchGit(RoleVersion, h, script, id, "", r.rootFile, r.lockFile)
		if cmd != nil {
			_ = QuiesceGroup(cmd.Process.Pid, cmd.Wait)
		}
		if _, statErr := os.Stat(filepath.Join(root, id, "as-gate")); statErr != nil {
			t.Fatalf("%s gate did not run inside ApplyLimits: %v", kind, lerr)
		}
		if _, statErr := os.Stat(sentinel); statErr == nil {
			t.Fatalf("exec ran after %s", kind)
		}
		_ = os.Remove(filepath.Join(root, id, "as-gate"))
	}
	gitLaunchArg = "__gitcache_as_ok"
	cmd, err := LaunchGit(RoleVersion, h, script, id, "", r.rootFile, r.lockFile)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, statErr := os.Stat(sentinel); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			if cmd != nil {
				_ = QuiesceGroup(cmd.Process.Pid, cmd.Wait)
			}
			t.Fatalf("successful map did not exec: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if cmd != nil {
		_ = QuiesceGroup(cmd.Process.Pid, cmd.Wait)
	}
}

func TestContApplyLimitsSeams(t *testing.T) {
	anchor := anchorCWD(t)
	if err := os.Chdir(t.TempDir()); err != nil {
		_ = syscall.Close(anchor.fd)
		t.Fatal(err)
	}
	anchor.bind(t)
	oldSet, oldGet, oldGate, oldNote := limitSet, limitGet, asGate, asNote
	t.Cleanup(func() {
		limitSet, limitGet, asGate, asNote = oldSet, oldGet, oldGate, oldNote
	})
	for _, kind := range []string{"get", "under", "probe", "over"} {
		installASSeam(kind)
		if err := ApplyLimits(1); err == nil {
			t.Fatalf("%s accepted", kind)
		}
		if _, statErr := os.Stat("as-gate"); statErr != nil {
			t.Fatalf("%s did not record the gate", kind)
		}
		_ = os.Remove("as-gate")
	}
	installASSeam("ok")
	if err := ApplyLimits(1); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat("as-gate"); statErr == nil {
		t.Fatal("success path wrote a failure note")
	}
}

func TestContLaunchDrainAfterKill(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	r, err := Open(root, h, QuotaMin)
	if err != nil {
		t.Fatal(err)
	}
	defer r.CrashCut()
	id, err := r.Reserve("dom", "tip")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Activate(id); err != nil {
		t.Fatal(err)
	}
	old := gitLaunchArg
	t.Cleanup(func() { gitLaunchArg = old; launchReaped = nil })
	for _, arg := range []string{"__gitcache_sleep", "__gitcache_launch_bad", "__gitcache_launch_part"} {
		gitLaunchArg = arg
		var pid int
		launchReaped = func(p int) { pid = p }
		done := make(chan error, 1)
		go func(arg string) {
			_, lerr := LaunchGit(RoleVersion, h, h, id, "", r.rootFile, r.lockFile)
			done <- lerr
		}(arg)
		select {
		case lerr := <-done:
			if lerr == nil {
				t.Fatalf("%s accepted", arg)
			}
			if pid == 0 {
				t.Fatalf("%s returned without a reaped group: %v", arg, lerr)
			}
			if kerr := syscall.Kill(-pid, 0); !errors.Is(kerr, syscall.ESRCH) {
				t.Fatalf("%s group still alive after %v: %v", arg, lerr, kerr)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s blocked past the ack deadline", arg)
		}
	}
}

func TestContReaderPinBeforeStart(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	dest := strings.Repeat("ab", 16)
	r, id := commitPacked(t, root, h, dest)
	defer r.CrashCut()
	if err := r.Pin(id); err != nil {
		t.Fatal(err)
	}
	_, _, before, err := r.State(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.StartReader(id); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var saw uint32
	for {
		b, rerr := os.ReadFile(filepath.Join(root, "reader-pin"))
		if rerr == nil && len(b) == 4 {
			saw = binary.LittleEndian.Uint32(b)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not observe the durable pin")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, _, n, err := r.State(id)
	if err != nil || n != before+1 || saw != n {
		t.Fatalf("pin before exec: ledger %d child %d before %d err %v", n, saw, before, err)
	}
	r.fs.callHook = func(op byte) error {
		if op == 8 {
			return ErrUnsupported
		}
		return nil
	}
	if err := r.StartReader(id); err == nil {
		t.Fatal("second child started")
	}
}

func TestContReaderFsyncStartsNoChild(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	dest := strings.Repeat("cd", 16)
	r, id := commitPacked(t, root, h, dest)
	defer r.CrashCut()
	if err := r.Pin(id); err != nil {
		t.Fatal(err)
	}
	_, _, before, err := r.State(id)
	if err != nil {
		t.Fatal(err)
	}
	r.fs.callHook = func(op byte) error {
		if op == 8 {
			return ErrUnsupported
		}
		return nil
	}
	if err := r.StartReader(id); err == nil {
		t.Fatal("start succeeded without a durable pin")
	}
	_, _, n, err := r.State(id)
	if err != nil || n != before+1 {
		t.Fatalf("fsync failure dropped the child pin %d -> %d %v", before, n, err)
	}
	if _, err := os.Stat(filepath.Join(root, "reader-pin")); err == nil {
		t.Fatal("child ran after fsync failure")
	}
	if err := r.StartReader(id); err == nil {
		t.Fatal("retry started a child after uncertain pin")
	}
	if _, err := os.Stat(filepath.Join(root, "reader-pin")); err == nil {
		t.Fatal("retry ran a child")
	}
}

func TestContPlantedLockUntouched(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	lock := filepath.Join(root, "root.lock")
	if err := os.WriteFile(lock, nil, 0644); err != nil {
		t.Fatal(err)
	}
	opened, oerr := Open(root, h, QuotaMin)
	if oerr == nil {
		opened.CrashCut()
		t.Errorf("0644 lock accepted")
	}
	st, err := os.Stat(lock)
	if err != nil || st.Mode().Perm() != 0644 {
		t.Errorf("planted lock changed %v %v", st, err)
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(lock, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, oerr := Open(root, h, QuotaMin)
		done <- oerr
	}()
	select {
	case oerr := <-done:
		if oerr == nil {
			t.Fatal("fifo lock accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("fifo lock blocked before type check")
	}
	st, err = os.Stat(lock)
	if err != nil || st.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("fifo lock replaced %v %v", st, err)
	}
}

func TestContUnknownChildBlocksSecondGeneration(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	r, err := Open(root, h, QuotaMin+Reserve)
	if err != nil {
		t.Fatal(err)
	}
	id1, err := r.Reserve("dom", "tip")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := r.Reserve("dom2", "tip")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Activate(id1); err != nil || r.Activate(id2) != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	r.git = cmd
	r.gitRole = RoleVersion
	r.gitGen = id1
	r.gitDone = done
	old := awaitClose
	awaitClose = func(int, func() error) error { return ErrNotQuiescent }
	t.Cleanup(func() {
		awaitClose = old
		skipLaunchIdentity = false
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		r.CrashCut()
	})
	if err := r.Quiesce(id2); err == nil {
		t.Fatal("mismatch treated unknown closure as finished")
	}
	if r.git != cmd || !r.gitHold {
		t.Fatal("unknown closure cleared the owned child")
	}
	if err := r.Quiesce(id1); err == nil {
		t.Fatal("retry quiesce after unknown closure")
	}
	if r.git != cmd {
		t.Fatal("retry cleared the owned child")
	}
	skipLaunchIdentity = true
	if err := r.Launch(RoleVersion, h, id2, "", BuildIdentity{}, "", 0); err != ErrNotQuiescent {
		t.Fatalf("second generation launch %v", err)
	}
	if r.git != cmd {
		t.Fatal("second launch replaced the owned child")
	}
}

func TestContPositiveCloseAllowsNextGeneration(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	r, err := Open(root, h, QuotaMin+Reserve)
	if err != nil {
		t.Fatal(err)
	}
	defer r.CrashCut()
	id1, err := r.Reserve("dom", "tip")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := r.Reserve("dom2", "tip")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Activate(id1); err != nil || r.Activate(id2) != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	r.git = cmd
	r.gitRole = RoleVersion
	r.gitGen = id1
	r.gitDone = done
	if err := r.Quiesce(id1); err != nil {
		t.Fatal(err)
	}
	if r.git != nil || r.gitHold {
		t.Fatal("positive closure kept the singleton")
	}
	old := gitLaunchArg
	gitLaunchArg = "__gitcache_sleep"
	skipLaunchIdentity = true
	t.Cleanup(func() {
		gitLaunchArg = old
		skipLaunchIdentity = false
	})
	err = r.Launch(RoleVersion, h, id2, "", BuildIdentity{}, "", 0)
	if r.git != nil || r.gitHold {
		t.Fatalf("next generation held after positive close: %v", err)
	}
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("next generation was not launched: %v", err)
	}
}

func TestContLaunchHandshakeHoldsChild(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	r, err := Open(root, h, QuotaMin+Reserve)
	if err != nil {
		t.Fatal(err)
	}
	id1, err := r.Reserve("dom", "tip")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := r.Reserve("dom2", "tip")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Activate(id1); err != nil || r.Activate(id2) != nil {
		t.Fatal(err)
	}
	oldArg := gitLaunchArg
	launchClosure = func(*exec.Cmd) error { return ErrNotQuiescent }
	gitLaunchArg = "__gitcache_sleep"
	skipLaunchIdentity = true
	t.Cleanup(func() {
		launchClosure = nil
		gitLaunchArg = oldArg
		skipLaunchIdentity = false
		r.CrashCut()
	})
	err = r.Launch(RoleVersion, h, id1, "", BuildIdentity{}, "", 0)
	if err == nil || r.git == nil || !r.gitHold {
		t.Fatalf("unclosed handshake dropped the child: %v hold %v", err, r.gitHold)
	}
	held := r.git
	if err := r.Launch(RoleVersion, h, id2, "", BuildIdentity{}, "", 0); err != ErrNotQuiescent {
		t.Fatalf("second generation after unclosed handshake %v", err)
	}
	if err := r.Launch(RoleVersion, h, id1, "", BuildIdentity{}, "", 0); err != ErrNotQuiescent {
		t.Fatalf("retry after unclosed handshake %v", err)
	}
	if r.git != held {
		t.Fatal("retry replaced the unclosed child")
	}
}

func TestContLauncherNaturalExit(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	r, err := Open(root, h, QuotaMin)
	if err != nil {
		t.Fatal(err)
	}
	defer r.CrashCut()
	id, err := r.Reserve("dom", "tip")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Activate(id); err != nil {
		t.Fatal(err)
	}
	old := gitLaunchArg
	gitLaunchArg = "__gitcache_launch_eof"
	var st *os.ProcessState
	launchReceipt = func(ps *os.ProcessState) { st = ps }
	t.Cleanup(func() {
		gitLaunchArg = old
		launchReceipt = nil
		launchReaped = nil
	})
	start := time.Now()
	var pid int
	launchReaped = func(p int) { pid = p }
	cmd, lerr := LaunchGit(RoleVersion, h, h, id, "", r.rootFile, r.lockFile)
	if time.Since(start) > 3*time.Second {
		t.Fatal("natural launcher exit was not bounded")
	}
	if lerr == nil || cmd != nil {
		if cmd != nil {
			_ = cmd.Wait()
		}
		t.Fatalf("eof launcher accepted %v", lerr)
	}
	if pid == 0 || st == nil || st.ExitCode() != 2 {
		t.Fatalf("owned receipt pid %d state %v err %v", pid, st, lerr)
	}
	if kerr := syscall.Kill(-pid, 0); !errors.Is(kerr, syscall.ESRCH) {
		t.Fatalf("group alive %v", kerr)
	}
}

func TestContGenUsesRootDevice(t *testing.T) {
	root := privateRoot(t)
	wide := filepath.Join(root, "wide")
	if err := os.Mkdir(wide, 0755); err != nil {
		t.Fatal(err)
	}
	wf, err := os.Open(wide)
	if err != nil {
		t.Fatal(err)
	}
	defer wf.Close()
	if _, err := heldRoot(int(wf.Fd())); err == nil {
		t.Fatal("0755 root accepted before chdir")
	}
	if err := os.Mkdir(filepath.Join(root, "g"), 0700); err != nil {
		t.Fatal(err)
	}
	anchor := anchorCWD(t)
	if err := os.Chdir(root); err != nil {
		_ = syscall.Close(anchor.fd)
		t.Fatal(err)
	}
	anchor.bind(t)
	var st syscall.Stat_t
	if err := syscall.Lstat("g", &st); err != nil {
		t.Fatal(err)
	}
	if err := enterGen("g", uint64(st.Dev)+1); err == nil {
		t.Fatal("generation accepted on a foreign device")
	}
	if err := enterGen("g", uint64(st.Dev)); err != nil {
		t.Fatal(err)
	}
}

func TestContForeignUID(t *testing.T) {
	root := privateRoot(t)
	rf, err := walkRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(int(rf.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	old := observedUID
	observedUID = func(*syscall.Stat_t) uint32 { return uint32(os.Geteuid()) + 1 }
	t.Cleanup(func() { observedUID = old })
	if err := fstatHeld(int(rf.Fd()), uint64(st.Dev), true, 0700); err == nil {
		t.Fatal("foreign uid accepted")
	}
}

func TestContHelperCWDSurvivesCleanup(t *testing.T) {
	anchorFD, err := syscall.Open(".", syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(anchorFD)
	var anchor syscall.Stat_t
	if err := syscall.Fstat(anchorFD, &anchor); err != nil {
		t.Fatal(err)
	}
	t.Run("fifo-openStat", func(t *testing.T) {
		root := privateRoot(t)
		rf, err := walkRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		defer rf.Close()
		var st syscall.Stat_t
		if err := syscall.Fstat(int(rf.Fd()), &st); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0600); err != nil {
			t.Fatal(err)
		}
		h := &fsHelper{root: int(rf.Fd()), dev: uint64(st.Dev)}
		done := make(chan uint32, 1)
		go func() {
			code, _ := h.openStat([]byte("pipe"))
			done <- code
		}()
		select {
		case code := <-done:
			if code == 0 {
				t.Errorf("fifo stat succeeded")
			}
		case <-time.After(time.Second):
			t.Errorf("fifo open blocked before type check")
		}
	})
	t.Run("missing-openStat", func(t *testing.T) {
		root := privateRoot(t)
		rf, err := walkRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		defer rf.Close()
		var st syscall.Stat_t
		if err := syscall.Fstat(int(rf.Fd()), &st); err != nil {
			t.Fatal(err)
		}
		h := &fsHelper{root: int(rf.Fd()), dev: uint64(st.Dev)}
		if code, _ := h.openStat([]byte("missing")); code == 0 {
			t.Errorf("missing stat succeeded")
		}
	})
	if _, err := os.Getwd(); err != nil {
		t.Fatalf("cwd lost after helper cleanup: %v", err)
	}
	cwd, err := syscall.Open(".", syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		t.Fatalf("cwd open after helper cleanup: %v", err)
	}
	defer syscall.Close(cwd)
	var st syscall.Stat_t
	if err := syscall.Fstat(cwd, &st); err != nil {
		t.Fatal(err)
	}
	if st.Dev != anchor.Dev || st.Ino != anchor.Ino {
		t.Fatalf("cwd identity changed dev %d/%d ino %d/%d", st.Dev, anchor.Dev, st.Ino, anchor.Ino)
	}
	probe, err := os.CreateTemp(".", "cwd-access-")
	if err != nil {
		t.Fatalf("filesystem access after helper cleanup: %v", err)
	}
	name := probe.Name()
	probe.Close()
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
}

// reapedChild is a real process that has already been waited. The group is ESRCH.
// The returned error is that Wait result.
func reapedChild(t *testing.T, code int) (*exec.Cmd, error) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit "+strconv.Itoa(code))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	if code == 0 && waitErr != nil {
		t.Fatal(waitErr)
	}
	if code != 0 && waitErr == nil {
		t.Fatal("expected a nonzero exit")
	}
	if kerr := syscall.Kill(-cmd.Process.Pid, 0); !errors.Is(kerr, syscall.ESRCH) {
		t.Fatalf("group still present %v", kerr)
	}
	return cmd, waitErr
}

func TestContUnknownGitWaitDropsSingleton(t *testing.T) {
	for _, mode := range []string{"generic", "absent", "mismatch"} {
		t.Run(mode, func(t *testing.T) {
			h := helperBin(t)
			root := privateRoot(t)
			r, err := Open(root, h, QuotaMin+Reserve)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { r.CrashCut() })
			id1, err := r.Reserve("dom", "tip")
			if err != nil {
				t.Fatal(err)
			}
			id2, err := r.Reserve("dom2", "tip")
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Activate(id1); err != nil || r.Activate(id2) != nil {
				t.Fatal(err)
			}
			used, err := r.Used()
			if err != nil {
				t.Fatal(err)
			}
			cmd, _ := reapedChild(t, 1)
			done := make(chan error, 1)
			switch mode {
			case "generic":
				done <- errors.New("unknown wait")
			case "absent":
				cmd.ProcessState = nil
				done <- errors.New("unknown wait")
			case "mismatch":
				other, oerr := reapedChild(t, 2)
				if oerr == nil {
					t.Fatal("mismatch child had no status")
				}
				done <- &exec.ExitError{ProcessState: other.ProcessState}
			}
			r.git = cmd
			r.gitRole = RoleVersion
			r.gitGen = id1
			r.gitDone = done
			if err := r.Quiesce(id1); err == nil {
				t.Error("unknown wait quiesced the role")
			}
			if r.git == nil || !r.gitHold {
				t.Errorf("unknown wait cleared the child")
			}
			if err := r.Quiesce(id1); err == nil || r.git == nil || !r.gitHold {
				t.Errorf("retry dropped the unknown child %v", err)
			}
			oldArg := gitLaunchArg
			gitLaunchArg = "__gitcache_launch_eof"
			skipLaunchIdentity = true
			t.Cleanup(func() {
				gitLaunchArg = oldArg
				skipLaunchIdentity = false
			})
			if err := r.Launch(RoleVersion, h, id2, "", BuildIdentity{}, "", 0); err != ErrNotQuiescent || r.git != cmd {
				t.Errorf("unknown wait allowed a second launch: %v", err)
			}
			if err := r.Close(); err != ErrNotQuiescent {
				t.Errorf("unknown wait Close released capacity: %v", err)
			}
			if got, gerr := r.Used(); gerr != nil || got != used {
				t.Errorf("unknown wait changed Used %d -> %d %v", used, got, gerr)
			}
		})
	}
}

func TestContKnownNonzeroGitAllowsOtherWork(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	r, err := Open(root, h, QuotaMin+Reserve)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.CrashCut() })
	id1, err := r.Reserve("dom", "tip")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := r.Reserve("dom2", "tip")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Activate(id1); err != nil || r.Activate(id2) != nil {
		t.Fatal(err)
	}
	cmd, waitErr := reapedChild(t, 1)
	done := make(chan error, 1)
	done <- waitErr
	r.git = cmd
	r.gitRole = RoleVersion
	r.gitGen = id1
	r.gitDone = done
	if err := r.Quiesce(id1); err == nil {
		t.Fatal("known nonzero role quiesced")
	}
	st, frozen, _, err := r.State(id1)
	if err != nil || !frozen || st == stateQuiescent || st == stateCommitted {
		t.Fatalf("known failure was promoted state %d frozen %v %v", st, frozen, err)
	}
	if r.git != nil || r.gitHold {
		t.Fatal("known nonzero exit kept the singleton")
	}
	if err := r.Quiesce(id1); err == nil {
		t.Fatal("retry promoted a known failed role")
	}
	oldArg := gitLaunchArg
	gitLaunchArg = "__gitcache_launch_eof"
	skipLaunchIdentity = true
	t.Cleanup(func() {
		gitLaunchArg = oldArg
		skipLaunchIdentity = false
	})
	if err := r.Launch(RoleVersion, h, id2, "", BuildIdentity{}, "", 0); err == ErrNotQuiescent {
		t.Fatal("known closed child blocked other work")
	}
}

func TestContUnknownReaderWaitReleasesOnClose(t *testing.T) {
	for _, mode := range []string{"generic", "absent", "mismatch"} {
		t.Run(mode, func(t *testing.T) {
			h := helperBin(t)
			root := privateRoot(t)
			dest := strings.Repeat("ab", 16)
			r, id := commitPacked(t, root, h, dest)
			t.Cleanup(func() { r.CrashCut() })
			i := -1
			for n := range r.slots {
				if r.slots[n].ID == id {
					i = n
				}
			}
			if i < 0 {
				t.Fatal("slot")
			}
			r.slots[i].ReadCount++
			r.ownedPins[i]++
			r.childSlot = i
			if err := r.persist(); err != nil {
				t.Fatal(err)
			}
			_, _, before, err := r.State(id)
			if err != nil {
				t.Fatal(err)
			}
			used, err := r.Used()
			if err != nil {
				t.Fatal(err)
			}
			pack := filepath.Join(root, dest, "objects/pack/input.pack")
			payload, err := os.ReadFile(pack)
			if err != nil {
				t.Fatal(err)
			}
			cmd, _ := reapedChild(t, 1)
			done := make(chan error, 1)
			switch mode {
			case "generic":
				done <- errors.New("unknown wait")
			case "absent":
				cmd.ProcessState = nil
				done <- errors.New("unknown wait")
			case "mismatch":
				other, oerr := reapedChild(t, 2)
				if oerr == nil {
					t.Fatal("mismatch child had no status")
				}
				done <- &exec.ExitError{ProcessState: other.ProcessState}
			}
			r.readers[i] = &slotReader{cmd: cmd, done: done}
			if err := r.Unpin(id); err == nil {
				t.Fatal("unknown reader unpin succeeded")
			}
			if err := r.Unpin(id); err == nil {
				t.Fatal("retry unpin released an unknown reader")
			}
			if _, _, n, err := r.State(id); err != nil || n != before {
				t.Fatalf("unpin changed ReadCount %d -> %d %v", before, n, err)
			}
			if err := r.Close(); err != ErrNotQuiescent {
				t.Errorf("unknown reader Close released the pin: %v", err)
			}
			if _, _, n, err := r.State(id); err != nil || n != before {
				t.Errorf("ReadCount %d want %d (%v)", n, before, err)
			}
			if got, gerr := r.Used(); gerr != nil || got != used {
				t.Errorf("Used %d -> %d %v", used, got, gerr)
			}
			if got, rerr := os.ReadFile(pack); rerr != nil || string(got) != string(payload) {
				t.Errorf("payload changed %v", rerr)
			}
			if err := r.StartReader(id); err == nil {
				t.Error("unknown reader allowed a new child")
			}
			if _, err := r.EvictLRU(); err == nil {
				t.Error("unknown reader eviction released the slot")
			}
			if got, rerr := os.ReadFile(pack); rerr != nil || string(got) != string(payload) {
				t.Errorf("payload after retry %v", rerr)
			}
			if _, _, n, err := r.State(id); err != nil || n != before {
				t.Errorf("retry ReadCount %d want %d (%v)", n, before, err)
			}
		})
	}
}

func TestContReaderKnownExitClose(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	dest := strings.Repeat("cd", 16)
	r, id := commitPacked(t, root, h, dest)
	defer r.Close()
	i := -1
	for n := range r.slots {
		if r.slots[n].ID == id {
			i = n
		}
	}
	if i < 0 {
		t.Fatal("slot")
	}
	_, _, before, err := r.State(id)
	if err != nil {
		t.Fatal(err)
	}
	pack := filepath.Join(root, dest, "objects/pack/input.pack")
	payload, err := os.ReadFile(pack)
	if err != nil {
		t.Fatal(err)
	}
	cmd, waitErr := reapedChild(t, 1)
	done := make(chan error, 1)
	done <- waitErr
	r.readers[i] = &slotReader{cmd: cmd, done: done}
	if err := r.Unpin(id); err == nil {
		t.Fatal("known nonzero reader unpin succeeded")
	}
	if _, _, n, err := r.State(id); err != nil || n != before {
		t.Fatalf("unpin dropped a known reader %d %v", n, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, n, err := r.State(id); err != nil || n != before-1 {
		t.Fatalf("known exit was not released at Close %d %v", n, err)
	}
	if got, rerr := os.ReadFile(pack); rerr != nil || string(got) != string(payload) {
		t.Fatalf("known exit removed payload %v", rerr)
	}
}
