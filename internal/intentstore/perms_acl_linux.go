//go:build linux

package intentstore

import "golang.org/x/sys/unix"

// rejectAccessACL refuses a POSIX access ACL. Mode 0600/0700 can still
// grant another account access through system.posix_acl_access.
func rejectAccessACL(path string) error {
	if hasPosixACL(path, "system.posix_acl_access") {
		return ErrUnsafePermissions
	}
	return nil
}

func hasPosixACL(path, name string) bool {
	_, err := unix.Lgetxattr(path, name, nil)
	switch err {
	case nil, unix.ERANGE:
		return true
	default:
		return false
	}
}
