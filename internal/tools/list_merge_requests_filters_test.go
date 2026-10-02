package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	glclient "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"
)

func TestListMergeRequests_inputSchemaRegistered(t *testing.T) {
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	}))
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "t"}, nil)
	RegisterMergeRequests(srv, Deps{Config: &config.Config{Token: "x"}, Client: cli})
	cs := testutil.MCPConnect(t, srv)
	listed, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	var schema any
	for _, tool := range listed.Tools {
		if tool != nil && tool.Name == "list_merge_requests" {
			schema = tool.InputSchema
			break
		}
	}
	if schema == nil {
		t.Fatal("list_merge_requests InputSchema missing")
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
	required := map[string]bool{}
	if req, ok := obj["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				required[s] = true
			}
		}
	}
	// Baseline Pagination: non-pointer ints → required in registered schema.
	if !required["page"] || !required["per_page"] {
		t.Fatalf("expected page/per_page required from Pagination baseline, required=%v schema=%s", required, raw)
	}
	for _, name := range []string{
		"project_id", "group_id", "state", "author_id", "reviewer_id",
		"scope", "updated_after", "updated_before", "order_by", "sort",
	} {
		if required[name] {
			t.Fatalf("%q must not be required: %s", name, raw)
		}
	}

	assertNullableInteger := func(name string) {
		t.Helper()
		pm, _ := props[name].(map[string]any)
		if pm == nil {
			t.Fatalf("missing %q", name)
		}
		typ, ok := pm["type"].([]any)
		if !ok || len(typ) != 2 {
			t.Fatalf("%q want [null,integer], got %#v", name, pm["type"])
		}
		got := map[string]bool{}
		for _, x := range typ {
			got[fmt.Sprint(x)] = true
		}
		if !got["null"] || !got["integer"] || got["number"] || got["string"] {
			t.Fatalf("%q want null|integer only, got %#v", name, typ)
		}
	}
	assertRequiredInteger := func(name string) {
		t.Helper()
		pm, _ := props[name].(map[string]any)
		if pm == nil {
			t.Fatalf("missing %q", name)
		}
		if pm["type"] != "integer" {
			t.Fatalf("%q want type integer (required Pagination), got %#v", name, pm["type"])
		}
	}
	assertNullableString := func(name string) {
		t.Helper()
		pm, _ := props[name].(map[string]any)
		if pm == nil {
			t.Fatalf("missing %q", name)
		}
		typ, ok := pm["type"].([]any)
		if !ok || len(typ) != 2 {
			t.Fatalf("%q want [null,string], got %#v", name, pm["type"])
		}
		got := map[string]bool{}
		for _, x := range typ {
			got[fmt.Sprint(x)] = true
		}
		if !got["null"] || !got["string"] || got["integer"] || got["number"] {
			t.Fatalf("%q want null|string only, got %#v", name, typ)
		}
	}

	assertNullableInteger("author_id")
	assertNullableInteger("reviewer_id")
	assertRequiredInteger("page")
	assertRequiredInteger("per_page")
	for _, name := range []string{
		"project_id", "group_id", "state", "scope", "updated_after", "updated_before", "order_by", "sort",
	} {
		assertNullableString(name)
	}
}

type listMRQueryCase struct {
	name      string
	args      map[string]any
	wantPath  string
	wantQuery map[string]string
	absent    []string
	wantNext  int64
}

func listMRFullFilterArgs(scopeKey, scopeVal string) map[string]any {
	args := map[string]any{
		"state":          "opened",
		"author_id":      5,
		"reviewer_id":    7,
		"scope":          "all",
		"updated_after":  "2019-03-15T08:00:00.123456789Z",
		"updated_before": "2019-03-15T09:00:00Z",
		"order_by":       "updated_at",
		"sort":           "asc",
		"page":           2,
		"per_page":       50,
	}
	args[scopeKey] = scopeVal
	return args
}

