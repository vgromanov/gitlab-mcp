//go:build linux || darwin

package sshtrust

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/knownhosts"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshconfig"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

const h1TestXattr = "user.native-ssh-test"

// h1ResolvedPath mirrors loadContext path binding so the seam matches the writer.
func h1ResolvedPath(t *testing.T, path string) string {
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
		t.Fatalf("resolve trust path: %v", err)
	}
	return real
}

type h1Kind string

const (
	h1ModeOnly h1Kind = "mode-only"
	h1AttrOnly h1Kind = "attr-only"
)

type h1PhaseMutation struct {
	reached  atomic.Bool
	mutated  atomic.Bool
	bytesOK  atomic.Bool
	attrErr  atomic.Value // string
	attrSkip atomic.Bool
}

func (m *h1PhaseMutation) install(t *testing.T, path string, intended []byte, kind h1Kind) {
	t.Helper()
	clear := SetReplacePostRenamePhaseForTest(path, func(gotPath string) {
		if gotPath != path {
			return
		}
		m.reached.Store(true)
		got, err := os.ReadFile(gotPath)
		if err != nil || !bytes.Equal(got, intended) {
			m.bytesOK.Store(false)
			return
		}
		m.bytesOK.Store(true)
		stBefore, err := os.Stat(gotPath)
		if err != nil {
			return
		}
		switch kind {
		case h1ModeOnly:
			if err := os.Chmod(gotPath, 0o644); err != nil {
				return
			}
			st, err := os.Stat(gotPath)
			if err != nil || st.Mode().Perm() != 0o644 {
				return
			}
			// Mode-only: leave attributes untouched.
			m.mutated.Store(true)
		case h1AttrOnly:
			if err := unix.Setxattr(gotPath, h1TestXattr, []byte("external-h1"), 0); err != nil {
				m.attrErr.Store(err.Error())
				m.attrSkip.Store(true)
				return
			}
			st, err := os.Stat(gotPath)
			if err != nil || st.Mode().Perm() != stBefore.Mode().Perm() {
				return
			}
			after, err := os.ReadFile(gotPath)
			if err != nil || !bytes.Equal(after, intended) {
				m.bytesOK.Store(false)
				return
			}
			m.mutated.Store(true)
		}
	})
	t.Cleanup(clear)
}

func (m *h1PhaseMutation) requireCompleted(t *testing.T, kind h1Kind) {
	t.Helper()
	if kind == h1AttrOnly && m.attrSkip.Load() {
		t.Fatalf("attribute-only control unsupported on this filesystem (not red evidence): %v", m.attrErr.Load())
	}
	if !m.reached.Load() || !m.mutated.Load() || !m.bytesOK.Load() {
		t.Fatalf("phase incomplete: kind=%s reached=%v mutated=%v bytesOK=%v", kind, m.reached.Load(), m.mutated.Load(), m.bytesOK.Load())
	}
}

func TestH1_ExternalMetadataBeforeCapture_EnrollmentRejected(t *testing.T) {
	for _, missing := range []bool{true, false} {
		for _, kind := range []h1Kind{h1ModeOnly, h1AttrOnly} {
			name := "existing/" + string(kind)
			if missing {
				name = "missing/" + string(kind)
			}
			t.Run(name, func(t *testing.T) {
				addr := "h1-" + strings.ReplaceAll(name, "/", "-") + ".example:22"
				k := key(t)
				var p string
				if missing {
					p = filepath.Join(t.TempDir(), "known_hosts")
				} else {
					p = file(t, nil)
				}
				resolved := h1ResolvedPath(t, p)
				line := []byte(knownhosts.Line([]string{knownhosts.Normalize(addr)}, k.PublicKey()) + "\n")
				mut := &h1PhaseMutation{}
				mut.install(t, resolved, line, kind)
				cfg := sshconfig.Config{StrictHostKeyChecking: "accept-new", UpdateHostKeys: "no", UserKnownHostsFiles: []string{p}}
				m, err := New(context.Background(), cfg, addr)
				if err != nil {
					t.Fatal(err)
				}
				if err := m.Callback(addr, &net.TCPAddr{}, k.PublicKey()); err != nil {
					t.Fatal(err)
				}
				mut.requireCompleted(t, kind)
				if err := m.Finalize(nil); err != nil {
					t.Fatal(err)
				}
				if receipts := m.Transitions(); len(receipts) != 0 {
					t.Fatalf("external %s metadata blessed into owned receipt: %d", kind, len(receipts))
				}
				if m.UpdateStatus() == "enrolled_initial_key" {
					t.Fatalf("enrollment claimed despite external %s interval", kind)
				}
			})
		}
	}
}

