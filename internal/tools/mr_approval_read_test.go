package tools

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

type approvalCallCounts struct {
	approvalState atomic.Int64
	approvals     atomic.Int64
	mrGet         atomic.Int64
	projectGet    atomic.Int64
}

func approvalFixtureHandler(counts *approvalCallCounts, route func(w http.ResponseWriter, r *http.Request, path string) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(path, "/approval_state"):
			counts.approvalState.Add(1)
			if route != nil && route(w, r, path) {
				return
			}
			_, _ = io.WriteString(w, `{"approval_rules_overwritten":false,"rules":[{"id":1,"name":"default","approvals_required":1,"approved":true,"contains_hidden_groups":false}]}`)
		case strings.Contains(path, "/approvals"):
			counts.approvals.Add(1)
			if route != nil && route(w, r, path) {
				return
			}
			_, _ = io.WriteString(w, `{"approved":true,"approvals_required":1,"approvals_left":0,"user_has_approved":false,"user_can_approve":true,"approval_rules_left":[]}`)
		case strings.Contains(path, "/merge_requests/") && r.Method == http.MethodGet:
			counts.mrGet.Add(1)
			if route != nil && route(w, r, path) {
				return
			}
			_, _ = io.WriteString(w, `{"id":1,"iid":1,"project_id":42,"source_project_id":42,"diff_refs":{"head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","base_sha":"b","start_sha":"b"}}`)
		case strings.Contains(path, "/projects/"):
			counts.projectGet.Add(1)
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}}`)
		default:
			http.NotFound(w, r)
		}
	})
}

func newApprovalDepsHTTP(t *testing.T, h http.Handler) (Deps, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(igl.BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	return Deps{Config: &config.Config{Token: "t"}, Client: cli}, ts
}

func newApprovalDepsTLS(t *testing.T, h http.Handler) (Deps, *httptest.Server) {
	t.Helper()
	ts := httptest.NewTLSServer(h)
	t.Cleanup(ts.Close)
	httpClient := ts.Client()
	if httpClient.Transport == nil {
		t.Fatal("tls client transport missing")
	}
	httpClient.Transport = igl.BudgetInterceptor()(httpClient.Transport)
	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithHTTPClient(httpClient),
	)
	if err != nil {
		t.Fatal(err)
	}
	return Deps{Config: &config.Config{Token: "t"}, Client: cli}, ts
}

func callApproval(t *testing.T, d Deps, ctx context.Context) (map[string]any, error) {
	t.Helper()
	_, out, err := getMergeRequestApprovalState(ctx, nil, getMergeRequestApprovalStateIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var tree map[string]any
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatal(err)
	}
	return tree, nil
}

func approvalShapeKeys(t *testing.T, m map[string]any) {
	t.Helper()
	for _, key := range []string{
		"endpoint", "approved", "actor_approved", "actor_can_approve",
		"approvals_required", "approvals_left", "rules", "rules_left",
		"rules_capability", "rules_complete", "section",
	} {
		if _, ok := m[key]; !ok {
			t.Fatalf("missing key %s", key)
		}
	}
	sec, ok := m["section"].(map[string]any)
	if !ok {
		t.Fatal("section type")
	}
	for _, key := range []string{
		"retrieved_at", "source", "provider", "capability_version", "head_sha",
		"pagination_exhausted", "content_complete", "consistency", "limitations",
		"next_cursor", "counts",
	} {
		if _, ok := sec[key]; !ok {
			t.Fatalf("section missing %s", key)
		}
	}
	if _, ok := sec["content_complete"].(string); !ok {
		t.Fatalf("content_complete type %T", sec["content_complete"])
	}
	if sec["capability_version"] != capabilityMRApprovalsV1 {
		t.Fatal(sec["capability_version"])
	}
	if sec["head_sha"] != nil {
		t.Fatalf("head_sha=%v", sec["head_sha"])
	}
}

func callApprovalMCPStreamable(t *testing.T, d Deps) (*mcp.CallToolResult, error) {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "t"}, nil)
	RegisterAll(srv, d)
	cs := testutil.StreamableMCPConnect(t, srv)
	return cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "get_merge_request_approval_state",
		Arguments: map[string]any{
			"project_id":        "42",
			"merge_request_iid": 1,
		},
	})
}

func structuredApprovalMap(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res == nil {
		t.Fatal("nil CallToolResult")
	}
	if res.IsError {
		t.Fatalf("IsError text=%q", toolErrorText(t, res))
	}
	if res.StructuredContent == nil {
		t.Fatal("StructuredContent nil — MCP schema transport not exercised")
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMRApproval_primarySuccess_1_0(t *testing.T) {
	counts := &approvalCallCounts{}
	d, _ := newApprovalDepsHTTP(t, approvalFixtureHandler(counts, nil))
	out, err := callApproval(t, d, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if counts.approvalState.Load() != 1 || counts.approvals.Load() != 0 {
		t.Fatalf("P/F=%d/%d", counts.approvalState.Load(), counts.approvals.Load())
	}
	if out["endpoint"] != endpointApprovalState {
		t.Fatalf("endpoint=%v", out["endpoint"])
	}
	// Overall stays unknown on approval_state — never inferred from rules.
	if out["approved"] != nil {
		t.Fatalf("approved=%v want null", out["approved"])
	}
	if out["rules_complete"] != readmeta.ContentCompleteUnknown {
		t.Fatalf("rules_complete=%v", out["rules_complete"])
	}
	sec := out["section"].(map[string]any)
	if sec["capability_version"] != capabilityMRApprovalsV1 {
		t.Fatal(sec["capability_version"])
	}
	if sec["head_sha"] != nil {
		t.Fatalf("head_sha=%v", sec["head_sha"])
	}
	if sec["consistency"] != readmeta.ConsistencyUnknown {
		t.Fatal(sec["consistency"])
	}
}

func TestMRApproval_primaryEmptyObject_unknownFields(t *testing.T) {
	counts := &approvalCallCounts{}
	d, _ := newApprovalDepsHTTP(t, approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
		if strings.Contains(path, "/approval_state") {
			_, _ = io.WriteString(w, `{}`)
			return true
		}
		return false
	}))
	out, err := callApproval(t, d, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out["approved"] != nil {
		t.Fatalf("approved=%v", out["approved"])
	}
	if out["rules"] != nil {
		t.Fatalf("rules=%v want null", out["rules"])
	}
	if out["rules_capability"] != rulesCapabilityUnknown {
		t.Fatal(out["rules_capability"])
	}
	sec := out["section"].(map[string]any)
	countsMap := sec["counts"].(map[string]any)
	if countsMap["items"] != nil {
		t.Fatalf("counts.items=%v want null", countsMap["items"])
	}
}

func TestMRApproval_explicitFalseZeroDistinct_overallStillUnknown(t *testing.T) {
	counts := &approvalCallCounts{}
	d, _ := newApprovalDepsHTTP(t, approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
		if strings.Contains(path, "/approval_state") {
			_, _ = io.WriteString(w, `{"approval_rules_overwritten":false,"rules":[{"id":0,"name":"","approvals_required":0,"approved":false,"contains_hidden_groups":false}]}`)
			return true
		}
		return false
	}))
	out, err := callApproval(t, d, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out["approved"] != nil {
		t.Fatalf("approved must stay unknown, got %v", out["approved"])
	}
	rules, _ := out["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("rules=%v", rules)
	}
	rule := rules[0].(map[string]any)
	if rule["approved"] != false {
		t.Fatal(rule["approved"])
	}
	if out["rules_complete"] != readmeta.ContentCompleteUnknown {
		t.Fatal(out["rules_complete"])
	}
}

func TestMRApproval_hiddenGroups_completeFalse_overallUnknown(t *testing.T) {
	counts := &approvalCallCounts{}
	d, _ := newApprovalDepsHTTP(t, approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
		if strings.Contains(path, "/approval_state") {
			_, _ = io.WriteString(w, `{"rules":[{"id":1,"approved":true,"contains_hidden_groups":true}]}`)
			return true
		}
		return false
	}))
	out, err := callApproval(t, d, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out["approved"] != nil {
		t.Fatal(out["approved"])
	}
	if out["rules_complete"] != readmeta.ContentCompleteFalse {
		t.Fatal(out["rules_complete"])
	}
}

func TestMRApproval_wrongApprovalRulesOverwrittenType_failClosed(t *testing.T) {
	counts := &approvalCallCounts{}
	d, _ := newApprovalDepsHTTP(t, approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
		if strings.Contains(path, "/approval_state") {
			_, _ = io.WriteString(w, `{"approval_rules_overwritten":"yes","rules":[]}`)
			return true
		}
		return false
	}))
	_, err := callApproval(t, d, context.Background())
	if err == nil {
		t.Fatal("want malformed")
	}
	if counts.approvals.Load() != 0 {
		t.Fatal("fallback must not run")
	}
}

func TestMRApproval_malformedPrimary_noFallback(t *testing.T) {
	for _, body := range []string{`null`, `[]`, `{"rules":true}`, `{"rules":[]}trailing`, `{`} {
		counts := &approvalCallCounts{}
		body := body
		d, _ := newApprovalDepsHTTP(t, approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
			if strings.Contains(path, "/approval_state") {
				_, _ = io.WriteString(w, body)
				return true
			}
			return false
		}))
		_, err := callApproval(t, d, context.Background())
		if err == nil {
			t.Fatalf("body %q: want error", body)
		}
		if counts.approvals.Load() != 0 {
			t.Fatalf("body %q: fallback=%d", body, counts.approvals.Load())
		}
		if strings.Contains(err.Error(), "http://") || strings.Contains(err.Error(), "api/v4") {
			t.Fatalf("leaked url: %v", err)
		}
	}
}

func TestMRApproval_deniedStatuses_noFallback(t *testing.T) {
	for _, status := range []int{401, 403, 404, 429, 500, 502, 503, 504} {
		status := status
		counts := &approvalCallCounts{}
		d, _ := newApprovalDepsHTTP(t, approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
			if strings.Contains(path, "/approval_state") {
				w.Header().Set("Allow", "POST, OPTIONS")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"message":"x"}`)
				return true
			}
			return false
		}))
		_, err := callApproval(t, d, context.Background())
		if err == nil {
			t.Fatalf("status %d: want error", status)
		}
		if counts.approvalState.Load() != 1 || counts.approvals.Load() != 0 {
			t.Fatalf("status %d: P/F=%d/%d", status, counts.approvalState.Load(), counts.approvals.Load())
		}
	}
}

