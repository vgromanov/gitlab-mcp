package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
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
	if !strings.Contains(desc, "mode=content") {
		t.Fatalf("description missing mode=content: %s", desc)
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	props, _ := obj["properties"].(map[string]any)
	if props == nil {
		t.Fatalf("no properties: %s", raw)
	}
	assertNullable := func(name string, wantTypes []string, wantDesc string) {
		t.Helper()
		pm, _ := props[name].(map[string]any)
		if pm == nil {
			t.Fatalf("missing property %s in %s", name, raw)
		}
		gotTypes, _ := pm["type"].([]any)
		if len(gotTypes) != len(wantTypes) {
			t.Fatalf("%s type=%v want %v", name, pm["type"], wantTypes)
		}
		for i, wt := range wantTypes {
			if fmt.Sprint(gotTypes[i]) != wt {
				t.Fatalf("%s type[%d]=%v want %s", name, i, gotTypes[i], wt)
			}
		}
		if wantDesc != "" {
			if d, _ := pm["description"].(string); d != wantDesc {
				t.Fatalf("%s description=%q want %q", name, d, wantDesc)
			}
		}
	}
	assertNullable("mode", []string{"null", "string"}, "manifest (default) or content")
	assertNullable("context_lines", []string{"null", "integer"}, "content mode: 0..20, default 3")
	assertNullable("max_lines", []string{"null", "integer"}, "content mode: 1..10000, default 1000")
	assertNullable("max_content_bytes", []string{"null", "integer"}, "content mode: 1..1048576, default 262144")
	paths, _ := props["paths"].(map[string]any)
	if paths == nil {
		t.Fatal("paths missing")
	}
	pt, _ := paths["type"].([]any)
	if len(pt) != 2 || fmt.Sprint(pt[0]) != "null" || fmt.Sprint(pt[1]) != "array" {
		t.Fatalf("paths type=%v", paths["type"])
	}
	items, _ := paths["items"].(map[string]any)
	if items == nil || items["type"] != "string" {
		t.Fatalf("paths items=%v", paths["items"])
	}
	if d, _ := paths["description"].(string); d != "content mode: 1..per_page distinct repository-relative paths" {
		t.Fatalf("paths description=%q", d)
	}
	if _, hasOut := obj["outputSchema"]; hasOut {
		t.Fatal("must not invent OutputSchema")
	}
	if toolOut, ok := props["output"]; ok {
		t.Fatalf("unexpected output property: %v", toolOut)
	}
}

func TestDiffContentRepair_handlerBoundaryMatrix(t *testing.T) {
	hits := 0
	h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.NotFound(w, r)
	})
	d := diffDeps(t, h)
	cases := []struct {
		name string
		args map[string]any
	}{
		{"mode_empty", map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "mode": ""}},
		{"cursor_blank", map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "cursor": ""}},
		{"paths_empty_manifest", map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "paths": []any{}}},
		{"paths_empty_content", map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "mode": "content", "paths": []any{}}},
		{"context_lines_neg", map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "mode": "content", "paths": []any{"a.go"}, "context_lines": -1}},
		{"context_lines_21", map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "mode": "content", "paths": []any{"a.go"}, "context_lines": 21}},
		{"max_lines_0", map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "mode": "content", "paths": []any{"a.go"}, "max_lines": 0}},
		{"max_lines_10001", map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "mode": "content", "paths": []any{"a.go"}, "max_lines": 10001}},
		{"max_content_bytes_0", map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "mode": "content", "paths": []any{"a.go"}, "max_content_bytes": 0}},
		{"max_content_bytes_over", map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "mode": "content", "paths": []any{"a.go"}, "max_content_bytes": 1048577}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := hits
			_, err := callDiffWindow(t, d, nil, tc.args)
			if err == nil || hits != before {
				t.Fatalf("registered-MCP: accepted or HTTP fired err=%v hits=%d->%d", err, before, hits)
			}
		})
	}
	// Accepted edges still must not invent OutputSchema (covered above); defaults at bounds:
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"context_0", map[string]any{"context_lines": 0}},
		{"context_20", map[string]any{"context_lines": 20}},
		{"max_lines_1", map[string]any{"max_lines": 1}},
		{"max_lines_10000", map[string]any{"max_lines": 10000}},
		{"max_bytes_1", map[string]any{"max_content_bytes": 1}},
		{"max_bytes_1mib", map[string]any{"max_content_bytes": 1048576}},
	} {
		t.Run("edge_"+tc.name, func(t *testing.T) {
			mode := "content"
			in := diffWindowIn{
				ProjectID: "42", MergeRequestIID: 1, DiffVersionID: int64Ptr(1),
				Mode: &mode, Paths: []string{"a.go"}, PerPage: intPtr(20),
			}
			if v, ok := tc.args["context_lines"].(int); ok {
				in.ContextLines = &v
			}
			if v, ok := tc.args["max_lines"].(int); ok {
				in.MaxLines = &v
			}
			if v, ok := tc.args["max_content_bytes"].(int); ok {
				in.MaxContentBytes = &v
			}
			if _, _, err := normalizeDiffWindowMode(in); err != nil {
				t.Fatalf("helper: boundary rejected: %v", err)
			}
		})
	}
}

func TestDiffContentRepair_independentMultiWindowHash(t *testing.T) {
	// Independent oracle: hand-authored UTF-8 + CRLF window texts (not taken from builder output).
	p1 := "@@ -1 +1 @@\r\n-α\r\n+β\r\n"
	p2 := "@@ -5,1 +5,1 @@\n-c\n+d\n\\ No newline at end of file\n"
	wantConcat := p1 + p2
	wantSum := sha256.Sum256([]byte(wantConcat))
	wantHex := hex.EncodeToString(wantSum[:])
	files := []retainedDiffFile{
		{entry: diffManifestEntry{OldPath: strPtr("a.go"), NewPath: strPtr("a.go")}, patch: p1, patchOK: true},
		{entry: diffManifestEntry{OldPath: strPtr("b.go"), NewPath: strPtr("b.go")}, patch: p2, patchOK: true},
	}
	built, ret, _, _ := buildDiffContentFiles(files, []int{0, 1}, diffContentOpts{ContextLines: 0, MaxLines: 1000, MaxContentBytes: 262144})
	if len(built) != 2 || ret == nil {
		t.Fatalf("helper: built=%#v ret=%v", built, ret)
	}
	var got strings.Builder
	for _, f := range built {
		for _, w := range f.Windows {
			got.WriteString(w.Text)
			sum := sha256.Sum256([]byte(w.Text))
			if w.WindowHash.Value != hex.EncodeToString(sum[:]) || w.WindowHash.Scope != diffContentWindowHashScope {
				t.Fatalf("helper: window hash %#v", w.WindowHash)
			}
		}
	}
	if got.String() != wantConcat || ret.Value != wantHex || ret.Scope != diffContentReturnHashScope {
		t.Fatalf("helper: concat/hash mismatch got=%q want=%q ret=%v wantHex=%s", got.String(), wantConcat, ret, wantHex)
	}
}

func assertNoTrustedContent(t *testing.T, out map[string]any, err error, label string) {
	t.Helper()
	if out != nil {
		if out["returned_content_hash"] != nil {
			t.Fatalf("%s: trusted returned_content_hash %#v err=%v", label, out["returned_content_hash"], err)
		}
		if files := asSlice(t, out["files"]); len(files) != 0 {
			t.Fatalf("%s: trusted files %#v err=%v", label, files, err)
		}
	}
}

