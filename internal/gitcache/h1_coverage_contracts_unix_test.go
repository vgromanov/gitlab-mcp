//go:build linux || darwin

package gitcache

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
)

func TestCoverage_LedgerSemanticsRejectsAccountingInvalid(t *testing.T) {
	quota := uint64(bounds.BrootBytes) + 1000
	base := func() (ledgerHeader, []Slot) {
		return ledgerHeader{Quota: quota}, make([]Slot, slotCount)
	}
	// Quota below broot.
	if err := validateLedgerSemantics(ledgerHeader{Quota: uint64(bounds.BrootBytes) - 1}, make([]Slot, slotCount)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("low quota: %v", err)
	}
	h, slots := base()
	// Dirty empty slot.
	slots[0].Flags = 1
	if err := validateLedgerSemantics(h, slots); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("dirty empty: %v", err)
	}
	h, slots = base()
	slots[0] = Slot{State: SlotReserved, Charge: 100, ID: sha256.Sum256([]byte("a"))}
	slots[1] = Slot{State: SlotReserved, Charge: 100, ID: slots[0].ID}
	if err := validateLedgerSemantics(h, slots); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("duplicate id: %v", err)
	}
	h, slots = base()
	slots[0] = Slot{State: SlotReserved, Charge: 100} // zero ID
	if err := validateLedgerSemantics(h, slots); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("zero id: %v", err)
	}
	h, slots = base()
	slots[0] = Slot{State: SlotCommitted, Charge: 200, PackSize: 10, IndexSize: 10, MetaSize: 1, ID: sha256.Sum256([]byte("c"))}
	if err := validateLedgerSemantics(h, slots); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("committed undersized: %v", err)
	}
	charge := uint64(32 + 1072 + 1)
	h, slots = base()
	h.Quota = uint64(bounds.BrootBytes) + charge
	slots[0] = Slot{
		State: SlotCommitted, Charge: charge, PackSize: 32, IndexSize: 1072, MetaSize: 1,
		ID: sha256.Sum256([]byte("ok")), ManifestDigest: sha256.Sum256([]byte("m")),
	}
	if err := validateLedgerSemantics(h, slots); err != nil {
		t.Fatalf("valid committed: %v", err)
	}
	// Total charge exceeds remaining quota (broot+total > quota).
	h, slots = base()
	slots[0] = Slot{State: SlotReserved, Charge: 1000, ID: sha256.Sum256([]byte("q"))}
	h.Quota = uint64(bounds.BrootBytes) + 10
	if err := validateLedgerSemantics(h, slots); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("over quota: %v", err)
	}
	if _, err := chargedTotal([]Slot{{State: SlotReserved, Charge: 0}}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("charged zero: %v", err)
	}
	if _, err := parseSlot(make([]byte, 3)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("short slot: %v", err)
	}
}

