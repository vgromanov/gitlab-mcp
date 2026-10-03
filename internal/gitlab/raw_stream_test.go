package gitlab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

type countingReadCloser struct {
	r      io.Reader
	n      *atomic.Int64
	closes *atomic.Int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func (c *countingReadCloser) Close() error {
	c.closes.Add(1)
	if closer, ok := c.r.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

type scriptedRT struct {
	status  int
	headers http.Header
	body    io.ReadCloser
	sawReq  atomic.Int64
}

func (s *scriptedRT) RoundTrip(req *http.Request) (*http.Response, error) {
	s.sawReq.Add(1)
	h := http.Header{}
	if s.headers != nil {
		h = s.headers.Clone()
	}
	return &http.Response{
		StatusCode:    s.status,
		Header:        h,
		Body:          s.body,
		Request:       req,
		ContentLength: -1,
	}, nil
}

func clientWithRT(t *testing.T, rt http.RoundTripper) *gitlab.Client {
	t.Helper()
	c, err := gitlab.NewClient("t",
		gitlab.WithBaseURL("https://example.test/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithHTTPClient(&http.Client{Transport: BudgetInterceptor()(rt)}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func clientWithServer(t *testing.T, h http.Handler) *gitlab.Client {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	c, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestStreamRawFile_SDK206BoundedCaptureThroughCheckResponse(t *testing.T) {
	// httptest 206 must traverse real Client.Do / CheckResponse (which rejects 206).
	var hits atomic.Int64
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Range") != "bytes=0-9" {
			t.Errorf("Range=%q", r.Header.Get("Range"))
		}
		w.Header().Set("Content-Range", "bytes 0-9/500")
		w.Header().Set("X-Gitlab-Size", "500")
		w.Header().Set("X-Gitlab-Blob-Id", strings.Repeat("ab", 20))
		w.Header().Set("X-Gitlab-Content-Sha256", strings.Repeat("cd", 32))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "0123456789"+strings.Repeat("X", 256*1024))
	})
	client := clientWithServer(t, h)
	parent := context.Background()
	ctx := WithBudget(parent, DefaultBudget())
	end := int64(10)
	res := StreamRawFile(ctx, client, RawStreamRequest{
		ProjectID: "42", FilePath: "f.txt", Ref: strings.Repeat("b", 40),
		RangeStart: int64Ptr(0), RangeEnd: &end, MaxReturnBytes: 1 << 20,
	})
	if res.Err != nil {
		t.Fatalf("err=%v", res.Err)
	}
	if res.Status != 206 {
		t.Fatalf("status=%d want 206 observed through SDK", res.Status)
	}
	if string(res.Data) != "0123456789" {
		t.Fatalf("data=%q", res.Data)
	}
	if !res.RangeHonored || !res.WindowComplete || !res.WriterSatisfied {
		t.Fatalf("honored=%v complete=%v satisfied=%v", res.RangeHonored, res.WindowComplete, res.WriterSatisfied)
	}
	if res.BlobID == "" || res.ContentSHA256 == "" {
		t.Fatal("expected attested blob/digest headers")
	}
	if parent.Err() != nil {
		t.Fatal("invocation must stay open for siblings")
	}
	if hits.Load() != 1 {
		t.Fatalf("hits=%d", hits.Load())
	}
}

func TestStreamRawFile_SDK206GiantBodyBoundedNotReadAll(t *testing.T) {
	var bytesWritten atomic.Int64
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-99/100000000")
		w.Header().Set("X-Gitlab-Size", "100000000")
		w.WriteHeader(http.StatusPartialContent)
		fl := w.(http.Flusher)
		n, _ := io.WriteString(w, strings.Repeat("A", 100))
		bytesWritten.Add(int64(n))
		fl.Flush()
		// Attempt to push giant remainder; client must stop/close early.
		for i := 0; i < 1000; i++ {
			n, err := io.WriteString(w, strings.Repeat("G", 4096))
			bytesWritten.Add(int64(n))
			if err != nil {
				return
			}
			fl.Flush()
		}
	})
	client := clientWithServer(t, h)
	ctx := WithBudget(context.Background(), DefaultBudget())
	end := int64(100)
	start := time.Now()
	res := StreamRawFile(ctx, client, RawStreamRequest{
		ProjectID: "42", FilePath: "big.bin", Ref: strings.Repeat("a", 40),
		RangeStart: int64Ptr(0), RangeEnd: &end, MaxReturnBytes: 1 << 20,
	})
	if res.Err != nil {
		t.Fatalf("err=%v", res.Err)
	}
	if res.Status != 206 || len(res.Data) != 100 {
		t.Fatalf("status=%d len=%d", res.Status, len(res.Data))
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("took too long — likely unbounded ReadAll of giant 206 body")
	}
	// Server may write more than 100 before TCP backpressure, but must not complete full giant.
	if bytesWritten.Load() >= 1000*4096 {
		t.Fatalf("server wrote entire giant payload (%d); client did not stop", bytesWritten.Load())
	}
}