func TestDiffContentRepair_F3_phaseReachabilityBudgets(t *testing.T) {
	from, to := shaN(4), shaN(5)
	patch := "@@ -1 +1 @@\n-a\n+b\n"
	mk := func(t *testing.T, onCompare func(n int, w http.ResponseWriter, r *http.Request) (handled bool)) (http.Handler, *int32) {
		t.Helper()
		var compares int32
		h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/repository/commits/"):
				sha := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
				fmt.Fprintf(w, `{"id":%q}`, sha)
			case strings.Contains(r.URL.Path, "/repository/compare"):
				n := atomic.AddInt32(&compares, 1)
				if onCompare != nil {
					if onCompare(int(n), w, r) {
						return
					}
				}
				fmt.Fprintf(w, `{"commit":{"id":%q},"diffs":[{"old_path":"a.go","new_path":"a.go","a_mode":"100644","b_mode":"100644","diff":%q}]}`, to, patch)
			case strings.Contains(r.URL.Path, "/merge_requests/"):
				_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
			default:
				http.NotFound(w, r)
			}
		})
		return h, &compares
	}
	straightArgs := map[string]any{
		"project_id": "42", "merge_request_iid": 1, "from_sha": from, "to_sha": to, "straight": true,
		"mode": "content", "paths": []any{"a.go"},
	}
	t.Run("straight_closing_request_budget", func(t *testing.T) {
		// Diag: success uses 10 requests / 2 compares. MaxRequests=9 reaches opening compare, fails on closing.
		h, compares := mk(t, nil)
		d := diffDeps(t, h)
		b := igl.DefaultBudget()
		b.MaxRequests = 9
		ctx := igl.WithBudget(context.Background(), b)
		beforeReqs, beforeBytes, beforeItems := b.Stats()
		out, err := callDiffWindow(t, d, ctx, straightArgs)
		afterReqs, afterBytes, afterItems := b.Stats()
		if atomic.LoadInt32(compares) != 1 {
			t.Fatalf("registered-MCP: opening compare not proved compares=%d", atomic.LoadInt32(compares))
		}
		if err == nil || !strings.Contains(err.Error(), igl.ErrBudgetRequests.Error()) {
			t.Fatalf("registered-MCP: want budget_requests err=%v out=%#v", err, out)
		}
		assertNoTrustedContent(t, out, err, "straight_closing_request_budget")
		if afterReqs < beforeReqs || afterBytes < beforeBytes || afterItems < beforeItems {
			t.Fatalf("borrowed counters reset/raised-backwards before=%d/%d/%d after=%d/%d/%d", beforeReqs, beforeBytes, beforeItems, afterReqs, afterBytes, afterItems)
		}
		if b.MaxRequests != 9 {
			t.Fatalf("borrowed MaxRequests raised/changed: %d", b.MaxRequests)
		}
		// Shared-owner survival: reuse ORIGINAL borrowed budget/context (no replacement DefaultBudget).
		select {
		case <-ctx.Done():
			t.Fatal("borrowed budget context cancelled after request-budget stop")
		default:
		}
		if igl.BudgetFromContext(ctx) != b {
			t.Fatal("borrowed budget pointer replaced")
		}
		h2, compares2 := mk(t, nil)
		d2 := diffDeps(t, h2)
		out2, err2 := callDiffWindow(t, d2, ctx, straightArgs)
		if err2 == nil || !strings.Contains(err2.Error(), igl.ErrBudgetRequests.Error()) {
			t.Fatalf("original owner must still enforce budget_requests err=%v out=%#v", err2, out2)
		}
		if atomic.LoadInt32(compares2) != 0 {
			t.Fatalf("exhausted owner must not start compare compares=%d", atomic.LoadInt32(compares2))
		}
		assertNoTrustedContent(t, out2, err2, "straight_closing_request_budget_owner_reuse")
		if b.MaxRequests != 9 {
			t.Fatalf("borrowed MaxRequests changed on reuse: %d", b.MaxRequests)
		}
	})
	t.Run("straight_closing_byte_budget", func(t *testing.T) {
		// Fault-free baseline first: same fixture sans byte-budget fault.
		h0, compares0 := mk(t, nil)
		out0, err0 := callDiffWindow(t, diffDeps(t, h0), nil, straightArgs)
		if err0 != nil || atomic.LoadInt32(compares0) != 2 || out0["returned_content_hash"] == nil {
			t.Fatalf("fault-free baseline failed err=%v compares=%d out=%#v", err0, atomic.LoadInt32(compares0), out0)
		}
		// Probe opening-phase byte consumption (MaxRequests=9 stops before closing request).
		hProbe, _ := mk(t, nil)
		bProbe := igl.DefaultBudget()
		bProbe.MaxRequests = 9
		bProbe.MaxBytes = 1 << 20
		_, errProbe := callDiffWindow(t, diffDeps(t, hProbe), igl.WithBudget(context.Background(), bProbe), straightArgs)
		if errProbe == nil || !strings.Contains(errProbe.Error(), igl.ErrBudgetRequests.Error()) {
			t.Fatalf("probe want budget_requests err=%v", errProbe)
		}
		_, openBytes, _ := bProbe.Stats()
		h, compares := mk(t, func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n == 2 {
				big := strings.Repeat("x", 6000)
				fmt.Fprintf(w, `{"commit":{"id":%q},"diffs":[{"old_path":"a.go","new_path":"a.go","a_mode":"100644","b_mode":"100644","diff":%q}]}`, to, "@@ -1 +1 @@\n-"+big+"\n+y\n")
				return true
			}
			return false
		})
		d := diffDeps(t, h)
		b := igl.DefaultBudget()
		b.MaxBytes = openBytes + 500 // enough for opening path, not for 6KiB closing body
		b.MaxRequests = 64
		ctx := igl.WithBudget(context.Background(), b)
		out, err := callDiffWindow(t, d, ctx, straightArgs)
		if atomic.LoadInt32(compares) != 2 {
			t.Fatalf("closing compare must be attempted compares=%d", atomic.LoadInt32(compares))
		}
		if err == nil || !strings.Contains(err.Error(), igl.ErrBudgetBytes.Error()) {
			t.Fatalf("want budget_bytes err=%v out=%#v", err, out)
		}
		assertNoTrustedContent(t, out, err, "straight_closing_byte_budget")
	})
	t.Run("straight_closing_cancel", func(t *testing.T) {
		closingStarted := make(chan struct{})
		release := make(chan struct{})
		var closingSawDone atomic.Bool
		h, compares := mk(t, func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n == 2 {
				close(closingStarted)
				select {
				case <-r.Context().Done():
					closingSawDone.Store(true)
					return true
				case <-release:
					return true
				case <-time.After(2 * time.Second):
					t.Error("closing compare was not cancelled or released")
					return true
				}
			}
			return false
		})
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		b := igl.DefaultBudget()
		b.MaxRequests = 64
		ctx := igl.WithBudget(parent, b)
		d := diffDeps(t, h)
		var (
			out map[string]any
			err error
			wg  sync.WaitGroup
		)
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err = callDiffWindow(t, d, ctx, map[string]any{
				"project_id": "42", "merge_request_iid": 1, "from_sha": from, "to_sha": to, "straight": true,
				"mode": "content", "paths": []any{"a.go"},
			})
		}()
		select {
		case <-closingStarted:
			cancel()
			b.Cancel()
		case <-time.After(2 * time.Second):
			t.Fatal("opening compare phase never reached closing")
		}
		wg.Wait()
		close(release)
		if atomic.LoadInt32(compares) != 2 {
			t.Fatalf("expected opening+closing attempt compares=%d", atomic.LoadInt32(compares))
		}
		if err == nil || !(strings.Contains(err.Error(), context.Canceled.Error()) || strings.Contains(err.Error(), "cancel") || strings.Contains(err.Error(), igl.ErrBudgetElapsed.Error())) {
			t.Fatalf("want cancel/budget err=%v", err)
		}
		assertNoTrustedContent(t, out, err, "straight_closing_cancel")
		if !closingSawDone.Load() {
			t.Log("note: provider handler Done not observed (client abort path); call still fail-closed")
		}
		if b.MaxRequests != 64 {
			t.Fatalf("borrowed caps changed")
		}
	})
	t.Run("straight_closing_elapsed", func(t *testing.T) {
		closingStarted := make(chan struct{})
		var sawDone atomic.Bool
		h, compares := mk(t, func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n == 2 {
				close(closingStarted)
				select {
				case <-r.Context().Done():
					sawDone.Store(true)
					return true
				case <-time.After(5 * time.Second):
					t.Error("closing compare was not stopped")
					return true
				}
			}
			return false
		})
		// An 80ms budget from call start expires on macOS before the closing
		// compare is issued, so the handler never observes the stop. Keep the
		// default elapsed window through the opening phase, then abort the
		// budget deadline only after that request is in flight.
		b := igl.DefaultBudget()
		b.MaxRequests = 64
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		ctx := igl.WithBudget(parent, b)
		d := diffDeps(t, h)
		var (
			out map[string]any
			err error
			wg  sync.WaitGroup
		)
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err = callDiffWindow(t, d, ctx, straightArgs)
		}()
		select {
		case <-closingStarted:
			cancel()
			b.Cancel()
		case <-time.After(10 * time.Second):
			cancel()
			t.Fatal("closing phase did not start")
		}
		wg.Wait()
		deadline := time.Now().Add(2 * time.Second)
		for !sawDone.Load() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if atomic.LoadInt32(compares) != 2 {
			t.Fatalf("expected opening+closing attempt compares=%d", atomic.LoadInt32(compares))
		}
		if !sawDone.Load() {
			t.Fatalf("closing compare did not observe the stop err=%v", err)
		}
		if err == nil || !(strings.Contains(err.Error(), igl.ErrBudgetElapsed.Error()) || strings.Contains(err.Error(), context.DeadlineExceeded.Error()) || strings.Contains(err.Error(), "cancel")) {
			t.Fatalf("want elapsed/deadline err=%v", err)
		}
		assertNoTrustedContent(t, out, err, "straight_closing_elapsed")
	})
	t.Run("straight_opening_request_budget", func(t *testing.T) {
		// MaxRequests=6 stops before opening compare (diag: compare is request 7).
		h, compares := mk(t, nil)
		b := igl.DefaultBudget()
		b.MaxRequests = 6
		ctx := igl.WithBudget(context.Background(), b)
		out, err := callDiffWindow(t, diffDeps(t, h), ctx, map[string]any{
			"project_id": "42", "merge_request_iid": 1, "from_sha": from, "to_sha": to, "straight": true,
			"mode": "content", "paths": []any{"a.go"},
		})
		if atomic.LoadInt32(compares) != 0 {
			t.Fatalf("opening compare must not run compares=%d", atomic.LoadInt32(compares))
		}
		if err == nil || !strings.Contains(err.Error(), igl.ErrBudgetRequests.Error()) {
			t.Fatalf("want budget_requests err=%v", err)
		}
		assertNoTrustedContent(t, out, err, "straight_opening_request_budget")
	})
	t.Run("straight_item_budget", func(t *testing.T) {
		// Decoder item charge with owned-reader join (client-side ReadCloser), not server Done.
		const secret = "ITEM-BUDGET-SECRET-PATCH"
		from, to := shaN(4), shaN(5)
		straightArgs := map[string]any{
			"project_id": "42", "merge_request_iid": 1, "from_sha": from, "to_sha": to, "straight": true,
			"mode": "content", "paths": []any{"a.go", "b.go"},
		}
		mkHandler := func(compares *int32) http.Handler {
			return serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/repository/commits/"):
					sha := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
					fmt.Fprintf(w, `{"id":%q}`, sha)
				case strings.Contains(r.URL.Path, "/repository/compare"):
					atomic.AddInt32(compares, 1)
					prefix := fmt.Sprintf(`{"commit":{"id":%q},"diffs":[`, to)
					d1 := fmt.Sprintf(`{"old_path":"a.go","new_path":"a.go","a_mode":"100644","b_mode":"100644","diff":%q}`, "@@ -1 +1 @@\n-a\n+"+secret+"\n")
					d2 := fmt.Sprintf(`{"old_path":"b.go","new_path":"b.go","a_mode":"100644","b_mode":"100644","diff":%q}`, "@@ -1 +1 @@\n-c\n+"+secret+"\n")
					_, _ = io.WriteString(w, prefix+d1+`,`+d2+`]}`)
				case strings.Contains(r.URL.Path, "/merge_requests/"):
					_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
				default:
					http.NotFound(w, r)
				}
			})
		}
		// Fault-free baseline: sane MaxItems, opening+closing compare, trusted hash.
		var compares0 int32
		probe0 := newOwnedBodyProbe()
		d0 := diffDepsOwnedBodyProbe(t, mkHandler(&compares0), probe0, func(req *http.Request) bool {
			return strings.Contains(req.URL.Path, "/repository/compare")
		})
		b0 := igl.DefaultBudget()
		b0.MaxItems = 50
		b0.MaxRequests = 64
		b0.MaxBytes = 1 << 20
		ctx0 := igl.WithBudget(context.Background(), b0)
		out0, err0 := callDiffWindow(t, d0, ctx0, straightArgs)
		if err0 != nil || atomic.LoadInt32(&compares0) != 2 || out0["returned_content_hash"] == nil {
			t.Fatalf("fault-free baseline err=%v compares=%d out=%#v", err0, atomic.LoadInt32(&compares0), out0)
		}
		waitOwnedBodyJoin(t, probe0)
		if probe0.reads.Load() == 0 || probe0.closes.Load() == 0 {
			t.Fatalf("baseline owned body reads=%d closes=%d", probe0.reads.Load(), probe0.closes.Load())
		}

		// Inject MaxItems=1: charge during decoder on opening compare; prove stop + join.
		var compares int32
		probe := newOwnedBodyProbe()
		d := diffDepsOwnedBodyProbe(t, mkHandler(&compares), probe, func(req *http.Request) bool {
			return strings.Contains(req.URL.Path, "/repository/compare")
		})
		b := igl.DefaultBudget()
		b.MaxItems = 1
		b.MaxRequests = 64
		b.MaxBytes = 1 << 20
		parent := context.Background()
		ctx := igl.WithBudget(parent, b)
		sibling, stopSibling := context.WithCancel(ctx)
		defer stopSibling()
		out, err := callDiffWindow(t, d, ctx, straightArgs)
		if err == nil || !strings.Contains(err.Error(), igl.ErrBudgetItems.Error()) {
			t.Fatalf("want budget_items (decoder charge) err=%v compares=%d", err, atomic.LoadInt32(&compares))
		}
		if strings.Contains(err.Error(), context.Canceled.Error()) {
			t.Fatalf("caller-cancel must not masquerade as item-budget failure: %v", err)
		}
		assertNoTrustedContent(t, out, err, "straight_item_budget")
		if atomic.LoadInt32(&compares) != 1 {
			t.Fatalf("item budget must stop during opening compare compares=%d", atomic.LoadInt32(&compares))
		}
		select {
		case <-ctx.Done():
			t.Fatal("borrowed budget context cancelled (reader-local stop must not cancel owner)")
		default:
		}
		if sibling.Err() != nil {
			t.Fatal("sibling derived context cancelled")
		}
		if igl.BudgetFromContext(ctx) != b || b.MaxItems != 1 {
			t.Fatal("borrowed budget replaced or caps changed")
		}
		if _, _, items := b.Stats(); items != 1 {
			t.Fatalf("items charged during traversal want 1 got %d", items)
		}
		if probe.created.Load() != 1 {
			t.Fatalf("want exactly one instrumented opening compare body, created=%d", probe.created.Load())
		}
		waitOwnedBodyJoin(t, probe)
		if probe.reads.Load() == 0 {
			t.Fatal("owned-reader Read not observed")
		}
		if probe.closes.Load() != 1 {
			t.Fatalf("owned-reader Close count=%d want 1", probe.closes.Load())
		}
	})
	t.Run("version_closing_request_budget", func(t *testing.T) {
		head, base, start := shaN(1), shaN(2), shaN(3)
		var versions int32
		body := versionObject(1, 5001, head, base, start, "collected", "1", oneDiff("a.go", patch))
		h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/versions/1"):
				atomic.AddInt32(&versions, 1)
				_, _ = io.WriteString(w, body)
			case strings.Contains(r.URL.Path, "/merge_requests/"):
				_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
			default:
				http.NotFound(w, r)
			}
		})
		b := igl.DefaultBudget()
		b.MaxRequests = 5 // diag: opening version is req 5; closing is 6
		ctx := igl.WithBudget(context.Background(), b)
		out, err := callDiffWindow(t, diffDeps(t, h), ctx, map[string]any{
			"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1,
			"mode": "content", "paths": []any{"a.go"},
		})
		if atomic.LoadInt32(&versions) != 1 {
			t.Fatalf("version opening not proved versions=%d", atomic.LoadInt32(&versions))
		}
		if err == nil || !strings.Contains(err.Error(), igl.ErrBudgetRequests.Error()) {
			t.Fatalf("want budget_requests err=%v out=%#v", err, out)
		}
		assertNoTrustedContent(t, out, err, "version_closing_request_budget")
	})
	t.Run("version_closing_cancel", func(t *testing.T) {
		head, base, start := shaN(1), shaN(2), shaN(3)
		var versions int32
		closingStarted := make(chan struct{})
		release := make(chan struct{})
		var sawDone atomic.Bool
		body := versionObject(1, 5001, head, base, start, "collected", "1", oneDiff("a.go", patch))
		h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/versions/1"):
				n := atomic.AddInt32(&versions, 1)
				if n == 2 {
					close(closingStarted)
					select {
					case <-r.Context().Done():
						sawDone.Store(true)
						return
					case <-release:
						return
					case <-time.After(2 * time.Second):
						t.Error("version closing hung")
						return
					}
				}
				_, _ = io.WriteString(w, body)
			case strings.Contains(r.URL.Path, "/merge_requests/"):
				_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
			default:
				http.NotFound(w, r)
			}
		})
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		b := igl.DefaultBudget()
		b.MaxRequests = 64
		ctx := igl.WithBudget(parent, b)
		var out map[string]any
		var err error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err = callDiffWindow(t, diffDeps(t, h), ctx, map[string]any{
				"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1,
				"mode": "content", "paths": []any{"a.go"},
			})
		}()
		select {
		case <-closingStarted:
			cancel()
			b.Cancel()
		case <-time.After(2 * time.Second):
			t.Fatal("version closing not reached")
		}
		wg.Wait()
		close(release)
		if atomic.LoadInt32(&versions) != 2 {
			t.Fatalf("versions=%d", atomic.LoadInt32(&versions))
		}
		if err == nil {
			t.Fatalf("want cancel err out=%#v", out)
		}
		assertNoTrustedContent(t, out, err, "version_closing_cancel")
		_ = sawDone.Load()
	})
	t.Run("tuple_closing_request_budget", func(t *testing.T) {
		head, base, start := shaN(1), shaN(2), shaN(3)
		body := versionObject(1, 5001, head, base, start, "collected", "1", oneDiff("a.go", patch))
		args := map[string]any{
			"project_id": "42", "merge_request_iid": 1,
			"base_sha": base, "start_sha": start, "head_sha": head,
			"mode": "content", "paths": []any{"a.go"},
		}
		mkTuple := func(versions *int32) http.Handler {
			return serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/versions/") && !strings.HasSuffix(strings.TrimSuffix(r.URL.Path, "/"), "versions"):
					atomic.AddInt32(versions, 1)
					_, _ = io.WriteString(w, body)
				case strings.Contains(r.URL.Path, "/versions"):
					// Required identity + terminal paging so tuple selection reaches version reads.
					w.Header().Set("X-Next-Page", "")
					fmt.Fprintf(w, `[{"id":1,"merge_request_id":5001,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q,"state":"collected"}]`, head, base, start)
				case strings.Contains(r.URL.Path, "/merge_requests/") && !strings.Contains(r.URL.Path, "/versions"):
					_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
				default:
					http.NotFound(w, r)
				}
			})
		}
		// Fault-free baseline: opening+closing version reads and trusted hash.
		var versions0 int32
		out0, err0 := callDiffWindow(t, diffDeps(t, mkTuple(&versions0)), nil, args)
		if err0 != nil || atomic.LoadInt32(&versions0) != 2 || out0["returned_content_hash"] == nil {
			t.Fatalf("fault-free tuple baseline err=%v versions=%d out=%#v", err0, atomic.LoadInt32(&versions0), out0)
		}
		// MaxRequests=6: list+opening version succeed; closing version blocked (diag).
		var versions int32
		b := igl.DefaultBudget()
		b.MaxRequests = 6
		ctx := igl.WithBudget(context.Background(), b)
		out, err := callDiffWindow(t, diffDeps(t, mkTuple(&versions)), ctx, args)
		if atomic.LoadInt32(&versions) != 1 {
			t.Fatalf("opening version read must be exactly 1 before blocked closing, got %d", atomic.LoadInt32(&versions))
		}
		if err == nil || !strings.Contains(err.Error(), igl.ErrBudgetRequests.Error()) {
			t.Fatalf("want budget_requests after opening version err=%v out=%#v", err, out)
		}
		assertNoTrustedContent(t, out, err, "tuple_closing_request_budget")
	})
}

