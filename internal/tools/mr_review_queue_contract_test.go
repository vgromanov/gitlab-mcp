package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"
)

func TestReviewQueue_resumeRejectsChangedBounds(t *testing.T) {
	var untilQ atomic.Value
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/user":
			_, _ = io.WriteString(w, `{"id":7}`)
		case r.URL.Path == "/api/v4/groups/9":
			_, _ = io.WriteString(w, `{"id":9,"full_path":"g","parent_id":0}`)
		case strings.HasPrefix(r.URL.Path, "/api/v4/groups/") && strings.HasSuffix(r.URL.Path, "/merge_requests"):
			untilQ.Store(r.URL.Query().Get("updated_before"))
			// Force discover continuation with a non-exhausted page needing resume.
			w.Header().Set("X-Next-Page", "2")
			_, _ = io.WriteString(w, `[{"iid":1,"project_id":42,"updated_at":"2026-10-03T11:00:00Z","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`)
		case strings.HasSuffix(r.URL.Path, "/merge_requests/1"):
			_, _ = io.WriteString(w, `{"id":1,"iid":1,"project_id":42,"state":"opened","source_project_id":42,"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","updated_at":"2026-10-03T11:00:00Z"}`)
		case r.URL.Path == "/api/v4/projects/42":
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":9,"kind":"group"}}`)
		default:
			http.NotFound(w, r)
		}
	})
	d := queueDeps(t, h, &config.Config{AllowedGroupIDs: []string{"9"}})
	before := "2026-10-03T11:30:00Z"
	out, err := callReviewQueue(t, d, map[string]any{
		"group_id": "9", "kinds": []any{"reviewer"}, "page_size": 1,
		"updated_before": before,
	})
	if err != nil {
		t.Fatal(err)
	}
	sec := sectionMap(out)
	nc, _ := sec["next_cursor"].(string)
	if nc == "" {
		t.Fatal("want continuation cursor")
	}
	pinned := untilQ.Load().(string)
	if pinned == "" {
		t.Fatal("want pinned updated_before on wire")
	}
	_, err = callReviewQueue(t, d, map[string]any{
		"group_id": "9", "kinds": []any{"reviewer"}, "page_size": 1,
		"updated_before": "2026-10-03T10:00:00Z", "cursor": nc,
	})
	if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
		t.Fatalf("changed updated_before must resync, got %v", err)
	}
	_, err = callReviewQueue(t, d, map[string]any{
		"group_id": "9", "kinds": []any{"reviewer"}, "page_size": 1,
		"updated_after": "2026-10-01T00:00:00Z", "updated_before": before, "cursor": nc,
	})
	if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
		t.Fatalf("changed updated_after must resync, got %v", err)
	}
}

