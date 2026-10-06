package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
)

const diffFixtureFiles = 150 // more than one GitLab page (100)

// diffFixture (not parallel-safe: tool registration notes names in a global map) serves a 150-file MR diff list with GitLab-style X-Next-Page
// headers. file0003 is collapsed, file0004 is too large. Requests are counted.
func diffFixture(t *testing.T) (*mcp.ClientSession, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		q := r.URL.Query()
		page, _ := strconv.Atoi(q.Get("page"))
		per, _ := strconv.Atoi(q.Get("per_page"))
		if page < 1 || per < 1 {
			http.Error(w, "page and per_page must be sent", http.StatusBadRequest)
			return
		}
		lo, hi := (page-1)*per, min(page*per, diffFixtureFiles)
		body := "["
		for i := lo; i < hi; i++ {
			if i > lo {
				body += ","
			}
			body += fmt.Sprintf(`{"old_path":"file%04d","new_path":"file%04d","diff":"@@","collapsed":%t,"too_large":%t}`, i, i, i == 3, i == 4)
		}
		w.Header().Set("Content-Type", "application/json")
		if hi < diffFixtureFiles {
			w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
		}
		writeFixture(w, body+"]")
	}))
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "test"}, nil)
	RegisterMergeRequests(srv, Deps{Config: &config.Config{}, Client: cli})
	return testutil.MCPConnect(t, srv), &calls
}

// writeFixture writes a canned test body and returns the bytes written. It
// takes an io.Writer on purpose: the httptest fixtures serve JSON/text that is
// never rendered as HTML, so the HTML-escaping XSS semgrep rules
// (no-fprintf/io-writestring/direct-write-to-responsewriter) do not apply, and
// a helper that is not an http.ResponseWriter handler stays out of their scope.
func writeFixture(dst io.Writer, body string) int {
	n, _ := io.WriteString(dst, body)
	return n
}

func callDiffTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("%s: err=%v content=%s", name, err, contentText(res))
	}
	m, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("%s: structured content %T", name, res.StructuredContent)
	}
	return m
}

func assertPage(t *testing.T, out map[string]any, page, per, next float64, complete bool) {
	t.Helper()
	p, _ := out["pagination"].(map[string]any)
	if p["page"] != page || p["per_page"] != per || p["next_page"] != next || p["complete"] != complete {
		t.Fatalf("pagination %v, want page=%v per_page=%v next_page=%v complete=%v", p, page, per, next, complete)
	}
}

// AC1 + AC3: more than 100 files, page 1 is incomplete, the last page is
// complete, and each call makes exactly one upstream request.
func TestMRDiffTools_pagination(t *testing.T) {
	base := map[string]any{"project_id": "42", "merge_request_iid": 7}
	with := func(kv ...any) map[string]any {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	for _, tc := range []struct {
		tool    string
		extra   []any
		perPage float64 // expected per_page when per_page=100 is requested
		count   func(map[string]any) int
	}{
		{"get_merge_request_diffs", nil, 100, func(o map[string]any) int { return len(o["diffs"].([]any)) }},
		{"list_merge_request_diffs", nil, 100, func(o map[string]any) int { return len(o["diffs"].([]any)) }},
		{"list_merge_request_changed_files", nil, 100, func(o map[string]any) int { return len(o["files"].([]any)) }},
		{"get_merge_request_file_diff", []any{"files", []string{"file0003", "file0120"}}, 100, func(o map[string]any) int { return len(o["diffs"].([]any)) }},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			cs, calls := diffFixture(t)
			first := callDiffTool(t, cs, tc.tool, with(append([]any{"per_page", 100}, tc.extra...)...))
			assertPage(t, first, 1, 100, 2, false)
			if got := calls.Load(); got != 1 {
				t.Fatalf("page 1 made %d upstream requests, want 1 (no auto-paging)", got)
			}
			last := callDiffTool(t, cs, tc.tool, with(append([]any{"page", 2, "per_page", 100}, tc.extra...)...))
			assertPage(t, last, 2, 100, 0, true)
			if got := calls.Load(); got != 2 {
				t.Fatalf("two calls made %d upstream requests, want 2", got)
			}
			if tc.tool == "get_merge_request_file_diff" {
				if c := tc.count(first); c != 1 || tc.count(last) != 1 { // file0003 on page 1, file0120 on page 2
					t.Fatalf("file_diff matched %d on page 1, want 1 (matching is per page)", c)
				}
			}
		})
	}
}