func TestCoverage_ServiceNilAndTokenContracts(t *testing.T) {
	var nilSvc *Service
	if nilSvc.Enabled() || nilSvc.Manager() != nil || nilSvc.Domain() != nil {
		t.Fatal("nil service surface leaked")
	}
	if err := nilSvc.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := nilSvc.Acquire(context.Background(), AcquireIntent{}, StaticAuthorizer{}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("nil acquire: %v", err)
	}
	disabled, err := OpenService(ServiceConfig{Enabled: false})
	if err != nil || disabled.Enabled() {
		t.Fatalf("disabled: %v", err)
	}
	if _, err := disabled.Acquire(context.Background(), AcquireIntent{}, StaticAuthorizer{}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled acquire: %v", err)
	}
	root := regressionRoot(t)
	svc, err := OpenService(ServiceConfig{
		Enabled: true, Root: root, QuotaBytes: bounds.BrootBytes + bounds.GenerationCharge()*2,
		Token: "service-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close(context.Background())
	auth := StaticAuthorizer{Grant: testGrantInternal()}
	intent := AcquireIntent{ProjectID: "1", MRIID: 7, Depth: 1, Token: "other-token"}
	if _, err := svc.Acquire(context.Background(), intent, auth); !errors.Is(err, ErrAuthz) {
		t.Fatalf("token mismatch: %v", err)
	}
	if _, err := OpenService(ServiceConfig{Enabled: true, Root: filepath.Join(root, "missing-child"), QuotaBytes: bounds.BrootBytes + bounds.GenerationCharge()}); err == nil {
		t.Fatal("open with missing root accepted")
	}
}

func TestCoverage_AuthzCanonicalAndCloneAndProvenance(t *testing.T) {
	if canonicalInstanceOK("example.com", "example.com") {
		t.Fatal("hostname-only accepted")
	}
	if canonicalInstanceOK("ftp://example.com/api/v4", "example.com") {
		t.Fatal("ftp scheme accepted")
	}
	if canonicalInstanceOK("https://user@example.com/api/v4", "example.com") {
		t.Fatal("userinfo instance accepted")
	}
	if canonicalInstanceOK("https://other.com/api/v4", "example.com") {
		t.Fatal("host mismatch accepted")
	}
	if !canonicalInstanceOK("https://example.com/api/v4", "example.com") {
		t.Fatal("valid instance rejected")
	}
	g := testGrantInternal()
	g.TargetHTTPSURL = ""
	g.TargetSSHURL = ""
	if err := ValidateGrant(g); !errors.Is(err, ErrAuthz) {
		t.Fatalf("missing target URL: %v", err)
	}
	g = testGrantInternal()
	g.MRIID = 0
	if err := ValidateGrant(g); !errors.Is(err, ErrAuthz) {
		t.Fatalf("mriid: %v", err)
	}
	a := TrustProvenanceFromIntent(AcquireIntent{Transport: "ssh", CAPath: "/ca", Insecure: true, AllowedInsecureHost: "h"})
	b := TrustProvenanceFromIntent(AcquireIntent{Transport: "https", Insecure: false})
	if a == b {
		t.Fatal("provenance ignored insecure/transport")
	}
	c := TrustProvenanceFromIntent(AcquireIntent{Transport: "ssh", Insecure: true, AllowedInsecureHost: "h"})
	if a == c {
		t.Fatal("provenance ignored CAPath")
	}
	if _, err := ParseSHA("abcd"); err == nil {
		t.Fatal("short sha")
	}
	if err := ValidateCloneURL("ssh://git@example.com/group/repo.git", "https://example.com/api/v4", "group/repo", false); err != nil {
		t.Fatalf("ssh clone: %v", err)
	}
	if err := ValidateCloneURL("https://example.com/group/repo.git", "https://evil.com/api/v4", "group/repo", false); !errors.Is(err, ErrAuthz) {
		t.Fatalf("host mismatch clone: %v", err)
	}
	if err := matchExpected(AcquireIntent{ExpectedBase: plumbing.NewHash("1111111111111111111111111111111111111111")}, testGrantInternal()); !errors.Is(err, ErrAuthz) {
		t.Fatalf("expected base: %v", err)
	}
	if err := matchExpected(AcquireIntent{ExpectedStart: plumbing.NewHash("1111111111111111111111111111111111111111")}, testGrantInternal()); !errors.Is(err, ErrAuthz) {
		t.Fatalf("expected start: %v", err)
	}
	g = testGrantInternal()
	if err := AuthorizeObject(g, ObjectProof{Hash: g.HeadSHA, Type: "blob", Role: RootHead}); !errors.Is(err, ErrAuthz) {
		t.Fatalf("non-commit type: %v", err)
	}
	if err := AuthorizeObject(g, ObjectProof{Hash: g.HeadSHA, Type: "commit", Role: RootRole("bad")}); !errors.Is(err, ErrAuthz) {
		t.Fatalf("bad role: %v", err)
	}
	if err := AuthorizeObject(g, ObjectProof{Hash: g.BaseSHA, Type: "commit", Role: RootBase}); err != nil {
		t.Fatalf("base role: %v", err)
	}
	if err := AuthorizeObject(g, ObjectProof{Hash: g.StartSHA, Type: "commit", Role: RootStart}); err != nil {
		t.Fatalf("start role: %v", err)
	}
	if err := ValidateCloneURL("https://example.com/group/repo.git?x=1", "https://example.com/api/v4", "group/repo", false); !errors.Is(err, ErrAuthz) {
		t.Fatalf("query clone: %v", err)
	}
	if err := ValidateCloneURL("https://example.com/group/repo.git", "https://user@example.com/api/v4", "group/repo", false); !errors.Is(err, ErrAuthz) {
		t.Fatalf("instance userinfo: %v", err)
	}
	fp := GrantFingerprint(g)
	if fp == "" {
		t.Fatal("fingerprint empty")
	}
	if GrantFingerprint(g) != fp {
		t.Fatal("fingerprint not deterministic for identical grant")
	}
	mutated := g
	mutated.ProjectID = g.ProjectID + "-other"
	if GrantFingerprint(mutated) == fp {
		t.Fatal("fingerprint ignored meaningful grant identity mutation")
	}
	wantMR := g.HeadSHA.String() + ":" + g.BaseSHA.String() + ":" + g.StartSHA.String()
	if got := MRVersionFromDiffRefs(g.HeadSHA, g.BaseSHA, g.StartSHA); got != wantMR {
		t.Fatalf("MR version encoding: got %q want %q", got, wantMR)
	}
}

func TestCoverage_FSPathAndComponentContracts(t *testing.T) {
	if componentOK("") || componentOK(".") || componentOK("..") || componentOK("a/b") || !componentOK("ok") {
		t.Fatal("componentOK contract")
	}
	if componentOK("a\x00b") {
		t.Fatal("nul component accepted")
	}
	if _, err := splitRel(""); !errors.Is(err, ErrPath) {
		t.Fatalf("empty rel: %v", err)
	}
	if _, err := splitRel("../x"); !errors.Is(err, ErrPath) {
		t.Fatalf("dotdot: %v", err)
	}
	if _, err := splitRel(strings.Repeat("a/", bounds.MaxPathDepth+2) + "z"); !errors.Is(err, ErrPath) {
		t.Fatalf("deep rel: %v", err)
	}
	parts, err := splitRel("a/b")
	if err != nil || len(parts) != 2 {
		t.Fatalf("splitRel: %v %#v", err, parts)
	}
	if _, _, err := walkRoot(""); !errors.Is(err, ErrPath) {
		t.Fatalf("empty walk: %v", err)
	}
	if _, _, err := walkRoot("relative"); !errors.Is(err, ErrPath) {
		t.Fatalf("relative walk: %v", err)
	}
	if _, _, err := walkRoot("/"); !errors.Is(err, ErrPath) {
		t.Fatalf("root-only walk: %v", err)
	}
	tmp := t.TempDir()
	// world-writable or wrong mode root must be rejected by OpenManager/walkRoot.
	bad := filepath.Join(tmp, "badroot")
	if err := os.Mkdir(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenManager(bad, bounds.BrootBytes+bounds.GenerationCharge()); err == nil {
		t.Fatal("non-0700 root accepted")
	}
	// checkRegular: start from an otherwise-valid fixture so mode/nlink/device
	// negatives reach their named checks rather than an early device mismatch.
	valid := &syscall.Stat_t{Mode: syscall.S_IFREG | 0o600, Nlink: 1, Uid: uint32(os.Geteuid()), Dev: 1}
	if err := checkRegular(valid, 1); err != nil {
		t.Fatalf("valid regular rejected: %v", err)
	}
	modeBad := *valid
	modeBad.Mode = syscall.S_IFREG | 0o644
	if err := checkRegular(&modeBad, 1); !errors.Is(err, ErrPath) {
		t.Fatalf("644 regular: %v", err)
	}
	nlinkBad := *valid
	nlinkBad.Nlink = 2
	if err := checkRegular(&nlinkBad, 1); !errors.Is(err, ErrPath) {
		t.Fatalf("nlink>1: %v", err)
	}
	devBad := *valid
	devBad.Dev = 2
	if err := checkRegular(&devBad, 1); !errors.Is(err, ErrPath) {
		t.Fatalf("dev mismatch: %v", err)
	}
	dst := &syscall.Stat_t{Mode: syscall.S_IFDIR | 0o755, Uid: uint32(os.Geteuid()), Dev: 1}
	if err := checkDir(dst, 1); !errors.Is(err, ErrPath) {
		t.Fatalf("755 dir: %v", err)
	}
	notDir := *dst
	notDir.Mode = syscall.S_IFREG | 0o700
	if err := checkDir(&notDir, 1); !errors.Is(err, ErrPath) {
		t.Fatalf("file as dir: %v", err)
	}
}