func TestMRApproval_authzDenied_0_0(t *testing.T) {
	counts := &approvalCallCounts{}
	d, _ := newApprovalDepsHTTP(t, approvalFixtureHandler(counts, nil))
	d.Config = &config.Config{Token: "t", AllowedProjectIDs: []string{"99"}}
	_, err := callApproval(t, d, context.Background())
	if err == nil {
		t.Fatal("expected authz deny")
	}
	if counts.approvalState.Load() != 0 || counts.approvals.Load() != 0 {
		t.Fatalf("P/F=%d/%d", counts.approvalState.Load(), counts.approvals.Load())
	}
}

func TestMRApproval_missingOwnerSourceMetadata_bothModes_0_0(t *testing.T) {
	modes := []struct {
		name string
		cfg  *config.Config
	}{
		{"inactive", &config.Config{Token: "t"}},
		{"active", &config.Config{Token: "t", AllowedProjectIDs: []string{"42"}}},
	}
	for _, mode := range modes {
		for _, mrBody := range []string{
			`{"id":1,"iid":1,"project_id":0,"source_project_id":42}`,
			`{"id":1,"iid":1,"project_id":42,"source_project_id":0}`,
			`{"id":1,"iid":2,"project_id":42,"source_project_id":42}`,
			`{"id":1,"iid":1,"project_id":99,"source_project_id":42}`,
		} {
			t.Run(mode.name+"/"+mrBody[:40], func(t *testing.T) {
				counts := &approvalCallCounts{}
				mrBody := mrBody
				d, _ := newApprovalDepsHTTP(t, approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
					if strings.Contains(path, "/merge_requests/") && !strings.Contains(path, "/approval") {
						_, _ = io.WriteString(w, mrBody)
						return true
					}
					return false
				}))
				d.Config = mode.cfg
				_, err := callApproval(t, d, context.Background())
				if err == nil {
					t.Fatal("want identity failure")
				}
				if counts.approvalState.Load() != 0 || counts.approvals.Load() != 0 {
					t.Fatalf("P/F=%d/%d", counts.approvalState.Load(), counts.approvals.Load())
				}
			})
		}
	}
}

