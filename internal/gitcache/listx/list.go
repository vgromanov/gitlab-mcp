// Package listx reads advertised refs over explicit HTTPS or Go SSH clients.
// It does not request a pack, write a remote, or use the default transport registry.
package listx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/origin"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/redact"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/single"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshconfig"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/tlsx"
)

var (
	ErrBusy             = single.ErrBusy
	ErrResponseTooLarge = errors.New("advertised refs response exceeds the byte limit")
	ErrTokenMissing     = errors.New("GITLAB_PERSONAL_ACCESS_TOKEN is not set in the process environment")
	ErrAgent            = errors.New("ssh agent is unavailable")
	ErrNoIdentities     = errors.New("SSH_AUTH_SOCK agent has no usable identities")
	ErrAuth             = errors.New("ssh agent authentication failed")
	ErrHostKey          = errors.New("ssh host key verification failed")
	ErrNetwork          = errors.New("ssh connection failed: network")
	ErrHandshake        = errors.New("ssh connection failed: handshake")
	ErrSSH              = errors.New("ssh connection failed")
	ErrKnownHosts       = errors.New("known_hosts path is required")
)

// Ref is one advertised hash reference.
type Ref struct {
	Name string
	Hash string
}

// Options configures one listing. Zero Timeout and MaxBytes use the production caps.
// Values above those caps are clamped down. allowLoopback is set only by tests.
type Options struct {
	prepared       *preparedSSH
	SSHConfig      *sshconfig.Input
	CAPath         string
	Insecure       bool
	Token          string
	KnownHosts     string
	AgentSocket    string
	IdentityFile   string
	Timeout        time.Duration
	MaxBytes       int64
	SSHDiagnostics bool
	Diag           *Diagnostic
	allowLoopback  bool
}

// Diagnostic is a sanitized SSH trace. It does not hold the raw error and
// has no Unwrap method. JSON is bounded by redact.MaxTotal.
type Diagnostic struct {
	SSHConfigResolved            bool           `json:"ssh_config_resolved,omitempty"`
	HostKeyUpdate                string         `json:"host_key_update,omitempty"`
	EnvironmentRequests          int            `json:"environment_requests,omitempty"`
	AgentHasSigners              bool           `json:"agent_has_signers"`
	TCPConnected                 bool           `json:"tcp_connected"`
	HostKeyCallbackInvoked       bool           `json:"host_key_callback_invoked"`
	HostKeyVerified              bool           `json:"host_key_verified"`
	PublicKeyAuthCallbackInvoked bool           `json:"public_key_auth_callback_invoked"`
	AuthMethod                   string         `json:"auth_method,omitempty"`
	HostTrust                    string         `json:"host_trust,omitempty"`
	Stage                        string         `json:"stage"`
	Chain                        []redact.Frame `json:"chain,omitempty"`
	Truncated                    bool           `json:"truncated,omitempty"`
	hostFrame                    redact.Frame
	sawHost                      bool
}

// JSON returns one bounded diagnostic object. When the object exceeds
// redact.MaxTotal, local algorithm names are dropped before peer names.
func (d *Diagnostic) JSON() string {
	if d == nil {
		return ""
	}
	b, err := json.Marshal(d)
	for err == nil && len(b) > redact.MaxTotal && d.dropAlgo() {
		d.Truncated = true
		b, err = json.Marshal(d)
	}
	if err != nil || len(b) > redact.MaxTotal {
		d.Chain = nil
		d.Truncated = true
		b, err = json.Marshal(d)
	}
	if err != nil || len(b) > redact.MaxTotal {
		return `{"truncated":true}`
	}
	return string(b)
}

func (d *Diagnostic) dropAlgo() bool {
	for i := range d.Chain {
		if n := len(d.Chain[i].Supported); n > 0 {
			d.Chain[i].Supported = d.Chain[i].Supported[:n-1]
			if len(d.Chain[i].Supported) == 0 {
				d.Chain[i].Supported = nil
			}
			d.Chain[i].Omitted++
			return true
		}
	}
	for i := range d.Chain {
		if n := len(d.Chain[i].Requested); n > 0 {
			d.Chain[i].Requested = d.Chain[i].Requested[:n-1]
			if len(d.Chain[i].Requested) == 0 {
				d.Chain[i].Requested = nil
			}
			d.Chain[i].Omitted++
			return true
		}
	}
	return false
}

