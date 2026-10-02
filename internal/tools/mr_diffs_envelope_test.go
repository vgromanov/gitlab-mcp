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
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

func newMRDiffsDeps(t *testing.T, handler http.Handler) Deps {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(igl.BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	return Deps{Config: &config.Config{Token: "t"}, Client: cli}
}

func mrJSON(projectID, sourceID int64, head string) string {
	return `{"id":1,"iid":1,"project_id":` + strconv.FormatInt(projectID, 10) +
		`,"source_project_id":` + strconv.FormatInt(sourceID, 10) +
		`,"diff_refs":{"head_sha":"` + head + `","base_sha":"b","start_sha":"b"}}`
}

func mrHandler(projectID, sourceID int64, diffsJSON, nextPage string, headerPresent bool) http.Handler {
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
			_, _ = io.WriteString(w, mrJSON(projectID, sourceID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
		case strings.Contains(path, "/projects/"):
			id := projectID
			ns := "g/p"
			if strings.Contains(path, "/"+strconv.FormatInt(sourceID, 10)) && sourceID != projectID {
				id = sourceID
				ns = "fork/p"
			}
			if strings.Contains(path, "/99") {
				id = 99
				ns = "fork/p"
			}
			_, _ = io.WriteString(w, `{"id":`+strconv.FormatInt(id, 10)+`,"path_with_namespace":"`+ns+`"}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	})
}

const knownPresenceDiff = `[{"old_path":"a.go","new_path":"a.go","a_mode":"100644","b_mode":"100644","diff":"+x\n","new_file":false,"renamed_file":false,"deleted_file":false,"generated_file":false,"collapsed":false,"too_large":false}]`

func TestListMergeRequestDiffs_envelopeSuccess(t *testing.T) {
	d := newMRDiffsDeps(t, mrHandler(42, 42, knownPresenceDiff, "", true))
	_, out, err := listMergeRequestDiffs(context.Background(), nil, listMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Diffs) != 1 || out.Diffs[0] == nil || out.Diffs[0].NewPath != "a.go" {
		t.Fatalf("diffs=%+v", out.Diffs)
	}
	if out.Pagination.NextPage != 0 {
		t.Fatalf("next_page=%d", out.Pagination.NextPage)
	}
	if out.Section.CapabilityVersion != readmeta.CapabilityMRDiffsV1 {
		t.Fatal(out.Section.CapabilityVersion)
	}
	if out.Section.ContentComplete != readmeta.ContentCompleteTrue {
		t.Fatalf("content_complete=%q", out.Section.ContentComplete)
	}
	if out.Section.Consistency != readmeta.ConsistencyConsistent {
		t.Fatalf("consistency=%q want verified consistent", out.Section.Consistency)
	}
	if out.Section.ManifestCoverage != readmeta.CoverageFull || out.Section.PatchCoverage != readmeta.CoverageFull {
		t.Fatalf("coverages manifest=%q patch=%q", out.Section.ManifestCoverage, out.Section.PatchCoverage)
	}
	if !out.Section.PaginationExhausted {
		t.Fatal("expected pagination_exhausted true with present empty X-Next-Page")
	}
	if out.Section.HeadSHA == nil || *out.Section.HeadSHA == "" {
		t.Fatal("expected head_sha")
	}
	raw, _ := json.Marshal(out)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	sec := m["section"].(map[string]any)
	if _, ok := sec["content_complete"].(string); !ok {
		t.Fatalf("content_complete type %T", sec["content_complete"])
	}
}

func TestListMergeRequestDiffs_headerAbsentNotExhausted(t *testing.T) {
	d := newMRDiffsDeps(t, mrHandler(42, 42, knownPresenceDiff, "", false))
	_, out, err := listMergeRequestDiffs(context.Background(), nil, listMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	if out.Section.PaginationExhausted {
		t.Fatal("must not claim exhausted when paging headers absent")
	}
	if out.Section.ContentComplete == readmeta.ContentCompleteTrue {
		t.Fatal("must not be complete when paging unknown")
	}
	if out.Section.ManifestCoverage != readmeta.CoverageUnknown || out.Section.PatchCoverage != readmeta.CoverageUnknown {
		t.Fatalf("unknown paging ⇒ unknown coverages, got m=%q p=%q", out.Section.ManifestCoverage, out.Section.PatchCoverage)
	}
	if out.Pagination.NextPage != 0 {
		t.Fatalf("legacy next_page still mirrors SDK (%d)", out.Pagination.NextPage)
	}
}

func TestListMergeRequestDiffs_NextPage0HeaderExplicit(t *testing.T) {
	d := newMRDiffsDeps(t, mrHandler(42, 42, knownPresenceDiff, "0", true))
	_, out, err := listMergeRequestDiffs(context.Background(), nil, listMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Section.PaginationExhausted {
		t.Fatal("explicit X-Next-Page:0 must be exhausted")
	}
	if out.Section.ContentComplete != readmeta.ContentCompleteTrue {
		t.Fatalf("known false presence + explicit exhausted ⇒ complete, got %q", out.Section.ContentComplete)
	}
}

func TestListMergeRequestDiffs_missingPresenceNotComplete(t *testing.T) {
	cases := []struct {
		name string
		diff string
	}{
		{"absent_fields", `[{"old_path":"a.go","new_path":"a.go","diff":"+x\n"}]`},
		{"null_fields", `[{"old_path":"a.go","new_path":"a.go","diff":"+x\n","collapsed":null,"too_large":null}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newMRDiffsDeps(t, mrHandler(42, 42, tc.diff, "", true))
			_, out, err := listMergeRequestDiffs(context.Background(), nil, listMergeRequestDiffsIn{
				pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
			}, d)
			if err != nil {
				t.Fatal(err)
			}
			if out.Section.ContentComplete == readmeta.ContentCompleteTrue {
				t.Fatal("absent/null collapsed|too_large must not yield content_complete=true")
			}
			if out.Section.ContentComplete != readmeta.ContentCompleteUnknown {
				t.Fatalf("want unknown, got %q", out.Section.ContentComplete)
			}
			if out.Section.PatchCoverage != readmeta.CoverageUnknown {
				t.Fatalf("patch_coverage=%q", out.Section.PatchCoverage)
			}
			// Paths are still listed on page1+exhausted ⇒ manifest can be full independently.
			if out.Section.ManifestCoverage != readmeta.CoverageFull {
				t.Fatalf("manifest_coverage=%q", out.Section.ManifestCoverage)
			}
		})
	}
}

func TestListMergeRequestDiffs_collapsedNotComplete(t *testing.T) {
	diff := `[{"old_path":"a.go","new_path":"a.go","diff":"","collapsed":true,"too_large":false}]`
	d := newMRDiffsDeps(t, mrHandler(42, 42, diff, "", true))
	_, out, err := listMergeRequestDiffs(context.Background(), nil, listMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	if out.Section.ContentComplete == readmeta.ContentCompleteTrue {
		t.Fatal("collapsed must not yield complete")
	}
	if out.Section.PatchCoverage != readmeta.CoveragePartial {
		t.Fatalf("patch_coverage=%q", out.Section.PatchCoverage)
	}
	if out.Section.ManifestCoverage != readmeta.CoverageFull {
		t.Fatalf("manifest should stay full when paths known on page1 exhausted, got %q", out.Section.ManifestCoverage)
	}
	found := false
	for _, lim := range out.Section.Limitations {
		if lim.Code == readmeta.CodeCollapsed {
			found = true
		}
	}
	if !found {
		t.Fatal("expected collapsed limitation")
	}
}

func TestListMergeRequestDiffs_invalidHeadSHANotProjected(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/diffs"):
			w.Header().Set("X-Next-Page", "")
			_, _ = io.WriteString(w, knownPresenceDiff)
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, mrJSON(42, 42, "abcdeadbeef")) // invalid short SHA
		case strings.Contains(r.URL.Path, "/projects/"):
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p"}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	})
	d := newMRDiffsDeps(t, h)
	_, out, err := listMergeRequestDiffs(context.Background(), nil, listMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	if out.Section.HeadSHA != nil {
		t.Fatalf("invalid head must project null, got %v", *out.Section.HeadSHA)
	}
	if out.Section.Consistency != readmeta.ConsistencyUnknown {
		t.Fatalf("consistency=%q want unknown without valid head bracket", out.Section.Consistency)
	}
}

func TestListMergeRequestDiffs_consistencyStaysUnknownWithoutBracket(t *testing.T) {
	// No head_sha on MR ⇒ cannot verify consistency.
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/diffs"):
			w.Header().Set("X-Next-Page", "")
			_, _ = io.WriteString(w, knownPresenceDiff)
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":1,"iid":1,"project_id":42,"source_project_id":42,"diff_refs":{}}`)
		case strings.Contains(r.URL.Path, "/projects/"):
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p"}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	})
	d := newMRDiffsDeps(t, h)
	_, out, err := listMergeRequestDiffs(context.Background(), nil, listMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	if out.Section.Consistency != readmeta.ConsistencyUnknown {
		t.Fatalf("consistency=%q want unknown without head bracket evidence", out.Section.Consistency)
	}
}

func TestListMergeRequestDiffs_headDriftInconsistent(t *testing.T) {
	var mrCalls atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/diffs"):
			w.Header().Set("X-Next-Page", "")
			_, _ = io.WriteString(w, knownPresenceDiff)
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			n := mrCalls.Add(1)
			head := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			if n >= 2 {
				head = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			}
			_, _ = io.WriteString(w, mrJSON(42, 42, head))
		case strings.Contains(r.URL.Path, "/projects/"):
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p"}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	})
	d := newMRDiffsDeps(t, h)
	_, out, err := listMergeRequestDiffs(context.Background(), nil, listMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	if out.Section.Consistency != readmeta.ConsistencyInconsistent {
		t.Fatalf("consistency=%q", out.Section.Consistency)
	}
	if out.Section.ContentComplete == readmeta.ContentCompleteTrue {
		t.Fatal("head drift must not claim complete")
	}
	found := false
	for _, lim := range out.Section.Limitations {
		if lim.Code == readmeta.CodeInconsistent {
			found = true
		}
	}
	if !found {
		t.Fatal("expected inconsistent limitation")
	}
}

func TestListMergeRequestDiffs_nonFirstPageNotFullCoverage(t *testing.T) {
	d := newMRDiffsDeps(t, mrHandler(42, 42, knownPresenceDiff, "", true))
	_, out, err := listMergeRequestDiffs(context.Background(), nil, listMergeRequestDiffsIn{
		pidMR:      pidMR{ProjectID: "42", MergeRequestIID: 1},
		Pagination: Pagination{Page: 2, PerPage: 20},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	if out.Section.ContentComplete == readmeta.ContentCompleteTrue {
		t.Fatal("page>1 must not claim content_complete=true")
	}
	if out.Section.ManifestCoverage == readmeta.CoverageFull || out.Section.PatchCoverage == readmeta.CoverageFull {
		t.Fatalf("page>1 must not claim full coverages: m=%q p=%q", out.Section.ManifestCoverage, out.Section.PatchCoverage)
	}
}

func TestListMergeRequestDiffs_overflowCountsRetained(t *testing.T) {
	items := make([]string, 0, 101)
	for i := 0; i < 101; i++ {
		items = append(items, fmt.Sprintf(
			`{"old_path":"f%d.go","new_path":"f%d.go","diff":"+x\n","collapsed":false,"too_large":false}`, i, i))
	}
	body := "[" + strings.Join(items, ",") + "]"
	d := newMRDiffsDeps(t, mrHandler(42, 42, body, "", true))
	_, out, err := listMergeRequestDiffs(context.Background(), nil, listMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Diffs) != igl.DefaultMaxItems {
		t.Fatalf("retained=%d want %d", len(out.Diffs), igl.DefaultMaxItems)
	}
	if out.Section.Counts.Items == nil || *out.Section.Counts.Items != len(out.Diffs) {
		t.Fatalf("counts.items=%v want retained %d", out.Section.Counts.Items, len(out.Diffs))
	}
	if out.Section.ContentComplete != readmeta.ContentCompleteFalse {
		t.Fatalf("content_complete=%q", out.Section.ContentComplete)
	}
	if out.Section.ManifestCoverage != readmeta.CoveragePartial || out.Section.PatchCoverage != readmeta.CoveragePartial {
		t.Fatalf("overflow coverages m=%q p=%q", out.Section.ManifestCoverage, out.Section.PatchCoverage)
	}
	found := false
	for _, lim := range out.Section.Limitations {
		if lim.Code == readmeta.CodeBudgetItems {
			found = true
		}
	}
	if !found {
		t.Fatal("expected budget_items limitation")
	}
}

func TestListMergeRequestDiffs_unknownCountCoverage(t *testing.T) {
	d := newMRDiffsDeps(t, mrHandler(42, 42, knownPresenceDiff, "", false))
	_, out, err := listMergeRequestDiffs(context.Background(), nil, listMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, lim := range out.Section.Limitations {
		if lim.Code == readmeta.CodeUnknownCount {
			found = true
		}
	}
	if !found {
		t.Fatal("expected unknown_count limitation when paging headers absent")
	}
	if out.Section.ManifestCoverage != readmeta.CoverageUnknown || out.Section.PatchCoverage != readmeta.CoverageUnknown {
		t.Fatalf("m=%q p=%q", out.Section.ManifestCoverage, out.Section.PatchCoverage)
	}
}

func TestListMergeRequestDiffs_fatalHTTPNoRawBodyLeak(t *testing.T) {
	const secret = "SECRET_BACKEND_TOKEN_do_not_leak"
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/diffs"):
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"message":"`+secret+`"}`)
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, mrJSON(42, 42, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
		case strings.Contains(r.URL.Path, "/projects/"):
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p"}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	})
	d := newMRDiffsDeps(t, h)
	_, _, err := listMergeRequestDiffs(context.Background(), nil, listMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err == nil {
		t.Fatal("expected fatal error")
	}
	msg := err.Error()
	if !strings.Contains(msg, readmeta.CodeHTTPError) {
		t.Fatalf("err=%v", err)
	}
	if strings.Contains(msg, secret) {
		t.Fatalf("raw backend body leaked into MCP error: %q", msg)
	}
	if strings.Contains(msg, "message") && strings.Contains(msg, secret) {
		t.Fatal("sensitive raw backend body present in MCP error")
	}
}

func TestListMergeRequestDiffs_authzDenied(t *testing.T) {
	d := newMRDiffsDeps(t, mrHandler(42, 42, `[]`, "", true))
	d.Config.AllowedProjectIDs = []string{"99"}
	_, _, err := listMergeRequestDiffs(context.Background(), nil, listMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err == nil {
		t.Fatal("expected authz denied")
	}
	if !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
		t.Fatalf("err=%v", err)
	}
}

func TestAuthorizeCanonicalProject_rejectsNonPositiveID(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/projects/") {
			_, _ = io.WriteString(w, `{"id":0,"path_with_namespace":"g/p"}`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	})
	d := newMRDiffsDeps(t, h)
	_, err := AuthorizeCanonicalProject(context.Background(), d, "0")
	if err == nil {
		t.Fatal("expected reject of zero canonical identity")
	}
	if !strings.Contains(err.Error(), readmeta.CodeIdentityUnresolved) {
		t.Fatalf("err=%v", err)
	}
	// Ensure content endpoint was never hit.
	var diffsHit atomic.Bool
	h2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/diffs") {
			diffsHit.Store(true)
		}
		if strings.Contains(r.URL.Path, "/projects/") {
			_, _ = io.WriteString(w, `{"id":0,"path_with_namespace":"g/p"}`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	})
	d2 := newMRDiffsDeps(t, h2)
	_, _, err = listMergeRequestDiffs(context.Background(), nil, listMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "0", MergeRequestIID: 1},
	}, d2)
	if err == nil {
		t.Fatal("expected fatal before content")
	}
	if diffsHit.Load() {
		t.Fatal("content diffs call must not run after malformed identity")
	}
}

func TestListMergeRequestDiffs_forkAuthz(t *testing.T) {
	d := newMRDiffsDeps(t, mrHandler(42, 99, knownPresenceDiff, "", true))
	d.Config.AllowedProjectIDs = []string{"42", "99"}
	_, out, err := listMergeRequestDiffs(context.Background(), nil, listMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Diffs) != 1 {
		t.Fatal(out.Diffs)
	}
}

func TestAuthorizeAdditionalProjects_syntheticFork(t *testing.T) {
	d := newMRDiffsDeps(t, mrHandler(42, 99, `[]`, "", true))
	d.Config.AllowedProjectIDs = []string{"42", "99"}
	got, err := AuthorizeAdditionalProjects(context.Background(), d, "99")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != 99 {
		t.Fatalf("%+v", got)
	}
}

func TestListMergeRequestDiffs_outputSchemaRegistered(t *testing.T) {
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	}))
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "t"}, nil)
	cfg := &config.Config{Token: "x"}
	RegisterMergeRequests(srv, Deps{Config: cfg, Client: cli})
	cs := testutil.MCPConnect(t, srv)
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, tool := range res.Tools {
		if tool != nil && tool.Name == "list_merge_request_diffs" {
			found = true
			if tool.OutputSchema == nil {
				t.Fatal("expected registered outputSchema")
			}
		}
	}
	if !found {
		t.Fatal("tool not registered")
	}
}

func TestListMergeRequestDiffs_emptyArrayNotNull(t *testing.T) {
	d := newMRDiffsDeps(t, mrHandler(42, 42, `[]`, "", true))
	_, out, err := listMergeRequestDiffs(context.Background(), nil, listMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	if out.Diffs == nil {
		t.Fatal("diffs must be non-nil empty slice")
	}
	if len(out.Diffs) != 0 {
		t.Fatalf("len=%d", len(out.Diffs))
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	arr, ok := m["diffs"].([]any)
	if !ok || arr == nil {
		t.Fatalf("diffs JSON must be [], got %T %#v", m["diffs"], m["diffs"])
	}
	if len(arr) != 0 {
		t.Fatalf("diffs=%v", arr)
	}
	if out.Section.Counts.Items == nil || *out.Section.Counts.Items != 0 {
		t.Fatalf("counts.items=%v", out.Section.Counts.Items)
	}
}

func TestListMergeRequestDiffs_nullAndMalformedEntries(t *testing.T) {
	body := `[null,{"old_path":"a.go","new_path":"a.go","diff":"+x\n","collapsed":false,"too_large":false},123]`
	d := newMRDiffsDeps(t, mrHandler(42, 42, body, "", true))
	_, out, err := listMergeRequestDiffs(context.Background(), nil, listMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Diffs) != 3 {
		t.Fatalf("retained=%d", len(out.Diffs))
	}
	if out.Diffs[0] != nil {
		t.Fatalf("null entry must stay nil, got %+v", out.Diffs[0])
	}
	if out.Diffs[1] == nil || out.Diffs[1].NewPath != "a.go" {
		t.Fatalf("mid=%+v", out.Diffs[1])
	}
	if out.Diffs[2] != nil {
		t.Fatalf("malformed must project as null, got %+v", out.Diffs[2])
	}
	raw, _ := json.Marshal(out)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	arr := m["diffs"].([]any)
	if arr[0] != nil || arr[2] != nil {
		t.Fatalf("JSON null slots lost: %v", arr)
	}
	if out.Section.Counts.Items == nil || *out.Section.Counts.Items != 3 {
		t.Fatalf("counts.items=%v want retained 3", out.Section.Counts.Items)
	}
	if out.Section.ContentComplete == readmeta.ContentCompleteTrue {
		t.Fatal("null/malformed entries must not yield complete")
	}
	if out.Section.ManifestCoverage == readmeta.CoverageFull || out.Section.PatchCoverage == readmeta.CoverageFull {
		t.Fatalf("coverages m=%q p=%q", out.Section.ManifestCoverage, out.Section.PatchCoverage)
	}
}

func TestListMergeRequestDiffs_legacySDKProjectionCompat(t *testing.T) {
	// Same fields as gitlab.MergeRequestDiff; pointer slice matches SDK []*MergeRequestDiff.
	sdkShape, _ := json.Marshal([]*gitlab.MergeRequestDiff{
		{
			OldPath: "a.go", NewPath: "a.go", AMode: "100644", BMode: "100644",
			Diff: "+x\n", Collapsed: false, TooLarge: false,
		},
		nil,
	})
	var sdkArr []any
	_ = json.Unmarshal(sdkShape, &sdkArr)

	d := newMRDiffsDeps(t, mrHandler(42, 42,
		`[{"old_path":"a.go","new_path":"a.go","a_mode":"100644","b_mode":"100644","diff":"+x\n","new_file":false,"renamed_file":false,"deleted_file":false,"generated_file":false,"collapsed":false,"too_large":false},null]`,
		"", true))
	_, out, err := listMergeRequestDiffs(context.Background(), nil, listMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(out.Diffs)
	var gotArr []any
	_ = json.Unmarshal(got, &gotArr)
	if len(gotArr) != len(sdkArr) {
		t.Fatalf("len got=%d sdk=%d", len(gotArr), len(sdkArr))
	}
	if gotArr[1] != nil || sdkArr[1] != nil {
		t.Fatalf("null element projection mismatch got=%v sdk=%v", gotArr[1], sdkArr[1])
	}
	g0 := gotArr[0].(map[string]any)
	s0 := sdkArr[0].(map[string]any)
	for _, k := range []string{"old_path", "new_path", "a_mode", "b_mode", "diff", "new_file", "renamed_file", "deleted_file", "generated_file", "collapsed", "too_large"} {
		if _, ok := g0[k]; !ok {
			t.Fatalf("missing SDK field %q in adapter projection", k)
		}
		if fmt.Sprint(g0[k]) != fmt.Sprint(s0[k]) {
			t.Fatalf("field %q got=%v sdk=%v", k, g0[k], s0[k])
		}
	}

	// Registered outputSchema must accept this structured envelope.
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	}))
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "t"}, nil)
	RegisterMergeRequests(srv, Deps{Config: &config.Config{Token: "x"}, Client: cli})
	cs := testutil.MCPConnect(t, srv)
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	for _, tool := range res.Tools {
		if tool != nil && tool.Name == "list_merge_request_diffs" {
			switch s := tool.OutputSchema.(type) {
			case map[string]any:
				schema = s
			default:
				b, _ := json.Marshal(tool.OutputSchema)
				_ = json.Unmarshal(b, &schema)
			}
		}
	}
	if schema == nil {
		t.Fatal("missing outputSchema")
	}
	props, _ := schema["properties"].(map[string]any)
	diffsSchema, _ := props["diffs"].(map[string]any)
	if diffsSchema == nil {
		t.Fatalf("outputSchema diffs missing: %#v", schema)
	}
	// Pointer slice ⇒ JSON Schema often encodes as ["null","array"] or type=array.
	switch typ := diffsSchema["type"].(type) {
	case string:
		if typ != "array" {
			t.Fatalf("diffs schema type=%v", typ)
		}
	case []any:
		ok := false
		for _, v := range typ {
			if v == "array" {
				ok = true
			}
		}
		if !ok {
			t.Fatalf("diffs schema type=%v", typ)
		}
	default:
		t.Fatalf("diffs schema type=%T %#v", diffsSchema["type"], diffsSchema["type"])
	}
}

func TestListMergeRequestDiffs_truncatedStreamPartialNotHTTP(t *testing.T) {
	// One full object then truncated (no closing ']').
	body := `[{"old_path":"a.go","new_path":"a.go","diff":"+x\n","collapsed":false,"too_large":false}`
	d := newMRDiffsDeps(t, mrHandler(42, 42, body, "", true))
	_, out, err := listMergeRequestDiffs(context.Background(), nil, listMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatalf("partial retain must not be fatal err: %v", err)
	}
	if len(out.Diffs) < 1 {
		t.Fatal("expected retained item")
	}
	var codes []string
	for _, lim := range out.Section.Limitations {
		codes = append(codes, lim.Code)
		if lim.Code == readmeta.CodeHTTPError {
			t.Fatalf("stream framing must not use http_error: %+v", out.Section.Limitations)
		}
	}
	found := false
	for _, c := range codes {
		if c == readmeta.CodePartial {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected partial limitation, got %v", codes)
	}
}

func TestReadmetaInformationalHelpers(t *testing.T) {
	if c := readmeta.InformationalNextCursor(0); c != nil {
		t.Fatal("expected nil cursor")
	}
	c := readmeta.InformationalNextCursor(3)
	if c == nil || *c != "3" {
		t.Fatalf("%v", c)
	}
	s := readmeta.NewMRDiffsSection(time.Now())
	s.AddLimitation("", "ignored")
	if len(s.Limitations) != 0 {
		t.Fatal(s.Limitations)
	}
	setContentComplete(&s, readmeta.ContentCompleteTrue, true)
	if s.ContentComplete != readmeta.ContentCompleteUnknown {
		t.Fatalf("latch overwrite=%q", s.ContentComplete)
	}
}

func TestPresenceQuartetViaHelpers(t *testing.T) {
	cases := map[string]readmeta.Presence{
		`{}`:                      readmeta.PresenceAbsent,
		`{"allow_failure":null}`:  readmeta.PresenceNull,
		`{"allow_failure":false}`: readmeta.PresenceFalse,
		`{"allow_failure":true}`:  readmeta.PresenceTrue,
	}
	for raw, want := range cases {
		p, err := igl.AllowFailurePresence(json.RawMessage(raw))
		if err != nil || p != want {
			t.Fatalf("%s: got %s err=%v", raw, p, err)
		}
	}
}
