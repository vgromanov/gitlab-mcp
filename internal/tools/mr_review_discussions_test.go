package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"
)

func discSHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func noteObj(id int, body string, extra string) string {
	if extra != "" {
		return fmt.Sprintf(`{"id":%d,"system":false,"author":{"id":9},"body":%q,%s}`, id, body, extra)
	}
	return fmt.Sprintf(`{"id":%d,"system":false,"author":{"id":9},"body":%q}`, id, body)
}

func discObj(id, notes string) string {
	return fmt.Sprintf(`{"id":%q,"individual_note":false,"notes":[%s]}`, id, notes)
}

func discPages(pages map[int]string, next map[int]string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("per_page") != "20" {
			http.Error(w, "per_page", http.StatusBadRequest)
			return
		}
		page := 1
		if raw := r.URL.Query().Get("page"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				http.Error(w, "page", http.StatusBadRequest)
				return
			}
			page = n
		}
		body, ok := pages[page]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-Next-Page", next[page])
		_, _ = io.WriteString(w, body)
	}
}

func TestDiscussionDigestGolden(t *testing.T) {
	emptySem := discSHA(`{"v":"discussions.semantic_feedback.v1","records":[]}`)
	emptyPos := discSHA(`{"v":"discussions.position.v1","records":[]}`)
	emptyFull := discSHA(`{"v":"discussions.full_revision.v1","records":[]}`)
	sem, pos, full, ok := discussionDigestDocuments([]byte(`[]`))
	if !ok || sem != emptySem || pos != emptyPos || full != emptyFull {
		t.Fatalf("empty digests ok=%v sem=%s pos=%s full=%s", ok, sem, pos, full)
	}
	a := []byte("[" + discObj("b", noteObj(2, "hello", "")) + "," + discObj("a", noteObj(10, "x", "")) + "]")
	b := []byte("[" + discObj("a", noteObj(10, "x", "")) + "," + discObj("b", noteObj(2, "hello", "")) + "]")
	as, ap, af, aok := discussionDigestDocuments(a)
	bs, bp, bf, bok := discussionDigestDocuments(b)
	if !aok || !bok || as != bs || ap != bp || af != bf {
		t.Fatalf("reorder sem %s/%s pos %s/%s full %s/%s", as, bs, ap, bp, af, bf)
	}
	base := []byte("[" + discObj("a", noteObj(2, "hello", "")) + "]")
	s0, p0, f0, ok0 := discussionDigestDocuments(base)
	if !ok0 {
		t.Fatal("baseline")
	}
	body := []byte("[" + discObj("a", noteObj(2, "edited", "")) + "]")
	bs, bp, bf, bok = discussionDigestDocuments(body)
	if !bok || bs == s0 || bf == f0 || bp != p0 {
		t.Fatalf("body edit sem=%s/%s pos=%s/%s full=%s/%s", s0, bs, p0, bp, f0, bf)
	}
	resolved := []byte("[" + discObj("a", noteObj(2, "hello", `"resolved":true`)) + "]")
	s1, p1, f1, ok1 := discussionDigestDocuments(resolved)
	if !ok0 || !ok1 || s1 == s0 || f1 == f0 || p1 != p0 {
		t.Fatalf("resolution sem %s/%s pos %s/%s full %s/%s", s0, s1, p0, p1, f0, f1)
	}
	deleted := []byte("[" + discObj("a", "") + "]")
	s2, _, f2, ok2 := discussionDigestDocuments(deleted)
	if !ok2 || s2 == s0 || f2 == f0 {
		t.Fatalf("deletion sem %s/%s full %s/%s", s0, s2, f0, f2)
	}
	moved := []byte("[" + discObj("a", noteObj(2, "hello", `"updated_at":"2026-10-04T00:00:00Z","position":{"base_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","start_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","head_sha":"cccccccccccccccccccccccccccccccccccccccc","position_type":"text","new_path":"f.go","new_line":1}`)) + "]")
	s3, p3, f3, ok3 := discussionDigestDocuments(moved)
	if !ok3 || s3 != s0 || p3 == p0 || f3 == f0 {
		t.Fatalf("position remap sem %s/%s pos %s/%s full %s/%s", s0, s3, p0, p3, f0, f3)
	}
	if _, _, _, bad := discussionDigestDocuments([]byte(`[{"id":"a","individual_note":false,"notes":[{"id":1,"id":2,"system":false,"author":{"id":9},"body":"a"}]}]`)); bad {
		t.Fatal("duplicate key minted a digest")
	}
	if _, _, _, bad := discussionDigestDocuments([]byte(`[{"id":"a","individual_note":false,"notes":[{"id":1,"system":false,"author":{"id":9},"body":"a","resolvable":1}]}]`)); bad {
		t.Fatal("wrong-typed resolvable minted a digest")
	}
	if _, _, _, bad := discussionDigestDocuments([]byte(`[{"id":"a","notes":[{"id":1,"system":false,"author":null,"body":"a"}]}]`)); bad {
		t.Fatal("null author minted a digest")
	}
}

func TestDiscussionNestedNoteSurvivesTruncation(t *testing.T) {
	body := `[{"id":"D","individual_note":false,"notes":[{"id":1,"system":false,"author":{"id":9},"body":"kept"},{"id":2,"system":false,"author":{"id":9},"body":"cut"`
	dec := decodeDiscussionPage(strings.NewReader(body), nil, "", "", "all", &discScanState{seenDisc: map[string]struct{}{}, seenNote: map[string]struct{}{}}, func() error { return nil })
	if dec.closed || len(dec.notes) != 1 || dec.notes[0].Body == nil || *dec.notes[0].Body != "kept" {
		t.Fatalf("truncated discussion lost the complete note: %+v terminal=%s", dec.notes, dec.terminal)
	}
	if dec.notes[0].NoteID == nil || *dec.notes[0].NoteID != "1" {
		t.Fatalf("note id %+v", dec.notes[0].NoteID)
	}
}

