package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"
)

func samplePatchAdditionDeletionContext() string {
	return "@@ -1,3 +1,3 @@\n" +
		" context-a\n" +
		"-deleted\n" +
		"+added\n" +
		" context-b\n"
}

func TestDiffContent_parseAndWindowOracle(t *testing.T) {
	patch := samplePatchAdditionDeletionContext()
	parsed := parseUnifiedDiff(patch)
	if !parsed.ok || len(parsed.hunks) != 1 {
		t.Fatalf("parsed=%#v", parsed)
	}
	h := parsed.hunks[0]
	if h.oldStart != 1 || h.oldCount != 3 || h.newStart != 1 || h.newCount != 3 {
		t.Fatalf("ranges=%#v", h)
	}
	budget := &contentEmitBudget{maxLines: 1000, maxBytes: 262144}
	wins, ok, trunc := selectDiffWindows(parsed, 1, budget)
	if !ok || trunc || len(wins) != 1 {
		t.Fatalf("wins=%d ok=%v trunc=%v", len(wins), ok, trunc)
	}
	w := wins[0]
	if w.Text != patch {
		t.Fatalf("window text mismatch:\n%s\n---\n%s", w.Text, patch)
	}
	sum := sha256.Sum256([]byte(w.Text))
	want := hex.EncodeToString(sum[:])
	if w.WindowHash.Algorithm != "sha256" || w.WindowHash.Scope != diffContentWindowHashScope || w.WindowHash.Value != want {
		t.Fatalf("hash=%#v want %s", w.WindowHash, want)
	}
	kinds := make([]string, 0, len(w.Lines))
	for _, ln := range w.Lines {
		kinds = append(kinds, ln.Kind)
	}
	if strings.Join(kinds, ",") != "context,deletion,addition,context" {
		t.Fatalf("kinds=%v", kinds)
	}
	if w.Lines[1].OldLine == nil || *w.Lines[1].OldLine != 2 || w.Lines[1].NewLine != nil {
		t.Fatalf("deletion coords %#v", w.Lines[1])
	}
	if w.Lines[2].NewLine == nil || *w.Lines[2].NewLine != 2 || w.Lines[2].OldLine != nil {
		t.Fatalf("addition coords %#v", w.Lines[2])
	}
}

func TestDiffContent_noNewlineMarker(t *testing.T) {
	patch := "@@ -1 +1 @@\n-old\n+new\n\\ No newline at end of file\n"
	parsed := parseUnifiedDiff(patch)
	if !parsed.ok || len(parsed.hunks) != 1 {
		t.Fatalf("parsed=%#v", parsed)
	}
	budget := &contentEmitBudget{maxLines: 100, maxBytes: 10000}
	wins, ok, _ := selectDiffWindows(parsed, 0, budget)
	if !ok || len(wins) != 1 {
		t.Fatalf("wins=%#v", wins)
	}
	last := wins[0].Lines[len(wins[0].Lines)-1]
	if last.Kind != diffLineKindAddition || !last.NoNewline {
		t.Fatalf("last=%#v", last)
	}
	for _, ln := range wins[0].Lines {
		if ln.Kind == diffLineKindMarker {
			t.Fatal("marker must not be emitted as an anchorable line")
		}
	}
}

func TestDiffContent_cropLimits(t *testing.T) {
	patch := "@@ -1,5 +1,5 @@\n" +
		" a\n-b\n+B\n" +
		" c\n-d\n+D\n" +
		" e\n"
	parsed := parseUnifiedDiff(patch)
	budget := &contentEmitBudget{maxLines: 3, maxBytes: 262144}
	wins, ok, trunc := selectDiffWindows(parsed, 0, budget)
	if !ok || !trunc {
		t.Fatalf("ok=%v trunc=%v wins=%d", ok, trunc, len(wins))
	}
}

func TestDiffContent_returnedHashConcat(t *testing.T) {
	files := []retainedDiffFile{{
		entry:   diffManifestEntry{OldPath: strPtr("a.go"), NewPath: strPtr("a.go")},
		patch:   samplePatchAdditionDeletionContext(),
		patchOK: true,
	}}
	built, ret, cropped := buildDiffContentFiles(files, []int{0}, diffContentOpts{ContextLines: 3, MaxLines: 1000, MaxContentBytes: 262144})
	if cropped || len(built) != 1 || built[0].Status != diffFileStatusText || ret == nil {
		t.Fatalf("built=%#v ret=%v cropped=%v", built, ret, cropped)
	}
	var concat strings.Builder
	for _, w := range built[0].Windows {
		concat.WriteString(w.Text)
	}
	sum := sha256.Sum256([]byte(concat.String()))
	if ret.Scope != diffContentReturnHashScope || ret.Value != hex.EncodeToString(sum[:]) {
		t.Fatalf("ret=%#v", ret)
	}
}

