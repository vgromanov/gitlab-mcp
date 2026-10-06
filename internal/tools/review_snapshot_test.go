package tools

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
)

// snapMR describes how the fixture answers for one MR ("project/iid").
// Zero values mean 200; unknown MRs are 404.
type snapMR struct {
	sha            string
	files          int
	diffsStatus    int // status of the diffs endpoint
	approvalStatus int // status of /approval_state; 404 makes the legacy /approvals answer
	legacyStatus   int
}

type snapFixture struct {
	cs *mcp.ClientSession

	mu     sync.Mutex
	paths  []string
	byPath map[string]int
}

func (f *snapFixture) count(suffix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for p, c := range f.byPath {
		if strings.HasSuffix(p, suffix) {
			n += c
		}
	}
	return n
}

func (f *snapFixture) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.paths)
}

var snapPath = regexp.MustCompile(`^/api/v4/projects/([^/]+)/merge_requests/(\d+)(/.*)?$`)

func snapMRBody(project, iid, sha string) string {
	return fmt.Sprintf(`{"id":%[2]s,"iid":%[2]s,"project_id":%[1]s,"title":"MR %[2]s","state":"opened","draft":true,
	"source_branch":"feat","target_branch":"main","sha":%[3]q,"web_url":"https://gl.example/mr/%[2]s",
	"updated_at":"2026-10-06T10:00:00Z","detailed_merge_status":"mergeable","description":"must not leak",
	"author":{"id":7,"username":"me","name":"Me","avatar_url":"https://x/a.png","state":"active"},
	"reviewers":[{"id":8,"username":"rev","name":"Rev","avatar_url":"https://x/b.png"}],
	"diff_refs":{"base_sha":"b","head_sha":%[3]q,"start_sha":"s"}}`, project, iid, sha)
}

func snapDiffsBody(page, per, total int) string {
	lo, hi := (page-1)*per, min(page*per, total)
	items := []string{}
	for i := lo; i < hi; i++ {
		items = append(items, fmt.Sprintf(`{"old_path":"old%04d","new_path":"f%04d","diff":"@@ patch must not leak","new_file":%t,"renamed_file":%t,"deleted_file":%t,"collapsed":%t,"too_large":%t}`,
			i, i, i == 0, i == 1, i == 2, i == 3, i == 4))
	}
	return "[" + strings.Join(items, ",") + "]"
}

func newSnapFixture(t *testing.T, cfg *config.Config, mrs map[string]snapMR) *snapFixture {
	t.Helper()
	f := &snapFixture{byPath: map[string]int{}}
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.byPath[r.URL.Path]++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		m := snapPath.FindStringSubmatch(r.URL.Path)
		var mr snapMR
		var ok bool
		if m != nil {
			mr, ok = mrs[m[1]+"/"+m[2]]
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			writeFixture(w, `{"message":"404 Not Found"}`)
			return
		}
		switch m[3] {
		case "":
			writeFixture(w, snapMRBody(m[1], m[2], mr.sha))
		case "/diffs":
			if mr.diffsStatus != 0 {
				w.WriteHeader(mr.diffsStatus)
				writeFixture(w, `{"message":"boom"}`)
				return
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
			if page < 1 || per != 100 {
				w.WriteHeader(http.StatusBadRequest)
				writeFixture(w, `{"message":"page and per_page=100 must be sent"}`)
				return
			}
			if page*per < mr.files {
				w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
			}
			writeFixture(w, snapDiffsBody(page, per, mr.files))
		case "/approval_state":
			if mr.approvalStatus != 0 {
				w.WriteHeader(mr.approvalStatus)
				writeFixture(w, `{"message":"nope"}`)
				return
			}
			writeFixture(w, approvalStateBody)
		case "/approvals":
			if mr.legacyStatus != 0 {
				w.WriteHeader(mr.legacyStatus)
				writeFixture(w, `{"message":"nope"}`)
				return
			}
			writeFixture(w, legacyApprovalsBody)
		default:
			w.WriteHeader(http.StatusNotFound)
			writeFixture(w, `{"message":"404 Not Found"}`)
		}
	}))
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "test"}, nil)
	RegisterMergeRequests(srv, Deps{Config: cfg, Client: cli})
	f.cs = testutil.MCPConnect(t, srv)
	return f
}

