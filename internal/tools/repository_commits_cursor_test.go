package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

const (
	testCursorKey = "0123456789abcdef0123456789abcdef" // 32 bytes
	tipSHA        = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sha1          = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	sha2          = "cccccccccccccccccccccccccccccccccccccccc"
	sha3          = "dddddddddddddddddddddddddddddddddddddddd"
	sha4          = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	sha5          = "ffffffffffffffffffffffffffffffffffffffff"
	sha6          = "1111111111111111111111111111111111111111"
)

type commitProbe struct {
	mu          sync.Mutex
	paths       []string
	userHits    atomic.Int64
	projectHits atomic.Int64
	commitHits  atomic.Int64
	listHits    atomic.Int64
	getCommit   atomic.Int64
	pages       map[string]int // page -> count
}

func (p *commitProbe) note(r *http.Request) {
	path := r.URL.Path
	p.mu.Lock()
	p.paths = append(p.paths, path+"?"+r.URL.RawQuery)
	if p.pages == nil {
		p.pages = map[string]int{}
	}
	p.mu.Unlock()
	switch {
	case path == "/api/v4/user" || strings.HasSuffix(path, "/user"):
		p.userHits.Add(1)
	case strings.Contains(path, "/repository/commits/") && !strings.HasSuffix(path, "/commits"):
		p.getCommit.Add(1)
		p.commitHits.Add(1)
	case strings.HasSuffix(path, "/repository/commits"):
		p.listHits.Add(1)
		p.commitHits.Add(1)
		p.mu.Lock()
		p.pages[r.URL.Query().Get("page")]++
		p.mu.Unlock()
	case strings.Contains(path, "/projects/"):
		p.projectHits.Add(1)
	}
}

func (p *commitProbe) snapshot() (user, project, list, getC int64) {
	return p.userHits.Load(), p.projectHits.Load(), p.listHits.Load(), p.getCommit.Load()
}

func newCursorDeps(t *testing.T, h http.Handler, cfg *config.Config, clk cursor.Clock) Deps {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.Token = "t"
	cfg.APIURL = ts.URL + "/api/v4"
	if len(cfg.CursorKey) == 0 {
		cfg.CursorKey = []byte(testCursorKey)
	}
	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(cfg.APIURL),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(igl.BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	return Deps{Config: cfg, Client: cli, Clock: clk}
}

func commitJSON(id string) string {
	return fmt.Sprintf(`{"id":%q,"short_id":"abc","title":"t","message":"m","committed_date":"2026-10-01T00:00:00Z"}`, id)
}

func pageCommits(ids ...string) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, commitJSON(id))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func listCommitsHandler(probe *commitProbe, mutate func(page string, body *string, next *string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if probe != nil {
			probe.note(r)
		}
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case path == "/api/v4/user" || strings.HasSuffix(path, "/api/v4/user"):
			_, _ = io.WriteString(w, `{"id":7,"username":"alice"}`)
		case strings.Contains(path, "/repository/commits/") && r.Method == http.MethodGet:
			// GetCommit pin
			_, _ = io.WriteString(w, commitJSON(tipSHA))
		case strings.HasSuffix(path, "/repository/commits"):
			page := r.URL.Query().Get("page")
			if page == "" {
				page = "1"
			}
			var body, next string
			switch page {
			case "1":
				body, next = pageCommits(sha1, sha2), "2"
			case "2":
				body, next = pageCommits(sha3, sha4), "3"
			case "3":
				body, next = pageCommits(sha5, sha6), ""
			default:
				body, next = "[]", ""
			}
			if mutate != nil {
				mutate(page, &body, &next)
			}
			if next == "" {
				w.Header().Set("X-Next-Page", "")
			} else {
				w.Header().Set("X-Next-Page", next)
			}
			_, _ = io.WriteString(w, body)
		case strings.Contains(path, "/projects/"):
			// Path alias group/proj → 42; other/proj and numeric 99 → 99.
			id := int64(42)
			ns := "group/proj"
			if strings.Contains(path, "/projects/99") ||
				strings.Contains(path, "other%2Fproj") ||
				strings.Contains(path, "/projects/other/proj") {
				id = 99
				ns = "other/proj"
			}
			_, _ = io.WriteString(w, fmt.Sprintf(`{"id":%d,"path_with_namespace":%q,"namespace":{"id":9,"kind":"group","full_path":"group","parent_id":0}}`, id, ns))
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	})
}

// callListCommitsMCPRaw performs registered CallTool with exact args (no page/per_page insertion).
func callListCommitsMCPRaw(t *testing.T, d Deps, args map[string]any) (map[string]any, error) {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "t"}, nil)
	RegisterRepository(srv, d)
	cs := testutil.MCPConnect(t, srv)
	names := testutil.ToolNames(t, cs)
	if !names["list_commits"] {
		t.Fatal("list_commits not registered")
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_commits",
		Arguments: args,
	})
	if err != nil {
		return nil, err
	}
	if res.IsError {
		var b strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		return nil, fmt.Errorf("%s", b.String())
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out, nil
}

func callListCommitsMCP(t *testing.T, d Deps, args map[string]any) (map[string]any, error) {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	// Convenience defaults for older tests; F4 omission cases must use callListCommitsMCPRaw.
	if _, ok := args["page"]; !ok {
		args["page"] = 1
	}
	if _, ok := args["per_page"]; !ok {
		args["per_page"] = 20
	}
	return callListCommitsMCPRaw(t, d, args)
}

func TestListCommits_legacyExactUnchanged(t *testing.T) {
	probe := &commitProbe{}
	d := newCursorDeps(t, listCommitsHandler(probe, nil), &config.Config{}, nil)
	// Legacy: no use_cursor — must not hit /user
	out, err := callListCommitsMCP(t, d, map[string]any{
		"project_id": "42",
		"page":       2,
		"per_page":   10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out["pagination"].(map[string]any); !ok {
		t.Fatalf("legacy pagination missing: %#v", out)
	}
	if _, ok := out["section"]; ok {
		t.Fatal("legacy must not emit section")
	}
	u, _, list, getC := probe.snapshot()
	if u != 0 || getC != 0 {
		t.Fatalf("legacy must not call user/pin getCommit: user=%d get=%d", u, getC)
	}
	if list < 1 {
		t.Fatal("expected list")
	}
}

func TestListCommits_missingKeyActionableNoBackend(t *testing.T) {
	probe := &commitProbe{}
	cfg := &config.Config{CursorKey: nil}
	d := newCursorDeps(t, listCommitsHandler(probe, nil), cfg, nil)
	d.Config.CursorKey = nil
	_, err := callListCommitsMCP(t, d, map[string]any{
		"project_id": "42",
		"use_cursor": true,
	})
	if err == nil || !strings.Contains(err.Error(), "GITLAB_MCP_CURSOR_KEY") {
		t.Fatalf("want config error, got %v", err)
	}
	u, p, list, getC := probe.snapshot()
	if u != 0 || p != 0 || list != 0 || getC != 0 {
		t.Fatalf("missing key must not hit backend: user=%d proj=%d list=%d get=%d", u, p, list, getC)
	}
}

func TestListCommits_emptyPolicyPathAliasCanonicalBindResume(t *testing.T) {
	probe := &commitProbe{}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	// Empty allowlists (PolicyActive false) — cursor must still bind numeric 42.
	cfg := &config.Config{CursorKey: []byte(testCursorKey)}
	d := newCursorDeps(t, listCommitsHandler(probe, nil), cfg, clk)

	out1, err := callListCommitsMCP(t, d, map[string]any{
		"project_id": "group/proj",
		"use_cursor": true,
		"per_page":   2,
	})
	if err != nil {
		t.Fatal(err)
	}
	sec, _ := out1["section"].(map[string]any)
	if sec == nil {
		t.Fatal("section required")
	}
	nc, _ := sec["next_cursor"].(string)
	if nc == "" {
		t.Fatal("expected next_cursor")
	}
	payload, err := cursor.Decode([]byte(testCursorKey), nc, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if payload.Scope.ProjectID != "42" {
		t.Fatalf("cursor must bind canonical numeric id, got %q", payload.Scope.ProjectID)
	}
	u1, p1, list1, get1 := probe.snapshot()
	if u1 < 1 || p1 < 1 {
		t.Fatalf("identity before content required: user=%d project=%d", u1, p1)
	}
	if get1 < 1 || list1 < 1 {
		t.Fatalf("pin+list required: get=%d list=%d", get1, list1)
	}
	// Ensure /user and /projects appear before first list in path log
	probe.mu.Lock()
	paths := append([]string{}, probe.paths...)
	probe.mu.Unlock()
	firstUser, firstProj, firstList := -1, -1, -1
	for i, p := range paths {
		if firstUser < 0 && strings.Contains(p, "/user") {
			firstUser = i
		}
		if firstProj < 0 && strings.Contains(p, "/projects/") && !strings.Contains(p, "/repository/") {
			firstProj = i
		}
		if firstList < 0 && strings.Contains(p, "/repository/commits?") {
			firstList = i
		}
	}
	if !(firstUser >= 0 && firstProj >= 0 && firstList > firstUser && firstList > firstProj) {
		t.Fatalf("identity-before-content order violated: user=%d proj=%d list=%d paths=%v", firstUser, firstProj, firstList, paths)
	}

	// Resume with path alias again — must re-resolve to 42 and continue.
	// Strict filter repeat: same normalized per_page (and empty selection fields).
	listBefore := probe.listHits.Load()
	out2, err := callListCommitsMCP(t, d, map[string]any{
		"project_id": "group/proj",
		"cursor":     nc,
		"per_page":   2,
	})
	if err != nil {
		t.Fatal(err)
	}
	sec2, _ := out2["section"].(map[string]any)
	nc2, _ := sec2["next_cursor"].(string)
	if nc2 == "" {
		t.Fatal("page2 next_cursor")
	}
	p2, err := cursor.Decode([]byte(testCursorKey), nc2, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if p2.Scope.ProjectID != "42" || p2.ExpiresAt != payload.ExpiresAt || p2.UpperBound != payload.UpperBound {
		t.Fatalf("pin/deadline must be copied: %#v vs %#v", p2, payload)
	}
	if probe.listHits.Load() <= listBefore {
		t.Fatal("resume must list (guard+next)")
	}
}

func TestListCommits_threePageFakeClockRoundTrip(t *testing.T) {
	probe := &commitProbe{}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, listCommitsHandler(probe, nil), &config.Config{CursorKey: []byte(testCursorKey)}, clk)

	var cursors []string
	args := map[string]any{"project_id": "42", "use_cursor": true, "per_page": 2}
	for page := 1; page <= 3; page++ {
		out, err := callListCommitsMCP(t, d, args)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		sec := out["section"].(map[string]any)
		if page < 3 {
			nc, _ := sec["next_cursor"].(string)
			if nc == "" {
				t.Fatalf("page %d missing cursor", page)
			}
			cursors = append(cursors, nc)
			// N1: advance wall clock between pages — absolute expiry/upper bound must not slide.
			clk.T = clk.T.Add(30 * time.Minute)
			args = map[string]any{"project_id": "42", "cursor": nc, "per_page": 2}
		} else {
			if sec["next_cursor"] != nil {
				t.Fatalf("last page should null cursor: %#v", sec["next_cursor"])
			}
			if sec["pagination_exhausted"] != true {
				t.Fatal("expected exhausted")
			}
			if sec["content_complete"] != readmeta.ContentCompleteUnknown {
				t.Fatalf("exhaustion ≠ complete: %v", sec["content_complete"])
			}
		}
	}
	p0, err := cursor.Decode([]byte(testCursorKey), cursors[0], time.Date(2026, 10, 3, 12, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	p1, err := cursor.Decode([]byte(testCursorKey), cursors[1], time.Date(2026, 10, 3, 13, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if p0.ExpiresAt != p1.ExpiresAt || p0.UpperBound != p1.UpperBound || cursor.TipRef(p0.ImmutableRefs) != tipSHA {
		t.Fatalf("pin/deadline drift: %+v %+v", p0, p1)
	}
	wantExpiry := time.Date(2026, 10, 3, 14, 0, 0, 0, time.UTC).Format(time.RFC3339)
	if p0.ExpiresAt != wantExpiry {
		t.Fatalf("exact expiry want %s got %s", wantExpiry, p0.ExpiresAt)
	}
	// Exact expiry: resume at expires_at must fail closed with zero list continuation.
	listBefore := probe.listHits.Load()
	clk.T = time.Date(2026, 10, 3, 14, 0, 0, 0, time.UTC)
	_, err = callListCommitsMCP(t, d, map[string]any{"project_id": "42", "cursor": cursors[0], "per_page": 2})
	if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
		t.Fatalf("exact expiry want resync, got %v", err)
	}
	if probe.listHits.Load() != listBefore {
		t.Fatal("exact expiry must not list")
	}
}

func TestListCommits_bindFailureZeroContinuation(t *testing.T) {
	probe := &commitProbe{}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	var actor atomic.Int64
	actor.Store(7)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probe.note(r)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/user") {
			_, _ = io.WriteString(w, fmt.Sprintf(`{"id":%d,"username":"u"}`, actor.Load()))
			return
		}
		listCommitsHandler(nil, nil).ServeHTTP(w, r)
	})
	d := newCursorDeps(t, h, &config.Config{CursorKey: []byte(testCursorKey)}, clk)
	out, err := callListCommitsMCP(t, d, map[string]any{"project_id": "42", "use_cursor": true, "per_page": 2})
	if err != nil {
		t.Fatal(err)
	}
	nc := out["section"].(map[string]any)["next_cursor"].(string)
	listAfterInit := probe.listHits.Load()
	getAfterInit := probe.getCommit.Load()

	// Tamper cursor
	_, err = callListCommitsMCP(t, d, map[string]any{"project_id": "42", "cursor": nc + "x", "per_page": 2})
	if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
		t.Fatalf("want resync, got %v", err)
	}
	if probe.listHits.Load() != listAfterInit || probe.getCommit.Load() != getAfterInit {
		t.Fatal("tamper must not continue ref/list")
	}

	// N1: same-instance actor mismatch — mutate /user on the same server/APIURL.
	actor.Store(99)
	userBefore := probe.userHits.Load()
	listBefore := probe.listHits.Load()
	getBefore := probe.getCommit.Load()
	_, err = callListCommitsMCP(t, d, map[string]any{"project_id": "42", "cursor": nc, "per_page": 2})
	if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
		t.Fatalf("actor mismatch: %v", err)
	}
	if probe.userHits.Load() <= userBefore {
		t.Fatal("actor check must reach GET /user on same instance")
	}
	if probe.listHits.Load() != listBefore || probe.getCommit.Load() != getBefore {
		t.Fatalf("actor mismatch must not list/ref: list=%d get=%d", probe.listHits.Load(), probe.getCommit.Load())
	}
}

func TestListCommits_keyRotationResync(t *testing.T) {
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, listCommitsHandler(&commitProbe{}, nil), &config.Config{CursorKey: []byte(testCursorKey)}, clk)
	out, err := callListCommitsMCP(t, d, map[string]any{"project_id": "42", "use_cursor": true, "per_page": 2})
	if err != nil {
		t.Fatal(err)
	}
	nc := out["section"].(map[string]any)["next_cursor"].(string)
	d.Config.CursorKey = []byte("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz") // rotated 32 bytes
	_, err = callListCommitsMCP(t, d, map[string]any{"project_id": "42", "cursor": nc, "per_page": 2})
	if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
		t.Fatalf("rotation: %v", err)
	}
	if strings.Contains(err.Error(), testCursorKey) || strings.Contains(err.Error(), "zzzz") {
		t.Fatal("secret leaked in error")
	}
}

