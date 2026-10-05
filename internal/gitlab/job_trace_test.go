package gitlab

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStreamJobTrace_suffix206(t *testing.T) {
	var rng atomic.Value
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng.Store(r.Header.Get("Range"))
		w.Header().Set("Content-Range", "bytes 6-11/12")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("world\n"))
	})
	res := StreamJobTrace(context.Background(), clientWithServer(t, h), JobTraceRequest{
		ProjectID: "42", JobID: 7, SuffixBytes: 6, MaxScanBytes: 100,
	})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if got, _ := rng.Load().(string); got != "bytes=-6" {
		t.Fatalf("Range %q", got)
	}
	if string(res.Data) != "world\n" || !res.RangeHonored || !res.SuffixAnchored || !res.SizeKnown || res.Size != 12 {
		t.Fatalf("%+v %q", res, res.Data)
	}
	if res.ObservedStart == nil || *res.ObservedStart != 6 {
		t.Fatalf("start %+v", res.ObservedStart)
	}
}

func TestStreamJobTrace_suffixUndersized206Rejected(t *testing.T) {
	run := func(cr string, n int, suffix int64) JobTraceResult {
		rt := &scriptedRT{
			status:  http.StatusPartialContent,
			headers: http.Header{"Content-Range": []string{cr}},
			body:    io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("a"), n))),
		}
		return StreamJobTrace(context.Background(), clientWithRT(t, rt), JobTraceRequest{
			ProjectID: "42", JobID: 1, SuffixBytes: suffix, MaxScanBytes: 4096,
		})
	}
	short := run("bytes 990-999/1000", 10, 512)
	if short.Err == nil || len(short.Data) != 0 || short.RangeHonored || short.SuffixAnchored {
		t.Fatalf("undersized suffix honored: %+v %q", short, short.Data)
	}
	full := run("bytes 488-999/1000", 512, 512)
	if full.Err != nil || !full.SuffixAnchored || len(full.Data) != 512 {
		t.Fatalf("exact suffix %+v", full)
	}
	small := run("bytes 0-9/10", 10, 512)
	if small.Err != nil || !small.SuffixAnchored || len(small.Data) != 10 {
		t.Fatalf("whole-object suffix %+v", small)
	}
}

func TestStreamJobTrace_emptyTraceSuffix416(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes */0")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		_, _ = w.Write([]byte("range not satisfiable"))
	})
	res := StreamJobTrace(context.Background(), clientWithServer(t, h), JobTraceRequest{
		ProjectID: "42", JobID: 7, SuffixBytes: 64, MaxScanBytes: 64,
	})
	if res.Err != nil || len(res.Data) != 0 || !res.EOF || !res.SizeKnown || res.Size != 0 || !res.SuffixAnchored {
		t.Fatalf("%+v %q", res, res.Data)
	}
	h2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes */500")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	})
	res = StreamJobTrace(context.Background(), clientWithServer(t, h2), JobTraceRequest{
		ProjectID: "42", JobID: 7, SuffixBytes: 64, MaxScanBytes: 64,
	})
	if res.Err == nil {
		t.Fatalf("non-empty 416 accepted: %+v", res)
	}
	start := int64(10)
	res = StreamJobTrace(context.Background(), clientWithServer(t, h), JobTraceRequest{
		ProjectID: "42", JobID: 7, RangeStart: &start, MaxScanBytes: 64,
	})
	if res.Err == nil {
		t.Fatalf("offset range on empty trace accepted: %+v", res)
	}
}

