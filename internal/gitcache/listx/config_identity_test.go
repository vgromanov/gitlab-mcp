package listx

import (
	"bytes"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshconfig"
	"golang.org/x/crypto/ssh"
)

func identityFixture(t *testing.T, encrypted bool) (string, ssh.Signer) {
	t.Helper()
	signer, priv := mustKey(t)
	block, err := ssh.MarshalPrivateKey(priv, "synthetic")
	if encrypted {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "synthetic", []byte("test-only"))
	}
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".pub", ssh.MarshalAuthorizedKey(signer.PublicKey()), 0600); err != nil {
		t.Fatal(err)
	}
	return path, signer
}
func TestConfiguredIdentitiesOnlyExcludesUnrelatedAgent(t *testing.T) {
	path, wanted := identityFixture(t, true) // Must select agent, never decrypt file.
	unrelated, _ := mustKey(t)
	c := sshconfig.Config{IdentityFiles: []string{path}, IdentityAgent: "synthetic", IdentitiesOnly: true, PublicKeyAuthentication: true}
	signers, err := configSigners(c, []ssh.Signer{unrelated, wanted})
	if err != nil || len(signers) != 1 || signers[0] != wanted {
		t.Fatalf("agent selection failed: %v", err)
	}
	if _, ok := signers[0].(ssh.AlgorithmSigner); !ok {
		t.Fatal("optional signer interface lost")
	}
	c.IdentitiesOnly = false
	signers, err = configSigners(c, []ssh.Signer{unrelated, wanted})
	if err != nil || len(signers) != 2 || signers[0] != wanted || signers[1] != unrelated {
		t.Fatal("configured match should precede unconfigured agent")
	}
}
func TestConfiguredFileAbsentFromAgentUsesFile(t *testing.T) {
	path, wanted := identityFixture(t, false)
	unrelated, _ := mustKey(t)
	c := sshconfig.Config{IdentityFiles: []string{filepath.Join(t.TempDir(), "missing"), path}, IdentityAgent: "synthetic", IdentitiesOnly: true, PublicKeyAuthentication: true}
	signers, err := configSigners(c, []ssh.Signer{unrelated})
	if err != nil || len(signers) != 1 || !bytes.Equal(signers[0].PublicKey().Marshal(), wanted.PublicKey().Marshal()) {
		t.Fatalf("file selection: %v", err)
	}
	c.IdentityAgent = ""
	c.IdentitiesOnly = false
	signers, err = configSigners(c, []ssh.Signer{unrelated})
	if err != nil || len(signers) != 1 {
		t.Fatal("disabled agent still used")
	}
}
func TestConfiguredPublicFileSelectsAgentWithoutPrivateKey(t *testing.T) {
	path, wanted := identityFixture(t, false)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	c := sshconfig.Config{IdentityFiles: []string{path + ".pub"}, IdentityAgent: "synthetic", IdentitiesOnly: true, PublicKeyAuthentication: true}
	got, err := configSigners(c, []ssh.Signer{wanted})
	if err != nil || len(got) != 1 || got[0] != wanted {
		t.Fatal("public-only identity did not match agent")
	}
}
func TestConfiguredSelectorErrorsDoNotFallback(t *testing.T) {
	path, _ := identityFixture(t, true)
	unrelated, _ := mustKey(t)
	c := sshconfig.Config{IdentityFiles: []string{path}, IdentityAgent: "synthetic", IdentitiesOnly: true, PublicKeyAuthentication: true}
	if _, err := configSigners(c, []ssh.Signer{unrelated}); !errors.Is(err, ErrIdentityEncrypted) {
		t.Fatalf("encrypted file got %v", err)
	}
	if err := os.WriteFile(path+"-cert.pub", []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := configSigners(c, nil); !errors.Is(err, ErrConfigCertificate) {
		t.Fatalf("certificate got %v", err)
	}
	c.PublicKeyAuthentication = false
	if _, err := configSigners(c, nil); !errors.Is(err, ErrConfigPublicKeyDisabled) {
		t.Fatal("disabled publickey accepted")
	}
	c.PublicKeyAuthentication = true
	c.IdentityFiles = nil
	if _, err := configSigners(c, []ssh.Signer{unrelated}); !errors.Is(err, ErrConfigNoIdentities) {
		t.Fatal("unconfigured agent escaped identities-only")
	}
}
