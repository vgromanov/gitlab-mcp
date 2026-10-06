package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

// batchFile describes how the fixture answers for one path. Unknown paths are 404.
type batchFile struct {
	content []byte
	status  int    // non-zero: answer with this status (403 is not retried by the client, unlike 5xx)
	rawB64  string // non-empty: sent as content instead of the encoding of content
}

type batchFixture struct {
	cs *mcp.ClientSession

	mu   sync.Mutex
	reqs []string // "<project>|<file>|<ref>" in arrival order
}

var batchFilePath = regexp.MustCompile(`^/api/v4/projects/([^/]+)/repository/files/(.+)$`)

func newBatchFixture(t *testing.T, cfg *config.Config, files map[string]batchFile) *batchFixture {
	t.Helper()
	f := &batchFixture{}
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		m := batchFilePath.FindStringSubmatch(r.URL.Path)
		if m == nil {
			w.WriteHeader(http.StatusNotFound)
			writeFixture(w, `{"message":"404 Not Found"}`)
			return
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, m[1]+"|"+m[2]+"|"+r.URL.Query().Get("ref"))
		f.mu.Unlock()
		spec, ok := files[m[2]]
		switch {
		case !ok:
			w.WriteHeader(http.StatusNotFound)
			writeFixture(w, `{"message":"404 File Not Found"}`)
		case spec.status != 0:
			w.WriteHeader(spec.status)
			writeFixture(w, `{"message":"nope"}`)
		default:
			b64 := base64.StdEncoding.EncodeToString(spec.content)
			if spec.rawB64 != "" {
				b64 = spec.rawB64
			}
			writeFixture(w, fmt.Sprintf(`{"file_name":"x","file_path":%q,"size":%d,"encoding":"base64","content":%q,"ref":%q,"blob_id":"blob-%s","commit_id":"c","content_sha256":"s","last_commit_id":"l"}`,
				m[2], len(spec.content), b64, r.URL.Query().Get("ref"), m[2]))
		}
	}))
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "test"}, nil)
	RegisterRepository(srv, Deps{Config: cfg, Client: cli})
	f.cs = testutil.MCPConnect(t, srv)
	return f
}

func (f *batchFixture) call(t *testing.T, args map[string]any) (map[string]any, string) {
	t.Helper()
	res, err := f.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "batch_get_file_contents", Arguments: args})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		return nil, contentText(res)
	}
	m, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content %T", res.StructuredContent)
	}
	return m, ""
}

func (f *batchFixture) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

func batchEntries(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	raw, _ := out["files"].([]any)
	entries := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		entries = append(entries, r.(map[string]any))
	}
	return entries
}

func batchArgs(paths ...string) map[string]any {
	return map[string]any{"project_id": "42", "sha": testSHA, "paths": paths}
}

// A text file comes back with content, blob_id and size; one request per path,
// in order, all at the exact SHA (lower-cased); a numeric project_id is accepted.
func TestBatchGetFileContents_ok(t *testing.T) {
	f := newBatchFixture(t, &config.Config{}, map[string]batchFile{
		"src/main.go": {content: []byte("package main\n")},
		"README.md":   {content: []byte("# hi\n")},
		"empty":       {content: nil},
	})
	args := batchArgs("src/main.go", "README.md", "empty")
	args["project_id"], args["sha"] = 42, strings.ToUpper(testSHA)
	out, errText := f.call(t, args)
	if errText != "" {
		t.Fatal(errText)
	}
	if out["project_id"] != "42" || out["sha"] != testSHA || out["max_bytes_per_file"] != float64(batchFilesBytesDefault) {
		t.Fatalf("top level = %v", out)
	}
	es := batchEntries(t, out)
	if len(es) != 3 {
		t.Fatalf("entries = %d", len(es))
	}
	want := []map[string]any{
		{"path": "src/main.go", "blob_id": "blob-src/main.go", "size": float64(13), "binary": false, "truncated": false, "content": "package main\n"},
		{"path": "README.md", "blob_id": "blob-README.md", "size": float64(5), "binary": false, "truncated": false, "content": "# hi\n"},
		{"path": "empty", "blob_id": "blob-empty", "size": float64(0), "binary": false, "truncated": false, "content": ""},
	}
	for i, w := range want {
		if len(es[i]) != len(w) {
			t.Fatalf("entry %d keys = %v, want %v", i, sortedMapKeys(es[i]), sortedMapKeys(w))
		}
		for k, v := range w {
			if es[i][k] != v {
				t.Fatalf("entry %d %s = %v, want %v", i, k, es[i][k], v)
			}
		}
	}
	wantReqs := []string{"42|src/main.go|" + testSHA, "42|README.md|" + testSHA, "42|empty|" + testSHA}
	if got := f.requests(); strings.Join(got, ",") != strings.Join(wantReqs, ",") {
		t.Fatalf("requests = %v, want %v", got, wantReqs)
	}
}

