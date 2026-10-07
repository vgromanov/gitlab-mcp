package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	glclient "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
)

// RVG-171: thread / reply / resolve guards. A tiny GitLab for MR 5 of project 1
// with stateful discussions. Failure cases use 4xx; the only 5xx is a POST
// (writes are never retried).

const (
	rtBase  = "b000000000000000000000000000000000000000"
	rtStart = "5000000000000000000000000000000000000000"
	rtHead  = "a000000000000000000000000000000000000000"
	rtOld   = "0ld0000000000000000000000000000000000000"
)

const rtProbeDiff = "@@ -12,7 +12,7 @@ l11\n l12\n l13\n l14\n-l15\n+l15-changed\n l16\n l17\n l18\n@@ -22,7 +22,6 @@ l21\n l22\n l23\n l24\n-l25\n l26\n l27\n l28\n"

type rtNote struct {
	ID         int64
	Body       string
	Resolvable bool
	Resolved   bool
}

type rtThread struct {
	ID    string
	Notes []*rtNote
}

type rtFixture struct {
	mu         sync.Mutex
	sha        string
	refs       [3]string // base, start, head
	diffs      [][]string
	threads    []*rtThread
	counts     map[string]int
	bodies     map[string]string // last request body per key
	forced     map[string]int    // key -> forced status for the response (after any store)
	forcedBody map[string]string // key -> body sent with the forced status
	hang       bool              // POST /discussions and replies: store, then hold until the client leaves
	putNoop    bool              // PUT resolve does not change the state
	stored     func(string) string
	headMove   string // MR head reported after the first write
	nextID     int64
}

func newRT() *rtFixture {
	f := &rtFixture{sha: rtHead, refs: [3]string{rtBase, rtStart, rtHead}, counts: map[string]int{}, bodies: map[string]string{}, forced: map[string]int{}, forcedBody: map[string]string{}, nextID: 100}
	f.diffs = [][]string{{
		`{"old_path":"probe.txt","new_path":"probe.txt","diff":` + strconv.Quote(rtProbeDiff) + `}`,
		`{"old_path":"old_name.txt","new_path":"new_name.txt","diff":"@@ -2,4 +2,4 @@ a\n b\n c\n d\n-e\n+e-changed\n"}`,
	}}
	return f
}

func (f *rtFixture) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[key]
}

func (f *rtFixture) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.counts {
		n += c
	}
	return n
}

func (f *rtFixture) addThread(id string, resolvable, resolved bool, body string) {
	f.nextID++
	f.threads = append(f.threads, &rtThread{ID: id, Notes: []*rtNote{{ID: f.nextID, Body: body, Resolvable: resolvable, Resolved: resolved}}})
}

func rtNoteJSON(n *rtNote) string {
	return rwJSON(map[string]any{"id": n.ID, "body": n.Body, "system": false, "resolvable": n.Resolvable, "resolved": n.Resolved, "author": map[string]any{"id": 7}})
}

func rtThreadJSON(t *rtThread) string {
	ns := make([]string, len(t.Notes))
	for i, n := range t.Notes {
		ns[i] = rtNoteJSON(n)
	}
	return fmt.Sprintf(`{"id":%q,"notes":[%s]}`, t.ID, strings.Join(ns, ","))
}

func (f *rtFixture) find(id string) *rtThread {
	for _, t := range f.threads {
		if t.ID == id {
			return t
		}
	}
	return nil
}

var (
	rtDiscRe = regexp.MustCompile(`^/api/v4/projects/1/merge_requests/5/discussions/([^/]+)(/notes)?$`)
	rtNoteRe = regexp.MustCompile(`^/api/v4/projects/1/merge_requests/5/notes/(\d+)$`)
)

