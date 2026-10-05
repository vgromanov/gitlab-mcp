//go:build linux || darwin

package gitcache

import (
	"context"
	"errors"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/pack"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/tree"
)

func TestParseSHAAndMRVersion(t *testing.T) {
	h, err := ParseSHA("2d132b02f44f8995d58d2909a9e1d8cfe929c37e")
	if err != nil || h == plumbing.ZeroHash {
		t.Fatal(err)
	}
	if _, err := ParseSHA("short"); err == nil {
		t.Fatal("short sha")
	}
	b := plumbing.NewHash("57da8a2a939bc8e28d1c3c3bcf91a93c27102cf2")
	v := MRVersionFromDiffRefs(h, b, b)
	if v == "" || v == h.String() {
		t.Fatalf("version %q", v)
	}
}

func TestSelectURLOriginHost(t *testing.T) {
	g := testGrantInternal()
	u, err := selectURL("https", g, false)
	if err != nil || u != g.SourceHTTPSURL {
		t.Fatalf("https: %q %v", u, err)
	}
	g.SourceHTTPSURL = "https://evil.example/x.git"
	g.HTTPSURL = g.SourceHTTPSURL
	if _, err := selectURL("https", g, false); !errors.Is(err, ErrAuthz) {
		t.Fatalf("host mismatch: %v", err)
	}
	g = testGrantInternal()
	g.SourceSSHURL = "ssh://git@example.com/u/fork.git"
	g.SSHURL = g.SourceSSHURL
	u, err = selectURL("ssh", g, false)
	if err != nil || u != g.SourceSSHURL {
		t.Fatalf("ssh: %q %v", u, err)
	}
	if _, err := selectURL("file", g, false); !errors.Is(err, ErrScheme) {
		t.Fatalf("scheme: %v", err)
	}
	g = testGrantInternal()
	g.OriginHost = "127.0.0.1"
	g.CanonicalInstance = "https://127.0.0.1:8443/api/v4"
	g.SourceHTTPSURL = "https://127.0.0.1:8443/u/fork.git"
	g.HTTPSURL = g.SourceHTTPSURL
	if _, err := selectURL("https", g, false); err == nil {
		t.Fatal("loopback without AllowLoopback")
	}
	if u, err = selectURL("https", g, true); err != nil || u != g.SourceHTTPSURL {
		t.Fatalf("loopback allowed: %q %v", u, err)
	}
}

func TestFSHelpersRejectBadComponents(t *testing.T) {
	if componentOK(".") || componentOK("..") || componentOK("a/b") || componentOK("") {
		t.Fatal("componentOK")
	}
	if _, err := splitRel("../x"); err == nil {
		t.Fatal("splitRel parent")
	}
	parts, err := splitRel("a/b")
	if err != nil || len(parts) != 2 {
		t.Fatalf("splitRel: %v %v", parts, err)
	}
}

func TestFetchObjectsRejectsBadDepthAndWants(t *testing.T) {
	if _, err := FetchObjects(context.Background(), "https://example.com/r.git", FetchOptions{Depth: 0, Wants: []plumbing.Hash{plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}, Token: "t"}); !errors.Is(err, ErrLimit) {
		t.Fatalf("depth0: %v", err)
	}
	if _, err := FetchObjects(context.Background(), "https://example.com/r.git", FetchOptions{Depth: 1, Wants: nil, Token: "t"}); !errors.Is(err, ErrLimit) {
		t.Fatalf("nowants: %v", err)
	}
}

func TestServiceDisabledAndDomain(t *testing.T) {
	svc, err := OpenService(ServiceConfig{Enabled: false})
	if err != nil || svc.Enabled() {
		t.Fatal(err)
	}
	if svc.Manager() != nil {
		t.Fatal("manager")
	}
	if svc.Domain() == nil {
		t.Fatal("domain")
	}
	if _, err := svc.Acquire(context.Background(), AcquireIntent{ProjectID: "1", MRIID: 1, Depth: 1}, StaticAuthorizer{}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled acquire: %v", err)
	}
	_ = svc.Close(context.Background())
}

