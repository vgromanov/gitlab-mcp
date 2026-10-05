//go:build linux || darwin

package gitcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/listx"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

type gAuthorizer struct {
	grant Grant
	calls atomic.Int32
	after func()
}

func (a *gAuthorizer) ResolveGrant(context.Context, AcquireIntent) (Grant, error) {
	if a.calls.Add(1) == 3 && a.after != nil {
		a.after()
	}
	return a.grant, nil
}

func gEnrollment(t *testing.T, strict, update string, missing, fork bool) (*Service, StaticAuthorizer, AcquireIntent, string, string, ssh.Signer) {
	return gEnrollmentWithHook(t, strict, update, missing, fork, nil)
}

func gEnrollmentWithHook(t *testing.T, strict, update string, missing, fork bool, hook func()) (*Service, StaticAuthorizer, AcquireIntent, string, string, ssh.Signer) {
	t.Helper()
	ip, head, base := commitIndexedPack(t)
	host := mustEd25519(t)
	extra := mustEd25519(t)
	id, client := writeSSHIdentity(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	known := filepath.Join(t.TempDir(), "known_hosts")
	if !missing {
		if err := os.WriteFile(known, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			raw, e := ln.Accept()
			if e != nil {
				return
			}
			go serveF3UploadWithHook(raw, host, client, ip.PackBytes, head, base, extra, hook)
		}
	}()
	t.Cleanup(func() { ln.Close(); <-done })
	home := t.TempDir()
	cfg := filepath.Join(home, "config")
	text := fmt.Sprintf("Match host 127.0.0.1\n User ssh-acquire\n Port %s\n IdentitiesOnly yes\n IdentityAgent none\n IdentityFile %s\n UserKnownHostsFile %s\n GlobalKnownHostsFile none\n StrictHostKeyChecking %s\n UpdateHostKeys %s\n", port, id, known, strict, update)
	if err := os.WriteFile(cfg, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	svc, auth, intent := openF3Acquire(t, home, addr, head, base)
	if !fork {
		auth.Grant.SourceFork = auth.Grant.TargetProjectID
	}
	if fork {
		auth.Grant.SourceFork = "2"
		auth.Grant.TargetProjectID = "9"
		auth.Grant.SourceSSHURL = "ssh://git@" + addr + "/source.git"
		auth.Grant.TargetSSHURL = "ssh://git@" + addr + "/target.git"
		auth.Grant.SourcePath = "source"
		auth.Grant.TargetPath = "target"
		auth.Grant.SSHURL = auth.Grant.SourceSSHURL
	}
	return svc, auth, intent, known, cfg, host
}

func TestG1_OwnedEnrollmentDoesNotWaiveUnrelatedChanges(t *testing.T) {
	for _, which := range []string{"affected-file", "unrelated-user-file", "global-file", "strict-policy", "update-policy", "identity", "agent-selection", "target-role"} {
		t.Run(which, func(t *testing.T) {
			svc, auth, intent, known, cfg, _ := gEnrollment(t, "accept-new", "no", false, which == "target-role")
			original, err := os.ReadFile(cfg)
			if err != nil {
				t.Fatal(err)
			}
			other := filepath.Join(t.TempDir(), "other_known_hosts")
			if err := os.WriteFile(other, []byte("# baseline\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if which == "unrelated-user-file" {
				original = bytes.ReplaceAll(original, []byte("UserKnownHostsFile "+known), []byte("UserKnownHostsFile "+known+" "+other))
			}
			if which == "global-file" {
				original = bytes.ReplaceAll(original, []byte("GlobalKnownHostsFile none"), []byte("GlobalKnownHostsFile "+other))
			}
			if err := os.WriteFile(cfg, original, 0600); err != nil {
				t.Fatal(err)
			}
			a := &gAuthorizer{grant: auth.Grant, after: func() {
				b, e := os.ReadFile(known)
				if e != nil || len(b) == 0 {
					t.Errorf("no real enrollment before mutation: %v", e)
					return
				}
				switch which {
				case "affected-file", "target-role":
					e = os.WriteFile(known, append(b, []byte("# EXTERNAL mutation\n")...), 0600)
				case "unrelated-user-file", "global-file":
					e = os.WriteFile(other, []byte("# EXTERNAL mutation\n"), 0600)
				case "strict-policy":
					e = os.WriteFile(cfg, bytes.ReplaceAll(original, []byte("StrictHostKeyChecking accept-new"), []byte("StrictHostKeyChecking no")), 0600)
				case "update-policy":
					e = os.WriteFile(cfg, bytes.ReplaceAll(original, []byte("UpdateHostKeys no"), []byte("UpdateHostKeys yes")), 0600)
				case "identity":
					id, _ := writeSSHIdentity(t)
					lines := strings.Split(string(original), "\n")
					for i, line := range lines {
						if strings.HasPrefix(strings.TrimSpace(line), "IdentityFile ") {
							lines[i] = " IdentityFile " + id
						}
					}
					e = os.WriteFile(cfg, []byte(strings.Join(lines, "\n")), 0600)
				case "agent-selection":
					e = os.WriteFile(cfg, bytes.ReplaceAll(original, []byte("IdentityAgent none"), []byte("IdentityAgent "+filepath.Join(t.TempDir(), "unavailable-agent"))), 0600)
				}
				if e != nil {
					t.Errorf("mutation: %v", e)
				}
			}}
			baseline, e := svc.Manager().ChargedBytes()
			if e != nil {
				t.Fatal(e)
			}
			res, err := svc.Acquire(context.Background(), intent, a)
			if !errors.Is(err, ErrAuthz) || res != nil {
				t.Errorf("owned enrollment waived %s: resultPresent=%v err=%v", which, res != nil, err)
			}
			charged, e := svc.Manager().ChargedBytes()
			if e != nil || charged != baseline {
				t.Errorf("unrelated change charged/published a generation: before=%d after=%d err=%v", baseline, charged, e)
			}
		})
	}
}

func TestG4_YesModeEnrollmentSurvivesIneligibleAnnouncement(t *testing.T) {
	for _, strict := range []string{"accept-new", "no"} {
		for _, missing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/missing=%v", strict, missing), func(t *testing.T) {
				svc, auth, intent, known, _, host := gEnrollment(t, strict, "yes", missing, false)
				cold, err := svc.Acquire(context.Background(), intent, auth)
				if err != nil {
					t.Fatalf("approved yes-mode enrollment: %v", err)
				}
				data, err := os.ReadFile(known)
				if err != nil {
					t.Fatal(err)
				}
				var keys [][]byte
				for len(bytes.TrimSpace(data)) > 0 {
					_, _, key, _, rest, e := ssh.ParseKnownHosts(data)
					if e != nil {
						t.Fatal(e)
					}
					keys = append(keys, key.Marshal())
					data = rest
				}
				if len(keys) != 1 || !bytes.Equal(keys[0], host.PublicKey().Marshal()) {
					t.Fatal("ineligible announcement enrolled additional keys")
				}
				warm, err := svc.Acquire(context.Background(), intent, auth)
				if err != nil || !warm.Warm || warm.GenerationID != cold.GenerationID {
					t.Fatalf("warm after yes enrollment: %v", err)
				}
			})
		}
	}
}

