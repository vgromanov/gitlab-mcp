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
	discs          int    // discussions served, 100 per page (see snapDiscussionsBody)
	discFailPage   int    // discussions page answering 500
	shaAfter       string // sha the MR reports from its 2nd read on (a push mid-snapshot)
	recheckFails   bool   // the 2nd MR read answers 500
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

// snapDiscussionsBody serves discussion i by kind i%4: 0 = unresolved diff
// thread (diff note with a position, a reply, a trailing system note); 1 =
// resolved thread; 2 = plain individual note; 3 = system-only discussion.
func snapDiscussionsBody(page, per, total int) string {
	const author = `{"id":7,"username":"me","name":"Me","email":"leak@x","avatar_url":"https://x/a.png"}`
	note := func(id int, body string, system, resolvable, resolved bool, pos string) string {
		return fmt.Sprintf(`{"id":%d,"type":"DiscussionNote","body":%q,"author":%s,"system":%t,"resolvable":%t,"resolved":%t,
		"created_at":"2026-10-06T10:00:00Z","updated_at":"2026-10-06T11:00:00Z"%s}`, id, body, author, system, resolvable, resolved, pos)
	}
	const pos = `,"position":{"base_sha":"b","start_sha":"s","head_sha":"h","position_type":"text","new_path":"f.go","new_line":3}`
	items := []string{}
	for i := (page - 1) * per; i < min(page*per, total); i++ {
		var notes []string
		switch i % 4 {
		case 0:
			notes = []string{note(i*10, "thread", false, true, false, pos), note(i*10+1, "reply", false, true, false, ""), note(i*10+2, "pushed a commit", true, false, false, "")}
		case 1:
			notes = []string{note(i*10, "fixed", false, true, true, pos), note(i*10+1, "ok", false, true, true, "")}
		case 2:
			notes = []string{note(i*10, "plain", false, false, false, "")}
		default:
			notes = []string{note(i*10, "added 1 commit", true, false, false, "")}
		}
		items = append(items, fmt.Sprintf(`{"id":"d%03d","individual_note":%t,"notes":[%s]}`, i, i%4 >= 2, strings.Join(notes, ",")))
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
		reads := f.byPath[r.URL.Path]
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
			if reads >= 2 && mr.recheckFails {
				w.WriteHeader(http.StatusForbidden) // not retried by the client, unlike 5xx
				writeFixture(w, `{"message":"nope"}`)
				return
			}
			sha := mr.sha
			if reads >= 2 && mr.shaAfter != "" {
				sha = mr.shaAfter
			}
			writeFixture(w, snapMRBody(m[1], m[2], sha))
		case "/discussions":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
			if page < 1 || per != 100 {
				w.WriteHeader(http.StatusBadRequest)
				writeFixture(w, `{"message":"page and per_page=100 must be sent"}`)
				return
			}
			if page == mr.discFailPage {
				w.WriteHeader(http.StatusForbidden) // not retried by the client, unlike 5xx
				writeFixture(w, `{"message":"nope"}`)
				return
			}
			if page*per < mr.discs {
				w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
			}
			writeFixture(w, snapDiscussionsBody(page, per, mr.discs))
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
	if inc := fmt.Sprint(out["include"]); inc != "[changes approvals discussions]" {
		t.Fatalf("include = %s, want [changes approvals discussions]", inc)
	}
	es := snapEntries(t, out)
	if len(es) != 3 {
		t.Fatalf("entries = %d", len(es))
	}
	// the 404 MR: only its own error, no half-filled metadata
	if !slices.Equal(sortedMapKeys(es[1]), []string{"error", "iid", "project_id"}) || !strings.Contains(es[1]["error"].(string), "404") {
		t.Fatalf("404 entry = %v", es[1])
	}
	wantKeys := []string{"approvals", "author", "changes", "detailed_merge_status", "diff_refs", "discussions", "draft", "head_changed", "iid", "project_id",
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

// AC2 (expected_sha part): a mismatch sets head_changed; a match says false. Sections are still returned for the new head.
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
	// no expected_sha: the end-of-snapshot recheck still reports a stable head
	if es[2]["head_changed"] != false || es[2]["expected_sha"] != nil || es[2]["current_sha"] != nil {
		t.Fatalf("no-expected entry = %v", es[2])
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

	// an unknown section rejects the call before any request
	for _, tc := range []struct{ include, want string }{
		{"bogus", `invalid include "bogus": must be one of changes, approvals, discussions, pipeline`},
		{"Pipeline", `invalid include "Pipeline": must be one of changes, approvals, discussions, pipeline`},
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

func snapDiscussions(t *testing.T, f *snapFixture, extra map[string]any) map[string]any {
	t.Helper()
	args := map[string]any{"include": []any{"discussions"}, "mrs": []any{map[string]any{"project_id": "42", "iid": 1}}}
	for k, v := range extra {
		args[k] = v
	}
	out, errText := f.call(t, args)
	if errText != "" {
		t.Fatal(errText)
	}
	return snapEntries(t, out)[0]["discussions"].(map[string]any)
}

// AC1: 250 discussions on three pages are compacted; the default cap (3 pages)
// covers them, so complete is true. System notes and system-only threads are out.
func TestGetReviewSnapshot_discussionsCompaction(t *testing.T) {
	f := newSnapFixture(t, &config.Config{}, map[string]snapMR{"42/1": {sha: "a", discs: 250}})
	ds := snapDiscussions(t, f, nil)
	list := ds["discussions"].([]any)
	// kinds 0, 1, 2 survive (63 + 63 + 62 of 250); the 62 system-only threads are dropped
	if len(list) != 188 || ds["complete"] != true || ds["truncated_reason"] != nil || ds["next_page"] != float64(0) ||
		ds["unresolved_count"] != float64(63) || f.count("/discussions") != 3 {
		t.Fatalf("discussions=%d complete=%v unresolved=%v next=%v requests=%d", len(list), ds["complete"], ds["unresolved_count"], ds["next_page"], f.count("/discussions"))
	}
	if !slices.Equal(sortedMapKeys(ds), []string{"complete", "discussions", "next_page", "truncated_reason", "unresolved_count"}) {
		t.Fatalf("section keys = %v", sortedMapKeys(ds))
	}
	byID := map[string]map[string]any{}
	for _, x := range list {
		byID[x.(map[string]any)["id"].(string)] = x.(map[string]any)
	}
	open, done, plain := byID["d000"], byID["d001"], byID["d002"]
	if byID["d003"] != nil {
		t.Fatal("system-only discussion must be dropped")
	}
	if !slices.Equal(sortedMapKeys(open), []string{"id", "notes", "resolvable", "resolved"}) ||
		open["resolvable"] != true || open["resolved"] != false || done["resolvable"] != true || done["resolved"] != true ||
		plain["resolvable"] != false || plain["resolved"] != false {
		t.Fatalf("states: open=%v done=%v plain=%v", open, done, plain)
	}
	notes := open["notes"].([]any)
	if len(notes) != 2 { // the trailing system note is excluded
		t.Fatalf("open notes = %d, want 2", len(notes))
	}
	n0 := notes[0].(map[string]any)
	if want := []string{"author", "body", "created_at", "id", "position", "system", "updated_at"}; !slices.Equal(sortedMapKeys(n0), want) {
		t.Fatalf("note keys = %v, want %v", sortedMapKeys(n0), want)
	}
	pos := n0["position"].(map[string]any)
	if n0["id"] != float64(0) || n0["body"] != "thread" || n0["system"] != false || n0["created_at"] != "2026-10-06T10:00:00Z" ||
		n0["updated_at"] != "2026-10-06T11:00:00Z" || pos["new_path"] != "f.go" || pos["new_line"] != float64(3) || pos["head_sha"] != "h" {
		t.Fatalf("note = %v", n0)
	}
	if !slices.Equal(sortedMapKeys(n0["author"].(map[string]any)), []string{"id", "name", "username"}) {
		t.Fatalf("author not compacted: %v", n0["author"])
	}
	if _, has := notes[1].(map[string]any)["position"]; has {
		t.Fatalf("reply must not carry a position: %v", notes[1])
	}
	if raw := fmt.Sprint(ds); strings.Contains(raw, "leak@x") || strings.Contains(raw, "x/a.png") {
		t.Fatal("discussions leaked author fields outside the contract")
	}
}

// AC1: complete is false, with next_page and a reason, when the cap cuts the list.
func TestGetReviewSnapshot_discussionsCap(t *testing.T) {
	f := newSnapFixture(t, &config.Config{}, map[string]snapMR{"42/1": {sha: "a", discs: 250}})
	before := f.count("/discussions")
	ds := snapDiscussions(t, f, map[string]any{"discussions_max_pages": 1})
	if ds["complete"] != false || ds["next_page"] != float64(2) || !strings.Contains(ds["truncated_reason"].(string), "discussions_max_pages=1") ||
		len(ds["discussions"].([]any)) != 75 || ds["unresolved_count"] != float64(25) || f.count("/discussions")-before != 1 {
		t.Fatalf("capped at 1 page: %v (requests %d)", ds, f.count("/discussions")-before)
	}
	// the cap equal to the pages needed is still complete; an absurd cap is clamped
	for _, n := range []int{3, 500} {
		if ds = snapDiscussions(t, f, map[string]any{"discussions_max_pages": n}); ds["complete"] != true || ds["next_page"] != float64(0) {
			t.Fatalf("cap %d: complete=%v", n, ds["complete"])
		}
	}
}

// AC3: system notes are excluded by default and kept with include_system.
func TestGetReviewSnapshot_discussionsSystemNotes(t *testing.T) {
	f := newSnapFixture(t, &config.Config{}, map[string]snapMR{"42/1": {sha: "a", discs: 4}})
	count := func(ds map[string]any) (discs, notes, system int) {
		for _, x := range ds["discussions"].([]any) {
			discs++
			for _, n := range x.(map[string]any)["notes"].([]any) {
				notes++
				if n.(map[string]any)["system"] == true {
					system++
				}
			}
		}
		return
	}
	for _, extra := range []map[string]any{nil, {"include_system": false}} {
		if d, n, s := count(snapDiscussions(t, f, extra)); d != 3 || n != 5 || s != 0 {
			t.Fatalf("default %v: discussions=%d notes=%d system=%d", extra, d, n, s)
		}
	}
	ds := snapDiscussions(t, f, map[string]any{"include_system": true})
	if d, n, s := count(ds); d != 4 || n != 7 || s != 2 || ds["unresolved_count"] != float64(1) {
		t.Fatalf("include_system: discussions=%d notes=%d system=%d unresolved=%v", d, n, s, ds["unresolved_count"])
	}
}

// A failed discussions page is a section error, never a half list; the rest survives.
func TestGetReviewSnapshot_discussionsPageError(t *testing.T) {
	f := newSnapFixture(t, &config.Config{}, map[string]snapMR{"42/1": {sha: "a", files: 1, discs: 250, discFailPage: 2}})
	out, errText := f.call(t, map[string]any{"mrs": []any{map[string]any{"project_id": "42", "iid": 1}}})
	if errText != "" {
		t.Fatal(errText)
	}
	e := snapEntries(t, out)[0]
	ds := e["discussions"].(map[string]any)
	if got, _ := ds["error"].(string); !strings.Contains(got, "page 2") || ds["discussions"] != nil || ds["complete"] != nil {
		t.Fatalf("discussions section = %v", ds)
	}
	if e["changes"].(map[string]any)["complete"] != true || e["approvals"].(map[string]any)["error"] != nil || e["sha"] != "a" {
		t.Fatalf("other sections lost: %v", sortedMapKeys(e))
	}
}

// AC2: the head is read again after the sections; a push in between sets
// head_changed and returns the new sha, with or without expected_sha.
func TestGetReviewSnapshot_headMovedDuringSnapshot(t *testing.T) {
	one := func(t *testing.T, mr snapMR, ref map[string]any, include []any) map[string]any {
		t.Helper()
		f := newSnapFixture(t, &config.Config{}, map[string]snapMR{"42/1": mr})
		ref["project_id"], ref["iid"] = "42", 1
		out, errText := f.call(t, map[string]any{"include": include, "mrs": []any{ref}})
		if errText != "" {
			t.Fatal(errText)
		}
		return snapEntries(t, out)[0]
	}
	changes := []any{"changes"}
	moved := snapMR{sha: "old", shaAfter: "new", files: 1}

	// moved, no expected_sha: sections describe sha, current_sha is the new head
	e := one(t, moved, map[string]any{}, changes)
	if e["sha"] != "old" || e["head_changed"] != true || e["current_sha"] != "new" || e["expected_sha"] != nil || e["changes"] == nil {
		t.Fatalf("moved entry = %v", e)
	}
	// moved and expected_sha matched the start: still changed
	if e = one(t, moved, map[string]any{"expected_sha": "old"}, changes); e["head_changed"] != true || e["current_sha"] != "new" || e["expected_sha"] != "old" {
		t.Fatalf("moved with expected_sha = %v", e)
	}
	// every section counts: a discussions-only snapshot is rechecked as well
	if e = one(t, moved, map[string]any{}, []any{"discussions"}); e["head_changed"] != true || e["current_sha"] != "new" {
		t.Fatalf("moved, discussions only = %v", e)
	}

	stable := snapMR{sha: "same", files: 1}
	if e = one(t, stable, map[string]any{"expected_sha": "same"}, changes); e["head_changed"] != false || e["current_sha"] != nil {
		t.Fatalf("stable entry = %v", e)
	}
	// stable head that differs from expected_sha: changed, but no current_sha (sha is the head)
	if e = one(t, stable, map[string]any{"expected_sha": "other"}, changes); e["head_changed"] != true || e["current_sha"] != nil || e["sha"] != "same" {
		t.Fatalf("stale expected_sha = %v", e)
	}

	// a failed recheck is reported, never read as "stable"
	broken := snapMR{sha: "old", shaAfter: "new", files: 1, recheckFails: true}
	e = one(t, broken, map[string]any{}, changes)
	if got, _ := e["head_recheck_error"].(string); !strings.Contains(got, "403") || e["head_changed"] != nil || e["current_sha"] != nil || e["changes"] == nil {
		t.Fatalf("recheck failure entry = %v", e)
	}
	if e = one(t, broken, map[string]any{"expected_sha": "other"}, changes); e["head_changed"] != true || e["head_recheck_error"] == nil {
		t.Fatalf("recheck failure with expected_sha = %v", e)
	}

	// metadata only: nothing is read after the metadata, so there is no recheck
	f := newSnapFixture(t, &config.Config{}, map[string]snapMR{"42/1": moved})
	out, _ := f.call(t, map[string]any{"include": []any{}, "mrs": []any{map[string]any{"project_id": "42", "iid": 1}}})
	if e = snapEntries(t, out)[0]; e["head_changed"] != nil || e["current_sha"] != nil || e["head_recheck_error"] != nil || f.total() != 1 {
		t.Fatalf("metadata-only entry = %v, requests %d", e, f.total())
	}
}