func TestMRApproval_exact405_multiAllow_fallback_1_1(t *testing.T) {
	counts := &approvalCallCounts{}
	d, _ := newApprovalDepsTLS(t, approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
		if strings.Contains(path, "/approval_state") {
			w.Header().Add("Allow", "POST")
			w.Header().Add("Allow", "OPTIONS, PUT")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = io.WriteString(w, `{"message":"method not allowed"}`)
			return true
		}
		return false
	}))
	out, err := callApproval(t, d, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if counts.approvalState.Load() != 1 || counts.approvals.Load() != 1 {
		t.Fatalf("P/F=%d/%d", counts.approvalState.Load(), counts.approvals.Load())
	}
	if out["endpoint"] != endpointApprovals {
		t.Fatalf("endpoint=%v", out["endpoint"])
	}
	if out["approved"] != true {
		t.Fatalf("approved=%v", out["approved"])
	}
	if out["actor_approved"] != false {
		t.Fatalf("actor_approved=%v", out["actor_approved"])
	}
	if out["rules_capability"] != rulesCapabilityPartial {
		t.Fatal(out["rules_capability"])
	}
	sec := out["section"].(map[string]any)
	if sec["head_sha"] != nil {
		t.Fatal("head_sha must stay null")
	}
	countsMap := sec["counts"].(map[string]any)
	if countsMap["items"] != float64(0) {
		t.Fatalf("explicit [] should count 0, got %v", countsMap["items"])
	}
}

func TestMRApproval_legacyMissingRulesLeft_unknownCounts(t *testing.T) {
	counts := &approvalCallCounts{}
	d, _ := newApprovalDepsTLS(t, approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
		if strings.Contains(path, "/approval_state") {
			w.Header().Set("Allow", "POST")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return true
		}
		if strings.Contains(path, "/approvals") {
			_, _ = io.WriteString(w, `{"approved":false,"user_has_approved":null,"user_can_approve":true}`)
			return true
		}
		return false
	}))
	out, err := callApproval(t, d, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out["rules_left"] != nil {
		t.Fatalf("rules_left=%v want null", out["rules_left"])
	}
	if out["rules_capability"] != rulesCapabilityUnknown {
		t.Fatal(out["rules_capability"])
	}
	sec := out["section"].(map[string]any)
	if sec["counts"].(map[string]any)["items"] != nil {
		t.Fatal("counts.items must be null when rules_left absent")
	}
	if out["actor_approved"] != nil {
		t.Fatal("null actor_approved must stay unknown")
	}
}

func TestMRApproval_ambiguousAllow_orTrust_noFallback(t *testing.T) {
	cases := []struct {
		name string
		tls  bool
		hdr  func(w http.ResponseWriter)
	}{
		{"missing", true, func(w http.ResponseWriter) {}},
		{"empty", true, func(w http.ResponseWriter) { w.Header().Set("Allow", "") }},
		{"whitespace", true, func(w http.ResponseWriter) { w.Header().Set("Allow", "   ") }},
		{"quoted", true, func(w http.ResponseWriter) { w.Header().Set("Allow", `"POST"`) }},
		{"get", true, func(w http.ResponseWriter) { w.Header().Set("Allow", "GET, POST") }},
		{"Get", true, func(w http.ResponseWriter) { w.Header().Set("Allow", "Get, POST") }},
		{"get_lower", true, func(w http.ResponseWriter) { w.Header().Set("Allow", "post, get") }},
		{"http_no_tls", false, func(w http.ResponseWriter) { w.Header().Set("Allow", "POST, OPTIONS") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			counts := &approvalCallCounts{}
			h := approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
				if strings.Contains(path, "/approval_state") {
					tc.hdr(w)
					w.WriteHeader(http.StatusMethodNotAllowed)
					_, _ = io.WriteString(w, `{}`)
					return true
				}
				return false
			})
			var d Deps
			if tc.tls {
				d, _ = newApprovalDepsTLS(t, h)
			} else {
				d, _ = newApprovalDepsHTTP(t, h)
			}
			_, err := callApproval(t, d, context.Background())
			if err == nil {
				t.Fatal("want error")
			}
			if counts.approvals.Load() != 0 {
				t.Fatalf("fallback=%d", counts.approvals.Load())
			}
		})
	}
}

func TestMatchesConfiguredTarget_exactBasePathAndMethod(t *testing.T) {
	base, err := url.Parse("https://gitlab.example/api/v4/")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(method, rawURL string) *http.Response {
		u, err := url.Parse(rawURL)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{Request: &http.Request{Method: method, URL: u}}
	}
	good := "https://gitlab.example/api/v4/projects/42/merge_requests/1/approval_state"
	if !matchesConfiguredTarget(mk(http.MethodGet, good), base, "42", 1, endpointApprovalState) {
		t.Fatal("exact target should match")
	}
	// Wrong API prefix that still suffixes the resource path.
	if matchesConfiguredTarget(mk(http.MethodGet, "https://gitlab.example/api/v4/evil/api/v4/projects/42/merge_requests/1/approval_state"), base, "42", 1, endpointApprovalState) {
		t.Fatal("suffix-only must not match")
	}
	if matchesConfiguredTarget(mk("get", good), base, "42", 1, endpointApprovalState) {
		t.Fatal("method EqualFold must not match")
	}
	if matchesConfiguredTarget(mk(http.MethodPost, good), base, "42", 1, endpointApprovalState) {
		t.Fatal("POST must not match")
	}
	if matchesConfiguredTarget(mk(http.MethodGet, good+"?x=1"), base, "42", 1, endpointApprovalState) {
		t.Fatal("query must not match")
	}
	if matchesConfiguredTarget(mk(http.MethodGet, good+"#f"), base, "42", 1, endpointApprovalState) {
		t.Fatal("fragment must not match")
	}
	altBase, _ := url.Parse("https://gitlab.example/api/v4/custom/")
	if matchesConfiguredTarget(mk(http.MethodGet, good), altBase, "42", 1, endpointApprovalState) {
		t.Fatal("changed base path must not match")
	}
}