func (f *rtFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	locked := true
	unlock := func() {
		if locked {
			locked = false
			f.mu.Unlock()
		}
	}
	defer unlock()
	p, m := r.URL.Path, r.Method
	reply := func(key string, status int, body string) {
		if s := f.forced[key]; s != 0 {
			status, body = s, f.forcedBody[key]
		}
		w.WriteHeader(status)
		writeFixture(w, body)
	}
	hold := func() bool {
		if !f.hang {
			return false
		}
		unlock()
		<-r.Context().Done()
		return true
	}
	var in struct {
		Body     string `json:"body"`
		Resolved *bool  `json:"resolved"`
	}
	_ = json.Unmarshal(raw, &in)
	key := ""
	switch {
	case m == http.MethodGet && p == "/api/v4/user":
		key = "GET user"
		f.counts[key]++
		reply(key, 200, `{"id":7,"username":"me"}`)
	case m == http.MethodGet && p == "/api/v4/projects/1/merge_requests/5":
		key = "GET mr"
		f.counts[key]++
		sha := f.sha
		if f.headMove != "" && f.counts["POST discussions"]+f.counts["POST reply"]+f.counts["PUT discussion"] > 0 {
			sha = f.headMove
		}
		reply(key, 200, rwJSON(map[string]any{"iid": 5, "sha": sha, "diff_refs": map[string]string{"base_sha": f.refs[0], "start_sha": f.refs[1], "head_sha": f.refs[2]}}))
	case m == http.MethodGet && strings.HasSuffix(p, "/merge_requests/5/diffs"):
		key = "GET diffs"
		f.counts[key]++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		page = max(page, 1)
		items := []string{}
		if page <= len(f.diffs) {
			items = f.diffs[page-1]
		}
		if page < len(f.diffs) {
			w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
		}
		reply(key, 200, "["+strings.Join(items, ",")+"]")
	case m == http.MethodGet && strings.HasSuffix(p, "/merge_requests/5/discussions"):
		key = "GET discussions"
		f.counts[key]++
		items := make([]string, len(f.threads))
		for i, t := range f.threads {
			items[i] = rtThreadJSON(t)
		}
		reply(key, 200, "["+strings.Join(items, ",")+"]")
	case m == http.MethodPost && strings.HasSuffix(p, "/merge_requests/5/discussions"):
		key = "POST discussions"
		f.counts[key]++
		f.bodies[key] = string(raw)
		body := in.Body
		if f.stored != nil {
			body = f.stored(body)
		}
		f.nextID++
		t := &rtThread{ID: fmt.Sprintf("d%d", f.nextID), Notes: []*rtNote{{ID: f.nextID, Body: body, Resolvable: true}}}
		f.threads = append(f.threads, t)
		if hold() {
			return
		}
		reply(key, 201, rtThreadJSON(t))
	case rtDiscRe.MatchString(p):
		sm := rtDiscRe.FindStringSubmatch(p)
		t := f.find(sm[1])
		if t == nil {
			w.WriteHeader(404)
			writeFixture(w, `{"message":"404 Not found"}`)
			return
		}
		switch {
		case sm[2] != "" && m == http.MethodPost:
			key = "POST reply"
			f.counts[key]++
			f.bodies[key] = string(raw)
			body := in.Body
			if f.stored != nil {
				body = f.stored(body)
			}
			f.nextID++
			n := &rtNote{ID: f.nextID, Body: body}
			t.Notes = append(t.Notes, n)
			if hold() {
				return
			}
			reply(key, 201, rtNoteJSON(n))
		case m == http.MethodGet:
			key = "GET discussion"
			f.counts[key]++
			reply(key, 200, rtThreadJSON(t))
		case m == http.MethodPut:
			key = "PUT discussion"
			f.counts[key]++
			f.bodies[key] = string(raw)
			res := r.URL.Query().Get("resolved") == "true"
			if in.Resolved != nil {
				res = *in.Resolved
			}
			if !f.putNoop {
				for _, n := range t.Notes {
					if n.Resolvable {
						n.Resolved = res
					}
				}
			}
			reply(key, 200, rtThreadJSON(t))
		}
	case m == http.MethodGet && rtNoteRe.MatchString(p):
		key = "GET note"
		f.counts[key]++
		id, _ := strconv.ParseInt(rtNoteRe.FindStringSubmatch(p)[1], 10, 64)
		for _, t := range f.threads {
			for _, n := range t.Notes {
				if n.ID == id {
					reply(key, 200, rtNoteJSON(n))
					return
				}
			}
		}
		w.WriteHeader(404)
		writeFixture(w, `{"message":"404 Not found"}`)
	default:
		w.WriteHeader(404)
		writeFixture(w, `{"message":"unexpected `+m+" "+p+`"}`)
	}
}

