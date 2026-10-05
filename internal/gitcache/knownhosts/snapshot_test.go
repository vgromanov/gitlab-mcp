package knownhosts

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"golang.org/x/crypto/ssh"
	upstream "golang.org/x/crypto/ssh/knownhosts"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotParserMatchesPinnedParser(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, _ := ssh.NewSignerFromKey(priv)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	bad, _ := ssh.NewSignerFromKey(other)
	cert := &ssh.Certificate{Key: bad.PublicKey(), CertType: ssh.HostCert, ValidPrincipals: []string{"git.example"}, ValidBefore: ssh.CertTimeInfinity}
	cert.SignCert(rand.Reader, s)
	for _, record := range []string{upstream.Line([]string{"git.example:7999"}, s.PublicKey()), upstream.Line([]string{upstream.HashHostname("[git.example]:7999")}, s.PublicKey()), upstream.Line([]string{"*.example:7999", "!excluded.example:7999"}, s.PublicKey()), "@revoked " + upstream.Line([]string{"git.example:7999"}, s.PublicKey()), "@cert-authority " + upstream.Line([]string{"git.example:7999"}, s.PublicKey())} {
		data := []byte("# fixture\n" + record + "\n")
		p := filepath.Join(t.TempDir(), "hosts")
		os.WriteFile(p, data, 0600)
		reference, e := upstream.New(p)
		if e != nil {
			t.Fatal(e)
		}
		snapshot, e := New(context.Background(), data)
		if e != nil {
			t.Fatal(e)
		}
		for _, host := range []string{"git.example:7999", "excluded.example:7999", "other.example:7999", "none.invalid:7999"} {
			for _, k := range []ssh.PublicKey{s.PublicKey(), bad.PublicKey(), cert} {
				a := reference(host, &net.TCPAddr{}, k)
				b := snapshot(host, &net.TCPAddr{}, k)
				if classification(a) != classification(b) {
					t.Fatal("snapshot parser changed trust semantics")
				}
			}
		}
	}
}
func classification(e error) string {
	if e == nil {
		return "trusted"
	}
	var a *upstream.KeyError
	var b *KeyError
	if errors.As(e, &a) {
		if len(a.Want) == 0 {
			return "missing"
		}
		return "changed"
	}
	if errors.As(e, &b) {
		if len(b.Want) == 0 {
			return "missing"
		}
		return "changed"
	}
	var c *upstream.RevokedError
	var d *RevokedError
	if errors.As(e, &c) || errors.As(e, &d) {
		return "revoked"
	}
	return "certificate-error"
}