func TestDiscussionReviewContext(t *testing.T) {
	t.Run("two pages all bundle", func(t *testing.T) {
		log := &pathLog{}
		script := &reviewScript{log: log, discussions: discPages(map[int]string{
			1: "[" + discObj("d1", noteObj(1, "one", "")) + "]",
			2: "[" + discObj("d2", noteObj(2, "two", "")) + "]",
		}, map[int]string{1: "2", 2: ""})}
		d := newReviewDeps(t, http.HandlerFunc(script.serve))
		item := metaItem("42", 1, "metadata", "discussions")
		item.DiscussionSelection = "all"
		out, err := callReviewDirect(t, d, context.Background(), []reviewContextItemIn{item})
		if err != nil {
			t.Fatal(err)
		}
		got := out.Items[0]
		if got.ContextRef == nil || got.Discussions == nil || got.Discussions.FullRevisionDigest == nil || got.Sections["discussions"].ContentComplete != readmeta.ContentCompleteTrue {
			t.Fatalf("bundle shape ref=%v disc=%+v sec=%+v", got.ContextRef != nil, got.Discussions, got.Sections["discussions"])
		}
		if got.Discussions.Returned != "all" || got.Discussions.Notes == nil || len(*got.Discussions.Notes) != 2 {
			t.Fatalf("notes %#v", got.Discussions)
		}
		if log.count("/discussions") != 2 {
			t.Fatalf("discussion gets %d", log.count("/discussions"))
		}
		sem, pos, full, ok := discussionDigestDocuments([]byte("[" + discObj("d1", noteObj(1, "one", "")) + "," + discObj("d2", noteObj(2, "two", "")) + "]"))
		if !ok || *got.Discussions.SemanticFeedbackDigest != sem || *got.Discussions.PositionDigest != pos || *got.Discussions.FullRevisionDigest != full {
			t.Fatalf("digest mismatch ok=%v", ok)
		}
		ev, err := bundleHex(sem, pos, full)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := cursor.Decode(d.Config.CursorKey, *got.ContextRef, time.Date(2026, 10, 4, 12, 30, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		if payload.ContextRef.Digests["discussions"] != ev || len(payload.ContextRef.Digests) != len(payload.ContextRef.Complete) {
			t.Fatalf("bundle digests %#v complete %#v", payload.ContextRef.Digests, payload.ContextRef.Complete)
		}
	})

	t.Run("bad header publishes nothing", func(t *testing.T) {
		log := &pathLog{}
		script := &reviewScript{log: log, discussions: func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "["+discObj("d1", noteObj(1, "hidden", ""))+"]")
		}}
		d := newReviewDeps(t, http.HandlerFunc(script.serve))
		item := metaItem("42", 1, "metadata", "discussions")
		item.DiscussionSelection = "all"
		out, err := callReviewDirect(t, d, context.Background(), []reviewContextItemIn{item})
		if err != nil {
			t.Fatal(err)
		}
		got := out.Items[0]
		if got.Discussions == nil || got.Discussions.Notes != nil || got.Discussions.FullRevisionDigest != nil || got.Sections["discussions"].NextCursor != nil {
			t.Fatalf("published despite bad header: %+v %+v", got.Discussions, got.Sections["discussions"])
		}
		if got.ContextRef == nil {
			t.Fatal("sibling ref cleared")
		}
		lim := got.Sections["discussions"].Limitations
		if len(lim) != 1 || lim[0].Code != readmeta.CodeProviderPageAmbiguous {
			t.Fatalf("limitation %#v", lim)
		}
	})

	t.Run("semantic excludes full revision", func(t *testing.T) {
		body := "[" + discObj("d", noteObj(1, "user", "")+","+`{"id":2,"system":true,"author":{"id":9},"body":"bot"}`) + "]"
		log := &pathLog{}
		script := &reviewScript{log: log, discussions: discPages(map[int]string{1: body}, map[int]string{1: ""})}
		d := newReviewDeps(t, http.HandlerFunc(script.serve))
		item := metaItem("42", 1, "metadata", "discussions")
		out, err := callReviewDirect(t, d, context.Background(), []reviewContextItemIn{item})
		if err != nil {
			t.Fatal(err)
		}
		got := out.Items[0]
		if got.Discussions == nil || got.Discussions.FullRevisionDigest != nil || got.Discussions.SemanticFeedbackDigest == nil || got.Sections["discussions"].ContentComplete == readmeta.ContentCompleteTrue {
			t.Fatalf("semantic %+v", got.Discussions)
		}
		if got.Discussions.Notes == nil || len(*got.Discussions.Notes) != 1 || *(*got.Discussions.Notes)[0].NoteID != "1" {
			t.Fatalf("semantic notes %+v", got.Discussions.Notes)
		}
		payload, err := cursor.Decode(d.Config.CursorKey, *got.ContextRef, time.Date(2026, 10, 4, 12, 30, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := payload.ContextRef.Digests["discussions"]; ok {
			t.Fatalf("semantic evidence key %#v", payload.ContextRef.Digests)
		}
	})

	t.Run("http 500 keeps sibling ref", func(t *testing.T) {
		log := &pathLog{}
		script := &reviewScript{log: log, discStatus: http.StatusInternalServerError}
		d := newReviewDeps(t, http.HandlerFunc(script.serve))
		out, err := callReviewDirect(t, d, context.Background(), []reviewContextItemIn{metaItem("42", 1, "metadata", "discussions")})
		if err != nil || out.Items[0].ContextRef == nil || out.Items[0].Metadata == nil {
			t.Fatalf("err=%v item=%+v", err, out.Items[0])
		}
		if out.Items[0].Discussions == nil || out.Items[0].Discussions.FullRevisionDigest != nil {
			t.Fatalf("500 discussions %+v", out.Items[0].Discussions)
		}
		if len(out.Items[0].Sections["discussions"].Limitations) != 1 || out.Items[0].Sections["discussions"].Limitations[0].Code != readmeta.CodeHTTPError {
			t.Fatalf("lim %#v", out.Items[0].Sections["discussions"].Limitations)
		}
	})

	t.Run("notes advance one at a time", func(t *testing.T) {
		body := "[" + discObj("d", noteObj(1, "n0", "")+","+noteObj(2, "n1", "")+","+noteObj(3, "n2", "")) + "]"
		pages := map[int]string{1: body}
		next := map[int]string{1: ""}
		script := &reviewScript{discussions: discPages(pages, next)}
		d := newReviewDeps(t, http.HandlerFunc(script.serve))
		var cursor string
		var itemsBefore int
		// Opening charges 6 items. Reserve is 3 (two bracket charges plus
		// one version row). 10/11/12 each accept one new note and still close.
		for i, maxItems := range []int{10, 11, 12} {
			log := &pathLog{}
			script.log = log
			b := reviewBudget(128)
			b.MaxItems = maxItems
			item := metaItem("42", 1, "metadata", "discussions")
			item.DiscussionSelection = "all"
			if cursor != "" {
				item.Cursors = []reviewContextCursorIn{{Section: "discussions", Cursor: cursor}}
			}
			beforeReqs, beforeBytes, beforeItems := b.Stats()
			out, err := callReviewDirect(t, d, igl.WithBudget(context.Background(), b), []reviewContextItemIn{item})
			afterReqs, afterBytes, afterItems := b.Stats()
			if err != nil {
				t.Fatal(err)
			}
			got := out.Items[0]
			if got.Discussions == nil || got.Discussions.Notes == nil || len(*got.Discussions.Notes) != 1 {
				t.Fatalf("call %d max %d notes=%v cause=%s lim=%#v items %d->%d", i, maxItems, notesOf(got), got.Cause, got.Sections["discussions"].Limitations, beforeItems, afterItems)
			}
			wantID := strconv.Itoa(i + 1)
			if *(*got.Discussions.Notes)[0].NoteID != wantID {
				t.Fatalf("call %d note %s", i, *(*got.Discussions.Notes)[0].NoteID)
			}
			if got.Discussions.SemanticFeedbackDigest != nil || got.Discussions.PositionDigest != nil || got.Discussions.FullRevisionDigest != nil || got.Sections["discussions"].ContentComplete == readmeta.ContentCompleteTrue {
				t.Fatalf("call %d claimed discussions evidence %+v", i, got.Discussions)
			}
			// One budget object: opening 6 + replayed-and-new notes + closing 3.
			// A reset would drop the end count; a missed closing charge would stop at 6+(i+1).
			inspected := i + 1
			wantItems := 6 + inspected + 3
			if beforeReqs != 0 || beforeBytes != 0 || beforeItems != 0 || afterItems != wantItems || afterItems != maxItems || afterReqs != 11 || afterReqs <= beforeReqs || afterBytes < beforeBytes {
				t.Fatalf("call %d counters reqs %d->%d bytes %d->%d items %d->%d wantItems %d wantReqs 11", i, beforeReqs, afterReqs, beforeBytes, afterBytes, beforeItems, afterItems, wantItems)
			}
			if log.count("/versions") != 2 || got.Cause != "" || got.ContextRef == nil {
				t.Fatalf("call %d versions %d cause=%s ref=%v", i, log.count("/versions"), got.Cause, got.ContextRef != nil)
			}
			assertMetadataRefExcludesDiscussions(t, d, *got.ContextRef)
			itemsBefore = afterItems
			if i < 2 {
				if got.Sections["discussions"].NextCursor == nil {
					t.Fatalf("call %d missing cursor lim=%#v items=%d", i, got.Sections["discussions"].Limitations, itemsBefore)
				}
				cursor = *got.Sections["discussions"].NextCursor
			} else if got.Sections["discussions"].NextCursor != nil || !got.Sections["discussions"].PaginationExhausted || got.Sections["discussions"].ContentComplete == readmeta.ContentCompleteTrue {
				t.Fatalf("resumed tail %+v", got.Sections["discussions"])
			}
			if log.count("/discussions") != 1 {
				t.Fatalf("call %d gets %d", i, log.count("/discussions"))
			}
		}
	})

	t.Run("same ceiling mints nothing", func(t *testing.T) {
		body := "[" + discObj("d", noteObj(1, "n0", "")+","+noteObj(2, "n1", "")) + "]"
		log := &pathLog{}
		script := &reviewScript{log: log, discussions: discPages(map[int]string{1: body}, map[int]string{1: ""})}
		d := newReviewDeps(t, http.HandlerFunc(script.serve))
		b := reviewBudget(128)
		b.MaxItems = 10
		item := metaItem("42", 1, "discussions")
		item.DiscussionSelection = "all"
		out, err := callReviewDirect(t, d, igl.WithBudget(context.Background(), b), []reviewContextItemIn{item})
		if err != nil {
			t.Fatal(err)
		}
		cur := out.Items[0].Sections["discussions"].NextCursor
		if cur == nil || out.Items[0].Discussions == nil || len(*out.Items[0].Discussions.Notes) != 1 {
			t.Fatalf("first %+v lim=%#v", out.Items[0].Discussions, out.Items[0].Sections["discussions"].Limitations)
		}
		b2 := reviewBudget(128)
		b2.MaxItems = 10
		item.Cursors = []reviewContextCursorIn{{Section: "discussions", Cursor: *cur}}
		out, err = callReviewDirect(t, d, igl.WithBudget(context.Background(), b2), []reviewContextItemIn{item})
		if err != nil {
			t.Fatal(err)
		}
		if out.Items[0].Sections["discussions"].NextCursor != nil {
			t.Fatal("no-progress minted a cursor")
		}
		if out.Items[0].Discussions.Notes != nil && len(*out.Items[0].Discussions.Notes) != 0 {
			t.Fatalf("no-progress returned %#v", out.Items[0].Discussions.Notes)
		}
	})

	t.Run("unproved bracket does no discussions get", func(t *testing.T) {
		log := &pathLog{}
		script := &reviewScript{log: log, missingBranchIID: 1, discussions: discPages(map[int]string{1: "[]"}, map[int]string{1: ""})}
		d := newReviewDeps(t, http.HandlerFunc(script.serve))
		out, err := callReviewDirect(t, d, context.Background(), []reviewContextItemIn{metaItem("42", 1, "metadata", "discussions")})
		if err != nil {
			t.Fatal(err)
		}
		if log.count("/discussions") != 0 || out.Items[0].ContextRef != nil {
			t.Fatalf("gets %d ref %v", log.count("/discussions"), out.Items[0].ContextRef != nil)
		}
	})

	t.Run("closing mismatch nulls digests", func(t *testing.T) {
		log := &pathLog{}
		script := &reviewScript{log: log, driftHeadIID: 1, driftHeadSHA: shaN(99), discussions: discPages(map[int]string{1: "[" + discObj("d", noteObj(1, "a", "")) + "]"}, map[int]string{1: ""})}
		d := newReviewDeps(t, http.HandlerFunc(script.serve))
		item := metaItem("42", 1, "metadata", "discussions")
		item.DiscussionSelection = "all"
		out, err := callReviewDirect(t, d, context.Background(), []reviewContextItemIn{item})
		if err != nil {
			t.Fatal(err)
		}
		got := out.Items[0]
		if got.ContextRef != nil || got.Discussions == nil || got.Discussions.SemanticFeedbackDigest != nil || got.Discussions.FullRevisionDigest != nil || got.Sections["discussions"].ContentComplete == readmeta.ContentCompleteTrue {
			t.Fatalf("mismatch ref=%v disc=%+v", got.ContextRef != nil, got.Discussions)
		}
		if log.count("/discussions") != 1 {
			t.Fatalf("gets %d", log.count("/discussions"))
		}
	})

	t.Run("duplicate ids withhold", func(t *testing.T) {
		body := "[" + discObj("d", noteObj(1, "a", "")+","+noteObj(1, "b", "")) + "]"
		log := &pathLog{}
		script := &reviewScript{log: log, discussions: discPages(map[int]string{1: body}, map[int]string{1: ""})}
		d := newReviewDeps(t, http.HandlerFunc(script.serve))
		item := metaItem("42", 1, "metadata", "discussions")
		item.DiscussionSelection = "all"
		out, err := callReviewDirect(t, d, context.Background(), []reviewContextItemIn{item})
		if err != nil {
			t.Fatal(err)
		}
		got := out.Items[0]
		if got.Discussions == nil || got.Discussions.FullRevisionDigest != nil || got.Sections["discussions"].NextCursor != nil || got.Sections["discussions"].Consistency != readmeta.ConsistencyInconsistent {
			t.Fatalf("dup %+v %+v", got.Discussions, got.Sections["discussions"])
		}
	})

	t.Run("streamable all", func(t *testing.T) {
		log := &pathLog{}
		script := &reviewScript{log: log, discussions: discPages(map[int]string{1: "[" + discObj("d", noteObj(4, "s", "")) + "]"}, map[int]string{1: ""})}
		d := newReviewDeps(t, http.HandlerFunc(script.serve))
		out, err := callReviewContext(t, d, nil, map[string]any{"items": []any{itemArg("42", 1, []any{"metadata", "discussions"}, map[string]any{"discussion_selection": "all"})}}, true)
		if err != nil {
			t.Fatal(err)
		}
		item := asMap(t, itemsOf(t, out)[0])
		disc := asMap(t, item["discussions"])
		sec := asMap(t, asMap(t, item["sections"])["discussions"])
		if item["context_ref"] == nil || disc["full_revision_digest"] == nil || sec["content_complete"] != readmeta.ContentCompleteTrue {
			t.Fatalf("streamable %#v %#v", disc, sec)
		}
	})

	t.Run("null selection does no http", func(t *testing.T) {
		log := &pathLog{}
		d := newReviewDeps(t, http.HandlerFunc((&reviewScript{log: log}).serve))
		before := log.snapshot()
		_, err := callReviewContext(t, d, nil, map[string]any{"items": []any{itemArg("42", 1, []any{"metadata"}, map[string]any{"discussion_selection": nil})}}, false)
		if err == nil || log.snapshot() != before {
			t.Fatalf("err=%v http %d->%d", err, before, log.snapshot())
		}
	})
}

func notesOf(item reviewContextItemOut) any {
	if item.Discussions == nil {
		return nil
	}
	return item.Discussions.Notes
}

func TestDiscussionResumeBindings(t *testing.T) {
	body := "[" + discObj("d", noteObj(1, "only", "")+","+noteObj(2, "next", "")) + "]"
	log := &pathLog{}
	script := &reviewScript{log: log, discussions: discPages(map[int]string{1: body}, map[int]string{1: ""})}
	d := newReviewDeps(t, http.HandlerFunc(script.serve))
	b := reviewBudget(128)
	b.MaxItems = 10
	item := metaItem("42", 1, "discussions")
	item.DiscussionSelection = "semantic"
	out, err := callReviewDirect(t, d, igl.WithBudget(context.Background(), b), []reviewContextItemIn{item})
	if err != nil {
		t.Fatal(err)
	}
	cur := out.Items[0].Sections["discussions"].NextCursor
	if cur == nil {
		t.Fatalf("need a cursor lim=%#v notes=%v", out.Items[0].Sections["discussions"].Limitations, notesOf(out.Items[0]))
	}
	t.Run("head change", func(t *testing.T) {
		script.driftHeadIID = 1
		script.driftHeadSHA = shaN(77)
		before := log.count("/discussions")
		item.Cursors = []reviewContextCursorIn{{Section: "discussions", Cursor: *cur}}
		_, err := callReviewDirect(t, d, context.Background(), []reviewContextItemIn{item})
		if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
			t.Fatalf("head: %v", err)
		}
		if log.count("/discussions") != before {
			t.Fatalf("discussions %d->%d", before, log.count("/discussions"))
		}
	})
	t.Run("expiry", func(t *testing.T) {
		d.Clock.(*cursor.FakeClock).Advance(3 * time.Hour)
		before := log.snapshot()
		item.Cursors = []reviewContextCursorIn{{Section: "discussions", Cursor: *cur}}
		_, err := callReviewDirect(t, d, context.Background(), []reviewContextItemIn{item})
		if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) || log.snapshot() != before {
			t.Fatalf("expiry err=%v http %d->%d", err, before, log.snapshot())
		}
	})
}

