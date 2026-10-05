//go:build darwin || ios

package intentstore

import (
	"encoding/binary"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	// kauthFilesecNoACL is KAUTH_FILESEC_NOACL: fsec_acl.acl_entrycount
	// for a file whose extended security carries owner/group GUIDs but no
	// ACL. Some filesystems return it, and getattrlist then still sets the
	// ATTR_CMN_EXTENDED_SECURITY bit in the returned attributes.
	kauthFilesecNoACL = 0xffffffff
	// KAUTH_FILESEC_MAGIC.
	kauthFilesecMagic = 0x012cc16d
	// kauth_filesec: magic, owner guid, group guid, then kauth_acl
	// {entrycount, flags, entries...}.
	kauthFilesecACLOffset = 4 + 16 + 16
	// attribute_set_t (5 x uint32) follows the leading length word when
	// ATTR_CMN_RETURNED_ATTRS is requested; the attrreference_t is next.
	attrRefOffset = 4 + 20
)

// rejectAccessACL refuses a file or directory with a populated access ACL.
// Mode 0600/0700 is not enough: chmod +a can grant another account
// access. macOS stores ACLs in the extended-security attribute, not
// as a listxattr name, so this uses getattrlist.
func rejectAccessACL(path string) error {
	attrList := unix.Attrlist{
		Bitmapcount: unix.ATTR_BIT_MAP_COUNT,
		Commonattr:  unix.ATTR_CMN_RETURNED_ATTRS | unix.ATTR_CMN_EXTENDED_SECURITY,
	}
	buf := make([]byte, 8192)
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
	if populatedACL(buf) {
		return ErrUnsafePermissions
	}
	return nil
}

// populatedACL parses a getattrlist reply for ATTR_CMN_RETURNED_ATTRS and
// ATTR_CMN_EXTENDED_SECURITY. It reports true unless the reply proves the
// object has no ACL entries; malformed or truncated replies fail closed.
func populatedACL(buf []byte) bool {
	if len(buf) < attrRefOffset {
		return true
	}
	total := binary.NativeEndian.Uint32(buf[0:4])
	if total < attrRefOffset || int64(total) > int64(len(buf)) {
		return true
	}
	returned := binary.NativeEndian.Uint32(buf[4:8])
	if returned&unix.ATTR_CMN_EXTENDED_SECURITY == 0 {
		return false
	}
	// attrreference_t: int32 offset (relative to this field) and uint32 length.
	if int64(total) < int64(attrRefOffset)+8 {
		return true
	}
	off := int32(binary.NativeEndian.Uint32(buf[attrRefOffset : attrRefOffset+4]))
	length := binary.NativeEndian.Uint32(buf[attrRefOffset+4 : attrRefOffset+8])
	start := int64(attrRefOffset) + int64(off)
	end := start + int64(length)
	if off < 0 || start < int64(attrRefOffset)+8 || end > int64(total) {
		return true
	}
	data := buf[start:end]
	if len(data) < kauthFilesecACLOffset+4 {
		return true
	}
	if binary.NativeEndian.Uint32(data[0:4]) != kauthFilesecMagic {
		return true
	}
	count := binary.NativeEndian.Uint32(data[kauthFilesecACLOffset : kauthFilesecACLOffset+4])
	// An empty ACL grants nothing, like the NOACL sentinel.
	return count != kauthFilesecNoACL && count != 0
}
