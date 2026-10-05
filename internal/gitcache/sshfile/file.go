//go:build unix

// Package sshfile checks the opened Unix file descriptor, not a preceding stat.
package sshfile

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"syscall"
)

var ErrFile = errors.New("SSH file unavailable or unsafe")

func OpenRegular(path string, max int64) (*os.File, os.FileInfo, error) {
	fd, e := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, nil, e
	}
	f := os.NewFile(uintptr(fd), path)
	s, e := f.Stat()
	if e != nil || !s.Mode().IsRegular() || s.Size() > max {
		f.Close()
		return nil, nil, ErrFile
	}
	return f, s, nil
}

// OpenSSH SSHCONF_CHECKPERM applies to default user config and all includes.
// Only the top-level system config omits this check.
func UserConfigOK(s os.FileInfo) bool {
	st, ok := s.Sys().(*syscall.Stat_t)
	return ok && (st.Uid == 0 || int(st.Uid) == os.Getuid()) && s.Mode().Perm()&022 == 0
}

// OpenSSH authfile checks group/other access only for a user-owned private key.
func PrivateKeyOK(s os.FileInfo) bool {
	st, ok := s.Sys().(*syscall.Stat_t)
	return ok && (int(st.Uid) != os.Getuid() || s.Mode().Perm()&077 == 0)
}
