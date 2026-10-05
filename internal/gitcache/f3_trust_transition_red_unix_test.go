//go:build linux || darwin

package gitcache

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
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

// F3: owned approved trust transitions must flow into publication/return.

func TestF3_ApprovedInitialEnrollmentPublishesAndWarms(t *testing.T) {
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
	known := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(known, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			raw, e := ln.Accept()
			if e != nil {
				return
			}
			go serveF3Upload(raw, hostKey, clientKey, packBytes, head, base, nil)
		}
	}()
	defer func() { _ = ln.Close(); <-done }()

	home := t.TempDir()
	body := fmt.Sprintf("Match host 127.0.0.1\n User ssh-acquire\n Port %s\n IdentitiesOnly yes\n IdentityAgent none\n IdentityFile %s\n UserKnownHostsFile %s\n GlobalKnownHostsFile none\n StrictHostKeyChecking accept-new\n UpdateHostKeys no\n", port, idPath, known)
	if err := os.WriteFile(filepath.Join(home, "config"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	svc, auth, intent := openF3Acquire(t, home, addr, head, base)
	cold, err := svc.Acquire(context.Background(), intent, auth)
	if err != nil {
		t.Fatalf("approved initial enrollment failed publication (F3): %v", err)
	}
	if cold.Warm || cold.Objects == 0 {
		t.Fatalf("cold %#v", cold)
	}
	after, _ := os.ReadFile(known)
	if len(bytes.TrimSpace(after)) == 0 {
		t.Fatal("enrollment did not persist known_hosts")
	}
	warm, err := svc.Acquire(context.Background(), intent, auth)
	if err != nil || !warm.Warm || warm.GenerationID != cold.GenerationID {
		t.Fatalf("bound warm after enrollment (F3): %#v err=%v", warm, err)
	}
}

func TestF3_HostKeyRotationUpdatePublishes(t *testing.T) {
	ip, head, base := commitIndexedPack(t)
	packBytes := append([]byte(nil), ip.PackBytes...)
	hostKey := mustEd25519(t)
	rotated := mustEd25519(t)
	idPath, clientKey := writeSSHIdentity(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	known := writeSSHKnownHosts(t, hostKey, addr)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			raw, e := ln.Accept()
			if e != nil {
				return
			}
			go serveF3Upload(raw, hostKey, clientKey, packBytes, head, base, rotated)
		}
	}()
	defer func() { _ = ln.Close(); <-done }()

	home := t.TempDir()
	body := fmt.Sprintf("Match host 127.0.0.1\n User ssh-acquire\n Port %s\n IdentitiesOnly yes\n IdentityAgent none\n IdentityFile %s\n UserKnownHostsFile %s\n GlobalKnownHostsFile none\n StrictHostKeyChecking yes\n UpdateHostKeys yes\n", port, idPath, known)
	if err := os.WriteFile(filepath.Join(home, "config"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	svc, auth, intent := openF3Acquire(t, home, addr, head, base)
	cold, err := svc.Acquire(context.Background(), intent, auth)
	if err != nil {
		t.Fatalf("approved host-key rotation failed publication (F3): %v", err)
	}
	if cold.Warm {
		t.Fatal("expected cold")
	}
	warm, err := svc.Acquire(context.Background(), intent, auth)
	if err != nil || !warm.Warm {
		t.Fatalf("warm after rotation (F3): %#v err=%v", warm, err)
	}
}

func TestF3_NoOpHostKeyUpdateSucceeds(t *testing.T) {
	home, port, idPath, known, addr, head, base, cleanup := startF2SSHFixture(t)
	defer cleanup()
	writeF2Config(t, home, port, idPath, known, "PubkeyAuthentication yes\nIdentitiesOnly yes\nIdentityAgent none\n")
	// Strict yes + UpdateHostKeys yes with already-current key is a no-op success.
	body := fmt.Sprintf("Match host 127.0.0.1\n User ssh-acquire\n Port %s\n IdentitiesOnly yes\n IdentityAgent none\n IdentityFile %s\n UserKnownHostsFile %s\n GlobalKnownHostsFile none\n StrictHostKeyChecking yes\n UpdateHostKeys yes\n", port, idPath, known)
	if err := os.WriteFile(filepath.Join(home, "config"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	svc, auth, intent := openF3Acquire(t, home, addr, head, base)
	if _, err := svc.Acquire(context.Background(), intent, auth); err != nil {
		t.Fatalf("no-op update (F3): %v", err)
	}
}

func TestF3_PreparedFingerprintMismatchRejected(t *testing.T) {
	home, port, idPath, known, addr, head, base, cleanup := startF2SSHFixture(t)
	defer cleanup()
	writeF2Config(t, home, port, idPath, known, "PubkeyAuthentication yes\nIdentitiesOnly yes\nIdentityAgent none\n")
	svc, auth, intent := openF3Acquire(t, home, addr, head, base)
	if _, err := svc.Acquire(context.Background(), intent, auth); err != nil {
		t.Fatalf("cold: %v", err)
	}
	// Simulate unrelated external rewrite of known_hosts between prepare checkpoints
	// by mutating the file then forcing a warm re-prepare with a poisoned trustFP.
	other := mustEd25519(t)
	if err := os.WriteFile(known, []byte(fmt.Sprintf("%s %s\n", addr, string(ssh.MarshalAuthorizedKey(other.PublicKey())))), 0o600); err != nil {
		t.Fatal(err)
	}
	intent.trustFP = "not-the-current-trust"
	intent.sourceSSH, intent.targetSSH = listx.Options{}, listx.Options{}
	_, err := svc.Acquire(context.Background(), intent, auth)
	if !errors.Is(err, ErrAuthz) {
		t.Fatalf("external trust change accepted (F3): %v", err)
	}
}

func TestF3_TwoRoleEndpointsShareKnownHostsEnrollment(t *testing.T) {
	ip, head, base := commitIndexedPack(t)
	hostKey := mustEd25519(t)
	idPath, clientKey := writeSSHIdentity(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	known := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(known, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	packBytes := append([]byte(nil), ip.PackBytes...)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			raw, e := ln.Accept()
			if e != nil {
				return
			}
			go serveF3Upload(raw, hostKey, clientKey, packBytes, head, base, nil)
		}
	}()
	defer func() { _ = ln.Close(); <-done }()

	home := t.TempDir()
	body := fmt.Sprintf("Match host 127.0.0.1\n User ssh-acquire\n Port %s\n IdentitiesOnly yes\n IdentityAgent none\n IdentityFile %s\n UserKnownHostsFile %s\n GlobalKnownHostsFile none\n StrictHostKeyChecking accept-new\n UpdateHostKeys no\n", port, idPath, known)
	if err := os.WriteFile(filepath.Join(home, "config"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	root := regressionRoot(t)
	svc, err := OpenService(ServiceConfig{Enabled: true, Root: root, QuotaBytes: bounds.BrootBytes + bounds.GenerationCharge()*4})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close(context.Background())
	var domain AuthDomain
	actor, token := "f3-2role", "f3-tok"
	g := completeTestGrant(Grant{
		OriginHost: "127.0.0.1", ProjectID: "1", ProjectPath: "g/p", SourceFork: "2",
		TargetProjectID: "9", SourcePath: "source", TargetPath: "target",
		AuthDomain: domain.Bind(actor, token), PolicyFP: "fp", MRIID: 7,
		MRVersion: MRVersionFromDiffRefs(head, base, base),
		HeadSHA:   head, BaseSHA: base, StartSHA: base, ActorID: actor,
		SourceSSHURL: "ssh://git@" + addr + "/source.git",
		TargetSSHURL: "ssh://git@" + addr + "/target.git",
		SSHURL:       "ssh://git@" + addr + "/source.git",
	})
	in := sshconfig.Input{Home: home, LocalUser: "local", UserConfig: filepath.Join(home, "config"), SystemConfig: filepath.Join(home, "missing-system")}
	intent := AcquireIntent{
		ProjectID: "1", MRIID: 7, Depth: 2, Token: token, Transport: "ssh",
		ExpectedHead: head, ExpectedBase: base, ExpectedStart: base,
		ExpectedMRVersion: g.MRVersion, AllowLoopback: true,
		SSH: listx.Options{SSHConfig: &in, Timeout: 8 * time.Second},
	}
	_, err = svc.Acquire(context.Background(), intent, StaticAuthorizer{Grant: g})
	if err != nil {
		t.Fatalf("two-role shared known_hosts enrollment (F3): %v", err)
	}
}

func serveF3Upload(raw net.Conn, hostKey, clientKey ssh.Signer, packBytes []byte, head, base plumbing.Hash, rotate ssh.Signer) {
	serveF3UploadWithHook(raw, hostKey, clientKey, packBytes, head, base, rotate, nil)
}

func serveF3UploadWithHook(raw net.Conn, hostKey, clientKey ssh.Signer, packBytes []byte, head, base plumbing.Hash, rotate ssh.Signer, afterAuth func()) {
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(20 * time.Second))
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if meta.User() == "ssh-acquire" && bytes.Equal(key.Marshal(), clientKey.PublicKey().Marshal()) {
			if afterAuth != nil {
				afterAuth()
			}
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
	go func() {
		for r := range reqs {
			switch r.Type {
			case "hostkeys-prove-00@openssh.com":
				if rotate == nil || !r.WantReply {
					_ = r.Reply(false, nil)
					continue
				}
				blobs, err := f3StringsPacket(r.Payload)
				if err != nil {
					_ = r.Reply(false, nil)
					continue
				}
				var sigs [][]byte
				for _, b := range blobs {
					data := ssh.Marshal(struct {
						Request      string
						Session, Key []byte
					}{"hostkeys-prove-00@openssh.com", server.SessionID(), b})
					sig, err := rotate.Sign(rand.Reader, data)
					if err != nil {
						_ = r.Reply(false, nil)
						continue
					}
					sigs = append(sigs, ssh.Marshal(sig))
				}
				_ = r.Reply(true, f3Packet(sigs))
			case "keepalive@openssh.com":
				if r.WantReply {
					_ = r.Reply(true, nil)
				}
			default:
				if r.WantReply {
					_ = r.Reply(false, nil)
				}
			}
		}
	}()
	if rotate != nil {
		// Valid OpenSSH hostkeys-00 announcement includes the current key plus additions.
		announced := [][]byte{hostKey.PublicKey().Marshal()}
		if !bytes.Equal(rotate.PublicKey().Marshal(), hostKey.PublicKey().Marshal()) {
			announced = append(announced, rotate.PublicKey().Marshal())
		}
		_, _, _ = server.SendRequest("hostkeys-00@openssh.com", false, f3Packet(announced))
	}
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
				_ = writeShallowAdv(channel, head, base)
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
}

func f3Packet(items [][]byte) []byte {
	var out []byte
	for _, item := range items {
		out = binary.BigEndian.AppendUint32(out, uint32(len(item)))
		out = append(out, item...)
	}
	return out
}

func f3StringsPacket(payload []byte) ([][]byte, error) {
	var out [][]byte
	for len(payload) > 0 {
		if len(payload) < 4 {
			return nil, errors.New("short")
		}
		n := int(binary.BigEndian.Uint32(payload))
		payload = payload[4:]
		if n <= 0 || n > len(payload) {
			return nil, errors.New("bad len")
		}
		out = append(out, payload[:n])
		payload = payload[n:]
	}
	if len(out) == 0 {
		return nil, errors.New("empty")
	}
	return out, nil
}

func openF3Acquire(t *testing.T, home, addr string, head, base plumbing.Hash) (*Service, StaticAuthorizer, AcquireIntent) {
	t.Helper()
	root := regressionRoot(t)
	svc, err := OpenService(ServiceConfig{Enabled: true, Root: root, QuotaBytes: bounds.BrootBytes + bounds.GenerationCharge()*4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close(context.Background()) })
	var domain AuthDomain
	actor, token := "f3-actor", "f3-tok"
	g := completeTestGrant(Grant{
		OriginHost: "127.0.0.1", ProjectID: "1", ProjectPath: "g/p", SourceFork: "2",
		AuthDomain: domain.Bind(actor, token), PolicyFP: "fp", MRIID: 7,
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