func (f *snapFixture) call(t *testing.T, args map[string]any) (map[string]any, string) {
	t.Helper()
	res, err := f.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_review_snapshot", Arguments: args})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		return nil, contentText(res)
	}
	m, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content %T", res.StructuredContent)
	}
	return m, ""
}

func snapEntries(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	raw, _ := out["merge_requests"].([]any)
	entries := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		entries = append(entries, r.(map[string]any))
	}
	return entries
}

func sortedMapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// AC1: one MR is a 404, the others succeed; order and echoed ids are kept, a
// numeric project_id (as get_review_queue emits) works, and nothing leaks.
func TestGetReviewSnapshot_batchPartialFailure(t *testing.T) {
	f := newSnapFixture(t, &config.Config{}, map[string]snapMR{
		"42/1": {sha: "aaa", files: 2}, "7/3": {sha: "ccc", files: 1},
	})
	out, errText := f.call(t, map[string]any{"mrs": []any{
		map[string]any{"project_id": "42", "iid": 1},
		map[string]any{"project_id": "42", "iid": 2}, // 404
		map[string]any{"project_id": 7, "iid": 3},
	}})
	if errText != "" {
		t.Fatal(errText)
	}
	if !slices.Equal(sortedMapKeys(map[string]any{"include": 0, "merge_requests": 0}), sortedMapKeys(out)) {
		t.Fatalf("top-level keys = %v", sortedMapKeys(out))
	}
	if inc := fmt.Sprint(out["include"]); inc != "[changes approvals]" {
		t.Fatalf("include = %s, want [changes approvals]", inc)
	}
	es := snapEntries(t, out)
	if len(es) != 3 {
		t.Fatalf("entries = %d", len(es))
	}
	// the 404 MR: only its own error, no half-filled metadata
	if !slices.Equal(sortedMapKeys(es[1]), []string{"error", "iid", "project_id"}) || !strings.Contains(es[1]["error"].(string), "404") {
		t.Fatalf("404 entry = %v", es[1])
	}
	wantKeys := []string{"approvals", "author", "changes", "detailed_merge_status", "diff_refs", "draft", "iid", "project_id",
		"reviewers", "sha", "source_branch", "state", "target_branch", "title", "updated_at", "web_url"}
	for _, i := range []int{0, 2} {
		if !slices.Equal(sortedMapKeys(es[i]), wantKeys) {
			t.Fatalf("entry %d keys = %v, want %v", i, sortedMapKeys(es[i]), wantKeys)
		}
	}
	e := es[0]
	if e["project_id"] != "42" || e["iid"] != float64(1) || e["title"] != "MR 1" || e["state"] != "opened" || e["draft"] != true ||
		e["source_branch"] != "feat" || e["target_branch"] != "main" || e["sha"] != "aaa" || e["detailed_merge_status"] != "mergeable" ||
		e["updated_at"] != "2026-10-06T10:00:00Z" {
		t.Fatalf("metadata = %v", e)
	}
	if es[2]["project_id"] != float64(7) || es[2]["sha"] != "ccc" {
		t.Fatalf("numeric project entry = %v", es[2])
	}
	if refs := e["diff_refs"].(map[string]any); refs["base_sha"] != "b" || refs["head_sha"] != "aaa" || refs["start_sha"] != "s" {
		t.Fatalf("diff_refs = %v", refs)
	}
	if !slices.Equal(sortedMapKeys(e["author"].(map[string]any)), []string{"id", "name", "username"}) ||
		!slices.Equal(sortedMapKeys(e["reviewers"].([]any)[0].(map[string]any)), []string{"id", "name", "username"}) {
		t.Fatalf("people not compacted: %v / %v", e["author"], e["reviewers"])
	}
	// changes: no patches, flags passed through
	ch := e["changes"].(map[string]any)
	files := ch["files"].([]any)
	if len(files) != 2 || ch["complete"] != true || ch["truncated_reason"] != nil || ch["next_page"] != float64(0) {
		t.Fatalf("changes = %v", ch)
	}
	if k := sortedMapKeys(files[0].(map[string]any)); !slices.Equal(k, []string{"collapsed", "deleted_file", "new_file", "new_path", "old_path", "renamed_file", "too_large"}) {
		t.Fatalf("file keys = %v", k)
	}
	if files[0].(map[string]any)["new_file"] != true || files[1].(map[string]any)["renamed_file"] != true {
		t.Fatalf("file flags = %v", files)
	}
	// approvals: SDK shape
	ap := e["approvals"].(map[string]any)
	if ap["approval_rules_overwritten"] != false || len(ap["rules"].([]any)) != 1 {
		t.Fatalf("approvals = %v", ap)
	}
	for _, p := range f.paths {
		if strings.Contains(p, "/merge_requests/2/") && !strings.HasSuffix(p, "/merge_requests/2") {
			t.Fatalf("sections requested for the 404 MR: %s", p)
		}
	}
	if raw := fmt.Sprint(out); strings.Contains(raw, "must not leak") || strings.Contains(raw, "x/a.png") || strings.Contains(raw, "x/b.png") {
		t.Fatal("snapshot leaked fields outside the contract")
	}
}

