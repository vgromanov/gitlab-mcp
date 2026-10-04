//go:build linux || darwin

package gitcache

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestGapOldSourceAndDestBothExist(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	dest := strings.Repeat("cd", 16)
	r, id := commitPacked(t, root, h, dest)
	if err := os.Mkdir(filepath.Join(root, id), 0700); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(root, id, "kept")
	if err := os.WriteFile(payload, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	r.CrashCut()
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("old source and dest were both admitted")
	}
	if b, err := os.ReadFile(payload); err != nil || string(b) != "keep" {
		t.Fatalf("rejected admission changed payload %q %v", b, err)
	}
}

func TestGapMissingLedgerWithScratch(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	r, err := Open(root, h, QuotaMin)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "ledger")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ledger.tmp"), []byte("scratch"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("missing ledger initialized over ledger.tmp")
	}
	if _, err := os.Stat(filepath.Join(root, "ledger")); err == nil {
		t.Fatal("failed admission created a ledger")
	}
	if b, err := os.ReadFile(filepath.Join(root, "ledger.tmp")); err != nil || string(b) != "scratch" {
		t.Fatalf("scratch changed %q %v", b, err)
	}
}

func TestGapUnknownCommittedPayload(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	dest := strings.Repeat("cd", 16)
	r, _ := commitPacked(t, root, h, dest)
	extra := filepath.Join(root, dest, "unexpected")
	if err := os.WriteFile(extra, bytes.Repeat([]byte("Z"), 9000), 0600); err != nil {
		t.Fatal(err)
	}
	r.CrashCut()
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("unknown committed payload admitted")
	}
	st, err := os.Stat(extra)
	if err != nil || st.Size() != 9000 {
		t.Fatalf("payload not preserved %+v %v", st, err)
	}
}