func TestDiscussionPinnedDeadline(t *testing.T) {
	body := "[" + discObj("d", noteObj(1, "a", "")+","+noteObj(2, "b", "")) + "]"
	log := &pathLog{}
	script := &reviewScript{log: log, discussions: discPages(map[int]string{1: body}, map[int]string{1: ""})}
	d := newReviewDeps(t, http.HandlerFunc(script.serve))
	b := reviewBudget(128)
	b.MaxItems = 10
	item := metaItem("42", 1, "discussions")
	item.DiscussionSelection = "all"
	out, err := callReviewDirect(t, d, igl.WithBudget(context.Background(), b), []reviewContextItemIn{item})
	if err != nil {
		t.Fatal(err)
	}
	cur := out.Items[0].Sections["discussions"].NextCursor
	if cur == nil {
		t.Fatalf("cursor lim=%#v", out.Items[0].Sections["discussions"].Limitations)
	}
	d.Clock.(*cursor.FakeClock).Advance(time.Minute)
	before := log.count("/discussions")
	item.Cursors = []reviewContextCursorIn{{Section: "discussions", Cursor: *cur}}
	in := getMergeRequestReviewContextIn{Items: []reviewContextItemIn{item}, MaxElapsedMS: ptr64(30000)}
	_, raw, err := getMergeRequestReviewContext(context.Background(), nil, in, d)
	if err != nil {
		t.Fatal(err)
	}
	got := raw.(reviewContextOut).Items[0]
	if log.count("/discussions") != before || got.Sections["discussions"].NextCursor != nil {
		t.Fatalf("deadline gets %d->%d sec=%+v", before, log.count("/discussions"), got.Sections["discussions"])
	}
	if len(got.Sections["discussions"].Limitations) == 0 || got.Sections["discussions"].Limitations[0].Code != readmeta.CodeBudgetElapsed {
		t.Fatalf("lim %#v", got.Sections["discussions"].Limitations)
	}
}

func ptr64(n int64) *int64 { return &n }

func TestDiscussionPresenceAndPrefix(t *testing.T) {
	base := []byte("[" + discObj("a", noteObj(2, "hello", "")) + "]")
	s0, p0, f0, ok0 := discussionDigestDocuments(base)
	if !ok0 {
		t.Fatal("baseline")
	}
	ranged := []byte("[" + discObj("a", noteObj(2, "hello", `"position":{"new_path":"a.go","new_line":1,"line_range":[{"line_code":"abc_1"}]}`)) + "]")
	sr, pr, fr, okr := discussionDigestDocuments(ranged)
	if !okr || sr != s0 || pr == p0 || fr == f0 {
		t.Fatalf("line_range sem %s/%s pos %s/%s full %s/%s ok=%v", s0, sr, p0, pr, f0, fr, okr)
	}
	nullRange := []byte("[" + discObj("a", noteObj(2, "hello", `"position":{"new_path":"a.go","new_line":1,"line_range":null}`)) + "]")
	sn, pn, fn, okn := discussionDigestDocuments(nullRange)
	if !okn || sn != s0 || pn == p0 || pn == pr || fn == f0 {
		t.Fatalf("null line_range pos %s ranged %s absent %s", pn, pr, p0)
	}
	if _, _, _, bad := discussionDigestDocuments([]byte("[" + discObj("a", noteObj(1, "a", "")) + "," + discObj("a", noteObj(2, "b", "")) + "]")); bad {
		t.Fatal("duplicate discussion id minted a digest")
	}
	if _, _, _, bad := discussionDigestDocuments([]byte(`{"id":"a"}`)); bad {
		t.Fatal("non-array page minted a digest")
	}
	if _, _, _, bad := discussionDigestDocuments([]byte(`[]true`)); bad {
		t.Fatal("trailing token minted a digest")
	}
	resume := &discCoord{DI: 0}
	mismatch := decodeDiscussionPage(strings.NewReader(string(base)), resume, strings.Repeat("ab", 32), "", "all", nil, func() error { return nil })
	if !mismatch.inconsistent || mismatch.cursorOK || mismatch.notes != nil {
		t.Fatalf("prefix mismatch %+v", mismatch)
	}
	past := &discCoord{DI: 4}
	unreached := decodeDiscussionPage(strings.NewReader(string(base)), past, "", "", "all", nil, func() error { return nil })
	if !unreached.inconsistent || unreached.cursorOK {
		t.Fatalf("unreached resume %+v", unreached)
	}
}

func TestDiscussionIdentityUnresolved(t *testing.T) {
	log := &pathLog{}
	script := &reviewScript{log: log, discStatus: http.StatusNotFound}
	d := newReviewDeps(t, http.HandlerFunc(script.serve))
	item := metaItem("42", 1, "metadata", "discussions")
	item.DiscussionSelection = "all"
	out, err := callReviewDirect(t, d, context.Background(), []reviewContextItemIn{item})
	if err != nil {
		t.Fatal(err)
	}
	got := out.Items[0]
	if log.count("/discussions") != 1 || got.ContextRef == nil || got.Cause != "" {
		t.Fatalf("gets %d ref %v cause %s", log.count("/discussions"), got.ContextRef != nil, got.Cause)
	}
	lim := got.Sections["discussions"].Limitations
	if len(lim) != 1 || lim[0].Code != readmeta.CodeIdentityUnresolved || got.Discussions == nil || got.Discussions.Notes != nil || got.Discussions.FullRevisionDigest != nil {
		t.Fatalf("404 disc=%+v lim=%#v", got.Discussions, lim)
	}
}

func TestDiscussionReplayPrefixMismatch(t *testing.T) {
	pages := map[int]string{1: "[" + discObj("d", noteObj(1, "a", "")+","+noteObj(2, "b", "")) + "]"}
	log := &pathLog{}
	script := &reviewScript{log: log, discussions: discPages(pages, map[int]string{1: ""})}
	d := newReviewDeps(t, http.HandlerFunc(script.serve))
	b := reviewBudget(128)
	b.MaxItems = 10
	item := metaItem("42", 1, "discussions")
	item.DiscussionSelection = "all"
	out, err := callReviewDirect(t, d, igl.WithBudget(context.Background(), b), []reviewContextItemIn{item})
	if err != nil {
		t.Fatal(err)
	}
	cur := out.Items[0].Sections["discussions"].NextCursor
	if cur == nil || out.Items[0].Discussions == nil || out.Items[0].Discussions.Notes == nil || len(*out.Items[0].Discussions.Notes) != 1 {
		t.Fatalf("seed %+v", out.Items[0].Discussions)
	}
	pages[1] = "[" + discObj("d", noteObj(9, "a", "")+","+noteObj(2, "b", "")) + "]"
	before := log.count("/discussions")
	b2 := reviewBudget(128)
	item.Cursors = []reviewContextCursorIn{{Section: "discussions", Cursor: *cur}}
	out, err = callReviewDirect(t, d, igl.WithBudget(context.Background(), b2), []reviewContextItemIn{item})
	if err != nil {
		t.Fatal(err)
	}
	got := out.Items[0]
	if log.count("/discussions") != before+1 {
		t.Fatalf("replay gets %d->%d", before, log.count("/discussions"))
	}
	sec := got.Sections["discussions"]
	if sec.Consistency != readmeta.ConsistencyInconsistent || sec.NextCursor != nil || sec.ContentComplete == readmeta.ContentCompleteTrue {
		t.Fatalf("prefix sec %+v", sec)
	}
	if got.Discussions == nil || got.Discussions.Notes != nil || got.Discussions.SemanticFeedbackDigest != nil || got.Discussions.FullRevisionDigest != nil {
		t.Fatalf("prefix published %+v", got.Discussions)
	}
}

func TestDiscussionReserveRefusesGET(t *testing.T) {
	log := &pathLog{}
	body := "[" + discObj("d", noteObj(1, "kept", "")) + "]"
	script := &reviewScript{log: log, discussions: discPages(map[int]string{1: body}, map[int]string{1: ""})}
	d := newReviewDeps(t, http.HandlerFunc(script.serve))
	b := reviewBudget(128)
	b.MaxItems = 9
	item := metaItem("42", 1, "metadata", "discussions")
	item.DiscussionSelection = "all"
	out, err := callReviewDirect(t, d, igl.WithBudget(context.Background(), b), []reviewContextItemIn{item})
	if err != nil {
		t.Fatal(err)
	}
	got := out.Items[0]
	if log.count("/discussions") != 0 || log.count("/versions") != 2 {
		t.Fatalf("discussions %d versions %d", log.count("/discussions"), log.count("/versions"))
	}
	if got.ContextRef == nil || got.Cause != "" {
		t.Fatalf("ref=%v cause=%s", got.ContextRef != nil, got.Cause)
	}
	lim := got.Sections["discussions"].Limitations
	if len(lim) != 1 || lim[0].Code != readmeta.CodeBudgetItems || got.Sections["discussions"].NextCursor != nil {
		t.Fatalf("lim %#v cursor %v", lim, got.Sections["discussions"].NextCursor)
	}
}

