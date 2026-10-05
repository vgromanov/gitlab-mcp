// Package origin validates cache fetch URLs and cache-only insecure TLS scope.
//
// Only https and ssh are permitted. file:, git:, ext::, and credential-bearing
// userinfo are rejected. Insecure TLS is never inherited from the API client;
// it requires an explicit cache opt-in whose ServerName equals the configured
// AllowedInsecureHost.
package origin

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// DefaultInsecureHost is the corporate canonical HTTPS hostname that may be
// named in cache-only insecure configuration. It is not an automatic trust
// relaxation.
const DefaultInsecureHost = "gitlabci.raiffeisen.ru"

var (
	ErrScheme   = errors.New("gitcache: only https and ssh URLs are allowed")
	ErrUserinfo = errors.New("gitcache: URL must not carry credentials")
	ErrHost     = errors.New("gitcache: URL host is invalid")
)

// Target is a validated remote endpoint. Credentials are never stored here.
type Target struct {
	Raw    string
	Scheme string
	Host   string
	Port   string
	Path   string
	User   string
}

// Parse accepts only https and ssh. Loopback is accepted solely when
// allowLoopback is set by tests. Credential userinfo is rejected. The ssh
// user "git" is routing, not a credential secret.
func Parse(raw string, allowLoopback bool) (Target, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "ext::") || strings.HasPrefix(raw, "file:") ||
		strings.HasPrefix(raw, "git:") || strings.HasPrefix(raw, "git@") {
		return Target{}, ErrScheme
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Target{}, ErrScheme
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "ssh" {
		return Target{}, fmt.Errorf("%w: %q", ErrScheme, scheme)
	}
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			return Target{}, ErrUserinfo
		}
		if scheme == "https" && u.User.Username() != "" {
			return Target{}, ErrUserinfo
		}
	}
	host := u.Hostname()
	if host == "" {
		return Target{}, ErrHost
	}
	if !allowLoopback {
		if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
			return Target{}, ErrHost
		}
		if host == "localhost" {
			return Target{}, ErrHost
		}
	}
	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "22"
		}
	}
	path := u.Path
	if path == "" {
		return Target{}, ErrHost
	}
	user := ""
	if u.User != nil {
		user = u.User.Username()
	}
	if scheme == "ssh" && user == "" {
		user = "git"
	}
	return Target{
		Raw:    raw,
		Scheme: scheme,
		Host:   host,
		Port:   port,
		Path:   path,
		User:   user,
	}, nil
}

// InsecureAllowed reports whether cache-only insecure TLS may apply for host
// under the operator-configured allow list (exact hostname match).
func InsecureAllowed(host, allowed string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	allowed = strings.ToLower(strings.TrimSpace(allowed))
	return host != "" && allowed != "" && host == allowed
}
