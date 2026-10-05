// Package redact removes credentials from errors before they are printed.
package redact

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const (
	// MaxLayers is the unwrap depth for one diagnostic chain.
	MaxLayers = 8
	// MaxString is the maximum length of one diagnostic type or text.
	MaxString = 240
	// MaxTotal is the maximum JSON diagnostic emitted by the caller.
	MaxTotal = 2048
)

// Error returns err with secrets and URL userinfo removed.
// A nil err returns nil. The result is a plain error so wrapped auth URLs
// cannot leak through Unwrap.
func Error(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		msg = strings.ReplaceAll(msg, secret, "[redacted]")
		if esc := url.QueryEscape(secret); esc != secret {
			msg = strings.ReplaceAll(msg, esc, "[redacted]")
		}
	}
	msg = stripUserinfo(msg)
	if msg == err.Error() {
		return err
	}
	return errors.New(msg)
}

var (
	// ErrHTTPS is the fixed category for an HTTPS failure that is not one of
	// the caller's local sentinels.
	ErrHTTPS = errors.New("https request failed")
	// ErrHTTPSTimeout is the fixed category for a network timeout. It does
	// not include an address.
	ErrHTTPSTimeout = errors.New("https connection timed out")
)

// PublicHTTPS returns a fixed HTTPS category. It does not call Error on err,
// does not copy response text, and the result does not unwrap to err.
// A listed local sentinel is returned as that sentinel. Context cancellation
// and deadlines are preserved. SSH listing does not use this function.
func PublicHTTPS(err error, keep ...error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	for _, sentinel := range keep {
		if sentinel != nil && errors.Is(err, sentinel) {
			return sentinel
		}
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return ErrHTTPSTimeout
	}
	return ErrHTTPS
}

// Frame is one sanitized error layer. It does not retain the original error.
// Negotiation frames keep What plus separate supported and requested lists.
// Those lists contain recognized public algorithm names only.
type Frame struct {
	Type      string   `json:"type"`
	Text      string   `json:"text"`
	What      string   `json:"what,omitempty"`
	Supported []string `json:"supported,omitempty"`
	Requested []string `json:"requested,omitempty"`
	Redacted  int      `json:"redacted,omitempty"`
	Omitted   int      `json:"omitted,omitempty"`
}

// Chain copies at most MaxLayers of err into sanitized frames.
// A typed algorithm negotiation error keeps its stage and both algorithm
// lists. Unknown names are counted in Redacted and are not copied. Every
// other string is kept only when it matches a fixed phrase after credential
// and key-blob redaction. The returned frames do not unwrap to err.
func Chain(err error) []Frame {
	var out []Frame
	for i := 0; err != nil && i < MaxLayers; i++ {
		out = append(out, frameOf(err))
		err = errors.Unwrap(err)
	}
	return out
}

func frameOf(err error) Frame {
	if e, ok := err.(*ssh.AlgorithmNegotiationError); ok {
		return negotiationFrame(e)
	}
	return Frame{Type: bound(fmt.Sprintf("%T", err)), Text: bound(layerText(err))}
}

// HostFrame is the fixed missing, mismatch, or revoked category.
// It does not include key material.
func HostFrame(class string) Frame {
	switch class {
	case "missing":
		return Frame{Type: "*knownhosts.KeyError", Text: "knownhosts: key is unknown"}
	case "mismatch":
		return Frame{Type: "*knownhosts.KeyError", Text: "knownhosts: key mismatch"}
	case "revoked":
		return Frame{Type: "*knownhosts.RevokedError", Text: "knownhosts: key is revoked"}
	default:
		return Frame{Type: "*knownhosts.KeyError", Text: "[redacted]"}
	}
}

func layerText(err error) string {
	switch e := err.(type) {
	case *knownhosts.KeyError:
		if len(e.Want) == 0 {
			return "knownhosts: key is unknown"
		}
		return "knownhosts: key mismatch"
	case *knownhosts.RevokedError:
		return "knownhosts: key is revoked"
	case *net.OpError:
		return netText(e)
	}
	if inner := errors.Unwrap(err); inner != nil {
		full := err.Error()
		suffix := ": " + inner.Error()
		if strings.HasSuffix(full, suffix) {
			return scrub(strings.TrimSuffix(full, suffix))
		}
		return "[redacted]"
	}
	return scrub(err.Error())
}