func TestDiscussionClosedNextPage(t *testing.T) {
	log := &pathLog{}
	pages := map[int]string{
		1: "[" + discObj("d1", noteObj(1, "a", "")) + "]",
		2: "[" + discObj("d2", noteObj(2, "b", "")) + "]",
	}
	script := &reviewScript{log: log, discussions: discPages(pages, map[int]string{1: "2", 2: ""})}
	d := newReviewDeps(t, http.HandlerFunc(script.serve))
	b := reviewBudget(128)
	b.MaxRequests = 11
	item := metaItem("42", 1, "metadata", "discussions")
	item.DiscussionSelection = "all"
	out, err := callReviewDirect(t, d, igl.WithBudget(context.Background(), b), []reviewContextItemIn{item})
	if err != nil {
		t.Fatal(err)
	}
	got := out.Items[0]
	if log.count("/discussions") != 1 || log.count("/versions") != 2 {
		t.Fatalf("discussions %d versions %d cause=%s lim=%#v", log.count("/discussions"), log.count("/versions"), got.Cause, got.Sections["discussions"].Limitations)
	}
	cur := got.Sections["discussions"].NextCursor
	if cur == nil || got.Discussions == nil || got.Discussions.Notes == nil || len(*got.Discussions.Notes) != 1 {
		t.Fatalf("page1 %+v lim=%#v", got.Discussions, got.Sections["discussions"].Limitations)
	}
	payload, err := cursor.Decode(d.Config.CursorKey, *cur, time.Date(2026, 10, 4, 12, 30, 0, 0, time.UTC))
	if err != nil || payload.DiscussionsCont == nil || payload.DiscussionsCont.P != 2 || payload.DiscussionsCont.DI != 0 || payload.DiscussionsCont.NI != 0 {
		t.Fatalf("cursor err=%v payload=%+v", err, payload)
	}
	if got.Sections["discussions"].ContentComplete == readmeta.ContentCompleteTrue || got.Discussions.FullRevisionDigest != nil {
		t.Fatal("partial page claimed completion")
	}
}

func TestDiscussionOversizedNote(t *testing.T) {
	log := &pathLog{}
	fat := strings.Repeat("x", 8<<20)
	body := "[" + discObj("d", noteObj(1, "kept", "")+","+noteObj(2, fat, "")) + "]"
	script := &reviewScript{log: log, discussions: discPages(map[int]string{1: body}, map[int]string{1: ""})}
	d := newReviewDeps(t, http.HandlerFunc(script.serve))
	item := metaItem("42", 1, "metadata", "discussions")
	item.DiscussionSelection = "all"
	out, err := callReviewDirect(t, d, context.Background(), []reviewContextItemIn{item})
	if err != nil {
		t.Fatal(err)
	}
	got := out.Items[0]
	if got.Discussions == nil || got.Discussions.Notes == nil || len(*got.Discussions.Notes) != 1 || *(*got.Discussions.Notes)[0].NoteID != "1" {
		t.Fatalf("notes=%v lim=%#v", notesOf(got), got.Sections["discussions"].Limitations)
	}
	lim := got.Sections["discussions"].Limitations
	if len(lim) != 1 || lim[0].Code != readmeta.CodeTooLarge || got.Sections["discussions"].NextCursor != nil {
		t.Fatalf("lim %#v cursor %v", lim, got.Sections["discussions"].NextCursor)
	}
	if got.ContextRef == nil {
		t.Fatal("sibling ref cleared")
	}
}

func TestDiscussionShortAllowanceCursor(t *testing.T) {
	log := &pathLog{}
	fat := strings.Repeat("y", 40<<10)
	body := "[" + discObj("d", noteObj(1, "kept", "")+","+noteObj(2, fat, "")) + "]"
	script := &reviewScript{log: log, discussions: discPages(map[int]string{1: body}, map[int]string{1: ""})}
	d := newReviewDeps(t, http.HandlerFunc(script.serve))
	b := reviewBudget(128)
	b.MaxBytes = 96 << 10
	item := metaItem("42", 1, "metadata", "discussions")
	item.DiscussionSelection = "all"
	beforeReqs, beforeBytes, beforeItems := b.Stats()
	out, err := callReviewDirect(t, d, igl.WithBudget(context.Background(), b), []reviewContextItemIn{item})
	afterReqs, afterBytes, afterItems := b.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if b.MaxBytes != 96<<10 {
		t.Fatalf("byte cap changed to %d", b.MaxBytes)
	}
	got := out.Items[0]
	if got.Discussions == nil || got.Discussions.Notes == nil || len(*got.Discussions.Notes) != 1 || *(*got.Discussions.Notes)[0].NoteID != "1" {
		t.Fatalf("notes=%v cause=%s lim=%#v", notesOf(got), got.Cause, got.Sections["discussions"].Limitations)
	}
	if got.Cause != "" || got.ContextRef == nil || log.count("/versions") != 2 {
		t.Fatalf("versions %d cause=%s ref=%v", log.count("/versions"), got.Cause, got.ContextRef != nil)
	}
	// Kept note is charged; the cut note is not. Closing still adds 3 on this single-row fixture.
	if beforeReqs != 0 || beforeBytes != 0 || beforeItems != 0 || afterItems != 6+1+3 || afterReqs <= beforeReqs || afterBytes <= beforeBytes || afterBytes > b.MaxBytes {
		t.Fatalf("counters reqs %d->%d bytes %d->%d items %d->%d cap %d", beforeReqs, afterReqs, beforeBytes, afterBytes, beforeItems, afterItems, b.MaxBytes)
	}
	sec := got.Sections["discussions"]
	if sec.ContentComplete == readmeta.ContentCompleteTrue || got.Discussions.SemanticFeedbackDigest != nil || got.Discussions.PositionDigest != nil || got.Discussions.FullRevisionDigest != nil {
		t.Fatalf("short allowance claimed evidence %+v sec=%+v", got.Discussions, sec)
	}
	for _, lim := range sec.Limitations {
		if lim.Code == readmeta.CodeTooLarge {
			t.Fatal("short allowance reported too_large")
		}
	}
	assertMetadataRefExcludesDiscussions(t, d, *got.ContextRef)
	cur := sec.NextCursor
	if cur == nil {
		t.Fatalf("short allowance minted nothing lim=%#v", sec.Limitations)
	}
	payload, err := cursor.Decode(d.Config.CursorKey, *cur, time.Date(2026, 10, 4, 12, 30, 0, 0, time.UTC))
	if err != nil || payload.DiscussionsCont == nil || payload.DiscussionsCont.P != 1 || payload.DiscussionsCont.NI != 1 || payload.DiscussionsCont.DI != 0 {
		t.Fatalf("cursor err=%v cont=%+v", err, payload.DiscussionsCont)
	}
}

func assertMetadataRefExcludesDiscussions(t *testing.T, d Deps, token string) {
	t.Helper()
	payload, err := cursor.Decode(d.Config.CursorKey, token, time.Date(2026, 10, 4, 12, 30, 0, 0, time.UTC))
	if err != nil || payload.ContextRef == nil {
		t.Fatalf("context ref err=%v", err)
	}
	ref := payload.ContextRef
	if len(ref.Complete) != 1 || ref.Complete[0] != "metadata" {
		t.Fatalf("complete %#v", ref.Complete)
	}
	if _, ok := ref.Digests["discussions"]; ok {
		t.Fatalf("discussions digest %#v", ref.Digests)
	}
	excluded := false
	for _, name := range ref.Excluded {
		if name == "discussions" {
			excluded = true
		}
	}
	if !excluded {
		t.Fatalf("excluded %#v", ref.Excluded)
	}
}

func TestDiscussionF1UnknownSystem(t *testing.T) {
	missing := []byte(`[{"id":"d","individual_note":false,"notes":[{"id":1,"author":{"id":9},"body":"x"}]}]`)
	nullSys := []byte(`[{"id":"d","individual_note":false,"notes":[{"id":1,"author":{"id":9},"body":"x","system":null}]}]`)
	if _, _, _, ok := discussionDigestDocuments(missing); ok {
		t.Fatal("absent system minted digests")
	}
	if _, _, _, ok := discussionDigestDocuments(nullSys); ok {
		t.Fatal("null system minted digests")
	}
	user := []byte("[" + discObj("d", noteObj(1, "x", "")) + "]")
	su, _, fu, uok := discussionDigestDocuments(user)
	if !uok || su == "" || fu == "" {
		t.Fatal("system false user lost")
	}
	bot := []byte("[" + discObj("d", `{"id":2,"system":true,"author":{"id":9},"body":"bot"}`) + "]")
	sb, pb, fb, bok := discussionDigestDocuments(bot)
	emptySem := discSHA(`{"v":"discussions.semantic_feedback.v1","records":[]}`)
	if !bok || sb != emptySem || pb == "" || fb == "" || fb == discSHA(`{"v":"discussions.full_revision.v1","records":[]}`) {
		t.Fatalf("system true bot sem=%s pos=%s full=%s ok=%v", sb, pb, fb, bok)
	}

	for _, body := range []string{string(missing), string(nullSys)} {
		log := &pathLog{}
		script := &reviewScript{log: log, discussions: discPages(map[int]string{1: body}, map[int]string{1: ""})}
		d := newReviewDeps(t, http.HandlerFunc(script.serve))
		item := metaItem("42", 1, "metadata", "discussions")
		out, err := callReviewDirect(t, d, context.Background(), []reviewContextItemIn{item})
		if err != nil {
			t.Fatal(err)
		}
		got := out.Items[0]
		if got.Discussions == nil || got.Discussions.SemanticFeedbackDigest != nil {
			t.Fatalf("semantic treated unknown system as feedback %+v", got.Discussions)
		}
		mcpOut, err := callReviewContext(t, d, nil, map[string]any{"items": []any{itemArg("42", 1, []any{"metadata", "discussions"}, map[string]any{"discussion_selection": "all"})}}, true)
		if err != nil {
			t.Fatal(err)
		}
		raw := asMap(t, itemsOf(t, mcpOut)[0])
		ref, _ := raw["context_ref"].(string)
		if ref == "" {
			t.Fatal("metadata ref missing")
		}
		assertMetadataRefExcludesDiscussions(t, d, ref)
		disc := asMap(t, raw["discussions"])
		sec := asMap(t, asMap(t, raw["sections"])["discussions"])
		if disc["semantic_feedback_digest"] != nil || disc["position_digest"] != nil || disc["full_revision_digest"] != nil || sec["content_complete"] == readmeta.ContentCompleteTrue {
			t.Fatalf("streamable claimed unknown system %#v %#v", disc, sec)
		}
	}
}

