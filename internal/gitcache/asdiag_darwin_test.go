//go:build darwin

package gitcache

import (
	"os"
	"unsafe"

	"syscall"
)

// Darwin baseline virtual size uses the documented proc_info syscall
// (SYS_proc_info 336 in <sys/syscall.h>) with the same arguments as
// proc_pidinfo(getpid(), PROC_PIDTASKINFO, ...). PROC_PIDTASKINFO is 4 and
// proc_taskinfo.pti_virtual_size is the virtual size in bytes
// (<sys/proc_info.h>). The pidinfo call number is 2, which is what libproc
// passes to that syscall. Only this process is queried. A failed call
// returns the kernel errno and no size is invented.
func helperVirtualSize() (uint64, error) {
	const (
		sysProcInfo         = 336
		procInfoCallPIDInfo = 2
		procPIDTaskInfo     = 4
	)
	var info struct {
		VirtualSize      uint64
		ResidentSize     uint64
		TotalUser        uint64
		TotalSystem      uint64
		ThreadsUser      uint64
		ThreadsSystem    uint64
		Policy           int32
		Faults           int32
		Pageins          int32
		CowFaults        int32
		MessagesSent     int32
		MessagesReceived int32
		SyscallsMach     int32
		SyscallsUnix     int32
		Csw              int32
		ThreadNum        int32
		NumRunning       int32
		Priority         int32
	}
	r1, _, e := syscall.Syscall6(
		sysProcInfo,
		procInfoCallPIDInfo,
		uintptr(os.Getpid()),
		procPIDTaskInfo,
		0,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if e != 0 {
		return 0, e
	}
	if r1 != unsafe.Sizeof(info) {
		return 0, syscall.EINVAL
	}
	return info.VirtualSize, nil
}
