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
