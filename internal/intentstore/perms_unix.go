//go:build unix

package intentstore

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func checkDirAccess(_ string, info os.FileInfo) error {
	if info.Mode().Perm() != 0o700 {
		return ErrUnsafePermissions
	}
	return checkOwner(info)
}

func checkFileAccess(_ string, info os.FileInfo) error {
	perm := info.Mode().Perm()
	if perm&0o200 == 0 {
		return ErrReadOnly
	}
	if perm != 0o600 {
		return ErrUnsafePermissions
	}
	return checkOwner(info)
}

// checkOwner rejects a directory or store file owned by another user.
// Mode 0700/0600 is not enough when this process is root: the directory
// owner can still replace intent.db and its sidecars.
func checkOwner(info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return ErrUnsafePermissions
	}
	if st.Uid != uint32(os.Geteuid()) {
		return ErrUnsafePermissions
	}
	return nil
}

func establishPrivate(path string, dir bool) error {
	mode := os.FileMode(0o600)
	if dir {
		mode = 0o700
	}
	return os.Chmod(path, mode)
}

// rejectSymlinkComponents refuses a symlink in any component of path.
// Descriptor traversal does not follow intermediate symlinks. macOS uses
// /var, /tmp, and /etc as symlinks to /private; those ancestors are the only
// exception, so a symlink created under them is still rejected.
func rejectSymlinkComponents(path string) error {
	fd, base, err := openParent(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstatat(fd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return ErrSymlink
	}
	return nil
}

func createExclusive(path string) error {
	fd, base, err := openParent(path)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	file, err := unix.Openat(fd, base, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	return unix.Close(file)
}

func makeParents(path string) error {
	clean := filepath.Clean(path)
	if clean == "/" || clean == "." {
		return nil
	}
	parts := splitPath(clean)
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	built := "/"
	for _, part := range parts {
		full := "/" + part
		if built != "/" {
			full = built + "/" + part
		}
		var st unix.Stat_t
		err := unix.Fstatat(fd, part, &st, unix.AT_SYMLINK_NOFOLLOW)
		if os.IsNotExist(err) {
			if err := unix.Mkdirat(fd, part, 0o700); err != nil && !os.IsExist(err) {
				return err
			}
			if err := unix.Fchmodat(fd, part, 0o700, 0); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		nfd, err := openComponent(fd, part, full)
		if err != nil {
			return err
		}
		_ = unix.Close(fd)
		fd = nfd
		built = full
	}
	return nil
}

// openParent opens the parent directory of path without following a symlink
// other than the macOS system ancestors. The caller closes the returned fd.
func openParent(path string) (int, string, error) {
	clean := filepath.Clean(path)
	parts := splitPath(clean)
	if len(parts) == 0 {
		return -1, "", ErrUnsafePermissions
	}
	base := parts[len(parts)-1]
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", err
	}
	built := "/"
	for _, part := range parts[:len(parts)-1] {
		full := "/" + part
		if built != "/" {
			full = built + "/" + part
		}
		nfd, err := openComponent(fd, part, full)
		if err != nil {
			_ = unix.Close(fd)
			return -1, "", err
		}
		_ = unix.Close(fd)
		fd = nfd
		built = full
	}
	return fd, base, nil
}

func openComponent(dirfd int, name, full string) (int, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return -1, err
	}
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		if !macosSystemSymlink(full) {
			return -1, ErrSymlink
		}
	} else if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return -1, ErrUnsafePermissions
	} else {
		flags |= unix.O_NOFOLLOW
	}
	return unix.Openat(dirfd, name, flags, 0)
}

func splitPath(clean string) []string {
	var parts []string
	for _, part := range strings.Split(clean, "/") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}

func macosSystemSymlink(path string) bool {
	if runtime.GOOS != "darwin" && runtime.GOOS != "ios" {
		return false
	}
	switch filepath.Clean(path) {
	case "/var", "/tmp", "/etc":
		return true
	default:
		return false
	}
}