func (d *Diagnostic) finish(fixed, raw error) {
	if d == nil {
		return
	}
	d.Stage = diagStage(fixed)
	chain := redact.Chain(raw)
	if d.sawHost {
		chain = append([]redact.Frame{d.hostFrame}, chain...)
		if len(chain) > redact.MaxLayers {
			chain = chain[:redact.MaxLayers]
			d.Truncated = true
		}
	}
	d.Chain = chain
}

func (o Options) withLoopback() Options {
	o.allowLoopback = true
	return o
}

func (o Options) limits() (time.Duration, int64) {
	timeout := o.Timeout
	if timeout <= 0 || timeout > bounds.Timeout {
		timeout = bounds.Timeout
	}
	maxBytes := o.MaxBytes
	if maxBytes <= 0 || maxBytes > bounds.MaxRefBytes {
		maxBytes = bounds.MaxRefBytes
	}
	return timeout, maxBytes
}

// List reads advertised refs for rawURL. The URL must be https or ssh and,
// outside tests, one of the two pinned corporate remotes. List and fetch share
// one process lock.
func List(ctx context.Context, rawURL string, opt Options) ([]Ref, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	target, err := origin.Parse(rawURL, opt.allowLoopback)
	if err != nil {
		return nil, err
	}
	var refs []Ref
	err = single.Do(func() error {
		timeout, maxBytes := opt.limits()
		opCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		var opErr error
		switch target.Scheme {
		case "https":
			refs, opErr = listHTTPS(opCtx, target, opt, timeout, maxBytes)
		case "ssh":
			if opt.allowLoopback && opt.SSHConfig == nil {
				refs, opErr = listSSH(opCtx, target, opt, timeout, maxBytes)
			} else {
				if opt.KnownHosts != "" || opt.IdentityFile != "" || opt.AgentSocket != "" {
					return ErrSSHOverrides
				}
				refs, opErr = listConfiguredSSH(opCtx, target, opt, timeout, maxBytes)
			}
		default:
			opErr = origin.ErrScheme
		}
		return opErr
	})
	// HTTPS uses a fixed category boundary. SSH keeps redact.Error so listing
	// diagnostics and fixed SSH stage errors stay on their existing path.
	if target.Scheme == "https" {
		return refs, redact.PublicHTTPS(err,
			ErrTokenMissing, ErrResponseTooLarge, single.ErrBusy,
			origin.ErrScheme, origin.ErrUserinfo,
			tlsx.ErrMalformedCA, tlsx.ErrInsecure,
		)
	}
	return refs, redact.Error(err, opt.Token)
}

func listHTTPS(ctx context.Context, target origin.Target, opt Options, timeout time.Duration, maxBytes int64) ([]Ref, error) {
	if opt.Token == "" {
		return nil, ErrTokenMissing
	}
	tlsCfg, err := tlsx.Build(tlsx.Input{
		ServerName: target.Host,
		CAPath:     opt.CAPath,
		Insecure:   opt.Insecure,
	})
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: timeout}
	rt := &http.Transport{
		Proxy:                 func(*http.Request) (*url.URL, error) { return nil, nil },
		DialContext:           dialer.DialContext,
		TLSClientConfig:       tlsCfg,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          1,
	}
	hc := &http.Client{
		Transport: &limitedTransport{base: rt, max: maxBytes},
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirects are disabled")
		},
	}
	client := githttp.NewClientWithOptions(hc, &githttp.ClientOptions{
		RedirectPolicy: githttp.NoFollowRedirects,
	})
	ep, err := transport.NewEndpoint(target.Raw)
	if err != nil {
		return nil, origin.ErrScheme
	}
	sess, err := client.NewUploadPackSession(ep, &githttp.BasicAuth{
		Username: "oauth2",
		Password: opt.Token,
	})
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	ar, err := sess.AdvertisedReferencesContext(ctx)
	if err != nil {
		return nil, err
	}
	return refsFromAdv(ar), nil
}