func TestG3_WrappedCancellationHasNoUnsafeTextOrUnwrap(t *testing.T) {
	want := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded} {
		for _, stage := range []string{"advertisement", "upload", "reader", "acquisition"} {
			t.Run(stage+"/"+sentinel.Error(), func(t *testing.T) {
				wrapped := fmt.Errorf("SYNTHETIC_SECRET https://synthetic-user:synthetic-password@example.test/private: %w", sentinel)
				var err error
				switch stage {
				case "advertisement":
					_, err = uploadPack(context.Background(), &f4Sess{advErr: wrapped}, FetchOptions{Depth: 1, Wants: []plumbing.Hash{want}}, 1<<20)
				case "upload":
					_, err = uploadPack(context.Background(), &f4Sess{upErr: wrapped}, FetchOptions{Depth: 1, Wants: []plumbing.Hash{want}}, 1<<20)
				case "reader":
					_, err = readLimited(context.Background(), &f4CancelReader{err: wrapped}, 1<<20)
				case "acquisition":
					m, e := OpenService(ServiceConfig{Enabled: true, Root: regressionRoot(t)})
					if e != nil {
						t.Fatal(e)
					}
					defer m.Close(context.Background())
					_, err = m.Acquire(context.Background(), AcquireIntent{Depth: 1}, AuthorizerFunc(func(context.Context, AcquireIntent) (Grant, error) { return Grant{}, wrapped }))
				}
				if !errors.Is(err, sentinel) || err.Error() != sentinel.Error() || errors.Unwrap(err) != nil {
					t.Errorf("unsafe context result: matches=%v textSafe=%v unwrapSafe=%v", errors.Is(err, sentinel), err != nil && err.Error() == sentinel.Error(), errors.Unwrap(err) == nil)
				}
			})
		}
	}
}