func TestStreamJobTrace_ignoredRangeDoesNotRead(t *testing.T) {
	var n atomic.Int64
	var closes atomic.Int64
	body := &countingReadCloser{r: bytes.NewReader(bytes.Repeat([]byte("z"), 8000)), n: &n, closes: &closes}
	start := int64(10)
	end := int64(20)
	rt := &scriptedRT{status: http.StatusOK, body: body, headers: http.Header{"Content-Type": []string{"text/plain"}}}
	res := StreamJobTrace(context.Background(), clientWithRT(t, rt), JobTraceRequest{
		ProjectID: "42", JobID: 1, RangeStart: &start, RangeEndExcl: &end, MaxScanBytes: 1000,
	})
	if !errors.Is(res.Err, ErrRangeIgnored) {
		t.Fatalf("err %v", res.Err)
	}
	if n.Load() != 0 || len(res.Data) != 0 {
		t.Fatalf("read %d data %d", n.Load(), len(res.Data))
	}
}

func TestStreamJobTrace_scanCapStops(t *testing.T) {
	var n atomic.Int64
	var closes atomic.Int64
	payload := bytes.Repeat([]byte("a"), 200_000)
	body := &countingReadCloser{r: bytes.NewReader(payload), n: &n, closes: &closes}
	rt := &scriptedRT{status: http.StatusOK, body: body}
	res := StreamJobTrace(context.Background(), clientWithRT(t, rt), JobTraceRequest{
		ProjectID: "42", JobID: 1, MaxScanBytes: 1000,
	})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.Truncated || len(res.Data) != 1000 {
		t.Fatalf("trunc=%v len=%d", res.Truncated, len(res.Data))
	}
	if n.Load() > 1001 {
		t.Fatalf("scanned %d", n.Load())
	}
	if closes.Load() == 0 {
		t.Fatal("body not closed")
	}
}

func TestStreamJobTrace_unexpectedNonzero206Rejected(t *testing.T) {
	body := []byte("lpat-" + string(bytes.Repeat([]byte("a"), 20)))
	rt := &scriptedRT{
		status:  http.StatusPartialContent,
		headers: http.Header{"Content-Range": []string{"bytes 4-28/100"}},
		body:    io.NopCloser(bytes.NewReader(body)),
	}
	res := StreamJobTrace(context.Background(), clientWithRT(t, rt), JobTraceRequest{
		ProjectID: "42", JobID: 1, MaxScanBytes: 4096,
	})
	if res.Err == nil || len(res.Data) != 0 || res.RangeHonored || res.ObservedStart != nil {
		t.Fatalf("unexpected 206 accepted: %+v %q", res, res.Data)
	}
}

func TestStreamJobTrace_zeroBased206DoesNotEOFShortSpan(t *testing.T) {
	rt := &scriptedRT{
		status:  http.StatusPartialContent,
		headers: http.Header{"Content-Range": []string{"bytes 0-1/5"}},
		body:    io.NopCloser(bytes.NewReader([]byte("ab"))),
	}
	res := StreamJobTrace(context.Background(), clientWithRT(t, rt), JobTraceRequest{
		ProjectID: "42", JobID: 1, MaxScanBytes: 4096,
	})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if string(res.Data) != "ab" || !res.SizeKnown || res.Size != 5 || res.EOF || res.SuffixAnchored {
		t.Fatalf("short 206 attested as object EOF: %+v %q", res, res.Data)
	}
	full := []byte("abcde")
	rt2 := &scriptedRT{
		status:  http.StatusPartialContent,
		headers: http.Header{"Content-Range": []string{"bytes 0-4/5"}},
		body:    io.NopCloser(bytes.NewReader(full)),
	}
	res = StreamJobTrace(context.Background(), clientWithRT(t, rt2), JobTraceRequest{
		ProjectID: "42", JobID: 1, MaxScanBytes: 4096,
	})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if string(res.Data) != "abcde" || !res.EOF || !res.SizeKnown || res.Size != 5 || !res.SuffixAnchored {
		t.Fatalf("covering 206 %+v %q", res, res.Data)
	}
}

