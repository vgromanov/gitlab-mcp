//go:build linux || darwin

package gitcache

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/pack"
)

func canonicalTempRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestManagerPublishLookupCorruptAndQuota(t *testing.T) {
	root := canonicalTempRoot(t)
	mgr, err := OpenManager(root, bounds.BrootBytes+bounds.GenerationCharge()*2)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer mgr.Close(context.Background())

	blob := pack.Object{Type: "blob", Data: []byte("hi")}
	blob.Hash = pack.HashObject("blob", blob.Data)
	packBytes, err := pack.Encode([]pack.Object{blob})
	if err != nil {
		t.Fatal(err)
	}
	ip, err := pack.DecodeIndexed(context.Background(), packBytes)
	if err != nil {
		t.Fatal(err)
	}
	gen, err := mgr.PublishGeneration(context.Background(), "gfp", "ns1", blob.Hash, blob.Hash, blob.Hash, ip)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	warm, err := mgr.lookupWarm(context.Background(), "gfp", "ns1")
	if err != nil || warm.ID != gen.ID {
		t.Fatalf("warm: %v %#v", err, warm)
	}
	if err := warm.Unpin(); err != nil {
		t.Fatal(err)
	}
	// corrupt pack
	genDir := filepath.Join(root, "generations", gen.ID)
	if err := os.WriteFile(filepath.Join(genDir, "pack.pack"), []byte("PACKCORRUPT"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.lookupWarm(context.Background(), "gfp", "ns1"); err == nil {
		t.Fatal("corrupt warm treated as miss/success")
	}
}

func TestSymlinkRootRejected(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenManager(link, bounds.DefaultQuotaBytes); err == nil {
		t.Fatal("symlink root accepted")
	}
}
