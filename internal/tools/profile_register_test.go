package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// literalExpectedReview27 is the independent AC oracle for review profiles
// (not derived from production ReviewReadTools()).
var literalExpectedReview27 = []string{
	"get_project",
	"get_merge_request",
	"list_merge_requests",
	"get_merge_request_approval_state",
	"get_merge_request_conflicts",
	"get_merge_request_diffs",
	"get_merge_request_file_diff",
	"list_merge_request_changed_files",
	"list_merge_request_versions",
	"get_merge_request_version",
	"list_merge_request_diffs",
	"mr_discussions",
	"get_merge_request_notes",
	"get_merge_request_discussion",
	"get_file_contents",
	"batch_get_file_contents",
	"get_repository_tree",
	"list_commits",
	"get_commit",
	"get_commit_diff",
	"list_pipelines",
	"get_pipeline",
	"list_pipeline_jobs",
	"list_pipeline_trigger_jobs",
	"get_pipeline_job",
	"get_pipeline_job_output",
	"get_merge_request_review_queue",
}

var reviewForbiddenTools = []string{
	"execute_graphql",
	"search_code",
	"search_repositories",
	"merge_merge_request",
	"approve_merge_request",
	"create_merge_request_note",
	"create_merge_request_discussion_note",
	"resolve_merge_request_thread",
	"create_pipeline",
	"download_job_artifacts",
	"list_deployments",
	"create_merge_request",
	"upload_markdown",
}

func assertExactToolSet(t *testing.T, names map[string]bool, want []string) {
	t.Helper()
	if len(names) != len(want) {
		got := keys(names)
		sort.Strings(got)
		t.Fatalf("count=%d want %d; got=%v", len(names), len(want), got)
	}
	for _, n := range want {
		if !names[n] {
			t.Fatalf("missing tool %q", n)
		}
	}
}

func TestRegisterAll_toolProfiles(t *testing.T) {
	t.Run("daily_profile_exact_41", func(t *testing.T) {
		names := registerNames(t, &config.Config{Token: "x", ToolProfile: "daily", Issues: true, EnabledTools: []string{"list_issues"}})
		assertExactToolSet(t, names, DailyTools())
	})
	for _, profile := range []string{"review_read", "review_write"} {
		profile := profile
		t.Run(profile+"_exact_27", func(t *testing.T) {
			names := registerNames(t, &config.Config{
				Token:         "x",
				ToolProfile:   profile,
				Pipeline:      true,
				EnabledTools:  []string{"execute_graphql", "list_issues"},
				UseDailyTools: true,
			})
			assertExactToolSet(t, names, literalExpectedReview27)
			if len(ReviewReadTools()) != 27 {
				t.Fatal("production ReviewReadTools must stay len 27")
			}
			for _, bad := range reviewForbiddenTools {
				if names[bad] {
					t.Fatalf("%s must not register forbidden %q", profile, bad)
				}
			}
		})
	}
	t.Run("review_read_disable_narrows", func(t *testing.T) {
		names := registerNames(t, &config.Config{
			Token:         "x",
			ToolProfile:   "review_read",
			DisabledTools: []string{"list_pipelines"},
		})
		if names["list_pipelines"] {
			t.Fatal("disabled list_pipelines must be absent")
		}
		if len(names) != 26 {
			t.Fatalf("got %d want 26", len(names))
		}
	})
}

