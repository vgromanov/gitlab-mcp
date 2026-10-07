package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
)

// RVG-172: approve (sha required in the review profile, readback) and merge
// (sha/squash/remove-branch/auto-merge, merged vs pending, refusals). Failure
// cases use 4xx only: writes are not retried and GET 5xx retries are slow.

const mgMRPath = "/api/v4/projects/42/merge_requests/3"

type mgResp struct {
	status int
	body   string
}

// mgFixture answers "METHOD /suffix" (suffix after the MR path, "" = the MR
// itself) from a response list (the last entry repeats), counts every request
// and keeps the last body per key. An unrouted request is a 404.
type mgFixture struct {
	mu     sync.Mutex
	routes map[string][]mgResp
	hits   map[string]int
	bodies map[string]string
	total  int
}

func (f *mgFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	key := r.Method + " " + strings.TrimPrefix(r.URL.Path, mgMRPath)
	f.mu.Lock()
	n := f.hits[key]
	f.hits[key]++
	f.total++
	f.bodies[key] = string(b)
	rs := f.routes[key]
	f.mu.Unlock()
	resp := mgResp{http.StatusNotFound, `{"message":"404 Not found"}`}
	if len(rs) > 0 {
		resp = rs[min(n, len(rs)-1)]
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.status)
	writeFixture(w, resp.body)
}

func (f *mgFixture) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[key]
}

func (f *mgFixture) sent(t *testing.T, key string) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	m := map[string]any{}
	if f.bodies[key] != "" {
		if err := json.Unmarshal([]byte(f.bodies[key]), &m); err != nil {
			t.Fatalf("%s body %q: %v", key, f.bodies[key], err)
		}
	}
	return m
}

func mgSession(t *testing.T, cfg *config.Config, routes map[string][]mgResp) (*mcp.ClientSession, *mgFixture) {
	t.Helper()
	f := &mgFixture{routes: routes, hits: map[string]int{}, bodies: map[string]string{}}
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	cli, err := gitlab.NewClient("test-token", gitlab.WithBaseURL(ts.URL+"/api/v4"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Token = "x"
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "test"}, nil)
	RegisterMergeRequests(srv, Deps{Config: cfg, Client: cli})
	return testutil.MCPConnect(t, srv), f
}

func mgCall(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (map[string]any, string, bool) {
	t.Helper()
	args["project_id"], args["merge_request_iid"] = "42", 3
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	m, _ := res.StructuredContent.(map[string]any)
	return m, contentText(res), res.IsError
}

func mgMR(state, sha string, mwps bool, detailed string) string {
	b, _ := json.Marshal(map[string]any{
		"iid": 3, "state": state, "sha": sha, "merge_when_pipeline_succeeds": mwps,
		"detailed_merge_status": detailed, "web_url": "https://gitlab.example/mr/3",
	})
	return string(b)
}

const (
	mgHead  = "1111111111111111111111111111111111111111"
	mgStale = "2222222222222222222222222222222222222222"
	mgOK    = http.StatusOK
)

var (
	mgApprovals = mgResp{mgOK, `{"id":1,"iid":3,"approved":true,"approvals_left":0,"approved_by":[{"user":{"id":2,"username":"bob"}}]}`}
	mgState     = mgResp{mgOK, approvalStateBody}
)

func review() *config.Config { return &config.Config{ToolProfile: config.ProfileReview} }

func TestApprove_reviewProfileRequiresSHA(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"missing": {}, "empty": {"sha": ""}, "blank": {"sha": "  "},
	} {
		t.Run(name, func(t *testing.T) {
			cs, f := mgSession(t, review(), nil)
			_, text, isErr := mgCall(t, cs, "approve_merge_request", args)
			if !isErr || !strings.Contains(text, "sha is required") {
				t.Fatalf("isError=%v text=%q, want an input error about sha", isErr, text)
			}
			if f.total != 0 {
				t.Fatalf("%d requests sent, want 0 (rejected before any request)", f.total)
			}
		})
	}
}

