package tree

import (
	"context"
	"errors"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

func TestProveCommitRootCompleteness(t *testing.T) {
	blob := obj("blob", []byte("leaf"))
	tr := treeObj(t, []TreeEntry{{Mode: ModeFile, Name: "f", Hash: blob.Hash}})
	c := commitObj(tr.Hash)
	objs := Map{blob.Hash: blob, tr.Hash: tr, c.Hash: c}
	if err := ProveCommitRoot(context.Background(), objs, c.Hash); err != nil {
		t.Fatal(err)
	}

	missing := plumbing.NewHash("ffffffffffffffffffffffffffffffffffffffff")
	if err := ProveCommitRoot(context.Background(), objs, missing); !errors.Is(err, ErrShallow) {
		t.Fatalf("missing: %v", err)
	}
	if err := ProveCommitRoot(context.Background(), objs, blob.Hash); !errors.Is(err, ErrWrongType) {
		t.Fatalf("blob as root: %v", err)
	}

	// gitlink leaf is accepted as a tree entry without recursion.
	link := plumbing.NewHash("1111111111111111111111111111111111111111")
	tr2 := treeObj(t, []TreeEntry{{Mode: ModeCommit, Name: "sub", Hash: link}})
	c2 := commitObj(tr2.Hash)
	objs2 := Map{tr2.Hash: tr2, c2.Hash: c2}
	if err := ProveCommitRoot(context.Background(), objs2, c2.Hash); err != nil {
		t.Fatalf("gitlink leaf: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ProveCommitRoot(ctx, objs, c.Hash); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}
