package gitlab

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// safeCheckRedirect refuses to follow redirects for non-GET/HEAD methods so
// mutating 307/308 bodies are never replayed, and strips GitLab credential
// headers on cross-origin follows (Private-Token / Job-Token are not in Go's
// default sensitive-header list).
func safeCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	if len(via) == 0 {
		return nil
	}
	orig := via[0]
	switch orig.Method {
	case http.MethodGet, http.MethodHead:
		// allowed
	default:
		return fmt.Errorf("refusing redirect for mutating method %s", orig.Method)
	}
	if crossOrigin(orig.URL, req.URL) {
		req.Header.Del("Private-Token")
		req.Header.Del("Job-Token")
		req.Header.Del("Authorization")
	}
	return nil
}

func crossOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return true
	}
	return !strings.EqualFold(a.Scheme, b.Scheme) || !strings.EqualFold(a.Host, b.Host)
}
