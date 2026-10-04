//go:build linux || darwin

package gitcache

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func saveWD(t *testing.T) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
}

func writePayload(rel string, max, off uint64, data []byte) []byte {
	var num [16]byte
	binary.LittleEndian.PutUint64(num[0:8], max)
	binary.LittleEndian.PutUint64(num[8:16], off)
	buf := append([]byte(rel), 0)
	buf = append(buf, num[:]...)
	return append(buf, data...)
}

func TestHelperOpsAndProcessBranches(t *testing.T) {
	saveWD(t)
	root := privateRoot(t)
	fd, err := syscall.Open(root, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		t.Fatal(err)
	}
	h := &fsHelper{root: fd, dev: uint64(st.Dev)}

	if code, _ := h.op(3, []byte("objects")); code != 0 {
		t.Fatalf("mkdir %d", code)
	}
	if code, _ := h.op(3, []byte("objects/pack")); code != 0 {
		t.Fatalf("mkdir pack %d", code)
	}
	if code, body := h.op(1, writePayload("objects/pack/input.pack", 64, 0, []byte("pack"))); code != 0 || len(body) != 4 {
		t.Fatalf("write %d %v", code, body)
	}
	if code, body := h.op(2, []byte("objects/pack/input.pack")); code != 0 || string(body) != "pack" {
		t.Fatalf("read %d %q", code, body)
	}
	if code, _ := h.op(1, writePayload("objects/pack/input.pack", 64, 4, []byte("MORE"))); code != 0 {
		t.Fatalf("append %d", code)
	}
	if code, _ := h.op(6, []byte("objects/pack/input.pack")); code != 0 {
		t.Fatal("lstat")
	}
	if code, body := h.op(7, []byte("objects")); code != 0 || !strings.Contains(string(body), "pack") {
		t.Fatalf("list %d %q", code, body)
	}
	if code, _ := h.op(4, []byte("objects/pack/input.pack\x00objects/pack/input.idx")); code != 0 {
		t.Fatal("rename")
	}
	if code, _ := h.op(5, []byte("objects/pack/input.idx")); code != 0 {
		t.Fatal("unlink")
	}
	if code, _ := h.op(8, nil); code != 0 {
		t.Fatal("fsync")
	}
	if code, _ := h.op(99, nil); code == 0 {
		t.Fatal("unknown op")
	}
	if code, _ := h.op(1, []byte("x")); code == 0 {
		t.Fatal("short write")
	}
	if code, _ := h.op(1, writePayload("objects/pack/input.pack", 2, 0, []byte("abcd"))); code == 0 {
		t.Fatal("over max")
	}
	if code, _ := h.op(2, []byte("missing")); code == 0 {
		t.Fatal("missing read")
	}
	if code, _ := h.op(4, []byte("onlyone")); code == 0 {
		t.Fatal("bad rename")
	}
	if _, err := splitRel(""); err == nil {
		t.Fatal("empty rel")
	}
	if _, err := splitRel("a/../b"); err == nil {
		t.Fatal("dotdot")
	}
	if mapErr(nil) != 0 || mapErr(ErrBusy) != 1 || mapErr(ErrCorrupt) != 2 || mapErr(ErrPath) != 3 || mapErr(ErrQuota) != 4 || mapErr(io.EOF) != 5 {
		t.Fatal("mapErr")
	}
	if fsStatus(1) != ErrBusy || fsStatus(9) != ErrUnsupported {
		t.Fatal("fsStatus")
	}
	var buf bytes.Buffer
	if err := writeStatus(&buf, 0, []byte("ok")); err != nil || buf.Len() != 10 {
		t.Fatal(err)
	}
	if !componentOK("a.b_c-1") || componentOK(".") || componentOK("a/b") {
		t.Fatal("component")
	}

	if _, err := roleFSIZE(Role("no")); err != ErrUnsupported {
		t.Fatal(err)
	}
	if _, err := roleFSIZE(RoleIndex); err != nil {
		t.Fatal(err)
	}
	if _, err := roleArgv(RoleIndex, "/usr/bin/git", "tip"); err != ErrPath {
		t.Fatal(err)
	}
	if _, err := roleArgv(RoleCatFile, "/usr/bin/git", ""); err != nil {
		t.Fatal(err)
	}
	tip := strings.Repeat("cd", 20)
	if _, err := roleArgv(RoleRevList, "/usr/bin/git", tip); err != nil {
		t.Fatal(err)
	}
	if _, err := roleArgv(Role("no"), "/usr/bin/git", ""); err != ErrUnsupported {
		t.Fatal(err)
	}
	if !relativeGen("objects/pack") || relativeGen("") || relativeGen("/abs") {
		t.Fatal("relativeGen")
	}
	if len(GitEnv()) < len(AllowEnv()) {
		t.Fatal("GitEnv")
	}
	tok, err := ReadToken(bytes.NewReader([]byte{4, 0, 0, 0, 'a', 'b', 'c', 'd'}))
	if err != nil || string(tok) != "abcd" {
		t.Fatalf("token %q %v", tok, err)
	}
	if _, err := ReadToken(bytes.NewReader([]byte{255, 255, 0, 0})); err != ErrQuota {
		t.Fatal(err)
	}
	if err := QuiesceGroup(1, nil); err != ErrNotQuiescent {
		t.Fatal(err)
	}
	closePipes(nil)

	if err := enterGen("no-such"); err == nil {
		t.Fatal("enter missing")
	}
	if err := os.Mkdir(filepath.Join(root, "stage"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Chdir(root); err != nil {
		t.Fatal(err)
	}
	if err := enterGen("stage"); err != nil {
		t.Fatal(err)
	}

	err = EnforceVirtualAS(LimitAS)
	if err != nil && !errors.Is(err, errASIneffective) && err != ErrLimit {
		t.Fatal(err)
	}
	if err := EnforceFSIZE(4096); err != nil {
		t.Fatal(err)
	}
	var now syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &now); err != nil || now.Cur < 1<<40 {
		t.Fatalf("FSIZE not restored: %+v %v", now, err)
	}
	if _, err := EnforceNOFILE(64); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &now); err != nil || now.Cur < 1024 {
		t.Fatalf("NOFILE not restored: %+v %v", now, err)
	}
}