func rtDeps(t *testing.T, f *rtFixture, cfg *config.Config) Deps {
	t.Helper()
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	cfg.Token, cfg.APIURL = "x", ts.URL+"/api/v4"
	cli, err := glclient.NewClient(cfg) // the production client: writes are never retried
	if err != nil {
		t.Fatal(err)
	}
	return Deps{Config: cfg, Client: cli}
}

func rtSession(t *testing.T, f *rtFixture, cfg *config.Config) *mcp.ClientSession {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "test"}, nil)
	RegisterMRNotes(srv, rtDeps(t, f, cfg))
	return testutil.MCPConnect(t, srv)
}

func rtCall(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (map[string]any, string, bool) {
	t.Helper()
	args["project_id"], args["merge_request_iid"] = "1", 5
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	m, _ := res.StructuredContent.(map[string]any)
	return m, contentText(res), res.IsError
}

func rtReview() *config.Config { return &config.Config{ToolProfile: config.ProfileReview} }

func rtPos(over map[string]any) map[string]any {
	p := map[string]any{"position_type": "text", "base_sha": rtBase, "start_sha": rtStart, "head_sha": rtHead, "new_path": "probe.txt", "old_path": "probe.txt", "new_line": 15}
	for k, v := range over {
		if v == nil {
			delete(p, k)
		} else {
			p[k] = v
		}
	}
	return p
}

func TestThread_validAnchorIsWrittenAndReadBack(t *testing.T) {
	f := newRT()
	cs := rtSession(t, f, rtReview())
	out, text, isErr := rtCall(t, cs, "create_merge_request_thread", map[string]any{
		"body": "finding", "expected_sha": rtHead, "op_key": "run.1", "position": rtPos(nil),
	})
	if isErr {
		t.Fatalf("error: %s", text)
	}
	if out["written"] != true || out["deduplicated"] != false || out["head_sha"] != rtHead || out["head_changed_after_write"] != false || out["discussion_id"] == "" || out["note_id"] == nil {
		t.Fatalf("result: %v", out)
	}
	if n := f.count("POST discussions"); n != 1 {
		t.Fatalf("POSTs: %d", n)
	}
	var sent struct {
		Body     string         `json:"body"`
		Position map[string]any `json:"position"`
	}
	_ = json.Unmarshal([]byte(f.bodies["POST discussions"]), &sent)
	if sent.Position["head_sha"] != rtHead || sent.Position["new_line"] != float64(15) || !strings.HasPrefix(sent.Body, "finding") || !strings.Contains(sent.Body, "gitlab-mcp:op=run.1") {
		t.Fatalf("wire body: %s", f.bodies["POST discussions"])
	}
	// A general thread (no position) is guarded the same way and skips the anchor reads.
	before := f.count("GET diffs")
	if _, text, isErr := rtCall(t, cs, "create_merge_request_thread", map[string]any{"body": "general", "expected_sha": rtHead}); isErr {
		t.Fatal(text)
	}
	if f.count("GET diffs") != before {
		t.Error("a thread without a position must not read the diffs")
	}
}

func TestThread_anchorRefusalsWriteNothing(t *testing.T) {
	cases := map[string]struct {
		pos  map[string]any
		want string
	}{
		"stale head_sha":     {rtPos(map[string]any{"head_sha": rtOld}), "anchor_stale"},
		"stale base_sha":     {rtPos(map[string]any{"base_sha": rtOld}), "anchor_stale"},
		"stale start_sha":    {rtPos(map[string]any{"start_sha": rtOld}), "anchor_stale"},
		"shas omitted":       {rtPos(map[string]any{"base_sha": nil, "start_sha": nil, "head_sha": nil}), "anchor_stale"},
		"unchanged path":     {rtPos(map[string]any{"new_path": "other.txt", "old_path": "other.txt"}), "anchor_not_in_diff"},
		"no path":            {rtPos(map[string]any{"new_path": nil, "old_path": nil}), "anchor_invalid"},
		"context new only":   {rtPos(map[string]any{"new_line": 13}), "anchor_not_in_diff"},
		"unchanged new only": {rtPos(map[string]any{"new_line": 3}), "anchor_not_in_diff"},
		"mismatched pair":    {rtPos(map[string]any{"new_line": 13, "old_line": 14}), "anchor_not_in_diff"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRT()
			cs := rtSession(t, f, rtReview())
			_, text, isErr := rtCall(t, cs, "create_merge_request_thread", map[string]any{"body": "x", "expected_sha": rtHead, "op_key": "k", "position": c.pos})
			if !isErr || !strings.Contains(text, c.want) || !strings.Contains(text, "nothing was written") {
				t.Fatalf("want %s, got isErr=%v %q", c.want, isErr, text)
			}
			if strings.Contains(text, "may have been applied") {
				t.Errorf("an anchor refusal wrote nothing, the retry hint would be false: %q", text)
			}
			if f.count("POST discussions") != 0 {
				t.Fatalf("a refused anchor must not POST")
			}
			if c.want == "anchor_stale" && !strings.Contains(text, rtHead) {
				t.Errorf("the current diff_refs must be in the message: %q", text)
			}
		})
	}
}