const maxAlgos = 24

func negotiationFrame(e *ssh.AlgorithmNegotiationError) Frame {
	what := "algorithm"
	if _, ok := knownWhat[e.What]; ok {
		what = e.What
	}
	supported, redactedS, omittedS := filterAlgos(e.SupportedAlgorithms)
	requested, redactedR, omittedR := filterAlgos(e.RequestedAlgorithms)
	return Frame{
		Type:      "*ssh.AlgorithmNegotiationError",
		Text:      "ssh: no common algorithm for " + what,
		What:      what,
		Supported: supported,
		Requested: requested,
		Redacted:  redactedS + redactedR,
		Omitted:   omittedS + omittedR,
	}
}

func filterAlgos(in []string) (kept []string, redacted, omitted int) {
	for _, name := range in {
		if _, ok := knownAlgo[name]; !ok {
			redacted++
			continue
		}
		if len(kept) == maxAlgos {
			omitted++
			continue
		}
		kept = append(kept, name)
	}
	return kept, redacted, omitted
}

var knownWhat = map[string]struct{}{
	"key exchange":                 {},
	"host key":                     {},
	"client to server cipher":      {},
	"server to client cipher":      {},
	"client to server MAC":         {},
	"server to client MAC":         {},
	"client to server compression": {},
	"server to client compression": {},
}

// knownAlgo is the closed set of public SSH algorithm names that a
// diagnostic may print. Matching is exact. A name outside this set is counted
// and dropped, including strings that only look like algorithm tokens.
var knownAlgo = map[string]struct{}{
	"curve25519-sha256": {}, "curve25519-sha256@libssh.org": {},
	"ecdh-sha2-nistp256": {}, "ecdh-sha2-nistp384": {}, "ecdh-sha2-nistp521": {},
	"diffie-hellman-group1-sha1": {}, "diffie-hellman-group14-sha1": {},
	"diffie-hellman-group14-sha256": {}, "diffie-hellman-group15-sha512": {},
	"diffie-hellman-group16-sha512": {}, "diffie-hellman-group17-sha512": {},
	"diffie-hellman-group18-sha512":      {},
	"diffie-hellman-group-exchange-sha1": {}, "diffie-hellman-group-exchange-sha256": {},
	"mlkem768x25519-sha256":  {},
	"sntrup761x25519-sha512": {}, "sntrup761x25519-sha512@openssh.com": {},
	"ext-info-c": {}, "ext-info-s": {},
	"kex-strict-c-v00@openssh.com": {}, "kex-strict-s-v00@openssh.com": {},
	"aes128-gcm@openssh.com": {}, "aes256-gcm@openssh.com": {},
	"chacha20-poly1305@openssh.com": {},
	"aes128-ctr":                    {}, "aes192-ctr": {}, "aes256-ctr": {},
	"aes128-cbc": {}, "aes192-cbc": {}, "aes256-cbc": {},
	"3des-cbc": {}, "arcfour": {}, "arcfour128": {}, "arcfour256": {},
	"hmac-sha2-256-etm@openssh.com": {}, "hmac-sha2-512-etm@openssh.com": {},
	"hmac-sha2-256": {}, "hmac-sha2-512": {}, "hmac-sha1": {}, "hmac-sha1-96": {},
	"hmac-sha1-etm@openssh.com": {}, "hmac-md5": {}, "hmac-md5-96": {},
	"hmac-md5-etm@openssh.com": {},
	"umac-64@openssh.com":      {}, "umac-128@openssh.com": {},
	"umac-64-etm@openssh.com": {}, "umac-128-etm@openssh.com": {},
	"none": {}, "zlib": {}, "zlib@openssh.com": {},
	"ssh-ed25519": {}, "ssh-rsa": {}, "ssh-dss": {},
	"rsa-sha2-256": {}, "rsa-sha2-512": {},
	"ecdsa-sha2-nistp256": {}, "ecdsa-sha2-nistp384": {}, "ecdsa-sha2-nistp521": {},
	"sk-ssh-ed25519@openssh.com": {}, "sk-ecdsa-sha2-nistp256@openssh.com": {},
	"ssh-ed25519-cert-v01@openssh.com": {}, "ssh-rsa-cert-v01@openssh.com": {},
	"ssh-dss-cert-v01@openssh.com":      {},
	"rsa-sha2-256-cert-v01@openssh.com": {}, "rsa-sha2-512-cert-v01@openssh.com": {},
	"ecdsa-sha2-nistp256-cert-v01@openssh.com":    {},
	"ecdsa-sha2-nistp384-cert-v01@openssh.com":    {},
	"ecdsa-sha2-nistp521-cert-v01@openssh.com":    {},
	"sk-ssh-ed25519-cert-v01@openssh.com":         {},
	"sk-ecdsa-sha2-nistp256-cert-v01@openssh.com": {},
}

