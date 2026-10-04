//go:build linux || darwin

package gitcache

import (
	"errors"
	"fmt"
	"io"
	"syscall"
)

// asMapOnSetterFail is nil. The __gitcache_as setter-failure path must not
// call it. A call prints as-map-invoked and would run a map after the setter
// failed, which is not evidence about an installed limit.
var asMapOnSetterFail func(uint64) error

func errnoNum(err error) int {
	var n syscall.Errno
	if errors.As(err, &n) {
		return int(n)
	}
	return 0
}

func errnoSym(err error) string {
	var n syscall.Errno
	if !errors.As(err, &n) {
		return "UNKNOWN"
	}
	switch n {
	case syscall.EPERM:
		return "EPERM"
	case syscall.EINVAL:
		return "EINVAL"
	case syscall.ENOMEM:
		return "ENOMEM"
	case syscall.EFAULT:
		return "EFAULT"
	case syscall.EACCES:
		return "EACCES"
	case syscall.EAGAIN:
		return "EAGAIN"
	case syscall.EINTR:
		return "EINTR"
	case syscall.EBADF:
		return "EBADF"
	case syscall.ENOSYS:
		return "ENOSYS"
	case syscall.ERANGE:
		return "ERANGE"
	default:
		return "ERRNO"
	}
}

// writeHelperASDiag records the inherited RLIMIT_AS and this process's
// virtual size. It does not change a limit and does not map memory.
func writeHelperASDiag(w io.Writer) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_AS, &lim); err != nil {
		fmt.Fprintf(w, "as-inherit unavailable errno=%d %s\n", errnoNum(err), errnoSym(err))
	} else {
		fmt.Fprintf(w, "as-inherit soft=%d hard=%d\n", lim.Cur, lim.Max)
	}
	n, err := helperVirtualSize()
	if err != nil {
		fmt.Fprintf(w, "as-vsize unavailable errno=%d %s\n", errnoNum(err), errnoSym(err))
		return
	}
	fmt.Fprintf(w, "as-vsize %d\n", n)
}
