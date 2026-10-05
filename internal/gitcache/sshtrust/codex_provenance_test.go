//go:build linux || darwin

package sshtrust

import (
	"context"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshconfig"
	"golang.org/x/crypto/ssh/knownhosts"
	"os"
	"testing"
)

func TestCodexEffectiveKnownHostContentProvenance(t *testing.T) {
	addr := "git.example:7999"
	a, b := key(t), key(t)
	p := file(t, []byte(knownhosts.Line([]string{addr}, a.PublicKey())+"\n"))
	cfg := sshconfig.Config{User: "git", StrictHostKeyChecking: "yes", UpdateHostKeys: "no", UserKnownHostsFiles: []string{p}}
	first, e := New(context.Background(), cfg, addr)
	if e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(knownhosts.Line([]string{addr}, b.PublicKey())+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	second, e := New(context.Background(), cfg, addr)
	if e != nil {
		t.Fatal(e)
	}
	if first.Provenance() == second.Provenance() {
		t.Fatal("same-path public trust content not bound")
	}
	cfg.StrictHostKeyChecking = "accept-new"
	third, e := New(context.Background(), cfg, addr)
	if e != nil {
		t.Fatal(e)
	}
	if third.Provenance() == second.Provenance() {
		t.Fatal("effective configured policy not bound")
	}
}
