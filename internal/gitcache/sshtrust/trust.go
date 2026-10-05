//go:build linux || darwin

// Package sshtrust applies configured SSH trust to one exact endpoint. It
// supports plain-key OpenSSH host-key updates; no commands or trust helpers run.
package sshtrust

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	knownhosts "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/knownhosts"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshconfig"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshfile"
	"golang.org/x/crypto/ssh"
	"io"
	"reflect"
)

const maxFileBytes = 1024 * 1024
const maxKeys = 16
const maxPacket = 64 * 1024

var (
	ErrTrust             = errors.New("ssh host-key policy rejected the server")
	ErrTrustFiles        = errors.New("ssh configured known_hosts files are unavailable, changed, or unsupported")
	ErrUpdate            = errors.New("ssh authenticated host-key update failed")
	ErrUpdateUnsupported = errors.New("ssh host-key update requires unsupported known_hosts patterns or file layout")
	ErrConfirmation      = errors.New("ssh host-key policy requires interactive confirmation; native transport is noninteractive")
)

type snapshot struct {
	path   string
	data   []byte
	exists bool
	info   os.FileInfo
	attrs  map[string][]byte
}
type Manager struct {
	ctx                context.Context
	cfg                sshconfig.Config
	addr               string
	files              []snapshot
	globals            []snapshot
	pending            []Transition
	finalized          bool
	finished           bool
	finalErr           error
	committed          []Transition
	callback           ssh.HostKeyCallback
	userCallbacks      []ssh.HostKeyCallback
	mu                 sync.Mutex
	verified, eligible bool
	status             string
	initial            bool
	acceptedKey        []byte
}

func New(ctx context.Context, c sshconfig.Config, addr string) (*Manager, error) {
	if c.UpdateHostKeys == "ask" {
		return nil, ErrConfirmation
	}
	if c.UpdateHostKeys != "yes" && c.UpdateHostKeys != "no" {
		return nil, ErrTrust
	}
	if len(c.UserKnownHostsFiles)+len(c.GlobalKnownHostsFiles) > 16 {
		return nil, ErrTrustFiles
	}
	m := &Manager{cfg: c, addr: addr, status: "not_received", ctx: ctx}
	var all [][]byte
	for i, path := range append(append([]string(nil), c.UserKnownHostsFiles...), c.GlobalKnownHostsFiles...) {
		s, err := loadContext(ctx, path)
		if err != nil {
			return nil, err
		}
		if i < len(c.UserKnownHostsFiles) {
			m.files = append(m.files, s)
			cb, e := knownhosts.New(ctx, s.data)
			if e != nil {
				return nil, ErrTrustFiles
			}
			m.userCallbacks = append(m.userCallbacks, cb)
		}
		if i >= len(c.UserKnownHostsFiles) {
			m.globals = append(m.globals, s)
		}
		all = append(all, s.data)
	}
	cb, err := knownhosts.New(ctx, all...)
	if err != nil {
		return nil, ErrTrustFiles
	}
	m.callback = cb
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return m, nil
}
func load(path string) (snapshot, error) { return loadContext(context.Background(), path) }
func loadContext(ctx context.Context, path string) (snapshot, error) {
	s := snapshot{path: path}
	if ctx.Err() != nil {
		return s, ctx.Err()
	}
	real, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		if parent, e := filepath.EvalSymlinks(filepath.Dir(path)); e == nil {
			s.path = filepath.Join(parent, filepath.Base(path))
		}
		return s, nil
	}
	if err != nil {
		return s, ErrTrustFiles
	}
	s.path = real
	f, st, err := sshfile.OpenRegular(real, maxFileBytes)
	if err != nil {
		return s, ErrTrustFiles
	}
	defer f.Close()
	attrs, err := readAttributes(f)
	if err != nil {
		return s, err
	}
	data, err := readBounded(ctx, f)
	if err != nil {
		return s, err
	}
	after, err := f.Stat()
	if err != nil || !sameMetadata(st, after) {
		return s, ErrTrustFiles
	}
	afterAttrs, err := readAttributes(f)
	if err != nil || !reflect.DeepEqual(attrs, afterAttrs) {
		return s, ErrTrustFiles
	}
	s.exists = true
	s.info = st
	s.data = data
	s.attrs = attrs
	return s, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
