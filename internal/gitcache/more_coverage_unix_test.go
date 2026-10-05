//go:build linux || darwin

package gitcache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/pack"
)

func TestCloseCanceledAndPinnedRace(t *testing.T) {
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err != nil {
		t.Fatal(err)
	}
	ip, h := tinyBlobPack(t)
	gen, err := mgr.PublishGeneration(context.Background(), "g", "ns", h, h, h, ip)
	if err != nil {
		t.Fatal(err)
	}
	if err := gen.Pin(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Close(context.Background()); !errors.Is(err, ErrPinned) {
		t.Fatalf("pinned: %v", err)
	}
	_ = gen.Unpin()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Canceled close may succeed if wg is already idle, or return ErrCanceled.
	if err := mgr.Close(ctx); err != nil && !errors.Is(err, ErrCanceled) && !errors.Is(err, ErrClosed) {
		t.Fatalf("close: %v", err)
	}
}

func TestServiceEnabledAcquireAuthz(t *testing.T) {
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
	if !svc.Enabled() || svc.Manager() == nil || svc.Domain() == nil {
		t.Fatal("enabled surface")
	}
	if dom := svc.Domain().Bind("1", "tok"); dom == "" {
		t.Fatal("bind")
	}
	if _, err := svc.Acquire(context.Background(), AcquireIntent{ProjectID: "1", MRIID: 1, Depth: 1}, nil); !errors.Is(err, ErrAuthz) {
		t.Fatalf("nil auth: %v", err)
	}
	auth := StaticAuthorizer{Grant: testGrantInternal(), Allow: func(AcquireIntent) error { return ErrDenied }}
	if _, err := svc.Acquire(context.Background(), AcquireIntent{ProjectID: "1", MRIID: 7, Depth: 1, Token: "t"}, auth); !errors.Is(err, ErrDenied) {
		t.Fatalf("allow deny: %v", err)
	}
}

func TestStaticAuthorizerContextAndValidate(t *testing.T) {
	auth := StaticAuthorizer{Grant: testGrantInternal()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := auth.ResolveGrant(ctx, AcquireIntent{MRIID: 7, Depth: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx: %v", err)
	}
	g := testGrantInternal()
	g.ActorID = ""
	auth.Grant = g
	if _, err := auth.ResolveGrant(context.Background(), AcquireIntent{MRIID: 7, Depth: 1}); !errors.Is(err, ErrAuthz) {
		t.Fatalf("validate: %v", err)
	}
}

func TestAuthorizeObjectRoles(t *testing.T) {
	g := testGrantInternal()
	if err := AuthorizeObject(g, ObjectProof{Hash: g.HeadSHA, Type: "commit", Role: RootHead}); err != nil {
		t.Fatal(err)
	}
	if err := AuthorizeObject(g, ObjectProof{Hash: g.BaseSHA, Type: "blob", Role: RootBase}); !errors.Is(err, ErrAuthz) {
		t.Fatalf("type: %v", err)
	}
	if err := AuthorizeObject(g, ObjectProof{Hash: g.StartSHA, Type: "commit", Role: RootRole("nope")}); !errors.Is(err, ErrAuthz) {
		t.Fatalf("role: %v", err)
	}
}

func TestPublishRejectsEmptyIndexed(t *testing.T) {
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(context.Background())
	h := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if _, err := mgr.PublishGeneration(context.Background(), "g", "ns", h, h, h, pack.IndexedPack{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("empty ip: %v", err)
	}
}

func TestResolveBoundMismatch(t *testing.T) {
	g := testGrantInternal()
	auth := StaticAuthorizer{Grant: g}
	first, err := resolveBound(context.Background(), auth, AcquireIntent{MRIID: 7, Depth: 1}, Grant{})
	if err != nil {
		t.Fatal(err)
	}
	g2 := g
	g2.HeadSHA[0] ^= 0xff
	auth2 := StaticAuthorizer{Grant: g2}
	if _, err := resolveBound(context.Background(), auth2, AcquireIntent{MRIID: 7, Depth: 1}, first); !errors.Is(err, ErrAuthz) {
		t.Fatalf("mismatch: %v", err)
	}
}

func TestDisabledServiceCloseIdempotent(t *testing.T) {
	svc, err := OpenService(ServiceConfig{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := svc.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityGateBranches(t *testing.T) {
	if err := CapabilityGate(true, true, false, false); !errors.Is(err, ErrCapability) {
		t.Fatalf("shallow: %v", err)
	}
	if err := CapabilityGate(false, false, false, true); !errors.Is(err, ErrCapability) {
		t.Fatalf("reachable: %v", err)
	}
	if err := CapabilityGate(true, true, true, false); err != nil {
		t.Fatal(err)
	}
}