func TestStreamRawFile_giantBodyStopsClosesWithoutInvocationCancel(t *testing.T) {
	var bytesRead, closes atomic.Int64
	gated := newPrefixGateReader([]byte(strings.Repeat("A", 100)))
	tracked := &countingReadCloser{r: gated, n: &bytesRead, closes: &closes}
	rt := &scriptedRT{
		status: http.StatusOK,
		headers: http.Header{
			"Content-Type":  []string{"application/octet-stream"},
			"X-Gitlab-Size": []string{"100000000"},
		},
		body: tracked,
	}
	client := clientWithRT(t, rt)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &Budget{MaxBytes: 8 << 20, MaxRequests: 16, MaxElapsed: time.Minute, MaxItems: 20, start: time.Now()}
	ctx := WithBudget(parent, b)
	end := int64(100)
	res := StreamRawFile(ctx, client, RawStreamRequest{
		ProjectID: "42", FilePath: "big.bin", Ref: strings.Repeat("a", 40),
		RangeStart: int64Ptr(0), RangeEnd: &end, MaxReturnBytes: 1 << 20,
	})
	if res.Err != nil {
		t.Fatalf("err=%v", res.Err)
	}
	if parent.Err() != nil {
		t.Fatal("ordinary window stop must not cancel parent")
	}
	if int64(len(res.Data)) != 100 || !res.WriterSatisfied {
		t.Fatalf("len=%d satisfied=%v", len(res.Data), res.WriterSatisfied)
	}
	if closes.Load() < 1 {
		t.Fatalf("expected Body.Close, closes=%d", closes.Load())
	}
	if bytesRead.Load() != 100 {
		t.Fatalf("bytesRead=%d want 100", bytesRead.Load())
	}
}

func TestStreamRawFile_RT206ClosesTrackedBody(t *testing.T) {
	var bytesRead, closes atomic.Int64
	gated := newPrefixGateReader([]byte("0123456789"))
	tracked := &countingReadCloser{r: gated, n: &bytesRead, closes: &closes}
	rt := &scriptedRT{
		status: http.StatusPartialContent,
		headers: http.Header{
			"Content-Range": []string{"bytes 0-9/1000"},
			"X-Gitlab-Size": []string{"1000"},
		},
		body: tracked,
	}
	client := clientWithRT(t, rt)
	ctx := WithBudget(context.Background(), DefaultBudget())
	end := int64(10)
	res := StreamRawFile(ctx, client, RawStreamRequest{
		ProjectID: "42", FilePath: "f.txt", Ref: strings.Repeat("b", 40),
		RangeStart: int64Ptr(0), RangeEnd: &end, MaxReturnBytes: 1 << 20,
	})
	if res.Err != nil || res.Status != 206 {
		t.Fatalf("err=%v status=%d", res.Err, res.Status)
	}
	if closes.Load() < 1 || bytesRead.Load() != 10 {
		t.Fatalf("closes=%d bytesRead=%d", closes.Load(), bytesRead.Load())
	}
}