// AC2: expected_sha mismatch sets head_changed; a match says false; no
// expected_sha leaves the key out. Sections are still returned for the new head.
func TestGetReviewSnapshot_expectedSHA(t *testing.T) {
	f := newSnapFixture(t, &config.Config{}, map[string]snapMR{"42/1": {sha: "new", files: 1}})
	out, errText := f.call(t, map[string]any{"include": []any{"changes"}, "mrs": []any{
		map[string]any{"project_id": "42", "iid": 1, "expected_sha": "old"},
		map[string]any{"project_id": "42", "iid": 1, "expected_sha": "new"},
		map[string]any{"project_id": "42", "iid": 1},
	}})
	if errText != "" {
		t.Fatal(errText)
	}
	es := snapEntries(t, out)
	if es[0]["head_changed"] != true || es[0]["expected_sha"] != "old" || es[0]["sha"] != "new" {
		t.Fatalf("mismatch entry = %v", es[0])
	}
	if es[0]["changes"] == nil {
		t.Fatal("sections must still be returned after head_changed")
	}
	if es[1]["head_changed"] != false || es[1]["expected_sha"] != "new" {
		t.Fatalf("match entry = %v", es[1])
	}
	if _, has := es[2]["head_changed"]; has {
		t.Fatalf("head_changed must be absent without expected_sha: %v", es[2])
	}
}

