//go:build linux || darwin

package gitcache

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/pack"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/tree"
)

// Contract tests for accepted R1–R11. On the initial frozen source they fail
// (red). After the consolidated repair they must pass (green).

func TestR1_NonemptyRootWithoutLedgerRefused(t *testing.T) {
	root := regressionRoot(t)
	if err := os.WriteFile(filepath.Join(root, "orphan.bin"), make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err == nil {
		t.Fatal("accepted nonempty root without ledger (R1)")
	}
}

func TestR1_SymlinkedLedgerRefused(t *testing.T) {
	root := regressionRoot(t)
	outside := filepath.Join(t.TempDir(), "outside-ledger")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "ledger")); err != nil {
		t.Fatal(err)
	}
	_, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err == nil {
		t.Fatal("accepted symlinked ledger via init overwrite (R1)")
	}
}

func TestR2_ServiceClosePinnedRetainsManager(t *testing.T) {
	root := regressionRoot(t)
	svc, err := OpenService(ServiceConfig{Enabled: true, Root: root, QuotaBytes: bounds.BrootBytes + bounds.GenerationCharge()*2})
	if err != nil {
		t.Fatal(err)
	}
	ip, h := tinyBlobPack(t)
	gen, err := svc.Manager().PublishGeneration(context.Background(), "g", "ns", h, h, h, ip)
	if err != nil {
		t.Fatal(err)
	}
	if err := gen.Pin(); err != nil {
		t.Fatal(err)
	}
	if err := svc.Close(context.Background()); !errors.Is(err, ErrPinned) {
		t.Fatalf("close pinned: %v", err)
	}
	if svc.Manager() == nil || !svc.Enabled() {
		t.Fatal("service dropped manager handle on pinned close (R2)")
	}
	_ = gen.Unpin()
	if err := svc.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestR2_DoublePinRequiresMatchingUnpin(t *testing.T) {
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(context.Background())
	ip, h := tinyBlobPack(t)
	gen, err := mgr.PublishGeneration(context.Background(), "g", "ns", h, h, h, ip)
	if err != nil {
		t.Fatal(err)
	}
	if err := gen.Pin(); err != nil {
		t.Fatal(err)
	}
	if err := gen.Pin(); err != nil {
		t.Fatal(err)
	}
	if err := gen.Unpin(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Close(context.Background()); !errors.Is(err, ErrPinned) {
		t.Fatalf("double-pin released by single unpin (R2): %v", err)
	}
	if err := gen.Unpin(); err != nil {
		t.Fatal(err)
	}
}

func TestR3_IndependentBaseNotDirectParent(t *testing.T) {
	blob := pack.Object{Type: "blob", Data: []byte("r3")}
	blob.Hash = pack.HashObject("blob", blob.Data)
	tb, err := tree.EncodeTree([]tree.TreeEntry{{Mode: tree.ModeFile, Name: "f", Hash: blob.Hash}})
	if err != nil {
		t.Fatal(err)
	}
	tr := pack.Object{Type: "tree", Data: tb, Hash: pack.HashObject("tree", tb)}
	baseBody := []byte("tree " + tr.Hash.String() + "\nauthor A <a@a> 1 +0000\ncommitter A <a@a> 1 +0000\n\nbase\n")
	base := pack.Object{Type: "commit", Data: baseBody, Hash: pack.HashObject("commit", baseBody)}
	midBody := []byte("tree " + tr.Hash.String() + "\nparent " + base.Hash.String() + "\nauthor A <a@a> 1 +0000\ncommitter A <a@a> 1 +0000\n\nmid\n")
	mid := pack.Object{Type: "commit", Data: midBody, Hash: pack.HashObject("commit", midBody)}
	headBody := []byte("tree " + tr.Hash.String() + "\nparent " + mid.Hash.String() + "\nauthor A <a@a> 1 +0000\ncommitter A <a@a> 1 +0000\n\nhead\n")
	head := pack.Object{Type: "commit", Data: headBody, Hash: pack.HashObject("commit", headBody)}
	objs := tree.Map{
		blob.Hash: blob, tr.Hash: tr, base.Hash: base, mid.Hash: mid, head.Hash: head,
	}
	// Accepted contract: independent role roots — base need not be a direct parent.
	if err := tree.ProveHead(context.Background(), objs, head.Hash, base.Hash, 2); err != nil {
		t.Fatalf("independent authorized base (ancestor, not direct parent) rejected (R3): %v", err)
	}
}

func TestR3_StartCommitAndTreeValidated(t *testing.T) {
	blob := pack.Object{Type: "blob", Data: []byte("start")}
	blob.Hash = pack.HashObject("blob", blob.Data)
	tb, err := tree.EncodeTree([]tree.TreeEntry{{Mode: tree.ModeFile, Name: "f", Hash: blob.Hash}})
	if err != nil {
		t.Fatal(err)
	}
	tr := pack.Object{Type: "tree", Data: tb, Hash: pack.HashObject("tree", tb)}
	headBody := []byte("tree " + tr.Hash.String() + "\nauthor A <a@a> 1 +0000\ncommitter A <a@a> 1 +0000\n\nhead\n")
	head := pack.Object{Type: "commit", Data: headBody, Hash: pack.HashObject("commit", headBody)}
	badStart := pack.Object{Type: "commit", Data: []byte("not-a-commit"), Hash: pack.HashObject("commit", []byte("not-a-commit"))}
	objs := tree.Map{blob.Hash: blob, tr.Hash: tr, head.Hash: head, badStart.Hash: badStart}
	g := completeTestGrant(Grant{
		OriginHost: "example.com", ProjectID: "1", ProjectPath: "g/p", SourceFork: "2",
		AuthDomain: "d", PolicyFP: "fp", MRIID: 1, MRVersion: "v",
		HeadSHA: head.Hash, BaseSHA: head.Hash, StartSHA: badStart.Hash, ActorID: "a",
		HTTPSURL: "https://example.com/g/p.git",
	})
	// Depth 2 materializes start independently; malformed start must fail closed.
	if err := proveGrant(context.Background(), objs, g, 2); err == nil {
		t.Fatal("malformed start commit accepted (R3)")
	}
}

func TestR6_LedgerTmpHardlinkRefused(t *testing.T) {
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(context.Background())
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret-outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(root, "ledger.tmp")
	if err := os.Link(outside, tmp); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(outside)
	ip, h := tinyBlobPack(t)
	_, _ = mgr.PublishGeneration(context.Background(), "g", "ns", h, h, h, ip)
	after, _ := os.ReadFile(outside)
	if string(before) != string(after) {
		t.Fatal("ledger.tmp hardlink clobbered outside file (R6)")
	}
}

func TestR7_DistinctObjectManifestCapacity(t *testing.T) {
	const n = 300
	objs := make([]pack.Object, 0, n)
	for i := 0; i < n; i++ {
		data := []byte{byte(i >> 8), byte(i), 'x'}
		o := pack.Object{Type: "blob", Data: data}
		o.Hash = pack.HashObject("blob", data)
		objs = append(objs, o)
	}
	raw, err := pack.Encode(objs)
	if err != nil {
		t.Fatal(err)
	}
	ip, err := pack.DecodeIndexed(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(context.Background())
	h := objs[0].Hash
	if _, err := mgr.PublishGeneration(context.Background(), "g", "ns", h, h, h, ip); err != nil {
		t.Fatalf("distinct-object publish failed under metadata budget (R7): %v", err)
	}
}

// Pre-cancel control: already passes on frozen source; not sufficient R8 proof.
func TestR8_PreCanceledAcquireControl(t *testing.T) {
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(context.Background())
	ip, head, base := commitIndexedPack(t)
	intent := AcquireIntent{
		ProjectID: "1", MRIID: 7, Depth: 2, Token: "t",
		ExpectedHead: head, ExpectedBase: base, ExpectedStart: base,
	}
	g := testGrantInternal()
	g.HeadSHA, g.BaseSHA, g.StartSHA = head, base, base
	g.MRVersion = MRVersionFromDiffRefs(head, base, base)
	intent.ExpectedMRVersion = g.MRVersion
	if intent.CAPath == "" {
		var err error
		intent, err = prepareAcquisitionTrust(context.Background(), g, intent)
		if err != nil {
			t.Fatal(err)
		}
		g.TrustProvenance = intent.trustFP
	} else {
		g.TrustProvenance = TrustProvenanceFromIntent(intent)
	}
	fp := GrantFingerprint(g)
	ns := NamespaceID(g.AuthDomain, g.CanonicalInstance, g.ProjectID, g.SourceFork)
	if _, err := mgr.PublishGeneration(context.Background(), fp, ns, head, base, base, ip); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	auth := StaticAuthorizer{Grant: g}
	_, err = mgr.Acquire(ctx, intent, auth)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled acquire control lost sentinel: %v", err)
	}
}

func TestR4_FingerprintBindsMRIIDPathCloneURLAndRefs(t *testing.T) {
	base := testGrantInternal()
	base.HeadRef = "refs/heads/feature"
	base.BaseRef = "refs/heads/main"
	base.StartRef = "refs/heads/main"
	fp0 := GrantFingerprint(base)
	cases := []struct {
		name string
		mut  func(*Grant)
	}{
		{"mriid", func(g *Grant) { g.MRIID = 99 }},
		{"project_path", func(g *Grant) { g.ProjectPath = "other/path" }},
		{"canonical_instance", func(g *Grant) { g.CanonicalInstance = "https://example.com:8443/prefix" }},
		{"source_https", func(g *Grant) { g.SourceHTTPSURL = "https://example.com/other/path.git" }},
		{"target_https", func(g *Grant) { g.TargetHTTPSURL = "https://example.com/tgt/other.git" }},
		{"trust_provenance", func(g *Grant) { g.TrustProvenance = "changed-trust" }},
		{"head_ref", func(g *Grant) { g.HeadRef = "refs/heads/other" }},
		{"base_ref", func(g *Grant) { g.BaseRef = "refs/heads/develop" }},
	}
	for _, tc := range cases {
		g := base
		tc.mut(&g)
		if GrantFingerprint(g) == fp0 {
			t.Fatalf("fingerprint ignores %s (R4)", tc.name)
		}
	}
}

func TestR5_MissingIndexRefusesWarm(t *testing.T) {
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(context.Background())
	ip, h := tinyBlobPack(t)
	gen, err := mgr.PublishGeneration(context.Background(), "gfp-r5", "ns", h, h, h, ip)
	if err != nil {
		t.Fatal(err)
	}
	if warm, err := mgr.lookupWarm(context.Background(), "gfp-r5", "ns"); err != nil {
		t.Fatalf("baseline warm: %v", err)
	} else if err := warm.Unpin(); err != nil {
		t.Fatal(err)
	}
	idxPath := filepath.Join(root, "generations", gen.ID, "pack.idx")
	if err := os.Remove(idxPath); err != nil {
		t.Fatal(err)
	}
	if warm, err := mgr.lookupWarm(context.Background(), "gfp-r5", "ns"); err == nil {
		t.Fatalf("missing pack.idx accepted as warm (R5): id=%s", warm.ID)
	}
}

func TestR5_ManifestRootAndObjectSetIntegrity(t *testing.T) {
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(context.Background())
	ip, head, base := commitIndexedPack(t)
	gen, err := mgr.PublishGeneration(context.Background(), "gfp-r5m", "ns", head, base, base, ip)
	if err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(root, "generations", gen.ID, "manifest.json")
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	meta["head"] = plumbing.NewHash("ffffffffffffffffffffffffffffffffffffffff").String()
	meta["objects"] = map[string]string{}
	rewritten, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
	if warm, err := mgr.lookupWarm(context.Background(), "gfp-r5m", "ns"); err == nil {
		t.Fatalf("altered manifest head/empty object set accepted (R5): id=%s", warm.ID)
	}
}

func TestR8_MidFetchCancelPreservesTypedSentinel(t *testing.T) {
	ip, head, base := commitIndexedPack(t)
	caPEM, serverCert := acquireTestServerCert(t)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	const token = "r8-mid-cancel"
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "oauth2" || pass != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if strings.Contains(r.URL.Path, "/info/refs") && r.URL.Query().Get("service") == "git-upload-pack" {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Second):
				return
			}
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			_ = writeShallowAdv(w, head, base)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		req := packp.NewUploadPackRequest()
		req.Depth = packp.DepthCommits(2)
		_ = req.Capabilities.Set(capability.Shallow)
		resp := packp.NewUploadPackResponseWithPackfile(req, io.NopCloser(bytes.NewReader(ip.PackBytes)))
		resp.Shallows = []plumbing.Hash{base}
		_ = resp.Encode(w)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(context.Background())

	var domain AuthDomain
	g := completeTestGrant(Grant{
		OriginHost: u.Hostname(), ProjectID: "1", ProjectPath: "g/p", SourceFork: "2",
		AuthDomain: domain.Bind("a", token), PolicyFP: "fp", MRIID: 7,
		MRVersion: MRVersionFromDiffRefs(head, base, base),
		HeadSHA:   head, BaseSHA: base, StartSHA: base, ActorID: "a",
		HTTPSURL: srv.URL + "/repo.git",
	})
	auth := StaticAuthorizer{Grant: g}
	intent := AcquireIntent{
		ProjectID: "1", MRIID: 7, Depth: 2, Token: token, CAPath: caPath,
		ExpectedHead: head, ExpectedBase: base, ExpectedStart: base,
		ExpectedMRVersion: g.MRVersion, AllowLoopback: true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := mgr.Acquire(ctx, intent, auth)
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("fetch did not reach advertisement")
	}
	cancel()
	err = <-errCh
	close(release)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-fetch cancel lost typed sentinel (R8): %v", err)
	}
}

