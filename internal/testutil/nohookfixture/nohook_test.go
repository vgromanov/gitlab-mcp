package nohookfixture_test

import (
	"os"
	"path/filepath"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
)

// This package deliberately does NOT define TestSubprocessChildHelper.
// RunAcceptedCountChild re-execs this test binary and must fail closed.

func TestMissingSubprocessHook_failsClosed(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	ackPath := filepath.Join(dir, "ack")

	// Plant a stale ack that must not satisfy a fresh-accept check.
	if err := os.WriteFile(ackPath, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := testutil.RunAcceptedCountChild(t, statePath, ackPath, testutil.ChildModeAccept)
	if err == nil {
		t.Fatal("expected non-nil error when hook is absent from this test binary")
	}

	st, rerr := testutil.ReadAcceptedState(statePath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if st.AcceptedCount != 0 {
		t.Fatalf("accepted count=%d want 0", st.AcceptedCount)
	}
	if _, serr := os.Stat(ackPath); !os.IsNotExist(serr) {
		b, _ := os.ReadFile(ackPath)
		t.Fatalf("fresh accept ack must be absent; present=%q err=%v", b, serr)
	}
}