func TestListCommits_boundaryDriftResyncBeforeNextPage(t *testing.T) {
	probe := &commitProbe{}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	var flip atomic.Bool
	h := listCommitsHandler(probe, func(page string, body *string, next *string) {
		if flip.Load() && page == "1" {
			// Drift: different SHAs on guard re-fetch
			*body = pageCommits(sha3, sha4)
		}
	})
	d := newCursorDeps(t, h, &config.Config{CursorKey: []byte(testCursorKey)}, clk)
	out, err := callListCommitsMCP(t, d, map[string]any{"project_id": "42", "use_cursor": true, "per_page": 2})
	if err != nil {
		t.Fatal(err)
	}
	nc := out["section"].(map[string]any)["next_cursor"].(string)
	flip.Store(true)
	listBefore := probe.listHits.Load()
	_, err = callListCommitsMCP(t, d, map[string]any{"project_id": "42", "cursor": nc, "per_page": 2})
	if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
		t.Fatalf("drift: %v", err)
	}
	// Guard list happened (+1) but next page must not (page=2 absent after drift)
	probe.mu.Lock()
	page2 := probe.pages["2"]
	probe.mu.Unlock()
	if page2 != 0 {
		t.Fatalf("next page must not run after drift; page2 hits=%d listΔ=%d", page2, probe.listHits.Load()-listBefore)
	}
}

func TestListCommits_jumpingNextPageNoUnsafeCursor(t *testing.T) {
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	h := listCommitsHandler(&commitProbe{}, func(page string, body *string, next *string) {
		if page == "1" {
			*next = "3" // jump
		}
	})
	d := newCursorDeps(t, h, &config.Config{CursorKey: []byte(testCursorKey)}, clk)
	out, err := callListCommitsMCP(t, d, map[string]any{"project_id": "42", "use_cursor": true, "per_page": 2})
	if err != nil {
		t.Fatal(err)
	}
	sec := out["section"].(map[string]any)
	if sec["next_cursor"] != nil {
		t.Fatal("must not mint cursor on jumping next page")
	}
	if sec["pagination_exhausted"] == true {
		t.Fatal("must not claim exhaustion on jump")
	}
	if sec["content_complete"] != readmeta.ContentCompleteUnknown {
		t.Fatalf("content_complete=%v", sec["content_complete"])
	}
}

func TestListCommits_ambiguousPageRejected(t *testing.T) {
	d := newCursorDeps(t, listCommitsHandler(&commitProbe{}, nil), &config.Config{CursorKey: []byte(testCursorKey)}, nil)
	_, err := callListCommitsMCP(t, d, map[string]any{"project_id": "42", "use_cursor": true, "page": 2})
	if err == nil {
		t.Fatal("expected ambiguous page error")
	}
}

func TestListCommits_instanceBindingStripsURLCredentials(t *testing.T) {
	const sentinel = "s3cr3tPASS-instance-bind-sentinel"
	probe := &commitProbe{}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	ts := httptest.NewServer(listCommitsHandler(probe, nil))
	t.Cleanup(ts.Close)
	// Plant credential-bearing APIURL pointing at the test server host/path.
	u := strings.TrimPrefix(ts.URL, "http://")
	cfg := &config.Config{
		Token:     "t",
		APIURL:    "http://oauth:" + sentinel + "@" + u + "/api/v4?private_token=" + sentinel,
		CursorKey: []byte(testCursorKey),
	}
	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(igl.BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Config: cfg, Client: cli, Clock: clk}
	out, err := callListCommitsMCP(t, d, map[string]any{"project_id": "42", "use_cursor": true, "per_page": 2})
	if err != nil {
		t.Fatal(err)
	}
	nc := out["section"].(map[string]any)["next_cursor"].(string)
	if strings.Contains(nc, sentinel) {
		t.Fatal("cursor token leaked URL secret")
	}
	p, err := cursor.Decode([]byte(testCursorKey), nc, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), sentinel) || strings.Contains(p.Instance, "oauth:") {
		t.Fatalf("payload leaked secret: instance=%q", p.Instance)
	}
	wantHost := strings.ToLower(strings.Split(u, "/")[0])
	if !strings.HasPrefix(p.Instance, "http://"+wantHost) || !strings.HasSuffix(p.Instance, "/api/v4") {
		t.Fatalf("canonical instance unexpected: %q", p.Instance)
	}
}

func TestListCommits_resumeFilterMismatchTable(t *testing.T) {
	probe := &commitProbe{}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	// N1: one instance for mint + resume so filter compare is reached (not masked by instance).
	d := newCursorDeps(t, listCommitsHandler(probe, nil), &config.Config{CursorKey: []byte(testCursorKey)}, clk)
	baseArgs := map[string]any{
		"project_id": "42",
		"use_cursor": true,
		"ref_name":   "main",
		"path":       "a.go",
		"since":      "2026-01-01T00:00:00Z",
		"until":      "2026-10-02T00:00:00Z",
		"per_page":   2,
	}
	out, err := callListCommitsMCP(t, d, baseArgs)
	if err != nil {
		t.Fatal(err)
	}
	nc := out["section"].(map[string]any)["next_cursor"].(string)
	listAfterInit := probe.listHits.Load()
	getAfterInit := probe.getCommit.Load()
	mutations := []struct {
		name string
		args map[string]any
	}{
		{"ref_name", map[string]any{"project_id": "42", "cursor": nc, "ref_name": "other", "path": "a.go", "since": "2026-01-01T00:00:00Z", "until": "2026-10-02T00:00:00Z", "per_page": 2}},
		{"path", map[string]any{"project_id": "42", "cursor": nc, "ref_name": "main", "path": "b.go", "since": "2026-01-01T00:00:00Z", "until": "2026-10-02T00:00:00Z", "per_page": 2}},
		{"since", map[string]any{"project_id": "42", "cursor": nc, "ref_name": "main", "path": "a.go", "since": "2026-02-01T00:00:00Z", "until": "2026-10-02T00:00:00Z", "per_page": 2}},
		{"until", map[string]any{"project_id": "42", "cursor": nc, "ref_name": "main", "path": "a.go", "since": "2026-01-01T00:00:00Z", "until": "2026-09-01T00:00:00Z", "per_page": 2}},
		{"per_page", map[string]any{"project_id": "42", "cursor": nc, "ref_name": "main", "path": "a.go", "since": "2026-01-01T00:00:00Z", "until": "2026-10-02T00:00:00Z", "per_page": 1}},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			listBefore := probe.listHits.Load()
			getBefore := probe.getCommit.Load()
			userBefore := probe.userHits.Load()
			_, err := callListCommitsMCP(t, d, tc.args)
			if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
				t.Fatalf("want resync on %s mismatch, got %v", tc.name, err)
			}
			if probe.userHits.Load() <= userBefore {
				t.Fatalf("%s: expected /user on same instance before filter reject", tc.name)
			}
			if probe.listHits.Load() != listBefore || probe.getCommit.Load() != getBefore {
				t.Fatalf("%s mismatch must not guard/list: list=%d get=%d", tc.name, probe.listHits.Load()-listAfterInit, probe.getCommit.Load()-getAfterInit)
			}
		})
	}
}