type AuthorizerFunc func(context.Context, AcquireIntent) (Grant, error)

func (f AuthorizerFunc) ResolveGrant(c context.Context, i AcquireIntent) (Grant, error) {
	return f(c, i)
}

type gAgent struct {
	ln      net.Listener
	entered chan struct{}
	ended   chan struct{}
	release chan struct{}
	once    sync.Once
}

func (a *gAgent) releaseNow() { a.once.Do(func() { close(a.release) }) }
func gSilentAgent(t *testing.T) (*gAgent, string) {
	t.Helper()
	dir, e := os.MkdirTemp("/tmp", "g-agent-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	a := &gAgent{ln: ln, entered: make(chan struct{}), ended: make(chan struct{}), release: make(chan struct{})}
	go func() {
		defer close(a.ended)
		c, e := ln.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			select {
			case <-a.release:
				c.Close()
			case <-stop:
			}
		}()
		packet := make([]byte, 5)
		if _, e = io.ReadFull(c, packet); e != nil {
			return
		}
		close(a.entered)
		_, _ = io.Copy(io.Discard, c)
	}()
	t.Cleanup(func() {
		a.releaseNow()
		ln.Close()
		select {
		case <-a.ended:
		case <-time.After(time.Second):
			t.Error("fake agent leaked")
		}
	})
	return a, sock
}

func TestG2_AgentInventoryCancellationAndCloseJoin(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline", "manager-close", "service-close"} {
		t.Run(mode, func(t *testing.T) {
			home, port, id, known, addr, head, base, cleanup := startF2SSHFixture(t)
			defer cleanup()
			a, sock := gSilentAgent(t)
			writeF2Config(t, home, port, id, known, "PubkeyAuthentication yes\nIdentitiesOnly yes\nIdentityAgent "+sock+"\n")
			svc, auth, intent := openF2Acquire(t, home, addr, head, base)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "deadline" {
				ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
				defer cancel()
			}
			result := make(chan error, 1)
			go func() { _, e := svc.Acquire(ctx, intent, auth); result <- e }()
			select {
			case <-a.entered:
			case <-time.After(time.Second):
				t.Fatal("inventory not requested")
			}
			closeResult := make(chan error, 1)
			switch mode {
			case "cancel":
				cancel()
			case "manager-close":
				go func() { closeResult <- svc.Manager().Close(context.Background()) }()
			case "service-close":
				go func() { closeResult <- svc.Close(context.Background()) }()
			}
			expected := context.Canceled
			if mode == "deadline" {
				expected = context.DeadlineExceeded
			}
			timedOut := false
			select {
			case e := <-result:
				if e != expected {
					t.Errorf("inventory result: %v", e)
				}
			case <-time.After(700 * time.Millisecond):
				t.Error("inventory RPC outlived cancellation/shutdown")
				timedOut = true
			}
			if timedOut {
				a.releaseNow()
				select {
				case <-result:
				case <-time.After(time.Second):
					t.Error("failed to unblock baseline")
				}
			}
			if mode == "manager-close" || mode == "service-close" {
				select {
				case e := <-closeResult:
					if e != nil {
						t.Error(e)
					}
				case <-time.After(time.Second):
					t.Error("close did not join")
				}
				m, e := OpenManager(svc.cfg.Root, svc.cfg.QuotaBytes)
				if e != nil {
					t.Errorf("lifetime lock retained: %v", e)
				} else {
					m.Close(context.Background())
				}
			}
			select {
			case <-a.ended:
			case <-time.After(time.Second):
				t.Error("RPC reader/socket leaked")
			}
		})
	}
}

