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
