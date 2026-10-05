//go:build unix

package intentstore

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

func TestWritableAncestorRejected(t *testing.T) {
	base := privateDir(t)
	shared := filepath.Join(base, "shared")
	if err := os.Mkdir(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	priv := filepath.Join(shared, "priv")
	if err := os.Mkdir(priv, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(priv, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Config{Path: filepath.Join(priv, "intent.db")}); !errors.Is(err, ErrUnsafePermissions) {
		t.Fatalf("writable ancestor: %v", err)
	}
	if err := os.Chmod(shared, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	s, err := Open(Config{Path: filepath.Join(priv, "sticky.db")})
	if err != nil {
		t.Fatalf("sticky ancestor: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
}

func TestMkdirRaceDoesNotChmodSymlinkTarget(t *testing.T) {
	base := privateDir(t)
	sticky := filepath.Join(base, "tmp")
	if err := os.Mkdir(sticky, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sticky, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	afterParentMissing = func(parent, part string) {
		afterParentMissing = nil
		if err := os.Symlink(target, filepath.Join(parent, part)); err != nil {
			t.Errorf("plant symlink: %v", err)
		}
	}
	t.Cleanup(func() { afterParentMissing = nil })
	_, err := Open(Config{Path: filepath.Join(sticky, "missing", "intent.db")})
	if !errors.Is(err, ErrSymlink) {
		t.Fatalf("mkdir race: %v", err)
	}
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("target mode %o, chmod followed the symlink", info.Mode().Perm())
	}
}

func TestAccessACLRejected(t *testing.T) {
	dir := privateDir(t)
	path := filepath.Join(dir, "intent.db")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	grantAccessACL(t, path)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkFileAccess(path, info); !errors.Is(err, ErrUnsafePermissions) {
		t.Fatalf("file ACL: %v", err)
	}

	parent := privateDir(t)
	grantAccessACL(t, parent)
	dinfo, err := os.Lstat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkDirAccess(parent, dinfo); !errors.Is(err, ErrUnsafePermissions) {
		t.Fatalf("parent ACL: %v", err)
	}
}

func TestOpenRejectsAccessACL(t *testing.T) {
	cfg, _ := fixedNow(t)
	s := openStore(t, cfg)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	grantAccessACL(t, cfg.Path)
	if _, err := Open(cfg); !errors.Is(err, ErrUnsafePermissions) {
		t.Fatalf("file ACL open: %v", err)
	}

	cfg2, _ := fixedNow(t)
	grantAccessACL(t, filepath.Dir(cfg2.Path))
	if _, err := Open(cfg2); !errors.Is(err, ErrUnsafePermissions) {
		t.Fatalf("parent ACL open: %v", err)
	}
}

func grantAccessACL(t *testing.T, path string) {
	t.Helper()
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin", "ios":
		cmd = exec.Command("chmod", "+a", "everyone allow read", path)
	case "linux":
		if _, err := exec.LookPath("setfacl"); err != nil {
			t.Skip("setfacl not installed")
		}
		cmd = exec.Command("setfacl", "-m", "u:nobody:r", path)
	default:
		t.Skip("no ACL grant helper")
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("grant ACL: %v\n%s", err, out)
	}
}

func TestOpenRejectsDotDotPastSymlink(t *testing.T) {
	base := privateDir(t)
	safe := filepath.Join(base, "safe")
	dbdir := filepath.Join(safe, "db")
	evil := filepath.Join(base, "evil")
	for _, dir := range []string{safe, dbdir, evil} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(safe, "link")
	if err := os.Symlink(evil, link); err != nil {
		t.Fatal(err)
	}
	// Clean would walk /safe/db while SQLite would open under the link.
	attack := link + "/../db/intent.db"
	if _, err := Open(Config{Path: attack}); err == nil {
		t.Fatal("accepted .. after symlink")
	}
	if _, err := os.Lstat(filepath.Join(evil, "intent.db")); err == nil {
		t.Fatal("opened through the symlink")
	}
	s, err := Open(Config{Path: filepath.Join(dbdir, "intent.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
}

func TestAncestorOwnedByOtherUserRejected(t *testing.T) {
	base := privateDir(t)
	path := filepath.Join(base, "store", "intent.db")
	prev := ancestorUID
	defer func() { ancestorUID = prev }()
	ancestorUID = func() uint32 { return uint32(os.Geteuid()) + 1 }
	if _, err := Open(Config{Path: path}); !errors.Is(err, ErrUnsafePermissions) {
		t.Fatalf("ancestor owned by another user: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(base, "store")); err == nil {
		t.Fatal("parent was created beneath an untrusted ancestor")
	}
	ancestorUID = prev
	s, err := Open(Config{Path: path})
	if err != nil {
		t.Fatalf("own ancestors: %v", err)
	}
	_ = s.Close()
}

func grantAncestorACL(t *testing.T, path string, write bool) {
	t.Helper()
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin", "ios":
		rule := "everyone allow list,search,readattr"
		if write {
			rule = "everyone allow list,search,add_file,add_subdirectory,delete_child"
		}
		cmd = exec.Command("chmod", "+a", rule, path)
	case "linux":
		if _, err := exec.LookPath("setfacl"); err != nil {
			t.Skip("setfacl not installed")
		}
		perm := "rx"
		if write {
			perm = "rwx"
		}
		cmd = exec.Command("setfacl", "-m", "u:nobody:"+perm, path)
	default:
		t.Skip("no ACL grant helper")
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("grant ACL unsupported here: %v\n%s", err, out)
	}
}

func TestAncestorWriteACLRejected(t *testing.T) {
	base := privateDir(t)
	grantAncestorACL(t, base, true)
	path := filepath.Join(base, "store", "intent.db")
	if _, err := Open(Config{Path: path}); !errors.Is(err, ErrUnsafePermissions) {
		t.Fatalf("ancestor with write ACL: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(base, "store")); err == nil {
		t.Fatal("parent was created beneath an ancestor with a write ACL")
	}
}

func TestAncestorReadOnlyACLAccepted(t *testing.T) {
	base := privateDir(t)
	grantAncestorACL(t, base, false)
	s, err := Open(Config{Path: filepath.Join(base, "store", "intent.db")})
	if err != nil {
		t.Fatalf("ancestor with read-only ACL: %v", err)
	}
	_ = s.Close()
}

func TestAncestorDenyDeleteACLAccepted(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("deny-delete is the stock macOS home directory ACL")
	}
	base := privateDir(t)
	if out, err := exec.Command("chmod", "+a", "everyone deny delete", base).CombinedOutput(); err != nil {
		t.Skipf("chmod +a: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("chmod", "-N", base).Run() })
	s, err := Open(Config{Path: filepath.Join(base, "store", "intent.db")})
	if err != nil {
		t.Fatalf("ancestor with deny-delete ACL: %v", err)
	}
	_ = s.Close()
}