func TestThread_anchorShapesAllowed(t *testing.T) {
	cases := map[string]map[string]any{
		"added line":         rtPos(nil),
		"removed line":       rtPos(map[string]any{"new_line": nil, "old_line": 25}),
		"context pair":       rtPos(map[string]any{"new_line": 13, "old_line": 13}),
		"shifted context":    rtPos(map[string]any{"new_line": 25, "old_line": 26}),
		"unfolded context":   rtPos(map[string]any{"new_line": 3, "old_line": 3}),
		"renamed file":       rtPos(map[string]any{"new_path": "new_name.txt", "old_path": "old_name.txt", "new_line": 5}),
		"old path only":      rtPos(map[string]any{"new_path": nil, "old_path": "old_name.txt", "new_line": 5}),
		"file level":         rtPos(map[string]any{"position_type": "file", "new_line": nil}),
		"position_type text": rtPos(map[string]any{"position_type": "text"}),
	}
	for name, pos := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRT()
			cs := rtSession(t, f, rtReview())
			_, text, isErr := rtCall(t, cs, "create_merge_request_thread", map[string]any{"body": "x", "expected_sha": rtHead, "position": pos})
			if isErr || f.count("POST discussions") != 1 {
				t.Fatalf("isErr=%v POSTs=%d %q", isErr, f.count("POST discussions"), text)
			}
		})
	}
	// A collapsed diff cannot be parsed: the path is checked, the line is left to GitLab.
	f := newRT()
	f.diffs = [][]string{{`{"old_path":"probe.txt","new_path":"probe.txt","diff":"","collapsed":true}`}}
	cs := rtSession(t, f, rtReview())
	if _, text, isErr := rtCall(t, cs, "create_merge_request_thread", map[string]any{"body": "x", "expected_sha": rtHead, "position": rtPos(map[string]any{"new_line": 99})}); isErr {
		t.Fatalf("collapsed diff: %s", text)
	}
}

func TestThread_anchorDiffPaging(t *testing.T) {
	filler := func(i int) []string {
		return []string{fmt.Sprintf(`{"old_path":"f%d","new_path":"f%d","diff":""}`, i, i)}
	}
	// Found on page 2.
	f := newRT()
	f.diffs = [][]string{filler(1), newRT().diffs[0]}
	cs := rtSession(t, f, rtReview())
	if _, text, isErr := rtCall(t, cs, "create_merge_request_thread", map[string]any{"body": "x", "expected_sha": rtHead, "position": rtPos(nil)}); isErr || f.count("GET diffs") != 2 {
		t.Fatalf("page 2: isErr=%v diffs=%d %q", isErr, f.count("GET diffs"), text)
	}
	// Not found and more pages than the cap: unverifiable, never "assume ok".
	f = newRT()
	f.diffs = nil
	for i := 0; i < anchorScanPages+2; i++ {
		f.diffs = append(f.diffs, filler(i))
	}
	cs = rtSession(t, f, rtReview())
	_, text, isErr := rtCall(t, cs, "create_merge_request_thread", map[string]any{"body": "x", "expected_sha": rtHead, "position": rtPos(nil)})
	if !isErr || !strings.Contains(text, "anchor_unverifiable") || f.count("POST discussions") != 0 || f.count("GET diffs") != anchorScanPages {
		t.Fatalf("cap: isErr=%v diffs=%d POSTs=%d %q", isErr, f.count("GET diffs"), f.count("POST discussions"), text)
	}
}

