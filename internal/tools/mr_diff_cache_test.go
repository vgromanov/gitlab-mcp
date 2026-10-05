package tools

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/gitdiff"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/pack"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/tree"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"
)

type fakeCacheHold struct {
	enabled bool
	hold    *gitcache.ObjectHold
	err     error
	calls   int
}

func (f *fakeCacheHold) Enabled() bool { return f.enabled }

func (f *fakeCacheHold) Hold(context.Context, gitcache.AcquireIntent, gitcache.Authorizer) (*gitcache.ObjectHold, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.hold, nil
}

func TestDiffWindow_cacheRecoversOverflow(t *testing.T) {
	if err := gitdiff.LookPath(); err != nil {
		t.Skip(err.Error())
	}
	objs, base, head := twoCommitObjects(t, "p.txt", "old\n", "new\n")
	hold := &gitcache.ObjectHold{
		Result: gitcache.AcquireResult{
			GenerationID: "gen-1",
			Grant: gitcache.Grant{
				ProjectID: "42", SourceFork: "42", TargetProjectID: "42",
				HeadSHA: plumbing.NewHash(head), BaseSHA: plumbing.NewHash(base), StartSHA: plumbing.NewHash(base),
			},
		},
		Objects: objs,
	}
	fake := &fakeCacheHold{enabled: true, hold: hold}
	log := &pathLog{}
	h := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/versions/1"):
			_, _ = io.WriteString(w, versionObject(1, 5001, head, base, base, "overflow", "1", "[]"))
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
		default:
			http.NotFound(w, r)
		}
	})
	d := diffDeps(t, h)
	d.cacheHold = fake
	out, err := callDiffWindow(t, d, nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 20})
	if err != nil {
		t.Fatal(err)
	}
	if fake.calls == 0 {
		t.Fatal("hold was not used")
	}
	sec := sectionMap(out)
	if sec["source"] != readmeta.SourceGitCache || sec["provider"] != readmeta.ProviderGit {
		t.Fatalf("provenance section=%#v", sec)
	}
	if sec["content_complete"] != readmeta.ContentCompleteTrue || sec["manifest_coverage"] != readmeta.CoverageFull {
		t.Fatalf("section=%#v", sec)
	}
	entries, _ := out["entries"].([]any)
	if len(entries) != 1 || asMap(t, entries[0])["new_path"] != "p.txt" {
		t.Fatalf("entries=%#v", entries)
	}
	prov, _ := out["provenance"].(map[string]any)
	if prov == nil || prov["semantics"] != gitdiff.SemanticsFullMR || prov["generation_id"] != "gen-1" {
		t.Fatalf("provenance=%#v", out["provenance"])
	}
}