func TestListCommits_perPageCap50(t *testing.T) {
	probe := &commitProbe{}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, listCommitsHandler(probe, nil), &config.Config{CursorKey: []byte(testCursorKey)}, clk)
	out, err := callListCommitsMCP(t, d, map[string]any{"project_id": "42", "use_cursor": true, "per_page": 100})
	if err != nil {
		t.Fatal(err)
	}
	nc := out["section"].(map[string]any)["next_cursor"].(string)
	p, err := cursor.Decode([]byte(testCursorKey), nc, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if p.Filters.PerPage != 50 {
		t.Fatalf("cap=%d", p.Filters.PerPage)
	}
}

func TestListCommits_missingPagingHeadersNullCursor(t *testing.T) {
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/user"):
			_, _ = io.WriteString(w, `{"id":7,"username":"alice"}`)
		case strings.Contains(r.URL.Path, "/repository/commits/") && !strings.HasSuffix(r.URL.Path, "/commits"):
			_, _ = io.WriteString(w, commitJSON(tipSHA))
		case strings.HasSuffix(r.URL.Path, "/repository/commits"):
			// No X-Next-Page header
			_, _ = io.WriteString(w, pageCommits(sha1))
		case strings.Contains(r.URL.Path, "/projects/"):
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":1,"kind":"group","full_path":"g","parent_id":0}}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	})
	d := newCursorDeps(t, h, &config.Config{CursorKey: []byte(testCursorKey)}, clk)
	out, err := callListCommitsMCP(t, d, map[string]any{"project_id": "42", "use_cursor": true})
	if err != nil {
		t.Fatal(err)
	}
	sec := out["section"].(map[string]any)
	if sec["next_cursor"] != nil {
		t.Fatalf("next_cursor must be null: %#v", sec["next_cursor"])
	}
	lim, _ := sec["limitations"].([]any)
	found := false
	for _, x := range lim {
		m, _ := x.(map[string]any)
		if m["code"] == cursor.ResyncRequired {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected resync limitation: %#v", lim)
	}
	// Retained items
	commits, _ := out["commits"].([]any)
	if len(commits) != 1 {
		t.Fatalf("retain items: %d", len(commits))
	}
}

func TestListCommits_policyFingerprintChangeResyncZeroContinuation(t *testing.T) {
	probe := &commitProbe{}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, listCommitsHandler(probe, nil), &config.Config{
		CursorKey:         []byte(testCursorKey),
		AllowedProjectIDs: []string{"42"},
	}, clk)
	out, err := callListCommitsMCP(t, d, map[string]any{"project_id": "42", "use_cursor": true, "per_page": 2})
	if err != nil {
		t.Fatal(err)
	}
	nc := out["section"].(map[string]any)["next_cursor"].(string)
	listAfterInit := probe.listHits.Load()
	getAfterInit := probe.getCommit.Load()
	projAfterInit := probe.projectHits.Load()
	// Fingerprint change (allowlist edit). Even though 42 is also revoked, AC requires
	// resync_required from binding compare — before authz would emit authz_denied.
	d.Config.AllowedProjectIDs = []string{"99"}
	_, err = callListCommitsMCP(t, d, map[string]any{"project_id": "42", "cursor": nc, "per_page": 2})
	if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
		t.Fatalf("want resync_required for fingerprint change, got %v", err)
	}
	if strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
		t.Fatalf("fingerprint mismatch must not surface as authz_denied: %v", err)
	}
	if probe.listHits.Load() != listAfterInit || probe.getCommit.Load() != getAfterInit {
		t.Fatal("fingerprint change must not continue list/ref")
	}
	if probe.projectHits.Load() != projAfterInit {
		t.Fatal("fingerprint change must not hit project authz before resync")
	}
}

func TestListCommits_samePolicyRevocationAuthzDeniedZeroContinuation(t *testing.T) {
	// Same policy fingerprint (AllowedGroupIDs unchanged); live group ancestry
	// changes so project no longer satisfies policy → authz_denied before content.
	probe := &commitProbe{}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	var moved atomic.Bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probe.note(r)
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/user"):
			_, _ = io.WriteString(w, `{"id":7,"username":"alice"}`)
		case strings.Contains(path, "/groups/10") || strings.HasSuffix(path, "/groups/acme"):
			_, _ = io.WriteString(w, `{"id":10,"full_path":"acme","parent_id":0}`)
		case strings.Contains(path, "/groups/99"):
			_, _ = io.WriteString(w, `{"id":99,"full_path":"other","parent_id":0}`)
		case strings.Contains(path, "/repository/commits/") && !strings.HasSuffix(path, "/commits"):
			_, _ = io.WriteString(w, commitJSON(tipSHA))
		case strings.HasSuffix(path, "/repository/commits"):
			page := r.URL.Query().Get("page")
			if page == "" {
				page = "1"
			}
			var body, next string
			switch page {
			case "1":
				body, next = pageCommits(sha1, sha2), "2"
			case "2":
				body, next = pageCommits(sha3, sha4), "3"
			default:
				body, next = "[]", ""
			}
			if next != "" {
				w.Header().Set("X-Next-Page", next)
			}
			_, _ = io.WriteString(w, body)
		case strings.Contains(path, "/projects/"):
			nsID, nsPath := int64(10), "acme"
			if moved.Load() {
				nsID, nsPath = 99, "other"
			}
			_, _ = io.WriteString(w, fmt.Sprintf(
				`{"id":42,"path_with_namespace":%q,"namespace":{"id":%d,"kind":"group","full_path":%q,"parent_id":0}}`,
				nsPath+"/p", nsID, nsPath))
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	})
	d := newCursorDeps(t, h, &config.Config{
		CursorKey:       []byte(testCursorKey),
		AllowedGroupIDs: []string{"10"},
	}, clk)
	fpBefore := d.Config.PolicyFingerprint()
	out, err := callListCommitsMCP(t, d, map[string]any{"project_id": "42", "use_cursor": true, "per_page": 2})
	if err != nil {
		t.Fatal(err)
	}
	nc := out["section"].(map[string]any)["next_cursor"].(string)
	listAfterInit := probe.listHits.Load()
	getAfterInit := probe.getCommit.Load()
	moved.Store(true)
	if d.Config.PolicyFingerprint() != fpBefore {
		t.Fatal("test bug: policy fingerprint must stay unchanged")
	}
	_, err = callListCommitsMCP(t, d, map[string]any{"project_id": "42", "cursor": nc, "per_page": 2})
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
		t.Fatalf("want authz_denied for same-policy revocation, got %v", err)
	}
	if probe.listHits.Load() != listAfterInit || probe.getCommit.Load() != getAfterInit {
		t.Fatal("same-policy revocation must not continue list/ref")
	}
}

func TestListCommits_mcpActorUnresolvedAndProjectAuthzDenied(t *testing.T) {
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}

	t.Run("actor_unresolved_registered_mcp", func(t *testing.T) {
		probe := &commitProbe{}
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			probe.note(r)
			if strings.HasSuffix(r.URL.Path, "/user") {
				http.Error(w, `{"message":"401"}`, http.StatusUnauthorized)
				return
			}
			listCommitsHandler(nil, nil).ServeHTTP(w, r)
		})
		d := newCursorDeps(t, h, &config.Config{CursorKey: []byte(testCursorKey)}, clk)
		_, err := callListCommitsMCP(t, d, map[string]any{"project_id": "42", "use_cursor": true, "per_page": 2})
		if err == nil || !strings.Contains(err.Error(), readmeta.CodeIdentityUnresolved) {
			t.Fatalf("want identity_unresolved via MCP, got %v", err)
		}
		if probe.listHits.Load() != 0 || probe.getCommit.Load() != 0 {
			t.Fatal("actor failure must not list/ref")
		}
	})

	t.Run("project_authz_denied_registered_mcp", func(t *testing.T) {
		probe := &commitProbe{}
		d := newCursorDeps(t, listCommitsHandler(probe, nil), &config.Config{
			CursorKey:         []byte(testCursorKey),
			AllowedProjectIDs: []string{"99"},
		}, clk)
		_, err := callListCommitsMCP(t, d, map[string]any{"project_id": "42", "use_cursor": true, "per_page": 2})
		if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
			t.Fatalf("want authz_denied via MCP, got %v", err)
		}
		if probe.listHits.Load() != 0 || probe.getCommit.Load() != 0 {
			t.Fatal("project authz deny must not list/ref")
		}
	})
}

func TestListCommits_invalidSinceUntilSafeProjection(t *testing.T) {
	probe := &commitProbe{}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	const sentinel = "raw-since-until-SECRET-echo-me"
	d := newCursorDeps(t, listCommitsHandler(probe, nil), &config.Config{CursorKey: []byte(testCursorKey)}, clk)
	_, err := callListCommitsMCP(t, d, map[string]any{
		"project_id": "42",
		"use_cursor": true,
		"since":      sentinel,
		"per_page":   2,
	})
	if err == nil {
		t.Fatal("expected invalid since error")
	}
	msg := err.Error()
	if strings.Contains(msg, sentinel) || strings.Contains(msg, "parse time") {
		t.Fatalf("error echoed raw input: %v", err)
	}
	if !strings.Contains(msg, "invalid since") {
		t.Fatalf("want static invalid since on initial, got %v", err)
	}
	_, err = callListCommitsMCP(t, d, map[string]any{
		"project_id": "42",
		"use_cursor": true,
		"until":      "not-a-date-" + sentinel,
		"per_page":   2,
	})
	if err == nil || strings.Contains(err.Error(), sentinel) || !strings.Contains(err.Error(), "invalid until") {
		t.Fatalf("want static invalid until without echo, got %v", err)
	}

	// N2: malformed resume filters → static resync_required (not "invalid since"), zero continuation.
	out, err := callListCommitsMCP(t, d, map[string]any{"project_id": "42", "use_cursor": true, "per_page": 2})
	if err != nil {
		t.Fatal(err)
	}
	nc := out["section"].(map[string]any)["next_cursor"].(string)
	listBefore := probe.listHits.Load()
	_, err = callListCommitsMCP(t, d, map[string]any{
		"project_id": "42",
		"cursor":     nc,
		"since":      sentinel,
		"per_page":   2,
	})
	if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
		t.Fatalf("resume malformed filter want resync_required, got %v", err)
	}
	if strings.Contains(err.Error(), sentinel) || strings.Contains(err.Error(), "invalid since") {
		t.Fatalf("resume must not echo raw or use initial invalid-since shape: %v", err)
	}
	if probe.listHits.Load() != listBefore {
		t.Fatal("malformed resume filter must not continue list")
	}
}

