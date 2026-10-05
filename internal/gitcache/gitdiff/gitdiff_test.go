package gitdiff

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/pack"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/tree"
)

func requireGit(t *testing.T) {
	t.Helper()
	if err := LookPath(); err != nil {
		t.Skip(err.Error())
	}
}

func TestRawSpecialPathsAndKinds(t *testing.T) {
	requireGit(t)
	repo := t.TempDir()
	gitCmd(t, repo, "init", "-q", "--initial-branch=main")
	gitCmd(t, repo, "config", "user.email", "t@t")
	gitCmd(t, repo, "config", "user.name", "t")
	write(t, filepath.Join(repo, "keep.txt"), "keep\n")
	write(t, filepath.Join(repo, "gone.txt"), "gone\n")
	write(t, filepath.Join(repo, "old name.txt"), "rename-me\n")
	write(t, filepath.Join(repo, "mode.txt"), "mode\n")
	write(t, filepath.Join(repo, "bin.dat"), "a\x00b")
	write(t, filepath.Join(repo, "file with spaces.txt"), "spaces\n")
	nl := filepath.Join(repo, "new\nline.txt")
	write(t, nl, "nl\n")
	gitCmd(t, repo, "add", "-A")
	sub := strings.Repeat("a", 40)
	gitCmd(t, repo, "update-index", "--add", "--cacheinfo", "160000,"+sub+",vendor/mod")
	gitCmd(t, repo, "commit", "-qm", "base")
	base := strings.TrimSpace(string(gitCmd(t, repo, "rev-parse", "HEAD")))

	if err := os.Remove(filepath.Join(repo, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(repo, "old name.txt"), filepath.Join(repo, "new name.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(repo, "mode.txt"), "mode\n")
	if err := os.Chmod(filepath.Join(repo, "mode.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(repo, "added.txt"), "add\n")
	write(t, filepath.Join(repo, "bin.dat"), "a\x00c")
	write(t, filepath.Join(repo, "file with spaces.txt"), "spaces-changed\n")
	write(t, nl, "nl-changed\n")
	gitCmd(t, repo, "add", "-A")
	sub2 := strings.Repeat("b", 40)
	gitCmd(t, repo, "update-index", "--add", "--cacheinfo", "160000,"+sub2+",vendor/mod")
	gitCmd(t, repo, "commit", "-qm", "head")
	head := strings.TrimSpace(string(gitCmd(t, repo, "rev-parse", "HEAD")))

	gitDir := filepath.Join(repo, ".git")
	objs := loadObjects(t, gitDir)
	bare := t.TempDir()
	if err := WriteBare(context.Background(), bare, objs); err != nil {
		t.Fatal(err)
	}
	plantHook(t, bare)
	res, err := Raw(context.Background(), bare, base, head, SemanticsFullMR, objs, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Partial {
		t.Fatalf("partial: %#v", res)
	}
	if _, err := os.Stat(filepath.Join(bare, "hooks", "fired")); !os.IsNotExist(err) {
		t.Fatal("hook executed")
	}
	byNew := map[string]File{}
	for _, f := range res.Files {
		byNew[f.NewPath] = f
	}
	if f, ok := byNew["added.txt"]; !ok || !f.NewFile {
		t.Fatalf("added %#v", res.Files)
	}
	if f, ok := byNew["gone.txt"]; !ok || !f.DeletedFile {
		t.Fatalf("deleted %#v", res.Files)
	}
	if f, ok := byNew["new name.txt"]; !ok || !f.RenamedFile || f.OldPath != "old name.txt" {
		t.Fatalf("rename %#v", res.Files)
	}
	if f, ok := byNew["bin.dat"]; !ok || !f.Binary {
		t.Fatalf("binary %#v", res.Files)
	}
	if _, ok := byNew["file with spaces.txt"]; !ok {
		t.Fatalf("spaces missing %#v", res.Files)
	}
	if _, ok := byNew["new\nline.txt"]; !ok {
		t.Fatalf("newline path missing %#v", res.Files)
	}
	if f, ok := byNew["vendor/mod"]; !ok || !f.Submodule {
		t.Fatalf("submodule %#v", res.Files)
	}
	if f, ok := byNew["mode.txt"]; !ok || f.AMode == f.BMode {
		t.Fatalf("mode %#v", res.Files)
	}
}

func TestRawMissingCommitFailsClosed(t *testing.T) {
	requireGit(t)
	objs := map[plumbing.Hash]pack.Object{}
	_, err := Raw(context.Background(), t.TempDir(), strings.Repeat("a", 40), strings.Repeat("b", 40), SemanticsStraight, objs, Limits{})
	if !errorsIs(err, ErrObject) {
		t.Fatalf("err=%v", err)
	}
}

func TestRawOutputLimitIsPartial(t *testing.T) {
	requireGit(t)
	objs, base, head := manyFiles(t, 80)
	bare := t.TempDir()
	if err := WriteBare(context.Background(), bare, objs); err != nil {
		t.Fatal(err)
	}
	res, err := Raw(context.Background(), bare, base, head, SemanticsStraight, objs, Limits{MaxBytes: 64, Timeout: 5 * time.Second})
	if !errorsIs(err, ErrPartial) || !res.Partial {
		t.Fatalf("want partial: %#v %v", res, err)
	}
}

func TestRawTimeLimitIsPartial(t *testing.T) {
	requireGit(t)
	objs, base, head := manyFiles(t, 80)
	bare := t.TempDir()
	if err := WriteBare(context.Background(), bare, objs); err != nil {
		t.Fatal(err)
	}
	res, err := Raw(context.Background(), bare, base, head, SemanticsStraight, objs, Limits{MaxBytes: 8 << 20, Timeout: time.Nanosecond})
	if !errorsIs(err, ErrPartial) || !res.Partial {
		t.Fatalf("want time partial: %#v %v", res, err)
	}
}

func TestNoExternalTextconvOrDiff(t *testing.T) {
	requireGit(t)
	repo := t.TempDir()
	gitCmd(t, repo, "init", "-q", "--initial-branch=main")
	gitCmd(t, repo, "config", "user.email", "t@t")
	gitCmd(t, repo, "config", "user.name", "t")
	write(t, filepath.Join(repo, "x.txt"), "one\n")
	write(t, filepath.Join(repo, ".gitattributes"), "* diff=evil\n")
	gitCmd(t, repo, "add", "-A")
	gitCmd(t, repo, "commit", "-qm", "base")
	base := strings.TrimSpace(string(gitCmd(t, repo, "rev-parse", "HEAD")))
	write(t, filepath.Join(repo, "x.txt"), "two\n")
	gitCmd(t, repo, "commit", "-am", "head")
	head := strings.TrimSpace(string(gitCmd(t, repo, "rev-parse", "HEAD")))
	objs := loadObjects(t, filepath.Join(repo, ".git"))
	bare := t.TempDir()
	if err := WriteBare(context.Background(), bare, objs); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(bare, "evil.sh")
	write(t, script, "#!/bin/sh\necho fired > \"$(dirname \"$0\")/evil.out\"\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_EXTERNAL_DIFF", script)
	res, err := Raw(context.Background(), bare, base, head, SemanticsStraight, objs, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(bare, "evil.out")); !os.IsNotExist(err) {
		t.Fatal("external diff ran")
	}
	if len(res.Files) == 0 {
		t.Fatal("empty")
	}
}

func TestLargeManifestMetadata(t *testing.T) {
	requireGit(t)
	objs, base, head := manyFiles(t, 3500)
	bare := t.TempDir()
	if err := WriteBare(context.Background(), bare, objs); err != nil {
		t.Fatal(err)
	}
	res, err := Raw(context.Background(), bare, base, head, SemanticsFullMR, objs, Limits{MaxBytes: 16 << 20, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) < 3475 {
		t.Fatalf("files=%d", len(res.Files))
	}
	for _, f := range res.Files {
		if f.NewPath == "" && f.OldPath == "" {
			t.Fatal("empty path")
		}
	}
}

func TestPatchAgreesWithRaw(t *testing.T) {
	requireGit(t)
	repo := t.TempDir()
	gitCmd(t, repo, "init", "-q", "--initial-branch=main")
	gitCmd(t, repo, "config", "user.email", "t@t")
	gitCmd(t, repo, "config", "user.name", "t")
	write(t, filepath.Join(repo, "a.txt"), "1\n2\n3\n")
	gitCmd(t, repo, "add", "a.txt")
	gitCmd(t, repo, "commit", "-qm", "base")
	base := strings.TrimSpace(string(gitCmd(t, repo, "rev-parse", "HEAD")))
	write(t, filepath.Join(repo, "a.txt"), "1\nX\n3\n")
	gitCmd(t, repo, "commit", "-am", "head")
	head := strings.TrimSpace(string(gitCmd(t, repo, "rev-parse", "HEAD")))
	objs := loadObjects(t, filepath.Join(repo, ".git"))
	bare := t.TempDir()
	if err := WriteBare(context.Background(), bare, objs); err != nil {
		t.Fatal(err)
	}
	res, err := Raw(context.Background(), bare, base, head, SemanticsStraight, objs, Limits{})
	if err != nil || len(res.Files) != 1 {
		t.Fatalf("%#v %v", res, err)
	}
	text, partial, _, err := Patch(context.Background(), bare, base, head, []string{"a.txt"}, Limits{})
	if err != nil || partial || !strings.Contains(text, "X") {
		t.Fatalf("patch %q partial=%v err=%v", text, partial, err)
	}
}

func manyFiles(t *testing.T, n int) (map[plumbing.Hash]pack.Object, string, string) {
	t.Helper()
	blob := pack.Object{Type: "blob", Data: []byte("x\n")}
	blob.Hash = pack.HashObject("blob", blob.Data)
	headEntries := make([]tree.TreeEntry, 0, n)
	for i := 0; i < n; i++ {
		headEntries = append(headEntries, tree.TreeEntry{Mode: tree.ModeFile, Name: fmt.Sprintf("f-%04d.txt", i), Hash: blob.Hash})
	}
	tb, err := tree.EncodeTree(nil)
	if err != nil {
		t.Fatal(err)
	}
	th, err := tree.EncodeTree(headEntries)
	if err != nil {
		t.Fatal(err)
	}
	baseTree := pack.Object{Type: "tree", Data: tb, Hash: pack.HashObject("tree", tb)}
	headTree := pack.Object{Type: "tree", Data: th, Hash: pack.HashObject("tree", th)}
	baseBody := []byte("tree " + baseTree.Hash.String() + "\nauthor A <a@a> 1 +0000\ncommitter A <a@a> 1 +0000\n\nbase\n")
	base := pack.Object{Type: "commit", Data: baseBody, Hash: pack.HashObject("commit", baseBody)}
	headBody := []byte("tree " + headTree.Hash.String() + "\nparent " + base.Hash.String() + "\nauthor A <a@a> 1 +0000\ncommitter A <a@a> 1 +0000\n\nhead\n")
	head := pack.Object{Type: "commit", Data: headBody, Hash: pack.HashObject("commit", headBody)}
	objs := map[plumbing.Hash]pack.Object{
		blob.Hash: blob, baseTree.Hash: baseTree, headTree.Hash: headTree, base.Hash: base, head.Hash: head,
	}
	return objs, base.Hash.String(), head.Hash.String()
}

func loadObjects(t *testing.T, gitDir string) map[plumbing.Hash]pack.Object {
	t.Helper()
	out := gitCmd(t, "", "--git-dir="+gitDir, "cat-file", "--batch-check", "--batch-all-objects")
	objs := map[plumbing.Hash]pack.Object{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 || f[1] == "unreachable" {
			continue
		}
		kind, sha := f[1], f[0]
		if kind != "blob" && kind != "tree" && kind != "commit" {
			continue
		}
		cmd := exec.Command("git", "--git-dir="+gitDir, "cat-file", kind, sha)
		data, err := cmd.Output()
		if err != nil {
			t.Fatalf("cat-file %s %s: %v", kind, sha, err)
		}
		obj := pack.Object{Type: kind, Data: data, Hash: plumbing.NewHash(sha)}
		if pack.HashObject(kind, data) != obj.Hash {
			t.Fatalf("hash %s", sha)
		}
		objs[obj.Hash] = obj
	}
	if len(objs) == 0 {
		t.Fatal("no objects")
	}
	return objs
}

func plantHook(t *testing.T, gitDir string) {
	t.Helper()
	p := filepath.Join(gitDir, "hooks", "pre-commit")
	write(t, p, "#!/bin/sh\necho fired > \"$(dirname \"$0\")/fired\"\n")
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func gitCmd(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return bytes.TrimRight(out, "\n")
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func errorsIs(err, target error) bool {
	return err != nil && (err == target || strings.Contains(err.Error(), target.Error()))
}

func TestWriteBareAndRequireCommits(t *testing.T) {
	if err := WriteBare(context.Background(), t.TempDir(), nil); !errorsIs(err, ErrObject) {
		t.Fatalf("empty: %v", err)
	}
	h := plumbing.NewHash(strings.Repeat("a", 40))
	bad := map[plumbing.Hash]pack.Object{h: {Type: "blob", Data: []byte("x"), Hash: plumbing.ZeroHash}}
	if err := WriteBare(context.Background(), t.TempDir(), bad); !errorsIs(err, ErrObject) {
		t.Fatalf("mismatch: %v", err)
	}
	if err := RequireCommits(nil, ""); !errorsIs(err, ErrObject) {
		t.Fatalf("empty sha: %v", err)
	}
	if err := RequireCommits(map[plumbing.Hash]pack.Object{}, strings.Repeat("b", 40)); !errorsIs(err, ErrObject) {
		t.Fatalf("missing: %v", err)
	}
	if err := LookPath(); err != nil {
		t.Skip(err.Error())
	}
}

func TestSplitPatchesKeys(t *testing.T) {
	if len(SplitPatches("")) != 0 {
		t.Fatal("empty")
	}
	text := "diff --git a/old.txt b/new.txt\n--- a/old.txt\n+++ b/new.txt\n@@ -1 +1 @@\n-a\n+b\n"
	got := SplitPatches(text)
	if got["old.txt"] == "" || got["new.txt"] == "" {
		t.Fatalf("%#v", got)
	}
}

func TestSplitPatchesQuotedPaths(t *testing.T) {
	text := "diff --git \"a/file with spaces.txt\" \"b/file with spaces.txt\"\n--- \"a/file with spaces.txt\"\n+++ \"b/file with spaces.txt\"\n@@ -1 +1 @@\n-a\n+b\n"
	got := SplitPatches(text)
	if got["file with spaces.txt"] == "" {
		t.Fatalf("%#v", got)
	}
}

func TestWriteBareHonorsCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	blob := pack.Object{Type: "blob", Data: []byte("x")}
	blob.Hash = pack.HashObject("blob", blob.Data)
	if err := WriteBare(ctx, t.TempDir(), map[plumbing.Hash]pack.Object{blob.Hash: blob}); !errorsIs(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}
