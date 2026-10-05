package pack

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
)

func TestVarintsRejectOverflowBeforeShift(t *testing.T) {
	var ten []byte
	for i := 0; i < 9; i++ {
		ten = append(ten, 0x80)
	}
	ten = append(ten, 0x7f)
	if _, _, err := readVarint(ten); !errors.Is(err, ErrMalformed) {
		t.Fatalf("varint: %v", err)
	}
	header := []byte{0x80 | (kindBlob << 4)}
	for i := 0; i < 8; i++ {
		header = append(header, 0x80)
	}
	header = append(header, 0x7f)
	if _, _, _, err := readTypeSize(header); !errors.Is(err, ErrMalformed) {
		t.Fatalf("type size: %v", err)
	}
	ofs := bytes.Repeat([]byte{0xff}, 9)
	ofs = append(ofs, 0x00)
	if _, _, err := readOFS(ofs); !errors.Is(err, ErrMalformed) {
		t.Fatalf("ofs: %v", err)
	}
}

func TestRefDeltaRequiresUndeltifiedBase(t *testing.T) {
	base := Object{Type: "blob", Data: []byte("hello")}
	pack, err := Encode([]Object{base})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(context.Background(), pack)
	if err != nil || len(got) != 1 {
		t.Fatalf("decode %+v %v", got, err)
	}
	delta, err := EncodeDelta(len(base.Data), len(base.Data), []DeltaInstruction{{Off: 0, Size: len(base.Data)}})
	if err != nil {
		t.Fatal(err)
	}
	comp, err := zlibCompress(delta)
	if err != nil {
		t.Fatal(err)
	}
	var missing plumbing.Hash
	missing[0] = 1
	body := packHeader(1)
	body = AppendRawEntry(body, kindREF, uint64(len(delta)), append(missing[:], comp...))
	body = FinishPack(body)
	if _, err := Decode(context.Background(), body); !errors.Is(err, ErrThinDelta) {
		t.Fatalf("external ref: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Decode(ctx, pack); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestRefBaseThatIsDeltaIsRejected(t *testing.T) {
	base := []byte("hello")
	delta, err := EncodeDelta(len(base), len(base)+1, []DeltaInstruction{{Off: 0, Size: len(base)}, {Insert: []byte("!")}})
	if err != nil {
		t.Fatal(err)
	}
	comp, err := zlibCompress(delta)
	if err != nil {
		t.Fatal(err)
	}
	body := packHeader(3)
	body = putTypeSize(body, kindBlob, uint64(len(base)))
	zbase, err := zlibCompress(base)
	if err != nil {
		t.Fatal(err)
	}
	baseAt := 12
	body = append(body, zbase...)
	ofsAt := len(body)
	off, err := EncodeOFS(uint64(ofsAt - baseAt))
	if err != nil {
		t.Fatal(err)
	}
	body = AppendRawEntry(body, kindOFS, uint64(len(delta)), append(off, comp...))
	expanded := HashObject("blob", []byte("hello!"))
	refComp, err := zlibCompress(delta)
	if err != nil {
		t.Fatal(err)
	}
	body = AppendRawEntry(body, kindREF, uint64(len(delta)), append(expanded[:], refComp...))
	body = FinishPack(body)
	if _, err := Decode(context.Background(), body); !errors.Is(err, ErrThinDelta) {
		t.Fatalf("ref of delta: %v", err)
	}
}

func TestDeltaDepthAndDeclaredSize(t *testing.T) {
	raw := []byte("hello")
	body := packHeader(34)
	body = putTypeSize(body, kindBlob, uint64(len(raw)))
	comp, err := zlibCompress(raw)
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, comp...)
	prev := 12
	delta, err := EncodeDelta(len(raw), len(raw), []DeltaInstruction{{Off: 0, Size: len(raw)}})
	if err != nil {
		t.Fatal(err)
	}
	dcomp, err := zlibCompress(delta)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 33; i++ {
		at := len(body)
		off, err := EncodeOFS(uint64(at - prev))
		if err != nil {
			t.Fatal(err)
		}
		body = AppendRawEntry(body, kindOFS, uint64(len(delta)), append(off, dcomp...))
		prev = at
	}
	body = FinishPack(body)
	if _, err := Decode(context.Background(), body); !errors.Is(err, ErrDeltaDepth) {
		t.Fatalf("depth: %v", err)
	}

	huge := packHeader(1)
	huge = putTypeSize(huge, kindBlob, uint64(bounds.MaxObjectBytes)+1)
	huge = append(huge, []byte("not-zlib")...)
	huge = FinishPack(huge)
	if _, err := Decode(context.Background(), huge); !errors.Is(err, ErrObjectTooLarge) {
		t.Fatalf("declared: %v", err)
	}
}

func TestDistinctOFSAndRefOutputs(t *testing.T) {
	base := []byte("base-bytes")
	ofsDelta, err := EncodeDelta(len(base), len("base-OFS"), []DeltaInstruction{{Off: 0, Size: 5}, {Insert: []byte("OFS")}})
	if err != nil {
		t.Fatal(err)
	}
	refDelta, err := EncodeDelta(len(base), len("base-REF"), []DeltaInstruction{{Off: 0, Size: 5}, {Insert: []byte("REF")}})
	if err != nil {
		t.Fatal(err)
	}
	zbase, err := zlibCompress(base)
	if err != nil {
		t.Fatal(err)
	}
	zofs, err := zlibCompress(ofsDelta)
	if err != nil {
		t.Fatal(err)
	}
	zref, err := zlibCompress(refDelta)
	if err != nil {
		t.Fatal(err)
	}
	body := packHeader(3)
	baseAt := len(body)
	body = append(putTypeSize(body, kindBlob, uint64(len(base))), zbase...)
	ofsAt := len(body)
	off, err := EncodeOFS(uint64(ofsAt - baseAt))
	if err != nil {
		t.Fatal(err)
	}
	body = AppendRawEntry(body, kindOFS, uint64(len(ofsDelta)), append(off, zofs...))
	baseHash := HashObject("blob", base)
	body = AppendRawEntry(body, kindREF, uint64(len(refDelta)), append(baseHash[:], zref...))
	got, err := Decode(context.Background(), FinishPack(body))
	if err != nil {
		t.Fatal(err)
	}
	ofs := got[HashObject("blob", []byte("base-OFS"))]
	ref := got[HashObject("blob", []byte("base-REF"))]
	if string(ofs.Data) != "base-OFS" || string(ref.Data) != "base-REF" || bytes.Equal(ofs.Data, ref.Data) || ofs.Hash == ref.Hash {
		t.Fatalf("ofs %q ref %q", ofs.Data, ref.Data)
	}
}

func TestAggregatePayloadLimit(t *testing.T) {
	zero := make([]byte, bounds.MaxObjectBytes)
	comp, err := zlibCompress(zero)
	if err != nil {
		t.Fatal(err)
	}
	const full = 16
	body := packHeader(full + 1)
	for i := 0; i < full; i++ {
		body = append(putTypeSize(body, kindBlob, uint64(len(zero))), comp...)
	}
	one := []byte{1}
	zone, err := zlibCompress(one)
	if err != nil {
		t.Fatal(err)
	}
	body = append(putTypeSize(body, kindBlob, uint64(len(one))), zone...)
	if _, err := Decode(context.Background(), FinishPack(body)); !errors.Is(err, ErrAggregate) {
		t.Fatalf("aggregate: %v", err)
	}
}

func TestCancelDuringInflateAndDelta(t *testing.T) {
	data := bytes.Repeat([]byte{1, 2, 3, 4}, (16*32*1024)/4+64)
	pack, err := Encode([]Object{{Type: "blob", Data: data, Hash: HashObject("blob", data)}})
	if err != nil {
		t.Fatal(err)
	}
	inflate := &budgetCtx{allow: 3}
	if _, err := Decode(inflate, pack); !errors.Is(err, context.Canceled) || inflate.calls < 4 {
		t.Fatalf("inflate calls %d err %v", inflate.calls, err)
	}

	base := []byte("bbbbbbbb")
	var steps []DeltaInstruction
	for i := 0; i < 1024; i++ {
		steps = append(steps, DeltaInstruction{Insert: []byte{'x'}})
	}
	delta, err := EncodeDelta(len(base), len(base)+1024, append([]DeltaInstruction{{Off: 0, Size: len(base)}}, steps...))
	if err != nil {
		t.Fatal(err)
	}
	zbase, err := zlibCompress(base)
	if err != nil {
		t.Fatal(err)
	}
	zdelta, err := zlibCompress(delta)
	if err != nil {
		t.Fatal(err)
	}
	body := packHeader(2)
	baseAt := len(body)
	body = append(putTypeSize(body, kindBlob, uint64(len(base))), zbase...)
	at := len(body)
	off, err := EncodeOFS(uint64(at - baseAt))
	if err != nil {
		t.Fatal(err)
	}
	body = AppendRawEntry(body, kindOFS, uint64(len(delta)), append(off, zdelta...))
	deltaCtx := &budgetCtx{allow: 9}
	if _, err := Decode(deltaCtx, FinishPack(body)); !errors.Is(err, context.Canceled) || deltaCtx.calls < 10 {
		t.Fatalf("delta calls %d err %v", deltaCtx.calls, err)
	}
}

type budgetCtx struct {
	allow int
	calls int
}

func (c *budgetCtx) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *budgetCtx) Done() <-chan struct{}       { return nil }
func (c *budgetCtx) Err() error {
	c.calls++
	if c.calls > c.allow {
		return context.Canceled
	}
	return nil
}
func (c *budgetCtx) Value(any) any { return nil }