func TestResolveCursorProjectCanonical_numeric(t *testing.T) {
	d := newCursorDeps(t, listCommitsHandler(&commitProbe{}, nil), &config.Config{}, nil)
	pid, err := resolveCursorProjectCanonical(context.Background(), d, "group/proj")
	if err != nil {
		t.Fatal(err)
	}
	if pid != "42" {
		t.Fatalf("got %s", pid)
	}
	// Empty policy: resolveProjectAuthz would return path; cursor helper must still be numeric.
	legacy, err := resolveProjectAuthz(context.Background(), d, "group/proj")
	if err != nil {
		t.Fatal(err)
	}
	if legacy != "group/proj" {
		t.Fatalf("legacy path expectation changed: %s", legacy)
	}
	if _, err := strconv.Atoi(pid); err != nil {
		t.Fatalf("cursor pid not numeric: %s", pid)
	}
}

func testCommitFilters(perPage int) cursor.Filters {
	return cursor.Filters{
		Until:     "2026-10-03T12:00:00Z",
		Order:     "provider_default",
		Selection: "list_commits",
		PerPage:   perPage,
	}
}

func streamCommitsPageBodyHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/repository/commits") {
			w.Header().Set("X-Next-Page", "2")
			_, _ = io.WriteString(w, body)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	})
}

func assertPartialEmitNullCursor(t *testing.T, out any, streamCommits []*gitlab.Commit, wantPrefix []string) {
	t.Helper()
	if len(streamCommits) != len(wantPrefix) {
		t.Fatalf("stream retained prefix len=%d want %d", len(streamCommits), len(wantPrefix))
	}
	for i, sha := range wantPrefix {
		if streamCommits[i] == nil || streamCommits[i].ID != sha {
			t.Fatalf("stream prefix[%d]=%v want %s", i, streamCommits[i], sha)
		}
	}
	// emitListCommitsCursor takes Section by value; assert the MCP Out() tree.
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("Out tree type %T", out)
	}
	sec, _ := m["section"].(map[string]any)
	if sec == nil {
		t.Fatalf("section missing: %#v", m)
	}
	if sec["next_cursor"] != nil {
		t.Fatalf("next_cursor must be null, got %#v", sec["next_cursor"])
	}
	if sec["pagination_exhausted"] == true {
		t.Fatal("must not claim pagination_exhausted on partial")
	}
	if sec["content_complete"] != readmeta.ContentCompleteFalse {
		t.Fatalf("content_complete=%v want %q", sec["content_complete"], readmeta.ContentCompleteFalse)
	}
	commits, _ := m["commits"].([]any)
	if len(commits) != len(wantPrefix) {
		t.Fatalf("Out commits len=%d want %d", len(commits), len(wantPrefix))
	}
	for i, sha := range wantPrefix {
		c, _ := commits[i].(map[string]any)
		if c["id"] != sha {
			t.Fatalf("Out prefix[%d]=%v want %s", i, c["id"], sha)
		}
	}
	lim, _ := sec["limitations"].([]any)
	foundResync := false
	for _, x := range lim {
		lm, _ := x.(map[string]any)
		if lm["code"] == cursor.ResyncRequired {
			foundResync = true
		}
	}
	if !foundResync {
		t.Fatalf("expected resync_required limitation: %#v", lim)
	}
}

func TestStreamCommitsPage_boundaryFixturesRetainPrefix(t *testing.T) {
	good := commitJSON(sha1)
	cases := []struct {
		name       string
		body       string
		wantPrefix []string
		errSubstr  string
	}{
		{
			name:       "nil_element",
			body:       "[" + good + `,null,` + commitJSON(sha2) + "]",
			wantPrefix: []string{sha1},
			errSubstr:  "nil commit",
		},
		{
			name:       "malformed_element",
			body:       "[" + good + `,"not-an-object",` + commitJSON(sha2) + "]",
			wantPrefix: []string{sha1},
			errSubstr:  "malformed commit",
		},
		{
			name:       "invalid_sha",
			body:       "[" + good + `,{"id":"not-a-valid-git-sha","title":"x"}]`,
			wantPrefix: []string{sha1},
			errSubstr:  "invalid commit SHA",
		},
		{
			name:       "duplicate_sha",
			body:       "[" + good + "," + good + "]",
			wantPrefix: []string{sha1},
			errSubstr:  "duplicate commit SHA",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newCursorDeps(t, streamCommitsPageBodyHandler(tc.body), &config.Config{CursorKey: []byte(testCursorKey)}, nil)
			b := &igl.Budget{
				MaxItems:    50,
				MaxBytes:    1 << 20,
				MaxRequests: 8,
				MaxElapsed:  time.Minute,
			}
			ctx := igl.WithBudget(context.Background(), b)
			filters := testCommitFilters(10)
			commits, res := streamCommitsPage(ctx, d, b, "42", tipSHA, filters, 1, nil)
			if !res.partial || !res.boundaryBroken || res.stopErr == nil {
				t.Fatalf("want partial+boundaryBroken+stopErr, got partial=%v broken=%v err=%v", res.partial, res.boundaryBroken, res.stopErr)
			}
			if !strings.Contains(res.stopErr.Error(), tc.errSubstr) {
				t.Fatalf("stopErr=%v want substr %q", res.stopErr, tc.errSubstr)
			}
			_, out, err := emitListCommitsCursor(newListCommitsSection(time.Now()), commits, res, d, 7, "42", []string{tipSHA},
				filters.Until, time.Now().Add(2*time.Hour).UTC().Format(time.RFC3339), filters, 1)
			if err != nil {
				t.Fatal(err)
			}
			assertPartialEmitNullCursor(t, out, commits, tc.wantPrefix)
		})
	}
}

