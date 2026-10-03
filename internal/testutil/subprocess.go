package testutil

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const (
	// SubprocessChildHookName is the exact test name dependent packages MUST
	// register so RunAcceptedCountChild can re-exec into their test binary:
	//
	//	func TestSubprocessChildHelper(t *testing.T) {
	//		testutil.MaybeRunSubprocessChild()
	//	}
	//
	// Without this hook, downstream binaries will not contain the child entry
	// point and RunAcceptedCountChild will fail closed.
	SubprocessChildHookName = "TestSubprocessChildHelper"

	subprocessChildEnv = "TESTUTIL_SUBPROCESS_CHILD"
	subprocessStateEnv = "TESTUTIL_SUBPROCESS_STATE"
	subprocessModeEnv  = "TESTUTIL_SUBPROCESS_MODE"
	subprocessAckEnv   = "TESTUTIL_SUBPROCESS_ACK"

	// ChildModeAccept persists accepted count, writes ack, exits 0.
	ChildModeAccept = "accept"
	// ChildModeCrashBefore exits non-zero without mutating accepted state.
	ChildModeCrashBefore = "crash-before"
	// ChildModeCrashAfterAccept persists accepted count, writes ack, then
	// exits abruptly with a non-zero status (crash after synchronized accept).
	ChildModeCrashAfterAccept = "crash-after-accept"
)

// AcceptedState is durable accepted-count state for crash/restart fixtures.
type AcceptedState struct {
	AcceptedCount int `json:"accepted_count"`
}

// ReadAcceptedState loads AcceptedState from path; missing file yields zero state.
func ReadAcceptedState(path string) (AcceptedState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return AcceptedState{}, nil
		}
		return AcceptedState{}, err
	}
	var st AcceptedState
	if err := json.Unmarshal(b, &st); err != nil {
		return AcceptedState{}, err
	}
	return st, nil
}

// WriteAcceptedState persists AcceptedState to path atomically and syncs.
func WriteAcceptedState(path string, st AcceptedState) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "accepted-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	enc := json.NewEncoder(tmp)
	if err := enc.Encode(st); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// MinimalSubprocessEnv returns an explicit, host-independent child environment.
// It does not copy os.Environ.
func MinimalSubprocessEnv(statePath, ackPath, mode string) []string {
	return []string{
		subprocessChildEnv + "=1",
		subprocessStateEnv + "=" + statePath,
		subprocessAckEnv + "=" + ackPath,
		subprocessModeEnv + "=" + mode,
	}
}

// RunAcceptedCountChild re-execs the current test binary into SubprocessChildHookName
// with a minimal controlled env (no os.Environ copy). Dependent packages must
// register that hook or this call fails.
func RunAcceptedCountChild(t *testing.T, statePath, ackPath, mode string) (exitErr error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "-test.run=^"+SubprocessChildHookName+"$", "-test.count=1")
	cmd.Env = MinimalSubprocessEnv(statePath, ackPath, mode)
	cmd.Dir = filepath.Dir(statePath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("child mode=%s: %w\n%s", mode, err, out)
	}
	return nil
}

// RunAcceptedCountChildExpectFailure is like RunAcceptedCountChild but requires
// a non-zero child exit (abrupt crash paths).
func RunAcceptedCountChildExpectFailure(t *testing.T, statePath, ackPath, mode string) error {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "-test.run=^"+SubprocessChildHookName+"$", "-test.count=1")
	cmd.Env = MinimalSubprocessEnv(statePath, ackPath, mode)
	cmd.Dir = filepath.Dir(statePath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return fmt.Errorf("child mode=%s: expected non-zero exit, got success\n%s", mode, out)
	}
	return nil
}

// MaybeRunSubprocessChild is the reusable child entrypoint. Dependent packages
// must call it from a test named exactly SubprocessChildHookName.
func MaybeRunSubprocessChild() {
	if os.Getenv(subprocessChildEnv) != "1" {
		return
	}
	statePath := os.Getenv(subprocessStateEnv)
	ackPath := os.Getenv(subprocessAckEnv)
	mode := os.Getenv(subprocessModeEnv)
	if statePath == "" || ackPath == "" || mode == "" {
		fmt.Fprintln(os.Stderr, "missing subprocess fixture env")
		os.Exit(2)
	}
	switch mode {
	case ChildModeCrashBefore:
		os.Exit(99)
	case ChildModeAccept, ChildModeCrashAfterAccept:
		st, err := ReadAcceptedState(statePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		st.AcceptedCount++
		if err := WriteAcceptedState(statePath, st); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if err := os.WriteFile(ackPath, []byte("accepted\n"), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if mode == ChildModeCrashAfterAccept {
			os.Exit(97) // abrupt non-zero after synchronized accept ack
		}
		os.Exit(0)
	default:
		fmt.Fprintln(os.Stderr, "unknown subprocess mode")
		os.Exit(2)
	}
}