func TestDiscussionF2OmittedNotes(t *testing.T) {
	omitted := []byte(`[{"id":"d","individual_note":false}]`)
	nullNotes := []byte(`[{"id":"d","individual_note":false,"notes":null}]`)
	if _, _, _, ok := discussionDigestDocuments(omitted); ok {
		t.Fatal("omitted notes minted a shell")
	}
	if _, _, _, ok := discussionDigestDocuments(nullNotes); ok {
		t.Fatal("null notes minted a shell")
	}
	emptyShell := []byte(`[{"id":"d","individual_note":false,"notes":[]}]`)
	es, ep, ef, eok := discussionDigestDocuments(emptyShell)
	if !eok || es == "" || ep == "" || ef == "" {
		t.Fatal("explicit empty notes lost")
	}
	zs, zp, zf, zok := discussionDigestDocuments([]byte(`[]`))
	emptySem := discSHA(`{"v":"discussions.semantic_feedback.v1","records":[]}`)
	if !zok || zs != emptySem || zp == "" || zf == "" {
		t.Fatal("empty list lost")
	}
	if es == zs {
		t.Fatal("empty shell hashed like an empty document")
	}

	for name, body := range map[string]string{"omitted": string(omitted), "null": string(nullNotes)} {
		log := &pathLog{}
		script := &reviewScript{log: log, discussions: discPages(map[int]string{1: body}, map[int]string{1: ""})}
		d := newReviewDeps(t, http.HandlerFunc(script.serve))
		out, err := callReviewContext(t, d, nil, map[string]any{"items": []any{itemArg("42", 1, []any{"metadata", "discussions"}, map[string]any{"discussion_selection": "all"})}}, true)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		raw := asMap(t, itemsOf(t, out)[0])
		ref, _ := raw["context_ref"].(string)
		if ref == "" {
			t.Fatalf("%s metadata ref missing", name)
		}
		assertMetadataRefExcludesDiscussions(t, d, ref)
		disc := asMap(t, raw["discussions"])
		sec := asMap(t, asMap(t, raw["sections"])["discussions"])
		notes, _ := disc["notes"].([]any)
		if disc["full_revision_digest"] != nil || sec["content_complete"] == readmeta.ContentCompleteTrue || len(notes) != 0 {
			t.Fatalf("%s proved empty %#v notes=%d", name, sec, len(notes))
		}
	}

	log := &pathLog{}
	script := &reviewScript{log: log, discussions: discPages(map[int]string{1: string(emptyShell)}, map[int]string{1: ""})}
	d := newReviewDeps(t, http.HandlerFunc(script.serve))
	out, err := callReviewContext(t, d, nil, map[string]any{"items": []any{itemArg("42", 1, []any{"metadata", "discussions"}, map[string]any{"discussion_selection": "all"})}}, true)
	if err != nil {
		t.Fatal(err)
	}
	raw := asMap(t, itemsOf(t, out)[0])
	ref, _ := raw["context_ref"].(string)
	payload, err := cursor.Decode(d.Config.CursorKey, ref, time.Date(2026, 10, 4, 12, 30, 0, 0, time.UTC))
	if err != nil || payload.ContextRef == nil || payload.ContextRef.Digests["discussions"] == "" {
		t.Fatalf("empty shell evidence err=%v digests=%v", err, payload.ContextRef)
	}
	want, err := bundleHex(es, ep, ef)
	if err != nil || payload.ContextRef.Digests["discussions"] != want {
		t.Fatalf("empty shell bundle got %v want %s", payload.ContextRef.Digests["discussions"], want)
	}
}

func TestDiscussionF3SignedBoundCrossRead(t *testing.T) {
	page := "[" + discObj("d", noteObj(1, "a", "")+","+noteObj(2, "b", "")) + "]"
	script := &reviewScript{discussions: discPages(map[int]string{1: page}, map[int]string{1: ""})}
	d := newReviewDeps(t, http.HandlerFunc(script.serve))
	b := reviewBudget(128)
	b.MaxItems = 10
	item := metaItem("42", 1, "metadata", "discussions")
	item.DiscussionSelection = "all"
	_, raw, err := getMergeRequestReviewContext(igl.WithBudget(context.Background(), b), nil, getMergeRequestReviewContextIn{Items: []reviewContextItemIn{item}, MaxElapsedMS: ptr64(5000)}, d)
	if err != nil {
		t.Fatal(err)
	}
	seed := raw.(reviewContextOut).Items[0]
	if seed.Sections["discussions"].NextCursor == nil {
		t.Fatalf("seed cursor missing %+v", seed.Sections["discussions"])
	}
	seedTok, err := cursor.Decode(d.Config.CursorKey, *seed.Sections["discussions"].NextCursor, time.Date(2026, 10, 4, 12, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	d.Clock.(*cursor.FakeClock).Advance(4700 * time.Millisecond)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	script.discussions = func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(900 * time.Millisecond):
			w.Header().Set("X-Next-Page", "")
			_, _ = io.WriteString(w, page)
		}
	}
	item.Cursors = []reviewContextCursorIn{{Section: "discussions", Cursor: *seed.Sections["discussions"].NextCursor}}
	b2 := reviewBudget(128)
	beforeReqs, _, beforeItems := b2.Stats()
	callDone := make(chan struct{})
	var got reviewContextItemOut
	var callErr error
	go func() {
		defer close(callDone)
		_, raw, callErr = getMergeRequestReviewContext(igl.WithBudget(context.Background(), b2), nil, getMergeRequestReviewContextIn{Items: []reviewContextItemIn{item}, MaxElapsedMS: ptr64(30000)}, d)
		if callErr == nil {
			got = raw.(reviewContextOut).Items[0]
		}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("discussions response did not start")
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight discussions was not cancelled across the signed bound")
	}
	select {
	case <-callDone:
	case <-time.After(2 * time.Second):
		t.Fatal("call did not return after cancellation")
	}
	if callErr != nil {
		t.Fatal(callErr)
	}
	sec := got.Sections["discussions"]
	if len(sec.Limitations) == 0 || sec.Limitations[0].Code != readmeta.CodeBudgetElapsed || sec.NextCursor != nil || sec.ContentComplete == readmeta.ContentCompleteTrue {
		t.Fatalf("cross-read sec=%+v disc=%+v", sec, got.Discussions)
	}
	if got.Discussions != nil && (got.Discussions.SemanticFeedbackDigest != nil || got.Discussions.FullRevisionDigest != nil) {
		t.Fatalf("cross-read minted evidence %+v", got.Discussions)
	}
	afterReqs, _, afterItems := b2.Stats()
	if afterReqs < beforeReqs || afterItems < beforeItems {
		t.Fatalf("counters reset %d->%d items %d->%d", beforeReqs, afterReqs, beforeItems, afterItems)
	}
	if seedTok.ExpiresAt == "" {
		t.Fatal("seed expiry empty")
	}
}