func TestStreamRawFile_writerTruncationDistinctFromSatisfied(t *testing.T) {
	var bytesRead, closes atomic.Int64
	tracked := &countingReadCloser{
		r: strings.NewReader(strings.Repeat("Z", 1000)), n: &bytesRead, closes: &closes,
	}
	rt := &scriptedRT{
		status:  http.StatusOK,
		headers: http.Header{"X-Gitlab-Size": []string{"1000"}},
		body:    tracked,
	}
	client := clientWithRT(t, rt)
	ctx := WithBudget(context.Background(), DefaultBudget())
	res := StreamRawFile(ctx, client, RawStreamRequest{
		ProjectID: "42", FilePath: "f.bin", Ref: strings.Repeat("c", 40),
		MaxReturnBytes: 50,
	})
	if res.Err != nil || !res.WriterTruncated || res.WriterSatisfied {
		t.Fatalf("err=%v trunc=%v sat=%v", res.Err, res.WriterTruncated, res.WriterSatisfied)
	}
	if closes.Load() < 1 {
		t.Fatal("expected Close")
	}
}

func TestStreamRawFile_contentRangeEndGeTotalRejected(t *testing.T) {
	var n, c atomic.Int64
	rt := &scriptedRT{
		status:  http.StatusPartialContent,
		headers: http.Header{"Content-Range": []string{"bytes 0-99/99"}},
		body:    &countingReadCloser{r: strings.NewReader(strings.Repeat("x", 100)), n: &n, closes: &c},
	}
	client := clientWithRT(t, rt)
	ctx := WithBudget(context.Background(), DefaultBudget())
	end := int64(100)
	res := StreamRawFile(ctx, client, RawStreamRequest{
		ProjectID: "42", FilePath: "f", Ref: strings.Repeat("d", 40),
		RangeStart: int64Ptr(0), RangeEnd: &end, MaxReturnBytes: 1 << 20,
	})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "end >= total") {
		t.Fatalf("got %v", res.Err)
	}
}

func TestStreamRawFile_contentRangeEndOverflowRejected(t *testing.T) {
	var n, c atomic.Int64
	rt := &scriptedRT{
		status:  http.StatusPartialContent,
		headers: http.Header{"Content-Range": []string{fmt.Sprintf("bytes 0-%d/*", int64(^uint64(0)>>1))}},
		body:    &countingReadCloser{r: strings.NewReader("x"), n: &n, closes: &c},
	}
	client := clientWithRT(t, rt)
	ctx := WithBudget(context.Background(), DefaultBudget())
	end := int64(1)
	res := StreamRawFile(ctx, client, RawStreamRequest{
		ProjectID: "42", FilePath: "f", Ref: strings.Repeat("e", 40),
		RangeStart: int64Ptr(0), RangeEnd: &end, MaxReturnBytes: 1 << 20,
	})
	if res.Err == nil {
		t.Fatal("expected overflow error")
	}
}

func TestStreamRawFile_parentCancelNotClearedByRawStop(t *testing.T) {
	var closes atomic.Int64
	br := &blockUntilCancelReader{closes: &closes, started: make(chan struct{})}
	rt := &scriptedRTWithReqCtx{
		status:  http.StatusOK,
		headers: http.Header{"Content-Type": []string{"application/octet-stream"}},
		body:    br,
	}
	client := clientWithRT(t, rt)
	parent, cancel := context.WithCancel(context.Background())
	ctx := WithBudget(parent, DefaultBudget())
	errCh := make(chan RawStreamResult, 1)
	go func() {
		errCh <- StreamRawFile(ctx, client, RawStreamRequest{
			ProjectID: "42", FilePath: "f", Ref: strings.Repeat("f", 40),
			MaxReturnBytes: 1 << 20,
		})
	}()
	select {
	case <-br.started:
	case <-time.After(3 * time.Second):
		t.Fatal("body Read did not start")
	}
	cancel()
	res := <-errCh
	if !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("want parent cancellation, got %v", res.Err)
	}
}

func TestBudget_CapLimitsSyncSafeTightenOnly(t *testing.T) {
	b := &Budget{MaxItems: 100, MaxBytes: 16 << 20, MaxRequests: 32, MaxElapsed: time.Hour}
	b.CapLimits(20, 8<<20, 16)
	items, bytes, reqs, elapsed := b.LimitsSnapshot()
	if items != 20 || bytes != 8<<20 || reqs != 16 || elapsed != time.Hour {
		t.Fatalf("%d %d %d %v", items, bytes, reqs, elapsed)
	}
	b.CapLimits(50, 16<<20, 32)
	items, bytes, reqs, _ = b.LimitsSnapshot()
	if items != 20 || bytes != 8<<20 || reqs != 16 {
		t.Fatalf("must not relax: %d %d %d", items, bytes, reqs)
	}
}