// AC3: 150 files need two pages of 100. complete tracks whether the cap cut the list.
func TestGetReviewSnapshot_changesPaging(t *testing.T) {
	f := newSnapFixture(t, &config.Config{}, map[string]snapMR{"42/1": {sha: "aaa", files: 150}})
	ref := []any{map[string]any{"project_id": "42", "iid": 1}}
	changes := func(extra map[string]any) map[string]any {
		args := map[string]any{"mrs": ref, "include": []any{"changes"}}
		for k, v := range extra {
			args[k] = v
		}
		out, errText := f.call(t, args)
		if errText != "" {
			t.Fatal(errText)
		}
		return snapEntries(t, out)[0]["changes"].(map[string]any)
	}

	before := f.count("/diffs")
	ch := changes(map[string]any{"changes_max_pages": 1})
	if ch["complete"] != false || ch["next_page"] != float64(2) || len(ch["files"].([]any)) != 100 ||
		!strings.Contains(ch["truncated_reason"].(string), "changes_max_pages=1") || f.count("/diffs")-before != 1 {
		t.Fatalf("capped at 1 page: %v (requests %d)", ch, f.count("/diffs")-before)
	}

	before = f.count("/diffs")
	ch = changes(nil) // default cap of 3 pages covers 150 files
	if ch["complete"] != true || ch["next_page"] != float64(0) || ch["truncated_reason"] != nil ||
		len(ch["files"].([]any)) != 150 || f.count("/diffs")-before != 2 {
		t.Fatalf("default cap: complete=%v files=%d requests=%d", ch["complete"], len(ch["files"].([]any)), f.count("/diffs")-before)
	}

	// cap exactly equal to the pages needed is still complete
	if ch = changes(map[string]any{"changes_max_pages": 2}); ch["complete"] != true || len(ch["files"].([]any)) != 150 {
		t.Fatalf("cap at the boundary: %v", ch["complete"])
	}
	// an absurd cap is clamped, not an error
	if ch = changes(map[string]any{"changes_max_pages": 500}); ch["complete"] != true {
		t.Fatalf("clamped cap: %v", ch["complete"])
	}
}

// A failing section reports its own error and leaves metadata and the other
// section intact; approvals use the 404-only legacy fallback.
func TestGetReviewSnapshot_sectionIsolation(t *testing.T) {
	f := newSnapFixture(t, &config.Config{}, map[string]snapMR{
		"42/1": {sha: "a", files: 1, diffsStatus: 500, approvalStatus: 404},  // diffs fail, approvals via legacy
		"42/2": {sha: "b", files: 1, approvalStatus: 403},                    // approvals fail (403 does not fall back)
		"42/3": {sha: "c", files: 1, approvalStatus: 404, legacyStatus: 500}, // both approval endpoints fail
	})
	out, errText := f.call(t, map[string]any{"mrs": []any{
		map[string]any{"project_id": "42", "iid": 1}, map[string]any{"project_id": "42", "iid": 2}, map[string]any{"project_id": "42", "iid": 3},
	}})
	if errText != "" {
		t.Fatal(errText)
	}
	es := snapEntries(t, out)
	for i, e := range es {
		if e["title"] == nil || e["sha"] == nil || e["error"] != nil {
			t.Fatalf("entry %d lost metadata or got an MR-level error: %v", i, e)
		}
	}
	if ce := es[0]["changes"].(map[string]any); ce["error"] == nil || ce["files"] != nil || ce["complete"] != nil {
		t.Fatalf("changes error section = %v", ce)
	}
	if rules := es[0]["approvals"].(map[string]any)["rules"].([]any); len(rules) != 1 || rules[0].(map[string]any)["name"] != "All Members" {
		t.Fatalf("legacy fallback approvals = %v", es[0]["approvals"])
	}
	if es[1]["changes"].(map[string]any)["complete"] != true || es[1]["approvals"].(map[string]any)["error"] == nil {
		t.Fatalf("entry 2 = %v", es[1])
	}
	if f.count("/merge_requests/2/approvals") != 0 {
		t.Fatal("403 on approval_state must not fall back to the legacy endpoint")
	}
	if es[2]["approvals"].(map[string]any)["error"] == nil {
		t.Fatalf("entry 3 approvals = %v", es[2]["approvals"])
	}
}