func TestApprove_reviewProfileReadsBack(t *testing.T) {
	cs, f := mgSession(t, review(), map[string][]mgResp{
		"POST /approve":       {mgApprovals},
		"GET /approval_state": {mgState},
		"GET ":                {{mgOK, mgMR("opened", mgHead, false, "mergeable")}},
	})
	out, text, isErr := mgCall(t, cs, "approve_merge_request", map[string]any{"sha": mgHead})
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if f.count("POST /approve") != 1 || f.count("GET /approval_state") != 1 || f.count("GET ") != 1 || f.total != 3 {
		t.Fatalf("requests: %v, want 1 approve + 1 state + 1 MR read", f.hits)
	}
	if got := f.sent(t, "POST /approve")["sha"]; got != mgHead {
		t.Fatalf("approve sent sha %v, want %s", got, mgHead)
	}
	if out["approved"] != true || out["sha"] != mgHead || out["head_sha"] != mgHead || out["head_changed_after_write"] != false {
		t.Fatalf("result = %v", out)
	}
	st, _ := out["approval_state"].(map[string]any)
	if rules, _ := st["rules"].([]any); len(rules) != 1 {
		t.Fatalf("approval_state not returned: %v", out["approval_state"])
	}
	if _, has := out["error"]; has {
		t.Fatalf("error set on a clean readback: %v", out["error"])
	}
}

func TestApprove_reviewProfileHeadMovedAfterWrite(t *testing.T) {
	cs, _ := mgSession(t, review(), map[string][]mgResp{
		"POST /approve":       {mgApprovals},
		"GET /approval_state": {mgState},
		"GET ":                {{mgOK, mgMR("opened", mgStale, false, "mergeable")}},
	})
	out, text, isErr := mgCall(t, cs, "approve_merge_request", map[string]any{"sha": mgHead})
	if isErr || out["head_changed_after_write"] != true || out["head_sha"] != mgStale || out["approved"] != true {
		t.Fatalf("isError=%v text=%s out=%v, want approved with head_changed_after_write", isErr, text, out)
	}
}

func TestApprove_reviewProfileFailedReadbackIsAnError(t *testing.T) {
	cs, _ := mgSession(t, review(), map[string][]mgResp{
		"POST /approve": {mgApprovals},
		// no approval_state, no legacy approvals, no MR: every readback is a 404
	})
	out, _, isErr := mgCall(t, cs, "approve_merge_request", map[string]any{"sha": mgHead})
	e, _ := out["error"].(string)
	if !isErr || out["approved"] != true || strings.Count(e, "readback_failed") != 2 {
		t.Fatalf("isError=%v out=%v, want approved=true with two readback_failed errors", isErr, out)
	}
}

func TestApprove_refusalsAreClear(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"stale sha", 409, `{"message":"SHA does not match HEAD of source branch: ` + mgHead + `"}`, "head_changed: GitLab answered 409"},
		{"own MR", 403, `{"message":"403 Forbidden"}`, "approval_not_allowed: GitLab answered 403"},
		{"unauthorized", 401, `{"message":"401 Unauthorized"}`, "approval_not_allowed: GitLab answered 401"},
		{"missing MR", 404, `{"message":"404 Not found"}`, "not_found: GitLab answered 404 (the merge request does not exist"},
		{"other conflict", 409, `{"message":"Something else"}`, "conflict: GitLab answered 409"},
	}
	for _, tc := range cases {
		for _, cfg := range []*config.Config{review(), {}} {
			t.Run(tc.name+"/"+cfg.ProfileName(), func(t *testing.T) {
				cs, f := mgSession(t, cfg, map[string][]mgResp{"POST /approve": {{tc.status, tc.body}}})
				out, text, isErr := mgCall(t, cs, "approve_merge_request", map[string]any{"sha": mgStale})
				if !isErr || !strings.Contains(text, tc.want) || out != nil {
					t.Fatalf("isError=%v text=%q, want %q", isErr, text, tc.want)
				}
				if tc.status == 409 && tc.name == "stale sha" && !strings.Contains(text, mgHead) {
					t.Fatalf("the current head from GitLab is missing: %q", text)
				}
				if f.count("POST /approve") != 1 || f.total != 1 {
					t.Fatalf("requests %v, want exactly one approve and nothing else (no retry, no readback)", f.hits)
				}
			})
		}
	}
}

func TestApprove_otherProfilesKeepTheirBehaviour(t *testing.T) {
	cs, f := mgSession(t, &config.Config{}, map[string][]mgResp{"POST /approve": {mgApprovals}})
	out, text, isErr := mgCall(t, cs, "approve_merge_request", map[string]any{})
	if isErr {
		t.Fatalf("sha must stay optional outside the review profile: %s", text)
	}
	if _, has := f.sent(t, "POST /approve")["sha"]; has || f.total != 1 {
		t.Fatalf("sent %v in %d requests, want no sha and one request", f.sent(t, "POST /approve"), f.total)
	}
	if out["approved"] != true || out["approvals_left"] == nil || out["head_sha"] != nil {
		t.Fatalf("daily output must stay the raw approvals object, got %v", out)
	}
}

