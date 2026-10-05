//go:build unix

package sshfile

import (
	"os"
	"syscall"
	"testing"
)

type info struct {
	os.FileInfo
	st   syscall.Stat_t
	mode os.FileMode
}

func (i info) Sys() any          { return &i.st }
func (i info) Mode() os.FileMode { return i.mode }
func TestOpenSSHOwnershipRules(t *testing.T) {
	for _, tc := range []struct {
		uid             uint32
		mode            os.FileMode
		config, private bool
	}{{uint32(os.Getuid()), 0600, true, true}, {uint32(os.Getuid()), 0644, true, false}, {uint32(os.Getuid()), 0660, false, false}, {0, 0644, true, os.Getuid() != 0}, {uint32(os.Getuid() + 10000), 0600, false, true}} {
		s := info{st: syscall.Stat_t{Uid: tc.uid}, mode: tc.mode}
		if UserConfigOK(s) != tc.config || PrivateKeyOK(s) != tc.private {
			t.Fatal("permission rule mismatch")
		}
	}
}
