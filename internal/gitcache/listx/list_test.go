package listx

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/redact"
)

func TestLimitsClamp(t *testing.T) {
	d, n := (Options{Timeout: time.Hour, MaxBytes: 1 << 40}).limits()
	if d != bounds.Timeout || n != bounds.MaxRefBytes {
		t.Fatalf("clamp timeout=%s bytes=%d", d, n)
	}
}

func TestFailSSHCategories(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		err  error
		want error
		leak string
	}{
		{
			err:  errors.New("ssh: handshake failed: ssh host key verification failed key ssh-ed25519 AAAALEAK"),
			want: ErrHostKey,
			leak: "AAAA",
		},
		{
			err:  wrapHost(ErrHostKey),
			want: ErrHostKey,
		},
		{
			err:  errors.New("ssh: handshake failed: ssh: unable to authenticate, attempted methods [publickey], no supported methods remain secret ssh-ed25519 AAAALEAK"),
			want: ErrAuth,
			leak: "ssh-ed25519",
		},
		{
			err:  errors.New(`ssh: handshake failed: ssh: no common algorithm for host key; client offered: [ssh-ed25519], server offered: [ssh-rsa] AAAALEAK`),
			want: ErrHandshake,
			leak: "AAAA",
		},
		{
			err:  &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused to 10.1.1.1")},
			want: ErrNetwork,
			leak: "10.1.1.1",
		},
		{
			err:  errors.New("ssh: handshake failed: read tcp 10.9.9.9:7999: connection reset by peer"),
			want: ErrNetwork,
			leak: "10.9.9.9",
		},
	}
	for _, tc := range cases {
		got := failSSH(ctx, tc.err)
		if !errors.Is(got, tc.want) {
			t.Fatalf("got %v want %v", got, tc.want)
		}
		if tc.leak != "" && strings.Contains(got.Error(), tc.leak) {
			t.Fatalf("leaked %q in %s", tc.leak, got.Error())
		}
	}
}

func wrapHost(err error) error { return errors.Join(errors.New("ssh: handshake failed"), err) }

func TestStopClosesTrackedAndLateBind(t *testing.T) {
	ctx := context.Background()
	group := newCancelClosers(ctx)
	left, right := net.Pipe()
	t.Cleanup(func() { _ = right.Close() })
	group.bind(ctx, left)
	group.stop()
	group.stop()
	assertClosed(t, left)

	cancelled, cancel := context.WithCancel(context.Background())
	lateGroup := newCancelClosers(cancelled)
	t.Cleanup(lateGroup.stop)
	cancel()
	lateGroup.close()
	late, other := net.Pipe()
	t.Cleanup(func() { _ = other.Close() })
	lateGroup.bind(cancelled, late)
	assertClosed(t, late)
}

func assertClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(200 * time.Millisecond))
	var buf [1]byte
	if _, err := conn.Write(buf[:]); err == nil {
		t.Fatal("connection still open")
	}
}

func TestHTTPSAdvertisedRefs(t *testing.T) {
	const token = "local-test-token"
	caPEM, serverCert := testServerCert(t)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	const wantHash = "2d132b02f44f8995d58d2909a9e1d8cfe929c37e"
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "oauth2" || pass != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !strings.Contains(r.URL.Path, "/info/refs") || r.URL.Query().Get("service") != "git-upload-pack" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		if err := writeAdv(w, wantHash); err != nil {
			http.Error(w, "encode", http.StatusInternalServerError)
		}
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	raw := srv.URL + "/repo.git"
	refs, err := List(context.Background(), raw, Options{Token: token, CAPath: caPath, Timeout: 5 * time.Second}.withLoopback())
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Name != "refs/heads/main" || refs[0].Hash != wantHash {
		t.Fatalf("refs %+v", refs)
	}

	_, err = List(context.Background(), raw, Options{Token: token + "-nope", CAPath: caPath, Timeout: 5 * time.Second}.withLoopback())
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("auth error leaked or succeeded: %v", err)
	}
}

