package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

const queueTestCursorKey = "0123456789abcdef0123456789abcdef"

func queueDeps(t *testing.T, h http.Handler, cfg *config.Config) Deps {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.Token = "t"
	cfg.APIURL = ts.URL + "/api/v4"
	if len(cfg.CursorKey) == 0 {
		cfg.CursorKey = []byte(queueTestCursorKey)
	}
	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(cfg.APIURL),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(igl.BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	return Deps{Config: cfg, Client: cli, Clock: &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}}
}

func callReviewQueue(t *testing.T, d Deps, args map[string]any) (map[string]any, error) {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "q", Version: "t"}, nil)
	RegisterMergeRequests(srv, d)
	cs := testutil.MCPConnect(t, srv)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: cursor.ToolReviewQueue, Arguments: args})
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

func confToolErr(res *mcp.CallToolResult) error {
	if res == nil {
		return io.ErrUnexpectedEOF
	}
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			return &toolTextError{s: t.Text}
		}
	}
	return io.ErrUnexpectedEOF
}

type toolTextError struct{ s string }

func (e *toolTextError) Error() string { return e.s }

func toolJSON(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	t.Fatal("no text content")
	return ""
}

func sectionMap(out map[string]any) map[string]any {
	sec, _ := out["section"].(map[string]any)
	return sec
}

func limitationCodes(sec map[string]any) []string {
	var codes []string
	lims, _ := sec["limitations"].([]any)
	for _, lim := range lims {
		m, _ := lim.(map[string]any)
		if c, _ := m["code"].(string); c != "" {
			codes = append(codes, c)
		}
	}
	return codes
}

func hasCode(codes []string, want string) bool {
	for _, c := range codes {
		if c == want {
			return true
		}
	}
	return false
}

func TestReviewQueue_missingCursorKey(t *testing.T) {
	d := queueDeps(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("no transport")
	}), &config.Config{CursorKey: nil})
	d.Config.CursorKey = nil
	_, err := callReviewQueue(t, d, map[string]any{"group_id": "9", "kinds": []any{"reviewer"}})
	if err == nil || !strings.Contains(err.Error(), "GITLAB_MCP_CURSOR_KEY") {
		t.Fatalf("got %v", err)
	}
}

