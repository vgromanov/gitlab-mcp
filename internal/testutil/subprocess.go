package testutil

import (
	"encoding/json"
	"errors"
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
	// point and RunAcceptedCountChild will fail closed (non-nil error, no ack,
	// unchanged accepted count).
	SubprocessChildHookName = "TestSubprocessChildHelper"

	subprocessChildEnv = "TESTUTIL_SUBPROCESS_CHILD"
	subprocessStateEnv = "TESTUTIL_SUBPROCESS_STATE"
	subprocessModeEnv  = "TESTUTIL_SUBPROCESS_MODE"
	subprocessAckEnv   = "TESTUTIL_SUBPROCESS_ACK"

	// ChildModeAccept persists accepted count, writes ack, exits 0.
	ChildModeAccept = "accept"
	// ChildModeCrashBefore exits with code 99 without mutating accepted state.
	ChildModeCrashBefore = "crash-before"
	// ChildModeCrashAfterAccept persists accepted count, writes ack, then
	// exits with code 97 (crash after synchronized accept).
	ChildModeCrashAfterAccept = "crash-after-accept"

	childExitCrashBefore = 99
	childExitCrashAfter  = 97

	acceptedAckPayload = "accepted\n"
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
// with a minimal controlled env (no os.Environ copy). It removes any stale ack
// before spawn and returns an error unless this child wrote a fresh ack and
// increased the durable accepted count. Downstream binaries that omit the hook
// therefore fail closed (Go's "no tests to run" exit 0 is not treated as success).
func RunAcceptedCountChild(t *testing.T, statePath, ackPath, mode string) error {
	t.Helper()
	if mode != ChildModeAccept {
		return fmt.Errorf("RunAcceptedCountChild only supports mode %q, got %q", ChildModeAccept, mode)
	}
	before, err := ReadAcceptedState(statePath)
	if err != nil {
		return err
	}
	if err := prepareFreshAck(ackPath); err != nil {
		return err
	}
	out, err := runSubprocessChild(statePath, ackPath, mode)
	if err != nil {
		return fmt.Errorf("child mode=%s: %w\n%s", mode, err, out)
	}
	if err := requireFreshAcceptAck(ackPath, out); err != nil {
		return err
	}
	after, err := ReadAcceptedState(statePath)
	if err != nil {
		return err
	}
	if after.AcceptedCount != before.AcceptedCount+1 {
		return fmt.Errorf("child mode=%s: accepted count did not increase (before=%d after=%d); hook missing?\n%s",
			mode, before.AcceptedCount, after.AcceptedCount, out)
	}
	return nil
}

// RunAcceptedCountChildExpectFailure re-execs the hook for intentional crash
// modes. Generic launch failures or unrelated exit codes cannot impersonate the
// intended crash: crash-before requires exit 99 with unchanged count and no ack;
// crash-after-accept requires exit 97 with count+1 and a fresh ack.
func RunAcceptedCountChildExpectFailure(t *testing.T, statePath, ackPath, mode string) error {
	t.Helper()
	before, err := ReadAcceptedState(statePath)
	if err != nil {
		return err
	}
	if err := prepareFreshAck(ackPath); err != nil {
		return err
	}
	out, err := runSubprocessChild(statePath, ackPath, mode)
	switch mode {
	case ChildModeCrashBefore:
		if err == nil {
			return fmt.Errorf("child mode=%s: expected non-zero exit, got success\n%s", mode, out)
		}
		if code := exitCode(err); code != childExitCrashBefore {
			return fmt.Errorf("child mode=%s: expected exit %d, got code=%d err=%v\n%s",
				mode, childExitCrashBefore, code, err, out)
		}
		after, rerr := ReadAcceptedState(statePath)
		if rerr != nil {
			return rerr
		}
		if after.AcceptedCount != before.AcceptedCount {
			return fmt.Errorf("child mode=%s: accepted count must stay %d, got %d",
				mode, before.AcceptedCount, after.AcceptedCount)
		}
		if ackPresent(ackPath) {
			return fmt.Errorf("child mode=%s: ack must be absent before accept", mode)
		}
		return nil
	case ChildModeCrashAfterAccept:
		if err == nil {
			return fmt.Errorf("child mode=%s: expected non-zero exit, got success\n%s", mode, out)
		}
		if code := exitCode(err); code != childExitCrashAfter {
			return fmt.Errorf("child mode=%s: expected exit %d, got code=%d err=%v\n%s",
				mode, childExitCrashAfter, code, err, out)
		}
		if err := requireFreshAcceptAck(ackPath, out); err != nil {
			return fmt.Errorf("child mode=%s: %v", mode, err)
		}
		after, rerr := ReadAcceptedState(statePath)
		if rerr != nil {
			return rerr
		}
		if after.AcceptedCount != before.AcceptedCount+1 {
			return fmt.Errorf("child mode=%s: accepted count must increase (before=%d after=%d)",
				mode, before.AcceptedCount, after.AcceptedCount)
		}
		return nil
	default:
		return fmt.Errorf("RunAcceptedCountChildExpectFailure unsupported mode %q", mode)
	}
}

func runSubprocessChild(statePath, ackPath, mode string) ([]byte, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, "-test.run=^"+SubprocessChildHookName+"$", "-test.count=1")
	cmd.Env = MinimalSubprocessEnv(statePath, ackPath, mode)
	cmd.Dir = filepath.Dir(statePath)
	return cmd.CombinedOutput()
}

func prepareFreshAck(ackPath string) error {
	if err := os.MkdirAll(filepath.Dir(ackPath), 0o755); err != nil {
		return err
	}
	_ = os.Remove(ackPath)
	if ackPresent(ackPath) {
		return fmt.Errorf("failed to clear stale ack %s", ackPath)
	}
	return nil
}

func requireFreshAcceptAck(ackPath string, out []byte) error {
	b, err := os.ReadFile(ackPath)
	if err != nil {
		return fmt.Errorf("missing fresh ack (hook missing or child did not accept): %w\n%s", err, out)
	}
	if string(b) != acceptedAckPayload {
		return fmt.Errorf("invalid ack payload %q\n%s", b, out)
	}
	return nil
}

func ackPresent(ackPath string) bool {
	_, err := os.Stat(ackPath)
	return err == nil
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
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
		os.Exit(childExitCrashBefore)
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
		if err := os.WriteFile(ackPath, []byte(acceptedAckPayload), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if mode == ChildModeCrashAfterAccept {
			os.Exit(childExitCrashAfter)
		}
		os.Exit(0)
	default:
		fmt.Fprintln(os.Stderr, "unknown subprocess mode")
		os.Exit(2)
	}
}