func TestApprove_descriptionNamesTheRule(t *testing.T) {
	desc := func(cfg *config.Config) string {
		cs, _ := mgSession(t, cfg, nil)
		res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range res.Tools {
			if x.Name == "approve_merge_request" {
				return x.Description
			}
		}
		t.Fatal("approve_merge_request not registered")
		return ""
	}
	if d := desc(review()); !strings.Contains(d, "sha is REQUIRED") {
		t.Fatalf("review description: %q", d)
	}
	if d := desc(&config.Config{}); strings.Contains(d, "REQUIRED") {
		t.Fatalf("daily description must not claim sha is required: %q", d)
	}
}

func TestMerge_optionsReachTheWire(t *testing.T) {
	cs, f := mgSession(t, &config.Config{}, map[string][]mgResp{"PUT /merge": {{mgOK, mgMR("merged", mgHead, false, "")}}})
	_, text, isErr := mgCall(t, cs, "merge_merge_request", map[string]any{
		"sha": mgHead, "squash": false, "should_remove_source_branch": true, "auto_merge": true, "merge_commit_message": "msg",
	})
	if isErr {
		t.Fatal(text)
	}
	sent := f.sent(t, "PUT /merge")
	want := map[string]any{"sha": mgHead, "squash": false, "should_remove_source_branch": true, "auto_merge": true, "merge_commit_message": "msg"}
	for k, v := range want {
		if sent[k] != v {
			t.Fatalf("PUT body %v: %s = %v, want %v", sent, k, sent[k], v)
		}
	}
}

func TestMerge_mergedWhenTheResponseSaysMerged(t *testing.T) {
	cs, f := mgSession(t, &config.Config{}, map[string][]mgResp{"PUT /merge": {{mgOK, mgMR("merged", mgHead, false, "")}}})
	out, _, isErr := mgCall(t, cs, "merge_merge_request", map[string]any{})
	if isErr || out["result"] != "merged" || out["state"] != "merged" || out["web_url"] == nil {
		t.Fatalf("out = %v", out)
	}
	if _, has := out["pending_reason"]; has || f.total != 1 {
		t.Fatalf("a merged response needs no re-read: %v, %d requests", out, f.total)
	}
	// Backwards compatible: nothing new is sent unless asked for.
	for _, k := range []string{"sha", "squash", "auto_merge", "should_remove_source_branch"} {
		if _, has := f.sent(t, "PUT /merge")[k]; has {
			t.Fatalf("unrequested option %s sent: %v", k, f.sent(t, "PUT /merge"))
		}
	}
}

func TestMerge_openedIsPendingWithOneWrite(t *testing.T) {
	cs, f := mgSession(t, &config.Config{}, map[string][]mgResp{
		"PUT /merge": {{mgOK, mgMR("opened", mgHead, false, "checking")}},
		"GET ":       {{mgOK, mgMR("opened", mgHead, false, "mergeable")}},
	})
	out, _, isErr := mgCall(t, cs, "merge_merge_request", map[string]any{})
	if isErr || out["result"] != "pending" || out["pending_reason"] != "not_merged_yet" || out["state"] != "opened" || out["detailed_merge_status"] != "mergeable" {
		t.Fatalf("out = %v", out)
	}
	if f.count("PUT /merge") != 1 || f.count("GET ") != 1 || f.total != 2 {
		t.Fatalf("requests %v, want exactly one merge write and one re-read", f.hits)
	}
}

func TestMerge_mergedOnlyWhenTheStateSaysSo(t *testing.T) {
	cs, _ := mgSession(t, &config.Config{}, map[string][]mgResp{
		"PUT /merge": {{mgOK, mgMR("opened", mgHead, false, "checking")}},
		"GET ":       {{mgOK, mgMR("merged", mgHead, false, "")}},
	})
	out, _, _ := mgCall(t, cs, "merge_merge_request", map[string]any{})
	if out["result"] != "merged" || out["state"] != "merged" {
		t.Fatalf("a re-read that says merged must win: %v", out)
	}
}

func TestMerge_autoMergeScheduledIsPending(t *testing.T) {
	cs, f := mgSession(t, &config.Config{}, map[string][]mgResp{
		"PUT /merge": {{mgOK, mgMR("opened", mgHead, true, "ci_still_running")}},
		"GET ":       {{mgOK, mgMR("opened", mgHead, true, "ci_still_running")}},
	})
	out, _, isErr := mgCall(t, cs, "merge_merge_request", map[string]any{"auto_merge": true})
	if isErr || out["result"] != "pending" || out["pending_reason"] != "auto_merge_scheduled" || out["merge_when_pipeline_succeeds"] != true {
		t.Fatalf("out = %v", out)
	}
	if f.count("PUT /merge") != 1 {
		t.Fatalf("merge written %d times, want 1", f.count("PUT /merge"))
	}
}

