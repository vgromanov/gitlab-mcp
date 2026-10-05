package listx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshconfig"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type algorithmRecorder struct {
	ssh.AlgorithmSigner
	mu   sync.Mutex
	algs []string
}

func (s *algorithmRecorder) SignWithAlgorithm(r io.Reader, data []byte, a string) (*ssh.Signature, error) {
	s.mu.Lock()
	s.algs = append(s.algs, a)
	s.mu.Unlock()
	return s.AlgorithmSigner.SignWithAlgorithm(r, data, a)
}
func TestModernAndLegacyUserSignatureNegotiation(t *testing.T) {
	for _, kind := range []string{"rsa-file-sha256", "rsa-agent-sha512", "rsa-sha1-only", "ecdsa", "ed25519"} {
		t.Run(kind, func(t *testing.T) {
			var private any
			switch kind {
			case "ecdsa":
				private, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			case "ed25519":
				_, private = mustKey(t)
			default:
				private, _ = rsa.GenerateKey(rand.Reader, 2048)
			}
			rawSigner, e := ssh.NewSignerFromKey(private)
			if e != nil {
				t.Fatal(e)
			}
			if kind == "rsa-agent-sha512" {
				ring := agent.NewKeyring()
				if e = ring.Add(agent.AddedKey{PrivateKey: private}); e != nil {
					t.Fatal(e)
				}
				a, b := net.Pipe()
				defer a.Close()
				defer b.Close()
				go agent.ServeAgent(ring, a)
				list, e := agent.NewClient(b).Signers()
				if e != nil || len(list) != 1 {
					t.Fatal("synthetic agent inventory failed")
				}
				rawSigner = list[0]
			}
			record := &algorithmRecorder{AlgorithmSigner: rawSigner.(ssh.AlgorithmSigner)}
			signers, e := configSigners(sshconfig.Config{IdentityAgent: "fixture", PublicKeyAuthentication: true}, []ssh.Signer{record})
			if e != nil {
				t.Fatal(e)
			}
			host, _ := mustKey(t)
			ln, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			defer ln.Close()
			done := make(chan bool, 1)
			go func() {
				raw, e := ln.Accept()
				if e != nil {
					done <- false
					return
				}
				defer raw.Close()
				raw.SetDeadline(time.Now().Add(5 * time.Second))
				sc := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil }}
				switch kind {
				case "rsa-file-sha256":
					sc.PublicKeyAuthAlgorithms = []string{ssh.KeyAlgoRSASHA256}
				case "rsa-agent-sha512":
					sc.PublicKeyAuthAlgorithms = []string{ssh.KeyAlgoRSASHA512}
				case "rsa-sha1-only":
					sc.PublicKeyAuthAlgorithms = []string{ssh.KeyAlgoRSA}
				}
				sc.AddHostKey(host)
				c, _, _, e := ssh.NewServerConn(raw, sc)
				done <- e == nil
				if e == nil {
					c.Close()
				}
			}()
			cfg := &ssh.ClientConfig{User: "fixture", Auth: []ssh.AuthMethod{ssh.PublicKeys(signers...)}, HostKeyCallback: ssh.FixedHostKey(host.PublicKey()), Timeout: 5 * time.Second}
			secureAlgorithms(cfg)
			c, e := ssh.Dial("tcp", ln.Addr().String(), cfg)
			if c != nil {
				c.Close()
			}
			ok := <-done
			record.mu.Lock()
			defer record.mu.Unlock()
			if kind == "rsa-sha1-only" {
				if e == nil || ok || len(record.algs) != 0 {
					t.Fatal("legacy-only server solicited signature")
				}
			} else {
				if e != nil || !ok || len(record.algs) != 1 {
					t.Fatalf("modern authentication failed: %v", e)
				}
			}
			for _, a := range record.algs {
				if a == ssh.KeyAlgoRSA {
					t.Fatal("SHA1 signature requested")
				}
			}
		})
	}
}
func TestRestrictedRSASignerRetainsNarrowSHA2Support(t *testing.T) {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	s, _ := ssh.NewSignerFromKey(k)
	limited, _ := ssh.NewSignerWithAlgorithms(s.(ssh.AlgorithmSigner), []string{ssh.KeyAlgoRSASHA256})
	got, e := secureSigner(limited)
	if e != nil {
		t.Fatal(e)
	}
	a := got.(ssh.MultiAlgorithmSigner).Algorithms()
	if len(a) != 1 || a[0] != ssh.KeyAlgoRSASHA256 {
		t.Fatal("narrow signature policy widened")
	}
}
