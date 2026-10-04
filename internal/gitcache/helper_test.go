//go:build linux || darwin

package gitcache

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && len(os.Args[1]) > 11 && os.Args[1][:11] == "__gitcache_" {
		os.Exit(dispatchHelper(os.Args[1]))
	}
	os.Exit(m.Run())
}

func dispatchHelper(role string) int {
	switch role {
	case "__gitcache_fs":
		return runFSHelper()
	case "__gitcache_git":
		return runGitLauncher()
	case "__gitcache_as_over":
		installASSeam("over")
		return runGitLauncher()
	case "__gitcache_as_get":
		installASSeam("get")
		return runGitLauncher()
	case "__gitcache_as_under":
		installASSeam("under")
		return runGitLauncher()
	case "__gitcache_as_probe":
		installASSeam("probe")
		return runGitLauncher()
	case "__gitcache_as_ok":
		installASSeam("ok")
		return runGitLauncher()
	case "__gitcache_git_failas":
		enforceVirtualAS = func(uint64) error { return errASIneffective }
		if err := enforceVirtualAS(LimitAS); errors.Is(err, errASIneffective) {
			fmt.Fprintln(os.Stderr, "as-branch=overmap")
		}
		return runGitLauncher()
	case "__gitcache_reader":
		b, err := os.ReadFile("ledger")
		if err != nil || len(b) < 256+256 {
			return 2
		}
		var best uint32
		for i := 0; i < 64; i++ {
			off := 256 + i*256 + 168
			if off+4 > len(b) {
				return 2
			}
			c := binary.LittleEndian.Uint32(b[off : off+4])
			if c > best {
				best = c
			}
		}
		if best == 0 {
			return 2
		}
		var buf [4]byte
		binary.LittleEndian.PutUint32(buf[:], best)
		if err := os.WriteFile("reader-pin", buf[:], 0600); err != nil {
			return 2
		}
		time.Sleep(time.Hour)
		return 0
	case "__gitcache_fsize":
		if err := EnforceFSIZE(4096); err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			return 1
		}
		fmt.Fprintln(os.Stdout, "fsize-enforced")
		return 0
	case "__gitcache_nofile":
		n, err := EnforceNOFILE(32)
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			return 1
		}
		fmt.Fprintf(os.Stdout, "nofile-enforced %d\n", n)
		return 0
	case "__gitcache_cpu":
		// Test-only: soft 1s asks the kernel for SIGXCPU, and hard 8s stays
		// above it so Linux does not SIGKILL in the same second. Production
		// ApplyLimits still sets soft=hard to LimitCPU (30s).
		lim := syscall.Rlimit{Cur: 1, Max: 8}
		if err := syscall.Setrlimit(syscall.RLIMIT_CPU, &lim); err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			return 2
		}
		var got syscall.Rlimit
		if err := syscall.Getrlimit(syscall.RLIMIT_CPU, &got); err != nil || got.Cur != 1 || got.Max != 8 {
			fmt.Fprintln(os.Stderr, "cpu-setup-failed")
			return 2
		}
		fmt.Fprintf(os.Stdout, "cpu-ready soft=%d hard=%d\n", got.Cur, got.Max)
		_ = os.Stdout.Sync()
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGXCPU)
		var x uint64
		for {
			x += uint64(os.Getpid())
			select {
			case sig := <-ch:
				if sig != syscall.SIGXCPU {
					fmt.Fprintf(os.Stderr, "cpu-unexpected %s\n", sig)
					return 2
				}
				fmt.Fprintln(os.Stdout, "cpu-enforced SIGXCPU")
				return 1
			default:
			}
		}
	case "__gitcache_core":
		lim := syscall.Rlimit{Cur: 0, Max: 0}
		if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &lim); err != nil {
			fmt.Fprintln(os.Stderr, "core-setup-failed")
			return 2
		}
		fmt.Fprintln(os.Stdout, "core-armed")
		_ = os.Stdout.Sync()
		// A Go process cannot restore SIG_DFL for SIGABRT. Replace this
		// process with a shell whose default disposition is abort.
		if err := syscall.Exec("/bin/sh", []string{"sh", "-c", "kill -ABRT $$"}, os.Environ()); err != nil {
			fmt.Fprintln(os.Stderr, "core-setup-failed")
			return 2
		}
		return 2
	case "__gitcache_as":
		writeHelperASDiag(os.Stderr)
		lim := syscall.Rlimit{Cur: LimitAS, Max: LimitAS}
		if err := syscall.Setrlimit(syscall.RLIMIT_AS, &lim); err != nil {
			fmt.Fprintf(os.Stderr, "as-errno=%d %s\n", errnoNum(err), errnoSym(err))
			fmt.Fprintln(os.Stderr, "as-branch=setter")
			fmt.Fprintln(os.Stderr, "as-reject")
			if asMapOnSetterFail != nil {
				fmt.Fprintln(os.Stderr, "as-map-invoked")
				_ = asMapOnSetterFail(LimitAS)
			}
			return 3
		}
		var got syscall.Rlimit
		if err := syscall.Getrlimit(syscall.RLIMIT_AS, &got); err != nil || got.Cur != LimitAS || got.Max != LimitAS {
			fmt.Fprintln(os.Stderr, "as-mismatch")
			return 3
		}
		err := EnforceVirtualAS(LimitAS)
		switch {
		case err == nil:
			fmt.Fprintln(os.Stdout, "as-enforced")
			return 0
		case errors.Is(err, errASIneffective):
			fmt.Fprintln(os.Stderr, "as-ineffective")
			return 3
		default:
			fmt.Fprintln(os.Stderr, "as-reject")
			return 3
		}
	case "__gitcache_token":
		if _, err := ReadToken(os.Stdin); err != nil {
			fmt.Fprintln(os.Stderr, "token-blocked")
			return 4
		}
		fmt.Fprintln(os.Stdout, "token-read")
		return 0
	case "__gitcache_token_high":
		lim := syscall.Rlimit{Cur: 1, Max: 1}
		if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &lim); err != nil {
			fmt.Fprintln(os.Stdout, "core-locked")
			return 0
		}
		defer syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{})
		if _, err := ReadToken(os.Stdin); err != nil {
			fmt.Fprintln(os.Stdout, "token-blocked")
			return 0
		}
		fmt.Fprintln(os.Stdout, "token-read")
		return 1
	case "__gitcache_ack_bad":
		_, _ = os.Stdout.Write([]byte("XXXXXXXX"))
		_ = os.Stdout.Sync()
		var one [1]byte
		_, _ = os.Stdin.Read(one[:])
		return 2
	case "__gitcache_ack_short":
		_, _ = os.Stdout.Write([]byte{0, 0})
		_ = os.Stdout.Sync()
		var one [1]byte
		_, _ = os.Stdin.Read(one[:])
		return 2
	case "__gitcache_sleep":
		time.Sleep(time.Hour)
		return 0
	case "__gitcache_launch_bad":
		_, _ = syscall.Write(6, []byte("BADACK!!"))
		time.Sleep(time.Hour)
		return 0
	case "__gitcache_launch_part":
		_, _ = syscall.Write(6, []byte("BAD"))
		time.Sleep(time.Hour)
		return 0
	case "__gitcache_launch_eof":
		return 2
	case "__gitcache_tree":
		exe, err := os.Executable()
		if err != nil {
			return 2
		}
		cmd := exec.Command(exe, "__gitcache_sleep")
		cmd.Env = AllowEnv()
		cmd.SysProcAttr = &syscall.SysProcAttr{}
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			return 2
		}
		fmt.Fprintf(os.Stdout, "%d\n", cmd.Process.Pid)
		select {}
	default:
		return 2
	}
}
