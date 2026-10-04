package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"
)

func TestRepair_F3F4_versionRowsAndPaging(t *testing.T) {
	t.Run("malformed plus valid sibling", func(t *testing.T) {
		log := &pathLog{}
		d := newReviewDeps(t, http.HandlerFunc((&reviewScript{log: log, version: "malformed-plus-match", versionIID: 2}).serve))
		out, err := callReviewContext(t, d, nil, map[string]any{"items": []any{
			itemArg("42", 1, []any{"metadata"}, nil),
			itemArg("42", 2, []any{"metadata"}, nil),
		}}, false)
		if err != nil {
			t.Fatal(err)
		}
		items := itemsOf(t, out)
		if asMap(t, items[0])["context_ref"] == nil {
			t.Fatal("valid sibling lost")
		}
		bad := asMap(t, items[1])
		if bad["context_ref"] != nil || bad["review_clean"] == true {
			t.Fatalf("malformed row selected %#v", bad)
		}
		raw, _ := json.Marshal(bad)
		if bytesContains(raw, shaN(9)) {
			t.Fatalf("echoed malformed neighbour %s", raw)
		}
	})
	t.Run("historical non-match still selects", func(t *testing.T) {
		d := newReviewDeps(t, http.HandlerFunc((&reviewScript{version: "historical-plus-match"}).serve))
		out, err := callReviewContext(t, d, nil, map[string]any{"items": []any{itemArg("42", 1, []any{"metadata"}, nil)}}, false)
		if err != nil {
			t.Fatal(err)
		}
		if asMap(t, itemsOf(t, out)[0])["context_ref"] == nil {
			t.Fatal("exact match beside a proved historical row was dropped")
		}
	})
	for _, mode := range []string{"dup-next", "repeat-next", "ws-next", "bad-next", "pad-next", "back-next", "jump-next"} {
		t.Run(mode, func(t *testing.T) {
			log := &pathLog{}
			d := newReviewDeps(t, http.HandlerFunc((&reviewScript{log: log, version: mode, versionIID: 2}).serve))
			out, err := callReviewContext(t, d, nil, map[string]any{"items": []any{
				itemArg("42", 1, []any{"metadata"}, nil),
				itemArg("42", 2, []any{"metadata"}, nil),
			}}, false)
			if err != nil {
				t.Fatal(err)
			}
			items := itemsOf(t, out)
			if asMap(t, items[0])["context_ref"] == nil || asMap(t, items[1])["context_ref"] != nil {
				t.Fatalf("%s sibling/ref %#v %#v", mode, asMap(t, items[0])["context_ref"] != nil, asMap(t, items[1])["cause"])
			}
			if strings.Contains(strings.Join(log.paths, "\n"), "?page=") || strings.Contains(strings.Join(log.paths, "\n"), "&page=") {
				t.Fatalf("%s fetched page 2", mode)
			}
		})
	}
	n := int64(2)
	hdr := http.Header{"X-Next-Page": []string{""}}
	if reviewVersionPaging(hdr, &n) {
		t.Fatal("empty header contradicted by SDK next was exhausted")
	}
}