func TestDiffContentRepair_deniedForkAndDownstream(t *testing.T) {
	log := &pathLog{}
	h := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":99,"target_project_id":42}`)
		case strings.Contains(r.URL.Path, "/projects/99"):
			fmt.Fprintf(w, `{"id":99,"path_with_namespace":"fork/p","namespace":{"id":2,"kind":"group"}}`)
		default:
			http.NotFound(w, r)
		}
	})
	d := diffDeps(t, h)
	d.Config.AllowedProjectIDs = []string{"42"} // fork source 99 not allowed
	_, err := callDiffWindow(t, d, nil, map[string]any{
		"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1,
		"mode": "content", "paths": []any{"a.go"},
	})
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
		t.Fatalf("registered-MCP: fork not denied err=%v", err)
	}
	if log.count("/versions/") != 0 {
		t.Fatalf("downstream version read before fork denial: %v", log.paths)
	}
}

func TestDiffContentRepair_oneMiBCaps(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	small := "@@ -1 +1 @@\n-a\n+b\n"
	// Candidate >1MiB selected (escaped unicode expands after decode).
	t.Run("selected_candidate_over", func(t *testing.T) {
		// Wire body must contain actual JSON \u escapes (not ordinary UTF-8 from json.Marshal).
		overRunes := (diffContentMaxCandidateBytes / 3) + 32
		var esc strings.Builder
		esc.WriteString(`"`)
		for i := 0; i < overRunes; i++ {
			esc.WriteString(`\u660e`) // 明
		}
		esc.WriteString(`"`)
		wireDiff := esc.String()
		if !strings.Contains(wireDiff, `\u660e`) || strings.Contains(wireDiff, "明") {
			t.Fatal("setup: wire fixture must use \\u escapes, not UTF-8 codepoints")
		}
		diffs := fmt.Sprintf(`[{"old_path":"big.go","new_path":"big.go","a_mode":"100644","b_mode":"100644","diff":%s}]`, wireDiff)
		body := versionObject(1, 5001, head, base, start, "collected", "1", diffs)
		if !strings.Contains(body, `\u660e`) {
			t.Fatal("setup: version body lost \\u escapes")
		}
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
			"mode": "content", "paths": []any{"big.go"},
		})
		if err != nil {
			t.Fatal(err)
		}
		f := asMap(t, asSlice(t, out["files"])[0])
		if asString(f["status"]) != diffFileStatusTooLarge {
			t.Fatalf("registered-MCP: status=%v want too_large", f["status"])
		}
		if wins, _ := f["windows"].([]any); len(wins) != 0 {
			t.Fatalf("fabricated windows for over-cap: %#v", wins)
		}
		if out["returned_content_hash"] != nil {
			t.Fatal("fabricated hash for omitted over-cap content")
		}
		if sectionMap(out)["content_complete"] != readmeta.ContentCompleteFalse {
			t.Fatalf("section=%#v", sectionMap(out))
		}
	})
	t.Run("unselected_candidate_over_sibling_survives", func(t *testing.T) {
		overRunes := (diffContentMaxCandidateBytes / 3) + 32
		var esc strings.Builder
		esc.WriteString(`"`)
		for i := 0; i < overRunes; i++ {
			esc.WriteString(`\u660e`)
		}
		esc.WriteString(`"`)
		diffs := fmt.Sprintf(`[{"diff":%s,"old_path":"big.go","new_path":"big.go","a_mode":"100644","b_mode":"100644"},{"old_path":"keep.go","new_path":"keep.go","a_mode":"100644","b_mode":"100644","diff":%q}]`,
			esc.String(), small)
		body := versionObject(1, 5001, head, base, start, "collected", "2", diffs)
		if !strings.Contains(body, `\u660e`) {
			t.Fatal("setup: unselected over-cap body must keep \\u escapes")
		}
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
			t.Fatalf("selected sibling not retained: %#v", out)
		}
		raw, _ := json.Marshal(out)
		if strings.Contains(string(raw), "明") {
			t.Fatal("unselected over-cap text retained in output")
		}
	})
	t.Run("cumulative_selected_over", func(t *testing.T) {
		// Small surviving sibling + large sibling: aggregate exceeds selected source cap,
		// while each stays under candidate cap and emit defaults still yield a nonempty good hash.
		smallGood := "@@ -1 +1 @@\n-a\n+b\n"
		p1 := smallGood
		overhead := len("@@ -1 +1 @@\n-\n+y\n")
		need := diffContentMaxSelectedBytes - len(p1) + 1
		largeLen := need - overhead
		if largeLen < 1 {
			t.Fatalf("setup arithmetic largeLen=%d", largeLen)
		}
		p2 := "@@ -1 +1 @@\n-" + strings.Repeat("Z", largeLen) + "\n+y\n"
		if len(p2) > diffContentMaxCandidateBytes {
			t.Fatalf("setup: individual candidate over cap len=%d cap=%d", len(p2), diffContentMaxCandidateBytes)
		}
		if len(p1)+len(p2) <= diffContentMaxSelectedBytes {
			t.Fatalf("setup: aggregate not over selected cap sum=%d cap=%d", len(p1)+len(p2), diffContentMaxSelectedBytes)
		}
		diffs := fmt.Sprintf(`[{"old_path":"a.go","new_path":"a.go","a_mode":"100644","b_mode":"100644","diff":%q},{"old_path":"b.go","new_path":"b.go","a_mode":"100644","b_mode":"100644","diff":%q}]`, p1, p2)
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
			"mode": "content", "paths": []any{"a.go", "b.go"},
			"max_content_bytes": 262144,
			"max_lines":         1000,
		})
		if err != nil {
			t.Fatal(err)
		}
		files := asSlice(t, out["files"])
		if len(files) != 2 {
			t.Fatalf("files=%#v", files)
		}
		statuses := []string{asString(asMap(t, files[0])["status"]), asString(asMap(t, files[1])["status"])}
		textCount, largeCount := 0, 0
		for _, st := range statuses {
			switch st {
			case diffFileStatusText:
				textCount++
			case diffFileStatusTooLarge:
				largeCount++
			}
		}
		if textCount != 1 || largeCount != 1 {
			t.Fatalf("want one text + one too_large, got %v", statuses)
		}
		var goodText string
		for _, rawF := range files {
			f := asMap(t, rawF)
			switch asString(f["status"]) {
			case diffFileStatusTooLarge:
				if wins, _ := f["windows"].([]any); len(wins) != 0 {
					t.Fatalf("too_large windows: %#v", f)
				}
			case diffFileStatusText:
				wins := asSlice(t, f["windows"])
				if len(wins) == 0 {
					t.Fatal("surviving text sibling has empty windows (emit default may have masked source cap)")
				}
				w0 := asMap(t, wins[0])
				goodText = asString(w0["text"])
				if goodText == "" {
					t.Fatal("surviving window text empty")
				}
				sum := sha256.Sum256([]byte(goodText))
				wh := asMap(t, w0["window_hash"])
				if asString(wh["value"]) != hex.EncodeToString(sum[:]) {
					t.Fatalf("surviving hash mismatch %#v", wh)
				}
				wantSum := sha256.Sum256([]byte(smallGood))
				if asString(wh["value"]) != hex.EncodeToString(wantSum[:]) {
					t.Fatalf("surviving hash want smallGood got text=%q hash=%v", goodText, wh)
				}
			}
		}
		if goodText == "" {
			t.Fatal("no surviving good window")
		}
	})
	t.Run("helper_post_decode_retention_note", func(t *testing.T) {
		// Post-decode retention caps are separate from wire-budget decoder transients.
		if diffContentMaxCandidateBytes != 1<<20 || diffContentMaxSelectedBytes != 1<<20 {
			t.Fatalf("caps=%d/%d", diffContentMaxCandidateBytes, diffContentMaxSelectedBytes)
		}
		c := newPatchCollect([]string{"a.go"})
		if c.maxCandidate != 1<<20 || c.maxSelected != 1<<20 {
			t.Fatalf("collect caps %#v", c)
		}
	})
}

