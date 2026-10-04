//go:build gitcache_ci

package gitcache

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
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
	if _, err := os.Stat(git); err != nil {
		t.Fatal(err)
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
}