func readBounded(ctx context.Context, r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx, r}, maxFileBytes+1))
	if err != nil || len(data) > maxFileBytes {
		return nil, ErrTrustFiles
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return data, nil
}
func unchanged(s snapshot) bool { return unchangedContext(context.Background(), s) }
func unchangedContext(ctx context.Context, s snapshot) bool {
	now, err := loadContext(ctx, s.path)
	return err == nil && now.exists == s.exists && bytes.Equal(now.data, s.data) && (!s.exists || sameMetadata(s.info, now.info) && reflect.DeepEqual(s.attrs, now.attrs))
}

func (m *Manager) Callback(host string, remote net.Addr, key ssh.PublicKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return m.ctx.Err()
	}
	if host != m.addr {
		return ErrTrust
	}
	// A rekey with the already accepted transport key needs no reenrollment.
	if m.initial && bytes.Equal(m.acceptedKey, key.Marshal()) {
		return nil
	}
	err := m.callback(host, remote, key)
	state := sshconfig.HostKeyTrusted
	if err != nil {
		var revoked *knownhosts.RevokedError
		var ke *knownhosts.KeyError
		switch {
		case errors.As(err, &revoked):
			state = sshconfig.HostKeyRevoked
		case errors.As(err, &ke):
			state = sshconfig.HostKeyChanged
			if len(ke.Want) == 0 {
				state = sshconfig.HostKeyMissing
			}
		default:
			return ErrTrust
		}
	}
	decision, err := sshconfig.DecideHostKey(m.cfg.StrictHostKeyChecking, state)
	if decision.Prompt {
		return ErrConfirmation
	}
	if err != nil || !decision.Accept {
		return ErrTrust
	}
	// Initial no/accept-new enrollment stores only the key proven by transport
	// KEX, never the later advertised key list. This connection remains ineligible
	// for UpdateHostKeys because its initial key was not previously trusted.
	if decision.Enroll && len(m.files) > 0 && !m.initial {
		data := append([]byte(nil), m.files[0].data...)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			data = append(data, '\n')
		}
		hostname := knownhosts.Normalize(m.addr)
		if m.cfg.HashKnownHosts {
			hostname = knownhosts.HashHostname(hostname)
		}
		data = append(data, []byte(knownhosts.Line([]string{hostname}, key)+"\n")...)
		// Like OpenSSH, inability to persist an accepted new key does not
		// reverse acceptance. Keep this connection ineligible for updates.
		if after, err := replaceSnapshot(m.ctx, m.files[0], data); err != nil {
			m.status = "enrollment_not_persisted"
		} else {
			m.status = "enrolled_initial_key"
			m.pending = append(m.pending, Transition{before: m.files[0], after: after, owner: m.Provenance()})
		}
	}
	eligible := false
	if decision.Verified {
		if _, cert := key.(*ssh.Certificate); !cert {
			for _, cb := range m.userCallbacks {
				if cb(host, remote, key) == nil {
					eligible = true
					break
				}
			}
		}
	}
	if !m.initial {
		m.verified = decision.Verified
		m.eligible = eligible && !decision.DisableHostKeyUpdates
		m.initial = true
	}
	m.acceptedKey = append([]byte(nil), key.Marshal()...)
	return nil
}
func (m *Manager) Verified() bool       { m.mu.Lock(); defer m.mu.Unlock(); return m.verified }
func (m *Manager) UpdateStatus() string { m.mu.Lock(); defer m.mu.Unlock(); return m.status }
func (m *Manager) setStatus(s string)   { m.mu.Lock(); m.status = s; m.mu.Unlock() }

