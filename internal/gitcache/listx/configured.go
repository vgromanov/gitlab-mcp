package listx

import (
	"context"
	"errors"
	"net"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/origin"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshconfig"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshtrust"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

var ErrSSHConfig = errors.New("ssh configuration could not be resolved")
var ErrSSHOverrides = errors.New("SSH reads existing configuration; separate identity, agent, or known_hosts overrides are not accepted")

func listConfiguredSSH(ctx context.Context, target origin.Target, opt Options, timeout time.Duration, maxBytes int64) ([]Ref, error) {
	input := sshconfig.Input{Host: target.Host}
	if opt.SSHConfig != nil {
		input = *opt.SSHConfig
		input.Host = target.Host
	}
	if input.Home == "" {
		var err error
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
	group := newCancelClosers(ctx)
	defer group.stop()
	var available []ssh.Signer
	if resolved.IdentityAgent != "" {
		ac, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "unix", resolved.IdentityAgent)
		if err == nil {
			group.bind(ctx, ac)
			available, _ = agent.NewClient(ac).Signers()
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Like OpenSSH, an unavailable configured agent does not suppress available
		// configured file identities. It never selects a different socket.
	}
	signers, err := configSigners(resolved, available)
	if err != nil {
		return nil, err
	}
	diag := opt.diag()
	if diag != nil {
		diag.AgentHasSigners = len(available) > 0
		diag.SSHConfigResolved = true
	}
	var hostErr error
	cfg := &ssh.ClientConfig{User: resolved.User, Timeout: timeout,
		Auth: []ssh.AuthMethod{ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
			if diag != nil {
				diag.PublicKeyAuthCallbackInvoked = true
				diag.AuthMethod = "publickey(config)"
			}
			return signers, nil
		})},
		HostKeyCallback: func(host string, remote net.Addr, key ssh.PublicKey) error {
			if diag != nil {
				diag.HostKeyCallbackInvoked = true
			}
			err := trust.Callback(host, remote, key)
			hostErr = err
			if diag != nil {
				diag.HostKeyVerified = trust.Verified()
			}
			return err
		},
	}
	secureAlgorithms(cfg)
	raw, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, failDiag(ctx, diag, err)
	}
	group.bind(ctx, raw)
	if diag != nil {
		diag.TCPConnected = true
	}
	conn, chans, requests, err := ssh.NewClientConn(raw, addr, cfg)
	if err != nil {
		if hostErr != nil {
			if diag != nil {
				diag.Stage = "hostkey"
			}
			return nil, hostErr
		}
		return nil, failDiag(ctx, diag, err)
	}
	updates := trust.Start(ctx, conn, requests)
	empty := make(chan *ssh.Request)
	close(empty)
	client := ssh.NewClient(conn, chans, empty)
	defer func() { _ = client.Close(); updates.Wait() }()
	session, err := client.NewSession()
	if err != nil {
		return nil, failDiag(ctx, diag, ErrSSH)
	}
	defer session.Close()
	// OpenSSH sends configured env requests without requesting an acknowledgement;
	// the server's AcceptEnv policy may decline them. Preserve that behavior.
	env := sshconfig.SessionEnvironment(resolved, os.Environ())
	if len(env) > 128 {
		return nil, errors.New("configured SSH environment exceeds request limit")
	}
	for _, entry := range env {
		if len(entry.Name)+len(entry.Value) > 32*1024 {
			return nil, errors.New("configured SSH environment exceeds request size limit")
		}
		_, err := session.SendRequest("env", false, ssh.Marshal(struct{ Name, Value string }{entry.Name, entry.Value}))
		if err != nil {
			return nil, ErrSSH
		}
	}
	if diag != nil {
		diag.EnvironmentRequests = len(env)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		return nil, ErrSSH
	}
	command, err := uploadPackCommand(target.Path)
	if err != nil {
		return nil, err
	}
	if err := session.Start(command); err != nil {
		return nil, ErrSSH
	}
	refs := packp.NewAdvRefs()
	err = refs.Decode(&limitedReader{r: stdout, max: maxBytes})
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
	if err := updates.Sync(); err != nil {
		return nil, err
	}
	_ = client.Close()
	updates.Wait()
	if err := updates.Error(); err != nil {
		return nil, err
	}
	if diag != nil {
		diag.HostKeyUpdate = trust.UpdateStatus()
		diag.Stage = "ok"
	}
	return refsFromAdv(refs), nil
}
