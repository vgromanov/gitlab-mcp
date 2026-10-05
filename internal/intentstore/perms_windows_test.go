//go:build windows

package intentstore

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMakeParentsCreatesOwnerOnlyChain(t *testing.T) {
	leaf := filepath.Join(t.TempDir(), "a", "b", "c")
	if err := makeParents(leaf); err != nil {
		t.Fatalf("makeParents: %v", err)
	}
	for _, p := range []string{filepath.Dir(filepath.Dir(leaf)), filepath.Dir(leaf), leaf} {
		if err := verifyOwnerOnly(p, true); err != nil {
			t.Fatalf("verifyOwnerOnly(%s): %v", p, err)
		}
	}
}

func TestMakeParentsRejectsJunctionAncestor(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J unavailable: %v: %s", err, out)
	}
	err := makeParents(filepath.Join(link, "child"))
	if !errors.Is(err, ErrSymlink) {
		t.Fatalf("want ErrSymlink, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(target, "child")); statErr == nil {
		t.Fatal("directory was created through the junction")
	}
}
