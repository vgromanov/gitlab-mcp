//go:build linux || darwin

package gitcache

import (
	"bytes"
	"context"
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

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/listx"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshconfig"
)

// F2: warm SSH reads must apply effective authentication admission and bind
// process-local credential-selection identity before content lookup.

func TestF2_WarmRejectsPubkeyAuthenticationDisabled(t *testing.T) {
	home, port, idPath, known, addr, head, base, cleanup := startF2SSHFixture(t)
	defer cleanup()

	writeF2Config(t, home, port, idPath, known, "PubkeyAuthentication yes\nIdentitiesOnly yes\nIdentityAgent none\n")
	svc, auth, intent := openF2Acquire(t, home, addr, head, base)

	cold, err := svc.Acquire(context.Background(), intent, auth)
	if err != nil {
		t.Fatalf("cold baseline: %v", err)
	}
	if cold.Warm {
		t.Fatal("expected cold")
	}

	writeF2Config(t, home, port, idPath, known, "PubkeyAuthentication no\nIdentitiesOnly yes\nIdentityAgent none\n")
	intent.trustFP = "" // fresh intent; prepare must re-admit from rewritten config
	intent.sourceSSH, intent.targetSSH = listx.Options{}, listx.Options{}
	intent.SSH = listx.Options{SSHConfig: &sshconfig.Input{Home: home, LocalUser: "local", UserConfig: filepath.Join(home, "config"), SystemConfig: filepath.Join(home, "missing-system")}, Timeout: 8 * time.Second}

	warm, err := svc.Acquire(context.Background(), intent, auth)
	if err == nil || warm != nil && warm.Warm {
		t.Fatalf("PubkeyAuthentication=no warm-hit bypassed auth admission (F2): warm=%#v err=%v", warm, err)
	}
	if !errors.Is(err, listx.ErrConfigPublicKeyDisabled) && !errors.Is(err, ErrAuthz) {
		// Admission may surface the native config error or map to authz; either fails closed.
		t.Logf("deny err=%v", err)
	}
	if errors.Is(err, listx.ErrConfigPublicKeyDisabled) || errors.Is(err, ErrAuthz) || errors.Is(err, listx.ErrConfigNoIdentities) {
		return
	}
	t.Fatalf("unexpected warm path error (F2): %v", err)
}

func TestF2_WarmMissesChangedIdentitySelection(t *testing.T) {
	home, port, idPath, known, addr, head, base, cleanup := startF2SSHFixture(t)
	defer cleanup()

	writeF2Config(t, home, port, idPath, known, "PubkeyAuthentication yes\nIdentitiesOnly yes\nIdentityAgent none\n")
	svc, auth, intent := openF2Acquire(t, home, addr, head, base)
	cold, err := svc.Acquire(context.Background(), intent, auth)
	if err != nil {
		t.Fatalf("cold: %v", err)
	}

	otherID, _ := writeSSHIdentity(t)
	writeF2Config(t, home, port, otherID, known, "PubkeyAuthentication yes\nIdentitiesOnly yes\nIdentityAgent none\n")
	intent.trustFP = ""
	intent.sourceSSH, intent.targetSSH = listx.Options{}, listx.Options{}
	intent.SSH = listx.Options{SSHConfig: &sshconfig.Input{Home: home, LocalUser: "local", UserConfig: filepath.Join(home, "config"), SystemConfig: filepath.Join(home, "missing-system")}, Timeout: 8 * time.Second}

	res, err := svc.Acquire(context.Background(), intent, auth)
	if err == nil && res != nil && res.Warm && res.GenerationID == cold.GenerationID {
		t.Fatal("changed IdentityFile reused warm generation (F2)")
	}
}