func TestRepair_F5_deadlinePhases(t *testing.T) {
	phases := []struct {
		name  string
		hit   int
		items []reviewContextItemIn
	}{
		{"proof", 2, []reviewContextItemIn{metaItem("42", 1, "metadata"), metaItem("42", 2, "metadata")}},
		{"section", 1, []reviewContextItemIn{metaItem("42", 1, "metadata"), metaItem("42", 2, "metadata", "approvals")}},
		{"bracket", 2, []reviewContextItemIn{metaItem("42", 1, "metadata"), metaItem("42", 2, "metadata")}},
		{"mint", 2, []reviewContextItemIn{metaItem("42", 1, "metadata"), metaItem("42", 2, "metadata")}},
	}
	for _, tc := range phases {
		t.Run(tc.name, func(t *testing.T) {
			var n int
			ctx := withReviewPhaseHook(igl.WithBudget(context.Background(), reviewBudget(128)), func(phase string) error {
				if phase != tc.name {
					return nil
				}
				n++
				if n == tc.hit {
					return context.DeadlineExceeded
				}
				return nil
			})
			d := newReviewDeps(t, http.HandlerFunc((&reviewScript{}).serve))
			out, err := callReviewDirect(t, d, ctx, tc.items)
			if err != nil || len(out.Items) != 2 {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if out.Items[0].ContextRef == nil || out.Items[1].ContextRef != nil || out.Items[1].Cause != readmeta.CodeBudgetElapsed || out.Items[1].ReviewClean {
				t.Fatalf("%s item0=%v item1=%+v", tc.name, out.Items[0].ContextRef != nil, out.Items[1])
			}
			sec, ok := out.Items[1].Sections["metadata"]
			if !ok {
				t.Fatalf("%s metadata envelope missing", tc.name)
			}
			if tc.name == "section" {
				ap, ok := out.Items[1].Sections["approvals"]
				if !ok || ap.ContentComplete != readmeta.ContentCompleteUnknown || ap.HeadSHA != nil || ap.NextCursor != nil {
					t.Fatalf("approvals envelope %+v", ap)
				}
			}
			if tc.name == "mint" {
				if sec.ContentComplete != readmeta.ContentCompleteTrue || sec.HeadSHA == nil {
					t.Fatalf("%s proved metadata %+v", tc.name, sec)
				}
			} else if sec.ContentComplete != readmeta.ContentCompleteUnknown || sec.HeadSHA != nil {
				t.Fatalf("%s metadata envelope %+v", tc.name, sec)
			}
		})
	}
	parent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	d := newReviewDeps(t, http.HandlerFunc((&reviewScript{}).serve))
	_, err := callReviewDirect(t, d, igl.WithBudget(parent, reviewBudget(128)), []reviewContextItemIn{metaItem("42", 1, "metadata")})
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeBudgetElapsed) || strings.Contains(err.Error(), readmeta.CodeCancelled) {
		t.Fatalf("principal deadline: %v", err)
	}
}

func TestRepair_F6_approvalStatusClass(t *testing.T) {
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
			d := newReviewDeps(t, http.HandlerFunc((&reviewScript{approval: tc.status}).serve))
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
			item := asMap(t, items[1])
			body := asMap(t, item["approvals"])
			approvalShapeKeys(t, body)
			if body["approved"] != nil || body["rules"] != nil {
				t.Fatalf("success fields %#v", body)
			}
			sec := asMap(t, item["sections"].(map[string]any)["approvals"])
			rawLim, _ := json.Marshal(sec["limitations"])
			if !strings.Contains(string(rawLim), tc.code) || !strings.Contains(string(rawLim), tc.msg) {
				t.Fatalf("%d limitations %s", tc.status, rawLim)
			}
			if tc.status != 500 && strings.Contains(string(rawLim), "upstream server error") {
				t.Fatalf("%d invented server error %s", tc.status, rawLim)
			}
			want, derr := approvalFailureDigest(endpointApprovalState, tc.code, []readmeta.Limitation{{Code: tc.code, Message: tc.msg}})
			if derr != nil || item["approval_digest"] != want {
				t.Fatalf("%d digest got %#v want %s", tc.status, item["approval_digest"], want)
			}
		})
	}
}

