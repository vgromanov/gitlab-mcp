package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

func callListMergeRequestDiffs(t *testing.T, d Deps, args map[string]any) (*mcp.CallToolResult, error) {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "t"}, nil)
	RegisterMergeRequests(srv, d)
	cs := testutil.MCPConnect(t, srv)
	return cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_merge_request_diffs",
		Arguments: args,
	})
}

func callArgs(projectID string, iid int64) map[string]any {
	// page/per_page are required by the derived input schema (Pagination has no omitempty).
	return map[string]any{
		"project_id":        projectID,
		"merge_request_iid": iid,
		"page":              1,
		"per_page":          20,
	}
}

func assertEnvelopeStructured(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res == nil {
		t.Fatal("nil CallToolResult")
	}
	if res.IsError {
		t.Fatalf("IsError=true Content=%v text=%q", res.Content, toolErrorText(t, res))
	}
	if res.StructuredContent == nil {
		t.Fatal("StructuredContent nil — registered outputSchema roundtrip did not populate structured content")
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal StructuredContent: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal StructuredContent: %v raw=%s", err, raw)
	}
	for _, k := range []string{"diffs", "pagination", "section"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing %q in %s", k, raw)
		}
	}
	sec := m["section"].(map[string]any)
	if _, ok := sec["content_complete"].(string); !ok {
		t.Fatalf("content_complete must be string enum, got %T", sec["content_complete"])
	}
	return m
}

