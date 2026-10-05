//go:build linux || darwin

package gitcache

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
)

// TestColdWarmAuthorizedHTTPAcquire exercises the real Acquire path: authorized
// HTTPS upload-pack → DecodeIndexed → PublishGeneration storage, then warm hit.
// Negatives cover stale Expected*, cancel, and source-fork namespace isolation.
func TestColdWarmAuthorizedHTTPAcquire(t *testing.T) {
	ip, head, base := commitIndexedPack(t)
	packBytes := append([]byte(nil), ip.PackBytes...)

	caPEM, serverCert := acquireTestServerCert(t)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	const token = "acquire-http-token"
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "oauth2" || pass != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "/info/refs") && r.URL.Query().Get("service") == "git-upload-pack":
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			if err := writeShallowAdv(w, head, base); err != nil {
				http.Error(w, "adv", http.StatusInternalServerError)
			}
		case strings.HasSuffix(r.URL.Path, "/git-upload-pack") && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			body, _ := io.ReadAll(io.LimitReader(r.Body, bounds.MaxPackBytes))
			_ = body
			req := packp.NewUploadPackRequest()
			req.Depth = packp.DepthCommits(2)
			_ = req.Capabilities.Set(capability.Shallow)
			resp := packp.NewUploadPackResponseWithPackfile(req, io.NopCloser(bytes.NewReader(packBytes)))
			resp.Shallows = []plumbing.Hash{base}
			if err := resp.Encode(w); err != nil {
				http.Error(w, "pack", http.StatusInternalServerError)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	host := u.Hostname()
	rawURL := srv.URL + "/repo.git"

	root := regressionRoot(t)
	svc, err := OpenService(ServiceConfig{
		Enabled:    true,
		Root:       root,
		QuotaBytes: bounds.BrootBytes + bounds.GenerationCharge()*4,
		CAPath:     caPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close(context.Background())

	var domain AuthDomain
	actor := "actor-http"
	authDomain := domain.Bind(actor, token)
	g := completeTestGrant(Grant{
		OriginHost:  host,
		ProjectID:   "1",
		ProjectPath: "g/p",
		SourceFork:  "2",
		AuthDomain:  authDomain,
		PolicyFP:    "fp",
		MRIID:       7,
		MRVersion:   MRVersionFromDiffRefs(head, base, base),
		HeadSHA:     head,
		BaseSHA:     base,
		StartSHA:    base,
		ActorID:     actor,
		HTTPSURL:    rawURL,
	})
	auth := StaticAuthorizer{Grant: g}
	intent := AcquireIntent{
		ProjectID: "1", MRIID: 7, Depth: 2, Token: token, CAPath: caPath,
		ExpectedHead: head, ExpectedBase: base, ExpectedStart: base,
		ExpectedMRVersion: g.MRVersion, AllowLoopback: true,
	}

	cold, err := svc.Acquire(context.Background(), intent, auth)
	if err != nil {
		t.Fatalf("cold: %v", err)
	}
	if cold.Warm || cold.Objects == 0 || cold.PackBytes == 0 || cold.GenerationID == "" {
		t.Fatalf("cold result %#v", cold)
	}
	if cold.Grant.SourceFork != "2" || cold.Grant.HeadSHA != head {
		t.Fatalf("cold grant %#v", cold.Grant)
	}

	warm, err := svc.Acquire(context.Background(), intent, auth)
	if err != nil {
		t.Fatalf("warm: %v", err)
	}
	if !warm.Warm || warm.GenerationID != cold.GenerationID || warm.Objects == 0 {
		t.Fatalf("warm result %#v", warm)
	}

	// Stale ExpectedHead must fail closed before content reuse.
	stale := intent
	stale.ExpectedHead = plumbing.NewHash("ffffffffffffffffffffffffffffffffffffffff")
	if _, err := svc.Acquire(context.Background(), stale, auth); !errors.Is(err, ErrAuthz) {
		t.Fatalf("stale: %v", err)
	}

	// Canceled context fails before grant resolve / fetch.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.Acquire(ctx, intent, auth); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}

	// Different source fork is a distinct namespace: no warm reuse of fork-2 content.
	forkGrant := g
	forkGrant.SourceFork = "99"
	forkAuth := StaticAuthorizer{Grant: forkGrant}
	forkCold, err := svc.Acquire(context.Background(), intent, forkAuth)
	if err != nil {
		t.Fatalf("fork cold: %v", err)
	}
	if forkCold.Warm || forkCold.GenerationID == cold.GenerationID {
		t.Fatalf("fork must not reuse other-fork generation: %#v vs %#v", forkCold, cold)
	}
}

func TestAcquireHTTPDeniedTokenAndWrongCA(t *testing.T) {
	ip, head, base := commitIndexedPack(t)
	caPEM, serverCert := acquireTestServerCert(t)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	wrongCA := filepath.Join(t.TempDir(), "wrong.pem")
	if err := os.WriteFile(wrongCA, []byte("not-a-cert\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "oauth2" || pass != "good" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if strings.Contains(r.URL.Path, "/info/refs") {
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			_ = writeShallowAdv(w, head, base)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		req := packp.NewUploadPackRequest()
		req.Depth = packp.DepthCommits(2)
		_ = req.Capabilities.Set(capability.Shallow)
		resp := packp.NewUploadPackResponseWithPackfile(req, io.NopCloser(bytes.NewReader(ip.PackBytes)))
		resp.Shallows = []plumbing.Hash{base}
		_ = resp.Encode(w)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	host, _ := url.Parse(srv.URL)
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(context.Background())

	var domain AuthDomain
	g := completeTestGrant(Grant{
		OriginHost: host.Hostname(), ProjectID: "1", ProjectPath: "g/p", SourceFork: "2",
		AuthDomain: domain.Bind("a", "good"), PolicyFP: "fp", MRIID: 7,
		MRVersion: MRVersionFromDiffRefs(head, base, base),
		HeadSHA:   head, BaseSHA: base, StartSHA: base, ActorID: "a",
		HTTPSURL: srv.URL + "/repo.git",
	})
	auth := StaticAuthorizer{Grant: g}
	baseIntent := AcquireIntent{
		ProjectID: "1", MRIID: 7, Depth: 2, Token: "bad", CAPath: caPath,
		ExpectedHead: head, ExpectedBase: base, ExpectedStart: base,
		ExpectedMRVersion: g.MRVersion, AllowLoopback: true,
	}
	if _, err := mgr.Acquire(context.Background(), baseIntent, auth); err == nil {
		t.Fatal("bad token succeeded")
	}
	badCA := baseIntent
	badCA.Token = "good"
	badCA.CAPath = wrongCA
	if _, err := mgr.Acquire(context.Background(), badCA, auth); err == nil {
		t.Fatal("wrong CA succeeded")
	}
}

func writeShallowAdv(w io.Writer, head, base plumbing.Hash) error {
	ar := packp.NewAdvRefs()
	ar.Head = &head
	ar.References["HEAD"] = head
	ar.References["refs/heads/main"] = head
	ar.References["refs/merge-requests/7/head"] = head
	ar.References["refs/merge-requests/7/base"] = base
	for _, c := range []capability.Capability{capability.Shallow, capability.OFSDelta, capability.AllowReachableSHA1InWant} {
		if err := ar.Capabilities.Add(c); err != nil {
			return err
		}
	}
	ar.Prefix = [][]byte{[]byte("# service=git-upload-pack"), {}}
	return ar.Encode(w)
}

func acquireTestServerCert(t *testing.T) ([]byte, tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "acquire-ca"},
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	return pemBytes, cert
}
