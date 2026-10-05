//go:build darwin || ios

package intentstore

import (
	"encoding/binary"
	"testing"

	"golang.org/x/sys/unix"
)

// aclReply builds a getattrlist reply carrying an extended-security
// attrreference_t followed by a kauth_filesec with the given ACL entry count.
func aclReply(entryCount uint32, aces int) []byte {
	filesec := make([]byte, kauthFilesecACLOffset+8+aces*24)
	binary.NativeEndian.PutUint32(filesec[0:], kauthFilesecMagic)
	binary.NativeEndian.PutUint32(filesec[kauthFilesecACLOffset:], entryCount)
	buf := make([]byte, attrRefOffset+8+len(filesec))
	binary.NativeEndian.PutUint32(buf[0:], uint32(len(buf)))
	binary.NativeEndian.PutUint32(buf[4:], unix.ATTR_CMN_EXTENDED_SECURITY)
	binary.NativeEndian.PutUint32(buf[attrRefOffset:], 8)
	binary.NativeEndian.PutUint32(buf[attrRefOffset+4:], uint32(len(filesec)))
	copy(buf[attrRefOffset+8:], filesec)
	return buf
}

func TestPopulatedACLNoACLSentinelAccepted(t *testing.T) {
	if populatedACL(aclReply(kauthFilesecNoACL, 0)) {
		t.Fatal("KAUTH_FILESEC_NOACL sentinel must not count as an ACL")
	}
	if populatedACL(aclReply(0, 0)) {
		t.Fatal("zero-entry ACL grants nothing")
	}
}

func TestPopulatedACLEntriesRejected(t *testing.T) {
	if !populatedACL(aclReply(1, 1)) {
		t.Fatal("populated ACL must be rejected")
	}
}

func TestPopulatedACLNoExtendedSecurity(t *testing.T) {
	buf := make([]byte, 64)
	binary.NativeEndian.PutUint32(buf[0:], uint32(attrRefOffset))
	if populatedACL(buf) {
		t.Fatal("no extended-security attribute means no ACL")
	}
}

func TestPopulatedACLMalformedFailsClosed(t *testing.T) {
	good := aclReply(kauthFilesecNoACL, 0)

	badMagic := append([]byte(nil), good...)
	binary.NativeEndian.PutUint32(badMagic[attrRefOffset+8:], 0)
	if !populatedACL(badMagic) {
		t.Fatal("bad filesec magic must fail closed")
	}

	overrun := append([]byte(nil), good...)
	binary.NativeEndian.PutUint32(overrun[attrRefOffset+4:], 1<<20)
	if !populatedACL(overrun) {
		t.Fatal("overrunning attrreference must fail closed")
	}

	short := append([]byte(nil), good[:attrRefOffset+4]...)
	if !populatedACL(short) {
		t.Fatal("truncated reply must fail closed")
	}
}

func aclReplyWithACEs(rights ...[2]uint32) []byte {
	reply := aclReply(uint32(len(rights)), len(rights))
	base := attrRefOffset + 8 + kauthFilesecACLOffset + 8
	for i, r := range rights {
		binary.NativeEndian.PutUint32(reply[base+i*kauthACESize+16:], r[0])
		binary.NativeEndian.PutUint32(reply[base+i*kauthACESize+20:], r[1])
	}
	return reply
}

func TestAncestorACLOnlyMutatingPermitsRejected(t *testing.T) {
	denyDelete := [2]uint32{2, kauthDelete}
	readOnly := [2]uint32{1, 1 << 1}
	if aclGrantsMutation(aclReply(kauthFilesecNoACL, 0)) {
		t.Fatal("no ACL must pass")
	}
	if aclGrantsMutation(aclReplyWithACEs(denyDelete)) {
		t.Fatal("stock deny-delete ACL must pass")
	}
	if aclGrantsMutation(aclReplyWithACEs(denyDelete, readOnly)) {
		t.Fatal("deny plus read-only permit must pass")
	}
	for name, rights := range map[string]uint32{
		"add_file": kauthWriteData, "add_subdirectory": kauthAppendData,
		"delete": kauthDelete, "delete_child": kauthDeleteChild,
		"writesecurity": kauthWriteSecurity, "chown": kauthTakeOwnership,
		"generic_write": kauthGenericWrite, "generic_all": kauthGenericAll,
	} {
		if !aclGrantsMutation(aclReplyWithACEs(denyDelete, [2]uint32{1, rights})) {
			t.Fatalf("permit %s must be rejected", name)
		}
	}
	truncated := aclReply(5, 1)
	if !aclGrantsMutation(truncated) {
		t.Fatal("entry count beyond the data must fail closed")
	}
}
