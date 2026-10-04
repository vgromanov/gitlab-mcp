package tools

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

func TestReviewQueue_F1CursorOmitsUnprovedFacts(t *testing.T) {
	for _, mode := range []string{"discovery", "emit_owner", "emit_source"} {
		t.Run(mode, func(t *testing.T) {
			tr := &repairRT{mode: mode, two: mode != "discovery", deniedOwner: mode == "emit_owner", fork: mode == "emit_source"}
			d := repairDeps(t, tr, []string{"42"})
			out, err := callReviewQueue(t, d, map[string]any{"group_id": "9", "kinds": []any{"reviewer"}, "page_size": 1})
			if err != nil {
				t.Fatal(err)
			}
			tok, _ := sectionMap(out)["next_cursor"].(string)
			if tok == "" {
				if mode == "discovery" {
					t.Fatal("discovery with a further page must return a cursor")
				}
				return
			}
			p, err := cursor.Decode(d.Config.CursorKey, tok, d.now())
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range p.QueueCont.CM {
				if (mode == "discovery" || mode == "emit_owner") && strings.HasPrefix(c.K, "99:") {
					t.Fatalf("unproved owner fact in cursor: %s", c.K)
				}
				if mode == "emit_source" && c.K == "42:2" {
					t.Fatalf("unproved fork source fact in cursor: %s", c.K)
				}
			}
		})
	}
}

func TestReviewQueue_F2OngoingProjectSelection(t *testing.T) {
	t.Run("excluded_seed", func(t *testing.T) {
		tr := &repairRT{}
		d := repairDeps(t, tr, nil)
		out, err := callReviewQueue(t, d, map[string]any{
			"group_id": "9", "kinds": []any{"ongoing"}, "project_ids": []any{"99"},
			"known_mrs": []any{map[string]any{"project_id": "42", "iid": 1}},
		})
		if err != nil {
			t.Fatal(err)
		}
		items, _ := out["items"].([]any)
		if len(items) > 0 || tr.discussionHits > 0 || tr.mrHits > 0 {
			t.Fatalf("excluded seed read/emitted items=%d disc=%d mr=%d", len(items), tr.discussionHits, tr.mrHits)
		}
	})
	t.Run("path_alias", func(t *testing.T) {
		tr := &repairRT{discuss: true}
		d := repairDeps(t, tr, nil)
		out := drainQueue(t, d, map[string]any{
			"group_id": "9", "kinds": []any{"ongoing"}, "project_ids": []any{"fixture/p"},
			"known_mrs": []any{map[string]any{"project_id": "42", "iid": 1}},
		})
		items, _ := out["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("path alias must select project 42, items=%v", items)
		}
	})
	t.Run("mixed_no_spurious_ongoing", func(t *testing.T) {
		tr := &repairRT{two: true}
		d := repairDeps(t, tr, nil)
		out := drainQueue(t, d, map[string]any{
			"group_id": "9", "kinds": []any{"reviewer", "ongoing"}, "project_ids": []any{"42"},
			"known_mrs": []any{map[string]any{"project_id": "99", "iid": 2}},
		})
		if tr.discussionHits != 0 {
			t.Fatalf("excluded ongoing seed must not read discussions, hits=%d", tr.discussionHits)
		}
		for _, item := range out["items"].([]any) {
			for _, k := range item.(map[string]any)["kinds"].([]any) {
				if k.(string) == "ongoing" {
					t.Fatal("spurious ongoing bit")
				}
			}
		}
	})
}

func TestReviewQueue_F3AuthBudgetPreservesProgress(t *testing.T) {
	t.Run("owner_requests", func(t *testing.T) {
		assertEmitBudget(t, "/projects/42", igl.ErrBudgetRequests)
	})
	t.Run("owner_bytes", func(t *testing.T) {
		assertEmitBudget(t, "/projects/42", igl.ErrBudgetBytes)
	})
	t.Run("owner_elapsed", func(t *testing.T) {
		assertEmitBudget(t, "/projects/42", igl.ErrBudgetElapsed)
	})
	t.Run("shared_request_cap", func(t *testing.T) {
		tr := &repairRT{}
		d := repairDeps(t, tr, nil)
		sec := newReviewQueueSection(d.now())
		b := &igl.Budget{MaxItems: 100, MaxBytes: 8 << 20, MaxRequests: 1, MaxElapsed: time.Minute}
		ctx := igl.WithBudget(context.Background(), b)
		defer b.Cancel()
		if _, _, err := d.Client.Users.CurrentUser(gitlab.WithContext(ctx)); err != nil {
			t.Fatal(err)
		}
		st := emitStub(d, b, &sec)
		_, err := st.runEmit(ctx)
		if !errors.Is(err, igl.ErrBudgetRequests) || st.qc.EI != 0 || tr.hits != 1 {
			t.Fatalf("err=%v EI=%d hits=%d", err, st.qc.EI, tr.hits)
		}
	})
	t.Run("ancestry_budget", func(t *testing.T) {
		tr := &repairRT{budgetPath: "/groups/8", budgetErr: igl.ErrBudgetRequests, ancestry: true}
		d := repairDeps(t, tr, nil)
		sec := newReviewQueueSection(d.now())
		b := igl.DefaultBudget()
		st := emitStub(d, b, &sec)
		_, err := st.runEmit(igl.WithBudget(context.Background(), b))
		if !errors.Is(err, igl.ErrBudgetRequests) || st.qc.EI != 0 {
			t.Fatalf("ancestry budget swallowed err=%v EI=%d", err, st.qc.EI)
		}
	})
	t.Run("fork_source_budget", func(t *testing.T) {
		tr := &repairRT{budgetPath: "/projects/99", budgetErr: igl.ErrBudgetBytes, fork: true}
		d := repairDeps(t, tr, []string{"42"})
		sec := newReviewQueueSection(d.now())
		b := igl.DefaultBudget()
		st := emitStub(d, b, &sec)
		st.qc.CM[0].K = "42:2"
		_, err := st.runEmit(igl.WithBudget(context.Background(), b))
		if !errors.Is(err, igl.ErrBudgetBytes) || st.qc.EI != 0 {
			t.Fatalf("fork budget swallowed err=%v EI=%d", err, st.qc.EI)
		}
	})
	t.Run("allowlist_budget", func(t *testing.T) {
		tr := &repairRT{budgetPath: "/projects/7", budgetErr: igl.ErrBudgetElapsed}
		d := repairDeps(t, tr, []string{"7"})
		sec := newReviewQueueSection(d.now())
		b := igl.DefaultBudget()
		st := emitStub(d, b, &sec)
		_, err := st.runEmit(igl.WithBudget(context.Background(), b))
		if !errors.Is(err, igl.ErrBudgetElapsed) || st.qc.EI != 0 {
			t.Fatalf("allowlist budget swallowed err=%v EI=%d", err, st.qc.EI)
		}
	})
}

