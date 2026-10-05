package listx

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshconfig"
	"golang.org/x/crypto/ssh"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the real configured production adapter, including its initial
// transport origin. The peer would accept any key if negotiation reaches auth.
func TestReviewLegacyTransportRejectedBeforeAuth(t *testing.T) {
	for _, kind := range []string{"host-sha1", "kex-sha1", "mac-sha1-96"} {
		t.Run(kind, func(t *testing.T) {
			host, _ := mustKey(t)
			if kind == "host-sha1" {
				k, e := rsa.GenerateKey(rand.Reader, 2048)
				if e != nil {
					t.Fatal(e)
				}
				s, _ := ssh.NewSignerFromKey(k)
				host, _ = ssh.NewSignerWithAlgorithms(s.(ssh.AlgorithmSigner), []string{ssh.KeyAlgoRSA})
			}
			identity, _ := identityFixture(t, false)
			ln, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			defer ln.Close()
			addr := ln.Addr().String()
			_, port, _ := net.SplitHostPort(addr)
			var auth atomic.Int32
			done := make(chan struct{})
			go func() {
				defer close(done)
				raw, e := ln.Accept()
				if e != nil {
					return
				}
				defer raw.Close()
				raw.SetDeadline(time.Now().Add(5 * time.Second))
				sc := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { auth.Add(1); return nil, nil }}
				if kind == "kex-sha1" {
					sc.KeyExchanges = []string{"diffie-hellman-group14-sha1"}
				}
				if kind == "mac-sha1-96" {
					sc.Ciphers = []string{"aes128-ctr"}
					sc.MACs = []string{"hmac-sha1-96"}
				}
				sc.AddHostKey(host)
				c, _, _, e := ssh.NewServerConn(raw, sc)
				if e == nil {
					c.Close()
				}
			}()
			home := t.TempDir()
			config := filepath.Join(home, "config")
			known := writeKnown(t, host, addr)
			body := fmt.Sprintf("Host *\n User git\n Port %s\n IdentityAgent none\n IdentityFile %s\n UserKnownHostsFile %s\n GlobalKnownHostsFile none\n StrictHostKeyChecking yes\n UpdateHostKeys no\n", port, identity, known)
			if e := os.WriteFile(config, []byte(body), 0600); e != nil {
				t.Fatal(e)
			}
			in := sshconfig.Input{Home: home, LocalUser: "fixture", UserConfig: config, SystemConfig: filepath.Join(home, "missing")}
			var diag Diagnostic
			_, e = List(context.Background(), "ssh://git@"+addr+"/repo.git", Options{SSHConfig: &in, SSHDiagnostics: true, Diag: &diag, Timeout: 5 * time.Second}.withLoopback())
			<-done
			if e == nil || auth.Load() != 0 || diag.PublicKeyAuthCallbackInvoked {
				t.Fatal("legacy transport reached user authentication")
			}
		})
	}
}
