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

const (
	kauthACESize     = 16 + 4 + 4 // applicable guid, flags, rights
	kauthACEKindMask = 0xf
	kauthACEPermit   = 1

	kauthWriteData     = 1 << 2
	kauthDelete        = 1 << 4
	kauthAppendData    = 1 << 5
	kauthDeleteChild   = 1 << 6
	kauthWriteSecurity = 1 << 12
	kauthTakeOwnership = 1 << 13
	kauthGenericAll    = 1 << 21
	kauthGenericWrite  = 1 << 23

	// kauthMutateRights are the rights that let a principal create, remove,
	// or rename entries in a directory, or change who may.
	kauthMutateRights = kauthWriteData | kauthDelete | kauthAppendData | kauthDeleteChild |
		kauthWriteSecurity | kauthTakeOwnership | kauthGenericAll | kauthGenericWrite
)

func extendedSecurityAttrs() unix.Attrlist {
	return unix.Attrlist{
		Bitmapcount: unix.ATTR_BIT_MAP_COUNT,
		Commonattr:  unix.ATTR_CMN_RETURNED_ATTRS | unix.ATTR_CMN_EXTENDED_SECURITY,
	}
}

// rejectAccessACL refuses a file or directory with a populated access ACL.
// Mode 0600/0700 is not enough: chmod +a can grant another account
// access. macOS stores ACLs in the extended-security attribute, not
// as a listxattr name, so this uses getattrlist.
func rejectAccessACL(path string) error {
	attrList := extendedSecurityAttrs()
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

// rejectAncestorACL refuses an ancestor directory whose ACL permits writing,
// deleting, or changing permissions. Deny entries, including the stock
// "everyone deny delete" on home directories, and read-only permits do not
// let anyone rename the validated path.
func rejectAncestorACL(fd int) error {
	attrList := extendedSecurityAttrs()
	buf := make([]byte, 8192)
	r1, _, errno := unix.Syscall6(
		unix.SYS_FGETATTRLIST,
		uintptr(fd),
		uintptr(unsafe.Pointer(&attrList)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		0,
		0,
	)
	if r1 != 0 || errno != 0 {
		return ErrUnsafePermissions
	}
	if aclGrantsMutation(buf) {
		return ErrUnsafePermissions
	}
	return nil
}

// parseACL decodes a getattrlist reply for ATTR_CMN_RETURNED_ATTRS and
// ATTR_CMN_EXTENDED_SECURITY. It returns the raw ACEs (nil when the reply
// proves there are none) and false for a malformed or truncated reply.
func parseACL(buf []byte) (aces []byte, ok bool) {
	if len(buf) < attrRefOffset {
		return nil, false
	}
	total := binary.NativeEndian.Uint32(buf[0:4])
	if total < attrRefOffset || int64(total) > int64(len(buf)) {
		return nil, false
	}
	returned := binary.NativeEndian.Uint32(buf[4:8])
	if returned&unix.ATTR_CMN_EXTENDED_SECURITY == 0 {
		return nil, true
	}
	// attrreference_t: int32 offset (relative to this field) and uint32 length.
	if int64(total) < int64(attrRefOffset)+8 {
		return nil, false
	}
	off := int32(binary.NativeEndian.Uint32(buf[attrRefOffset : attrRefOffset+4]))
	length := binary.NativeEndian.Uint32(buf[attrRefOffset+4 : attrRefOffset+8])
	start := int64(attrRefOffset) + int64(off)
	end := start + int64(length)
	if off < 0 || start < int64(attrRefOffset)+8 || end > int64(total) {
		return nil, false
	}
	data := buf[start:end]
	if len(data) < kauthFilesecACLOffset+4 {
		return nil, false
	}
	if binary.NativeEndian.Uint32(data[0:4]) != kauthFilesecMagic {
		return nil, false
	}
	count := binary.NativeEndian.Uint32(data[kauthFilesecACLOffset : kauthFilesecACLOffset+4])
	if count == kauthFilesecNoACL || count == 0 {
		return nil, true
	}
	if len(data) < kauthFilesecACLOffset+8 {
		return nil, false
	}
	body := data[kauthFilesecACLOffset+8:]
	if uint64(count)*kauthACESize > uint64(len(body)) {
		return nil, false
	}
	return body[:int(count)*kauthACESize], true
}

// populatedACL reports true unless the reply proves the object has no ACL
// entries; malformed replies fail closed.
func populatedACL(buf []byte) bool {
	aces, ok := parseACL(buf)
	return !ok || len(aces) > 0
}

// aclGrantsMutation reports true when any permit entry carries a right that
// lets its principal create, delete, or rename entries or change security.
// Malformed replies fail closed.
func aclGrantsMutation(buf []byte) bool {
	aces, ok := parseACL(buf)
	if !ok {
		return true
	}
	for len(aces) >= kauthACESize {
		flags := binary.NativeEndian.Uint32(aces[16:20])
		rights := binary.NativeEndian.Uint32(aces[20:24])
		if flags&kauthACEKindMask == kauthACEPermit && rights&kauthMutateRights != 0 {
			return true
		}
		aces = aces[kauthACESize:]
	}
	return false
}