func TestStreamCommitsPage_budgetCapsAndCancelClose(t *testing.T) {
	filters := testCommitFilters(10)
	body3 := "[" + commitJSON(sha1) + "," + commitJSON(sha2) + "," + commitJSON(sha3) + "]"

	t.Run("items", func(t *testing.T) {
		d := newCursorDeps(t, streamCommitsPageBodyHandler(body3), &config.Config{CursorKey: []byte(testCursorKey)}, nil)
		b := &igl.Budget{MaxItems: 1, MaxBytes: 1 << 20, MaxRequests: 8, MaxElapsed: time.Minute}
		ctx := igl.WithBudget(context.Background(), b)
		commits, res := streamCommitsPage(ctx, d, b, "42", tipSHA, filters, 1, nil)
		if !errors.Is(res.stopErr, igl.ErrBudgetItems) || !res.partial {
			t.Fatalf("want budget_items partial, got err=%v partial=%v", res.stopErr, res.partial)
		}
		if len(commits) != 1 || commits[0].ID != sha1 {
			t.Fatalf("retain good prefix: %#v", commits)
		}
		_, _, items := b.Stats()
		if items != 1 {
			t.Fatalf("items counter=%d", items)
		}
		_, out, _ := emitListCommitsCursor(newListCommitsSection(time.Now()), commits, res, d, 7, "42", []string{tipSHA},
			filters.Until, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), filters, 1)
		assertPartialEmitNullCursor(t, out, commits, []string{sha1})
		sec := out.(map[string]any)["section"].(map[string]any)
		foundBudget := false
		for _, x := range sec["limitations"].([]any) {
			if x.(map[string]any)["code"] == readmeta.CodeBudgetItems {
				foundBudget = true
			}
		}
		if !foundBudget {
			t.Fatalf("want budget_items limitation: %#v", sec["limitations"])
		}
	})

	t.Run("bytes", func(t *testing.T) {
		d := newCursorDeps(t, streamCommitsPageBodyHandler(body3), &config.Config{CursorKey: []byte(testCursorKey)}, nil)
		// Tiny byte cap forces mid-stream stop while still allowing a prefix decode.
		b := &igl.Budget{MaxItems: 50, MaxBytes: int64(len(commitJSON(sha1)) + 8), MaxRequests: 8, MaxElapsed: time.Minute}
		ctx := igl.WithBudget(context.Background(), b)
		commits, res := streamCommitsPage(ctx, d, b, "42", tipSHA, filters, 1, nil)
		if !res.partial || res.stopErr == nil {
			t.Fatalf("want partial byte stop, got partial=%v err=%v commits=%d", res.partial, res.stopErr, len(commits))
		}
		if !errors.Is(res.stopErr, igl.ErrBudgetBytes) && !strings.Contains(res.stopErr.Error(), "budget_bytes") {
			// stopErr may be typed or wrapped via stream mapping
			if !errors.Is(res.stopErr, igl.ErrBudgetBytes) {
				t.Fatalf("want budget_bytes, got %v", res.stopErr)
			}
		}
		if len(commits) > 2 {
			t.Fatalf("byte cap must not retain full page: %d", len(commits))
		}
		_, out, _ := emitListCommitsCursor(newListCommitsSection(time.Now()), commits, res, d, 7, "42", []string{tipSHA},
			filters.Until, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), filters, 1)
		sec := out.(map[string]any)["section"].(map[string]any)
		if sec["next_cursor"] != nil || sec["pagination_exhausted"] == true {
			t.Fatal("bytes partial must null cursor and not exhausted")
		}
	})

	t.Run("requests", func(t *testing.T) {
		d := newCursorDeps(t, streamCommitsPageBodyHandler(body3), &config.Config{CursorKey: []byte(testCursorKey)}, nil)
		b := &igl.Budget{MaxItems: 50, MaxBytes: 1 << 20, MaxRequests: 1, MaxElapsed: time.Minute}
		ctx := igl.WithBudget(context.Background(), b)
		// Burn the single allowed RoundTrip so streamCommitsPage request is denied.
		req, err := d.Client.NewRequest(http.MethodGet, "user", nil, []gitlab.RequestOptionFunc{gitlab.WithContext(ctx)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Client.Do(req, new(bytesDiscard)); err != nil && !errors.Is(err, igl.ErrBudgetRequests) {
			// user endpoint may 404 on streamCommitsPageBodyHandler — still charges request
			_ = err
		}
		commits, res := streamCommitsPage(ctx, d, b, "42", tipSHA, filters, 1, nil)
		if !errors.Is(res.stopErr, igl.ErrBudgetRequests) || !res.partial {
			t.Fatalf("want budget_requests, got err=%v partial=%v", res.stopErr, res.partial)
		}
		if len(commits) != 0 {
			t.Fatalf("requests cap must not retain items: %d", len(commits))
		}
		reqs, _, _ := b.Stats()
		if reqs < 1 {
			t.Fatalf("requests counter not wired: %d", reqs)
		}
	})

	t.Run("elapsed", func(t *testing.T) {
		started := make(chan struct{})
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if !strings.HasSuffix(r.URL.Path, "/repository/commits") {
				_, _ = io.WriteString(w, `{}`)
				return
			}
			close(started)
			select {
			case <-r.Context().Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
			w.Header().Set("X-Next-Page", "2")
			_, _ = io.WriteString(w, body3)
		})
		d := newCursorDeps(t, h, &config.Config{CursorKey: []byte(testCursorKey)}, nil)
		b := &igl.Budget{MaxItems: 50, MaxBytes: 1 << 20, MaxRequests: 8, MaxElapsed: 40 * time.Millisecond}
		ctx := igl.WithBudget(context.Background(), b)
		commits, res := streamCommitsPage(ctx, d, b, "42", tipSHA, filters, 1, nil)
		<-started
		if !res.partial || res.stopErr == nil {
			t.Fatalf("want elapsed partial, got partial=%v err=%v", res.partial, res.stopErr)
		}
		if !errors.Is(res.stopErr, igl.ErrBudgetElapsed) && !errors.Is(res.stopErr, context.DeadlineExceeded) {
			t.Fatalf("want budget_elapsed/deadline, got %v", res.stopErr)
		}
		if len(commits) != 0 {
			t.Fatalf("elapsed before body should retain 0, got %d", len(commits))
		}
		_, out, _ := emitListCommitsCursor(newListCommitsSection(time.Now()), commits, res, d, 7, "42", []string{tipSHA},
			filters.Until, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), filters, 1)
		sec := out.(map[string]any)["section"].(map[string]any)
		if sec["next_cursor"] != nil || sec["pagination_exhausted"] == true {
			t.Fatal("elapsed partial must null cursor and not exhausted")
		}
	})

	t.Run("cancel_closes_upstream", func(t *testing.T) {
		var cancelSeen atomic.Int64
		var writes atomic.Int64
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if !strings.HasSuffix(r.URL.Path, "/repository/commits") {
				_, _ = io.WriteString(w, `{}`)
				return
			}
			flusher, _ := w.(http.Flusher)
			w.Header().Set("X-Next-Page", "2")
			_, _ = io.WriteString(w, "["+commitJSON(sha1)+",")
			writes.Add(1)
			if flusher != nil {
				flusher.Flush()
			}
			// Remaining payload is large; budget MaxItems=1 cancels and must close upstream.
			rest := commitJSON(sha2) + "," + commitJSON(sha3) + "," + commitJSON(sha4) + "]"
			for i := 0; i < len(rest); i += 8 {
				select {
				case <-r.Context().Done():
					cancelSeen.Add(1)
					return
				default:
				}
				end := i + 8
				if end > len(rest) {
					end = len(rest)
				}
				n, err := io.WriteString(w, rest[i:end])
				if n > 0 {
					writes.Add(1)
				}
				if err != nil {
					cancelSeen.Add(1)
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
		d := newCursorDeps(t, h, &config.Config{CursorKey: []byte(testCursorKey)}, nil)
		b := &igl.Budget{MaxItems: 1, MaxBytes: 1 << 20, MaxRequests: 8, MaxElapsed: time.Minute}
		ctx := igl.WithBudget(context.Background(), b)
		commits, res := streamCommitsPage(ctx, d, b, "42", tipSHA, filters, 1, nil)
		if !errors.Is(res.stopErr, igl.ErrBudgetItems) {
			t.Fatalf("want budget_items, got %v", res.stopErr)
		}
		if len(commits) != 1 {
			t.Fatalf("retain 1, got %d", len(commits))
		}
		// Budget.Cancel from StreamJSONArray stop must cancel the WithBudget ctx.
		if ctx.Err() == nil {
			t.Fatal("budget cancel/close must cancel stream context")
		}
		deadline := time.Now().Add(2 * time.Second)
		for cancelSeen.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if cancelSeen.Load() == 0 {
			t.Fatal("upstream handler must observe cancel/close after item budget stop")
		}
		if writes.Load() < 1 {
			t.Fatal("expected at least one write before cancel")
		}
	})

	t.Run("guard_budget_no_next_page", func(t *testing.T) {
		probe := &commitProbe{}
		d := newCursorDeps(t, listCommitsHandler(probe, nil), &config.Config{CursorKey: []byte(testCursorKey)}, nil)
		// Shared invocation budget: guard page consumes MaxItems → next page must not run.
		b := &igl.Budget{MaxItems: 2, MaxBytes: 1 << 20, MaxRequests: 16, MaxElapsed: time.Minute}
		ctx := igl.WithBudget(context.Background(), b)
		guard, gres := streamCommitsPage(ctx, d, b, "42", tipSHA, testCommitFilters(2), 1, nil)
		if gres.partial || gres.stopErr != nil || len(guard) != 2 {
			t.Fatalf("guard setup failed: partial=%v err=%v n=%d", gres.partial, gres.stopErr, len(guard))
		}
		listAfterGuard := probe.listHits.Load()
		next, nres := streamCommitsPage(ctx, d, b, "42", tipSHA, testCommitFilters(2), 2, nil)
		if !errors.Is(nres.stopErr, igl.ErrBudgetItems) || !nres.partial {
			t.Fatalf("next page must hit items budget, got err=%v partial=%v", nres.stopErr, nres.partial)
		}
		if len(next) != 0 {
			t.Fatalf("no items after guard budget exhaustion, got %d", len(next))
		}
		// list may still charge a RoundTrip before AddItem fails — page counter must show page=2 was attempted
		// OR if request happens, items stay 0. Resume path skips next when guard partial; here we prove
		// shared budget leaves zero retained next-page items (handler wiring).
		_ = listAfterGuard
		_, _, items := b.Stats()
		if items != 2 {
			t.Fatalf("items charged only for guard page, got %d", items)
		}
	})
}

// bytesDiscard is a tiny Write sink for burning a budget RoundTrip in tests.
type bytesDiscard struct{}

func (bytesDiscard) Write(p []byte) (int, error) { return len(p), nil }

func TestListCommits_mcpPartialHTTPRetainPrefixNullCursor(t *testing.T) {
	probe := &commitProbe{}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	h := listCommitsHandler(probe, func(page string, body *string, next *string) {
		if page == "1" {
			// Meaningful partial: one good commit then malformed element (not a per_page>100 case).
			*body = "[" + commitJSON(sha1) + `,{"id":`
			*next = "2"
		}
	})
	d := newCursorDeps(t, h, &config.Config{CursorKey: []byte(testCursorKey)}, clk)
	out, err := callListCommitsMCP(t, d, map[string]any{"project_id": "42", "use_cursor": true, "per_page": 2})
	if err != nil {
		t.Fatal(err)
	}
	sec, _ := out["section"].(map[string]any)
	if sec["next_cursor"] != nil {
		t.Fatalf("next_cursor must be null on partial HTTP: %#v", sec["next_cursor"])
	}
	if sec["pagination_exhausted"] == true {
		t.Fatal("partial must not claim exhausted")
	}
	if sec["content_complete"] != string(readmeta.ContentCompleteFalse) && sec["content_complete"] != readmeta.ContentCompleteFalse {
		t.Fatalf("content_complete=%v", sec["content_complete"])
	}
	commits, _ := out["commits"].([]any)
	if len(commits) != 1 {
		t.Fatalf("retain good prefix len=%d", len(commits))
	}
	c0, _ := commits[0].(map[string]any)
	if c0["id"] != sha1 {
		t.Fatalf("prefix id=%v", c0["id"])
	}
	lim, _ := sec["limitations"].([]any)
	foundResync := false
	for _, x := range lim {
		m, _ := x.(map[string]any)
		if m["code"] == cursor.ResyncRequired {
			foundResync = true
		}
	}
	if !foundResync {
		t.Fatalf("want resync limitation: %#v", lim)
	}
}

func TestListCommits_guardFailureNoNextPageZeroGuardData(t *testing.T) {
	probe := &commitProbe{}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	var breakGuard atomic.Bool
	h := listCommitsHandler(probe, func(page string, body *string, next *string) {
		if breakGuard.Load() && page == "1" {
			// Guard re-fetch becomes partial/malformed — resume must not fetch next page
			// and must not return guard page commits as tool content.
			*body = "[" + commitJSON(sha1) + `,null]`
			*next = "2"
		}
	})
	d := newCursorDeps(t, h, &config.Config{CursorKey: []byte(testCursorKey)}, clk)
	out, err := callListCommitsMCP(t, d, map[string]any{"project_id": "42", "use_cursor": true, "per_page": 2})
	if err != nil {
		t.Fatal(err)
	}
	nc := out["section"].(map[string]any)["next_cursor"].(string)
	breakGuard.Store(true)
	probe.mu.Lock()
	page2Before := probe.pages["2"]
	probe.mu.Unlock()
	out2, err := callListCommitsMCP(t, d, map[string]any{"project_id": "42", "cursor": nc, "per_page": 2})
	if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
		t.Fatalf("want guard resync_required, got out=%v err=%v", out2, err)
	}
	msg := err.Error()
	for _, sha := range []string{sha1, sha2, sha3, sha4} {
		if strings.Contains(msg, sha) {
			t.Fatalf("guard data leaked into error: %s in %q", sha, msg)
		}
	}
	if out2 != nil {
		if commits, ok := out2["commits"].([]any); ok && len(commits) > 0 {
			t.Fatalf("guard data must not be returned as content: %#v", commits)
		}
	}
	probe.mu.Lock()
	page2After := probe.pages["2"]
	probe.mu.Unlock()
	if page2After != page2Before {
		t.Fatalf("next page must not run after guard failure: before=%d after=%d", page2Before, page2After)
	}
}

