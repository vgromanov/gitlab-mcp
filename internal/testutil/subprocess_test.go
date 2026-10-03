package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSubprocessChildHelper is the required reusable hook name
// (SubprocessChildHookName). Dependent packages must register an identically
// named test that calls MaybeRunSubprocessChild.
func TestSubprocessChildHelper(t *testing.T) {
	MaybeRunSubprocessChild()
}

func TestSubprocessCrashRestart_acceptedPersists(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	ackPath := filepath.Join(dir, "ack")

	// Abrupt exit before accept: count stays 0, no ack.
	if err := RunAcceptedCountChildExpectFailure(t, statePath, ackPath, ChildModeCrashBefore); err != nil {
		t.Fatal(err)
	}
	st, err := ReadAcceptedState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if st.AcceptedCount != 0 {
		t.Fatalf("after crash-before count=%d", st.AcceptedCount)
	}
	if _, err := os.Stat(ackPath); !os.IsNotExist(err) {
		t.Fatalf("ack should be absent after crash-before, err=%v", err)
	}

	// Crash after synchronized accept acknowledgment: count=1, ack present, non-zero exit.
	if err := RunAcceptedCountChildExpectFailure(t, statePath, ackPath, ChildModeCrashAfterAccept); err != nil {
		t.Fatal(err)
	}
	st, err = ReadAcceptedState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if st.AcceptedCount != 1 {
		t.Fatalf("after crash-after-accept count=%d want 1", st.AcceptedCount)
	}
	ack, err := os.ReadFile(ackPath)
	if err != nil || string(ack) != acceptedAckPayload {
		t.Fatalf("ack=%q err=%v", ack, err)
	}

	// Restart with ordinary accept: count persists and increments to 2.
	if err := RunAcceptedCountChild(t, statePath, ackPath, ChildModeAccept); err != nil {
		t.Fatal(err)
	}
	st, err = ReadAcceptedState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if st.AcceptedCount != 2 {
		t.Fatalf("after restart accept count=%d want 2", st.AcceptedCount)
	}
	ack, err = os.ReadFile(ackPath)
	if err != nil || string(ack) != acceptedAckPayload {
		t.Fatalf("accept ack=%q err=%v", ack, err)
	}
}

func TestSubprocessExpectFailure_rejectsUnrelatedExit(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	ackPath := filepath.Join(dir, "ack")
	if err := RunAcceptedCountChildExpectFailure(t, statePath, ackPath, "not-a-crash-mode"); err == nil {
		t.Fatal("unsupported mode must error")
	}
}

func TestSubprocessMinimalEnv_noHostLeak(t *testing.T) {
	env := MinimalSubprocessEnv("/tmp/state", "/tmp/ack", ChildModeAccept)
	for _, e := range env {
		if len(e) >= 5 && e[:5] == "HOME=" {
			t.Fatalf("host HOME leaked into minimal env: %v", env)
		}
		if len(e) >= 5 && e[:5] == "PATH=" {
			t.Fatalf("host PATH leaked into minimal env: %v", env)
		}
	}
	if len(env) != 4 {
		t.Fatalf("expected 4 controlled vars, got %d: %v", len(env), env)
	}
	if SubprocessChildHookName != "TestSubprocessChildHelper" {
		t.Fatalf("hook name drift: %q", SubprocessChildHookName)
	}
}