func TestReviewQueue_pinUntilDefaultAndFutureAndResume(t *testing.T) {
	var seen []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/user":
			_, _ = io.WriteString(w, `{"id":7}`)
		case r.URL.Path == "/api/v4/groups/9":
			_, _ = io.WriteString(w, `{"id":9,"full_path":"g","parent_id":0}`)
		case strings.HasPrefix(r.URL.Path, "/api/v4/groups/") && strings.HasSuffix(r.URL.Path, "/merge_requests"):
			seen = append(seen, r.URL.Query().Get("updated_before"))
			w.Header().Set("X-Next-Page", "2")
			_, _ = io.WriteString(w, `[{"iid":1,"project_id":42,"updated_at":"2026-10-03T11:00:00Z","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`)
		case strings.HasSuffix(r.URL.Path, "/merge_requests/1"):
			_, _ = io.WriteString(w, `{"id":1,"iid":1,"project_id":42,"state":"opened","source_project_id":42,"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","updated_at":"2026-10-03T11:00:00Z"}`)
		case r.URL.Path == "/api/v4/projects/42":
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":9,"kind":"group"}}`)
		default:
			http.NotFound(w, r)
		}
	})
	d := queueDeps(t, h, &config.Config{AllowedGroupIDs: []string{"9"}})
	now := d.now().UTC().Format(time.RFC3339)

	out, err := callReviewQueue(t, d, map[string]any{"group_id": "9", "kinds": []any{"reviewer"}, "page_size": 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) < 1 || seen[0] != now {
		t.Fatalf("no caller_before => until=now want %q got %q", now, seen)
	}
	nc := sectionMap(out)["next_cursor"].(string)

	seen = nil
	future := "2099-01-01T00:00:00Z"
	d2 := queueDeps(t, h, &config.Config{AllowedGroupIDs: []string{"9"}})
	out2, err := callReviewQueue(t, d2, map[string]any{
		"group_id": "9", "kinds": []any{"reviewer"}, "page_size": 1, "updated_before": future,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) < 1 || seen[0] != d2.now().UTC().Format(time.RFC3339) {
		t.Fatalf("future before => min(now) got %q", seen)
	}
	_ = out2

	// Resume keeps pinned until (does not renew to a later clock).
	seen = nil
	clk := d.Clock.(*cursor.FakeClock)
	clk.Advance(time.Hour)
	_, err = callReviewQueue(t, d, map[string]any{
		"group_id": "9", "kinds": []any{"reviewer"}, "page_size": 1, "cursor": nc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) < 1 || seen[0] != now {
		t.Fatalf("resume must keep pinned until %q got %q", now, seen)
	}
}

func TestReviewQueue_ongoingTwoSeedsAndMissingSystem(t *testing.T) {
	var scanned [2]atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/user":
			_, _ = io.WriteString(w, `{"id":7}`)
		case r.URL.Path == "/api/v4/groups/9":
			_, _ = io.WriteString(w, `{"id":9,"full_path":"g","parent_id":0}`)
		case r.URL.Path == "/api/v4/projects/42" || r.URL.Path == "/api/v4/projects/43":
			id := strings.TrimPrefix(r.URL.Path, "/api/v4/projects/")
			_, _ = io.WriteString(w, `{"id":`+id+`,"path_with_namespace":"g/p`+id+`","namespace":{"id":9,"kind":"group"}}`)
		case strings.HasSuffix(r.URL.Path, "/merge_requests/5"):
			_, _ = io.WriteString(w, `{"id":1,"iid":5,"project_id":42,"state":"opened","source_project_id":42,"sha":"cccccccccccccccccccccccccccccccccccccccc","updated_at":"2026-10-03T10:00:00Z"}`)
		case strings.HasSuffix(r.URL.Path, "/merge_requests/6"):
			_, _ = io.WriteString(w, `{"id":2,"iid":6,"project_id":43,"state":"opened","source_project_id":43,"sha":"dddddddddddddddddddddddddddddddddddddddd","updated_at":"2026-10-03T09:00:00Z"}`)
		case strings.Contains(r.URL.Path, "/merge_requests/5/discussions"):
			scanned[0].Add(1)
			w.Header().Set("X-Next-Page", "")
			// First seed qualifies.
			_, _ = io.WriteString(w, `[{"id":"d1","notes":[{"id":1,"system":false,"body":"fix","author":{"id":7}}]}]`)
		case strings.Contains(r.URL.Path, "/merge_requests/6/discussions"):
			scanned[1].Add(1)
			w.Header().Set("X-Next-Page", "")
			// Missing system must not qualify; seed still scanned after first qualifies.
			_, _ = io.WriteString(w, `[{"id":"d2","notes":[{"id":2,"body":"no system","author":{"id":7}}]}]`)
		default:
			http.NotFound(w, r)
		}
	})
	d := queueDeps(t, h, &config.Config{AllowedGroupIDs: []string{"9"}})
	out := drainQueue(t, d, map[string]any{
		"group_id": "9", "kinds": []any{"ongoing"},
		"known_mrs": []any{
			map[string]any{"project_id": "42", "iid": 5},
			map[string]any{"project_id": "43", "iid": 6},
		},
	})
	if scanned[0].Load() < 1 || scanned[1].Load() < 1 {
		t.Fatalf("both seeds must be scanned after first qualifies: %d %d", scanned[0].Load(), scanned[1].Load())
	}
	items, _ := out["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("only first seed qualifies, got items=%v section=%v", items, out["section"])
	}
}