func TestGetReviewSnapshot_include(t *testing.T) {
	f := newSnapFixture(t, &config.Config{}, map[string]snapMR{"42/1": {sha: "a", files: 1}})
	ref := []any{map[string]any{"project_id": "42", "iid": 1}}

	// unknown and not-yet-implemented sections reject the call before any request
	for _, tc := range []struct{ include, want string }{
		{"bogus", `invalid include "bogus": must be one of changes, approvals, discussions, pipeline`},
		{"discussions", `include "discussions" is not implemented yet (available: changes, approvals)`},
		{"pipeline", `include "pipeline" is not implemented yet`},
	} {
		_, errText := f.call(t, map[string]any{"mrs": ref, "include": []any{"changes", tc.include}})
		if !strings.Contains(errText, tc.want) {
			t.Fatalf("include %q: error = %q, want %q", tc.include, errText, tc.want)
		}
	}
	if f.total() != 0 {
		t.Fatalf("rejected include made %d requests", f.total())
	}

	// subset: only approvals
	out, _ := f.call(t, map[string]any{"mrs": ref, "include": []any{"approvals", "approvals"}})
	e := snapEntries(t, out)[0]
	if e["approvals"] == nil || e["changes"] != nil || fmt.Sprint(out["include"]) != "[approvals]" || f.count("/diffs") != 0 {
		t.Fatalf("approvals-only entry = %v (include %v)", sortedMapKeys(e), out["include"])
	}

	// empty list: metadata only
	before := f.total()
	out, _ = f.call(t, map[string]any{"mrs": ref, "include": []any{}})
	e = snapEntries(t, out)[0]
	if e["changes"] != nil || e["approvals"] != nil || e["sha"] != "a" || f.total()-before != 1 || fmt.Sprint(out["include"]) != "[]" {
		t.Fatalf("metadata-only entry = %v, requests %d", sortedMapKeys(e), f.total()-before)
	}
}

func TestGetReviewSnapshot_inputValidation(t *testing.T) {
	f := newSnapFixture(t, &config.Config{DefaultProjectID: "42", AllowedProjectIDs: []string{"42"}}, map[string]snapMR{
		"42/1": {sha: "a", files: 1}, "7/1": {sha: "z", files: 1},
	})
	if _, errText := f.call(t, map[string]any{"mrs": []any{}}); !strings.Contains(errText, "mrs must hold 1 to 10") {
		t.Fatalf("empty mrs: %q", errText)
	}
	eleven := make([]any, 11)
	for i := range eleven {
		eleven[i] = map[string]any{"project_id": "42", "iid": 1}
	}
	if _, errText := f.call(t, map[string]any{"mrs": eleven}); !strings.Contains(errText, "got 11") {
		t.Fatalf("11 mrs: %q", errText)
	}
	if f.total() != 0 {
		t.Fatalf("invalid batch made %d requests", f.total())
	}

	// per-MR problems stay per MR and make no request for that MR
	out, errText := f.call(t, map[string]any{"include": []any{}, "mrs": []any{
		map[string]any{"iid": 1},                     // default project 42
		map[string]any{"project_id": "7", "iid": 1},  // not allowlisted
		map[string]any{"project_id": "42", "iid": 0}, // bad iid
		map[string]any{"project_id": 1.5, "iid": 1},  // not an id
		map[string]any{"project_id": true, "iid": 1}, // wrong type
	}})
	if errText != "" {
		t.Fatal(errText)
	}
	es := snapEntries(t, out)
	if es[0]["error"] != nil || es[0]["project_id"] != "42" || es[0]["sha"] != "a" {
		t.Fatalf("default-project entry = %v", es[0])
	}
	for i, want := range map[int]string{1: "not allowed", 2: "iid must be >= 1", 3: "positive integer", 4: "positive integer"} {
		if got, _ := es[i]["error"].(string); !strings.Contains(got, want) || es[i]["sha"] != nil {
			t.Fatalf("entry %d error = %q, want %q", i, got, want)
		}
	}
	if f.total() != 1 {
		t.Fatalf("requests = %d, want only the one valid MR", f.total())
	}
}

func TestGetReviewSnapshot_defaultProjectRequired(t *testing.T) {
	f := newSnapFixture(t, &config.Config{}, nil)
	out, _ := f.call(t, map[string]any{"mrs": []any{map[string]any{"iid": 1}}})
	if got, _ := snapEntries(t, out)[0]["error"].(string); !strings.Contains(got, "project_id is required") || f.total() != 0 {
		t.Fatalf("error = %q, requests %d", got, f.total())
	}
}