func TestG2_ResponsiveAgentPolicyAndFileFallback(t *testing.T) {
	home, port, id, known, addr, head, base, cleanup := startF2SSHFixture(t)
	defer cleanup()
	body, err := os.ReadFile(id)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.ParseRawPrivateKey(body)
	clear(body)
	if err != nil {
		t.Fatal(err)
	}
	ring := agent.NewKeyring()
	if err := ring.Add(agent.AddedKey{PrivateKey: key}); err != nil {
		t.Fatal(err)
	}
	dir, e := os.MkdirTemp("/tmp", "g-responsive-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "responsive.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			go func() { defer c.Close(); _ = agent.ServeAgent(ring, c) }()
		}
	}()
	defer func() { ln.Close(); <-done }()
	writeF2Config(t, home, port, id, known, "PubkeyAuthentication yes\nIdentitiesOnly no\nIdentityAgent "+sock+"\n")
	svc, auth, intent := openF2Acquire(t, home, addr, head, base)
	cold, err := svc.Acquire(context.Background(), intent, auth)
	if err != nil {
		t.Fatal(err)
	}
	warm, err := svc.Acquire(context.Background(), intent, auth)
	if err != nil || !warm.Warm || warm.GenerationID != cold.GenerationID {
		t.Fatalf("responsive agent warm: %v", err)
	}
	otherPath, _ := writeSSHIdentity(t)
	otherBytes, e := os.ReadFile(otherPath)
	if e != nil {
		t.Fatal(e)
	}
	otherKey, e := ssh.ParseRawPrivateKey(otherBytes)
	clear(otherBytes)
	if e != nil {
		t.Fatal(e)
	}
	if e := ring.Add(agent.AddedKey{PrivateKey: otherKey}); e != nil {
		t.Fatal(e)
	}
	inventory, e := svc.Acquire(context.Background(), intent, auth)
	if e != nil || inventory.Warm || inventory.GenerationID == cold.GenerationID {
		t.Fatalf("changed selected agent inventory reused warm identity: %v", e)
	}
	inventoryWarm, e := svc.Acquire(context.Background(), intent, auth)
	if e != nil || !inventoryWarm.Warm || inventoryWarm.GenerationID != inventory.GenerationID {
		t.Fatalf("unchanged new inventory failed warm: %v", e)
	}
	writeF2Config(t, home, port, id, known, "PubkeyAuthentication yes\nIdentitiesOnly no\nIdentityAgent "+filepath.Join(t.TempDir(), "unavailable")+"\n")
	isolated, err := svc.Acquire(context.Background(), intent, auth)
	if err != nil || isolated.Warm || isolated.GenerationID == cold.GenerationID {
		t.Fatalf("changed agent/file fallback must acquire in separate identity: %v", err)
	}
}