func TestReviewQueue_authRestrictedForkAndMissingProjectAndNilUpdated(t *testing.T) {
	t.Run("restricted_fork_source", func(t *testing.T) {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/api/v4/user":
				_, _ = io.WriteString(w, `{"id":7}`)
			case r.URL.Path == "/api/v4/groups/9":
				_, _ = io.WriteString(w, `{"id":9,"full_path":"g","parent_id":0}`)
			case strings.HasPrefix(r.URL.Path, "/api/v4/groups/") && strings.HasSuffix(r.URL.Path, "/merge_requests"):
				w.Header().Set("X-Next-Page", "")
				_, _ = io.WriteString(w, `[{"iid":1,"project_id":42,"updated_at":"2026-10-03T11:00:00Z","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`)
			case strings.HasSuffix(r.URL.Path, "/merge_requests/1"):
				_, _ = io.WriteString(w, `{"id":1,"iid":1,"project_id":42,"state":"opened","source_project_id":99,"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","updated_at":"2026-10-03T11:00:00Z"}`)
			case r.URL.Path == "/api/v4/projects/42":
				_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":9,"kind":"group"}}`)
			case r.URL.Path == "/api/v4/projects/99":
				http.NotFound(w, r)
			default:
				http.NotFound(w, r)
			}
		})
		d := queueDeps(t, h, &config.Config{AllowedGroupIDs: []string{"9"}, AllowedProjectIDs: []string{"42"}})
		out := drainQueue(t, d, map[string]any{"group_id": "9", "kinds": []any{"reviewer"}})
		items, _ := out["items"].([]any)
		if len(items) != 0 {
			t.Fatalf("restricted fork source must reject, got %v", items)
		}
		if !hasCode(limitationCodes(sectionMap(out)), readmeta.CodeIdentityUnresolved) &&
			!hasCode(limitationCodes(sectionMap(out)), readmeta.CodeAuthzDenied) {
			t.Fatalf("want authz/identity limitation: %v", sectionMap(out)["limitations"])
		}
	})

	t.Run("missing_project_id", func(t *testing.T) {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/api/v4/user":
				_, _ = io.WriteString(w, `{"id":7}`)
			case r.URL.Path == "/api/v4/groups/9":
				_, _ = io.WriteString(w, `{"id":9,"full_path":"g","parent_id":0}`)
			case strings.HasPrefix(r.URL.Path, "/api/v4/groups/") && strings.HasSuffix(r.URL.Path, "/merge_requests"):
				w.Header().Set("X-Next-Page", "")
				_, _ = io.WriteString(w, `[{"iid":1,"project_id":0,"updated_at":"2026-10-03T11:00:00Z","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`)
			default:
				http.NotFound(w, r)
			}
		})
		d := queueDeps(t, h, &config.Config{AllowedGroupIDs: []string{"9"}})
		out, err := callReviewQueue(t, d, map[string]any{"group_id": "9", "kinds": []any{"reviewer"}})
		// Malformed project_id<1 fails closed during stream decode or as incomplete.
		if err == nil {
			sec := sectionMap(out)
			if sec["content_complete"] == readmeta.ContentCompleteTrue {
				t.Fatal("must not complete on missing project_id")
			}
		}
	})

	t.Run("nil_updated_at_not_now", func(t *testing.T) {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/api/v4/user":
				_, _ = io.WriteString(w, `{"id":7}`)
			case r.URL.Path == "/api/v4/groups/9":
				_, _ = io.WriteString(w, `{"id":9,"full_path":"g","parent_id":0}`)
			case strings.HasPrefix(r.URL.Path, "/api/v4/groups/") && strings.HasSuffix(r.URL.Path, "/merge_requests"):
				w.Header().Set("X-Next-Page", "")
				_, _ = io.WriteString(w, `[{"iid":1,"project_id":42,"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`)
			case strings.HasSuffix(r.URL.Path, "/merge_requests/1"):
				// updated_at omitted
				_, _ = io.WriteString(w, `{"id":1,"iid":1,"project_id":42,"state":"opened","source_project_id":42,"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
			case r.URL.Path == "/api/v4/projects/42":
				_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":9,"kind":"group"}}`)
			default:
				http.NotFound(w, r)
			}
		})
		d := queueDeps(t, h, &config.Config{AllowedGroupIDs: []string{"9"}})
		out := drainQueue(t, d, map[string]any{"group_id": "9", "kinds": []any{"reviewer"}})
		items, _ := out["items"].([]any)
		if len(items) != 0 {
			t.Fatalf("nil updated_at must not fabricate now: %v", items)
		}
		if !hasCode(limitationCodes(sectionMap(out)), readmeta.CodePartial) {
			t.Fatalf("want date unknown partial: %v", sectionMap(out)["limitations"])
		}
	})

	t.Run("identity_mismatch", func(t *testing.T) {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/api/v4/user":
				_, _ = io.WriteString(w, `{"id":7}`)
			case r.URL.Path == "/api/v4/groups/9":
				_, _ = io.WriteString(w, `{"id":9,"full_path":"g","parent_id":0}`)
			case strings.HasPrefix(r.URL.Path, "/api/v4/groups/") && strings.HasSuffix(r.URL.Path, "/merge_requests"):
				w.Header().Set("X-Next-Page", "")
				_, _ = io.WriteString(w, `[{"iid":1,"project_id":42,"updated_at":"2026-10-03T11:00:00Z","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`)
			case strings.HasSuffix(r.URL.Path, "/merge_requests/1"):
				_, _ = io.WriteString(w, `{"id":1,"iid":2,"project_id":42,"state":"opened","source_project_id":42,"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","updated_at":"2026-10-03T11:00:00Z"}`)
			case r.URL.Path == "/api/v4/projects/42":
				_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":9,"kind":"group"}}`)
			default:
				http.NotFound(w, r)
			}
		})
		d := queueDeps(t, h, &config.Config{AllowedGroupIDs: []string{"9"}})
		out := drainQueue(t, d, map[string]any{"group_id": "9", "kinds": []any{"reviewer"}})
		if items, _ := out["items"].([]any); len(items) != 0 {
			t.Fatalf("iid mismatch must reject: %v", items)
		}
	})
}

