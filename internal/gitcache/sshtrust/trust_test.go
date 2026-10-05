//go:build linux || darwin

package sshtrust

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshconfig"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func key(t *testing.T) ssh.Signer {
	t.Helper()
	_, k, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	s, e := ssh.NewSignerFromKey(k)
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func file(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "known_hosts")
	if e := os.WriteFile(p, data, 0600); e != nil {
		t.Fatal(e)
	}
	return p
}
func manager(t *testing.T, addr, path string) *Manager {
	t.Helper()
	m, e := New(context.Background(), sshconfig.Config{StrictHostKeyChecking: "yes", UpdateHostKeys: "yes", UserKnownHostsFiles: []string{path}}, addr)
	if e != nil {
		t.Fatal(e)
	}
	return m
}

type fakeProof struct {
	session       []byte
	signers       map[string]ssh.Signer
	bad           bool
	calls         int
	before        func()
	hostAlgo      string
	signAlgorithm string
}

func (f *fakeProof) SessionID() []byte { return f.session }
func (f *fakeProof) Algorithms() ssh.NegotiatedAlgorithms {
	return ssh.NegotiatedAlgorithms{HostKey: f.hostAlgo}
}
func (f *fakeProof) SendRequest(name string, reply bool, payload []byte) (bool, []byte, error) {
	f.calls++
	if f.before != nil {
		f.before()
	}
	if name != proofRequest || !reply {
		return false, nil, ErrUpdate
	}
	blobs, e := stringsPacket(payload)
	if e != nil {
		return false, nil, e
	}
	var result [][]byte
	for _, b := range blobs {
		s, ok := f.signers[string(b)]
		if !ok {
			return false, nil, ErrUpdate
		}
		session := f.session
		if f.bad {
			session = []byte("wrong session")
		}
		var sig *ssh.Signature
		var e error
		if f.signAlgorithm != "" {
			sig, e = s.(ssh.AlgorithmSigner).SignWithAlgorithm(rand.Reader, proofData(session, b), f.signAlgorithm)
		} else {
			sig, e = s.Sign(rand.Reader, proofData(session, b))
		}
		if e != nil {
			return false, nil, e
		}
		result = append(result, ssh.Marshal(sig))
	}
	return true, packet(result), nil
}
func TestProofBeforeAtomicRotationPreservesOtherHosts(t *testing.T) {
	addr := "git.example:7999"
	old, newKey, other := key(t), key(t), key(t)
	start := []byte("# preserved\n" + knownhosts.Line([]string{addr, "other.example:7999"}, old.PublicKey()) + " shared comment\n" + knownhosts.Line([]string{"unrelated.example:22"}, other.PublicKey()) + "\n")
	p := file(t, start)
	m := manager(t, addr, p)
	f := &fakeProof{session: []byte("session"), signers: map[string]ssh.Signer{string(newKey.PublicKey().Marshal()): newKey}}
	if e := m.apply(context.Background(), f, packet([][]byte{newKey.PublicKey().Marshal()})); e != nil {
		t.Fatal(e)
	}
	got, _ := os.ReadFile(p)
	if f.calls != 1 || !recorded(got, addr, newKey.PublicKey()) || recorded(got, addr, old.PublicKey()) || !bytes.Contains(got, []byte("# preserved\n")) || !bytes.Contains(got, []byte("shared comment")) {
		t.Fatal("rotation did not preserve/change correct entries")
	}
	cb, e := knownhosts.New(p)
	if e != nil {
		t.Fatal(e)
	}
	if cb("other.example:7999", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 7999}, old.PublicKey()) != nil {
		t.Fatal("shared other-host trust lost")
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0600 {
		t.Fatal("file mode changed")
	}
}
func TestBadProofAndConcurrentChangeNeverCommit(t *testing.T) {
	for _, kind := range []string{"bad-proof", "concurrent-change", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			addr := "git.example:7999"
			old, next := key(t), key(t)
			start := []byte(knownhosts.Line([]string{addr}, old.PublicKey()) + "\n")
			p := file(t, start)
			m := manager(t, addr, p)
			f := &fakeProof{session: []byte("session"), signers: map[string]ssh.Signer{string(next.PublicKey().Marshal()): next}, bad: kind == "bad-proof"}
			want := start
			if kind == "concurrent-change" {
				want = append(append([]byte(nil), start...), []byte("# external change\n")...)
				f.before = func() { _ = os.WriteFile(p, want, 0600) }
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "canceled" {
				cancel()
			}
			if e := m.apply(ctx, f, packet([][]byte{old.PublicKey().Marshal(), next.PublicKey().Marshal()})); e == nil {
				t.Fatal("unsafe update accepted")
			}
			got, _ := os.ReadFile(p)
			if !bytes.Equal(got, want) {
				t.Fatal("file modified despite failure")
			}
		})
	}
}
func TestNoOpAndMalformedUpdates(t *testing.T) {
	addr := "git.example:7999"
	old := key(t)
	start := []byte(knownhosts.Line([]string{addr}, old.PublicKey()) + "\n")
	p := file(t, start)
	m := manager(t, addr, p)
	st, _ := os.Stat(p)
	f := &fakeProof{session: []byte("session")}
	if e := m.apply(context.Background(), f, packet([][]byte{old.PublicKey().Marshal()})); e != nil {
		t.Fatal(e)
	}
	now, _ := os.Stat(p)
	if !os.SameFile(st, now) || f.calls != 0 || m.UpdateStatus() != "already_current" {
		t.Fatal("no-op caused file replacement/proof")
	}
	for _, payload := range [][]byte{nil, {0, 0, 0, 5, 1}, packet([][]byte{old.PublicKey().Marshal(), old.PublicKey().Marshal()}), make([]byte, maxPacket+1)} {
		if e := m.apply(context.Background(), f, payload); e == nil {
			t.Fatal("malformed/duplicate/oversized accepted")
		}
	}
}
func TestHashedHostAndInitialNoModeEligibility(t *testing.T) {
	addr := "git.example:7999"
	old := key(t)
	start := []byte(knownhosts.Line([]string{knownhosts.HashHostname(knownhosts.Normalize(addr))}, old.PublicKey()) + "\n")
	if !recorded(start, addr, old.PublicKey()) {
		t.Fatal("hashed entry not matched")
	}
	p := file(t, nil)
	m, e := New(context.Background(), sshconfig.Config{StrictHostKeyChecking: "no", UpdateHostKeys: "yes", UserKnownHostsFiles: []string{p}}, addr)
	if e != nil {
		t.Fatal(e)
	}
	if e := m.Callback(addr, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 7999}, old.PublicKey()); e != nil {
		t.Fatal(e)
	}
	if m.Verified() || m.eligible {
		t.Fatal("new no-mode host promoted to existing trust")
	}
	beforeAdvertisement, _ := os.ReadFile(p)
	other := key(t)
	proof := &fakeProof{session: []byte("synthetic"), signers: map[string]ssh.Signer{string(other.PublicKey().Marshal()): other}}
	updates := &Updates{manager: m, conn: proof, ctx: context.Background()}
	updates.handle(&ssh.Request{Type: announcement, Payload: packet([][]byte{other.PublicKey().Marshal()})})
	afterAdvertisement, _ := os.ReadFile(p)
	if proof.calls != 0 || !bytes.Equal(beforeAdvertisement, afterAdvertisement) || m.UpdateStatus() != "ineligible_initial_trust" {
		t.Fatal("untrusted initial connection processed advertised key set")
	}

	got, _ := os.ReadFile(p)
	if !recorded(got, addr, old.PublicKey()) {
		t.Fatal("configured initial enrollment missing")
	}
}
func TestLocalAuthenticatedHostkeysProtocol(t *testing.T) {
	for _, bad := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "wrong-session"}[bad], func(t *testing.T) {
			host, next, clientKey := key(t), key(t), key(t)
			ln, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			defer ln.Close()
			addr := ln.Addr().String()
			start := []byte(knownhosts.Line([]string{addr}, host.PublicKey()) + "\n")
			p := file(t, start)
			serverDone := make(chan struct{})
			go func() {
				defer close(serverDone)
				raw, e := ln.Accept()
				if e != nil {
					return
				}
				defer raw.Close()
				_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
				cfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil }}
				cfg.AddHostKey(host)
				server, channels, requests, e := ssh.NewServerConn(raw, cfg)
				if e != nil {
					return
				}
				defer server.Close()
				go func() {
					for ch := range channels {
						_ = ch.Reject(ssh.Prohibited, "fixture")
					}
				}()
				_, _, _ = server.SendRequest(announcement, false, packet([][]byte{host.PublicKey().Marshal(), next.PublicKey().Marshal()}))
				for req := range requests {
					if req.Type == proofRequest {
						blobs, e := stringsPacket(req.Payload)
						if e != nil {
							_ = req.Reply(false, nil)
							continue
						}
						var sigs [][]byte
						for _, b := range blobs {
							session := server.SessionID()
							if bad {
								session = []byte("wrong")
							}
							sig, _ := next.Sign(rand.Reader, proofData(session, b))
							sigs = append(sigs, ssh.Marshal(sig))
						}
						_ = req.Reply(true, packet(sigs))
					} else {
						_ = req.Reply(false, nil)
					}
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			m, e := New(ctx, sshconfig.Config{StrictHostKeyChecking: "yes", UpdateHostKeys: "yes", UserKnownHostsFiles: []string{p}}, addr)
			if e != nil {
				t.Fatal(e)
			}
			raw, e := net.DialTimeout("tcp", addr, time.Second)
			if e != nil {
				t.Fatal(e)
			}
			_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
			conn, _, reqs, e := ssh.NewClientConn(raw, addr, &ssh.ClientConfig{User: "git", Auth: []ssh.AuthMethod{ssh.PublicKeys(clientKey)}, HostKeyCallback: m.Callback})
			if e != nil {
				t.Fatal(e)
			}
			updates := m.Start(ctx, conn, reqs)
			e = updates.Sync()
			_ = conn.Close()
			updates.Wait()
			<-serverDone
			if bad && !errors.Is(e, ErrUpdate) {
				t.Fatalf("wrong-session got %v", e)
			}
			if !bad && e != nil {
				t.Fatal(e)
			}
			got, _ := os.ReadFile(p)
			if bad && !bytes.Equal(got, start) {
				t.Fatal("bad proof changed file")
			}
			if !bad && !recorded(got, addr, next.PublicKey()) {
				t.Fatal("valid proof not recorded")
			}
		})
	}
}
func TestUnsupportedMutationLayouts(t *testing.T) {
	k := key(t)
	p := file(t, []byte(knownhosts.Line([]string{"*.example:7999"}, k.PublicKey())+"\n"))
	m := manager(t, "git.example:7999", p)
	if _, e := rewrite(m.files[0].data, m.addr, []ssh.PublicKey{k.PublicKey()}, false); e == nil {
		t.Fatal("wildcard mutation not rejected")
	}
	if !strings.Contains(ErrUpdateUnsupported.Error(), "unsupported") {
		t.Fatal("unsupported layout not actionable")
	}
}