func TestRegisterAll_annotationProbes(t *testing.T) {
	// Legacy unrestricted: new-family booleans false (Issues must stay false so
	// RestrictedMode is off). Opt into legacy Pipeline/Wiki gates as needed.
	cfg := &config.Config{Token: "x", Wiki: true, Pipeline: true}
	if cfg.RestrictedMode() {
		t.Fatal("annotation probe cfg must remain unrestricted")
	}
	cli, _ := testutil.NewGitLabClient(t, stubGitLabAPI())
	srv := mcp.NewServer(&mcp.Implementation{Name: "ann", Version: "t"}, nil)
	RegisterAll(srv, Deps{Config: cfg, Client: cli})
	cs := testutil.MCPConnect(t, srv)
	listed, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range listed.Tools {
		if tool != nil {
			byName[tool.Name] = tool
		}
	}

	assertAnn := func(name string, wantRO bool, wantDest *bool) {
		t.Helper()
		tool := byName[name]
		if tool == nil {
			t.Fatalf("missing %q", name)
		}
		if tool.Annotations == nil {
			t.Fatalf("%s: annotations nil", name)
		}
		if tool.Annotations.ReadOnlyHint != wantRO {
			t.Fatalf("%s: ReadOnlyHint=%v want %v", name, tool.Annotations.ReadOnlyHint, wantRO)
		}
		if tool.Annotations.IdempotentHint {
			t.Fatalf("%s: IdempotentHint must not be true without source proof", name)
		}
		if wantDest == nil {
			if tool.Annotations.DestructiveHint != nil {
				t.Fatalf("%s: DestructiveHint should be omitted for read-only, got %v", name, *tool.Annotations.DestructiveHint)
			}
			return
		}
		if tool.Annotations.DestructiveHint == nil {
			t.Fatalf("%s: DestructiveHint nil (SDK default true) — want explicit %v", name, *wantDest)
		}
		if *tool.Annotations.DestructiveHint != *wantDest {
			t.Fatalf("%s: DestructiveHint=%v want %v", name, *tool.Annotations.DestructiveHint, *wantDest)
		}
	}

	f, tr := false, true
	assertAnn("get_project", true, nil)
	assertAnn("create_issue", false, &f)           // additive create
	assertAnn("delete_issue", false, &tr)          // destructive
	assertAnn("create_or_update_file", false, &tr) // overwrite path
	assertAnn("push_files", false, &tr)            // may delete/update
	assertAnn("create_pipeline", false, &tr)       // arbitrary pipeline jobs
	assertAnn("play_pipeline_job", false, &tr)     // arbitrary job play
	assertAnn("publish_draft_note", false, &tr)    // consumes draft state
	assertAnn("bulk_publish_draft_notes", false, &tr)
	assertAnn("create_merge_request_note", false, &tr) // quick-action body
	assertAnn("create_note", false, &tr)               // quick-action body
	assertAnn("create_merge_request_thread", false, &tr)
	assertAnn("create_merge_request_discussion_note", false, &tr)
	assertAnn("create_issue_note", false, &tr)
	assertAnn("create_work_item_note", false, &tr)
	assertAnn("create_draft_note", false, &tr)
	assertAnn("execute_graphql", false, &tr)
	assertAnn("upload_markdown", false, &tr)
	assertAnn("download_job_artifacts", false, &tr) // os.Create truncates
	assertAnn("download_release_asset", false, &tr)

	static := &mcp.Tool{Name: "get_project", Description: "x"}
	_ = applyToolAnnotations(static, false)
	if static.Annotations != nil {
		t.Fatal("applyToolAnnotations must not mutate caller descriptor")
	}
}