func TestReviewQueue_movingUnknownAndAmbiguousNoCursor(t *testing.T) {
	t.Run("moving_unknown", func(t *testing.T) {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/api/v4/user":
				_, _ = io.WriteString(w, `{"id":7}`)
			case r.URL.Path == "/api/v4/groups/9":
				_, _ = io.WriteString(w, `{"id":9,"full_path":"g","parent_id":0}`)
			case strings.HasPrefix(r.URL.Path, "/api/v4/groups/") && strings.HasSuffix(r.URL.Path, "/merge_requests"):
				w.Header().Set("X-Next-Page", "")
				_, _ = io.WriteString(w, `[{"iid":1,"project_id":42,"updated_at":"2026-10-03T11:00:00Z","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`)
			case strings.HasSuffix(r.URL.Path, "/merge_requests/1"):
				_, _ = io.WriteString(w, `{"id":1,"iid":1,"project_id":42,"state":"opened","source_project_id":42,"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","updated_at":"2026-10-03T11:00:00Z"}`)
			case r.URL.Path == "/api/v4/projects/42":
				_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":9,"kind":"group"}}`)
			default:
				http.NotFound(w, r)
			}
		})
		d := queueDeps(t, h, &config.Config{AllowedGroupIDs: []string{"9"}})
		out := drainQueue(t, d, map[string]any{"group_id": "9", "kinds": []any{"reviewer"}})
		sec := sectionMap(out)
		if sec["consistency"] != readmeta.ConsistencyUnknown {
			t.Fatalf("%v", sec["consistency"])
		}
		if sec["content_complete"] == readmeta.ContentCompleteTrue {
			t.Fatal("moving queue must not claim content_complete true")
		}
		if sec["pagination_exhausted"] == true {
			t.Fatal("must not infer exhausted=>complete")
		}
		if !hasCode(limitationCodes(sec), readmeta.CodeInconsistent) {
			t.Fatalf("want moving limitation")
		}
	})

	t.Run("ambiguous_small_page_no_header", func(t *testing.T) {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/api/v4/user":
				_, _ = io.WriteString(w, `{"id":7}`)
			case r.URL.Path == "/api/v4/groups/9":
				_, _ = io.WriteString(w, `{"id":9,"full_path":"g","parent_id":0}`)
			case strings.HasPrefix(r.URL.Path, "/api/v4/groups/") && strings.HasSuffix(r.URL.Path, "/merge_requests"):
				// No X-Next-Page header: must NOT treat small page as exhausted.
				_, _ = io.WriteString(w, `[{"iid":1,"project_id":42,"updated_at":"2026-10-03T11:00:00Z","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`)
			case strings.HasSuffix(r.URL.Path, "/merge_requests/1"):
				_, _ = io.WriteString(w, `{"id":1,"iid":1,"project_id":42,"state":"opened","source_project_id":42,"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","updated_at":"2026-10-03T11:00:00Z"}`)
			case r.URL.Path == "/api/v4/projects/42":
				_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":9,"kind":"group"}}`)
			default:
				http.NotFound(w, r)
			}
		})
		d := queueDeps(t, h, &config.Config{AllowedGroupIDs: []string{"9"}})
		out, err := callReviewQueue(t, d, map[string]any{"group_id": "9", "kinds": []any{"reviewer"}, "page_size": 20})
		if err != nil {
			t.Fatal(err)
		}
		sec := sectionMap(out)
		if sec["next_cursor"] != nil && sec["next_cursor"] != "" {
			t.Fatalf("ambiguous page must not mint next cursor: %v", sec["next_cursor"])
		}
		if !hasCode(limitationCodes(sec), readmeta.CodeProviderPageAmbiguous) {
			t.Fatalf("want ambiguous: %v", sec["limitations"])
		}
		if sec["content_complete"] != readmeta.ContentCompleteFalse {
			t.Fatalf("%v", sec["content_complete"])
		}
	})
}