func TestThread_gitlabWriteErrorsAreMapped(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		want   string
		hint   bool
	}{
		{400, `{"message":"400 Bad request - Note {:line_code=>[\"can't be blank\", \"must be a valid line code\"]}"}`, "anchor_invalid", false},
		{500, `{"message":"500 Internal Server Error"}`, "gitlab_error", true},
	} {
		f := newRT()
		f.forced["POST discussions"], f.forcedBody["POST discussions"] = c.status, c.body
		cs := rtSession(t, f, rtReview())
		// The anchor passes the guard; GitLab's own answer to the POST is mapped.
		_, text, isErr := rtCall(t, cs, "create_merge_request_thread", map[string]any{"body": "x", "expected_sha": rtHead, "op_key": "k", "position": rtPos(nil)})
		if !isErr || !strings.Contains(text, c.want) || strings.Contains(text, "may have been applied") != c.hint {
			t.Errorf("%d: want %s (hint=%v), got isErr=%v %q", c.status, c.want, c.hint, isErr, text)
		}
		if f.count("POST discussions") != 1 {
			t.Errorf("%d: the POST must be sent exactly once (no retry), got %d", c.status, f.count("POST discussions"))
		}
	}
}

func TestReviewWrites_requireExpectedSHAAndRefuseStale(t *testing.T) {
	calls := map[string]map[string]any{
		"create_merge_request_thread":          {"body": "x"},
		"create_merge_request_discussion_note": {"body": "x", "discussion_id": "d1"},
		"resolve_merge_request_thread":         {"discussion_id": "d1", "resolved": true},
	}
	for tool, args := range calls {
		t.Run(tool, func(t *testing.T) {
			f := newRT()
			f.addThread("d1", true, false, "first")
			cs := rtSession(t, f, rtReview())
			_, text, isErr := rtCall(t, cs, tool, cloneArgs(args))
			if !isErr || !strings.Contains(text, "expected_sha is required") || f.total() != 0 {
				t.Fatalf("missing expected_sha: isErr=%v requests=%d %q", isErr, f.total(), text)
			}
			stale := cloneArgs(args)
			stale["expected_sha"] = rtOld
			_, text, isErr = rtCall(t, cs, tool, stale)
			if !isErr || !strings.Contains(text, "head_changed") || !strings.Contains(text, rtHead) {
				t.Fatalf("stale expected_sha: isErr=%v %q", isErr, text)
			}
			if f.count("POST discussions")+f.count("POST reply")+f.count("PUT discussion") != 0 {
				t.Fatalf("a stale head must not write")
			}
		})
	}
}

func cloneArgs(m map[string]any) map[string]any {
	c := map[string]any{}
	for k, v := range m {
		c[k] = v
	}
	return c
}

func TestReply_writesDedupesAndSurfacesHelperFields(t *testing.T) {
	f := newRT()
	f.addThread("d1", true, false, "first")
	cs := rtSession(t, f, rtReview())
	args := func() map[string]any {
		return map[string]any{"discussion_id": "d1", "body": "reply text", "expected_sha": rtHead, "op_key": "rep-1"}
	}
	out, text, isErr := rtCall(t, cs, "create_merge_request_discussion_note", args())
	if isErr || out["written"] != true || out["discussion_id"] != "d1" || out["deduplicated"] != false {
		t.Fatalf("reply: isErr=%v %v %q", isErr, out, text)
	}
	out, _, isErr = rtCall(t, cs, "create_merge_request_discussion_note", args())
	if isErr || out["deduplicated"] != true || out["written"] != false || f.count("POST reply") != 1 {
		t.Fatalf("retry: %v replies=%d", out, f.count("POST reply"))
	}
	if len(f.find("d1").Notes) != 2 {
		t.Fatalf("exactly one reply expected, notes=%d", len(f.find("d1").Notes))
	}
}

