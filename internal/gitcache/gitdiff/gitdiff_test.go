package gitdiff

import (
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	fdiff "github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/pack"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/tree"
)

// fileSpec describes one blob (or gitlink) in a synthetic snapshot.
type fileSpec struct {
	mode string
	data string
	sha  string
}

func reg(data string) fileSpec  { return fileSpec{mode: tree.ModeFile, data: data} }
func exec(data string) fileSpec { return fileSpec{mode: tree.ModeExec, data: data} }
func link(sha string) fileSpec  { return fileSpec{mode: tree.ModeCommit, sha: sha} }

type store map[plumbing.Hash]pack.Object

func (s store) put(kind string, data []byte) plumbing.Hash {
	h := pack.HashObject(kind, data)
	s[h] = pack.Object{Type: kind, Data: data, Hash: h}
	return h
}

// snapshot writes a commit whose tree holds files (slash-separated paths).
func (s store) snapshot(t *testing.T, parent string, files map[string]fileSpec) string {
	t.Helper()
	root := s.tree(t, files, "")
	body := "tree " + root.String() + "\n"
	if parent != "" {
		body += "parent " + parent + "\n"
	}
	body += "author A <a@a> 1 +0000\ncommitter A <a@a> 1 +0000\n\nmsg\n"
	return s.put("commit", []byte(body)).String()
}

func (s store) tree(t *testing.T, files map[string]fileSpec, prefix string) plumbing.Hash {
	t.Helper()
	var entries []tree.TreeEntry
	dirs := map[string]map[string]fileSpec{}
	for p, f := range files {
		name, rest, nested := strings.Cut(p, "/")
		if nested {
			if dirs[name] == nil {
				dirs[name] = map[string]fileSpec{}
			}
			dirs[name][rest] = f
			continue
		}
		if f.mode == tree.ModeCommit {
			entries = append(entries, tree.TreeEntry{Mode: f.mode, Name: name, Hash: plumbing.NewHash(f.sha)})
			continue
		}
		entries = append(entries, tree.TreeEntry{Mode: f.mode, Name: name, Hash: s.put("blob", []byte(f.data))})
	}
	for name, sub := range dirs {
		entries = append(entries, tree.TreeEntry{Mode: tree.ModeTree, Name: name, Hash: s.tree(t, sub, prefix+name+"/")})
	}
	return s.put("tree", encodeTree(entries))
}

// encodeTree writes git tree bytes without the cache's name validation so
// fixtures can carry spaces and other legal path bytes.
func encodeTree(entries []tree.TreeEntry) []byte {
	key := func(e tree.TreeEntry) string {
		if e.Mode == tree.ModeTree {
			return e.Name + "/"
		}
		return e.Name
	}
	sort.Slice(entries, func(i, j int) bool { return key(entries[i]) < key(entries[j]) })
	var body []byte
	for _, e := range entries {
		body = append(body, e.Mode+" "+e.Name+"\x00"...)
		body = append(body, e.Hash[:]...)
	}
	return body
}

func bg() context.Context { return context.Background() }

