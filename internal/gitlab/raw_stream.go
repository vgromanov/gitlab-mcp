package gitlab

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	retryablehttp "github.com/hashicorp/go-retryablehttp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// ErrRawStreamStop stops bounded copy after the retained window is full.
// Before returning, the writer cancels a request-local context so cappedBody
// closes the upstream reader and SDK deferred Discard cannot drain a giant body.
// Ordinary window stop must not cancel the invocation/parent context.
var ErrRawStreamStop = errors.New("raw_stream_stop")

// ErrRangeIgnored is returned when a Range was requested but the provider returned 200
// with a non-zero start that cannot be recovered without an unbounded scan restart.
var ErrRangeIgnored = errors.New("range_ignored")

// RawStreamRequest describes one bounded raw-file GET.
type RawStreamRequest struct {
	ProjectID      string
	FilePath       string
	Ref            string
	RangeStart     *int64
	RangeEnd       *int64
	MaxReturnBytes int64
}

// RawStreamResult is the bounded observation for one raw GET.
type RawStreamResult struct {
	Status           int
	Data             []byte
	ReturnedRangeSHA string
	BlobID           string
	ContentSHA256    string
	SizeKnown        bool
	Size             int64
	ObservedStart    *int64
	ObservedEndExcl  *int64
	RangeRequested   bool
	RangeHonored     bool
	WindowComplete   bool
	FullContentKnown bool
	WriterSatisfied  bool
	WriterTruncated  bool
	HeaderEncoding   string
	Err              error
}

type rawStreamBridge struct {
	writer    *rawCaptureWriter
	status    int
	headers   http.Header
	preCopied bool // body already copied in CheckRetry (206 path)
	copyErr   error
	reqCancel context.CancelFunc
}

// rawStreamCheckRetry never retries. For 206 it bound-copies the body into the
// capture writer before SDK CheckResponse's io.ReadAll, then replaces the body
// with an empty reader so ReadAll cannot unbounded-drain. Status/headers are
// preserved on the bridge for accurate observed provenance.
func rawStreamCheckRetry(bridge *rawStreamBridge) retryablehttp.CheckRetry {
	return func(_ context.Context, resp *http.Response, err error) (bool, error) {
		if resp == nil {
			return false, err
		}
		bridge.status = resp.StatusCode
		if resp.Header != nil {
			bridge.headers = resp.Header.Clone()
		}
		if resp.StatusCode == http.StatusPartialContent && resp.Body != nil && !bridge.preCopied {
			_, copyErr := io.Copy(bridge.writer, resp.Body)
			_ = resp.Body.Close()
			bridge.preCopied = true
			bridge.copyErr = copyErr
			if errors.Is(copyErr, ErrRawStreamStop) {
				bridge.copyErr = nil // ordinary window stop
			}
			// Prevent CheckResponse ReadAll from seeing the real (possibly giant) body.
			resp.Body = io.NopCloser(bytes.NewReader(nil))
		}
		return false, err
	}
}