func netText(op *net.OpError) string {
	if op == nil || (op.Op != "dial" && op.Op != "read" && op.Op != "write") || op.Err == nil {
		return "[redacted]"
	}
	msg := op.Err.Error()
	for _, phrase := range []string{"connection refused", "connection reset", "i/o timeout", "no such host", "network is unreachable"} {
		if strings.Contains(msg, phrase) {
			return op.Op + ": " + phrase
		}
	}
	return "[redacted]"
}

var (
	pemRE  = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]{1,40}-----.*?-----END [A-Z0-9 ]{1,40}-----`)
	keyRE  = regexp.MustCompile(`(?:ssh-(?:ed25519|rsa|dss)|ecdsa-sha2-[A-Za-z0-9-]+|sk-(?:ssh-ed25519|ecdsa-sha2-nistp256)@openssh\.com)\s+[A-Za-z0-9+/=]{8,}`)
	fpRE   = regexp.MustCompile(`SHA256:[A-Za-z0-9+/]{8,}`)
	b64RE  = regexp.MustCompile(`[A-Za-z0-9+/]{40,}={0,2}`)
	credRE = regexp.MustCompile(`(?i)\b(?:password|passwd|secret|token|authorization)\b\s*[:=]\s*\S+`)
	authRE = regexp.MustCompile(`^ssh: unable to authenticate, attempted methods \[[a-z0-9 ]*\], no supported methods remain$`)
	netRE  = regexp.MustCompile(`^(dial|read|write): (connection refused|connection reset|i/o timeout|no such host|network is unreachable)$`)
)

func scrub(s string) string {
	s = stripUserinfo(s)
	s = pemRE.ReplaceAllString(s, "[redacted]")
	s = keyRE.ReplaceAllString(s, "[redacted]")
	s = fpRE.ReplaceAllString(s, "[redacted]")
	s = b64RE.ReplaceAllString(s, "[redacted]")
	s = credRE.ReplaceAllString(s, "[redacted]")
	s = bound(s)
	if s == "[redacted]" || allowed(s) {
		return s
	}
	return "[redacted]"
}

func allowed(s string) bool {
	switch s {
	case "ssh: handshake failed",
		"ssh host key verification failed",
		"ssh connection failed",
		"ssh connection failed: handshake",
		"ssh connection failed: network",
		"ssh agent authentication failed",
		"SSH_AUTH_SOCK agent has no usable identities",
		"ssh agent is unavailable",
		"knownhosts: key is unknown",
		"knownhosts: key mismatch",
		"knownhosts: key is revoked",
		"context canceled",
		"context deadline exceeded",
		"EOF",
		"unexpected EOF",
		"ssh: invalid packet length, packet too large",
		"ssh: invalid packet length, packet too small",
		"ssh: invalid packet length":
		return true
	}
	return authRE.MatchString(s) || netRE.MatchString(s)
}

func bound(s string) string {
	if len(s) <= MaxString {
		return s
	}
	return s[:MaxString]
}

func stripUserinfo(s string) string {
	var b strings.Builder
	rest := s
	for {
		i := strings.Index(rest, "://")
		if i < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:i+3])
		rest = rest[i+3:]
		slash := strings.IndexAny(rest, "/ \t\r\n\"'")
		authority := rest
		tail := ""
		if slash >= 0 {
			authority = rest[:slash]
			tail = rest[slash:]
		}
		if at := strings.LastIndex(authority, "@"); at >= 0 {
			b.WriteString("[redacted]@")
			b.WriteString(authority[at+1:])
		} else {
			b.WriteString(authority)
		}
		if slash < 0 {
			return b.String()
		}
		rest = tail
	}
}
