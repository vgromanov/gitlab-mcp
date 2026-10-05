package gitlab

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"

	retryablehttp "github.com/hashicorp/go-retryablehttp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

const (
	// DefaultJobTraceScanBytes is the retained scan cap for one trace GET.
	DefaultJobTraceScanBytes int64 = 1 << 20
	// HardJobTraceScanBytes is the largest scan cap a caller may request.
	HardJobTraceScanBytes int64 = 8 << 20
)

// errScanLimit stops a trace copy after MaxScanBytes plus one peek byte.
var errScanLimit = errors.New("job_trace_scan_limit")

// JobTraceRequest is one bounded GET of a job trace.
// SuffixBytes requests a tail Range (bytes=-N). RangeStart/RangeEndExcl request
// an explicit byte range (end exclusive). A zero start with no suffix is a prefix.
type JobTraceRequest struct {
	ProjectID    string
	JobID        int64
	RangeStart   *int64
	RangeEndExcl *int64
	SuffixBytes  int64
	MaxScanBytes int64
}

// JobTraceResult is the bounded observation. Data holds at most MaxScanBytes.
// Scanned counts bytes actually read from the response body, including one peek
// byte used to tell an exact-fit body from a longer one.
type JobTraceResult struct {
	Status          int
	Data            []byte
	Scanned         int64
	SizeKnown       bool
	Size            int64
	ObservedStart   *int64
	ObservedEndExcl *int64
	RangeRequested  bool
	RangeHonored    bool
	SuffixAnchored  bool
	EOF             bool
	Truncated       bool
	Err             error
}

type jobTraceBridge struct {
	status    int
	headers   http.Header
	buf       []byte
	scanned   int64
	eof       bool
	truncated bool
	ignored   bool
	copyErr   error
}

// StreamJobTrace GETs projects/:id/jobs/:id/trace without buffering the SDK's
// full body. A Range that comes back as 200 for a non-zero or suffix request
// is not scanned: ErrRangeIgnored is set and Data stays empty.
func StreamJobTrace(ctx context.Context, client *gitlab.Client, req JobTraceRequest) JobTraceResult {
	out := JobTraceResult{}
	if client == nil {
		out.Err = fmt.Errorf("nil client")
		return out
	}
	if strings.TrimSpace(req.ProjectID) == "" || req.JobID <= 0 {
		out.Err = fmt.Errorf("project_id and job_id are required")
		return out
	}
	maxScan := req.MaxScanBytes
	if maxScan <= 0 {
		maxScan = DefaultJobTraceScanBytes
	}
	if maxScan > HardJobTraceScanBytes {
		maxScan = HardJobTraceScanBytes
	}

	hdr, ranged := traceRangeHeader(req)
	out.RangeRequested = ranged
	dropIgnored := ranged && (req.SuffixBytes > 0 || (req.RangeStart != nil && *req.RangeStart > 0))

	path := fmt.Sprintf("projects/%s/jobs/%d/trace", gitlab.PathEscape(req.ProjectID), req.JobID)
	bridge := &jobTraceBridge{}
	opts := []gitlab.RequestOptionFunc{
		gitlab.WithContext(ctx),
		gitlab.WithRequestRetry(jobTraceCheckRetry(ctx, bridge, maxScan, dropIgnored)),
	}
	if hdr != "" {
		opts = append(opts, gitlab.WithHeader("Range", hdr))
	}
	httpReq, err := client.NewRequest(http.MethodGet, path, nil, opts)
	if err != nil {
		out.Err = err
		return out
	}
	resp, doErr := client.Do(httpReq, io.Discard)
	status := bridge.status
	if status == 0 && resp != nil {
		status = resp.StatusCode
	}
	headers := bridge.headers
	if headers == nil && resp != nil && resp.Response != nil {
		headers = resp.Response.Header
	}
	out.Status = status
	out.Scanned = bridge.scanned

	if bridge.ignored {
		out.RangeHonored = false
		out.Data = []byte{}
		out.Err = fmt.Errorf("%w: provider returned 200", ErrRangeIgnored)
		return out
	}

	data := bridge.buf
	if data == nil {
		data = []byte{}
	}
	if int64(len(data)) > maxScan {
		out.Truncated = true
		out.Scanned = int64(len(data))
		if out.Scanned < bridge.scanned {
			out.Scanned = bridge.scanned
		}
		data = data[:maxScan]
	} else if bridge.truncated {
		out.Truncated = true
	}
	out.Data = data
	out.EOF = bridge.eof && !out.Truncated

	copyErr := bridge.copyErr
	if copyErr != nil && !errors.Is(copyErr, errScanLimit) {
		copyErr = mapBudgetContextErr(ctx, copyErr)
	} else {
		copyErr = nil
	}
	if doErr != nil && copyErr == nil && !errors.Is(doErr, errScanLimit) {
		// 206 is an ErrorResponse after we already captured the body.
		if status == http.StatusPartialContent && bridge.copyErr == nil {
			doErr = nil
		} else if status == http.StatusOK && bridge.copyErr == nil && ctx.Err() == nil {
			doErr = nil
		}
	}
	if err := ctx.Err(); err != nil {
		out.Err = mapBudgetContextErr(ctx, err)
		return out
	}
	if copyErr != nil {
		out.Err = copyErr
		return out
	}
	if doErr != nil && status != http.StatusPartialContent && status != http.StatusOK {
		out.Data = []byte{}
		out.Err = mapBudgetContextErr(ctx, doErr)
		if out.Status == http.StatusNotFound || errors.Is(doErr, gitlab.ErrNotFound) {
			out.Status = http.StatusNotFound
			out.Err = gitlab.ErrNotFound
		}
		return out
	}

	finalizeJobTrace(&out, req, headers, maxScan)
	return out
}