// reviewAuthzPath documents the 018 authorization entry for each review tool.
// Map keys are the independent literal 27-tool oracle.
var reviewAuthzPath = map[string]string{
	"get_project":                      "resolveProjectAuthz",
	"get_merge_request":                "pidMR.resolve",
	"list_merge_requests":              "resolveProjectAuthz|AuthorizeCanonicalGroup|filterMergeRequestsByPolicy",
	"get_merge_request_approval_state": "authorizeAndVerifyApprovalMR",
	"get_merge_request_conflicts":      "authorizeMROwnerAndForks",
	"get_merge_request_diffs":          "authorizeMROwnerAndForks",
	"get_merge_request_file_diff":      "authorizeMROwnerAndForks",
	"list_merge_request_changed_files": "authorizeMROwnerAndForks",
	"list_merge_request_versions":      "authorizeMROwnerAndForks",
	"get_merge_request_version":        "authorizeMROwnerAndForks",
	"list_merge_request_diffs":         "AuthorizeCanonicalProject+020envelope",
	"mr_discussions":                   "pidMR.resolve",
	"get_merge_request_notes":          "pidMR.resolve",
	"get_merge_request_discussion":     "pidMR.resolve",
	"get_file_contents":                "resolveProjectAuthz",
	"batch_get_file_contents":          "AuthorizeCanonicalProject",
	"get_repository_tree":              "resolveProjectAuthz",
	"list_commits":                     "resolveProjectAuthz",
	"get_commit":                       "resolveProjectAuthz",
	"get_commit_diff":                  "resolveProjectAuthz",
	"list_pipelines":                   "resolvePipelineProject",
	"get_pipeline":                     "resolvePipelineProject",
	"list_pipeline_jobs":               "resolvePipelineProject",
	"list_pipeline_trigger_jobs":       "resolvePipelineProject",
	"get_pipeline_job":                 "resolvePipelineProject",
	"get_pipeline_job_output":          "resolvePipelineProject",
	"get_merge_request_review_queue":   "AuthorizeCanonicalGroup|authorizeGroupProject|AuthorizeAdditionalProjects",
}

func TestReviewProfile_authzPathTableExact27(t *testing.T) {
	if len(literalExpectedReview27) != 27 || len(reviewAuthzPath) != 27 {
		t.Fatalf("literal want 27; list=%d map=%d", len(literalExpectedReview27), len(reviewAuthzPath))
	}
	for _, name := range literalExpectedReview27 {
		if reviewAuthzPath[name] == "" {
			t.Fatalf("missing 018 authz path for literal %q", name)
		}
	}
	for name := range reviewAuthzPath {
		found := false
		for _, n := range literalExpectedReview27 {
			if n == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("authz map has extra key %q not in literal expected27", name)
		}
	}
	// Production helper must match the independent literal set.
	prod := ReviewReadTools()
	assertExactToolSet(t, func() map[string]bool {
		m := map[string]bool{}
		for _, n := range prod {
			m[n] = true
		}
		return m
	}(), literalExpectedReview27)
}

func TestReviewProfiles_handlerPolicyProbes(t *testing.T) {
	for _, profile := range []string{"review_read", "review_write"} {
		profile := profile
		t.Run(profile, func(t *testing.T) {
			var content, mut int32
			d := authzDeps(t, matrixFixture(t, &content, &mut, nil))
			d.Config.ToolProfile = profile
			d.Config.AllowedProjectIDs = []string{"42"}

			srv := mcp.NewServer(&mcp.Implementation{Name: "prof", Version: "t"}, nil)
			RegisterAll(srv, d)
			cs := testutil.MCPConnect(t, srv)

			names := testutil.ToolNames(t, cs)
			assertExactToolSet(t, names, literalExpectedReview27)

			call := func(name string, args map[string]any) *mcp.CallToolResult {
				t.Helper()
				res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
				if err != nil {
					t.Fatal(err)
				}
				return res
			}

			res := call("get_project", map[string]any{"project_id": "42"})
			if res == nil || res.IsError {
				t.Fatalf("allow get_project: %#v", res)
			}

			atomic.StoreInt32(&content, 0)
			res = call("get_project", map[string]any{"project_id": "99"})
			if res == nil || !res.IsError {
				t.Fatal("expected get_project deny")
			}
			if !strings.Contains(toolErrorText(t, res), readmeta.CodeAuthzDenied) {
				t.Fatalf("want authz_denied, got %q", toolErrorText(t, res))
			}
			if atomic.LoadInt32(&content) != 0 {
				t.Fatalf("content hits=%d want 0", content)
			}

			atomic.StoreInt32(&content, 0)
			res = call("get_merge_request", map[string]any{"project_id": "99", "merge_request_iid": 1})
			if res == nil || !res.IsError || !strings.Contains(toolErrorText(t, res), readmeta.CodeAuthzDenied) {
				t.Fatalf("mr deny: %#v %q", res, toolErrorText(t, res))
			}
			if atomic.LoadInt32(&content) != 0 {
				t.Fatalf("mr content hits=%d", content)
			}

			atomic.StoreInt32(&content, 0)
			res = call("get_merge_request_notes", map[string]any{
				"project_id": "99", "merge_request_iid": 1, "page": 1, "per_page": 20,
			})
			if res == nil || !res.IsError || !strings.Contains(toolErrorText(t, res), readmeta.CodeAuthzDenied) {
				t.Fatalf("notes deny: %q", toolErrorText(t, res))
			}
			if atomic.LoadInt32(&content) != 0 {
				t.Fatalf("notes content hits=%d", content)
			}

			atomic.StoreInt32(&content, 0)
			res = call("list_pipelines", map[string]any{"project_id": "99", "page": 1, "per_page": 20})
			if res == nil || !res.IsError || !strings.Contains(toolErrorText(t, res), readmeta.CodeAuthzDenied) {
				t.Fatalf("pipeline deny: %q", toolErrorText(t, res))
			}
			if atomic.LoadInt32(&content) != 0 {
				t.Fatalf("pipeline content hits=%d", content)
			}

			atomic.StoreInt32(&content, 0)
			res = call("list_merge_request_diffs", map[string]any{
				"project_id": "99", "merge_request_iid": 1, "page": 1, "per_page": 20,
			})
			if res == nil || !res.IsError || !strings.Contains(toolErrorText(t, res), readmeta.CodeAuthzDenied) {
				t.Fatalf("diffs deny: %q", toolErrorText(t, res))
			}
			if atomic.LoadInt32(&content) != 0 {
				t.Fatalf("diffs content hits=%d", content)
			}
		})
	}
}

