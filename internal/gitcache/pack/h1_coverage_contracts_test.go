package pack

import (
	"context"
	"errors"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

func TestCoverage_MergeAndIndexIntegrityContracts(t *testing.T) {
	ctx := context.Background()
	h := HashObject("blob", []byte("a"))
	dst := map[plumbing.Hash]Object{h: {Hash: h, Type: "blob", Data: []byte("a")}}
	// Collision with different payload.
	bad := map[plumbing.Hash]Object{h: {Hash: h, Type: "blob", Data: []byte("b")}}
	if err := MergeObjects(ctx, dst, bad); !errors.Is(err, ErrChecksum) {
		t.Fatalf("collision: %v", err)
	}
	// Identical merge is a no-op success.
	if err := MergeObjects(ctx, dst, map[plumbing.Hash]Object{h: {Hash: h, Type: "blob", Data: []byte("a")}}); err != nil {
		t.Fatalf("identical merge: %v", err)
	}
	// Bad object hash in source.
	wrong := plumbing.NewHash("1111111111111111111111111111111111111111")
	if err := MergeObjects(ctx, dst, map[plumbing.Hash]Object{wrong: {Hash: wrong, Type: "blob", Data: []byte("a")}}); !errors.Is(err, ErrChecksum) {
		t.Fatalf("hash mismatch: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	// Empty src does not evaluate cancellation; omit that no-op. Nonempty src must.
	h2 := HashObject("blob", []byte("zz"))
	if err := MergeObjects(canceled, map[plumbing.Hash]Object{}, map[plumbing.Hash]Object{h2: {Hash: h2, Type: "blob", Data: []byte("zz")}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("merge cancel: %v", err)
	}
	if _, err := EncodeIndexed(ctx, nil); !errors.Is(err, ErrTooManyObjects) {
		t.Fatalf("empty encode: %v", err)
	}
	if _, err := EncodeIndexed(canceled, dst); !errors.Is(err, context.Canceled) {
		t.Fatalf("encode cancel: %v", err)
	}
	badObj := map[plumbing.Hash]Object{h: {Hash: h, Type: "blob", Data: []byte("nope")}}
	if _, err := EncodeIndexed(ctx, badObj); !errors.Is(err, ErrChecksum) {
		t.Fatalf("encode checksum: %v", err)
	}
	ip, err := EncodeIndexed(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BuildIndexV2(canceled, ip); !errors.Is(err, context.Canceled) {
		t.Fatalf("index cancel: %v", err)
	}
	// Corrupt entry offset beyond pack.
	broken := ip
	broken.Entries = append([]IndexEntry(nil), ip.Entries...)
	broken.Entries[0].Offset = uint64(ip.PackByteCount) + 1
	if _, err := BuildIndexV2(ctx, broken); !errors.Is(err, ErrMalformed) {
		t.Fatalf("bad offset: %v", err)
	}
	if _, err := objectCRC(ip.PackBytes, -1, 1); !errors.Is(err, ErrMalformed) {
		t.Fatalf("crc bounds: %v", err)
	}
	if _, err := BuildIndexV2FromPack(ctx, ip.PackBytes, map[plumbing.Hash]Object{}); !errors.Is(err, ErrChecksum) {
		t.Fatalf("objs mismatch: %v", err)
	}
	idx, err := BuildIndexV2FromPack(ctx, ip.PackBytes, nil)
	if err != nil || len(idx) == 0 {
		t.Fatalf("index from pack: %v", err)
	}
}