func TestStreamJobTrace_starTotal206IsNotObjectEOF(t *testing.T) {
	body := []byte("pre\nglpat-abc")
	rt := &scriptedRT{
		status:  http.StatusPartialContent,
		headers: http.Header{"Content-Range": []string{"bytes 0-12/*"}},
		body:    io.NopCloser(bytes.NewReader(body)),
	}
	res := StreamJobTrace(context.Background(), clientWithRT(t, rt), JobTraceRequest{
		ProjectID: "42", JobID: 1, MaxScanBytes: 4096,
	})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if string(res.Data) != string(body) || res.SizeKnown || res.EOF || res.SuffixAnchored {
		t.Fatalf("star-total 206 attested as object EOF: %+v %q", res, res.Data)
	}
}

func TestStreamJobTrace_boundedRangeShortProviderSpanRejected(t *testing.T) {
	start := int64(0)
	wantEnd := int64(612)
	rt := &scriptedRT{
		status:  http.StatusPartialContent,
		headers: http.Header{"Content-Range": []string{"bytes 0-49/1000"}},
		body:    io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("a"), 50))),
	}
	res := StreamJobTrace(context.Background(), clientWithRT(t, rt), JobTraceRequest{
		ProjectID: "42", JobID: 1, RangeStart: &start, RangeEndExcl: &wantEnd, MaxScanBytes: 4096,
	})
	if res.Err == nil || len(res.Data) != 0 || res.RangeHonored {
		t.Fatalf("short 206 honored: %+v %q", res, res.Data)
	}
	full := bytes.Repeat([]byte("b"), 50)
	rt2 := &scriptedRT{
		status:  http.StatusPartialContent,
		headers: http.Header{"Content-Range": []string{"bytes 0-49/50"}},
		body:    io.NopCloser(bytes.NewReader(full)),
	}
	res = StreamJobTrace(context.Background(), clientWithRT(t, rt2), JobTraceRequest{
		ProjectID: "42", JobID: 1, RangeStart: &start, RangeEndExcl: &wantEnd, MaxScanBytes: 4096,
	})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if string(res.Data) != string(full) || !res.RangeHonored || !res.EOF || !res.SizeKnown || res.Size != 50 {
		t.Fatalf("object-end 206 %+v %q", res, res.Data)
	}
	covered := bytes.Repeat([]byte("c"), 612)
	rt3 := &scriptedRT{
		status:  http.StatusPartialContent,
		headers: http.Header{"Content-Range": []string{"bytes 0-611/1000"}},
		body:    io.NopCloser(bytes.NewReader(covered)),
	}
	res = StreamJobTrace(context.Background(), clientWithRT(t, rt3), JobTraceRequest{
		ProjectID: "42", JobID: 1, RangeStart: &start, RangeEndExcl: &wantEnd, MaxScanBytes: 4096,
	})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if len(res.Data) != 612 || !res.RangeHonored || res.EOF || !res.SizeKnown {
		t.Fatalf("requested-end 206 %+v len=%d", res, len(res.Data))
	}
}

func TestStreamJobTrace_shortContentLengthIsIncomplete(t *testing.T) {
	var n atomic.Int64
	var closes atomic.Int64
	body := &countingReadCloser{r: bytes.NewReader([]byte("ok\n")), n: &n, closes: &closes}
	rt := &scriptedRT{
		status:  http.StatusOK,
		headers: http.Header{"Content-Length": []string{"1000"}},
		body:    body,
	}
	res := StreamJobTrace(context.Background(), clientWithRT(t, rt), JobTraceRequest{
		ProjectID: "42", JobID: 1, MaxScanBytes: 4096,
	})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if string(res.Data) != "ok\n" {
		t.Fatalf("data %q", res.Data)
	}
	if res.SizeKnown || res.Size != 0 || res.EOF || res.SuffixAnchored || !res.Truncated {
		t.Fatalf("short read attested as complete: %+v", res)
	}
	if closes.Load() == 0 {
		t.Fatal("body not closed")
	}

	full := []byte("ok\n")
	rt2 := &scriptedRT{
		status:  http.StatusOK,
		headers: http.Header{"Content-Length": []string{"3"}},
		body:    io.NopCloser(bytes.NewReader(full)),
	}
	res = StreamJobTrace(context.Background(), clientWithRT(t, rt2), JobTraceRequest{
		ProjectID: "42", JobID: 1, MaxScanBytes: 4096,
	})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.EOF || !res.SizeKnown || res.Size != 3 || res.Truncated || !res.SuffixAnchored {
		t.Fatalf("matching length %+v", res)
	}
}

