package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
)

const rwMe = int64(7) // the current user in the fixture

type rwNote struct {
	id, author int64
	system     bool
	body       string
}

// rwFixture is a tiny GitLab for one MR (project 1, iid 5): current user, MR
// head, discussions (one note each), note create/get. Every POST is counted.
type rwFixture struct {
	mu            sync.Mutex
	sha           string
	shaAfterPost  string // head reported once a note was posted
	notes         []rwNote
	posts         int
	postBodies    []string
	noteGetStatus int                 // non-zero: GET note answers this status
	mrFailFrom    int                 // from this MR read on (1-based) answer 404
	mrReads       int                 // MR reads so far
	stored        func(string) string // what GitLab keeps of a posted body
	hang          bool                // after storing, hold the POST open until the client leaves
}

func (f *rwFixture) client(t *testing.T) Deps {
	t.Helper()
	cli, _ := testutil.NewGitLabClient(t, f)
	return Deps{Config: &config.Config{}, Client: cli}
}

func rwJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (f *rwFixture) noteJSON(n rwNote) string {
	return rwJSON(map[string]any{"id": n.id, "body": n.body, "system": n.system, "author": map[string]any{"id": n.author}})
}

var rwNotePath = regexp.MustCompile(`^/api/v4/projects/1/merge_requests/5/notes/(\d+)$`)

func (f *rwFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if f.serve(w, r) {
		<-r.Context().Done() // hang mode: the lock is already released, nothing is written
	}
}

