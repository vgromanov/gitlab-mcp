package knownhosts

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestLineHashHostnameAndErrors(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	line := Line([]string{"127.0.0.1:2222", "[::1]:22"}, signer.PublicKey())
	if !strings.Contains(line, "127.0.0.1") || !strings.Contains(line, "ssh-ed25519") {
		t.Fatalf("line %q", line)
	}
	hashed := HashHostname("example.com")
	if !strings.HasPrefix(hashed, "|1|") || strings.Contains(hashed, "example.com") {
		t.Fatalf("hashed %q", hashed)
	}
	kk := KnownKey{Key: signer.PublicKey(), Filename: "kh", Line: 3}
	if s := kk.String(); !strings.Contains(s, "kh:3:") || !strings.Contains(s, "ssh-ed25519") {
		t.Fatalf("KnownKey.String %q", s)
	}
	if (&KeyError{}).Error() != "knownhosts: key is unknown" {
		t.Fatal("unknown")
	}
	if (&KeyError{Want: []KnownKey{kk}}).Error() != "knownhosts: key mismatch" {
		t.Fatal("mismatch")
	}
	if (&RevokedError{Revoked: kk}).Error() != "knownhosts: key is revoked" {
		t.Fatal("revoked")
	}
	if Normalize("[127.0.0.1]:22") != "127.0.0.1" {
		t.Fatalf("normalize default port: %q", Normalize("[127.0.0.1]:22"))
	}
	if Normalize("127.0.0.1:2222") != "[127.0.0.1]:2222" {
		t.Fatalf("normalize nondefault: %q", Normalize("127.0.0.1:2222"))
	}
}