func TestListCommits_guardPagingDriftResyncNoNextPage(t *testing.T) {
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	cases := []struct {
		name string
		next string // guard (page=1) X-Next-Page on resume
	}{
		{"exhausted", ""},
		{"jump", "3"},
		{"garbage", "nope"},
		{"missing", "__missing__"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := &commitProbe{}
			var resume atomic.Bool
			h := listCommitsHandler(probe, func(page string, body *string, next *string) {
				if resume.Load() && page == "1" {
					switch tc.next {
					case "__missing__":
						*next = "__missing__"
					default:
						*next = tc.next
					}
				}
			})
			// Specialize missing-header path: strip X-Next-Page entirely.
			if tc.next == "__missing__" {
				base := h
				h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if resume.Load() && strings.HasSuffix(r.URL.Path, "/repository/commits") && r.URL.Query().Get("page") == "1" {
						probe.note(r)
						w.Header().Set("Content-Type", "application/json")
						// Intentionally omit X-Next-Page
						_, _ = io.WriteString(w, pageCommits(sha1, sha2))
						return
					}
					base.ServeHTTP(w, r)
				})
			}
			d := newCursorDeps(t, h, &config.Config{CursorKey: []byte(testCursorKey)}, clk)
			out, err := callListCommitsMCP(t, d, map[string]any{"project_id": "42", "use_cursor": true, "per_page": 2})
			if err != nil {
				t.Fatal(err)
			}
			nc := out["section"].(map[string]any)["next_cursor"].(string)
			resume.Store(true)
			probe.mu.Lock()
			page2Before := probe.pages["2"]
			probe.mu.Unlock()
			_, err = callListCommitsMCP(t, d, map[string]any{"project_id": "42", "cursor": nc, "per_page": 2})
			if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
				t.Fatalf("want resync_required, got %v", err)
			}
			probe.mu.Lock()
			page2After := probe.pages["2"]
			probe.mu.Unlock()
			if page2After != page2Before {
				t.Fatalf("next page must not run: before=%d after=%d", page2Before, page2After)
			}
		})
	}
}

func TestListCommits_nextPageOverlapNullCursor(t *testing.T) {
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	t.Run("overlap_at_first", func(t *testing.T) {
		probe := &commitProbe{}
		h := listCommitsHandler(probe, func(page string, body *string, next *string) {
			switch page {
			case "1":
				*body, *next = pageCommits(sha1, sha2), "2"
			case "2":
				// Entire previous page replayed as "next"
				*body, *next = pageCommits(sha1, sha2), "3"
			}
		})
		d := newCursorDeps(t, h, &config.Config{CursorKey: []byte(testCursorKey)}, clk)
		out, err := callListCommitsMCP(t, d, map[string]any{"project_id": "42", "use_cursor": true, "per_page": 2})
		if err != nil {
			t.Fatal(err)
		}
		nc := out["section"].(map[string]any)["next_cursor"].(string)
		out2, err := callListCommitsMCP(t, d, map[string]any{"project_id": "42", "cursor": nc, "per_page": 2})
		if err != nil {
			t.Fatal(err)
		}
		sec := out2["section"].(map[string]any)
		if sec["next_cursor"] != nil {
			t.Fatalf("next_cursor must be null on overlap: %#v", sec["next_cursor"])
		}
		if sec["pagination_exhausted"] == true {
			t.Fatal("must not claim exhausted")
		}
		if sec["content_complete"] != readmeta.ContentCompleteFalse {
			t.Fatalf("content_complete=%v", sec["content_complete"])
		}
		if sec["consistency"] != readmeta.ConsistencyInconsistent {
			t.Fatalf("consistency=%v", sec["consistency"])
		}
		commits, _ := out2["commits"].([]any)
		if len(commits) != 0 {
			t.Fatalf("overlap at first retains empty prefix, got %d", len(commits))
		}
	})
	t.Run("overlap_after_prefix", func(t *testing.T) {
		probe := &commitProbe{}
		h := listCommitsHandler(probe, func(page string, body *string, next *string) {
			switch page {
			case "1":
				*body, *next = pageCommits(sha1, sha2), "2"
			case "2":
				// Safe prefix sha3 then overlap sha2 from previous page
				*body, *next = pageCommits(sha3, sha2), "3"
			}
		})
		d := newCursorDeps(t, h, &config.Config{CursorKey: []byte(testCursorKey)}, clk)
		out, err := callListCommitsMCP(t, d, map[string]any{"project_id": "42", "use_cursor": true, "per_page": 2})
		if err != nil {
			t.Fatal(err)
		}
		nc := out["section"].(map[string]any)["next_cursor"].(string)
		out2, err := callListCommitsMCP(t, d, map[string]any{"project_id": "42", "cursor": nc, "per_page": 2})
		if err != nil {
			t.Fatal(err)
		}
		sec := out2["section"].(map[string]any)
		if sec["next_cursor"] != nil || sec["pagination_exhausted"] == true {
			t.Fatalf("overlap must null cursor and not exhaust: %#v", sec)
		}
		if sec["consistency"] != readmeta.ConsistencyInconsistent {
			t.Fatalf("consistency=%v", sec["consistency"])
		}
		commits, _ := out2["commits"].([]any)
		if len(commits) != 1 {
			t.Fatalf("retain safe prefix len=%d", len(commits))
		}
		if commits[0].(map[string]any)["id"] != sha3 {
			t.Fatalf("prefix id=%v", commits[0].(map[string]any)["id"])
		}
		for _, sha := range []string{sha1, sha2} {
			raw, _ := json.Marshal(out2)
			if strings.Contains(string(raw), sha) && sha != sha3 {
				// sha2 must not appear (not appended); sha1 is guard-only
				if sha == sha2 || sha == sha1 {
					t.Fatalf("guard/overlap sha %s must not appear in content: %s", sha, string(raw))
				}
			}
		}
	})
}

func TestListCommits_whitespaceCursorMalformedNoBackend(t *testing.T) {
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	ws := "  \t\n "
	padded := " " + testCursorKey[:8] + "deadbeefdeadbeefdeadbeefdeadbeef " // padded garbage, nonempty after trim ≠ raw

	t.Run("whitespace_use_cursor_false", func(t *testing.T) {
		probe := &commitProbe{}
		d := newCursorDeps(t, listCommitsHandler(probe, nil), &config.Config{CursorKey: []byte(testCursorKey)}, clk)
		_, err := callListCommitsMCPRaw(t, d, map[string]any{"project_id": "42", "cursor": ws})
		if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
			t.Fatalf("want resync_required, got %v", err)
		}
		if strings.Contains(err.Error(), "GITLAB_MCP_CURSOR_KEY") {
			t.Fatalf("malformed must not be key error: %v", err)
		}
		if probe.listHits.Load() != 0 || probe.userHits.Load() != 0 || probe.projectHits.Load() != 0 {
			t.Fatalf("zero backend required: user=%d proj=%d list=%d", probe.userHits.Load(), probe.projectHits.Load(), probe.listHits.Load())
		}
	})
	t.Run("whitespace_use_cursor_true", func(t *testing.T) {
		probe := &commitProbe{}
		d := newCursorDeps(t, listCommitsHandler(probe, nil), &config.Config{CursorKey: []byte(testCursorKey)}, clk)
		_, err := callListCommitsMCPRaw(t, d, map[string]any{"project_id": "42", "use_cursor": true, "cursor": ws})
		if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
			t.Fatalf("want resync_required, got %v", err)
		}
		if probe.listHits.Load() != 0 || probe.userHits.Load() != 0 {
			t.Fatal("zero backend")
		}
	})
	t.Run("whitespace_keyless_resync_not_key_error", func(t *testing.T) {
		probe := &commitProbe{}
		cfg := &config.Config{CursorKey: nil}
		d := newCursorDeps(t, listCommitsHandler(probe, nil), cfg, clk)
		d.Config.CursorKey = nil
		_, err := callListCommitsMCPRaw(t, d, map[string]any{"project_id": "42", "cursor": ws})
		if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
			t.Fatalf("keyless malformed want resync_required, got %v", err)
		}
		if strings.Contains(err.Error(), "GITLAB_MCP_CURSOR_KEY") {
			t.Fatalf("keyless malformed must not surface key error: %v", err)
		}
		if probe.listHits.Load() != 0 || probe.userHits.Load() != 0 || probe.projectHits.Load() != 0 {
			t.Fatal("zero backend")
		}
	})
	t.Run("padded_token_resync", func(t *testing.T) {
		probe := &commitProbe{}
		d := newCursorDeps(t, listCommitsHandler(probe, nil), &config.Config{CursorKey: []byte(testCursorKey)}, clk)
		_, err := callListCommitsMCPRaw(t, d, map[string]any{"project_id": "42", "cursor": padded})
		if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
			t.Fatalf("want resync_required, got %v", err)
		}
		if probe.listHits.Load() != 0 {
			t.Fatal("zero backend")
		}
	})
	t.Run("keyless_initial_actionable_key_error", func(t *testing.T) {
		probe := &commitProbe{}
		d := newCursorDeps(t, listCommitsHandler(probe, nil), &config.Config{CursorKey: nil}, clk)
		d.Config.CursorKey = nil
		_, err := callListCommitsMCPRaw(t, d, map[string]any{"project_id": "42", "use_cursor": true})
		if err == nil || !strings.Contains(err.Error(), "GITLAB_MCP_CURSOR_KEY") {
			t.Fatalf("want actionable key error, got %v", err)
		}
		if strings.Contains(err.Error(), cursor.ResyncRequired) {
			t.Fatalf("initial keyless must not be resync: %v", err)
		}
		if probe.listHits.Load() != 0 || probe.userHits.Load() != 0 {
			t.Fatal("zero backend")
		}
	})
	t.Run("keyless_valid_looking_cursor_key_error", func(t *testing.T) {
		probe := &commitProbe{}
		d := newCursorDeps(t, listCommitsHandler(probe, nil), &config.Config{CursorKey: nil}, clk)
		d.Config.CursorKey = nil
		// Nonempty raw without surrounding whitespace — key gate, not malformed.
		_, err := callListCommitsMCPRaw(t, d, map[string]any{"project_id": "42", "cursor": "v1.not-a-real-token-but-nonempty"})
		if err == nil || !strings.Contains(err.Error(), "GITLAB_MCP_CURSOR_KEY") {
			t.Fatalf("want key error, got %v", err)
		}
		if strings.Contains(err.Error(), "malformed cursor") {
			t.Fatalf("valid-shaped nonempty must reach key gate: %v", err)
		}
		if probe.listHits.Load() != 0 {
			t.Fatal("zero backend")
		}
	})
}

