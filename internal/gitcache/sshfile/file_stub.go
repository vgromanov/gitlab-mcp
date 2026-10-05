//go:build !unix

package sshfile

import (
	"errors"
	"os"
)

var ErrFile = errors.New("SSH file unavailable or unsafe")

func OpenRegular(string, int64) (*os.File, os.FileInfo, error) { return nil, nil, ErrFile }
func UserConfigOK(os.FileInfo) bool                            { return false }
func PrivateKeyOK(os.FileInfo) bool                            { return false }
