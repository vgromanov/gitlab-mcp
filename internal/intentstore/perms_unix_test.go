//go:build unix

package intentstore

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

type ownerInfo struct {
	os.FileInfo
	mode os.FileMode
	sys  any
}

func (o ownerInfo) Mode() os.FileMode { return o.mode }
func (o ownerInfo) Sys() any          { return o.sys }

func TestOwnerMustMatchEffectiveUser(t *testing.T) {
	dir := privateDir(t)
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkDirAccess(dir, info); err != nil {
		t.Fatalf("owned dir: %v", err)
	}
	path := filepath.Join(dir, "intent.db")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkFileAccess(path, file); err != nil {
		t.Fatalf("owned file: %v", err)
	}

	other := uint32(os.Geteuid()) ^ 1
	foreignDir := ownerInfo{mode: 0o700, sys: &syscall.Stat_t{Uid: other}}
	if err := checkDirAccess(dir, foreignDir); !errors.Is(err, ErrUnsafePermissions) {
		t.Fatalf("foreign dir: %v", err)
	}
	foreignFile := ownerInfo{mode: 0o600, sys: &syscall.Stat_t{Uid: other}}
	if err := checkFileAccess(path, foreignFile); !errors.Is(err, ErrUnsafePermissions) {
		t.Fatalf("foreign file: %v", err)
	}
	if err := checkDirAccess(dir, ownerInfo{mode: 0o700, sys: nil}); !errors.Is(err, ErrUnsafePermissions) {
		t.Fatalf("missing stat: %v", err)
	}
	if err := checkFileAccess(path, ownerInfo{mode: 0o600, sys: "no-stat"}); !errors.Is(err, ErrUnsafePermissions) {
		t.Fatalf("wrong stat: %v", err)
	}
}
