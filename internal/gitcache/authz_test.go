package gitcache_test

import (
	"context"
	"errors"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache"
)

func testGrant() gitcache.Grant {
	h := plumbing.NewHash("2d132b02f44f8995d58d2909a9e1d8cfe929c37e")
	b := plumbing.NewHash("57da8a2a939bc8e28d1c3c3bcf91a93c27102cf2")
	src := "https://example.com/u/fork.git"
	tgt := "https://example.com/g/p.git"
	return gitcache.Grant{
		CanonicalInstance: "https://example.com/api/v4",
		OriginHost:        "example.com",
		ProjectID:         "1",
		ProjectPath:       "g/p",
		SourceFork:        "2",
		SourcePath:        "u/fork",
		TargetProjectID:   "1",
		TargetPath:        "g/p",
		AuthDomain:        "dom",
		PolicyFP:          "fp",
		MRIID:             7,
		MRVersion:         "v1",
		HeadSHA:           h,
		BaseSHA:           b,
		StartSHA:          b,
		HeadRef:           "feature",
		BaseRef:           "main",
		StartRef:          "main",
		ActorID:           "actor",
		SourceHTTPSURL:    src,
		TargetHTTPSURL:    tgt,
		HTTPSURL:          src,
		TrustProvenance:   "test-trust",
	}
}

func TestCallerSHAAloneCannotGrant(t *testing.T) {
	g := testGrant()
	g.HeadSHA = plumbing.ZeroHash
	if err := gitcache.ValidateGrant(g); err == nil {
		t.Fatal("zero head accepted")
	}
	g = testGrant()
	other := plumbing.NewHash("1111111111111111111111111111111111111111")
	if err := gitcache.AuthorizeObject(g, gitcache.ObjectProof{Hash: other, Type: "commit", Role: gitcache.RootHead}); err == nil {
		t.Fatal("caller sha granted")
	}
}

func TestCapabilityAllowTipInsufficient(t *testing.T) {
	if err := gitcache.CapabilityGate(true, false, true, true); err == nil {
		t.Fatal("allow-tip-only path accepted")
	}
	if err := gitcache.CapabilityGate(true, true, true, true); err != nil {
		t.Fatal(err)
	}
}

func TestValidateDepthRejectsFullHistory(t *testing.T) {
	if err := gitcache.ValidateDepth(0); err == nil {
		t.Fatal("depth 0 accepted")
	}
	if err := gitcache.ValidateDepth(3); err == nil {
		t.Fatal("depth 3 accepted")
	}
	if err := gitcache.ValidateDepth(1); err != nil {
		t.Fatal(err)
	}
	if err := gitcache.ValidateDepth(2); err != nil {
		t.Fatal(err)
	}
}

func TestStaticAuthorizerIgnoresCallerGrantMutation(t *testing.T) {
	base := testGrant()
	auth := gitcache.StaticAuthorizer{Grant: base}
	intent := gitcache.AcquireIntent{
		ProjectID:    "1",
		MRIID:        7,
		Depth:        1,
		ExpectedHead: plumbing.NewHash("1111111111111111111111111111111111111111"),
	}
	if _, err := auth.ResolveGrant(context.Background(), intent); !errors.Is(err, gitcache.ErrAuthz) {
		t.Fatalf("stale expected head accepted: %v", err)
	}
	intent.ExpectedHead = plumbing.ZeroHash
	intent.ExpectedMRVersion = "other"
	if _, err := auth.ResolveGrant(context.Background(), intent); !errors.Is(err, gitcache.ErrAuthz) {
		t.Fatalf("stale mr version accepted: %v", err)
	}
	intent.ExpectedMRVersion = ""
	g, err := auth.ResolveGrant(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if g.HeadSHA != base.HeadSHA {
		t.Fatal("resolved grant diverged from authorizer binding")
	}
}

func TestAuthDomainDoesNotPersistToken(t *testing.T) {
	var d gitcache.AuthDomain
	a := d.Bind("actor", "token-one")
	b := d.Bind("actor", "token-two")
	if a == "" || b == "" || a == b {
		t.Fatalf("domain keys: %q %q", a, b)
	}
}

func TestGrantFingerprintBindsCanonicalInstanceAndTrust(t *testing.T) {
	g := testGrant()
	fp0 := gitcache.GrantFingerprint(g)
	g2 := g
	g2.CanonicalInstance = "https://example.com:8443/gitlab"
	if gitcache.GrantFingerprint(g2) == fp0 {
		t.Fatal("fingerprint ignores canonical instance port/prefix")
	}
	g3 := g
	g3.TrustProvenance = "other-trust"
	if gitcache.GrantFingerprint(g3) == fp0 {
		t.Fatal("fingerprint ignores trust provenance")
	}
	g4 := g
	g4.TargetHTTPSURL = "https://example.com/other/p.git"
	if gitcache.GrantFingerprint(g4) == fp0 {
		t.Fatal("fingerprint ignores target clone URL")
	}
}

func TestValidateGrantRejectsHostnameOnlyCanonicalInstance(t *testing.T) {
	g := testGrant()
	g.CanonicalInstance = "example.com"
	if err := gitcache.ValidateGrant(g); err == nil {
		t.Fatal("hostname-only CanonicalInstance accepted (R4)")
	}
	g = testGrant()
	g.CanonicalInstance = "https://example.com:8443/gitlab/api/v4"
	if err := gitcache.ValidateGrant(g); err != nil {
		t.Fatalf("full instance port/prefix rejected: %v", err)
	}
}

func TestValidateGrantRejectsZeroProjectIDs(t *testing.T) {
	g := testGrant()
	g.ProjectID = "0"
	if err := gitcache.ValidateGrant(g); err == nil {
		t.Fatal("ProjectID 0 accepted (R4)")
	}
	g = testGrant()
	g.TargetProjectID = "0"
	if err := gitcache.ValidateGrant(g); err == nil {
		t.Fatal("TargetProjectID 0 accepted (R4)")
	}
	g = testGrant()
	g.SourceFork = "0"
	if err := gitcache.ValidateGrant(g); err == nil {
		t.Fatal("SourceFork 0 accepted (R4)")
	}
}
