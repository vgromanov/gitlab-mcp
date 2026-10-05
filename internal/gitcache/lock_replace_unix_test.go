//go:build linux || darwin

package gitcache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
)

func TestReplacedRootLockDoesNotAdmitSecondManager(t *testing.T) {
	root := regressionRoot(t)
	a, err := OpenManager(root, bounds.DefaultQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	ip, h := tinyBlobPack(t)
	g, err := a.PublishGeneration(context.Background(), "fp", "ns", h, h, h, ip)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Pin(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = g.Unpin()
		_ = a.Close(context.Background())
	}()
	if other, err := OpenManager(root, bounds.DefaultQuotaBytes); !errors.Is(err, ErrBusy) {
		if other != nil {
			_ = other.Close(context.Background())
		}
		t.Fatalf("second open: %v", err)
	}
	if err := a.EvictGeneration(context.Background(), g.ID); !errors.Is(err, ErrPinned) {
		t.Fatalf("pinned evict: %v", err)
	}
	lockPath := filepath.Join(root, lockName)
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if other, err := OpenManager(root, bounds.DefaultQuotaBytes); !errors.Is(err, ErrBusy) {
		if other != nil {
			_ = other.Close(context.Background())
		}
		t.Fatalf("open after lock replacement: %v", err)
	}
	if err := a.EvictGeneration(context.Background(), g.ID); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("evict after lock replacement: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, gensDir, g.ID)); err != nil {
		t.Fatalf("pinned generation removed: %v", err)
	}
}