func TestF2_UnsupportedCertificateIdentityRejectedBeforeWarm(t *testing.T) {
	home, port, idPath, known, addr, head, base, cleanup := startF2SSHFixture(t)
	defer cleanup()
	writeF2Config(t, home, port, idPath, known, "PubkeyAuthentication yes\nIdentitiesOnly yes\nIdentityAgent none\n")
	svc, auth, intent := openF2Acquire(t, home, addr, head, base)
	if _, err := svc.Acquire(context.Background(), intent, auth); err != nil {
		t.Fatalf("cold: %v", err)
	}
	// Certificate sidecar next to identity is unsupported by native selector.
	if err := os.WriteFile(idPath+"-cert.pub", []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA cert\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	intent.trustFP = ""
	intent.sourceSSH, intent.targetSSH = listx.Options{}, listx.Options{}
	_, err := svc.Acquire(context.Background(), intent, auth)
	if err == nil {
		t.Fatal("certificate identity warm path accepted (F2)")
	}
	if !errors.Is(err, listx.ErrConfigCertificate) && !errors.Is(err, ErrAuthz) {
		t.Fatalf("certificate deny: %v", err)
	}
}

func TestF2_UnchangedSupportedConfigWarmPositive(t *testing.T) {
	home, port, idPath, known, addr, head, base, cleanup := startF2SSHFixture(t)
	defer cleanup()
	writeF2Config(t, home, port, idPath, known, "PubkeyAuthentication yes\nIdentitiesOnly yes\nIdentityAgent none\n")
	svc, auth, intent := openF2Acquire(t, home, addr, head, base)
	cold, err := svc.Acquire(context.Background(), intent, auth)
	if err != nil {
		t.Fatalf("cold: %v", err)
	}
	warm, err := svc.Acquire(context.Background(), intent, auth)
	if err != nil || !warm.Warm || warm.GenerationID != cold.GenerationID {
		t.Fatalf("unchanged warm positive (F2): %#v err=%v", warm, err)
	}
}

func TestF2_WarmMissesIdentitiesOnlyPolicyChange(t *testing.T) {
	home, port, idPath, known, addr, head, base, cleanup := startF2SSHFixture(t)
	defer cleanup()
	writeF2Config(t, home, port, idPath, known, "PubkeyAuthentication yes\nIdentitiesOnly yes\nIdentityAgent none\n")
	svc, auth, intent := openF2Acquire(t, home, addr, head, base)
	cold, err := svc.Acquire(context.Background(), intent, auth)
	if err != nil {
		t.Fatalf("cold: %v", err)
	}
	writeF2Config(t, home, port, idPath, known, "PubkeyAuthentication yes\nIdentitiesOnly no\nIdentityAgent none\n")
	intent.trustFP = ""
	intent.sourceSSH, intent.targetSSH = listx.Options{}, listx.Options{}
	intent.SSH = listx.Options{SSHConfig: &sshconfig.Input{Home: home, LocalUser: "local", UserConfig: filepath.Join(home, "config"), SystemConfig: filepath.Join(home, "missing-system")}, Timeout: 8 * time.Second}
	res, err := svc.Acquire(context.Background(), intent, auth)
	if err == nil && res != nil && res.Warm && res.GenerationID == cold.GenerationID {
		t.Fatal("IdentitiesOnly policy change reused warm generation (F2)")
	}
}

func TestF2_MissingSoleIdentityRejectedBeforeWarm(t *testing.T) {
	home, port, idPath, known, addr, head, base, cleanup := startF2SSHFixture(t)
	defer cleanup()
	writeF2Config(t, home, port, idPath, known, "PubkeyAuthentication yes\nIdentitiesOnly yes\nIdentityAgent none\n")
	svc, auth, intent := openF2Acquire(t, home, addr, head, base)
	if _, err := svc.Acquire(context.Background(), intent, auth); err != nil {
		t.Fatalf("cold: %v", err)
	}
	if err := os.Remove(idPath); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(idPath + ".pub")
	intent.trustFP = ""
	intent.sourceSSH, intent.targetSSH = listx.Options{}, listx.Options{}
	_, err := svc.Acquire(context.Background(), intent, auth)
	if err == nil {
		t.Fatal("missing sole identity warm path accepted (F2)")
	}
	if !errors.Is(err, listx.ErrConfigNoIdentities) && !errors.Is(err, ErrAuthz) {
		t.Fatalf("missing identity deny: %v", err)
	}
}

func writeF2Config(t *testing.T, home, port, idPath, known, authLines string) {
	t.Helper()
	body := fmt.Sprintf("Match host 127.0.0.1\n User ssh-acquire\n Port %s\n %s IdentityFile %s\n UserKnownHostsFile %s\n GlobalKnownHostsFile none\n StrictHostKeyChecking yes\n UpdateHostKeys no\n", port, authLines, idPath, known)
	if err := os.WriteFile(filepath.Join(home, "config"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func startF2SSHFixture(t *testing.T) (home, port, idPath, known, addr string, head, base plumbing.Hash, cleanup func()) {
	t.Helper()
	ip, h, b := commitIndexedPack(t)
	packBytes := append([]byte(nil), ip.PackBytes...)
	hostKey := mustEd25519(t)
	idPath, clientKey := writeSSHIdentity(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr = ln.Addr().String()
	_, port, _ = net.SplitHostPort(addr)
	known = writeSSHKnownHosts(t, hostKey, addr)
	done := make(chan struct{})
	go func() {
		defer close(done)
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
							_ = writeShallowAdv(channel, h, b)
							_, _ = io.Copy(io.Discard, io.LimitReader(channel, bounds.MaxPackBytes))
							req := packp.NewUploadPackRequest()
							req.Depth = packp.DepthCommits(2)
							_ = req.Capabilities.Set(capability.Shallow)
							resp := packp.NewUploadPackResponseWithPackfile(req, io.NopCloser(bytes.NewReader(packBytes)))
							resp.Shallows = []plumbing.Hash{b}
							_ = resp.Encode(channel)
							_ = channel.CloseWrite()
							_ = channel.Close()
						}
					}
				}
			}(raw)
		}
	}()
	home = t.TempDir()
	cleanup = func() {
		_ = ln.Close()
		<-done
	}
	return home, port, idPath, known, addr, h, b, cleanup
}

