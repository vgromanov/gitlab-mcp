//go:build linux

package intentstore

import (
	"encoding/binary"

	"golang.org/x/sys/unix"
)

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

const (
	posixACLVersion  = 2
	posixACLUser     = 0x02
	posixACLGroup    = 0x08
	posixACLMask     = 0x10
	posixACLWrite    = 0x02
	posixACLEntrySiz = 8
)

// rejectAncestorACL refuses an ancestor directory whose POSIX access ACL
// grants write to a named user or group other than root and this process's
// user. Such a principal can rename the validated path even when the mode
// bits look private.
func rejectAncestorACL(fd int) error {
	buf := make([]byte, 4096)
	n, err := unix.Fgetxattr(fd, "system.posix_acl_access", buf)
	switch err {
	case nil:
	case unix.ERANGE:
		return ErrUnsafePermissions
	default:
		return nil
	}
	if posixACLGrantsWrite(buf[:n], ancestorUID()) {
		return ErrUnsafePermissions
	}
	return nil
}

func posixACLGrantsWrite(acl []byte, trusted uint32) bool {
	if len(acl) < 4 || binary.LittleEndian.Uint32(acl) != posixACLVersion || (len(acl)-4)%posixACLEntrySiz != 0 {
		return true
	}
	type named struct {
		perm uint16
		id   uint32
	}
	var entries []named
	mask := uint16(0x7)
	for e := acl[4:]; len(e) >= posixACLEntrySiz; e = e[posixACLEntrySiz:] {
		tag := binary.LittleEndian.Uint16(e[0:2])
		perm := binary.LittleEndian.Uint16(e[2:4])
		id := binary.LittleEndian.Uint32(e[4:8])
		switch tag {
		case posixACLMask:
			mask = perm
		case posixACLUser, posixACLGroup:
			entries = append(entries, named{perm, id})
		}
	}
	for _, e := range entries {
		if e.id == 0 || e.id == trusted {
			continue
		}
		if e.perm&mask&posixACLWrite != 0 {
			return true
		}
	}
	return false
}
