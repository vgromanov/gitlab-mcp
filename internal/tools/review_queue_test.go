package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
)

type queueMR struct {
	project, iid int64
	updated      string
}

// The current user (id 7) reviews five MRs and authors three; (100,5) is both.
// Same iid 5 in project 200 is a different MR. Lists are newest-first, like order_by=updated_at&sort=desc.
var (
	queueReviewerMRs = []queueMR{
		{100, 5, "2026-10-06T10:05:00Z"}, {100, 4, "2026-10-06T10:04:00Z"}, {100, 3, "2026-10-06T10:03:00Z"},
		{100, 2, "2026-10-06T10:02:00Z"}, {100, 1, "2026-10-06T10:01:00Z"},
	}
	queueAuthorMRs = []queueMR{
		{100, 9, "2026-10-06T10:09:00Z"}, {200, 5, "2026-10-06T10:06:00Z"}, {100, 5, "2026-10-06T10:05:00Z"},
	}
)

type queueFixture struct {
	cs *mcp.ClientSession

	mu        sync.Mutex
	userCalls int
	lists     []url.Values // query of every group list request, in order
	total     int
}

func (f *queueFixture) listsFor(param string) []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []url.Values
	for _, q := range f.lists {
		if q.Get(param) != "" {
			out = append(out, q)
		}
	}
	return out
}

func (f *queueFixture) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.total
}

func queueBody(mrs []queueMR) string {
	out := make([]map[string]any, 0, len(mrs))
	for _, m := range mrs {
		out = append(out, map[string]any{
			"id": m.project*1000 + m.iid, "iid": m.iid, "project_id": m.project,
			"title": "MR " + strconv.FormatInt(m.iid, 10), "state": "opened", "sha": "sha-" + strconv.FormatInt(m.iid, 10),
			"web_url":    "https://gl.example/p/" + strconv.FormatInt(m.project, 10) + "/-/merge_requests/" + strconv.FormatInt(m.iid, 10),
			"updated_at": m.updated, "description": "must not leak",
			"author":    map[string]any{"id": 7, "username": "me", "name": "Me", "avatar_url": "https://x/a.png", "state": "active"},
			"reviewers": []map[string]any{{"id": 8, "username": "rev", "name": "Rev", "avatar_url": "https://x/b.png"}},
		})
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// newQueueFixture serves /user and the group MR list with GitLab-style paging.
// userStatus / listStatus, when non-zero, make that endpoint answer with the status.
func newQueueFixture(t *testing.T, userStatus, listStatus int) *queueFixture {
	t.Helper()
	f := &queueFixture{}
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.total++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/user":
			f.mu.Lock()
			f.userCalls++
			f.mu.Unlock()
			if userStatus != 0 {
				w.WriteHeader(userStatus)
				writeFixture(w, `{"message":"nope"}`)
				return
			}
			writeFixture(w, `{"id":7,"username":"me","name":"Me","email":"must-not-leak@example.com"}`)
		case "/api/v4/groups/g1/merge_requests":
			q := r.URL.Query()
			f.mu.Lock()
			f.lists = append(f.lists, q)
			f.mu.Unlock()
			if listStatus != 0 {
				w.WriteHeader(listStatus)
				writeFixture(w, `{"message":"nope"}`)
				return
			}
			data := queueAuthorMRs
			if q.Get("reviewer_id") == "7" {
				data = queueReviewerMRs
			}
			page, _ := strconv.Atoi(q.Get("page"))
			per, _ := strconv.Atoi(q.Get("per_page"))
			if page < 1 || per < 1 {
				w.WriteHeader(http.StatusBadRequest)
				writeFixture(w, `{"message":"page and per_page must be sent"}`)
				return
			}
			lo, hi := min((page-1)*per, len(data)), min(page*per, len(data))
			if hi < len(data) {
				w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
			}
			writeFixture(w, queueBody(data[lo:hi]))
		default:
			w.WriteHeader(http.StatusNotFound)
			writeFixture(w, `{"message":"404 Not Found"}`)
		}
	}))
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "test"}, nil)
	RegisterMergeRequests(srv, Deps{Config: &config.Config{}, Client: cli})
	f.cs = testutil.MCPConnect(t, srv)
	return f
}

func (f *queueFixture) call(t *testing.T, args map[string]any) (map[string]any, string) {
	t.Helper()
	res, err := f.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_review_queue", Arguments: args})
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

func queueRows(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	raw, _ := out["merge_requests"].([]any)
	rows := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		rows = append(rows, r.(map[string]any))
	}
	return rows
}

func rowID(r map[string]any) string {
	return strconv.Itoa(int(r["project_id"].(float64))) + "/" + strconv.Itoa(int(r["iid"].(float64)))
}