// StreamRawFile GETs projects/:id/repository/files/:path/raw at Ref with optional Range.
func StreamRawFile(ctx context.Context, client *gitlab.Client, req RawStreamRequest) RawStreamResult {
	out := RawStreamResult{RangeRequested: req.RangeStart != nil || req.RangeEnd != nil}
	if client == nil {
		out.Err = fmt.Errorf("nil client")
		return out
	}
	if req.MaxReturnBytes <= 0 {
		out.Err = fmt.Errorf("max_return_bytes must be > 0")
		return out
	}
	if strings.TrimSpace(req.ProjectID) == "" || strings.TrimSpace(req.FilePath) == "" || strings.TrimSpace(req.Ref) == "" {
		out.Err = fmt.Errorf("project_id, file_path, and ref are required")
		return out
	}

	reqStart := int64(0)
	if req.RangeStart != nil {
		if *req.RangeStart < 0 {
			out.Err = fmt.Errorf("range start_byte must be >= 0")
			return out
		}
		reqStart = *req.RangeStart
	}
	var reqEndExcl *int64
	if req.RangeEnd != nil {
		if *req.RangeEnd <= reqStart {
			out.Err = fmt.Errorf("range end_byte must be > start_byte")
			return out
		}
		reqEndExcl = req.RangeEnd
	}

	maxRet := req.MaxReturnBytes
	closedTarget := false
	if reqEndExcl != nil {
		span := *reqEndExcl - reqStart
		if span < maxRet {
			maxRet = span
		}
		closedTarget = maxRet == span
	}

	path := fmt.Sprintf("projects/%s/repository/files/%s/raw",
		gitlab.PathEscape(req.ProjectID),
		gitlab.PathEscape(req.FilePath),
	)
	opt := &gitlab.GetRawFileOptions{Ref: gitlab.Ptr(req.Ref)}

	reqCtx, reqCancel := context.WithCancel(ctx)
	defer reqCancel()
	prov := RawProvenance{Ref: req.Ref}
	reqCtx = WithRawProvenance(reqCtx, prov)

	capW := &rawCaptureWriter{
		maxReturn:    maxRet,
		closedTarget: closedTarget,
		hash:         sha256.New(),
		onStop:       reqCancel,
	}
	bridge := &rawStreamBridge{writer: capW, reqCancel: reqCancel}

	var opts []gitlab.RequestOptionFunc
	opts = append(opts,
		gitlab.WithContext(reqCtx),
		gitlab.WithRequestRetry(rawStreamCheckRetry(bridge)),
	)
	if out.RangeRequested {
		var hdr string
		if reqEndExcl != nil {
			hdr = fmt.Sprintf("bytes=%d-%d", reqStart, *reqEndExcl-1)
		} else {
			hdr = fmt.Sprintf("bytes=%d-", reqStart)
		}
		opts = append(opts, gitlab.WithHeader("Range", hdr))
	}

	httpReq, err := client.NewRequest(http.MethodGet, path, opt, opts)
	if err != nil {
		out.Err = err
		return out
	}

	resp, doErr := client.Do(httpReq, capW)

	// Prefer bridge-observed status (accurate for 206 before SDK ErrorResponse).
	status := bridge.status
	if status == 0 && resp != nil {
		status = resp.StatusCode
	}
	out.Status = status
	headers := bridge.headers
	if headers == nil && resp != nil && resp.Response != nil {
		headers = resp.Response.Header
	}

	// Immutable provenance: final response (and redirect chain) must stay bound
	// to the authorized canonical raw URL including verified ref. Fail closed:
	// drop content and refuse completeness when origin cannot be attested.
	// Provider headers are adopted only after provenance succeeds.
	dropProvenance := func(perr error) RawStreamResult {
		out.Err = perr
		out.Data = nil
		out.ReturnedRangeSHA = ""
		out.WindowComplete = false
		out.FullContentKnown = false
		out.WriterSatisfied = false
		out.WriterTruncated = false
		out.ObservedStart = nil
		out.ObservedEndExcl = nil
		out.BlobID = ""
		out.ContentSHA256 = ""
		out.SizeKnown = false
		out.Size = 0
		out.HeaderEncoding = ""
		out.RangeHonored = false
		return out
	}
	if doErr != nil && (errors.Is(doErr, ErrRawProvenance) || strings.Contains(doErr.Error(), "raw_provenance") || strings.Contains(doErr.Error(), "ref changed")) {
		return dropProvenance(fmt.Errorf("%w: %v", ErrRawProvenance, doErr))
	}
	if resp != nil && resp.Response != nil && resp.Response.Request != nil {
		if perr := ValidateRawResponseProvenance(prov, resp.Response.Request); perr != nil {
			return dropProvenance(perr)
		}
	} else {
		// Require a verifiable final request whenever we captured bytes or would
		// treat the Do as success (incl. 206 pre-copy). Missing origin ⇒ drop.
		looksSuccessful := doErr == nil || bridge.preCopied || errors.Is(doErr, ErrRawStreamStop)
		if looksSuccessful || len(capW.buf) > 0 {
			return dropProvenance(fmt.Errorf("%w: unverifiable response origin", ErrRawProvenance))
		}
	}
	applyRawHeaders(&out, headers)

	out.WriterSatisfied = capW.satisfied
	out.WriterTruncated = capW.truncated
	if !capW.stopped && closedTarget && int64(len(capW.buf)) == maxRet {
		out.WriterSatisfied = true
	}

	// 206: body was bound-copied in CheckRetry; SDK returns ErrorResponse — clear that
	// when capture succeeded. Preserve parent cancellation over raw stop.
	if bridge.preCopied {
		if err := ctx.Err(); err != nil {
			out.Err = err
			out.Data = capW.bytes()
			if len(out.Data) > 0 {
				out.ReturnedRangeSHA = hex.EncodeToString(capW.hash.Sum(nil))
			}
			return out
		}
		if bridge.copyErr != nil {
			out.Err = mapBudgetContextErr(ctx, bridge.copyErr)
			out.Data = capW.bytes()
			if len(out.Data) > 0 {
				out.ReturnedRangeSHA = hex.EncodeToString(capW.hash.Sum(nil))
			}
			return out
		}
		doErr = nil // discard SDK 206 ErrorResponse after successful bounded capture
	}

	switch {
	case errors.Is(doErr, ErrRawStreamStop):
		if err := ctx.Err(); err != nil {
			out.Err = err
			out.Data = capW.bytes()
			if len(out.Data) > 0 {
				out.ReturnedRangeSHA = hex.EncodeToString(capW.hash.Sum(nil))
			}
			return out
		}
		doErr = nil
	case doErr != nil:
		doErr = mapBudgetContextErr(ctx, doErr)
		var er *gitlab.ErrorResponse
		if errors.As(doErr, &er) && er.Response != nil {
			if out.Status == 0 {
				out.Status = er.Response.StatusCode
			}
			if headers == nil {
				applyRawHeaders(&out, er.Response.Header)
			}
		}
		if errors.Is(doErr, context.Canceled) {
			if err := ctx.Err(); err != nil {
				out.Err = err
			} else if capW.satisfied || (closedTarget && int64(len(capW.buf)) == maxRet && !capW.truncated) {
				doErr = nil
			} else {
				out.Err = context.Canceled
			}
		} else if errors.Is(ctx.Err(), context.Canceled) {
			out.Err = context.Canceled
		} else if out.Status == http.StatusNotFound || errors.Is(doErr, gitlab.ErrNotFound) {
			out.Status = http.StatusNotFound
			out.Err = gitlab.ErrNotFound
		} else {
			out.Err = doErr
		}
		out.Data = capW.bytes()
		if len(out.Data) > 0 {
			out.ReturnedRangeSHA = hex.EncodeToString(capW.hash.Sum(nil))
		}
		if out.Err != nil {
			return out
		}
	}

	if err := ctx.Err(); err != nil {
		out.Err = err
		out.Data = capW.bytes()
		if len(out.Data) > 0 {
			out.ReturnedRangeSHA = hex.EncodeToString(capW.hash.Sum(nil))
		}
		return out
	}

	out.Data = capW.bytes()
	out.ReturnedRangeSHA = hex.EncodeToString(capW.hash.Sum(nil))
	finalizeRawObservation(&out, reqStart, reqEndExcl, headers)
	return out
}