func TestReply_headMovedAfterWriteAndStoredBodyDiffer(t *testing.T) {
	f := newRT()
	f.addThread("d1", true, false, "first")
	f.headMove = "c000000000000000000000000000000000000000"
	cs := rtSession(t, f, rtReview())
	out, _, isErr := rtCall(t, cs, "create_merge_request_discussion_note", map[string]any{"discussion_id": "d1", "body": "x", "expected_sha": rtHead})
	if isErr || out["head_changed_after_write"] != true || out["head_sha"] != f.headMove {
		t.Fatalf("head moved after write: isErr=%v %v", isErr, out)
	}
	f = newRT()
	f.addThread("d1", true, false, "first")
	f.stored = func(string) string { return "different" }
	cs = rtSession(t, f, rtReview())
	out, text, isErr := rtCall(t, cs, "create_merge_request_discussion_note", map[string]any{"discussion_id": "d1", "body": "kept line\nvanished line", "expected_sha": rtHead})
	if !isErr || out["written"] != true || !strings.Contains(text, "stored_body_differs") {
		t.Fatalf("the detective signal must be an error with written=true: isErr=%v %q", isErr, text)
	}
}

func TestThreadAndReply_bodyIsNeutralizedAndNotEchoed(t *testing.T) {
	f := newRT()
	f.addThread("d1", true, false, "first")
	cs := rtSession(t, f, rtReview())
	body := "/title Changed\nfine line"
	for tool, args := range map[string]map[string]any{
		"create_merge_request_thread":          {"body": body, "expected_sha": rtHead},
		"create_merge_request_discussion_note": {"body": body, "expected_sha": rtHead, "discussion_id": "d1"},
	} {
		out, text, isErr := rtCall(t, cs, tool, args)
		if isErr || out["body_modified"] != true || out["lines_changed"] != float64(1) {
			t.Fatalf("%s: isErr=%v %v", tool, isErr, out)
		}
		if strings.Contains(strings.ToLower(text), "title") || strings.Contains(text, "Changed") {
			t.Errorf("%s: the result must not echo the matched text: %q", tool, text)
		}
	}
	for _, key := range []string{"POST discussions", "POST reply"} {
		var sent struct{ Body string }
		_ = json.Unmarshal([]byte(f.bodies[key]), &sent)
		if strings.HasPrefix(sent.Body, "/") || !strings.HasPrefix(sent.Body, `\/title`) {
			t.Errorf("%s: the wire body must carry the neutralised text, got %q", key, sent.Body)
		}
	}
}

func TestOpKeyRetryAfterTimeoutYieldsOneThreadAndOneReply(t *testing.T) {
	f := newRT()
	f.addThread("d1", true, false, "first")
	d := rtDeps(t, f, rtReview())
	run := func(name string, call func(ctx context.Context) (*mcp.CallToolResult, any, error)) {
		f.mu.Lock()
		f.hang = true
		f.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		defer cancel()
		_, _, err := call(ctx)
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "retrying with the same op_key will not duplicate") {
			t.Fatalf("%s first call: want a deadline error with the retry hint, got %v", name, err)
		}
		f.mu.Lock()
		f.hang = false
		f.mu.Unlock()
		_, out, err := call(context.Background())
		res, _ := out.(*guardedResult)
		if err != nil || res == nil || !res.Deduplicated || res.Written {
			t.Fatalf("%s retry: want deduplicated, got %v %v", name, out, err)
		}
	}
	run("thread", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
		return guardedThread(ctx, d, "1", createMergeRequestThreadIn{pidMR: pidMR{MergeRequestIID: 5}, Body: "finding", ExpectedSHA: gitlab.Ptr(rtHead), OpKey: gitlab.Ptr("t1")})
	})
	run("reply", func(ctx context.Context) (*mcp.CallToolResult, any, error) {
		return guardedReply(ctx, d, "1", createMergeRequestDiscussionNoteIn{pidMR: pidMR{MergeRequestIID: 5}, DiscussionID: "d1", Body: "reply", ExpectedSHA: gitlab.Ptr(rtHead), OpKey: gitlab.Ptr("r1")})
	})
	if f.count("POST discussions") != 1 || f.count("POST reply") != 1 {
		t.Fatalf("exactly one thread and one reply must exist, POSTs: thread=%d reply=%d", f.count("POST discussions"), f.count("POST reply"))
	}
	if len(f.threads) != 2 || len(f.find("d1").Notes) != 2 {
		t.Fatalf("threads=%d notes in d1=%d", len(f.threads), len(f.find("d1").Notes))
	}
}

