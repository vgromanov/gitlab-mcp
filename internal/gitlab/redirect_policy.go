package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ErrRawProvenance is returned when a raw-file redirect/response is not bound
// to the authorized canonical project/path/ref (immutable commit SHA).
var ErrRawProvenance = fmt.Errorf("raw_provenance")

type rawProvenanceKey struct{}

// RawProvenance activates immutable raw redirect/response binding on a request
// context. Opt-in only; legacy tools leave the context unset so safeCheckRedirect
// keeps its prior credential/method behavior without path/ref binding.
type RawProvenance struct {
	Ref string // verified full commit SHA (required)
}

// WithRawProvenance activates immutable raw redirect binding on ctx.
func WithRawProvenance(ctx context.Context, p RawProvenance) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, rawProvenanceKey{}, p)
}

func rawProvenanceFromContext(ctx context.Context) (RawProvenance, bool) {
	if ctx == nil {
		return RawProvenance{}, false
	}
	p, ok := ctx.Value(rawProvenanceKey{}).(RawProvenance)
	return p, ok && p.Ref != ""
}

// SafeCheckRedirectForTest exposes the production redirect policy for synthetic
// HTTP clients constructed in unit tests (gitlab.WithHTTPClient).
func SafeCheckRedirectForTest(req *http.Request, via []*http.Request) error {
	return safeCheckRedirect(req, via)
}

// safeCheckRedirect refuses to follow redirects for non-GET/HEAD methods so
// mutating 307/308 bodies are never replayed, and strips GitLab credential
// headers on cross-origin follows (Private-Token / Job-Token are not in Go's
// default sensitive-header list).
//
// When RawProvenance is present on the request context (opt-in), redirects must
// preserve the trusted original request's exact origin, escaped path, and sole
// ref=<verifiedSHA> query. Exact same authorized URL hops remain allowed and
// still charge every hop via the budget transport.
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
	if p, ok := rawProvenanceFromContext(req.Context()); ok {
		if err := bindRawURL(p, orig.URL, req.URL); err != nil {
			return err
		}
	}
	return nil
}

// bindRawURL requires next to match the trusted authorized request identity:
// same origin, same escaped path, and the same sole ref=<verifiedSHA> query
// (no forged extra query parameters).
func bindRawURL(p RawProvenance, authorized, next *url.URL) error {
	if authorized == nil || next == nil {
		return fmt.Errorf("%w: missing url", ErrRawProvenance)
	}
	if p.Ref == "" {
		return fmt.Errorf("%w: missing verified ref", ErrRawProvenance)
	}
	if crossOrigin(authorized, next) {
		return fmt.Errorf("%w: cross-origin redirect", ErrRawProvenance)
	}
	authPath := trustedEscapedPath(authorized)
	nextPath := trustedEscapedPath(next)
	if authPath == "" || nextPath == "" {
		return fmt.Errorf("%w: missing escaped path", ErrRawProvenance)
	}
	if authPath != nextPath {
		return fmt.Errorf("%w: path changed", ErrRawProvenance)
	}
	if err := bindSoleVerifiedRefQuery(p.Ref, authorized); err != nil {
		return fmt.Errorf("%w: authorized %v", ErrRawProvenance, err)
	}
	if err := bindSoleVerifiedRefQuery(p.Ref, next); err != nil {
		return fmt.Errorf("%w: %v", ErrRawProvenance, err)
	}
	return nil
}

func trustedEscapedPath(u *url.URL) string {
	if u == nil {
		return ""
	}
	// Prefer RawPath when the GitLab client stamped the PathEscape form
	// (e.g. dir%2Fa%2Etxt). Fall back to EscapedPath.
	p := u.RawPath
	if p == "" {
		p = u.EscapedPath()
	}
	if p == "" {
		p = u.Path
	}
	return normalizeURLPath(p)
}

// bindSoleVerifiedRefQuery requires RawQuery to parse cleanly as exactly one
// key "ref" with the verified SHA. url.URL.Query() silently drops malformed
// semicolon-bearing pairs, so we use url.ParseQuery and fail closed on error.
func bindSoleVerifiedRefQuery(ref string, u *url.URL) error {
	if u == nil {
		return fmt.Errorf("missing url")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return fmt.Errorf("malformed query")
	}
	if len(q) != 1 {
		return fmt.Errorf("forged or missing query")
	}
	vals, ok := q["ref"]
	if !ok || len(vals) != 1 || vals[0] != ref {
		return fmt.Errorf("ref changed")
	}
	return nil
}

func normalizeURLPath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimSuffix(p, "/")
}

// ValidateRawResponseProvenance walks the final request redirect chain and
// ensures every hop stayed bound to the trusted authorized raw URL identity
// (exact origin/escaped-path/sole verified ref query). On failure callers must
// drop content and not attest completeness/consistency.
func ValidateRawResponseProvenance(p RawProvenance, final *http.Request) error {
	if final == nil || final.URL == nil {
		return fmt.Errorf("%w: missing final request", ErrRawProvenance)
	}
	if p.Ref == "" {
		return fmt.Errorf("%w: missing verified ref", ErrRawProvenance)
	}
	// Walk Response.Request links from final back to the original authorized request.
	var chain []*http.Request
	for r := final; r != nil; {
		chain = append(chain, r)
		if r.Response == nil || r.Response.Request == nil || r.Response.Request == r {
			break
		}
		r = r.Response.Request
	}
	orig := chain[len(chain)-1]
	if orig == nil || orig.URL == nil {
		return fmt.Errorf("%w: missing authorized request", ErrRawProvenance)
	}
	for _, r := range chain {
		if r == nil || r.URL == nil {
			return fmt.Errorf("%w: nil hop", ErrRawProvenance)
		}
		if err := bindRawURL(p, orig.URL, r.URL); err != nil {
			return err
		}
	}
	return nil
}

func crossOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return true
	}
	return !strings.EqualFold(a.Scheme, b.Scheme) || !strings.EqualFold(a.Host, b.Host)
}
