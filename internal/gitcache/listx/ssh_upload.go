package listx

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/origin"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshconfig"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshtrust"
)

// WithLoopback enables loopback URLs for hermetic tests only.
func (o Options) WithLoopback() Options {
	o.allowLoopback = true
	return o
}

// NewSSHUploadPackSession dials native SSH using the reviewed configuration,
// identity, algorithm, and trust path used for listing, then returns a
// go-git UploadPackSession for object fetch.
func NewSSHUploadPackSession(ctx context.Context, rawURL string, opt Options) (transport.UploadPackSession, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	target, err := origin.Parse(rawURL, opt.allowLoopback)
	if err != nil {
		return nil, nil, err
	}
	if target.Scheme != "ssh" {
		return nil, nil, origin.ErrScheme
	}
	timeout, maxBytes := opt.limits()
	if opt.KnownHosts != "" || opt.IdentityFile != "" || opt.AgentSocket != "" {
		if !(opt.allowLoopback && opt.SSHConfig == nil) {
			return nil, nil, ErrSSHOverrides
		}
	}

	prepared := opt.prepared
	if prepared == nil {
		prepared, err = prepareSSH(ctx, target, opt)
		if err != nil {
			return nil, nil, err
		}
	}
	if prepared.raw != target.Raw {
		return nil, nil, ErrSSHConfig
	}
	resolved, trust, addr := prepared.cfg, prepared.trust, prepared.addr
	if _, err := configuredCandidates(resolved); err != nil {
		return nil, nil, err
	}
	group := newCancelClosers(ctx)
	var available []ssh.Signer
	if resolved.IdentityAgent != "" {
		ac, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "unix", resolved.IdentityAgent)
		if err == nil {
			group.bind(ctx, ac)
			available, _ = agent.NewClient(ac).Signers()
		}
		if ctx.Err() != nil {
			group.stop()
			return nil, nil, ctx.Err()
		}
	}
	signers, err := configSigners(resolved, available)
	if err != nil {
		group.stop()
		return nil, nil, err
	}
	if prepared.authFP != "" && prepared.authFP != authSelectionFingerprint(resolved, signers) {
		group.stop()
		return nil, nil, ErrSSHConfig
	}
	var hostErr error
	cfg := &ssh.ClientConfig{
		User:    resolved.User,
		Timeout: timeout,
		Auth: []ssh.AuthMethod{ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
			return signers, nil
		})},
		HostKeyCallback: func(host string, remote net.Addr, key ssh.PublicKey) error {
			err := trust.Callback(host, remote, key)
			hostErr = err
			return err
		},
	}
	secureAlgorithms(cfg)
	raw, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		group.stop()
		return nil, nil, failSSH(ctx, err)
	}
	group.bind(ctx, raw)
	conn, chans, requests, err := ssh.NewClientConn(raw, addr, cfg)
	if err != nil {
		group.stop()
		if hostErr != nil {
			return nil, nil, hostErr
		}
		return nil, nil, failSSH(ctx, err)
	}
	updates := trust.Start(ctx, conn, requests)
	empty := make(chan *ssh.Request)
	close(empty)
	client := ssh.NewClient(conn, chans, empty)
	session, err := client.NewSession()
	if err != nil {
		_ = client.Close()
		updates.Wait()
		group.stop()
		return nil, nil, ErrSSH
	}
	env := sshconfig.SessionEnvironment(resolved, os.Environ())
	if len(env) > 128 {
		_ = session.Close()
		_ = client.Close()
		updates.Wait()
		group.stop()
		return nil, nil, errors.New("configured SSH environment exceeds request limit")
	}
	for _, entry := range env {
		if len(entry.Name)+len(entry.Value) > 32*1024 {
			_ = session.Close()
			_ = client.Close()
			updates.Wait()
			group.stop()
			return nil, nil, errors.New("configured SSH environment exceeds request size limit")
		}
		if _, err := session.SendRequest("env", false, ssh.Marshal(struct{ Name, Value string }{entry.Name, entry.Value})); err != nil {
			_ = session.Close()
			_ = client.Close()
			updates.Wait()
			group.stop()
			return nil, nil, ErrSSH
		}
	}
	command, err := uploadPackCommand(target.Path)
	if err != nil {
		_ = session.Close()
		_ = client.Close()
		updates.Wait()
		group.stop()
		return nil, nil, err
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		_ = client.Close()
		updates.Wait()
		group.stop()
		return nil, nil, ErrSSH
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		_ = client.Close()
		updates.Wait()
		group.stop()
		return nil, nil, ErrSSH
	}
	if err := session.Start(command); err != nil {
		_ = session.Close()
		_ = client.Close()
		updates.Wait()
		group.stop()
		return nil, nil, ErrSSH
	}
	s := &sshUploadSession{
		stdout:   &limitedReader{r: stdout, max: maxBytes},
		stdin:    stdin,
		session:  session,
		client:   client,
		updates:  updates,
		group:    group,
		maxBytes: maxBytes,
		started:  time.Now(),
		trust:    trust,
	}
	return s, func() { _ = s.Close() }, nil
}