func TestDiffWindow_cacheRecoversContent(t *testing.T) {
	if err := gitdiff.LookPath(); err != nil {
		t.Skip(err.Error())
	}
	objs, base, head := twoCommitObjects(t, "p.txt", "old\n", "new\n")
	hold := &gitcache.ObjectHold{
		Result: gitcache.AcquireResult{
			GenerationID: "gen-c",
			Grant: gitcache.Grant{
				ProjectID: "42", SourceFork: "42", TargetProjectID: "42",
				HeadSHA: plumbing.NewHash(head), BaseSHA: plumbing.NewHash(base), StartSHA: plumbing.NewHash(base),
			},
		},
		Objects: objs,
	}
	h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/versions/1"):
			_, _ = io.WriteString(w, versionObject(1, 5001, head, base, base, "overflow", "1", "[]"))
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
		default:
			http.NotFound(w, r)
		}
	})
	d := diffDeps(t, h)
	d.cacheHold = &fakeCacheHold{enabled: true, hold: hold}
	out, err := callDiffWindow(t, d, nil, map[string]any{
		"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1,
		"mode": "content", "paths": []string{"p.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sec := sectionMap(out)
	if sec["source"] != readmeta.SourceGitCache {
		t.Fatalf("section=%#v", sec)
	}
	files, _ := out["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("files=%#v", files)
	}
	if asMap(t, files[0])["status"] != diffFileStatusText {
		t.Fatalf("file=%#v", files[0])
	}
	sel, _ := out["selection"].(map[string]any)
	if sel["version_id"] != float64(1) {
		t.Fatalf("selection=%#v", sel)
	}
}

func TestSortManifestEntriesMatchesAPI(t *testing.T) {
	z := "z"
	a := "a"
	b := "b"
	entries := []diffManifestEntry{
		{OldPath: &z, NewPath: &a},
		{OldPath: &b, NewPath: &b},
	}
	sortManifestEntries(entries)
	if pathStr(entries[0].OldPath) != "b" || pathStr(entries[1].OldPath) != "z" {
		t.Fatalf("order=%s then %s", pathStr(entries[0].OldPath), pathStr(entries[1].OldPath))
	}
}

func TestDiffWindow_cacheItemBudgetLeavesSelectorsUnobserved(t *testing.T) {
	if err := gitdiff.LookPath(); err != nil {
		t.Skip(err.Error())
	}
	objs, base, head := twoFileCommitObjects(t)
	hold := &gitcache.ObjectHold{
		Result: gitcache.AcquireResult{
			GenerationID: "gen-items",
			Grant: gitcache.Grant{
				ProjectID: "42", SourceFork: "42", TargetProjectID: "42",
				HeadSHA: plumbing.NewHash(head), BaseSHA: plumbing.NewHash(base), StartSHA: plumbing.NewHash(base),
			},
		},
		Objects: objs,
	}
	d := diffDeps(t, overflowHandler(head, base))
	d.cacheHold = &fakeCacheHold{enabled: true, hold: hold}
	out, err := callDiffWindow(t, d, nil, map[string]any{
		"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1,
		"mode": "content", "paths": []string{"a.txt", "b.txt"}, "max_items": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	sec := sectionMap(out)
	if sec["source"] != readmeta.SourceGitCache || sec["content_complete"] != readmeta.ContentCompleteFalse {
		t.Fatalf("section=%#v", sec)
	}
	byPath := map[string]string{}
	for _, raw := range asSlice(t, out["selectors"]) {
		s := asMap(t, raw)
		byPath[asString(s["path"])] = asString(s["status"])
	}
	if byPath["a.txt"] != diffSelectorMatched {
		t.Fatalf("first selector=%#v", byPath)
	}
	if byPath["b.txt"] != diffSelectorUnobserved {
		t.Fatalf("truncated selector must stay unobserved, got %#v", byPath)
	}
}

func TestDiffWindow_cacheMismatchNeverVerified(t *testing.T) {
	if err := gitdiff.LookPath(); err != nil {
		t.Skip(err.Error())
	}
	objs, base, head := twoCommitObjects(t, "p.txt", "old\n", "new\n")
	hold := &gitcache.ObjectHold{
		Result: gitcache.AcquireResult{
			Grant: gitcache.Grant{
				ProjectID: "42",
				HeadSHA:   plumbing.NewHash(strings.Repeat("c", 40)),
				BaseSHA:   plumbing.NewHash(base),
				StartSHA:  plumbing.NewHash(base),
			},
		},
		Objects: objs,
	}
	h := overflowHandler(head, base)
	d := diffDeps(t, h)
	d.cacheHold = &fakeCacheHold{enabled: true, hold: hold}
	out, err := callDiffWindow(t, d, nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1})
	if err != nil {
		t.Fatal(err)
	}
	sec := sectionMap(out)
	if sec["source"] == readmeta.SourceGitCache || sec["content_complete"] == readmeta.ContentCompleteTrue {
		t.Fatalf("verified mismatch: %#v", sec)
	}
}

func TestDiffWindow_cacheMissingObjectNeverVerified(t *testing.T) {
	objs, base, head := twoCommitObjects(t, "p.txt", "old\n", "new\n")
	delete(objs, plumbing.NewHash(head))
	hold := &gitcache.ObjectHold{
		Result: gitcache.AcquireResult{
			Grant: gitcache.Grant{
				ProjectID: "42", HeadSHA: plumbing.NewHash(head), BaseSHA: plumbing.NewHash(base), StartSHA: plumbing.NewHash(base),
			},
		},
		Objects: objs,
	}
	d := diffDeps(t, overflowHandler(head, base))
	d.cacheHold = &fakeCacheHold{enabled: true, hold: hold}
	out, err := callDiffWindow(t, d, nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1})
	if err != nil {
		t.Fatal(err)
	}
	sec := sectionMap(out)
	if sec["source"] == readmeta.SourceGitCache || sec["content_complete"] == readmeta.ContentCompleteTrue {
		t.Fatalf("verified missing: %#v", sec)
	}
}

func TestDiffWindow_overflowWithoutCacheStaysPartial(t *testing.T) {
	head, base := shaN(1), shaN(2)
	out, err := callDiffWindow(t, diffDeps(t, overflowHandler(head, base)), nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1})
	if err != nil {
		t.Fatal(err)
	}
	sec := sectionMap(out)
	if sec["content_complete"] != readmeta.ContentCompleteFalse || sec["source"] != readmeta.SourceGitLabREST {
		t.Fatalf("section=%#v", sec)
	}
}

func TestDiffWindow_cacheAuthzFailureNeverVerified(t *testing.T) {
	head, base := shaN(1), shaN(2)
	d := diffDeps(t, overflowHandler(head, base))
	d.cacheHold = &fakeCacheHold{enabled: true, err: gitcache.ErrAuthz}
	out, err := callDiffWindow(t, d, nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1})
	if err != nil {
		t.Fatal(err)
	}
	if sectionMap(out)["source"] == readmeta.SourceGitCache {
		t.Fatal("authz failure recovered")
	}
}