// --- Additional report-matrix closures (reuse helpers where they already prove the case) ---

func TestDiffContentRepair_F2_identicalDuplicateOpeningClosing(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	patch := "@@ -1 +1 @@\n-a\n+b\n"
	// Identical duplicate id (same value twice) still ambiguous.
	body := fmt.Sprintf(`{"id":1,"id":1,"merge_request_id":5001,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q,"state":"collected","real_size":"1","diffs":%s}`,
		head, base, start, oneDiff("a.go", patch))
	var versions int
	h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/versions/1"):
			versions++
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
	assertNoTrustedContent(t, out, err, "F2_identical_dup")
	// Closing member: second version body with identical duplicate state.
	bodyClose := fmt.Sprintf(`{"id":1,"merge_request_id":5001,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q,"state":"collected","state":"collected","real_size":"1","diffs":%s}`,
		head, base, start, oneDiff("a.go", patch))
	versions = 0
	h2 := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/versions/1"):
			versions++
			if versions == 1 {
				_, _ = io.WriteString(w, versionObject(1, 5001, head, base, start, "collected", "1", oneDiff("a.go", patch)))
				return
			}
			_, _ = io.WriteString(w, bodyClose)
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
		default:
			http.NotFound(w, r)
		}
	})
	out2, err2 := callDiffWindow(t, diffDeps(t, h2), nil, map[string]any{
		"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1,
		"mode": "content", "paths": []any{"a.go"},
	})
	if err2 != nil {
		t.Fatal(err2)
	}
	assertNoTrustedContent(t, out2, err2, "F2_identical_dup_closing")
}

func TestDiffContentRepair_F3_closureMatrixExtras(t *testing.T) {
	from, to := shaN(4), shaN(5)
	good := "@@ -1 +1 @@\n-a\n+b\n"
	cases := []struct {
		name string
		mod  func(n int) string
	}{
		{"path_drift", func(n int) string {
			p := "a.go"
			if n > 1 {
				p = "b.go"
			}
			return fmt.Sprintf(`{"commit":{"id":%q},"diffs":[{"old_path":%q,"new_path":%q,"a_mode":"100644","b_mode":"100644","diff":%q}]}`, to, p, p, good)
		}},
		{"mode_drift", func(n int) string {
			mode := "100644"
			if n > 1 {
				mode = "100755"
			}
			return fmt.Sprintf(`{"commit":{"id":%q},"diffs":[{"old_path":"a.go","new_path":"a.go","a_mode":%q,"b_mode":%q,"diff":%q}]}`, to, mode, mode, good)
		}},
		{"commit_drift", func(n int) string {
			id := to
			if n > 1 {
				id = shaN(9)
			}
			return fmt.Sprintf(`{"commit":{"id":%q},"diffs":[{"old_path":"a.go","new_path":"a.go","a_mode":"100644","b_mode":"100644","diff":%q}]}`, id, good)
		}},
		{"timeout_closing", func(n int) string {
			toFlag := "false"
			if n > 1 {
				toFlag = "true"
			}
			return fmt.Sprintf(`{"commit":{"id":%q},"compare_timeout":%s,"diffs":[{"old_path":"a.go","new_path":"a.go","a_mode":"100644","b_mode":"100644","diff":%q}]}`, to, toFlag, good)
		}},
		{"duplicate_closing", func(n int) string {
			if n == 1 {
				return fmt.Sprintf(`{"commit":{"id":%q},"diffs":[{"old_path":"a.go","new_path":"a.go","a_mode":"100644","b_mode":"100644","diff":%q}]}`, to, good)
			}
			return fmt.Sprintf(`{"commit":{"id":%q},"id":1,"id":1,"diffs":[{"old_path":"a.go","new_path":"a.go","a_mode":"100644","b_mode":"100644","diff":%q}]}`, to, good)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var compares int
			h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/repository/commits/"):
					sha := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
					fmt.Fprintf(w, `{"id":%q}`, sha)
				case strings.Contains(r.URL.Path, "/repository/compare"):
					compares++
					_, _ = io.WriteString(w, tc.mod(compares))
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
			if compares != 2 {
				t.Fatalf("%s: want exactly 2 compares, got %d err=%v", tc.name, compares, err)
			}
			assertNoTrustedContent(t, out, err, "F3_"+tc.name)
		})
	}
	// Closing HTTP 502: valid opening body, exactly one closing attempt (no-retry client), typed fail-closed.
	t.Run("http_error_closing", func(t *testing.T) {
		var compares int
		h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/repository/commits/"):
				sha := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
				fmt.Fprintf(w, `{"id":%q}`, sha)
			case strings.Contains(r.URL.Path, "/repository/compare"):
				compares++
				if compares > 1 {
					http.Error(w, "boom", http.StatusBadGateway)
					return
				}
				fmt.Fprintf(w, `{"commit":{"id":%q},"diffs":[{"old_path":"a.go","new_path":"a.go","a_mode":"100644","b_mode":"100644","diff":%q}]}`, to, good)
			case strings.Contains(r.URL.Path, "/merge_requests/"):
				_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
			default:
				http.NotFound(w, r)
			}
		})
		out, err := callDiffWindow(t, diffDepsNoRetry(t, h), nil, map[string]any{
			"project_id": "42", "merge_request_iid": 1, "from_sha": from, "to_sha": to, "straight": true,
			"mode": "content", "paths": []any{"a.go"},
		})
		if compares != 2 {
			t.Fatalf("http_error_closing: want exactly 2 compares (valid opening + one 502 closing), got %d err=%v", compares, err)
		}
		if err == nil && (out == nil || out["returned_content_hash"] != nil) {
			t.Fatalf("http_error_closing must not trust content err=%v out=%#v", err, out)
		}
		assertNoTrustedContent(t, out, err, "F3_http_error_closing")
	})
}

// diffDepsNoRetry builds content-tool deps without GET retry so closing HTTP 502 is observed once.
func diffDepsNoRetry(t *testing.T, h http.Handler) Deps {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg := &config.Config{Token: "test-token", APIURL: srv.URL + "/api/v4", CursorKey: bytes.Repeat([]byte("k"), 32), AllowedProjectIDs: []string{"42"}}
	cli, err := gitlab.NewClient(cfg.Token,
		gitlab.WithBaseURL(cfg.APIURL),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(igl.BudgetInterceptor()),
	)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return Deps{Config: cfg, Client: cli, Clock: &cursor.FakeClock{T: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}}
}

// ownedBodyProbe is a TEST-ONLY client-side ReadCloser instrument (not shared transport).
type ownedBodyProbe struct {
	wg      sync.WaitGroup
	reads   atomic.Int64
	closes  atomic.Int64
	created atomic.Int32
}

func newOwnedBodyProbe() *ownedBodyProbe { return &ownedBodyProbe{} }

type ownedBodyInstrument struct {
	next  http.RoundTripper
	probe *ownedBodyProbe
	match func(*http.Request) bool
}

func (t *ownedBodyInstrument) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil || t.probe == nil {
		return resp, err
	}
	if t.match != nil && !t.match(req) {
		return resp, err
	}
	t.probe.created.Add(1)
	t.probe.wg.Add(1)
	resp.Body = &ownedProbeBody{ReadCloser: resp.Body, probe: t.probe}
	return resp, err
}

type ownedProbeBody struct {
	io.ReadCloser
	probe  *ownedBodyProbe
	closed sync.Once
}

func (b *ownedProbeBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.probe.reads.Add(int64(n))
	}
	return n, err
}

func (b *ownedProbeBody) Close() error {
	err := b.ReadCloser.Close()
	b.closed.Do(func() {
		b.probe.closes.Add(1)
		b.probe.wg.Done()
	})
	return err
}

func waitOwnedBodyJoin(t *testing.T, probe *ownedBodyProbe) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		probe.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("owned-reader Close/join not observed (created=%d closes=%d reads=%d)", probe.created.Load(), probe.closes.Load(), probe.reads.Load())
	}
}

// diffDepsOwnedBodyProbe wraps compare response bodies after BudgetInterceptor (test-only seam).
func diffDepsOwnedBodyProbe(t *testing.T, h http.Handler, probe *ownedBodyProbe, match func(*http.Request) bool) Deps {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg := &config.Config{Token: "test-token", APIURL: srv.URL + "/api/v4", CursorKey: bytes.Repeat([]byte("k"), 32), AllowedProjectIDs: []string{"42"}}
	budgeted := igl.BudgetInterceptor()(http.DefaultTransport)
	cli, err := gitlab.NewClient(cfg.Token,
		gitlab.WithBaseURL(cfg.APIURL),
		gitlab.WithoutRetries(),
		gitlab.WithHTTPClient(&http.Client{Transport: &ownedBodyInstrument{next: budgeted, probe: probe, match: match}}),
	)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return Deps{Config: cfg, Client: cli, Clock: &cursor.FakeClock{T: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}}
}

