//go:build darwin

package gitcache

import (
	"syscall"

	"golang.org/x/sys/unix"
)

func openatDir(dirfd int, name string) (int, error) {
	return unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
}

func openatNoFollow(dirfd int, name string, flags int, perm uint32) (int, error) {
	return unix.Openat(dirfd, name, flags|unix.O_NOFOLLOW, perm)
}

func mkdirat(dirfd int, name string, perm uint32) error {
	return unix.Mkdirat(dirfd, name, perm)
}

func unlinkat(dirfd int, name string, flags int) error {
	return unix.Unlinkat(dirfd, name, flags)
}

func renameat(olddirfd int, oldpath string, newdirfd int, newpath string) error {
	return unix.Renameat(olddirfd, oldpath, newdirfd, newpath)
}

func fstat(fd int) (syscall.Stat_t, error) {
	var st syscall.Stat_t
	err := syscall.Fstat(fd, &st)
	return st, err
}