type prefixGateReader struct {
	prefix []byte
	off    int
	gate   chan struct{}
	closed atomic.Bool
}

func newPrefixGateReader(prefix []byte) *prefixGateReader {
	return &prefixGateReader{prefix: prefix, gate: make(chan struct{})}
}

func (g *prefixGateReader) Read(p []byte) (int, error) {
	if g.off < len(g.prefix) {
		n := copy(p, g.prefix[g.off:])
		g.off += n
		return n, nil
	}
	<-g.gate
	return 0, errors.New("closed")
}

func (g *prefixGateReader) Close() error {
	if g.closed.CompareAndSwap(false, true) {
		close(g.gate)
	}
	return nil
}

type blockUntilCancelReader struct {
	started chan struct{}
	closes  *atomic.Int64
	once    atomic.Bool
	ctx     context.Context
}

func (b *blockUntilCancelReader) Read(p []byte) (int, error) {
	if b.once.CompareAndSwap(false, true) && b.started != nil {
		close(b.started)
	}
	if b.ctx != nil {
		<-b.ctx.Done()
		return 0, b.ctx.Err()
	}
	select {}
}

func (b *blockUntilCancelReader) Close() error {
	b.closes.Add(1)
	return nil
}

type scriptedRTWithReqCtx struct {
	status  int
	headers http.Header
	body    *blockUntilCancelReader
	sawReq  atomic.Int64
}

func (s *scriptedRTWithReqCtx) RoundTrip(req *http.Request) (*http.Response, error) {
	s.sawReq.Add(1)
	s.body.ctx = req.Context()
	h := http.Header{}
	if s.headers != nil {
		h = s.headers.Clone()
	}
	return &http.Response{StatusCode: s.status, Header: h, Body: s.body, Request: req}, nil
}

func int64Ptr(v int64) *int64 { return &v }

func TestStreamRawFile_206LocalCapReportsRetainedWindow(t *testing.T) {
	// Closed range larger than max_bytes: provider extent 10-1009, retain 100.
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=10-1009" {
			t.Errorf("Range=%q", r.Header.Get("Range"))
		}
		w.Header().Set("Content-Range", "bytes 10-1009/2000")
		w.Header().Set("X-Gitlab-Size", "2000")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, strings.Repeat("A", 1000))
	})
	client := clientWithServer(t, h)
	ctx := WithBudget(context.Background(), DefaultBudget())
	end := int64(1010)
	res := StreamRawFile(ctx, client, RawStreamRequest{
		ProjectID: "42", FilePath: "a.txt", Ref: strings.Repeat("b", 40),
		RangeStart: int64Ptr(10), RangeEnd: &end, MaxReturnBytes: 100,
	})
	if res.Err != nil {
		t.Fatalf("intentional local cap must not be framing mismatch: %v", res.Err)
	}
	if res.Status != 206 || !res.WriterTruncated || res.WindowComplete || res.FullContentKnown {
		t.Fatalf("status=%d trunc=%v win=%v full=%v", res.Status, res.WriterTruncated, res.WindowComplete, res.FullContentKnown)
	}
	if res.ObservedStart == nil || *res.ObservedStart != 10 {
		t.Fatalf("start=%v", res.ObservedStart)
	}
	if res.ObservedEndExcl == nil || *res.ObservedEndExcl != 110 {
		t.Fatalf("end=%v want 110 (start+retained)", res.ObservedEndExcl)
	}
	if len(res.Data) != 100 {
		t.Fatalf("len=%d", len(res.Data))
	}
	if res.ReturnedRangeSHA == "" {
		t.Fatal("returned hash required for retained bytes")
	}
}

