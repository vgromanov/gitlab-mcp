//go:build linux || darwin

package gitcache

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/pack"
)

func regressionRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func tinyBlobPack(t *testing.T) (pack.IndexedPack, plumbing.Hash) {
	t.Helper()
	blob := pack.Object{Type: "blob", Data: []byte("regression-blob")}
	blob.Hash = pack.HashObject("blob", blob.Data)
	raw, err := pack.Encode([]pack.Object{blob})
	if err != nil {
		t.Fatal(err)
	}
	ip, err := pack.DecodeIndexed(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	return ip, blob.Hash
}

func TestCrashRecoveryRetainsAmbiguousCharge(t *testing.T) {
	root := regressionRoot(t)
	quota := bounds.BrootBytes + bounds.GenerationCharge()*2
	mgr, err := OpenManager(root, quota)
	if err != nil {
		t.Fatal(err)
	}
	ip, h := tinyBlobPack(t)
	mgr.SetFaultPoints(&FaultPoints{
		AfterStage: func() error { return errors.New("injected crash after stage") },
	})
	if _, err := mgr.PublishGeneration(context.Background(), "gfp", "ns", h, h, h, ip); err == nil {
		t.Fatal("expected injected crash")
	}
	if mgr.AmbiguousSlots() != 1 {
		t.Fatalf("ambiguous=%d", mgr.AmbiguousSlots())
	}
	charged, err := mgr.ChargedBytes()
	if err != nil || charged == 0 {
		t.Fatalf("charged=%d err=%v", charged, err)
	}
	if err := mgr.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Reopen: recovery must keep ambiguous charge (not treat as free miss).
	mgr2, err := OpenManager(root, quota)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr2.Close(context.Background())
	if mgr2.AmbiguousSlots() != 1 {
		t.Fatalf("reopen ambiguous=%d", mgr2.AmbiguousSlots())
	}
	charged2, err := mgr2.ChargedBytes()
	if err != nil || charged2 == 0 {
		t.Fatalf("reopen charged=%d err=%v", charged2, err)
	}
}

func TestQuotaRejectsBeforeOutputWrite(t *testing.T) {
	root := regressionRoot(t)
	// Only enough for root bookkeeping + one generation charge.
	quota := bounds.BrootBytes + bounds.GenerationCharge()
	mgr, err := OpenManager(root, quota)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(context.Background())
	ip, h := tinyBlobPack(t)
	if _, err := mgr.PublishGeneration(context.Background(), "gfp", "ns", h, h, h, ip); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	ip2, _ := tinyBlobPack(t)
	if _, err := mgr.PublishGeneration(context.Background(), "gfp2", "ns", h, h, h, ip2); !errors.Is(err, ErrQuota) {
		t.Fatalf("second publish want quota, got %v", err)
	}
}

func TestPinnedReaderBlocksClose(t *testing.T) {
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err != nil {
		t.Fatal(err)
	}
	ip, h := tinyBlobPack(t)
	gen, err := mgr.PublishGeneration(context.Background(), "gfp", "ns", h, h, h, ip)
	if err != nil {
		t.Fatal(err)
	}
	if err := gen.Pin(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Close(context.Background()); !errors.Is(err, ErrPinned) {
		t.Fatalf("close with pin: %v", err)
	}
	if err := gen.Unpin(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPublishCanceledContext(t *testing.T) {
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(context.Background())
	ip, h := tinyBlobPack(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := mgr.PublishGeneration(ctx, "gfp", "ns", h, h, h, ip); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled publish: %v", err)
	}
}

func TestAcquireCanceledBeforeResolve(t *testing.T) {
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(context.Background())
	h := plumbing.NewHash("2d132b02f44f8995d58d2909a9e1d8cfe929c37e")
	b := plumbing.NewHash("57da8a2a939bc8e28d1c3c3bcf91a93c27102cf2")
	auth := StaticAuthorizer{Grant: completeTestGrant(Grant{
		OriginHost: "example.com", ProjectID: "1", ProjectPath: "g/p", SourceFork: "2",
		AuthDomain: "dom", PolicyFP: "fp", MRIID: 7, MRVersion: "v",
		HeadSHA: h, BaseSHA: b, StartSHA: b, ActorID: "a",
		HTTPSURL: "https://example.com/g/p.git",
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = mgr.Acquire(ctx, AcquireIntent{ProjectID: "1", MRIID: 7, Depth: 1, Token: "t"}, auth)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire canceled: %v", err)
	}
}

func TestAcquireRejectsDepthZero(t *testing.T) {
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(context.Background())
	auth := StaticAuthorizer{Grant: testGrantInternal()}
	_, err = mgr.Acquire(context.Background(), AcquireIntent{ProjectID: "1", MRIID: 7, Depth: 0, Token: "t"}, auth)
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("depth0: %v", err)
	}
}

func TestServiceAcquireIsSingleEntry(t *testing.T) {
	root := regressionRoot(t)
	svc, err := OpenService(ServiceConfig{
		Enabled:    true,
		Root:       root,
		QuotaBytes: bounds.BrootBytes + bounds.GenerationCharge()*2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close(context.Background())
	auth := StaticAuthorizer{Grant: testGrantInternal()}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	// No warm content and no network: resolve succeeds then cold fetch fails closed.
	_, err = svc.Acquire(ctx, AcquireIntent{
		ProjectID: "1", MRIID: 7, Depth: 1, Token: "t",
		Transport: "https",
	}, auth)
	if err == nil {
		t.Fatal("unexpected warm/cold success without objects")
	}
}

func testGrantInternal() Grant {
	h := plumbing.NewHash("2d132b02f44f8995d58d2909a9e1d8cfe929c37e")
	b := plumbing.NewHash("57da8a2a939bc8e28d1c3c3bcf91a93c27102cf2")
	return completeTestGrant(Grant{
		OriginHost: "example.com", ProjectID: "1", ProjectPath: "g/p", SourceFork: "2",
		AuthDomain: "dom", PolicyFP: "fp", MRIID: 7, MRVersion: "v",
		HeadSHA: h, BaseSHA: b, StartSHA: b, ActorID: "a",
		HTTPSURL: "https://example.com/u/fork.git",
	})
}

// completeTestGrant fills role/instance fields required by ValidateGrant for hermetic fixtures.
func completeTestGrant(g Grant) Grant {
	if g.CanonicalInstance == "" && g.OriginHost != "" {
		g.CanonicalInstance = "https://" + g.OriginHost + "/api/v4"
	}
	if g.TargetProjectID == "" {
		g.TargetProjectID = g.ProjectID
	}
	if g.TargetPath == "" {
		g.TargetPath = g.ProjectPath
	}
	if g.SourceHTTPSURL == "" {
		g.SourceHTTPSURL = g.HTTPSURL
	}
	if g.SourceSSHURL == "" {
		g.SourceSSHURL = g.SSHURL
	}
	if g.TargetHTTPSURL == "" {
		// Hermetic default: same endpoint as source; production authorizer sets target URLs.
		g.TargetHTTPSURL = g.SourceHTTPSURL
	}
	if g.TargetSSHURL == "" {
		g.TargetSSHURL = g.SourceSSHURL
	}
	if g.HTTPSURL == "" {
		g.HTTPSURL = g.SourceHTTPSURL
	}
	if u, err := url.Parse(g.SourceHTTPSURL); err == nil && u.Host != "" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost") {
		g.CanonicalInstance = u.Scheme + "://" + u.Host + "/api/v4"
	}
	source := g.SourceHTTPSURL
	if source == "" {
		source = g.SourceSSHURL
	}
	target := g.TargetHTTPSURL
	if target == "" {
		target = g.TargetSSHURL
	}
	pathOf := func(raw string) string {
		u, _ := url.Parse(raw)
		if u == nil {
			return ""
		}
		return strings.TrimPrefix(strings.TrimSuffix(u.Path, ".git"), "/")
	}
	if g.SourcePath == "" {
		g.SourcePath = pathOf(source)
	}
	// Default target fixture endpoint is the source. Keep its exact fixture path.
	if g.TargetHTTPSURL == g.SourceHTTPSURL && g.TargetSSHURL == g.SourceSSHURL {
		g.TargetPath = pathOf(target)
	}
	if g.HeadRef == "" {
		g.HeadRef = "feature"
	}
	if g.BaseRef == "" {
		g.BaseRef = "main"
	}
	if g.StartRef == "" {
		g.StartRef = g.BaseRef
	}
	if g.TrustProvenance == "" {
		g.TrustProvenance = "test-trust"
	}
	return g
}