func TestListMergeRequestDiffs_CallToolOutputSchemaRoundtrip(t *testing.T) {
	type want struct {
		isError           bool
		errSubstr         string
		contentComplete   string
		diffsLen          *int
		expectNullSlot    bool
		expectPartialCode bool
		safeNoSecret      string
		headSHANull       bool
		consistency       string
	}
	cases := []struct {
		name    string
		handler http.Handler
		cfg     func(*config.Config)
		args    map[string]any
		want    want
	}{
		{
			name:    "success_complete",
			handler: mrHandler(42, 42, knownPresenceDiff, "", true),
			args:    callArgs("42", 1),
			want: want{
				isError:         false,
				contentComplete: readmeta.ContentCompleteTrue,
				diffsLen:        intPtr(1),
				consistency:     readmeta.ConsistencyConsistent,
			},
		},
		{
			name:    "empty_array",
			handler: mrHandler(42, 42, `[]`, "", true),
			args:    callArgs("42", 1),
			want: want{
				isError:         false,
				contentComplete: readmeta.ContentCompleteTrue,
				diffsLen:        intPtr(0),
			},
		},
		{
			name:    "unknown_paging_partial",
			handler: mrHandler(42, 42, knownPresenceDiff, "", false),
			args:    callArgs("42", 1),
			want: want{
				isError:           false,
				contentComplete:   readmeta.ContentCompleteUnknown,
				diffsLen:          intPtr(1),
				expectPartialCode: true,
			},
		},
		{
			name: "collapsed_partial",
			handler: mrHandler(42, 42,
				`[{"old_path":"a.go","new_path":"a.go","a_mode":"100644","b_mode":"100644","diff":"","new_file":false,"renamed_file":false,"deleted_file":false,"generated_file":false,"collapsed":true,"too_large":false}]`,
				"", true),
			args: callArgs("42", 1),
			want: want{
				isError:         false,
				contentComplete: readmeta.ContentCompleteFalse,
				diffsLen:        intPtr(1),
			},
		},
		{
			name: "nullable_entries",
			handler: mrHandler(42, 42,
				`[null,{"old_path":"a.go","new_path":"a.go","a_mode":"100644","b_mode":"100644","diff":"+x\n","new_file":false,"renamed_file":false,"deleted_file":false,"generated_file":false,"collapsed":false,"too_large":false},null]`,
				"", true),
			args: callArgs("42", 1),
			want: want{
				isError:         false,
				contentComplete: readmeta.ContentCompleteUnknown,
				diffsLen:        intPtr(3),
				expectNullSlot:  true,
			},
		},
		{
			name:    "fatal_authz",
			handler: mrHandler(42, 42, `[]`, "", true),
			cfg:     func(c *config.Config) { c.AllowedProjectIDs = []string{"99"} },
			args:    callArgs("42", 1),
			want: want{
				isError:   true,
				errSubstr: readmeta.CodeAuthzDenied,
			},
		},
		{
			name: "fatal_identity_nonpositive",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(r.URL.Path, "/projects/") {
					_, _ = io.WriteString(w, `{"id":0,"path_with_namespace":"g/p"}`)
					return
				}
				_, _ = io.WriteString(w, `{}`)
			}),
			args: callArgs("0", 1),
			want: want{
				isError:   true,
				errSubstr: readmeta.CodeIdentityUnresolved,
			},
		},
		{
			name: "fatal_http_safe_message",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.Contains(r.URL.Path, "/diffs"):
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = io.WriteString(w, `{"message":"SECRET_BACKEND_TOKEN_do_not_leak"}`)
				case strings.Contains(r.URL.Path, "/merge_requests/"):
					_, _ = io.WriteString(w, mrJSON(42, 42, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
				case strings.Contains(r.URL.Path, "/projects/"):
					_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p"}`)
				default:
					_, _ = io.WriteString(w, `{}`)
				}
			}),
			args: callArgs("42", 1),
			want: want{
				isError:      true,
				errSubstr:    readmeta.CodeHTTPError,
				safeNoSecret: "SECRET_BACKEND_TOKEN_do_not_leak",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newMRDiffsDeps(t, tc.handler)
			if tc.cfg != nil {
				tc.cfg(d.Config)
			}
			res, err := callListMergeRequestDiffs(t, d, tc.args)
			// Output schema validation failure ⇒ CallTool returns err ("validating tool output").
			if err != nil {
				t.Fatalf("CallTool protocol/schema error (output must validate against registered schema): %v", err)
			}
			if tc.want.isError {
				if !res.IsError {
					t.Fatalf("want IsError=true, StructuredContent=%v", res.StructuredContent)
				}
				msg := toolErrorText(t, res)
				if tc.want.errSubstr != "" && !strings.Contains(msg, tc.want.errSubstr) {
					t.Fatalf("error text %q missing %q", msg, tc.want.errSubstr)
				}
				if tc.want.safeNoSecret != "" && strings.Contains(msg, tc.want.safeNoSecret) {
					t.Fatalf("raw backend secret leaked into MCP error: %q", msg)
				}
				return
			}
			m := assertEnvelopeStructured(t, res)
			diffs, ok := m["diffs"].([]any)
			if !ok {
				t.Fatalf("diffs must be JSON array (not null), got %T %#v", m["diffs"], m["diffs"])
			}
			if tc.want.diffsLen != nil && len(diffs) != *tc.want.diffsLen {
				t.Fatalf("diffs len=%d want %d", len(diffs), *tc.want.diffsLen)
			}
			if tc.want.expectNullSlot && (len(diffs) < 1 || diffs[0] != nil) {
				t.Fatalf("expected null slot at [0], got %#v", diffs)
			}
			sec := m["section"].(map[string]any)
			if tc.want.contentComplete != "" {
				if got, _ := sec["content_complete"].(string); got != tc.want.contentComplete {
					t.Fatalf("content_complete=%q want %q", got, tc.want.contentComplete)
				}
			}
			if tc.want.consistency != "" {
				if got, _ := sec["consistency"].(string); got != tc.want.consistency {
					t.Fatalf("consistency=%q want %q", got, tc.want.consistency)
				}
			}
			if tc.want.headSHANull && sec["head_sha"] != nil {
				t.Fatalf("head_sha want null, got %v", sec["head_sha"])
			}
			if tc.want.expectPartialCode {
				lims, _ := sec["limitations"].([]any)
				found := false
				for _, lim := range lims {
					lm, _ := lim.(map[string]any)
					code, _ := lm["code"].(string)
					switch code {
					case readmeta.CodeUnknownCount, readmeta.CodePartial, readmeta.CodeCollapsed, readmeta.CodeTooLarge:
						found = true
					}
				}
				if !found {
					t.Fatalf("expected honesty limitation codes, got %#v", lims)
				}
			}
		})
	}
}