// Enrollment is already persisted when the server admits public-key auth, before
// the first acquisition refresh. This exercises the writer-to-refresh interval.
func TestG1_ExternalWriteBeforeFirstRefreshRejected(t *testing.T) {
	ready := make(chan struct{})
	mutated := make(chan struct{})
	var known string
	var once sync.Once
	var mutationErr error
	svc, auth, intent, p, _, _ := gEnrollmentWithHook(t, "accept-new", "no", false, false, func() {
		<-ready
		once.Do(func() {
			defer close(mutated)
			data, e := os.ReadFile(known)
			if e != nil {
				mutationErr = e
				return
			}
			if len(data) == 0 {
				mutationErr = errors.New("enrollment not yet committed")
				return
			}
			mutationErr = os.WriteFile(known, append(data, []byte("# external-before-refresh\n")...), 0600)
		})
	})
	known = p
	close(ready)
	baseline, e := svc.Manager().ChargedBytes()
	if e != nil {
		t.Fatal(e)
	}
	result, e := svc.Acquire(context.Background(), intent, auth)
	select {
	case <-mutated:
	case <-time.After(time.Second):
		t.Fatal("checkpoint mutation not exercised")
	}
	if mutationErr != nil {
		t.Fatal(mutationErr)
	}
	if !errors.Is(e, ErrAuthz) || result != nil {
		t.Fatalf("external writer accepted before first refresh: %v", e)
	}
	after, e := svc.Manager().ChargedBytes()
	if e != nil || after != baseline {
		t.Fatal("external mutation published or reserved content")
	}
}

func TestG1_SequentialSharedFileOwnedTransitionsPublish(t *testing.T) {
	svc, auth, intent, known, _, _ := gEnrollment(t, "accept-new", "yes", false, true)
	cold, e := svc.Acquire(context.Background(), intent, auth)
	if e != nil {
		t.Fatalf("sequential shared-file enrollment/update: %v", e)
	}
	data, e := os.ReadFile(known)
	if e != nil {
		t.Fatal(e)
	}
	count := 0
	for len(bytes.TrimSpace(data)) > 0 {
		_, _, _, _, rest, e := ssh.ParseKnownHosts(data)
		if e != nil {
			t.Fatal(e)
		}
		count++
		data = rest
	}
	if count != 2 {
		t.Fatalf("second trusted role's authenticated addition not committed: %d keys", count)
	}
	warm, e := svc.Acquire(context.Background(), intent, auth)
	if e != nil || !warm.Warm || warm.GenerationID != cold.GenerationID {
		t.Fatalf("shared transition warm read: %v", e)
	}
}

type gHost struct{ signer ssh.Signer }