func TestListCommitsScopeEqual(t *testing.T) {
	mr := int64(1)
	pipe := int64(9)
	want := cursor.Scope{Kind: cursor.ScopeProject, ProjectID: "42"}
	cases := []struct {
		name string
		got  cursor.Scope
		eq   bool
	}{
		{"exact", cursor.Scope{Kind: cursor.ScopeProject, ProjectID: "42"}, true},
		{"project_mismatch", cursor.Scope{Kind: cursor.ScopeProject, ProjectID: "99"}, false},
		{"kind_group", cursor.Scope{Kind: cursor.ScopeGroupQueue, GroupID: "10"}, false},
		{"polluted_group_id", cursor.Scope{Kind: cursor.ScopeProject, ProjectID: "42", GroupID: "10"}, false},
		{"polluted_mr", cursor.Scope{Kind: cursor.ScopeProject, ProjectID: "42", MergeRequestIID: &mr}, false},
		{"polluted_pipeline", cursor.Scope{Kind: cursor.ScopeProject, ProjectID: "42", PipelineID: &pipe}, false},
	}
	for _, tc := range cases {
		if got := listCommitsScopeEqual(tc.got, want); got != tc.eq {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.eq)
		}
	}
}

// F5: project/scope binding mismatch must resync before policy denial; same-scope
// same-fingerprint revocation remains authz_denied. All cases use one server instance.
func TestListCommits_projectMismatchRecoveryOrderingMCP(t *testing.T) {
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}

	t.Run("mismatch_project99_denied_project_allowlist_resync", func(t *testing.T) {
		probe := &commitProbe{}
		d := newCursorDeps(t, listCommitsHandler(probe, nil), &config.Config{
			CursorKey:         []byte(testCursorKey),
			AllowedProjectIDs: []string{"42"},
		}, clk)
		out, err := callListCommitsMCPRaw(t, d, map[string]any{"project_id": "42", "use_cursor": true, "per_page": 2})
		if err != nil {
			t.Fatal(err)
		}
		tok := out["section"].(map[string]any)["next_cursor"].(string)
		list0, get0 := probe.listHits.Load(), probe.getCommit.Load()
		proj0 := probe.projectHits.Load()
		_, err = callListCommitsMCPRaw(t, d, map[string]any{"project_id": "99", "cursor": tok, "per_page": 2})
		if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
			t.Fatalf("want resync_required for project scope mismatch, got %v", err)
		}
		if strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
			t.Fatalf("scope mismatch must not surface as authz_denied: %v", err)
		}
		if probe.listHits.Load() != list0 || probe.getCommit.Load() != get0 {
			t.Fatal("mismatch must not continue list/ref")
		}
		// Narrow identity lookup of 99 only; no allowlist re-resolution for policy.
		delta := probe.projectHits.Load() - proj0
		if delta < 1 || delta > 2 {
			t.Fatalf("bounded identity lookup expected, projectHits delta=%d", delta)
		}
	})

	t.Run("mismatch_alias_resolving_99_resync", func(t *testing.T) {
		probe := &commitProbe{}
		d := newCursorDeps(t, listCommitsHandler(probe, nil), &config.Config{
			CursorKey:         []byte(testCursorKey),
			AllowedProjectIDs: []string{"42"},
		}, clk)
		out, err := callListCommitsMCPRaw(t, d, map[string]any{"project_id": "42", "use_cursor": true, "per_page": 2})
		if err != nil {
			t.Fatal(err)
		}
		tok := out["section"].(map[string]any)["next_cursor"].(string)
		list0, get0 := probe.listHits.Load(), probe.getCommit.Load()
		_, err = callListCommitsMCPRaw(t, d, map[string]any{"project_id": "other/proj", "cursor": tok, "per_page": 2})
		if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
			t.Fatalf("want resync_required for alias→99 scope mismatch, got %v", err)
		}
		if strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
			t.Fatalf("alias mismatch must not surface as authz_denied: %v", err)
		}
		if probe.listHits.Load() != list0 || probe.getCommit.Load() != get0 {
			t.Fatal("alias mismatch must not continue list/ref")
		}
	})

	t.Run("mismatch_project99_denied_group_policy_resync", func(t *testing.T) {
		probe := &commitProbe{}
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			probe.note(r)
			w.Header().Set("Content-Type", "application/json")
			path := r.URL.Path
			switch {
			case strings.HasSuffix(path, "/user"):
				_, _ = io.WriteString(w, `{"id":7,"username":"alice"}`)
			case strings.Contains(path, "/groups/10") || strings.HasSuffix(path, "/groups/acme"):
				_, _ = io.WriteString(w, `{"id":10,"full_path":"acme","parent_id":0}`)
			case strings.Contains(path, "/repository/commits/") && !strings.HasSuffix(path, "/commits"):
				_, _ = io.WriteString(w, commitJSON(tipSHA))
			case strings.HasSuffix(path, "/repository/commits"):
				w.Header().Set("X-Next-Page", "2")
				_, _ = io.WriteString(w, pageCommits(sha1, sha2))
			case strings.Contains(path, "/projects/"):
				id, nsID, nsPath := int64(42), int64(10), "acme"
				if strings.Contains(path, "/projects/99") ||
					strings.Contains(path, "other%2Fproj") ||
					strings.Contains(path, "/projects/other/proj") {
					id, nsID, nsPath = 99, 99, "other"
				}
				_, _ = io.WriteString(w, fmt.Sprintf(
					`{"id":%d,"path_with_namespace":%q,"namespace":{"id":%d,"kind":"group","full_path":%q,"parent_id":0}}`,
					id, nsPath+"/p", nsID, nsPath))
			default:
				_, _ = io.WriteString(w, `{}`)
			}
		})
		d := newCursorDeps(t, h, &config.Config{
			CursorKey:       []byte(testCursorKey),
			AllowedGroupIDs: []string{"10"},
		}, clk)
		out, err := callListCommitsMCPRaw(t, d, map[string]any{"project_id": "42", "use_cursor": true, "per_page": 2})
		if err != nil {
			t.Fatal(err)
		}
		tok := out["section"].(map[string]any)["next_cursor"].(string)
		list0, get0 := probe.listHits.Load(), probe.getCommit.Load()
		_, err = callListCommitsMCPRaw(t, d, map[string]any{"project_id": "99", "cursor": tok, "per_page": 2})
		if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
			t.Fatalf("want resync_required under denied group policy, got %v", err)
		}
		if strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
			t.Fatalf("group-policy-denied mismatch must not surface as authz_denied: %v", err)
		}
		if probe.listHits.Load() != list0 || probe.getCommit.Load() != get0 {
			t.Fatal("group-policy mismatch must not continue list/ref")
		}
	})

	t.Run("same_scope_path_allowlist_remap_authz_denied", func(t *testing.T) {
		// Fingerprint stays on raw token "group/proj"; live resolve remaps away from 42.
		probe := &commitProbe{}
		var remapped atomic.Bool
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			probe.note(r)
			w.Header().Set("Content-Type", "application/json")
			path := r.URL.Path
			switch {
			case strings.HasSuffix(path, "/user"):
				_, _ = io.WriteString(w, `{"id":7,"username":"alice"}`)
			case strings.Contains(path, "/repository/commits/") && !strings.HasSuffix(path, "/commits"):
				_, _ = io.WriteString(w, commitJSON(tipSHA))
			case strings.HasSuffix(path, "/repository/commits"):
				w.Header().Set("X-Next-Page", "2")
				_, _ = io.WriteString(w, pageCommits(sha1, sha2))
			case strings.Contains(path, "/projects/"):
				id := int64(42)
				ns := "group/proj"
				isPathAlias := strings.Contains(path, "group%2Fproj") || strings.Contains(path, "/projects/group/proj")
				if remapped.Load() && isPathAlias {
					id, ns = 99, "other/proj"
				} else if strings.Contains(path, "/projects/99") {
					id, ns = 99, "other/proj"
				}
				_, _ = io.WriteString(w, fmt.Sprintf(
					`{"id":%d,"path_with_namespace":%q,"namespace":{"id":9,"kind":"group","full_path":"group","parent_id":0}}`,
					id, ns))
			default:
				_, _ = io.WriteString(w, `{}`)
			}
		})
		d := newCursorDeps(t, h, &config.Config{
			CursorKey:         []byte(testCursorKey),
			AllowedProjectIDs: []string{"group/proj"},
		}, clk)
		fpBefore := d.Config.PolicyFingerprint()
		out, err := callListCommitsMCPRaw(t, d, map[string]any{"project_id": "42", "use_cursor": true, "per_page": 2})
		if err != nil {
			t.Fatal(err)
		}
		tok := out["section"].(map[string]any)["next_cursor"].(string)
		list0, get0 := probe.listHits.Load(), probe.getCommit.Load()
		remapped.Store(true)
		if d.Config.PolicyFingerprint() != fpBefore {
			t.Fatal("test bug: fingerprint must stay unchanged")
		}
		_, err = callListCommitsMCPRaw(t, d, map[string]any{"project_id": "42", "cursor": tok, "per_page": 2})
		if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
			t.Fatalf("want authz_denied for same-scope path allowlist remap, got %v", err)
		}
		if strings.Contains(err.Error(), cursor.ResyncRequired) {
			t.Fatalf("same-scope revocation must not surface as resync: %v", err)
		}
		if probe.listHits.Load() != list0 || probe.getCommit.Load() != get0 {
			t.Fatal("same-scope project revocation must not continue list/ref")
		}
	})

	t.Run("same_scope_group_ancestry_revocation_authz_denied", func(t *testing.T) {
		probe := &commitProbe{}
		var moved atomic.Bool
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			probe.note(r)
			w.Header().Set("Content-Type", "application/json")
			path := r.URL.Path
			switch {
			case strings.HasSuffix(path, "/user"):
				_, _ = io.WriteString(w, `{"id":7,"username":"alice"}`)
			case strings.Contains(path, "/groups/10") || strings.HasSuffix(path, "/groups/acme"):
				_, _ = io.WriteString(w, `{"id":10,"full_path":"acme","parent_id":0}`)
			case strings.Contains(path, "/groups/99"):
				_, _ = io.WriteString(w, `{"id":99,"full_path":"other","parent_id":0}`)
			case strings.Contains(path, "/repository/commits/") && !strings.HasSuffix(path, "/commits"):
				_, _ = io.WriteString(w, commitJSON(tipSHA))
			case strings.HasSuffix(path, "/repository/commits"):
				w.Header().Set("X-Next-Page", "2")
				_, _ = io.WriteString(w, pageCommits(sha1, sha2))
			case strings.Contains(path, "/projects/"):
				nsID, nsPath := int64(10), "acme"
				if moved.Load() {
					nsID, nsPath = 99, "other"
				}
				_, _ = io.WriteString(w, fmt.Sprintf(
					`{"id":42,"path_with_namespace":%q,"namespace":{"id":%d,"kind":"group","full_path":%q,"parent_id":0}}`,
					nsPath+"/p", nsID, nsPath))
			default:
				_, _ = io.WriteString(w, `{}`)
			}
		})
		d := newCursorDeps(t, h, &config.Config{
			CursorKey:       []byte(testCursorKey),
			AllowedGroupIDs: []string{"10"},
		}, clk)
		fpBefore := d.Config.PolicyFingerprint()
		out, err := callListCommitsMCPRaw(t, d, map[string]any{"project_id": "42", "use_cursor": true, "per_page": 2})
		if err != nil {
			t.Fatal(err)
		}
		tok := out["section"].(map[string]any)["next_cursor"].(string)
		list0, get0 := probe.listHits.Load(), probe.getCommit.Load()
		moved.Store(true)
		if d.Config.PolicyFingerprint() != fpBefore {
			t.Fatal("test bug: fingerprint must stay unchanged")
		}
		_, err = callListCommitsMCPRaw(t, d, map[string]any{"project_id": "42", "cursor": tok, "per_page": 2})
		if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
			t.Fatalf("want authz_denied for same-scope group ancestry revocation, got %v", err)
		}
		if probe.listHits.Load() != list0 || probe.getCommit.Load() != get0 {
			t.Fatal("group ancestry revocation must not continue list/ref")
		}
	})

	t.Run("alias_equivalence_42_allowlist_success", func(t *testing.T) {
		probe := &commitProbe{}
		d := newCursorDeps(t, listCommitsHandler(probe, nil), &config.Config{
			CursorKey:         []byte(testCursorKey),
			AllowedProjectIDs: []string{"42"},
		}, clk)
		out, err := callListCommitsMCPRaw(t, d, map[string]any{"project_id": "group/proj", "use_cursor": true, "per_page": 2})
		if err != nil {
			t.Fatal(err)
		}
		tok := out["section"].(map[string]any)["next_cursor"].(string)
		p, err := cursor.Decode([]byte(testCursorKey), tok, clk.Now())
		if err != nil {
			t.Fatal(err)
		}
		if p.Scope.ProjectID != "42" {
			t.Fatalf("cursor must bind canonical 42, got %q", p.Scope.ProjectID)
		}
		list0 := probe.listHits.Load()
		out2, err := callListCommitsMCPRaw(t, d, map[string]any{"project_id": "group/proj", "cursor": tok, "per_page": 2})
		if err != nil {
			t.Fatalf("alias→42 resume must succeed: %v", err)
		}
		if out2["section"].(map[string]any)["next_cursor"] == nil && out2["section"].(map[string]any)["pagination_exhausted"] != true {
			t.Fatalf("expected page-2 progress: %#v", out2["section"])
		}
		if probe.listHits.Load() <= list0 {
			t.Fatal("alias resume must list (guard+next)")
		}
	})

	t.Run("actor_mismatch_on_revoked_scope_resync_before_policy", func(t *testing.T) {
		probe := &commitProbe{}
		var moved atomic.Bool
		var actorID atomic.Int64
		actorID.Store(7)
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			probe.note(r)
			w.Header().Set("Content-Type", "application/json")
			path := r.URL.Path
			switch {
			case strings.HasSuffix(path, "/user"):
				_, _ = io.WriteString(w, fmt.Sprintf(`{"id":%d,"username":"alice"}`, actorID.Load()))
			case strings.Contains(path, "/groups/10") || strings.HasSuffix(path, "/groups/acme"):
				_, _ = io.WriteString(w, `{"id":10,"full_path":"acme","parent_id":0}`)
			case strings.Contains(path, "/groups/99"):
				_, _ = io.WriteString(w, `{"id":99,"full_path":"other","parent_id":0}`)
			case strings.Contains(path, "/repository/commits/") && !strings.HasSuffix(path, "/commits"):
				_, _ = io.WriteString(w, commitJSON(tipSHA))
			case strings.HasSuffix(path, "/repository/commits"):
				w.Header().Set("X-Next-Page", "2")
				_, _ = io.WriteString(w, pageCommits(sha1, sha2))
			case strings.Contains(path, "/projects/"):
				nsID, nsPath := int64(10), "acme"
				if moved.Load() {
					nsID, nsPath = 99, "other"
				}
				_, _ = io.WriteString(w, fmt.Sprintf(
					`{"id":42,"path_with_namespace":%q,"namespace":{"id":%d,"kind":"group","full_path":%q,"parent_id":0}}`,
					nsPath+"/p", nsID, nsPath))
			default:
				_, _ = io.WriteString(w, `{}`)
			}
		})
		d := newCursorDeps(t, h, &config.Config{
			CursorKey:       []byte(testCursorKey),
			AllowedGroupIDs: []string{"10"},
		}, clk)
		out, err := callListCommitsMCPRaw(t, d, map[string]any{"project_id": "42", "use_cursor": true, "per_page": 2})
		if err != nil {
			t.Fatal(err)
		}
		tok := out["section"].(map[string]any)["next_cursor"].(string)
		list0, get0 := probe.listHits.Load(), probe.getCommit.Load()
		proj0 := probe.projectHits.Load()
		moved.Store(true)
		actorID.Store(8) // different actor on same instance; must win over revoked-scope denial
		user0 := probe.userHits.Load()
		_, err = callListCommitsMCPRaw(t, d, map[string]any{"project_id": "42", "cursor": tok, "per_page": 2})
		if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
			t.Fatalf("want resync_required for actor mismatch on revoked scope, got %v", err)
		}
		if strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
			t.Fatalf("actor mismatch must not be masked by authz_denied: %v", err)
		}
		if probe.userHits.Load() <= user0 {
			t.Fatal("actor check must reach /user on same instance")
		}
		if probe.listHits.Load() != list0 || probe.getCommit.Load() != get0 {
			t.Fatal("actor mismatch must not continue list/ref")
		}
		if probe.projectHits.Load() != proj0 {
			t.Fatal("actor mismatch must precede project identity/policy")
		}
	})

	t.Run("filter_mismatch_on_revoked_scope_resync_before_policy", func(t *testing.T) {
		probe := &commitProbe{}
		var moved atomic.Bool
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			probe.note(r)
			w.Header().Set("Content-Type", "application/json")
			path := r.URL.Path
			switch {
			case strings.HasSuffix(path, "/user"):
				_, _ = io.WriteString(w, `{"id":7,"username":"alice"}`)
			case strings.Contains(path, "/groups/10") || strings.HasSuffix(path, "/groups/acme"):
				_, _ = io.WriteString(w, `{"id":10,"full_path":"acme","parent_id":0}`)
			case strings.Contains(path, "/groups/99"):
				_, _ = io.WriteString(w, `{"id":99,"full_path":"other","parent_id":0}`)
			case strings.Contains(path, "/repository/commits/") && !strings.HasSuffix(path, "/commits"):
				_, _ = io.WriteString(w, commitJSON(tipSHA))
			case strings.HasSuffix(path, "/repository/commits"):
				w.Header().Set("X-Next-Page", "2")
				_, _ = io.WriteString(w, pageCommits(sha1, sha2))
			case strings.Contains(path, "/projects/"):
				nsID, nsPath := int64(10), "acme"
				if moved.Load() {
					nsID, nsPath = 99, "other"
				}
				_, _ = io.WriteString(w, fmt.Sprintf(
					`{"id":42,"path_with_namespace":%q,"namespace":{"id":%d,"kind":"group","full_path":%q,"parent_id":0}}`,
					nsPath+"/p", nsID, nsPath))
			default:
				_, _ = io.WriteString(w, `{}`)
			}
		})
		d := newCursorDeps(t, h, &config.Config{
			CursorKey:       []byte(testCursorKey),
			AllowedGroupIDs: []string{"10"},
		}, clk)
		out, err := callListCommitsMCPRaw(t, d, map[string]any{"project_id": "42", "use_cursor": true, "per_page": 2})
		if err != nil {
			t.Fatal(err)
		}
		tok := out["section"].(map[string]any)["next_cursor"].(string)
		list0, get0 := probe.listHits.Load(), probe.getCommit.Load()
		moved.Store(true)
		_, err = callListCommitsMCPRaw(t, d, map[string]any{
			"project_id": "42", "cursor": tok, "per_page": 2, "path": "changed/filter",
		})
		if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
			t.Fatalf("want resync_required for filter mismatch on revoked scope, got %v", err)
		}
		if strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
			t.Fatalf("filter mismatch must not be masked by authz_denied: %v", err)
		}
		if probe.listHits.Load() != list0 || probe.getCommit.Load() != get0 {
			t.Fatal("filter mismatch must not continue list/ref")
		}
	})
}

