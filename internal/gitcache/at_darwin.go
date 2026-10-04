package gitcache

import (
	"syscall"
	_ "unsafe"
)

//go:linkname libcOpenat syscall.openat
func libcOpenat(fd int, path string, flags int, perm uint32) (int, error)

func openatDir(dirfd int, name string) (int, error) {
	return libcOpenat(dirfd, name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
}

func openatNoFollow(dirfd int, name string, flags int, perm uint32) (int, error) {
	return libcOpenat(dirfd, name, flags|syscall.O_NOFOLLOW, perm)
}

func dupTo(oldfd, newfd int) error {
	return syscall.Dup2(oldfd, newfd)
}

func mapAnon(n uintptr) (uintptr, error) {
	// PROT_NONE. MAP_ANON|MAP_PRIVATE. No bytes are written.
	const protNone = 0
	const flags = 0x1000 | 0x0002
	r, _, errno := syscall.Syscall6(syscall.SYS_MMAP, 0, n, protNone, flags, ^uintptr(0), 0)
	if errno != 0 {
		return 0, errno
	}
	return r, nil
}

func unmapAnon(addr, n uintptr) error {
	_, _, errno := syscall.Syscall(syscall.SYS_MUNMAP, addr, n, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
