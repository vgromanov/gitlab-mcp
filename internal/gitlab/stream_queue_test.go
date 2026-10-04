package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

func TestStreamJSONArrayQueue_stopDoesNotCancelSharedBudget(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"id":1},{"id":2},{"id":3}]`)
	}))
	t.Cleanup(ts.Close)

	c, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	b := DefaultBudget()
	ctx := WithBudget(context.Background(), b)
	var n int
	_, err = StreamJSONArrayQueue(ctx, c, http.MethodGet, "projects/1/merge_requests", nil, func(raw json.RawMessage) error {
		n++
		if n >= 2 {
			return ErrBudgetItems // application stop mid-array
		}
		_ = raw
		return nil
	})
	if !errors.Is(err, ErrBudgetItems) {
		t.Fatalf("want ErrBudgetItems, got %v", err)
	}
	select {
	case <-ctx.Done():
		t.Fatal("queue callback stop must not cancel shared WithBudget context")
	default:
	}
	if err := b.AddItem(); err != nil {
		t.Fatalf("shared budget must remain usable after queue stop: %v", err)
	}
	reqs, _, _ := b.Stats()
	if reqs < 1 {
		t.Fatalf("transport must still charge requests, stats reqs=%d hits=%d", reqs, hits.Load())
	}
}

func TestStreamJSONArray_legacyStillCancelsBudget(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"id":1},{"id":2}]`)
	}))
	t.Cleanup(ts.Close)
	c, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	b := DefaultBudget()
	ctx := WithBudget(context.Background(), b)
	_, err = StreamJSONArray(ctx, c, http.MethodGet, "projects/1/merge_requests", nil, func(json.RawMessage) error {
		return ErrBudgetItems
	})
	if !errors.Is(err, ErrBudgetItems) {
		t.Fatalf("want ErrBudgetItems, got %v", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("legacy StreamJSONArray must Cancel attached budget")
	}
}

func TestStreamJSONArrayQueue_closesReaderOnStop(t *testing.T) {
	started := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `[{"id":1}`)
		if flusher != nil {
			flusher.Flush()
		}
		close(started)
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, `,{"id":2}]`)
	}))
	t.Cleanup(ts.Close)
	c, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	b := DefaultBudget()
	ctx := WithBudget(context.Background(), b)
	_, err = StreamJSONArrayQueue(ctx, c, http.MethodGet, "projects/1/merge_requests", nil, func(json.RawMessage) error {
		<-started
		return errors.New("stop")
	})
	if err == nil {
		t.Fatal("expected stop error")
	}
	select {
	case <-ctx.Done():
		t.Fatal("queue stop must not cancel parent budget ctx")
	default:
	}
}

// TestStreamJSONArrayQueue_stopPreservesParentAndCounters asserts the actual HTTP
// request uses the derived child context (cancelled on callback stop) even when a
// caller mistakenly passes gitlab.WithContext(parent). Parent budget remains
// usable and upstream request caps are preserved.
func TestStreamJSONArrayQueue_stopPreservesParentAndCounters(t *testing.T) {
	type probeBody struct {
		io.Reader
		closed *atomic.Int32
	}
	closeFn := func(b *probeBody) error { b.closed.Add(1); return nil }

	type probeTransport struct {
		calls, closes atomic.Int32
		mu            sync.Mutex
		firstCtx      context.Context
	}
	for _, capRequests := range []int{2, 3} {
		name := map[int]string{2: "remaining_cap_enforced", 3: "parent_remains_usable"}[capRequests]
		t.Run(name, func(t *testing.T) {
			tr := &probeTransport{}
			trDo := func(r *http.Request) (*http.Response, error) {
				if tr.calls.Add(1) == 1 {
					tr.mu.Lock()
					tr.firstCtx = r.Context()
					tr.mu.Unlock()
				}
				body := &probeBody{Reader: strings.NewReader(`[{"id":1},{"id":2},{"id":3}]`), closed: &tr.closes}
				return &http.Response{
					StatusCode: 200, Status: "200 OK",
					Header:  http.Header{"Content-Type": []string{"application/json"}},
					Body:    readCloser{Reader: body.Reader, closeFn: func() error { return closeFn(body) }},
					Request: r,
				}, nil
			}
			c, err := gitlab.NewClient("fixture-only",
				gitlab.WithBaseURL("http://fixture.invalid/api/v4"),
				gitlab.WithoutRetries(),
				gitlab.WithHTTPClient(&http.Client{Transport: roundTripFunc(trDo)}),
				gitlab.WithInterceptor(BudgetInterceptor()),
			)
			if err != nil {
				t.Fatal(err)
			}
			b := DefaultBudget()
			b.MaxRequests = capRequests
			ctx := WithBudget(context.Background(), b)
			defer b.Cancel()
			if err := b.chargeRequest(); err != nil {
				t.Fatal(err)
			}
			// Mistaken parent WithContext must not override the derived child.
			_, err = StreamJSONArrayQueue(ctx, c, http.MethodGet, "projects/1/merge_requests", nil, func(json.RawMessage) error {
				return ErrBudgetItems
			}, gitlab.WithContext(ctx))
			if !errors.Is(err, ErrBudgetItems) {
				t.Fatalf("queue stop cause=%v", err)
			}
			if ctx.Err() != nil {
				t.Fatalf("parent cancelled: %v", ctx.Err())
			}
			tr.mu.Lock()
			actualCtx := tr.firstCtx
			tr.mu.Unlock()
			if actualCtx == nil || actualCtx.Err() == nil {
				t.Fatal("actual HTTP request context was not cancelled: caller option overrode derived child")
			}
			reqs, bytes, _ := b.Stats()
			if reqs != 2 || bytes <= 0 {
				t.Fatalf("parent accounting reset reqs=%d bytes=%d", reqs, bytes)
			}
			if tr.closes.Load() != 1 {
				t.Fatalf("stopped reader closes=%d", tr.closes.Load())
			}
			_, err = StreamJSONArrayQueue(ctx, c, http.MethodGet, "projects/1/merge_requests", nil, func(json.RawMessage) error {
				return nil
			})
			if capRequests == 2 {
				if !errors.Is(err, ErrBudgetRequests) || tr.calls.Load() != 1 {
					t.Fatalf("upstream cap bypassed err=%v calls=%d", err, tr.calls.Load())
				}
			}
			if capRequests == 3 {
				if err != nil || tr.calls.Load() != 2 || tr.closes.Load() != 2 {
					t.Fatalf("parent unavailable after child stop err=%v calls=%d closes=%d", err, tr.calls.Load(), tr.closes.Load())
				}
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type readCloser struct {
	io.Reader
	closeFn func() error
}

func (r readCloser) Close() error {
	if r.closeFn != nil {
		return r.closeFn()
	}
	return nil
}
