package gitlab

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

func mustRawURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestBindRawURL_exactTrustedIdentity(t *testing.T) {
	t.Parallel()
	const sha = "1234567890abcdef1234567890abcdef12345678"
	p := RawProvenance{Ref: sha}

	// Nested + dotted path in PathEscape form (trusted client stamp).
	auth := mustRawURL(t, "http://example.test/api/v4/projects/42/repository/files/dir%2Fa%2Etxt/raw?ref="+sha)
	if err := bindRawURL(p, auth, auth); err != nil {
		t.Fatalf("identical authorized URL must pass: %v", err)
	}
	sameHop := mustRawURL(t, auth.String())
	if err := bindRawURL(p, auth, sameHop); err != nil {
		t.Fatalf("exact same authorized hop must pass: %v", err)
	}

	changedRef := mustRawURL(t, "http://example.test/api/v4/projects/42/repository/files/dir%2Fa%2Etxt/raw?ref=main")
	if err := bindRawURL(p, auth, changedRef); err == nil || !errors.Is(err, ErrRawProvenance) {
		t.Fatalf("want ref change refusal, got %v", err)
	}
	changedProj := mustRawURL(t, "http://example.test/api/v4/projects/99/repository/files/dir%2Fa%2Etxt/raw?ref="+sha)
	if err := bindRawURL(p, auth, changedProj); err == nil {
		t.Fatal("want project path change refusal")
	}
	changedFile := mustRawURL(t, "http://example.test/api/v4/projects/42/repository/files/other%2Eb%2Etxt/raw?ref="+sha)
	if err := bindRawURL(p, auth, changedFile); err == nil {
		t.Fatal("want file path change refusal")
	}
	// Decoded-looking path must not forge equality with escaped trusted path.
	decodedForge := mustRawURL(t, "http://example.test/api/v4/projects/42/repository/files/dir/a.txt/raw?ref="+sha)
	if trustedEscapedPath(auth) == trustedEscapedPath(decodedForge) {
		t.Fatal("test setup: escaped vs decoded paths unexpectedly equal")
	}
	if err := bindRawURL(p, auth, decodedForge); err == nil {
		t.Fatal("want refusal when escaped path identity diverges")
	}
	extraQuery := mustRawURL(t, "http://example.test/api/v4/projects/42/repository/files/dir%2Fa%2Etxt/raw?ref="+sha+"&evil=1")
	if err := bindRawURL(p, auth, extraQuery); err == nil {
		t.Fatal("want forged extra query refusal")
	}
	// Semicolon-bearing junk is dropped by URL.Query() but must fail ParseQuery.
	malformed := mustRawURL(t, "http://example.test/api/v4/projects/42/repository/files/dir%2Fa%2Etxt/raw")
	malformed.RawQuery = "ref=" + sha + "&unexpected=value;bad"
	if _, qerr := url.ParseQuery(malformed.RawQuery); qerr == nil {
		t.Fatal("test setup: expected ParseQuery error for semicolon pair")
	}
	if got := malformed.Query(); len(got) != 1 || got.Get("ref") != sha {
		t.Fatalf("test setup: Query() should silently keep sole ref, got %v", got)
	}
	if err := bindRawURL(p, auth, malformed); err == nil || !errors.Is(err, ErrRawProvenance) {
		t.Fatalf("want malformed query refusal, got %v", err)
	}
	cross := mustRawURL(t, "https://evil.test/api/v4/projects/42/repository/files/dir%2Fa%2Etxt/raw?ref="+sha)
	if err := bindRawURL(p, auth, cross); err == nil {
		t.Fatal("want cross-origin refusal")
	}
	// Trailing slash must not be normalized away under exact escaped-path contract.
	withSlash := mustRawURL(t, "http://example.test/api/v4/projects/42/repository/files/dir%2Fa%2Etxt/raw/?ref="+sha)
	if err := bindRawURL(p, auth, withSlash); err == nil {
		t.Fatal("want refusal when trailing slash diverges from trusted path")
	}
	emptyPath := mustRawURL(t, "http://example.test?ref="+sha)
	emptyPath.Path = ""
	emptyPath.RawPath = ""
	if err := bindRawURL(p, auth, emptyPath); err == nil {
		t.Fatal("want refusal for empty escaped path identity")
	}
}