func TestR8_MidFetchDeadlinePreservesTypedSentinel(t *testing.T) {
	_, head, base := commitIndexedPack(t)
	caPEM, serverCert := acquireTestServerCert(t)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	const token = "r8-mid-deadline"
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "oauth2" || pass != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if strings.Contains(r.URL.Path, "/info/refs") {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Second):
				return
			}
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			_ = writeShallowAdv(w, head, base)
			return
		}
		http.NotFound(w, r)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(context.Background())

	var domain AuthDomain
	g := completeTestGrant(Grant{
		OriginHost: u.Hostname(), ProjectID: "1", ProjectPath: "g/p", SourceFork: "2",
		AuthDomain: domain.Bind("a", token), PolicyFP: "fp", MRIID: 7,
		MRVersion: MRVersionFromDiffRefs(head, base, base),
		HeadSHA:   head, BaseSHA: base, StartSHA: base, ActorID: "a",
		HTTPSURL: srv.URL + "/repo.git",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err = mgr.Acquire(ctx, AcquireIntent{
		ProjectID: "1", MRIID: 7, Depth: 2, Token: token, CAPath: caPath,
		ExpectedHead: head, ExpectedBase: base, ExpectedStart: base,
		ExpectedMRVersion: g.MRVersion, AllowLoopback: true,
	}, StaticAuthorizer{Grant: g})
	close(release)
	select {
	case <-started:
	default:
		t.Fatal("deadline test never reached advertisement")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("mid-fetch deadline lost typed sentinel (R8): %v", err)
	}
}

func TestR9_ServiceConfigCAPathAppliedOnAcquire(t *testing.T) {
	ip, head, base := commitIndexedPack(t)
	caPEM, serverCert := acquireTestServerCert(t)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	const token = "r9-svc-ca"
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "oauth2" || pass != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "/info/refs") && r.URL.Query().Get("service") == "git-upload-pack":
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			_ = writeShallowAdv(w, head, base)
		case strings.HasSuffix(r.URL.Path, "/git-upload-pack") && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			req := packp.NewUploadPackRequest()
			req.Depth = packp.DepthCommits(2)
			_ = req.Capabilities.Set(capability.Shallow)
			resp := packp.NewUploadPackResponseWithPackfile(req, io.NopCloser(bytes.NewReader(ip.PackBytes)))
			resp.Shallows = []plumbing.Hash{base}
			_ = resp.Encode(w)
		default:
			http.NotFound(w, r)
		}
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	root := regressionRoot(t)
	svc, err := OpenService(ServiceConfig{
		Enabled:    true,
		Root:       root,
		QuotaBytes: bounds.BrootBytes + bounds.GenerationCharge()*4,
		CAPath:     caPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close(context.Background())

	var domain AuthDomain
	g := completeTestGrant(Grant{
		OriginHost: u.Hostname(), ProjectID: "1", ProjectPath: "g/p", SourceFork: "2",
		AuthDomain: domain.Bind("a", token), PolicyFP: "fp", MRIID: 7,
		MRVersion: MRVersionFromDiffRefs(head, base, base),
		HeadSHA:   head, BaseSHA: base, StartSHA: base, ActorID: "a",
		HTTPSURL: srv.URL + "/repo.git",
	})
	// Intent omits CAPath: ServiceConfig must supply it.
	_, err = svc.Acquire(context.Background(), AcquireIntent{
		ProjectID: "1", MRIID: 7, Depth: 2, Token: token,
		ExpectedHead: head, ExpectedBase: base, ExpectedStart: base,
		ExpectedMRVersion: g.MRVersion, AllowLoopback: true,
	}, StaticAuthorizer{Grant: g})
	if err != nil {
		t.Fatalf("ServiceConfig CAPath not applied on Acquire (R9): %v", err)
	}
}

func TestR2_AcquireJoinsBeforeCloseUnlock(t *testing.T) {
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	blocked := make(chan struct{})
	auth := StaticAuthorizer{Grant: testGrantInternal(), Allow: func(AcquireIntent) error {
		close(started)
		select {
		case <-blocked:
		case <-time.After(5 * time.Second):
		}
		return errors.New("hold")
	}}
	errCh := make(chan error, 1)
	go func() {
		_, err := mgr.Acquire(context.Background(), AcquireIntent{ProjectID: "1", MRIID: 7, Depth: 1, Token: "t"}, auth)
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("acquire did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	closeErr := mgr.Close(ctx)
	close(blocked)
	<-errCh
	if closeErr == nil {
		t.Fatal("Close returned nil while acquisition still running (R2)")
	}
}

func TestR6_CheckRegularRejectsWorldReadable(t *testing.T) {
	root := regressionRoot(t)
	f, err := os.OpenFile(filepath.Join(root, "world"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	_, dev, err := walkRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkRegular(&st, dev); err == nil {
		t.Fatal("world-readable regular file accepted (R6)")
	}
}

func TestR1_EmptyRootInitAndUnknownFileRefused(t *testing.T) {
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stray"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2); err == nil {
		t.Fatal("unknown root file accepted on reopen (R1)")
	}
}

func TestR5_WarmRequiresIntactIndexAndRoots(t *testing.T) {
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(context.Background())
	ip, head, base := commitIndexedPack(t)
	gen, err := mgr.PublishGeneration(context.Background(), "gfp-ok", "ns", head, base, base, ip)
	if err != nil {
		t.Fatal(err)
	}
	warm, err := mgr.lookupWarm(context.Background(), "gfp-ok", "ns")
	if err != nil || warm == nil || warm.ID != gen.ID {
		t.Fatalf("intact generation warm: %v %#v", err, warm)
	}
	defer warm.Unpin()
	if warm.IndexSize <= 0 || warm.PackSize <= 0 {
		t.Fatalf("warm omitted size identity: %#v", warm)
	}
}

func TestR2_SecondAcquireRefusedWhileBusy(t *testing.T) {
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	auth := StaticAuthorizer{Grant: testGrantInternal(), Allow: func(AcquireIntent) error {
		close(started)
		<-release
		return errors.New("hold")
	}}
	errCh := make(chan error, 1)
	go func() {
		_, err := mgr.Acquire(context.Background(), AcquireIntent{ProjectID: "1", MRIID: 7, Depth: 1, Token: "t"}, auth)
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first acquire did not start")
	}
	_, err = mgr.Acquire(context.Background(), AcquireIntent{ProjectID: "1", MRIID: 7, Depth: 1, Token: "t"}, StaticAuthorizer{Grant: testGrantInternal()})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent acquire want busy, got %v", err)
	}
	close(release)
	<-errCh
}

func TestR3_TargetBaseFetchedWhenAbsentFromSourcePack(t *testing.T) {
	// Source pack has only head; base lives only in a second "target" pack/URL.
	blob := pack.Object{Type: "blob", Data: []byte("r3-target")}
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
	sourceRaw, err := pack.Encode([]pack.Object{blob, tr, head}) // deliberately omit base
	if err != nil {
		t.Fatal(err)
	}
	targetRaw, err := pack.Encode([]pack.Object{blob, tr, base})
	if err != nil {
		t.Fatal(err)
	}
	caPEM, serverCert := acquireTestServerCert(t)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	const token = "r3-target-root"
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "oauth2" || pass != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		isTarget := strings.Contains(r.URL.Path, "/target.git")
		packBytes := sourceRaw
		advHead, advBase := head.Hash, head.Hash
		if isTarget {
			packBytes = targetRaw
			advHead, advBase = base.Hash, base.Hash
		}
		switch {
		case strings.Contains(r.URL.Path, "/info/refs"):
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			_ = writeShallowAdv(w, advHead, advBase)
		case strings.HasSuffix(r.URL.Path, "/git-upload-pack"):
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			req := packp.NewUploadPackRequest()
			req.Depth = packp.DepthCommits(2)
			_ = req.Capabilities.Set(capability.Shallow)
			resp := packp.NewUploadPackResponseWithPackfile(req, io.NopCloser(bytes.NewReader(packBytes)))
			resp.Shallows = []plumbing.Hash{advBase}
			_ = resp.Encode(w)
		default:
			http.NotFound(w, r)
		}
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*4)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(context.Background())
	g := completeTestGrant(Grant{
		OriginHost: u.Hostname(), ProjectID: "1", ProjectPath: "g/p", SourceFork: "2",
		TargetProjectID: "1", SourcePath: "source", TargetPath: "target",
		AuthDomain: "d", PolicyFP: "fp", MRIID: 7,
		MRVersion: MRVersionFromDiffRefs(head.Hash, base.Hash, base.Hash),
		HeadSHA:   head.Hash, BaseSHA: base.Hash, StartSHA: base.Hash, ActorID: "a",
		SourceHTTPSURL: srv.URL + "/source.git",
		TargetHTTPSURL: srv.URL + "/target.git",
		HTTPSURL:       srv.URL + "/source.git",
	})
	_, err = mgr.Acquire(context.Background(), AcquireIntent{
		ProjectID: "1", MRIID: 7, Depth: 2, Token: token, CAPath: caPath,
		ExpectedHead: head.Hash, ExpectedBase: base.Hash, ExpectedStart: base.Hash,
		ExpectedMRVersion: g.MRVersion, AllowLoopback: true,
	}, StaticAuthorizer{Grant: g})
	if err != nil {
		t.Fatalf("target-absent-from-source acquisition: %v", err)
	}
}

func TestR3_SourceOnlyURLMissesTargetBaseFailClosed(t *testing.T) {
	// Base absent from source fork; Grant wrongly binds TargetHTTPSURL to source.
	// Role acquisition must fail closed (cannot invent target roots from the fork).
	blob := pack.Object{Type: "blob", Data: []byte("r3-neg")}
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
	sourceRaw, err := pack.Encode([]pack.Object{blob, tr, head}) // omit base
	if err != nil {
		t.Fatal(err)
	}
	caPEM, serverCert := acquireTestServerCert(t)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	const token = "r3-source-only-neg"
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "oauth2" || pass != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "/info/refs"):
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			_ = writeShallowAdv(w, head.Hash, head.Hash) // base not advertised on fork
		case strings.HasSuffix(r.URL.Path, "/git-upload-pack"):
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			req := packp.NewUploadPackRequest()
			req.Depth = packp.DepthCommits(2)
			_ = req.Capabilities.Set(capability.Shallow)
			resp := packp.NewUploadPackResponseWithPackfile(req, io.NopCloser(bytes.NewReader(sourceRaw)))
			resp.Shallows = []plumbing.Hash{head.Hash}
			_ = resp.Encode(w)
		default:
			http.NotFound(w, r)
		}
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*4)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(context.Background())
	srcURL := srv.URL + "/source.git"
	g := completeTestGrant(Grant{
		OriginHost: u.Hostname(), ProjectID: "1", ProjectPath: "g/p", SourceFork: "2",
		TargetProjectID: "1", SourcePath: "source", TargetPath: "target",
		AuthDomain: "d", PolicyFP: "fp", MRIID: 7,
		MRVersion: MRVersionFromDiffRefs(head.Hash, base.Hash, base.Hash),
		HeadSHA:   head.Hash, BaseSHA: base.Hash, StartSHA: base.Hash, ActorID: "a",
		SourceHTTPSURL: srcURL,
		TargetHTTPSURL: srcURL, // negative: target role incorrectly bound to fork
		HTTPSURL:       srcURL,
	})
	_, err = mgr.Acquire(context.Background(), AcquireIntent{
		ProjectID: "1", MRIID: 7, Depth: 2, Token: token, CAPath: caPath,
		ExpectedHead: head.Hash, ExpectedBase: base.Hash, ExpectedStart: base.Hash,
		ExpectedMRVersion: g.MRVersion, AllowLoopback: true,
	}, StaticAuthorizer{Grant: g})
	if err == nil {
		t.Fatal("target root absent from source fork accepted via source-only URL (R3)")
	}
}