func TestListCommits_omittedPagePerPageMCP(t *testing.T) {
	probe := &commitProbe{}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, listCommitsHandler(probe, nil), &config.Config{CursorKey: []byte(testCursorKey)}, clk)

	t.Run("omit_page_and_per_page_initial", func(t *testing.T) {
		out, err := callListCommitsMCPRaw(t, d, map[string]any{"project_id": "42", "use_cursor": true})
		if err != nil {
			t.Fatalf("omitted page/per_page must be accepted: %v", err)
		}
		nc, _ := out["section"].(map[string]any)["next_cursor"].(string)
		if nc == "" {
			t.Fatal("expected next_cursor with default per_page")
		}
		p, err := cursor.Decode([]byte(testCursorKey), nc, clk.Now())
		if err != nil {
			t.Fatal(err)
		}
		if p.Filters.PerPage != 20 {
			t.Fatalf("default per_page want 20 got %d", p.Filters.PerPage)
		}
		if p.PageState.PerPage != 20 {
			t.Fatalf("page state per_page=%d", p.PageState.PerPage)
		}
	})
	t.Run("omit_page_explicit_per_page_resume", func(t *testing.T) {
		out, err := callListCommitsMCPRaw(t, d, map[string]any{"project_id": "42", "use_cursor": true, "per_page": 2})
		if err != nil {
			t.Fatal(err)
		}
		nc := out["section"].(map[string]any)["next_cursor"].(string)
		out2, err := callListCommitsMCPRaw(t, d, map[string]any{"project_id": "42", "cursor": nc, "per_page": 2})
		if err != nil {
			t.Fatalf("resume omitting page: %v", err)
		}
		if out2["section"].(map[string]any)["next_cursor"] == nil && out2["section"].(map[string]any)["pagination_exhausted"] != true {
			// page 2 may have next or exhaust depending on fixture; just ensure CallTool succeeded
		}
	})
	t.Run("invalid_page_with_use_cursor", func(t *testing.T) {
		_, err := callListCommitsMCPRaw(t, d, map[string]any{"project_id": "42", "use_cursor": true, "page": 2, "per_page": 2})
		if err == nil {
			t.Fatal("page>1 with use_cursor must reject")
		}
	})
	t.Run("legacy_omit_defaults", func(t *testing.T) {
		out, err := callListCommitsMCPRaw(t, d, map[string]any{"project_id": "42"})
		if err != nil {
			t.Fatalf("legacy omit page/per_page: %v", err)
		}
		if _, ok := out["pagination"].(map[string]any); !ok {
			t.Fatalf("legacy pagination missing: %#v", out)
		}
	})
}
