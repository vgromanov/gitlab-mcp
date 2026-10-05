//go:build linux || darwin

package gitcache

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/listx"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshconfig"
)

// TestColdWarmAuthorizedSSHAcquire exercises Acquire → fetchSSH → publish → warm
// with StartSHA≠BaseSHA proveGrant, plus fork-miss and cancel negatives.
func TestColdWarmAuthorizedSSHAcquire(t *testing.T) {
	ip, head, base := commitIndexedPack(t)
	packBytes := append([]byte(nil), ip.PackBytes...)

	hostKey := mustEd25519(t)
	idPath, clientKey := writeSSHIdentity(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	known := writeSSHKnownHosts(t, hostKey, addr)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for {
			raw, e := ln.Accept()
			if e != nil {
				return
			}
			go func(raw net.Conn) {
				defer raw.Close()
				_ = raw.SetDeadline(time.Now().Add(15 * time.Second))
				cfg := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
					if meta.User() == "ssh-acquire" && bytes.Equal(key.Marshal(), clientKey.PublicKey().Marshal()) {
						return nil, nil
					}
					return nil, fmt.Errorf("reject")
				}}
				cfg.AddHostKey(hostKey)
				server, chans, reqs, e := ssh.NewServerConn(raw, cfg)
				if e != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(reqs)
				for ch := range chans {
					channel, requests, e := ch.Accept()
					if e != nil {
						continue
					}
					for r := range requests {
						switch r.Type {
						case "env":
							continue
						case "exec":
							_ = r.Reply(true, nil)
							if err := writeShallowAdv(channel, head, base); err != nil {
								_ = channel.Close()
								return
							}
							_, _ = io.Copy(io.Discard, io.LimitReader(channel, bounds.MaxPackBytes))
							req := packp.NewUploadPackRequest()
							req.Depth = packp.DepthCommits(2)
							_ = req.Capabilities.Set(capability.Shallow)
							resp := packp.NewUploadPackResponseWithPackfile(req, io.NopCloser(bytes.NewReader(packBytes)))
							resp.Shallows = []plumbing.Hash{base}
							_ = resp.Encode(channel)
							_ = channel.CloseWrite()
							_ = channel.Close()
						}
					}
				}
			}(raw)
		}
	}()

	home := t.TempDir()
	config := filepath.Join(home, "config")
	body := fmt.Sprintf("Match host 127.0.0.1\n User ssh-acquire\n Port %s\n IdentitiesOnly yes\n IdentityAgent none\n IdentityFile %s\n UserKnownHostsFile %s\n GlobalKnownHostsFile none\n StrictHostKeyChecking yes\n UpdateHostKeys no\n", port, idPath, known)
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	in := sshconfig.Input{Home: home, LocalUser: "local", UserConfig: config, SystemConfig: filepath.Join(home, "missing-system")}

	root := regressionRoot(t)
	svc, err := OpenService(ServiceConfig{
		Enabled:    true,
		Root:       root,
		QuotaBytes: bounds.BrootBytes + bounds.GenerationCharge()*4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close(context.Background())

	var domain AuthDomain
	actor := "ssh-actor"
	token := "ssh-tok"
	authDomain := domain.Bind(actor, token)
	g := completeTestGrant(Grant{
		OriginHost:  "127.0.0.1",
		ProjectID:   "1",
		ProjectPath: "g/p",
		SourceFork:  "2",
		AuthDomain:  authDomain,
		PolicyFP:    "fp",
		MRIID:       7,
		MRVersion:   MRVersionFromDiffRefs(head, base, head),
		HeadSHA:     head,
		BaseSHA:     base,
		StartSHA:    head, // ≠ base
		ActorID:     actor,
		SSHURL:      "ssh://git@" + addr + "/repo.git",
	})
	auth := StaticAuthorizer{Grant: g}
	intent := AcquireIntent{
		ProjectID: "1", MRIID: 7, Depth: 2, Token: token, Transport: "ssh",
		ExpectedHead: head, ExpectedBase: base, ExpectedStart: head,
		ExpectedMRVersion: g.MRVersion, AllowLoopback: true,
		SSH: listx.Options{SSHConfig: &in, Timeout: 8 * time.Second},
	}

	cold, err := svc.Acquire(context.Background(), intent, auth)
	if err != nil {
		t.Fatalf("cold ssh: %v", err)
	}
	if cold.Warm || cold.Objects == 0 || cold.PackBytes == 0 {
		t.Fatalf("cold %#v", cold)
	}
	warm, err := svc.Acquire(context.Background(), intent, auth)
	if err != nil {
		t.Fatalf("warm: %v", err)
	}
	if !warm.Warm || warm.GenerationID != cold.GenerationID {
		t.Fatalf("warm %#v", warm)
	}

	// Fork isolation: other SourceFork is a distinct namespace — no warm hit.
	otherNS := NamespaceID(authDomain, g.CanonicalInstance, g.ProjectID, "77")
	if hit, err := svc.Manager().lookupWarm(context.Background(), GrantFingerprint(g), otherNS); err != nil && !errors.Is(err, ErrUnavailable) {
		t.Fatalf("lookup: %v", err)
	} else if hit != nil {
		t.Fatal("other fork warm-hit")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.Acquire(ctx, intent, auth); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	_ = ln.Close()
	<-serverDone
}

func mustEd25519(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func writeSSHIdentity(t *testing.T) (string, ssh.Signer) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".pub", ssh.MarshalAuthorizedKey(s.PublicKey()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, s
}

func writeSSHKnownHosts(t *testing.T, host ssh.Signer, addr string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	line := knownhosts.Line([]string{addr}, host.PublicKey())
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
