package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

func shaN(n int) string { return fmt.Sprintf("%040x", n) }

type pathLog struct {
	mu    sync.Mutex
	paths []string
}

func (p *pathLog) add(path, rawQuery string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.paths = append(p.paths, path+"?"+rawQuery)
	p.mu.Unlock()
}

func (p *pathLog) count(substr string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, path := range p.paths {
		if strings.Contains(path, substr) {
			n++
		}
	}
	return n
}

func (p *pathLog) snapshot() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.paths)
}

type reviewScript struct {
	log                 *pathLog
	detailN             map[int64]int
	branchN             map[string]int
	approval            int
	version             string
	versionIID          int64
	driftIID            int64
	driftSHA            string
	driftHeadIID        int64
	driftHeadSHA        string
	driftBranchIID      int64
	driftVersionIID     int64
	secondTgtIID        int64
	missingBranchIID    int64
	verN                map[int64]int
	forkID              int64
	redirect            string
	cancel              context.CancelFunc
	cancelAt            int
	hits                int
	destHits            int
	redirected          bool
	abort               bool
	cancelOnApproval    bool
	cancelOnDetail      int
	cancelIID           int64
	cancelAfterVersions int
	versionWrites       int
	sourceSHA           string
	targetSHA           string
	secondTgt           string
	discussions         func(http.ResponseWriter, *http.Request)
	discStatus          int
	mu                  sync.Mutex
}

func (s *reviewScript) serve(w http.ResponseWriter, r *http.Request) {
	s.log.add(r.URL.Path, r.URL.RawQuery)
	s.mu.Lock()
	if strings.Contains(r.URL.Path, "/projects/999") || strings.Contains(r.URL.Path, "/evil") {
		s.destHits++
	}
	s.hits++
	hit := s.hits
	s.mu.Unlock()
	if s.cancel != nil && s.cancelAt > 0 && hit == s.cancelAt {
		s.cancel()
		if s.abort {
			<-r.Context().Done()
			return
		}
	}
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/user"):
		_, _ = io.WriteString(w, `{"id":7}`)
	case strings.Contains(path, "/repository/branches/"):
		if s.missingBranchIID != 0 && strings.Contains(path, "/feature-"+strconv.FormatInt(s.missingBranchIID, 10)) {
			http.NotFound(w, r)
			return
		}
		s.writeBranch(w, r)
	case strings.Contains(path, "/approval_state"):
		if s.approval == 401 || s.approval == 403 || s.approval == 404 || s.approval == 405 {
			w.WriteHeader(s.approval)
			_, _ = io.WriteString(w, `{"message":"nope"}`)
			return
		}
		if s.cancelOnApproval && s.cancel != nil {
			s.cancel()
			<-r.Context().Done()
			return
		}
		if s.approval == 500 {
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, `{"rules":[]}`)
	case strings.Contains(path, "/versions"):
		s.writeVersions(w, r)
	case strings.Contains(path, "/discussions"):
		if s.discussions != nil {
			s.discussions(w, r)
			return
		}
		if s.discStatus != 0 {
			w.WriteHeader(s.discStatus)
			_, _ = io.WriteString(w, `{"message":"nope"}`)
			return
		}
		w.Header().Set("X-Next-Page", "")
		_, _ = io.WriteString(w, `[]`)
	case strings.Contains(path, "/merge_requests/"):
		s.writeDetail(w, r)
	case strings.Contains(path, "/projects/"):
		id := projectIDFromPath(path)
		fmt.Fprintf(w, `{"id":%d,"path_with_namespace":"g/p","namespace":{"id":1,"kind":"group"}}`, id)
	default:
		http.NotFound(w, r)
	}
}

func projectIDFromPath(path string) int64 {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, p := range parts {
		if p == "projects" && i+1 < len(parts) {
			id, _ := strconv.ParseInt(parts[i+1], 10, 64)
			return id
		}
	}
	return 0
}

func iidFromPath(path string) int64 {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, p := range parts {
		if p == "merge_requests" && i+1 < len(parts) {
			id, _ := strconv.ParseInt(parts[i+1], 10, 64)
			return id
		}
	}
	return 0
}

