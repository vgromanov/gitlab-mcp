//go:build integration

package testutil

import (
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// LOCAL-ONLY fake proofs: no dotenv lookup in these cases, no live mutation,
// fake token/env only. Demonstrates mutating-path fail-closed before any dial.
func TestLOCALONLY_DenyWriteAndAuthorizeCompileProof(t *testing.T) {
	fail := &FailIfCalledRoundTripper{}
	rt := WrapDenyWrite(fail)
	req, _ := http.NewRequest(http.MethodDelete, "https://gitlab.example/api/v4/projects/1", nil)
	_, err := rt.RoundTrip(req)
	if !errors.Is(err, ErrLiveWriteDenied) {
		t.Fatalf("err=%v", err)
	}
	if fail.Called.Load() {
		t.Fatal("deny-write must not dial")
	}

	auth := AuthorizedLiveTarget{APIURL: "https://gitlab.example/api/v4", ProjectID: "7"}
	t.Setenv("INTEGRATION_ALLOW_WRITE", "")
	t.Setenv("GITLAB_API_URL", auth.APIURL)
	t.Setenv("GITLAB_TEST_PROJECT_ID", "7")
	t.Setenv("GITLAB_PERSONAL_ACCESS_TOKEN", "fake-token-not-for-live")
	if err := AuthorizeLiveWrite(auth); err == nil {
		t.Fatal("missing write flag must fail closed")
	}
	_, err = WrapAuthorizedScopedLive(fail, auth)
	if err == nil {
		t.Fatal("exported mutating wrap must not bypass authorize")
	}
	if fail.Called.Load() {
		t.Fatal("zero underlying calls on reject")
	}

	t.Setenv("INTEGRATION_ALLOW_WRITE", "1")
	t.Setenv("GITLAB_API_URL", "https://evil.example/api/v4")
	if err := AuthorizeLiveWrite(auth); err == nil {
		t.Fatal("wrong claim must fail closed")
	}
	_, err = WrapAuthorizedScopedLive(fail, auth)
	if err == nil {
		t.Fatal("exported mutating wrap must not bypass wrong claim")
	}
	if fail.Called.Load() {
		t.Fatal("zero underlying calls on reject")
	}

	// Private direct guard seam for local fake scope proofs (already authorized shape).
	t.Setenv("GITLAB_API_URL", auth.APIURL)
	rt2, err := newScopedLiveTransport(fail, auth)
	if err != nil {
		t.Fatal(err)
	}
	req, _ = http.NewRequest(http.MethodPost, "https://gitlab.example/api/v4/projects/8/issues", nil)
	_, err = rt2.RoundTrip(req)
	if err == nil || fail.Called.Load() {
		t.Fatalf("cross-project must deny without dial err=%v called=%v", err, fail.Called.Load())
	}
}

// TestLOCALONLY_MutatingClientFailsClosedBeforeDial re-execs this test binary to
// prove GitLabMutatingIntegrationClient fatals on missing write authorization
// before any HTTP client use (no live dial).
func TestLOCALONLY_MutatingClientFailsClosedBeforeDial(t *testing.T) {
	if os.Getenv("TESTUTIL_MUTATING_FATAL_CHILD") == "1" {
		auth := AuthorizedLiveTarget{APIURL: "https://gitlab.example/api/v4", ProjectID: "1"}
		// Missing INTEGRATION_ALLOW_WRITE → AuthorizeLiveWrite fails → Fatalf.
		_, _ = GitLabMutatingIntegrationClient(t, auth)
		t.Fatal("unreachable: expected Fatalf from missing authorize")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestLOCALONLY_MutatingClientFailsClosedBeforeDial$", "-test.count=1")
	cmd.Env = []string{
		"TESTUTIL_MUTATING_FATAL_CHILD=1",
		"GITLAB_API_URL=https://gitlab.example/api/v4",
		"GITLAB_TEST_PROJECT_ID=1",
		"GITLAB_PERSONAL_ACCESS_TOKEN=fake-token-not-for-live",
		// INTEGRATION_ALLOW_WRITE intentionally unset
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected child failure, got success\n%s", out)
	}
	if !strings.Contains(string(out), "authorize live write") {
		t.Fatalf("expected authorize fatal in child output, got err=%v out=%s", err, out)
	}
}