func listSSH(ctx context.Context, target origin.Target, opt Options, timeout time.Duration, maxBytes int64) ([]Ref, error) {
	if opt.KnownHosts == "" {
		return nil, ErrKnownHosts
	}
	if st, err := os.Stat(opt.KnownHosts); err != nil || st.IsDir() || st.Size() == 0 {
		return nil, ErrKnownHosts
	}
	callback, err := knownhosts.New(opt.KnownHosts)
	if err != nil {
		return nil, ErrKnownHosts
	}
	group := newCancelClosers(ctx)
	defer group.stop()
	var signers []ssh.Signer
	if opt.IdentityFile != "" {
		if opt.AgentSocket != "" {
			return nil, ErrIdentityConflict
		}
		signer, err := readIdentity(opt.IdentityFile)
		if err != nil {
			return nil, err
		}
		signers = []ssh.Signer{signer}
	} else {
		socket := opt.AgentSocket
		if socket == "" {
			socket = os.Getenv("SSH_AUTH_SOCK")
		}
		if socket == "" {
			return nil, ErrAgent
		}
		agentConn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "unix", socket)
		if err != nil {
			return nil, failSSH(ctx, ErrAgent)
		}
		group.bind(ctx, agentConn)
		signers, err = agent.NewClient(agentConn).Signers()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, ErrAgent
		}
	}
	diag := opt.diag()
	if diag != nil {
		diag.AgentHasSigners = opt.IdentityFile == "" && len(signers) > 0
	}
	if len(signers) == 0 {
		if diag != nil {
			diag.Stage = "agent"
		}
		return nil, ErrNoIdentities
	}

	user := target.User
	if user == "" {
		user = "git"
	}
	cfg := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
			if diag != nil {
				diag.PublicKeyAuthCallbackInvoked = true
				diag.AuthMethod = "publickey(agent)"
				if opt.IdentityFile != "" {
					diag.AuthMethod = "publickey(file)"
				}
			}
			return signers, nil
		})},
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			if diag != nil {
				diag.HostKeyCallbackInvoked = true
			}
			herr := callback(hostname, remote, key)
			if herr != nil {
				if diag != nil {
					diag.HostTrust = hostClass(herr)
					diag.hostFrame = redact.HostFrame(diag.HostTrust)
					diag.sawHost = true
					diag.HostKeyVerified = false
				}
				return ErrHostKey
			}
			if diag != nil {
				diag.HostKeyVerified = true
			}
			return nil
		},
		Timeout: timeout,
	}
	port := target.Port
	if port == "" {
		port = "22"
	}
	addr := net.JoinHostPort(target.Host, port)
	raw, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, failDiag(ctx, diag, err)
	}
	if diag != nil {
		diag.TCPConnected = true
	}
	group.bind(ctx, raw)
	sshConn, chans, reqs, err := ssh.NewClientConn(raw, addr, cfg)
	if err != nil {
		return nil, failDiag(ctx, diag, err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return nil, failDiag(ctx, diag, ErrSSH)
	}
	defer session.Close()
	stdout, err := session.StdoutPipe()
	if err != nil {
		return nil, failDiag(ctx, diag, ErrSSH)
	}
	command, err := uploadPackCommand(target.Path)
	if err != nil {
		return nil, err
	}
	if err := session.Start(command); err != nil {
		return nil, failDiag(ctx, diag, ErrSSH)
	}
	ar := packp.NewAdvRefs()
	err = ar.Decode(&limitedReader{r: stdout, max: maxBytes})
	_ = session.Close()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrResponseTooLarge) {
			return nil, err
		}
		return nil, ErrSSH
	}
	if diag != nil {
		diag.Stage = "ok"
	}
	return refsFromAdv(ar), nil
}

func (o Options) diag() *Diagnostic {
	if !o.SSHDiagnostics {
		return nil
	}
	return o.Diag
}

func failDiag(ctx context.Context, diag *Diagnostic, err error) error {
	fixed := failSSH(ctx, err)
	if diag != nil {
		diag.finish(fixed, err)
	}
	return fixed
}

func diagStage(err error) string {
	switch {
	case errors.Is(err, ErrNetwork):
		return "network"
	case errors.Is(err, ErrAuth):
		return "authentication"
	case errors.Is(err, ErrHostKey):
		return "hostkey"
	case errors.Is(err, ErrHandshake):
		return "handshake"
	case errors.Is(err, ErrAgent), errors.Is(err, ErrNoIdentities):
		return "agent"
	default:
		return "ssh"
	}
}

