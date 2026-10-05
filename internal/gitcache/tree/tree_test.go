package tree

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/pack"
)

func obj(kind string, data []byte) pack.Object {
	o := pack.Object{Type: kind, Data: data}
	o.Hash = pack.HashObject(kind, data)
	return o
}

func treeObj(t *testing.T, entries []TreeEntry) pack.Object {
	t.Helper()
	body, err := EncodeTree(entries)
	if err != nil {
		t.Fatal(err)
	}
	return obj("tree", body)
}

func commitObj(tree plumbing.Hash, parents ...plumbing.Hash) pack.Object {
	var buf bytes.Buffer
	buf.WriteString("tree " + tree.String() + "\n")
	for _, p := range parents {
		buf.WriteString("parent " + p.String() + "\n")
	}
	buf.WriteString("author Fixture <fixture@example> 1 +0000\n")
	buf.WriteString("committer Fixture <fixture@example> 1 +0000\n\nmsg\n")
	return obj("commit", buf.Bytes())
}

func TestNamesAndDuplicatesRejected(t *testing.T) {
	blob := obj("blob", []byte("x"))
	for _, name := range []string{".", "..", "a/b", "a\x00b", "a b"} {
		if _, err := EncodeTree([]TreeEntry{{Mode: ModeFile, Name: name, Hash: blob.Hash}}); !errors.Is(err, ErrName) {
			t.Errorf("%q: %v", name, err)
		}
	}
	_, err := EncodeTree([]TreeEntry{
		{Mode: ModeFile, Name: "a", Hash: blob.Hash},
		{Mode: ModeFile, Name: "a", Hash: blob.Hash},
	})
	if !errors.Is(err, ErrName) {
		t.Fatalf("duplicate: %v", err)
	}
}

func TestWalkCapsBeforePathGrowth(t *testing.T) {
	blob := obj("blob", []byte("x"))
	entries := make([]TreeEntry, bounds.MaxObjects+1)
	for i := range entries {
		entries[i] = TreeEntry{Mode: ModeFile, Name: "f" + strconvName(i), Hash: blob.Hash}
	}
	body, err := manualTree(entries[:bounds.MaxObjects])
	if err != nil {
		t.Fatal(err)
	}
	root := obj("tree", body)
	objs := Map{blob.Hash: blob, root.Hash: root}
	got, err := Walk(context.Background(), objs, root.Hash)
	if err != nil || len(got) != bounds.MaxObjects {
		t.Fatalf("cap walk len %d err %v", len(got), err)
	}
	over, err := manualTree(entries)
	if err != nil {
		t.Fatal(err)
	}
	overRoot := obj("tree", over)
	objs[overRoot.Hash] = blob
	if _, err := Walk(context.Background(), Map{blob.Hash: blob, overRoot.Hash: overRoot}, overRoot.Hash); !errors.Is(err, ErrEntries) {
		t.Fatalf("over: %v", err)
	}

	long := strings.Repeat("a", MaxPathBytes+1)
	longTree := treeObj(t, []TreeEntry{{Mode: ModeFile, Name: long, Hash: blob.Hash}})
	if _, err := Walk(context.Background(), Map{blob.Hash: blob, longTree.Hash: longTree}, longTree.Hash); !errors.Is(err, ErrName) && !errors.Is(err, ErrPath) {
		t.Fatalf("long name: %v", err)
	}

	cur := blob
	var top pack.Object
	objs = Map{blob.Hash: blob}
	for i := 0; i < MaxDepth+2; i++ {
		tr := treeObj(t, []TreeEntry{{Mode: ModeTree, Name: "d", Hash: cur.Hash}})
		if i == 0 {
			tr = treeObj(t, []TreeEntry{{Mode: ModeFile, Name: "d", Hash: blob.Hash}})
		}
		objs[tr.Hash] = tr
		cur = tr
		top = tr
	}
	if _, err := Walk(context.Background(), objs, top.Hash); !errors.Is(err, ErrDepth) {
		t.Fatalf("depth: %v", err)
	}
}