func TestReviewProfiles_listMergeRequestDiffsCallToolEnvelope(t *testing.T) {
	// Explicit item budget: DefaultMaxItems (100); fixture returns 101 diffs.
	items := make([]string, 0, igl.DefaultMaxItems+1)
	for i := 0; i < igl.DefaultMaxItems+1; i++ {
		items = append(items, fmt.Sprintf(
			`{"old_path":"f%d.go","new_path":"f%d.go","diff":"+x\n","collapsed":false,"too_large":false}`, i, i))
	}
	body := "[" + strings.Join(items, ",") + "]"

	for _, profile := range []string{"review_read", "review_write"} {
		profile := profile
		t.Run(profile+"_budget_items", func(t *testing.T) {
			var diffsHits int32
			base := mrHandler(42, 42, body, "", true)
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/diffs") {
					atomic.AddInt32(&diffsHits, 1)
				}
				base.ServeHTTP(w, r)
			})
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
			d := Deps{Config: &config.Config{
				Token:             "t",
				ToolProfile:       profile,
				AllowedProjectIDs: []string{"42"},
			}, Client: cli}

			res, err := callListMergeRequestDiffs(t, d, callArgs("42", 1))
			if err != nil {
				t.Fatal(err)
			}
			if res.IsError {
				t.Fatalf("budget path should envelope, not IsError: %q", toolErrorText(t, res))
			}
			m := assertEnvelopeStructured(t, res)
			sec, _ := m["section"].(map[string]any)
			if sec == nil {
				t.Fatal("missing section")
			}
			if cc, _ := sec["content_complete"].(string); cc == readmeta.ContentCompleteTrue {
				t.Fatalf("budget overflow must not claim complete, got %q", cc)
			}
			lims, _ := sec["limitations"].([]any)
			found := false
			for _, raw := range lims {
				lim, _ := raw.(map[string]any)
				if lim != nil && lim["code"] == readmeta.CodeBudgetItems {
					found = true
				}
			}
			if !found {
				t.Fatalf("want %s limitation under %s; section=%v", readmeta.CodeBudgetItems, profile, sec)
			}
			diffs, _ := m["diffs"].([]any)
			if len(diffs) != igl.DefaultMaxItems {
				t.Fatalf("retained diffs=%d want %d", len(diffs), igl.DefaultMaxItems)
			}
			if atomic.LoadInt32(&diffsHits) < 1 {
				t.Fatalf("diffs content hit count=%d want >=1", diffsHits)
			}
		})

		t.Run(profile+"_http_redaction", func(t *testing.T) {
			const secret = "SECRET_BACKEND_TOKEN_do_not_leak"
			var diffsHits int32
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.Contains(r.URL.Path, "/diffs"):
					atomic.AddInt32(&diffsHits, 1)
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
			d := Deps{Config: &config.Config{
				Token:             "t",
				ToolProfile:       profile,
				AllowedProjectIDs: []string{"42"},
			}, Client: cli}
			res, err := callListMergeRequestDiffs(t, d, callArgs("42", 1))
			if err != nil {
				t.Fatal(err)
			}
			if res == nil || !res.IsError {
				t.Fatal("expected CallTool IsError on fatal HTTP")
			}
			txt := toolErrorText(t, res)
			if !strings.Contains(txt, readmeta.CodeHTTPError) {
				t.Fatalf("want http_error, got %q", txt)
			}
			if strings.Contains(txt, secret) {
				t.Fatalf("raw backend body leaked: %q", txt)
			}
			if atomic.LoadInt32(&diffsHits) < 1 {
				t.Fatalf("diffs hit before failure=%d", diffsHits)
			}
		})
	}
}

