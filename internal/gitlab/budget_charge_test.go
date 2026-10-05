package gitlab

import (
	"errors"
	"testing"
)

func TestChargeBytesHonorsRemaining(t *testing.T) {
	b := DefaultBudget()
	b.MaxBytes = 8
	if err := b.ChargeBytes(5); err != nil {
		t.Fatal(err)
	}
	if left := b.RemainingBytes(); left != 3 {
		t.Fatalf("left=%d", left)
	}
	if err := b.ChargeBytes(4); !errors.Is(err, ErrBudgetBytes) {
		t.Fatalf("over: %v", err)
	}
	if err := b.ChargeBytes(0); err != nil {
		t.Fatalf("zero: %v", err)
	}
}