func TestR4_HostnameOnlyCanonicalInstanceRejected(t *testing.T) {
	g := testGrantInternal()
	g.CanonicalInstance = g.OriginHost
	if err := ValidateGrant(g); err == nil {
		t.Fatal("hostname-only CanonicalInstance accepted (R4)")
	}
	g = testGrantInternal()
	g.CanonicalInstance = "https://" + g.OriginHost + ":8443/gitlab/api/v4"
	fp0 := GrantFingerprint(g)
	g2 := g
	g2.CanonicalInstance = "https://" + g.OriginHost + "/api/v4"
	if GrantFingerprint(g2) == fp0 {
		t.Fatal("fingerprint ignores instance port/prefix (R4)")
	}
}

func TestR4_TrustProvenanceChangeMissesWarm(t *testing.T) {
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(context.Background())
	ip, head, base := commitIndexedPack(t)
	intent := AcquireIntent{ProjectID: "1", MRIID: 7, Depth: 2, Token: "t", CAPath: "/ca/a.pem"}
	g := testGrantInternal()
	g.HeadSHA, g.BaseSHA, g.StartSHA = head, base, base
	g.MRVersion = MRVersionFromDiffRefs(head, base, base)
	intent.ExpectedMRVersion = g.MRVersion
	intent.ExpectedHead, intent.ExpectedBase, intent.ExpectedStart = head, base, base
	if intent.CAPath == "" {
		var err error
		intent, err = prepareAcquisitionTrust(context.Background(), g, intent)
		if err != nil {
			t.Fatal(err)
		}
		g.TrustProvenance = intent.trustFP
	} else {
		g.TrustProvenance = TrustProvenanceFromIntent(intent)
	}
	fp := GrantFingerprint(g)
	ns := NamespaceID(g.AuthDomain, g.CanonicalInstance, g.ProjectID, g.SourceFork)
	if _, err := mgr.PublishGeneration(context.Background(), fp, ns, head, base, base, ip); err != nil {
		t.Fatal(err)
	}
	// Same SHAs/instance but changed CA path must not warm-hit.
	intent2 := intent
	intent2.CAPath = "/ca/b.pem"
	g2 := g
	g2.TrustProvenance = TrustProvenanceFromIntent(intent2)
	if warm, err := mgr.lookupWarm(context.Background(), GrantFingerprint(g2), ns); err == nil && warm != nil {
		t.Fatal("trust provenance change reused warm generation (R4)")
	}
}

