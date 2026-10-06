package gitlab

import (
	"context"
	"errors"
	"testing"
)

func TestChargeItem_replayDoesNotConsumeMainBudget(t *testing.T) {
	b := DefaultBudget()
	b.SetMaxItems(100)
	ctx := WithBudget(context.Background(), b)
	ctx = WithReplayBudget(ctx, 80)
	for i := 0; i < 80; i++ {
		if err := ChargeItem(ctx, b); err != nil {
			t.Fatalf("replay charge %d: %v", i, err)
		}
	}
	if err := ChargeItem(ctx, b); !errors.Is(err, ErrBudgetItems) {
		t.Fatalf("expected replay cap, got %v", err)
	}
	_, _, items := b.Stats()
	if items != 0 {
		t.Fatalf("main items=%d want 0", items)
	}
	if err := ChargeItem(ctx, b); err == nil {
		t.Fatal("replay exhausted")
	}
	if err := ChargeItem(context.Background(), b); err != nil {
		t.Fatalf("main charge: %v", err)
	}
	_, _, items = b.Stats()
	if items != 1 {
		t.Fatalf("main items=%d want 1", items)
	}
}

func TestReserveItems_holdsForwardPageAllowance(t *testing.T) {
	b := DefaultBudget()
	b.SetMaxItems(100)
	if err := b.ReserveItems(20); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 80; i++ {
		if err := b.AddItem(); err != nil {
			t.Fatalf("item %d: %v", i, err)
		}
	}
	if err := b.AddItem(); !errors.Is(err, ErrBudgetItems) {
		t.Fatalf("expected cap at 80+reserved, got %v", err)
	}
	b.ReleaseItems(20)
	if err := b.AddItem(); err != nil {
		t.Fatalf("after release: %v", err)
	}
}