func traceRangeHeader(req JobTraceRequest) (string, bool) {
	if req.SuffixBytes > 0 {
		return fmt.Sprintf("bytes=-%d", req.SuffixBytes), true
	}
	if req.RangeStart == nil && req.RangeEndExcl == nil {
		return "", false
	}
	start := int64(0)
	if req.RangeStart != nil {
		start = *req.RangeStart
	}
	if req.RangeEndExcl != nil {
		if *req.RangeEndExcl <= start {
			return fmt.Sprintf("bytes=%d-%d", start, start), true
		}
		return fmt.Sprintf("bytes=%d-%d", start, *req.RangeEndExcl-1), true
	}
	return fmt.Sprintf("bytes=%d-", start), true
}

func jobTraceCheckRetry(ctx context.Context, bridge *jobTraceBridge, maxScan int64, dropIgnored bool) retryablehttp.CheckRetry {
	return func(_ context.Context, resp *http.Response, err error) (bool, error) {
		if resp == nil {
			return false, err
		}
		bridge.status = resp.StatusCode
		if resp.Header != nil {
			bridge.headers = resp.Header.Clone()
		}
		if resp.Body == nil {
			return false, err
		}
		if dropIgnored && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(nil))
			bridge.ignored = true
			return false, err
		}
		limited := &scanLimitReader{rc: resp.Body, left: maxScan + 1, ctx: ctx}
		_, copyErr := io.Copy(bridgeWriter{bridge, maxScan + 1}, limited)
		bridge.scanned = limited.scannedBytes()
		bridge.eof = errors.Is(copyErr, io.EOF) || limited.sawEOF()
		if errors.Is(copyErr, errScanLimit) || errors.Is(copyErr, ErrBudgetBytes) || limited.hitLimit() {
			bridge.truncated = true
			copyErr = nil
		}
		_ = limited.Close()
		bridge.copyErr = copyErr
		resp.Body = io.NopCloser(bytes.NewReader(nil))
		return false, err
	}
}

type bridgeWriter struct {
	b     *jobTraceBridge
	limit int64
}

func (w bridgeWriter) Write(p []byte) (int, error) {
	room := w.limit - int64(len(w.b.buf))
	if room <= 0 {
		return 0, errScanLimit
	}
	if int64(len(p)) > room {
		p = p[:room]
		w.b.buf = append(w.b.buf, p...)
		return len(p), errScanLimit
	}
	w.b.buf = append(w.b.buf, p...)
	return len(p), nil
}

