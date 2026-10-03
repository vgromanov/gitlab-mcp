package gitlab

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

func TestSafeCheckRedirect_policy(t *testing.T) {
	t.Parallel()
	viaGET := []*http.Request{mustReq(t, http.MethodGet, "http://a.example/api/v4/x")}
	next := mustReq(t, http.MethodGet, "http://b.example/api/v4/y")
	next.Header.Set("Private-Token", "tok")
	next.Header.Set("Job-Token", "job")
	next.Header.Set("Authorization", "Bearer x")
	if err := safeCheckRedirect(next, viaGET); err != nil {
		t.Fatal(err)
	}
	if next.Header.Get("Private-Token") != "" || next.Header.Get("Job-Token") != "" || next.Header.Get("Authorization") != "" {
		t.Fatalf("expected credential headers stripped on cross-origin GET, got %v", next.Header)
	}

	viaPOST := []*http.Request{mustReq(t, http.MethodPost, "http://a.example/api/v4/x")}
	nextPOST := mustReq(t, http.MethodPost, "http://a.example/api/v4/y")
	if err := safeCheckRedirect(nextPOST, viaPOST); err == nil {
		t.Fatal("expected refuse redirect for POST")
	}
}

func TestSafeReadCheckRetry_failClosedAndUnsafe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ok, err := safeReadCheckRetry(ctx, nil, fmt.Errorf("transport boom"))
	if err != nil || ok {
		t.Fatalf("method-less must fail closed: retry=%v err=%v", ok, err)
	}
	postResp := &http.Response{StatusCode: 500, Request: mustReq(t, http.MethodPost, "http://x/")}
	ok, err = safeReadCheckRetry(ctx, postResp, nil)
	if err != nil || ok {
		t.Fatalf("POST 500 must not retry: retry=%v err=%v", ok, err)
	}
	getResp := &http.Response{StatusCode: 500, Request: mustReq(t, http.MethodGet, "http://x/")}
	ok, err = safeReadCheckRetry(ctx, getResp, nil)
	if err != nil || !ok {
		t.Fatalf("GET 500 should retry: retry=%v err=%v", ok, err)
	}
}

func TestNewGuardedClient_basic(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(ts.Close)
	c, err := NewGuardedClient(&config.Config{Token: "fake-token-not-live", APIURL: ts.URL + "/api/v4"})
	if err != nil || c == nil {
		t.Fatalf("guarded: %v %v", c, err)
	}
	read, guarded, err := NewClients(&config.Config{Token: "fake-token-not-live", APIURL: ts.URL + "/api/v4"})
	if err != nil || read == nil || guarded == nil {
		t.Fatalf("NewClients: %v", err)
	}
}

func TestAC1_unsafeMethods_bothConstructors(t *testing.T) {
	methods := []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
	outcomes := []struct {
		name    string
		handler func(barrier *testutil.AcceptedBarrier) http.Handler
		cancel  bool
	}{
		{name: "429", handler: func(_ *testutil.AcceptedBarrier) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"message":"rate"}`)
			})
		}},
		{name: "500", handler: func(_ *testutil.AcceptedBarrier) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"message":"boom"}`)
			})
		}},
		{name: "accepted_drop", cancel: true, handler: func(b *testutil.AcceptedBarrier) http.Handler {
			return testutil.ScriptAcceptedThenDrop(b)
		}},
	}
	ctors := []struct {
		name string
		new  func(*config.Config, ...Option) (*gitlab.Client, error)
	}{
		{name: "NewClient", new: NewClient},
		{name: "NewGuardedClient", new: NewGuardedClient},
	}

	for _, ctor := range ctors {
		for _, method := range methods {
			for _, outcome := range outcomes {
				ctor, method, outcome := ctor, method, outcome
				t.Run(ctor.name+"/"+method+"/"+outcome.name, func(t *testing.T) {
					barrier := testutil.NewAcceptedBarrier()
					rec := &testutil.RecordingHandler{Next: outcome.handler(barrier)}
					ts := httptest.NewServer(rec)
					t.Cleanup(ts.Close)
					cli, err := ctor.new(&config.Config{Token: "fake-token-not-live", APIURL: ts.URL + "/api/v4"})
					if err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()

					var callErr error
					var wg sync.WaitGroup
					if outcome.cancel {
						wg.Add(1)
						go func() {
							defer wg.Done()
							callErr = doMethod(ctx, cli, method)
						}()
						select {
						case <-barrier.Accepted():
						case <-time.After(5 * time.Second):
							t.Fatal("accepted barrier timeout")
						}
						if got := rec.Attempts(); got != 1 {
							t.Fatalf("attempts at accept=%d want 1", got)
						}
						cancel()
						wg.Wait()
						if callErr == nil {
							t.Fatal("expected error after drop/cancel")
						}
					} else {
						callErr = doMethod(ctx, cli, method)
						if callErr == nil {
							t.Fatal("expected error")
						}
					}
					if got := rec.Attempts(); got != 1 {
						t.Fatalf("attempts=%d want exact 1 methods=%v", got, rec.Snapshot())
					}
					if got := rec.MethodCount(method); got != 1 {
						t.Fatalf("%s count=%d want 1", method, got)
					}
				})
			}
		}
	}
}

