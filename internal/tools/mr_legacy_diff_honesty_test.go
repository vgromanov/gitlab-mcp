package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

func legacyMRHandler(projectID int64, diffsJSON, nextPage string, headerPresent bool, hasConflicts bool, head string) http.Handler {
	status := "can_be_merged"
	if hasConflicts {
		status = "conflict"
	}
	if head == "" {
		head = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(path, "/merge_requests/") && strings.HasSuffix(path, "/diffs"):
			if headerPresent {
				w.Header().Set("X-Next-Page", nextPage)
			}
			_, _ = io.WriteString(w, diffsJSON)
		case strings.Contains(path, "/merge_requests/"):
			hc := "false"
			if hasConflicts {
				hc = "true"
			}
			_, _ = io.WriteString(w, fmt.Sprintf(
				`{"id":1,"iid":1,"project_id":%d,"source_project_id":%d,"has_conflicts":%s,"detailed_merge_status":%q,"diff_refs":{"head_sha":%q,"base_sha":"b","start_sha":"b"}}`,
				projectID, projectID, hc, status, head))
		case strings.Contains(path, "/projects/"):
			_, _ = io.WriteString(w, fmt.Sprintf(`{"id":%d,"path_with_namespace":"g/p"}`, projectID))
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	})
}

func diffItemJSON(path, diff string, collapsed, tooLarge *bool) string {
	c, t := "null", "null"
	if collapsed != nil {
		if *collapsed {
			c = "true"
		} else {
			c = "false"
		}
	}
	if tooLarge != nil {
		if *tooLarge {
			t = "true"
		} else {
			t = "false"
		}
	}
	return fmt.Sprintf(
		`{"old_path":%q,"new_path":%q,"a_mode":"100644","b_mode":"100644","diff":%q,"new_file":false,"renamed_file":false,"deleted_file":false,"generated_file":false,"collapsed":%s,"too_large":%s}`,
		path, path, diff, c, t)
}

func legacyBool(v bool) *bool { return &v }

func sectionFromOut(t *testing.T, out any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	sec, _ := m["section"].(map[string]any)
	if sec == nil {
		t.Fatalf("missing section in %s", raw)
	}
	return sec
}