func clearObservedWindow(out *RawStreamResult) {
	out.ObservedStart = nil
	out.ObservedEndExcl = nil
	out.WindowComplete = false
	out.FullContentKnown = false
}

func finalizeRawObservation(out *RawStreamResult, reqStart int64, reqEndExcl *int64, hdr http.Header) {
	_ = hdr
	switch out.Status {
	case http.StatusPartialContent:
		out.RangeHonored = out.RangeRequested
		crStart, crEndIncl, crTotal, crOK := parseContentRange(headerGet(hdr, "Content-Range"))
		if !crOK {
			clearObservedWindow(out)
			out.Err = fmt.Errorf("malformed Content-Range")
			return
		}
		if crEndIncl == math.MaxInt64 {
			clearObservedWindow(out)
			out.Err = fmt.Errorf("malformed Content-Range: end+1 overflow")
			return
		}
		providerEndExcl := crEndIncl + 1
		if providerEndExcl < crEndIncl {
			clearObservedWindow(out)
			out.Err = fmt.Errorf("malformed Content-Range: end+1 overflow")
			return
		}
		if crTotal >= 0 {
			if crEndIncl >= crTotal {
				clearObservedWindow(out)
				out.Err = fmt.Errorf("malformed Content-Range: end >= total")
				return
			}
			out.SizeKnown = true
			out.Size = crTotal
		}
		if crStart != reqStart {
			clearObservedWindow(out)
			out.Err = fmt.Errorf("Content-Range start mismatch")
			return
		}
		retained := int64(len(out.Data))
		if providerEndExcl < crStart {
			clearObservedWindow(out)
			out.Err = fmt.Errorf("Content-Range length mismatch")
			return
		}
		providerSpan := providerEndExcl - crStart
		// Intentional local max_bytes truncation: report the retained interval,
		// not the provider's larger Content-Range extent — but only when the
		// retained prefix fits inside the attested provider span and start+len
		// cannot overflow. Retained > providerSpan (or overflow) is framing,
		// not a valid local-cap window.
		if out.WriterTruncated {
			if retained > providerSpan {
				clearObservedWindow(out)
				out.Err = fmt.Errorf("Content-Range length mismatch")
				return
			}
			if retained > 0 && crStart > math.MaxInt64-retained {
				clearObservedWindow(out)
				out.Err = fmt.Errorf("Content-Range length mismatch")
				return
			}
			endRetained := crStart + retained
			out.ObservedStart = &crStart
			out.ObservedEndExcl = &endRetained
			out.WindowComplete = false
			out.FullContentKnown = false
			return
		}
		if retained != providerSpan {
			// Genuine framing mismatch (not local cap): offsets unknown.
			clearObservedWindow(out)
			out.Err = fmt.Errorf("Content-Range length mismatch")
			return
		}
		out.ObservedStart = &crStart
		out.ObservedEndExcl = &providerEndExcl
		out.WindowComplete = windowCoversRequest(reqStart, reqEndExcl, crStart, providerEndExcl, out.SizeKnown, out.Size)
		out.FullContentKnown = out.SizeKnown && crStart == 0 && providerEndExcl == out.Size

	case http.StatusOK:
		if out.RangeRequested && reqStart > 0 {
			out.RangeHonored = false
			out.WindowComplete = false
			out.FullContentKnown = false
			out.Data = nil
			out.ReturnedRangeSHA = ""
			out.Err = fmt.Errorf("%w: non-zero start", ErrRangeIgnored)
			return
		}
		start := int64(0)
		endExcl := int64(len(out.Data))
		out.ObservedStart = &start
		out.ObservedEndExcl = &endExcl
		out.RangeHonored = !out.RangeRequested
		if out.RangeRequested {
			out.RangeHonored = false
		}
		if out.WriterTruncated {
			out.WindowComplete = false
			out.FullContentKnown = false
			return
		}
		if out.SizeKnown {
			if out.RangeRequested && reqEndExcl != nil {
				out.WindowComplete = endExcl == *reqEndExcl || (*reqEndExcl > out.Size && endExcl == out.Size)
				out.FullContentKnown = start == 0 && endExcl == out.Size
			} else if out.RangeRequested {
				out.WindowComplete = endExcl == out.Size
				out.FullContentKnown = endExcl == out.Size
			} else {
				out.WindowComplete = endExcl == out.Size
				out.FullContentKnown = endExcl == out.Size
			}
		} else {
			out.FullContentKnown = false
			if out.RangeRequested && reqEndExcl != nil && endExcl == *reqEndExcl {
				out.WindowComplete = true
			} else {
				out.WindowComplete = false
			}
		}

	default:
		if out.Err == nil && out.Status != 0 && out.Status != http.StatusNotFound {
			out.Err = fmt.Errorf("unexpected status %d", out.Status)
		}
	}
}