func rolesOf(r map[string]any) []string {
	var out []string
	for _, x := range r["roles"].([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestGetReviewQueue_capReached(t *testing.T) {
	f := newQueueFixture(t, 0, 0)
	// 5 reviewer MRs at 2 per page need 3 pages; the cap is 2. The 3 author MRs fit in 2 pages.
	out, errText := f.call(t, map[string]any{"group_id": "g1", "per_page": 2, "max_pages": 2})
	if errText != "" {
		t.Fatal(errText)
	}
	if out["complete"] != false {
		t.Fatalf("complete = %v, want false at the cap", out["complete"])
	}
	reason, _ := out["truncated_reason"].(string)
	if !strings.Contains(reason, "reviewer") || strings.Contains(reason, "author") || !strings.Contains(reason, "max_pages=2") {
		t.Fatalf("truncated_reason = %q, want only the reviewer cap", reason)
	}
	rev, auth := f.listsFor("reviewer_id"), f.listsFor("author_id")
	if len(rev) != 2 || len(auth) != 2 || f.userCalls != 1 || f.requests() != 5 {
		t.Fatalf("requests: reviewer=%d author=%d user=%d total=%d, want 2/2/1/5", len(rev), len(auth), f.userCalls, f.requests())
	}
	if rev[0].Get("page") != "1" || rev[1].Get("page") != "2" {
		t.Fatalf("reviewer pages = %v, %v", rev[0].Get("page"), rev[1].Get("page"))
	}
	var ids []string
	for _, r := range queueRows(t, out) {
		ids = append(ids, rowID(r))
	}
	// MR 100/1 sits on the unread third reviewer page, so it must be absent.
	want := []string{"100/9", "200/5", "100/5", "100/4", "100/3", "100/2"}
	if !slices.Equal(ids, want) {
		t.Fatalf("rows = %v, want %v", ids, want)
	}
}

func TestGetReviewQueue_capBoundaryIsComplete(t *testing.T) {
	f := newQueueFixture(t, 0, 0)
	// Exactly 3 pages of 2 hold the 5 reviewer MRs: the last page has no next page, so nothing is cut.
	out, errText := f.call(t, map[string]any{"group_id": "g1", "roles": []string{"reviewer"}, "per_page": 2, "max_pages": 3})
	if errText != "" {
		t.Fatal(errText)
	}
	if out["complete"] != true || out["truncated_reason"] != nil || len(queueRows(t, out)) != 5 {
		t.Fatalf("complete=%v reason=%v rows=%d, want complete with 5 rows", out["complete"], out["truncated_reason"], len(queueRows(t, out)))
	}
}

func TestGetReviewQueue_dedupBothRoles(t *testing.T) {
	f := newQueueFixture(t, 0, 0)
	out, errText := f.call(t, map[string]any{"group_id": "g1"})
	if errText != "" {
		t.Fatal(errText)
	}
	if out["complete"] != true || out["truncated_reason"] != nil {
		t.Fatalf("complete=%v reason=%v", out["complete"], out["truncated_reason"])
	}
	cu, _ := out["current_user"].(map[string]any)
	if len(cu) != 3 || cu["id"] != float64(7) || cu["username"] != "me" || cu["name"] != "Me" {
		t.Fatalf("current_user = %v", cu)
	}
	rows := queueRows(t, out)
	var ids []string
	byID := map[string]map[string]any{}
	for _, r := range rows {
		ids = append(ids, rowID(r))
		byID[rowID(r)] = r
	}
	// 5 reviewer + 3 author MRs, one in both roles, sorted newest first.
	want := []string{"100/9", "200/5", "100/5", "100/4", "100/3", "100/2", "100/1"}
	if !slices.Equal(ids, want) {
		t.Fatalf("rows = %v, want %v", ids, want)
	}
	if got := rolesOf(byID["100/5"]); !slices.Equal(got, []string{"reviewer", "author"}) {
		t.Fatalf("both-role MR roles = %v", got)
	}
	if got := rolesOf(byID["200/5"]); !slices.Equal(got, []string{"author"}) {
		t.Fatalf("same iid in another project must stay separate, roles = %v", got)
	}
	if got := rolesOf(byID["100/4"]); !slices.Equal(got, []string{"reviewer"}) {
		t.Fatalf("reviewer-only roles = %v", got)
	}
	// Exact row shape; nothing beyond it (description, avatar, email) leaks.
	r := byID["100/5"]
	keys := make([]string, 0, len(r))
	for k := range r {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"author", "iid", "project_id", "reviewers", "roles", "sha", "title", "updated_at", "web_url"}; !slices.Equal(keys, want) {
		t.Fatalf("row keys = %v, want %v", keys, want)
	}
	if r["sha"] != "sha-5" || r["title"] != "MR 5" || r["updated_at"] != "2026-10-06T10:05:00Z" ||
		r["web_url"] != "https://gl.example/p/100/-/merge_requests/5" {
		t.Fatalf("row = %v", r)
	}
	au := r["author"].(map[string]any)
	rv := r["reviewers"].([]any)[0].(map[string]any)
	if len(au) != 3 || au["username"] != "me" || len(rv) != 3 || rv["username"] != "rev" {
		t.Fatalf("author=%v reviewer=%v, want {id, username, name} only", au, rv)
	}
}

func TestGetReviewQueue_wire(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     map[string]any
		reviewer int
		author   int
		check    func(t *testing.T, q url.Values)
	}{
		{
			name:   "author only with filters",
			args:   map[string]any{"group_id": "g1", "roles": []string{"author"}, "state": "merged", "updated_after": "2026-10-01T10:00:00Z", "per_page": 500},
			author: 1,
			check: func(t *testing.T, q url.Values) {
				for k, v := range map[string]string{
					"author_id": "7", "state": "merged", "updated_after": "2026-10-01T10:00:00Z",
					"order_by": "updated_at", "sort": "desc", "per_page": "100", "page": "1",
				} {
					if q.Get(k) != v {
						t.Errorf("%s = %q, want %q", k, q.Get(k), v)
					}
				}
				if q.Has("reviewer_id") {
					t.Error("reviewer_id sent for the author role")
				}
			},
		},
		{
			name:     "reviewer only, duplicate roles collapse, default state, date-only updated_after",
			args:     map[string]any{"group_id": "g1", "roles": []string{"reviewer", "reviewer"}, "updated_after": "2026-10-05"},
			reviewer: 1,
			check: func(t *testing.T, q url.Values) {
				if q.Get("reviewer_id") != "7" || q.Get("state") != "opened" || q.Get("updated_after") != "2026-10-05T00:00:00Z" || q.Has("author_id") {
					t.Errorf("query = %v", q)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newQueueFixture(t, 0, 0)
			if _, errText := f.call(t, tc.args); errText != "" {
				t.Fatal(errText)
			}
			rev, auth := f.listsFor("reviewer_id"), f.listsFor("author_id")
			if len(rev) != tc.reviewer || len(auth) != tc.author {
				t.Fatalf("list requests reviewer=%d author=%d, want %d/%d", len(rev), len(auth), tc.reviewer, tc.author)
			}
			tc.check(t, append(rev, auth...)[0])
		})
	}
}

func TestGetReviewQueue_validation(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"missing group":     {"roles": []string{"author"}},
		"blank group":       {"group_id": "  "},
		"invalid role":      {"group_id": "g1", "roles": []string{"approver"}},
		"invalid state":     {"group_id": "g1", "state": "open"},
		"bad updated_after": {"group_id": "g1", "updated_after": "yesterday"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newQueueFixture(t, 0, 0)
			_, errText := f.call(t, args)
			if errText == "" {
				t.Fatal("want a tool error")
			}
			if f.requests() != 0 {
				t.Fatalf("%d HTTP requests made before validation failed", f.requests())
			}
		})
	}
}

func TestGetReviewQueue_apiErrors(t *testing.T) {
	_, errText := newQueueFixture(t, http.StatusUnauthorized, 0).call(t, map[string]any{"group_id": "g1"})
	if !strings.Contains(errText, "resolve current user") {
		t.Fatalf("user error = %q", errText)
	}
	f := newQueueFixture(t, 0, http.StatusForbidden)
	_, errText = f.call(t, map[string]any{"group_id": "g1"})
	if !strings.Contains(errText, "list reviewer merge requests (page 1)") {
		t.Fatalf("list error = %q", errText)
	}
	if len(f.listsFor("author_id")) != 0 {
		t.Fatal("a failed role must fail the call, not fall through to a partial queue")
	}
}

func TestQueueNilSafety(t *testing.T) {
	if toQueueUser(nil) != nil {
		t.Fatal("nil user must stay nil")
	}
	t0, t1 := time.Unix(1, 0), time.Unix(2, 0)
	for _, tc := range []struct {
		a, b *time.Time
		want int
	}{{nil, nil, 0}, {nil, &t0, -1}, {&t0, nil, 1}, {&t0, &t1, -1}, {&t1, &t0, 1}, {&t0, &t0, 0}} {
		if got := compareTimePtr(tc.a, tc.b); got != tc.want {
			t.Errorf("compareTimePtr(%v, %v) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
	r := newQueueRow(&gitlab.BasicMergeRequest{IID: 1, Reviewers: []*gitlab.BasicUser{nil}})
	if r.Author != nil || r.UpdatedAt != nil || r.Reviewers == nil || len(r.Reviewers) != 0 {
		t.Fatalf("row = %+v, want null author/updated_at and an empty (non-nil) reviewers list", r)
	}
}