func TestMerge_failedReadbackStaysPending(t *testing.T) {
	cs, f := mgSession(t, &config.Config{}, map[string][]mgResp{"PUT /merge": {{mgOK, mgMR("opened", mgHead, false, "checking")}}})
	out, _, isErr := mgCall(t, cs, "merge_merge_request", map[string]any{})
	e, _ := out["readback_error"].(string)
	if isErr || out["result"] != "pending" || !strings.HasPrefix(e, "readback_failed") || f.count("PUT /merge") != 1 {
		t.Fatalf("out = %v", out)
	}
}

func TestMerge_refusalsAreClear(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"stale sha", 409, `{"message":"SHA does not match HEAD of source branch: ` + mgHead + `"}`, "head_changed: GitLab answered 409"},
		{"not mergeable", 405, `{"message":"405 Method Not Allowed"}`, "not_mergeable: GitLab answered 405 405 Method Not Allowed (state=opened, detailed_merge_status=not_approved)"},
		{"conflicts", 406, `{"message":"Branch cannot be merged"}`, "not_mergeable: GitLab answered 406 Branch cannot be merged (state=opened"},
		{"unauthorized", 401, `{"message":"401 Unauthorized"}`, "merge_not_permitted: GitLab answered 401"},
		{"forbidden", 403, `{"message":"403 Forbidden"}`, "merge_not_permitted: GitLab answered 403"},
		{"missing", 404, `{"message":"404 Not found"}`, "not_found: GitLab answered 404 (the merge request does not exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs, f := mgSession(t, &config.Config{}, map[string][]mgResp{
				"PUT /merge": {{tc.status, tc.body}},
				"GET ":       {{mgOK, mgMR("opened", mgHead, false, "not_approved")}},
			})
			out, text, isErr := mgCall(t, cs, "merge_merge_request", map[string]any{"sha": mgStale})
			if !isErr || out != nil || !strings.Contains(text, tc.want) {
				t.Fatalf("isError=%v text=%q, want %q", isErr, text, tc.want)
			}
			if tc.status == 409 && !strings.Contains(text, mgHead) {
				t.Fatalf("the current head from GitLab is missing: %q", text)
			}
			if f.count("PUT /merge") != 1 {
				t.Fatalf("merge sent %d times, want exactly 1 (never retried)", f.count("PUT /merge"))
			}
		})
	}
}

func TestMerge_refusalWithoutReadableReasonStillClear(t *testing.T) {
	cs, _ := mgSession(t, &config.Config{}, map[string][]mgResp{"PUT /merge": {{405, `{"message":"405 Method Not Allowed"}`}}})
	_, text, isErr := mgCall(t, cs, "merge_merge_request", map[string]any{})
	if !isErr || !strings.HasPrefix(text, "not_mergeable: GitLab answered 405") || strings.Contains(text, "detailed_merge_status") {
		t.Fatalf("isError=%v text=%q", isErr, text)
	}
}

func TestRefusal_passesThroughOtherErrors(t *testing.T) {
	if err := refusal(io.ErrUnexpectedEOF, mergeCodes); err != io.ErrUnexpectedEOF {
		t.Fatalf("non-GitLab error changed: %v", err)
	}
	cs, _ := mgSession(t, &config.Config{}, map[string][]mgResp{"PUT /merge": {{400, `{"message":"400 Bad request"}`}}})
	_, text, isErr := mgCall(t, cs, "merge_merge_request", map[string]any{})
	if !isErr || strings.Contains(text, "not_mergeable") || !strings.Contains(text, "400") {
		t.Fatalf("unmapped status must pass through unchanged: %q", text)
	}
}

// RVG-178: in the review profile merge_merge_request refuses unless the MR's
// author is the current user (one MR read + one user lookup, before the PUT).

const mgUser = "GET /api/v4/user"

func mgMRBy(authorID int, state string) string {
	b, _ := json.Marshal(map[string]any{
		"iid": 3, "state": state, "sha": mgHead, "author": map[string]any{"id": authorID, "username": "someone"},
		"web_url": "https://gitlab.example/mr/3",
	})
	return string(b)
}

const mgMeBody = `{"id":7,"username":"me"}`