func headerGet(h http.Header, key string) string {
	if h == nil {
		return ""
	}
	return h.Get(key)
}

func applyRawHeaders(out *RawStreamResult, h http.Header) {
	if h == nil {
		return
	}
	if blob, ok := attestedSHA1(h.Get("X-Gitlab-Blob-Id")); ok {
		out.BlobID = blob
	}
	if sum, ok := attestedSHA256(h.Get("X-Gitlab-Content-Sha256")); ok {
		out.ContentSHA256 = sum
	}
	if enc := strings.TrimSpace(h.Get("X-Gitlab-Encoding")); enc != "" {
		out.HeaderEncoding = enc
	}
	if sz := strings.TrimSpace(h.Get("X-Gitlab-Size")); sz != "" {
		n, err := strconv.ParseInt(sz, 10, 64)
		if err == nil && n >= 0 {
			out.SizeKnown = true
			out.Size = n
		}
	}
}

func attestedSHA1(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) != 40 {
		return "", false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return "", false
		}
	}
	return strings.ToLower(s), true
}

func attestedSHA256(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) != 64 {
		return "", false
	}
	for i := 0; i < 64; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return "", false
		}
	}
	return strings.ToLower(s), true
}

func parseContentRange(v string) (start, endIncl, total int64, ok bool) {
	v = strings.TrimSpace(v)
	if v == "" || !strings.HasPrefix(v, "bytes ") {
		return 0, 0, -1, false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(v, "bytes "))
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		return 0, 0, -1, false
	}
	se := strings.Split(parts[0], "-")
	if len(se) != 2 {
		return 0, 0, -1, false
	}
	var err error
	start, err = strconv.ParseInt(se[0], 10, 64)
	if err != nil || start < 0 {
		return 0, 0, -1, false
	}
	endIncl, err = strconv.ParseInt(se[1], 10, 64)
	if err != nil || endIncl < start {
		return 0, 0, -1, false
	}
	if parts[1] == "*" {
		return start, endIncl, -1, true
	}
	total, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil || total < 0 {
		return 0, 0, -1, false
	}
	return start, endIncl, total, true
}

