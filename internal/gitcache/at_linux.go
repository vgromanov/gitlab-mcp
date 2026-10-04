package gitcache

import "syscall"

func openatDir(dirfd int, name string) (int, error) {
	return syscall.Openat(dirfd, name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
}

func openatNoFollow(dirfd int, name string, flags int, perm uint32) (int, error) {
	return syscall.Openat(dirfd, name, flags|syscall.O_NOFOLLOW, perm)
}

func dupTo(oldfd, newfd int) error {
	return syscall.Dup3(oldfd, newfd, 0)
}

func mapAnon(n uintptr) (uintptr, error) {
	// PROT_NONE so the mapping is virtual only. MAP_NORESERVE avoids commit charge.
	const protNone = 0
	const flags = 0x02 | 0x20 | 0x4000 // MAP_PRIVATE|MAP_ANONYMOUS|MAP_NORESERVE
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
