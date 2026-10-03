package testutil

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestCanonicalAPIIdentity(t *testing.T) {
	got, err := CanonicalAPIIdentity("https://gitlab.example/api/v4")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://gitlab.example:443/api/v4" {
		t.Fatalf("got %q", got)
	}
	for _, bad := range []string{
		"",
		"https://user:pass@gitlab.example/api/v4",
		"https://gitlab.example/api/v4?x=1",
		"https://gitlab.example/api/v4#frag",
		"https://gitlab.example/api/v3",
		"ftp://gitlab.example/api/v4",
	} {
		if _, err := CanonicalAPIIdentity(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestCanonicalProjectID(t *testing.T) {
	got, err := CanonicalProjectID("042")
	if err != nil || got != "42" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	got, err = CanonicalProjectID("group%2Fproj")
	if err != nil || got != "group/proj" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	for _, bad := range []string{
		"",
		"group%252Fproj",
		"group//proj",
		"/group/proj",
		"group/proj/",
		"group/./proj",
		"group/../proj",
		"group/\x00proj",
		"a%2",
	} {
		if _, err := CanonicalProjectID(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestAuthorizeLiveWrite_matrix(t *testing.T) {
	auth := AuthorizedLiveTarget{APIURL: "https://gitlab.example/api/v4", ProjectID: "42"}
	t.Setenv("INTEGRATION_ALLOW_WRITE", "")
	t.Setenv("GITLAB_API_URL", "https://gitlab.example/api/v4")
	t.Setenv("GITLAB_TEST_PROJECT_ID", "42")
	if err := AuthorizeLiveWrite(auth); err == nil {
		t.Fatal("expected deny without write flag")
	}

	t.Setenv("INTEGRATION_ALLOW_WRITE", "1")
	if err := AuthorizeLiveWrite(auth); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GITLAB_TEST_PROJECT_ID", "99")
	if err := AuthorizeLiveWrite(auth); err == nil {
		t.Fatal("expected project mismatch")
	}

	t.Setenv("GITLAB_TEST_PROJECT_ID", "42")
	t.Setenv("GITLAB_API_URL", "https://other.example/api/v4")
	if err := AuthorizeLiveWrite(auth); err == nil {
		t.Fatal("expected instance mismatch")
	}

	if err := AuthorizeLiveWrite(AuthorizedLiveTarget{}); err == nil {
		t.Fatal("empty authorized must fail closed")
	}
}

func TestDenyWriteTransport_rejectsMutations(t *testing.T) {
	fail := &FailIfCalledRoundTripper{}
	rt := WrapDenyWrite(fail)
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1/api/v4/projects", strings.NewReader("{}"))
	_, err := rt.RoundTrip(req)
	if !errors.Is(err, ErrLiveWriteDenied) {
		t.Fatalf("err=%v", err)
	}
	if fail.Called.Load() {
		t.Fatal("underlying RoundTrip must not be called")
	}

	okNext := &captureRT{status: 200, body: `[]`}
	rt = WrapDenyWrite(okNext)
	req, _ = http.NewRequest(http.MethodGet, "http://127.0.0.1/api/v4/projects", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !okNext.called {
		t.Fatal("GET should reach underlying")
	}
}

func TestScopedLiveTransport_allowAndReject(t *testing.T) {
	auth := AuthorizedLiveTarget{APIURL: "https://gitlab.example/api/v4", ProjectID: "42"}
	fail := &FailIfCalledRoundTripper{}
	rt, err := newScopedLiveTransport(fail, auth)
	if err != nil {
		t.Fatal(err)
	}

	allow, _ := http.NewRequest(http.MethodGet, "https://gitlab.example/api/v4/projects/42/merge_requests?state=opened", nil)
	okNext := &captureRT{status: 200, body: `[]`}
	rtOK, err := newScopedLiveTransport(okNext, auth)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rtOK.RoundTrip(allow)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !okNext.called {
		t.Fatal("allowed project request must reach underlying (query preserved)")
	}
	if allow.URL.RawQuery != "state=opened" {
		t.Fatalf("query mutated: %q", allow.URL.RawQuery)
	}

	rejects := []struct {
		name    string
		url     string
		host    string
		rawPath string // preserve ambiguous forms Go's Parse would otherwise clean
		mut     func(*http.Request)
	}{
		{"cross-project", "https://gitlab.example/api/v4/projects/43/issues", "", "", nil},
		{"global-projects", "https://gitlab.example/api/v4/projects", "", "", nil},
		{"cross-instance", "https://evil.example/api/v4/projects/42/issues", "", "", nil},
		{"traversal", "https://gitlab.example/api/v4/projects/42/issues", "", "/api/v4/projects/42/../../projects/43/issues", nil},
		{"encoded-dotdot", "https://gitlab.example/api/v4/projects/42/issues", "", "/api/v4/projects/42/%2e%2e/projects/43/issues", nil},
		{"encoded-slash-suffix", "https://gitlab.example/api/v4/projects/42/issues", "", "/api/v4/projects/42/a%2Fb", nil},
		{"double-slash", "https://gitlab.example/api/v4/projects/42/issues", "", "/api/v4/projects/42//issues", nil},
		{"double-encoded-project", "https://gitlab.example/api/v4/projects/42/issues", "", "/api/v4/projects/42%252Fxx/issues", nil},
		{"host-override", "https://gitlab.example/api/v4/projects/42/issues", "evil.example", "", nil},
		{"opaque", "https://gitlab.example/api/v4/projects/42/issues", "", "", func(r *http.Request) {
			r.URL.Opaque = "//gitlab.example/api/v4/projects/43/issues"
		}},
	}
	for _, tc := range rejects {
		t.Run(tc.name, func(t *testing.T) {
			fail.Called.Store(false)
			req, err := http.NewRequest(http.MethodGet, tc.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.rawPath != "" {
				req.URL.Path = tc.rawPath
				req.URL.RawPath = tc.rawPath
			}
			if tc.host != "" {
				req.Host = tc.host
			}
			if tc.mut != nil {
				tc.mut(req)
			}
			_, err = rt.RoundTrip(req)
			if err == nil {
				t.Fatal("expected deny")
			}
			if !errors.Is(err, ErrLiveScopeDenied) {
				t.Fatalf("expected ErrLiveScopeDenied, got %v", err)
			}
			if fail.Called.Load() {
				t.Fatal("underlying RoundTrip must not be called")
			}
		})
	}
}

func TestScopedLiveTransport_pathProject(t *testing.T) {
	auth := AuthorizedLiveTarget{APIURL: "http://127.0.0.1:8080/api/v4", ProjectID: "group/proj"}
	okNext := &captureRT{status: 200, body: `{}`}
	rt, err := newScopedLiveTransport(okNext, auth)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/v4/projects/group%2Fproj/issues", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !okNext.called {
		t.Fatal("path project allow failed")
	}

	fail := &FailIfCalledRoundTripper{}
	rt, err = newScopedLiveTransport(fail, auth)
	if err != nil {
		t.Fatal(err)
	}
	req, _ = http.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/v4/projects/group%2Fother/issues", nil)
	_, err = rt.RoundTrip(req)
	if err == nil || fail.Called.Load() {
		t.Fatalf("cross path project must deny without dial err=%v called=%v", err, fail.Called.Load())
	}
}

func TestWrapAuthorizedScopedLive_requiresFlag(t *testing.T) {
	auth := AuthorizedLiveTarget{APIURL: "https://gitlab.example/api/v4", ProjectID: "1"}
	t.Setenv("INTEGRATION_ALLOW_WRITE", "")
	t.Setenv("GITLAB_API_URL", auth.APIURL)
	t.Setenv("GITLAB_TEST_PROJECT_ID", auth.ProjectID)
	fail := &FailIfCalledRoundTripper{}
	_, err := WrapAuthorizedScopedLive(fail, auth)
	if err == nil {
		t.Fatal("exported mutating wrap must require authorization")
	}
	if fail.Called.Load() {
		t.Fatal("must not call underlying during authorize failure")
	}

	client := &http.Client{Transport: fail}
	if err := InstallAuthorizedScopedLiveTransport(client, auth); err == nil {
		t.Fatal("exported install must require authorization")
	}
}

func TestWrapAuthorizedScopedLive_success(t *testing.T) {
	auth := AuthorizedLiveTarget{APIURL: "https://gitlab.example/api/v4", ProjectID: "1"}
	t.Setenv("INTEGRATION_ALLOW_WRITE", "1")
	t.Setenv("GITLAB_API_URL", "https://gitlab.example/api/v4")
	t.Setenv("GITLAB_TEST_PROJECT_ID", "1")
	ok := &captureRT{status: 200, body: `{}`}
	rt, err := WrapAuthorizedScopedLive(ok, auth)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://gitlab.example/api/v4/projects/1", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !ok.called {
		t.Fatal("authorized scoped GET must reach underlying")
	}
}

type captureRT struct {
	called bool
	status int
	body   string
}

func (c *captureRT) RoundTrip(req *http.Request) (*http.Response, error) {
	c.called = true
	return &http.Response{
		StatusCode: c.status,
		Body:       io.NopCloser(strings.NewReader(c.body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func TestAuthorizeEscapedPath_direct(t *testing.T) {
	if err := authorizeEscapedPath("/api/v4/projects/42/merge_requests", "42"); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("https://gitlab.example/api/v4/projects/42/%2E%2E/%2E%2E/projects/43")
	if err := authorizeEscapedPath(u.EscapedPath(), "42"); err == nil {
		t.Fatal("encoded traversal must fail")
	}
}
