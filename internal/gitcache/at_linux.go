//go:build linux

package gitcache

import "syscall"

func openatDir(dirfd int, name string) (int, error) {
	return syscall.Openat(dirfd, name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
}

func openatNoFollow(dirfd int, name string, flags int, perm uint32) (int, error) {
	return syscall.Openat(dirfd, name, flags|syscall.O_NOFOLLOW, perm)
}

func mkdirat(dirfd int, name string, perm uint32) error {
	return syscall.Mkdirat(dirfd, name, perm)
}

func unlinkat(dirfd int, name string, flags int) error {
	return syscall.Unlinkat(dirfd, name, flags)
}

func renameat(olddirfd int, oldpath string, newdirfd int, newpath string) error {
	return syscall.Renameat(olddirfd, oldpath, newdirfd, newpath)
}

func fstat(fd int) (syscall.Stat_t, error) {
	var st syscall.Stat_t
	err := syscall.Fstat(fd, &st)
	return st, err
}
