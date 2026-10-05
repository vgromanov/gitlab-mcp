//go:build linux || darwin

package gitcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshtrust"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

const h1AcquireXattr = "user.native-ssh-test"

func h1ResolveKnown(t *testing.T, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		parent, e := filepath.EvalSymlinks(filepath.Dir(path))
		if e != nil {
			return path
		}
		return filepath.Join(parent, filepath.Base(path))
	}
	if err != nil {
		t.Fatalf("resolve known_hosts: %v", err)
	}
	return real
}

func h1InstallAcquireMutation(t *testing.T, resolved string, modeOnly bool, reached, mutated, bytesOK, attrSkip *atomic.Bool, attrErr *atomic.Value) func() {
	t.Helper()
	return sshtrust.SetReplacePostRenamePhaseForTest(resolved, func(path string) {
		if path != resolved {
			return
		}
		reached.Store(true)
		data, err := os.ReadFile(path)
		if err != nil || len(data) == 0 {
			bytesOK.Store(false)
			return
		}
		intended := append([]byte(nil), data...)
		bytesOK.Store(true)
		stBefore, err := os.Stat(path)
		if err != nil {
			return
		}
		if modeOnly {
			if err := os.Chmod(path, 0o644); err != nil {
				return
			}
			st, err := os.Stat(path)
			if err != nil || st.Mode().Perm() != 0o644 {
				return
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, intended) {
				bytesOK.Store(false)
				return
			}
			mutated.Store(true)
			return
		}
		if err := unix.Setxattr(path, h1AcquireXattr, []byte("external-h1-acquire"), 0); err != nil {
			attrErr.Store(err.Error())
			attrSkip.Store(true)
			return
		}
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() != stBefore.Mode().Perm() {
			return
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(after, intended) {
			bytesOK.Store(false)
			return
		}
		mutated.Store(true)
	})
}

// Acquisition-level H1 controls: distinct mode-only and attribute-only mutations
// in the post-rename/pre-capture interval must not publish or increase charge.
func TestH1_ExternalMetadataBeforeCapture_AcquireRejects(t *testing.T) {
	type caseSpec struct {
		name    string
		missing bool
		mode    bool // true=mode-only, false=attr-only
	}
	cases := []caseSpec{
		{name: "existing/mode-only", missing: false, mode: true},
		{name: "existing/attr-only", missing: false, mode: false},
		{name: "missing/mode-only", missing: true, mode: true},
		{name: "missing/attr-only", missing: true, mode: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reached, mutated, bytesOK, attrSkip atomic.Bool
			var attrErr atomic.Value
			svc, auth, intent, known, _, _ := gEnrollment(t, "accept-new", "no", tc.missing, false)
			resolved := h1ResolveKnown(t, known)
			clear := h1InstallAcquireMutation(t, resolved, tc.mode, &reached, &mutated, &bytesOK, &attrSkip, &attrErr)
			t.Cleanup(clear)

			baseline, err := svc.Manager().ChargedBytes()
			if err != nil {
				t.Fatal(err)
			}
			result, err := svc.Acquire(context.Background(), intent, auth)
			if attrSkip.Load() {
				t.Fatalf("attribute-only control unsupported on this filesystem (not red evidence): %v", attrErr.Load())
			}
			if !reached.Load() || !mutated.Load() || !bytesOK.Load() {
				t.Fatalf("H1 phase incomplete: reached=%v mutated=%v bytesOK=%v acquire_err=%v", reached.Load(), mutated.Load(), bytesOK.Load(), err)
			}
			if !errors.Is(err, ErrAuthz) || result != nil {
				t.Fatalf("external %s metadata published: result=%v err=%v", tc.name, result, err)
			}
			after, err := svc.Manager().ChargedBytes()
			if err != nil || after != baseline {
				t.Fatalf("charge increased despite H1 interval: baseline=%d after=%d err=%v", baseline, after, err)
			}
		})
	}
}

