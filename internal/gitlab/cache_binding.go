package gitlab

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

type cacheBindingKey struct{}
type cacheBinding struct {
	mu       sync.Mutex
	base     *url.URL
	token    string
	requests int
	failed   bool
}

var ErrCacheBinding = errors.New("cache API authentication or origin binding rejected")

// WithCacheBinding requires the actual outbound API headers and canonical
// endpoint to match acquisition. Values remain in process memory and are never
// returned, logged, serialized, or persisted.
func WithCacheBinding(ctx context.Context, base *url.URL, token string) context.Context {
	copyURL := *base
	return context.WithValue(ctx, cacheBindingKey{}, &cacheBinding{base: &copyURL, token: token})
}
func checkCacheBinding(req *http.Request) error {
	b, _ := req.Context().Value(cacheBindingKey{}).(*cacheBinding)
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	prefix := strings.TrimRight(b.base.Path, "/") + "/"
	ok := req.URL.User == nil && req.URL.Scheme == b.base.Scheme && req.URL.Host == b.base.Host && strings.HasPrefix(req.URL.Path, prefix) && req.Header.Get("Authorization") == "" && req.Header.Get("Job-Token") == "" && req.Header.Get("Sudo") == "" && subtle.ConstantTimeCompare([]byte(req.Header.Get("Private-Token")), []byte(b.token)) == 1
	if !ok || b.token == "" {
		b.failed = true
		return ErrCacheBinding
	}
	b.requests++
	return nil
}
func CacheBindingObserved(ctx context.Context) bool {
	b, _ := ctx.Value(cacheBindingKey{}).(*cacheBinding)
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.failed && b.requests > 0
}