type sshUploadSession struct {
	mu      sync.Mutex
	stdout  io.Reader
	stdin   io.WriteCloser
	session *ssh.Session
	client  *ssh.Client
	updates interface {
		Wait()
		Sync() error
		Error() error
	}
	trust    *sshtrust.Manager
	group    *cancelClosers
	maxBytes int64
	adv      *packp.AdvRefs
	closed   bool
	started  time.Time
}

func (s *sshUploadSession) AdvertisedReferences() (*packp.AdvRefs, error) {
	return s.AdvertisedReferencesContext(context.Background())
}

func (s *sshUploadSession) AdvertisedReferencesContext(ctx context.Context) (*packp.AdvRefs, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrSSH
	}
	if s.adv != nil {
		return s.adv, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	refs := packp.NewAdvRefs()
	if err := refs.Decode(s.stdout); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrResponseTooLarge) {
			return nil, err
		}
		return nil, ErrSSH
	}
	s.adv = refs
	return refs, nil
}

func (s *sshUploadSession) UploadPack(ctx context.Context, req *packp.UploadPackRequest) (*packp.UploadPackResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrSSH
	}
	if s.adv == nil {
		return nil, ErrSSH
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := req.UploadRequest.Encode(s.stdin); err != nil {
		return nil, ErrSSH
	}
	if err := req.UploadHaves.Encode(s.stdin, false); err != nil {
		return nil, ErrSSH
	}
	if err := pktline.NewEncoder(s.stdin).EncodeString("done\n"); err != nil {
		return nil, ErrSSH
	}
	_ = s.stdin.Close()
	resp := packp.NewUploadPackResponse(req)
	rc := io.NopCloser(s.stdout)
	if err := resp.Decode(rc); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrSSH
	}
	return resp, nil
}

func (s *sshUploadSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	if s.session != nil {
		_ = s.session.Close()
	}
	// Match configured listing: propagate Sync/Error before success; ignore
	// routine session close noise that is not a trust-update failure.
	var updateErr error
	if s.updates != nil {
		updateErr = s.updates.Sync()
	}
	if s.client != nil {
		_ = s.client.Close()
	}
	if s.updates != nil {
		s.updates.Wait()
		if err := s.updates.Error(); err != nil {
			updateErr = err
		}
	}
	if s.group != nil {
		s.group.stop()
	}
	if s.trust != nil {
		updateErr = s.trust.Finalize(updateErr)
	}
	return updateErr
}

type preparedSSH struct {
	raw, addr string
	authFP    string
	cfg       sshconfig.Config
	trust     *sshtrust.Manager
}

// PrepareTrust validates configured trust and effective public-key authentication
// admission without enrollment. The returned Options carry the same snapshot into
// the cold SSH session. Provenance binds trust bytes/policy plus process-local
// credential-selection identity (never private keys or secret digests).
func PrepareTrust(ctx context.Context, rawURL string, opt Options) (Options, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return opt, "", err
	}
	target, err := origin.Parse(rawURL, opt.allowLoopback)
	if err != nil {
		return opt, "", err
	}
	if target.Scheme != "ssh" {
		return opt, "", origin.ErrScheme
	}
	if opt.KnownHosts != "" || opt.IdentityFile != "" || opt.AgentSocket != "" {
		if !(opt.allowLoopback && opt.SSHConfig == nil) {
			return opt, "", ErrSSHOverrides
		}
	}
	p, err := prepareSSH(ctx, target, opt)
	if err != nil {
		return opt, "", err
	}
	authFP, err := admitPreparedAuth(ctx, p)
	if err != nil {
		return opt, "", err
	}
	p.authFP = authFP
	opt.prepared = p
	return opt, p.trust.Provenance() + ":" + authFP, nil
}