func TestAC1_graphQL_bothConstructors(t *testing.T) {
	outcomes := []struct {
		name    string
		handler func(barrier *testutil.AcceptedBarrier) http.Handler
		cancel  bool
	}{
		{name: "429", handler: func(_ *testutil.AcceptedBarrier) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"message":"rate"}`)
			})
		}},
		{name: "500", handler: func(_ *testutil.AcceptedBarrier) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"errors":[{"message":"boom"}]}`)
			})
		}},
		{name: "accepted_drop", cancel: true, handler: func(b *testutil.AcceptedBarrier) http.Handler {
			return testutil.ScriptAcceptedThenDrop(b)
		}},
	}
	ctors := []struct {
		name string
		new  func(*config.Config, ...Option) (*gitlab.Client, error)
	}{
		{name: "NewClient", new: NewClient},
		{name: "NewGuardedClient", new: NewGuardedClient},
	}
	for _, ctor := range ctors {
		for _, outcome := range outcomes {
			ctor, outcome := ctor, outcome
			t.Run(ctor.name+"/GraphQL/"+outcome.name, func(t *testing.T) {
				barrier := testutil.NewAcceptedBarrier()
				rec := &testutil.RecordingHandler{Next: outcome.handler(barrier)}
				ts := httptest.NewServer(rec)
				t.Cleanup(ts.Close)
				cli, err := ctor.new(&config.Config{Token: "fake-token-not-live", APIURL: ts.URL + "/api/v4"})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var callErr error
				var wg sync.WaitGroup
				run := func() {
					var out any
					_, callErr = cli.GraphQL.Do(gitlab.GraphQLQuery{Query: "{ __typename }"}, &out, gitlab.WithContext(ctx))
				}
				if outcome.cancel {
					wg.Add(1)
					go func() { defer wg.Done(); run() }()
					select {
					case <-barrier.Accepted():
					case <-time.After(5 * time.Second):
						t.Fatal("accepted timeout")
					}
					cancel()
					wg.Wait()
				} else {
					run()
				}
				if callErr == nil {
					t.Fatal("expected GraphQL error")
				}
				if got := rec.Attempts(); got != 1 {
					t.Fatalf("attempts=%d want 1", got)
				}
				if got := rec.MethodCount(http.MethodPost); got != 1 {
					t.Fatalf("POST=%d want 1 snapshot=%v", got, rec.Snapshot())
				}
			})
		}
	}
}