func TestGitlinkIsNotOpened(t *testing.T) {
	var link plumbing.Hash
	link[0] = 1
	root := treeObj(t, []TreeEntry{{Mode: ModeCommit, Name: "sub", Hash: link}})
	got, err := Walk(context.Background(), Map{root.Hash: root}, root.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if got["sub"].Mode != ModeCommit || got["sub"].Hash != link {
		t.Fatalf("%+v", got["sub"])
	}
}

func TestCompareRenameIsDeleteAndAdd(t *testing.T) {
	old := obj("blob", []byte("before"))
	edited := obj("blob", []byte("before-edited"))
	bin := obj("blob", []byte{0, 1})
	baseRoot := treeObj(t, []TreeEntry{{Mode: ModeFile, Name: "old-name.txt", Hash: old.Hash}})
	headRoot := treeObj(t, []TreeEntry{
		{Mode: ModeFile, Name: "new-name.txt", Hash: edited.Hash},
		{Mode: ModeFile, Name: "data.bin", Hash: bin.Hash},
		{Mode: ModeSym, Name: "link.txt", Hash: old.Hash},
		{Mode: ModeExec, Name: "run.sh", Hash: old.Hash},
	})
	base, err := Walk(context.Background(), Map{old.Hash: old, baseRoot.Hash: baseRoot}, baseRoot.Hash)
	if err != nil {
		t.Fatal(err)
	}
	head, err := Walk(context.Background(), Map{
		old.Hash: old, edited.Hash: edited, bin.Hash: bin, headRoot.Hash: headRoot,
	}, headRoot.Hash)
	if err != nil {
		t.Fatal(err)
	}
	s := Compare(base, head)
	if s.Deleted != 1 || s.Added != 4 || s.Binary != 1 || s.Symlink != 1 || s.Executable != 1 || s.Gitlink != 0 {
		t.Fatalf("%+v", s)
	}
	if s.Changed() != s.Added+s.Deleted {
		t.Fatalf("rename changed paths %+v", s)
	}
}

func TestCompareCountsUniquePathsAndDeletedBinary(t *testing.T) {
	text := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	edited := plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	bin := plumbing.NewHash("cccccccccccccccccccccccccccccccccccccccc")
	base := map[string]Entry{
		"both.txt": {Mode: ModeFile, Hash: text},
		"gone.bin": {Mode: ModeFile, Hash: bin, Binary: true},
		"mode.bin": {Mode: ModeFile, Hash: bin, Binary: true},
	}
	head := map[string]Entry{
		"both.txt": {Mode: ModeExec, Hash: edited},
		"mode.bin": {Mode: ModeExec, Hash: bin, Binary: true},
	}
	s := Compare(base, head)
	if s.Modified != 1 || s.ModeChanged != 2 || s.Deleted != 1 || s.Added != 0 {
		t.Fatalf("categories %+v", s)
	}
	if s.Binary != 2 || s.Changed() != 3 {
		t.Fatalf("unique paths=%d binary=%d summary %+v", s.Changed(), s.Binary, s)
	}
}

func TestProvePinnedBaseIsAnyParent(t *testing.T) {
	blob := obj("blob", []byte("x"))
	tree := treeObj(t, []TreeEntry{{Mode: ModeFile, Name: "a", Hash: blob.Hash}})
	base := commitObj(tree.Hash)
	other := plumbing.NewHash("1111111111111111111111111111111111111111")
	head := commitObj(tree.Hash, other, base.Hash)
	objs := Map{blob.Hash: blob, tree.Hash: tree, base.Hash: base, head.Hash: head}
	if err := ProveHead(context.Background(), objs, head.Hash, base.Hash, 2); err != nil {
		t.Fatal(err)
	}
	if err := ProveHead(context.Background(), objs, head.Hash, base.Hash, 1); err != nil {
		t.Fatal(err)
	}
	delete(objs, base.Hash)
	if err := ProveHead(context.Background(), objs, head.Hash, base.Hash, 2); !errors.Is(err, ErrShallow) {
		t.Fatalf("shallow: %v", err)
	}
	objs[base.Hash] = blob
	if err := ProveHead(context.Background(), objs, head.Hash, base.Hash, 2); !errors.Is(err, ErrWrongType) {
		t.Fatalf("type: %v", err)
	}
	// Independent authorized base may be absent from the shallow pack.
	lone := commitObj(tree.Hash, other)
	if err := ProveHead(context.Background(), Map{blob.Hash: blob, tree.Hash: tree, lone.Hash: lone}, lone.Hash, base.Hash, 2); !errors.Is(err, ErrShallow) {
		t.Fatalf("missing independent base: %v", err)
	}
	// Ancestor (not direct parent) base is accepted when materialized.
	mid := commitObj(tree.Hash, base.Hash)
	head2 := commitObj(tree.Hash, mid.Hash)
	objs2 := Map{blob.Hash: blob, tree.Hash: tree, base.Hash: base, mid.Hash: mid, head2.Hash: head2}
	if err := ProveHead(context.Background(), objs2, head2.Hash, base.Hash, 2); err != nil {
		t.Fatalf("independent ancestor base: %v", err)
	}
	body := "tree " + tree.Hash.String() + "\n" +
		"author Fixture <fixture@example> 1 +0000\n" +
		"committer Fixture <fixture@example> 1 +0000\n" +
		"encoding UTF-8\n\nmsg\n"
	badEnc := strings.Replace(body, "encoding UTF-8\n", "encoding ISO-8859-1\n", 1)
	if _, err := ParseCommit([]byte(badEnc)); !errors.Is(err, ErrTree) {
		t.Fatalf("encoding: %v", err)
	}
	unknown := strings.Replace(body, "encoding UTF-8\n", "mergetag foo\n", 1)
	if _, err := ParseCommit([]byte(unknown)); !errors.Is(err, ErrTree) {
		t.Fatalf("mergetag: %v", err)
	}
}

func manualTree(entries []TreeEntry) ([]byte, error) {
	return EncodeTree(entries)
}

func strconvName(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var b [16]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = digits[i%10]
		i /= 10
	}
	return string(b[n:])
}
