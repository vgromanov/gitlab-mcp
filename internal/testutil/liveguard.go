package testutil

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode"
)

const (
	envIntegrationAllowWrite = "INTEGRATION_ALLOW_WRITE"
	envGitLabAPIURL          = "GITLAB_API_URL"
	envGitLabTestProjectID   = "GITLAB_TEST_PROJECT_ID"
)

var (
	errRoundTripForbidden = errors.New("testutil: round trip forbidden")
	// ErrLiveWriteDenied is returned when a non-safe method hits deny-write.
	ErrLiveWriteDenied = errors.New("testutil: integration write denied by default deny-write transport")
	// ErrLiveScopeDenied is returned when a request fails the live-scope guard.
	ErrLiveScopeDenied = errors.New("testutil: live request outside authorized instance/project scope")
	// ErrLiveAuthorize is returned when authorize-for-write validation fails.
	ErrLiveAuthorize = errors.New("testutil: live write not authorized")
)

// AuthorizedLiveTarget is an independently supplied live target. The type
// cannot prove provenance; callers must review values outside this package.
type AuthorizedLiveTarget struct {
	APIURL    string
	ProjectID string
}

// CanonicalAPIIdentity normalizes a GitLab API base URL for identity compare:
// scheme+host+default-port, reject userinfo/fragment/opaque, forbid non-empty
// query, trim trailing slash, require exactly /api/v4.
func CanonicalAPIIdentity(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("%w: empty API URL", ErrLiveAuthorize)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: parse API URL: %v", ErrLiveAuthorize, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%w: unsupported scheme %q", ErrLiveAuthorize, u.Scheme)
	}
	if u.Opaque != "" {
		return "", fmt.Errorf("%w: opaque URL not allowed", ErrLiveAuthorize)
	}
	if u.User != nil {
		return "", fmt.Errorf("%w: userinfo not allowed in API URL", ErrLiveAuthorize)
	}
	if u.Fragment != "" {
		return "", fmt.Errorf("%w: fragment not allowed in API URL", ErrLiveAuthorize)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", fmt.Errorf("%w: query not allowed in API URL identity", ErrLiveAuthorize)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%w: missing host in API URL", ErrLiveAuthorize)
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	if path == "" {
		path = "/api/v4"
	}
	if path != "/api/v4" {
		return "", fmt.Errorf("%w: API identity path must be exactly /api/v4", ErrLiveAuthorize)
	}
	return u.Scheme + "://" + net.JoinHostPort(host, port) + "/api/v4", nil
}

// CanonicalProjectID normalizes a project id or path for identity compare.
// Decimal digit IDs become the decimal form; path forms allow a single
// PathUnescape. Mixed/ambiguous/%252F/partial-escape forms and invalid
// segments (empty, ".", "..", controls, residual escapes) are rejected.
func CanonicalProjectID(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("%w: empty project id", ErrLiveAuthorize)
	}
	if strings.Contains(strings.ToLower(raw), "%252f") {
		return "", fmt.Errorf("%w: double-encoded project id", ErrLiveAuthorize)
	}
	if isAllDigits(raw) {
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return "", fmt.Errorf("%w: invalid numeric project id", ErrLiveAuthorize)
		}
		return strconv.FormatUint(n, 10), nil
	}
	decoded := raw
	if strings.Contains(raw, "%") {
		once, err := url.PathUnescape(raw)
		if err != nil {
			return "", fmt.Errorf("%w: invalid project path escape", ErrLiveAuthorize)
		}
		if once == raw {
			return "", fmt.Errorf("%w: partial/invalid project path escape", ErrLiveAuthorize)
		}
		if strings.Contains(once, "%") {
			return "", fmt.Errorf("%w: ambiguous residual escape in project path", ErrLiveAuthorize)
		}
		decoded = once
	}
	if err := validateProjectPathForm(decoded); err != nil {
		return "", err
	}
	return decoded, nil
}

func validateProjectPathForm(path string) error {
	if path == "" || strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") || strings.Contains(path, "//") {
		return fmt.Errorf("%w: invalid project path form", ErrLiveAuthorize)
	}
	if strings.Contains(path, "\\") {
		return fmt.Errorf("%w: invalid project path form", ErrLiveAuthorize)
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%w: invalid project path segment", ErrLiveAuthorize)
		}
		if strings.Contains(seg, "%") {
			return fmt.Errorf("%w: residual escape in project path segment", ErrLiveAuthorize)
		}
		for _, r := range seg {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("%w: control character in project path", ErrLiveAuthorize)
			}
		}
	}
	return nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// AuthorizeLiveWrite validates write authorization: INTEGRATION_ALLOW_WRITE=1