func intPtr(n int) *int { return &n }

func toolErrorText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		} else {
			raw, _ := json.Marshal(c)
			b.Write(raw)
		}
	}
	return b.String()
}

func TestDocsReadEnvelopesExamplesValidateRegisteredOutputSchema(t *testing.T) {
	// Plan step 5: docs examples validate against the intended tool contract.
	// List-diff fences keep registered list_merge_request_diffs OutputSchema checks.
	// Content-mode fences are associated with get_merge_request_diff_window and
	// validated against the actual serialized content contract (no fabricated
	// list-diff fields; no unplanned content OutputSchema registration).
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	}))
	reg := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "t"}, nil)
	RegisterMergeRequests(reg, Deps{Config: &config.Config{Token: "x"}, Client: cli})
	cs := testutil.MCPConnect(t, reg)
	listed, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	var listDiffSchema any
	for _, tool := range listed.Tools {
		if tool != nil && tool.Name == "list_merge_request_diffs" {
			listDiffSchema = tool.OutputSchema
		}
	}
	if listDiffSchema == nil {
		t.Fatal("missing list_merge_request_diffs outputSchema")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	docsPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "docs", "read-envelopes.md")
	body, err := os.ReadFile(docsPath)
	if err != nil {
		t.Fatal(err)
	}
	blocks := extractJSONFencedBlocks(string(body))
	if len(blocks) < 2 {
		t.Fatalf("expected ≥2 fenced JSON examples in docs, got %d", len(blocks))
	}

	listCount, contentCount := 0, 0
	for i, block := range blocks {
		var fixture map[string]any
		if err := json.Unmarshal([]byte(block), &fixture); err != nil {
			t.Fatalf("docs example %d not JSON: %v\n%s", i, err, block)
		}
		toolKind := classifyDocsEnvelopeExample(fixture)
		switch toolKind {
		case "list_merge_request_diffs":
			listCount++
			// Guard against abbreviated diffs missing SDK fields in advertised examples.
			diffs, _ := fixture["diffs"].([]any)
			if len(diffs) > 0 && diffs[0] != nil {
				item, _ := diffs[0].(map[string]any)
				for _, k := range []string{"old_path", "new_path", "a_mode", "b_mode", "diff", "new_file", "renamed_file", "deleted_file", "generated_file", "collapsed", "too_large"} {
					if _, ok := item[k]; !ok {
						t.Fatalf("docs example %d diffs[0] missing SDK field %q", i, k)
					}
				}
			}
			if sec, _ := fixture["section"].(map[string]any); sec != nil {
				if hs, ok := sec["head_sha"].(string); ok {
					if _, valid := readmeta.ObservedHeadSHA(hs); !valid {
						t.Fatalf("docs example %d head_sha %q is not valid 40-hex", i, hs)
					}
				}
			}
			toolName := fmt.Sprintf("docs_envelope_validate_%d", i)
			payload := fixture
			valSrv := mcp.NewServer(&mcp.Implementation{Name: "docs-val", Version: "t"}, nil)
			mcp.AddTool(valSrv, &mcp.Tool{
				Name:         toolName,
				Description:  "temporary validator for docs list-diff envelope fixture",
				OutputSchema: listDiffSchema,
			}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, map[string]any, error) {
				return nil, payload, nil
			})
			valCS := testutil.MCPConnect(t, valSrv)
			res, err := valCS.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      toolName,
				Arguments: map[string]any{},
			})
			if err != nil {
				t.Fatalf("docs example %d failed SDK applySchema via CallTool: %v\n%s", i, err, block)
			}
			if res.IsError {
				t.Fatalf("docs example %d IsError=true: %q", i, toolErrorText(t, res))
			}
			if res.StructuredContent == nil {
				t.Fatalf("docs example %d missing StructuredContent after schema validation", i)
			}
		case "get_merge_request_diff_window_content":
			contentCount++
			assertDocsContentEnvelopeContract(t, i, fixture)
		default:
			t.Fatalf("docs example %d unrecognized envelope shape: keys=%v", i, mapKeys(fixture))
		}
	}
	if listCount < 2 {
		t.Fatalf("expected ≥2 list-diff docs examples, got %d", listCount)
	}
	if contentCount < 3 {
		t.Fatalf("expected ≥3 content-mode docs examples, got %d", contentCount)
	}
	// Actual registered CallTool projection oracles for the three content docs examples.
	oracles := registeredContentDocsOracles(t)
	contentIdx := 0
	for i, block := range blocks {
		var fixture map[string]any
		if err := json.Unmarshal([]byte(block), &fixture); err != nil {
			t.Fatalf("docs example %d not JSON: %v", i, err)
		}
		if classifyDocsEnvelopeExample(fixture) != "get_merge_request_diff_window_content" {
			continue
		}
		contentIdx++
		var oracle map[string]any
		var label string
		switch contentIdx {
		case 1:
			oracle, label = oracles["version"], "version"
		case 2:
			oracle, label = oracles["straight"], "straight"
		case 3:
			oracle, label = oracles["drift"], "drift"
		default:
			t.Fatalf("unexpected content example index %d", contentIdx)
		}
		assertDocsContentMatchesCallToolOracle(t, i, label, fixture, oracle)
	}
	// Negative controls: mutate documented fields; oracle comparison must fail.
	t.Run("negative_mutate_selection_kind", func(t *testing.T) {
		bad := cloneMap(oracles["version"])
		sel := asMap(t, bad["selection"])
		sel["kind"] = "version" // not emitted
		bad["selection"] = sel
		err := docsContentOracleMismatch(oracles["version"], bad)
		if err == nil {
			t.Fatal("mutated selection.kind must fail oracle")
		}
	})
	t.Run("negative_mutate_limitation_detail", func(t *testing.T) {
		bad := cloneMap(oracles["straight"])
		sec := asMap(t, bad["section"])
		sec["limitations"] = []any{map[string]any{"code": "partial", "detail": "compare"}}
		bad["section"] = sec
		err := docsContentOracleMismatch(oracles["straight"], bad)
		if err == nil {
			t.Fatal("mutated limitation detail must fail oracle")
		}
	})
	t.Run("negative_mutate_drift_selectors_empty", func(t *testing.T) {
		bad := cloneMap(oracles["drift"])
		bad["selectors"] = []any{}
		err := docsContentOracleMismatch(oracles["drift"], bad)
		if err == nil {
			t.Fatal("erased drift selectors must fail oracle")
		}
	})
}