func TestRSAProofAlgorithmRules(t *testing.T) {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		host, proof string
		wantError   bool
	}{
		{ssh.KeyAlgoED25519, ssh.KeyAlgoRSASHA512, false},
		{ssh.KeyAlgoED25519, ssh.KeyAlgoRSA, true},
		{ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512, true},
		{ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA256, false},
	} {
		old := key(t)
		addr := "git.example:7999"
		before := []byte(knownhosts.Line([]string{addr}, old.PublicKey()) + "\n")
		path := file(t, before)
		m := manager(t, addr, path)
		f := &fakeProof{session: []byte("session"), signers: map[string]ssh.Signer{string(signer.PublicKey().Marshal()): signer}, hostAlgo: tc.host, signAlgorithm: tc.proof}
		err := m.apply(context.Background(), f, packet([][]byte{old.PublicKey().Marshal(), signer.PublicKey().Marshal()}))
		if (err != nil) != tc.wantError {
			t.Fatal("RSA proof algorithm policy mismatch")
		}
		if tc.wantError {
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("invalid RSA proof changed trust")
			}
		}
	}
}
func TestGlobalOnlyTrustCannotAuthorizeUserFileRotation(t *testing.T) {
	addr := "git.example:7999"
	host := key(t)
	global := file(t, []byte(knownhosts.Line([]string{addr}, host.PublicKey())+"\n"))
	local := file(t, nil)
	m, err := New(context.Background(), sshconfig.Config{StrictHostKeyChecking: "no", UpdateHostKeys: "yes", UserKnownHostsFiles: []string{local}, GlobalKnownHostsFiles: []string{global}}, addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Callback(addr, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 7999}, host.PublicKey()); err != nil {
		t.Fatal(err)
	}
	if !m.Verified() || m.eligible {
		t.Fatal("global trust incorrectly made user-file updates eligible")
	}
}
