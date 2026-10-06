package tools

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
)

// traceSession serves h as GET /projects/42/jobs/3/trace and returns an MCP
// session with the pipeline tools registered (not parallel-safe, see
// diffFixture).
func traceSession(t *testing.T, h http.HandlerFunc) *mcp.ClientSession {
	t.Helper()
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/projects/42/jobs/3/trace" {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}))
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "test"}, nil)
	RegisterPipelines(srv, Deps{Config: &config.Config{Pipeline: true}, Client: cli})
	return testutil.MCPConnect(t, srv)
}

func traceCall(t *testing.T, cs *mcp.ClientSession, args map[string]any) map[string]any {
	t.Helper()
	args["project_id"], args["job_id"] = "42", 3
	return callDiffTool(t, cs, "get_pipeline_job_output", args)
}

func serveText(s string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, s) }
}

func TestTailBuffer(t *testing.T) {
	tb := &tailBuffer{max: 4}
	for _, s := range []string{"ab", "cd", "ef", "ghijklmnop", "q"} {
		if n, err := tb.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
		if len(tb.buf) > 2*tb.max {
			t.Fatalf("buffer grew to %d, want <= %d", len(tb.buf), 2*tb.max)
		}
	}
	if got := string(tb.window()); got != "nopq" || tb.total != 17 {
		t.Fatalf("window %q total %d, want %q 17", got, tb.total, "nopq")
	}
	if got := string((&tailBuffer{max: 8, buf: []byte("abc")}).window()); got != "abc" {
		t.Fatalf("short window %q", got)
	}
}

func TestLastLines(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
		cut  bool
	}{
		{"a\nb\nc\n", 2, "b\nc\n", true},
		{"a\nb\nc", 2, "b\nc", true},
		{"a\nb\n", 2, "a\nb\n", false},
		{"a\nb\n", 5, "a\nb\n", false},
		{"", 3, "", false},
	} {
		got, cut := lastLines([]byte(c.in), c.n)
		if string(got) != c.want || cut != c.cut {
			t.Errorf("lastLines(%q, %d) = %q, %v; want %q, %v", c.in, c.n, got, cut, c.want, c.cut)
		}
	}
}

// AC1: trace, truncated and total_bytes; defaults and the line window.
func TestGetPipelineJobOutput_window(t *testing.T) {
	var all strings.Builder
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&all, "line %d\n", i)
	}
	cs := traceSession(t, serveText(all.String()))

	out := traceCall(t, cs, map[string]any{})
	lines := strings.Split(strings.TrimSuffix(out["trace"].(string), "\n"), "\n")
	if len(lines) != defaultTraceTailLines || lines[0] != "line 101" || lines[199] != "line 300" {
		t.Fatalf("default window: %d lines, first %q last %q", len(lines), lines[0], lines[len(lines)-1])
	}
	if out["truncated"] != true || out["total_bytes"] != float64(all.Len()) {
		t.Fatalf("truncated=%v total_bytes=%v, want true %d", out["truncated"], out["total_bytes"], all.Len())
	}

	out = traceCall(t, cs, map[string]any{"tail_lines": 1000})
	if out["trace"] != all.String() || out["truncated"] != false {
		t.Fatalf("whole trace expected, truncated=%v", out["truncated"])
	}
}

// The byte window drops the partial leading line; max_bytes is honored and
// clamped.
func TestGetPipelineJobOutput_maxBytes(t *testing.T) {
	cs := traceSession(t, serveText("first line\nsecond line\nthird line\n"))
	out := traceCall(t, cs, map[string]any{"max_bytes": 15})
	if out["trace"] != "third line\n" || out["truncated"] != true || out["total_bytes"] != float64(34) {
		t.Fatalf("got %v", out)
	}
	big := strings.Repeat("x", 2<<20) // one huge line, no newline to resync on
	cs = traceSession(t, serveText(big))
	out = traceCall(t, cs, map[string]any{"max_bytes": 100 << 20})
	if got := len(out["trace"].(string)); got != maxTraceMaxBytes || out["truncated"] != true {
		t.Fatalf("clamp: %d bytes truncated=%v, want %d", got, out["truncated"], maxTraceMaxBytes)
	}
}

// AC3: truncate_lines still works (now as the last N lines) and an upstream
// error is returned.
func TestGetPipelineJobOutput_truncateLinesAlias(t *testing.T) {
	cs := traceSession(t, serveText("a\nb\nc\n"))
	if out := traceCall(t, cs, map[string]any{"truncate_lines": 1}); out["trace"] != "c\n" || out["truncated"] != true {
		t.Fatalf("alias: %v", out)
	}
	if out := traceCall(t, cs, map[string]any{"truncate_lines": 1, "tail_lines": 2}); out["trace"] != "b\nc\n" {
		t.Fatalf("tail_lines must win: %v", out)
	}
	cs = traceSession(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "nope", http.StatusForbidden) })
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_pipeline_job_output", Arguments: map[string]any{"project_id": "42", "job_id": 3}})
	if err != nil || !res.IsError {
		t.Fatalf("403 must surface as a tool error: err=%v res=%+v", err, res)
	}
}

// AC2: a 50 MiB trace is generated on the fly by the server (never held in
// memory or committed); the tool returns the tail with the exact total_bytes
// and allocates far less than the trace size, which a ReadAll path could not.
func TestGetPipelineJobOutput_50MBStreamed(t *testing.T) {
	const size = 50 << 20
	var written atomic.Int64
	cs := traceSession(t, func(w http.ResponseWriter, _ *http.Request) {
		chunk := bytes.Repeat([]byte(strings.Repeat("x", 63)+"\n"), 1024) // 64 KiB of 64-byte lines
		n, _ := w.Write([]byte("FIRST LINE\n"))
		written.Store(int64(n))
		for written.Load() < size {
			n, _ = w.Write(chunk)
			written.Add(int64(n))
		}
		n, _ = fmt.Fprint(w, "ERROR: job failed\n")
		written.Add(int64(n))
	})

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	out := traceCall(t, cs, map[string]any{"tail_lines": 5})
	runtime.ReadMemStats(&after)

	if got := out["trace"].(string); !strings.HasSuffix(got, "ERROR: job failed\n") || strings.Contains(got, "FIRST LINE") || strings.Count(got, "\n") != 5 {
		t.Fatalf("not the 5-line tail: %q", got)
	}
	if out["total_bytes"] != float64(written.Load()) || written.Load() < size || out["truncated"] != true {
		t.Fatalf("total_bytes=%v written=%d truncated=%v", out["total_bytes"], written.Load(), out["truncated"])
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
		t.Fatalf("allocated %d MiB for a %d MiB trace; the trace is not streamed", alloc>>20, size>>20)
	}
}