func TestStreamRawFile_206TruncatedMalformedProviderSpanRejectsOffsets(t *testing.T) {
	// Provider claims a 1-byte span while the body/cap retains 100 — ambiguous;
	// must not attest [start, start+100). Covers ordinary and near-MaxInt64 starts.
	for _, start := range []int64{10, math.MaxInt64 - 2} {
		start := start
		t.Run(fmt.Sprintf("start-%d", start), func(t *testing.T) {
			client := clientWithServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/*", start, start))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, strings.Repeat("A", 1000))
			}))
			res := StreamRawFile(WithBudget(context.Background(), DefaultBudget()), client, RawStreamRequest{
				ProjectID: "42", FilePath: "a.txt", Ref: strings.Repeat("b", 40),
				RangeStart: &start, MaxReturnBytes: 100,
			})
			if res.Err == nil {
				t.Fatalf("want framing error, got nil truncated=%v retained=%d", res.WriterTruncated, len(res.Data))
			}
			if res.ObservedStart != nil || res.ObservedEndExcl != nil {
				t.Fatalf("offsets must be unknown on malformed span: start=%v end=%v", res.ObservedStart, res.ObservedEndExcl)
			}
			if res.WindowComplete || res.FullContentKnown {
				t.Fatalf("must not attest completeness: win=%v full=%v", res.WindowComplete, res.FullContentKnown)
			}
		})
	}
}

func TestStreamRawFile_206OpenEndedLocalCapReportsRetainedWindow(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=5-" {
			t.Errorf("Range=%q", r.Header.Get("Range"))
		}
		w.Header().Set("Content-Range", "bytes 5-504/900")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, strings.Repeat("B", 500))
	})
	client := clientWithServer(t, h)
	ctx := WithBudget(context.Background(), DefaultBudget())
	res := StreamRawFile(ctx, client, RawStreamRequest{
		ProjectID: "42", FilePath: "a.txt", Ref: strings.Repeat("c", 40),
		RangeStart: int64Ptr(5), MaxReturnBytes: 50,
	})
	if res.Err != nil {
		t.Fatalf("err=%v", res.Err)
	}
	if res.ObservedStart == nil || *res.ObservedStart != 5 || res.ObservedEndExcl == nil || *res.ObservedEndExcl != 55 {
		t.Fatalf("want [5,55) got start=%v end=%v", res.ObservedStart, res.ObservedEndExcl)
	}
	if len(res.Data) != 50 || res.WindowComplete || res.FullContentKnown {
		t.Fatalf("len=%d win=%v full=%v", len(res.Data), res.WindowComplete, res.FullContentKnown)
	}
}

func TestStreamRawFile_redirectChangedPathClearsProviderMetadata(t *testing.T) {
	const sha = "1234567890abcdef1234567890abcdef12345678"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/raw") {
			http.NotFound(w, r)
			return
		}
		if strings.Contains(r.URL.EscapedPath(), "/files/f%2Etxt/raw") && r.URL.RawQuery == "ref="+sha {
			u := *r.URL
			// Same ref, different file path — provenance must refuse.
			u.Path = strings.Replace(u.Path, "/files/f.txt/raw", "/files/other.txt/raw", 1)
			u.RawPath = strings.Replace(u.RawPath, "/files/f%2Etxt/raw", "/files/other%2Etxt/raw", 1)
			http.Redirect(w, r, u.String(), http.StatusFound)
			return
		}
		w.Header().Set("X-Gitlab-Blob-Id", strings.Repeat("ab", 20))
		w.Header().Set("X-Gitlab-Content-Sha256", strings.Repeat("cd", 32))
		w.Header().Set("X-Gitlab-Size", "9")
		_, _ = io.WriteString(w, "MOVEDPATH")
	}))
	t.Cleanup(ts.Close)
	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	res := StreamRawFile(context.Background(), cli, RawStreamRequest{
		ProjectID: "42", FilePath: "f.txt", Ref: sha, MaxReturnBytes: 1024,
	})
	if res.Err == nil || !errors.Is(res.Err, ErrRawProvenance) {
		t.Fatalf("want provenance err, got %v", res.Err)
	}
	if len(res.Data) != 0 || res.BlobID != "" || res.ContentSHA256 != "" || res.SizeKnown || res.ReturnedRangeSHA != "" {
		t.Fatalf("changed-path reject must clear content/metadata: %+v", res)
	}
}