func TestMoreBranches(t *testing.T) {
	h := helperBin(t)
	if _, err := Open("relative", h, QuotaMin); err != ErrPath {
		t.Fatalf("relative %v", err)
	}
	if _, err := Open(privateRoot(t), h, 1); err != ErrQuota {
		t.Fatalf("quota %v", err)
	}
	root := privateRoot(t)
	if _, err := walkRoot(root + "/missing"); err != ErrPath {
		t.Fatal(err)
	}
	r, err := Open(root, h, RootBytes+Reserve*2)
	if err != nil {
		t.Fatal(err)
	}
	used, err := r.Used()
	if err != nil || used < RootBytes {
		t.Fatalf("used %d %v", used, err)
	}
	if _, err := r.Reserve("", "t"); err != ErrPath {
		t.Fatal(err)
	}
	id, err := r.Reserve("dom", "tip")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.WritePack(id, []byte{1}); err != ErrState {
		t.Fatal(err)
	}
	if err := r.WriteMeta(id, "config", []byte(AllowedConfig)); err != ErrState {
		t.Fatal(err)
	}
	if err := r.Activate(id); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteMeta(id, "config", []byte("trace2.eventTarget=1\n")); err != ErrAudit {
		t.Fatal(err)
	}
	if err := r.WriteMeta(id, "HEAD", []byte(AllowedHEAD)); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteMeta(id, "config", []byte(AllowedConfig)); err != nil {
		t.Fatal(err)
	}
	if err := r.Pin(id); err != ErrState {
		t.Fatal(err)
	}
	if err := r.Commit(id, strings.Repeat("ab", 16)); err != ErrState {
		t.Fatal(err)
	}
	if _, err := LaunchGit(RoleIndex, "git", id, "", r.rootFile, r.lockFile); err != ErrPath {
		t.Fatal(err)
	}
	if _, err := LaunchGit(RoleIndex, h, id, "", r.rootFile, r.lockFile); err != ErrLimit {
		t.Fatalf("launch %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != ErrClosed {
		t.Fatal(err)
	}
}

func TestEdgeCoverage(t *testing.T) {
	h := helperBin(t)
	root := privateRoot(t)
	if err := os.Symlink("ledger", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("symlink inside root")
	}
	root = privateRoot(t)
	if err := os.WriteFile(filepath.Join(root, strings.Repeat("ab", 16)), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, h, QuotaMin); err == nil {
		t.Fatal("hex file")
	}
	root = privateRoot(t)
	r, err := Open(root, h, RootBytes+Reserve*2)
	if err != nil {
		t.Fatal(err)
	}
	id, err := r.Reserve("d", "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Activate(id); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteMeta(id, "nope", []byte("z")); err == nil {
		t.Fatal("unknown meta")
	}
	if err := r.WriteMeta(id, "provenance", []byte("p")); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteMeta(id, "manifest", []byte("m")); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteMeta(id, "refs/heads/acquired", []byte("r")); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteIndex(id, []byte("idx")); err != nil {
		t.Fatal(err)
	}
	if err := r.Quiesce("missing"); err == nil {
		t.Fatal("missing quiesce")
	}
	if err := r.Verify(id); err == nil {
		t.Fatal("verify before quiesce")
	}
	if err := r.Quiesce(id); err != nil {
		t.Fatal(err)
	}
	if err := r.Commit(id, id); err == nil {
		t.Fatal("dest==id")
	}
	if err := r.Verify(id); err != nil {
		t.Fatal(err)
	}
	dest := strings.Repeat("ef", 16)
	if err := r.Commit(id, dest); err != nil {
		t.Fatal(err)
	}
	if err := r.Unpin(id); err == nil {
		t.Fatal("unpin zero")
	}
	for i := 0; i < MaxLeases; i++ {
		if err := r.Pin(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Pin(id); err != ErrBusy {
		t.Fatal(err)
	}
	if _, err := r.EvictLRU(); err != ErrBusy {
		t.Fatal(err)
	}
	r.draining = true
	if _, err := r.Reserve("other", "t"); err != ErrClosed {
		t.Fatal(err)
	}
	r.draining = false
	for i := 0; i < MaxLeases; i++ {
		if err := r.Unpin(id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.EvictLRU(); err != nil {
		t.Fatal(err)
	}
	if err := r.Launch(RoleIndex, h, id, "", BuildIdentity{}, "", 0); err == nil {
		t.Fatal("launch missing")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (failWriter) Close() error              { return nil }

type memWriteCloser struct{ bytes.Buffer }

func (m *memWriteCloser) Close() error { return nil }

func TestDenseBranches(t *testing.T) {
	saveWD(t)
	if _, err := walkRoot("//nope"); err != ErrPath {
		t.Fatal(err)
	}
	if _, err := walkRoot("/bad name"); err != ErrPath {
		t.Fatal(err)
	}
	loose := privateRoot(t)
	if err := os.Chmod(loose, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := walkRoot(loose); err != ErrPath {
		t.Fatal("group-writable root")
	}
	if err := os.Chmod(loose, 0700); err != nil {
		t.Fatal(err)
	}
	rf, err := walkRoot(loose)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := startFS("/no/such/gitcache-helper", rf); err == nil {
		t.Fatal("missing helper")
	}
	rf, err = walkRoot(loose)
	if err != nil {
		t.Fatal(err)
	}
	fs, err := startFS("", rf)
	if err != nil {
		t.Fatal(err)
	}
	fs.dead = true
	if _, err := fs.call(8, nil); err != ErrClosed {
		t.Fatal(err)
	}
	fs.dead = false
	if _, err := fs.call(8, make([]byte, 1<<20+1)); err != ErrQuota {
		t.Fatal(err)
	}
	if err := fs.stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.call(8, nil); err != ErrClosed {
		t.Fatal(err)
	}

	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[4:], 1<<20+1)
	c := &fsClient{in: &memWriteCloser{}, out: bytes.NewReader(hdr[:])}
	if _, err := c.call(2, nil); err != ErrCorrupt {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint32(hdr[:4], 2)
	binary.LittleEndian.PutUint32(hdr[4:], 0)
	c.out = bytes.NewReader(hdr[:])
	if _, err := c.call(2, nil); err != ErrCorrupt {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint32(hdr[:4], 3)
	c.out = bytes.NewReader(hdr[:])
	if _, err := c.call(2, nil); err != ErrPath {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint32(hdr[:4], 4)
	c.out = bytes.NewReader(hdr[:])
	if _, err := c.call(2, nil); err != ErrQuota {
		t.Fatal(err)
	}
	c.in = failWriter{}
	c.out = bytes.NewReader(nil)
	if _, err := c.call(2, []byte("x")); err == nil {
		t.Fatal("write fail")
	}
	c.in = &memWriteCloser{}
	if _, err := c.call(2, []byte("x")); err == nil {
		t.Fatal("short read")
	}
	if err := writeStatus(failWriter{}, 1, []byte("body")); err == nil {
		t.Fatal("status write")
	}
	drainLimit(io.NopCloser(bytes.NewReader(make([]byte, 8000))), 1<<20, 100)

	root := privateRoot(t)
	fd, err := syscall.Open(root, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		t.Fatal(err)
	}
	h := &fsHelper{root: fd, dev: uint64(st.Dev)}
	if err := os.Symlink("objects", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if code, _ := h.op(3, []byte("objects")); code != 0 {
		t.Fatal(code)
	}
	if code, _ := h.op(6, []byte("link")); code != 0 {
		t.Fatal("lstat link")
	}
	if code, _ := h.op(1, writePayload("link/x", 32, 0, []byte("z"))); code == 0 {
		t.Fatal("write through link")
	}
	if code, _ := h.op(2, []byte("objects")); code == 0 {
		t.Fatal("read dir")
	}
	if code, _ := h.op(2, []byte("link")); code == 0 {
		t.Fatal("read link")
	}
	if code, _ := h.op(3, []byte("objects")); code == 0 {
		t.Fatal("mkdir exists")
	}
	if code, _ := h.op(4, []byte("objects/missing\x00objects/other")); code == 0 {
		t.Fatal("rename missing")
	}
	if code, _ := h.op(4, []byte("/abs\x00objects/x")); code == 0 {
		t.Fatal("rename abs")
	}
	if code, _ := h.op(4, []byte("objects\x00objects/pack/nested")); code == 0 {
		t.Fatal("rename depth")
	}
	if code, _ := h.op(5, []byte("link")); code != 0 {
		t.Fatal("unlink link")
	}
	if code, _ := h.op(5, []byte("objects")); code != 0 {
		t.Fatal("unlink dir")
	}
	if code, _ := h.op(5, []byte("missing")); code == 0 {
		t.Fatal("unlink missing")
	}
	if code, _ := h.op(7, []byte("a/b/c/d/e/f/g/h/i")); code == 0 {
		t.Fatal("deep list")
	}
	if code, _ := h.op(7, []byte("missing")); code == 0 {
		t.Fatal("list missing")
	}
	bad := &fsHelper{root: -1, dev: 1}
	if code, _ := bad.op(8, nil); code == 0 {
		t.Fatal("fsync bad")
	}
	if code, _ := bad.op(1, writePayload("a", 8, 0, []byte("z"))); code == 0 {
		t.Fatal("write bad root")
	}
	if _, err := splitRel("a//b"); err == nil {
		t.Fatal("split")
	}
	if _, err := splitRel("a/b/c/d/e/f/g/h/i"); err == nil {
		t.Fatal("deep split")
	}
	if mapErr(syscall.EAGAIN) != 1 || mapErr(syscall.ELOOP) != 3 || mapErr(syscall.EFBIG) != 4 {
		t.Fatal("mapErr syscall")
	}
	if !componentOK("A") || componentOK(" ") {
		t.Fatal("component")
	}
	if _, err := roleArgv(RoleVersion, "/usr/bin/git", "tip"); err != ErrPath {
		t.Fatal(err)
	}
	if _, err := roleArgv(RoleCatFile, "/usr/bin/git", "tip"); err != ErrPath {
		t.Fatal(err)
	}
	if _, err := roleFSIZE(RoleVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := LaunchGit(Role("no"), "/usr/bin/git", "gen", "", nil, nil); err != ErrUnsupported {
		t.Fatal(err)
	}
	if _, err := LaunchGit(RoleIndex, "/usr/bin/git", "", "", nil, nil); err != ErrPath {
		t.Fatal(err)
	}
	if _, err := LaunchGit(RoleIndex, "/usr/bin/git", "/abs", "", nil, nil); err != ErrPath {
		t.Fatal(err)
	}
	if err := QuiesceGroup(1<<20, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadToken(bytes.NewReader(nil)); err != ErrCorrupt {
		t.Fatal(err)
	}
	if err := EnforceVirtualAS(1 << 20); err != ErrLimit {
		t.Fatal(err)
	}
	if err := enterGen(""); err == nil {
		t.Fatal("empty gen")
	}
	r, err := Open(privateRoot(t), helperBin(t), QuotaMin)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.State("missing"); err == nil {
		t.Fatal("state")
	}
	id, err := r.Reserve("dom", "tip")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Activate(id); err != nil {
		t.Fatal(err)
	}
	junk := filepath.Join(r.path, id, "unexpected")
	if err := os.WriteFile(junk, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.EvictLRU(); err == nil {
		t.Fatal("evict active")
	}
	if err := r.removeGen(id); err == nil {
		t.Fatal("unexpected name")
	}
	r.CrashCut()
}
