package tlsx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/origin"
)

func TestMalformedCAFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(Input{ServerName: "127.0.0.1", CAPath: path}); err == nil {
		t.Fatal("malformed CA was accepted")
	}
	if err := os.WriteFile(path, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(Input{ServerName: "127.0.0.1", CAPath: path}); err == nil {
		t.Fatal("empty CA was accepted")
	}
}

func TestInsecureOnlyCorporateHost(t *testing.T) {
	if _, err := Build(Input{ServerName: "127.0.0.1", Insecure: true}); err == nil {
		t.Fatal("insecure accepted for loopback")
	}
	cfg, err := Build(Input{ServerName: origin.DefaultInsecureHost, Insecure: true})
	if err != nil || !cfg.InsecureSkipVerify {
		t.Fatalf("corporate insecure: %v %#v", err, cfg)
	}
	plain, err := Build(Input{ServerName: origin.DefaultInsecureHost})
	if err != nil || plain.InsecureSkipVerify {
		t.Fatal("insecure defaulted on")
	}
}

func TestTrustedWrongAndHostname(t *testing.T) {
	caCert, caKey, caPEM := newCA(t, "right-ca")
	_, _, wrongPEM := newCA(t, "wrong-ca")
	rightPath := writePEM(t, caPEM)
	wrongPath := writePEM(t, wrongPEM)

	goodLeaf := leaf(t, caCert, caKey, []net.IP{net.ParseIP("127.0.0.1")}, nil)
	badName := leaf(t, caCert, caKey, nil, []string{"wrong.example"})

	trusted := mustBuild(t, Input{ServerName: "127.0.0.1", CAPath: rightPath})
	if err := handshake(t, goodLeaf, trusted); err != nil {
		t.Fatalf("trusted CA: %v", err)
	}
	wrong := mustBuild(t, Input{ServerName: "127.0.0.1", CAPath: wrongPath})
	if err := handshake(t, goodLeaf, wrong); err == nil {
		t.Fatal("wrong CA was trusted")
	}
	mismatch := mustBuild(t, Input{ServerName: "127.0.0.1", CAPath: rightPath})
	if err := handshake(t, badName, mismatch); err == nil {
		t.Fatal("hostname mismatch was trusted")
	}
	systemOnly := mustBuild(t, Input{ServerName: "127.0.0.1"})
	if err := handshake(t, goodLeaf, systemOnly); err == nil {
		t.Fatal("private CA was trusted by system roots alone")
	}
}

func TestInsecureCorporateSkipsLocalVerify(t *testing.T) {
	caCert, caKey, _ := newCA(t, "unused")
	leafCert := leaf(t, caCert, caKey, nil, []string{"wrong.example"})
	cfg := mustBuild(t, Input{ServerName: origin.DefaultInsecureHost, Insecure: true})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	errCh := make(chan error, 1)
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		srv := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{leafCert}, MinVersion: tls.VersionTLS12})
		errCh <- srv.Handshake()
		_ = srv.Close()
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client := tls.Client(raw, cfg)
	if err := client.Handshake(); err != nil {
		t.Fatalf("explicit insecure handshake: %v", err)
	}
	_ = client.Close()
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func mustBuild(t *testing.T, in Input) *tls.Config {
	t.Helper()
	cfg, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func handshake(t *testing.T, server tls.Certificate, clientCfg *tls.Config) error {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	errCh := make(chan error, 1)
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		srv := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{server}, MinVersion: tls.VersionTLS12})
		errCh <- srv.Handshake()
		_ = srv.Close()
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	client := tls.Client(raw, clientCfg)
	err = client.Handshake()
	_ = client.Close()
	<-errCh
	return err
}

func writePEM(t *testing.T, der []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, block, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newCA(t *testing.T, name string) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key, der
}

func leaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, ips []net.IP, dns []string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  ips,
		DNSNames:     dns,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der, ca.Raw}, PrivateKey: key}
}