func TestResponseBoundAndBusy(t *testing.T) {
	caPEM, serverCert := testServerCert(t)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	body := advBytes(t, 40)
	capBytes := int64(len(body) / 2)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	raw := srv.URL + "/repo.git"
	_, err := List(context.Background(), raw, Options{Token: "local-test-token", CAPath: caPath, MaxBytes: capBytes, Timeout: 5 * time.Second}.withLoopback())
	if !errors.Is(err, ErrResponseTooLarge) || strings.Contains(err.Error(), "pkt-len") {
		t.Fatalf("bound: %v", err)
	}

	release := make(chan struct{})
	started := make(chan struct{})
	var once bool
	block := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !once {
			once = true
			close(started)
			<-release
		}
		_ = writeAdv(w, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	}))
	block.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12}
	block.StartTLS()
	var unblock sync.Once
	stopBlock := func() { unblock.Do(func() { close(release) }) }
	t.Cleanup(func() {
		stopBlock()
		block.Close()
	})
	errCh := make(chan error, 1)
	go func() {
		_, err := List(context.Background(), block.URL+"/repo.git", Options{Token: "local-test-token", CAPath: caPath, Timeout: 5 * time.Second}.withLoopback())
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first list did not reach the server")
	}
	_, err = List(context.Background(), block.URL+"/repo.git", Options{Token: "local-test-token", CAPath: caPath, Timeout: 5 * time.Second}.withLoopback())
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("busy: %v", err)
	}
	stopBlock()
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestAgentWithoutIdentities(t *testing.T) {
	sock, stop := serveAgent(t, nil)
	defer stop()
	hostKey, _ := mustKey(t)
	known := writeKnown(t, hostKey, "127.0.0.1:1")
	_, err := List(context.Background(), "ssh://git@127.0.0.1:1/repo.git", Options{
		KnownHosts:  known,
		AgentSocket: sock,
		Timeout:     2 * time.Second,
	}.withLoopback())
	if !errors.Is(err, ErrNoIdentities) {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "ssh-ed25519") || strings.Contains(err.Error(), "SHA256") {
		t.Fatalf("leaked identity: %s", err.Error())
	}
}

func TestSSHHostKeyMismatchNotAuth(t *testing.T) {
	host, _ := mustKey(t)
	other, _ := mustKey(t)
	clientKey, clientPriv := mustKey(t)
	addr, stop := serveGitSSH(t, host, clientKey.PublicKey(), true)
	defer stop()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	known := writeKnown(t, other, net.JoinHostPort("127.0.0.1", port))
	sock, astop := serveAgent(t, clientPriv)
	defer astop()
	_, err = List(context.Background(), "ssh://git@"+addr+"/repo.git", Options{
		KnownHosts:  known,
		AgentSocket: sock,
		Timeout:     5 * time.Second,
	}.withLoopback())
	if !errors.Is(err, ErrHostKey) || errors.Is(err, ErrAuth) {
		t.Fatalf("got %v", err)
	}
	blob := string(ssh.MarshalAuthorizedKey(host.PublicKey()))
	if strings.Contains(err.Error(), strings.TrimSpace(blob)) || strings.Contains(err.Error(), "ssh-ed25519") {
		t.Fatalf("leaked host key: %s", err.Error())
	}
}

func TestSSHAuthFailureNotHostKey(t *testing.T) {
	host, _ := mustKey(t)
	clientKey, clientPriv := mustKey(t)
	other, _ := mustKey(t)
	addr, stop := serveGitSSH(t, host, other.PublicKey(), true)
	defer stop()
	known := writeKnown(t, host, addr)
	sock, astop := serveAgent(t, clientPriv)
	defer astop()
	_, err := List(context.Background(), "ssh://git@"+addr+"/repo.git", Options{
		KnownHosts:  known,
		AgentSocket: sock,
		Timeout:     5 * time.Second,
	}.withLoopback())
	if !errors.Is(err, ErrAuth) || errors.Is(err, ErrHostKey) {
		t.Fatalf("got %v", err)
	}
	blob := string(ssh.MarshalAuthorizedKey(clientKey.PublicKey()))
	if strings.Contains(err.Error(), strings.TrimSpace(blob)) {
		t.Fatalf("leaked client key: %s", err.Error())
	}
}

func TestSSHListSuccess(t *testing.T) {
	host, _ := mustKey(t)
	clientKey, clientPriv := mustKey(t)
	addr, stop := serveGitSSH(t, host, clientKey.PublicKey(), true)
	defer stop()
	known := writeKnown(t, host, addr)
	sock, astop := serveAgent(t, clientPriv)
	defer astop()
	refs, err := List(context.Background(), "ssh://git@"+addr+"/repo.git", Options{
		KnownHosts:  known,
		AgentSocket: sock,
		Timeout:     5 * time.Second,
	}.withLoopback())
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Name != "refs/heads/main" {
		t.Fatalf("refs %+v", refs)
	}
}

