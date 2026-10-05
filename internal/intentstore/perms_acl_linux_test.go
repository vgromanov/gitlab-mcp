//go:build linux

package intentstore

import (
	"encoding/binary"
	"testing"
)

func posixACL(entries ...[3]uint32) []byte {
	buf := make([]byte, 4+len(entries)*posixACLEntrySiz)
	binary.LittleEndian.PutUint32(buf, posixACLVersion)
	for i, e := range entries {
		o := 4 + i*posixACLEntrySiz
		binary.LittleEndian.PutUint16(buf[o:], uint16(e[0]))
		binary.LittleEndian.PutUint16(buf[o+2:], uint16(e[1]))
		binary.LittleEndian.PutUint32(buf[o+4:], e[2])
	}
	return buf
}

func TestPosixACLGrantsWrite(t *testing.T) {
	const (
		userObj  = 0x01
		groupObj = 0x04
		other    = 0x20
	)
	base := [][3]uint32{{userObj, 7, 0}, {groupObj, 5, 0}, {other, 0, 0}}
	with := func(extra ...[3]uint32) []byte { return posixACL(append(append([][3]uint32{}, base...), extra...)...) }

	cases := []struct {
		name string
		acl  []byte
		want bool
	}{
		{"base entries only", posixACL(base...), false},
		{"named read-only user", with([3]uint32{posixACLUser, 5, 1234}, [3]uint32{posixACLMask, 5, 0}), false},
		{"named writable user", with([3]uint32{posixACLUser, 7, 1234}, [3]uint32{posixACLMask, 7, 0}), true},
		{"named writable group", with([3]uint32{posixACLGroup, 6, 1234}, [3]uint32{posixACLMask, 7, 0}), true},
		{"write masked off", with([3]uint32{posixACLUser, 7, 1234}, [3]uint32{posixACLMask, 5, 0}), false},
		{"writable root entry", with([3]uint32{posixACLUser, 7, 0}, [3]uint32{posixACLMask, 7, 0}), false},
		{"writable service entry", with([3]uint32{posixACLUser, 7, 42}, [3]uint32{posixACLMask, 7, 0}), false},
		{"short", []byte{1, 2}, true},
		{"bad version", append([]byte{9, 0, 0, 0}, make([]byte, 8)...), true},
		{"ragged", append(posixACL(base...), 1), true},
	}
	for _, c := range cases {
		if got := posixACLGrantsWrite(c.acl, 42); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
