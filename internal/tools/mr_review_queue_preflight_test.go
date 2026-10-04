package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
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

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// countingQueueTransport records actual HTTP RoundTrips for zero-transport assertions.
type countingQueueTransport struct {
	hits, userHits, groupHits, listHits atomic.Int32
	userID                              int64
}

func (t *countingQueueTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.hits.Add(1)
	body := ""
	hdr := http.Header{"Content-Type": []string{"application/json"}}
	switch {
	case r.URL.Path == "/api/v4/user":
		t.userHits.Add(1)
		id := t.userID
		if id == 0 {
			id = 7
		}
		body = `{"id":` + itoa64(id) + `}`
	case r.URL.Path == "/api/v4/users/99":
		return &http.Response{StatusCode: 404, Status: "404 Not Found", Header: hdr, Body: io.NopCloser(strings.NewReader(`{"message":"404"}`)), Request: r}, nil
	case r.URL.Path == "/api/v4/groups/9":
		t.groupHits.Add(1)
		body = `{"id":9,"full_path":"g","parent_id":0}`
	case strings.HasPrefix(r.URL.Path, "/api/v4/groups/") && strings.HasSuffix(r.URL.Path, "/merge_requests"):
		t.listHits.Add(1)
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			if n, err := strconv.Atoi(p); err == nil && n > 0 {
				page = n
			}
		}
		hdr.Set("X-Next-Page", strconv.Itoa(page+1))
		body = `[{"iid":1,"project_id":42,"updated_at":"2026-10-03T11:00:00Z","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`
	case strings.HasSuffix(r.URL.Path, "/merge_requests/1"):
		body = `{"id":1,"iid":1,"project_id":42,"state":"opened","source_project_id":42,"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","updated_at":"2026-10-03T11:00:00Z"}`
	case r.URL.Path == "/api/v4/projects/42":
		body = `{"id":42,"path_with_namespace":"g/p","namespace":{"id":9,"kind":"group"}}`
	default:
		return &http.Response{StatusCode: 404, Status: "404 Not Found", Header: hdr, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
	}
	return &http.Response{StatusCode: 200, Status: "200 OK", Header: hdr, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func itoa64(v int64) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(
		jsonNumber(v), " ", ""), "+", ""))
}