func TestGapMeasureSuccessfulIndex(t *testing.T) {
	buf := newLaunchBuf()
	if _, err := buf.Write([]byte("git version 2.50.1\n")); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "git version 2.50.1\n" {
		t.Fatalf("capture %q", buf.String())
	}
	buf.finish()
	buf.finish()
	select {
	case <-buf.done:
	default:
		t.Fatal("capture did not finish")
	}

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
	if err := r.WritePack(id, []byte{1, 2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	idx := filepath.Join(root, id, "objects", "pack", "input.idx")
	if err := os.WriteFile(idx, []byte("idx!"), 0444); err != nil {
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
	r.gitRole = RoleIndex
	r.gitGen = id
	r.gitDone = done
	if err := r.Quiesce(id); err != nil {
		t.Fatal(err)
	}
	st, frozen, _, err := r.State(id)
	if err != nil || frozen || st != stateQuiescent {
		t.Fatalf("measured state %d frozen %v %v", st, frozen, err)
	}
	var pack, index uint64
	for _, s := range r.slots {
		if s.ID == id {
			pack, index = s.Pack, s.Index
		}
	}
	if pack != 4 || index != 4 {
		t.Fatalf("measured C inputs pack %d idx %d", pack, index)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGapQuiesceKillsSuccessfulChild(t *testing.T) {
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
	marker := filepath.Join(t.TempDir(), "done")
	cmd := exec.Command("/bin/sh", "-c", "sleep 0.3; touch "+marker+"; exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r.git = cmd
	r.gitRole = RoleVersion
	r.gitGen = id
	if err := r.Quiesce(id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("child killed before natural zero exit")
	}
	st, _, _, err := r.State(id)
	if err != nil || st != stateQuiescent {
		t.Fatalf("state %d %v", st, err)
	}
}

func TestGapNonzeroChildStillPromoted(t *testing.T) {
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
	idx := filepath.Join(root, id, "objects", "pack", "input.idx")
	if err := os.WriteFile(idx, []byte("partial"), 0444); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "exit 1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r.git = cmd
	r.gitRole = RoleIndex
	r.gitGen = id
	if err := r.Quiesce(id); err == nil {
		t.Fatal("nonzero child promoted")
	}
	st, _, _, err := r.State(id)
	if err != nil {
		t.Fatal(err)
	}
	if st == stateQuiescent || st == stateVerified || st == stateCommitted {
		t.Fatalf("promoted after cancel/nonzero %d", st)
	}
}

func TestGapQuiesceWrongGeneration(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	r, err := Open(root, h, RootBytes+Reserve*2)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
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
	idx := filepath.Join(root, id2, "objects", "pack", "input.idx")
	if err := os.WriteFile(idx, []byte("idx"), 0444); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = QuiesceGroup(cmd.Process.Pid, cmd.Wait) }()
	r.git = cmd
	r.gitRole = RoleIndex
	r.gitGen = id1
	if err := r.Quiesce(id2); err == nil {
		t.Fatal("measured a generation that does not own the git child")
	}
}

func TestGapCommitDropsChargeOnPersistFail(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	dest := strings.Repeat("cd", 16)
	r, id := commitPacked(t, root, h, dest)
	// commitPacked already committed. Build a verified slot instead.
	_ = id
	r.Close()
	root = privateRoot(t)
	r, err := Open(root, h, QuotaMin)
	if err != nil {
		t.Fatal(err)
	}
	id, err = r.Reserve("dom", "tip")
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
	before, err := r.Used()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	r.fs.callHook = func(op byte) error {
		if op == 8 {
			n++
			if n == 2 {
				return ErrUnsupported
			}
		}
		return nil
	}
	dest = strings.Repeat("ab", 16)
	err = r.Commit(id, dest)
	r.fs.callHook = nil
	if err == nil {
		t.Fatal("commit succeeded despite fsync failure")
	}
	st, _, _, serr := r.State(id)
	if serr != nil {
		t.Fatal(serr)
	}
	if st == stateCommitted {
		t.Fatal("in-memory committed after ambiguous persist")
	}
	after, aerr := r.Used()
	if aerr != nil || after != before {
		t.Fatalf("charge %d -> %d %v", before, after, aerr)
	}
	r.CrashCut()
	r, err = Open(root, h, QuotaMin)
	if err != nil {
		t.Fatalf("dest-only reservation denied: %v", err)
	}
	st, frozen, _, err := r.State(id)
	if err != nil || !frozen || st == stateCommitted {
		t.Fatalf("reopen state %d frozen %v %v", st, frozen, err)
	}
	used, uerr := r.Used()
	if uerr != nil || used < Reserve {
		t.Fatalf("reopen charge %d %v", used, uerr)
	}
	r.CrashCut()
	if _, err := os.Stat(filepath.Join(root, dest, "config")); err != nil {
		t.Fatal(err)
	}
}

func TestGapPublicationPinMissing(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	dest := strings.Repeat("cd", 16)
	r, id := commitPacked(t, root, h, dest)
	defer r.Close()
	_, _, n, err := r.State(id)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("publication pin %d, want 1", n)
	}
}

func TestGapTruncBeforeValidate(t *testing.T) {
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
	if err := os.Mkdir(filepath.Join(root, "g"), 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "g", "meta")
	if err := os.WriteFile(target, []byte("KEEP"), 0644); err != nil {
		t.Fatal(err)
	}
	h := &fsHelper{root: int(rf.Fd()), dev: uint64(st.Dev)}
	payload := []byte("g/meta\x00")
	var num [16]byte
	// max=8 off=0 data=ZZ
	num[0] = 8
	payload = append(payload, num[:]...)
	payload = append(payload, 'Z', 'Z')
	code, body := h.write(payload)
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if code == 0 || string(b) != "KEEP" {
		t.Fatalf("trunc before validate code %d body %q file %q", code, body, b)
	}
}

func TestGapDirModeNotCheckedOnOpenFD(t *testing.T) {
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
	dir := filepath.Join(root, "wide")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	saveWD(t)
	if err := syscall.Fchdir(int(rf.Fd())); err != nil {
		t.Fatal(err)
	}
	h := &fsHelper{root: int(rf.Fd()), dev: uint64(st.Dev)}
	if err := h.step("wide"); err == nil {
		t.Fatal("0755 directory fd accepted")
	}
}

func TestGapRequestWriteBlocks(t *testing.T) {
	root := privateRoot(t)
	rf, err := walkRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	script := filepath.Join(t.TempDir(), "acksleep")
	body := "#!/bin/sh\nprintf '\\0\\0\\0\\0\\0\\0\\0\\0'\nsleep 30\n"
	if err := os.WriteFile(script, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	fs, err := startFS(script, rf)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.cancelForTest()
	done := make(chan error, 1)
	go func() {
		_, err := fs.call(2, bytes.Repeat([]byte("x"), 1<<20))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked helper accepted the request")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request write blocked without a deadline")
	}
}

func TestGapDeadlineSetupIgnored(t *testing.T) {
	c := &fsClient{in: &memWriteCloser{}, out: deadlineFailReader{}}
	done := make(chan error, 1)
	go func() {
		_, err := c.call(2, []byte("x"))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("deadline setup failure was ignored")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("read blocked after deadline setup failed")
	}
}

func asSetterDiag(text string) bool {
	inherit, vsize, setter := false, false, false
	for _, ln := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(ln, "as-inherit soft=") && strings.Contains(ln, " hard="):
			inherit = true
		case strings.HasPrefix(ln, "as-inherit unavailable errno="):
			inherit = true
		case strings.HasPrefix(ln, "as-vsize unavailable errno="):
			vsize = true
		case strings.HasPrefix(ln, "as-vsize "):
			rest := strings.TrimPrefix(ln, "as-vsize ")
			if rest != "" && rest[0] >= '0' && rest[0] <= '9' {
				vsize = true
			}
		case strings.HasPrefix(ln, "as-errno="):
			fields := strings.Fields(ln)
			if len(fields) == 2 && strings.HasPrefix(fields[0], "as-errno=") && fields[1] != "" {
				setter = true
			}
		}
	}
	return inherit && vsize && setter
}

func TestGapASBranchMasked(t *testing.T) {
	cmd := exec.Command(helperBin(t), "__gitcache_as")
	cmd.Env = AllowEnv()
	out, err := cmd.CombinedOutput()
	text := string(out)
	switch {
	case bytes.Contains(out, []byte("as-branch=setter")):
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 || !bytes.Contains(out, []byte("as-reject")) || !asSetterDiag(text) || bytes.Contains(out, []byte("as-map")) {
			t.Fatalf("setter failure diagnostics: %v %s", err, text)
		}
	case bytes.Contains(out, []byte("as-enforced")), bytes.Contains(out, []byte("as-ineffective")):
	default:
		t.Fatalf("AS result has no branch marker: %s", text)
	}
	fail := exec.Command(helperBin(t), "__gitcache_git_failas")
	fail.Env = AllowEnv()
	fout, _ := fail.CombinedOutput()
	if !bytes.Contains(fout, []byte("as-branch=overmap")) {
		t.Fatalf("over-map sentinel did not run: %s", fout)
	}
}

func TestGapDuplicateIDsDestsAndCross(t *testing.T) {
	h := helperBin(t)
	dest := strings.Repeat("cd", 16)
	root := privateRoot(t)
	r, id := commitPacked(t, root, h, dest)
	r.CrashCut()
	payload := filepath.Join(root, dest, "config")
	before, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	other := strings.Repeat("ab", 16)
	rewriteSlots(t, root, func(slots []Slot) {
		slots[1] = slots[0]
		slots[1].ID = other
	})
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("duplicate dest admitted")
	}
	if b, err := os.ReadFile(payload); err != nil || !bytes.Equal(b, before) {
		t.Fatalf("duplicate dest changed payload %v", err)
	}

	root = privateRoot(t)
	r, id = commitPacked(t, root, h, dest)
	r.CrashCut()
	rewriteSlots(t, root, func(slots []Slot) {
		slots[1] = slots[0]
		slots[1].DestID = strings.Repeat("ef", 16)
	})
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("duplicate id admitted")
	}
	if _, err := os.Stat(filepath.Join(root, id)); err == nil {
		t.Fatal("duplicate id reclaimed the source")
	}

	root = privateRoot(t)
	r, _ = commitPacked(t, root, h, dest)
	r.CrashCut()
	rewriteSlots(t, root, func(slots []Slot) {
		slots[1] = slots[0]
		slots[1].ID = slots[0].DestID
		slots[1].DestID = strings.Repeat("ef", 16)
	})
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("cross collision admitted")
	}
	if _, err := os.Stat(filepath.Join(root, dest)); err != nil {
		t.Fatal(err)
	}
}

