package tools

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
)

// runListMRs calls listMergeRequests against a fixture server and returns the
// request path, query string and number of requests that reached the wire.
func runListMRs(t *testing.T, in listMergeRequestsIn) (string, url.Values, int32, error) {
	t.Helper()
	var path string
	var q url.Values
	var calls atomic.Int32
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		path, q = r.URL.Path, r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	}))
	_, _, err := listMergeRequests(context.Background(), nil, in, Deps{Config: &config.Config{}, Client: cli})
	return path, q, calls.Load(), err
}

func allMRFilters() listMergeRequestsIn {
	return listMergeRequestsIn{
		State: ptr("opened"), AuthorID: ptr(int64(7)), ReviewerID: ptr(int64(9)),
		Scope: ptr("assigned_to_me"), UpdatedAfter: ptr("2026-10-01T10:00:00Z"),
		UpdatedBefore: ptr("2026-10-05"), OrderBy: ptr("updated_at"), Sort: ptr("desc"),
	}
}

func TestListMergeRequests_filtersOnWire(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"state": "opened", "author_id": "7", "reviewer_id": "9", "scope": "assigned_to_me",
		"updated_after": "2026-10-01T10:00:00Z", "updated_before": "2026-10-05T00:00:00Z",
		"order_by": "updated_at", "sort": "desc",
	}
	for _, tc := range []struct {
		name, path string
		set        func(*listMergeRequestsIn)
	}{
		{"project", "/api/v4/projects/42/merge_requests", func(in *listMergeRequestsIn) { in.ProjectID = ptr("42") }},
		{"group", "/api/v4/groups/10/merge_requests", func(in *listMergeRequestsIn) { in.GroupID = ptr("10") }},
		{"global", "/api/v4/merge_requests", func(*listMergeRequestsIn) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := allMRFilters()
			tc.set(&in)
			path, q, _, err := runListMRs(t, in)
			if err != nil {
				t.Fatal(err)
			}
			if path != tc.path {
				t.Fatalf("path %q, want %q", path, tc.path)
			}
			for k, v := range want {
				if got := q.Get(k); got != v {
					t.Errorf("%s=%q, want %q (query %v)", k, got, v, q)
				}
			}
		})
	}
}

// Regression: the group list used to drop author_id.
func TestListMergeRequests_groupAuthorID(t *testing.T) {
	t.Parallel()
	path, q, _, err := runListMRs(t, listMergeRequestsIn{GroupID: ptr("10"), AuthorID: ptr(int64(7))})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/api/v4/groups/10/merge_requests" || q.Get("author_id") != "7" {
		t.Fatalf("path %q query %v", path, q)
	}
}

func TestListMergeRequests_omittedFiltersNotSent(t *testing.T) {
	t.Parallel()
	_, q, _, err := runListMRs(t, listMergeRequestsIn{GroupID: ptr("10")})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"reviewer_id", "scope", "updated_after", "updated_before", "order_by", "sort", "author_id"} {
		if q.Has(k) {
			t.Errorf("%s unexpectedly sent: %v", k, q)
		}
	}
}

func TestListMergeRequests_invalidFilters(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		set  func(*listMergeRequestsIn)
		want string
	}{
		{"scope", func(in *listMergeRequestsIn) { in.Scope = ptr("mine") }, `invalid scope "mine": must be one of created_by_me, assigned_to_me, all`},
		{"order_by", func(in *listMergeRequestsIn) { in.OrderBy = ptr("id") }, `invalid order_by "id": must be one of created_at, title, updated_at`},
		{"sort", func(in *listMergeRequestsIn) { in.Sort = ptr("up") }, `invalid sort "up": must be one of asc, desc`},
		{"updated_after", func(in *listMergeRequestsIn) { in.UpdatedAfter = ptr("yesterday") }, `invalid updated_after "yesterday"`},
		{"updated_before", func(in *listMergeRequestsIn) { in.UpdatedBefore = ptr("nope") }, `invalid updated_before "nope"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := listMergeRequestsIn{GroupID: ptr("10")}
			tc.set(&in)
			_, _, calls, err := runListMRs(t, in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want containing %q", err, tc.want)
			}
			if calls != 0 {
				t.Fatalf("%d request(s) sent despite invalid input", calls)
			}
		})
	}
}

func TestListMergeRequests_upstreamError(t *testing.T) {
	t.Parallel()
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"nope"}`, http.StatusForbidden)
	}))
	for _, in := range []listMergeRequestsIn{{ProjectID: ptr("42")}, {GroupID: ptr("10")}, {}} {
		if _, _, err := listMergeRequests(context.Background(), nil, in, Deps{Config: &config.Config{}, Client: cli}); err == nil {
			t.Fatalf("expected upstream error for %+v", in)
		}
	}
}

func TestListMergeRequests_projectGuards(t *testing.T) {
	t.Parallel()
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("request sent despite rejected project")
	}))
	ctx := context.Background()
	if _, _, err := listMergeRequests(ctx, nil, listMergeRequestsIn{ProjectID: ptr(" ")}, Deps{Config: &config.Config{}, Client: cli}); err == nil {
		t.Fatal("blank project_id: expected error")
	}
	cfg := &config.Config{AllowedProjectIDs: []string{"1"}}
	if _, _, err := listMergeRequests(ctx, nil, listMergeRequestsIn{ProjectID: ptr("42")}, Deps{Config: cfg, Client: cli}); err == nil {
		t.Fatal("disallowed project: expected error")
	}
}