func TestReviewQueue_defaultActorWireScopeAll(t *testing.T) {
	var sawScope, sawReviewer atomic.Bool
	var listHits, getMRHits atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/user":
			_, _ = io.WriteString(w, `{"id":7,"username":"me"}`)
		case strings.HasPrefix(r.URL.Path, "/api/v4/groups/") && strings.HasSuffix(r.URL.Path, "/merge_requests"):
			listHits.Add(1)
			q := r.URL.Query()
			if q.Get("scope") == "all" {
				sawScope.Store(true)
			}
			if q.Get("reviewer_id") == "7" {
				sawReviewer.Store(true)
			}
			w.Header().Set("X-Next-Page", "")
			_, _ = io.WriteString(w, `[{"iid":1,"project_id":42,"updated_at":"2026-10-03T11:00:00Z","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","state":"opened","source_project_id":42}]`)
		case strings.HasSuffix(r.URL.Path, "/merge_requests/1"):
			getMRHits.Add(1)
			_, _ = io.WriteString(w, `{"id":1,"iid":1,"project_id":42,"state":"opened","source_project_id":42,"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","updated_at":"2026-10-03T11:00:00Z"}`)
		case r.URL.Path == "/api/v4/projects/42":
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":9,"kind":"group"}}`)
		case r.URL.Path == "/api/v4/groups/9":
			_, _ = io.WriteString(w, `{"id":9,"full_path":"g","parent_id":0}`)
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
		}
	})
	d := queueDeps(t, h, &config.Config{AllowedGroupIDs: []string{"9"}})
	out, err := callReviewQueue(t, d, map[string]any{
		"group_id": "9", "kinds": []any{"reviewer"}, "page_size": 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawScope.Load() || !sawReviewer.Load() {
		t.Fatalf("wire filters missing scope/reviewer_id")
	}
	sec := sectionMap(out)
	for i := 0; i < 5; i++ {
		items, _ := out["items"].([]any)
		if len(items) > 0 {
			it := items[0].(map[string]any)
			if int64(it["project_id"].(float64)) != 42 {
				t.Fatalf("%v", it)
			}
			kinds := it["kinds"].([]any)
			if len(kinds) != 1 || kinds[0].(string) != "reviewer" {
				t.Fatalf("kinds=%v", kinds)
			}
			if listHits.Load() < 1 || getMRHits.Load() < 1 {
				t.Fatalf("backend counts list=%d getMR=%d", listHits.Load(), getMRHits.Load())
			}
			if sec["consistency"] != readmeta.ConsistencyUnknown {
				t.Fatalf("moving queue must be unknown, got %v", sec["consistency"])
			}
			if !hasCode(limitationCodes(sec), readmeta.CodeInconsistent) {
				t.Fatalf("want moving limitation: %v", sec["limitations"])
			}
			return
		}
		nc, _ := sec["next_cursor"].(string)
		if nc == "" {
			break
		}
		out, err = callReviewQueue(t, d, map[string]any{
			"group_id": "9", "kinds": []any{"reviewer"}, "page_size": 20, "cursor": nc,
		})
		if err != nil {
			t.Fatal(err)
		}
		sec = sectionMap(out)
	}
	t.Fatal("expected emitted items")
}

func TestReviewQueue_explicitActorAndOverlapLabels(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/user":
			_, _ = io.WriteString(w, `{"id":1}`)
		case r.URL.Path == "/api/v4/users/99":
			_, _ = io.WriteString(w, `{"id":99}`)
		case strings.Contains(r.URL.Path, "/merge_requests") && !strings.Contains(r.URL.Path, "/merge_requests/3") && r.URL.Query().Get("reviewer_id") == "99":
			w.Header().Set("X-Next-Page", "")
			_, _ = io.WriteString(w, `[{"iid":3,"project_id":42,"updated_at":"2026-10-03T11:00:00Z","sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]`)
		case strings.Contains(r.URL.Path, "/merge_requests") && !strings.Contains(r.URL.Path, "/merge_requests/3") && r.URL.Query().Get("author_id") == "99":
			w.Header().Set("X-Next-Page", "")
			_, _ = io.WriteString(w, `[{"iid":3,"project_id":42,"updated_at":"2026-10-03T11:00:00Z","sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]`)
		case strings.HasSuffix(r.URL.Path, "/merge_requests/3"):
			_, _ = io.WriteString(w, `{"id":3,"iid":3,"project_id":42,"state":"opened","source_project_id":42,"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","updated_at":"2026-10-03T11:00:00Z"}`)
		case r.URL.Path == "/api/v4/projects/42":
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":9,"kind":"group"}}`)
		case r.URL.Path == "/api/v4/groups/9":
			_, _ = io.WriteString(w, `{"id":9,"full_path":"g","parent_id":0}`)
		default:
			http.NotFound(w, r)
		}
	})
	d := queueDeps(t, h, &config.Config{AllowedGroupIDs: []string{"9"}})
	out := drainQueue(t, d, map[string]any{
		"group_id": "9", "kinds": []any{"authored", "reviewer"}, "actor_id": 99, "page_size": 10,
	})
	items := out["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items=%v", items)
	}
	kinds := items[0].(map[string]any)["kinds"].([]any)
	joined := ""
	for _, k := range kinds {
		joined += k.(string) + ","
	}
	if !strings.Contains(joined, "authored") || !strings.Contains(joined, "reviewer") {
		t.Fatalf("want both labels, got %v", kinds)
	}
}