func TestG1_TrustedNoOpAndDurableRotationPositive(t *testing.T) {
	for _, mode := range []string{"no-op", "addition-rotation"} {
		t.Run(mode, func(t *testing.T) {
			ip, head, base := commitIndexedPack(t)
			host, newKey := mustEd25519(t), mustEd25519(t)
			id, client := writeSSHIdentity(t)
			ln, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			addr := ln.Addr().String()
			_, port, _ := net.SplitHostPort(addr)
			known := writeSSHKnownHosts(t, host, addr)
			before, e := os.ReadFile(known)
			if e != nil {
				t.Fatal(e)
			}
			var active atomic.Pointer[gHost]
			active.Store(&gHost{host})
			announced := newKey
			if mode == "no-op" {
				announced = host
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				for {
					raw, e := ln.Accept()
					if e != nil {
						return
					}
					go serveF3Upload(raw, active.Load().signer, client, ip.PackBytes, head, base, announced)
				}
			}()
			defer func() { ln.Close(); <-done }()
			home := t.TempDir()
			body := fmt.Sprintf("Match host 127.0.0.1\n User ssh-acquire\n Port %s\n IdentitiesOnly yes\n IdentityAgent none\n IdentityFile %s\n UserKnownHostsFile %s\n GlobalKnownHostsFile none\n StrictHostKeyChecking yes\n UpdateHostKeys yes\n", port, id, known)
			if e := os.WriteFile(filepath.Join(home, "config"), []byte(body), 0600); e != nil {
				t.Fatal(e)
			}
			svc, auth, intent := openF3Acquire(t, home, addr, head, base)
			prepared, _, e := listx.PrepareTrust(context.Background(), auth.Grant.SourceSSHURL, intent.SSH.WithLoopback())
			if e != nil {
				t.Fatal(e)
			}
			fetched, e := FetchObjects(context.Background(), auth.Grant.SourceSSHURL, FetchOptions{AllowLoopback: true, Depth: 2, Wants: []plumbing.Hash{head, base}, SSH: prepared})
			if e != nil || len(fetched.Objects()) == 0 {
				t.Fatalf("real object fetch/update: %v", e)
			}
			after, e := os.ReadFile(known)
			if e != nil {
				t.Fatal(e)
			}
			if mode == "no-op" {
				if prepared.TrustUpdateStatus() != "already_current" || !bytes.Equal(before, after) || len(prepared.TrustTransitions()) != 0 {
					t.Fatal("actual already-current announcement changed trust or lacked no-op outcome")
				}
			} else {
				if prepared.TrustUpdateStatus() != "updated" || !bytes.Contains(after, bytes.TrimSpace(ssh.MarshalAuthorizedKey(newKey.PublicKey()))) || len(prepared.TrustTransitions()) != 1 {
					t.Fatal("authenticated addition was not durably recorded")
				}
				active.Store(&gHost{newKey})
				next, e := FetchObjects(context.Background(), auth.Grant.SourceSSHURL, FetchOptions{AllowLoopback: true, Depth: 2, Wants: []plumbing.Hash{head, base}, SSH: intent.SSH})
				if e != nil || len(next.Objects()) == 0 {
					t.Fatalf("subsequent new-key connection/object read failed: %v", e)
				}
				latest, e := os.ReadFile(known)
				if e != nil {
					t.Fatal(e)
				}
				if bytes.Contains(latest, bytes.TrimSpace(ssh.MarshalAuthorizedKey(host.PublicKey()))) || !bytes.Contains(latest, bytes.TrimSpace(ssh.MarshalAuthorizedKey(newKey.PublicKey()))) {
					t.Fatal("authenticated replacement did not remove retired key")
				}
			}
			cold, e := svc.Acquire(context.Background(), intent, auth)
			if e != nil || cold.Warm {
				t.Fatalf("cold publication after trusted update: %v", e)
			}
			warm, e := svc.Acquire(context.Background(), intent, auth)
			if e != nil || !warm.Warm || warm.GenerationID != cold.GenerationID {
				t.Fatalf("unchanged trusted warm positive: %v", e)
			}
		})
	}
}

func TestG3_PublicFetchCanceledIsBareAndCommonBoundaryNormalizes(t *testing.T) {
	c, cancel := context.WithCancel(context.Background())
	cancel()
	_, e := FetchObjects(c, "https://127.0.0.1/repo.git", FetchOptions{AllowLoopback: true, Token: "synthetic-token", Depth: 1, Wants: []plumbing.Hash{plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}})
	if e != context.Canceled || errors.Unwrap(e) != nil {
		t.Fatalf("public FetchObjects unsafe cancellation: %v", e)
	}
	for _, want := range []error{context.Canceled, context.DeadlineExceeded} {
		if got := safeContextError(fmt.Errorf("synthetic-userinfo https://user:pass@example.test: %w", want)); got != want || errors.Unwrap(got) != nil {
			t.Fatal("public early-return normalizer retains unsafe payload")
		}
	}
	if safeContextError(errors.New("ordinary")) != nil {
		t.Fatal("ordinary error normalized as cancellation")
	}
}