func TestListMergeRequests_CallTool_queryTableAllScopes(t *testing.T) {

	afterFrac := "2019-03-15T08:00:00.123456789Z"
	beforeWhole := "2019-03-15T09:00:00Z"
	afterParsed, err := time.Parse(time.RFC3339Nano, afterFrac)
	if err != nil {
		t.Fatal(err)
	}
	wantAfter := glclient.FormatUpdatedBound(afterParsed)

	fullWant := map[string]string{
		"state": "opened", "author_id": "5", "reviewer_id": "7", "scope": "all",
		"updated_after": wantAfter, "updated_before": beforeWhole,
		"order_by": "updated_at", "sort": "asc", "page": "2", "per_page": "50",
	}
	fullAbsent := []string{} // none of the optional filters absent when full

	cases := []listMRQueryCase{
		{
			name: "project_full_filters", args: listMRFullFilterArgs("project_id", "42"),
			wantPath: "/api/v4/projects/42/merge_requests", wantQuery: fullWant, absent: fullAbsent, wantNext: 3,
		},
		{
			name: "group_full_filters", args: listMRFullFilterArgs("group_id", "10"),
			wantPath: "/api/v4/groups/10/merge_requests", wantQuery: fullWant, absent: fullAbsent, wantNext: 3,
		},
		{
			name: "global_full_filters", args: map[string]any{
				"state": "opened", "author_id": 5, "reviewer_id": 7, "scope": "all",
				"updated_after": afterFrac, "updated_before": beforeWhole,
				"order_by": "updated_at", "sort": "asc", "page": 2, "per_page": 50,
			},
			wantPath: "/api/v4/merge_requests", wantQuery: fullWant, absent: fullAbsent, wantNext: 3,
		},
		{
			name: "project_state_only", args: map[string]any{"project_id": "42", "state": "opened", "page": 1, "per_page": 20},
			wantPath:  "/api/v4/projects/42/merge_requests",
			wantQuery: map[string]string{"state": "opened", "page": "1", "per_page": "20"},
			absent:    []string{"author_id", "reviewer_id", "scope", "updated_after", "updated_before", "order_by", "sort"},
			wantNext:  3,
		},
		{
			name: "group_state_only", args: map[string]any{"group_id": "10", "state": "opened", "page": 1, "per_page": 20},
			wantPath:  "/api/v4/groups/10/merge_requests",
			wantQuery: map[string]string{"state": "opened", "page": "1", "per_page": "20"},
			absent:    []string{"author_id", "reviewer_id", "scope", "updated_after", "updated_before", "order_by", "sort"},
			wantNext:  3,
		},
		{
			name: "global_state_only", args: map[string]any{"state": "opened", "page": 1, "per_page": 20},
			wantPath:  "/api/v4/merge_requests",
			wantQuery: map[string]string{"state": "opened", "page": "1", "per_page": "20"},
			absent:    []string{"author_id", "reviewer_id", "scope", "updated_after", "updated_before", "order_by", "sort"},
			wantNext:  3,
		},
		{
			name: "group_author_regression", args: map[string]any{"group_id": "10", "state": "opened", "author_id": 5, "page": 1, "per_page": 20},
			wantPath:  "/api/v4/groups/10/merge_requests",
			wantQuery: map[string]string{"state": "opened", "author_id": "5", "page": "1", "per_page": "20"},
			absent:    []string{"reviewer_id", "scope", "updated_after", "updated_before", "order_by", "sort"},
			wantNext:  3,
		},
		{
			name: "global_reviewer_discovery", args: map[string]any{"scope": "all", "reviewer_id": 7, "page": 1, "per_page": 20},
			wantPath:  "/api/v4/merge_requests",
			wantQuery: map[string]string{"scope": "all", "reviewer_id": "7", "page": "1", "per_page": "20"},
			absent:    []string{"author_id", "state", "updated_after", "updated_before", "order_by", "sort"},
			wantNext:  3,
		},
		{
			name: "project_updated_after_only", args: map[string]any{
				"project_id": "42", "updated_after": afterFrac, "page": 1, "per_page": 20,
			},
			wantPath:  "/api/v4/projects/42/merge_requests",
			wantQuery: map[string]string{"updated_after": wantAfter, "page": "1", "per_page": "20"},
			absent:    []string{"updated_before", "scope", "author_id", "reviewer_id"},
			wantNext:  3,
		},
		{
			name: "global_updated_before_only_offset_canon", args: map[string]any{
				"updated_before": "2019-03-15T11:00:00.100+02:00", "page": 1, "per_page": 20,
			},
			wantPath: "/api/v4/merge_requests",
			wantQuery: map[string]string{
				"updated_before": glclient.FormatUpdatedBound(mustParseMRTime(t, "2019-03-15T11:00:00.100+02:00")),
				"page":           "1", "per_page": "20",
			},
			absent:   []string{"updated_after", "scope"},
			wantNext: 3,
		},
		{
			name: "pagination_clamp_project", args: map[string]any{"project_id": "42", "page": 0, "per_page": 500},
			wantPath:  "/api/v4/projects/42/merge_requests",
			wantQuery: map[string]string{"page": "1", "per_page": "100"},
			absent:    []string{"scope", "author_id", "updated_after"},
			wantNext:  3,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			var sawPath string
			var sawQuery url.Values
			var listHits int32
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(r.URL.Path, "/merge_requests") {
					atomic.AddInt32(&listHits, 1)
					sawPath = r.URL.Path
					sawQuery = r.URL.Query()
					w.Header().Set("X-Next-Page", "3")
					_, _ = io.WriteString(w, `[{"id":1,"iid":1,"project_id":42}]`)
					return
				}
				_, _ = io.WriteString(w, `{}`)
			})
			cli, _ := testutil.NewGitLabClient(t, h)
			srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "t"}, nil)
			RegisterMergeRequests(srv, Deps{Config: &config.Config{Token: "x"}, Client: cli})
			cs := testutil.MCPConnect(t, srv)
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
				Name: "list_merge_requests", Arguments: c.args,
			})
			if err != nil {
				t.Fatal(err)
			}
			if res == nil || res.IsError {
				t.Fatalf("CallTool error: %s", toolErrorText(t, res))
			}
			if atomic.LoadInt32(&listHits) != 1 {
				t.Fatalf("list hits=%d", listHits)
			}
			if sawPath != c.wantPath {
				t.Fatalf("path=%q want %q", sawPath, c.wantPath)
			}
			for k, want := range c.wantQuery {
				if got := sawQuery.Get(k); got != want {
					t.Fatalf("%s=%q want %q (query=%v)", k, got, want, sawQuery)
				}
			}
			for _, k := range c.absent {
				if _, ok := sawQuery[k]; ok {
					t.Fatalf("unexpected %s=%v", k, sawQuery[k])
				}
			}
			next := structuredNextPage(t, res)
			if next != c.wantNext {
				t.Fatalf("next_page=%d want %d", next, c.wantNext)
			}
		})
	}
}

