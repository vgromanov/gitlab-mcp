//go:build linux || darwin

package listx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshconfig"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestPrepareTrustProvenanceChangesWithKnownHosts(t *testing.T) {
	host := mustPrepareKey(t)
	id := writePrepareIdentity(t)
	home := t.TempDir()
	known := filepath.Join(home, "known_hosts")
	line := knownhosts.Line([]string{"127.0.0.1:22"}, host.PublicKey())
	if err := os.WriteFile(known, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(home, "config")
	body := fmt.Sprintf("Host 127.0.0.1\n User git\n Port 22\n IdentitiesOnly yes\n IdentityAgent none\n IdentityFile %s\n UserKnownHostsFile %s\n GlobalKnownHostsFile none\n StrictHostKeyChecking yes\n UpdateHostKeys no\n", id, known)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	in := sshconfig.Input{Home: home, LocalUser: "local", UserConfig: cfg, SystemConfig: filepath.Join(home, "missing")}
	opt := Options{SSHConfig: &in}.WithLoopback()
	_, fp1, err := PrepareTrust(context.Background(), "ssh://git@127.0.0.1:22/repo.git", opt)
	if err != nil {
		t.Fatal(err)
	}
	other := mustPrepareKey(t)
	if err := os.WriteFile(known, []byte(knownhosts.Line([]string{"127.0.0.1:22"}, other.PublicKey())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, fp2, err := PrepareTrust(context.Background(), "ssh://git@127.0.0.1:22/repo.git", opt.ClearPrepared())
	if err != nil {
		t.Fatal(err)
	}
	if fp1 == fp2 {
		t.Fatal("known_hosts change did not alter PrepareTrust provenance")
	}
}

func TestPrepareTrustRejectsPubkeyDisabledWithoutDial(t *testing.T) {
	host := mustPrepareKey(t)
	id := writePrepareIdentity(t)
	home := t.TempDir()
	known := filepath.Join(home, "known_hosts")
	_ = os.WriteFile(known, []byte(knownhosts.Line([]string{"127.0.0.1:22"}, host.PublicKey())+"\n"), 0o600)
	cfg := filepath.Join(home, "config")
	body := fmt.Sprintf("Host 127.0.0.1\n User git\n Port 22\n PubkeyAuthentication no\n IdentitiesOnly yes\n IdentityAgent none\n IdentityFile %s\n UserKnownHostsFile %s\n GlobalKnownHostsFile none\n StrictHostKeyChecking yes\n UpdateHostKeys no\n", id, known)
	_ = os.WriteFile(cfg, []byte(body), 0o600)
	in := sshconfig.Input{Home: home, LocalUser: "local", UserConfig: cfg, SystemConfig: filepath.Join(home, "missing")}
	_, _, err := PrepareTrust(context.Background(), "ssh://git@127.0.0.1:22/repo.git", Options{SSHConfig: &in}.WithLoopback())
	if !errors.Is(err, ErrConfigPublicKeyDisabled) {
		t.Fatalf("pubkey disabled: %v", err)
	}
}

func TestPrepareTrustRejectsResolvedHostMismatch(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, "config")
	_ = os.WriteFile(cfg, []byte("Host 127.0.0.1\n HostName other.invalid\n Port 22\n User git\n"), 0o600)
	in := sshconfig.Input{Home: home, LocalUser: "local", UserConfig: cfg, SystemConfig: filepath.Join(home, "missing")}
	_, _, err := PrepareTrust(context.Background(), "ssh://git@127.0.0.1:22/repo.git", Options{SSHConfig: &in}.WithLoopback())
	if err == nil {
		t.Fatal("host mismatch accepted")
	}
}

func mustPrepareKey(t *testing.T) ssh.Signer {
	t.Helper()
	s, _ := mustKey(t)
	return s
}

func writePrepareIdentity(t *testing.T) string {
	t.Helper()
	path, _ := identityFixture(t, false)
	return path
}