func TestDiscussionF3ClockAdvanceAcrossBound(t *testing.T) {
	script := &reviewScript{}
	d := newReviewDeps(t, http.HandlerFunc(script.serve))
	script.discussions = discPages(map[int]string{
		1: "[" + discObj("d1", noteObj(1, "a", "")) + "]",
		2: "[" + discObj("d2", noteObj(2, "b", "")) + "]",
		3: "[" + discObj("d3", noteObj(3, "c", "")) + "]",
	}, map[int]string{1: "2", 2: "3", 3: ""})
	b := reviewBudget(128)
	b.MaxRequests = 11
	item := metaItem("42", 1, "discussions")
	item.DiscussionSelection = "all"
	out, err := callReviewDirect(t, d, igl.WithBudget(context.Background(), b), []reviewContextItemIn{item})
	if err != nil {
		t.Fatal(err)
	}
	cur := out.Items[0].Sections["discussions"].NextCursor
	if cur == nil {
		t.Fatalf("need page-2 cursor %+v", out.Items[0].Sections["discussions"])
	}
	seed, err := cursor.Decode(d.Config.CursorKey, *cur, time.Date(2026, 10, 4, 12, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	var page3 int
	script.discussions = func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page == "3" {
			page3++
		}
		if page == "2" || page == "" {
			d.Clock.(*cursor.FakeClock).Advance(time.Minute)
			w.Header().Set("X-Next-Page", "3")
			_, _ = io.WriteString(w, "["+discObj("d2", noteObj(2, "b", ""))+"]")
			return
		}
		w.Header().Set("X-Next-Page", "")
		_, _ = io.WriteString(w, "["+discObj("d3", noteObj(3, "c", ""))+"]")
	}
	item.Cursors = []reviewContextCursorIn{{Section: "discussions", Cursor: *cur}}
	b2 := reviewBudget(128)
	beforeReqs, _, beforeItems := b2.Stats()
	out, err = callReviewDirect(t, d, igl.WithBudget(context.Background(), b2), []reviewContextItemIn{item})
	if err != nil {
		t.Fatal(err)
	}
	got := out.Items[0]
	sec := got.Sections["discussions"]
	if page3 != 0 || sec.NextCursor != nil || sec.ContentComplete == readmeta.ContentCompleteTrue {
		t.Fatalf("page3=%d cursor=%v complete=%v lim=%#v", page3, sec.NextCursor != nil, sec.ContentComplete, sec.Limitations)
	}
	if len(sec.Limitations) == 0 || sec.Limitations[0].Code != readmeta.CodeBudgetElapsed {
		t.Fatalf("lim %#v", sec.Limitations)
	}
	afterReqs, _, afterItems := b2.Stats()
	if afterReqs <= beforeReqs || afterItems < beforeItems {
		t.Fatalf("counters reset reqs %d->%d items %d->%d", beforeReqs, afterReqs, beforeItems, afterItems)
	}
	if got.Discussions != nil && got.Discussions.FullRevisionDigest != nil {
		t.Fatal("promoted evidence after the bound")
	}
	_ = seed
}

func TestDiscussionF3ParentDeadlineAndCopiedExpiry(t *testing.T) {
	page := "[" + discObj("d", noteObj(1, "a", "")+","+noteObj(2, "b", "")+","+noteObj(3, "c", "")) + "]"
	script := &reviewScript{discussions: discPages(map[int]string{1: page}, map[int]string{1: ""})}
	d := newReviewDeps(t, http.HandlerFunc(script.serve))
	b := reviewBudget(128)
	b.MaxItems = 10
	item := metaItem("42", 1, "discussions")
	item.DiscussionSelection = "all"
	out, err := callReviewDirect(t, d, igl.WithBudget(context.Background(), b), []reviewContextItemIn{item})
	if err != nil {
		t.Fatal(err)
	}
	cur := out.Items[0].Sections["discussions"].NextCursor
	if cur == nil {
		t.Fatal("seed")
	}
	seed, err := cursor.Decode(d.Config.CursorKey, *cur, time.Date(2026, 10, 4, 12, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	cancelled := make(chan struct{})
	script.discussions = func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(2 * time.Second):
			w.Header().Set("X-Next-Page", "")
			_, _ = io.WriteString(w, page)
		}
	}
	item.Cursors = []reviewContextCursorIn{{Section: "discussions", Cursor: *cur}}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	b2 := reviewBudget(128)
	beforeReqs, _, beforeItems := b2.Stats()
	_, _, err = getMergeRequestReviewContext(igl.WithBudget(ctx, b2), nil, getMergeRequestReviewContextIn{Items: []reviewContextItemIn{item}, MaxElapsedMS: ptr64(30000)}, d)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("parent-deadline response did not start")
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("earlier parent deadline did not cancel the child")
	}
	afterReqs, _, afterItems := b2.Stats()
	if afterReqs < beforeReqs || afterItems < beforeItems {
		t.Fatalf("counters reset %d->%d %d->%d", beforeReqs, afterReqs, beforeItems, afterItems)
	}
	_ = err

	script.discussions = discPages(map[int]string{1: page}, map[int]string{1: ""})
	b3 := reviewBudget(128)
	b3.MaxItems = 11
	out, err = callReviewDirect(t, d, igl.WithBudget(context.Background(), b3), []reviewContextItemIn{item})
	if err != nil {
		t.Fatal(err)
	}
	next := out.Items[0].Sections["discussions"].NextCursor
	if next == nil || out.Items[0].Sections["discussions"].ContentComplete == readmeta.ContentCompleteTrue {
		t.Fatalf("before-bound resume %+v", out.Items[0].Sections["discussions"])
	}
	got, err := cursor.Decode(d.Config.CursorKey, *next, time.Date(2026, 10, 4, 12, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got.ExpiresAt != seed.ExpiresAt || got.UpperBound != seed.UpperBound {
		t.Fatalf("renewed expiry %s->%s bound %s->%s", seed.ExpiresAt, got.ExpiresAt, seed.UpperBound, got.UpperBound)
	}
}

func TestDiscussionF4LineRangeOrder(t *testing.T) {
	posA := `"position":{"new_path":"a.go","new_line":1,"line_range":{"end":{"new_line":2,"line_code":"z"},"start":{"line_code":"a","new_line":1}}}`
	posB := `"position":{"line_range":{"start":{"new_line":1,"line_code":"a"},"end":{"line_code":"z","new_line":2}},"new_line":1,"new_path":"a.go"}`
	a := []byte("[" + discObj("d", noteObj(1, "x", posA)) + "]")
	b := []byte("[" + discObj("d", noteObj(1, "x", posB)) + "]")
	as, ap, af, aok := discussionDigestDocuments(a)
	bs, bp, bf, bok := discussionDigestDocuments(b)
	if !aok || !bok || as != bs || ap != bp || af != bf {
		t.Fatalf("reorder sem %s/%s pos %s/%s full %s/%s", as, bs, ap, bp, af, bf)
	}
	changed := `"position":{"new_path":"a.go","new_line":1,"line_range":{"start":{"line_code":"a","new_line":9},"end":{"line_code":"z","new_line":2}}}`
	cs, cp, cf, cok := discussionDigestDocuments([]byte("[" + discObj("d", noteObj(1, "x", changed)) + "]"))
	if !cok || cs != as || cp == ap || cf == af {
		t.Fatalf("value change sem %s/%s pos %s/%s full %s/%s", as, cs, ap, cp, af, cf)
	}
	arr1 := `"position":{"new_path":"a.go","new_line":1,"line_range":[{"line_code":"a"},{"line_code":"z"}]}`
	arr2 := `"position":{"new_path":"a.go","new_line":1,"line_range":[{"line_code":"z"},{"line_code":"a"}]}`
	_, p1, f1, ok1 := discussionDigestDocuments([]byte("[" + discObj("d", noteObj(1, "x", arr1)) + "]"))
	_, p2, f2, ok2 := discussionDigestDocuments([]byte("[" + discObj("d", noteObj(1, "x", arr2)) + "]"))
	if !ok1 || !ok2 || p1 == p2 || f1 == f2 {
		t.Fatal("array order collapsed")
	}
	n1 := `"position":{"new_path":"a.go","new_line":1,"line_range":{"start":{"new_line":1}}}`
	n2 := `"position":{"new_path":"a.go","new_line":1,"line_range":{"start":{"new_line":1.0}}}`
	_, pn1, _, on1 := discussionDigestDocuments([]byte("[" + discObj("d", noteObj(1, "x", n1)) + "]"))
	_, pn2, _, on2 := discussionDigestDocuments([]byte("[" + discObj("d", noteObj(1, "x", n2)) + "]"))
	if !on1 || !on2 || pn1 == pn2 {
		t.Fatal("numeric lexeme collapsed")
	}
	if _, _, _, bad := discussionDigestDocuments([]byte(`[{"id":"d","individual_note":false,"notes":[{"id":1,"system":false,"author":{"id":9},"body":"x","position":{"line_range":{"start":{"line_code":"a","line_code":"b"}}}}]}]`)); bad {
		t.Fatal("duplicate nested key minted")
	}
	absent := []byte("[" + discObj("d", noteObj(1, "x", "")) + "]")
	nullRange := []byte("[" + discObj("d", noteObj(1, "x", `"position":{"new_path":"a.go","new_line":1,"line_range":null}`)) + "]")
	_, pa, _, oa := discussionDigestDocuments(absent)
	_, pn, _, on := discussionDigestDocuments(nullRange)
	if !oa || !on || pa == pn || pa == ap || pn == ap {
		t.Fatal("absent/null/value line_range collapsed")
	}

	log := &pathLog{}
	script := &reviewScript{log: log, discussions: discPages(map[int]string{1: string(a)}, map[int]string{1: ""})}
	d := newReviewDeps(t, http.HandlerFunc(script.serve))
	first, err := callReviewContext(t, d, nil, map[string]any{"items": []any{itemArg("42", 1, []any{"metadata", "discussions"}, map[string]any{"discussion_selection": "all"})}}, true)
	if err != nil {
		t.Fatal(err)
	}
	script.discussions = discPages(map[int]string{1: string(b)}, map[int]string{1: ""})
	second, err := callReviewContext(t, d, nil, map[string]any{"items": []any{itemArg("42", 1, []any{"metadata", "discussions"}, map[string]any{"discussion_selection": "all"})}}, true)
	if err != nil {
		t.Fatal(err)
	}
	d1 := asMap(t, asMap(t, itemsOf(t, first)[0])["discussions"])
	d2 := asMap(t, asMap(t, itemsOf(t, second)[0])["discussions"])
	if d1["semantic_feedback_digest"] != d2["semantic_feedback_digest"] || d1["position_digest"] != d2["position_digest"] || d1["full_revision_digest"] != d2["full_revision_digest"] {
		t.Fatalf("registered reorder %#v %#v", d1, d2)
	}
}

func TestDiscussionN1EmptyInspectedCount(t *testing.T) {
	log := &pathLog{}
	script := &reviewScript{log: log, discussions: discPages(map[int]string{1: "[]"}, map[int]string{1: ""})}
	d := newReviewDeps(t, http.HandlerFunc(script.serve))
	item := metaItem("42", 1, "metadata", "discussions")
	item.DiscussionSelection = "all"
	out, err := callReviewDirect(t, d, context.Background(), []reviewContextItemIn{item})
	if err != nil {
		t.Fatal(err)
	}
	sec := out.Items[0].Sections["discussions"]
	if sec.Counts.Items == nil || *sec.Counts.Items != 0 || sec.ContentComplete != readmeta.ContentCompleteTrue {
		t.Fatalf("empty list counts=%v complete=%v", sec.Counts.Items, sec.ContentComplete)
	}
	b := reviewBudget(128)
	b.MaxItems = 9
	log2 := &pathLog{}
	script2 := &reviewScript{log: log2, discussions: discPages(map[int]string{1: "[]"}, map[int]string{1: ""})}
	d2 := newReviewDeps(t, http.HandlerFunc(script2.serve))
	out, err = callReviewDirect(t, d2, igl.WithBudget(context.Background(), b), []reviewContextItemIn{item})
	if err != nil {
		t.Fatal(err)
	}
	refused := out.Items[0].Sections["discussions"]
	if log2.count("/discussions") != 0 || refused.Counts.Items != nil {
		t.Fatalf("refused items=%v gets=%d", refused.Counts.Items, log2.count("/discussions"))
	}
}

func f5Note(system bool, inner string) string {
	flag := "false"
	if system {
		flag = "true"
	}
	if inner == "" {
		return fmt.Sprintf(`{"id":1,"system":%s}`, flag)
	}
	return fmt.Sprintf(`{"id":1,"system":%s,%s}`, flag, inner)
}

func f5Page(note string) string {
	return "[" + discObj("d", note) + "]"
}

func TestDiscussionF5SystemMandatoryRed(t *testing.T) {
	pages := []string{
		f5Page(f5Note(true, "")),
		f5Page(f5Note(true, `"author":null,"body":null`)),
		f5Page(f5Note(true, `"body":"x"`)),
		f5Page(f5Note(true, `"author":null,"body":"x"`)),
		f5Page(f5Note(true, `"author":{"id":9}`)),
		f5Page(f5Note(true, `"author":{"id":9},"body":null`)),
	}
	for _, page := range pages {
		if _, _, _, ok := discussionDigestDocuments([]byte(page)); ok {
			t.Errorf("helper minted digests without proved author/body: %s", page)
		}
	}
	if t.Failed() {
		page := pages[0]
		log := &pathLog{}
		script := &reviewScript{log: log, discussions: discPages(map[int]string{1: page}, map[int]string{1: ""})}
		d := newReviewDeps(t, http.HandlerFunc(script.serve))
		mcpOut, err := callReviewContext(t, d, nil, map[string]any{"items": []any{itemArg("42", 1, []any{"metadata", "discussions"}, map[string]any{"discussion_selection": "all"})}}, true)
		if err != nil {
			t.Fatal(err)
		}
		raw := asMap(t, itemsOf(t, mcpOut)[0])
		disc := asMap(t, raw["discussions"])
		sec := asMap(t, asMap(t, raw["sections"])["discussions"])
		if disc["semantic_feedback_digest"] != nil || disc["position_digest"] != nil || disc["full_revision_digest"] != nil || sec["content_complete"] == readmeta.ContentCompleteTrue {
			t.Errorf("streamable minted unproved system note digests %#v %#v", disc, sec)
		}
	}
}

func TestDiscussionF5MandatoryFieldMatrix(t *testing.T) {
	type row struct {
		name   string
		inner  string
		raw    string
		claim  bool
		stream bool
	}
	rows := []row{
		{name: "both-omitted", claim: false, stream: true},
		{name: "both-null", inner: `"author":null,"body":null`, claim: false, stream: true},
		{name: "author-omitted", inner: `"body":"x"`, claim: false, stream: true},
		{name: "author-null", inner: `"author":null,"body":"x"`, claim: false, stream: true},
		{name: "body-omitted", inner: `"author":{"id":9}`, claim: false, stream: true},
		{name: "body-null", inner: `"author":{"id":9},"body":null`, claim: false, stream: true},
		{name: "author-wrong-type", inner: `"author":"nope","body":"x"`, claim: false, stream: true},
		{name: "author-id-missing", inner: `"author":{},"body":"x"`, claim: false},
		{name: "author-id-null", inner: `"author":{"id":null},"body":"x"`, claim: false},
		{name: "author-id-zero", inner: `"author":{"id":0},"body":"x"`, claim: false, stream: true},
		{name: "author-id-negative", inner: `"author":{"id":-1},"body":"x"`, claim: false},
		{name: "author-id-string", inner: `"author":{"id":"9"},"body":"x"`, claim: false},
		{name: "body-wrong-type", inner: `"author":{"id":9},"body":1`, claim: false, stream: true},
		{name: "note-id-omitted", raw: `{"system":%s,"author":{"id":9},"body":"x"}`, claim: false},
		{name: "note-id-null", raw: `{"id":null,"system":%s,"author":{"id":9},"body":"x"}`, claim: false, stream: true},
		{name: "note-id-zero", raw: `{"id":0,"system":%s,"author":{"id":9},"body":"x"}`, claim: false},
		{name: "note-id-string", raw: `{"id":"1","system":%s,"author":{"id":9},"body":"x"}`, claim: false},
		{name: "valid", inner: `"author":{"id":9},"body":"x"`, claim: true},
		{name: "empty-body", inner: `"author":{"id":9},"body":""`, claim: true},
	}
	emptySem := discSHA(`{"v":"discussions.semantic_feedback.v1","records":[]}`)
	emptyFull := discSHA(`{"v":"discussions.full_revision.v1","records":[]}`)
	for _, system := range []bool{true, false} {
		flag := "false"
		if system {
			flag = "true"
		}
		for _, row := range rows {
			row := row
			system := system
			t.Run(fmt.Sprintf("system-%s/%s", flag, row.name), func(t *testing.T) {
				note := f5Note(system, row.inner)
				if row.raw != "" {
					note = fmt.Sprintf(row.raw, flag)
				}
				page := f5Page(note)
				sem, _, full, ok := discussionDigestDocuments([]byte(page))
				if row.claim {
					if !ok || sem == "" || full == "" || full == emptyFull {
						t.Fatalf("valid note lost sem=%s full=%s ok=%v", sem, full, ok)
					}
					if system && sem != emptySem {
						t.Fatalf("system note entered semantic digest %s", sem)
					}
					if !system && sem == emptySem {
						t.Fatalf("user note missing from semantic digest")
					}
					all := decodeDiscussionPage(strings.NewReader(page), nil, "", "", "all", &discScanState{seenDisc: map[string]struct{}{}, seenNote: map[string]struct{}{}}, func() error { return nil })
					semSel := decodeDiscussionPage(strings.NewReader(page), nil, "", "", "semantic", &discScanState{seenDisc: map[string]struct{}{}, seenNote: map[string]struct{}{}}, func() error { return nil })
					if system && (len(all.notes) != 1 || len(semSel.notes) != 0) {
						t.Fatalf("selection all=%d semantic=%d", len(all.notes), len(semSel.notes))
					}
					if !system && (len(all.notes) != 1 || len(semSel.notes) != 1) {
						t.Fatalf("user selection all=%d semantic=%d", len(all.notes), len(semSel.notes))
					}
					return
				}
				if ok {
					t.Fatalf("helper minted digests for %s", page)
				}
				if !row.stream {
					return
				}
				log := &pathLog{}
				script := &reviewScript{log: log, discussions: discPages(map[int]string{1: page}, map[int]string{1: ""})}
				d := newReviewDeps(t, http.HandlerFunc(script.serve))
				mcpOut, err := callReviewContext(t, d, nil, map[string]any{"items": []any{itemArg("42", 1, []any{"metadata", "discussions"}, map[string]any{"discussion_selection": "all"})}}, true)
				if err != nil {
					t.Fatal(err)
				}
				raw := asMap(t, itemsOf(t, mcpOut)[0])
				ref, _ := raw["context_ref"].(string)
				if ref == "" {
					t.Fatal("metadata ref missing")
				}
				assertMetadataRefExcludesDiscussions(t, d, ref)
				disc := asMap(t, raw["discussions"])
				sec := asMap(t, asMap(t, raw["sections"])["discussions"])
				if disc["semantic_feedback_digest"] != nil || disc["position_digest"] != nil || disc["full_revision_digest"] != nil || sec["content_complete"] == readmeta.ContentCompleteTrue {
					t.Fatalf("streamable claimed invalid note %#v %#v", disc, sec)
				}
			})
		}
	}
}

func TestDiscussionF6FractionalBound(t *testing.T) {
	clock := time.Date(2026, 10, 4, 12, 0, 0, int(500*time.Millisecond), time.UTC)
	page := "[" + discObj("d", noteObj(1, "a", "")+","+noteObj(2, "b", "")+","+noteObj(3, "c", "")) + "]"
	log := &pathLog{}
	script := &reviewScript{log: log, discussions: discPages(map[int]string{1: page}, map[int]string{1: ""})}
	d := newReviewDeps(t, http.HandlerFunc(script.serve))
	d.Clock = &cursor.FakeClock{T: clock}
	b := reviewBudget(128)
	b.MaxItems = 10
	item := metaItem("42", 1, "discussions")
	item.DiscussionSelection = "all"
	_, raw, err := getMergeRequestReviewContext(igl.WithBudget(context.Background(), b), nil, getMergeRequestReviewContextIn{Items: []reviewContextItemIn{item}, MaxElapsedMS: ptr64(200)}, d)
	if err != nil {
		t.Fatal(err)
	}
	seed := raw.(reviewContextOut).Items[0]
	sec := seed.Sections["discussions"]
	if sec.NextCursor == nil {
		t.Fatalf("seed fixture cursor missing lim=%#v complete=%v", sec.Limitations, sec.ContentComplete)
	}
	payload, err := cursor.Decode(d.Config.CursorKey, *sec.NextCursor, clock)
	if err != nil {
		t.Fatal(err)
	}
	wantBound := clock.Add(200 * time.Millisecond)
	wantExp := clock.Add(cursor.DefaultTTL)
	gotBound, berr := time.Parse(time.RFC3339Nano, payload.UpperBound)
	gotUntil, uerr := time.Parse(time.RFC3339Nano, payload.Filters.Until)
	gotExp, eerr := time.Parse(time.RFC3339Nano, payload.ExpiresAt)
	if berr != nil || uerr != nil || eerr != nil || !gotBound.Equal(wantBound) || !gotUntil.Equal(wantBound) || payload.UpperBound != payload.Filters.Until || !gotExp.Equal(wantExp) {
		t.Errorf("signed time upper=%s until=%s exp=%s wantBound=%s wantExp=%s", payload.UpperBound, payload.Filters.Until, payload.ExpiresAt, wantBound.Format(time.RFC3339Nano), wantExp.Format(time.RFC3339Nano))
	}
	before := log.count("/discussions")
	item.Cursors = []reviewContextCursorIn{{Section: "discussions", Cursor: *sec.NextCursor}}
	b2 := reviewBudget(128)
	b2.MaxItems = 11
	beforeReqs, _, beforeItems := b2.Stats()
	_, raw, err = getMergeRequestReviewContext(igl.WithBudget(context.Background(), b2), nil, getMergeRequestReviewContextIn{Items: []reviewContextItemIn{item}, MaxElapsedMS: ptr64(30000)}, d)
	if err != nil {
		t.Fatal(err)
	}
	resumed := raw.(reviewContextOut).Items[0]
	rsec := resumed.Sections["discussions"]
	after := log.count("/discussions")
	if after == before || rsec.NextCursor == nil || (len(rsec.Limitations) > 0 && rsec.Limitations[0].Code == readmeta.CodeBudgetElapsed) {
		t.Errorf("resume with 200ms left gets %d->%d cursor=%v lim=%#v", before, after, rsec.NextCursor != nil, rsec.Limitations)
	}
	if t.Failed() {
		return
	}
	next, err := cursor.Decode(d.Config.CursorKey, *rsec.NextCursor, clock)
	if err != nil {
		t.Fatal(err)
	}
	if next.UpperBound != payload.UpperBound || next.Filters.Until != payload.Filters.Until || next.ExpiresAt != payload.ExpiresAt {
		t.Fatalf("renewed upper %s->%s until %s->%s exp %s->%s", payload.UpperBound, next.UpperBound, payload.Filters.Until, next.Filters.Until, payload.ExpiresAt, next.ExpiresAt)
	}
	if next.DiscussionsCont == nil || payload.DiscussionsCont == nil || next.DiscussionsCont.NI != payload.DiscussionsCont.NI+1 {
		t.Fatalf("coordinate did not advance %#v -> %#v", payload.DiscussionsCont, next.DiscussionsCont)
	}
	afterReqs, _, afterItems := b2.Stats()
	if afterReqs <= beforeReqs || afterItems < beforeItems {
		t.Fatalf("counters reset reqs %d->%d items %d->%d", beforeReqs, afterReqs, beforeItems, afterItems)
	}
	d.Clock.(*cursor.FakeClock).Advance(200 * time.Millisecond)
	atBound := log.count("/discussions")
	item.Cursors = []reviewContextCursorIn{{Section: "discussions", Cursor: *rsec.NextCursor}}
	b3 := reviewBudget(128)
	b3.MaxItems = 12
	_, raw, err = getMergeRequestReviewContext(igl.WithBudget(context.Background(), b3), nil, getMergeRequestReviewContextIn{Items: []reviewContextItemIn{item}, MaxElapsedMS: ptr64(30000)}, d)
	if err != nil {
		t.Fatal(err)
	}
	stopped := raw.(reviewContextOut).Items[0].Sections["discussions"]
	if log.count("/discussions") != atBound || stopped.NextCursor != nil || stopped.ContentComplete == readmeta.ContentCompleteTrue {
		t.Fatalf("exact bound gets %d->%d cursor=%v complete=%v lim=%#v", atBound, log.count("/discussions"), stopped.NextCursor != nil, stopped.ContentComplete, stopped.Limitations)
	}
	if len(stopped.Limitations) == 0 || stopped.Limitations[0].Code != readmeta.CodeBudgetElapsed {
		t.Fatalf("exact bound lim %#v", stopped.Limitations)
	}
}

func f7Notes() string {
	return "[" + discObj("d", noteObj(1, "a", "")+","+noteObj(2, "b", "")+","+noteObj(3, "c", "")) + "]"
}

func TestDiscussionF7ClosingBound(t *testing.T) {
	t.Run("clock_crosses_on_resume_closing_versions", func(t *testing.T) {
		clock := time.Date(2026, 10, 4, 12, 0, 0, int(500*time.Millisecond), time.UTC)
		page := f7Notes()
		var versions int
		var script *reviewScript
		var d Deps
		d = newReviewDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/versions") {
				versions++
				if versions == 4 {
					d.Clock.(*cursor.FakeClock).Advance(200 * time.Millisecond)
				}
			}
			script.serve(w, r)
		}))
		d.Clock = &cursor.FakeClock{T: clock}
		script = &reviewScript{discussions: discPages(map[int]string{1: page}, map[int]string{1: ""})}
		b := reviewBudget(128)
		b.MaxItems = 10
		item := metaItem("42", 1, "metadata", "discussions")
		item.DiscussionSelection = "all"
		_, raw, err := getMergeRequestReviewContext(igl.WithBudget(context.Background(), b), nil, getMergeRequestReviewContextIn{Items: []reviewContextItemIn{item}, MaxElapsedMS: ptr64(200)}, d)
		if err != nil {
			t.Fatal(err)
		}
		seed := raw.(reviewContextOut).Items[0]
		if seed.Sections["discussions"].NextCursor == nil {
			t.Fatalf("seed fixture cursor missing lim=%#v", seed.Sections["discussions"].Limitations)
		}
		seedTok, err := cursor.Decode(d.Config.CursorKey, *seed.Sections["discussions"].NextCursor, clock)
		if err != nil || seedTok.DiscussionsCont == nil || seedTok.DiscussionsCont.NI != 1 {
			t.Fatalf("seed index fixture %#v err=%v", seedTok.DiscussionsCont, err)
		}
		item.Cursors = []reviewContextCursorIn{{Section: "discussions", Cursor: *seed.Sections["discussions"].NextCursor}}
		b2 := reviewBudget(128)
		b2.MaxItems = 11
		ctx := igl.WithBudget(context.Background(), b2)
		beforeReqs, _, beforeItems := b2.Stats()
		maxItems, _, _, _ := b2.LimitsSnapshot()
		_, raw, err = getMergeRequestReviewContext(ctx, nil, getMergeRequestReviewContextIn{Items: []reviewContextItemIn{item}, MaxElapsedMS: ptr64(30000)}, d)
		if err != nil {
			t.Fatal(err)
		}
		got := raw.(reviewContextOut).Items[0]
		sec := got.Sections["discussions"]
		if sec.NextCursor != nil {
			tok, decErr := cursor.Decode(d.Config.CursorKey, *sec.NextCursor, clock)
			ni := -1
			if decErr == nil && tok.DiscussionsCont != nil {
				ni = tok.DiscussionsCont.NI
			}
			t.Errorf("staged continuation survived closing versions past the bound ni=%d lim=%#v", ni, sec.Limitations)
		}
		if sec.ContentComplete == readmeta.ContentCompleteTrue || (got.Discussions != nil && (got.Discussions.SemanticFeedbackDigest != nil || got.Discussions.PositionDigest != nil || got.Discussions.FullRevisionDigest != nil)) {
			t.Errorf("closing cross promoted discussions evidence %+v", got.Discussions)
		}
		if len(sec.Limitations) == 0 || sec.Limitations[0].Code != readmeta.CodeBudgetElapsed {
			t.Errorf("discussions lim %#v", sec.Limitations)
		}
		if got.Cause != "" || got.ContextRef == nil {
			t.Errorf("sibling proof cause=%q ref=%v", got.Cause, got.ContextRef != nil)
		} else {
			assertMetadataRefExcludesDiscussions(t, d, *got.ContextRef)
		}
		afterReqs, _, afterItems := b2.Stats()
		maxAfter, _, _, _ := b2.LimitsSnapshot()
		if afterReqs <= beforeReqs || afterItems < beforeItems || maxAfter != maxItems || ctx.Err() != nil {
			t.Errorf("budget ownership reqs %d->%d items %d->%d cap %d->%d ctx=%v", beforeReqs, afterReqs, beforeItems, afterItems, maxItems, maxAfter, ctx.Err())
		}
		if versions != 4 {
			t.Errorf("versions responses %d", versions)
		}
	})

	t.Run("wall_delay_on_resume_closing_versions", func(t *testing.T) {
		clock := time.Date(2026, 10, 4, 12, 0, 0, int(500*time.Millisecond), time.UTC)
		page := f7Notes()
		var versions int
		var script *reviewScript
		outcome := make(chan string, 1)
		var d Deps
		d = newReviewDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/versions") {
				versions++
				if versions == 4 {
					select {
					case <-r.Context().Done():
						outcome <- "cancelled:" + r.Context().Err().Error()
						return
					case <-time.After(450 * time.Millisecond):
						outcome <- "completed"
					}
				}
			}
			script.serve(w, r)
		}))
		d.Clock = &cursor.FakeClock{T: clock}
		script = &reviewScript{discussions: discPages(map[int]string{1: page}, map[int]string{1: ""})}
		b := reviewBudget(128)
		b.MaxItems = 10
		item := metaItem("42", 1, "metadata", "discussions")
		item.DiscussionSelection = "all"
		_, raw, err := getMergeRequestReviewContext(igl.WithBudget(context.Background(), b), nil, getMergeRequestReviewContextIn{Items: []reviewContextItemIn{item}, MaxElapsedMS: ptr64(200)}, d)
		if err != nil {
			t.Fatal(err)
		}
		cur := raw.(reviewContextOut).Items[0].Sections["discussions"].NextCursor
		if cur == nil {
			t.Fatal("seed fixture")
		}
		item.Cursors = []reviewContextCursorIn{{Section: "discussions", Cursor: *cur}}
		b2 := reviewBudget(128)
		b2.MaxItems = 11
		start := time.Now()
		_, raw, err = getMergeRequestReviewContext(igl.WithBudget(context.Background(), b2), nil, getMergeRequestReviewContextIn{Items: []reviewContextItemIn{item}, MaxElapsedMS: ptr64(30000)}, d)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		var saw string
		select {
		case saw = <-outcome:
		case <-time.After(500 * time.Millisecond):
			saw = "no-outcome"
		}
		got := raw.(reviewContextOut).Items[0]
		sec := got.Sections["discussions"]
		if saw != "cancelled:context deadline exceeded" && !strings.HasPrefix(saw, "cancelled:") {
			t.Errorf("closing versions outcome %q elapsed=%s cursor=%v", saw, elapsed, sec.NextCursor != nil)
		}
		if sec.NextCursor != nil || got.Cause != readmeta.CodeBudgetElapsed || got.ContextRef != nil {
			t.Errorf("unbounded closing publication outcome=%q cause=%q cursor=%v ref=%v lim=%#v elapsed=%s", saw, got.Cause, sec.NextCursor != nil, got.ContextRef != nil, sec.Limitations, elapsed)
		}
	})

	t.Run("before_bound_copies_nano", func(t *testing.T) {
		clock := time.Date(2026, 10, 4, 12, 0, 0, int(500*time.Millisecond), time.UTC)
		page := f7Notes()
		script := &reviewScript{discussions: discPages(map[int]string{1: page}, map[int]string{1: ""})}
		d := newReviewDeps(t, http.HandlerFunc(script.serve))
		d.Clock = &cursor.FakeClock{T: clock}
		b := reviewBudget(128)
		b.MaxItems = 10
		item := metaItem("42", 1, "metadata", "discussions")
		item.DiscussionSelection = "all"
		_, raw, err := getMergeRequestReviewContext(igl.WithBudget(context.Background(), b), nil, getMergeRequestReviewContextIn{Items: []reviewContextItemIn{item}, MaxElapsedMS: ptr64(200)}, d)
		if err != nil {
			t.Fatal(err)
		}
		cur := raw.(reviewContextOut).Items[0].Sections["discussions"].NextCursor
		if cur == nil {
			t.Fatal("seed fixture")
		}
		item.Cursors = []reviewContextCursorIn{{Section: "discussions", Cursor: *cur}}
		b2 := reviewBudget(128)
		b2.MaxItems = 11
		_, raw, err = getMergeRequestReviewContext(igl.WithBudget(context.Background(), b2), nil, getMergeRequestReviewContextIn{Items: []reviewContextItemIn{item}, MaxElapsedMS: ptr64(30000)}, d)
		if err != nil {
			t.Fatal(err)
		}
		got := raw.(reviewContextOut).Items[0]
		if got.Sections["discussions"].NextCursor == nil || got.Cause != "" {
			t.Fatalf("before-bound resume %+v cause=%q", got.Sections["discussions"], got.Cause)
		}
		next, err := cursor.Decode(d.Config.CursorKey, *got.Sections["discussions"].NextCursor, clock)
		if err != nil {
			t.Fatal(err)
		}
		wantBound := clock.Add(200 * time.Millisecond).Format(time.RFC3339Nano)
		wantExp := clock.Add(cursor.DefaultTTL).Format(time.RFC3339Nano)
		if next.UpperBound != wantBound || next.Filters.Until != wantBound || next.ExpiresAt != wantExp || next.DiscussionsCont == nil || next.DiscussionsCont.NI != 2 {
			t.Fatalf("copied bound upper=%s until=%s exp=%s ni=%v", next.UpperBound, next.Filters.Until, next.ExpiresAt, next.DiscussionsCont)
		}
	})

	t.Run("parent_deadline_cancels_closing", func(t *testing.T) {
		page := f7Notes()
		var versions int
		var script *reviewScript
		cancelled := make(chan struct{})
		var d Deps
		d = newReviewDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/versions") {
				versions++
				if versions == 4 {
					select {
					case <-r.Context().Done():
						close(cancelled)
						return
					case <-time.After(2 * time.Second):
					}
				}
			}
			script.serve(w, r)
		}))
		script = &reviewScript{discussions: discPages(map[int]string{1: page}, map[int]string{1: ""})}
		b := reviewBudget(128)
		b.MaxItems = 10
		item := metaItem("42", 1, "discussions")
		item.DiscussionSelection = "all"
		_, raw, err := getMergeRequestReviewContext(igl.WithBudget(context.Background(), b), nil, getMergeRequestReviewContextIn{Items: []reviewContextItemIn{item}, MaxElapsedMS: ptr64(30000)}, d)
		if err != nil {
			t.Fatal(err)
		}
		cur := raw.(reviewContextOut).Items[0].Sections["discussions"].NextCursor
		if cur == nil {
			t.Fatal("seed fixture")
		}
		item.Cursors = []reviewContextCursorIn{{Section: "discussions", Cursor: *cur}}
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		defer cancel()
		b2 := reviewBudget(128)
		b2.MaxItems = 11
		_, _, err = getMergeRequestReviewContext(igl.WithBudget(ctx, b2), nil, getMergeRequestReviewContextIn{Items: []reviewContextItemIn{item}, MaxElapsedMS: ptr64(30000)}, d)
		select {
		case <-cancelled:
		case <-time.After(2 * time.Second):
			t.Fatal("earlier parent deadline did not cancel closing versions")
		}
		_ = err
	})
}