func mustParseMRTime(t *testing.T, s string) time.Time {
	t.Helper()
	tt, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return tt
}

func structuredNextPage(t *testing.T, res *mcp.CallToolResult) int64 {
	t.Helper()
	if res.StructuredContent == nil {
		t.Fatal("missing StructuredContent")
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	pag, _ := m["pagination"].(map[string]any)
	if pag == nil {
		t.Fatalf("missing pagination: %s", raw)
	}
	switch v := pag["next_page"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			t.Fatal(err)
		}
		return n
	default:
		t.Fatalf("next_page type %T value %#v", pag["next_page"], pag["next_page"])
		return 0
	}
}

func TestListMergeRequests_rejectsBeforeTransport(t *testing.T) {
	var listHits int32
	var identityHits int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(path, "/merge_requests") {
			atomic.AddInt32(&listHits, 1)
			_, _ = io.WriteString(w, `[]`)
			return
		}
		if isProjectIdentityPath(path) || isGroupIdentityPath(path) {
			atomic.AddInt32(&identityHits, 1)
		}
		_, _ = io.WriteString(w, `{}`)
	})
	cli, _ := testutil.NewGitLabClient(t, h)
	d := Deps{Config: &config.Config{AllowedProjectIDs: []string{"42"}}, Client: cli}

	zero := int64(0)
	neg := int64(-1)
	badScope := "reviewer"
	badOrder := "merged_at"
	badSort := "up"
	malformed := "not-a-time"
	after := "2019-03-15T09:00:00.200Z"
	before := "2019-03-15T09:00:00.100Z"
	equalAfter := "2019-03-15T09:00:00.100Z"
	equalBefore := "2019-03-15T09:00:00.100Z"
	offsetEqAfter := "2019-03-15T11:00:00.100+02:00"
	offsetEqBefore := "2019-03-15T09:00:00.100Z"
	oneDigitHour := "2026-10-02T2:00:00Z"
	commaFrac := "2026-10-02T22:00:00,123Z"
	zoneHour24 := "2026-10-02T22:00:00+24:00"
	zoneMin60 := "2026-10-02T22:00:00+00:60"
	overPrec1 := "2026-10-02T22:00:00.1234567891Z"
	overPrec9 := "2026-10-02T22:00:00.1234567899Z"
	proj := "42"

	cases := []struct {
		name string
		in   listMergeRequestsIn
		sub  string
	}{
		{"conflict_selectors", listMergeRequestsIn{ProjectID: ptr("42"), GroupID: ptr("10")}, "mutually exclusive"},
		{"author_zero", listMergeRequestsIn{AuthorID: &zero}, "author_id"},
		{"reviewer_neg", listMergeRequestsIn{ReviewerID: &neg}, "reviewer_id"},
		{"bad_scope", listMergeRequestsIn{Scope: &badScope}, "scope"},
		{"bad_order", listMergeRequestsIn{OrderBy: &badOrder}, "order_by"},
		{"bad_sort", listMergeRequestsIn{Sort: &badSort}, "sort"},
		{"malformed_time", listMergeRequestsIn{UpdatedAfter: &malformed}, "updated_after"},
		{"inverted_frac_same_second", listMergeRequestsIn{UpdatedAfter: &after, UpdatedBefore: &before}, "updated_after"},
		{"one_digit_hour", listMergeRequestsIn{ProjectID: &proj, UpdatedAfter: &oneDigitHour}, "updated_after"},
		{"comma_fraction", listMergeRequestsIn{ProjectID: &proj, UpdatedAfter: &commaFrac}, "updated_after"},
		{"zone_hour_24", listMergeRequestsIn{ProjectID: &proj, UpdatedAfter: &zoneHour24}, "updated_after"},
		{"zone_minute_60", listMergeRequestsIn{ProjectID: &proj, UpdatedAfter: &zoneMin60}, "updated_after"},
		{"over_nanosecond_prec_a", listMergeRequestsIn{ProjectID: &proj, UpdatedAfter: &overPrec1}, "updated_after"},
		{"over_nanosecond_prec_b", listMergeRequestsIn{ProjectID: &proj, UpdatedAfter: &overPrec9}, "updated_after"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			atomic.StoreInt32(&listHits, 0)
			atomic.StoreInt32(&identityHits, 0)
			_, _, err := listMergeRequests(context.Background(), nil, c.in, d)
			if err == nil || !strings.Contains(err.Error(), c.sub) {
				t.Fatalf("err=%v want substring %q", err, c.sub)
			}
			if atomic.LoadInt32(&listHits) != 0 || atomic.LoadInt32(&identityHits) != 0 {
				t.Fatalf("transport/identity after validation fail: list=%d identity=%d", listHits, identityHits)
			}
		})
	}

	d2 := Deps{Config: &config.Config{}, Client: cli}
	atomic.StoreInt32(&listHits, 0)
	_, _, err := listMergeRequests(context.Background(), nil, listMergeRequestsIn{
		UpdatedAfter: &equalAfter, UpdatedBefore: &equalBefore,
	}, d2)
	if err != nil {
		t.Fatalf("equal bounds: %v", err)
	}
	if atomic.LoadInt32(&listHits) != 1 {
		t.Fatalf("list hits=%d", listHits)
	}

	atomic.StoreInt32(&listHits, 0)
	var sawEq url.Values
	hEq := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/merge_requests") {
			atomic.AddInt32(&listHits, 1)
			sawEq = r.URL.Query()
			_, _ = io.WriteString(w, `[]`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	})
	cliEq, _ := testutil.NewGitLabClient(t, hEq)
	_, _, err = listMergeRequests(context.Background(), nil, listMergeRequestsIn{
		UpdatedAfter: &offsetEqAfter, UpdatedBefore: &offsetEqBefore,
	}, Deps{Config: &config.Config{}, Client: cliEq})
	if err != nil {
		t.Fatalf("offset-equal bounds: %v", err)
	}
	if !strings.HasPrefix(sawEq.Get("updated_after"), "2019-03-15T09:00:00.1") {
		t.Fatalf("updated_after=%q", sawEq.Get("updated_after"))
	}
	if !strings.HasPrefix(sawEq.Get("updated_before"), "2019-03-15T09:00:00.1") {
		t.Fatalf("updated_before=%q", sawEq.Get("updated_before"))
	}
}