func TestWarmAcquireProveGrant(t *testing.T) {
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(context.Background())

	ip, head, base := commitIndexedPack(t)
	var domain AuthDomain
	actor := "42"
	token := "tok"
	authDomain, err := domain.Bind(actor, token)
	if err != nil {
		t.Fatal(err)
	}
	intent := AcquireIntent{
		ProjectID: "1", MRIID: 7, Depth: 2, Token: token,
		ExpectedHead: head, ExpectedBase: base, ExpectedStart: base,
	}
	g := completeTestGrant(Grant{
		OriginHost:  "example.com",
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
		HTTPSURL:    "https://example.com/g/p.git",
	})
	intent.ExpectedMRVersion = g.MRVersion
	intent, err = prepareAcquisitionTrust(context.Background(), g, intent)
	if err != nil {
		t.Fatal(err)
	}
	g.TrustProvenance = intent.trustFP
	if _, err := mgr.PublishGeneration(context.Background(), GrantFingerprint(g), NamespaceID(authDomain, g.CanonicalInstance, g.ProjectID, g.SourceFork), head, base, base, ip); err != nil {
		t.Fatal(err)
	}
	auth := StaticAuthorizer{Grant: g}
	res, err := mgr.Acquire(context.Background(), intent, auth)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Warm || res.Objects == 0 {
		t.Fatalf("warm result %#v", res)
	}
}

func TestProveGrantDirect(t *testing.T) {
	ip, head, base := commitIndexedPack(t)
	g := completeTestGrant(Grant{
		OriginHost: "example.com", ProjectID: "1", ProjectPath: "g/p", SourceFork: "2",
		AuthDomain: "d", PolicyFP: "fp", MRIID: 1, MRVersion: "v",
		HeadSHA: head, BaseSHA: base, StartSHA: base, ActorID: "a",
		HTTPSURL: "https://example.com/g/p.git",
	})
	if err := proveGrant(context.Background(), tree.Map(ip.Objects), g, 2); err != nil {
		t.Fatal(err)
	}
	if err := proveGrant(context.Background(), tree.Map(ip.Objects), g, 1); err != nil {
		t.Fatal(err)
	}
}

func commitIndexedPack(t *testing.T) (pack.IndexedPack, plumbing.Hash, plumbing.Hash) {
	t.Helper()
	blob := pack.Object{Type: "blob", Data: []byte("cov")}
	blob.Hash = pack.HashObject("blob", blob.Data)
	tb, err := tree.EncodeTree([]tree.TreeEntry{{Mode: tree.ModeFile, Name: "f", Hash: blob.Hash}})
	if err != nil {
		t.Fatal(err)
	}
	tr := pack.Object{Type: "tree", Data: tb, Hash: pack.HashObject("tree", tb)}
	baseBody := []byte("tree " + tr.Hash.String() + "\nauthor A <a@a> 1 +0000\ncommitter A <a@a> 1 +0000\n\nbase\n")
	base := pack.Object{Type: "commit", Data: baseBody, Hash: pack.HashObject("commit", baseBody)}
	headBody := []byte("tree " + tr.Hash.String() + "\nparent " + base.Hash.String() + "\nauthor A <a@a> 1 +0000\ncommitter A <a@a> 1 +0000\n\nhead\n")
	head := pack.Object{Type: "commit", Data: headBody, Hash: pack.HashObject("commit", headBody)}
	raw, err := pack.Encode([]pack.Object{blob, tr, base, head})
	if err != nil {
		t.Fatal(err)
	}
	ip, err := pack.DecodeIndexed(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	return ip, head.Hash, base.Hash
}

func TestFetchResultAccessors(t *testing.T) {
	ip, _, _ := commitIndexedPack(t)
	fr := FetchResult{Indexed: ip}
	if fr.Objects() == nil || fr.PackBytes() == nil {
		t.Fatal("accessors")
	}
	if len(fr.Objects()) != len(ip.Objects) || len(fr.PackBytes()) != ip.PackByteCount {
		t.Fatal("alias")
	}
}

func TestValidateGrantEdges(t *testing.T) {
	g := testGrantInternal()
	if err := ValidateGrant(g); err != nil {
		t.Fatal(err)
	}
	g.SourceFork = ""
	if err := ValidateGrant(g); err == nil {
		t.Fatal("empty fork")
	}
	g = testGrantInternal()
	g.HTTPSURL = ""
	g.SSHURL = ""
	g.SourceHTTPSURL = ""
	g.SourceSSHURL = ""
	g.TargetHTTPSURL = ""
	g.TargetSSHURL = ""
	if err := ValidateGrant(g); err == nil {
		t.Fatal("no urls")
	}
}

func TestCheckDirAndRegular(t *testing.T) {
	root := regressionRoot(t)
	f, dev, err := walkRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := fstat(int(f.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkDir(&st, dev); err != nil {
		t.Fatal(err)
	}
	if err := checkDir(&st, dev+1); err == nil {
		t.Fatal("dev mismatch")
	}
}
