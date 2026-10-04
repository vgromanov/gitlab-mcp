//go:build linux

package gitcache

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// helperVirtualSize reads the process virtual size from /proc/self/statm.
// proc(5) documents the first field as the total program size in pages.
// A read or parse failure returns that errno instead of a substitute size.
func helperVirtualSize() (uint64, error) {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, err
	}
	field, _, _ := strings.Cut(strings.TrimSpace(string(b)), " ")
	pages, err := strconv.ParseUint(field, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w", syscall.EINVAL)
	}
	ps := uint64(syscall.Getpagesize())
	if ps == 0 || pages > ^uint64(0)/ps {
		return 0, syscall.EINVAL
	}
	return pages * ps, nil
}