func TestRepair_N1_isolatedDrifts(t *testing.T) {
	cases := []struct {
		name   string
		script *reviewScript
		absent string
	}{
		{"target", &reviewScript{secondTgt: shaN(77), secondTgtIID: 2}, shaN(77)},
		{"head", &reviewScript{driftHeadIID: 2, driftHeadSHA: shaN(77)}, shaN(77)},
		{"branch", &reviewScript{driftBranchIID: 2}, "renamed"},
		{"version", &reviewScript{driftVersionIID: 2}, "8002"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := &pathLog{}
			tc.script.log = log
			d := newReviewDeps(t, http.HandlerFunc(tc.script.serve))
			out, err := callReviewContext(t, d, nil, map[string]any{"items": []any{
				itemArg("42", 1, []any{"metadata"}, nil),
				itemArg("42", 2, []any{"metadata"}, nil),
			}}, false)
			if err != nil {
				t.Fatal(err)
			}
			items := itemsOf(t, out)
			if asMap(t, items[0])["context_ref"] == nil || asMap(t, items[1])["context_ref"] != nil {
				t.Fatalf("refs %#v %#v", asMap(t, items[0])["context_ref"] != nil, asMap(t, items[1]))
			}
			raw, _ := json.Marshal(items[1])
			if strings.Contains(string(raw), tc.absent) {
				t.Fatalf("echoed %s in %s", tc.absent, raw)
			}
		})
	}
}

func TestRepair_N6_missingBranchIsUnresolved(t *testing.T) {
	log := &pathLog{}
	d := newReviewDeps(t, http.HandlerFunc((&reviewScript{log: log, missingBranchIID: 2}).serve))
	out, err := callReviewContext(t, d, nil, map[string]any{"items": []any{
		itemArg("42", 1, []any{"metadata"}, nil),
		itemArg("42", 2, []any{"metadata"}, nil),
	}}, false)
	if err != nil {
		t.Fatal(err)
	}
	items := itemsOf(t, out)
	bad := asMap(t, items[1])
	if asMap(t, items[0])["context_ref"] == nil || bad["context_ref"] != nil || bad["cause"] != readmeta.CodeIdentityUnresolved {
		t.Fatalf("%#v %#v", asMap(t, items[0])["context_ref"] != nil, bad)
	}
	if strings.Contains(fmtCause(bad), readmeta.CodeHTTPError) {
		t.Fatalf("404 became http_error %#v", bad)
	}
}

func TestRepair_redirectLaterHopBudget(t *testing.T) {
	plainLog := &pathLog{}
	plainDeps := newReviewDeps(t, http.HandlerFunc((&reviewScript{log: plainLog}).serve))
	plainBudget := reviewBudget(128)
	plainOut, err := callReviewDirect(t, plainDeps, igl.WithBudget(context.Background(), plainBudget), []reviewContextItemIn{metaItem("42", 1, "metadata")})
	if err != nil || plainOut.Items[0].ContextRef == nil {
		t.Fatalf("baseline: %v", err)
	}
	baseReqs, _, _ := plainBudget.Stats()
	if baseReqs <= 3 {
		t.Fatalf("baseline requests %d", baseReqs)
	}
	log := &pathLog{}
	script := &reviewScript{log: log, redirect: "same"}
	d := newReviewDeps(t, http.HandlerFunc(script.serve))
	b := reviewBudget(3)
	out, err := callReviewDirect(t, d, igl.WithBudget(context.Background(), b), []reviewContextItemIn{metaItem("42", 1, "metadata")})
	if err != nil {
		t.Fatal(err)
	}
	if out.Items[0].ContextRef != nil || out.Items[0].Cause != readmeta.CodeBudgetRequests {
		t.Fatalf("later hop minted %+v reqs baseline %d", out.Items[0], baseReqs)
	}
	if got := log.count("/merge_requests/1?"); got != 1 {
		t.Fatalf("follow hop reached the server: detail hits %d", got)
	}
	script = &reviewScript{redirect: "path"}
	d = newReviewDeps(t, http.HandlerFunc(script.serve))
	mcpOut, err := callReviewContext(t, d, nil, map[string]any{"items": []any{itemArg("42", 1, []any{"metadata"}, nil)}}, false)
	if err != nil || script.destHits != 0 || asMap(t, itemsOf(t, mcpOut)[0])["context_ref"] != nil {
		t.Fatalf("destination reads %d err %v", script.destHits, err)
	}
}

func fmtCause(item map[string]any) string {
	raw, _ := json.Marshal(item)
	return string(raw)
}

func bytesContains(raw []byte, needle string) bool {
	return strings.Contains(string(raw), needle)
}
