//go:build linux || darwin

package listx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshconfig"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

func agentPrepareOptions(t *testing.T, socket, pubkey string) (Options, string) {
	t.Helper()
	home := t.TempDir()
	id := writePrepareIdentity(t)
	known := filepath.Join(home, "known_hosts")
	if e := os.WriteFile(known, []byte(knownhosts.Line([]string{"127.0.0.1:22"}, mustPrepareKey(t).PublicKey())+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	cfg := filepath.Join(home, "config")
	body := fmt.Sprintf("Host 127.0.0.1\n User git\n Port 22\n PubkeyAuthentication %s\n IdentitiesOnly no\n IdentityAgent %s\n IdentityFile %s\n UserKnownHostsFile %s\n GlobalKnownHostsFile none\n StrictHostKeyChecking yes\n UpdateHostKeys no\n", pubkey, socket, id, known)
	if e := os.WriteFile(cfg, []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
	in := sshconfig.Input{Home: home, LocalUser: "local", UserConfig: cfg, SystemConfig: filepath.Join(home, "missing")}
	return Options{SSHConfig: &in}.WithLoopback(), id
}

func TestPrepareTrustAgentRPCContextOwnership(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline", "pubkey-disabled"} {
		t.Run(mode, func(t *testing.T) {
			dir, e := os.MkdirTemp("/tmp", "native-agent-")
			if e != nil {
				t.Fatal(e)
			}
			defer os.RemoveAll(dir)
			sock := filepath.Join(dir, "agent")
			ln, e := net.Listen("unix", sock)
			if e != nil {
				t.Fatal(e)
			}
			entered, done, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			go func() {
				defer close(done)
				c, e := ln.Accept()
				if e != nil {
					return
				}
				defer c.Close()
				stop := make(chan struct{})
				defer close(stop)
				go func() {
					select {
					case <-release:
						c.Close()
					case <-stop:
					}
				}()
				request := make([]byte, 5)
				if _, e := io.ReadFull(c, request); e != nil {
					return
				}
				close(entered)
				_, _ = io.Copy(io.Discard, c)
			}()
			defer func() {
				unblock()
				ln.Close()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("agent reader leaked")
				}
			}()
			pub := "yes"
			if mode == "pubkey-disabled" {
				pub = "no"
			}
			opt, _ := agentPrepareOptions(t, sock, pub)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "deadline" || mode == "pubkey-disabled" {
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
			}
			result := make(chan error, 1)
			go func() { _, _, e := PrepareTrust(ctx, "ssh://git@127.0.0.1:22/repo.git", opt); result <- e }()
			if mode != "pubkey-disabled" {
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("no agent inventory request")
				}
				if mode == "cancel" {
					cancel()
				}
			}
			expected := context.Canceled
			if mode == "deadline" {
				expected = context.DeadlineExceeded
			}
			if mode == "pubkey-disabled" {
				expected = ErrConfigPublicKeyDisabled
			}
			select {
			case e := <-result:
				if e != expected || errors.Unwrap(e) != nil {
					t.Fatalf("unsafe or incorrect admission result: %v", e)
				}
			case <-time.After(500 * time.Millisecond):
				unblock()
				t.Fatal("agent RPC exceeded caller lifetime")
			}
			if mode == "pubkey-disabled" {
				select {
				case <-entered:
					t.Fatal("unsupported auth policy requested inventory")
				default:
				}
				if ctx.Err() != nil {
					t.Fatal("unsupported policy consumed the caller deadline")
				}
			} else {
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("owned agent socket not closed")
				}
			}
		})
	}
}

func TestPrepareTrustAgentInventoryAndFileFallbackIdentity(t *testing.T) {
	dir, e := os.MkdirTemp("/tmp", "native-agent-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "agent")
	ring := agent.NewKeyring()
	ln, e := net.Listen("unix", sock)
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan struct{})
	var requests sync.WaitGroup
	go func() {
		defer close(done)
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			requests.Add(1)
			go func() { defer requests.Done(); defer c.Close(); _ = agent.ServeAgent(ring, c) }()
		}
	}()
	defer func() { ln.Close(); <-done; requests.Wait() }()
	opt, id := agentPrepareOptions(t, sock, "yes")
	add := func(path string) {
		b, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		key, e := ssh.ParseRawPrivateKey(b)
		clear(b)
		if e != nil {
			t.Fatal(e)
		}
		if e := ring.Add(agent.AddedKey{PrivateKey: key}); e != nil {
			t.Fatal(e)
		}
	}
	add(id)
	_, first, e := PrepareTrust(context.Background(), "ssh://git@127.0.0.1:22/repo.git", opt)
	if e != nil || first == "" {
		t.Fatalf("responsive inventory: %v", e)
	}
	_, same, e := PrepareTrust(context.Background(), "ssh://git@127.0.0.1:22/repo.git", opt)
	if e != nil || same != first {
		t.Fatal("unchanged inventory changed admission identity")
	}
	add(writePrepareIdentity(t))
	_, changed, e := PrepareTrust(context.Background(), "ssh://git@127.0.0.1:22/repo.git", opt)
	if e != nil || changed == first {
		t.Fatal("new selected public identity reused admission identity")
	}
	body, e := os.ReadFile(opt.SSHConfig.UserConfig)
	if e != nil {
		t.Fatal(e)
	}
	unavailable := filepath.Join(dir, "not-present")
	body = []byte(strings.ReplaceAll(string(body), "IdentityAgent "+sock, "IdentityAgent "+unavailable))
	if e := os.WriteFile(opt.SSHConfig.UserConfig, body, 0600); e != nil {
		t.Fatal(e)
	}
	_, fallback, e := PrepareTrust(context.Background(), "ssh://git@127.0.0.1:22/repo.git", opt)
	if e != nil || fallback == changed {
		t.Fatalf("configured file fallback/policy isolation: %v", e)
	}
}