func TestR8_WarmProvePreservesCancel(t *testing.T) {
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(context.Background())
	ip, head, base := commitIndexedPack(t)
	intent := AcquireIntent{ProjectID: "1", MRIID: 7, Depth: 2, Token: "t"}
	g := testGrantInternal()
	g.HeadSHA, g.BaseSHA, g.StartSHA = head, base, base
	g.MRVersion = MRVersionFromDiffRefs(head, base, base)
	intent.ExpectedMRVersion = g.MRVersion
	intent.ExpectedHead, intent.ExpectedBase, intent.ExpectedStart = head, base, base
	if intent.CAPath == "" {
		var err error
		intent, err = prepareAcquisitionTrust(context.Background(), g, intent)
		if err != nil {
			t.Fatal(err)
		}
		g.TrustProvenance = intent.trustFP
	} else {
		g.TrustProvenance = TrustProvenanceFromIntent(intent)
	}
	fp := GrantFingerprint(g)
	ns := NamespaceID(g.AuthDomain, g.CanonicalInstance, g.ProjectID, g.SourceFork)
	if _, err := mgr.PublishGeneration(context.Background(), fp, ns, head, base, base, ip); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	var n int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	auth := StaticAuthorizer{Grant: g, Allow: func(AcquireIntent) error {
		if atomic.AddInt32(&n, 1) == 2 {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}}
	errCh := make(chan error, 1)
	go func() {
		_, err := mgr.Acquire(ctx, intent, auth)
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("warm reauth never reached")
	}
	cancel()
	err = <-errCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("warm/reauth path lost cancel (R8): %v", err)
	}
}