func TestReviewQueue_F4DecodedPartialSuccess(t *testing.T) {
	for _, mode := range []string{"noheader", "truncated", "junk"} {
		t.Run(mode, func(t *testing.T) {
			tr := &repairRT{mode: mode}
			d := repairDeps(t, tr, nil)
			out, err := callReviewQueue(t, d, map[string]any{"group_id": "9", "kinds": []any{"reviewer"}})
			if err != nil {
				t.Fatalf("valid prefix lost as error: %v", err)
			}
			items, _ := out["items"].([]any)
			if len(items) != 1 {
				t.Fatalf("items=%d", len(items))
			}
			if nc, _ := sectionMap(out)["next_cursor"].(string); nc != "" {
				t.Fatal("ambiguous page minted a cursor")
			}
		})
	}
	t.Run("prior_page", func(t *testing.T) {
		tr := &repairRT{mode: "prior"}
		d := repairDeps(t, tr, nil)
		out, err := callReviewQueue(t, d, map[string]any{"group_id": "9", "kinds": []any{"reviewer"}, "page_size": 20})
		if err != nil {
			t.Fatal(err)
		}
		items, _ := out["items"].([]any)
		if len(items) < 1 {
			t.Fatalf("prior page success dropped, items=%d limits=%v", len(items), sectionMap(out)["limitations"])
		}
	})
}