func TestSafeCheckRedirect_rawProvenanceOptInOnly(t *testing.T) {
	t.Parallel()
	const sha = "1234567890abcdef1234567890abcdef12345678"
	via := []*http.Request{mustReq(t, http.MethodGet, "http://example.test/api/v4/projects/42/repository/files/f%2Etxt/raw?ref="+sha)}
	nextChanged := mustReq(t, http.MethodGet, "http://example.test/api/v4/projects/42/repository/files/f%2Etxt/raw?ref=main")

	// Legacy (no opt-in): method/credential policy only — changed ref is not refused here.
	if err := safeCheckRedirect(nextChanged, via); err != nil {
		t.Fatalf("legacy safeCheckRedirect must not apply raw provenance: %v", err)
	}

	ctx := WithRawProvenance(context.Background(), RawProvenance{Ref: sha})
	via[0] = via[0].WithContext(ctx)
	nextChanged = nextChanged.WithContext(ctx)
	if err := safeCheckRedirect(nextChanged, via); err == nil || !errors.Is(err, ErrRawProvenance) {
		t.Fatalf("opt-in provenance must refuse changed ref, got %v", err)
	}

	same := mustReq(t, http.MethodGet, via[0].URL.String()).WithContext(ctx)
	if err := safeCheckRedirect(same, via); err != nil {
		t.Fatalf("same authorized URL hop must be allowed: %v", err)
	}
}

func TestValidateRawResponseProvenance_awayThenBack(t *testing.T) {
	t.Parallel()
	const sha = "1234567890abcdef1234567890abcdef12345678"
	p := RawProvenance{Ref: sha}
	orig := mustReq(t, http.MethodGet, "http://example.test/api/v4/projects/42/repository/files/dir%2Fa%2Etxt/raw?ref="+sha)
	midResp := &http.Response{StatusCode: http.StatusFound, Request: orig}
	mid := mustReq(t, http.MethodGet, "http://example.test/api/v4/projects/42/repository/files/dir%2Fa%2Etxt/raw?ref=main")
	mid.Response = midResp
	finalResp := &http.Response{StatusCode: http.StatusFound, Request: mid}
	final := mustReq(t, http.MethodGet, "http://example.test/api/v4/projects/42/repository/files/dir%2Fa%2Etxt/raw?ref="+sha)
	final.Response = finalResp
	if err := ValidateRawResponseProvenance(p, final); err == nil || !errors.Is(err, ErrRawProvenance) {
		t.Fatalf("away-then-back must fail without maintained provenance, got %v", err)
	}
}

func TestStreamRawFile_redirectChangedRefDropsContent(t *testing.T) {
	testStreamRawFileRedirectDrops(t, "ref=main")
}

func TestStreamRawFile_redirectMalformedExtraQueryDropsContent(t *testing.T) {
	const sha = "1234567890abcdef1234567890abcdef12345678"
	testStreamRawFileRedirectDrops(t, "ref="+sha+"&unexpected=value;bad")
}