// Real service acquisition authenticated-update path on an already-trusted
// existing known_hosts file: mode-only and attr-only post-rename mutations
// must not publish or increase charge.
func TestH1_ExternalMetadataBeforeCapture_AcquireAuthenticatedUpdateRejects(t *testing.T) {
	for _, modeOnly := range []bool{true, false} {
		name := "attr-only"
		if modeOnly {
			name = "mode-only"
		}
		t.Run(name, func(t *testing.T) {
			ip, head, base := commitIndexedPack(t)
			packBytes := append([]byte(nil), ip.PackBytes...)
			hostKey := mustEd25519(t)
			rotated := mustEd25519(t)
			idPath, clientKey := writeSSHIdentity(t)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := ln.Addr().String()
			_, port, _ := net.SplitHostPort(addr)
			known := writeSSHKnownHosts(t, hostKey, addr)
			resolved := h1ResolveKnown(t, known)
			done := make(chan struct{})
			go func() {
				defer close(done)
				for {
					raw, e := ln.Accept()
					if e != nil {
						return
					}
					go serveF3Upload(raw, hostKey, clientKey, packBytes, head, base, rotated)
				}
			}()
			t.Cleanup(func() { _ = ln.Close(); <-done })

			home := t.TempDir()
			body := fmt.Sprintf("Match host 127.0.0.1\n User ssh-acquire\n Port %s\n IdentitiesOnly yes\n IdentityAgent none\n IdentityFile %s\n UserKnownHostsFile %s\n GlobalKnownHostsFile none\n StrictHostKeyChecking yes\n UpdateHostKeys yes\n", port, idPath, known)
			if err := os.WriteFile(filepath.Join(home, "config"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			svc, auth, intent := openF3Acquire(t, home, addr, head, base)

			var reached, mutated, bytesOK, attrSkip atomic.Bool
			var attrErr atomic.Value
			clear := h1InstallAcquireMutation(t, resolved, modeOnly, &reached, &mutated, &bytesOK, &attrSkip, &attrErr)
			t.Cleanup(clear)

			baseline, err := svc.Manager().ChargedBytes()
			if err != nil {
				t.Fatal(err)
			}
			result, err := svc.Acquire(context.Background(), intent, auth)
			if attrSkip.Load() {
				t.Fatalf("attribute-only control unsupported on this filesystem (not red evidence): %v", attrErr.Load())
			}
			if !reached.Load() || !mutated.Load() || !bytesOK.Load() {
				t.Fatalf("authenticated-update H1 phase incomplete: reached=%v mutated=%v bytesOK=%v acquire_err=%v", reached.Load(), mutated.Load(), bytesOK.Load(), err)
			}
			// Update-path rejection may surface as ErrAuthz (refresh) or the
			// writer's ErrTrustFiles before publication; either must leave nil result.
			if result != nil || err == nil || !(errors.Is(err, ErrAuthz) || errors.Is(err, sshtrust.ErrTrustFiles)) {
				t.Fatalf("external authenticated-update %s metadata published: result=%v err=%v", name, result, err)
			}
			after, err := svc.Manager().ChargedBytes()
			if err != nil || after != baseline {
				t.Fatalf("charge increased despite authenticated-update H1 interval: baseline=%d after=%d err=%v", baseline, after, err)
			}
			// Key bytes from the attempted update must remain the authenticated content
			// present at mutation time; mode/attr-only must not rewrite them away.
			got, err := os.ReadFile(known)
			if err != nil || !bytes.Contains(got, bytes.TrimSpace(ssh.MarshalAuthorizedKey(hostKey.PublicKey()))) {
				t.Fatalf("intended host key bytes lost during metadata interval: %v", err)
			}
		})
	}
}

func TestH1_FaultFreeAcquireWarmPositive(t *testing.T) {
	sshtrust.SetReplacePostRenamePhaseForTest("", nil)
	svc, auth, intent, known, _, _ := gEnrollment(t, "accept-new", "no", false, false)
	cold, err := svc.Acquire(context.Background(), intent, auth)
	if err != nil {
		t.Fatalf("fault-free enrollment acquire: %v", err)
	}
	st, err := os.Stat(known)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("fault-free known_hosts mode: %v %v", st, err)
	}
	warm, err := svc.Acquire(context.Background(), intent, auth)
	if err != nil || !warm.Warm || warm.GenerationID != cold.GenerationID {
		t.Fatalf("fault-free same-generation warm: %v", err)
	}
}

// Fault-free real authenticated update + warm (same fixture shape as F3 rotation).
func TestH1_FaultFreeAcquireAuthenticatedUpdateWarmPositive(t *testing.T) {
	sshtrust.SetReplacePostRenamePhaseForTest("", nil)
	ip, head, base := commitIndexedPack(t)
	packBytes := append([]byte(nil), ip.PackBytes...)
	hostKey := mustEd25519(t)
	rotated := mustEd25519(t)
	idPath, clientKey := writeSSHIdentity(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	known := writeSSHKnownHosts(t, hostKey, addr)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			raw, e := ln.Accept()
			if e != nil {
				return
			}
			go serveF3Upload(raw, hostKey, clientKey, packBytes, head, base, rotated)
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); <-done })
	home := t.TempDir()
	body := fmt.Sprintf("Match host 127.0.0.1\n User ssh-acquire\n Port %s\n IdentitiesOnly yes\n IdentityAgent none\n IdentityFile %s\n UserKnownHostsFile %s\n GlobalKnownHostsFile none\n StrictHostKeyChecking yes\n UpdateHostKeys yes\n", port, idPath, known)
	if err := os.WriteFile(filepath.Join(home, "config"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	svc, auth, intent := openF3Acquire(t, home, addr, head, base)
	cold, err := svc.Acquire(context.Background(), intent, auth)
	if err != nil {
		t.Fatalf("fault-free authenticated update publish: %v", err)
	}
	data, err := os.ReadFile(known)
	if err != nil || !bytes.Contains(data, bytes.TrimSpace(ssh.MarshalAuthorizedKey(rotated.PublicKey()))) {
		t.Fatalf("fault-free update did not persist authenticated addition: %v", err)
	}
	st, err := os.Stat(known)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("fault-free update mode: %v %v", st, err)
	}
	warm, err := svc.Acquire(context.Background(), intent, auth)
	if err != nil || !warm.Warm || warm.GenerationID != cold.GenerationID {
		t.Fatalf("fault-free update warm: %v", err)
	}
}
