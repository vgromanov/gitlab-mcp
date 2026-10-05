//go:build linux || darwin

package sshtrust

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshconfig"
)

func TestFinalizedTransitionExactStateAndSharedComposition(t *testing.T) {
	ctx := context.Background()
	p := file(t, nil)
	cfg := sshconfig.Config{StrictHostKeyChecking: "accept-new", UpdateHostKeys: "yes", UserKnownHostsFiles: []string{p}}
	newManager := func(addr string) *Manager {
		m, e := New(ctx, cfg, addr)
		if e != nil {
			t.Fatal(e)
		}
		return m
	}
	original := newManager("one.example:22")
	secondOriginal := newManager("two.example:22")
	a, b := key(t), key(t)
	if e := original.Callback("one.example:22", &net.TCPAddr{}, a.PublicKey()); e != nil {
		t.Fatal(e)
	}
	if len(original.Transitions()) != 0 {
		t.Fatal("unfinalized enrollment exposed")
	}
	if e := original.Finalize(nil); e != nil {
		t.Fatal(e)
	}
	receipts := original.Transitions()
	if len(receipts) != 1 {
		t.Fatal("missing exact enrollment receipt")
	}
	secondPrepared := newManager("two.example:22")
	if e := secondOriginal.ValidateRefresh(ctx, secondPrepared, receipts); e != nil {
		t.Fatalf("shared-file transition refused: %v", e)
	}
	if e := secondPrepared.Callback("two.example:22", &net.TCPAddr{}, b.PublicKey()); e != nil {
		t.Fatal(e)
	}
	if e := secondPrepared.Finalize(nil); e != nil {
		t.Fatal(e)
	}
	receipts = append(receipts, secondPrepared.Transitions()...)
	freshOne, freshTwo := newManager("one.example:22"), newManager("two.example:22")
	for _, pair := range [][2]*Manager{{original, freshOne}, {secondOriginal, freshTwo}} {
		if e := pair[0].ValidateRefresh(ctx, pair[1], receipts); e != nil {
			t.Fatalf("sequential composition: %v", e)
		}
	}
	if original.ValidateRefresh(ctx, freshOne, receipts[:1]) == nil {
		t.Fatal("missing second transition accepted")
	}
	reversed := []Transition{receipts[1], receipts[0]}
	if original.ValidateRefresh(ctx, freshOne, reversed) == nil {
		t.Fatal("out-of-order receipt replay accepted")
	}
	duplicate := append(append([]Transition(nil), receipts...), receipts[1])
	if original.ValidateRefresh(ctx, freshOne, duplicate) == nil {
		t.Fatal("duplicate replay accepted")
	}
	if original.ValidateRefresh(ctx, freshOne, []Transition{{}}) == nil {
		t.Fatal("empty manufactured receipt accepted")
	}
	c, cancel := context.WithCancel(ctx)
	cancel()
	if e := original.ValidateRefresh(c, freshOne, receipts); !errors.Is(e, context.Canceled) {
		t.Fatal("cancellation lost")
	}
	data, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, data, 0600); e != nil {
		t.Fatal(e)
	}
	if original.ValidateRefresh(ctx, newManager("one.example:22"), receipts) == nil {
		t.Fatal("same-content external metadata rewrite accepted")
	}
}

func TestFailedFinalizationDoesNotExposeEnrollment(t *testing.T) {
	p := file(t, nil)
	cfg := sshconfig.Config{StrictHostKeyChecking: "accept-new", UpdateHostKeys: "no", UserKnownHostsFiles: []string{p}}
	m, e := New(context.Background(), cfg, "one.example:22")
	if e != nil {
		t.Fatal(e)
	}
	if e := m.Callback("one.example:22", &net.TCPAddr{}, key(t).PublicKey()); e != nil {
		t.Fatal(e)
	}
	if !errors.Is(m.Finalize(ErrUpdate), ErrUpdate) || len(m.Transitions()) != 0 {
		t.Fatal("failed finalization exposed receipt")
	}
}
