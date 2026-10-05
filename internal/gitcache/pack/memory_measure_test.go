package pack

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"testing"
)

// TestMemoryMeasureTinyFixtureOnly records the sub-KiB sanity samples.
// These are explicitly NOT a production memory bound. Representative (>3475
// paths) and MiB-scale delta/malformed evidence come from the precompiled
// helper: internal/gitcache/stage/memprobe.
func TestMemoryMeasureTinyFixtureOnly(t *testing.T) {
	runtime.GC()
	var before, after runtime.MemStats

	blob := Object{Type: "blob", Data: []byte("tiny-fixture")}
	blob.Hash = HashObject("blob", blob.Data)
	raw, err := Encode([]Object{blob})
	if err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&before)
	got, err := Decode(context.Background(), raw)
	if err != nil || len(got) != 1 {
		t.Fatalf("tiny decode: %v len=%d", err, len(got))
	}
	runtime.ReadMemStats(&after)
	fmt.Fprintf(os.Stdout, "memory_measure label=tiny_fixture pack_bytes=%d objects=1 heap_delta_bytes=%d note=%q\n",
		len(raw), int64(after.HeapAlloc)-int64(before.HeapAlloc),
		"NOT a production memory bound; see stage/memprobe for representative/MiB evidence")

	runtime.GC()
	runtime.ReadMemStats(&before)
	truncLen := 16
	if truncLen > len(raw) {
		truncLen = len(raw)
	}
	_, err = Decode(context.Background(), raw[:truncLen])
	runtime.ReadMemStats(&after)
	fmt.Fprintf(os.Stdout, "memory_measure label=tiny_malformed_truncated err=%v heap_delta_bytes=%d note=%q\n",
		err != nil, int64(after.HeapAlloc)-int64(before.HeapAlloc),
		"NOT a production memory bound; tiny truncated pack only")
	if err == nil {
		t.Fatal("truncated pack accepted")
	}
}
