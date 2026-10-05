//go:build darwin

package sshtrust

import "syscall"

func sameChangeTime(a, b *syscall.Stat_t) bool { return a.Ctimespec == b.Ctimespec }