func TestAgentCancelDoesNotWaitTimeout(t *testing.T) {
	ln, err := net.Listen("unix", filepath.Join(shortDir(t), "a.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		close(accepted)
		time.Sleep(30 * time.Second)
		_ = conn.Close()
	}()
	hostKey, _ := mustKey(t)
	known := writeKnown(t, hostKey, "127.0.0.1:1")
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := List(ctx, "ssh://git@127.0.0.1:1/repo.git", Options{
			KnownHosts:  known,
			AgentSocket: ln.Addr().String(),
			Timeout:     10 * time.Second,
		}.withLoopback())
		errCh <- err
	}()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("agent was not contacted")
	}
	cancel()
	select {
	case err := <-errCh:
		if time.Since(start) > 2*time.Second {
			t.Fatalf("cancel waited %s: %v", time.Since(start), err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled list kept running")
	}
}

func TestAgentDeadline(t *testing.T) {
	ln, err := net.Listen("unix", filepath.Join(shortDir(t), "a.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		time.Sleep(30 * time.Second)
		_ = conn.Close()
	}()
	hostKey, _ := mustKey(t)
	known := writeKnown(t, hostKey, "127.0.0.1:1")
	start := time.Now()
	_, err = List(context.Background(), "ssh://git@127.0.0.1:1/repo.git", Options{
		KnownHosts:  known,
		AgentSocket: ln.Addr().String(),
		Timeout:     300 * time.Millisecond,
	}.withLoopback())
	if time.Since(start) > 2*time.Second {
		t.Fatalf("agent read ignored deadline, waited %s", time.Since(start))
	}
	if err == nil {
		t.Fatal("expected timeout")
	}
}

func TestSSHListClosesAgent(t *testing.T) {
	host, _ := mustKey(t)
	_, clientPriv := mustKey(t)
	clientKey, err := ssh.NewSignerFromKey(clientPriv)
	if err != nil {
		t.Fatal(err)
	}
	addr, stop := serveGitSSH(t, host, clientKey.PublicKey(), true)
	defer stop()
	known := writeKnown(t, host, addr)
	path := filepath.Join(shortDir(t), "a.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	keyring := agent.NewKeyring()
	if err := keyring.Add(agent.AddedKey{PrivateKey: clientPriv}); err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_ = agent.ServeAgent(keyring, conn)
		close(served)
	}()
	refs, err := List(context.Background(), "ssh://git@"+addr+"/repo.git", Options{
		KnownHosts:  known,
		AgentSocket: path,
		Timeout:     5 * time.Second,
	}.withLoopback())
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs %+v", refs)
	}
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("agent connection stayed open after a successful list")
	}
}

func TestSSHNetworkAndHandshakeStages(t *testing.T) {
	_, clientPriv := mustKey(t)
	hostKey, _ := mustKey(t)
	path := filepath.Join(shortDir(t), "a.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	keyring := agent.NewKeyring()
	if err := keyring.Add(agent.AddedKey{PrivateKey: clientPriv}); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = agent.ServeAgent(keyring, conn) }()
		}
	}()

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := closed.Addr().String()
	_ = closed.Close()
	known := writeKnown(t, hostKey, refused)
	_, err = List(context.Background(), "ssh://git@"+refused+"/repo.git", Options{
		KnownHosts:  known,
		AgentSocket: path,
		Timeout:     2 * time.Second,
	}.withLoopback())
	if !errors.Is(err, ErrNetwork) || strings.Contains(err.Error(), refused) {
		t.Fatalf("network: %v", err)
	}

	plain, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	go func() {
		conn, err := plain.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		go func() { _, _ = io.Copy(io.Discard, conn) }()
		_, _ = conn.Write([]byte("SSH-2.0-fixture\r\n\xff\xff\xff\xff\xff"))
		time.Sleep(2 * time.Second)
	}()
	peer := plain.Addr().String()
	known = writeKnown(t, hostKey, peer)
	_, err = List(context.Background(), "ssh://git@"+peer+"/repo.git", Options{
		KnownHosts:  known,
		AgentSocket: path,
		Timeout:     2 * time.Second,
	}.withLoopback())
	if !errors.Is(err, ErrHandshake) || errors.Is(err, ErrAuth) || errors.Is(err, ErrHostKey) {
		t.Fatalf("handshake: %v", err)
	}
	if strings.Contains(err.Error(), "ssh-ed25519") || strings.Contains(err.Error(), peer) {
		t.Fatalf("handshake leaked detail: %s", err.Error())
	}
}