func overflowHandler(head, base string) http.Handler {
	return serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/versions/1"):
			_, _ = io.WriteString(w, versionObject(1, 5001, head, base, base, "overflow", "1", "[]"))
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
		default:
			http.NotFound(w, r)
		}
	})
}

func twoFileCommitObjects(t *testing.T) (map[plumbing.Hash]pack.Object, string, string) {
	t.Helper()
	oldA := pack.Object{Type: "blob", Data: []byte("old-a\n")}
	oldA.Hash = pack.HashObject("blob", oldA.Data)
	newA := pack.Object{Type: "blob", Data: []byte("new-a\n")}
	newA.Hash = pack.HashObject("blob", newA.Data)
	oldB := pack.Object{Type: "blob", Data: []byte("old-b\n")}
	oldB.Hash = pack.HashObject("blob", oldB.Data)
	newB := pack.Object{Type: "blob", Data: []byte("new-b\n")}
	newB.Hash = pack.HashObject("blob", newB.Data)
	tb, err := tree.EncodeTree([]tree.TreeEntry{
		{Mode: tree.ModeFile, Name: "a.txt", Hash: oldA.Hash},
		{Mode: tree.ModeFile, Name: "b.txt", Hash: oldB.Hash},
	})
	if err != nil {
		t.Fatal(err)
	}
	th, err := tree.EncodeTree([]tree.TreeEntry{
		{Mode: tree.ModeFile, Name: "a.txt", Hash: newA.Hash},
		{Mode: tree.ModeFile, Name: "b.txt", Hash: newB.Hash},
	})
	if err != nil {
		t.Fatal(err)
	}
	baseTree := pack.Object{Type: "tree", Data: tb, Hash: pack.HashObject("tree", tb)}
	headTree := pack.Object{Type: "tree", Data: th, Hash: pack.HashObject("tree", th)}
	baseBody := []byte("tree " + baseTree.Hash.String() + "\nauthor A <a@a> 1 +0000\ncommitter A <a@a> 1 +0000\n\nbase\n")
	base := pack.Object{Type: "commit", Data: baseBody, Hash: pack.HashObject("commit", baseBody)}
	headBody := []byte("tree " + headTree.Hash.String() + "\nparent " + base.Hash.String() + "\nauthor A <a@a> 1 +0000\ncommitter A <a@a> 1 +0000\n\nhead\n")
	head := pack.Object{Type: "commit", Data: headBody, Hash: pack.HashObject("commit", headBody)}
	return map[plumbing.Hash]pack.Object{
		oldA.Hash: oldA, newA.Hash: newA, oldB.Hash: oldB, newB.Hash: newB,
		baseTree.Hash: baseTree, headTree.Hash: headTree, base.Hash: base, head.Hash: head,
	}, base.Hash.String(), head.Hash.String()
}