func TestReviewQueue_emitBudgetPreservesEI(t *testing.T) {
	var getMR2 atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/user":
			_, _ = io.WriteString(w, `{"id":7}`)
		case r.URL.Path == "/api/v4/groups/9":
			_, _ = io.WriteString(w, `{"id":9,"full_path":"g","parent_id":0}`)
		case strings.HasPrefix(r.URL.Path, "/api/v4/groups/") && strings.HasSuffix(r.URL.Path, "/merge_requests"):
			w.Header().Set("X-Next-Page", "")
			_, _ = io.WriteString(w, `[
				{"iid":1,"project_id":42,"updated_at":"2026-10-03T11:00:00Z","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				{"iid":2,"project_id":42,"updated_at":"2026-10-03T10:00:00Z","sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
			]`)
		case strings.HasSuffix(r.URL.Path, "/merge_requests/1"):
			_, _ = io.WriteString(w, `{"id":1,"iid":1,"project_id":42,"state":"opened","source_project_id":42,"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","updated_at":"2026-10-03T11:00:00Z"}`)
		case strings.HasSuffix(r.URL.Path, "/merge_requests/2"):
			getMR2.Add(1)
			_, _ = io.WriteString(w, `{"id":2,"iid":2,"project_id":42,"state":"opened","source_project_id":42,"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","updated_at":"2026-10-03T10:00:00Z"}`)
		case r.URL.Path == "/api/v4/projects/42":
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":9,"kind":"group"}}`)
		default:
			http.NotFound(w, r)
		}
	})
	d := queueDeps(t, h, &config.Config{AllowedGroupIDs: []string{"9"}})

	// Unit-level: emit budget stop must keep EI on the unprocessed candidate.
	st := &queueRuntime{
		d: d, budget: &igl.Budget{MaxItems: 1, MaxBytes: 1 << 20, MaxRequests: 16, MaxElapsed: time.Minute},
		group: CanonicalGroup{ID: 9}, groupID: "9",
		authActor: 7, discoveryActor: 7,
		norm:    normalizedQueue{wantRev: true, kinds: []string{"reviewer"}, states: []string{"opened"}, pageSize: 20},
		filters: cursor.Filters{Until: "2026-10-03T12:00:00Z", PerPage: 20},
		section: &readmeta.Section{Limitations: []readmeta.Limitation{}},
		qc: &cursor.QueueCont{
			V: cursor.QueueContSchemaRQ2, Phase: "emit", Kinds: []string{"reviewer"},
			CM: []cursor.QueueCandidate{
				{K: "42:1", B: queueBitReviewer, U: "2026-10-03T11:00:00Z"},
				{K: "42:2", B: queueBitReviewer, U: "2026-10-03T10:00:00Z"},
			},
			EI: 0,
		},
		now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
	}
	ctx := igl.WithBudget(context.Background(), st.budget)
	_, err := st.runEmit(ctx)
	if !isTypedBudget(err) && !errors.Is(err, igl.ErrBudgetItems) {
		// First verifyMR charges AddItem then may succeed; second hits budget.
		if err == nil && st.qc.EI > 1 {
			t.Fatalf("EI advanced past budget stop: ei=%d", st.qc.EI)
		}
	}
	// Critical: EI must remain at the unprocessed candidate index (0 or 1), never skip past it.
	if st.qc.EI > 1 {
		t.Fatalf("budget must preserve EI, got %d", st.qc.EI)
	}
}

