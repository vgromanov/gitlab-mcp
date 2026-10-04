//go:build linux || darwin

package gitcache

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func privateRoot(t *testing.T) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(real, "cache")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func helperBin(t *testing.T) string {
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOpenReserveCrashReopen(t *testing.T) {
	root := privateRoot(t)
	h := helperBin(t)
	r, err := Open(root, h, QuotaMin)
	if err != nil {
		t.Fatal(err)
	}
	id, err := r.Reserve("domain-a", "tip")
	if err != nil {
		t.Fatal(err)
	}
	st, frozen, _, err := r.State(id)
	if err != nil || st != stateReserved || frozen {
		t.Fatalf("state %d frozen %v err %v", st, frozen, err)
	}
	r.CrashCut()
	r2, err := Open(root, h, QuotaMin)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	st, frozen, _, err = r2.State(id)
	if err != nil || st != stateReserved || !frozen {
		t.Fatalf("reopen state %d frozen %v err %v", st, frozen, err)
	}
	if _, err := r2.Reserve("domain-b", "tip"); err != ErrQuota {
		t.Fatalf("orphan must keep R, got %v", err)
	}
	if _, err := r2.EvictLRU(); err != ErrBusy {
		t.Fatalf("frozen must not be evicted, got %v", err)
	}
}

func TestSymlinkUnknownHardlinkCorrupt(t *testing.T) {
	h := helperBin(t)
	t.Run("symlink", func(t *testing.T) {
		parent := privateRoot(t)
		link := parent + "-link"
		if err := os.Symlink(parent, link); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(link, h, QuotaMin); err != ErrPath {
			t.Fatalf("symlink root %v", err)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		root := privateRoot(t)
		if err := os.WriteFile(filepath.Join(root, "notes"), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(root, h, QuotaMin); err != ErrCorrupt {
			t.Fatalf("unknown %v", err)
		}
	})
	t.Run("hardlink", func(t *testing.T) {
		root := privateRoot(t)
		r, err := Open(root, h, QuotaMin)
		if err != nil {
			t.Fatal(err)
		}
		r.Close()
		ledger := filepath.Join(root, "ledger")
		if err := os.Link(ledger, filepath.Join(root, "ledger.tmp")); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(root, h, QuotaMin); err == nil {
			t.Fatal("hardlink must fail closed")
		}
	})
	t.Run("corrupt", func(t *testing.T) {
		root := privateRoot(t)
		r, err := Open(root, h, QuotaMin)
		if err != nil {
			t.Fatal(err)
		}
		r.Close()
		p := filepath.Join(root, "ledger")
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		b[80] ^= 0xff
		if err := os.WriteFile(p, b, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(root, h, QuotaMin); err != ErrCorrupt {
			t.Fatalf("corrupt %v", err)
		}
	})
}

func TestLifecyclePinLRUAndClose(t *testing.T) {
	root := privateRoot(t)
	h := helperBin(t)
	q := RootBytes + Reserve*2
	r, err := Open(root, h, q)
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
	over := make([]byte, PackMax+1)
	if err := r.WritePack(id, over); err != ErrQuota {
		t.Fatalf("P+1 write %v", err)
	}
	info, err := os.Stat(filepath.Join(root, id, "objects", "pack", "input.pack"))
	if err != nil || info.Size() != 4 {
		t.Fatalf("pack size kept %v %+v", err, info)
	}
	if err := r.WriteMeta(id, "manifest", []byte("m")); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteMeta(id, "manifest", []byte("mm")); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteIndex(id, []byte("idx")); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(filepath.Join(root, id, "objects", "pack"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name() != "input.pack" && e.Name() != "input.idx" {
			t.Fatalf("unexpected pack file %s", e.Name())
		}
	}
	if err := r.Quiesce(id); err != nil {
		t.Fatal(err)
	}
	if err := r.Verify(id); err != nil {
		t.Fatal(err)
	}
	dest := strings.Repeat("cd", 16)
	if err := r.Commit(id, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, id)); !os.IsNotExist(err) {
		t.Fatal("source dir must be gone after promotion")
	}
	if err := r.Pin(id); err != nil {
		t.Fatal(err)
	}
	if _, _, n, err := r.State(id); err != nil || n != 1 {
		t.Fatalf("pin %d %v", n, err)
	}
	if err := r.Close(); err != ErrPinned {
		t.Fatalf("close pinned %v", err)
	}
	if _, err := r.EvictLRU(); err != ErrBusy {
		t.Fatalf("pinned evict %v", err)
	}
	if err := r.Unpin(id); err != nil {
		t.Fatal(err)
	}
	evicted, err := r.EvictLRU()
	if err != nil || evicted != id {
		t.Fatalf("evict %s %v", evicted, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentQuota(t *testing.T) {
	root := privateRoot(t)
	h := helperBin(t)
	r, err := Open(root, h, QuotaMin)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var wg sync.WaitGroup
	var mu sync.Mutex
	okN, failN := 0, 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Reserve("same", "tip")
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				okN++
			} else if errors.Is(err, ErrQuota) || errors.Is(err, ErrBusy) {
				failN++
			} else {
				t.Errorf("reserve %v", err)
			}
		}()
	}
	wg.Wait()
	if okN != 1 || failN != 7 {
		t.Fatalf("ok %d fail %d", okN, failN)
	}
}

func TestBusySecondOpen(t *testing.T) {
	root := privateRoot(t)
	h := helperBin(t)
	r, err := Open(root, h, QuotaMin)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := Open(root, h, QuotaMin); err != ErrBusy {
		t.Fatalf("second open %v", err)
	}
}

func TestLimitsAndQuiescence(t *testing.T) {
	exe := helperBin(t)
	cmd := exec.Command(exe, "__gitcache_fsize")
	cmd.Env = AllowEnv()
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "fsize-enforced") {
		t.Fatalf("fsize %v %s", err, out)
	}
	cmd = exec.Command(exe, "__gitcache_nofile")
	cmd.Env = AllowEnv()
	out, err = cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "nofile-enforced") {
		t.Fatalf("nofile %v %s", err, out)
	}
	cmd = exec.Command(exe, "__gitcache_as")
	cmd.Env = AllowEnv()
	out, err = cmd.CombinedOutput()
	text := string(out)
	switch {
	case err == nil && strings.Contains(text, "as-enforced"):
	case err != nil && strings.Contains(text, "as-reject") && !strings.Contains(text, "as-set"):
	default:
		t.Fatalf("AS must be enforced or rejected, not merely recorded: %v %s", err, text)
	}
	if strings.Contains(text, "as-ineffective") || strings.Contains(text, "as-set") {
		t.Fatalf("AS getrlimit agreement is not enforcement: %s", text)
	}
	dir := t.TempDir()
	cmd = exec.Command(exe, "__gitcache_core")
	cmd.Env = AllowEnv()
	cmd.Dir = dir
	_ = cmd.Run()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.Contains(e.Name(), "core") {
			info, _ := e.Info()
			if info != nil && info.Size() > 0 {
				t.Fatalf("core file %s size %d", e.Name(), info.Size())
			}
		}
	}
	cmd = exec.Command(exe, "__gitcache_cpu")
	cmd.Env = AllowEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err = cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "cpu-enforced") {
		t.Fatalf("cpu %v %s", err, out)
	}
}

