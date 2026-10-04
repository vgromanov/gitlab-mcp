//go:build linux || darwin

package gitcache

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOrphanAndQuotaRecovery(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	if err := os.Mkdir(filepath.Join(root, strings.Repeat("ab", 16)), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("orphan hex directory")
	}
	root = privateRoot(t)
	if err := os.WriteFile(filepath.Join(root, "scratch"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("missing ledger with payload")
	}
	root = privateRoot(t)
	r, err := Open(root, h, QuotaMin)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, h, QuotaMin+Reserve); err == nil {
		t.Fatal("quota mismatch")
	}
}

func TestUnknownPinCannotUnpin(t *testing.T) {
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
	if err := r.WriteMeta(id, "HEAD", []byte(AllowedHEAD)); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteMeta(id, "config", []byte(AllowedConfig)); err != nil {
		t.Fatal(err)
	}
	if err := r.WritePack(id, []byte{1, 2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	if err := fillProof(r, id); err != nil {
		t.Fatal(err)
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
	if err := r.Pin(id); err != nil {
		t.Fatal(err)
	}
	r.CrashCut()
	r, err = Open(root, h, QuotaMin)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Unpin(id); err == nil {
		t.Fatal("unknown restart pin")
	}
	if _, err := r.EvictLRU(); err == nil {
		t.Fatal("pinned eviction")
	}
	if err := r.Close(); err != ErrPinned {
		t.Fatalf("close %v", err)
	}
	if err := r.Pin(id); err != ErrClosed {
		t.Fatalf("pin while draining %v", err)
	}
	if _, err := r.Reserve("other", "t"); err != ErrClosed {
		t.Fatal(err)
	}
}

func TestDeletionKeepsChargeOnStatError(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	r, err := Open(root, h, RootBytes+Reserve*2)
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
	used, err := r.Used()
	if err != nil {
		t.Fatal(err)
	}
	r.fs.mu.Lock()
	prev := r.fs
	r.fs.mu.Unlock()
	_ = prev
	r.fs.callHook = func(op byte) error {
		if op == 6 {
			return ErrUnsupported
		}
		return nil
	}
	if err := r.removeGen(id); err == nil {
		t.Fatal("stat error treated as absence")
	}
	r.fs.callHook = nil
	now, err := r.Used()
	if err != nil || now != used {
		t.Fatalf("charge changed %d -> %d %v", used, now, err)
	}
	if _, err := r.Reserve("more", "t"); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestASGateBlocksExec(t *testing.T) {
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
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "ran")
	script := filepath.Join(dir, "notgit")
	body := "#!/bin/sh\ntouch " + sentinel + "\n"
	if err := os.WriteFile(script, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	old := gitLaunchArg
	gitLaunchArg = "__gitcache_git_failas"
	t.Cleanup(func() { gitLaunchArg = old })
	cmd, err := LaunchGit(RoleIndex, h, script, id, "", r.rootFile, r.lockFile)
	if err != ErrLimit || cmd != nil {
		if cmd != nil {
			_ = QuiesceGroup(cmd.Process.Pid, cmd.Wait)
		}
		t.Fatalf("AS gate %v", err)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("exec ran after AS failure")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHelperACKAndENOENT(t *testing.T) {
	if mapErr(os.ErrInvalid) == 6 {
		t.Fatal("generic error is not ENOENT")
	}
	if mapErr(syscall.ENOENT) != 6 {
		t.Fatal("confirmed ENOENT")
	}
	root := privateRoot(t)
	rf, err := walkRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := startFS("/usr/bin/true", rf); err == nil {
		t.Fatal("bad ack")
	}
	for _, arg := range []string{"__gitcache_ack_bad", "__gitcache_ack_short"} {
		old := fsHelperArg
		fsHelperArg = arg
		rf, err = walkRoot(root)
		if err != nil {
			fsHelperArg = old
			t.Fatal(err)
		}
		_, err = startFS(helperBin(t), rf)
		fsHelperArg = old
		if err != ErrBusy {
			t.Fatalf("%s ack %v", arg, err)
		}
	}
}

func TestModeLockAndCommittedMismatch(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("0755 root")
	}
	root = privateRoot(t)
	if err := os.Chmod(root, 0775); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("0775 root")
	}
	root = privateRoot(t)
	if err := syscall.Mkfifo(filepath.Join(root, strings.Repeat("ab", 16)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("special file")
	}
	root = privateRoot(t)
	r, err := Open(root, h, QuotaMin)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(root, "root.lock"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("oversized lock")
	}
	root = privateRoot(t)
	r, err = Open(root, h, QuotaMin)
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
	if err := os.Chmod(filepath.Join(root, id), 0755); err != nil {
		t.Fatal(err)
	}
	r.CrashCut()
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("0755 generation")
	}
	root = privateRoot(t)
	dest := strings.Repeat("cd", 16)
	r, _ = commitPacked(t, root, h, dest)
	if err := os.Remove(filepath.Join(root, dest, "objects", "pack", "input.pack")); err != nil {
		t.Fatal(err)
	}
	r.CrashCut()
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("missing committed pack")
	}
}

func TestDeletionHooksRetainCharge(t *testing.T) {
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
	used, err := r.Used()
	if err != nil {
		t.Fatal(err)
	}
	r.fs.callHook = func(op byte) error {
		if op == 5 {
			return ErrUnsupported
		}
		return nil
	}
	if err := r.removeGen(id); err == nil {
		t.Fatal("unlink error treated as absence")
	}
	r.fs.callHook = nil
	now, err := r.Used()
	if err != nil || now != used {
		t.Fatalf("unlink charge %d -> %d %v", used, now, err)
	}
	if _, err := r.Reserve("more", "t"); err == nil {
		t.Fatal("reservation accepted against an ambiguous slot")
	}
	r.fs.callHook = func(op byte) error {
		if op == 8 {
			return ErrUnsupported
		}
		return nil
	}
	if err := r.removeGen(id); err == nil {
		t.Fatal("fsync error cleared the slot")
	}
	r.CrashCut()
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("ambiguous deletion restored the root")
	}
}

func TestWriteAndPinFreeze(t *testing.T) {
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
	r.fs.callHook = func(op byte) error {
		if op == 1 {
			return ErrUnsupported
		}
		return nil
	}
	if err := r.WritePack(id, []byte{1, 2, 3, 4}); err == nil {
		t.Fatal("write accepted")
	}
	r.fs.callHook = nil
	st, frozen, _, err := r.State(id)
	if err != nil || !frozen || st != stateActive {
		t.Fatalf("freeze %d %v %v", st, frozen, err)
	}
	if err := r.Quiesce(id); err == nil {
		t.Fatal("quiesce frozen")
	}
	if err := r.Verify(id); err == nil {
		t.Fatal("verify frozen")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	dest := strings.Repeat("cd", 16)
	root = privateRoot(t)
	r, id = commitPacked(t, root, h, dest)
	r.fs.callHook = func(op byte) error {
		if op == 8 {
			return ErrUnsupported
		}
		return nil
	}
	if err := r.Pin(id); err == nil {
		t.Fatal("pin persisted through fsync failure")
	}
	r.fs.callHook = nil
	_, _, n, err := r.State(id)
	if err != nil || n != 1 {
		t.Fatalf("pin rollback %d %v", n, err)
	}
	if err := r.Close(); err != ErrNotQuiescent {
		t.Fatalf("close after uncertain pin %v", err)
	}
}

func TestListCapAndDescriptor(t *testing.T) {
	root := privateRoot(t)
	rf, err := walkRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(int(rf.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	h := &fsHelper{root: int(rf.Fd()), dev: uint64(st.Dev)}
	for i := 0; i < 129; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("n%03d", i)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if code, _ := h.op(7, []byte(".")); code != 2 {
		t.Fatalf("list cap %d", code)
	}
	ents, err := os.ReadDir(root)
	if err != nil || len(ents) < 129 {
		t.Fatalf("overflow touched names %d %v", len(ents), err)
	}
	h.dev++
	if code, _ := h.op(7, []byte(".")); code != 3 {
		t.Fatalf("device %d", code)
	}
	rf.Close()
	f, err := os.CreateTemp(t.TempDir(), "notdir")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	bad := &fsHelper{root: int(f.Fd()), dev: 1}
	if code, _ := bad.op(7, nil); code == 0 {
		t.Fatal("file descriptor accepted as a directory")
	}
}

func TestBlockedIPCReturns(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()
	c := &fsClient{in: &memWriteCloser{}, out: pr}
	start := time.Now()
	_, err = c.call(2, []byte("x"))
	if err != ErrNotQuiescent {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("ipc exceeded the ack deadline")
	}
}

func commitPacked(t *testing.T, root, helper, dest string) (*Root, string) {
	t.Helper()
	r, err := Open(root, helper, QuotaMin)
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
	if err := r.Quiesce(id); err != nil {
		t.Fatal(err)
	}
	if err := r.Verify(id); err != nil {
		t.Fatal(err)
	}
	if err := r.Commit(id, dest); err != nil {
		t.Fatal(err)
	}
	return r, id
}

func fillProof(r *Root, id string) error {
	for _, meta := range []string{"provenance", "manifest", "refs/heads/acquired"} {
		if err := r.WriteMeta(id, meta, []byte("x")); err != nil {
			return err
		}
	}
	return r.WriteIndex(id, []byte("idx"))
}