// TestAnnotation_noteBodiesDestructiveWithQuickActionCapture proves tools/list
// advertises destructiveHint=true for command-capable free-form note/discussion
// tools, and CallTool forwards body/note "/close" unchanged to the synthetic
// GitLab API (no live quick-action execution). Exact JSON field equality —
// not substring Contains — proves the quick-action text was not altered.
func TestAnnotation_noteBodiesDestructiveWithQuickActionCapture(t *testing.T) {
	const quick = "/close"
	var captured []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Body != nil && (r.Method == http.MethodPost || r.Method == http.MethodPut) {
			b, _ := io.ReadAll(r.Body)
			captured = append(captured, string(b))
			_ = r.Body.Close()
		}
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/graphql"):
			_, _ = io.WriteString(w, `{"data":{"workItemNoteCreate":{"note":{"id":"gid://1","body":"ok"},"errors":[]}}}`)
		case strings.Contains(path, "/draft_notes"):
			_, _ = io.WriteString(w, `{"id":1,"note":"ok"}`)
		case strings.Contains(path, "/notes"):
			// Top-level notes and discussion-note replies both return Note (id int64).
			_, _ = io.WriteString(w, `{"id":1,"body":"ok"}`)
		case strings.Contains(path, "/discussions"):
			_, _ = io.WriteString(w, `{"id":"d1","notes":[{"id":1,"body":"ok"}]}`)
		case strings.Contains(path, "/merge_requests/"):
			_, _ = io.WriteString(w, mrJSON(42, 42, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
		case strings.Contains(path, "/projects/"):
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p"}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	})
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
	)
	if err != nil {
		t.Fatal(err)
	}
	// Legacy unrestricted (new-family booleans false) so note tools register.
	cfg := &config.Config{Token: "t", Wiki: true, Pipeline: true}
	if cfg.RestrictedMode() {
		t.Fatal("must stay unrestricted")
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "note-ann", Version: "t"}, nil)
	RegisterAll(srv, Deps{Config: cfg, Client: cli})
	cs := testutil.MCPConnect(t, srv)

	listed, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range listed.Tools {
		if tool != nil {
			byName[tool.Name] = tool
		}
	}

	commandCapable := []string{
		"create_merge_request_note",
		"create_note",
		"create_merge_request_thread",
		"create_merge_request_discussion_note",
		"create_issue_note",
		"create_work_item_note",
		"create_draft_note",
	}
	for _, name := range commandCapable {
		tool := byName[name]
		if tool == nil {
			t.Fatalf("missing registered tool %q", name)
		}
		if tool.Annotations == nil || tool.Annotations.ReadOnlyHint {
			t.Fatalf("%s: want ReadOnlyHint=false", name)
		}
		if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint != true {
			t.Fatalf("%s: want explicit DestructiveHint=true (command-capable body), got %#v", name, tool.Annotations.DestructiveHint)
		}
		if tool.Annotations.IdempotentHint {
			t.Fatalf("%s: IdempotentHint must stay false", name)
		}
	}
	// Genuinely additive create of a new object remains destructiveHint=false.
	if issue := byName["create_issue"]; issue == nil || issue.Annotations == nil ||
		issue.Annotations.DestructiveHint == nil || *issue.Annotations.DestructiveHint != false {
		t.Fatalf("create_issue must remain additive destructiveHint=false")
	}

	// extractQuickAction returns the exact forwarded free-form text field for
	// each tool's synthetic request JSON (tool-specific paths, not a shared oracle).
	extractQuickAction := func(toolName, raw string) (string, error) {
		var top map[string]any
		if err := json.Unmarshal([]byte(raw), &top); err != nil {
			return "", fmt.Errorf("parse request JSON: %w; raw=%q", err, raw)
		}
		switch toolName {
		case "create_merge_request_note", "create_note", "create_merge_request_thread",
			"create_merge_request_discussion_note", "create_issue_note":
			v, ok := top["body"].(string)
			if !ok {
				return "", fmt.Errorf("missing string field body; raw=%q", raw)
			}
			return v, nil
		case "create_draft_note":
			v, ok := top["note"].(string)
			if !ok {
				return "", fmt.Errorf("missing string field note; raw=%q", raw)
			}
			return v, nil
		case "create_work_item_note":
			vars, _ := top["variables"].(map[string]any)
			if vars == nil {
				return "", fmt.Errorf("missing GraphQL variables; raw=%q", raw)
			}
			input, _ := vars["input"].(map[string]any)
			if input == nil {
				return "", fmt.Errorf("missing GraphQL variables.input; raw=%q", raw)
			}
			// Handler maps CallTool body → GraphQL input.note.
			v, ok := input["note"].(string)
			if !ok {
				return "", fmt.Errorf("missing GraphQL variables.input.note; raw=%q", raw)
			}
			return v, nil
		default:
			return "", fmt.Errorf("unknown tool %q", toolName)
		}
	}

	call := func(name string, args map[string]any) {
		t.Helper()
		before := len(captured)
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s CallTool err: %v", name, err)
		}
		if res == nil {
			t.Fatalf("%s: nil CallToolResult", name)
		}
		if res.IsError {
			t.Fatalf("%s IsError: %q", name, toolErrorText(t, res))
		}
		if len(captured) <= before {
			t.Fatalf("%s: no synthetic HTTP body captured", name)
		}
		last := captured[len(captured)-1]
		got, err := extractQuickAction(name, last)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != quick {
			t.Fatalf("%s: forwarded text %q want exact %q; raw=%q", name, got, quick, last)
		}
	}

	call("create_merge_request_note", map[string]any{
		"project_id": "42", "merge_request_iid": 1, "body": quick,
	})
	call("create_note", map[string]any{
		"project_id": "42", "noteable_type": "merge_request", "noteable_iid": 1, "body": quick,
	})
	call("create_merge_request_thread", map[string]any{
		"project_id": "42", "merge_request_iid": 1, "body": quick,
	})
	call("create_merge_request_discussion_note", map[string]any{
		"project_id": "42", "merge_request_iid": 1, "discussion_id": "d1", "body": quick,
	})
	call("create_issue_note", map[string]any{
		"project_id": "42", "issue_iid": 1, "discussion_id": "d1", "body": quick,
	})
	call("create_draft_note", map[string]any{
		"project_id": "42", "merge_request_iid": 1, "note": quick,
	})
	call("create_work_item_note", map[string]any{
		"id": "gid://gitlab/WorkItem/1", "body": quick,
	})
}
