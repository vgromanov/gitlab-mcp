package tools

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
)

const approvalStateBody = `{"approval_rules_overwritten":false,"rules":[{"id":1,"name":"Default","rule_type":"regular","approvals_required":1,"approved":true,"approved_by":[{"id":2,"username":"bob"}]}]}`

const legacyApprovalsBody = `{"id":1,"iid":3,"approved":true,"approvals_required":2,"approvals_left":0,
	"approved_by":[{"user":{"id":2,"username":"bob"}},{"user":null}]}`

// approvalSession serves /approval_state with stateStatus/stateBody and the
// legacy /approvals with a fixed body, and counts requests per path.
func approvalSession(t *testing.T, stateStatus int, stateBody string) (*mcp.ClientSession, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	return approvalSessionLegacy(t, stateStatus, stateBody, http.StatusOK)
}

func approvalSessionLegacy(t *testing.T, stateStatus int, stateBody string, legacyStatus int) (*mcp.ClientSession, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var state, legacy atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/projects/42/merge_requests/3/approval_state":
			state.Add(1)
			w.WriteHeader(stateStatus)
			writeFixture(w, stateBody)
		case "/api/v4/projects/42/merge_requests/3/approvals":
			legacy.Add(1)
			w.WriteHeader(legacyStatus)
			_, _ = fmt.Fprint(w, legacyApprovalsBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	// No SDK retries: a 5xx is observed exactly once.
	cli, err := gitlab.NewClient("test-token", gitlab.WithBaseURL(ts.URL+"/api/v4"), gitlab.WithoutRetries())
	if err != nil {
		t.Fatal(err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "test"}, nil)
	RegisterMergeRequests(srv, Deps{Config: &config.Config{}, Client: cli})
	return testutil.MCPConnect(t, srv), &state, &legacy
}

func keysOf(m map[string]any) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

var approvalArgs = map[string]any{"project_id": "42", "merge_request_iid": 3}

func TestApprovalState_fallbackOn404SameShape(t *testing.T) {
	cs, _, _ := approvalSession(t, http.StatusOK, approvalStateBody)
	primary := callDiffTool(t, cs, "get_merge_request_approval_state", approvalArgs)

	cs, state, legacy := approvalSession(t, http.StatusNotFound, `{"message":"404 Not found"}`)
	fallback := callDiffTool(t, cs, "get_merge_request_approval_state", approvalArgs)
	if state.Load() != 1 || legacy.Load() != 1 {
		t.Fatalf("requests: approval_state=%d approvals=%d, want 1 and 1", state.Load(), legacy.Load())
	}
	if !slices.Equal(keysOf(primary), keysOf(fallback)) {
		t.Fatalf("shape differs: primary %v, fallback %v", keysOf(primary), keysOf(fallback))
	}
	rules, _ := fallback["rules"].([]any)
	rule, _ := rules[0].(map[string]any)
	by, _ := rule["approved_by"].([]any)
	if len(rules) != 1 || rule["approvals_required"] != float64(2) || rule["approved"] != true || len(by) != 1 {
		t.Fatalf("converted rule = %v", rules)
	}
	prules, _ := primary["rules"].([]any)
	if !slices.Equal(keysOf(prules[0].(map[string]any)), keysOf(rule)) {
		t.Fatalf("rule shape differs: %v vs %v", keysOf(prules[0].(map[string]any)), keysOf(rule))
	}
}

func TestApprovalState_primaryNoFallback(t *testing.T) {
	cs, state, legacy := approvalSession(t, http.StatusOK, approvalStateBody)
	callDiffTool(t, cs, "get_merge_request_approval_state", approvalArgs)
	if state.Load() != 1 || legacy.Load() != 0 {
		t.Fatalf("requests: approval_state=%d approvals=%d, want 1 and 0", state.Load(), legacy.Load())
	}
}

func TestApprovalState_noFallbackOtherErrors(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cs, state, legacy := approvalSession(t, status, `{"message":"nope"}`)
			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_merge_request_approval_state", Arguments: approvalArgs})
			if err != nil || !res.IsError {
				t.Fatalf("want a tool error, got err=%v res=%v", err, res)
			}
			if legacy.Load() != 0 {
				t.Fatalf("fell back on %d", status)
			}
			if state.Load() != 1 {
				t.Fatalf("approval_state requests = %d, want 1", state.Load())
			}
		})
	}
}

func TestApprovalState_bothFail(t *testing.T) {
	cs, _, _ := approvalSessionLegacy(t, http.StatusNotFound, `{}`, http.StatusInternalServerError)
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_merge_request_approval_state", Arguments: approvalArgs})
	if err != nil || !res.IsError {
		t.Fatalf("want a tool error, got err=%v res=%v", err, res)
	}
}