// serve runs the whole critical section under a deferred unlock and reports
// whether the request must hang (wait for the client to give up) afterwards.
func (f *rwFixture) serve(w http.ResponseWriter, r *http.Request) (hang bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && p == "/api/v4/user":
		writeFixture(w, `{"id":7,"username":"me","name":"Me"}`)
	case r.Method == http.MethodGet && p == "/api/v4/projects/1/merge_requests/5":
		f.mrReads++
		if f.mrFailFrom > 0 && f.mrReads >= f.mrFailFrom {
			w.WriteHeader(http.StatusNotFound)
			writeFixture(w, `{"message":"404 Not found"}`)
			return
		}
		sha := f.sha
		if f.posts > 0 && f.shaAfterPost != "" {
			sha = f.shaAfterPost
		}
		writeFixture(w, rwJSON(map[string]any{"iid": 5, "sha": sha}))
	case r.Method == http.MethodGet && p == "/api/v4/projects/1/merge_requests/5/discussions":
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		page, per = max(page, 1), max(per, 1)
		lo, hi := min((page-1)*per, len(f.notes)), min(page*per, len(f.notes))
		items := []string{}
		for _, n := range f.notes[lo:hi] {
			items = append(items, fmt.Sprintf(`{"id":"d%d","notes":[%s]}`, n.id, f.noteJSON(n)))
		}
		if hi < len(f.notes) {
			w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
		}
		writeFixture(w, "["+strings.Join(items, ",")+"]")
	case r.Method == http.MethodPost && p == "/api/v4/projects/1/merge_requests/5/notes":
		var in struct {
			Body string `json:"body"`
		}
		raw, _ := io.ReadAll(r.Body)
		f.postBodies = append(f.postBodies, string(raw))
		_ = json.Unmarshal(raw, &in)
		f.posts++
		body := in.Body
		if f.stored != nil {
			body = f.stored(body)
		}
		n := rwNote{id: int64(1000 + f.posts), author: rwMe, body: body}
		f.notes = append(f.notes, n)
		if f.hang {
			return true
		}
		w.WriteHeader(http.StatusCreated)
		writeFixture(w, f.noteJSON(n))
	case r.Method == http.MethodGet && rwNotePath.MatchString(p):
		if f.noteGetStatus != 0 {
			w.WriteHeader(f.noteGetStatus)
			writeFixture(w, `{"message":"gone"}`)
			return
		}
		id, _ := strconv.Atoi(rwNotePath.FindStringSubmatch(p)[1])
		for _, n := range f.notes {
			if n.id == int64(id) {
				writeFixture(w, f.noteJSON(n))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		writeFixture(w, `{"message":"404 Not found"}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		writeFixture(w, `{"message":"unexpected `+p+`"}`)
	}
	return false
}

func (f *rwFixture) postCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.posts
}

// rwReq builds a request whose Write posts a general MR note through the SDK,
// the way RVG-171/172 will; timeout > 0 bounds only the write call.
func rwReq(d Deps, expected, opKey, body string, timeout time.Duration) guardedWriteReq {
	return guardedWriteReq{
		ProjectID: "1", IID: 5, ExpectedSHA: expected, OpKey: opKey, Body: body,
		Write: func(ctx context.Context, b string) (int64, string, error) {
			if timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			n, _, err := d.Client.Notes.CreateMergeRequestNote("1", 5, &gitlab.CreateMergeRequestNoteOptions{Body: &b}, gitlab.WithContext(ctx))
			if err != nil {
				return 0, "", err
			}
			return n.ID, "", nil
		},
	}
}

func TestGuardedWrite_staleHeadRefusedWithoutWrite(t *testing.T) {
	f := &rwFixture{sha: "aaa"}
	d := f.client(t)
	_, err := guardedNoteWrite(context.Background(), d, rwReq(d, "bbb", "k1", "finding", 0))
	var hc *headChangedError
	if !errors.As(err, &hc) || hc.Current != "aaa" || hc.Expected != "bbb" {
		t.Fatalf("want head_changed with current sha, got %v", err)
	}
	if !strings.Contains(err.Error(), "head_changed") || !strings.Contains(err.Error(), "aaa") {
		t.Errorf("message must carry the code and the current sha: %v", err)
	}
	if f.postCount() != 0 {
		t.Errorf("a refused write must not POST, got %d", f.postCount())
	}
	// expected_sha is required.
	if _, err := guardedNoteWrite(context.Background(), d, rwReq(d, " ", "", "finding", 0)); err == nil || !strings.Contains(err.Error(), "expected_sha is required") {
		t.Errorf("missing expected_sha: %v", err)
	}
}

func TestGuardedWrite_writesAndReadsBack(t *testing.T) {
	f := &rwFixture{sha: "aaa"}
	d := f.client(t)
	res, err := guardedNoteWrite(context.Background(), d, rwReq(d, "aaa", "run.1:a-b_c", "a finding\n\nwith detail", 0))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Written || res.Deduplicated || res.NoteID != 1001 || res.HeadSHA != "aaa" || res.HeadChangedAfterWrite || res.BodyModified || res.LinesChanged != 0 || res.Error != "" {
		t.Errorf("unexpected result: %+v", res)
	}
	if got := f.notes[0].body; got != "a finding\n\nwith detail\n\n<!-- gitlab-mcp:op=run.1:a-b_c -->" {
		t.Errorf("stored body: %q", got)
	}
	// Step 4 is dropped: no head guard parameter is ever sent.
	if strings.Contains(strings.Join(f.postBodies, ""), "merge_request_diff_head_sha") {
		t.Error("merge_request_diff_head_sha must not be sent")
	}
	// No op_key: no marker, no user lookup needed.
	if _, err := guardedNoteWrite(context.Background(), d, rwReq(d, "aaa", "", "plain", 0)); err != nil || f.notes[1].body != "plain" {
		t.Errorf("plain write: %v %q", err, f.notes[1].body)
	}
}

func TestGuardedWrite_headMovedAfterWrite(t *testing.T) {
	f := &rwFixture{sha: "aaa", shaAfterPost: "ccc"}
	d := f.client(t)
	res, err := guardedNoteWrite(context.Background(), d, rwReq(d, "aaa", "", "x", 0))
	if err != nil || !res.Written || !res.HeadChangedAfterWrite || res.HeadSHA != "ccc" || res.Error != "" {
		t.Errorf("want written with head_changed_after_write and the new head, got %+v %v", res, err)
	}
}

func TestGuardedWrite_markerDedupe(t *testing.T) {
	marker := opMarker("k1")
	tests := []struct {
		name      string
		note      rwNote
		wantDedup bool
	}{
		{"own note", rwNote{id: 55, author: rwMe, body: "finding\n\n" + marker}, true},
		{"own note, trailing blank lines", rwNote{id: 56, author: rwMe, body: "finding\r\n" + marker + "\r\n\r\n"}, true},
		{"other author", rwNote{id: 57, author: 99, body: "x\n\n" + marker}, false},
		{"system note", rwNote{id: 58, author: rwMe, system: true, body: "x\n\n" + marker}, false},
		{"marker not last line", rwNote{id: 59, author: rwMe, body: marker + "\n\nquoted later"}, false},
		{"other key", rwNote{id: 60, author: rwMe, body: "x\n\n" + opMarker("k2")}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &rwFixture{sha: "aaa", notes: []rwNote{tc.note}}
			d := f.client(t)
			res, err := guardedNoteWrite(context.Background(), d, rwReq(d, "aaa", "k1", "finding", 0))
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantDedup {
				if !res.Deduplicated || res.Written || res.NoteID != tc.note.id || res.DiscussionID != fmt.Sprintf("d%d", tc.note.id) || f.postCount() != 0 {
					t.Errorf("want existing note %d, no write: %+v posts=%d", tc.note.id, res, f.postCount())
				}
			} else if res.Deduplicated || !res.Written || f.postCount() != 1 {
				t.Errorf("must write: %+v posts=%d", res, f.postCount())
			}
		})
	}
}

func TestGuardedWrite_scanPagingAndCap(t *testing.T) {
	notes := make([]rwNote, 250) // 3 pages of 100
	for i := range notes {
		notes[i] = rwNote{id: int64(i + 1), author: rwMe, body: "n"}
	}
	hit := append([]rwNote(nil), notes...)
	hit[149].body = "finding\n\n" + opMarker("k1") // page 2

	// Found on page 2 within the cap.
	f := &rwFixture{sha: "aaa", notes: hit}
	d := f.client(t)
	res, err := guardedNoteWrite(context.Background(), d, rwReq(d, "aaa", "k1", "x", 0))
	if err != nil || !res.Deduplicated || res.NoteID != 150 || f.postCount() != 0 {
		t.Fatalf("page 2 hit: %+v %v", res, err)
	}

	// Cap reached before the marker's page: refuse, never write.
	req := rwReq(d, "aaa", "k1", "x", 0)
	req.MaxScanPages = 1
	_, err = guardedNoteWrite(context.Background(), d, req)
	var inc *dedupeIncompleteError
	if !errors.As(err, &inc) || inc.Pages != 1 || !strings.Contains(err.Error(), "complete=false") || f.postCount() != 0 {
		t.Fatalf("want dedupe_incomplete without a write, got %v posts=%d", err, f.postCount())
	}

	// Whole list scanned without a hit: write.
	f2 := &rwFixture{sha: "aaa", notes: notes}
	d2 := f2.client(t)
	res, err = guardedNoteWrite(context.Background(), d2, rwReq(d2, "aaa", "k1", "x", 0))
	if err != nil || !res.Written || f2.postCount() != 1 {
		t.Fatalf("complete scan, no marker: %+v %v", res, err)
	}
	// A cap equal to the page count is still complete when there is no next page.
	f3 := &rwFixture{sha: "aaa", notes: notes[:100]}
	d3 := f3.client(t)
	req = rwReq(d3, "aaa", "k1", "x", 0)
	req.MaxScanPages = 1
	if res, err = guardedNoteWrite(context.Background(), d3, req); err != nil || !res.Written {
		t.Fatalf("exactly one full page is complete: %+v %v", res, err)
	}
}

func TestGuardedWrite_timeoutThenRetryWritesOnce(t *testing.T) {
	f := &rwFixture{sha: "aaa", hang: true}
	d := f.client(t)
	// First attempt: the server stores the note but the client never sees the answer.
	_, err := guardedNoteWrite(context.Background(), d, rwReq(d, "aaa", "retry-1", "finding", 200*time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want a deadline error, got %v", err)
	}
	if !strings.Contains(err.Error(), "op_key") {
		t.Errorf("error should say the retry with the same op_key is safe: %v", err)
	}
	// Retry: the marker is found, nothing is written.
	f.mu.Lock()
	f.hang = false
	f.mu.Unlock()
	res, err := guardedNoteWrite(context.Background(), d, rwReq(d, "aaa", "retry-1", "finding", 0))
	if err != nil || !res.Deduplicated || res.Written || res.NoteID != 1001 {
		t.Fatalf("retry: %+v %v", res, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 1 || len(f.notes) != 1 {
		t.Errorf("exactly one note expected, posts=%d notes=%d", f.posts, len(f.notes))
	}
}

func TestGuardedWrite_readbackFailuresAreSurfaced(t *testing.T) {
	// Note readback fails.
	f := &rwFixture{sha: "aaa", noteGetStatus: http.StatusNotFound}
	d := f.client(t)
	res, err := guardedNoteWrite(context.Background(), d, rwReq(d, "aaa", "", "x", 0))
	if err != nil || !res.Written || res.NoteID != 1001 || !strings.Contains(res.Error, "readback_failed: read note") {
		t.Errorf("note readback: %+v %v", res, err)
	}
	// Head readback fails (2nd MR read: the first is the preflight).
	f = &rwFixture{sha: "aaa", mrFailFrom: 2}
	d = f.client(t)
	res, err = guardedNoteWrite(context.Background(), d, rwReq(d, "aaa", "", "x", 0))
	if err != nil || !res.Written || res.HeadSHA != "" || res.HeadChangedAfterWrite || !strings.Contains(res.Error, "readback_failed: read merge request head") {
		t.Errorf("head readback: %+v %v", res, err)
	}
	// A write that returns no note (a commands-only body) is not a success.
	f = &rwFixture{sha: "aaa"}
	d = f.client(t)
	req := rwReq(d, "aaa", "", "x", 0)
	req.Write = func(context.Context, string) (int64, string, error) { return 0, "", nil }
	res, err = guardedNoteWrite(context.Background(), d, req)
	if err != nil || !strings.Contains(res.Error, "no note") {
		t.Errorf("no note: %+v %v", res, err)
	}
}

func TestGuardedWrite_storedBodyDetective(t *testing.T) {
	// GitLab consumed a line of the posted body: error-level signal, ids kept.
	f := &rwFixture{sha: "aaa", stored: func(s string) string { return strings.Replace(s, "KEEPOUT\n", "", 1) }}
	d := f.client(t)
	res, err := guardedNoteWrite(context.Background(), d, rwReq(d, "aaa", "", "one\nKEEPOUT\ntwo", 0))
	if err != nil || !res.Written || res.NoteID != 1001 || !strings.HasPrefix(res.Error, "stored_body_differs: 1 line") {
		t.Errorf("want the detective signal, got %+v %v", res, err)
	}
	// Normalisation GitLab does (CR removed, trailing whitespace trimmed) is not a signal.
	f = &rwFixture{sha: "aaa", stored: func(s string) string { return strings.TrimSpace(strings.ReplaceAll(s, "\r", "")) }}
	d = f.client(t)
	res, err = guardedNoteWrite(context.Background(), d, rwReq(d, "aaa", "k", "one\r\ntwo  \r\n", 0))
	if err != nil || res.Error != "" {
		t.Errorf("normalisation must not alarm: %+v %v", res, err)
	}
}

func TestGuardedWrite_opKeyValidation(t *testing.T) {
	f := &rwFixture{sha: "aaa"}
	d := f.client(t)
	bad := []string{"a b", "a\nb", "a-->b", "k\"", "k<", "k\r", "é", strings.Repeat("a", 65), "a/b", "k -->\n/merge"}
	for _, k := range bad {
		if _, err := guardedNoteWrite(context.Background(), d, rwReq(d, "aaa", k, "x", 0)); err == nil || !strings.Contains(err.Error(), "op_key") {
			t.Errorf("op_key %q must be rejected, got %v", k, err)
		}
	}
	for _, k := range []string{"a", "A.b_c:d-e", strings.Repeat("a", 64)} {
		if _, err := guardedNoteWrite(context.Background(), d, rwReq(d, "aaa", k, "x", 0)); err != nil {
			t.Errorf("op_key %q must be accepted: %v", k, err)
		}
	}
	if f.postCount() != 3 {
		t.Errorf("only valid keys write, got %d posts", f.postCount())
	}
	if _, err := guardedNoteWrite(context.Background(), d, rwReq(d, "aaa", "", "  \n", 0)); err == nil {
		t.Error("an empty body is invalid")
	}
}

// A marker quoted inside the caller's text must not be able to forge or suppress a dedupe.
func TestGuardedWrite_quotedMarkerCannotSuppress(t *testing.T) {
	f := &rwFixture{sha: "aaa"}
	d := f.client(t)
	forged := "attacker text\n" + opMarker("k9")
	res, err := guardedNoteWrite(context.Background(), d, rwReq(d, "aaa", "", forged, 0))
	if err != nil || !res.Written || !res.BodyModified || res.LinesChanged != 1 {
		t.Fatalf("quoted marker: %+v %v", res, err)
	}
	if strings.Contains(f.notes[0].body, opMarker("k9")) {
		t.Fatalf("the stored note carries a forged marker: %q", f.notes[0].body)
	}
	res, err = guardedNoteWrite(context.Background(), d, rwReq(d, "aaa", "k9", "real finding", 0))
	if err != nil || !res.Written || res.Deduplicated || f.postCount() != 2 {
		t.Errorf("the real op_key must still write: %+v %v", res, err)
	}
}

// The result reports flatly: counts only, never the matched text.
func TestGuardedWrite_quickActionBodyIsNeutralizedAndNotEchoed(t *testing.T) {
	f := &rwFixture{sha: "aaa"}
	d := f.client(t)
	body := "intro\n/title SECRETPAYLOAD\n\u200b/label ~sneaky\nfine"
	res, err := guardedNoteWrite(context.Background(), d, rwReq(d, "aaa", "k1", body, 0))
	if err != nil || !res.Written || !res.BodyModified || res.LinesChanged != 2 || res.Error != "" {
		t.Fatalf("result: %+v %v", res, err)
	}
	out, _ := json.Marshal(res)
	for _, leak := range []string{"SECRETPAYLOAD", "title", "label", "sneaky", "quick"} {
		if strings.Contains(string(out), leak) {
			t.Errorf("result leaks %q: %s", leak, out)
		}
	}
	if quickActionLeft(f.notes[0].body) {
		t.Errorf("posted body still carries a command line: %q", f.notes[0].body)
	}
}

var (
	// An independent model of what must not survive: after padding, a slash and a word.
	execModel = regexp.MustCompile(`^[\p{Z}\p{C}\p{M}>]*/[\p{C}\p{M}]*[\p{L}\p{N}_]`)
	// GitLab as measured on 17.11.7: CR removed, a line starting with slash and a word runs.
	glModel = regexp.MustCompile(`(?m)^/\w`)
)

func quickActionLeft(s string) bool {
	if glModel.MatchString(strings.ReplaceAll(s, "\r", "")) {
		return true
	}
	for _, l := range strings.FieldsFunc(s, func(r rune) bool {
		return r == '\n' || r == '\r' || r == '\v' || r == '\f' || r == '\u0085' || r == '\u2028' || r == '\u2029'
	}) {
		if execModel.MatchString(l) {
			return true
		}
	}
	return false
}

func TestSanitizeNoteBody_table(t *testing.T) {
	tests := []struct {
		name, in, want string
		changed        int
	}{
		{"empty", "", "", 0},
		{"plain", "plain text", "plain text", 0},
		{"line 1", "/merge", `\/merge`, 1},
		{"line N", "text\n\n/approve now", "text\n\n" + `\/approve now`, 1},
		{"leading space", " /close", ` \/close`, 1},
		{"leading tab", "\t/close", "\t" + `\/close`, 1},
		{"indented", "    /close", `    \/close`, 1},
		{"CRLF", "a\r\n/merge\r\nb", "a\r\n" + `\/merge` + "\r\nb", 1},
		{"lone CR before", "\r/merge", "\r" + `\/merge`, 1},
		{"CR inside", "/\rmerge", `\/` + "\rmerge", 1},
		{"CR as the only separator", "text\r/merge", "text\r" + `\/merge`, 1},
		{"uppercase", "/MERGE", `\/MERGE`, 1},
		{"mixed case", "/Merge", `\/Merge`, 1},
		{"nbsp", "\u00a0/merge", "\u00a0" + `\/merge`, 1},
		{"ideographic space", "\u3000/merge", "\u3000" + `\/merge`, 1},
		{"zero width before", "\u200b/merge", "\u200b" + `\/merge`, 1},
		{"BOM before", "\ufeff/merge", "\ufeff" + `\/merge`, 1},
		{"zero width inside", "/\u200bmerge", `\/` + "\u200bmerge", 1},
		{"combining before", "\u0301/merge", "\u0301" + `\/merge`, 1},
		{"line separator", "x\u2028/merge", "x\u2028" + `\/merge`, 1},
		{"NEL", "x\u0085/merge", "x\u0085" + `\/merge`, 1},
		{"fenced", "```\n/merge\n```", "```\n" + `\/merge` + "\n```", 1},
		{"blockquote", "> /merge", `> \/merge`, 1},
		{"blockquote no space", ">/merge", `>\/merge`, 1},
		{"nested quote", ">>> \n/merge\n>>>", ">>> \n" + `\/merge` + "\n>>>", 1},
		{"html block", "<details>\n\n/merge\n\n</details>", "<details>\n\n" + `\/merge` + "\n\n</details>", 1},
		{"after code span line", "`x`\n/merge", "`x`\n" + `\/merge`, 1},
		{"command with trailing text", "/merge now please", `\/merge now please`, 1},
		{"three lines", "/a\n/b\nx\n/c", `\/a` + "\n" + `\/b` + "\nx\n" + `\/c`, 3},
		{"digit command", "/2fa", `\/2fa`, 1},
		{"quoted marker prefix", "q <!-- gitlab-mcp:op=k -->", "q <!-- gitlab-mcp:op&#61;k -->", 1},
		// Not command lines: left alone.
		{"mid sentence", "see /merge now", "see /merge now", 0},
		{"after code span same line", "`x` /merge", "`x` /merge", 0},
		{"path in text", "a/b and http://x/y", "a/b and http://x/y", 0},
		{"comment slashes", "// comment\n/* c */", "// comment\n/* c */", 0},
		{"bare slash", "/\n/ x", "/\n/ x", 0},
		{"fullwidth slash", "\uff0fmerge", "\uff0fmerge", 0},
		{"already escaped", `\/merge`, `\/merge`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, n := sanitizeNoteBody(tc.in)
			if got != tc.want || n != tc.changed {
				t.Errorf("sanitizeNoteBody(%q) = %q, %d; want %q, %d", tc.in, got, n, tc.want, tc.changed)
			}
			if again, n2 := sanitizeNoteBody(got); again != got || n2 != 0 {
				t.Errorf("not idempotent: %q -> %q (%d)", got, again, n2)
			}
		})
	}
}

// Randomised obfuscation: padding, separators, case, zero-width runes and line
// endings in every position; no variant may leave a command line behind.
func TestSanitizeNoteBody_obfuscationVariants(t *testing.T) {
	pads := []string{"", " ", "  ", "\t", "\u00a0", "\u2003", "\u3000", "\u200b", "\u200c", "\u200d", "\u2060", "\ufeff", "\u00ad", "\u200e", "\u0301", "\x01", "\x7f", "\r", ">", "> ", ">>", "\r>", "\u2028>"}
	inner := []string{"", "\u200b", "\r", "\u00ad", "\ufe0f", "\x01"}
	words := []string{"merge", "MERGE", "Approve", "close", "assign", "label", "title", "x", "2fa", "_u", "été"}
	breaks := []string{"\n", "\r\n", "\r", "\u2028", "\u2029", "\u0085", "\v", "\f", "\n\n", "\r\r\n"}
	rng := rand.New(rand.NewSource(169))
	pick := func(s []string) string { return s[rng.Intn(len(s))] }
	for i := 0; i < 5000; i++ {
		var b strings.Builder
		for l, lines := 0, 1+rng.Intn(4); l < lines; l++ {
			if rng.Intn(3) == 0 {
				b.WriteString(pick([]string{"text ", "`c` ", "```\n", "<div>", "- "}))
			}
			b.WriteString(pick(pads) + pick(pads) + "/" + pick(inner) + pick(words) + " arg")
			b.WriteString(pick(breaks))
		}
		in := b.String()
		got, _ := sanitizeNoteBody(in)
		if quickActionLeft(got) {
			t.Fatalf("command line survives: %q -> %q", in, got)
		}
		if again, _ := sanitizeNoteBody(got); again != got {
			t.Fatalf("not idempotent: %q", in)
		}
	}
}

func FuzzSanitizeNoteBody(f *testing.F) {
	for _, s := range []string{
		"", "/merge", "text\n/approve", " /close", "\u200b/merge", "/\u200bmerge", "a\r\n/merge", "/\rmerge", "> /merge",
		"```\n/merge\n```", "<details>\n\n/merge\n\n</details>", "x\u2028/merge", "<!-- gitlab-mcp:op=k -->", "\r/merge", "see /merge",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got, n := sanitizeNoteBody(in)
		if n < 0 {
			t.Fatalf("negative count")
		}
		if n == 0 && got != string([]rune(in)) {
			t.Fatalf("unchanged count but different bytes: %q -> %q", in, got)
		}
		if quickActionLeft(got) {
			t.Fatalf("command line survives: %q -> %q", in, got)
		}
		if again, n2 := sanitizeNoteBody(got); again != got || n2 != 0 {
			t.Fatalf("not idempotent: %q -> %q", got, again)
		}
	})
}