func openF2Acquire(t *testing.T, home, addr string, head, base plumbing.Hash) (*Service, StaticAuthorizer, AcquireIntent) {
	t.Helper()
	root := regressionRoot(t)
	svc, err := OpenService(ServiceConfig{Enabled: true, Root: root, QuotaBytes: bounds.BrootBytes + bounds.GenerationCharge()*4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close(context.Background()) })
	var domain AuthDomain
	actor, token := "f2-actor", "f2-tok"
	authDomain := domain.Bind(actor, token)
	g := completeTestGrant(Grant{
		OriginHost: "127.0.0.1", ProjectID: "1", ProjectPath: "g/p", SourceFork: "2",
		AuthDomain: authDomain, PolicyFP: "fp", MRIID: 7,
		MRVersion: MRVersionFromDiffRefs(head, base, base),
		HeadSHA:   head, BaseSHA: base, StartSHA: base, ActorID: actor,
		SSHURL: "ssh://git@" + addr + "/repo.git",
	})
	in := sshconfig.Input{Home: home, LocalUser: "local", UserConfig: filepath.Join(home, "config"), SystemConfig: filepath.Join(home, "missing-system")}
	intent := AcquireIntent{
		ProjectID: "1", MRIID: 7, Depth: 2, Token: token, Transport: "ssh",
		ExpectedHead: head, ExpectedBase: base, ExpectedStart: base,
		ExpectedMRVersion: g.MRVersion, AllowLoopback: true,
		SSH: listx.Options{SSHConfig: &in, Timeout: 8 * time.Second},
	}
	return svc, StaticAuthorizer{Grant: g}, intent
}
