//go:build integration

package testutil

import (
	"errors"
	"net/http"
	"testing"
)

// LOCAL-ONLY fake proofs: no dotenv lookup, no live mutation, fake token/env only.
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
	t.Setenv("INTEGRATION_ALLOW_WRITE", "1")
	t.Setenv("GITLAB_API_URL", "https://evil.example/api/v4")
	t.Setenv("GITLAB_TEST_PROJECT_ID", "7")
	if err := AuthorizeLiveWrite(auth); err == nil {
		t.Fatal("wrong claim must fail closed")
	}
	_, err = WrapAuthorizedScopedLive(fail, auth)
	if err == nil {
		t.Fatal("exported mutating wrap must not bypass authorize")
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