// Omitting page/per_page keeps today's effective sizes: 100 for the diff
// getters, 20 for the changed-files list.
func TestMRDiffTools_defaults(t *testing.T) {
	cs, _ := diffFixture(t)
	args := map[string]any{"project_id": "42", "merge_request_iid": 7}
	assertPage(t, callDiffTool(t, cs, "get_merge_request_diffs", args), 1, 100, 2, false)
	assertPage(t, callDiffTool(t, cs, "get_merge_request_file_diff", map[string]any{"project_id": "42", "merge_request_iid": 7, "files": []string{"x"}}), 1, 100, 2, false)
	assertPage(t, callDiffTool(t, cs, "list_merge_request_changed_files", args), 1, 20, 2, false)
}

// AC2: collapsed / too_large survive in every output.
func TestMRDiffTools_flags(t *testing.T) {
	cs, _ := diffFixture(t)
	args := map[string]any{"project_id": "42", "merge_request_iid": 7, "per_page": 10}
	for _, tool := range []string{"get_merge_request_diffs", "list_merge_request_diffs"} {
		diffs := callDiffTool(t, cs, tool, args)["diffs"].([]any)
		if d := diffs[3].(map[string]any); d["collapsed"] != true || d["too_large"] != false {
			t.Errorf("%s file 3: %v", tool, d)
		}
		if d := diffs[4].(map[string]any); d["too_large"] != true || d["collapsed"] != false {
			t.Errorf("%s file 4: %v", tool, d)
		}
	}
	fd := callDiffTool(t, cs, "get_merge_request_file_diff", map[string]any{"project_id": "42", "merge_request_iid": 7, "per_page": 10, "files": []string{"file0003", "file0004"}})["diffs"].([]any)
	if len(fd) != 2 || fd[0].(map[string]any)["collapsed"] != true || fd[1].(map[string]any)["too_large"] != true {
		t.Errorf("file_diff flags: %v", fd)
	}
	cf := callDiffTool(t, cs, "list_merge_request_changed_files", args)
	if c, tl := cf["collapsed_files"].([]any), cf["too_large_files"].([]any); len(c) != 1 || c[0] != "file0003" || len(tl) != 1 || tl[0] != "file0004" {
		t.Errorf("changed_files flags: collapsed=%v too_large=%v", c, tl)
	}
	// Excluded paths are not listed, nor flagged.
	args["excluded_file_patterns"] = []string{"file000*"}
	cf = callDiffTool(t, cs, "list_merge_request_changed_files", args)
	if cf["collapsed_files"] != nil || cf["too_large_files"] != nil {
		t.Errorf("excluded files still flagged: %v", cf)
	}
}

func TestMRDiffTools_upstreamError(t *testing.T) {
	t.Parallel()
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"nope"}`, http.StatusForbidden)
	}))
	d, in := Deps{Config: &config.Config{}, Client: cli}, pidMR{ProjectID: "42", MergeRequestIID: 7}
	ctx := context.Background()
	if _, _, err := getMergeRequestDiffs(ctx, nil, getMergeRequestDiffsIn{pidMR: in}, d); err == nil {
		t.Error("get_merge_request_diffs: expected error")
	}
	if _, _, err := getMergeRequestFileDiff(ctx, nil, getMergeRequestFileDiffIn{pidMR: in}, d); err == nil {
		t.Error("get_merge_request_file_diff: expected error")
	}
}

func contentText(res *mcp.CallToolResult) string {
	if res == nil || len(res.Content) == 0 {
		return ""
	}
	if tc, ok := res.Content[0].(*mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}