func TestDiscussionN2Int64Identity(t *testing.T) {
	maxID := "9223372036854775807"
	over := "9223372036854775808"
	maxNote := fmt.Sprintf(`{"id":%s,"system":false,"author":{"id":9},"body":"x"}`, maxID)
	maxAuthor := fmt.Sprintf(`{"id":1,"system":false,"author":{"id":%s},"body":"x"}`, maxID)
	overNote := fmt.Sprintf(`{"id":%s,"system":false,"author":{"id":9},"body":"x"}`, over)
	overAuthor := fmt.Sprintf(`{"id":1,"system":false,"author":{"id":%s},"body":"x"}`, over)
	if _, _, _, ok := discussionDigestDocuments([]byte(f5Page(maxNote))); !ok {
		t.Fatal("max int64 note id rejected")
	}
	if _, _, _, ok := discussionDigestDocuments([]byte(f5Page(maxAuthor))); !ok {
		t.Fatal("max int64 author id rejected")
	}
	if _, _, _, ok := discussionDigestDocuments([]byte(f5Page(overNote))); ok {
		t.Error("note id max+1 minted digests")
	}
	if _, _, _, ok := discussionDigestDocuments([]byte(f5Page(overAuthor))); ok {
		t.Error("author id max+1 minted digests")
	}
}