func TestGroupWaitIsNotQuiescence(t *testing.T) {
	exe := helperBin(t)
	cmd := exec.Command(exe, "__gitcache_tree")
	cmd.Env = AllowEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := stdout.Read(buf)
	if err != nil {
		_ = QuiesceGroup(cmd.Process.Pid, cmd.Wait)
		t.Fatal(err)
	}
	_ = n
	// Kill only the leader. The grandchild stays in the group.
	_ = syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Wait()
	if err := syscall.Kill(-cmd.Process.Pid, 0); err != nil {
		t.Fatalf("grandchild should still be reachable, kill probe %v", err)
	}
	if err := QuiesceGroup(cmd.Process.Pid, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestTokenCoreBeforeRead(t *testing.T) {
	exe := helperBin(t)
	cmd := exec.Command(exe, "__gitcache_token")
	cmd.Env = AllowEnv()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var frame [8]byte
	frame[0] = 4
	copy(frame[4:], []byte("tokn"))
	if _, err := stdin.Write(frame[:]); err != nil {
		t.Fatal(err)
	}
	stdin.Close()
	out := make([]byte, 64)
	n, _ := stdout.Read(out)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out[:n]), "token-read") {
		t.Fatalf("token output %q", out[:n])
	}

	high := exec.Command(exe, "__gitcache_token_high")
	high.Env = AllowEnv()
	highOut, err := high.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := high.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	got := make(chan int, 1)
	go func() {
		n, _ := highOut.Read(buf)
		got <- n
	}()
	select {
	case n := <-got:
		_ = high.Wait()
		text := string(buf[:n])
		if !strings.Contains(text, "token-blocked") && !strings.Contains(text, "core-locked") {
			t.Fatalf("high core %q", text)
		}
	case <-time.After(2 * time.Second):
		_ = high.Process.Kill()
		t.Fatal("token read started before the core check")
	}
}

func TestDeadlineQuiesce(t *testing.T) {
	exe := helperBin(t)
	cmd := exec.Command(exe, "__gitcache_sleep")
	cmd.Env = AllowEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := QuiesceGroup(cmd.Process.Pid, cmd.Wait); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchSkipsExecWhenUnaudited(t *testing.T) {
	root := privateRoot(t)
	h := helperBin(t)
	r, err := Open(root, h, QuotaMin)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	id, err := r.Reserve("d", "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Activate(id); err != nil {
		t.Fatal(err)
	}
	idN := BuildIdentity{SourceSHA256: "nope", OS: "darwin", Arch: "arm64", Compiler: "c", BinarySHA256: "b", VersionLine: "v", Flags: GitBuildFlags}
	err = r.Launch(RoleIndex, "/usr/bin/git", id, "", idN, "", 24)
	if err != ErrAudit {
		t.Fatalf("unaudited launch %v", err)
	}
	if r.git != nil {
		t.Fatal("git process started")
	}
}
