//go:build linux

package sshtrust

import "syscall"

func sameChangeTime(a, b *syscall.Stat_t) bool { return a.Ctim == b.Ctim }