func TestAC3_boundedGETRetry_NewClient(t *testing.T) {
	const retryMax = 5 // SDK default retained on NewClient
	rec := &testutil.RecordingHandler{Next: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"boom"}`)
	})}
	ts := httptest.NewServer(rec)
	t.Cleanup(ts.Close)
	// Production NewClient (not a vendored reassembly). Optional production seam
	// WithRetryWaitMinMax(0,0) only accelerates waits; retry policy remains NewClient's.
	cli, err := NewClient(
		&config.Config{Token: "fake-token-not-live", APIURL: ts.URL + "/api/v4"},
		WithRetryWaitMinMax(0, 0),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = cli.Projects.ListProjects(&gitlab.ListProjectsOptions{}, gitlab.WithContext(context.Background()))
	if err == nil {
		t.Fatal("expected 500")
	}
	want := int64(1 + retryMax)
	if got := rec.Attempts(); got != want {
		t.Fatalf("attempts=%d want %d", got, want)
	}
	if got := rec.MethodCount(http.MethodGet); got != int(want) {
		t.Fatalf("GET=%d want %d", got, want)
	}
}

func TestAC2_mutatingRedirect_noReplay(t *testing.T) {
	var secondHits atomic.Int64
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHits.Add(1)
		_, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(second.Close)

	ctors := []struct {
		name string
		new  func(*config.Config, ...Option) (*gitlab.Client, error)
	}{
		{"NewClient", NewClient},
		{"NewGuardedClient", NewGuardedClient},
	}
	statuses := []struct {
		name   string
		status int
	}{
		{"307", http.StatusTemporaryRedirect},
		{"308", http.StatusPermanentRedirect},
	}

	for _, ctor := range ctors {
		for _, st := range statuses {
			ctor, st := ctor, st
			t.Run(ctor.name+"/"+st.name, func(t *testing.T) {
				secondHits.Store(0)
				rec := &testutil.RecordingHandler{Next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Location", second.URL+"/api/v4/projects")
					w.WriteHeader(st.status)
				})}
				ts := httptest.NewServer(rec)
				t.Cleanup(ts.Close)
				cli, err := ctor.new(&config.Config{Token: "fake-token-not-live", APIURL: ts.URL + "/api/v4"})
				if err != nil {
					t.Fatal(err)
				}
				err = doMethodWithBody(context.Background(), cli, http.MethodPost, map[string]any{
					"name":        "rvg125-replay-payload",
					"description": "nonempty body must not be replayed on mutating redirect",
				})
				if err == nil {
					t.Fatal("expected redirect refusal error")
				}
				if got := rec.Attempts(); got != 1 {
					t.Fatalf("upstream attempts=%d want 1", got)
				}
				if secondHits.Load() != 0 {
					t.Fatalf("redirect target hits=%d want 0 (no body replay)", secondHits.Load())
				}
			})
		}
	}
}

func TestAC2_crossOrigin_stripsGitLabCredentials(t *testing.T) {
	var (
		originHits                           atomic.Int64
		destHits                             atomic.Int64
		originPrivate, originJob, originAuth atomic.Bool
		destPrivate, destJob, destAuth       atomic.Bool
	)
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destHits.Add(1)
		if r.Header.Get("Private-Token") != "" {
			destPrivate.Store(true)
		}
		if r.Header.Get("Job-Token") != "" {
			destJob.Store(true)
		}
		if r.Header.Get("Authorization") != "" {
			destAuth.Store(true)
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(dest.Close)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits.Add(1)
		if r.Header.Get("Private-Token") != "" {
			originPrivate.Store(true)
		}
		if r.Header.Get("Job-Token") != "" {
			originJob.Store(true)
		}
		if r.Header.Get("Authorization") != "" {
			originAuth.Store(true)
		}
		w.Header().Set("Location", dest.URL+"/api/v4/projects")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	cli, err := NewClient(&config.Config{Token: "fake-token-not-live", APIURL: origin.URL + "/api/v4"})
	if err != nil {
		t.Fatal(err)
	}
	// Set synthetic headers via SDK request options before Do so they are part of
	// the redirect header-copy snapshot (not late-injected in RoundTrip).
	_, _, err = cli.Projects.ListProjects(&gitlab.ListProjectsOptions{},
		gitlab.WithContext(context.Background()),
		gitlab.WithHeader("Job-Token", "synthetic-job-token"),
		gitlab.WithHeader("Authorization", "Bearer synthetic-auth"),
	)
	if err != nil {
		t.Fatalf("expected successful cross-origin GET follow: %v", err)
	}
	if originHits.Load() != 1 {
		t.Fatalf("origin hits=%d want 1", originHits.Load())
	}
	if destHits.Load() != 1 {
		t.Fatalf("destination hits=%d want exactly 1", destHits.Load())
	}
	if !originPrivate.Load() || !originJob.Load() || !originAuth.Load() {
		t.Fatalf("origin must receive synthetic credentials private=%v job=%v auth=%v",
			originPrivate.Load(), originJob.Load(), originAuth.Load())
	}
	if destPrivate.Load() || destJob.Load() || destAuth.Load() {
		t.Fatalf("destination must not receive credentials private=%v job=%v auth=%v",
			destPrivate.Load(), destJob.Load(), destAuth.Load())
	}
}

func TestAC4_syntheticCATLSHandshake(t *testing.T) {
	caPEM, serverCert, serverKey := mustGenerateCAAndServer(t)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	rec := &testutil.RecordingHandler{Next: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})}
	ts := httptest.NewUnstartedServer(rec)
	cert, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	ts.StartTLS()
	t.Cleanup(ts.Close)

	cli, err := NewClient(&config.Config{
		Token:      "fake-token-not-live",
		APIURL:     ts.URL + "/api/v4",
		CACertPath: caPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = cli.Projects.ListProjects(&gitlab.ListProjectsOptions{}, gitlab.WithContext(context.Background()))
	if err != nil {
		t.Fatalf("TLS handshake/list via synthetic CA failed: %v", err)
	}
	if rec.Attempts() < 1 {
		t.Fatal("expected successful RoundTrip through TLS")
	}

	gcli, err := NewGuardedClient(&config.Config{
		Token:      "fake-token-not-live",
		APIURL:     ts.URL + "/api/v4",
		CACertPath: caPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = gcli.Projects.ListProjects(&gitlab.ListProjectsOptions{}, gitlab.WithContext(context.Background()))
	if err != nil {
		t.Fatalf("guarded TLS: %v", err)
	}
}

func TestAC4_localProxyObservesRequest(t *testing.T) {
	backendRec := &testutil.RecordingHandler{Next: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})}
	backend := httptest.NewServer(backendRec)
	t.Cleanup(backend.Close)

	var proxyHits atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		outReq := r.Clone(r.Context())
		outReq.RequestURI = "" // always clear for client RoundTrip
		if outReq.URL.Scheme == "" {
			u, err := url.Parse(r.RequestURI)
			if err != nil {
				http.Error(w, err.Error(), 502)
				return
			}
			outReq.URL = u
		}
		resp, err := http.DefaultTransport.RoundTrip(outReq)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(proxy.Close)

	cli, err := NewClient(&config.Config{
		Token:     "fake-token-not-live",
		APIURL:    backend.URL + "/api/v4",
		HTTPProxy: proxy.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = cli.Projects.ListProjects(&gitlab.ListProjectsOptions{}, gitlab.WithContext(context.Background()))
	if err != nil {
		t.Fatalf("via proxy: %v", err)
	}
	if proxyHits.Load() < 1 {
		t.Fatal("expected local proxy to observe at least one request")
	}
	if backendRec.Attempts() < 1 {
		t.Fatal("expected backend hit through proxy")
	}
}

func doMethod(ctx context.Context, cli *gitlab.Client, method string) error {
	return doMethodWithBody(ctx, cli, method, nil)
}

func doMethodWithBody(ctx context.Context, cli *gitlab.Client, method string, body any) error {
	req, err := cli.NewRequest(method, "projects", body, []gitlab.RequestOptionFunc{gitlab.WithContext(ctx)})
	if err != nil {
		return err
	}
	_, err = cli.Do(req, nil)
	return err
}

func mustReq(t *testing.T, method, raw string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(method, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustGenerateCAAndServer(t *testing.T) (caPEM, certPEM, keyPEM []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "rvg125-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER})
	keyBytes, err := x509.MarshalECPrivateKey(srvKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	return caPEM, certPEM, keyPEM
}
