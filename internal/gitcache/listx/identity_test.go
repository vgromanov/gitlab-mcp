package listx

import (
	"context"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestSSHExplicitIdentityWithoutAgent(t *testing.T) {
	host, _ := mustKey(t)
	key, priv := mustKey(t)
	block, err := ssh.MarshalPrivateKey(priv, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(identity, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_AUTH_SOCK", filepath.Join(t.TempDir(), "unavailable.sock"))
	addr, stop := serveGitSSH(t, host, key.PublicKey(), true)
	defer stop()
	var diag Diagnostic
	refs, err := List(context.Background(), "ssh://git@"+addr+"/repo.git", Options{
		KnownHosts: writeKnown(t, host, addr), IdentityFile: identity,
		Timeout: 5 * time.Second, SSHDiagnostics: true, Diag: &diag,
	}.withLoopback())
	if err != nil || len(refs) != 1 || diag.Stage != "ok" || !diag.HostKeyVerified || diag.AgentHasSigners || diag.AuthMethod != "publickey(file)" {
		t.Fatalf("file authentication: err=%v refs=%d diagnostic=%s", err, len(refs), diag.JSON())
	}
}

func TestIdentityReadFailsClosed(t *testing.T) {
	_, priv := mustKey(t)
	encrypted, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "synthetic", []byte("synthetic-test-only"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		data []byte
		want error
	}{
		{"empty", nil, ErrIdentityFile},
		{"malformed", []byte("secret-marker-invalid-key"), ErrIdentityFile},
		{"public-only", ssh.MarshalAuthorizedKey(mustSigner(t, priv).PublicKey()), ErrIdentityFile},
		{"oversized", []byte(strings.Repeat("x", maxIdentityBytes+1)), ErrIdentityFile},
		{"encrypted", pem.EncodeToMemory(encrypted), ErrIdentityEncrypted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "identity")
			if err := os.WriteFile(path, tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			signer, err := readIdentity(path)
			if signer != nil || !errors.Is(err, tc.want) {
				t.Fatalf("got signer=%t error=%v", signer != nil, err)
			}
			if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "secret-marker") {
				t.Fatal("error leaked input")
			}
		})
	}
	for _, path := range []string{t.TempDir(), filepath.Join(t.TempDir(), "missing")} {
		if _, err := readIdentity(path); !errors.Is(err, ErrIdentityFile) {
			t.Fatalf("got %v", err)
		}
	}
}

func mustSigner(t *testing.T, key interface{}) ssh.Signer {
	t.Helper()
	s, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestIdentityInvalidOrConflictingStopsBeforeDial(t *testing.T) {
	host, _ := mustKey(t)
	known := writeKnown(t, host, "127.0.0.1:1")
	for _, tc := range []struct {
		socket string
		want   error
	}{{"", ErrIdentityFile}, {"unused", ErrIdentityConflict}} {
		var diag Diagnostic
		_, err := List(context.Background(), "ssh://git@127.0.0.1:1/repo.git", Options{
			KnownHosts: known, IdentityFile: filepath.Join(t.TempDir(), "missing"), AgentSocket: tc.socket,
			SSHDiagnostics: true, Diag: &diag,
		}.withLoopback())
		if !errors.Is(err, tc.want) || diag.TCPConnected || diag.PublicKeyAuthCallbackInvoked {
			t.Fatalf("got %v diagnostic=%s", err, diag.JSON())
		}
	}
}