// and claimed GITLAB_API_URL / GITLAB_TEST_PROJECT_ID must match the independently
// supplied AuthorizedLiveTarget after canonicalize. Empty authorized fails closed.
// This is not env-to-env authorization.
func AuthorizeLiveWrite(auth AuthorizedLiveTarget) error {
	if strings.TrimSpace(auth.APIURL) == "" || strings.TrimSpace(auth.ProjectID) == "" {
		return fmt.Errorf("%w: empty authorized target", ErrLiveAuthorize)
	}
	if strings.TrimSpace(os.Getenv(envIntegrationAllowWrite)) != "1" {
		return fmt.Errorf("%w: %s must be 1", ErrLiveAuthorize, envIntegrationAllowWrite)
	}
	wantURL, err := CanonicalAPIIdentity(auth.APIURL)
	if err != nil {
		return err
	}
	wantProj, err := CanonicalProjectID(auth.ProjectID)
	if err != nil {
		return err
	}
	claimURL := strings.TrimSpace(os.Getenv(envGitLabAPIURL))
	claimProj := strings.TrimSpace(os.Getenv(envGitLabTestProjectID))
	gotURL, err := CanonicalAPIIdentity(claimURL)
	if err != nil {
		return fmt.Errorf("%w: claimed API URL: %v", ErrLiveAuthorize, err)
	}
	gotProj, err := CanonicalProjectID(claimProj)
	if err != nil {
		return fmt.Errorf("%w: claimed project: %v", ErrLiveAuthorize, err)
	}
	if gotURL != wantURL {
		return fmt.Errorf("%w: claimed API URL does not match authorized target", ErrLiveAuthorize)
	}
	if gotProj != wantProj {
		return fmt.Errorf("%w: claimed project does not match authorized target", ErrLiveAuthorize)
	}
	return nil
}

// InstallDenyWriteTransport wraps client.Transport to reject non-safe methods
// before the underlying RoundTrip. Redirect following is refused.
func InstallDenyWriteTransport(client *http.Client) {
	if client == nil {
		return
	}
	next := client.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	client.Transport = &denyWriteTransport{next: next}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return ErrLiveWriteDenied
	}
}

// InstallAuthorizedScopedLiveTransport authorizes (flag + matching independently
// supplied target claims) then installs the per-request scoped transport.
// This is the only exported mutating install path; it cannot bypass authorization.
func InstallAuthorizedScopedLiveTransport(client *http.Client, auth AuthorizedLiveTarget) error {
	if err := AuthorizeLiveWrite(auth); err != nil {
		return err
	}
	return installScopedLiveTransport(client, auth)
}

// WrapDenyWrite installs deny-write in front of next (nil → DefaultTransport).
func WrapDenyWrite(next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return &denyWriteTransport{next: next}
}

// WrapAuthorizedScopedLive authorizes then wraps next with the scoped transport.
// This is the only exported mutating RoundTripper wrapper; it cannot bypass authorization.
func WrapAuthorizedScopedLive(next http.RoundTripper, auth AuthorizedLiveTarget) (http.RoundTripper, error) {
	if err := AuthorizeLiveWrite(auth); err != nil {
		return nil, err
	}
	return newScopedLiveTransport(next, auth)
}

// installScopedLiveTransport is the private direct guard seam for local/fake
// tests that already hold a validated AuthorizedLiveTarget.
func installScopedLiveTransport(client *http.Client, auth AuthorizedLiveTarget) error {
	if client == nil {
		return fmt.Errorf("%w: nil http client", ErrLiveAuthorize)
	}
	rt, err := newScopedLiveTransport(client.Transport, auth)
	if err != nil {
		return err
	}
	client.Transport = rt
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return ErrLiveScopeDenied
	}
	return nil
}

func newScopedLiveTransport(next http.RoundTripper, auth AuthorizedLiveTarget) (http.RoundTripper, error) {
	inst, err := CanonicalAPIIdentity(auth.APIURL)
	if err != nil {
		return nil, err
	}
	proj, err := CanonicalProjectID(auth.ProjectID)
	if err != nil {
		return nil, err
	}
	if next == nil {
		next = http.DefaultTransport
	}
	return &scopedLiveTransport{next: next, instance: inst, project: proj}, nil
}

type denyWriteTransport struct {
	next http.RoundTripper
}

func (d *denyWriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, ErrLiveWriteDenied
	}
	if !isSafeMethod(req.Method) {
		return nil, fmt.Errorf("%w: method %s", ErrLiveWriteDenied, req.Method)
	}
	return d.next.RoundTrip(req)
}

type scopedLiveTransport struct {
	next     http.RoundTripper
	instance string
	project  string
}

func (s *scopedLiveTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, ErrLiveScopeDenied
	}
	if err := authorizeLiveRequest(req, s.instance, s.project); err != nil {
		return nil, err
	}
	return s.next.RoundTrip(req)
}

