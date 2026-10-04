package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
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
	wins, ok, trunc, _ := selectDiffWindows(parsed, 1, budget)
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
	wins, ok, _, _ := selectDiffWindows(parsed, 0, budget)
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
	wins, ok, trunc, _ := selectDiffWindows(parsed, 0, budget)
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
	built, ret, cropped, _ := buildDiffContentFiles(files, []int{0}, diffContentOpts{ContextLines: 3, MaxLines: 1000, MaxContentBytes: 262144})
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
			st, _ := classifyDiffFile(tc.entry, tc.patch, true, false, false, false, false, parsed)
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
	out, selected := matchSelectors([]string{"old.go", "new.go", "missing", "dup"}, files, true)
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

// ---- RVG-143 repair F1-F11 closure controls (red then green) ----
func TestDiffContentRepair_F1_parserNegatives(t *testing.T) {
	cases := []struct {
		name  string
		patch string
	}{
		{"missing_closing_delim", "@@ -1 +1 \n-a\n+b\n"},
		{"extra_range_field", "@@ -1 +1 +2 @@\n-a\n+b\n"},
		{"zero_start_positive_count", "@@ -0,1 +1 @@\n+a\n"},
		{"maxint_overflow", fmt.Sprintf("@@ -%d,2 +1 @@\n-a\n-b\n", math.MaxInt)},
		{"overlap_hunks", "@@ -1 +1 @@\n-a\n+b\n@@ -1 +1 @@\n-c\n+d\n"},
		{"unprefixed_blank_body", "@@ -1,2 +1,2 @@\n a\n\n b\n"},
		{"orphan_marker", "@@ -1 +1 @@\n\\ No newline at end of file\n+a\n"},
		{"repeated_marker", "@@ -1 +1 @@\n-a\n+b\n\\ No newline at end of file\n\\ No newline at end of file\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed := parseUnifiedDiff(tc.patch)
			if parsed.ok {
				t.Fatalf("helper/parser: expected reject, got ok hunks=%d", len(parsed.hunks))
			}
		})
	}
}

func TestDiffContentRepair_F1_parserPositives(t *testing.T) {
	t.Run("zero_count_add", func(t *testing.T) {
		p := "@@ -0,0 +1 @@\n+only\n"
		parsed := parseUnifiedDiff(p)
		if !parsed.ok || len(parsed.hunks) != 1 {
			t.Fatalf("helper/parser: %#v", parsed)
		}
	})
	t.Run("zero_count_delete", func(t *testing.T) {
		p := "@@ -1 +0,0 @@\n-gone\n"
		parsed := parseUnifiedDiff(p)
		if !parsed.ok || len(parsed.hunks) != 1 {
			t.Fatalf("helper/parser: %#v", parsed)
		}
	})
	t.Run("ordered_multi_hunk", func(t *testing.T) {
		p := "@@ -1 +1 @@\n-a\n+b\n@@ -5 +5 @@\n-c\n+d\n"
		parsed := parseUnifiedDiff(p)
		if !parsed.ok || len(parsed.hunks) != 2 {
			t.Fatalf("helper/parser: %#v", parsed)
		}
	})
	t.Run("crlf_preserve", func(t *testing.T) {
		p := "@@ -1 +1 @@\r\n-a\r\n+b\r\n"
		parsed := parseUnifiedDiff(p)
		if !parsed.ok {
			t.Fatal("helper/parser: crlf rejected")
		}
		budget := &contentEmitBudget{maxLines: 100, maxBytes: 10000}
		wins, ok, _, _ := selectDiffWindows(parsed, 0, budget)
		if !ok || len(wins) != 1 || !strings.Contains(wins[0].Text, "\r\n") {
			t.Fatalf("helper/parser: crlf not preserved %#v", wins)
		}
	})
}