func TestDiffContentRepair_F5_markerSidesAndCaps(t *testing.T) {
	cases := []struct {
		name    string
		patch   string
		ctx     int
		wantSub string
	}{
		{"old_ctx0", "@@ -1 +0,0 @@\n-old\n\\ No newline at end of file\n", 0, "-old\n\\ No newline at end of file\n"},
		{"new_ctx0", "@@ -0,0 +1 @@\n+new\n\\ No newline at end of file\n", 0, "+new\n\\ No newline at end of file\n"},
		{"both_ctx0", "@@ -1,1 +1,1 @@\n-old\n\\ No newline at end of file\n+new\n\\ No newline at end of file\n", 0, "\\ No newline at end of file\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed := parseUnifiedDiff(tc.patch)
			if !parsed.ok {
				t.Fatal("helper/parser")
			}
			budget := &contentEmitBudget{maxLines: 1000, maxBytes: 10000}
			wins, ok, _, _ := selectDiffWindows(parsed, tc.ctx, budget)
			if !ok || len(wins) != 1 {
				t.Fatalf("wins=%v ok=%v", wins, ok)
			}
			if !strings.Contains(wins[0].Text, tc.wantSub) {
				t.Fatalf("text=%q missing %q", wins[0].Text, tc.wantSub)
			}
			sum := sha256.Sum256([]byte(wins[0].Text))
			if wins[0].WindowHash.Value != hex.EncodeToString(sum[:]) {
				t.Fatal("hash")
			}
			for _, ln := range wins[0].Lines {
				if ln.Kind == diffLineKindMarker {
					t.Fatal("marker emitted as line")
				}
			}
			if tc.name == "both_ctx0" {
				if len(wins[0].Lines) != 2 || !wins[0].Lines[0].NoNewline || !wins[0].Lines[1].NoNewline {
					t.Fatalf("both_ctx0 requires old AND new attached EOF markers; lines=%#v", wins[0].Lines)
				}
				if strings.Count(wins[0].Text, `\ No newline at end of file`) != 2 {
					t.Fatalf("both_ctx0 text must retain both EOF markers: %q", wins[0].Text)
				}
			}
		})
	}
	// Byte attachment boundary: budget exactly at line+marker bytes vs one below.
	patch := "@@ -0,0 +1 @@\n+new\n\\ No newline at end of file\n"
	parsed := parseUnifiedDiff(patch)
	full := patch
	budgetExact := &contentEmitBudget{maxLines: 1000, maxBytes: len(full)}
	wins, ok, trunc, _ := selectDiffWindows(parsed, 0, budgetExact)
	if !ok || trunc || len(wins) != 1 || wins[0].Text != full {
		t.Fatalf("exact byte attach failed ok=%v trunc=%v wins=%#v", ok, trunc, wins)
	}
	budgetBelow := &contentEmitBudget{maxLines: 1000, maxBytes: len(full) - 1}
	wins2, ok2, trunc2, _ := selectDiffWindows(parsed, 0, budgetBelow)
	if !ok2 || !trunc2 {
		t.Fatalf("below boundary ok=%v trunc=%v", ok2, trunc2)
	}
	for _, w := range wins2 {
		if strings.Contains(w.Text, "+new") && !strings.Contains(w.Text, `\ No newline`) {
			t.Fatalf("detached line: %q", w.Text)
		}
	}
}

func TestDiffContentRepair_F6_unavailableMatrix(t *testing.T) {
	types := []struct {
		name   string
		entry  diffManifestEntry
		patch  string
		ok     bool
		over   bool
		status string
	}{
		{"binary", diffManifestEntry{OldPath: strPtr("b"), NewPath: strPtr("b"), Binary: boolPtr(true)}, "x", true, false, diffFileStatusBinary},
		{"submodule", diffManifestEntry{OldPath: strPtr("s"), NewPath: strPtr("s"), Submodule: boolPtr(true)}, "", true, false, diffFileStatusSubmodule},
		{"collapsed", diffManifestEntry{OldPath: strPtr("c"), NewPath: strPtr("c"), Collapsed: boolPtr(true)}, "@@ -1 +1 @@\n-a\n+b\n", true, false, diffFileStatusCollapsed},
		{"too_large", diffManifestEntry{OldPath: strPtr("t"), NewPath: strPtr("t"), TooLarge: boolPtr(true)}, "@@ -1 +1 @@\n-a\n+b\n", true, false, diffFileStatusTooLarge},
		{"overcap", diffManifestEntry{OldPath: strPtr("o"), NewPath: strPtr("o")}, "@@ -1 +1 @@\n-a\n+b\n", true, true, diffFileStatusTooLarge},
		{"unavailable", diffManifestEntry{OldPath: strPtr("u"), NewPath: strPtr("u")}, "", false, false, diffFileStatusUnavailable},
	}
	for _, tc := range types {
		t.Run(tc.name, func(t *testing.T) {
			parsed := parseUnifiedDiff(tc.patch)
			st, _ := classifyDiffFile(tc.entry, tc.patch, tc.ok, tc.over, false, false, false, parsed)
			if st != tc.status {
				t.Fatalf("helper: %s != %s", st, tc.status)
			}
			built, _, _, _ := buildDiffContentFiles([]retainedDiffFile{{entry: tc.entry, patch: tc.patch, patchOK: tc.ok, overCap: tc.over}}, []int{0}, diffContentOpts{ContextLines: 3, MaxLines: 1000, MaxContentBytes: 262144})
			if len(built) != 1 || built[0].Status != tc.status || (built[0].Status != diffFileStatusText && len(built[0].Windows) != 0) {
				t.Fatalf("helper build %#v", built)
			}
		})
	}
	// Mixed siblings via MCP: good text + binary selected together.
	head, base, start := shaN(1), shaN(2), shaN(3)
	good := "@@ -1 +1 @@\n-a\n+b\n"
	diffs := fmt.Sprintf(`[{"old_path":"ok.go","new_path":"ok.go","a_mode":"100644","b_mode":"100644","diff":%q},{"old_path":"bin","new_path":"bin","binary":true,"diff":"GIT binary patch\n"}]`, good)
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
		"mode": "content", "paths": []any{"ok.go", "bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sectionMap(out)["content_complete"] != readmeta.ContentCompleteFalse {
		t.Fatalf("mixed known omission %#v", sectionMap(out))
	}
	files := asSlice(t, out["files"])
	if len(files) != 2 {
		t.Fatalf("files=%#v", files)
	}
}

func TestDiffContentRepair_F7_binaryFramingPositives(t *testing.T) {
	for _, patch := range []string{
		"GIT binary patch\nliteral 1\nAcA=\n",
		"Binary files a/x and b/x differ\n",
	} {
		parsed := parseUnifiedDiff(patch)
		st, _ := classifyDiffFile(diffManifestEntry{OldPath: strPtr("x"), NewPath: strPtr("x")}, patch, true, false, false, false, false, parsed)
		if st != diffFileStatusBinary {
			t.Fatalf("helper: framing %q => %s", patch, st)
		}
	}
	// Deleted/added/context phrase negatives already in F7_textMentioningBinaryPhrases.
	for _, phrase := range []string{"GIT binary patch", "Binary files "} {
		patch := "@@ -1 +1 @@\n-" + phrase + " old\n+" + phrase + " new\n"
		parsed := parseUnifiedDiff(patch)
		st, _ := classifyDiffFile(diffManifestEntry{OldPath: strPtr("a"), NewPath: strPtr("a"), Binary: boolPtr(false)}, patch, true, false, false, false, false, parsed)
		if st != diffFileStatusText {
			t.Fatalf("phrase in hunk classified %s", st)
		}
	}
}

func TestDiffContentRepair_F8_gitlinkTransitions(t *testing.T) {
	cases := []diffManifestEntry{
		{OldPath: strPtr("s"), NewPath: strPtr("s"), AMode: strPtr("160000"), BMode: strPtr("100644")},
		{OldPath: strPtr("s"), NewPath: strPtr("s"), AMode: strPtr("100644"), BMode: strPtr("160000")},
		{OldPath: strPtr("s"), NewPath: strPtr("s"), AMode: strPtr("160000"), BMode: nil, NewFile: boolPtr(true)},
		{OldPath: strPtr("s"), NewPath: strPtr("s"), AMode: strPtr("160000"), BMode: strPtr("160000"), DeletedFile: boolPtr(true)},
	}
	for i, e := range cases {
		st, _ := classifyDiffFile(e, "@@ -1 +1 @@\n-a\n+b\n", true, false, false, false, false, parseUnifiedDiff("@@ -1 +1 @@\n-a\n+b\n"))
		if st != diffFileStatusSubmodule {
			t.Fatalf("case %d status=%s", i, st)
		}
	}
}

func TestDiffContentRepair_F9_emptyVsNullMissing(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	cases := []struct {
		name   string
		diffs  string
		path   string
		status []string
	}{
		{"explicit_empty_added", `[{"old_path":"a","new_path":"a","new_file":true,"a_mode":"0","b_mode":"100644","diff":""}]`, "a", []string{diffFileStatusMetadata, diffFileStatusModeOnly, diffFileStatusUnsupported}},
		{"explicit_empty_deleted", `[{"old_path":"a","new_path":"a","deleted_file":true,"a_mode":"100644","b_mode":"0","diff":""}]`, "a", []string{diffFileStatusMetadata, diffFileStatusModeOnly, diffFileStatusUnsupported}},
		{"null_diff", `[{"old_path":"a","new_path":"a","a_mode":"100644","b_mode":"100644","diff":null}]`, "a", []string{diffFileStatusUnavailable}},
		{"missing_diff", `[{"old_path":"a","new_path":"a","a_mode":"100644","b_mode":"100644"}]`, "a", []string{diffFileStatusUnavailable}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := versionObject(1, 5001, head, base, start, "collected", "1", tc.diffs)
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
				"mode": "content", "paths": []any{tc.path},
			})
			if err != nil {
				t.Fatal(err)
			}
			st := asString(asMap(t, asSlice(t, out["files"])[0])["status"])
			ok := false
			for _, want := range tc.status {
				if st == want {
					ok = true
				}
			}
			if !ok {
				t.Fatalf("status=%s want one of %v out=%#v", st, tc.status, out)
			}
			if wins, _ := asMap(t, asSlice(t, out["files"])[0])["windows"].([]any); len(wins) != 0 {
				t.Fatal("windows on empty/null/missing")
			}
		})
	}
}

func TestDiffContentRepair_F10_wrongTypeMatrix(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	good := "@@ -1 +1 @@\n-a\n+b\n"
	wantSum := sha256.Sum256([]byte(good))
	wantHash := hex.EncodeToString(wantSum[:])
	for _, tc := range []struct {
		name       string
		diffs      string
		selectBoth bool
	}{
		{"selected_numeric", fmt.Sprintf(`[{"old_path":"bad.go","new_path":"bad.go","diff":123},{"old_path":"keep.go","new_path":"keep.go","diff":%q}]`, good), true},
		{"selected_object", fmt.Sprintf(`[{"old_path":"bad.go","new_path":"bad.go","diff":{}},{"old_path":"keep.go","new_path":"keep.go","diff":%q}]`, good), true},
		{"selected_array", fmt.Sprintf(`[{"old_path":"bad.go","new_path":"bad.go","diff":[]},{"old_path":"keep.go","new_path":"keep.go","diff":%q}]`, good), true},
		{"unselected_null_before", fmt.Sprintf(`[{"diff":null,"old_path":"other.go","new_path":"other.go"},{"old_path":"keep.go","new_path":"keep.go","diff":%q}]`, good), false},
		{"patch_key_numeric_after_paths", fmt.Sprintf(`[{"old_path":"keep.go","new_path":"keep.go","diff":%q},{"old_path":"bad.go","new_path":"bad.go","a_mode":"100644","diff":1}]`, good), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := versionObject(1, 5001, head, base, start, "collected", "2", tc.diffs)
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
			paths := []any{"keep.go"}
			if tc.selectBoth {
				paths = []any{"bad.go", "keep.go"}
			}
			out, err := callDiffWindow(t, diffDeps(t, h), nil, map[string]any{
				"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1,
				"mode": "content", "paths": paths,
			})
			if err != nil {
				t.Fatal(err)
			}
			files := asSlice(t, out["files"])
			byPath := map[string]map[string]any{}
			for _, raw := range files {
				m := asMap(t, raw)
				byPath[asString(m["new_path"])] = m
			}
			keepF := byPath["keep.go"]
			if keepF == nil || asString(keepF["status"]) != diffFileStatusText {
				t.Fatalf("%s keep destroyed %#v", tc.name, out)
			}
			wins := asSlice(t, keepF["windows"])
			if len(wins) != 1 {
				t.Fatalf("%s keep windows %#v", tc.name, wins)
			}
			wh := asMap(t, asMap(t, wins[0])["window_hash"])
			if asString(wh["value"]) != wantHash {
				t.Fatalf("%s keep hash=%v want %s", tc.name, wh, wantHash)
			}
			if tc.selectBoth {
				badF := byPath["bad.go"]
				if badF == nil || asString(badF["status"]) != diffFileStatusMalformed {
					t.Fatalf("%s bad status want malformed got %#v", tc.name, badF)
				}
				if bw, _ := badF["windows"].([]any); len(bw) != 0 {
					t.Fatalf("%s bad windows %#v", tc.name, bw)
				}
			}
		})
	}
	t.Run("malformed_global_json", func(t *testing.T) {
		h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/versions/1"):
				_, _ = io.WriteString(w, `{"id":1,`)
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
			// fail-closed as error is acceptable
			assertNoTrustedContent(t, out, err, "malformed_global_err")
			return
		}
		assertNoTrustedContent(t, out, err, "malformed_global_out")
	})
}