func (s *reviewScript) writeDetail(w http.ResponseWriter, r *http.Request) {
	iid := iidFromPath(r.URL.Path)
	if s.detailN == nil {
		s.detailN = map[int64]int{}
	}
	s.detailN[iid]++
	if s.cancel != nil && s.cancelOnDetail > 0 && iid == s.cancelIID && s.detailN[iid] == s.cancelOnDetail {
		s.cancel()
		<-r.Context().Done()
		return
	}
	if s.redirect != "" && s.detailN[iid] == 1 && iid == 1 {
		switch s.redirect {
		case "same":
			http.Redirect(w, r, r.URL.String(), http.StatusFound)
			return
		case "path":
			s.redirected = true
			http.Redirect(w, r, "/api/v4/projects/42/merge_requests/1/evil", http.StatusFound)
			return
		case "query":
			http.Redirect(w, r, r.URL.Path+"?injected=1", http.StatusFound)
			return
		case "origin":
			http.Redirect(w, r, "http://evil.test/api/v4/projects/42/merge_requests/1", http.StatusFound)
			return
		case "project":
			http.Redirect(w, r, "/api/v4/projects/999/merge_requests/1", http.StatusFound)
			return
		}
	}
	head := shaN(int(iid))
	base := shaN(100 + int(iid))
	start := shaN(200 + int(iid))
	source := int64(42)
	if s.forkID > 0 {
		source = s.forkID
	}
	if s.driftIID == iid && s.detailN[iid] >= 2 {
		head = s.driftSHA
		source = 999
	}
	if s.driftHeadIID == iid && s.detailN[iid] >= 2 && s.driftHeadSHA != "" {
		head = s.driftHeadSHA
	}
	sourceBranch := fmt.Sprintf("feature-%d", iid)
	if s.driftBranchIID == iid && s.detailN[iid] >= 2 {
		sourceBranch = "renamed"
	}
	fmt.Fprintf(w, `{"id":%d,"iid":%d,"project_id":42,"source_project_id":%d,"target_project_id":42,"source_branch":%q,"target_branch":"main-%d","sha":%q,"diff_refs":{"base_sha":%q,"head_sha":%q,"start_sha":%q}}`,
		5000+iid, iid, source, sourceBranch, iid, head, base, head, start)
}

func (s *reviewScript) writeBranch(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	if s.branchN == nil {
		s.branchN = map[string]int{}
	}
	s.branchN[name]++
	var iid int64 = 1
	if i := strings.Index(name, "-"); i >= 0 {
		iid, _ = strconv.ParseInt(name[i+1:], 10, 64)
	}
	var sha string
	if strings.HasPrefix(name, "main-") {
		sha = s.targetSHA
		if sha == "" {
			sha = shaN(300 + int(iid))
		}
		if s.secondTgt != "" && s.secondTgtIID == iid && s.branchN[name] >= 2 {
			sha = s.secondTgt
		}
	} else {
		sha = s.sourceSHA
		if sha == "" {
			sha = shaN(int(iid))
		}
	}
	fmt.Fprintf(w, `{"name":%q,"commit":{"id":%q}}`, name, sha)
}

func (s *reviewScript) writeVersions(w http.ResponseWriter, r *http.Request) {
	iid := iidFromPath(r.URL.Path)
	if r.URL.Query().Get("per_page") != "20" {
		http.Error(w, "per_page", http.StatusBadRequest)
		return
	}
	head := shaN(int(iid))
	base := shaN(100 + int(iid))
	start := shaN(200 + int(iid))
	mode := s.version
	if s.versionIID != 0 && iid != s.versionIID {
		mode = ""
	}
	switch mode {
	case "empty":
		w.Header().Set("X-Next-Page", "")
		_, _ = io.WriteString(w, `[]`)
	case "next":
		w.Header().Set("X-Next-Page", "2")
		fmt.Fprintf(w, `[{"id":5,"merge_request_id":%d,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q}]`, 5000+iid, head, base, start)
	case "multi":
		w.Header().Set("X-Next-Page", "")
		fmt.Fprintf(w, `[{"id":5,"merge_request_id":%d,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q},{"id":6,"merge_request_id":%d,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q}]`,
			5000+iid, head, base, start, 5000+iid, head, base, start)
	case "head-mismatch":
		w.Header().Set("X-Next-Page", "")
		fmt.Fprintf(w, `[{"id":5,"merge_request_id":%d,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q}]`, 5000+iid, head, shaN(9), start)
	case "unknown-paging":
		fmt.Fprintf(w, `[{"id":5,"merge_request_id":%d,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q}]`, 5000+iid, head, base, start)
	case "malformed-plus-match":
		w.Header().Set("X-Next-Page", "")
		other := shaN(9)
		fmt.Fprintf(w, `[{},{"id":%d,"merge_request_id":%d,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q},{"id":8,"merge_request_id":%d,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q}]`,
			7000+iid, 5000+iid, head, base, start, 5000+iid, other, other, other)
	case "dup-next":
		w.Header().Add("X-Next-Page", "")
		w.Header().Add("X-Next-Page", "2")
		fmt.Fprintf(w, `[{"id":%d,"merge_request_id":%d,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q}]`, 7000+iid, 5000+iid, head, base, start)
	case "repeat-next":
		w.Header().Add("X-Next-Page", "2")
		w.Header().Add("X-Next-Page", "2")
		fmt.Fprintf(w, `[{"id":%d,"merge_request_id":%d,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q}]`, 7000+iid, 5000+iid, head, base, start)
	case "ws-next":
		w.Header().Set("X-Next-Page", " 2")
		fmt.Fprintf(w, `[{"id":%d,"merge_request_id":%d,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q}]`, 7000+iid, 5000+iid, head, base, start)
	case "bad-next":
		w.Header().Set("X-Next-Page", "next")
		fmt.Fprintf(w, `[{"id":%d,"merge_request_id":%d,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q}]`, 7000+iid, 5000+iid, head, base, start)
	case "pad-next":
		w.Header().Set("X-Next-Page", "02")
		fmt.Fprintf(w, `[{"id":%d,"merge_request_id":%d,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q}]`, 7000+iid, 5000+iid, head, base, start)
	case "back-next":
		w.Header().Set("X-Next-Page", "1")
		fmt.Fprintf(w, `[{"id":%d,"merge_request_id":%d,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q}]`, 7000+iid, 5000+iid, head, base, start)
	case "jump-next":
		w.Header().Set("X-Next-Page", "3")
		fmt.Fprintf(w, `[{"id":%d,"merge_request_id":%d,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q}]`, 7000+iid, 5000+iid, head, base, start)
	case "historical-plus-match":
		w.Header().Set("X-Next-Page", "")
		other := shaN(9)
		fmt.Fprintf(w, `[{"id":4,"merge_request_id":%d,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q},{"id":%d,"merge_request_id":%d,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q}]`,
			5000+iid, other, other, other, 7000+iid, 5000+iid, head, base, start)
	default:
		w.Header().Set("X-Next-Page", "")
		if s.verN == nil {
			s.verN = map[int64]int{}
		}
		s.verN[iid]++
		verID := int64(7000 + iid)
		if s.driftVersionIID == iid && s.verN[iid] >= 2 {
			verID = 8000 + iid
		}
		fmt.Fprintf(w, `[{"id":%d,"merge_request_id":%d,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q}]`, verID, 5000+iid, head, base, start)
	}
	s.versionWrites++
	if s.cancel != nil && s.cancelAfterVersions > 0 && s.versionWrites == s.cancelAfterVersions {
		s.cancel()
	}
}