func TestReviewQueue_reviewerOnlyNoAuthored(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/user":
			_, _ = io.WriteString(w, `{"id":7}`)
		case strings.HasPrefix(r.URL.Path, "/api/v4/groups/") && strings.HasSuffix(r.URL.Path, "/merge_requests"):
			w.Header().Set("X-Next-Page", "")
			_, _ = io.WriteString(w, `[{"iid":1,"project_id":42,"updated_at":"2026-10-03T11:00:00Z","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`)
		case strings.HasSuffix(r.URL.Path, "/merge_requests/1"):
			_, _ = io.WriteString(w, `{"id":1,"iid":1,"project_id":42,"state":"opened","source_project_id":42,"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","updated_at":"2026-10-03T11:00:00Z"}`)
		case r.URL.Path == "/api/v4/projects/42":
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":9,"kind":"group"}}`)
		case r.URL.Path == "/api/v4/groups/9":
			_, _ = io.WriteString(w, `{"id":9,"full_path":"g","parent_id":0}`)
		default:
			http.NotFound(w, r)
		}
	})
	d := queueDeps(t, h, &config.Config{AllowedGroupIDs: []string{"9"}})
	out := drainQueue(t, d, map[string]any{"group_id": "9", "kinds": []any{"reviewer"}})
	kinds := out["items"].([]any)[0].(map[string]any)["kinds"].([]any)
	for _, k := range kinds {
		if k.(string) == "authored" {
			t.Fatal("authored must not contaminate reviewer-only")
		}
	}
}

func TestReviewQueue_seedlessOngoingUnsupported(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/user":
			_, _ = io.WriteString(w, `{"id":7}`)
		case r.URL.Path == "/api/v4/groups/9":
			_, _ = io.WriteString(w, `{"id":9,"full_path":"g","parent_id":0}`)
		default:
			http.NotFound(w, r)
		}
	})
	d := queueDeps(t, h, &config.Config{AllowedGroupIDs: []string{"9"}})
	out := drainQueue(t, d, map[string]any{"group_id": "9", "kinds": []any{"ongoing"}})
	sec := sectionMap(out)
	if sec["content_complete"] != readmeta.ContentCompleteFalse {
		t.Fatalf("%v", sec)
	}
	if !hasCode(limitationCodes(sec), readmeta.CodeUnsupported) {
		t.Fatalf("want unsupported: %v", sec["limitations"])
	}
	if items, _ := out["items"].([]any); len(items) != 0 {
		t.Fatalf("items=%v", items)
	}
}

func TestReviewQueue_ongoingPredicate(t *testing.T) {
	var discHits, getMRHits atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/user":
			_, _ = io.WriteString(w, `{"id":7}`)
		case r.URL.Path == "/api/v4/groups/9":
			_, _ = io.WriteString(w, `{"id":9,"full_path":"g","parent_id":0}`)
		case r.URL.Path == "/api/v4/projects/42":
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":9,"kind":"group"}}`)
		case strings.HasSuffix(r.URL.Path, "/merge_requests/5"):
			getMRHits.Add(1)
			_, _ = io.WriteString(w, `{"id":1,"iid":5,"project_id":42,"state":"opened","source_project_id":42,"sha":"cccccccccccccccccccccccccccccccccccccccc","updated_at":"2026-10-03T10:00:00Z"}`)
		case strings.Contains(r.URL.Path, "/discussions"):
			discHits.Add(1)
			w.Header().Set("X-Next-Page", "")
			_, _ = io.WriteString(w, `[{"id":"d1","notes":[{"id":1,"system":false,"body":"please fix","author":{"id":7}},{"id":2,"system":true,"body":"changed","author":{"id":7}}]}]`)
		default:
			http.NotFound(w, r)
		}
	})
	d := queueDeps(t, h, &config.Config{AllowedGroupIDs: []string{"9"}, AllowedProjectIDs: []string{"42"}})
	out := drainQueue(t, d, map[string]any{
		"group_id": "9", "kinds": []any{"ongoing"},
		"known_mrs": []any{map[string]any{"project_id": "42", "iid": 5}},
	})
	items, _ := out["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("want ongoing item, got items=%v section=%v", out["items"], out["section"])
	}
	if discHits.Load() < 1 || getMRHits.Load() < 1 {
		t.Fatalf("disc=%d getMR=%d", discHits.Load(), getMRHits.Load())
	}
}