func TestStreamJobTrace_overlongContentLengthIsIncomplete(t *testing.T) {
	var n atomic.Int64
	var closes atomic.Int64
	body := &countingReadCloser{r: bytes.NewReader([]byte("abcd")), n: &n, closes: &closes}
	rt := &scriptedRT{
		status:  http.StatusOK,
		headers: http.Header{"Content-Length": []string{"2"}},
		body:    body,
	}
	res := StreamJobTrace(context.Background(), clientWithRT(t, rt), JobTraceRequest{
		ProjectID: "42", JobID: 1, MaxScanBytes: 4096,
	})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if string(res.Data) != "abcd" {
		t.Fatalf("data %q", res.Data)
	}
	if res.SizeKnown || res.Size != 0 || res.EOF || res.SuffixAnchored || !res.Truncated {
		t.Fatalf("overlong body attested as complete: %+v", res)
	}
	if res.ObservedEndExcl == nil || *res.ObservedEndExcl != 4 {
		t.Fatalf("observed end %+v", res.ObservedEndExcl)
	}
	if closes.Load() == 0 {
		t.Fatal("body not closed")
	}

	// A scan-capped prefix of a larger object still attests Content-Length.
	payload := bytes.Repeat([]byte("a"), 200)
	rt2 := &scriptedRT{
		status:  http.StatusOK,
		headers: http.Header{"Content-Length": []string{"1000"}},
		body:    io.NopCloser(bytes.NewReader(payload)),
	}
	res = StreamJobTrace(context.Background(), clientWithRT(t, rt2), JobTraceRequest{
		ProjectID: "42", JobID: 1, MaxScanBytes: 50,
	})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.Truncated || !res.SizeKnown || res.Size != 1000 || res.EOF || len(res.Data) != 50 {
		t.Fatalf("scan-capped prefix %+v len=%d", res, len(res.Data))
	}
}

func TestStreamJobTrace_cancelClosesReader(t *testing.T) {
	body := &gateBody{started: make(chan struct{}), unblock: make(chan struct{})}
	rt := &scriptedRT{status: http.StatusOK, body: body}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan JobTraceResult, 1)
	go func() {
		done <- StreamJobTrace(ctx, clientWithRT(t, rt), JobTraceRequest{ProjectID: "42", JobID: 1, MaxScanBytes: 100})
	}()
	select {
	case <-body.started:
	case <-time.After(2 * time.Second):
		t.Fatal("read did not start")
	}
	cancel()
	select {
	case res := <-done:
		if !errors.Is(res.Err, context.Canceled) {
			t.Fatalf("err %v", res.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not stop the read")
	}
}

func TestStreamJobTrace_cancelDuringBudgetedRead(t *testing.T) {
	body := &gateBody{started: make(chan struct{}), unblock: make(chan struct{})}
	rt := &scriptedRT{status: http.StatusOK, body: body}
	b := DefaultBudget()
	b.MaxBytes = 1 << 20
	ctx, cancel := context.WithCancel(WithBudget(context.Background(), b))
	defer cancel()
	done := make(chan JobTraceResult, 1)
	go func() {
		done <- StreamJobTrace(ctx, clientWithRT(t, rt), JobTraceRequest{ProjectID: "42", JobID: 1, MaxScanBytes: 100})
	}()
	select {
	case <-body.started:
	case <-time.After(2 * time.Second):
		t.Fatal("read did not start")
	}
	cancel()
	select {
	case res := <-done:
		if !errors.Is(res.Err, context.Canceled) {
			t.Fatalf("err %v", res.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel deadlocked in cappedBody")
	}
}

func TestStreamJobTrace_206ExactSpanExtraPeekByteRejected(t *testing.T) {
	body := bytes.Repeat([]byte("a"), 101)
	rt := &scriptedRT{
		status:  http.StatusPartialContent,
		headers: http.Header{"Content-Range": []string{"bytes 0-99/500"}},
		body:    io.NopCloser(bytes.NewReader(body)),
	}
	res := StreamJobTrace(context.Background(), clientWithRT(t, rt), JobTraceRequest{
		ProjectID: "42", JobID: 1, MaxScanBytes: 100,
	})
	if res.Err == nil || len(res.Data) != 0 || res.RangeHonored || res.ObservedStart != nil {
		t.Fatalf("extra peek accepted: %+v scanned=%d", res, res.Scanned)
	}
}

func TestStreamJobTrace_capped206RejectsSpanOverflow(t *testing.T) {
	start := int64(100)
	rt := &scriptedRT{
		status:  http.StatusPartialContent,
		headers: http.Header{"Content-Range": []string{"bytes 100-101/999"}},
		body:    io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("a"), 50))),
	}
	res := StreamJobTrace(context.Background(), clientWithRT(t, rt), JobTraceRequest{
		ProjectID: "42", JobID: 1, RangeStart: &start, MaxScanBytes: 8,
	})
	if res.Err == nil || len(res.Data) != 0 || res.ObservedStart != nil || res.ObservedEndExcl != nil || res.RangeHonored {
		t.Fatalf("%+v %q", res, res.Data)
	}
}