func newReviewDeps(t *testing.T, h http.Handler) Deps {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg := &config.Config{Token: "test-token", APIURL: srv.URL + "/api/v4", CursorKey: bytes.Repeat([]byte("k"), 32)}
	cli, err := igl.NewClient(cfg, igl.WithRetryWaitMinMax(0, 0))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return Deps{Config: cfg, Client: cli, Clock: &cursor.FakeClock{T: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}}
}

func callReviewContext(t *testing.T, d Deps, ctx context.Context, args map[string]any, streamable bool) (map[string]any, error) {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "review-context", Version: "t"}, nil)
	RegisterMergeRequests(srv, d)
	var cs *mcp.ClientSession
	if streamable {
		cs = testutil.StreamableMCPConnect(t, srv)
	} else {
		cs = testutil.MCPConnect(t, srv)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: cursor.ToolReviewContext, Arguments: args})
	if err != nil {
		return nil, err
	}
	if res.IsError {
		return nil, confToolErr(res)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(toolJSON(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	return out, nil
}

func metaItem(project string, iid int64, sections ...string) reviewContextItemIn {
	return reviewContextItemIn{ProjectID: project, MergeRequestIID: iid, Sections: sections}
}

func callReviewDirect(t *testing.T, d Deps, ctx context.Context, items []reviewContextItemIn) (reviewContextOut, error) {
	t.Helper()
	_, raw, err := getMergeRequestReviewContext(ctx, nil, getMergeRequestReviewContextIn{Items: items}, d)
	if err != nil {
		return reviewContextOut{}, err
	}
	out, ok := raw.(reviewContextOut)
	if !ok {
		t.Fatalf("result type %T", raw)
	}
	return out, nil
}

func reviewBudget(maxRequests int) *igl.Budget {
	b := igl.DefaultBudget()
	b.MaxRequests = maxRequests
	b.MaxItems = 1000
	b.MaxBytes = 8 << 20
	b.MaxElapsed = 30 * time.Second
	return b
}

func itemArg(project string, iid int64, sections []any, extra map[string]any) map[string]any {
	m := map[string]any{"project_id": project, "merge_request_iid": iid, "sections": sections}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func itemsOf(t *testing.T, out map[string]any) []any {
	t.Helper()
	items, _ := out["items"].([]any)
	if items == nil {
		t.Fatalf("no items: %#v", out)
	}
	return items
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("map: %#v", v)
	}
	return m
}

func TestReviewContext_registeredMatrix(t *testing.T) {
	t.Run("ten mixed and streamable control", func(t *testing.T) {
		log := &pathLog{}
		script := &reviewScript{log: log, approval: 500}
		d := newReviewDeps(t, http.HandlerFunc(script.serve))
		var items []any
		for i := int64(1); i <= 9; i++ {
			items = append(items, itemArg("42", i, []any{"metadata"}, nil))
		}
		items = append(items, itemArg("42", 10, []any{"metadata", "approvals"}, nil))
		out, err := callReviewContext(t, d, nil, map[string]any{"items": items}, false)
		if err != nil {
			t.Fatal(err)
		}
		if out["atomic_snapshot"] != false || out["signature_attests_review"] != false {
			t.Fatalf("aggregate claim: %#v", out)
		}
		got := itemsOf(t, out)
		if len(got) != 10 {
			t.Fatalf("len %d", len(got))
		}
		for i, raw := range got[:9] {
			item := asMap(t, raw)
			if item["context_ref"] == nil || item["context_ref"] == "" {
				t.Fatalf("item %d ref: %#v", i, item)
			}
		}
		last := asMap(t, got[9])
		if last["context_ref"] == nil {
			t.Fatalf("metadata-only mask missing ref: %#v", last)
		}
		if last["approvals"] == nil {
			t.Fatal("500 approvals skeleton missing")
		}
		approvalShapeKeys(t, asMap(t, last["approvals"]))
		if asMap(t, last["approvals"])["approved"] != nil || asMap(t, last["approvals"])["rules"] != nil {
			t.Fatalf("500 success fields: %#v", last["approvals"])
		}
		secs := asMap(t, last["sections"])
		ap := asMap(t, secs["approvals"])
		if ap["content_complete"] == readmeta.ContentCompleteTrue {
			t.Fatal("approval section marked complete")
		}
		if !strings.Contains(fmt.Sprint(ap["limitations"]), readmeta.CodeHTTPError) {
			t.Fatalf("approval section: %#v", ap)
		}
		ref := last["context_ref"].(string)
		payload, err := cursor.Decode(d.Config.CursorKey, ref, time.Date(2026, 10, 4, 12, 30, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		stored := payload.ContextRef
		live := cursor.ReviewLiveRefs{
			OwnerProjectID: stored.OwnerProjectID, SourceProjectID: stored.SourceProjectID, TargetProjectID: stored.TargetProjectID,
			SourceBranch: stored.SourceBranch, TargetBranch: stored.TargetBranch,
			SourceSHA: stored.SourceSHA, TargetSHA: stored.TargetSHA, VersionID: stored.VersionID,
			VersionHead: stored.VersionHead, VersionBase: stored.VersionBase, VersionStart: stored.VersionStart,
		}
		if err := cursor.VerifyContextBinding(payload, payload.Instance, payload.ActorID, payload.PolicyFP, payload.Tool, payload.Section, payload.Scope, payload.Filters, payload.UpperBound, live, []string{"approvals"}); err == nil {
			t.Fatal("metadata token accepted approvals demand")
		}
		dig1, _ := last["approval_digest"].(string)
		if dig1 == "" {
			t.Fatal("approval digest missing")
		}
		if log.count("/user") != 1 {
			t.Fatalf("user calls %d", log.count("/user"))
		}
		d.Clock = &cursor.FakeClock{T: time.Date(2026, 10, 4, 18, 0, 0, 0, time.UTC)}
		later, err := callReviewContext(t, d, nil, map[string]any{"items": []any{itemArg("42", 10, []any{"metadata", "approvals"}, nil)}}, false)
		if err != nil {
			t.Fatal(err)
		}
		if asMap(t, itemsOf(t, later)[0])["approval_digest"] != dig1 {
			t.Fatal("approval digest changed with the clock")
		}
		out2, err := callReviewContext(t, d, nil, map[string]any{"items": []any{itemArg("42", 1, []any{"metadata"}, nil)}}, true)
		if err != nil {
			t.Fatal(err)
		}
		if itemsOf(t, out2)[0].(map[string]any)["context_ref"] == nil {
			t.Fatal("streamable control")
		}
	})

	t.Run("local rejects make no http", func(t *testing.T) {
		log := &pathLog{}
		d := newReviewDeps(t, http.HandlerFunc((&reviewScript{log: log}).serve))
		cases := []map[string]any{
			{"items": []any{}},
			{"items": make([]any, 11)},
			{"items": []any{itemArg("42", 0, []any{"metadata"}, nil)}},
			{"items": []any{itemArg("42", 1, []any{"pipeline"}, nil)}},
			{"items": []any{itemArg("42", 1, []any{"diff"}, nil)}},
			{"items": []any{itemArg("42", 1, []any{"metadata"}, map[string]any{"expected_head": "zz"})}},
			{"max_requests": 0, "items": []any{itemArg("42", 1, []any{"metadata"}, nil)}},
			{"max_items": 0, "items": []any{itemArg("42", 1, []any{"metadata"}, nil)}},
			{"items": []any{itemArg("42", 1, []any{"metadata"}, map[string]any{"cursors": []any{map[string]any{"section": "metadata", "cursor": "  x"}}})}},
		}
		for i, args := range cases {
			before := log.snapshot()
			if _, err := callReviewContext(t, d, nil, args, false); err == nil {
				t.Fatalf("case %d accepted %#v", i, args)
			}
			if log.snapshot() != before {
				t.Fatalf("case %d performed http", i)
			}
		}
		d.Config.CursorKey = nil
		if _, err := callReviewContext(t, d, nil, map[string]any{"items": []any{itemArg("42", 1, []any{"metadata"}, nil)}}, false); err == nil || !strings.Contains(err.Error(), "GITLAB_MCP_CURSOR_KEY") {
			t.Fatalf("missing key: %v", err)
		}
	})

	t.Run("denied fork has no content path", func(t *testing.T) {
		log := &pathLog{}
		script := &reviewScript{log: log, forkID: 99}
		d := newReviewDeps(t, http.HandlerFunc(script.serve))
		d.Config.AllowedProjectIDs = []string{"42"}
		out, err := callReviewContext(t, d, nil, map[string]any{"items": []any{itemArg("42", 1, []any{"metadata"}, nil)}}, false)
		if err != nil {
			t.Fatal(err)
		}
		item := asMap(t, itemsOf(t, out)[0])
		if item["context_ref"] != nil || item["cause"] != readmeta.CodeAuthzDenied {
			t.Fatalf("%#v", item)
		}
		if log.count("/repository/branches/") != 0 || log.count("/versions") != 0 || log.count("/approval_state") != 0 {
			t.Fatalf("content paths: %+v", log.paths)
		}
	})

	t.Run("unproven source", func(t *testing.T) {
		log := &pathLog{}
		d := newReviewDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.add(r.URL.Path, r.URL.RawQuery)
			if strings.HasSuffix(r.URL.Path, "/user") {
				_, _ = io.WriteString(w, `{"id":7}`)
				return
			}
			if strings.Contains(r.URL.Path, "/merge_requests/") {
				_, _ = io.WriteString(w, `{"id":1,"iid":1,"project_id":42,"sha":"`+shaN(9)+`"}`)
				return
			}
			if strings.Contains(r.URL.Path, "/projects/") {
				_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":1,"kind":"group"}}`)
				return
			}
			http.NotFound(w, r)
		}))
		out, err := callReviewContext(t, d, nil, map[string]any{"items": []any{itemArg("42", 1, []any{"metadata"}, nil)}}, false)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(out)
		if bytes.Contains(raw, []byte(shaN(9))) || bytes.Contains(raw, []byte(`"context_ref":"`)) {
			t.Fatalf("invented identity: %s", raw)
		}
		if log.count("/repository/branches/") != 0 {
			t.Fatal("followed unproven source")
		}
	})

	t.Run("expected head and sibling drift", func(t *testing.T) {
		log := &pathLog{}
		script := &reviewScript{log: log, driftIID: 2, driftSHA: shaN(77)}
		d := newReviewDeps(t, http.HandlerFunc(script.serve))
		out, err := callReviewContext(t, d, nil, map[string]any{"items": []any{
			itemArg("42", 1, []any{"metadata"}, map[string]any{"expected_head": shaN(9)}),
			itemArg("42", 2, []any{"metadata"}, nil),
		}}, false)
		if err != nil {
			t.Fatal(err)
		}
		first := asMap(t, itemsOf(t, out)[0])
		second := asMap(t, itemsOf(t, out)[1])
		if first["context_ref"] == nil || first["review_clean"] != false || first["expected_head_match"] != false {
			t.Fatalf("expected head: %#v", first)
		}
		if second["context_ref"] != nil {
			t.Fatalf("drift minted: %#v", second)
		}
		raw, _ := json.Marshal(second)
		if bytes.Contains(raw, []byte(shaN(77))) || bytes.Contains(raw, []byte("999")) {
			t.Fatalf("echoed drift: %s", raw)
		}
		if log.count("/projects/999") != 0 {
			t.Fatal("followed drifted project")
		}
		if first["context_ref"] == nil {
			t.Fatal("sibling ref cleared")
		}
	})

	t.Run("version match outcomes", func(t *testing.T) {
		for _, mode := range []string{"empty", "next", "multi", "head-mismatch", "unknown-paging"} {
			log := &pathLog{}
			d := newReviewDeps(t, http.HandlerFunc((&reviewScript{log: log, version: mode}).serve))
			out, err := callReviewContext(t, d, nil, map[string]any{"items": []any{itemArg("42", 1, []any{"metadata"}, nil)}}, false)
			if err != nil {
				t.Fatalf("%s: %v", mode, err)
			}
			item := asMap(t, itemsOf(t, out)[0])
			if item["context_ref"] != nil {
				t.Fatalf("%s minted %#v", mode, item)
			}
			if strings.Contains(strings.Join(log.paths, "\n"), "?page=") || strings.Contains(strings.Join(log.paths, "\n"), "&page=") {
				t.Fatalf("%s requested another page: %v", mode, log.paths)
			}
			if mode == "head-mismatch" && item["observational_consistency"] != readmeta.ConsistencyInconsistent && item["cause"] != readmeta.CodeInconsistent {
				t.Fatalf("%s cause %#v", mode, item)
			}
		}
	})

	t.Run("preconsumed budget keeps counters", func(t *testing.T) {
		log := &pathLog{}
		d := newReviewDeps(t, http.HandlerFunc((&reviewScript{log: log}).serve))
		parent, parentCancel := context.WithCancel(context.Background())
		defer parentCancel()
		b := igl.DefaultBudget()
		b.MaxRequests = 1
		ctx := igl.WithBudget(parent, b)
		req, err := d.Client.NewRequest(http.MethodGet, "user", nil, []gitlab.RequestOptionFunc{gitlab.WithContext(ctx)})
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if _, err := d.Client.Do(req, &buf); err != nil {
			t.Fatal(err)
		}
		reqs, _, _ := b.Stats()
		before := log.snapshot()
		_, _, err = getMergeRequestReviewContext(ctx, nil, getMergeRequestReviewContextIn{
			Items: []reviewContextItemIn{{ProjectID: "42", MergeRequestIID: 1, Sections: []string{"metadata"}}},
		}, d)
		if err == nil || !strings.Contains(err.Error(), readmeta.CodeBudgetRequests) {
			t.Fatalf("preflight: %v", err)
		}
		got, _, _ := b.Stats()
		if got != reqs || log.snapshot() != before {
			t.Fatalf("counters %d->%d hits %d->%d", reqs, got, before, log.snapshot())
		}
		if parent.Err() != nil || ctx.Err() != nil {
			t.Fatal("borrowed budget cancel was invoked")
		}
	})

	t.Run("redirect refusal before destination", func(t *testing.T) {
		for _, mode := range []string{"path", "query", "origin", "project"} {
			log := &pathLog{}
			script := &reviewScript{log: log, redirect: mode}
			d := newReviewDeps(t, http.HandlerFunc(script.serve))
			out, err := callReviewContext(t, d, nil, map[string]any{"items": []any{itemArg("42", 1, []any{"metadata"}, nil)}}, false)
			if err != nil {
				t.Fatalf("%s: %v", mode, err)
			}
			item := asMap(t, itemsOf(t, out)[0])
			if item["context_ref"] != nil {
				t.Fatalf("%s minted dest=%d paths=%v", mode, script.destHits, log.paths)
			}
			if script.destHits != 0 {
				t.Fatalf("%s destination hits %d", mode, script.destHits)
			}
		}
		plainLog := &pathLog{}
		plainDeps := newReviewDeps(t, http.HandlerFunc((&reviewScript{log: plainLog}).serve))
		plainBudget := reviewBudget(128)
		plainOut, err := callReviewDirect(t, plainDeps, igl.WithBudget(context.Background(), plainBudget), []reviewContextItemIn{metaItem("42", 1, "metadata")})
		if err != nil || plainOut.Items[0].ContextRef == nil {
			t.Fatalf("baseline: %v", err)
		}
		baseReqs, _, _ := plainBudget.Stats()
		log := &pathLog{}
		d := newReviewDeps(t, http.HandlerFunc((&reviewScript{log: log, redirect: "same"}).serve))
		b := reviewBudget(128)
		out, err := callReviewDirect(t, d, igl.WithBudget(context.Background(), b), []reviewContextItemIn{metaItem("42", 1, "metadata")})
		if err != nil || out.Items[0].ContextRef == nil {
			t.Fatalf("same-url hop was not followed: %v", err)
		}
		reqs, _, _ := b.Stats()
		if reqs != baseReqs+1 || log.count("/merge_requests/1?") < 3 {
			t.Fatalf("same-url hop not charged: requests=%d baseline=%d detail=%d", reqs, baseReqs, log.count("/merge_requests/1?"))
		}
	})

	t.Run("budget and cancel crossings keep the sibling ref", func(t *testing.T) {
		measureLog := &pathLog{}
		measureDeps := newReviewDeps(t, http.HandlerFunc((&reviewScript{log: measureLog}).serve))
		measured := reviewBudget(128)
		measuredOut, err := callReviewDirect(t, measureDeps, igl.WithBudget(context.Background(), measured), []reviewContextItemIn{metaItem("42", 1, "metadata")})
		if err != nil || measuredOut.Items[0].ContextRef == nil {
			t.Fatalf("measure: %v", err)
		}
		oneReqs, _, _ := measured.Stats()
		shared := measureLog.count("/user") + measureLog.count("/projects/42?")
		itemReads := oneReqs - shared
		if measureLog.count("/versions") != 2 || itemReads < 4 || itemReads%2 != 0 {
			t.Fatalf("measure requests=%d shared=%d versions=%d", oneReqs, shared, measureLog.count("/versions"))
		}

		t.Run("proof", func(t *testing.T) {
			log := &pathLog{}
			d := newReviewDeps(t, http.HandlerFunc((&reviewScript{log: log}).serve))
			parent, parentCancel := context.WithCancel(context.Background())
			defer parentCancel()
			b := reviewBudget(1)
			out, err := callReviewDirect(t, d, igl.WithBudget(parent, b), []reviewContextItemIn{metaItem("42", 1, "metadata")})
			if err != nil || len(out.Items) != 1 || out.Items[0].Cause != readmeta.CodeBudgetRequests || out.Items[0].ContextRef != nil {
				t.Fatalf("proof budget: err=%v item=%+v", err, out.Items)
			}
			if log.count("/merge_requests/") != 0 || log.count("/repository/branches/") != 0 {
				t.Fatalf("proof reached content: %v", log.paths)
			}
			if parent.Err() != nil {
				t.Fatal("parent cancel invoked")
			}
		})

		t.Run("final bracket", func(t *testing.T) {
			log := &pathLog{}
			d := newReviewDeps(t, http.HandlerFunc((&reviewScript{log: log}).serve))
			b := reviewBudget(oneReqs + itemReads/2)
			out, err := callReviewDirect(t, d, igl.WithBudget(context.Background(), b), []reviewContextItemIn{
				metaItem("42", 1, "metadata"),
				metaItem("42", 2, "metadata"),
			})
			if err != nil || len(out.Items) != 2 {
				t.Fatal(err)
			}
			if out.Items[0].ContextRef == nil || out.Items[1].ContextRef != nil || out.Items[1].Cause != readmeta.CodeBudgetRequests {
				t.Fatalf("final bracket %#v %#v", out.Items[0].Cause, out.Items[1])
			}
			if log.count("/merge_requests/2/versions") != 1 {
				t.Fatalf("final bracket continued: %v", log.paths)
			}
		})

		t.Run("pre-encode", func(t *testing.T) {
			log := &pathLog{}
			d := newReviewDeps(t, http.HandlerFunc((&reviewScript{log: log}).serve))
			b := reviewBudget(oneReqs + itemReads)
			out, err := callReviewDirect(t, d, igl.WithBudget(context.Background(), b), []reviewContextItemIn{
				metaItem("42", 1, "metadata"),
				metaItem("42", 2, "metadata"),
			})
			if err != nil || len(out.Items) != 2 {
				t.Fatal(err)
			}
			if out.Items[0].ContextRef == nil || out.Items[1].ContextRef != nil || out.Items[1].Cause != readmeta.CodeBudgetRequests || out.Items[1].Metadata == nil {
				t.Fatalf("pre-encode item0 ref nil=%v item1=%+v", out.Items[0].ContextRef == nil, out.Items[1])
			}
		})

		t.Run("cancel proof", func(t *testing.T) {
			log := &pathLog{}
			parent, cancel := context.WithCancel(context.Background())
			script := &reviewScript{log: log, cancel: cancel, cancelAt: 2, abort: true}
			d := newReviewDeps(t, http.HandlerFunc(script.serve))
			out, err := callReviewDirect(t, d, igl.WithBudget(parent, reviewBudget(128)), []reviewContextItemIn{metaItem("42", 1, "metadata")})
			if err != nil || out.Items[0].Cause != readmeta.CodeCancelled || out.Items[0].ContextRef != nil {
				t.Fatalf("cancel proof: err=%v cause=%s", err, out.Items[0].Cause)
			}
			if log.count("/merge_requests/") != 0 {
				t.Fatalf("cancel proof reached content: %v", log.paths)
			}
		})

		t.Run("cancel section", func(t *testing.T) {
			log := &pathLog{}
			parent, cancel := context.WithCancel(context.Background())
			script := &reviewScript{log: log, cancel: cancel, cancelOnApproval: true}
			d := newReviewDeps(t, http.HandlerFunc(script.serve))
			out, err := callReviewDirect(t, d, igl.WithBudget(parent, reviewBudget(128)), []reviewContextItemIn{
				metaItem("42", 1, "metadata"),
				metaItem("42", 2, "metadata", "approvals"),
			})
			if err != nil || out.Items[0].ContextRef == nil || out.Items[1].ContextRef != nil || out.Items[1].Cause != readmeta.CodeCancelled {
				t.Fatalf("cancel section: err=%v causes %s %s", err, out.Items[0].Cause, out.Items[1].Cause)
			}
		})

		t.Run("cancel final bracket", func(t *testing.T) {
			log := &pathLog{}
			parent, cancel := context.WithCancel(context.Background())
			script := &reviewScript{log: log, cancel: cancel, cancelOnDetail: 2, cancelIID: 2}
			d := newReviewDeps(t, http.HandlerFunc(script.serve))
			out, err := callReviewDirect(t, d, igl.WithBudget(parent, reviewBudget(128)), []reviewContextItemIn{
				metaItem("42", 1, "metadata"),
				metaItem("42", 2, "metadata"),
			})
			if err != nil || out.Items[0].ContextRef == nil || out.Items[1].ContextRef != nil || out.Items[1].Cause != readmeta.CodeCancelled {
				t.Fatalf("cancel bracket: err=%v item1=%+v", err, out.Items[1])
			}
		})

		t.Run("cancel pre-encode", func(t *testing.T) {
			log := &pathLog{}
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			var proved int
			ctx := withReviewMintHook(igl.WithBudget(parent, reviewBudget(128)), func() {
				proved++
				if proved == 2 {
					cancel()
				}
			})
			d := newReviewDeps(t, http.HandlerFunc((&reviewScript{log: log}).serve))
			out, err := callReviewDirect(t, d, ctx, []reviewContextItemIn{
				metaItem("42", 1, "metadata"),
				metaItem("42", 2, "metadata"),
			})
			if err != nil || out.Items[0].ContextRef == nil || out.Items[1].ContextRef != nil || out.Items[1].Cause != readmeta.CodeCancelled || out.Items[1].Metadata == nil {
				t.Fatalf("cancel pre-encode: err=%v item1=%+v", err, out.Items[1])
			}
		})
	})

	t.Run("approval success stays incomplete", func(t *testing.T) {
		log := &pathLog{}
		d := newReviewDeps(t, http.HandlerFunc((&reviewScript{log: log}).serve))
		out, err := callReviewContext(t, d, nil, map[string]any{"items": []any{itemArg("42", 1, []any{"metadata", "approvals", "discussions", "pipeline_graph", "diff_manifest"}, nil)}}, false)
		if err != nil {
			t.Fatal(err)
		}
		item := asMap(t, itemsOf(t, out)[0])
		if item["context_ref"] == nil || item["approvals"] == nil || item["approval_digest"] == nil {
			t.Fatalf("success shape: %#v", item)
		}
		ap := asMap(t, asMap(t, item["sections"])["approvals"])
		if ap["content_complete"] == readmeta.ContentCompleteTrue || ap["consistency"] == readmeta.ConsistencyConsistent {
			t.Fatalf("approval promoted: %#v", ap)
		}
		for _, name := range []string{"diff_manifest"} {
			sec := asMap(t, asMap(t, item["sections"])[name])
			if sec["content_complete"] != readmeta.ContentCompleteUnknown || sec["head_sha"] != nil || sec["next_cursor"] != nil {
				t.Fatalf("%s filler: %#v", name, sec)
			}
		}
		pg := asMap(t, asMap(t, item["sections"])["pipeline_graph"])
		if pg["content_complete"] == readmeta.ContentCompleteTrue {
			t.Fatalf("graph without CI fixtures must stay incomplete: %#v", pg)
		}
		disc := asMap(t, asMap(t, item["sections"])["discussions"])
		if disc["content_complete"] == readmeta.ContentCompleteTrue || disc["next_cursor"] != nil {
			t.Fatalf("semantic discussions must not be complete evidence: %#v", disc)
		}
		if log.count("/discussions") != 1 || log.count("/approval_state") != 1 {
			t.Fatalf("approval calls %d", log.count("/approval_state"))
		}
	})

	t.Run("typed byte and elapsed budgets", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			code string
			edit func(*igl.Budget)
		}{
			{"bytes", readmeta.CodeBudgetBytes, func(b *igl.Budget) { b.MaxBytes = 1 }},
			{"elapsed", readmeta.CodeBudgetElapsed, func(b *igl.Budget) { b.MaxElapsed = time.Nanosecond }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				log := &pathLog{}
				d := newReviewDeps(t, http.HandlerFunc((&reviewScript{log: log}).serve))
				b := reviewBudget(128)
				tc.edit(b)
				_, err := callReviewDirect(t, d, igl.WithBudget(context.Background(), b), []reviewContextItemIn{metaItem("42", 1, "metadata", "discussions")})
				if err == nil || !strings.Contains(err.Error(), tc.code) {
					t.Fatalf("%s: %v", tc.name, err)
				}
			})
		}
	})

	t.Run("cursor semantic rejects do no http", func(t *testing.T) {
		log := &pathLog{}
		d := newReviewDeps(t, http.HandlerFunc((&reviewScript{log: log}).serve))
		key := d.Config.CursorKey
		legacy := cursor.Payload{
			SchemaVersion: cursor.SchemaV1, Instance: "https://gitlab.example/api/v4", ActorID: 7, PolicyFP: "p",
			Tool: cursor.ToolListCommits, Section: cursor.SectionListCommits,
			Scope:   cursor.Scope{Kind: cursor.ScopeProject, ProjectID: "42"},
			Filters: cursor.Filters{PerPage: 1, Selection: "list_commits"}, ImmutableRefs: []string{shaN(1)},
			UpperBound: "2026-10-04T12:00:00Z", ExpiresAt: "2026-10-04T16:00:00Z",
			PageState: cursor.PageState{Page: 1, PerPage: 1, ItemsOnPage: 1, LastSHA: shaN(1), SequenceDigest: strings.Repeat("ab", 32)},
		}
		tok, err := cursor.Encode(key, legacy)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cursor.Decode(key, tok, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)); err != nil {
			t.Fatalf("foreign token expired or inauthentic: %v", err)
		}
		before := log.snapshot()
		if _, err := callReviewContext(t, d, nil, map[string]any{"items": []any{itemArg("42", 1, []any{"metadata"}, map[string]any{"cursors": []any{map[string]any{"section": "metadata", "cursor": tok}}})}}, false); err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
			t.Fatalf("valid foreign cursor: %v", err)
		}
		bad := tok[:len(tok)-1] + "A"
		if _, err := callReviewContext(t, d, nil, map[string]any{"items": []any{itemArg("42", 1, []any{"metadata"}, map[string]any{"cursors": []any{map[string]any{"section": "metadata", "cursor": bad}}})}}, false); err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
			t.Fatalf("corrupt: %v", err)
		}
		if log.snapshot() != before {
			t.Fatal("cursor reject performed http")
		}
	})
}