func cloneMap(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func registeredContentDocsOracles(t *testing.T) map[string]map[string]any {
	t.Helper()
	head, base, start := shaN(1), shaN(2), shaN(3)
	from, to := shaN(4), shaN(5)
	patch := "@@ -1 +1 @@\n-a\n+b\n"
	out := map[string]map[string]any{}

	// version success
	body := versionObject(1, 5001, head, base, start, "collected", "1", oneDiff("a.go", patch))
	hVer := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/versions/1"):
			_, _ = io.WriteString(w, body)
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
		default:
			http.NotFound(w, r)
		}
	})
	ver, err := callDiffWindow(t, diffDeps(t, hVer), nil, map[string]any{
		"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1,
		"mode": "content", "paths": []any{"a.go"},
	})
	if err != nil {
		t.Fatalf("version oracle CallTool: %v", err)
	}
	out["version"] = ver

	// straight partial (matched + unobserved)
	hStr := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/repository/commits/"):
			sha := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			fmt.Fprintf(w, `{"id":%q}`, sha)
		case strings.Contains(r.URL.Path, "/repository/compare"):
			fmt.Fprintf(w, `{"commit":{"id":%q},"diffs":[{"old_path":"a.go","new_path":"a.go","a_mode":"100644","b_mode":"100644","diff":%q}]}`, to, patch)
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
		default:
			http.NotFound(w, r)
		}
	})
	str, err := callDiffWindow(t, diffDeps(t, hStr), nil, map[string]any{
		"project_id": "42", "merge_request_iid": 1, "from_sha": from, "to_sha": to, "straight": true,
		"mode": "content", "paths": []any{"a.go", "missing.go"},
	})
	if err != nil {
		t.Fatalf("straight oracle CallTool: %v", err)
	}
	out["straight"] = str

	// closing path drift
	var n int
	hDrift := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/repository/commits/"):
			sha := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			fmt.Fprintf(w, `{"id":%q}`, sha)
		case strings.Contains(r.URL.Path, "/repository/compare"):
			n++
			p := "a.go"
			if n > 1 {
				p = "b.go"
			}
			fmt.Fprintf(w, `{"commit":{"id":%q},"diffs":[{"old_path":%q,"new_path":%q,"a_mode":"100644","b_mode":"100644","diff":%q}]}`, to, p, p, patch)
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
		default:
			http.NotFound(w, r)
		}
	})
	drift, err := callDiffWindow(t, diffDeps(t, hDrift), nil, map[string]any{
		"project_id": "42", "merge_request_iid": 1, "from_sha": from, "to_sha": to, "straight": true,
		"mode": "content", "paths": []any{"a.go"},
	})
	if err != nil {
		t.Fatalf("drift oracle CallTool: %v", err)
	}
	out["drift"] = drift
	return out
}

