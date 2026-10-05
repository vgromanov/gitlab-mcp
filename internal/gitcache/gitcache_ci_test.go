//go:build gitcache_ci

package gitcache_test

import (
	"context"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache"
)

// Focused native CI entry: disabled-default and platform open smoke.
func TestCINativeSmoke(t *testing.T) {
	svc, err := gitcache.OpenService(gitcache.ServiceConfig{Enabled: false})
	if err != nil || svc.Enabled() {
		t.Fatalf("disabled: %v enabled=%v", err, svc != nil && svc.Enabled())
	}
	_ = svc.Close(context.Background())
}