// AC1: only a full commit SHA is accepted; branch/tag names, short and long
// ids are rejected before any request.
func TestBatchGetFileContents_rejectsNonSHA(t *testing.T) {
	f := newBatchFixture(t, &config.Config{}, map[string]batchFile{"a": {content: []byte("a")}})
	for _, sha := range []string{
		"main", "HEAD", "v1.0.0", "feature/x", "", "   ",
		testSHA[:39], testSHA + "0", testSHA[:7], strings.Repeat("g", 40), strings.Repeat("a", 64), "origin/" + testSHA[:33],
	} {
		args := batchArgs("a")
		args["sha"] = sha
		if _, errText := f.call(t, args); !strings.Contains(errText, "40-character") {
			t.Fatalf("sha %q: error = %q, want a 40-character SHA complaint", sha, errText)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("rejected input sent %d requests", n)
	}
	args := batchArgs("a")
	args["sha"] = " " + testSHA + " " // surrounding whitespace is trimmed, nothing else
	if out, errText := f.call(t, args); errText != "" || out["sha"] != testSHA {
		t.Fatalf("trimmed sha: %v %q", out, errText)
	}
}

// AC2: a missing path (404) or an unreadable one (403) is an error for that
// file only; the others still return and the batch does not fail.
func TestBatchGetFileContents_missingPathIsolated(t *testing.T) {
	f := newBatchFixture(t, &config.Config{}, map[string]batchFile{
		"a.txt":    {content: []byte("a")},
		"secret":   {status: http.StatusForbidden},
		"c.txt":    {content: []byte("c")},
		"badbytes": {rawB64: "!!not base64!!"},
	})
	out, errText := f.call(t, batchArgs("a.txt", "gone.txt", "secret", "c.txt", "badbytes"))
	if errText != "" {
		t.Fatal(errText)
	}
	es := batchEntries(t, out)
	if len(es) != 5 {
		t.Fatalf("entries = %d", len(es))
	}
	for _, i := range []int{0, 3} {
		if es[i]["error"] != nil || es[i]["content"] == nil || es[i]["blob_id"] == nil {
			t.Fatalf("entry %d must be a plain success: %v", i, es[i])
		}
	}
	for i, frag := range map[int]string{1: "404", 2: "403", 4: "decode content"} {
		e, _ := es[i]["error"].(string)
		if !strings.Contains(e, frag) || len(es[i]) != 2 || es[i]["path"] == nil {
			t.Fatalf("entry %d = %v, want only {path, error} mentioning %q", i, es[i], frag)
		}
	}
	if n := len(f.requests()); n != 5 {
		t.Fatalf("requests = %d, want 5 (a failure neither stops nor retries the batch)", n)
	}
}

// AC3: a binary file is flagged and its content is not returned (blob_id and
// size stay); text is not mistaken for binary.
func TestBatchGetFileContents_binary(t *testing.T) {
	late := []byte(strings.Repeat("x", batchFilesBinarySniff))
	early := append(append([]byte(nil), late[:batchFilesBinarySniff-1]...), 0) // NUL at index 7999
	late = append(late, 0)                                                     // NUL at index 8000
	f := newBatchFixture(t, &config.Config{}, map[string]batchFile{
		"logo.png":   {content: []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")},
		"latin1.txt": {content: []byte("caf\xe9")},
		"nul-7999":   {content: early},
		"nul-8000":   {content: late},
		"utf8.txt":   {content: []byte("héllo wörld ✓")},
	})
	out, errText := f.call(t, batchArgs("logo.png", "latin1.txt", "nul-7999", "nul-8000", "utf8.txt"))
	if errText != "" {
		t.Fatal(errText)
	}
	es := batchEntries(t, out)
	for i, wantBinary := range []bool{true, true, true, false, false} {
		if es[i]["binary"] != wantBinary {
			t.Fatalf("%v: binary = %v, want %v", es[i]["path"], es[i]["binary"], wantBinary)
		}
		_, hasContent := es[i]["content"]
		if hasContent == wantBinary || es[i]["blob_id"] == nil || es[i]["truncated"] != false {
			t.Fatalf("%v: content present=%v blob_id=%v truncated=%v", es[i]["path"], hasContent, es[i]["blob_id"], es[i]["truncated"])
		}
	}
	if es[0]["size"] != float64(16) {
		t.Fatalf("binary size = %v, want the blob size 16", es[0]["size"])
	}
}

// Content is cut at max_bytes_per_file without splitting a character; size
// stays the full blob size; the effective limit is echoed (default, clamp).
func TestBatchGetFileContents_truncation(t *testing.T) {
	f := newBatchFixture(t, &config.Config{}, map[string]batchFile{
		"hello": {content: []byte("hello world")},
		"exact": {content: []byte("hello")},
		"multi": {content: []byte("aé✓")}, // a=1 byte, é=2, ✓=3
		"big":   {content: []byte(strings.Repeat("y", batchFilesBytesDefault+1))},
	})
	run := func(limit any, paths ...string) (map[string]any, []map[string]any) {
		args := batchArgs(paths...)
		if limit != nil {
			args["max_bytes_per_file"] = limit
		}
		out, errText := f.call(t, args)
		if errText != "" {
			t.Fatal(errText)
		}
		return out, batchEntries(t, out)
	}
	_, es := run(5, "hello", "exact")
	if es[0]["content"] != "hello" || es[0]["truncated"] != true || es[0]["size"] != float64(11) {
		t.Fatalf("cut file = %v", es[0])
	}
	if es[1]["content"] != "hello" || es[1]["truncated"] != false {
		t.Fatalf("file of exactly the limit must not be truncated: %v", es[1])
	}
	for limit, want := range map[int]string{1: "a", 2: "a", 3: "aé", 5: "aé", 6: "aé✓"} {
		_, es = run(limit, "multi")
		if es[0]["content"] != want || es[0]["truncated"] != (limit < 6) {
			t.Fatalf("limit %d: %v, want %q", limit, es[0], want)
		}
	}
	out, es := run(nil, "big")
	if out["max_bytes_per_file"] != float64(batchFilesBytesDefault) || len(es[0]["content"].(string)) != batchFilesBytesDefault || es[0]["truncated"] != true {
		t.Fatalf("default limit: %v", out)
	}
	for _, limit := range []any{0, -3} {
		if out, _ = run(limit, "hello"); out["max_bytes_per_file"] != float64(batchFilesBytesDefault) {
			t.Fatalf("limit %v echoed %v, want the default", limit, out["max_bytes_per_file"])
		}
	}
	if out, _ = run(10<<20, "hello"); out["max_bytes_per_file"] != float64(batchFilesBytesLimit) {
		t.Fatalf("clamp echoed %v", out["max_bytes_per_file"])
	}
}

func TestBatchGetFileContents_pathLimits(t *testing.T) {
	f := newBatchFixture(t, &config.Config{}, map[string]batchFile{"a": {content: []byte("a")}})
	var many []string
	for i := 0; i < batchFilesMaxPaths+1; i++ {
		many = append(many, fmt.Sprintf("f%d", i))
	}
	for _, paths := range [][]string{nil, {}, many} {
		if _, errText := f.call(t, batchArgs(paths...)); !strings.Contains(errText, "1 to 20") {
			t.Fatalf("%d paths: error = %q", len(paths), errText)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("rejected input sent %d requests", n)
	}
	// 20 is allowed; an empty path and a duplicate are per-file matters.
	out, errText := f.call(t, batchArgs(append([]string{"a", "a", "  ", ""}, many[:16]...)...))
	if errText != "" {
		t.Fatal(errText)
	}
	es := batchEntries(t, out)
	if len(es) != 20 || es[0]["content"] != "a" || es[1]["content"] != "a" {
		t.Fatalf("entries = %d, first two %v %v", len(es), es[0], es[1])
	}
	if es[2]["error"] != "path is empty" || es[3]["error"] != "path is empty" {
		t.Fatalf("empty paths: %v %v", es[2], es[3])
	}
	if n := len(f.requests()); n != 2+16 {
		t.Fatalf("requests = %d, want 18 (duplicates read independently, empty paths skipped)", n)
	}
}

func TestBatchGetFileContents_project(t *testing.T) {
	files := map[string]batchFile{"a": {content: []byte("a")}}
	f := newBatchFixture(t, &config.Config{DefaultProjectID: "42", AllowedProjectIDs: []string{"42"}}, files)
	args := batchArgs("a")
	delete(args, "project_id")
	if out, errText := f.call(t, args); errText != "" || out["project_id"] != "42" {
		t.Fatalf("default project: %v %q", out, errText)
	}
	args["project_id"] = "43"
	if _, errText := f.call(t, args); !strings.Contains(errText, "GITLAB_ALLOWED_PROJECT_IDS") {
		t.Fatalf("disallowed project: %q", errText)
	}
	args["project_id"] = 1.5
	if _, errText := f.call(t, args); !strings.Contains(errText, "project_id") {
		t.Fatalf("bad project id: %q", errText)
	}
	if n := len(f.requests()); n != 1 {
		t.Fatalf("requests = %d, want 1 (only the allowed call reads)", n)
	}
	// No default and no project_id: an input error, no request.
	g := newBatchFixture(t, &config.Config{}, files)
	delete(args, "project_id")
	if _, errText := g.call(t, args); errText == "" || len(g.requests()) != 0 {
		t.Fatalf("missing project: %q, %d requests", errText, len(g.requests()))
	}
}