func assertDocsContentMatchesCallToolOracle(t *testing.T, docIdx int, label string, doc, oracle map[string]any) {
	t.Helper()
	oSec := asMap(t, oracle["section"])
	dSec, _ := doc["section"].(map[string]any)
	if dSec == nil {
		t.Fatalf("docs %d/%s missing section", docIdx, label)
	}
	// Exact mandatory fields from registered CallTool (docs may abridge other readmeta keys).
	for _, k := range []string{"content_complete", "manifest_coverage", "patch_coverage", "consistency"} {
		if dSec[k] != nil && dSec[k] != oSec[k] {
			t.Fatalf("docs %d/%s section.%s=%v want oracle %v (CallTool)", docIdx, label, k, dSec[k], oSec[k])
		}
		if dSec[k] == nil {
			t.Fatalf("docs %d/%s section.%s omitted; required by content contract (mark abridgement only for non-mandatory keys)", docIdx, label, k)
		}
	}
	if oSel, _ := oracle["selection"].(map[string]any); oSel != nil {
		dSel, _ := doc["selection"].(map[string]any)
		if dSel == nil || dSel["kind"] != oSel["kind"] {
			t.Fatalf("docs %d/%s selection.kind=%v want %v", docIdx, label, dSel, oSel["kind"])
		}
	}
	// Limitations: when present in docs, code+message must match oracle (not detail).
	if dLims, ok := dSec["limitations"].([]any); ok && len(dLims) > 0 {
		oLims, _ := oSec["limitations"].([]any)
		if len(oLims) == 0 {
			t.Fatalf("docs %d/%s has limitations but oracle has none", docIdx, label)
		}
		d0 := asMap(t, dLims[0])
		o0 := asMap(t, oLims[0])
		if d0["code"] != o0["code"] || d0["message"] != o0["message"] {
			t.Fatalf("docs %d/%s limitation %#v want %#v", docIdx, label, d0, o0)
		}
		if _, has := d0["detail"]; has {
			t.Fatalf("docs %d/%s limitation uses detail", docIdx, label)
		}
	}
	// Selectors: docs must retain requested path outcomes matching oracle statuses.
	oSels := asSlice(t, oracle["selectors"])
	dSels, _ := doc["selectors"].([]any)
	if len(dSels) == 0 && len(oSels) > 0 {
		t.Fatalf("docs %d/%s erased selectors; oracle keeps %#v", docIdx, label, oSels)
	}
	byPath := map[string]string{}
	for _, raw := range oSels {
		m := asMap(t, raw)
		byPath[asString(m["path"])] = asString(m["status"])
	}
	for _, raw := range dSels {
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		p := asString(m["path"])
		if want, ok := byPath[p]; ok && asString(m["status"]) != want {
			t.Fatalf("docs %d/%s selector %s status=%v want %s", docIdx, label, p, m["status"], want)
		}
	}
	// Hash nullability
	if label == "drift" {
		if oracle["returned_content_hash"] != nil {
			t.Fatalf("drift oracle unexpectedly trusted hash")
		}
		if doc["returned_content_hash"] != nil {
			t.Fatalf("docs %d/drift returned_content_hash must be null", docIdx)
		}
	} else if doc["returned_content_hash"] == nil {
		t.Fatalf("docs %d/%s missing returned_content_hash (illustrative value ok)", docIdx, label)
	}
	if doc["full_patch_hash"] != nil {
		t.Fatalf("docs %d/%s full_patch_hash must be null", docIdx, label)
	}
}

