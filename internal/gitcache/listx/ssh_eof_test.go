package listx

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// Count at the agent protocol boundary, without observing or retaining signed
// data. An auth callback only enumerates signers; it does not prove signing.
type countingAgent struct {
	agent.ExtendedAgent
	calls atomic.Int32
}

func (a *countingAgent) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	a.calls.Add(1)
	return a.ExtendedAgent.Sign(key, data)
}
func (a *countingAgent) SignWithFlags(key ssh.PublicKey, data []byte, flags agent.SignatureFlags) (*ssh.Signature, error) {
	a.calls.Add(1)
	return a.ExtendedAgent.SignWithFlags(key, data, flags)
}

// This is a synthetic reproduction of the observed failure boundary, not a
// claim that the corporate server runs this callback or intentionally closes.
func TestSSHUnsignedEnquiryOutcomes(t *testing.T) {
	for _, outcome := range []string{"accept", "reject", "close"} {
		t.Run(outcome, func(t *testing.T) {
			host, _ := mustKey(t)
			key, priv := mustKey(t)
			ring := agent.NewKeyring()
			if err := ring.Add(agent.AddedKey{PrivateKey: priv}); err != nil {
				t.Fatal(err)
			}
			counter := &countingAgent{ExtendedAgent: ring.(agent.ExtendedAgent)}
			sock := filepath.Join(shortDir(t), "a.sock")
			al, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			defer al.Close()
			agentDone := make(chan struct{})
			go func() {
				defer close(agentDone)
				c, err := al.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				_ = agent.ServeAgent(counter, c)
			}()

			var addr string
			if outcome != "close" {
				allow := key.PublicKey()
				if outcome == "reject" {
					allow = nil
				}
				var stop func()
				addr, stop = serveGitSSH(t, host, allow, true)
				defer stop()
			} else {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				addr = listener.Addr().String()
				go func() {
					c, err := listener.Accept()
					if err != nil {
						return
					}
					defer c.Close()
					_ = c.SetDeadline(time.Now().Add(5 * time.Second))
					cfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
						_ = c.Close() // End transport without acknowledging the unsigned enquiry.
						return nil, errors.New("synthetic transport closure")
					}}
					cfg.AddHostKey(host)
					_, _, _, _ = ssh.NewServerConn(c, cfg)
				}()
			}
			var diag Diagnostic
			refs, err := List(context.Background(), "ssh://git@"+addr+"/repo.git", Options{
				KnownHosts: writeKnown(t, host, addr), AgentSocket: sock,
				Timeout: 5 * time.Second, SSHDiagnostics: true, Diag: &diag,
			}.withLoopback())
			select {
			case <-agentDone:
			case <-time.After(5 * time.Second):
				t.Fatal("agent connection not closed")
			}
			if !diag.HostKeyVerified || !diag.PublicKeyAuthCallbackInvoked {
				t.Fatal("expected verified host and public-key callback")
			}
			switch outcome {
			case "accept":
				if err != nil || len(refs) != 1 || counter.calls.Load() != 1 || diag.Stage != "ok" {
					t.Fatalf("accepted enquiry: error=%v refs=%d signs=%d stage=%s", err, len(refs), counter.calls.Load(), diag.Stage)
				}
			case "reject":
				if !errors.Is(err, ErrAuth) || counter.calls.Load() != 0 || diag.Stage != "authentication" {
					t.Fatalf("rejected enquiry: error=%v signs=%d stage=%s", err, counter.calls.Load(), diag.Stage)
				}
			case "close":
				if !errors.Is(err, ErrHandshake) || errors.Is(err, ErrAuth) || counter.calls.Load() != 0 || diag.Stage != "handshake" || !strings.Contains(diag.JSON(), `"text":"EOF"`) {
					t.Fatalf("closed enquiry: error=%v signs=%d diagnostic=%s", err, counter.calls.Load(), diag.JSON())
				}
			}
		})
	}
}
