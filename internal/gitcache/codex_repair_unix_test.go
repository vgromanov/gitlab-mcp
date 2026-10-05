//go:build linux || darwin

package gitcache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCodexRootInventoryRepeatAndUnknownOwnership(t *testing.T) {
	root := regressionRoot(t)
	m, e := OpenManager(root, bounds.DefaultQuotaBytes)
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 3; i++ {
		names, e := m.listRootNames()
		if e != nil || len(names) != 4 {
			t.Fatalf("scan %d: %v %v", i, names, e)
		}
	}
	if e := m.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"staging/unowned", "generations/unrecorded/pack.pack"} {
		t.Run(name, func(t *testing.T) {
			if e := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0700); e != nil {
				t.Fatal(e)
			}
			path := filepath.Join(root, name)
			if e := os.WriteFile(path, []byte("unowned"), 0600); e != nil {
				t.Fatal(e)
			}
			if manager, e := OpenManager(root, bounds.DefaultQuotaBytes); e == nil {
				_ = manager.Close(context.Background())
				t.Fatal("unknown ownership accepted")
			}
			if data, e := os.ReadFile(path); e != nil || string(data) != "unowned" {
				t.Fatal("unknown storage was altered")
			}
			if e := os.Remove(path); e != nil {
				t.Fatal(e)
			}
			if name != "staging/unowned" {
				if e := os.Remove(filepath.Dir(path)); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
	if e := os.WriteFile(filepath.Join(root, lockName), []byte("oversize lock"), 0600); e != nil {
		t.Fatal(e)
	}
	if m, e := OpenManager(root, bounds.DefaultQuotaBytes); e == nil {
		_ = m.Close(context.Background())
		t.Fatal("nonempty lock accepted")
	}
}
func TestCodexBoundedDirectoryEnumeration(t *testing.T) {
	root := regressionRoot(t)
	m, e := OpenManager(root, bounds.DefaultQuotaBytes)
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close(context.Background())
	for _, name := range []string{"extra-one", "extra-two"} {
		if e := os.WriteFile(filepath.Join(root, name), nil, 0600); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := m.listRootNames(); !errors.Is(e, ErrLimit) {
		t.Fatalf("enumeration cap: %v", e)
	}
}
func TestCodexRetryCloseRetainsLifetimeLock(t *testing.T) {
	root := regressionRoot(t)
	m, e := OpenManager(root, bounds.DefaultQuotaBytes)
	if e != nil {
		t.Fatal(e)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	auth := StaticAuthorizer{Grant: testGrantInternal(), Allow: func(AcquireIntent) error { close(entered); <-release; return context.Canceled }}
	done := make(chan error, 1)
	go func() {
		_, e := m.Acquire(context.Background(), AcquireIntent{ProjectID: "1", MRIID: 7, Depth: 2}, auth)
		done <- e
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if e := m.Close(ctx); !errors.Is(e, ErrCanceled) {
		t.Fatalf("close: %v", e)
	}
	if other, e := OpenManager(root, bounds.DefaultQuotaBytes); !errors.Is(e, ErrBusy) {
		if other != nil {
			_ = other.Close(context.Background())
		}
		t.Fatalf("root unlocked early: %v", e)
	}
	if _, e := m.Acquire(context.Background(), AcquireIntent{ProjectID: "1", MRIID: 7, Depth: 2}, auth); !errors.Is(e, ErrClosed) {
		t.Fatalf("closing admitted work: %v", e)
	}
	close(release)
	if e := <-done; !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if e := m.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
	reopened, e := OpenManager(root, bounds.DefaultQuotaBytes)
	if e != nil {
		t.Fatal(e)
	}
	if e := reopened.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func TestCodexWarmIndexAndManifestByteIntegrity(t *testing.T) {
	for _, name := range []string{"pack.idx", "manifest.json"} {
		t.Run(name, func(t *testing.T) {
			root := regressionRoot(t)
			m, e := OpenManager(root, bounds.DefaultQuotaBytes)
			if e != nil {
				t.Fatal(e)
			}
			defer m.Close(context.Background())
			ip, h := tinyBlobPack(t)
			g, e := m.PublishGeneration(context.Background(), "fp", "ns", h, h, h, ip)
			if e != nil {
				t.Fatal(e)
			}
			path := filepath.Join(root, gensDir, g.ID, name)
			data, e := os.ReadFile(path)
			if e != nil {
				t.Fatal(e)
			}
			if name == "pack.idx" {
				data[8+1024+20] ^= 1
			} else {
				var meta genManifest
				if e := json.Unmarshal(data, &meta); e != nil {
					t.Fatal(e)
				}
				meta.Base = "1111111111111111111111111111111111111111"
				data, e = json.Marshal(meta)
				if e != nil {
					t.Fatal(e)
				}
			}
			if e := os.WriteFile(path, data, 0600); e != nil {
				t.Fatal(e)
			}
			if warm, e := m.lookupWarm(context.Background(), "fp", "ns"); e == nil {
				_ = warm.Unpin()
				t.Fatal("same-size corruption accepted")
			}
		})
	}
}
func TestCodexPositiveReadbackComparesContent(t *testing.T) {
	root := regressionRoot(t)
	m, e := OpenManager(root, bounds.DefaultQuotaBytes)
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close(context.Background())
	if e := writeFileAtFD(int(m.root.Fd()), "probe", []byte("bad"), m); e != nil {
		t.Fatal(e)
	}
	if e := compareFileAt(context.Background(), int(m.root.Fd()), "probe", []byte("ok!"), m.rootDev); !errors.Is(e, ErrIntegrity) {
		t.Fatal(e)
	}
	if e := os.Remove(filepath.Join(root, "probe")); e != nil {
		t.Fatal(e)
	}
}
func TestCodexOwnedEvictionAndStaleGenerationPin(t *testing.T) {
	root := regressionRoot(t)
	m, e := OpenManager(root, bounds.DefaultQuotaBytes)
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close(context.Background())
	ip, h := tinyBlobPack(t)
	g, e := m.PublishGeneration(context.Background(), "fp", "ns", h, h, h, ip)
	if e != nil {
		t.Fatal(e)
	}
	if e := g.Pin(); e != nil {
		t.Fatal(e)
	}
	if e := m.EvictGeneration(context.Background(), g.ID); !errors.Is(e, ErrPinned) {
		t.Fatal(e)
	}
	if e := g.Unpin(); e != nil {
		t.Fatal(e)
	}
	m.SetFaultPoints(&FaultPoints{BeforeDelete: func() error { return errors.New("injected deletion") }})
	if e := m.EvictGeneration(context.Background(), g.ID); e == nil {
		t.Fatal("deletion fault ignored")
	}
	charged, e := m.ChargedBytes()
	if e != nil || charged == 0 {
		t.Fatalf("charge released before deletion: %d %v", charged, e)
	}
	m.SetFaultPoints(nil)
	if e := m.EvictGeneration(context.Background(), g.ID); e != nil {
		t.Fatal(e)
	}
	if charged, e := m.ChargedBytes(); e != nil || charged != 0 {
		t.Fatalf("owned deletion: %d %v", charged, e)
	}
	g2, e := m.PublishGeneration(context.Background(), "fp2", "ns", h, h, h, ip)
	if e != nil {
		t.Fatal(e)
	}
	if g.ID == g2.ID {
		t.Fatal("generation id reused")
	}
	if e := g.Pin(); e == nil {
		t.Fatal("stale handle pinned reused slot")
	}
}
func TestCodexFailedCommitRetainsFullReservation(t *testing.T) {
	root := regressionRoot(t)
	m, e := OpenManager(root, bounds.DefaultQuotaBytes)
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close(context.Background())
	ip, h := tinyBlobPack(t)
	m.SetFaultPoints(&FaultPoints{AfterLedger: func() error {
		for _, s := range m.slots {
			if s.State == SlotCommitted {
				return errors.New("injected commit persistence")
			}
		}
		return nil
	}})
	if _, e := m.PublishGeneration(context.Background(), "fp", "ns", h, h, h, ip); e == nil {
		t.Fatal("commit fault ignored")
	}
	charge, e := m.ChargedBytes()
	if e != nil || charge != uint64(bounds.GenerationCharge()) {
		t.Fatalf("ambiguous charge=%d %v", charge, e)
	}
}

type cancelAfterChecks struct {
	context.Context
	n      atomic.Int32
	at     int32
	cancel context.CancelFunc
}

func (c *cancelAfterChecks) Err() error {
	if c.n.Add(1) == c.at {
		c.cancel()
	}
	return c.Context.Err()
}
func TestCodexWarmLocalCancellationIsTyped(t *testing.T) {
	root := regressionRoot(t)
	m, e := OpenManager(root, bounds.DefaultQuotaBytes)
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close(context.Background())
	ip, h := tinyBlobPack(t)
	g, e := m.PublishGeneration(context.Background(), "fp", "ns", h, h, h, ip)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &cancelAfterChecks{Context: ctx, at: 8, cancel: cancel}
	if warm, e := m.lookupWarm(c, "fp", "ns"); !errors.Is(e, context.Canceled) {
		if warm != nil {
			_ = warm.Unpin()
		}
		t.Fatalf("local cancel: %v %s", e, g.ID)
	}
}
func TestCodexLedgerSemanticDigestAndFlags(t *testing.T) {
	h := ledgerHeader{Quota: uint64(bounds.DefaultQuotaBytes)}
	slots := make([]Slot, slotCount)
	for _, edit := range []func(*Slot){func(s *Slot) { s.Flags = 1 }, func(s *Slot) { s.ReadCount = bounds.MaxReaders + 1 }, func(s *Slot) { s.PackSize = s.Charge + 1 }} {
		slots[0] = Slot{State: SlotAmbiguous, ID: sha256.Sum256([]byte("id")), Charge: 100}
		edit(&slots[0])
		if e := validateLedgerSemantics(h, slots); e == nil {
			t.Fatal("invalid accounting accepted")
		}
	}
}
func TestCodexExactCloneIdentity(t *testing.T) {
	instance := "https://git.example:8443/gitlab/api/v4"
	if e := ValidateCloneURL("https://git.example:8443/gitlab/group/repo.git", instance, "group/repo", false); e != nil {
		t.Fatal(e)
	}
	for _, raw := range []string{"https://git.example/gitlab/group/repo.git", "https://git.example:8443/group/repo.git", "https://git.example:8443/gitlab/group/Repo.git", "https://git.example:8443/gitlab/group/repo.git?token=x"} {
		if e := ValidateCloneURL(raw, instance, "group/repo", false); e == nil {
			t.Fatal("clone URL mismatch accepted")
		}
	}
	if _, e := ParseSHA("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"); e == nil {
		t.Fatal("malformed SHA accepted")
	}
}

func TestCodexServiceStartupTrustIsAuthoritative(t *testing.T) {
	intent := AcquireIntent{CAPath: "caller", Insecure: true, AllowedInsecureHost: "caller"}
	got := applyServiceTLS(intent, ServiceConfig{CAPath: "configured", AllowedInsecureHost: "corporate"})
	if got.CAPath != "configured" || got.Insecure || got.AllowedInsecureHost != "corporate" {
		t.Fatal("intent overrode service trust policy")
	}
}
func TestCodexUnknownLedgerScratchRefusedWithoutTruncation(t *testing.T) {
	root := regressionRoot(t)
	m, e := OpenManager(root, bounds.DefaultQuotaBytes)
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close(context.Background())
	path := filepath.Join(root, ledgerTmp)
	data := []byte("unowned malformed scratch")
	if e := os.WriteFile(path, data, 0600); e != nil {
		t.Fatal(e)
	}
	ip, h := tinyBlobPack(t)
	if _, e := m.PublishGeneration(context.Background(), "fp", "ns", h, h, h, ip); e == nil {
		t.Fatal("unknown scratch accepted")
	}
	got, e := os.ReadFile(path)
	if e != nil || string(got) != string(data) {
		t.Fatal("unknown scratch was truncated")
	}
}

func TestCodexServiceTLSOutcomesAndChangedWarmCA(t *testing.T) {
	ip, head, base := commitIndexedPack(t)
	ca, cert := acquireTestServerCert(t)
	wrong, _ := acquireTestServerCert(t)
	dir := t.TempDir()
	rightPath := filepath.Join(dir, "right.pem")
	wrongPath := filepath.Join(dir, "wrong.pem")
	if e := os.WriteFile(rightPath, ca, 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(wrongPath, wrong, 0600); e != nil {
		t.Fatal(e)
	}
	var hits atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, pass, ok := r.BasicAuth()
		if !ok || pass != "test-token" {
			http.Error(w, "unauthorized", 401)
			return
		}
		if strings.Contains(r.URL.Path, "/info/refs") {
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			_ = writeShallowAdv(w, head, base)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		req := packp.NewUploadPackRequest()
		req.Depth = packp.DepthCommits(2)
		_ = req.Capabilities.Set(capability.Shallow)
		response := packp.NewUploadPackResponseWithPackfile(req, io.NopCloser(bytes.NewReader(ip.PackBytes)))
		response.Shallows = []plumbing.Hash{base}
		_ = response.Encode(w)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()
	for _, tc := range []struct {
		name, ca                    string
		mismatch, insecure, success bool
	}{{"trusted", rightPath, false, false, true}, {"valid wrong CA", wrongPath, false, false, false}, {"hostname mismatch", rightPath, true, false, false}, {"explicit named insecure", wrongPath, true, true, true}, {"insecure defaults off", wrongPath, false, false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			raw := srv.URL
			if tc.mismatch {
				raw = strings.Replace(raw, "127.0.0.1", "localhost", 1)
			}
			u, _ := url.Parse(raw)
			g := completeTestGrant(Grant{CanonicalInstance: raw + "/api/v4", OriginHost: u.Hostname(), ProjectID: "1", ProjectPath: "repo", SourceFork: "1", SourcePath: "repo", TargetPath: "repo", MRIID: 7, MRVersion: MRVersionFromDiffRefs(head, base, base), HeadSHA: head, BaseSHA: base, StartSHA: base, ActorID: "a", AuthDomain: "domain", HTTPSURL: raw + "/repo.git"})
			svc, e := OpenService(ServiceConfig{Enabled: true, Root: regressionRoot(t), QuotaBytes: bounds.DefaultQuotaBytes, Token: "test-token", CAPath: tc.ca, Insecure: tc.insecure, AllowedInsecureHost: u.Hostname()})
			if e != nil {
				t.Fatal(e)
			}
			defer svc.Close(context.Background())
			intent := AcquireIntent{ProjectID: "1", MRIID: 7, Depth: 2, AllowLoopback: true}
			result, e := svc.Acquire(context.Background(), intent, StaticAuthorizer{Grant: g})
			if tc.success {
				if e != nil || result.Warm {
					t.Fatalf("cold: %v %+v", e, result)
				}
				result, e = svc.Acquire(context.Background(), intent, StaticAuthorizer{Grant: g})
				if e != nil || !result.Warm {
					t.Fatalf("warm: %v %+v", e, result)
				}
			} else if e == nil {
				t.Fatal("untrusted TLS succeeded")
			}
			if tc.name == "trusted" {
				before := hits.Load()
				if e := os.WriteFile(rightPath, []byte("malformed changed trust"), 0600); e != nil {
					t.Fatal(e)
				}
				if _, e := svc.Acquire(context.Background(), intent, StaticAuthorizer{Grant: g}); e == nil {
					t.Fatal("malformed same-path CA warm-hit")
				}
				if hits.Load() != before {
					t.Fatal("invalid CA reached transport before failing")
				}
				if e := os.WriteFile(rightPath, ca, 0600); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
}