func docsContentOracleMismatch(oracle, candidate map[string]any) error {
	// Minimal oracle equality for negative controls on documented fields.
	oSec, _ := oracle["section"].(map[string]any)
	cSec, _ := candidate["section"].(map[string]any)
	if cSec == nil {
		return fmt.Errorf("missing section")
	}
	for _, k := range []string{"content_complete", "manifest_coverage", "patch_coverage", "consistency"} {
		if cSec[k] != oSec[k] {
			return fmt.Errorf("section.%s", k)
		}
	}
	oSel, _ := oracle["selection"].(map[string]any)
	cSel, _ := candidate["selection"].(map[string]any)
	if oSel != nil && (cSel == nil || cSel["kind"] != oSel["kind"]) {
		return fmt.Errorf("selection.kind")
	}
	oLims, _ := oSec["limitations"].([]any)
	cLims, _ := cSec["limitations"].([]any)
	if len(oLims) > 0 {
		if len(cLims) == 0 {
			return fmt.Errorf("limitations missing")
		}
		o0, _ := oLims[0].(map[string]any)
		c0, _ := cLims[0].(map[string]any)
		if c0["code"] != o0["code"] || c0["message"] != o0["message"] || c0["detail"] != nil {
			return fmt.Errorf("limitation")
		}
	}
	oSels, _ := oracle["selectors"].([]any)
	cSels, _ := candidate["selectors"].([]any)
	if len(oSels) > 0 && len(cSels) == 0 {
		return fmt.Errorf("selectors")
	}
	return nil
}

func assertDocsContentEnvelopeContract(t *testing.T, i int, fixture map[string]any) {
	t.Helper()
	// Structural guards only; field truth comes from registeredContentDocsOracles CallTool.
	if _, ok := fixture["diffs"]; ok {
		t.Fatalf("content example %d must not fabricate list-diff fields", i)
	}
	if _, ok := fixture["pagination"]; ok {
		t.Fatalf("content example %d must not fabricate list pagination", i)
	}
	sec, _ := fixture["section"].(map[string]any)
	if sec == nil {
		t.Fatalf("content example %d missing section", i)
	}
}

func classifyDocsEnvelopeExample(fixture map[string]any) string {
	if _, ok := fixture["diffs"]; ok {
		if _, ok := fixture["pagination"]; ok {
			return "list_merge_request_diffs"
		}
	}
	_, hasFiles := fixture["files"]
	_, hasSelectors := fixture["selectors"]
	_, hasSelection := fixture["selection"]
	_, hasReturned := fixture["returned_content_hash"]
	_, hasFullPatch := fixture["full_patch_hash"]
	if hasFiles || hasSelectors || hasSelection || hasReturned || hasFullPatch {
		return "get_merge_request_diff_window_content"
	}
	return ""
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func extractJSONFencedBlocks(md string) []string {
	re := regexp.MustCompile("(?s)```json\\s*\\n(.*?)\\n```")
	matches := re.FindAllStringSubmatch(md, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) > 1 {
			out = append(out, strings.TrimSpace(m[1]))
		}
	}
	return out
}

func TestListMergeRequestDiffs_CallToolUsesBudgetInterceptor(t *testing.T) {
	ts := httptest.NewServer(mrHandler(42, 42, knownPresenceDiff, "", true))
	t.Cleanup(ts.Close)
	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(igl.BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Config: &config.Config{Token: "t"}, Client: cli}
	res, err := callListMergeRequestDiffs(t, d, callArgs("42", 1))
	if err != nil {
		t.Fatal(err)
	}
	_ = assertEnvelopeStructured(t, res)
}