func windowCoversRequest(reqStart int64, reqEndExcl *int64, obsStart, obsEndExcl int64, sizeKnown bool, size int64) bool {
	if obsStart != reqStart {
		return false
	}
	if reqEndExcl != nil {
		return obsEndExcl == *reqEndExcl || (sizeKnown && *reqEndExcl > size && obsEndExcl == size)
	}
	if sizeKnown {
		return obsEndExcl == size
	}
	return false
}

type rawCaptureWriter struct {
	maxReturn    int64
	closedTarget bool
	buf          []byte
	hash         hash.Hash
	truncated    bool
	satisfied    bool
	stopped      bool
	onStop       func()
}

func (w *rawCaptureWriter) stop(asTruncation bool) error {
	if asTruncation {
		w.truncated = true
	} else {
		w.satisfied = true
	}
	w.stopped = true
	if w.onStop != nil {
		w.onStop()
	}
	return ErrRawStreamStop
}

func (w *rawCaptureWriter) Write(p []byte) (int, error) {
	if w.stopped {
		return 0, ErrRawStreamStop
	}
	want := w.maxReturn - int64(len(w.buf))
	if want <= 0 {
		if w.closedTarget {
			return 0, w.stop(false)
		}
		return 0, w.stop(true)
	}
	if int64(len(p)) <= want {
		w.buf = append(w.buf, p...)
		_, _ = w.hash.Write(p)
		if w.closedTarget && int64(len(w.buf)) == w.maxReturn {
			return len(p), w.stop(false)
		}
		return len(p), nil
	}
	take := int(want)
	w.buf = append(w.buf, p[:take]...)
	_, _ = w.hash.Write(p[:take])
	if w.closedTarget {
		return take, w.stop(false)
	}
	return take, w.stop(true)
}

func (w *rawCaptureWriter) bytes() []byte {
	if w.buf == nil {
		return []byte{}
	}
	return w.buf
}

var _ io.Writer = (*rawCaptureWriter)(nil)