func TestDiffContent_binarySubmoduleMode(t *testing.T) {
	cases := []struct {
		name   string
		entry  diffManifestEntry
		patch  string
		status string
	}{
		{"binary", diffManifestEntry{Binary: boolPtr(true), OldPath: strPtr("b"), NewPath: strPtr("b")}, "x", diffFileStatusBinary},
		{"submodule", diffManifestEntry{Submodule: boolPtr(true), OldPath: strPtr("s"), NewPath: strPtr("s")}, "", diffFileStatusSubmodule},
		{"mode", diffManifestEntry{RenamedFile: boolPtr(true), OldPath: strPtr("old"), NewPath: strPtr("new"), AMode: strPtr("100644"), BMode: strPtr("100755")}, "", diffFileStatusModeOnly},
		{"collapsed", diffManifestEntry{Collapsed: boolPtr(true), OldPath: strPtr("c"), NewPath: strPtr("c")}, "@@ -1 +1 @@\n-a\n+b\n", diffFileStatusCollapsed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed := parseUnifiedDiff(tc.patch)
			st, _ := classifyDiffFile(tc.entry, tc.patch, true, false, false, false, parsed)
			if st != tc.status {
				t.Fatalf("status=%s want %s", st, tc.status)
			}
		})
	}
}

func TestDiffContent_pathValidation(t *testing.T) {
	mode := "content"
	_, _, err := normalizeDiffWindowMode(diffWindowIn{
		Mode: &mode, Paths: []string{"/abs"}, PerPage: intPtr(20),
		DiffVersionID: int64Ptr(1), ProjectID: "42", MergeRequestIID: 1,
	})
	if err == nil {
		t.Fatal("absolute path accepted")
	}
	_, _, err = normalizeDiffWindowMode(diffWindowIn{
		Mode: &mode, Paths: []string{"a/../b"}, PerPage: intPtr(20),
		DiffVersionID: int64Ptr(1), ProjectID: "42", MergeRequestIID: 1,
	})
	if err == nil {
		t.Fatal("dot segment accepted")
	}
	_, _, err = normalizeDiffWindowMode(diffWindowIn{
		Mode: &mode, Paths: []string{"ok file.go"}, PerPage: intPtr(20),
		DiffVersionID: int64Ptr(1), ProjectID: "42", MergeRequestIID: 1,
	})
	if err != nil {
		t.Fatalf("spaces must be preserved: %v", err)
	}
	manifest := "manifest"
	_, _, err = normalizeDiffWindowMode(diffWindowIn{
		Mode: &manifest, Paths: []string{"a"}, DiffVersionID: int64Ptr(1), ProjectID: "42", MergeRequestIID: 1,
	})
	if err == nil {
		t.Fatal("content fields in manifest mode accepted")
	}
	cur := "tok"
	_, _, err = normalizeDiffWindowMode(diffWindowIn{
		Mode: &mode, Paths: []string{"a"}, Cursor: &cur, DiffVersionID: int64Ptr(1), ProjectID: "42", MergeRequestIID: 1,
	})
	if err == nil {
		t.Fatal("content cursor accepted")
	}
}

func TestDiffContent_matchSelectors(t *testing.T) {
	files := []retainedDiffFile{
		{entry: diffManifestEntry{OldPath: strPtr("old.go"), NewPath: strPtr("new.go")}},
		{entry: diffManifestEntry{OldPath: strPtr("x"), NewPath: strPtr("x")}},
		{entry: diffManifestEntry{OldPath: strPtr("dup"), NewPath: strPtr("a")}},
		{entry: diffManifestEntry{OldPath: strPtr("dup"), NewPath: strPtr("b")}},
	}
	out, selected := matchSelectors([]string{"old.go", "new.go", "missing", "dup"}, files)
	if len(selected) != 1 || selected[0] != 0 {
		t.Fatalf("selected=%v", selected)
	}
	byPath := map[string]string{}
	for _, o := range out {
		byPath[o.Path] = o.Status
	}
	if byPath["old.go"] != diffSelectorMatched || byPath["new.go"] != diffSelectorCoalesced {
		t.Fatalf("outcomes=%v", byPath)
	}
	if byPath["missing"] != diffSelectorAbsent || byPath["dup"] != diffSelectorAmbiguous {
		t.Fatalf("outcomes=%v", byPath)
	}
}