func hostClass(err error) string {
	var revoked *knownhosts.RevokedError
	if errors.As(err, &revoked) {
		return "revoked"
	}
	var key *knownhosts.KeyError
	if errors.As(err, &key) {
		if len(key.Want) == 0 {
			return "missing"
		}
		return "mismatch"
	}
	return "rejected"
}

func failSSH(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		return ErrSSH
	}
	if errors.Is(err, ErrHostKey) || errors.Is(err, ErrAgent) || errors.Is(err, ErrNoIdentities) || errors.Is(err, ErrAuth) || errors.Is(err, ErrNetwork) || errors.Is(err, ErrHandshake) || errors.Is(err, ErrSSH) {
		return err
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unable to authenticate") || strings.Contains(msg, "no supported methods"):
		return ErrAuth
	case strings.Contains(msg, "algorithm"):
		return ErrHandshake
	case strings.Contains(msg, "host key"):
		return ErrHostKey
	case networkFailure(err):
		return ErrNetwork
	case strings.Contains(msg, "handshake"):
		return ErrHandshake
	default:
		return ErrSSH
	}
}

func networkFailure(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	msg := err.Error()
	for _, phrase := range []string{
		"connection refused",
		"connection reset",
		"i/o timeout",
		"no such host",
		"network is unreachable",
		"broken pipe",
		"use of closed network connection",
	} {
		if strings.Contains(msg, phrase) {
			return true
		}
	}
	return false
}

func uploadPackCommand(path string) (string, error) {
	if path == "" || path[0] != '/' {
		return "", errors.New("ssh repository path is invalid")
	}
	for _, r := range path {
		if r == '\'' || r == '\n' || r == '\r' || r == '\\' {
			return "", errors.New("ssh repository path is invalid")
		}
	}
	return "git-upload-pack '" + path + "'", nil
}

type cancelClosers struct {
	mu       sync.Mutex
	conns    []io.Closer
	closed   bool
	stopOnce sync.Once
	done     chan struct{}
	exited   chan struct{}
}

func newCancelClosers(ctx context.Context) *cancelClosers {
	c := &cancelClosers{done: make(chan struct{}), exited: make(chan struct{})}
	go func() {
		defer close(c.exited)
		select {
		case <-ctx.Done():
			c.close()
		case <-c.done:
		}
	}()
	return c
}

func (c *cancelClosers) stop() {
	c.close()
	c.stopOnce.Do(func() { close(c.done) })
	<-c.exited
}

func (c *cancelClosers) bind(ctx context.Context, conn net.Conn) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || ctx.Err() != nil {
		_ = conn.Close()
		return
	}
	c.conns = append(c.conns, conn)
}

func (c *cancelClosers) close() {
	c.mu.Lock()
	c.closed = true
	conns := c.conns
	c.conns = nil
	c.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func refsFromAdv(ar *packp.AdvRefs) []Ref {
	if ar == nil {
		return nil
	}
	out := make([]Ref, 0, len(ar.References))
	for name, hash := range ar.References {
		out = append(out, Ref{Name: name, Hash: hash.String()})
	}
	return out
}

type limitedTransport struct {
	base http.RoundTripper
	max  int64
}

func (t *limitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := t.base.RoundTrip(req)
	if err != nil || res == nil || res.Body == nil {
		return res, err
	}
	res.Body = &limitedReadCloser{limitedReader: limitedReader{r: res.Body, max: t.max}, c: res.Body}
	return res, nil
}

type limitedReadCloser struct {
	limitedReader
	c io.Closer
}

func (l *limitedReadCloser) Close() error { return l.c.Close() }

type limitedReader struct {
	r   io.Reader
	n   int64
	max int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if l.n > l.max {
		return 0, ErrResponseTooLarge
	}
	if l.n == l.max {
		var one [1]byte
		n, err := l.r.Read(one[:])
		if n > 0 {
			l.n += int64(n)
			return 0, ErrResponseTooLarge
		}
		return 0, err
	}
	remain := l.max - l.n
	if int64(len(p)) > remain {
		p = p[:remain]
	}
	n, err := l.r.Read(p)
	l.n += int64(n)
	return n, err
}