func jsonNumber(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func queueDepsCounting(t *testing.T, tr *countingQueueTransport, cfg *config.Config) Deps {
	t.Helper()
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.Token = "t"
	if cfg.APIURL == "" {
		cfg.APIURL = "http://fixture.invalid/api/v4"
	}
	if len(cfg.CursorKey) == 0 {
		cfg.CursorKey = []byte(queueTestCursorKey)
	}
	if len(cfg.AllowedGroupIDs) == 0 {
		cfg.AllowedGroupIDs = []string{"9"}
	}
	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(cfg.APIURL),
		gitlab.WithoutRetries(),
		gitlab.WithHTTPClient(&http.Client{Transport: tr}),
		gitlab.WithInterceptor(igl.BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	return Deps{Config: cfg, Client: cli, Clock: &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}}
}

func mintQueueContinuation(t *testing.T, d Deps) string {
	t.Helper()
	out, err := callReviewQueue(t, d, map[string]any{
		"group_id": "9", "kinds": []any{"reviewer"}, "page_size": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	nc, _ := sectionMap(out)["next_cursor"].(string)
	if nc == "" {
		t.Fatal("want continuation cursor")
	}
	return nc
}

func callReviewQueueResult(t *testing.T, d Deps, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "q", Version: "t"}, nil)
	RegisterMergeRequests(srv, d)
	cs := testutil.MCPConnect(t, srv)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: cursor.ToolReviewQueue, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestReviewQueue_preflightChangedBoundZeroTransport(t *testing.T) {
	tr := &countingQueueTransport{}
	d := queueDepsCounting(t, tr, &config.Config{})
	tok := mintQueueContinuation(t, d)
	before := tr.hits.Load()
	res := callReviewQueueResult(t, d, map[string]any{
		"group_id": "9", "kinds": []any{"reviewer"}, "page_size": 1,
		"cursor": tok, "updated_before": "2026-10-03T10:00:00Z",
	})
	if !res.IsError {
		t.Fatal("changed updated_before must resync")
	}
	if tr.hits.Load() != before {
		t.Fatalf("changed bound must be zero-transport: before=%d after=%d", before, tr.hits.Load())
	}
	if tr.listHits.Load() < 1 {
		t.Fatal("mint must have listed")
	}
}

func TestReviewQueue_preflightChangedSelectionPolicyInstanceZeroTransport(t *testing.T) {
	tr := &countingQueueTransport{}
	cfg := &config.Config{AllowedProjectIDs: []string{"42"}}
	d := queueDepsCounting(t, tr, cfg)
	tok := mintQueueContinuation(t, d)

	t.Run("kinds", func(t *testing.T) {
		before := tr.hits.Load()
		res := callReviewQueueResult(t, d, map[string]any{
			"group_id": "9", "kinds": []any{"authored"}, "page_size": 1, "cursor": tok,
		})
		if !res.IsError {
			t.Fatal("changed kinds must resync")
		}
		if tr.hits.Load() != before {
			t.Fatalf("selection mismatch transport before=%d after=%d", before, tr.hits.Load())
		}
	})

	t.Run("policy", func(t *testing.T) {
		before := tr.hits.Load()
		cfg2 := *cfg
		cfg2.AllowedProjectIDs = []string{"42", "99"}
		cfg2.AllowedGroupIDs = append([]string{}, cfg.AllowedGroupIDs...)
		cfg2.CursorKey = append([]byte{}, cfg.CursorKey...)
		cfg2.APIURL = cfg.APIURL
		cfg2.Token = cfg.Token
		d2 := Deps{Config: &cfg2, Client: d.Client, Clock: d.Clock}
		res := callReviewQueueResult(t, d2, map[string]any{
			"group_id": "9", "kinds": []any{"reviewer"}, "page_size": 1, "cursor": tok,
		})
		if !res.IsError {
			t.Fatal("policy fingerprint change must resync")
		}
		if tr.hits.Load() != before {
			t.Fatalf("policy mismatch transport before=%d after=%d", before, tr.hits.Load())
		}
	})

	t.Run("instance", func(t *testing.T) {
		before := tr.hits.Load()
		cfg2 := *cfg
		cfg2.AllowedProjectIDs = append([]string{}, cfg.AllowedProjectIDs...)
		cfg2.AllowedGroupIDs = append([]string{}, cfg.AllowedGroupIDs...)
		cfg2.CursorKey = append([]byte{}, cfg.CursorKey...)
		cfg2.APIURL = "http://other.invalid/api/v4"
		cfg2.Token = cfg.Token
		d2 := Deps{Config: &cfg2, Client: d.Client, Clock: d.Clock}
		res := callReviewQueueResult(t, d2, map[string]any{
			"group_id": "9", "kinds": []any{"reviewer"}, "page_size": 1, "cursor": tok,
		})
		if !res.IsError {
			t.Fatal("instance change must resync")
		}
		if tr.hits.Load() != before {
			t.Fatalf("instance mismatch transport before=%d after=%d", before, tr.hits.Load())
		}
	})
}

func TestReviewQueue_identityOnlyActorMismatchAndUnknown(t *testing.T) {
	t.Run("current_actor_changed", func(t *testing.T) {
		tr := &countingQueueTransport{userID: 7}
		d := queueDepsCounting(t, tr, &config.Config{})
		tok := mintQueueContinuation(t, d)
		before := tr.hits.Load()
		groupsBefore := tr.groupHits.Load()
		listsBefore := tr.listHits.Load()
		tr.userID = 8 // CurrentUser now differs from signed ActorID
		res := callReviewQueueResult(t, d, map[string]any{
			"group_id": "9", "kinds": []any{"reviewer"}, "page_size": 1, "cursor": tok,
		})
		if !res.IsError {
			t.Fatal("changed authenticated actor must resync")
		}
		if tr.hits.Load() != before+1 || tr.userHits.Load() < 1 {
			t.Fatalf("identity-only: want exactly one new user hit; before=%d after=%d user=%d", before, tr.hits.Load(), tr.userHits.Load())
		}
		if tr.groupHits.Load() != groupsBefore || tr.listHits.Load() != listsBefore {
			t.Fatalf("must not continue to group/list after actor mismatch groups=%d→%d lists=%d→%d",
				groupsBefore, tr.groupHits.Load(), listsBefore, tr.listHits.Load())
		}
	})

	t.Run("unknown_explicit_actor_zero_list", func(t *testing.T) {
		tr := &countingQueueTransport{}
		d := queueDepsCounting(t, tr, &config.Config{})
		tok := mintQueueContinuation(t, d)
		before := tr.hits.Load()
		listsBefore := tr.listHits.Load()
		// Explicit actor changes selection → local preflight, zero transport.
		res := callReviewQueueResult(t, d, map[string]any{
			"group_id": "9", "kinds": []any{"reviewer"}, "page_size": 1, "cursor": tok,
			"actor_id": 99,
		})
		if !res.IsError {
			t.Fatal("changed discovery actor must resync")
		}
		if tr.hits.Load() != before {
			t.Fatalf("explicit actor selection mismatch must be zero-transport: before=%d after=%d", before, tr.hits.Load())
		}
		if tr.listHits.Load() != listsBefore {
			t.Fatal("must not list after actor selection mismatch")
		}
	})
}