func finalizeJobTrace(out *JobTraceResult, req JobTraceRequest, hdr http.Header, maxScan int64) {
	switch out.Status {
	case http.StatusPartialContent:
		crStart, crEndIncl, crTotal, crOK := parseContentRange(headerGet(hdr, "Content-Range"))
		if !crOK || crEndIncl == math.MaxInt64 {
			out.Data = []byte{}
			out.Err = fmt.Errorf("malformed Content-Range")
			out.RangeHonored = false
			return
		}
		endExcl := crEndIncl + 1
		if crTotal >= 0 {
			if crEndIncl >= crTotal {
				out.Data = []byte{}
				out.Err = fmt.Errorf("malformed Content-Range")
				return
			}
			out.SizeKnown = true
			out.Size = crTotal
		}
		// An un-ranged prefix/error GET wants start 0. A nonzero 206 would
		// otherwise skip this check (RangeStart is nil) and treat a mid-object
		// slice as offset 0, leaking a credential that began before crStart.
		if req.SuffixBytes <= 0 {
			wantStart := int64(0)
			if req.RangeStart != nil {
				wantStart = *req.RangeStart
			}
			if crStart != wantStart {
				out.Data = []byte{}
				out.Err = fmt.Errorf("Content-Range start mismatch")
				out.RangeHonored = false
				return
			}
		}
		if boundedRangeProviderShort(req, endExcl, out.SizeKnown, out.Size) {
			out.Data = []byte{}
			out.Err = fmt.Errorf("Content-Range shorter than requested range")
			out.RangeHonored = false
			return
		}
		retained := int64(len(out.Data))
		if out.Truncated {
			// A local scan cap may keep a prefix of the attested span. A body
			// larger than that span is framing, not a truncated window: do not
			// invent an end past the declared range.
			if endExcl < crStart {
				out.Data = []byte{}
				out.Err = fmt.Errorf("Content-Range length mismatch")
				out.RangeHonored = false
				return
			}
			providerSpan := endExcl - crStart
			if retained > providerSpan {
				out.Data = []byte{}
				out.Err = fmt.Errorf("Content-Range length mismatch")
				out.RangeHonored = false
				return
			}
			if retained > 0 && crStart > math.MaxInt64-retained {
				out.Data = []byte{}
				out.Err = fmt.Errorf("Content-Range length mismatch")
				out.RangeHonored = false
				return
			}
			end := crStart + retained
			out.ObservedStart = &crStart
			out.ObservedEndExcl = &end
			out.RangeHonored = true
			out.SuffixAnchored = false
			applyJobTraceObjectEOF(out)
			return
		}
		providerSpan := endExcl - crStart
		if retained != providerSpan {
			out.Data = []byte{}
			out.Err = fmt.Errorf("Content-Range length mismatch")
			out.RangeHonored = false
			return
		}
		out.ObservedStart = &crStart
		out.ObservedEndExcl = &endExcl
		out.RangeHonored = true
		out.SuffixAnchored = out.SizeKnown && endExcl == out.Size
		applyJobTraceObjectEOF(out)
		_ = maxScan
	case http.StatusOK:
		start := int64(0)
		end := int64(len(out.Data))
		out.ObservedStart = &start
		out.ObservedEndExcl = &end
		out.RangeHonored = false
		cl, hasCL := parseContentLength(hdr)
		// A clean EOF with fewer bytes than Content-Length, or a body that ran
		// past Content-Length, is not the declared object. Keeping that size,
		// or leaving EOF set, lets a prefix look like a finished trace
		// (content_complete, or an error search with no match). A scan-capped
		// prefix (end < cl and not EOF) is still a legitimate truncated read
		// of a larger object and may attest Size from Content-Length.
		if hasCL && ((out.EOF && end < cl) || end > cl) {
			out.SizeKnown = false
			out.Size = 0
			out.EOF = false
			out.Truncated = true
			out.SuffixAnchored = false
			return
		}
		if hasCL {
			out.SizeKnown = true
			out.Size = cl
		}
		if out.EOF && !out.SizeKnown {
			out.SizeKnown = true
			out.Size = end
		}
		if out.EOF && out.SizeKnown && end == out.Size && start == 0 {
			out.SuffixAnchored = true
		}
	default:
		if out.Err == nil && out.Status != 0 {
			if out.Status == http.StatusNotFound {
				out.Err = gitlab.ErrNotFound
				out.Data = []byte{}
				return
			}
			out.Err = fmt.Errorf("unexpected status %d", out.Status)
			out.Data = []byte{}
		}
	}
}