func TestMerge_reviewProfileMergesOwnMR(t *testing.T) {
	cs, f := mgSession(t, review(), map[string][]mgResp{
		"GET ":       {{mgOK, mgMRBy(7, "opened")}, {mgOK, mgMRBy(7, "merged")}},
		mgUser:       {{mgOK, mgMeBody}},
		"PUT /merge": {{mgOK, mgMRBy(7, "merged")}},
	})
	out, text, isErr := mgCall(t, cs, "merge_merge_request", map[string]any{"sha": mgHead})
	if isErr || out["result"] != "merged" {
		t.Fatalf("own MR must merge: isErr=%v out=%v text=%q", isErr, out, text)
	}
	// Upstream cost of the guard: one MR read + one user lookup, then the single PUT.
	if f.count("GET ") != 1 || f.count(mgUser) != 1 || f.count("PUT /merge") != 1 || f.total != 3 {
		t.Fatalf("requests %v, want 1 MR read + 1 user lookup + 1 PUT", f.hits)
	}
}

func TestMerge_reviewProfileRefusesOthersMR(t *testing.T) {
	tests := []struct {
		name   string
		routes map[string][]mgResp
		want   string // prefix of the error text
		exact  bool
	}{
		{"other author", map[string][]mgResp{"GET ": {{mgOK, mgMRBy(8, "opened")}}, mgUser: {{mgOK, mgMeBody}}},
			"merge_not_permitted: MR is not authored by the current user", true},
		{"no author in the MR", map[string][]mgResp{"GET ": {{mgOK, mgMR("opened", mgHead, false, "")}}, mgUser: {{mgOK, mgMeBody}}},
			"merge_not_permitted: MR is not authored by the current user", true},
		{"user lookup fails", map[string][]mgResp{"GET ": {{mgOK, mgMRBy(7, "opened")}}, mgUser: {{401, `{"message":"401 Unauthorized"}`}}},
			"resolve current user", false},
		{"MR read fails", map[string][]mgResp{"GET ": {{404, `{"message":"404 Not found"}`}}, mgUser: {{mgOK, mgMeBody}}},
			"not_found: GitLab answered 404", false},
		{"MR read forbidden", map[string][]mgResp{"GET ": {{403, `{"message":"403 Forbidden"}`}}, mgUser: {{mgOK, mgMeBody}}},
			"merge_not_permitted: GitLab answered 403", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.routes["PUT /merge"] = []mgResp{{mgOK, mgMRBy(7, "merged")}}
			cs, f := mgSession(t, review(), tc.routes)
			_, text, isErr := mgCall(t, cs, "merge_merge_request", map[string]any{"sha": mgHead})
			if !isErr || !strings.HasPrefix(text, tc.want) {
				t.Fatalf("isErr=%v text=%q, want prefix %q", isErr, text, tc.want)
			}
			if tc.exact && text != tc.want {
				t.Fatalf("refusal must be flat, got %q", text) // no author name or id
			}
			if f.count("PUT /merge") != 0 {
				t.Fatalf("a refused merge wrote %d time(s)", f.count("PUT /merge"))
			}
		})
	}
}

// Other profiles keep today's merge: no MR read before the write, no user
// lookup, an MR of another author merges.
func TestMerge_otherProfilesDoNotCheckTheAuthor(t *testing.T) {
	for name, cfg := range map[string]*config.Config{
		"default": {}, "daily": {UseDailyTools: true},
	} {
		t.Run(name, func(t *testing.T) {
			cs, f := mgSession(t, cfg, map[string][]mgResp{
				"GET ":       {{mgOK, mgMRBy(8, "merged")}},
				mgUser:       {{mgOK, mgMeBody}},
				"PUT /merge": {{mgOK, mgMRBy(8, "merged")}},
			})
			out, text, isErr := mgCall(t, cs, "merge_merge_request", map[string]any{})
			if isErr || out["result"] != "merged" {
				t.Fatalf("isErr=%v out=%v text=%q", isErr, out, text)
			}
			if f.count(mgUser) != 0 || f.count("GET ") != 0 || f.total != 1 {
				t.Fatalf("requests %v, want the single PUT only", f.hits)
			}
		})
	}
}

func TestMerge_descriptionMentionsOwnMRRuleInReviewProfile(t *testing.T) {
	desc := func(cfg *config.Config) string {
		cs, _ := mgSession(t, cfg, nil)
		lt, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range lt.Tools {
			if x.Name == "merge_merge_request" {
				return x.Description
			}
		}
		t.Fatal("merge_merge_request not registered")
		return ""
	}
	if d := desc(review()); !strings.Contains(d, "authored by the current user") {
		t.Fatalf("review description: %q", d)
	}
	if d := desc(&config.Config{}); strings.Contains(d, "authored by the current user") {
		t.Fatalf("default description must not mention the rule: %q", d)
	}
}