func TestMRApproval_redirectExactOriginalURL_awayBack_budgeted_noFallback(t *testing.T) {
	// Exact original URL away/back (no query mutation): project + MR + approval_state
	// + hop redirect target + approval_state again = 5 budgeted RoundTrips; P/F=1/0.
	counts := &approvalCallCounts{}
	var hops atomic.Int64
	base := approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
		if strings.HasSuffix(path, "/approval_state") {
			n := hops.Add(1)
			if n == 1 {
				http.Redirect(w, r, "/approval-hop", http.StatusFound)
				return true
			}
			w.Header().Set("Allow", "POST, OPTIONS")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = io.WriteString(w, `{}`)
			return true
		}
		return false
	})
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/approval-hop" {
			http.Redirect(w, r, "/api/v4/projects/42/merge_requests/1/approval_state", http.StatusFound)
			return
		}
		base.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	httpClient := ts.Client()
	httpClient.Transport = igl.BudgetInterceptor()(httpClient.Transport)
	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithHTTPClient(httpClient),
	)
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Config: &config.Config{Token: "t"}, Client: cli}
	b := &igl.Budget{MaxItems: 100, MaxBytes: 1 << 20, MaxRequests: 16, MaxElapsed: time.Minute}
	ctx := igl.WithBudget(context.Background(), b)
	defer b.Cancel()
	_, err = callApproval(t, d, ctx)
	if err == nil {
		t.Fatal("want reject redirected 405")
	}
	if counts.approvals.Load() != 0 {
		t.Fatalf("fallback=%d", counts.approvals.Load())
	}
	if counts.approvalState.Load() != 2 {
		t.Fatalf("approval_state hits=%d want 2 (away+back)", counts.approvalState.Load())
	}
	rq, _, _ := b.Stats()
	if rq != 5 {
		t.Fatalf("budgeted round trips=%d want 5 (project+MR+state+hop+state)", rq)
	}
}

func TestMRApproval_fallbackFailure_stop(t *testing.T) {
	counts := &approvalCallCounts{}
	d, _ := newApprovalDepsTLS(t, approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
		if strings.Contains(path, "/approval_state") {
			w.Header().Set("Allow", "POST, OPTIONS")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = io.WriteString(w, `{}`)
			return true
		}
		if strings.Contains(path, "/approvals") {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"message":"denied"}`)
			return true
		}
		return false
	}))
	_, err := callApproval(t, d, context.Background())
	if err == nil {
		t.Fatal("want error")
	}
	if counts.approvalState.Load() != 1 || counts.approvals.Load() != 1 {
		t.Fatalf("P/F=%d/%d", counts.approvalState.Load(), counts.approvals.Load())
	}
}

func TestMRApproval_headSHANeverFromMR(t *testing.T) {
	counts := &approvalCallCounts{}
	d, _ := newApprovalDepsHTTP(t, approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
		if strings.Contains(path, "/approval_state") {
			_, _ = io.WriteString(w, `{"rules":[],"head_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`)
			return true
		}
		return false
	}))
	out, err := callApproval(t, d, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sec := out["section"].(map[string]any)
	if sec["head_sha"] != nil {
		t.Fatalf("head_sha=%v", sec["head_sha"])
	}
}

func TestMRApproval_itemBudgetExhausted_beforeNextRule(t *testing.T) {
	counts := &approvalCallCounts{}
	d, _ := newApprovalDepsHTTP(t, approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
		if strings.Contains(path, "/approval_state") {
			_, _ = io.WriteString(w, `{"rules":[{"id":1,"approved":true},{"id":2,"approved":false}]}`)
			return true
		}
		return false
	}))
	b := &igl.Budget{MaxItems: 1, MaxBytes: 1 << 20, MaxRequests: 16, MaxElapsed: time.Minute}
	ctx := igl.WithBudget(context.Background(), b)
	defer b.Cancel()
	_, err := callApproval(t, d, ctx)
	if err == nil {
		t.Fatal("want item budget error")
	}
	if !strings.Contains(err.Error(), readmeta.CodeBudgetItems) {
		t.Fatalf("err=%v", err)
	}
	if counts.approvals.Load() != 0 {
		t.Fatal("no fallback after item exhaustion")
	}
}

func TestMRApproval_preContentBudgetExhaustion_0_0(t *testing.T) {
	// Retained separately from post-405 phase tests: these limits stop before primary.
	for _, kind := range []string{"bytes128", "requests2"} {
		t.Run(kind, func(t *testing.T) {
			counts := &approvalCallCounts{}
			d, _ := newApprovalDepsTLS(t, approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
				if strings.Contains(path, "/approval_state") {
					w.Header().Set("Allow", "POST, OPTIONS")
					w.WriteHeader(http.StatusMethodNotAllowed)
					_, _ = io.WriteString(w, strings.Repeat("x", 4096))
					return true
				}
				return false
			}))
			b := &igl.Budget{MaxItems: 100, MaxBytes: 1 << 20, MaxRequests: 16, MaxElapsed: time.Minute}
			if kind == "bytes128" {
				b.MaxBytes = 128
			} else {
				b.MaxRequests = 2
			}
			ctx := igl.WithBudget(context.Background(), b)
			defer b.Cancel()
			_, err := callApproval(t, d, ctx)
			if err == nil || counts.approvalState.Load() != 0 || counts.approvals.Load() != 0 {
				t.Fatalf("want pre-content P/F=0/0; got P/F=%d/%d err=%v", counts.approvalState.Load(), counts.approvals.Load(), err)
			}
		})
	}
}

func TestMRApproval_cancelBeforeContent(t *testing.T) {
	counts := &approvalCallCounts{}
	d, _ := newApprovalDepsHTTP(t, approvalFixtureHandler(counts, nil))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := callApproval(t, d, ctx)
	if err == nil {
		t.Fatal("want cancel error")
	}
	if counts.approvalState.Load() != 0 {
		t.Fatalf("dispatched after cancel: %d", counts.approvalState.Load())
	}
}