func int64Ptr(v int64) *int64 { return &v }

func TestDiffContent_registeredMCPVersionSuccess(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	patch := samplePatchAdditionDeletionContext()
	sibling := "@@ -1 +1 @@\n-a\n+b\n"
	diffs := fmt.Sprintf(`[{"old_path":"keep.go","new_path":"keep.go","a_mode":"100644","b_mode":"100644","new_file":false,"renamed_file":false,"deleted_file":false,"diff":%q},{"old_path":"other.go","new_path":"other.go","a_mode":"100644","b_mode":"100644","new_file":false,"renamed_file":false,"deleted_file":false,"diff":%q}]`, patch, sibling)
	body := versionObject(1, 5001, head, base, start, "collected", "2", diffs)
	log := &pathLog{}
	h := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/versions/1"):
			_, _ = io.WriteString(w, body)
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
		default:
			http.NotFound(w, r)
		}
	})
	d := diffDeps(t, h)
	out, err := callDiffWindow(t, d, nil, map[string]any{
		"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1,
		"mode": "content", "paths": []any{"keep.go"}, "per_page": 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	sec := sectionMap(out)
	if sec["capability_version"] != capabilityDiffContentV1 {
		t.Fatalf("section=%#v", sec)
	}
	if sec["next_cursor"] != nil || out["full_patch_hash"] != nil {
		t.Fatalf("cursor/full hash leaked: %#v", out)
	}
	if log.count("/versions/1") < 2 {
		t.Fatalf("paths=%v", log.paths)
	}
	files := asSlice(t, out["files"])
	if len(files) != 1 || asMap(t, files[0])["status"] != diffFileStatusText {
		t.Fatalf("files=%#v", files)
	}
	selectors := asSlice(t, out["selectors"])
	if len(selectors) != 1 || asMap(t, selectors[0])["status"] != diffSelectorMatched {
		t.Fatalf("selectors=%#v", selectors)
	}
	ret := asMap(t, out["returned_content_hash"])
	if ret["scope"] != diffContentReturnHashScope || ret["algorithm"] != "sha256" {
		t.Fatalf("hash=%#v", ret)
	}
}

func TestDiffContent_invalidInputsZeroHTTP(t *testing.T) {
	log := &pathLog{}
	hits := 0
	h := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.NotFound(w, r)
	})
	d := diffDeps(t, h)
	_, err := callDiffWindow(t, d, nil, map[string]any{
		"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1,
		"mode": "nope",
	})
	if err == nil || hits != 0 {
		t.Fatalf("bad mode err=%v hits=%d", err, hits)
	}
	_, err = callDiffWindow(t, d, nil, map[string]any{
		"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1,
		"paths": []any{"a.go"},
	})
	if err == nil || hits != 0 {
		t.Fatalf("manifest+paths err=%v hits=%d", err, hits)
	}
	_, err = callDiffWindow(t, d, nil, map[string]any{
		"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1,
		"mode": "content", "paths": []any{"a.go"}, "cursor": "x",
	})
	if err == nil || hits != 0 {
		t.Fatalf("content cursor err=%v hits=%d", err, hits)
	}
}

func TestDiffContent_manifestDefaultUnchanged(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	body := versionObject(1, 5001, head, base, start, "collected", "1", oneDiff("historical.txt", "SECRET"))
	log := &pathLog{}
	h := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/versions/1"):
			_, _ = io.WriteString(w, body)
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
		default:
			http.NotFound(w, r)
		}
	})
	out, err := callDiffWindow(t, diffDeps(t, h), nil, map[string]any{
		"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	sec := sectionMap(out)
	if sec["capability_version"] != capabilityDiffV1 || sec["content_complete"] != readmeta.ContentCompleteTrue {
		t.Fatalf("section=%#v", sec)
	}
	raw, _ := json.Marshal(out["entries"])
	if strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), `"diff"`) {
		t.Fatalf("patch leaked: %s", raw)
	}
}