func TestSSHDiagnosticsStages(t *testing.T) {
	host, _ := mustKey(t)
	clientKey, clientPriv := mustKey(t)
	addr, stop := serveGitSSH(t, host, clientKey.PublicKey(), true, ssh.InsecureKeyExchangeDH1SHA1)
	defer stop()
	known := writeKnown(t, host, addr)
	sock, astop := serveAgent(t, clientPriv)
	defer astop()
	var algo Diagnostic
	_, err := List(context.Background(), "ssh://git@"+addr+"/repo.git", Options{
		KnownHosts: known, AgentSocket: sock, Timeout: 5 * time.Second,
		SSHDiagnostics: true, Diag: &algo,
	}.withLoopback())
	if !errors.Is(err, ErrHandshake) || strings.Contains(err.Error(), "diffie-hellman") {
		t.Fatalf("algorithm fixed error: %v", err)
	}
	if !algo.AgentHasSigners || !algo.TCPConnected || algo.HostKeyCallbackInvoked || algo.HostKeyVerified || algo.PublicKeyAuthCallbackInvoked {
		t.Fatalf("algorithm flags %+v", algo)
	}
	js := algo.JSON()
	if algo.Stage != "handshake" || !strings.Contains(js, `"what":"key exchange"`) || !strings.Contains(js, `"requested"`) || !strings.Contains(js, "diffie-hellman-group1-sha1") || !strings.Contains(js, "curve25519-sha256") || len(js) > redact.MaxTotal {
		t.Fatalf("algorithm diag %s", js)
	}

	other, _ := mustKey(t)
	addr, stop = serveGitSSH(t, host, clientKey.PublicKey(), true)
	defer stop()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	known = writeKnown(t, other, net.JoinHostPort("127.0.0.1", port))
	var hostDiag Diagnostic
	_, err = List(context.Background(), "ssh://git@"+addr+"/repo.git", Options{
		KnownHosts: known, AgentSocket: sock, Timeout: 5 * time.Second,
		SSHDiagnostics: true, Diag: &hostDiag,
	}.withLoopback())
	blob := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(host.PublicKey())))
	if !errors.Is(err, ErrHostKey) || errors.Is(err, ErrAuth) || strings.Contains(err.Error(), blob) {
		t.Fatalf("host fixed error: %v", err)
	}
	if !hostDiag.HostKeyCallbackInvoked || hostDiag.HostKeyVerified || hostDiag.PublicKeyAuthCallbackInvoked || hostDiag.HostTrust != "mismatch" || hostDiag.Stage != "hostkey" {
		t.Fatalf("host flags %+v", hostDiag)
	}
	if strings.Contains(hostDiag.JSON(), blob) || strings.Contains(hostDiag.JSON(), "AAAA") {
		t.Fatalf("host diag leaked %s", hostDiag.JSON())
	}

	addr, stop = serveGitSSH(t, host, other.PublicKey(), true)
	defer stop()
	known = writeKnown(t, host, addr)
	var authDiag Diagnostic
	_, err = List(context.Background(), "ssh://git@"+addr+"/repo.git", Options{
		KnownHosts: known, AgentSocket: sock, Timeout: 5 * time.Second,
		SSHDiagnostics: true, Diag: &authDiag,
	}.withLoopback())
	clientBlob := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(clientKey.PublicKey())))
	if !errors.Is(err, ErrAuth) || errors.Is(err, ErrHostKey) || strings.Contains(err.Error(), clientBlob) {
		t.Fatalf("auth fixed error: %v", err)
	}
	if !authDiag.HostKeyVerified || !authDiag.PublicKeyAuthCallbackInvoked || authDiag.AuthMethod != "publickey(agent)" || authDiag.Stage != "authentication" {
		t.Fatalf("auth flags %+v", authDiag)
	}
	if strings.Contains(authDiag.JSON(), clientBlob) {
		t.Fatalf("auth diag leaked %s", authDiag.JSON())
	}

	addr, stop = serveGitSSH(t, host, clientKey.PublicKey(), true)
	defer stop()
	known = writeKnown(t, host, addr)
	var okDiag Diagnostic
	refs, err := List(context.Background(), "ssh://git@"+addr+"/repo.git", Options{
		KnownHosts: known, AgentSocket: sock, Timeout: 5 * time.Second,
		SSHDiagnostics: true, Diag: &okDiag,
	}.withLoopback())
	if err != nil || len(refs) != 1 || okDiag.Stage != "ok" || !okDiag.HostKeyVerified || !okDiag.PublicKeyAuthCallbackInvoked {
		t.Fatalf("success %+v refs %v err %v", okDiag, refs, err)
	}
}

