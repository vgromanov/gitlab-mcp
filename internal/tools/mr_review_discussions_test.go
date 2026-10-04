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
			before, _, beforeItems := b.Stats()
			_ = before
			out, err := callReviewDirect(t, d, igl.WithBudget(context.Background(), b), []reviewContextItemIn{item})
			_, _, afterItems := b.Stats()
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
			if got.Discussions.FullRevisionDigest != nil || got.Sections["discussions"].ContentComplete == readmeta.ContentCompleteTrue {
				t.Fatalf("call %d claimed full revision", i)
			}
			delta := afterItems - beforeItems
			if i == 0 && delta < 1 {
				t.Fatalf("call 0 inspected %d", delta)
			}
			if i > 0 && delta < i+1 {
				t.Fatalf("call %d replay+new inspected %d", i, delta)
			}
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
	item := metaItem("42", 1, "discussions")
	item.DiscussionSelection = "all"
	out, err := callReviewDirect(t, d, igl.WithBudget(context.Background(), b), []reviewContextItemIn{item})
	if err != nil {
		t.Fatal(err)
	}
	got := out.Items[0]
	if got.Discussions == nil || got.Discussions.Notes == nil || len(*got.Discussions.Notes) != 1 || *(*got.Discussions.Notes)[0].NoteID != "1" {
		t.Fatalf("notes=%v cause=%s lim=%#v", notesOf(got), got.Cause, got.Sections["discussions"].Limitations)
	}
	cur := got.Sections["discussions"].NextCursor
	if cur == nil {
		t.Fatalf("short allowance minted nothing lim=%#v", got.Sections["discussions"].Limitations)
	}
	for _, lim := range got.Sections["discussions"].Limitations {
		if lim.Code == readmeta.CodeTooLarge {
			t.Fatal("short allowance reported too_large")
		}
	}
	payload, err := cursor.Decode(d.Config.CursorKey, *cur, time.Date(2026, 10, 4, 12, 30, 0, 0, time.UTC))
	if err != nil || payload.DiscussionsCont == nil || payload.DiscussionsCont.P != 1 || payload.DiscussionsCont.NI != 1 {
		t.Fatalf("cursor err=%v cont=%+v", err, payload.DiscussionsCont)
	}
}