func TestNoteQualifiesOngoingRaw(t *testing.T) {
	if noteQualifiesOngoingRaw([]byte(`{"system":false,"body":"x","author":{"id":7}}`), 7) != true {
		t.Fatal("want qualify")
	}
	if noteQualifiesOngoingRaw([]byte(`{"body":"x","author":{"id":7}}`), 7) {
		t.Fatal("missing system must not qualify")
	}
	if noteQualifiesOngoingRaw([]byte(`{"system":null,"body":"x","author":{"id":7}}`), 7) {
		t.Fatal("null system must not qualify")
	}
	if noteQualifiesOngoingRaw([]byte(`{"system":true,"body":"x","author":{"id":7}}`), 7) {
		t.Fatal("system-only")
	}
	if noteQualifiesOngoingRaw([]byte(`{"system":false,"body":"","author":{"id":7}}`), 7) {
		t.Fatal("blank body")
	}
	if noteQualifiesOngoingRaw([]byte(`{"system":false,"body":"x","author":{"id":8}}`), 7) {
		t.Fatal("other author")
	}
}

func TestReviewQueue_unknownActorZeroList(t *testing.T) {
	var lists atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/user":
			_, _ = io.WriteString(w, `{"id":1}`)
		case r.URL.Path == "/api/v4/users/404":
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/merge_requests"):
			lists.Add(1)
			_, _ = io.WriteString(w, `[]`)
		case r.URL.Path == "/api/v4/groups/9":
			_, _ = io.WriteString(w, `{"id":9,"full_path":"g","parent_id":0}`)
		default:
			http.NotFound(w, r)
		}
	})
	d := queueDeps(t, h, &config.Config{AllowedGroupIDs: []string{"9"}})
	_, err := callReviewQueue(t, d, map[string]any{"group_id": "9", "kinds": []any{"reviewer"}, "actor_id": 404})
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeIdentityUnresolved) {
		t.Fatalf("got %v", err)
	}
	if lists.Load() != 0 {
		t.Fatalf("list calls=%d", lists.Load())
	}
}

func drainQueue(t *testing.T, d Deps, args map[string]any) map[string]any {
	t.Helper()
	const maxResume = 40
	seen := map[string]string{}
	var accumulated []any
	var out map[string]any
	var err error
	args2 := map[string]any{}
	for k, v := range args {
		args2[k] = v
	}
	for i := 0; i < maxResume; i++ {
		out, err = callReviewQueue(t, d, args2)
		if err != nil {
			t.Fatal(err)
		}
		rawItems, _ := out["items"].([]any)
		for _, item := range rawItems {
			m := item.(map[string]any)
			key := strconv.FormatInt(int64(m["project_id"].(float64)), 10) + ":" + strconv.FormatInt(int64(m["iid"].(float64)), 10)
			labels := kindLabel(m["kinds"])
			if prev, ok := seen[key]; ok && prev != labels {
				t.Fatalf("canonical key %s changed labels %s -> %s", key, prev, labels)
			}
			if _, ok := seen[key]; ok {
				t.Fatalf("duplicate canonical key %s", key)
			}
			seen[key] = labels
			accumulated = append(accumulated, item)
		}
		nc, _ := sectionMap(out)["next_cursor"].(string)
		if nc == "" {
			out["items"] = accumulated
			return out
		}
		args2["cursor"] = nc
	}
	t.Fatalf("review queue did not reach a terminal page within %d resumes", maxResume)
	return nil
}

func kindLabel(v any) string {
	items, _ := v.([]any)
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, fmt.Sprint(it))
	}
	return strings.Join(parts, ",")
}
