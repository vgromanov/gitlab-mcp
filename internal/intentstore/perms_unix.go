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

func identifyFile(path string) (fileID, error) {
	fd, base, err := openParent(path)
	if err != nil {
		return fileID{}, err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstatat(fd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fileID{}, err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return fileID{}, ErrSymlink
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return fileID{}, ErrUnsafePermissions
	}
	return fileID{dev: uint64(st.Dev), ino: uint64(st.Ino), ok: true}, nil
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
	if err := checkOpenedDir(fd); err != nil {
		return err
	}
	built := "/"
	for _, part := range parts {
		full := "/" + part
		if built != "/" {
			full = built + "/" + part
		}
		var st unix.Stat_t
		err := unix.Fstatat(fd, part, &st, unix.AT_SYMLINK_NOFOLLOW)
		if os.IsNotExist(err) {
			if afterParentMissing != nil {
				afterParentMissing(built, part)
			}
			if err := mkdirPrivate(fd, part); err != nil {
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

// afterParentMissing runs after a path component is observed missing and
// before Mkdirat. Tests use it to plant a symlink in that gap.
var afterParentMissing func(parent, part string)

// mkdirPrivate creates part and chmods the opened directory, not the path
// name. Fchmodat after Mkdirat EEXIST would follow a symlink an attacker
// planted in a sticky directory such as /tmp.
func mkdirPrivate(dirfd int, part string) error {
	err := unix.Mkdirat(dirfd, part, 0o700)
	if err != nil && !os.IsExist(err) {
		return err
	}
	if err != nil {
		// Lost the create. Do not chmod the name; openComponent will
		// refuse a symlink and accept a directory that already exists.
		return nil
	}
	nfd, err := unix.Openat(dirfd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(nfd)
	return unix.Fchmod(nfd, 0o700)
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
	if err := checkOpenedDir(fd); err != nil {
		_ = unix.Close(fd)
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
		if err := checkSharedDirMode(st); err != nil {
			return -1, err
		}
		flags |= unix.O_NOFOLLOW
	}
	nfd, err := unix.Openat(dirfd, name, flags, 0)
	if err != nil {
		return -1, err
	}
	if err := checkOpenedDir(nfd); err != nil {
		_ = unix.Close(nfd)
		return -1, err
	}
	return nfd, nil
}

// checkSharedDirMode rejects a non-sticky directory that group or other
// can write. Such an ancestor can be renamed and replaced with a symlink
// between the path walk and sql.Open.
func checkSharedDirMode(st unix.Stat_t) error {
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return ErrUnsafePermissions
	}
	if st.Mode&0o022 != 0 && st.Mode&unix.S_ISVTX == 0 {
		return ErrUnsafePermissions
	}
	return nil
}

func checkOpenedDir(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	return checkSharedDirMode(st)
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