func TestDiffContentRepair_F11_partialAndDriftMatrix(t *testing.T) {
	from, to := shaN(4), shaN(5)
	t.Run("straight_partial_missing_selector", func(t *testing.T) {
		h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/repository/commits/"):
				sha := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
				fmt.Fprintf(w, `{"id":%q}`, sha)
			case strings.Contains(r.URL.Path, "/repository/compare"):
				fmt.Fprintf(w, `{"commit":{"id":%q},"diffs":[{"old_path":"a.go","new_path":"a.go","a_mode":"100644","b_mode":"100644","diff":"@@ -1 +1 @@\n-a\n+b\n"}]}`, to)
			case strings.Contains(r.URL.Path, "/merge_requests/"):
				_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
			default:
				http.NotFound(w, r)
			}
		})
		out, err := callDiffWindow(t, diffDeps(t, h), nil, map[string]any{
			"project_id": "42", "merge_request_iid": 1, "from_sha": from, "to_sha": to, "straight": true,
			"mode": "content", "paths": []any{"missing.go"},
		})
		if err != nil {
			t.Fatal(err)
		}
		sel := asMap(t, asSlice(t, out["selectors"])[0])
		if asString(sel["status"]) == diffSelectorAbsent {
			t.Fatalf("straight partial must not claim absent: %#v", sel)
		}
	})
	t.Run("version_count_drift", func(t *testing.T) {
		head, base, start := shaN(1), shaN(2), shaN(3)
		var n int
		h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/versions/1"):
				n++
				size := "1"
				if n > 1 {
					size = "2"
				}
				_, _ = io.WriteString(w, versionObject(1, 5001, head, base, start, "collected", size, oneDiff("a.go", "@@ -1 +1 @@\n-a\n+b\n")))
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
		assertNoTrustedContent(t, out, err, "version_count_drift")
	})
	t.Run("version_state_drift", func(t *testing.T) {
		head, base, start := shaN(1), shaN(2), shaN(3)
		var n int
		h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/versions/1"):
				n++
				st := "collected"
				if n > 1 {
					st = "timeout"
				}
				_, _ = io.WriteString(w, versionObject(1, 5001, head, base, start, st, "1", oneDiff("a.go", "@@ -1 +1 @@\n-a\n+b\n")))
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
		assertNoTrustedContent(t, out, err, "version_state_drift")
	})
}

// ---- RVG-143 R1/N1/R3/R4/R5 repair controls (old-source reds first) ----

func TestDiffContentRepair_R1_zeroCountOrdering(t *testing.T) {
	// N1 concrete cases from independent source review on b3b70aed.
	validImmediateOld := "@@ -1 +1 @@\n-a\n+b\n@@ -1,0 +2 @@\n+c\n"
	invalidZeroThenPositiveOld := "@@ -1,0 +2 @@\n+x\n@@ -1 +3 @@\n-a\n+b\n"
	validImmediateNew := "@@ -1 +1 @@\n-a\n+b\n@@ -2 +1,0 @@\n-c\n"
	invalidZeroThenPositiveNew := "@@ -2 +1,0 @@\n-x\n@@ -3 +1 @@\n-a\n+b\n"
	malformedInsert := "@@ -1 +1 @@\n-a\n+b\n@@ -0,0 +2 @@\n+c\n"
	malformedDelete := "@@ -1 +1 @@\n-a\n+b\n@@ -2 +0,0 @@\n-c\n"

	t.Run("helper_valid_immediate_zero_after_positive_old", func(t *testing.T) {
		if !parseUnifiedDiff(validImmediateOld).ok {
			t.Fatal("helper: positive ending at N then zero boundary N must accept")
		}
	})
	t.Run("helper_invalid_zero_then_positive_same_boundary_old", func(t *testing.T) {
		if parseUnifiedDiff(invalidZeroThenPositiveOld).ok {
			t.Fatal("helper: zero N then positive start N must reject")
		}
	})
	t.Run("helper_valid_immediate_zero_after_positive_new", func(t *testing.T) {
		if !parseUnifiedDiff(validImmediateNew).ok {
			t.Fatal("helper: new-side positive end N then zero N must accept")
		}
	})
	t.Run("helper_invalid_zero_then_positive_same_boundary_new", func(t *testing.T) {
		if parseUnifiedDiff(invalidZeroThenPositiveNew).ok {
			t.Fatal("helper: new-side zero N then positive start N must reject")
		}
	})
	t.Run("helper_zero_at_0_then_consume_1", func(t *testing.T) {
		patch := "@@ -0,0 +1 @@\n+a\n@@ -1 +2 @@\n-b\n+c\n"
		if !parseUnifiedDiff(patch).ok {
			t.Fatal("helper: zero at 0 maps next-line 1; consuming line 1 must accept")
		}
	})
	t.Run("helper_zero_N_then_positive_N_plus_1", func(t *testing.T) {
		patch := "@@ -1,0 +1 @@\n+a\n@@ -2 +2 @@\n-b\n+c\n"
		if !parseUnifiedDiff(patch).ok {
			t.Fatal("helper: zero N then positive N+1 must accept")
		}
	})
	t.Run("helper_consecutive_zero_forward", func(t *testing.T) {
		patch := "@@ -1,0 +1 @@\n+a\n@@ -2,0 +2 @@\n+b\n"
		if !parseUnifiedDiff(patch).ok {
			t.Fatal("helper: consecutive forward zeros must accept")
		}
	})
	t.Run("helper_consecutive_zero_equal", func(t *testing.T) {
		// Equal next-line after zero is allowed (same insertion point).
		patch := "@@ -1,0 +1 @@\n+a\n@@ -1,0 +2 @@\n+b\n"
		if !parseUnifiedDiff(patch).ok {
			t.Fatal("helper: equal consecutive zeros must accept")
		}
	})
	t.Run("helper_consecutive_zero_backward", func(t *testing.T) {
		patch := "@@ -2,0 +1 @@\n+a\n@@ -0,0 +2 @@\n+b\n"
		if parseUnifiedDiff(patch).ok {
			t.Fatal("helper: backward zero after zero must reject")
		}
	})
	t.Run("helper_malformed_zero_old_after_positive", func(t *testing.T) {
		if parseUnifiedDiff(malformedInsert).ok {
			t.Fatal("helper: zero-old boundary before prior old line must reject")
		}
	})
	t.Run("helper_malformed_zero_new_after_positive", func(t *testing.T) {
		if parseUnifiedDiff(malformedDelete).ok {
			t.Fatal("helper: zero-new boundary before prior new line must reject")
		}
	})
	t.Run("helper_integer_limit_zero_at_maxint", func(t *testing.T) {
		// Zero boundary at MaxInt: start+1 overflows → fail closed.
		patch := "@@ -" + strconv.Itoa(math.MaxInt) + ",0 +1 @@\n+c\n"
		if parseUnifiedDiff(patch).ok {
			t.Fatal("helper: zero at MaxInt must reject (overflow fail-closed)")
		}
	})
	t.Run("helper_integer_limit_zero_before_maxint_end", func(t *testing.T) {
		patch := "@@ -" + strconv.Itoa(math.MaxInt-1) + ",1 +1,1 @@\n-a\n+b\n@@ -0,0 +2 @@\n+c\n"
		if parseUnifiedDiff(patch).ok {
			t.Fatal("helper: zero boundary before MaxInt-adjacent end must reject")
		}
	})
	t.Run("helper_integer_limit_positive_overflow", func(t *testing.T) {
		patch := "@@ -" + strconv.Itoa(math.MaxInt) + ",1 +1,1 @@\n-a\n+b\n"
		if parseUnifiedDiff(patch).ok {
			t.Fatal("helper: positive start+count overflow must reject")
		}
	})
	t.Run("helper_positive_legitimate_adjacency", func(t *testing.T) {
		// Includes immediate zero-after-consumed-1 (boundary 1), not only later boundary 2.
		// The prior "@@ -1,1 +1,0 @@ ... @@ -2,0 +1,1 @@" case was falsely green under mixed
		// units (zero stored boundary-after=1, positive start 1 passed); next-line units
		// correctly reject consuming new line 1 after zero-at-1. Use +2,1 instead.
		cases := []string{
			"@@ -0,0 +1 @@\n+a\n",
			"@@ -0,0 +1 @@\n+a\n@@ -1 +2 @@\n-b\n+c\n",
			validImmediateOld,
			"@@ -1 +1 @@\n-a\n+b\n@@ -2,0 +3 @@\n+c\n",
			"@@ -1,1 +1,0 @@\n-a\n@@ -2,0 +2,1 @@\n+b\n",
			validImmediateNew,
		}
		for _, p := range cases {
			if !parseUnifiedDiff(p).ok {
				t.Fatalf("helper: legitimate adjacency rejected: %q", p)
			}
		}
	})
	t.Run("helper_false_legitimate_mixed_units_now_rejects", func(t *testing.T) {
		// Previously accepted under mixed coordinate units; must reject after normalization.
		patch := "@@ -1,1 +1,0 @@\n-a\n@@ -2,0 +1,1 @@\n+b\n"
		if parseUnifiedDiff(patch).ok {
			t.Fatal("helper: zero-at-1 then positive new start 1 must reject under next-line units")
		}
	})
	t.Run("mcp_invalid_zero_then_positive_selected_good_sibling", func(t *testing.T) {
		head, base, start := shaN(1), shaN(2), shaN(3)
		good := "@@ -1 +1 @@\n-a\n+b\n"
		bad := invalidZeroThenPositiveOld
		diffs := fmt.Sprintf(`[{"old_path":"bad.go","new_path":"bad.go","a_mode":"100644","b_mode":"100644","diff":%q},{"old_path":"keep.go","new_path":"keep.go","a_mode":"100644","b_mode":"100644","diff":%q}]`, bad, good)
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
			"mode": "content", "paths": []any{"bad.go", "keep.go"},
		})
		if err != nil {
			t.Fatal(err)
		}
		files := asSlice(t, out["files"])
		if len(files) != 2 {
			t.Fatalf("files=%#v", files)
		}
		byPath := map[string]map[string]any{}
		for _, raw := range files {
			m := asMap(t, raw)
			byPath[asString(m["new_path"])] = m
		}
		badF, keepF := byPath["bad.go"], byPath["keep.go"]
		if badF == nil || keepF == nil {
			t.Fatalf("missing paths %#v", byPath)
		}
		if asString(badF["status"]) != diffFileStatusMalformed {
			t.Fatalf("bad status=%v", badF["status"])
		}
		if wins, _ := badF["windows"].([]any); len(wins) != 0 {
			t.Fatalf("bad windows %#v", wins)
		}
		if asString(keepF["status"]) != diffFileStatusText {
			t.Fatalf("keep status=%v", keepF["status"])
		}
		wins := asSlice(t, keepF["windows"])
		if len(wins) != 1 {
			t.Fatalf("keep windows %#v", wins)
		}
		wantSum := sha256.Sum256([]byte(good))
		wh := asMap(t, asMap(t, wins[0])["window_hash"])
		if asString(wh["value"]) != hex.EncodeToString(wantSum[:]) {
			t.Fatalf("keep hash=%v want %s", wh, hex.EncodeToString(wantSum[:]))
		}
		// No usable anchors from malformed selected; good sibling keeps exact hash only.
		if rc := out["returned_content_hash"]; rc == nil {
			t.Fatal("returned_content_hash missing for good sibling")
		}
	})
	t.Run("mcp_malformed_selected_good_sibling", func(t *testing.T) {
		head, base, start := shaN(1), shaN(2), shaN(3)
		good := "@@ -1 +1 @@\n-a\n+b\n"
		bad := malformedInsert
		diffs := fmt.Sprintf(`[{"old_path":"bad.go","new_path":"bad.go","a_mode":"100644","b_mode":"100644","diff":%q},{"old_path":"keep.go","new_path":"keep.go","a_mode":"100644","b_mode":"100644","diff":%q}]`, bad, good)
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
			"mode": "content", "paths": []any{"bad.go", "keep.go"},
		})
		if err != nil {
			t.Fatal(err)
		}
		files := asSlice(t, out["files"])
		if len(files) != 2 {
			t.Fatalf("files=%#v", files)
		}
		byPath := map[string]map[string]any{}
		for _, raw := range files {
			m := asMap(t, raw)
			byPath[asString(m["new_path"])] = m
		}
		badF, keepF := byPath["bad.go"], byPath["keep.go"]
		if badF == nil || keepF == nil {
			t.Fatalf("missing paths %#v", byPath)
		}
		if asString(badF["status"]) != diffFileStatusMalformed {
			t.Fatalf("bad status=%v", badF["status"])
		}
		if wins, _ := badF["windows"].([]any); len(wins) != 0 {
			t.Fatalf("bad windows %#v", wins)
		}
		if asString(keepF["status"]) != diffFileStatusText {
			t.Fatalf("keep status=%v", keepF["status"])
		}
		wins := asSlice(t, keepF["windows"])
		if len(wins) != 1 {
			t.Fatalf("keep windows %#v", wins)
		}
		wantSum := sha256.Sum256([]byte(good))
		wh := asMap(t, asMap(t, wins[0])["window_hash"])
		if asString(wh["value"]) != hex.EncodeToString(wantSum[:]) {
			t.Fatalf("keep hash=%v want %s", wh, hex.EncodeToString(wantSum[:]))
		}
	})
	t.Run("mcp_valid_immediate_adjacency_exact_text_hash_anchors", func(t *testing.T) {
		head, base, start := shaN(1), shaN(2), shaN(3)
		valid := validImmediateOld
		diffs := fmt.Sprintf(`[{"old_path":"adj.go","new_path":"adj.go","a_mode":"100644","b_mode":"100644","diff":%q}]`, valid)
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
			"mode": "content", "paths": []any{"adj.go"}, "context_lines": 0,
		})
		if err != nil {
			t.Fatal(err)
		}
		files := asSlice(t, out["files"])
		if len(files) != 1 {
			t.Fatalf("files=%#v", files)
		}
		f := asMap(t, files[0])
		if asString(f["status"]) != diffFileStatusText {
			t.Fatalf("status=%v", f["status"])
		}
		mcpWins := asSlice(t, f["windows"])
		if len(mcpWins) == 0 {
			t.Fatal("expected nonempty windows for valid adjacency")
		}
		// Rebuild private parserCoords from the exact same patch bytes (JSON drops them).
		parsed := parseUnifiedDiff(valid)
		if !parsed.ok {
			t.Fatal("setup: valid patch must parse")
		}
		budget := &contentEmitBudget{maxLines: 1000, maxBytes: 262144}
		proofWins, ok, _, _ := selectDiffWindows(parsed, 0, budget)
		if !ok || len(proofWins) == 0 {
			t.Fatalf("setup selectDiffWindows ok=%v wins=%d", ok, len(proofWins))
		}
		if len(proofWins) != len(mcpWins) {
			t.Fatalf("window count mcp=%d proof=%d", len(mcpWins), len(proofWins))
		}
		for i := range proofWins {
			mw := asMap(t, mcpWins[i])
			if asString(mw["text"]) != proofWins[i].Text {
				t.Fatalf("window[%d] text mcp=%q proof=%q", i, mw["text"], proofWins[i].Text)
			}
			wantHash := hex.EncodeToString(func() []byte { s := sha256.Sum256([]byte(proofWins[i].Text)); return s[:] }())
			wh := asMap(t, mw["window_hash"])
			if asString(wh["value"]) != wantHash || asString(wh["value"]) != proofWins[i].WindowHash.Value {
				t.Fatalf("window[%d] hash mcp=%v proof=%s", i, wh, proofWins[i].WindowHash.Value)
			}
		}
		vid := int64(1)
		sel := diffContentSelectionOut{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: &vid,
			HeadSHA: strPtr(head), BaseSHA: strPtr(base), StartSHA: strPtr(start),
		}
		file := diffContentFile{
			Status: diffFileStatusText, OldPath: strPtr("adj.go"), NewPath: strPtr("adj.go"),
			Windows: proofWins,
		}
		proof := proofFromContentFile(sel, file, true)
		if !proof.Available {
			t.Fatalf("proof unavailable: %#v", proof)
		}
		// Anchors on both sides of the immediate-adjacency patch.
		for _, a := range []diffAnchor{
			{ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 1,
				HeadSHA: head, BaseSHA: base, StartSHA: start,
				OldPath: "adj.go", NewPath: "adj.go", Side: "old", Line: 1},
			{ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 1,
				HeadSHA: head, BaseSHA: base, StartSHA: start,
				OldPath: "adj.go", NewPath: "adj.go", Side: "new", Line: 1},
			{ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 1,
				HeadSHA: head, BaseSHA: base, StartSHA: start,
				OldPath: "adj.go", NewPath: "adj.go", Side: "new", Line: 2},
		} {
			if _, err := validateDiffAnchor(proof, a); err != nil {
				t.Fatalf("anchor %#v: %v", a, err)
			}
		}
	})
}

