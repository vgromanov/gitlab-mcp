//go:build darwin || ios

package intentstore

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// rejectAccessACL refuses a file or directory with an access ACL.
// Mode 0600/0700 is not enough: chmod +a can grant another account
// access. macOS stores ACLs in the extended-security attribute, not
// as a listxattr name, so this uses getattrlist.
func rejectAccessACL(path string) error {
	attrList := unix.Attrlist{
		Bitmapcount: unix.ATTR_BIT_MAP_COUNT,
		Commonattr:  unix.ATTR_CMN_RETURNED_ATTRS | unix.ATTR_CMN_EXTENDED_SECURITY,
	}
	buf := make([]byte, 4096)
	p, err := unix.BytePtrFromString(path)
	if err != nil {
		return ErrUnsafePermissions
	}
	r1, _, errno := unix.Syscall6(
		unix.SYS_GETATTRLIST,
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&attrList)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		uintptr(unix.FSOPT_NOFOLLOW),
		0,
	)
	if r1 != 0 || errno != 0 {
		return ErrUnsafePermissions
	}
	if len(buf) < 8 {
		return ErrUnsafePermissions
	}
	returned := *(*uint32)(unsafe.Pointer(&buf[4]))
	if returned&unix.ATTR_CMN_EXTENDED_SECURITY != 0 {
		return ErrUnsafePermissions
	}
	return nil
}
