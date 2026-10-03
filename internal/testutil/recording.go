package testutil

import (
	"net/http"
	"sync"
	"sync/atomic"
)

// RecordingHandler wraps an http.Handler and records method/attempt counts.
type RecordingHandler struct {
	Next http.Handler

	mu       sync.Mutex
	methods  map[string]int
	attempts atomic.Int64
}

// ServeHTTP increments attempt/method counters then delegates to Next.
func (r *RecordingHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.attempts.Add(1)
	r.mu.Lock()
	if r.methods == nil {
		r.methods = make(map[string]int)
	}
	r.methods[req.Method]++
	r.mu.Unlock()
	if r.Next != nil {
		r.Next.ServeHTTP(w, req)
	}
}

// Attempts returns the total number of HTTP requests observed.
func (r *RecordingHandler) Attempts() int64 {
	return r.attempts.Load()
}

// MethodCount returns how many times method was observed.
func (r *RecordingHandler) MethodCount(method string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.methods == nil {
		return 0
	}
	return r.methods[method]
}

// Snapshot returns a copy of method counts.
func (r *RecordingHandler) Snapshot() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(r.methods))
	for k, v := range r.methods {
		out[k] = v
	}
	return out
}

// RecordingRoundTripper wraps a RoundTripper and records method/attempt counts.
type RecordingRoundTripper struct {
	Next http.RoundTripper

	mu       sync.Mutex
	methods  map[string]int
	attempts atomic.Int64
}

// RoundTrip increments counters then delegates.
func (r *RecordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.attempts.Add(1)
	r.mu.Lock()
	if r.methods == nil {
		r.methods = make(map[string]int)
	}
	r.methods[req.Method]++
	r.mu.Unlock()
	next := r.Next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(req)
}

// Attempts returns total RoundTrip calls.
func (r *RecordingRoundTripper) Attempts() int64 {
	return r.attempts.Load()
}

// MethodCount returns how many times method was observed.
func (r *RecordingRoundTripper) MethodCount(method string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.methods == nil {
		return 0
	}
	return r.methods[method]
}
