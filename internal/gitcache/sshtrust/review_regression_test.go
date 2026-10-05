//go:build linux || darwin

package sshtrust

import (
	"context"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshconfig"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"testing"
)

func TestReviewDisabledTrustStorage(t *testing.T) {
	m, e := New(context.Background(), sshconfig.Config{StrictHostKeyChecking: "no", UpdateHostKeys: "yes"}, "git.example:7999")
	if e != nil {
		t.Fatal(e)
	}
	if e = m.Callback("git.example:7999", &net.TCPAddr{}, key(t).PublicKey()); e != nil {
		t.Fatal("disabled storage rejected approved unknown host")
	}
	if m.Verified() || m.eligible {
		t.Fatal("untrusted key promoted")
	}
}
func TestReviewCanceledTrustLoad(t *testing.T) {
	c, cancel := context.WithCancel(context.Background())
	cancel()
	p := file(t, nil)
	if _, e := New(c, sshconfig.Config{StrictHostKeyChecking: "yes", UpdateHostKeys: "yes", UserKnownHostsFiles: []string{p}}, "git.example:7999"); e == nil {
		t.Fatal("canceled trust load accepted")
	}
}
func TestReviewAttributeConflict(t *testing.T) {
	p := file(t, []byte("# original\n"))
	s, e := load(p)
	if e != nil {
		t.Fatal(e)
	}
	if e := unix.Setxattr(p, "user.native-ssh-test", []byte("changed"), 0); e != nil {
		t.Fatal(e)
	}
	if unchanged(s) {
		t.Error("completed xattr change undetected")
	}
	if e := replace(context.Background(), s, []byte("# replacement\n")); e == nil {
		t.Error("metadata conflict overwritten")
	}
	b, _ := os.ReadFile(p)
	if string(b) != "# original\n" {
		t.Error("content changed on metadata conflict")
	}
}