func TestResolve_idempotentAndReadBack(t *testing.T) {
	f := newRT()
	f.addThread("d1", true, false, "first")
	cs := rtSession(t, f, rtReview())
	args := func(resolved bool) map[string]any {
		return map[string]any{"discussion_id": "d1", "resolved": resolved, "expected_sha": rtHead}
	}
	out, text, isErr := rtCall(t, cs, "resolve_merge_request_thread", args(true))
	if isErr || out["written"] != true || out["resolved"] != true || out["already_in_state"] != false || out["head_sha"] != rtHead || out["head_changed_after_write"] != false {
		t.Fatalf("resolve: isErr=%v %v %q", isErr, out, text)
	}
	if f.count("PUT discussion") != 1 || f.count("GET discussion") != 2 {
		t.Fatalf("one PUT and a read before and after expected: PUT=%d GET=%d", f.count("PUT discussion"), f.count("GET discussion"))
	}
	out, _, isErr = rtCall(t, cs, "resolve_merge_request_thread", args(true))
	if isErr || out["written"] != false || out["already_in_state"] != true || out["resolved"] != true || f.count("PUT discussion") != 1 {
		t.Fatalf("already resolved must not write: %v PUTs=%d", out, f.count("PUT discussion"))
	}
	out, _, isErr = rtCall(t, cs, "resolve_merge_request_thread", args(false))
	if isErr || out["written"] != true || out["resolved"] != false || f.count("PUT discussion") != 2 {
		t.Fatalf("unresolve: %v PUTs=%d", out, f.count("PUT discussion"))
	}
}

func TestResolve_failuresAreSurfaced(t *testing.T) {
	// The write is accepted but the state does not change: an error, never success.
	f := newRT()
	f.addThread("d1", true, false, "first")
	f.putNoop = true
	cs := rtSession(t, f, rtReview())
	out, text, isErr := rtCall(t, cs, "resolve_merge_request_thread", map[string]any{"discussion_id": "d1", "resolved": true, "expected_sha": rtHead})
	if !isErr || !strings.Contains(text, "state_differs") || out["written"] != true || out["resolved"] != false {
		t.Fatalf("state_differs: isErr=%v %v %q", isErr, out, text)
	}
	// A thread with no resolvable note.
	f = newRT()
	f.addThread("d2", false, false, "plain note")
	cs = rtSession(t, f, rtReview())
	_, text, isErr = rtCall(t, cs, "resolve_merge_request_thread", map[string]any{"discussion_id": "d2", "resolved": true, "expected_sha": rtHead})
	if !isErr || !strings.Contains(text, "not_resolvable") || f.count("PUT discussion") != 0 {
		t.Fatalf("not_resolvable: isErr=%v %q", isErr, text)
	}
	// Unknown discussion: GitLab's 404 comes back, nothing is written.
	_, text, isErr = rtCall(t, cs, "resolve_merge_request_thread", map[string]any{"discussion_id": "nope", "resolved": true, "expected_sha": rtHead})
	if !isErr || f.count("PUT discussion") != 0 {
		t.Fatalf("404: isErr=%v %q", isErr, text)
	}
	// The head moves after the write.
	f = newRT()
	f.addThread("d1", true, false, "first")
	f.headMove = "c000000000000000000000000000000000000000"
	cs = rtSession(t, f, rtReview())
	out, _, isErr = rtCall(t, cs, "resolve_merge_request_thread", map[string]any{"discussion_id": "d1", "resolved": true, "expected_sha": rtHead})
	if isErr || out["head_changed_after_write"] != true || out["head_sha"] != f.headMove {
		t.Fatalf("head moved: isErr=%v %v", isErr, out)
	}
}