func TestGetMergeRequestDiffs_completeSinglePage(t *testing.T) {
	d := newMRDiffsDeps(t, legacyMRHandler(42, knownPresenceDiff, "", true, false, ""))
	_, out, err := getMergeRequestDiffs(context.Background(), nil, getMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	m := mustMap(t, out)
	diffs, _ := m["diffs"].([]any)
	if len(diffs) != 1 {
		t.Fatalf("diffs=%v", diffs)
	}
	pag, _ := m["pagination"].(map[string]any)
	if pag["next_page"].(float64) != 0 {
		t.Fatalf("next_page=%v", pag["next_page"])
	}
	sec := sectionFromOut(t, out)
	if sec["content_complete"] != readmeta.ContentCompleteTrue {
		t.Fatalf("content_complete=%v", sec["content_complete"])
	}
	if sec["pagination_exhausted"] != true {
		t.Fatal("expected exhausted")
	}
}

func TestGetMergeRequestDiffs_over100IncompleteWithContinuation(t *testing.T) {
	// AC1: >100 files + NextPage continuation metadata.
	items := make([]string, 0, 101)
	for i := 0; i < 101; i++ {
		items = append(items, diffItemJSON(fmt.Sprintf("f%d.go", i), "+x\n", legacyBool(false), legacyBool(false)))
	}
	body := "[" + strings.Join(items, ",") + "]"
	d := newMRDiffsDeps(t, legacyMRHandler(42, body, "2", true, false, ""))
	_, out, err := getMergeRequestDiffs(context.Background(), nil, getMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	m := mustMap(t, out)
	pag, _ := m["pagination"].(map[string]any)
	if pag["next_page"].(float64) != 2 {
		t.Fatalf("next_page=%v want 2", pag["next_page"])
	}
	sec := sectionFromOut(t, out)
	if sec["content_complete"] == readmeta.ContentCompleteTrue {
		t.Fatal("must not claim complete when NextPage remains")
	}
	if sec["next_cursor"] == nil || sec["next_cursor"] == "" {
		t.Fatal("expected informational next_cursor")
	}
	if sec["pagination_exhausted"] == true {
		t.Fatal("must not mark exhausted when X-Next-Page=2")
	}
}

func TestGetMergeRequestDiffs_localTruncateIncomplete(t *testing.T) {
	// AC1: local truncate_lines marks incomplete.
	long := "line1\nline2\nline3\nline4\n"
	body := "[" + diffItemJSON("a.go", long, legacyBool(false), legacyBool(false)) + "]"
	d := newMRDiffsDeps(t, legacyMRHandler(42, body, "", true, false, ""))
	_, out, err := getMergeRequestDiffs(context.Background(), nil, getMergeRequestDiffsIn{
		pidMR:         pidMR{ProjectID: "42", MergeRequestIID: 1},
		TruncateLines: 2,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	sec := sectionFromOut(t, out)
	if sec["content_complete"] != readmeta.ContentCompleteFalse {
		t.Fatalf("content_complete=%v want false after truncate", sec["content_complete"])
	}
	lims, _ := sec["limitations"].([]any)
	found := false
	for _, l := range lims {
		lm, _ := l.(map[string]any)
		if strings.Contains(fmt.Sprint(lm["message"]), "truncate_lines") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected truncate limitation, got %#v", lims)
	}
	m := mustMap(t, out)
	diffs, _ := m["diffs"].([]any)
	item, _ := diffs[0].(map[string]any)
	got := fmt.Sprint(item["diff"])
	if !strings.Contains(got, "truncated") || strings.Contains(got, "line3\nline4") {
		t.Fatalf("diff not truncated as expected: %q", got)
	}
}

func TestGetMergeRequestDiffs_exhaustedOmittedPresenceNotComplete(t *testing.T) {
	// AC2: exhausted page with omitted collapsed/too_large still incomplete/unknown.
	body := `[{"old_path":"a.go","new_path":"a.go","diff":"+x\n","collapsed":null,"too_large":null}]`
	d := newMRDiffsDeps(t, legacyMRHandler(42, body, "", true, false, ""))
	_, out, err := getMergeRequestDiffs(context.Background(), nil, getMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	sec := sectionFromOut(t, out)
	if sec["pagination_exhausted"] != true {
		t.Fatal("expected exhausted")
	}
	if sec["content_complete"] == readmeta.ContentCompleteTrue {
		t.Fatal("null presence must not yield complete")
	}
}

func TestGetMergeRequestDiffs_collapsedNotComplete(t *testing.T) {
	// AC2: exhausted + collapsed → incomplete.
	body := "[" + diffItemJSON("big.go", "", legacyBool(true), legacyBool(false)) + "]"
	d := newMRDiffsDeps(t, legacyMRHandler(42, body, "", true, false, ""))
	_, out, err := getMergeRequestDiffs(context.Background(), nil, getMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	sec := sectionFromOut(t, out)
	if sec["content_complete"] != readmeta.ContentCompleteFalse {
		t.Fatalf("content_complete=%v", sec["content_complete"])
	}
}

func TestGetMergeRequestFileDiff_missingOnPartialPageUnobserved(t *testing.T) {
	// AC3: requested file not on inspected partial page is unobserved, not absent.
	body := "[" + diffItemJSON("a.go", "+a\n", legacyBool(false), legacyBool(false)) + "]"
	d := newMRDiffsDeps(t, legacyMRHandler(42, body, "2", true, false, ""))
	_, out, err := getMergeRequestFileDiff(context.Background(), nil, getMergeRequestFileDiffIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
		Files: []string{"a.go", "missing.go"},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	m := mustMap(t, out)
	diffs, _ := m["diffs"].([]any)
	if len(diffs) != 1 {
		t.Fatalf("diffs=%v", diffs)
	}
	sec := sectionFromOut(t, out)
	if sec["content_complete"] == readmeta.ContentCompleteTrue {
		t.Fatal("must not claim complete when requested file unobserved on partial page")
	}
	lims, _ := sec["limitations"].([]any)
	found := false
	for _, l := range lims {
		lm, _ := l.(map[string]any)
		msg := fmt.Sprint(lm["message"])
		if strings.Contains(msg, "unobserved") && strings.Contains(msg, "missing.go") {
			found = true
		}
		if strings.Contains(strings.ToLower(msg), "absent") && strings.Contains(msg, "proven") {
			// "not proven absent" is OK; "conclusively absent" would be wrong
		}
	}
	if !found {
		t.Fatalf("expected unobserved limitation, got %#v", lims)
	}
}

func TestGetMergeRequestConflicts_flagsPreservedHeuristicCoverage(t *testing.T) {
	// AC4: empty heuristic scan must not override authoritative conflict flags.
	body := "[" + diffItemJSON("a.go", "+no markers\n", legacyBool(false), legacyBool(false)) + "]"
	d := newMRDiffsDeps(t, legacyMRHandler(42, body, "", true, true, ""))
	_, out, err := getMergeRequestConflicts(context.Background(), nil, getMergeRequestConflictsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	m := mustMap(t, out)
	if m["has_conflicts"] != true {
		t.Fatalf("has_conflicts=%v want true from MR", m["has_conflicts"])
	}
	if m["detailed_merge_status"] != "conflict" {
		t.Fatalf("detailed_merge_status=%v", m["detailed_merge_status"])
	}
	cf, _ := m["conflict_files"].([]any)
	if len(cf) != 0 {
		t.Fatalf("conflict_files=%v want empty scan", cf)
	}
	sec := sectionFromOut(t, out)
	lims, _ := sec["limitations"].([]any)
	found := false
	for _, l := range lims {
		lm, _ := l.(map[string]any)
		if strings.Contains(fmt.Sprint(lm["message"]), "heuristic") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected heuristic coverage limitation, got %#v", lims)
	}
}

func TestGetMergeRequestConflicts_over200Incomplete(t *testing.T) {
	// AC1 for conflicts ceiling: NextPage with large page.
	items := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		items = append(items, diffItemJSON(fmt.Sprintf("c%d.go", i), "<<<<<<< HEAD\nx\n>>>>>>>\n", legacyBool(false), legacyBool(false)))
	}
	body := "[" + strings.Join(items, ",") + "]"
	d := newMRDiffsDeps(t, legacyMRHandler(42, body, "2", true, true, ""))
	_, out, err := getMergeRequestConflicts(context.Background(), nil, getMergeRequestConflictsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	m := mustMap(t, out)
	if m["has_conflicts"] != true {
		t.Fatal("flags must stay true")
	}
	cf, _ := m["conflict_files"].([]any)
	if len(cf) != 5 {
		t.Fatalf("conflict_files=%v", cf)
	}
	sec := sectionFromOut(t, out)
	if sec["content_complete"] == readmeta.ContentCompleteTrue {
		t.Fatal("NextPage must keep scan incomplete")
	}
}

func TestGetMergeRequestDiffs_headerAbsentNotExhausted(t *testing.T) {
	d := newMRDiffsDeps(t, legacyMRHandler(42, knownPresenceDiff, "", false, false, ""))
	_, out, err := getMergeRequestDiffs(context.Background(), nil, getMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	sec := sectionFromOut(t, out)
	if sec["pagination_exhausted"] == true {
		t.Fatal("absent X-Next-Page must not exhaust")
	}
	if sec["content_complete"] == readmeta.ContentCompleteTrue {
		t.Fatal("unknown paging must not claim complete")
	}
}

func TestGetMergeRequestDiffs_shortHeadSHANull(t *testing.T) {
	d := newMRDiffsDeps(t, legacyMRHandler(42, knownPresenceDiff, "", true, false, "abc"))
	_, out, err := getMergeRequestDiffs(context.Background(), nil, getMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	sec := sectionFromOut(t, out)
	if sec["head_sha"] != nil {
		t.Fatalf("short head must be null, got %v", sec["head_sha"])
	}
	if sec["consistency"] != readmeta.ConsistencyUnknown {
		t.Fatalf("consistency=%v", sec["consistency"])
	}
}

func mustMap(t *testing.T, out any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestReviewProfiles_legacyDiffGettersCallTool(t *testing.T) {
	// Both review profiles must surface honesty section on registered CallTool.
	body := knownPresenceDiff
	for _, profile := range []string{"review_read", "review_write"} {
		for _, toolName := range []string{"get_merge_request_diffs", "get_merge_request_file_diff", "get_merge_request_conflicts"} {
			profile, toolName := profile, toolName
			t.Run(profile+"/"+toolName, func(t *testing.T) {
				var hits int32
				base := legacyMRHandler(42, body, "", true, true, "")
				h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/diffs") {
						atomic.AddInt32(&hits, 1)
					}
					base.ServeHTTP(w, r)
				})
				ts := httptest.NewServer(h)
				t.Cleanup(ts.Close)
				cli, err := gitlab.NewClient("t",
					gitlab.WithBaseURL(ts.URL+"/api/v4"),
					gitlab.WithoutRetries(),
					gitlab.WithInterceptor(igl.BudgetInterceptor()),
				)
				if err != nil {
					t.Fatal(err)
				}
				d := Deps{Config: &config.Config{
					Token:             "t",
					ToolProfile:       profile,
					AllowedProjectIDs: []string{"42"},
				}, Client: cli}

				srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
				RegisterAll(srv, d)
				cs := testutil.MCPConnect(t, srv)

				args := map[string]any{"project_id": "42", "merge_request_iid": 1}
				if toolName == "get_merge_request_file_diff" {
					args["files"] = []any{"a.go"}
				}
				res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: toolName, Arguments: args})
				if err != nil {
					t.Fatal(err)
				}
				if res.IsError {
					t.Fatalf("IsError: %v", res)
				}
				m := structuredOrTextMap(t, res)
				if _, ok := m["section"].(map[string]any); !ok {
					t.Fatalf("missing section: %#v", m)
				}
				if toolName != "get_merge_request_conflicts" {
					if _, ok := m["diffs"]; !ok {
						t.Fatalf("missing diffs: %#v", m)
					}
				} else if _, ok := m["has_conflicts"]; !ok {
					t.Fatalf("missing has_conflicts: %#v", m)
				}
				if atomic.LoadInt32(&hits) < 1 {
					t.Fatal("expected diffs fetch")
				}
			})
		}
	}
}

func structuredOrTextMap(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res.StructuredContent != nil {
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok && tc != nil {
			var m map[string]any
			if err := json.Unmarshal([]byte(tc.Text), &m); err == nil {
				return m
			}
		}
	}
	t.Fatalf("no structured/text JSON content: %#v", res)
	return nil
}

func TestGetMergeRequestDiffs_omittedPatchNotComplete(t *testing.T) {
	// Exhausted page + known collapsed/too_large false, but diff field omitted → unknown/not complete.
	body := `[{"old_path":"a.go","new_path":"a.go","a_mode":"100644","b_mode":"100644","new_file":false,"renamed_file":false,"deleted_file":false,"generated_file":false,"collapsed":false,"too_large":false}]`
	d := newMRDiffsDeps(t, legacyMRHandler(42, body, "", true, false, ""))
	_, out, err := getMergeRequestDiffs(context.Background(), nil, getMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	sec := sectionFromOut(t, out)
	if sec["pagination_exhausted"] != true {
		t.Fatal("expected exhausted")
	}
	if sec["content_complete"] == readmeta.ContentCompleteTrue {
		t.Fatal("omitted diff patch must not claim content_complete=true")
	}
	lims, _ := sec["limitations"].([]any)
	found := false
	for _, l := range lims {
		lm, _ := l.(map[string]any)
		if strings.Contains(fmt.Sprint(lm["message"]), "omitted or null") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected omitted/null patch limitation, got %#v", lims)
	}
}

func TestGetMergeRequestDiffs_nullPatchNotComplete(t *testing.T) {
	body := `[{"old_path":"a.go","new_path":"a.go","diff":null,"collapsed":false,"too_large":false}]`
	d := newMRDiffsDeps(t, legacyMRHandler(42, body, "", true, false, ""))
	_, out, err := getMergeRequestDiffs(context.Background(), nil, getMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	sec := sectionFromOut(t, out)
	if sec["content_complete"] == readmeta.ContentCompleteTrue {
		t.Fatal("null diff patch must not claim complete")
	}
}

func TestGetMergeRequestDiffs_explicitEmptyPatchMayComplete(t *testing.T) {
	// Explicit "" is known empty — with exhausted page and known flags, complete is allowed.
	body := `[{"old_path":"a.go","new_path":"a.go","a_mode":"100644","b_mode":"100644","diff":"","new_file":false,"renamed_file":false,"deleted_file":false,"generated_file":false,"collapsed":false,"too_large":false}]`
	d := newMRDiffsDeps(t, legacyMRHandler(42, body, "", true, false, ""))
	_, out, err := getMergeRequestDiffs(context.Background(), nil, getMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	sec := sectionFromOut(t, out)
	if sec["content_complete"] != readmeta.ContentCompleteTrue {
		t.Fatalf("explicit empty patch should allow complete, got %v", sec["content_complete"])
	}
}

func TestGetMergeRequestDiffs_serverOverflowNotComplete(t *testing.T) {
	body := `[{"old_path":"a.go","new_path":"a.go","diff":"+x\n","collapsed":false,"too_large":false,"overflow":true}]`
	d := newMRDiffsDeps(t, legacyMRHandler(42, body, "", true, false, ""))
	_, out, err := getMergeRequestDiffs(context.Background(), nil, getMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	sec := sectionFromOut(t, out)
	if sec["content_complete"] != readmeta.ContentCompleteFalse {
		t.Fatalf("overflow must force incomplete, got %v", sec["content_complete"])
	}
	// Item JSON shape remains SDK fields (no overflow projected onto diffs item).
	m := mustMap(t, out)
	diffs, _ := m["diffs"].([]any)
	item, _ := diffs[0].(map[string]any)
	if _, ok := item["overflow"]; ok {
		t.Fatal("overflow must not be projected onto preserved SDK item shape")
	}
	for _, k := range []string{"old_path", "new_path", "a_mode", "b_mode", "diff", "new_file", "renamed_file", "deleted_file", "generated_file", "collapsed", "too_large"} {
		if _, ok := item[k]; !ok {
			t.Fatalf("missing SDK field %q in %#v", k, item)
		}
	}
}

func TestGetMergeRequestDiffs_preservesSDKItemFields(t *testing.T) {
	d := newMRDiffsDeps(t, legacyMRHandler(42, knownPresenceDiff, "", true, false, ""))
	_, out, err := getMergeRequestDiffs(context.Background(), nil, getMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	m := mustMap(t, out)
	diffs, _ := m["diffs"].([]any)
	item, _ := diffs[0].(map[string]any)
	for _, k := range []string{"old_path", "new_path", "a_mode", "b_mode", "diff", "new_file", "renamed_file", "deleted_file", "generated_file", "collapsed", "too_large"} {
		if _, ok := item[k]; !ok {
			t.Fatalf("missing %q", k)
		}
	}
	if item["diff"] != "+x\n" || item["collapsed"] != false || item["too_large"] != false {
		t.Fatalf("item=%#v", item)
	}
}

func TestGetMergeRequestDiffs_firstPageCeilingPerPage100(t *testing.T) {
	var gotPerPage string
	base := legacyMRHandler(42, knownPresenceDiff, "", true, false, "")
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/diffs") {
			gotPerPage = r.URL.Query().Get("per_page")
		}
		base.ServeHTTP(w, r)
	})
	d := newMRDiffsDeps(t, h)
	_, _, err := getMergeRequestDiffs(context.Background(), nil, getMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	if gotPerPage != "100" {
		t.Fatalf("per_page=%q want 100", gotPerPage)
	}
}

func TestGetMergeRequestConflicts_firstPageCeilingPerPage200(t *testing.T) {
	var gotPerPage string
	base := legacyMRHandler(42, knownPresenceDiff, "", true, true, "")
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/diffs") {
			gotPerPage = r.URL.Query().Get("per_page")
		}
		base.ServeHTTP(w, r)
	})
	d := newMRDiffsDeps(t, h)
	_, _, err := getMergeRequestConflicts(context.Background(), nil, getMergeRequestConflictsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	if gotPerPage != "200" {
		t.Fatalf("per_page=%q want 200", gotPerPage)
	}
}

func TestGetMergeRequestDiffs_contextualBudgetStop(t *testing.T) {
	// Inject tight MaxItems without the helper resetting a custom synthetic budget.
	items := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		items = append(items, diffItemJSON(fmt.Sprintf("f%d.go", i), "+x\n", legacyBool(false), legacyBool(false)))
	}
	body := "[" + strings.Join(items, ",") + "]"
	d := newMRDiffsDeps(t, legacyMRHandler(42, body, "", true, false, ""))
	budget := igl.DefaultBudget()
	budget.MaxItems = 2
	ctx := igl.WithBudget(context.Background(), budget)
	defer budget.Cancel()

	_, out, err := getMergeRequestDiffs(ctx, nil, getMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	m := mustMap(t, out)
	diffs, _ := m["diffs"].([]any)
	if len(diffs) != 2 {
		t.Fatalf("retained=%d want 2 under contextual budget", len(diffs))
	}
	sec := sectionFromOut(t, out)
	if sec["content_complete"] != readmeta.ContentCompleteFalse {
		t.Fatalf("budget stop must be incomplete, got %v", sec["content_complete"])
	}
	lims, _ := sec["limitations"].([]any)
	found := false
	for _, l := range lims {
		lm, _ := l.(map[string]any)
		if lm["code"] == readmeta.CodeBudgetItems {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected budget_items limitation, got %#v", lims)
	}
}

func TestGetMergeRequestDiffs_freshMRForkChangeDenyBeforeDiff(t *testing.T) {
	// authorizeMROwnerAndForks sees source=42; helper's fresh MR metadata flips to 99 → deny before /diffs.
	var mrHits, diffHits int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/diffs"):
			atomic.AddInt32(&diffHits, 1)
			_, _ = io.WriteString(w, knownPresenceDiff)
		case strings.Contains(path, "/merge_requests/"):
			n := atomic.AddInt32(&mrHits, 1)
			src := int64(42)
			if n >= 2 {
				src = 99 // fresh metadata diverges after authorize snapshot
			}
			_, _ = io.WriteString(w, fmt.Sprintf(
				`{"id":1,"iid":1,"project_id":42,"source_project_id":%d,"has_conflicts":false,"detailed_merge_status":"can_be_merged","diff_refs":{"head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","base_sha":"b","start_sha":"b"}}`,
				src))
		case strings.Contains(path, "/projects/42"):
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}}`)
		case strings.Contains(path, "/projects/99"):
			_, _ = io.WriteString(w, `{"id":99,"path_with_namespace":"fork/p","namespace":{"id":8,"kind":"group"}}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	})
	d := newMRDiffsDeps(t, h)
	d.Config.AllowedProjectIDs = []string{"42"}
	_, _, err := getMergeRequestDiffs(context.Background(), nil, getMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
		t.Fatalf("want authz_denied on fresh fork identity, got %v", err)
	}
	if atomic.LoadInt32(&diffHits) != 0 {
		t.Fatalf("diffs must not stream after fresh-MR fork deny, hits=%d", diffHits)
	}
	if atomic.LoadInt32(&mrHits) < 2 {
		t.Fatalf("expected authorize + fresh helper MR fetches, mrHits=%d", mrHits)
	}
}

func TestGetMergeRequestFileDiff_emptyFilesReturnsNoDiffs(t *testing.T) {
	// Compatibility: files:[] / nil must not return the whole page.
	d := newMRDiffsDeps(t, legacyMRHandler(42, knownPresenceDiff, "", true, false, ""))
	for _, files := range [][]string{nil, {}} {
		_, out, err := getMergeRequestFileDiff(context.Background(), nil, getMergeRequestFileDiffIn{
			pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
			Files: files,
		}, d)
		if err != nil {
			t.Fatal(err)
		}
		m := mustMap(t, out)
		diffs, _ := m["diffs"].([]any)
		if len(diffs) != 0 {
			t.Fatalf("files=%v diffs=%v want empty", files, diffs)
		}
	}
}

func TestGetMergeRequestFileDiff_oldPathMatch(t *testing.T) {
	body := `[{"old_path":"old.go","new_path":"new.go","a_mode":"100644","b_mode":"100644","diff":"+x\n","new_file":false,"renamed_file":true,"deleted_file":false,"generated_file":false,"collapsed":false,"too_large":false}]`
	d := newMRDiffsDeps(t, legacyMRHandler(42, body, "", true, false, ""))
	_, out, err := getMergeRequestFileDiff(context.Background(), nil, getMergeRequestFileDiffIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
		Files: []string{"old.go"},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	m := mustMap(t, out)
	diffs, _ := m["diffs"].([]any)
	if len(diffs) != 1 {
		t.Fatalf("old_path match failed: %v", diffs)
	}
}

func TestReviewProfiles_fileDiffEmptyFilesNoPatches(t *testing.T) {
	body := knownPresenceDiff
	for _, profile := range []string{"review_read", "review_write"} {
		profile := profile
		t.Run(profile, func(t *testing.T) {
			d := newMRDiffsDeps(t, legacyMRHandler(42, body, "", true, false, ""))
			d.Config.ToolProfile = profile
			d.Config.AllowedProjectIDs = []string{"42"}
			srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
			RegisterAll(srv, d)
			cs := testutil.MCPConnect(t, srv)
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
				Name: "get_merge_request_file_diff",
				Arguments: map[string]any{
					"project_id":        "42",
					"merge_request_iid": 1,
					"files":             []any{},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.IsError {
				t.Fatalf("IsError: %v", res)
			}
			m := structuredOrTextMap(t, res)
			diffs, _ := m["diffs"].([]any)
			if len(diffs) != 0 {
				t.Fatalf("files:[] must return no patches, got %#v", diffs)
			}
			if _, ok := m["section"].(map[string]any); !ok {
				t.Fatal("missing section")
			}
		})
	}
}

func TestGetMergeRequestDiffs_fullPageNotFiltered(t *testing.T) {
	// get_merge_request_diffs must still return the full inspected page (no FilterFiles).
	body := "[" + diffItemJSON("a.go", "+a\n", legacyBool(false), legacyBool(false)) + "," +
		diffItemJSON("b.go", "+b\n", legacyBool(false), legacyBool(false)) + "]"
	d := newMRDiffsDeps(t, legacyMRHandler(42, body, "", true, false, ""))
	_, out, err := getMergeRequestDiffs(context.Background(), nil, getMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	m := mustMap(t, out)
	diffs, _ := m["diffs"].([]any)
	if len(diffs) != 2 {
		t.Fatalf("diffs=%v want full page", diffs)
	}
}

func TestDecodeDiffPatchPresence(t *testing.T) {
	cases := []struct {
		raw  string
		want patchPresence
	}{
		{`{"collapsed":false}`, patchAbsent},
		{`{"diff":null,"collapsed":false}`, patchNull},
		{`{"diff":"","collapsed":false}`, patchEmptyString},
		{`{"diff":"+x\n","collapsed":false}`, patchNonEmpty},
		{`{"diff":123}`, patchWrongType},
	}
	for _, tc := range cases {
		got, err := decodeDiffPatchPresence(json.RawMessage(tc.raw))
		if err != nil {
			t.Fatalf("raw=%s err=%v", tc.raw, err)
		}
		if got != tc.want {
			t.Fatalf("raw=%s got=%v want=%v", tc.raw, got, tc.want)
		}
	}
}