func TestH1_ExternalMetadataBeforeCapture_AuthenticatedUpdateRejected(t *testing.T) {
	for _, kind := range []h1Kind{h1ModeOnly, h1AttrOnly} {
		t.Run(string(kind), func(t *testing.T) {
			addr := "h1-update-" + string(kind) + ".example:7999"
			old, next := key(t), key(t)
			start := []byte(knownhosts.Line([]string{addr}, old.PublicKey()) + "\n")
			p := file(t, start)
			resolved := h1ResolvedPath(t, p)
			// Intended update bytes are produced inside apply; assert exact
			// post-rename readback equals the bytes present at phase time and
			// remain unchanged by the metadata-only mutation.
			var intended atomic.Value
			mut := &h1PhaseMutation{}
			clear := SetReplacePostRenamePhaseForTest(resolved, func(gotPath string) {
				if gotPath != resolved {
					return
				}
				mut.reached.Store(true)
				got, err := os.ReadFile(gotPath)
				if err != nil || !bytes.Contains(got, []byte("ssh-ed25519")) || len(bytes.TrimSpace(got)) == 0 {
					mut.bytesOK.Store(false)
					return
				}
				intended.Store(append([]byte(nil), got...))
				mut.bytesOK.Store(true)
				stBefore, err := os.Stat(gotPath)
				if err != nil {
					return
				}
				switch kind {
				case h1ModeOnly:
					if err := os.Chmod(gotPath, 0o644); err != nil {
						return
					}
					st, err := os.Stat(gotPath)
					if err != nil || st.Mode().Perm() != 0o644 {
						return
					}
					after, err := os.ReadFile(gotPath)
					if err != nil || !bytes.Equal(after, intended.Load().([]byte)) {
						mut.bytesOK.Store(false)
						return
					}
					mut.mutated.Store(true)
				case h1AttrOnly:
					if err := unix.Setxattr(gotPath, h1TestXattr, []byte("external-h1-update"), 0); err != nil {
						mut.attrErr.Store(err.Error())
						mut.attrSkip.Store(true)
						return
					}
					st, err := os.Stat(gotPath)
					if err != nil || st.Mode().Perm() != stBefore.Mode().Perm() {
						return
					}
					after, err := os.ReadFile(gotPath)
					if err != nil || !bytes.Equal(after, intended.Load().([]byte)) {
						mut.bytesOK.Store(false)
						return
					}
					mut.mutated.Store(true)
				}
			})
			t.Cleanup(clear)
			m := manager(t, addr, p)
			f := &fakeProof{session: []byte("session"), signers: map[string]ssh.Signer{string(next.PublicKey().Marshal()): next}}
			err := m.apply(context.Background(), f, packet([][]byte{next.PublicKey().Marshal()}))
			mut.requireCompleted(t, kind)
			if err == nil {
				t.Fatalf("authenticated update accepted external %s metadata as owned", kind)
			}
			if !errors.Is(err, ErrTrustFiles) && !errors.Is(err, ErrUpdateUnsupported) && !errors.Is(err, ErrUpdate) {
				t.Fatalf("unexpected update error: %v", err)
			}
			if ferr := m.Finalize(nil); ferr != nil {
				t.Fatal(ferr)
			}
			if receipts := m.Transitions(); len(receipts) != 0 {
				t.Fatalf("update receipt exposed after external %s interval: %d", kind, len(receipts))
			}
		})
	}
}

func TestH1_FaultFreeEnrollmentAndUpdateStillPublish(t *testing.T) {
	SetReplacePostRenamePhaseForTest("", nil)
	addr := "h1-positive.example:22"
	k := key(t)
	p := file(t, nil)
	cfg := sshconfig.Config{StrictHostKeyChecking: "accept-new", UpdateHostKeys: "yes", UserKnownHostsFiles: []string{p}}
	m, err := New(context.Background(), cfg, addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Callback(addr, &net.TCPAddr{}, k.PublicKey()); err != nil {
		t.Fatal(err)
	}
	if err := m.Finalize(nil); err != nil {
		t.Fatal(err)
	}
	if len(m.Transitions()) != 1 {
		t.Fatal("fault-free enrollment missing receipt")
	}
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("fault-free enrollment mode: %v %v", st, err)
	}

	old, next := key(t), key(t)
	start := []byte(knownhosts.Line([]string{"h1-update-pos.example:7999"}, old.PublicKey()) + "\n")
	up := file(t, start)
	um := manager(t, "h1-update-pos.example:7999", up)
	f := &fakeProof{session: []byte("session"), signers: map[string]ssh.Signer{string(next.PublicKey().Marshal()): next}}
	if err := um.apply(context.Background(), f, packet([][]byte{next.PublicKey().Marshal()})); err != nil {
		t.Fatalf("fault-free authenticated update: %v", err)
	}
	if err := um.Finalize(nil); err != nil {
		t.Fatal(err)
	}
	if len(um.Transitions()) != 1 {
		t.Fatal("fault-free update missing receipt")
	}
}
