package listx

import (
	"crypto/rand"
	"crypto/rsa"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshconfig"
	"golang.org/x/crypto/ssh"
	"os"
	"testing"
)

func TestReviewUnsafePrivateIdentity(t *testing.T) {
	p, _ := identityFixture(t, false)
	os.Remove(p + ".pub")
	os.Chmod(p, 0644)
	if _, e := readIdentity(p); e == nil {
		t.Error("unsafe private signing accepted")
	}
	if _, e := configuredPublic(p); e == nil {
		t.Error("unsafe private metadata accepted")
	}
}
func TestReviewRSAIdentityForbidsSHA1(t *testing.T) {
	k, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	s, e := ssh.NewSignerFromKey(k)
	if e != nil {
		t.Fatal(e)
	}
	got, e := configSigners(sshconfig.Config{IdentityAgent: "fixture", PublicKeyAuthentication: true}, []ssh.Signer{s})
	if e != nil {
		t.Fatal(e)
	}
	if _, e := got[0].(ssh.AlgorithmSigner).SignWithAlgorithm(rand.Reader, []byte("synthetic"), ssh.KeyAlgoRSA); e == nil {
		t.Fatal("SHA1 authentication signature allowed")
	}
}
func TestPublicSidecarAvoidsUnsafePrivateRead(t *testing.T) {
	p, s := identityFixture(t, false)
	os.Chmod(p, 0644)
	c := sshconfig.Config{IdentityFiles: []string{p}, IdentityAgent: "fixture", IdentitiesOnly: true, PublicKeyAuthentication: true}
	if _, e := configSigners(c, []ssh.Signer{s}); e != nil {
		t.Fatal(e)
	}
	os.Chmod(p+".pub", 0644)
	c.IdentityFiles = []string{p + ".pub"}
	if _, e := configSigners(c, []ssh.Signer{s}); e != nil {
		t.Fatal(e)
	}
}
func TestAgentCertificateRejected(t *testing.T) {
	s, _ := mustKey(t)
	ca, _ := mustKey(t)
	c := &ssh.Certificate{Key: s.PublicKey(), CertType: ssh.UserCert, ValidBefore: ssh.CertTimeInfinity}
	if e := c.SignCert(rand.Reader, ca); e != nil {
		t.Fatal(e)
	}
	cs, e := ssh.NewCertSigner(c, s)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := configSigners(sshconfig.Config{IdentityAgent: "fixture", PublicKeyAuthentication: true}, []ssh.Signer{cs}); e != ErrConfigCertificate {
		t.Fatal("agent certificate not explicitly rejected")
	}
}