// boundedRangeProviderShort is true when a bounded Range GET is answered with
// a 206 that ends before both the requested (context-expanded) end and the
// known object total. bytes 0-49/1000 for a request of 0-611 is not honored.
func boundedRangeProviderShort(req JobTraceRequest, endExcl int64, sizeKnown bool, size int64) bool {
	if req.SuffixBytes > 0 || req.RangeEndExcl == nil {
		return false
	}
	if sizeKnown && endExcl == size {
		return false
	}
	return endExcl < *req.RangeEndExcl
}

// applyJobTraceObjectEOF clears EOF when a 206 does not cover the whole
// object. HTTP body EOF on bytes 0-1/5 is not the end of the trace, and a
// total of "*" cannot prove the object ended either.
func applyJobTraceObjectEOF(out *JobTraceResult) {
	if out == nil {
		return
	}
	if !out.SizeKnown {
		out.EOF = false
		return
	}
	if out.ObservedEndExcl == nil {
		return
	}
	if *out.ObservedEndExcl != out.Size {
		out.EOF = false
	}
}

func parseContentLength(h http.Header) (int64, bool) {
	if h == nil {
		return 0, false
	}
	v := strings.TrimSpace(h.Get("Content-Length"))
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

type scanLimitReader struct {
	rc    io.ReadCloser
	ctx   context.Context
	mu    sync.Mutex
	left  int64
	read  int64
	done  bool
	eof   bool
	limit bool
}

func (s *scanLimitReader) scannedBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read
}

func (s *scanLimitReader) sawEOF() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.eof
}

func (s *scanLimitReader) hitLimit() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limit
}

func (s *scanLimitReader) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return nil
	}
	s.done = true
	if s.rc == nil {
		return nil
	}
	return s.rc.Close()
}

func (s *scanLimitReader) Read(p []byte) (int, error) {
	if s.ctx != nil {
		if err := s.ctx.Err(); err != nil {
			_ = s.Close()
			return 0, err
		}
	}
	s.mu.Lock()
	if s.done && s.left <= 0 {
		s.mu.Unlock()
		return 0, errScanLimit
	}
	if s.left <= 0 {
		s.limit = true
		s.mu.Unlock()
		_ = s.Close()
		return 0, errScanLimit
	}
	if int64(len(p)) > s.left {
		p = p[:s.left]
	}
	s.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}

	type rr struct {
		n   int
		err error
	}
	ch := make(chan rr, 1)
	go func() {
		n, err := s.rc.Read(p)
		ch <- rr{n, err}
	}()
	var r rr
	if s.ctx == nil {
		r = <-ch
	} else {
		select {
		case <-s.ctx.Done():
			// Close the body without waiting on a lock Read still holds.
			// A reader that only returns after Close must be able to finish
			// so this receive cannot outlive the deadline.
			_ = s.Close()
			r = <-ch
			if r.n > 0 {
				s.add(r.n)
			}
			return r.n, s.ctx.Err()
		case r = <-ch:
		}
	}
	if r.n > 0 {
		s.add(r.n)
	}
	if r.err != nil {
		if errors.Is(r.err, io.EOF) {
			s.mu.Lock()
			s.eof = true
			s.mu.Unlock()
		}
		return r.n, r.err
	}
	s.mu.Lock()
	left := s.left
	s.mu.Unlock()
	if left == 0 {
		s.mu.Lock()
		s.limit = true
		s.mu.Unlock()
		_ = s.Close()
		return r.n, errScanLimit
	}
	return r.n, nil
}

func (s *scanLimitReader) add(n int) {
	s.mu.Lock()
	s.read += int64(n)
	s.left -= int64(n)
	if s.left < 0 {
		s.left = 0
	}
	s.mu.Unlock()
}