func TestCompareKindsPathsAndModes(t *testing.T) {
	s := store{}
	sub1, sub2 := strings.Repeat("a", 40), strings.Repeat("b", 40)
	lines := strings.Repeat("line of rename-me content\n", 20)
	base := s.snapshot(t, "", map[string]fileSpec{
		"keep.txt":             reg("keep\n"),
		"gone.txt":             reg("gone\n"),
		"old name.txt":         reg(lines),
		"mode.txt":             reg("mode\n"),
		"bin.dat":              reg("a\x00b"),
		"file with spaces.txt": reg("spaces\n"),
		"dir/sub/deep.txt":     reg("deep\n"),
		"vendor/mod":           link(sub1),
	})
	head := s.snapshot(t, base, map[string]fileSpec{
		"keep.txt":             reg("keep\n"),
		"new name.txt":         reg(lines),
		"mode.txt":             exec("mode\n"),
		"added.txt":            reg("add\n"),
		"bin.dat":              reg("a\x00c"),
		"file with spaces.txt": reg("spaces-changed\n"),
		"dir/sub/deep.txt":     reg("deep-changed\n"),
		"vendor/mod":           link(sub2),
	})
	t.Setenv("PATH", "")
	cmp, err := Compare(bg(), base, head, SemanticsFullMR, s, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	res := cmp.Result
	if res.Partial || res.Command != RawCommand || res.From != base || res.To != head || res.Semantics != SemanticsFullMR {
		t.Fatalf("result %#v", res)
	}
	if res.RawBytes <= 0 {
		t.Fatalf("raw bytes %d", res.RawBytes)
	}
	byNew := map[string]File{}
	for _, f := range res.Files {
		byNew[f.NewPath] = f
	}
	if f, ok := byNew["added.txt"]; !ok || !f.NewFile || f.AMode != "0" || f.BMode != "100644" || f.Status != "A" {
		t.Fatalf("added %#v", res.Files)
	}
	if f, ok := byNew["gone.txt"]; !ok || !f.DeletedFile || f.BMode != "0" || f.Status != "D" || f.OldPath != "gone.txt" {
		t.Fatalf("deleted %#v", res.Files)
	}
	if f, ok := byNew["new name.txt"]; !ok || !f.RenamedFile || f.OldPath != "old name.txt" || f.Status != "R" {
		t.Fatalf("rename %#v", res.Files)
	}
	if f, ok := byNew["bin.dat"]; !ok || !f.Binary {
		t.Fatalf("binary %#v", res.Files)
	}
	if f, ok := byNew["file with spaces.txt"]; !ok || f.Binary || f.Submodule {
		t.Fatalf("spaces %#v", res.Files)
	}
	if _, ok := byNew["dir/sub/deep.txt"]; !ok {
		t.Fatalf("nested path missing %#v", res.Files)
	}
	if f, ok := byNew["vendor/mod"]; !ok || !f.Submodule || f.AMode != "160000" {
		t.Fatalf("submodule %#v", res.Files)
	}
	if f, ok := byNew["mode.txt"]; !ok || f.AMode != "100644" || f.BMode != "100755" {
		t.Fatalf("mode %#v", res.Files)
	}
	if _, ok := byNew["keep.txt"]; ok {
		t.Fatalf("unchanged file listed: %#v", res.Files)
	}
}

func TestCompareMissingCommitFailsClosed(t *testing.T) {
	cmp, err := Compare(bg(), strings.Repeat("a", 40), strings.Repeat("b", 40), SemanticsStraight, store{}, Limits{})
	if !errors.Is(err, ErrObject) || cmp == nil {
		t.Fatalf("err=%v", err)
	}
}

func TestCompareMissingTreeOrBlobFailsClosed(t *testing.T) {
	s := store{}
	base := s.snapshot(t, "", map[string]fileSpec{"a.txt": reg("1\n")})
	head := s.snapshot(t, base, map[string]fileSpec{"a.txt": reg("2\n")})
	for h, o := range s {
		if o.Type == "tree" {
			delete(s, h)
			break
		}
	}
	if _, err := Compare(bg(), base, head, SemanticsStraight, s, Limits{}); !errors.Is(err, ErrObject) {
		t.Fatalf("missing tree: %v", err)
	}
}

func TestCompareRejectsTamperedObject(t *testing.T) {
	s := store{}
	base := s.snapshot(t, "", map[string]fileSpec{"a.txt": reg("1\n")})
	head := s.snapshot(t, base, map[string]fileSpec{"a.txt": reg("2\n")})
	for h, o := range s {
		if o.Type == "tree" {
			o.Data = append([]byte(nil), o.Data...)
			o.Data[len(o.Data)-1] ^= 0xff
			s[h] = o
		}
	}
	if _, err := Compare(bg(), base, head, SemanticsStraight, s, Limits{}); !errors.Is(err, ErrObject) {
		t.Fatalf("tampered: %v", err)
	}
	other := pack.Object{Type: "commit", Data: []byte("x"), Hash: plumbing.NewHash(strings.Repeat("c", 40))}
	if err := RequireCommits(map[plumbing.Hash]pack.Object{other.Hash: other}, other.Hash.String()); err != nil {
		t.Fatalf("require commit: %v", err)
	}
}

func TestCompareOutputLimitIsPartial(t *testing.T) {
	objs, base, head := manyFiles(t, 80)
	cmp, err := Compare(bg(), base, head, SemanticsStraight, objs, Limits{MaxBytes: 64, Timeout: 5 * time.Second})
	if !errors.Is(err, ErrPartial) || !cmp.Partial || cmp.Reason != "output limit" {
		t.Fatalf("want partial: %#v %v", cmp.Result, err)
	}
	if cmp.RawBytes > 64 {
		t.Fatalf("raw bytes %d exceed cap", cmp.RawBytes)
	}
}

func TestCompareTimeLimitIsPartial(t *testing.T) {
	objs, base, head := manyFiles(t, 80)
	cmp, err := Compare(bg(), base, head, SemanticsStraight, objs, Limits{MaxBytes: 8 << 20, Timeout: time.Nanosecond})
	if !errors.Is(err, ErrPartial) || !cmp.Partial || cmp.Reason != "time limit" {
		t.Fatalf("want time partial: %#v %v", cmp.Result, err)
	}
}

func TestCompareCanceledContext(t *testing.T) {
	objs, base, head := manyFiles(t, 5)
	ctx, cancel := context.WithCancel(bg())
	cancel()
	if _, err := Compare(ctx, base, head, SemanticsStraight, objs, Limits{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestCompareNilContext(t *testing.T) {
	objs, base, head := manyFiles(t, 3)
	//nolint:staticcheck // nil context is tolerated deliberately
	cmp, err := Compare(nil, base, head, SemanticsStraight, objs, Limits{})
	if err != nil || len(cmp.Files) != 3 {
		t.Fatalf("%v %#v", err, cmp.Result)
	}
	res, err := cmp.Patches(nil, nil, Limits{}) //nolint:staticcheck
	if err != nil || len(res.Patches) != 3 {
		t.Fatalf("%v %#v", err, res)
	}
}

func TestLargeManifestMetadata(t *testing.T) {
	objs, base, head := manyFiles(t, 3500)
	cmp, err := Compare(bg(), base, head, SemanticsFullMR, objs, Limits{MaxBytes: 16 << 20, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if len(cmp.Files) != 3500 {
		t.Fatalf("files=%d", len(cmp.Files))
	}
	for _, f := range cmp.Files {
		if f.NewPath == "" && f.OldPath == "" {
			t.Fatal("empty path")
		}
	}
}

func TestPatchAgreesWithManifest(t *testing.T) {
	s := store{}
	base := s.snapshot(t, "", map[string]fileSpec{"a.txt": reg("1\n2\n3\n"), "b.txt": reg("b\n")})
	head := s.snapshot(t, base, map[string]fileSpec{"a.txt": reg("1\nX\n3\n"), "b.txt": reg("b2\n")})
	cmp, err := Compare(bg(), base, head, SemanticsStraight, s, Limits{})
	if err != nil || len(cmp.Files) != 2 {
		t.Fatalf("%#v %v", cmp.Result, err)
	}
	res, err := cmp.Patches(bg(), []string{"a.txt"}, Limits{})
	if err != nil || res.Partial || res.Command != PatchCommand || len(res.Patches) != 1 {
		t.Fatalf("patch %#v err=%v", res, err)
	}
	p := res.Patches[0]
	if p.OldPath != "a.txt" || p.NewPath != "a.txt" || !strings.Contains(p.Text, "+X") || !strings.Contains(p.Text, "-2") {
		t.Fatalf("text %q", p.Text)
	}
	if res.Bytes != len(p.Text) {
		t.Fatalf("bytes %d vs %d", res.Bytes, len(p.Text))
	}
	all, err := cmp.Patches(bg(), nil, Limits{})
	if err != nil || len(all.Patches) != 2 {
		t.Fatalf("all %#v %v", all, err)
	}
}

func TestPatchPathsAreLiteralAndRenameAware(t *testing.T) {
	s := store{}
	lines := strings.Repeat("shared rename body\n", 30)
	base := s.snapshot(t, "", map[string]fileSpec{"old.txt": reg(lines), "other.txt": reg("o\n")})
	head := s.snapshot(t, base, map[string]fileSpec{"new.txt": reg(lines + "extra\n"), "other.txt": reg("o2\n")})
	cmp, err := Compare(bg(), base, head, SemanticsStraight, s, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	glob, err := cmp.Patches(bg(), []string{"*.txt"}, Limits{})
	if err != nil || len(glob.Patches) != 0 {
		t.Fatalf("globbing leaked: %#v %v", glob, err)
	}
	for _, side := range []string{"old.txt", "new.txt"} {
		res, err := cmp.Patches(bg(), []string{side}, Limits{})
		if err != nil || len(res.Patches) != 1 {
			t.Fatalf("side %s: %#v %v", side, res, err)
		}
		if p := res.Patches[0]; p.OldPath != "old.txt" || p.NewPath != "new.txt" || !strings.Contains(p.Text, "+extra") {
			t.Fatalf("side %s: %#v", side, p)
		}
	}
}

func TestPatchSkipsSubmodulesAndHandlesBinary(t *testing.T) {
	s := store{}
	base := s.snapshot(t, "", map[string]fileSpec{"vendor/mod": link(strings.Repeat("a", 40)), "bin.dat": reg("a\x00b")})
	head := s.snapshot(t, base, map[string]fileSpec{"vendor/mod": link(strings.Repeat("b", 40)), "bin.dat": reg("a\x00c")})
	cmp, err := Compare(bg(), base, head, SemanticsStraight, s, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := cmp.Patches(bg(), nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Patches) != 1 || res.Patches[0].NewPath != "bin.dat" {
		t.Fatalf("patches %#v", res.Patches)
	}
	res, err = cmp.Patches(bg(), []string{"vendor/mod"}, Limits{})
	if err != nil || len(res.Patches) != 0 {
		t.Fatalf("submodule patch: %#v %v", res, err)
	}
}

func TestPatchOutputLimitOmitsOversizedFile(t *testing.T) {
	s := store{}
	big := strings.Repeat("0123456789\n", 400)
	base := s.snapshot(t, "", map[string]fileSpec{"big.txt": reg(""), "small.txt": reg("s\n")})
	head := s.snapshot(t, base, map[string]fileSpec{"big.txt": reg(big), "small.txt": reg("t\n")})
	cmp, err := Compare(bg(), base, head, SemanticsStraight, s, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := cmp.Patches(bg(), nil, Limits{MaxBytes: 300})
	if !errors.Is(err, ErrPartial) || !res.Partial || res.Reason != "output limit" {
		t.Fatalf("want partial: %#v %v", res, err)
	}
	for _, p := range res.Patches {
		if p.NewPath == "big.txt" {
			t.Fatalf("truncated patch returned: %#v", p)
		}
	}
	if res.Bytes > 300 {
		t.Fatalf("bytes %d exceed cap", res.Bytes)
	}
	exhausted, err := cmp.Patches(bg(), nil, Limits{MaxBytes: 1})
	if !errors.Is(err, ErrPartial) || len(exhausted.Patches) != 0 {
		t.Fatalf("exhausted: %#v %v", exhausted, err)
	}
}

func TestPatchTimeAndCancel(t *testing.T) {
	objs, base, head := manyFiles(t, 5)
	cmp, err := Compare(bg(), base, head, SemanticsStraight, objs, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := cmp.Patches(bg(), nil, Limits{Timeout: time.Nanosecond})
	if !errors.Is(err, ErrPartial) || !res.Partial || res.Reason != "time limit" {
		t.Fatalf("time: %#v %v", res, err)
	}
	ctx, cancel := context.WithCancel(bg())
	cancel()
	if _, err := cmp.Patches(ctx, nil, Limits{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestObjectStoreContract(t *testing.T) {
	s := store{}
	base := s.snapshot(t, "", map[string]fileSpec{"a.txt": reg("1\n")})
	st := memStore(s)
	h := plumbing.NewHash(base)
	if err := st.HasEncodedObject(h); err != nil {
		t.Fatal(err)
	}
	if err := st.HasEncodedObject(plumbing.NewHash(strings.Repeat("e", 40))); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("has: %v", err)
	}
	if n, err := st.EncodedObjectSize(h); err != nil || n != int64(len(s[h].Data)) {
		t.Fatalf("size %d %v", n, err)
	}
	if _, err := st.EncodedObjectSize(plumbing.ZeroHash); err == nil {
		t.Fatal("size of missing object")
	}
	if _, err := st.EncodedObject(plumbing.BlobObject, h); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("type mismatch: %v", err)
	}
	if _, err := st.EncodedObject(plumbing.AnyObject, h); err != nil {
		t.Fatal(err)
	}
	it, err := st.IterEncodedObjects(plumbing.BlobObject)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	_ = it.ForEach(func(plumbing.EncodedObject) error { n++; return nil })
	if n != 1 {
		t.Fatalf("iter count %d", n)
	}
	all, err := st.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		t.Fatal(err)
	}
	n = 0
	_ = all.ForEach(func(plumbing.EncodedObject) error { n++; return nil })
	if n != len(s) {
		t.Fatalf("iter all %d want %d", n, len(s))
	}
	if st.NewEncodedObject() == nil {
		t.Fatal("new object")
	}
	if _, err := st.SetEncodedObject(st.NewEncodedObject()); err == nil {
		t.Fatal("store must be read-only")
	}
	if err := st.AddAlternate("x"); err == nil {
		t.Fatal("alternates must be refused")
	}
	bad := store{plumbing.NewHash(strings.Repeat("d", 40)): {Type: "bogus", Data: []byte("x"), Hash: plumbing.NewHash(strings.Repeat("d", 40))}}
	if _, err := memStore(bad).EncodedObject(plumbing.AnyObject, plumbing.NewHash(strings.Repeat("d", 40))); err == nil {
		t.Fatal("unknown type accepted")
	}
}

func TestRequireCommits(t *testing.T) {
	if err := RequireCommits(nil, ""); !errors.Is(err, ErrObject) {
		t.Fatalf("empty sha: %v", err)
	}
	if err := RequireCommits(map[plumbing.Hash]pack.Object{}, strings.Repeat("b", 40)); !errors.Is(err, ErrObject) {
		t.Fatalf("missing: %v", err)
	}
	h := plumbing.NewHash(strings.Repeat("a", 40))
	blob := map[plumbing.Hash]pack.Object{h: {Type: "blob", Data: []byte("x"), Hash: h}}
	if err := RequireCommits(blob, h.String()); !errors.Is(err, ErrObject) {
		t.Fatalf("non-commit: %v", err)
	}
}

func TestPackageNeverImportsExec(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v %d", err, len(files))
	}
	fset := token.NewFileSet()
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			switch strings.Trim(imp.Path.Value, `"`) {
			case "os/exec", "os":
				t.Fatalf("%s imports %s; comparison must stay in-process", name, imp.Path.Value)
			}
		}
	}
}

func TestRawRowSizeAndModeString(t *testing.T) {
	plain := rawRowSize(File{OldPath: "a", NewPath: "a", AMode: "100644", BMode: "100644", Status: "M"})
	renamed := rawRowSize(File{OldPath: "a", NewPath: "bb", AMode: "100644", BMode: "100644", Status: "R", RenamedFile: true})
	if renamed <= plain {
		t.Fatalf("rename row %d not larger than %d", renamed, plain)
	}
	if modeString(0) != "0" {
		t.Fatal("empty mode")
	}
}

func TestLimitsDefaults(t *testing.T) {
	l := Limits{}.apply(123)
	if l.MaxBytes != 123 || l.Timeout != defaultTimeout {
		t.Fatalf("%#v", l)
	}
	keep := Limits{MaxBytes: 5, Timeout: time.Second}.apply(123)
	if keep.MaxBytes != 5 || keep.Timeout != time.Second {
		t.Fatalf("%#v", keep)
	}
}

func manyFiles(t *testing.T, n int) (map[plumbing.Hash]pack.Object, string, string) {
	t.Helper()
	s := store{}
	files := map[string]fileSpec{}
	for i := 0; i < n; i++ {
		files[fmt.Sprintf("f-%04d.txt", i)] = reg("x\n")
	}
	base := s.snapshot(t, "", map[string]fileSpec{})
	head := s.snapshot(t, base, files)
	return s, base, head
}

func TestComparePathsWithQuotesAndControlBytes(t *testing.T) {
	s := store{}
	oneTree := func(name, data string) plumbing.Hash {
		return s.put("tree", encodeTree([]tree.TreeEntry{{Mode: tree.ModeFile, Name: name, Hash: s.put("blob", []byte(data))}}))
	}
	commit := func(root plumbing.Hash, parent string) string {
		body := "tree " + root.String() + "\n"
		if parent != "" {
			body += "parent " + parent + "\n"
		}
		return s.put("commit", []byte(body+"author A <a@a> 1 +0000\ncommitter A <a@a> 1 +0000\n\nm\n")).String()
	}
	quoted := `we "ird".txt`
	base := commit(oneTree(quoted, "one\n"), "")
	head := commit(oneTree(quoted, "two\n"), base)
	cmp, err := Compare(bg(), base, head, SemanticsStraight, s, Limits{})
	if err != nil || len(cmp.Files) != 1 || cmp.Files[0].NewPath != quoted {
		t.Fatalf("%v %#v", err, cmp.Result)
	}
	res, err := cmp.Patches(bg(), []string{quoted}, Limits{})
	if err != nil || len(res.Patches) != 1 || !strings.Contains(res.Patches[0].Text, "+two") {
		t.Fatalf("%v %#v", err, res)
	}

	badBase := commit(oneTree("new\nline.txt", "one\n"), "")
	badHead := commit(oneTree("new\nline.txt", "two\n"), badBase)
	if cmp, err := Compare(bg(), badBase, badHead, SemanticsStraight, s, Limits{}); err == nil || len(cmp.Files) != 0 {
		t.Fatalf("control-byte path must fail closed: %v %#v", err, cmp.Result)
	}
}

func TestPatchEmptyAddAndDeleteAreNotBinary(t *testing.T) {
	cases := map[string]struct{ before, after map[string]fileSpec }{
		"add":    {map[string]fileSpec{"keep.txt": reg("k\n")}, map[string]fileSpec{"keep.txt": reg("k\n"), "fresh.txt": reg("")}},
		"delete": {map[string]fileSpec{"keep.txt": reg("k\n"), "gone.txt": reg("")}, map[string]fileSpec{"keep.txt": reg("k\n")}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := store{}
			base := s.snapshot(t, "", tc.before)
			head := s.snapshot(t, base, tc.after)
			cmp, err := Compare(bg(), base, head, SemanticsStraight, s, Limits{})
			if err != nil || len(cmp.Files) != 1 || cmp.Files[0].Binary {
				t.Fatalf("files %#v %v", cmp.Files, err)
			}
			res, err := cmp.Patches(bg(), nil, Limits{})
			if err != nil || len(res.Patches) != 1 || res.Patches[0].Text != "" {
				t.Fatalf("empty file patch must be empty: %#v %v", res, err)
			}
		})
	}
}

func TestPatchBinaryAddStillFramedBinary(t *testing.T) {
	s := store{}
	base := s.snapshot(t, "", map[string]fileSpec{"keep.txt": reg("k\n")})
	head := s.snapshot(t, base, map[string]fileSpec{"keep.txt": reg("k\n"), "new.bin": reg("a\x00b")})
	cmp, err := Compare(bg(), base, head, SemanticsStraight, s, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := cmp.Patches(bg(), []string{"new.bin"}, Limits{})
	if err != nil || len(res.Patches) != 1 || !strings.Contains(res.Patches[0].Text, "Binary files") {
		t.Fatalf("patches %#v %v", res, err)
	}
}

func TestObjectStoreLiveCheckStopsReadsUntilCleared(t *testing.T) {
	s := store{}
	h := s.put("blob", []byte("x"))
	stop := errors.New("deadline")
	st := &objectStore{objs: s, live: func() error { return stop }}
	if _, err := st.EncodedObject(plumbing.AnyObject, h); !errors.Is(err, stop) {
		t.Fatalf("err=%v", err)
	}
	st.live = nil
	if _, err := st.EncodedObject(plumbing.AnyObject, h); err != nil {
		t.Fatal(err)
	}
}

// Rename detection hashes blobs through the object store, so a deadline that
// expires mid-detection must surface instead of letting it run to completion.
func TestRenameDetectionStopsWhenStoreGoesStale(t *testing.T) {
	s := store{}
	oldFiles, newFiles := map[string]fileSpec{}, map[string]fileSpec{}
	body := strings.Repeat("rename candidate line\n", 40)
	for i := 0; i < 20; i++ {
		oldFiles[fmt.Sprintf("old%02d.txt", i)] = reg(fmt.Sprintf("%s%d\n", body, i))
		newFiles[fmt.Sprintf("new%02d.txt", i)] = reg(fmt.Sprintf("%s%d!\n", body, i))
	}
	base := s.snapshot(t, "", oldFiles)
	head := s.snapshot(t, base, newFiles)

	reads := 0
	stale := errors.New("deadline")
	st := &objectStore{objs: s}
	from, err := commitTree(st, base)
	if err != nil {
		t.Fatal(err)
	}
	to, err := commitTree(st, head)
	if err != nil {
		t.Fatal(err)
	}
	opts := &object.DiffTreeOptions{DetectRenames: true, RenameScore: renameScore, RenameLimit: renameLimit}

	st.live = func() error {
		reads++
		if reads > 30 {
			return stale
		}
		return nil
	}
	if _, err := object.DiffTreeWithOptions(bg(), from, to, opts); !errors.Is(err, stale) {
		t.Fatalf("rename stage ignored the stale store: %v (reads=%d)", err, reads)
	}

	st.live = nil
	changes, err := object.DiffTreeWithOptions(bg(), from, to, opts)
	if err != nil || len(changes) == 0 {
		t.Fatalf("cleared store must work: %v", err)
	}
}

func TestCompareClearsLiveCheckSoPatchesOutliveDeadline(t *testing.T) {
	s := store{}
	base := s.snapshot(t, "", map[string]fileSpec{"a.txt": reg("1\n")})
	head := s.snapshot(t, base, map[string]fileSpec{"a.txt": reg("2\n")})
	ctx, cancel := context.WithCancel(bg())
	cmp, err := Compare(ctx, base, head, SemanticsStraight, s, Limits{})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	res, err := cmp.Patches(bg(), nil, Limits{})
	if err != nil || len(res.Patches) != 1 {
		t.Fatalf("patches %#v %v", res, err)
	}
}

// A large text diff must stop at the request deadline: go-git's own
// PatchContext diffs with a one-hour timeout and ignores ctx while doing so.
func TestPatchRenderHonorsDeadline(t *testing.T) {
	var oldB, newB strings.Builder
	for i := 0; i < 80000; i++ {
		fmt.Fprintf(&oldB, "old unique line %d\n", i)
		fmt.Fprintf(&newB, "new unique line %d\n", i)
	}
	s := store{}
	base := s.snapshot(t, "", map[string]fileSpec{"big.txt": reg(oldB.String())})
	head := s.snapshot(t, base, map[string]fileSpec{"big.txt": reg(newB.String())})
	cmp, err := Compare(bg(), base, head, SemanticsStraight, s, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res, err := cmp.Patches(bg(), []string{"big.txt"}, Limits{Timeout: 100 * time.Millisecond})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrPartial) || !res.Partial || res.Reason != "time limit" || len(res.Patches) != 0 {
		t.Fatalf("want time-limit partial without a patch: %#v %v", res, err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("render ignored the deadline: %v", elapsed)
	}
}

func TestPatchRenderCanceledContext(t *testing.T) {
	s := store{}
	base := s.snapshot(t, "", map[string]fileSpec{"a.txt": reg("1\n")})
	head := s.snapshot(t, base, map[string]fileSpec{"a.txt": reg("2\n")})
	cmp, err := Compare(bg(), base, head, SemanticsStraight, s, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(bg())
	cancel()
	if _, err := cmp.Patches(ctx, nil, Limits{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

// The deadline-aware patch must encode exactly what go-git's own patch does.
func TestChangePatchMatchesGoGitPatch(t *testing.T) {
	s := store{}
	lines := strings.Repeat("shared line\n", 20)
	base := s.snapshot(t, "", map[string]fileSpec{
		"mod.txt": reg(lines + "tail\n"), "del.txt": reg("bye\n"), "mode.sh": reg("#!/bin/sh\n"),
		"bin.dat": reg("a\x00b"), "empty.txt": reg(""),
	})
	head := s.snapshot(t, base, map[string]fileSpec{
		"mod.txt": reg(lines + "TAIL\nmore\n"), "add.txt": reg("hello\nworld\n"), "mode.sh": exec("#!/bin/sh\n"),
		"bin.dat": reg("a\x00c"), "empty.txt": reg("now text\n"),
	})
	cmp, err := Compare(bg(), base, head, SemanticsStraight, s, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cmp.changes) < 5 {
		t.Fatalf("changes=%d", len(cmp.changes))
	}
	for _, ch := range cmp.changes {
		mine, err := changePatch(bg(), ch)
		if err != nil {
			t.Fatal(err)
		}
		theirs, err := ch.PatchContext(bg())
		if err != nil {
			t.Fatal(err)
		}
		var a, b strings.Builder
		if err := fdiff.NewUnifiedEncoder(&a, contextLines).Encode(mine); err != nil {
			t.Fatal(err)
		}
		if err := fdiff.NewUnifiedEncoder(&b, contextLines).Encode(theirs); err != nil {
			t.Fatal(err)
		}
		if a.String() != b.String() {
			t.Fatalf("%s -> %s differs:\n%q\nvs\n%q", ch.From.Name, ch.To.Name, a.String(), b.String())
		}
	}
}