func TestReviewQueue_sanitizerNoProviderEcho(t *testing.T) {
	err := sanitizeQueueErr(io.EOF)
	if strings.Contains(err.Error(), "EOF") {
		t.Fatalf("echoed: %v", err)
	}
	raw := identityErr("super secret upstream body https://evil/token")
	got := sanitizeQueueErr(raw)
	if strings.Contains(got.Error(), "secret") || strings.Contains(got.Error(), "evil") || strings.Contains(got.Error(), "token") {
		t.Fatalf("echoed detail: %v", got)
	}
	if !strings.HasPrefix(got.Error(), readmeta.CodeIdentityUnresolved) {
		t.Fatalf("want identity code, got %v", got)
	}
	cases := []struct {
		in   error
		want string
	}{
		{nil, ""},
		{fmt.Errorf("%s: x", readmeta.CodeAuthzDenied), readmeta.CodeAuthzDenied},
		{fmt.Errorf("%s: x", readmeta.CodeUnsupported), readmeta.CodeUnsupported},
		{fmt.Errorf("%s", readmeta.CodeBudgetItems), readmeta.CodeBudgetItems},
		{fmt.Errorf("%s", readmeta.CodeBudgetBytes), readmeta.CodeBudgetBytes},
		{fmt.Errorf("%s", readmeta.CodeBudgetRequests), readmeta.CodeBudgetRequests},
		{fmt.Errorf("%s", readmeta.CodeBudgetElapsed), readmeta.CodeBudgetElapsed},
		{fmt.Errorf("%s: y", cursor.ResyncRequired), cursor.ResyncRequired},
		{fmt.Errorf("%s: z", readmeta.CodeProviderPageAmbiguous), readmeta.CodeProviderPageAmbiguous},
		{fmt.Errorf("%s: http body", readmeta.CodeHTTPError), readmeta.CodeHTTPError},
		{fmt.Errorf("%s missing", errCursorKeyMissingQueue), "GITLAB_MCP_CURSOR_KEY"},
	}
	for _, tc := range cases {
		out := sanitizeQueueErr(tc.in)
		if tc.in == nil {
			if out != nil {
				t.Fatalf("nil in => nil out, got %v", out)
			}
			continue
		}
		if !strings.Contains(out.Error(), tc.want) {
			t.Fatalf("in=%v got=%v want contains %q", tc.in, out, tc.want)
		}
		if strings.Contains(out.Error(), "http body") || strings.Contains(out.Error(), "provider leaked") {
			t.Fatalf("echoed: %v", out)
		}
	}
}

func TestReviewQueue_projectIDsScopeFilter(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/user":
			_, _ = io.WriteString(w, `{"id":7}`)
		case r.URL.Path == "/api/v4/groups/9":
			_, _ = io.WriteString(w, `{"id":9,"full_path":"g","parent_id":0}`)
		case strings.HasPrefix(r.URL.Path, "/api/v4/groups/") && strings.HasSuffix(r.URL.Path, "/merge_requests"):
			w.Header().Set("X-Next-Page", "")
			_, _ = io.WriteString(w, `[`+
				`{"iid":1,"project_id":42,"updated_at":"2026-10-03T11:00:00Z","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},`+
				`{"iid":2,"project_id":99,"updated_at":"2026-10-03T10:00:00Z","sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`+
				`]`)
		case strings.HasSuffix(r.URL.Path, "/merge_requests/1"):
			_, _ = io.WriteString(w, `{"id":1,"iid":1,"project_id":42,"state":"opened","source_project_id":42,"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","updated_at":"2026-10-03T11:00:00Z"}`)
		case r.URL.Path == "/api/v4/projects/42":
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":9,"kind":"group"}}`)
		case r.URL.Path == "/api/v4/projects/99":
			_, _ = io.WriteString(w, `{"id":99,"path_with_namespace":"other/p","namespace":{"id":8,"kind":"group"}}`)
		default:
			http.NotFound(w, r)
		}
	})
	d := queueDeps(t, h, &config.Config{AllowedGroupIDs: []string{"9"}, AllowedProjectIDs: []string{"42", "99"}})
	out := drainQueue(t, d, map[string]any{
		"group_id": "9", "kinds": []any{"reviewer"}, "project_ids": []any{"42"},
	})
	items, _ := out["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("scoped project_ids must keep only 42, got %v", items)
	}
}

func TestNormalizeReviewQueueInput_edges(t *testing.T) {
	_, err := normalizeReviewQueueInput(getMergeRequestReviewQueueIn{})
	if err == nil {
		t.Fatal("group_id required")
	}
	neg := int64(-1)
	_, err = normalizeReviewQueueInput(getMergeRequestReviewQueueIn{GroupID: "9", Kinds: []string{"reviewer"}, ActorID: &neg})
	if err == nil {
		t.Fatal("actor_id must be positive")
	}
	_, err = normalizeReviewQueueInput(getMergeRequestReviewQueueIn{GroupID: "9", Kinds: []string{"nope"}})
	if err == nil {
		t.Fatal("bad kind")
	}
	_, err = normalizeReviewQueueInput(getMergeRequestReviewQueueIn{GroupID: "9", Kinds: []string{"reviewer"}, States: []string{"all", "opened"}})
	if err == nil {
		t.Fatal("all cannot combine")
	}
	_, err = normalizeReviewQueueInput(getMergeRequestReviewQueueIn{GroupID: "9", Kinds: []string{"reviewer"}, PageSize: 99})
	if err == nil {
		t.Fatal("page_size cap")
	}
	after := "2026-10-04T00:00:00Z"
	before := "2026-10-03T00:00:00Z"
	_, err = normalizeReviewQueueInput(getMergeRequestReviewQueueIn{GroupID: "9", Kinds: []string{"reviewer"}, UpdatedAfter: &after, UpdatedBefore: &before})
	if err == nil {
		t.Fatal("after>before")
	}
}

