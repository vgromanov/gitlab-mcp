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

// Parse accepts https, ssh://, and GitLab SCP-style git@host:path. Loopback
// is accepted solely when allowLoopback is set by tests. Credential userinfo
// is rejected. The ssh user "git" is routing, not a credential secret.
func Parse(raw string, allowLoopback bool) (Target, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "ext::") || strings.HasPrefix(raw, "file:") || strings.HasPrefix(raw, "git:") {
		return Target{}, ErrScheme
	}
	if user, host, path, ok := splitSCP(raw); ok {
		return finishSCP(raw, user, host, path, allowLoopback)
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

// splitSCP reports a GitLab scp-style user@host:path. ssh:// URLs are not this form.
func splitSCP(raw string) (user, host, path string, ok bool) {
	if strings.Contains(raw, "://") || strings.ContainsAny(raw, " \t?#") {
		return "", "", "", false
	}
	at := strings.IndexByte(raw, '@')
	if at <= 0 || strings.Contains(raw[at+1:], "@") {
		return "", "", "", false
	}
	user = raw[:at]
	rest := raw[at+1:]
	if strings.ContainsAny(user, ":/") {
		return user, "", "", true
	}
	if strings.HasPrefix(rest, "[") {
		end := strings.IndexByte(rest, ']')
		if end < 2 || end+1 >= len(rest) || rest[end+1] != ':' {
			return "", "", "", false
		}
		return user, rest[1:end], rest[end+2:], true
	}
	colon := strings.IndexByte(rest, ':')
	if colon <= 0 {
		return "", "", "", false
	}
	return user, rest[:colon], rest[colon+1:], true
}

func finishSCP(raw, user, host, path string, allowLoopback bool) (Target, error) {
	if user == "" || strings.ContainsAny(user, ":/") {
		return Target{}, ErrUserinfo
	}
	if err := checkHost(host, allowLoopback); err != nil {
		return Target{}, err
	}
	path, err := repoPath(path)
	if err != nil {
		return Target{}, err
	}
	return Target{Raw: raw, Scheme: "ssh", Host: host, Port: "22", Path: path, User: user}, nil
}

func checkHost(host string, allowLoopback bool) error {
	if host == "" {
		return ErrHost
	}
	if !allowLoopback {
		if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
			return ErrHost
		}
		if strings.EqualFold(host, "localhost") {
			return ErrHost
		}
	}
	return nil
}

func repoPath(path string) (string, error) {
	if path == "" || strings.ContainsAny(path, "\\?#\r\n\x00") {
		return "", ErrHost
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if strings.Contains(path, "//") {
		return "", ErrHost
	}
	for _, seg := range strings.Split(strings.Trim(path, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", ErrHost
		}
	}
	return path, nil
}

// InsecureAllowed reports whether cache-only insecure TLS may apply for host
// under the operator-configured allow list (exact hostname match).
func InsecureAllowed(host, allowed string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	allowed = strings.ToLower(strings.TrimSpace(allowed))
	return host != "" && allowed != "" && host == allowed
}