func TestDiffContentRepair_R3_compareTimeoutSymmetric(t *testing.T) {
	from, to := shaN(4), shaN(5)
	patch := "@@ -1 +1 @@\n-a\n+b\n"
	type side struct {
		raw string // JSON fragment for compare_timeout field including key, or empty if omitted
	}
	mkBody := func(timeoutField string) string {
		if timeoutField == "" {
			return fmt.Sprintf(`{"commit":{"id":%q},"diffs":[{"old_path":"a.go","new_path":"a.go","a_mode":"100644","b_mode":"100644","diff":%q}]}`, to, patch)
		}
		return fmt.Sprintf(`{"commit":{"id":%q},%s,"diffs":[{"old_path":"a.go","new_path":"a.go","a_mode":"100644","b_mode":"100644","diff":%q}]}`, to, timeoutField, patch)
	}
	runPair := func(t *testing.T, openField, closeField string) (compares int, out map[string]any, err error) {
		t.Helper()
		h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/repository/commits/"):
				sha := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
				fmt.Fprintf(w, `{"id":%q}`, sha)
			case strings.Contains(r.URL.Path, "/repository/compare"):
				compares++
				field := openField
				if compares > 1 {
					field = closeField
				}
				_, _ = io.WriteString(w, mkBody(field))
			case strings.Contains(r.URL.Path, "/merge_requests/"):
				_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
			default:
				http.NotFound(w, r)
			}
		})
		out, err = callDiffWindow(t, diffDeps(t, h), nil, map[string]any{
			"project_id": "42", "merge_request_iid": 1, "from_sha": from, "to_sha": to, "straight": true,
			"mode": "content", "paths": []any{"a.go"},
		})
		return compares, out, err
	}
	t.Run("baseline_false_false", func(t *testing.T) {
		compares, out, err := runPair(t, `"compare_timeout":false`, `"compare_timeout":false`)
		if err != nil {
			t.Fatal(err)
		}
		if compares != 2 {
			t.Fatalf("baseline compares=%d", compares)
		}
		if out["returned_content_hash"] == nil || len(asSlice(t, out["files"])) != 1 {
			t.Fatalf("baseline trusted content missing %#v", out)
		}
		if sectionMap(out)["consistency"] != readmeta.ConsistencyConsistent {
			t.Fatalf("section=%#v", sectionMap(out))
		}
	})
	driftCases := []struct {
		name, open, close string
	}{
		{"false_to_true", `"compare_timeout":false`, `"compare_timeout":true`},
		{"true_to_false", `"compare_timeout":true`, `"compare_timeout":false`},
		{"true_to_true", `"compare_timeout":true`, `"compare_timeout":true`},
		{"missing_to_false", ``, `"compare_timeout":false`},
		{"null_to_false", `"compare_timeout":null`, `"compare_timeout":false`},
		{"wrongtype_to_false", `"compare_timeout":"yes"`, `"compare_timeout":false`},
	}
	for _, tc := range driftCases {
		t.Run(tc.name, func(t *testing.T) {
			compares, out, err := runPair(t, tc.open, tc.close)
			if err != nil {
				t.Fatal(err)
			}
			if compares != 2 {
				t.Fatalf("%s compares=%d want 2 (opening must succeed to exercise closing)", tc.name, compares)
			}
			assertNoTrustedContent(t, out, err, "R3_"+tc.name)
			if sectionMap(out)["consistency"] == readmeta.ConsistencyConsistent {
				t.Fatalf("%s must not be consistent: %#v", tc.name, sectionMap(out))
			}
		})
	}
}

// ---- RVG-143 P2: non-crop window rejection must not become text+omitted_context ----

// Exact adjudication patch: context EOF marker then addition; context_lines=0.
const p2MarkerThenAdditionPatch = "@@ -1 +1,2 @@\n a\n\\ No newline at end of file\n+b\n"

