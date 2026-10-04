//go:build gitcache_ci

package gitcache

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestNativeFixture fails closed when the pinned Git build, its identity, or
// RLIMIT_AS enforcement is missing. It must not skip.
func TestNativeFixture(t *testing.T) {
	git := os.Getenv("GITCACHE_GIT")
	idPath := os.Getenv("GITCACHE_IDENTITY")
	helper := os.Getenv("GITCACHE_HELPER")
	if git == "" || idPath == "" || helper == "" {
		t.Fatal("pinned fixture inputs missing")
	}
	text, err := os.ReadFile(idPath)
	if err != nil {
		t.Fatal(err)
	}
	vals := map[string]string{}
	for _, line := range strings.Split(string(text), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			vals[k] = v
		}
	}
	major, _ := strconv.Atoi(vals["darwin_major"])
	id := BuildIdentity{
		SourceSHA256: vals["source_sha256"],
		Flags:        strings.Split(vals["flags"], ","),
		OS:           vals["os"],
		Arch:         vals["arch"],
		Compiler:     vals["compiler"],
		BinarySHA256: vals["binary_sha256"],
		VersionLine:  vals["version_line"],
	}
	gf, err := os.Open(git)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, gf); err != nil {
		gf.Close()
		t.Fatal(err)
	}
	gf.Close()
	if hex.EncodeToString(sum.Sum(nil)) != id.BinarySHA256 {
		t.Errorf("git hash does not match the recorded identity")
	}
	if _, err := os.Stat(helper); err != nil || helper == "" {
		t.Fatal("selected helper missing")
	}
	if err := IdentityOK(id, vals["kernel"], major); err != nil {
		t.Errorf("fixture identity rejected: %v compiler=%q os=%q kernel=%q darwin_major=%d version=%q binary=%q",
			err, id.Compiler, id.OS, vals["kernel"], major, id.VersionLine, id.BinarySHA256)
	}
	cmd := exec.Command(helper, "__gitcache_as")
	cmd.Env = AllowEnv()
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "as-enforced") || strings.Contains(string(out), "as-set") {
		t.Errorf("RLIMIT_AS %d virtual map was not enforced: %v %s", LimitAS, err, out)
	}
	cmd = exec.Command(helper, "__gitcache_fsize")
	cmd.Env = AllowEnv()
	out, err = cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "fsize-enforced") {
		t.Fatalf("FSIZE not enforced: %v %s", err, out)
	}
	cmd = exec.Command(helper, "__gitcache_nofile")
	cmd.Env = AllowEnv()
	out, err = cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "nofile-enforced") {
		t.Fatalf("NOFILE not enforced: %v %s", err, out)
	}
	wrongRoot := privateRoot(t)
	rf, err := walkRoot(wrongRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	if _, err := LaunchGit(RoleVersion, "/usr/bin/true", git, "gen", "", rf, rf); err == nil {
		t.Fatal("wrong helper accepted")
	}
	nativeVersionAndIndex(t, helper, git, id, vals["kernel"], major)
}

func nativeVersionAndIndex(t *testing.T, helper, git string, idn BuildIdentity, kernel string, major int) {
	t.Helper()
	root := privateRoot(t)
	r, err := Open(root, helper, RootBytes+Reserve*2)
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
	buf := newLaunchBuf()
	old := launchCapture
	launchCapture = buf
	t.Cleanup(func() { launchCapture = old })
	if err := r.Launch(RoleVersion, git, id, "", idn, kernel, major); err != nil {
		t.Fatal(err)
	}
	if err := r.Quiesce(id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-buf.done:
	case <-time.After(roleWait):
		t.Fatal("version stdout drain did not finish")
	}
	if strings.TrimSpace(buf.String()) != idn.VersionLine {
		t.Fatalf("version %q want %q", buf.String(), idn.VersionLine)
	}

	id2, err := r.Reserve("dom2", "tip")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Activate(id2); err != nil {
		t.Fatal(err)
	}
	pack, err := PackV2EmptyBlob()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.WritePack(id2, pack); err != nil {
		t.Fatal(err)
	}
	if err := r.Launch(RoleIndex, git, id2, "", idn, kernel, major); err != nil {
		t.Fatal(err)
	}
	if err := r.Quiesce(id2); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = r.State(id2)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(privateJoin(root, id2, "objects", "pack", "input.idx"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0444 && st.Mode().Perm() != 0600 {
		t.Fatalf("idx mode %o", st.Mode().Perm())
	}
	if st.Size() == 0 {
		t.Fatal("empty idx")
	}
	charge, err := CommittedCharge(uint64(len(pack)), uint64(st.Size()))
	if err != nil || charge != uint64(len(pack))+uint64(st.Size())+MetaMax {
		t.Fatalf("C %d %v", charge, err)
	}
}

func privateJoin(elem ...string) string {
	return strings.Join(elem, "/")
}