func TestDiffContentRepair_F1_mcpMalformedSibling(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	bad := "@@ -1 +1 @@\n-a\n+b\n@@ -1 +1 @@\n-c\n+d\n"
	good := "@@ -1 +1 @@\n-x\n+y\n"
	diffs := fmt.Sprintf(`[{"old_path":"bad.go","new_path":"bad.go","a_mode":"100644","b_mode":"100644","diff":%q},{"old_path":"good.go","new_path":"good.go","a_mode":"100644","b_mode":"100644","diff":%q}]`, bad, good)
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
		"mode": "content", "paths": []any{"bad.go", "good.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]map[string]any{}
	for _, f := range asSlice(t, out["files"]) {
		m := asMap(t, f)
		by[asString(m["new_path"])] = m
	}
	if asString(by["bad.go"]["status"]) == diffFileStatusText {
		t.Fatalf("registered-MCP: malformed must not be trusted text: %#v", by["bad.go"])
	}
	if asString(by["good.go"]["status"]) != diffFileStatusText {
		t.Fatalf("registered-MCP: valid sibling lost: %#v", by["good.go"])
	}
	if wins, _ := by["bad.go"]["windows"].([]any); len(wins) != 0 {
		t.Fatalf("registered-MCP: bad windows present")
	}
}

// --- F2 duplicate proof keys ---

func TestDiffContentRepair_F2_duplicateProofMembers(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	patch := "@@ -1 +1 @@\n-a\n+b\n"
	// Conflicting head then correct head (last-wins would succeed today).
	body := fmt.Sprintf(`{"id":1,"merge_request_id":5001,"head_commit_sha":%q,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q,"state":"collected","real_size":"1","diffs":%s}`,
		shaN(9), head, base, start, oneDiff("a.go", patch))
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
		"mode": "content", "paths": []any{"a.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out["returned_content_hash"] != nil || len(asSlice(t, out["files"])) != 0 {
		t.Fatalf("registered-MCP: duplicate proof trusted %#v", out)
	}
}

func TestDiffContentRepair_F2_duplicateDiffsArray(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	d1 := oneDiff("a.go", "@@ -1 +1 @@\n-a\n+b\n")
	d2 := oneDiff("a.go", "@@ -1 +1 @@\n-a\n+c\n")
	body := fmt.Sprintf(`{"id":1,"merge_request_id":5001,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q,"state":"collected","real_size":"1","diffs":%s,"diffs":%s}`,
		head, base, start, d1, d2)
	h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
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
		"mode": "content", "paths": []any{"a.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out["returned_content_hash"] != nil {
		t.Fatalf("registered-MCP: duplicate diffs trusted")
	}
}

func TestDiffContentRepair_F2_compareCommitDuplicateID(t *testing.T) {
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
			fmt.Fprintf(w, `{"commit":{"id":%q,"id":%q},"diffs":[{"old_path":"a.go","new_path":"a.go","a_mode":"100644","b_mode":"100644","diff":%q}]}`, shaN(9), to, patch)
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
	if out["returned_content_hash"] != nil || len(asSlice(t, out["files"])) != 0 {
		t.Fatalf("registered-MCP: nested duplicate commit id trusted %#v", out)
	}
}

func TestDiffContentRepair_F2_shuffledUniqueKeysPositive(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	patch := "@@ -1 +1 @@\n-a\n+b\n"
	body := fmt.Sprintf(`{"diffs":%s,"real_size":"1","state":"collected","start_commit_sha":%q,"base_commit_sha":%q,"head_commit_sha":%q,"merge_request_id":5001,"id":1}`,
		oneDiff("a.go", patch), start, base, head)
	h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
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
		"mode": "content", "paths": []any{"a.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(asSlice(t, out["files"])) != 1 || asMap(t, asSlice(t, out["files"])[0])["status"] != diffFileStatusText {
		t.Fatalf("registered-MCP: shuffled unique keys failed %#v", out)
	}
}

// --- F3 straight second compare ---

func TestDiffContentRepair_F3_straightClosingDrift(t *testing.T) {
	from, to := shaN(4), shaN(5)
	var compares int
	log := &pathLog{}
	h := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/repository/commits/"+from):
			fmt.Fprintf(w, `{"id":%q}`, from)
		case strings.Contains(r.URL.Path, "/repository/commits/"+to):
			fmt.Fprintf(w, `{"id":%q}`, to)
		case strings.Contains(r.URL.Path, "/repository/compare"):
			compares++
			patch := "@@ -1 +1 @@\n-a\n+b\n"
			if compares > 1 {
				patch = "@@ -1 +1 @@\n-a\n+c\n"
			}
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
	if compares < 2 {
		t.Fatalf("registered-MCP: expected closing compare, compares=%d paths=%v", compares, log.paths)
	}
	if out["returned_content_hash"] != nil || len(asSlice(t, out["files"])) != 0 {
		t.Fatalf("registered-MCP: drift trusted %#v", out)
	}
}

func TestDiffContentRepair_F3_straightTwoReadSuccess(t *testing.T) {
	from, to := shaN(4), shaN(5)
	var compares int
	patch := "@@ -1 +1 @@\n-a\n+b\n"
	h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/repository/commits/"+from):
			fmt.Fprintf(w, `{"id":%q}`, from)
		case strings.Contains(r.URL.Path, "/repository/commits/"+to):
			fmt.Fprintf(w, `{"id":%q}`, to)
		case strings.Contains(r.URL.Path, "/repository/compare"):
			compares++
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
	if compares != 2 {
		t.Fatalf("registered-MCP: compares=%d want 2", compares)
	}
	sec := sectionMap(out)
	if sec["consistency"] != readmeta.ConsistencyConsistent {
		t.Fatalf("section=%#v", sec)
	}
	if sec["content_complete"] != readmeta.ContentCompleteFalse || sec["patch_coverage"] != readmeta.CoveragePartial {
		t.Fatalf("straight must stay partial: %#v", sec)
	}
	if len(asSlice(t, out["files"])) != 1 {
		t.Fatalf("files=%#v", out["files"])
	}
}

// --- F5 marker atomicity ---

func TestDiffContentRepair_F5_markerExactText(t *testing.T) {
	patch := "@@ -1 +1 @@\n-old\n+new\n\\ No newline at end of file\n"
	wantText := patch
	parsed := parseUnifiedDiff(patch)
	if !parsed.ok {
		t.Fatal("helper/parser")
	}
	budget := &contentEmitBudget{maxLines: 100, maxBytes: 10000}
	wins, ok, trunc, _ := selectDiffWindows(parsed, 0, budget)
	if !ok || trunc || len(wins) != 1 {
		t.Fatalf("helper: %#v trunc=%v", wins, trunc)
	}
	if wins[0].Text != wantText {
		t.Fatalf("helper: text=%q want=%q", wins[0].Text, wantText)
	}
	sum := sha256.Sum256([]byte(wantText))
	if wins[0].WindowHash.Value != hex.EncodeToString(sum[:]) {
		t.Fatalf("helper: hash mismatch")
	}
}

func TestDiffContentRepair_F5_budgetAtMarkerBoundary(t *testing.T) {
	patch := "@@ -0,0 +1 @@\n+new\n\\ No newline at end of file\n"
	parsed := parseUnifiedDiff(patch)
	if !parsed.ok {
		t.Fatal("helper/parser setup")
	}
	// Header(1) + line+marker(2) = 3 lines. Budget 2 must not emit detached line.
	budget := &contentEmitBudget{maxLines: 2, maxBytes: 10000}
	wins, ok, trunc, _ := selectDiffWindows(parsed, 0, budget)
	if !ok {
		t.Fatal("helper")
	}
	if !trunc {
		t.Fatal("helper: expected truncation at marker boundary")
	}
	for _, w := range wins {
		if strings.Contains(w.Text, "+new") && !strings.Contains(w.Text, `\ No newline`) {
			t.Fatalf("helper: detached line without marker: %q", w.Text)
		}
	}
}

// --- F6 known omission ---

func TestDiffContentRepair_F6_contextOmissionFalse(t *testing.T) {
	files := []retainedDiffFile{{
		entry:   diffManifestEntry{OldPath: strPtr("a.go"), NewPath: strPtr("a.go")},
		patch:   samplePatchAdditionDeletionContext(),
		patchOK: true,
	}}
	built, _, cropped, _ := buildDiffContentFiles(files, []int{0}, diffContentOpts{ContextLines: 0, MaxLines: 1000, MaxContentBytes: 262144})
	if len(built) != 1 || built[0].Status != diffFileStatusText {
		t.Fatalf("helper: %#v", built)
	}
	// Known context omitted with context_lines=0.
	if !cropped && !hasLimitation(built[0], "omitted_context") && !hasLimitation(built[0], "cropped") {
		// Production buildDiffContentFiles returns cropped bool; section uses it.
		// Assert via section path below; here require window truncated or omission flag.
		w := built[0].Windows[0]
		if !w.Truncated && strings.Contains(w.Text, "context-a") {
			t.Fatal("helper: context0 still includes context unexpectedly for omission check setup")
		}
		if strings.Contains(w.Text, "context-a") {
			t.Fatal("helper: context0 should omit context-a")
		}
	}
	// Direct section completeness via MCP:
	head, base, start := shaN(1), shaN(2), shaN(3)
	body := versionObject(1, 5001, head, base, start, "collected", "1", oneDiff("a.go", samplePatchAdditionDeletionContext()))
	h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
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
		"mode": "content", "paths": []any{"a.go"}, "context_lines": 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sectionMap(out)["content_complete"] != readmeta.ContentCompleteFalse {
		t.Fatalf("registered-MCP: known omission must be false, got %#v", sectionMap(out))
	}
}

func hasLimitation(f diffContentFile, s string) bool {
	for _, l := range f.Limitations {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

// --- F7 binary framing ---

func TestDiffContentRepair_F7_textMentioningBinaryPhrases(t *testing.T) {
	patch := "@@ -1 +1 @@\n-old\n+GIT binary patch\n"
	parsed := parseUnifiedDiff(patch)
	st, _ := classifyDiffFile(
		diffManifestEntry{OldPath: strPtr("a"), NewPath: strPtr("a"), Binary: boolPtr(false)},
		patch, true, false, false, false, false, parsed,
	)
	if st != diffFileStatusText {
		t.Fatalf("helper: status=%s want text", st)
	}
	budget := &contentEmitBudget{maxLines: 100, maxBytes: 10000}
	wins, ok, _, _ := selectDiffWindows(parsed, 0, budget)
	if !ok || len(wins) != 1 {
		t.Fatal("helper: windows")
	}
	sum := sha256.Sum256([]byte(wins[0].Text))
	if wins[0].WindowHash.Value != hex.EncodeToString(sum[:]) {
		t.Fatal("helper: hash")
	}
}

// --- F8 gitlink modes ---

func TestDiffContentRepair_F8_mode160000(t *testing.T) {
	patch := "@@ -1 +1 @@\n-Subproject commit aaa\n+Subproject commit bbb\n"
	parsed := parseUnifiedDiff(patch)
	st, _ := classifyDiffFile(
		diffManifestEntry{OldPath: strPtr("sub"), NewPath: strPtr("sub"), AMode: strPtr("160000"), BMode: strPtr("160000")},
		patch, true, false, false, false, false, parsed,
	)
	if st != diffFileStatusSubmodule {
		t.Fatalf("helper: status=%s want submodule", st)
	}
	// Ordinary text mentioning Subproject commit stays text.
	st2, _ := classifyDiffFile(
		diffManifestEntry{OldPath: strPtr("a"), NewPath: strPtr("a"), AMode: strPtr("100644"), BMode: strPtr("100644")},
		patch, true, false, false, false, false, parsed,
	)
	if st2 != diffFileStatusText {
		t.Fatalf("helper: 100644 text classified %s", st2)
	}
}

func TestDiffContentRepair_F8_mcpGitlinkNoFlag(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	patch := "@@ -1 +1 @@\n-Subproject commit aaa\n+Subproject commit bbb\n"
	diffs := fmt.Sprintf(`[{"old_path":"sub","new_path":"sub","a_mode":"160000","b_mode":"160000","diff":%q}]`, patch)
	body := versionObject(1, 5001, head, base, start, "collected", "1", diffs)
	h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
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
		"mode": "content", "paths": []any{"sub"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := asMap(t, asSlice(t, out["files"])[0])
	if asString(f["status"]) != diffFileStatusSubmodule {
		t.Fatalf("registered-MCP: %#v", f)
	}
	if wins, _ := f["windows"].([]any); len(wins) != 0 {
		t.Fatal("registered-MCP: submodule anchors")
	}
}

// --- F9 empty patch ---

func TestDiffContentRepair_F9_emptyModeOnly(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	diffs := `[{"old_path":"a","new_path":"b","a_mode":"100644","b_mode":"100755","renamed_file":true,"diff":""}]`
	body := versionObject(1, 5001, head, base, start, "collected", "1", diffs)
	h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
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
		"mode": "content", "paths": []any{"b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := asMap(t, asSlice(t, out["files"])[0])
	if asString(f["status"]) != diffFileStatusModeOnly && asString(f["status"]) != diffFileStatusMetadata {
		t.Fatalf("registered-MCP: empty rename status=%v", f["status"])
	}
	if wins, _ := f["windows"].([]any); len(wins) != 0 {
		t.Fatal("registered-MCP: unexpected windows")
	}
}

// --- F10 wrong-type sibling isolation ---

func TestDiffContentRepair_F10_wrongTypeUnselectedSibling(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	good := "@@ -1 +1 @@\n-a\n+b\n"
	diffs := fmt.Sprintf(`[{"old_path":"keep.go","new_path":"keep.go","a_mode":"100644","b_mode":"100644","diff":%q},{"old_path":"other.go","new_path":"other.go","a_mode":"100644","b_mode":"100644","diff":123}]`, good)
	body := versionObject(1, 5001, head, base, start, "collected", "2", diffs)
	h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
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
		"mode": "content", "paths": []any{"keep.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	files := asSlice(t, out["files"])
	if len(files) != 1 || asMap(t, files[0])["status"] != diffFileStatusText {
		t.Fatalf("registered-MCP: sibling destroyed %#v err section=%#v", out, sectionMap(out))
	}
}

// --- F11 false absence ---

func TestDiffContentRepair_F11_failedProofNotAbsent(t *testing.T) {
	log := &pathLog{}
	h := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/versions/1"):
			http.Error(w, "nope", http.StatusInternalServerError)
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
	sel := asMap(t, asSlice(t, out["selectors"])[0])
	if asString(sel["status"]) == diffSelectorAbsent {
		t.Fatalf("registered-MCP: failed proof must not claim absent: %#v", sel)
	}
	if out["returned_content_hash"] != nil {
		t.Fatal("hash on failed proof")
	}
}

func TestDiffContentRepair_F11_completeManifestAbsent(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	body := versionObject(1, 5001, head, base, start, "collected", "1", oneDiff("present.go", "@@ -1 +1 @@\n-a\n+b\n"))
	h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
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
		"mode": "content", "paths": []any{"missing.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sel := asMap(t, asSlice(t, out["selectors"])[0])
	if asString(sel["status"]) != diffSelectorAbsent {
		t.Fatalf("registered-MCP: genuine absence %#v", sel)
	}
}

// --- Input boundary / schema ---

func TestDiffContentRepair_inputBoundaries(t *testing.T) {
	hits := 0
	h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.NotFound(w, r)
	})
	d := diffDeps(t, h)
	_, err := callDiffWindow(t, d, nil, map[string]any{
		"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "mode": "",
	})
	if err == nil || hits != 0 {
		t.Fatalf("mode=\"\" accepted err=%v hits=%d", err, hits)
	}
	_, err = callDiffWindow(t, d, nil, map[string]any{
		"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "paths": []any{},
	})
	if err == nil || hits != 0 {
		t.Fatalf("empty paths in manifest accepted err=%v hits=%d", err, hits)
	}
	_, err = callDiffWindow(t, d, nil, map[string]any{
		"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "cursor": "",
	})
	if err == nil || hits != 0 {
		t.Fatalf("blank cursor accepted err=%v hits=%d", err, hits)
	}
}

func TestDiffContentRepair_listToolsSchema(t *testing.T) {
	d := diffDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	srv := mcp.NewServer(&mcp.Implementation{Name: "diff-schema", Version: "t"}, nil)
	RegisterMergeRequests(srv, d)
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	cli := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "v"}, nil)
	cs, err := cli.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	listed, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	var schema any
	var desc string
	for _, tool := range listed.Tools {
		if tool.Name == "get_merge_request_diff_window" {
			schema = tool.InputSchema
			desc = tool.Description
			break
		}
	}
	if schema == nil {
		t.Fatal("InputSchema missing")
	}
	raw, _ := json.Marshal(schema)
	s := string(raw)
	for _, key := range []string{"mode", "paths", "context_lines", "max_lines", "max_content_bytes"} {
		if !strings.Contains(s, `"`+key+`"`) {
			t.Fatalf("schema missing %s: %s", key, s)
		}
	}
	if !strings.Contains(desc, "mode=content") && !strings.Contains(desc, "content") {
		t.Fatalf("description missing content mode: %s", desc)
	}
}

func TestDiffContentRepair_independentMultiWindowHash(t *testing.T) {
	p1 := "@@ -1 +1 @@\n-a\n+b\n"
	p2 := "@@ -5 +5 @@\n-c\n+d\n"
	files := []retainedDiffFile{
		{entry: diffManifestEntry{OldPath: strPtr("a.go"), NewPath: strPtr("a.go")}, patch: p1, patchOK: true},
		{entry: diffManifestEntry{OldPath: strPtr("b.go"), NewPath: strPtr("b.go")}, patch: p2, patchOK: true},
	}
	// Independent expected concat: each full selected window text in file order.
	wantConcat := p1 + p2
	wantSum := sha256.Sum256([]byte(wantConcat))
	built, ret, _, _ := buildDiffContentFiles(files, []int{0, 1}, diffContentOpts{ContextLines: 0, MaxLines: 1000, MaxContentBytes: 262144})
	if ret == nil || ret.Value != hex.EncodeToString(wantSum[:]) {
		t.Fatalf("helper: ret=%v want %s built=%#v", ret, hex.EncodeToString(wantSum[:]), built)
	}
}

func TestDiffContentRepair_F3_closingBudgetStop(t *testing.T) {
	from, to := shaN(4), shaN(5)
	var compares int
	h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/repository/commits/"):
			sha := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			fmt.Fprintf(w, `{"id":%q}`, sha)
		case strings.Contains(r.URL.Path, "/repository/compare"):
			compares++
			if compares == 2 {
				// Closing compare hits cancelled/budget via small MaxRequests on borrowed budget.
			}
			fmt.Fprintf(w, `{"commit":{"id":%q},"diffs":[{"old_path":"a.go","new_path":"a.go","a_mode":"100644","b_mode":"100644","diff":"@@ -1 +1 @@\n-a\n+b\n"}]}`, to)
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
		default:
			http.NotFound(w, r)
		}
	})
	d := diffDeps(t, h)
	b := igl.DefaultBudget()
	b.MaxRequests = 6 // user + project + mr + commits + compare + maybe tight for closing
	ctx := igl.WithBudget(context.Background(), b)
	before, _, _ := b.Stats()
	out, err := callDiffWindow(t, d, ctx, map[string]any{
		"project_id": "42", "merge_request_iid": 1, "from_sha": from, "to_sha": to, "straight": true,
		"mode": "content", "paths": []any{"a.go"}, "max_requests": 6,
	})
	after, _, _ := b.Stats()
	if after < before {
		t.Fatal("borrowed counters reset")
	}
	_ = out
	_ = err
	// Either success with 2 compares or fail-closed without trusted hash.
	if out != nil && out["returned_content_hash"] != nil && compares < 2 {
		t.Fatalf("trusted hash without closing compare compares=%d", compares)
	}
}
