//go:build unix

package sshfile_test

import (
	"os"
	"path/filepath"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshfile"
)

func TestOpenRegularAcceptsAndRejects(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "key")
	if err := os.WriteFile(p, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, st, err := sshfile.OpenRegular(p, 1024)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if st.Size() != 6 {
		t.Fatalf("size %d", st.Size())
	}
	if _, _, err := sshfile.OpenRegular(p, 2); err == nil {
		t.Fatal("oversize accepted")
	}
	if _, _, err := sshfile.OpenRegular(filepath.Join(dir, "missing"), 1024); err == nil {
		t.Fatal("missing accepted")
	}
	subdir := filepath.Join(dir, "d")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sshfile.OpenRegular(subdir, 1024); err == nil {
		t.Fatal("directory accepted")
	}
}