func TestListMergeRequests_CallTool_validationBranches(t *testing.T) {
	var listHits, identityHits int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		if strings.Contains(path, "/merge_requests") {
			atomic.AddInt32(&listHits, 1)
			_, _ = io.WriteString(w, `[]`)
			return
		}
		if isProjectIdentityPath(path) || isGroupIdentityPath(path) {
			atomic.AddInt32(&identityHits, 1)
		}
		_, _ = io.WriteString(w, `{}`)
	})
	cli, _ := testutil.NewGitLabClient(t, h)
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "t"}, nil)
	RegisterMergeRequests(srv, Deps{Config: &config.Config{Token: "x", AllowedProjectIDs: []string{"42"}}, Client: cli})
	cs := testutil.MCPConnect(t, srv)

	badCases := []map[string]any{
		{"project_id": "1", "group_id": "2", "page": 1, "per_page": 20},
		{"project_id": "42", "updated_after": "2026-10-02T2:00:00Z", "page": 1, "per_page": 20},
		{"project_id": "42", "updated_after": "2026-10-02T22:00:00.1234567891Z", "page": 1, "per_page": 20},
		{"project_id": "42", "updated_after": "2026-10-02T22:00:00+00:60", "page": 1, "per_page": 20},
		{"group_id": "10", "author_id": 0, "page": 1, "per_page": 20},
		{"reviewer_id": -3, "page": 1, "per_page": 20},
		{"scope": "reviewer", "page": 1, "per_page": 20},
		{"order_by": "merged_at", "page": 1, "per_page": 20},
		{"updated_after": "2019-03-15T09:00:00.200Z", "updated_before": "2019-03-15T09:00:00.100Z", "page": 1, "per_page": 20},
	}
	for i, args := range badCases {
		atomic.StoreInt32(&listHits, 0)
		atomic.StoreInt32(&identityHits, 0)
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "list_merge_requests", Arguments: args,
		})
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if res == nil || !res.IsError {
			t.Fatalf("case %d: expected validation error for %#v", i, args)
		}
		if atomic.LoadInt32(&listHits) != 0 || atomic.LoadInt32(&identityHits) != 0 {
			t.Fatalf("case %d transport: list=%d id=%d args=%#v", i, listHits, identityHits, args)
		}
	}

	atomic.StoreInt32(&listHits, 0)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "list_merge_requests",
		Arguments: map[string]any{
			"author_id": "not-an-int", "page": 1, "per_page": 20,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !res.IsError {
		t.Fatal("expected schema/type error for string author_id")
	}
	if atomic.LoadInt32(&listHits) != 0 {
		t.Fatalf("list ran on bad type: %d", listHits)
	}
}