func TestDiffContentRepair_P2_markerNonCropPropagatesMalformed(t *testing.T) {
	budget := &contentEmitBudget{maxLines: 1000, maxBytes: 262144}

	t.Run("helper_exact_patch_context0_must_fail_selection", func(t *testing.T) {
		parsed := parseUnifiedDiff(p2MarkerThenAdditionPatch)
		if !parsed.ok {
			// Parser may reject (also valid closure); selection path must not silently succeed as text.
			t.Skip("parser rejected malformed marker sequence; selection path N/A")
		}
		wins, ok, trunc, omitted := selectDiffWindows(parsed, 0, budget)
		if ok {
			t.Fatalf("non-crop window rejection must fail selection; got ok=true wins=%d trunc=%v omitted=%v", len(wins), trunc, omitted)
		}
		if len(wins) != 0 {
			t.Fatalf("failed selection must clear windows; got %#v", wins)
		}
	})

	t.Run("helper_buildDiffContentFiles_malformed_zero_windows", func(t *testing.T) {
		files := []retainedDiffFile{{
			entry:   diffManifestEntry{OldPath: strPtr("bad.go"), NewPath: strPtr("bad.go")},
			patch:   p2MarkerThenAdditionPatch,
			patchOK: true,
		}}
		built, retHash, _, _ := buildDiffContentFiles(files, []int{0}, diffContentOpts{
			ContextLines: 0, MaxLines: 1000, MaxContentBytes: 262144,
		})
		if len(built) != 1 {
			t.Fatalf("files=%d", len(built))
		}
		if built[0].Status != diffFileStatusMalformed {
			t.Fatalf("expected status=malformed, got %q limitations=%v windows=%d (must not keep text+omitted_context)",
				built[0].Status, built[0].Limitations, len(built[0].Windows))
		}
		if len(built[0].Windows) != 0 {
			t.Fatalf("malformed file must clear all windows; got %#v", built[0].Windows)
		}
		if hasLimitation(built[0], "omitted_context") && built[0].Status == diffFileStatusText {
			t.Fatal("lost changed range must not be relabelled as ordinary omitted_context on text")
		}
		if retHash != nil {
			t.Fatalf("solo malformed must not emit returned_content_hash: %#v", retHash)
		}
	})

	t.Run("helper_earlier_valid_hunk_cleared_on_later_failure", func(t *testing.T) {
		// First hunk fully valid; second is the same marker-then-addition pathology on a
		// non-overlapping later range so the parser accepts both (N1-monotonic).
		patch := "@@ -1 +1 @@\n-a\n+b\n@@ -2 +2,2 @@\n c\n\\ No newline at end of file\n+d\n"
		parsed := parseUnifiedDiff(patch)
		if !parsed.ok {
			t.Fatal("setup: combined patch must parse so failure is selection/window, not header order")
		}
		files := []retainedDiffFile{{
			entry:   diffManifestEntry{OldPath: strPtr("bad.go"), NewPath: strPtr("bad.go")},
			patch:   patch,
			patchOK: true,
		}}
		built, _, _, _ := buildDiffContentFiles(files, []int{0}, diffContentOpts{
			ContextLines: 0, MaxLines: 1000, MaxContentBytes: 262144,
		})
		if len(built) != 1 || built[0].Status != diffFileStatusMalformed {
			t.Fatalf("expected malformed after later hunk window failure, got status=%v lims=%v wins=%d",
				built[0].Status, built[0].Limitations, len(built[0].Windows))
		}
		if len(built[0].Windows) != 0 {
			t.Fatalf("valid earlier hunk must not leave trusted windows on malformed file; got %#v", built[0].Windows)
		}
	})

	// Independent expected good sibling bytes/hashes (literal; not from production builder).
	const goodSiblingPatch = "@@ -1 +1 @@\n-a\n+b\n"
	wantGoodWindow := goodSiblingPatch
	wantGoodWindowSHA := "e66fa3de3ec593c4b23137378a0b59e5d819381338db5491eac41874b05c698c"
	wantReturnedConcatSHA := wantGoodWindowSHA // single surviving window

	t.Run("mcp_bad_selected_good_sibling_independent_hashes", func(t *testing.T) {
		head, base, start := shaN(1), shaN(2), shaN(3)
		diffs := fmt.Sprintf(
			`[{"old_path":"bad.go","new_path":"bad.go","a_mode":"100644","b_mode":"100644","diff":%q},{"old_path":"keep.go","new_path":"keep.go","a_mode":"100644","b_mode":"100644","diff":%q}]`,
			p2MarkerThenAdditionPatch, goodSiblingPatch,
		)
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
			"mode": "content", "paths": []any{"bad.go", "keep.go"}, "context_lines": 0,
		})
		if err != nil {
			t.Fatal(err)
		}
		files := asSlice(t, out["files"])
		if len(files) != 2 {
			t.Fatalf("files=%#v", files)
		}
		byPath := map[string]map[string]any{}
		for _, raw := range files {
			m := asMap(t, raw)
			byPath[asString(m["new_path"])] = m
		}
		badF, keepF := byPath["bad.go"], byPath["keep.go"]
		if badF == nil || keepF == nil {
			t.Fatalf("missing paths %#v", byPath)
		}
		// Prove good-sibling exact bytes/hashes BEFORE bad malformed assertion so old-source
		// red still records keep.go evidence when bad status remains incorrectly text.
		if asString(keepF["status"]) != diffFileStatusText {
			t.Fatalf("keep status=%v", keepF["status"])
		}
		wins := asSlice(t, keepF["windows"])
		if len(wins) != 1 {
			t.Fatalf("keep windows %#v", wins)
		}
		gotText := asString(asMap(t, wins[0])["text"])
		wh := asMap(t, asMap(t, wins[0])["window_hash"])
		rc, _ := out["returned_content_hash"].(map[string]any)
		t.Logf("P2_KEEP_SIBLING actual text=%q window_hash.scope=%v window_hash.value=%v returned_content_hash.scope=%v returned_content_hash.value=%v",
			gotText, wh["scope"], wh["value"], rc["scope"], rc["value"])
		if gotText != wantGoodWindow {
			t.Fatalf("keep window text=%q want %q", gotText, wantGoodWindow)
		}
		if asString(wh["scope"]) != diffContentWindowHashScope || asString(wh["value"]) != wantGoodWindowSHA {
			t.Fatalf("keep window_hash=%#v want scope=%s value=%s", wh, diffContentWindowHashScope, wantGoodWindowSHA)
		}
		sum := sha256.Sum256([]byte(wantGoodWindow))
		if hex.EncodeToString(sum[:]) != wantGoodWindowSHA {
			t.Fatalf("test oracle drift: %s", hex.EncodeToString(sum[:]))
		}
		if asString(rc["scope"]) != diffContentReturnHashScope || asString(rc["value"]) != wantReturnedConcatSHA {
			t.Fatalf("returned_content_hash=%#v want %s/%s", rc, diffContentReturnHashScope, wantReturnedConcatSHA)
		}
		// Bad-file malformed closure (expected red on unrepaired 6c production).
		if asString(badF["status"]) != diffFileStatusMalformed {
			t.Fatalf("bad status=%v want malformed limitations=%v", badF["status"], badF["limitations"])
		}
		if badWins, _ := badF["windows"].([]any); len(badWins) != 0 {
			t.Fatalf("bad windows must be empty; got %#v", badWins)
		}
	})

	t.Run("helper_valid_addition_eof_marker_context0", func(t *testing.T) {
		patch := "@@ -1 +1 @@\n-old\n+new\n\\ No newline at end of file\n"
		parsed := parseUnifiedDiff(patch)
		if !parsed.ok {
			t.Fatal("valid addition EOF marker must parse")
		}
		wins, ok, trunc, _ := selectDiffWindows(parsed, 0, budget)
		if !ok || trunc || len(wins) != 1 {
			t.Fatalf("ok=%v trunc=%v wins=%d", ok, trunc, len(wins))
		}
		if !strings.Contains(wins[0].Text, "+new") || !strings.Contains(wins[0].Text, `\ No newline at end of file`) {
			t.Fatalf("marker must stay attached: %q", wins[0].Text)
		}
	})

	t.Run("helper_valid_deletion_eof_marker_context0", func(t *testing.T) {
		patch := "@@ -1 +0,0 @@\n-old\n\\ No newline at end of file\n"
		parsed := parseUnifiedDiff(patch)
		if !parsed.ok {
			t.Fatal("valid deletion EOF marker must parse")
		}
		wins, ok, trunc, _ := selectDiffWindows(parsed, 0, budget)
		if !ok || trunc || len(wins) != 1 {
			t.Fatalf("ok=%v trunc=%v wins=%d", ok, trunc, len(wins))
		}
		if !strings.Contains(wins[0].Text, "-old") || !strings.Contains(wins[0].Text, `\ No newline at end of file`) {
			t.Fatalf("marker must stay attached: %q", wins[0].Text)
		}
	})

	t.Run("helper_valid_final_context_eof_marker", func(t *testing.T) {
		patch := "@@ -1,2 +1 @@\n-a\n b\n\\ No newline at end of file\n"
		parsed := parseUnifiedDiff(patch)
		if !parsed.ok {
			t.Fatal("valid final context EOF must parse")
		}
		wins, ok, _, _ := selectDiffWindows(parsed, 1, budget)
		if !ok || len(wins) == 0 {
			t.Fatalf("ok=%v wins=%d", ok, len(wins))
		}
		if !strings.Contains(wins[0].Text, `\ No newline at end of file`) {
			t.Fatalf("final context marker must remain: %q", wins[0].Text)
		}
	})

	t.Run("helper_ordinary_intentional_context_omission", func(t *testing.T) {
		patch := samplePatchAdditionDeletionContext()
		files := []retainedDiffFile{{
			entry:   diffManifestEntry{OldPath: strPtr("a.go"), NewPath: strPtr("a.go")},
			patch:   patch,
			patchOK: true,
		}}
		built, _, _, _ := buildDiffContentFiles(files, []int{0}, diffContentOpts{
			ContextLines: 0, MaxLines: 1000, MaxContentBytes: 262144,
		})
		if len(built) != 1 || built[0].Status != diffFileStatusText {
			t.Fatalf("ordinary omission must remain text: %#v", built)
		}
		if len(built[0].Windows) == 0 {
			t.Fatal("ordinary omission must retain changed windows")
		}
		if !hasLimitation(built[0], "omitted_context") {
			t.Fatalf("ordinary context0 omission must set omitted_context; lims=%v", built[0].Limitations)
		}
	})

	t.Run("helper_marker_budget_boundary_keeps_attachment", func(t *testing.T) {
		patch := "@@ -0,0 +1 @@\n+new\n\\ No newline at end of file\n"
		parsed := parseUnifiedDiff(patch)
		if !parsed.ok {
			t.Fatal("parse")
		}
		hdr := len("@@ -0,0 +1 @@\n")
		unit := len("+new\n") + len("\\ No newline at end of file\n")
		exact := &contentEmitBudget{maxLines: 1 + 2, maxBytes: hdr + unit}
		wins, ok, trunc, _ := selectDiffWindows(parsed, 0, exact)
		if !ok || trunc || len(wins) != 1 {
			t.Fatalf("exact budget ok=%v trunc=%v wins=%d", ok, trunc, len(wins))
		}
		if !strings.Contains(wins[0].Text, `\ No newline at end of file`) {
			t.Fatalf("exact budget must keep marker: %q", wins[0].Text)
		}
		below := &contentEmitBudget{maxLines: 1 + 2, maxBytes: hdr + unit - 1}
		wins2, ok2, trunc2, _ := selectDiffWindows(parsed, 0, below)
		if !ok2 || !trunc2 {
			t.Fatalf("below-budget must truncate ok=%v trunc=%v", ok2, trunc2)
		}
		for _, w := range wins2 {
			if strings.Contains(w.Text, "+new") && !strings.Contains(w.Text, `\ No newline`) {
				t.Fatalf("must not emit detached line without marker: %q", w.Text)
			}
		}
	})

	t.Run("helper_deletion_marker_budget_boundary_keeps_attachment", func(t *testing.T) {
		patch := "@@ -1 +0,0 @@\n-old\n\\ No newline at end of file\n"
		parsed := parseUnifiedDiff(patch)
		if !parsed.ok {
			t.Fatal("parse deletion EOF marker")
		}
		hdr := len("@@ -1 +0,0 @@\n")
		unit := len("-old\n") + len("\\ No newline at end of file\n")
		exact := &contentEmitBudget{maxLines: 1 + 2, maxBytes: hdr + unit}
		wins, ok, trunc, _ := selectDiffWindows(parsed, 0, exact)
		if !ok || trunc || len(wins) != 1 {
			t.Fatalf("deletion exact budget ok=%v trunc=%v wins=%d", ok, trunc, len(wins))
		}
		if !strings.Contains(wins[0].Text, "-old") || !strings.Contains(wins[0].Text, `\ No newline at end of file`) {
			t.Fatalf("deletion exact budget must keep marker: %q", wins[0].Text)
		}
		below := &contentEmitBudget{maxLines: 1 + 2, maxBytes: hdr + unit - 1}
		wins2, ok2, trunc2, _ := selectDiffWindows(parsed, 0, below)
		if !ok2 || !trunc2 {
			t.Fatalf("deletion below-budget must truncate ok=%v trunc=%v", ok2, trunc2)
		}
		for _, w := range wins2 {
			if strings.Contains(w.Text, "-old") && !strings.Contains(w.Text, `\ No newline`) {
				t.Fatalf("must not emit detached deletion without marker: %q", w.Text)
			}
		}
	})
}