func TestReviewQueue_F5DiscussionNoteTerminal(t *testing.T) {
	t.Run("oversized_first", func(t *testing.T) {
		tr := &repairRT{notes: 108, discuss: true}
		d := repairDeps(t, tr, nil)
		out, err := callReviewQueue(t, d, map[string]any{
			"group_id": "9", "kinds": []any{"ongoing"},
			"known_mrs": []any{map[string]any{"project_id": "42", "iid": 1}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if nc, _ := sectionMap(out)["next_cursor"].(string); nc != "" {
			t.Fatal("mid-discussion stop must not mint a cursor")
		}
		codes := limitationCodes(sectionMap(out))
		if !hasCode(codes, readmeta.CodeBudgetItems) || !hasCode(codes, readmeta.CodePartial) || !hasCode(codes, readmeta.CodeMembershipIncomplete) {
			t.Fatalf("limitations=%v", codes)
		}
		items, _ := out["items"].([]any)
		if len(items) != 0 {
			t.Fatal("qualifying note past the budget must not be claimed")
		}
	})
	t.Run("leading_then_oversized", func(t *testing.T) {
		tr := &repairRT{notes: 108, discuss: true, leading: true}
		d := repairDeps(t, tr, nil)
		args := map[string]any{
			"group_id": "9", "kinds": []any{"ongoing"},
			"known_mrs": []any{map[string]any{"project_id": "42", "iid": 1}},
		}
		var first cursor.QueueOngoingProg
		for n := 0; n < 3; n++ {
			out, err := callReviewQueue(t, d, args)
			if err != nil {
				t.Fatal(err)
			}
			tok, _ := sectionMap(out)["next_cursor"].(string)
			if tok == "" {
				return
			}
			p, err := cursor.Decode(d.Config.CursorKey, tok, d.now())
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				first = *p.QueueCont.OG
			} else if *p.QueueCont.OG == first {
				t.Fatalf("nonadvancing discussion cursor %#v", first)
			}
			args["cursor"] = tok
		}
		t.Fatal("discussion scan neither advanced nor terminated")
	})
}

func TestReviewQueue_F5DiscussionBoundaryProgress(t *testing.T) {
	tr := &repairRT{notes: 1, leading: true, leadingNotes: 97, discuss: true}
	d := repairDeps(t, tr, nil)
	args := map[string]any{
		"group_id": "9", "kinds": []any{"ongoing"},
		"known_mrs": []any{map[string]any{"project_id": "42", "iid": 1}},
	}
	var prior cursor.QueueOngoingProg
	for n := 0; n < 3; n++ {
		out, err := callReviewQueue(t, d, args)
		if err != nil {
			t.Fatal(err)
		}
		tok, _ := sectionMap(out)["next_cursor"].(string)
		if tok == "" {
			codes := limitationCodes(sectionMap(out))
			for _, code := range []string{readmeta.CodeBudgetItems, readmeta.CodePartial, readmeta.CodeMembershipIncomplete} {
				if !hasCode(codes, code) {
					t.Fatalf("boundary terminal missing %s: %v", code, codes)
				}
			}
			items, _ := out["items"].([]any)
			if len(items) != 0 {
				t.Fatalf("unseen qualifying note emitted: %v", items)
			}
			qc, _ := out["queue_counts"].(map[string]any)
			if _, ok := qc["unobserved_membership_count"]; !ok || qc["unobserved_membership_count"] != nil {
				t.Fatalf("unfinished seed membership must stay unknown, counts=%v", qc)
			}
			return
		}
		p, err := cursor.Decode(d.Config.CursorKey, tok, d.now())
		if err != nil {
			t.Fatal(err)
		}
		cur := *p.QueueCont.OG
		if n > 0 && cur == prior {
			t.Fatalf("identical discussion-boundary cursor on attempt %d: %#v", n+1, cur)
		}
		prior = cur
		args["cursor"] = tok
	}
	t.Fatal("97-note boundary neither progressed nor terminalized")
}

func TestReviewQueue_F5ResumableDiscussionBoundary(t *testing.T) {
	tr := &repairRT{perSeed: true, leading: true, leadingNotes: 87, notes: 1, discuss: true}
	d := repairDeps(t, tr, nil)
	args := map[string]any{
		"group_id": "9", "kinds": []any{"ongoing"},
		"known_mrs": []any{
			map[string]any{"project_id": "42", "iid": 1},
			map[string]any{"project_id": "42", "iid": 2},
		},
	}
	out, err := callReviewQueue(t, d, args)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := sectionMap(out)["next_cursor"].(string)
	if tok == "" {
		t.Fatal("representable discussion boundary was terminalized")
	}
	p, err := cursor.Decode(d.Config.CursorKey, tok, d.now())
	if err != nil {
		t.Fatal(err)
	}
	if p.QueueCont.OG == nil || p.QueueCont.OG.SI != 1 || p.QueueCont.OG.DP != 1 || p.QueueCont.OG.CN != 1 {
		t.Fatalf("expected seed-2 discussion boundary, got %#v", p.QueueCont.OG)
	}
	args["cursor"] = tok
	out, err = callReviewQueue(t, d, args)
	if err != nil {
		t.Fatal(err)
	}
	tok2, _ := sectionMap(out)["next_cursor"].(string)
	if tok2 != "" {
		p2, err := cursor.Decode(d.Config.CursorKey, tok2, d.now())
		if err != nil {
			t.Fatal(err)
		}
		if p2.QueueCont.OG != nil && *p2.QueueCont.OG == *p.QueueCont.OG {
			t.Fatalf("resume replayed the same boundary: %#v", p2.QueueCont.OG)
		}
	}
	items, _ := out["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("resume did not emit the qualifying note, items=%v", items)
	}
}

func TestReviewQueue_R1InvalidPageProofBudget(t *testing.T) {
	for _, mode := range []string{"noheader", "dupheader", "jump", "truncated", "junk", ""} {
		for _, where := range []string{"owner", "source_after_proved"} {
			for _, cause := range []error{igl.ErrBudgetRequests, igl.ErrBudgetBytes} {
				name := mode + "_" + where + "_" + cause.Error()
				if mode == "" {
					name = "valid_" + where + "_" + cause.Error()
				}
				t.Run(name, func(t *testing.T) {
					tr := &repairRT{mode: mode, budgetErr: cause, budgetPath: "/projects/42"}
					if where == "source_after_proved" {
						tr.two = true
						tr.fork = true
						tr.budgetPath = "/projects/99"
					}
					d := repairDeps(t, tr, nil)
					out, err := callReviewQueue(t, d, map[string]any{"group_id": "9", "kinds": []any{"reviewer"}, "page_size": 20})
					if err != nil {
						t.Fatal(err)
					}
					tok, _ := sectionMap(out)["next_cursor"].(string)
					if mode == "" {
						if tok == "" {
							t.Fatal("valid page lost its budget continuation")
						}
						return
					}
					if tok != "" {
						t.Fatal("invalid page minted a continuation after proof budget")
					}
					if !hasCode(limitationCodes(sectionMap(out)), readmeta.CodeProviderPageAmbiguous) {
						t.Fatal("invalid page lost provider_page_ambiguous")
					}
					items, _ := out["items"].([]any)
					if where == "source_after_proved" && len(items) != 1 {
						t.Fatalf("proved prefix lost: items=%d", len(items))
					}
					for _, raw := range items {
						if raw.(map[string]any)["iid"] == float64(2) {
							t.Fatal("unproved fork candidate emitted")
						}
					}
				})
			}
		}
	}
}

func TestReviewQueue_R1InvalidDiscussionNoteBudget(t *testing.T) {
	for _, mode := range []string{"disc_noheader", "disc_duplicate", "disc_jump", "disc_truncated", "disc_junk", ""} {
		t.Run(mode, func(t *testing.T) {
			if mode == "" {
				t.Run("valid", func(t *testing.T) { testR1Discussion(t, "") })
				return
			}
			testR1Discussion(t, mode)
		})
	}
}

func testR1Discussion(t *testing.T, mode string) {
	t.Helper()
	tr := &repairRT{mode: mode, discuss: true}
	d := repairDeps(t, tr, nil)
	b := igl.DefaultBudget()
	t.Cleanup(b.Cancel)
	for n := 0; n < 98; n++ {
		if err := b.AddItem(); err != nil {
			t.Fatal(err)
		}
	}
	_, v, err := getMergeRequestReviewQueue(igl.WithBudget(context.Background(), b), nil, getMergeRequestReviewQueueIn{
		GroupID: "9", Kinds: []string{"ongoing"}, PageSize: 20,
		KnownMRs: []knownMRSeed{{ProjectID: "42", MergeRequestIID: 1}},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	out := v.(map[string]any)
	tok, _ := sectionMap(out)["next_cursor"].(string)
	if mode == "" {
		if tok == "" {
			t.Fatal("valid discussion lost note-0 continuation")
		}
		return
	}
	if tok != "" {
		t.Fatal("invalid discussion minted a continuation")
	}
	if !hasCode(limitationCodes(sectionMap(out)), readmeta.CodeProviderPageAmbiguous) {
		t.Fatal("invalid discussion lost provider_page_ambiguous")
	}
}

func TestReviewQueue_R2DirectMRContext(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		for _, phase := range []string{"discovery", "emit", "seed"} {
			t.Run(cause.Error()+"_"+phase, func(t *testing.T) {
				tr := &repairRT{budgetPath: "/projects/42/merge_requests/1", budgetErr: fmt.Errorf("fake direct metadata cause: %w", cause)}
				d := repairDeps(t, tr, nil)
				in := getMergeRequestReviewQueueIn{GroupID: "9", Kinds: []string{"reviewer"}, PageSize: 20}
				if phase == "seed" {
					in.Kinds = []string{"ongoing"}
					in.KnownMRs = []knownMRSeed{{ProjectID: "42", MergeRequestIID: 1}}
				}
				n, err := normalizeReviewQueueInput(in)
				if err != nil {
					t.Fatal(err)
				}
				b := igl.DefaultBudget()
				t.Cleanup(b.Cancel)
				sec := newReviewQueueSection(d.now())
				ctx := igl.WithBudget(context.Background(), b)
				st := &queueRuntime{d: d, budget: b, group: CanonicalGroup{ID: 9}, groupID: "9", authActor: 7, discoveryActor: 7, norm: n, filters: cursor.Filters{Until: "2026-10-03T12:00:00Z", PerPage: n.pageSize}, section: &sec, qc: newQueueCont(n), ctx: ctx, now: d.now()}
				switch phase {
				case "discovery":
					err = st.discoverKindStream(ctx, 0)
					if st.qc.KP[0].CN != 0 || st.qc.KP[0].E {
						t.Fatalf("discovery progress consumed CN=%d E=%v", st.qc.KP[0].CN, st.qc.KP[0].E)
					}
				case "emit":
					st.qc.Phase = "emit"
					st.qc.CM = []cursor.QueueCandidate{{K: "42:1", B: 1, U: "2026-10-03T11:00:00Z"}}
					_, err = st.runEmit(ctx)
					if st.qc.EI != 0 {
						t.Fatalf("EI consumed=%d", st.qc.EI)
					}
				case "seed":
					err = st.discoverOngoing(ctx)
					if st.qc.OG.SI != 0 || st.qc.OG.E {
						t.Fatalf("seed consumed SI=%d E=%v", st.qc.OG.SI, st.qc.OG.E)
					}
				}
				want := errQueueCancelled
				if errors.Is(cause, context.DeadlineExceeded) {
					want = igl.ErrBudgetElapsed
				}
				if !errors.Is(err, want) {
					t.Fatalf("got %v want %v", err, want)
				}
			})
		}
	}
}

func TestReviewQueue_R3SeedlessOngoingEmitResume(t *testing.T) {
	tr := &repairRT{two: true}
	d := repairDeps(t, tr, nil)
	args := map[string]any{"group_id": "9", "kinds": []any{"reviewer", "ongoing"}, "page_size": 1}
	seenEmit := false
	for n := 0; n < 8; n++ {
		out, err := callReviewQueue(t, d, args)
		if err != nil {
			t.Fatal(err)
		}
		sec := out["sections"].(map[string]any)["ongoing"].(map[string]any)
		if sec["pagination_exhausted"] != false {
			t.Fatalf("response %d exhausted seedless ongoing: %v", n+1, sec["pagination_exhausted"])
		}
		if sec["content_complete"] == readmeta.ContentCompleteTrue {
			t.Fatal("seedless ongoing claimed complete")
		}
		items, _ := out["items"].([]any)
		if len(items) > 0 && !hasCode(limitationCodes(sec), readmeta.CodeUnsupported) {
			t.Fatal("emit page dropped unsupported")
		}
		tok, _ := sectionMap(out)["next_cursor"].(string)
		if tok == "" {
			if !seenEmit {
				t.Fatal("fixture never resumed emission")
			}
			return
		}
		p, err := cursor.Decode(d.Config.CursorKey, tok, d.now())
		if err != nil {
			t.Fatal(err)
		}
		if p.QueueCont.Phase == "emit" {
			seenEmit = true
		}
		args["cursor"] = tok
	}
	t.Fatal("seedless resume did not finish")
}

func repairSystemNotes(start, count int) string {
	ns := make([]string, 0, count)
	for n := 0; n < count; n++ {
		ns = append(ns, fmt.Sprintf(`{"id":%d,"system":true,"body":"fixture","author":{"id":7}}`, start+n))
	}
	return strings.Join(ns, ",")
}

func TestReviewQueue_F6StrictProviderPages(t *testing.T) {
	for _, mode := range []string{"repeat", "jump", "dupheader"} {
		t.Run(mode, func(t *testing.T) {
			tr := &repairRT{mode: mode}
			d := repairDeps(t, tr, nil)
			out, err := callReviewQueue(t, d, map[string]any{"group_id": "9", "kinds": []any{"reviewer"}, "page_size": 1})
			if err != nil {
				t.Fatal(err)
			}
			if nc, _ := sectionMap(out)["next_cursor"].(string); nc != "" {
				t.Fatalf("bad paging minted a cursor after %d lists", tr.listHits)
			}
			items, _ := out["items"].([]any)
			if len(items) != 1 {
				t.Fatalf("valid entry dropped, items=%d", len(items))
			}
		})
	}
	t.Run("sequential_chain", func(t *testing.T) {
		tr := &repairRT{mode: "chain", chainPages: 2}
		d := repairDeps(t, tr, nil)
		out := drainQueue(t, d, map[string]any{"group_id": "9", "kinds": []any{"reviewer"}, "page_size": 20})
		items, _ := out["items"].([]any)
		if len(items) != 2 {
			t.Fatalf("sequential pages items=%d", len(items))
		}
	})
}

func TestReviewQueue_F7PerKindCountsAndCap(t *testing.T) {
	tr := &repairRT{}
	d := repairDeps(t, tr, nil)
	out, err := callReviewQueue(t, d, map[string]any{"group_id": "9", "kinds": []any{"reviewer", "ongoing"}})
	if err != nil {
		t.Fatal(err)
	}
	sections, ok := out["sections"].(map[string]any)
	if !ok || sections["reviewer"] == nil || sections["ongoing"] == nil {
		t.Fatalf("sections=%v", out["sections"])
	}
	rev := sections["reviewer"].(map[string]any)
	if _, ok := rev["counts"]; !ok || rev["consistency"] != readmeta.ConsistencyUnknown {
		t.Fatalf("per-kind section=%v", rev)
	}
	qc, ok := out["queue_counts"].(map[string]any)
	if !ok || qc["confirmed_candidates"] == nil || qc["known_terminal_omitted"] == nil {
		t.Fatalf("queue_counts=%v", out["queue_counts"])
	}

	sec := newReviewQueueSection(d.now())
	b := igl.DefaultBudget()
	st := emitStub(d, b, &sec)
	st.norm.pageSize = 1
	st.qc.Term = true
	st.qc.CM = append(st.qc.CM, cursor.QueueCandidate{K: "42:2", B: 1, U: "2026-10-03T10:00:00Z"})
	items, err := st.runEmit(igl.WithBudget(context.Background(), b))
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := st.finalize(items)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(terminal)
	if !strings.Contains(string(raw), "known_terminal_omitted") {
		t.Fatalf("missing omission accounting: %s", raw)
	}
	body := terminal["queue_counts"].(queueCountBody)
	if body.KnownTerminalOmitted < 1 || body.ReturnedItems < 1 {
		t.Fatalf("counts=%+v", body)
	}

	st.outputLimitBytes = 180
	st.qc.Term = true
	st.qc.EI = len(st.qc.CM)
	capped, err := st.finalize(items)
	if err != nil {
		t.Fatal(err)
	}
	craw, _ := json.Marshal(capped)
	if len(craw) > st.outputLimitBytes && len(capped["items"].([]reviewQueueItem)) > 0 {
		t.Fatalf("output cap not applied len=%d", len(craw))
	}
	if nc := capped["section"].(*readmeta.Section).NextCursor; nc != nil {
		t.Fatal("over-cap output kept a cursor")
	}
}

func TestReviewQueue_F8StrictSchema(t *testing.T) {
	n, err := normalizeReviewQueueInput(getMergeRequestReviewQueueIn{GroupID: "9", Kinds: []string{"reviewer"}, States: []string{"closed", "opened"}, PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	q := newQueueCont(n)
	q.KP = nil
	if validateQueueContProgress(q, n) == nil {
		t.Fatal("missing streams accepted")
	}
	q = newQueueCont(n)
	q.Phase = "emit"
	q.KI = len(q.KP)
	if validateQueueContProgress(q, n) == nil {
		t.Fatal("emit with unfinished discovery accepted")
	}
	if _, err = normalizeReviewQueueInput(getMergeRequestReviewQueueIn{GroupID: "9", Kinds: []string{"reviewer"}, PageSize: -1}); err == nil {
		t.Fatal("negative page_size defaulted")
	}
	if _, err = normalizeReviewQueueInput(getMergeRequestReviewQueueIn{GroupID: "9", Kinds: []string{"reviewer"}, PageSize: 0}); err != nil {
		t.Fatal("omitted page_size must stay valid")
	}
	n.pageSize = 1
	q = newQueueCont(n)
	for i := range q.KP {
		q.KP[i].E = true
	}
	q.KI = len(q.KP)
	q.Phase = "emit"
	p := repairPayload(q)
	key := []byte(queueTestCursorKey)
	raw, _ := json.Marshal(p)
	dup := append([]byte(`{"actor_id":7,`), raw[1:]...)
	mac := hmac.New(sha256.New, key)
	mac.Write(dup)
	tok := "v1." + base64.RawURLEncoding.EncodeToString(dup) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if _, err = cursor.Decode(key, tok, time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("duplicate JSON member accepted")
	}
	p.QueueCont.CM = []cursor.QueueCandidate{{K: "42:1", B: 1, U: "2026-10-03T11:00:00Z"}, {K: "042:1", B: 1, U: "2026-10-03T11:00:00Z"}}
	if _, err = cursor.Encode(key, p); err == nil {
		t.Fatal("noncanonical key accepted")
	}
	p.QueueCont.CM = nil
	p.QueueCont.Lim = []string{"arbitrary", "arbitrary"}
	if _, err = cursor.Encode(key, p); err == nil {
		t.Fatal("arbitrary duplicate limitation accepted")
	}
	tr := &repairRT{}
	d := repairDeps(t, tr, nil)
	before := tr.hits
	_, err = callReviewQueue(t, d, map[string]any{"group_id": "9", "kinds": []any{"reviewer"}, "cursor": "   "})
	if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) || tr.hits != before {
		t.Fatalf("padded cursor err=%v hits=%d", err, tr.hits)
	}
	q = newQueueCont(n)
	q.KP[0].N = -1
	if _, err = cursor.Encode(key, repairPayload(q)); err == nil {
		t.Fatal("negative next accepted")
	}
}

func TestReviewQueue_F9FractionalPinnedBound(t *testing.T) {
	before, err := time.Parse(time.RFC3339Nano, "2026-10-03T11:30:00.500Z")
	if err != nil {
		t.Fatal(err)
	}
	f := cursor.Filters{}
	pinUntil(&f, time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC), &before)
	got, err := time.Parse(time.RFC3339Nano, f.Until)
	if err != nil || !got.Equal(before) {
		t.Fatalf("caller bound got=%s err=%v", f.Until, err)
	}
	now := time.Date(2026, 10, 3, 12, 0, 0, 500000000, time.UTC)
	pinUntil(&f, now, nil)
	got, err = time.Parse(time.RFC3339Nano, f.Until)
	if err != nil || !got.Equal(now) {
		t.Fatalf("clock bound got=%s err=%v", f.Until, err)
	}
	after := before
	if _, err = normalizeReviewQueueInput(getMergeRequestReviewQueueIn{
		GroupID: "9", Kinds: []string{"reviewer"},
		UpdatedAfter: strPtr(after.Format(time.RFC3339Nano)), UpdatedBefore: strPtr(before.Format(time.RFC3339Nano)),
	}); err != nil {
		t.Fatalf("equal after/before rejected: %v", err)
	}

	tr := &repairRT{mode: "frac"}
	d := repairDeps(t, tr, nil)
	d.Clock = &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	out, err := callReviewQueue(t, d, map[string]any{
		"group_id": "9", "kinds": []any{"reviewer"},
		"updated_before": "2026-10-03T11:30:00.500Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	items, _ := out["items"].([]any)
	if len(items) != 1 || !strings.Contains(tr.pinned, "11:30:00.5") {
		t.Fatalf("in-window fractional MR dropped items=%d pinned=%s", len(items), tr.pinned)
	}
}

func strPtr(s string) *string { return &s }

func assertEmitBudget(t *testing.T, path string, cause error) {
	t.Helper()
	tr := &repairRT{budgetPath: path, budgetErr: cause}
	d := repairDeps(t, tr, nil)
	sec := newReviewQueueSection(d.now())
	b := igl.DefaultBudget()
	st := emitStub(d, b, &sec)
	_, err := st.runEmit(igl.WithBudget(context.Background(), b))
	if !errors.Is(err, cause) || st.qc.EI != 0 {
		t.Fatalf("cause=%s err=%v EI=%d", cause, err, st.qc.EI)
	}
}

func emitStub(d Deps, b *igl.Budget, sec *readmeta.Section) *queueRuntime {
	return &queueRuntime{
		d: d, budget: b, group: CanonicalGroup{ID: 9}, groupID: "9",
		authActor: 7, discoveryActor: 7,
		norm:    normalizedQueue{wantRev: true, kinds: []string{"reviewer"}, states: []string{"opened"}, pageSize: 20},
		filters: cursor.Filters{Until: "2026-10-03T12:00:00Z", PerPage: 20},
		section: sec, now: d.now(),
		qc: &cursor.QueueCont{V: "rq2", Phase: "emit", Kinds: []string{"reviewer"}, Term: false,
			CM: []cursor.QueueCandidate{{K: "42:1", B: 1, U: "2026-10-03T11:00:00Z"}}},
	}
}

func repairPayload(q *cursor.QueueCont) cursor.Payload {
	p := cursor.Payload{
		SchemaVersion: cursor.SchemaV1,
		Instance:      "http://fixture.invalid/api/v4",
		ActorID:       7,
		PolicyFP:      "fixture-policy",
		Tool:          cursor.ToolReviewQueue,
		Section:       cursor.SectionReviewQueue,
		Scope:         cursor.Scope{Kind: cursor.ScopeGroupQueue, GroupID: "9"},
		Filters:       cursor.Filters{Until: "2026-10-03T12:00:00Z", PerPage: 1, Order: "updated_at:desc|tie=project_id,iid"},
		UpperBound:    "2026-10-03T12:00:00Z",
		ExpiresAt:     "2026-10-03T14:00:00Z",
		QueueCont:     q,
	}
	return p
}

type repairRT struct {
	mode              string
	two, deniedOwner  bool
	fork, discuss     bool
	leading, ancestry bool
	notes, chainPages int
	leadingNotes      int
	perSeed           bool
	budgetPath        string
	budgetErr         error
	hits, listHits    int
	discussionHits    int
	mrHits            int
	pinned            string
}

func (tr *repairRT) RoundTrip(r *http.Request) (*http.Response, error) {
	tr.hits++
	if tr.budgetPath != "" && strings.Contains(r.URL.Path, tr.budgetPath) {
		return nil, tr.budgetErr
	}
	hdr := http.Header{"Content-Type": []string{"application/json"}}
	body := ""
	switch {
	case r.URL.Path == "/api/v4/user":
		body = `{"id":7}`
	case strings.HasPrefix(r.URL.Path, "/api/v4/groups/") && !strings.HasSuffix(r.URL.Path, "/merge_requests"):
		id := strings.TrimPrefix(r.URL.Path, "/api/v4/groups/")
		parent := "0"
		if tr.ancestry && id == "8" {
			parent = "9"
		}
		if id == "42" {
			id = "9"
		}
		if tr.mode == "ancestry_spoof" && id == "11" {
			id = "9"
		}
		body = fmt.Sprintf(`{"id":%s,"full_path":"fixture","parent_id":%s}`, id, parent)
	case r.URL.Path == "/api/v4/groups/9/merge_requests":
		if tr.mode == "http500" {
			return &http.Response{StatusCode: 500, Status: "500 Internal Server Error", Header: hdr, Body: io.NopCloser(strings.NewReader(`{"message":"fixture"}`)), Request: r}, nil
		}
		tr.listHits++
		tr.pinned = r.URL.Query().Get("updated_before")
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 0 {
			page = 1
		}
		hdr["X-Next-Page"] = []string{""}
		switch tr.mode {
		case "discovery":
			hdr["X-Next-Page"] = []string{strconv.Itoa(page + 1)}
		case "repeat":
			hdr["X-Next-Page"] = []string{"2"}
		case "jump":
			hdr["X-Next-Page"] = []string{"3"}
		case "dupheader":
			hdr["X-Next-Page"] = []string{"2", "3"}
		case "noheader":
			delete(hdr, "X-Next-Page")
		case "chain":
			if page < tr.chainPages {
				hdr["X-Next-Page"] = []string{strconv.Itoa(page + 1)}
			}
		case "prior":
			if page == 1 {
				hdr["X-Next-Page"] = []string{"2"}
			} else {
				delete(hdr, "X-Next-Page")
			}
		}
		pid := 42
		if tr.mode == "discovery" {
			pid = 99
		}
		updated := "2026-10-03T11:00:00Z"
		if tr.mode == "frac" {
			updated = "2026-10-03T11:30:00.250Z"
		}
		iid := 1
		if tr.mode == "chain" && page > 1 {
			iid = page
		}
		body = fmt.Sprintf(`[{"iid":%d,"project_id":%d,"updated_at":%q,"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, iid, pid, updated)
		if tr.two {
			pid2 := 42
			if tr.deniedOwner {
				pid2 = 99
			}
			body += fmt.Sprintf(`,{"iid":2,"project_id":%d,"updated_at":"2026-10-03T10:00:00Z","sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`, pid2)
		}
		if tr.mode == "prior" && page == 1 {
			body += `,{"iid":2,"project_id":42,"updated_at":"2026-10-03T10:00:00Z","sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`
		}
		if tr.mode == "truncated" {
			body += `,{"iid":`
		} else if tr.mode == "junk" {
			body += `] junk`
		} else {
			body += `]`
		}
	case strings.HasSuffix(r.URL.Path, "/discussions"):
		tr.discussionHits++
		hdr["X-Next-Page"] = []string{""}
		parts := strings.Split(r.URL.Path, "/")
		iid := ""
		if len(parts) >= 2 {
			iid = parts[len(parts)-2]
		}
		if tr.perSeed && iid == "1" {
			body = `[{"id":"seed1","notes":[` + repairSystemNotes(1, 8) + `]}]`
			break
		}
		count := tr.notes
		if count == 0 {
			count = 1
		}
		ns := make([]string, 0, count)
		for i := 1; i <= count; i++ {
			sys := true
			if i == count {
				sys = false
			}
			ns = append(ns, fmt.Sprintf(`{"id":%d,"system":%t,"body":"fixture note","author":{"id":7}}`, i, sys))
		}
		body = `[{"id":"d1","notes":[` + strings.Join(ns, ",") + `]}]`
		if tr.leading {
			leadCount := tr.leadingNotes
			if leadCount == 0 {
				leadCount = 1
			}
			body = `[{"id":"lead","notes":[` + repairSystemNotes(1, leadCount) + `]},` + body[1:]
		}
		switch tr.mode {
		case "disc_noheader":
			delete(hdr, "X-Next-Page")
		case "disc_duplicate":
			hdr["X-Next-Page"] = []string{"2", "3"}
		case "disc_jump":
			hdr["X-Next-Page"] = []string{"3"}
		case "disc_truncated":
			body = strings.TrimSuffix(body, "]") + `,{"id":`
		case "disc_junk":
			body += ` trailing-junk`
		}
	case strings.Contains(r.URL.Path, "/merge_requests/"):
		tr.mrHits++
		parts := strings.Split(r.URL.Path, "/")
		pid := parts[len(parts)-3]
		iid := parts[len(parts)-1]
		source := pid
		if tr.fork && iid == "2" {
			source = "99"
		}
		hour := "11"
		if iid == "2" {
			hour = "10"
		}
		updated := fmt.Sprintf("2026-10-03T%s:00:00Z", hour)
		if tr.mode == "frac" {
			updated = "2026-10-03T11:30:00.250Z"
		}
		state := "opened"
		updatedField := fmt.Sprintf(`,"updated_at":%q`, updated)
		if tr.mode == "seed_closed" {
			state = "closed"
		}
		if tr.mode == "seed_nodate" {
			updatedField = ""
		}
		body = fmt.Sprintf(`{"id":1,"iid":%s,"project_id":%s,"state":%q,"source_project_id":%s,"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"%s}`, iid, pid, state, source, updatedField)
	case strings.HasPrefix(r.URL.Path, "/api/v4/projects/"):
		if tr.mode == "seed_unknown" {
			return &http.Response{StatusCode: 404, Status: "404 Not Found", Header: hdr, Body: io.NopCloser(strings.NewReader(`{"message":"not found"}`)), Request: r}, nil
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/v4/projects/")
		if id == "fixture/p" {
			id = "42"
		}
		ns := "9"
		kind := "group"
		if tr.ancestry {
			ns = "8"
		}
		if tr.mode == "ancestry_spoof" {
			ns = "11"
		}
		if tr.mode == "user_ns" {
			kind = "user"
		}
		body = fmt.Sprintf(`{"id":%s,"path_with_namespace":"fixture/p","namespace":{"id":%s,"kind":%q}}`, id, ns, kind)
	default:
		return nil, fmt.Errorf("unexpected fake-only path %s", r.URL.Path)
	}
	return &http.Response{StatusCode: 200, Status: "200 OK", Header: hdr, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func repairDeps(t *testing.T, tr *repairRT, allowed []string) Deps {
	t.Helper()
	cfg := &config.Config{Token: "fixture-only", APIURL: "http://fixture.invalid/api/v4", CursorKey: []byte(queueTestCursorKey), AllowedGroupIDs: []string{"9"}, AllowedProjectIDs: allowed}
	cli, err := gitlab.NewClient("fixture-only", gitlab.WithBaseURL(cfg.APIURL), gitlab.WithoutRetries(), gitlab.WithHTTPClient(&http.Client{Transport: tr}), gitlab.WithInterceptor(igl.BudgetInterceptor()))
	if err != nil {
		t.Fatal(err)
	}
	return Deps{Config: cfg, Client: cli, Clock: &cursor.FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}}
}

func TestReviewQueue_seedRejectionsAndProviderFailure(t *testing.T) {
	seed := map[string]any{"group_id": "9", "kinds": []any{"ongoing"}, "known_mrs": []any{map[string]any{"project_id": "42", "iid": 1}}}
	for _, tc := range []struct {
		mode string
		code string
		args map[string]any
	}{
		{"seed_unknown", readmeta.CodeIdentityUnresolved, map[string]any{"group_id": "9", "kinds": []any{"ongoing"}, "known_mrs": []any{map[string]any{"project_id": "404", "iid": 1}}}},
		{"seed_closed", readmeta.CodeUnsupported, seed},
		{"seed_nodate", readmeta.CodePartial, seed},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			tr := &repairRT{mode: tc.mode}
			d := repairDeps(t, tr, nil)
			out, err := callReviewQueue(t, d, tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if items, _ := out["items"].([]any); len(items) != 0 || tr.discussionHits != 0 {
				t.Fatalf("rejected seed emitted items=%d discussions=%d", len(items), tr.discussionHits)
			}
			raw, _ := json.Marshal(out["section"])
			if !strings.Contains(string(raw), tc.code) {
				t.Fatalf("section missing %s: %s", tc.code, raw)
			}
		})
	}
	t.Run("spoofed_ancestry", func(t *testing.T) {
		tr := &repairRT{mode: "ancestry_spoof"}
		d := repairDeps(t, tr, []string{"42"})
		out, err := callReviewQueue(t, d, map[string]any{"group_id": "9", "kinds": []any{"reviewer"}, "page_size": 1})
		if err != nil {
			t.Fatal(err)
		}
		if items, _ := out["items"].([]any); len(items) != 0 {
			t.Fatalf("spoofed ancestry authorized items=%d", len(items))
		}
	})
	t.Run("provider_http", func(t *testing.T) {
		tr := &repairRT{mode: "http500"}
		d := repairDeps(t, tr, nil)
		out, err := callReviewQueue(t, d, map[string]any{"group_id": "9", "kinds": []any{"reviewer"}})
		if err != nil {
			t.Fatal(err)
		}
		if items, _ := out["items"].([]any); len(items) != 0 {
			t.Fatalf("http object body emitted items=%d", len(items))
		}
		raw, _ := json.Marshal(out)
		if !strings.Contains(string(raw), readmeta.CodeProviderPageAmbiguous) || strings.Contains(string(raw), "fixture") {
			t.Fatalf("500 object body: %s", raw)
		}
		if tok, _ := sectionMap(out)["next_cursor"].(string); tok != "" {
			t.Fatal("non-array provider page returned a cursor")
		}
	})
}

func TestQueueProviderFailClassification(t *testing.T) {
	d := repairDeps(t, &repairRT{}, nil)
	sec := newReviewQueueSection(d.now())
	st := emitStub(d, igl.DefaultBudget(), &sec)
	if st.handleProviderFail(nil) {
		t.Fatal("nil is not a provider failure")
	}
	if !st.handleProviderFail(fmt.Errorf("%s: paging metadata unavailable", readmeta.CodeProviderPageAmbiguous)) || !st.qc.Term {
		t.Fatal("ambiguous provider failure must terminalize")
	}
	sec = newReviewQueueSection(d.now())
	st = emitStub(d, igl.DefaultBudget(), &sec)
	if st.handleProviderFail(fmt.Errorf("gitlab: 500")) {
		t.Fatal("unclassified provider error must stay an http failure")
	}
	st.qc.KP = []cursor.QueueKindProg{{Kind: "reviewer", State: "opened", P: 1, PSz: 20}}
	st.filters.Until = "not-a-time"
	if err := st.runDiscover(context.Background()); err == nil || st.handleProviderFail(err) {
		t.Fatalf("corrupt pinned bound: %v", err)
	}
}

func TestReviewQueue_emitStoredDenialAndTinyCap(t *testing.T) {
	t.Run("user_namespace", func(t *testing.T) {
		tr := &repairRT{mode: "user_ns"}
		d := repairDeps(t, tr, nil)
		sec := newReviewQueueSection(d.now())
		b := igl.DefaultBudget()
		st := emitStub(d, b, &sec)
		items, err := st.runEmit(igl.WithBudget(context.Background(), b))
		if err != nil || len(items) != 0 || st.qc.EI != 1 {
			t.Fatalf("user namespace emit items=%d err=%v EI=%d", len(items), err, st.qc.EI)
		}
	})
	t.Run("missing_updated_at", func(t *testing.T) {
		tr := &repairRT{mode: "seed_nodate"}
		d := repairDeps(t, tr, nil)
		sec := newReviewQueueSection(d.now())
		b := igl.DefaultBudget()
		st := emitStub(d, b, &sec)
		items, err := st.runEmit(igl.WithBudget(context.Background(), b))
		if err != nil || len(items) != 0 || st.qc.EI != 1 {
			t.Fatalf("undated emit items=%d err=%v EI=%d", len(items), err, st.qc.EI)
		}
	})
	t.Run("cap_below_empty_document", func(t *testing.T) {
		tr := &repairRT{}
		d := repairDeps(t, tr, nil)
		sec := newReviewQueueSection(d.now())
		b := igl.DefaultBudget()
		st := emitStub(d, b, &sec)
		st.qc.Term = true
		st.qc.EI = 1
		st.outputLimitBytes = 1
		out, err := st.finalize([]reviewQueueItem{{ProjectID: 42, IID: 1, Kinds: []string{"reviewer"}}})
		if err != nil {
			t.Fatal(err)
		}
		if len(out["items"].([]reviewQueueItem)) != 0 || out["section"].(*readmeta.Section).NextCursor != nil {
			t.Fatalf("tiny cap kept items or a cursor: %#v", out["items"])
		}
	})
}

func TestObserveQueuePage_strictContract(t *testing.T) {
	hdr := func(vals ...string) http.Header {
		h := http.Header{}
		h["X-Next-Page"] = vals
		return h
	}
	if obs := observeQueuePage(1, hdr(""), 0, 1); !obs.exhausted || obs.ambiguous {
		t.Fatalf("exhaustion: %+v", obs)
	}
	if obs := observeQueuePage(1, hdr("2"), 2, 1); obs.ambiguous || obs.next != 2 {
		t.Fatalf("sequential: %+v", obs)
	}
	for _, tc := range []struct {
		name string
		obs  queuePageObs
	}{
		{"missing", observeQueuePage(1, http.Header{}, 0, 1)},
		{"duplicate", observeQueuePage(1, hdr("2", "3"), 2, 1)},
		{"padded", observeQueuePage(1, hdr(" 2"), 2, 1)},
		{"jump", observeQueuePage(1, hdr("3"), 3, 1)},
		{"repeat", observeQueuePage(2, hdr("2"), 2, 1)},
		{"empty_positive", observeQueuePage(1, hdr("2"), 2, 0)},
		{"blank_vs_sdk", observeQueuePage(1, hdr(""), 2, 1)},
		{"noncanonical", observeQueuePage(1, hdr("02"), 2, 1)},
	} {
		if !tc.obs.ambiguous || tc.obs.exhausted || tc.obs.next != 0 {
			t.Fatalf("%s accepted: %+v", tc.name, tc.obs)
		}
	}
}