func TestListMergeRequests_activePolicy_withNewFilters(t *testing.T) {

	t.Run("canonical_project_alias", func(t *testing.T) {
		var listHits, identityHits int32
		var sawPath string
		var sawQuery url.Values
		d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			// Prefer EscapedPath: alias tokens keep %2F so identity is a single segment.
			ep := r.URL.EscapedPath()
			switch {
			case strings.Contains(ep, "/merge_requests"):
				atomic.AddInt32(&listHits, 1)
				sawPath = r.URL.Path
				sawQuery = r.URL.Query()
				w.Header().Set("X-Next-Page", "3")
				_, _ = io.WriteString(w, `[{"id":1,"iid":1,"project_id":42}]`)
			case isProjectIdentityPath(ep):
				atomic.AddInt32(&identityHits, 1)
				id := echoNumericID(ep, 42)
				_, _ = io.WriteString(w, `{"id":`+strconv.FormatInt(id, 10)+`,"path_with_namespace":"g/p","namespace":{"id":10,"kind":"group"}}`)
			default:
				http.NotFound(w, r)
			}
		}))
		d.Config.AllowedProjectIDs = []string{"moved/old/path"}
		scope := "all"
		reviewer := int64(7)
		after := "2019-03-15T08:00:00.123456789Z"
		_, out, err := listMergeRequests(context.Background(), nil, listMergeRequestsIn{
			ProjectID: ptr("moved/old/path"), Scope: &scope, ReviewerID: &reviewer, UpdatedAfter: &after,
		}, d)
		if err != nil {
			t.Fatal(err)
		}
		if atomic.LoadInt32(&identityHits) < 1 {
			t.Fatal("expected project identity resolve")
		}
		if atomic.LoadInt32(&listHits) != 1 {
			t.Fatalf("list hits=%d", listHits)
		}
		if !strings.Contains(sawPath, "/projects/42/") {
			t.Fatalf("expected canonical project path, got %s", sawPath)
		}
		if sawQuery.Get("reviewer_id") != "7" || sawQuery.Get("scope") != "all" {
			t.Fatalf("query=%v", sawQuery)
		}
		if got := sawQuery.Get("updated_after"); got != "2019-03-15T08:00:00.123456789Z" {
			t.Fatalf("updated_after=%q", got)
		}
		assertNextPageMap(t, out, 3)
	})

	t.Run("canonical_group_path_with_author", func(t *testing.T) {
		var listHits, groupGet int32
		var sawQuery url.Values
		d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			path := r.URL.Path
			switch {
			case strings.Contains(path, "/groups/") && strings.Contains(path, "/merge_requests"):
				atomic.AddInt32(&listHits, 1)
				if !strings.Contains(path, "/groups/10/") {
					t.Errorf("non-canonical group MR path %s", path)
				}
				sawQuery = r.URL.Query()
				w.Header().Set("X-Next-Page", "3")
				_, _ = io.WriteString(w, `[{"id":1,"iid":1,"project_id":42}]`)
			case isGroupIdentityPath(path) || (strings.Contains(path, "/groups/") && !strings.Contains(path, "/merge_requests")):
				atomic.AddInt32(&groupGet, 1)
				_, _ = io.WriteString(w, `{"id":10,"full_path":"g","parent_id":0}`)
			case isProjectIdentityPath(path):
				id := echoNumericID(path, 42)
				_, _ = io.WriteString(w, `{"id":`+strconv.FormatInt(id, 10)+`,"path_with_namespace":"g/p","namespace":{"id":10,"kind":"group"}}`)
			default:
				http.NotFound(w, r)
			}
		}))
		d.Config.AllowedGroupIDs = []string{"moved/old/group"}
		author := int64(5)
		state := "opened"
		_, out, err := listMergeRequests(context.Background(), nil, listMergeRequestsIn{
			GroupID: ptr("moved/old/group"), State: &state, AuthorID: &author,
		}, d)
		if err != nil {
			t.Fatal(err)
		}
		if atomic.LoadInt32(&groupGet) < 1 {
			t.Fatal("expected GetGroup")
		}
		if atomic.LoadInt32(&listHits) != 1 {
			t.Fatalf("list hits=%d", listHits)
		}
		if sawQuery.Get("author_id") != "5" || sawQuery.Get("state") != "opened" {
			t.Fatalf("query=%v", sawQuery)
		}
		assertNextPageMap(t, out, 3)
	})

	t.Run("group_deny_before_list", func(t *testing.T) {
		var listHits int32
		d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if strings.Contains(r.URL.Path, "/merge_requests") {
				atomic.AddInt32(&listHits, 1)
			}
			if strings.Contains(r.URL.Path, "/groups/") {
				id := echoNumericID(r.URL.Path, 99)
				_, _ = io.WriteString(w, `{"id":`+strconv.FormatInt(id, 10)+`,"full_path":"x","parent_id":0}`)
				return
			}
			http.NotFound(w, r)
		}))
		d.Config.AllowedGroupIDs = []string{"10"}
		scope := "all"
		reviewer := int64(7)
		_, _, err := listMergeRequests(context.Background(), nil, listMergeRequestsIn{
			GroupID: ptr("99"), Scope: &scope, ReviewerID: &reviewer,
		}, d)
		if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
			t.Fatalf("want authz deny, got %v", err)
		}
		if atomic.LoadInt32(&listHits) != 0 {
			t.Fatalf("list must not run: %d", listHits)
		}
	})

	t.Run("global_filter_oos", func(t *testing.T) {
		d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			path := r.URL.Path
			switch {
			case isProjectIdentityPath(path):
				id := echoNumericID(path, 42)
				_, _ = io.WriteString(w, `{"id":`+strconv.FormatInt(id, 10)+`,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}}`)
			case strings.Contains(path, "/merge_requests"):
				w.Header().Set("X-Next-Page", "3")
				_, _ = io.WriteString(w, `[{"id":1,"iid":1,"project_id":42},{"id":2,"iid":2,"project_id":99}]`)
			default:
				_, _ = io.WriteString(w, `{}`)
			}
		}))
		d.Config.AllowedProjectIDs = []string{"42"}
		scope := "all"
		reviewer := int64(7)
		_, out, err := listMergeRequests(context.Background(), nil, listMergeRequestsIn{
			Scope: &scope, ReviewerID: &reviewer,
		}, d)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(out)
		if strings.Contains(string(raw), `"project_id":99`) {
			t.Fatalf("global MR OOS leak: %s", raw)
		}
		assertNextPageMap(t, out, 3)
	})
}

func assertNextPageMap(t *testing.T, out any, want int64) {
	t.Helper()
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("out %T", out)
	}
	pag, _ := m["pagination"].(map[string]any)
	if pag == nil {
		t.Fatalf("missing pagination: %#v", m)
	}
	switch v := pag["next_page"].(type) {
	case int64:
		if v != want {
			t.Fatalf("next_page=%d want %d", v, want)
		}
	case float64:
		if int64(v) != want {
			t.Fatalf("next_page=%v want %d", v, want)
		}
	default:
		t.Fatalf("next_page type %T %#v", pag["next_page"], pag["next_page"])
	}
}
