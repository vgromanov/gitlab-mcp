//go:build linux || darwin

package sshtrust

import (
	"bytes"
	"context"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshconfig"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshfile"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"
)

type endlessGrowth struct{ read int }

func (r *endlessGrowth) Read(p []byte) (int, error) { clear(p); r.read += len(p); return len(p), nil }
func TestBoundedGrowthRead(t *testing.T) {
	r := &endlessGrowth{}
	if _, e := readBounded(context.Background(), r); e == nil || r.read != maxFileBytes+1 {
		t.Fatalf("growth read not bounded: %d", r.read)
	}
}
func TestSnapshotUserAndGlobalRemainImmutable(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(map[bool]string{false: "user", true: "global"}[global], func(t *testing.T) {
			addr := "git.example:7999"
			a, b := key(t), key(t)
			p := file(t, []byte(knownhosts.Line([]string{addr}, a.PublicKey())+"\n"))
			c := sshconfig.Config{StrictHostKeyChecking: "yes", UpdateHostKeys: "no"}
			if global {
				c.GlobalKnownHostsFiles = []string{p}
			} else {
				c.UserKnownHostsFiles = []string{p}
			}
			m, e := New(context.Background(), c, addr)
			if e != nil {
				t.Fatal(e)
			}
			os.WriteFile(p, []byte(knownhosts.Line([]string{addr}, b.PublicKey())+"\n"), 0600)
			remote := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 7999}
			if m.Callback(addr, remote, b.PublicKey()) == nil {
				t.Fatal("post-load bytes reached trust callback")
			}
			if e := m.Callback(addr, remote, a.PublicKey()); e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestMalformedOversizedAndNonregularSnapshots(t *testing.T) {
	for _, data := range [][]byte{[]byte("not a host record\n"), bytes.Repeat([]byte("x"), 64*1024+1), bytes.Repeat([]byte("#"), maxFileBytes+1)} {
		p := file(t, data)
		if _, e := New(context.Background(), sshconfig.Config{UpdateHostKeys: "no", UserKnownHostsFiles: []string{p}}, "git.example:7999"); e == nil {
			t.Fatal("invalid snapshot accepted")
		}
	}
	p := filepath.Join(t.TempDir(), "fifo")
	if e := unix.Mkfifo(p, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := load(p); e == nil {
		t.Fatal("nonregular replacement accepted")
	}
}
func TestMetadataPreservedAndConflictsDetected(t *testing.T) {
	p := file(t, []byte("# original\n"))
	if e := unix.Setxattr(p, "user.native-ssh-test", []byte("original"), 0); e != nil {
		t.Fatal(e)
	}
	s, e := load(p)
	if e != nil {
		t.Fatal(e)
	}
	if e := replace(context.Background(), s, []byte("# updated\n")); e != nil {
		t.Fatal(e)
	}
	after, e := load(p)
	if e != nil {
		t.Fatal(e)
	}
	a, b := s.info.Sys().(*syscall.Stat_t), after.info.Sys().(*syscall.Stat_t)
	if a.Uid != b.Uid || a.Gid != b.Gid || s.info.Mode() != after.info.Mode() || !reflect.DeepEqual(s.attrs, after.attrs) {
		t.Fatal("metadata not preserved")
	}
	// Completed mode change restored to original mode/mtime still changes ctime.
	time.Sleep(2 * time.Millisecond)
	os.Chmod(p, 0640)
	os.Chmod(p, 0600)
	os.Chtimes(p, after.info.ModTime(), after.info.ModTime())
	if unchanged(after) {
		t.Fatal("restored mode/mtime hid completed change")
	}
	// Verify ownership/group comparison even where chown is unavailable.
	copyStat := *b
	copyStat.Gid++
	if sameMetadata(after.info, alteredInfo{after.info, &copyStat}) {
		t.Fatal("group change ignored")
	}
	copyStat = *b
	copyStat.Uid++
	if sameMetadata(after.info, alteredInfo{after.info, &copyStat}) {
		t.Fatal("owner change ignored")
	}
}

type alteredInfo struct {
	os.FileInfo
	stat *syscall.Stat_t
}

func (i alteredInfo) Sys() any { return i.stat }
func TestDisabledUpdatesIgnoreMalformedRepeats(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		m := &Manager{cfg: sshconfig.Config{UpdateHostKeys: "yes"}}
		if disabled {
			m.cfg.UpdateHostKeys = "no"
			m.eligible = true
		}
		u := &Updates{manager: m}
		for i := 0; i < 2; i++ {
			u.handle(&ssh.Request{Type: announcement, Payload: []byte("invalid")})
		}
		if u.Error() != nil {
			t.Fatal("ignored feature caused failure")
		}
	}
}
func TestUnrelatedWildcardSecondaryFile(t *testing.T) {
	addr := "git.example:7999"
	a := key(t)
	p := file(t, []byte(knownhosts.Line([]string{addr}, a.PublicKey())+"\n"))
	other := file(t, []byte(knownhosts.Line([]string{"*.unrelated.test:7999"}, a.PublicKey())+"\n"))
	m, e := New(context.Background(), sshconfig.Config{StrictHostKeyChecking: "yes", UpdateHostKeys: "yes", UserKnownHostsFiles: []string{p, other}}, addr)
	if e != nil {
		t.Fatal(e)
	}
	if e := m.apply(context.Background(), &fakeProof{}, packet([][]byte{a.PublicKey().Marshal()})); e != nil {
		t.Fatal(e)
	}
}
func TestEnrollmentAndRotationRekey(t *testing.T) {
	addr := "git.example:7999"
	remote := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 7999}
	a, b := key(t), key(t)
	p := file(t, nil)
	m, e := New(context.Background(), sshconfig.Config{StrictHostKeyChecking: "no", UpdateHostKeys: "yes", UserKnownHostsFiles: []string{p}}, addr)
	if e != nil {
		t.Fatal(e)
	}
	if e = m.Callback(addr, remote, a.PublicKey()); e != nil {
		t.Fatal(e)
	}
	s, _ := load(p)
	if e = m.Callback(addr, remote, a.PublicKey()); e != nil {
		t.Fatal(e)
	}
	if !unchanged(s) || m.eligible || m.Verified() {
		t.Fatal("rekey reenrolled or promoted untrusted key")
	}
	trusted := manager(t, addr, p)
	if e = trusted.Callback(addr, remote, a.PublicKey()); e != nil {
		t.Fatal(e)
	}
	f := &fakeProof{session: []byte("session"), signers: map[string]ssh.Signer{string(b.PublicKey().Marshal()): b}}
	if e = trusted.apply(context.Background(), f, packet([][]byte{b.PublicKey().Marshal()})); e != nil {
		t.Fatal(e)
	}
	if e = trusted.Callback(addr, remote, b.PublicKey()); e != nil {
		t.Fatal("authenticated rotation not available on rekey")
	}
}
func TestEnrollmentPersistenceFailureRetainsDecision(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing-parent", "known_hosts")
	m, e := New(context.Background(), sshconfig.Config{StrictHostKeyChecking: "accept-new", UpdateHostKeys: "yes", UserKnownHostsFiles: []string{p}}, "git.example:7999")
	if e != nil {
		t.Fatal(e)
	}
	if e = m.Callback("git.example:7999", &net.TCPAddr{}, key(t).PublicKey()); e != nil {
		t.Fatal(e)
	}
	if m.Verified() || m.eligible || m.UpdateStatus() != "enrollment_not_persisted" {
		t.Fatal("incorrect failed-enrollment state")
	}
}

var _ io.Reader = (*endlessGrowth)(nil)

func TestFileGrowthAfterDescriptorValidationIsBounded(t *testing.T) {
	p := file(t, []byte("# small\n"))
	f, _, e := sshfile.OpenRegular(p, maxFileBytes)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	if e := os.WriteFile(p, bytes.Repeat([]byte("#"), maxFileBytes+1000), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := readBounded(context.Background(), f); e == nil {
		t.Fatal("growth after descriptor validation accepted")
	}
	offset, e := f.Seek(0, io.SeekCurrent)
	if e != nil || offset != maxFileBytes+1 {
		t.Fatal("growth read exceeded limit")
	}
}