func TestReviewQueue_reapplyLimitationsAcrossResume(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/user":
			_, _ = io.WriteString(w, `{"id":7}`)
		case r.URL.Path == "/api/v4/groups/9":
			_, _ = io.WriteString(w, `{"id":9,"full_path":"g","parent_id":0}`)
		case strings.HasPrefix(r.URL.Path, "/api/v4/groups/") && strings.HasSuffix(r.URL.Path, "/merge_requests"):
			// Ambiguous: small page, no next header.
			_, _ = io.WriteString(w, `[{"iid":1,"project_id":42,"updated_at":"2026-10-03T11:00:00Z","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`)
		case strings.HasSuffix(r.URL.Path, "/merge_requests/1"):
			_, _ = io.WriteString(w, `{"id":1,"iid":1,"project_id":42,"state":"opened","source_project_id":42,"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","updated_at":"2026-10-03T11:00:00Z"}`)
		case r.URL.Path == "/api/v4/projects/42":
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":9,"kind":"group"}}`)
		default:
			http.NotFound(w, r)
		}
	})
	d := queueDeps(t, h, &config.Config{AllowedGroupIDs: []string{"9"}})
	out, err := callReviewQueue(t, d, map[string]any{"group_id": "9", "kinds": []any{"reviewer"}})
	if err != nil {
		t.Fatal(err)
	}
	sec := sectionMap(out)
	if !hasCode(limitationCodes(sec), readmeta.CodeProviderPageAmbiguous) {
		t.Fatalf("want ambiguous: %v", sec["limitations"])
	}
	if nc, _ := sec["next_cursor"].(string); nc != "" {
		t.Fatal("ambiguous must not mint cursor")
	}
}

func TestReviewQueue_codecRejectsContradictoryPageAndBits(t *testing.T) {
	key := []byte(queueTestCursorKey)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	p := cursor.Payload{
		SchemaVersion: cursor.SchemaV1,
		Instance:      "https://gitlab.example/api/v4",
		ActorID:       7,
		PolicyFP:      "fp",
		Tool:          cursor.ToolReviewQueue,
		Section:       cursor.SectionReviewQueue,
		Scope:         cursor.Scope{Kind: cursor.ScopeGroupQueue, GroupID: "9"},
		Filters: cursor.Filters{
			Until: "2026-10-03T12:00:00Z", Order: "updated_at:desc|tie=project_id,iid",
			Selection: "kinds=reviewer|actor=7|states=closed,merged,opened|projects=*|seeds=none",
			PerPage:   20,
		},
		UpperBound: "2026-10-03T12:00:00Z",
		ExpiresAt:  now.Add(cursor.DefaultTTL).Format(time.RFC3339),
		PageState:  cursor.PageState{Page: 1}, // contradictory legacy page fields
		QueueCont: &cursor.QueueCont{
			V: cursor.QueueContSchemaRQ2, Phase: "discover", Kinds: []string{"reviewer"},
			KP: []cursor.QueueKindProg{{Kind: "reviewer", State: "opened", P: 1, PSz: 20}},
		},
	}
	if _, err := cursor.Encode(key, p); err == nil {
		t.Fatal("contradictory page_state must reject")
	}
	p.PageState = cursor.PageState{}
	p.QueueCont.CM = []cursor.QueueCandidate{{
		K: "42:1", B: queueBitAuthored, // authored bit not requested
		U: "2026-10-03T11:00:00Z",
	}}
	if _, err := cursor.Encode(key, p); err == nil {
		t.Fatal("bits outside requested kinds must reject")
	}
}