// TrustUpdateStatus reports the prepared trust manager's last owned update status.
func (o Options) TrustUpdateStatus() string {
	if o.prepared == nil || o.prepared.trust == nil {
		return ""
	}
	return o.prepared.trust.UpdateStatus()
}

// ClearPrepared drops a prior trust/auth snapshot so PrepareTrust reloads from disk.
func (o Options) ClearPrepared() Options {
	o.prepared = nil
	return o
}

func admitPreparedAuth(ctx context.Context, p *preparedSSH) (string, error) {
	if p == nil {
		return "", ErrSSHConfig
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if _, err := configuredCandidates(p.cfg); err != nil {
		return "", err
	}
	group := newCancelClosers(ctx)
	defer group.stop()
	timeout := 5 * time.Second
	var available []ssh.Signer
	if p.cfg.IdentityAgent != "" {
		ac, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "unix", p.cfg.IdentityAgent)
		if err == nil {
			group.bind(ctx, ac)
			_ = ac.SetDeadline(agentDeadline(ctx, timeout))
			available, _ = agent.NewClient(ac).Signers()
			if d, ok := ctx.Deadline(); ok && !time.Now().Before(d) {
				return "", context.DeadlineExceeded
			}
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	signers, err := configSigners(p.cfg, available)
	if err != nil {
		return "", err
	}
	return authSelectionFingerprint(p.cfg, signers), nil
}

// sshInputForTarget copies optional SSH config and always binds the authorized
// URL user. Configured User still wins inside Resolve; an absent User, including
// %r, must not fall back to the local OS account.
func sshInputForTarget(target origin.Target, opt Options) sshconfig.Input {
	port := 0
	if n, err := strconv.Atoi(target.Port); err == nil && n >= 1 && n <= 65535 {
		port = n
	}
	input := sshconfig.Input{Host: target.Host, RemoteUser: target.User, RemotePort: port}
	if opt.SSHConfig != nil {
		input = *opt.SSHConfig
		input.Host = target.Host
		input.RemoteUser = target.User
		input.RemotePort = port
	}
	return input
}

func prepareSSH(ctx context.Context, target origin.Target, opt Options) (*preparedSSH, error) {
	var err error
	input := sshInputForTarget(target, opt)
	if input.Home == "" {
		input.Home, err = os.UserHomeDir()
		if err != nil {
			return nil, ErrSSHConfig
		}
	}
	if input.LocalUser == "" {
		u, err := user.Current()
		if err != nil {
			return nil, ErrSSHConfig
		}
		input.LocalUser = u.Username
	}
	resolved, err := sshconfig.Resolve(input)
	if err != nil {
		return nil, err
	}
	if err := sshconfig.CheckHostKeyUpdateSupport(resolved); err != nil {
		return nil, err
	}
	if !strings.EqualFold(resolved.HostName, target.Host) || strconv.Itoa(resolved.Port) != target.Port {
		return nil, errors.New("SSH configuration resolves outside the authorized host/port; connection refused")
	}
	if resolved.ControlPath != "" {
		if _, err := os.Lstat(resolved.ControlPath); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("SSH control path became available; native multiplexing is unsupported")
		}
	}
	addr := net.JoinHostPort(resolved.HostName, strconv.Itoa(resolved.Port))
	trust, err := sshtrust.New(ctx, resolved, addr)
	if err != nil {
		return nil, err
	}
	return &preparedSSH{raw: target.Raw, addr: addr, cfg: resolved, trust: trust}, nil
}

func agentDeadline(ctx context.Context, timeout time.Duration) time.Time {
	d := time.Now().Add(timeout)
	if caller, ok := ctx.Deadline(); ok && caller.Before(d) {
		return caller
	}
	return d
}

// TrustTransitions exposes only finalized immutable receipts, never a status waiver.
func (o Options) TrustTransitions() []sshtrust.Transition {
	if o.prepared == nil || o.prepared.trust == nil {
		return nil
	}
	return o.prepared.trust.Transitions()
}
func (o Options) ValidateRefresh(ctx context.Context, fresh Options, receipts []sshtrust.Transition) error {
	p, q := o.prepared, fresh.prepared
	if p == nil || q == nil || p.raw != q.raw || p.addr != q.addr || p.authFP == "" || p.authFP != q.authFP {
		return ErrSSHConfig
	}
	return p.trust.ValidateRefresh(ctx, q.trust, receipts)
}