func matchesHash(pattern, host string) bool {
	parts := strings.Split(pattern, "|")
	if len(parts) != 4 || parts[1] != "1" {
		return false
	}
	salt, e1 := base64.StdEncoding.DecodeString(parts[2])
	want, e2 := base64.StdEncoding.DecodeString(parts[3])
	if e1 != nil || e2 != nil {
		return false
	}
	mac := hmac.New(sha1.New, salt)
	mac.Write([]byte(host))
	return hmac.Equal(mac.Sum(nil), want)
}
func exactHost(entry, target string) bool {
	return entry == target || strings.HasPrefix(entry, "|1|") && matchesHash(entry, target)
}

// rewrite changes only exact/hashed endpoint entries, preserving every unrelated
// raw line and other names on shared lines. Wildcards/markers are not rewritten.
func rewrite(data []byte, addr string, keys []ssh.PublicKey, hash bool) ([]byte, error) {
	target := knownhosts.Normalize(addr)
	desired := map[string]ssh.PublicKey{}
	for _, key := range keys {
		desired[string(key.Marshal())] = key
	}
	present := map[string]bool{}
	var output bytes.Buffer
	for _, raw := range bytes.SplitAfter(data, []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] == '#' {
			output.Write(raw)
			continue
		}
		marker, hosts, key, comment, _, err := ssh.ParseKnownHosts(raw)
		if err != nil {
			return nil, ErrTrustFiles
		}
		hit := false
		for _, h := range hosts {
			if strings.ContainsAny(h, "*?!") && hostPatternMatch(strings.TrimPrefix(h, "!"), target) {
				return nil, ErrUpdateUnsupported
			}
		}
		for _, h := range hosts {
			if exactHost(h, target) {
				hit = true
			}
		}
		if !hit {
			output.Write(raw)
			continue
		}
		if marker != "" {
			return nil, ErrUpdateUnsupported
		}
		id := string(key.Marshal())
		if _, ok := desired[id]; ok {
			present[id] = true
			output.Write(raw)
			continue
		}
		var others []string
		for _, h := range hosts {
			if !exactHost(h, target) {
				others = append(others, h)
			}
		}
		if len(others) > 0 {
			line := knownhosts.Line(others, key)
			if comment != "" {
				line += " " + comment
			}
			output.WriteString(line + "\n")
		}
	}
	for _, key := range keys {
		if present[string(key.Marshal())] {
			continue
		}
		if output.Len() > 0 && !bytes.HasSuffix(output.Bytes(), []byte("\n")) {
			output.WriteByte('\n')
		}
		host := target
		if hash {
			host = knownhosts.HashHostname(host)
		}
		output.WriteString(knownhosts.Line([]string{host}, key) + "\n")
	}
	if output.Len() > maxFileBytes {
		return nil, ErrTrustFiles
	}
	return output.Bytes(), nil
}

func hostPatternMatch(p, s string) bool {
	si, pi, star, retry := 0, 0, -1, 0
	for si < len(s) {
		if pi < len(p) && (p[pi] == '?' || p[pi] == s[si]) {
			si++
			pi++
			continue
		}
		if pi < len(p) && p[pi] == '*' {
			star = pi
			pi++
			retry = si
			continue
		}
		if star >= 0 {
			retry++
			si = retry
			pi = star + 1
			continue
		}
		return false
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// Provenance binds public known-host bytes and the effective host trust policy.
// Authentication material and environment values are deliberately excluded.
func (m *Manager) Provenance() string {
	h := sha256.New()
	b, _ := json.Marshal(struct {
		Addr, User, Strict, Update string
		Hash                       bool
		UserFiles, GlobalFiles     []string
	}{m.addr, m.cfg.User, m.cfg.StrictHostKeyChecking, m.cfg.UpdateHostKeys, m.cfg.HashKnownHosts, m.cfg.UserKnownHostsFiles, m.cfg.GlobalKnownHostsFiles})
	h.Write(b)
	for _, s := range m.files {
		h.Write([]byte{0})
		h.Write([]byte(s.path))
		if s.exists {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
		}
		sum := sha256.Sum256(s.data)
		h.Write(sum[:])
	}
	for _, s := range m.globals {
		h.Write([]byte{0})
		sum := sha256.Sum256(s.data)
		h.Write(sum[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}