func isSafeMethod(m string) bool {
	switch strings.ToUpper(m) {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// authorizeLiveRequest enforces instance/project scope before RoundTrip.
// Request query parameters are preserved for normal project API use; only
// identity URLs forbid queries (see CanonicalAPIIdentity).
func authorizeLiveRequest(req *http.Request, instance, project string) error {
	u := req.URL
	if u.Opaque != "" {
		return fmt.Errorf("%w: opaque URL forbidden", ErrLiveScopeDenied)
	}
	if u.User != nil {
		return fmt.Errorf("%w: userinfo forbidden", ErrLiveScopeDenied)
	}
	if u.Fragment != "" {
		return fmt.Errorf("%w: fragment forbidden", ErrLiveScopeDenied)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("%w: bad scheme", ErrLiveScopeDenied)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: missing host", ErrLiveScopeDenied)
	}
	if req.Host != "" && !sameHTTPHost(req.Host, u.Host) {
		return fmt.Errorf("%w: Host override changes virtual instance", ErrLiveScopeDenied)
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	reqInst := scheme + "://" + net.JoinHostPort(host, port) + "/api/v4"
	if reqInst != instance {
		return fmt.Errorf("%w: cross-instance", ErrLiveScopeDenied)
	}
	return authorizeEscapedPath(u.EscapedPath(), project)
}

func sameHTTPHost(a, b string) bool {
	ahost, aport, aerr := splitHostPortDefault(a)
	bhost, bport, berr := splitHostPortDefault(b)
	if aerr != nil || berr != nil {
		return strings.EqualFold(a, b)
	}
	return strings.EqualFold(ahost, bhost) && aport == bport
}

func splitHostPortDefault(hostport string) (string, string, error) {
	if hostport == "" {
		return "", "", fmt.Errorf("empty")
	}
	if strings.HasPrefix(hostport, "[") {
		h, p, err := net.SplitHostPort(hostport)
		if err != nil {
			return "", "", err
		}
		return h, p, nil
	}
	if strings.Count(hostport, ":") == 1 {
		return net.SplitHostPort(hostport)
	}
	return hostport, "", nil
}

func authorizeEscapedPath(escaped, project string) error {
	if escaped == "" || !strings.HasPrefix(escaped, "/") {
		return fmt.Errorf("%w: invalid path", ErrLiveScopeDenied)
	}
	if strings.Contains(escaped, "//") {
		return fmt.Errorf("%w: empty/double-slash path segment", ErrLiveScopeDenied)
	}
	if strings.Contains(strings.ToLower(escaped), "%252f") {
		return fmt.Errorf("%w: ambiguous escaped path", ErrLiveScopeDenied)
	}
	trimmed := strings.TrimRight(escaped, "/")
	parts := strings.Split(strings.TrimPrefix(trimmed, "/"), "/")
	if len(parts) < 4 || parts[0] != "api" || parts[1] != "v4" || parts[2] != "projects" {
		return fmt.Errorf("%w: path outside /api/v4/projects", ErrLiveScopeDenied)
	}
	if err := validateProjectSegment(parts[3], project); err != nil {
		return err
	}
	for _, seg := range parts[4:] {
		if err := validateSuffixSegment(seg); err != nil {
			return err
		}
	}
	return nil
}

func validateSuffixSegment(seg string) error {
	if seg == "" {
		return fmt.Errorf("%w: empty path segment", ErrLiveScopeDenied)
	}
	lower := strings.ToLower(seg)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%252f") {
		return fmt.Errorf("%w: encoded slash in path segment", ErrLiveScopeDenied)
	}
	unesc, err := url.PathUnescape(seg)
	if err != nil {
		return fmt.Errorf("%w: bad path segment escape", ErrLiveScopeDenied)
	}
	if unesc == "." || unesc == ".." {
		return fmt.Errorf("%w: path traversal segment", ErrLiveScopeDenied)
	}
	if strings.Contains(unesc, "/") || strings.Contains(unesc, "\\") {
		return fmt.Errorf("%w: slash in path segment", ErrLiveScopeDenied)
	}
	if strings.Contains(unesc, "%") {
		return fmt.Errorf("%w: residual escape in path segment", ErrLiveScopeDenied)
	}
	for _, r := range unesc {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: control character in path segment", ErrLiveScopeDenied)
		}
	}
	return nil
}

func validateProjectSegment(seg, canonical string) error {
	if isAllDigits(canonical) {
		if seg != canonical {
			return fmt.Errorf("%w: project segment mismatch", ErrLiveScopeDenied)
		}
		return nil
	}
	if strings.Contains(strings.ToLower(seg), "%252f") {
		return fmt.Errorf("%w: double-encoded project segment", ErrLiveScopeDenied)
	}
	unesc, err := url.PathUnescape(seg)
	if err != nil {
		return fmt.Errorf("%w: bad project segment escape", ErrLiveScopeDenied)
	}
	if unesc != canonical {
		return fmt.Errorf("%w: project segment mismatch", ErrLiveScopeDenied)
	}
	if err := validateProjectPathForm(unesc); err != nil {
		return fmt.Errorf("%w: %v", ErrLiveScopeDenied, err)
	}
	// Allow only the unescaped canonical form or the exact PathEscape spelling.
	// Do not re-unescape both sides — that would accept alternate encodings.
	canonEscaped := url.PathEscape(canonical)
	if seg != canonical && seg != canonEscaped {
		return fmt.Errorf("%w: non-canonical project segment encoding", ErrLiveScopeDenied)
	}
	return nil
}
