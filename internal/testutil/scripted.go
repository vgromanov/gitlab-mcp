package testutil

import (
	"net/http"
	"sync"
	"sync/atomic"
)

// AcceptedBarrier signals that a fake side-effect has been accepted before the
// response is withheld or the connection is dropped.
type AcceptedBarrier struct {
	once sync.Once
	ch   chan struct{}
	n    atomic.Int64
}

// NewAcceptedBarrier returns a barrier whose Accepted channel closes once.
func NewAcceptedBarrier() *AcceptedBarrier {
	return &AcceptedBarrier{ch: make(chan struct{})}
}

// Accepted returns a channel closed after Signal.
func (b *AcceptedBarrier) Accepted() <-chan struct{} {
	return b.ch
}

// Signal marks the accepted side-effect. Safe for concurrent use; only the
// first call closes the channel.
func (b *AcceptedBarrier) Signal() {
	b.n.Add(1)
	b.once.Do(func() { close(b.ch) })
}

// Count returns how many times Signal was called.
func (b *AcceptedBarrier) Count() int64 {
	return b.n.Load()
}

// ScriptAcceptedThenDrop returns a handler that signals the accepted barrier,
// then hijacks and closes the connection without writing a response body.
// Callers waiting on the barrier can cancel their context after acceptance.
func ScriptAcceptedThenDrop(barrier *AcceptedBarrier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if barrier != nil {
			barrier.Signal()
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack unsupported", http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	})
}

// ScriptAcceptedThenHang signals acceptance then blocks until the request
// context is canceled (or the connection closes). Useful with ctx cancel after
// Accepted().
func ScriptAcceptedThenHang(barrier *AcceptedBarrier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if barrier != nil {
			barrier.Signal()
		}
		<-r.Context().Done()
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	})
}

// FailIfCalledRoundTripper fails any RoundTrip; used to prove zero HTTP on reject.
type FailIfCalledRoundTripper struct {
	Called atomic.Bool
}

// RoundTrip records the call and always errors.
func (f *FailIfCalledRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	f.Called.Store(true)
	return nil, errRoundTripForbidden
}