func TestDiffContent_binaryAndAbsentSelectors(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	diffs := `[{"old_path":"bin.dat","new_path":"bin.dat","a_mode":"100644","b_mode":"100644","binary":true,"diff":"Binary files a and b differ\n"},{"old_path":"ok.go","new_path":"ok.go","a_mode":"100644","b_mode":"100644","diff":"@@ -1 +1 @@\n-a\n+b\n"}]`
	body := versionObject(1, 5001, head, base, start, "collected", "2", diffs)
	log := &pathLog{}
	h := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/versions/1"):
			_, _ = io.WriteString(w, body)
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
		default:
			http.NotFound(w, r)
		}
	})
	out, err := callDiffWindow(t, diffDeps(t, h), nil, map[string]any{
		"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1,
		"mode": "content", "paths": []any{"bin.dat", "missing.go", "ok.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	byStatus := map[string]string{}
	for _, f := range asSlice(t, out["files"]) {
		m := asMap(t, f)
		byStatus[asString(m["new_path"])] = asString(m["status"])
	}
	if byStatus["bin.dat"] != diffFileStatusBinary || byStatus["ok.go"] != diffFileStatusText {
		t.Fatalf("files=%v", byStatus)
	}
	selStatus := map[string]string{}
	for _, s := range asSlice(t, out["selectors"]) {
		m := asMap(t, s)
		selStatus[asString(m["path"])] = asString(m["status"])
	}
	if selStatus["missing.go"] != diffSelectorAbsent {
		t.Fatalf("selectors=%v", selStatus)
	}
	if sectionMap(out)["content_complete"] != readmeta.ContentCompleteFalse {
		t.Fatalf("section=%#v", sectionMap(out))
	}
}

func TestDiffContent_closingPatchDrift(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	var n int
	log := &pathLog{}
	h := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/versions/1"):
			n++
			patch := "@@ -1 +1 @@\n-a\n+b\n"
			if n%2 == 0 {
				patch = "@@ -1 +1 @@\n-a\n+c\n"
			}
			body := versionObject(1, 5001, head, base, start, "collected", "1", oneDiff("a.go", patch))
			_, _ = io.WriteString(w, body)
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
		default:
			http.NotFound(w, r)
		}
	})
	out, err := callDiffWindow(t, diffDeps(t, h), nil, map[string]any{
		"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1,
		"mode": "content", "paths": []any{"a.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(asSlice(t, out["files"])) != 0 || out["returned_content_hash"] != nil {
		t.Fatalf("drift accepted %#v", out)
	}
	if sectionMap(out)["content_complete"] == readmeta.ContentCompleteTrue {
		t.Fatalf("section=%#v", sectionMap(out))
	}
}

func TestDiffContent_deniedOwner(t *testing.T) {
	log := &pathLog{}
	h := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	d := diffDeps(t, h)
	d.Config.AllowedProjectIDs = []string{"99"}
	_, err := callDiffWindow(t, d, nil, map[string]any{
		"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1,
		"mode": "content", "paths": []any{"a.go"},
	})
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
		t.Fatalf("err=%v", err)
	}
	if log.count("/versions/") != 0 {
		t.Fatalf("content before authz: %v", log.paths)
	}
}

func TestDiffContent_straightPartial(t *testing.T) {
	from, to := shaN(4), shaN(5)
	patch := "@@ -1 +1 @@\n-a\n+b\n"
	log := &pathLog{}
	h := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/repository/commits/"+from):
			fmt.Fprintf(w, `{"id":%q}`, from)
		case strings.Contains(r.URL.Path, "/repository/commits/"+to):
			fmt.Fprintf(w, `{"id":%q}`, to)
		case strings.Contains(r.URL.Path, "/repository/compare"):
			fmt.Fprintf(w, `{"commit":{"id":%q},"diffs":[{"old_path":"a.go","new_path":"a.go","a_mode":"100644","b_mode":"100644","diff":%q}]}`, to, patch)
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
		default:
			http.NotFound(w, r)
		}
	})
	out, err := callDiffWindow(t, diffDeps(t, h), nil, map[string]any{
		"project_id": "42", "merge_request_iid": 1, "from_sha": from, "to_sha": to, "straight": true,
		"mode": "content", "paths": []any{"a.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sec := sectionMap(out)
	if sec["content_complete"] != readmeta.ContentCompleteFalse || sec["patch_coverage"] != readmeta.CoveragePartial {
		t.Fatalf("section=%#v", sec)
	}
	if len(asSlice(t, out["files"])) != 1 {
		t.Fatalf("files=%#v", out["files"])
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