func TestDailyProfileKeepsToday(t *testing.T) {
	f := newRT()
	f.addThread("d1", true, false, "first")
	cs := rtSession(t, f, &config.Config{})
	// No expected_sha, no preflight, raw SDK output.
	out, text, isErr := rtCall(t, cs, "create_merge_request_thread", map[string]any{"body": "x"})
	if isErr || out["id"] == nil || out["written"] != nil || f.count("GET mr") != 0 || f.count("GET user") != 0 {
		t.Fatalf("thread: isErr=%v %v %q mrReads=%d", isErr, out, text, f.count("GET mr"))
	}
	out, _, isErr = rtCall(t, cs, "create_merge_request_discussion_note", map[string]any{"discussion_id": "d1", "body": "x"})
	if isErr || out["body"] != "x" || out["written"] != nil || f.count("GET mr") != 0 {
		t.Fatalf("reply: isErr=%v %v", isErr, out)
	}
	out, _, isErr = rtCall(t, cs, "resolve_merge_request_thread", map[string]any{"discussion_id": "d1", "resolved": true})
	if isErr || out["id"] != "d1" || out["written"] != nil || f.count("GET mr") != 0 || f.count("PUT discussion") != 1 || f.count("GET discussion") != 0 {
		t.Fatalf("resolve: isErr=%v %v PUT=%d", isErr, out, f.count("PUT discussion"))
	}
	// Even a stale position goes straight through (GitLab decides).
	if _, text, isErr := rtCall(t, cs, "create_merge_request_thread", map[string]any{"body": "x", "position": rtPos(map[string]any{"head_sha": rtOld})}); isErr || f.count("GET diffs") != 0 {
		t.Fatalf("daily position: isErr=%v %q", isErr, text)
	}
}

func TestToolDescriptionsPerProfile(t *testing.T) {
	desc := func(cfg *config.Config) map[string]string {
		cs := rtSession(t, newRT(), cfg)
		res, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		for _, tl := range res.Tools {
			m[tl.Name] = tl.Description
		}
		return m
	}
	rev, day := desc(rtReview()), desc(&config.Config{})
	for _, name := range []string{"create_merge_request_thread", "create_merge_request_discussion_note", "resolve_merge_request_thread"} {
		if !strings.Contains(rev[name], "REQUIRED expected_sha") || strings.Contains(day[name], "expected_sha") {
			t.Errorf("%s: review %q / daily %q", name, rev[name], day[name])
		}
		for _, bad := range []string{"quick", "slash", "command"} {
			if strings.Contains(strings.ToLower(rev[name]), bad) {
				t.Errorf("%s: the description must not mention %q", name, bad)
			}
		}
	}
}

func TestLineInDiff(t *testing.T) {
	for _, c := range []struct {
		name     string
		old, new int64
		want     bool
	}{
		{"added line", 0, 15, true},
		{"added line used as pair", 15, 15, false},
		{"removed line", 15, 0, true},
		{"removed line 25", 25, 0, true},
		{"context new only", 0, 13, false},
		{"context old only", 13, 0, false},
		{"context pair", 13, 13, true},
		{"shifted context pair", 26, 25, true},
		{"mismatch inside hunk", 14, 13, false},
		{"unfolded pair before", 3, 3, true},
		{"unfolded pair between", 20, 20, true},
		{"unfolded pair after", 40, 39, true},
		{"unfolded new only", 0, 3, false},
		{"unfolded old only", 3, 0, false},
		{"new only on the other side's number", 0, 25, false},
	} {
		if got := lineInDiff(rtProbeDiff, c.old, c.new); got != c.want {
			t.Errorf("%s (old %d new %d): got %v want %v", c.name, c.old, c.new, got, c.want)
		}
	}
	if !lineInDiff("@@ -1 +1 @@\n-a\n+b\n\\ No newline at end of file\n", 1, 0) || !lineInDiff("@@ -1 +1 @@\n-a\n+b\n\\ No newline at end of file\n", 0, 1) {
		t.Error("headers without a length and the no-newline marker must parse")
	}
}
