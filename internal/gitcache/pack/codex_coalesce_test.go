package pack

import (
	"bytes"
	"context"
	"errors"
	"github.com/go-git/go-git/v5/plumbing"
	"testing"
)

func TestCodexRemainingDecodeBudget(t *testing.T) {
	o := Object{Type: "blob", Data: []byte("abc")}
	o.Hash = HashObject(o.Type, o.Data)
	raw, e := Encode([]Object{o})
	if e != nil {
		t.Fatal(e)
	}
	for _, l := range []Limits{{Objects: 0, InputBytes: 3, OutputBytes: 3}, {Objects: 1, InputBytes: 2, OutputBytes: 3}, {Objects: 1, InputBytes: 3, OutputBytes: 2}} {
		if _, e := DecodeIndexedLimits(context.Background(), raw, l); e == nil {
			t.Fatal("remaining budget exceeded")
		}
	}
}
func TestCodexCollisionConsistencyAndOneEncode(t *testing.T) {
	o := Object{Type: "blob", Data: []byte("abc")}
	o.Hash = HashObject(o.Type, o.Data)
	objects := map[plumbing.Hash]Object{o.Hash: o}
	bad := o
	bad.Data = []byte("xyz")
	if e := MergeObjects(context.Background(), objects, map[plumbing.Hash]Object{o.Hash: bad}); !errors.Is(e, ErrChecksum) {
		t.Fatal(e)
	}
	if !bytes.Equal(objects[o.Hash].Data, o.Data) {
		t.Fatal("collision overwrote prior object")
	}
	if e := MergeObjects(context.Background(), objects, map[plumbing.Hash]Object{o.Hash: o}); e != nil {
		t.Fatal(e)
	}
	ip, e := EncodeIndexed(context.Background(), objects)
	if e != nil {
		t.Fatal(e)
	}
	if len(ip.Objects) != 1 || &ip.Objects[o.Hash].Data[0] != &o.Data[0] {
		t.Fatal("encoder copied resolved payload")
	}
	decoded, e := DecodeIndexed(context.Background(), ip.PackBytes)
	if e != nil {
		t.Fatal(e)
	}
	a, e := BuildIndexV2(context.Background(), ip)
	if e != nil {
		t.Fatal(e)
	}
	b, e := BuildIndexV2(context.Background(), decoded)
	if e != nil || !bytes.Equal(a, b) {
		t.Fatal("encoded index differs from independent decode")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := EncodeIndexed(ctx, objects); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
