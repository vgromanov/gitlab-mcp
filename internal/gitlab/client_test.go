package gitlab

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
)

func TestNewClient_basic(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(ts.Close)
	c, err := NewClient(&config.Config{Token: "t", APIURL: ts.URL + "/api/v4"})
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("nil client")
	}
}

func TestNewClient_proxyAndInsecure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(ts.Close)
	_, err := NewClient(&config.Config{
		Token: "t", APIURL: ts.URL + "/api/v4",
		InsecureSkipVerify: true,
		HTTPSProxy:         "http://127.0.0.1:9",
		HTTPProxy:          "http://127.0.0.1:9",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewClient(&config.Config{
		Token: "t", APIURL: ts.URL + "/api/v4",
		HTTPProxy: "http://127.0.0.1:9",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewClient(&config.Config{
		Token: "t", APIURL: ts.URL + "/api/v4",
		HTTPSProxy: ":bad",
		HTTPProxy:  ":bad",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNewClient_withCACert(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pemPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(pemPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(ts.Close)
	_, err = NewClient(&config.Config{Token: "t", APIURL: ts.URL + "/api/v4", CACertPath: pemPath})
	if err != nil {
		t.Fatal(err)
	}
	_ = tlsConfig(&config.Config{CACertPath: filepath.Join(t.TempDir(), "missing.pem")})
}

func TestTLSConfig_insecure(t *testing.T) {
	tc := tlsConfig(&config.Config{InsecureSkipVerify: true})
	if !tc.InsecureSkipVerify {
		t.Fatal("expected insecure")
	}
}