func TestMRApproval_sensitiveErrorProjection(t *testing.T) {
	counts := &approvalCallCounts{}
	d, _ := newApprovalDepsHTTP(t, approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
		if strings.Contains(path, "/approval_state") {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"message":"token=SECRET project=g/p header=yes"}`)
			return true
		}
		return false
	}))
	_, err := callApproval(t, d, context.Background())
	if err == nil {
		t.Fatal("want error")
	}
	msg := err.Error()
	for _, bad := range []string{"SECRET", "g/p", "api/v4", "http://", "token="} {
		if strings.Contains(msg, bad) {
			t.Fatalf("leaked %q in %q", bad, msg)
		}
	}
}

func TestMRApproval_MCPBothProfiles_primaryAndFallback_streamable(t *testing.T) {
	for _, profile := range []string{"review_read", "review_write"} {
		profile := profile
		t.Run(profile+"_primary", func(t *testing.T) {
			counts := &approvalCallCounts{}
			d, _ := newApprovalDepsHTTP(t, approvalFixtureHandler(counts, nil))
			d.Config = &config.Config{Token: "t", ToolProfile: profile}
			res, err := callApprovalMCPStreamable(t, d)
			if err != nil {
				t.Fatal(err)
			}
			m := structuredApprovalMap(t, res)
			approvalShapeKeys(t, m)
			if m["endpoint"] != endpointApprovalState {
				t.Fatal(m["endpoint"])
			}
			if counts.approvalState.Load() != 1 || counts.approvals.Load() != 0 {
				t.Fatalf("P/F=%d/%d", counts.approvalState.Load(), counts.approvals.Load())
			}
		})
		t.Run(profile+"_fallback", func(t *testing.T) {
			counts := &approvalCallCounts{}
			d, _ := newApprovalDepsTLS(t, approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
				if strings.Contains(path, "/approval_state") {
					w.Header().Set("Allow", "POST, OPTIONS")
					w.WriteHeader(http.StatusMethodNotAllowed)
					return true
				}
				return false
			}))
			d.Config = &config.Config{Token: "t", ToolProfile: profile}
			res, err := callApprovalMCPStreamable(t, d)
			if err != nil {
				t.Fatal(err)
			}
			m := structuredApprovalMap(t, res)
			approvalShapeKeys(t, m)
			if m["endpoint"] != endpointApprovals {
				t.Fatal(m["endpoint"])
			}
			if counts.approvalState.Load() != 1 || counts.approvals.Load() != 1 {
				t.Fatalf("P/F=%d/%d", counts.approvalState.Load(), counts.approvals.Load())
			}
		})
	}
}

func TestMRApproval_getHiddenInSecondAllowField_noFallback(t *testing.T) {
	counts := &approvalCallCounts{}
	d, _ := newApprovalDepsTLS(t, approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
		if strings.Contains(path, "/approval_state") {
			w.Header().Add("Allow", "POST")
			w.Header().Add("Allow", "OPTIONS, GET")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return true
		}
		return false
	}))
	_, err := callApproval(t, d, context.Background())
	if err == nil {
		t.Fatal("want reject")
	}
	if counts.approvals.Load() != 0 {
		t.Fatal(counts.approvals.Load())
	}
}

func TestMRApproval_redirected200_rejected(t *testing.T) {
	counts := &approvalCallCounts{}
	var primary *httptest.Server
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(path, "/approval_state"):
			counts.approvalState.Add(1)
			if r.URL.Query().Get("hop") == "" {
				http.Redirect(w, r, primary.URL+"/api/v4/projects/42/merge_requests/1/approval_state?hop=1", http.StatusFound)
				return
			}
			_, _ = io.WriteString(w, `{"approval_rules_overwritten":false,"rules":[]}`)
		case strings.Contains(path, "/approvals"):
			counts.approvals.Add(1)
			_, _ = io.WriteString(w, `{"approved":true}`)
		case strings.Contains(path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":1,"iid":1,"project_id":42,"source_project_id":42}`)
		case strings.Contains(path, "/projects/"):
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}}`)
		default:
			http.NotFound(w, r)
		}
	})
	primary = httptest.NewTLSServer(h)
	t.Cleanup(primary.Close)
	httpClient := primary.Client()
	httpClient.Transport = igl.BudgetInterceptor()(httpClient.Transport)
	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(primary.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithHTTPClient(httpClient),
	)
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Config: &config.Config{Token: "t"}, Client: cli}
	_, err = callApproval(t, d, context.Background())
	if err == nil {
		t.Fatal("want redirected success rejected")
	}
	if counts.approvals.Load() != 0 {
		t.Fatal("no fallback")
	}
	if counts.approvalState.Load() < 2 {
		t.Fatalf("redirect hops not observed: %d", counts.approvalState.Load())
	}
}

func TestMRApproval_nilResponseRequestURL_noFallback(t *testing.T) {
	base, _ := url.Parse("https://gitlab.example/api/v4/")
	if qualifiesApprovalStateMethodFallback(Deps{}, "42", 1, approvalRawRead{Status: 405, Response: nil}) {
		t.Fatal("nil response")
	}
	resp := &http.Response{StatusCode: 405, Header: http.Header{"Allow": []string{"POST"}}, Request: nil}
	if matchesConfiguredTarget(resp, base, "42", 1, endpointApprovalState) {
		t.Fatal("nil request")
	}
	resp.Request = &http.Request{Method: http.MethodGet, URL: nil}
	if matchesConfiguredTarget(resp, base, "42", 1, endpointApprovalState) {
		t.Fatal("nil URL")
	}
	if tlsVerifiedTrust(nil) || tlsVerifiedTrust(&http.Response{}) {
		t.Fatal("missing TLS")
	}
}

func TestMRApproval_insecureTLS_noFallback(t *testing.T) {
	counts := &approvalCallCounts{}
	ts := httptest.NewTLSServer(approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
		if strings.Contains(path, "/approval_state") {
			w.Header().Set("Allow", "POST, OPTIONS")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return true
		}
		return false
	}))
	t.Cleanup(ts.Close)
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}
	httpClient := &http.Client{Transport: igl.BudgetInterceptor()(tr)}
	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithHTTPClient(httpClient),
	)
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Config: &config.Config{Token: "t"}, Client: cli}
	_, err = callApproval(t, d, context.Background())
	if err == nil {
		t.Fatal("disabled TLS verification must fail closed")
	}
	if counts.approvals.Load() != 0 {
		t.Fatal(counts.approvals.Load())
	}
}

func TestMRApproval_byteBudgetOnPrimaryBody_noFallback(t *testing.T) {
	counts := &approvalCallCounts{}
	huge := `{"approval_rules_overwritten":false,"rules":[` + strings.Repeat(`{"id":1,"approved":true,"name":"x"},`, 200) + `{"id":2,"approved":false}]}`
	d, _ := newApprovalDepsHTTP(t, approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
		if strings.Contains(path, "/approval_state") {
			_, _ = io.WriteString(w, huge)
			return true
		}
		return false
	}))
	// Enough budget for canonical owner + MR metadata; exhaust on primary approval body.
	b := &igl.Budget{MaxItems: 100, MaxBytes: 2048, MaxRequests: 16, MaxElapsed: time.Minute}
	ctx := igl.WithBudget(context.Background(), b)
	defer b.Cancel()
	_, err := callApproval(t, d, ctx)
	if err == nil {
		t.Fatal("want byte budget error")
	}
	if !strings.Contains(err.Error(), readmeta.CodeBudgetBytes) && !strings.Contains(err.Error(), readmeta.CodeHTTPError) {
		t.Fatalf("err=%v", err)
	}
	if counts.approvals.Load() != 0 {
		t.Fatal("no fallback")
	}
	if counts.approvalState.Load() != 1 {
		t.Fatalf("primary=%d", counts.approvalState.Load())
	}
}

func TestMRApproval_truncated405Body_noFallback(t *testing.T) {
	// R1: Content-Length lies → UnexpectedEOF on error-body read → P/F=1/0.
	for _, active := range []bool{false, true} {
		name := "allowall"
		if active {
			name = "active"
		}
		t.Run(name, func(t *testing.T) {
			counts := &approvalCallCounts{}
			body := &approvalProbeBody{}
			d := newApprovalProbeDepsTLS(t, counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
				if strings.HasSuffix(path, "/approval_state") {
					w.Header().Set("Allow", "POST, OPTIONS")
					w.Header().Set("Content-Length", "128")
					w.WriteHeader(http.StatusMethodNotAllowed)
					_, _ = io.WriteString(w, `{}`)
					return true
				}
				return false
			}, body, false)
			if active {
				d.Config.AllowedProjectIDs = []string{"42"}
			}
			out, err := callApproval(t, d, context.Background())
			if counts.mrGet.Load() != 1 || counts.approvalState.Load() != 1 || !body.unexpected.Load() || !body.closed.Load() {
				t.Fatalf("fixture phase/closure: MR=%d P=%d unexpectedEOF=%v closed=%v", counts.mrGet.Load(), counts.approvalState.Load(), body.unexpected.Load(), body.closed.Load())
			}
			if err == nil || counts.approvals.Load() != 0 {
				t.Fatalf("truncated 405 must not fallback: P/F=%d/%d err=%v endpoint=%v", counts.approvalState.Load(), counts.approvals.Load(), err, out["endpoint"])
			}
			if strings.Contains(err.Error(), "unexpected EOF") || strings.Contains(err.Error(), "partial") {
				t.Fatalf("must not leak raw read/body detail: %v", err)
			}
		})
	}
}

func TestMRApproval_empty405Body_stillFallback(t *testing.T) {
	// Legitimate empty 405 body (clean EOF) remains trusted; distinguishes from failed reads.
	counts := &approvalCallCounts{}
	d, _ := newApprovalDepsTLS(t, approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
		if strings.HasSuffix(path, "/approval_state") {
			w.Header().Set("Allow", "POST, OPTIONS")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return true
		}
		if strings.HasSuffix(path, "/approvals") {
			_, _ = io.WriteString(w, `{"approved":true,"user_has_approved":false,"user_can_approve":true,"approved_by":[],"approvers":[],"approver_groups":[]}`)
			return true
		}
		return false
	}))
	out, err := callApproval(t, d, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if counts.approvalState.Load() != 1 || counts.approvals.Load() != 1 {
		t.Fatalf("want P/F=1/1 got %d/%d", counts.approvalState.Load(), counts.approvals.Load())
	}
	if out["endpoint"] != endpointApprovals {
		t.Fatalf("endpoint=%v", out["endpoint"])
	}
}

func TestMRApproval_customReadErrorOn405_noFallback(t *testing.T) {
	counts := &approvalCallCounts{}
	base := approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
		if strings.HasSuffix(path, "/approval_state") {
			w.Header().Set("Allow", "POST, OPTIONS")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = io.WriteString(w, `{}`)
			return true
		}
		return false
	})
	ts := httptest.NewTLSServer(base)
	t.Cleanup(ts.Close)
	httpClient := ts.Client()
	baseRT := httpClient.Transport
	httpClient.Transport = igl.BudgetInterceptor()(approvalRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := baseRT.RoundTrip(req)
		if err != nil || resp == nil || resp.Body == nil {
			return resp, err
		}
		if strings.HasSuffix(req.URL.Path, "/approval_state") {
			resp.Body = &errOnReadBody{ReadCloser: resp.Body, err: errors.New("injected-read-failure")}
		}
		return resp, nil
	}))
	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithHTTPClient(httpClient),
	)
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Config: &config.Config{Token: "t"}, Client: cli}
	_, err = callApproval(t, d, context.Background())
	if err == nil {
		t.Fatal("custom body read error must stop before fallback")
	}
	if counts.approvalState.Load() != 1 || counts.approvals.Load() != 0 {
		t.Fatalf("want P/F=1/0 got %d/%d", counts.approvalState.Load(), counts.approvals.Load())
	}
	if strings.Contains(err.Error(), "injected-read-failure") {
		t.Fatalf("must not leak raw error: %v", err)
	}
}

type errOnReadBody struct {
	io.ReadCloser
	err error
}

func (b *errOnReadBody) Read(p []byte) (int, error) {
	return 0, b.err
}

type approvalProbeBody struct {
	io.ReadCloser
	reads      atomic.Int64
	closed     atomic.Bool
	unexpected atomic.Bool
	onRead     func()
}

func (b *approvalProbeBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	if b.onRead != nil {
		b.onRead()
	}
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		b.unexpected.Store(true)
	}
	return n, err
}

func (b *approvalProbeBody) Close() error {
	b.closed.Store(true)
	return b.ReadCloser.Close()
}

func newApprovalProbeDepsTLS(t *testing.T, counts *approvalCallCounts, route func(http.ResponseWriter, *http.Request, string) bool, body *approvalProbeBody, wrapFallback bool) Deps {
	t.Helper()
	base := approvalFixtureHandler(counts, route)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/approval-hop" {
			http.Redirect(w, r, "/api/v4/projects/42/merge_requests/1/approval_state", http.StatusFound)
			return
		}
		base.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	httpClient := ts.Client()
	next := httpClient.Transport
	httpClient.Transport = igl.BudgetInterceptor()(approvalRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := next.RoundTrip(req)
		if err != nil || resp == nil || resp.Body == nil || body == nil {
			return resp, err
		}
		wrapPrimary := !wrapFallback && strings.HasSuffix(req.URL.Path, "/approval_state")
		wrapFB := wrapFallback && strings.HasSuffix(req.URL.Path, "/approvals") && !strings.HasSuffix(req.URL.Path, "/approval_state")
		if wrapPrimary || wrapFB {
			body.ReadCloser = resp.Body
			resp.Body = body
		}
		return resp, nil
	}))
	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithHTTPClient(httpClient),
	)
	if err != nil {
		t.Fatal(err)
	}
	return Deps{Config: &config.Config{Token: "t"}, Client: cli}
}

func TestMRApproval_post405_budgetPhaseAndClosure(t *testing.T) {
	// Reviewer fixtures: MaxBytes=1024 / MaxRequests=3 / elapsed after primary → P/F=1/0 + closed.
	for _, kind := range []string{"bytes", "requests", "elapsed"} {
		t.Run(kind, func(t *testing.T) {
			counts := &approvalCallCounts{}
			body := &approvalProbeBody{}
			d := newApprovalProbeDepsTLS(t, counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
				if strings.HasSuffix(path, "/approval_state") {
					w.Header().Set("Allow", "POST, OPTIONS")
					w.WriteHeader(http.StatusMethodNotAllowed)
					if kind == "elapsed" {
						if f, ok := w.(http.Flusher); ok {
							f.Flush()
						}
						<-r.Context().Done()
						return true
					}
					_, _ = io.WriteString(w, strings.Repeat("x", 4096))
					return true
				}
				return false
			}, body, false)
			b := &igl.Budget{MaxItems: 100, MaxBytes: 1 << 20, MaxRequests: 16, MaxElapsed: time.Minute}
			if kind == "bytes" {
				b.MaxBytes = 1024
			}
			if kind == "requests" {
				b.MaxRequests = 3
			}
			if kind == "elapsed" {
				b.MaxElapsed = 500 * time.Millisecond
			}
			ctx := igl.WithBudget(context.Background(), b)
			defer b.Cancel()
			_, err := callApproval(t, d, ctx)
			if err == nil || counts.approvalState.Load() != 1 || counts.approvals.Load() != 0 || !body.closed.Load() {
				t.Fatalf("phase/closure: P/F=%d/%d closed=%v reads=%d err=%v", counts.approvalState.Load(), counts.approvals.Load(), body.closed.Load(), body.reads.Load(), err)
			}
		})
	}
}

func TestMRApproval_elapsedBudget_noNextDispatch(t *testing.T) {
	counts := &approvalCallCounts{}
	d, _ := newApprovalDepsHTTP(t, approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
		if strings.Contains(path, "/approval_state") {
			time.Sleep(40 * time.Millisecond)
			_, _ = io.WriteString(w, `{"rules":[]}`)
			return true
		}
		return false
	}))
	b := &igl.Budget{MaxItems: 100, MaxBytes: 1 << 20, MaxRequests: 16, MaxElapsed: 5 * time.Millisecond}
	ctx := igl.WithBudget(context.Background(), b)
	defer b.Cancel()
	_, err := callApproval(t, d, ctx)
	if err == nil {
		t.Fatal("want elapsed budget error")
	}
	if counts.approvals.Load() != 0 {
		t.Fatal("no fallback")
	}
}

func TestMRApproval_cancelBetweenEndpoints_noFallback(t *testing.T) {
	counts := &approvalCallCounts{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, _ := newApprovalDepsTLS(t, approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
		if strings.Contains(path, "/approval_state") {
			w.Header().Set("Allow", "POST, OPTIONS")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = io.WriteString(w, `{}`)
			cancel()
			return true
		}
		return false
	}))
	b := igl.DefaultBudget()
	ctx = igl.WithBudget(ctx, b)
	defer b.Cancel()
	_, err := callApproval(t, d, ctx)
	if err == nil {
		t.Fatal("want cancel/budget error")
	}
	if counts.approvals.Load() != 0 {
		t.Fatalf("fallback after cancel: %d", counts.approvals.Load())
	}
}

func TestMRApproval_cancelDuringPrimaryBody_closedReader(t *testing.T) {
	// R2: cancel on first body Read; completed short body must not publish success.
	counts := &approvalCallCounts{}
	body := &approvalProbeBody{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body.onRead = cancel
	d := newApprovalProbeDepsTLS(t, counts, nil, body, false)
	b := igl.DefaultBudget()
	ctx = igl.WithBudget(ctx, b)
	defer b.Cancel()
	out, err := callApproval(t, d, ctx)
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("did not cancel inside body read")
	}
	if err == nil || counts.approvalState.Load() != 1 || counts.approvals.Load() != 0 || body.reads.Load() == 0 || !body.closed.Load() {
		t.Fatalf("reader phase/closure: P/F=%d/%d reads=%d closed=%v err=%v endpoint=%v", counts.approvalState.Load(), counts.approvals.Load(), body.reads.Load(), body.closed.Load(), err, out["endpoint"])
	}
}

func TestMRApproval_cancelDuringFallbackBody_closedReader(t *testing.T) {
	counts := &approvalCallCounts{}
	body := &approvalProbeBody{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body.onRead = cancel
	d := newApprovalProbeDepsTLS(t, counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
		if strings.HasSuffix(path, "/approval_state") {
			w.Header().Set("Allow", "POST, OPTIONS")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = io.WriteString(w, `{}`)
			return true
		}
		if strings.HasSuffix(path, "/approvals") {
			_, _ = io.WriteString(w, `{"approved":true,"user_has_approved":false,"user_can_approve":true,"approved_by":[],"approvers":[],"approver_groups":[]}`)
			return true
		}
		return false
	}, body, true)
	b := igl.DefaultBudget()
	ctx = igl.WithBudget(ctx, b)
	defer b.Cancel()
	out, err := callApproval(t, d, ctx)
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("did not cancel inside fallback body read")
	}
	if err == nil || counts.approvalState.Load() != 1 || counts.approvals.Load() != 1 || body.reads.Load() == 0 || !body.closed.Load() {
		t.Fatalf("fallback reader: P/F=%d/%d reads=%d closed=%v err=%v endpoint=%v", counts.approvalState.Load(), counts.approvals.Load(), body.reads.Load(), body.closed.Load(), err, out["endpoint"])
	}
}

func TestMRApproval_maxRequestsEqualsCompletedRead_stillOK(t *testing.T) {
	// Next-dispatch gating ≠ completed-read validation: finishing exactly at MaxRequests is OK.
	counts := &approvalCallCounts{}
	d, _ := newApprovalDepsHTTP(t, approvalFixtureHandler(counts, nil))
	b := &igl.Budget{MaxItems: 100, MaxBytes: 1 << 20, MaxRequests: 3, MaxElapsed: time.Minute}
	ctx := igl.WithBudget(context.Background(), b)
	defer b.Cancel()
	out, err := callApproval(t, d, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out["endpoint"] != endpointApprovalState {
		t.Fatalf("endpoint=%v", out["endpoint"])
	}
	rq, _, _ := b.Stats()
	if rq != 3 {
		t.Fatalf("requests=%d want 3", rq)
	}
}

func TestMRApproval_fallbackTransportError_stop(t *testing.T) {
	counts := &approvalCallCounts{}
	var fallbackDispatch atomic.Int64
	base := approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
		if strings.Contains(path, "/approval_state") {
			w.Header().Set("Allow", "POST, OPTIONS")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = io.WriteString(w, `{}`)
			return true
		}
		return false
	})
	ts := httptest.NewTLSServer(base)
	t.Cleanup(ts.Close)
	httpClient := ts.Client()
	next := httpClient.Transport
	httpClient.Transport = igl.BudgetInterceptor()(approvalRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "/approvals") && !strings.Contains(req.URL.Path, "/approval_state") {
			fallbackDispatch.Add(1)
			return nil, io.ErrUnexpectedEOF
		}
		return next.RoundTrip(req)
	}))
	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithHTTPClient(httpClient),
	)
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Config: &config.Config{Token: "t"}, Client: cli}
	_, err = callApproval(t, d, context.Background())
	if err == nil {
		t.Fatal("want transport failure")
	}
	if counts.approvalState.Load() != 1 {
		t.Fatalf("primary=%d", counts.approvalState.Load())
	}
	if fallbackDispatch.Load() != 1 {
		t.Fatalf("fallback dispatches=%d want 1 (no retry)", fallbackDispatch.Load())
	}
	msg := err.Error()
	for _, bad := range []string{"://", "api/v4"} {
		if strings.Contains(msg, bad) {
			t.Fatalf("leaked transport detail %q in %q", bad, msg)
		}
	}
}

type approvalRoundTripFunc func(*http.Request) (*http.Response, error)

func (f approvalRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMRApproval_fallbackMalformed_stop(t *testing.T) {
	counts := &approvalCallCounts{}
	d, _ := newApprovalDepsTLS(t, approvalFixtureHandler(counts, func(w http.ResponseWriter, r *http.Request, path string) bool {
		if strings.Contains(path, "/approval_state") {
			w.Header().Set("Allow", "POST")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return true
		}
		if strings.Contains(path, "/approvals") {
			_, _ = io.WriteString(w, `null`)
			return true
		}
		return false
	}))
	_, err := callApproval(t, d, context.Background())
	if err == nil {
		t.Fatal("want error")
	}
	if counts.approvalState.Load() != 1 || counts.approvals.Load() != 1 {
		t.Fatalf("P/F=%d/%d", counts.approvalState.Load(), counts.approvals.Load())
	}
}

func TestAllowExcludesGET(t *testing.T) {
	h := http.Header{}
	h.Add("Allow", "POST")
	h.Add("Allow", "OPTIONS")
	if !allowExcludesGET(h) {
		t.Fatal("expected true")
	}
	h2 := http.Header{}
	h2.Set("Allow", "POST, GET")
	if allowExcludesGET(h2) {
		t.Fatal("GET must reject")
	}
}

func TestApprovalDigest_presenceRulesAndFailure(t *testing.T) {
	body := []byte(`{
		"approved": true,
		"user_has_approved": null,
		"approvals_required": 2,
		"approvals_left": null,
		"rules": [
			{"id": 2, "name": "b", "approved": false, "approvals_required": 1, "contains_hidden_groups": false},
			{"id": 2, "name": null, "approved": true, "approvals_required": null},
			{"id": 1, "name": "a", "approved": null}
		],
		"approval_rules_left": []
	}`)
	limits := []readmeta.Limitation{
		{Code: "z", Message: "later"},
		{Code: "a", Message: "second"},
		{Code: "a", Message: "first"},
	}
	first, err := approvalSemanticDigest(endpointApprovalState, body, limits)
	if err != nil || len(first) != 64 {
		t.Fatalf("digest: %v %q", err, first)
	}
	second, err := approvalSemanticDigest(endpointApprovalState, body, []readmeta.Limitation{
		{Code: "a", Message: "first"},
		{Code: "a", Message: "second"},
		{Code: "z", Message: "later"},
	})
	if err != nil || first != second {
		t.Fatalf("order changed digest: %v %s %s", err, first, second)
	}
	absent, err := approvalSemanticDigest(endpointApprovalState, []byte(`{"rules":null}`), nil)
	if err != nil || absent == first {
		t.Fatalf("null rules: %v %s", err, absent)
	}
	if _, err := approvalSemanticDigest(endpointApprovalState, []byte(`[]`), nil); err == nil {
		t.Fatal("array body accepted")
	}
	if _, err := approvalSemanticDigest(endpointApprovalState, []byte(`{"rules":{"id":1}}`), nil); err == nil {
		t.Fatal("object rules accepted")
	}
	if _, err := approvalSemanticDigest(endpointApprovalState, []byte(`{"rules":[1]}`), nil); err == nil {
		t.Fatal("scalar rule accepted")
	}
	if _, err := approvalSemanticDigest(endpointApprovalState, []byte(`{"rules":[{"name":1}]}`), nil); err == nil {
		t.Fatal("numeric name accepted")
	}
	fail1, err := approvalFailureDigest(endpointApprovalState, readmeta.CodeHTTPError, limits)
	if err != nil || len(fail1) != 64 || fail1 == first {
		t.Fatalf("failure digest: %v %s", err, fail1)
	}
	fail2, err := approvalFailureDigest(endpointApprovalState, readmeta.CodeHTTPError, limits)
	if err != nil || fail1 != fail2 {
		t.Fatalf("failure digest unstable: %v", err)
	}
}
