//go:build linux || darwin

package gitcache

import (
	"context"
	"reflect"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
)

// TestPublishKeepsSingleObjectsMap ensures PublishGeneration indexes from an
// existing IndexedPack without a second decode/output map. Generation.Objects
// must be the same map as ip.Objects.
func TestPublishKeepsSingleObjectsMap(t *testing.T) {
	root := regressionRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(context.Background())
	ip, h := tinyBlobPack(t)
	gen, err := mgr.PublishGeneration(context.Background(), "gfp", "ns", h, h, h, ip)
	if err != nil {
		t.Fatal(err)
	}
	if gen.Objects == nil || len(gen.Objects) != len(ip.Objects) {
		t.Fatalf("objects missing")
	}
	if reflect.ValueOf(gen.Objects).Pointer() != reflect.ValueOf(ip.Objects).Pointer() {
		t.Fatal("PublishGeneration retained a second Objects map")
	}
	if int64(ip.PackByteCount) > bounds.MaxRetainedInput {
		t.Fatal("input over budget")
	}
	if ip.RetainedOutputBytes() > bounds.MaxRetainedOutput {
		t.Fatal("output over budget")
	}
}