func twoCommitObjects(t *testing.T, name, oldBody, newBody string) (map[plumbing.Hash]pack.Object, string, string) {
	t.Helper()
	oldBlob := pack.Object{Type: "blob", Data: []byte(oldBody)}
	oldBlob.Hash = pack.HashObject("blob", oldBlob.Data)
	newBlob := pack.Object{Type: "blob", Data: []byte(newBody)}
	newBlob.Hash = pack.HashObject("blob", newBlob.Data)
	tb, err := tree.EncodeTree([]tree.TreeEntry{{Mode: tree.ModeFile, Name: name, Hash: oldBlob.Hash}})
	if err != nil {
		t.Fatal(err)
	}
	th, err := tree.EncodeTree([]tree.TreeEntry{{Mode: tree.ModeFile, Name: name, Hash: newBlob.Hash}})
	if err != nil {
		t.Fatal(err)
	}
	baseTree := pack.Object{Type: "tree", Data: tb, Hash: pack.HashObject("tree", tb)}
	headTree := pack.Object{Type: "tree", Data: th, Hash: pack.HashObject("tree", th)}
	baseBody := []byte("tree " + baseTree.Hash.String() + "\nauthor A <a@a> 1 +0000\ncommitter A <a@a> 1 +0000\n\nbase\n")
	base := pack.Object{Type: "commit", Data: baseBody, Hash: pack.HashObject("commit", baseBody)}
	headBody := []byte("tree " + headTree.Hash.String() + "\nparent " + base.Hash.String() + "\nauthor A <a@a> 1 +0000\ncommitter A <a@a> 1 +0000\n\nhead\n")
	head := pack.Object{Type: "commit", Data: headBody, Hash: pack.HashObject("commit", headBody)}
	return map[plumbing.Hash]pack.Object{
		oldBlob.Hash: oldBlob, newBlob.Hash: newBlob,
		baseTree.Hash: baseTree, headTree.Hash: headTree, base.Hash: base, head.Hash: head,
	}, base.Hash.String(), head.Hash.String()
}

func TestDiffWindow_completeAPISkipsCache(t *testing.T) {
	objs, base, head := twoCommitObjects(t, "p.txt", "old\n", "new\n")
	hold := &gitcache.ObjectHold{
		Result: gitcache.AcquireResult{
			Grant: gitcache.Grant{
				ProjectID: "42", HeadSHA: plumbing.NewHash(head), BaseSHA: plumbing.NewHash(base), StartSHA: plumbing.NewHash(base),
			},
		},
		Objects: objs,
	}
	fake := &fakeCacheHold{enabled: true, hold: hold}
	h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/versions/1"):
			_, _ = io.WriteString(w, versionObject(1, 5001, head, base, base, "collected", "1", oneDiff("p.txt", "@@ -1 +1 @@\n-old\n+new\n")))
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
		default:
			http.NotFound(w, r)
		}
	})
	d := diffDeps(t, h)
	d.cacheHold = fake
	out, err := callDiffWindow(t, d, nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 20})
	if err != nil {
		t.Fatal(err)
	}
	if fake.calls != 0 {
		t.Fatalf("complete API used cache: calls=%d", fake.calls)
	}
	sec := sectionMap(out)
	if sec["source"] != readmeta.SourceGitLabREST || sec["content_complete"] != readmeta.ContentCompleteTrue {
		t.Fatalf("section=%#v", sec)
	}
}