func TestStreamJobTrace_capped206WithinSpanKeepsPrefix(t *testing.T) {
	start := int64(100)
	rt := &scriptedRT{
		status:  http.StatusPartialContent,
		headers: http.Header{"Content-Range": []string{"bytes 100-998/999"}},
		body:    io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("a"), 50))),
	}
	res := StreamJobTrace(context.Background(), clientWithRT(t, rt), JobTraceRequest{
		ProjectID: "42", JobID: 1, RangeStart: &start, MaxScanBytes: 8,
	})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.Truncated || !res.RangeHonored || len(res.Data) != 8 || res.ObservedEndExcl == nil || *res.ObservedEndExcl != 108 {
		t.Fatalf("%+v %q", res, res.Data)
	}
}

func TestStreamJobTrace_openEndedRangeShortProviderSpanRejected(t *testing.T) {
	start := int64(100)
	run := func(cr string, n int) JobTraceResult {
		rt := &scriptedRT{
			status:  http.StatusPartialContent,
			headers: http.Header{"Content-Range": []string{cr}},
			body:    io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("a"), n))),
		}
		return StreamJobTrace(context.Background(), clientWithRT(t, rt), JobTraceRequest{
			ProjectID: "42", JobID: 1, RangeStart: &start, MaxScanBytes: 4096,
		})
	}
	short := run("bytes 100-199/1000", 100)
	if short.Err == nil || len(short.Data) != 0 || short.RangeHonored {
		t.Fatalf("short open-ended 206 honored: %+v %q", short, short.Data)
	}
	whole := run("bytes 100-999/1000", 900)
	if whole.Err != nil || !whole.RangeHonored || len(whole.Data) != 900 || !whole.SizeKnown {
		t.Fatalf("object-end open-ended 206 %+v", whole)
	}
	unknown := run("bytes 100-199/*", 100)
	if unknown.Err != nil || !unknown.RangeHonored || unknown.EOF {
		t.Fatalf("unknown-total open-ended 206 %+v", unknown)
	}
}

type gateBody struct {
	started chan struct{}
	unblock chan struct{}
	once    sync.Once
	mu      sync.Mutex
	closed  bool
}

func (g *gateBody) Read(p []byte) (int, error) {
	g.once.Do(func() { close(g.started) })
	<-g.unblock
	return 0, io.EOF
}

func (g *gateBody) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.closed {
		g.closed = true
		close(g.unblock)
	}
	return nil
}