func testStreamRawFileRedirectDrops(t *testing.T, redirectQuery string) {
	t.Helper()
	const sha = "1234567890abcdef1234567890abcdef12345678"
	var pinned, floating atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/raw") {
			http.NotFound(w, r)
			return
		}
		if r.URL.RawQuery == "ref="+sha {
			pinned.Add(1)
			u := *r.URL
			u.RawQuery = redirectQuery
			http.Redirect(w, r, u.String(), http.StatusFound)
			return
		}
		floating.Add(1)
		w.Header().Set("X-Gitlab-Blob-Id", strings.Repeat("1", 40))
		w.Header().Set("X-Gitlab-Content-Sha256", strings.Repeat("2", 64))
		w.Header().Set("X-Gitlab-Size", "7")
		w.Header().Set("X-Gitlab-Encoding", "text")
		_, _ = io.WriteString(w, "MOVING!")
	}))
	t.Cleanup(ts.Close)

	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	res := StreamRawFile(context.Background(), cli, RawStreamRequest{
		ProjectID: "42", FilePath: "f.txt", Ref: sha, MaxReturnBytes: 1024,
	})
	if pinned.Load() != 1 {
		t.Fatalf("pinned=%d", pinned.Load())
	}
	if floating.Load() != 1 {
		t.Fatalf("follow client must hit floating target once, floating=%d", floating.Load())
	}
	if res.Err == nil || !errors.Is(res.Err, ErrRawProvenance) {
		t.Fatalf("want ErrRawProvenance, got err=%v data=%q", res.Err, res.Data)
	}
	if len(res.Data) != 0 || res.FullContentKnown || res.WindowComplete {
		t.Fatalf("must drop content and refuse completeness: %+v", res)
	}
	if res.BlobID != "" || res.ContentSHA256 != "" || res.SizeKnown || res.Size != 0 || res.HeaderEncoding != "" {
		t.Fatalf("unattested provider metadata must be cleared: %+v", res)
	}
	if res.ObservedStart != nil || res.ObservedEndExcl != nil || res.ReturnedRangeSHA != "" {
		t.Fatalf("offsets/hash must be unknown after provenance reject: %+v", res)
	}
}

func TestStreamRawFile_nestedDottedPctEncodedPinnedSucceeds(t *testing.T) {
	const sha = "1234567890abcdef1234567890abcdef12345678"
	var seenEscaped atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/raw") {
			http.NotFound(w, r)
			return
		}
		seenEscaped.Store(r.URL.EscapedPath())
		if r.URL.Query().Get("ref") != sha {
			http.Error(w, "bad ref", 400)
			return
		}
		// Accept PathEscape identity for nested dotted path.
		if !strings.Contains(r.URL.EscapedPath(), "/files/dir%2Fa%2Etxt/raw") {
			http.Error(w, "bad escaped path "+r.URL.EscapedPath(), 400)
			return
		}
		w.Header().Set("X-Gitlab-Size", "7")
		_, _ = io.WriteString(w, "PINNED!")
	}))
	t.Cleanup(ts.Close)

	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	res := StreamRawFile(context.Background(), cli, RawStreamRequest{
		ProjectID: "42", FilePath: "dir/a.txt", Ref: sha, MaxReturnBytes: 1024,
	})
	if res.Err != nil {
		t.Fatalf("nested pinned path must succeed: err=%v escaped=%v", res.Err, seenEscaped.Load())
	}
	if string(res.Data) != "PINNED!" || !res.FullContentKnown {
		t.Fatalf("data=%q full=%v", res.Data, res.FullContentKnown)
	}
}

func TestStreamRawFile_sameURLRedirectAllowedAndChargesHops(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var hops atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/raw") {
			http.NotFound(w, r)
			return
		}
		n := hops.Add(1)
		if n == 1 {
			http.Redirect(w, r, r.URL.String(), http.StatusFound)
			return
		}
		w.Header().Set("X-Gitlab-Size", "4")
		_, _ = io.WriteString(w, "same")
	}))
	t.Cleanup(ts.Close)

	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithHTTPClient(&http.Client{
			Transport:     BudgetInterceptor()(http.DefaultTransport),
			CheckRedirect: safeCheckRedirect,
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	budget := DefaultBudget()
	budget.MaxRequests = 10
	budget.MaxBytes = 1 << 20
	budget.MaxElapsed = 0
	ctx := WithBudget(context.Background(), budget)
	res := StreamRawFile(ctx, cli, RawStreamRequest{
		ProjectID: "42", FilePath: "f.txt", Ref: sha, MaxReturnBytes: 1024,
	})
	if res.Err != nil {
		t.Fatalf("same-URL redirect should succeed: %v", res.Err)
	}
	if string(res.Data) != "same" {
		t.Fatalf("data=%q", res.Data)
	}
	if hops.Load() != 2 {
		t.Fatalf("hops=%d want 2", hops.Load())
	}
	reqN, _, _ := budget.Stats()
	if reqN < 2 {
		t.Fatalf("budget requests=%d want ≥2 (every hop charged)", reqN)
	}
}