func TestDiffWindow_apiAndGitAgreeOnFixture(t *testing.T) {
	if err := gitdiff.LookPath(); err != nil {
		t.Skip(err.Error())
	}
	objs, base, head := twoCommitObjects(t, "p.txt", "old\n", "new\n")
	h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/versions/1"):
			_, _ = io.WriteString(w, versionObject(1, 5001, head, base, base, "collected", "1", oneDiff("p.txt", "@@ -1 +1 @@\n-old\n+new\n")))
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
		default:
			http.NotFound(w, r)
		}
	})
	out, err := callDiffWindow(t, diffDeps(t, h), nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 20})
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := out["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("api entries=%#v", entries)
	}
	api := asMap(t, entries[0])
	dir := t.TempDir()
	if err := gitdiff.WriteBare(context.Background(), dir, objs); err != nil {
		t.Fatal(err)
	}
	res, err := gitdiff.Raw(context.Background(), dir, base, head, gitdiff.SemanticsFullMR, objs, gitdiff.Limits{})
	if err != nil || len(res.Files) != 1 {
		t.Fatalf("git %#v %v", res, err)
	}
	g := res.Files[0]
	if api["new_path"] != g.NewPath || api["old_path"] != g.OldPath {
		t.Fatalf("paths api=%#v git=%#v", api, g)
	}
	if api["new_file"] != g.NewFile || api["deleted_file"] != g.DeletedFile || api["renamed_file"] != g.RenamedFile {
		t.Fatalf("flags api=%#v git=%#v", api, g)
	}
	sec := sectionMap(out)
	if sec["head_sha"] != head {
		t.Fatalf("head api=%#v git=%s", sec["head_sha"], head)
	}
}

func TestDiffWindow_cacheRecoversIncremental(t *testing.T) {
	if err := gitdiff.LookPath(); err != nil {
		t.Skip(err.Error())
	}
	objs, base, head := twoCommitObjects(t, "p.txt", "old\n", "new\n")
	hold := &gitcache.ObjectHold{
		Result: gitcache.AcquireResult{
			GenerationID: "gen-i",
			Grant: gitcache.Grant{
				ProjectID: "42", HeadSHA: plumbing.NewHash(head), BaseSHA: plumbing.NewHash(base), StartSHA: plumbing.NewHash(base),
			},
		},
		Objects: objs,
	}
	h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/repository/compare"):
			_, _ = io.WriteString(w, `{"commit":{"id":"`+head+`"},"diffs":[{"old_path":"p.txt","new_path":"p.txt","a_mode":"100644","b_mode":"100644"}]}`)
		case strings.Contains(r.URL.Path, "/repository/commits/"):
			sha := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			_, _ = io.WriteString(w, `{"id":"`+sha+`"}`)
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
		default:
			http.NotFound(w, r)
		}
	})
	d := diffDeps(t, h)
	d.cacheHold = &fakeCacheHold{enabled: true, hold: hold}
	out, err := callDiffWindow(t, d, nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "from_sha": base, "to_sha": head, "straight": true, "per_page": 20})
	if err != nil {
		t.Fatal(err)
	}
	sec := sectionMap(out)
	if sec["source"] != readmeta.SourceGitCache || sec["content_complete"] != readmeta.ContentCompleteTrue {
		t.Fatalf("section=%#v", sec)
	}
	prov, _ := out["provenance"].(map[string]any)
	if prov["semantics"] != gitdiff.SemanticsStraight {
		t.Fatalf("provenance=%#v", prov)
	}
}