func TestReviewQueue_mcpBackendCounts(t *testing.T) {
	var lists, discussions, gets atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/user":
			_, _ = io.WriteString(w, `{"id":7}`)
		case r.URL.Path == "/api/v4/groups/9":
			_, _ = io.WriteString(w, `{"id":9,"full_path":"g","parent_id":0}`)
		case strings.HasPrefix(r.URL.Path, "/api/v4/groups/") && strings.HasSuffix(r.URL.Path, "/merge_requests"):
			lists.Add(1)
			w.Header().Set("X-Next-Page", "")
			_, _ = io.WriteString(w, `[{"iid":1,"project_id":42,"updated_at":"2026-10-03T11:00:00Z","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`)
		case strings.HasSuffix(r.URL.Path, "/merge_requests/1") && !strings.Contains(r.URL.Path, "discussions"):
			gets.Add(1)
			_, _ = io.WriteString(w, `{"id":1,"iid":1,"project_id":42,"state":"opened","source_project_id":42,"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","updated_at":"2026-10-03T11:00:00Z"}`)
		case strings.Contains(r.URL.Path, "/discussions"):
			discussions.Add(1)
			w.Header().Set("X-Next-Page", "")
			_, _ = io.WriteString(w, `[]`)
		case r.URL.Path == "/api/v4/projects/42":
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":9,"kind":"group"}}`)
		default:
			http.NotFound(w, r)
		}
	})
	d := queueDeps(t, h, &config.Config{AllowedGroupIDs: []string{"9"}})
	_ = drainQueue(t, d, map[string]any{"group_id": "9", "kinds": []any{"reviewer"}})
	if lists.Load() < 1 {
		t.Fatalf("ListGroupMergeRequests hits=%d", lists.Load())
	}
	if gets.Load() < 1 {
		t.Fatalf("GetMergeRequest hits=%d", gets.Load())
	}
	raw, _ := json.Marshal(map[string]int64{"lists": int64(lists.Load()), "gets": int64(gets.Load()), "discussions": int64(discussions.Load())})
	t.Logf("backend counts %s", raw)
}

func TestReviewQueue_capacityTerminalNoCursor(t *testing.T) {
	// 65 unique keys via two pages (50+15) must stop scanning, preserve partial
	// successes, set dedupe_capacity, and mint no next_cursor.
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/user":
			_, _ = io.WriteString(w, `{"id":7}`)
		case r.URL.Path == "/api/v4/groups/9":
			_, _ = io.WriteString(w, `{"id":9,"full_path":"g","parent_id":0}`)
		case strings.HasPrefix(r.URL.Path, "/api/v4/groups/") && strings.HasSuffix(r.URL.Path, "/merge_requests"):
			page := r.URL.Query().Get("page")
			var b strings.Builder
			b.WriteByte('[')
			n, start := 50, 1
			if page == "2" {
				n, start = 15, 51
			}
			for i := 0; i < n; i++ {
				if i > 0 {
					b.WriteByte(',')
				}
				iid := start + i
				fmt.Fprintf(&b, `{"iid":%d,"project_id":42,"updated_at":"2026-10-03T11:%02d:00Z","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, iid, iid%60)
			}
			b.WriteByte(']')
			if page == "" || page == "1" {
				w.Header().Set("X-Next-Page", "2")
			} else {
				w.Header().Set("X-Next-Page", "")
			}
			_, _ = io.WriteString(w, b.String())
		case strings.Contains(r.URL.Path, "/merge_requests/") && !strings.Contains(r.URL.Path, "discussions"):
			parts := strings.Split(r.URL.Path, "/")
			iid := parts[len(parts)-1]
			_, _ = io.WriteString(w, `{"id":1,"iid":`+iid+`,"project_id":42,"state":"opened","source_project_id":42,"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","updated_at":"2026-10-03T11:00:00Z"}`)
		case r.URL.Path == "/api/v4/projects/42":
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":9,"kind":"group"}}`)
		default:
			http.NotFound(w, r)
		}
	})
	d := queueDeps(t, h, &config.Config{AllowedGroupIDs: []string{"9"}})
	out, err := callReviewQueue(t, d, map[string]any{
		"group_id": "9", "kinds": []any{"reviewer"}, "page_size": 50, "states": []any{"opened"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sec := sectionMap(out)
	if nc, _ := sec["next_cursor"].(string); nc != "" {
		t.Fatalf("capacity must be terminal with null cursor, got %q", nc)
	}
	if !hasCode(limitationCodes(sec), readmeta.CodeDedupeCapacity) {
		t.Fatalf("want dedupe_capacity: %v", sec["limitations"])
	}
	items, _ := out["items"].([]any)
	if len(items) == 0 {
		t.Fatal("partial successes must survive capacity stop")
	}
	if sec["content_complete"] == readmeta.ContentCompleteTrue {
		t.Fatal("capacity must not claim content_complete")
	}
}
