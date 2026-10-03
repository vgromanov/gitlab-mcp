package tools

import (
	"context"
	"testing"
	"time"

	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
)

func TestEnsureBatchBudget_upstreamLongElapsedGetsLocalDeadline(t *testing.T) {
	restore := setBatchInvocationMaxElapsedForTest(40 * time.Millisecond)
	defer restore()

	upstream := igl.DefaultBudget()
	upstream.MaxItems = 100
	upstream.MaxBytes = 64 << 20
	upstream.MaxRequests = 64
	upstream.MaxElapsed = time.Hour
	budgetParent := igl.WithBudget(context.Background(), upstream)

	ctx, b, release := ensureBatchBudget(budgetParent)
	defer release()

	if b != upstream {
		t.Fatal("must reuse upstream budget object")
	}
	items, bytes, reqs, elapsed := b.LimitsSnapshot()
	if items != batchLocalMaxItems || bytes != batchLocalMaxScannedBytes || reqs != batchLocalMaxRequests {
		t.Fatalf("CapLimits not applied: items=%d bytes=%d reqs=%d", items, bytes, reqs)
	}
	if elapsed != time.Hour {
		t.Fatalf("MaxElapsed field must remain upstream value (deadline composed separately), got %v", elapsed)
	}
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("local deadline required when upstream elapsed is long")
	}
	if time.Until(dl) > 200*time.Millisecond {
		t.Fatalf("local deadline too far: %v", time.Until(dl))
	}

	select {
	case <-ctx.Done():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("composed local deadline must fire")
	}
	if budgetParent.Err() != nil {
		t.Fatalf("upstream WithBudget ctx should remain open under long MaxElapsed, err=%v", budgetParent.Err())
	}
}

func TestEnsureBatchBudget_upstreamNoElapsedStillCapped(t *testing.T) {
	restore := setBatchInvocationMaxElapsedForTest(30 * time.Millisecond)
	defer restore()

	upstream := igl.DefaultBudget()
	upstream.MaxItems = 5 // tighter items must win
	upstream.MaxBytes = 0 // unlimited → capped
	upstream.MaxRequests = 0
	upstream.MaxElapsed = 0 // no elapsed / no WithBudget deadline
	pctx := igl.WithBudget(context.Background(), upstream)

	out, b, release := ensureBatchBudget(pctx)
	defer release()

	items, bytes, reqs, _ := b.LimitsSnapshot()
	if items != 5 {
		t.Fatalf("tighter upstream MaxItems must win, got %d", items)
	}
	if bytes != batchLocalMaxScannedBytes || reqs != batchLocalMaxRequests {
		t.Fatalf("unlimited upstream must be capped: bytes=%d reqs=%d", bytes, reqs)
	}
	if _, ok := out.Deadline(); !ok {
		t.Fatal("local deadline required when upstream MaxElapsed is zero")
	}
	select {
	case <-out.Done():
	case <-time.After(400 * time.Millisecond):
		t.Fatal("local deadline must cancel")
	}
}

func TestEnsureBatchBudget_preservesTighterParentDeadline(t *testing.T) {
	restore := setBatchInvocationMaxElapsedForTest(time.Hour)
	defer restore()

	parent, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	upstream := igl.DefaultBudget()
	upstream.MaxElapsed = time.Hour
	pctx := igl.WithBudget(parent, upstream)
	ctx, _, release := ensureBatchBudget(pctx)
	defer release()

	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected deadline")
	}
	if time.Until(dl) > 200*time.Millisecond {
		t.Fatalf("must preserve tighter parent deadline, until=%v", time.Until(dl))
	}
}

func TestEnsureBatchBudget_ownedDefaultGetsLocalElapsed(t *testing.T) {
	restore := setBatchInvocationMaxElapsedForTest(35 * time.Millisecond)
	defer restore()

	ctx, b, release := ensureBatchBudget(context.Background())
	defer release()
	if igl.BudgetFromContext(ctx) != b {
		t.Fatal("owned budget must be on context")
	}
	select {
	case <-ctx.Done():
	case <-time.After(400 * time.Millisecond):
		t.Fatal("owned budget local deadline must fire")
	}
}