func TestDiagnosticJSONKeepsPeerAlgorithms(t *testing.T) {
	var supported []string
	for i := 0; i < 40; i++ {
		supported = append(supported, ssh.KeyExchangeDHGEXSHA256)
	}
	raw := fmt.Errorf("ssh: handshake failed: %w", &ssh.AlgorithmNegotiationError{
		What:                "key exchange",
		SupportedAlgorithms: supported,
		RequestedAlgorithms: []string{ssh.InsecureKeyExchangeDH1SHA1, "opaque-credential-aaaaaaaaaaaaaaaa"},
	})
	fixed := failSSH(context.Background(), raw)
	if !errors.Is(fixed, ErrHandshake) || strings.Contains(fixed.Error(), "diffie-hellman") || strings.Contains(fixed.Error(), "opaque-credential") {
		t.Fatalf("fixed error changed: %v", fixed)
	}
	var diag Diagnostic
	diag.finish(fixed, raw)
	js := diag.JSON()
	if len(js) > redact.MaxTotal || !strings.Contains(js, `"what":"key exchange"`) || !strings.Contains(js, ssh.InsecureKeyExchangeDH1SHA1) || strings.Contains(js, "opaque-credential") {
		t.Fatalf("json len %d body %s", len(js), js)
	}
	if _, ok := any(&diag).(interface{ Unwrap() error }); ok {
		t.Fatal("diagnostic exports unwrap")
	}
}

func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func serveAgent(t *testing.T, priv ed25519.PrivateKey) (string, func()) {
	t.Helper()
	path := filepath.Join(shortDir(t), "a.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	keyring := agent.NewKeyring()
	if priv != nil {
		if err := keyring.Add(agent.AddedKey{PrivateKey: priv}); err != nil {
			t.Fatal(err)
		}
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = agent.ServeAgent(keyring, conn) }()
		}
	}()
	return path, func() { _ = ln.Close() }
}

func serveGitSSH(t *testing.T, host ssh.Signer, allow ssh.PublicKey, sendAdv bool, kex ...string) (string, func()) {
	t.Helper()
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if allow != nil && key.Type() == allow.Type() && string(key.Marshal()) == string(allow.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("rejected")
		},
	}
	if len(kex) > 0 {
		cfg.KeyExchanges = kex
	}
	cfg.AddHostKey(host)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		server, chans, reqs, err := ssh.NewServerConn(conn, cfg)
		if err != nil {
			_ = conn.Close()
			return
		}
		go ssh.DiscardRequests(reqs)
		for ch := range chans {
			if ch.ChannelType() != "session" {
				_ = ch.Reject(ssh.UnknownChannelType, "unsupported")
				continue
			}
			channel, requests, err := ch.Accept()
			if err != nil {
				continue
			}
			go func() {
				defer channel.Close()
				for req := range requests {
					ok := req.Type == "exec" && sendAdv
					if req.WantReply {
						_ = req.Reply(ok, nil)
					}
					if ok {
						_ = writeAdv(channel, "2d132b02f44f8995d58d2909a9e1d8cfe929c37e")
						_ = channel.CloseWrite()
					}
				}
			}()
		}
		_ = server.Close()
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func writeKnown(t *testing.T, host ssh.Signer, addr string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	line := knownhosts.Line([]string{addr}, host.PublicKey())
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustKey(t *testing.T) (ssh.Signer, ed25519.PrivateKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s, priv
}

func advBytes(t *testing.T, refs int) []byte {
	t.Helper()
	ar := packp.NewAdvRefs()
	h := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	ar.Head = &h
	ar.References["HEAD"] = h
	ar.References["refs/heads/main"] = h
	for i := 0; i < refs; i++ {
		ar.References[fmt.Sprintf("refs/heads/n%04d", i)] = h
	}
	if err := ar.Capabilities.Add(capability.OFSDelta); err != nil {
		t.Fatal(err)
	}
	ar.Prefix = [][]byte{[]byte("# service=git-upload-pack"), {}}
	var buf bytes.Buffer
	if err := ar.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() < 128 {
		t.Fatalf("advertisement too small: %d", buf.Len())
	}
	return buf.Bytes()
}

func writeAdv(w io.Writer, hexHash string) error {
	ar := packp.NewAdvRefs()
	h := plumbing.NewHash(hexHash)
	ar.Head = &h
	ar.References["refs/heads/main"] = h
	ar.References["HEAD"] = h
	if err := ar.Capabilities.Add(capability.OFSDelta); err != nil {
		return err
	}
	ar.Prefix = [][]byte{[]byte("# service=git-upload-pack"), {}}
	return ar.Encode(w)
}

func testServerCert(t *testing.T) ([]byte, tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "local-ca"},
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	return pemBytes, cert
}