func TestGapCommittedBoundsAndFD(t *testing.T) {
	h := helperBin(t)
	dest := strings.Repeat("cd", 16)
	root := privateRoot(t)
	r, _ := commitPacked(t, root, h, dest)
	r.CrashCut()
	cfg := filepath.Join(root, dest, "config")
	huge := bytes.Repeat([]byte("Q"), int(MetaConfig)+1)
	if err := os.WriteFile(cfg, huge, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("huge metadata admitted")
	}
	if b, err := os.ReadFile(cfg); err != nil || !bytes.Equal(b, huge) {
		t.Fatal("huge metadata was rewritten")
	}

	root = privateRoot(t)
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
	stage := filepath.Join(root, id, "objects", "pack", "input.pack")
	f, err := os.OpenFile(stage, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(PackMax) + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	r.CrashCut()
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("huge stage pack admitted")
	}
	st, err := os.Stat(stage)
	if err != nil || st.Size() != int64(PackMax)+1 {
		t.Fatalf("stage pack mutated %+v %v", st, err)
	}
	root = privateRoot(t)
	r, err = Open(root, h, QuotaMin)
	if err != nil {
		t.Fatal(err)
	}
	id, err = r.Reserve("dom", "tip")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Activate(id); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, id, "scratch")
	if err := os.WriteFile(scratch, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	r.CrashCut()
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("unknown stage scratch admitted")
	}
	if b, err := os.ReadFile(scratch); err != nil || string(b) != "keep" {
		t.Fatal("scratch mutated")
	}

	root = privateRoot(t)
	r, _ = commitPacked(t, root, h, dest)
	r.CrashCut()
	raw, err := os.ReadFile(filepath.Join(root, "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	bad := Slot{ID: strings.Repeat("ab", 16), Domain: "d", Tip: "t", State: 9}
	if err := putSlot(raw[256:512], bad); err != nil {
		t.Fatal(err)
	}
	sum := hashLedger(raw)
	copy(raw[32:64], sum[:])
	if err := os.WriteFile(filepath.Join(root, "ledger"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("invalid state admitted")
	}

	root = privateRoot(t)
	r, _ = commitPacked(t, root, h, dest)
	r.CrashCut()
	cfg = filepath.Join(root, dest, "config")
	if err := os.Chmod(cfg, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("0644 metadata admitted")
	}

	root = privateRoot(t)
	r, _ = commitPacked(t, root, h, dest)
	r.CrashCut()
	cfg = filepath.Join(root, dest, "config")
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Link(cfg, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("nlink metadata admitted")
	}
	if _, err := os.Stat(alias); err != nil {
		t.Fatal(err)
	}

	root = privateRoot(t)
	r, _ = commitPacked(t, root, h, dest)
	r.CrashCut()
	cfg = filepath.Join(root, dest, "config")
	if err := os.Remove(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cfg, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("directory swapped for metadata")
	}
	if st, err := os.Stat(cfg); err != nil || !st.IsDir() {
		t.Fatal("swapped directory removed")
	}
}

func rewriteSlots(t *testing.T, root string, fn func([]Slot)) {
	t.Helper()
	p := filepath.Join(root, "ledger")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	hdr, slots, err := parseLedger(b)
	if err != nil {
		t.Fatal(err)
	}
	fn(slots)
	out, err := writeLedger(hdr.Quota, slots)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, out, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestGapCPUUnattributed(t *testing.T) {
	if cpuTerminationOK(0, "", true, 0) {
		t.Fatal("bare SIGKILL accepted")
	}
	// A SIGKILL with no setup marker must not count. The current helper
	// acceptance lives in TestLimitsAndQuiescence. This control calls the
	// shared predicate once it exists; until then the direct helper must
	// print a setup readback before any signal is accepted.
	cmd := exec.Command(helperBin(t), "__gitcache_cpu")
	cmd.Env = AllowEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-time.After(20 * time.Second):
		_ = QuiesceGroup(cmd.Process.Pid, func() error {
			waitErr = <-done
			return waitErr
		})
		t.Fatal("cpu helper exceeded bound without a setup marker")
	}
	text := buf.String()
	if !strings.Contains(text, "cpu-ready") {
		t.Fatalf("cpu termination is not attributable: %v %s", waitErr, text)
	}
}

func TestGapMultiChunkPhysical(t *testing.T) {
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
	calls := 0
	r.fs.callHook = func(op byte) error {
		if op != 1 {
			return nil
		}
		calls++
		if calls >= 2 {
			return ErrUnsupported
		}
		return nil
	}
	data := bytes.Repeat([]byte("a"), (256<<10)+10)
	err = r.WritePack(id, data)
	r.fs.callHook = nil
	if err == nil {
		t.Fatal("interrupted multi-chunk write succeeded")
	}
	st, frozen, _, serr := r.State(id)
	if serr != nil || !frozen || st == stateCommitted {
		t.Fatalf("freeze %d %v %v", st, frozen, serr)
	}
	info, ierr := os.Stat(filepath.Join(root, id, "objects", "pack", "input.pack"))
	if ierr != nil || info.Size() == 0 {
		t.Fatalf("first chunk not physical %+v %v", info, ierr)
	}
}

type deadlineFailReader struct{}

func (deadlineFailReader) Read([]byte) (int, error) {
	time.Sleep(30 * time.Second)
	return 0, io.EOF
}

func (deadlineFailReader) SetReadDeadline(time.Time) error {
	return errors.New("deadline unsupported")
}

func (c *fsClient) cancelForTest() {
	if c == nil || c.pgid <= 1 {
		return
	}
	_ = syscall.Kill(-c.pgid, syscall.SIGKILL)
	if c.cmd != nil {
		_ = c.cmd.Wait()
	}
}

func cpuTerminationOK(code int, text string, signaledKill bool, cpuTime time.Duration) bool {
	if !strings.Contains(text, "cpu-ready") {
		return false
	}
	if code == 1 && strings.Contains(text, "cpu-enforced SIGXCPU") && !signaledKill {
		return true
	}
	if signaledKill && strings.Contains(text, "hard=8") && cpuTime >= 8*time.Second {
		return true
	}
	return false
}

func TestGapBareSIGKILLRejected(t *testing.T) {
	if cpuTerminationOK(0, "", true, 0) {
		t.Fatal("bare SIGKILL accepted")
	}
	if !cpuTerminationOK(1, "cpu-ready\ncpu-enforced SIGXCPU\n", false, 0) {
		t.Fatal("attributable SIGXCPU rejected")
	}
}

func TestGapFsyncAfterPhysicalWrite(t *testing.T) {
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
	helperFsyncFault = func() error { return errors.New("fsync") }
	defer func() { helperFsyncFault = nil }()
	payload := []byte("synced\x00")
	var num [16]byte
	num[0] = 8
	payload = append(payload, num[:]...)
	payload = append(payload, 'a', 'b', 'c', 'd')
	code, body := h.write(payload)
	if len(body) < 4 {
		t.Fatalf("count %q", body)
	}
	n := int(body[0])
	info, err := os.Stat(filepath.Join(root, "synced"))
	if err != nil {
		t.Fatal(err)
	}
	if code == 0 || n != 4 || info.Size() != 4 {
		t.Fatalf("fsync-after-write code %d n %d size %d", code, n, info.Size())
	}
}

func TestGapShortPhysicalWrite(t *testing.T) {
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
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	defer syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old)
	lim := old
	lim.Cur = 4
	signal.Ignore(syscall.SIGXFSZ)
	defer signal.Reset(syscall.SIGXFSZ)
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lim); err != nil {
		t.Fatal(err)
	}
	payload := []byte("short\x00")
	var num [16]byte
	num[0] = 100
	payload = append(payload, num[:]...)
	payload = append(payload, bytes.Repeat([]byte("b"), 50)...)
	code, body := h.write(payload)
	if len(body) != 4 {
		t.Fatalf("count missing %q", body)
	}
	n := int(body[0]) | int(body[1])<<8 | int(body[2])<<16 | int(body[3])<<24
	info, err := os.Stat(filepath.Join(root, "short"))
	if err != nil {
		t.Fatal(err)
	}
	if code == 0 || n <= 0 || int64(n) != info.Size() || info.Size() >= 50 {
		t.Fatalf("physical short write code %d n %d size %d", code, n, info.Size())
	}
	fmt.Fprintf(os.Stderr, "short-write n=%d size=%d code=%d\n", n, info.Size(), code)
}
