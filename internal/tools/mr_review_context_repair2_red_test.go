package tools

import (
	"context"
	"net/http"
	"testing"

	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"
)

// TestRepair2_R1_unattemptedApprovalsCapability stops before any approval read.
// The DeadlineExceeded value is injected by the phase hook. It is a classification
// sentinel, not a wall-clock timeout.
func TestRepair2_R1_unattemptedApprovalsCapability(t *testing.T) {
	cases := []struct {
		name  string
		phase string
		hit   int
		items []reviewContextItemIn
	}{
		{
			name:  "proof",
			phase: "proof",
			hit:   2,
			items: []reviewContextItemIn{metaItem("42", 1, "metadata"), metaItem("42", 2, "metadata", "approvals")},
		},
		{
			name:  "section",
			phase: "section",
			hit:   1,
			items: []reviewContextItemIn{metaItem("42", 1, "metadata"), metaItem("42", 2, "metadata", "approvals")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := &pathLog{}
			var n int
			ctx := withReviewPhaseHook(igl.WithBudget(context.Background(), reviewBudget(128)), func(phase string) error {
				if phase != tc.phase {
					return nil
				}
				n++
				if n == tc.hit {
					return context.DeadlineExceeded
				}
				return nil
			})
			d := newReviewDeps(t, http.HandlerFunc((&reviewScript{log: log}).serve))
			out, err := callReviewDirect(t, d, ctx, tc.items)
			if err != nil || len(out.Items) != 2 {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if out.Items[0].ContextRef == nil || out.Items[1].ContextRef != nil || out.Items[1].Cause != readmeta.CodeBudgetElapsed || out.Items[1].ReviewClean {
				t.Fatalf("%s sibling/ref %+v %+v", tc.name, out.Items[0].ContextRef != nil, out.Items[1])
			}
			if out.Items[1].Approvals != nil {
				t.Fatalf("%s unattempted approvals object %#v", tc.name, out.Items[1].Approvals)
			}
			if got := log.count("/approval_state"); got != 0 {
				t.Fatalf("%s approval HTTP %d", tc.name, got)
			}
			sec, ok := out.Items[1].Sections["approvals"]
			if !ok {
				t.Fatalf("%s approvals section missing", tc.name)
			}
			if sec.CapabilityVersion != capabilityMRApprovalsV1 {
				t.Fatalf("%s capability %q", tc.name, sec.CapabilityVersion)
			}
			if sec.ContentComplete != readmeta.ContentCompleteUnknown || sec.Consistency != readmeta.ConsistencyUnknown || sec.HeadSHA != nil || sec.NextCursor != nil {
				t.Fatalf("%s envelope %+v", tc.name, sec)
			}
			meta, ok := out.Items[1].Sections["metadata"]
			if !ok || meta.CapabilityVersion != capabilityReviewContextMetaV1 {
				t.Fatalf("%s metadata capability %+v", tc.name, meta.CapabilityVersion)
			}
		})
	}
}

func TestRepair2_R1_approvalErrorSkeleton(t *testing.T) {
	cases := []struct {
		status int
		code   string
		msg    string
	}{
		{401, readmeta.CodeInaccessible, "authorization denied"},
		{403, readmeta.CodeInaccessible, "authorization denied"},
		{404, readmeta.CodeUnsupported, "approval resource unavailable"},
		{405, readmeta.CodeUnsupported, "method not allowed"},
		{500, readmeta.CodeHTTPError, "upstream server error"},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			log := &pathLog{}
			d := newReviewDeps(t, http.HandlerFunc((&reviewScript{log: log, approval: tc.status}).serve))
			out, err := callReviewContext(t, d, nil, map[string]any{"items": []any{
				itemArg("42", 1, []any{"metadata"}, nil),
				itemArg("42", 2, []any{"metadata", "approvals"}, nil),
			}}, false)
			if err != nil {
				t.Fatal(err)
			}
			items := itemsOf(t, out)
			if asMap(t, items[0])["context_ref"] == nil {
				t.Fatal("sibling ref cleared")
			}
			if got := log.count("/approval_state"); got != 1 {
				t.Fatalf("approval HTTP %d", got)
			}
			item := asMap(t, items[1])
			if item["context_ref"] == nil || item["review_clean"] == true {
				t.Fatalf("metadata ref/clean %#v", item["context_ref"] != nil)
			}
			if item["approvals"] == nil {
				t.Fatal("error skeleton missing")
			}
			body := asMap(t, item["approvals"])
			if body["endpoint"] != endpointApprovalState {
				t.Fatalf("endpoint %#v", body["endpoint"])
			}
			if body["rules_capability"] != rulesCapabilityUnknown || body["rules_complete"] != readmeta.ContentCompleteUnknown {
				t.Fatalf("rules_capability=%#v rules_complete=%#v", body["rules_capability"], body["rules_complete"])
			}
			for _, key := range []string{"approved", "actor_approved", "actor_can_approve", "approvals_required", "approvals_left", "rules", "rules_left"} {
				if body[key] != nil {
					t.Fatalf("%s=%#v", key, body[key])
				}
			}
			sec := asMap(t, body["section"])
			if sec["capability_version"] != capabilityMRApprovalsV1 || sec["head_sha"] != nil || sec["content_complete"] != readmeta.ContentCompleteUnknown {
				t.Fatalf("nested section %#v", sec)
			}
			outer := asMap(t, asMap(t, item["sections"])["approvals"])
			if outer["capability_version"] != capabilityMRApprovalsV1 {
				t.Fatalf("outer capability %#v", outer["capability_version"])
			}
			lims, ok := sec["limitations"].([]any)
			if !ok || len(lims) != 1 {
				t.Fatalf("limitations %#v", sec["limitations"])
			}
			lim := asMap(t, lims[0])
			if lim["code"] != tc.code || lim["message"] != tc.msg {
				t.Fatalf("limitation %#v", lim)
			}
			want, derr := approvalFailureDigest(endpointApprovalState, tc.code, []readmeta.Limitation{{Code: tc.code, Message: tc.msg}})
			if derr != nil || item["approval_digest"] != want {
				t.Fatalf("digest %#v want %s", item["approval_digest"], want)
			}
		})
	}
}
