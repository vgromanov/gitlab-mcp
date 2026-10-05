package gitcache_test

import (
	"context"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache"
)

func TestDisabledServiceOpensNoRoot(t *testing.T) {
	svc, err := gitcache.OpenService(gitcache.ServiceConfig{Enabled: false, Root: "/no/such/cache/root"})
	if err != nil {
		t.Fatal(err)
	}
	if svc.Enabled() {
		t.Fatal("disabled service reported enabled")
	}
	if svc.Manager() != nil {
		t.Fatal("disabled service opened a manager")
	}
	if err := svc.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultOffDoesNotRequireRoot(t *testing.T) {
	svc, err := gitcache.OpenService(gitcache.ServiceConfig{})
	if err != nil || svc.Enabled() {
		t.Fatalf("default: enabled=%v err=%v", svc != nil && svc.Enabled(), err)
	}
}
