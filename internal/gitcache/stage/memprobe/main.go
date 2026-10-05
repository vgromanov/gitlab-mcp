//go:build memprobe

// Command memprobe measures peak RSS / heap for gitcache pack decode fixtures.
// Excluded from default ./... (requires -tags=memprobe):
//
//	CGO_ENABLED=0 go build -mod=vendor -tags=memprobe -o /tmp/gitcache-memprobe ./internal/gitcache/stage/memprobe
//	/tmp/gitcache-memprobe
//
// Labels:
//   - tiny_fixture: sub-KiB sanity (NOT a production memory bound)
//   - representative_paths: >3475-path tree walk decode
//   - mib_delta_expand / mib_malformed_limit: bounded MiB-scale workloads
package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"golang.org/x/sys/unix"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/pack"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/tree"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "memprobe: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	runtime.LockOSThread()
	if err := tinyFixture(); err != nil {
		return err
	}
	if err := representativePaths(3500); err != nil {
		return err
	}
	if err := mibDeltaExpand(); err != nil {
		return err
	}
	return mibMalformedLimit()
}

func report(label, note string, packBytes, objects, paths int, retainedOut, heapEnd uint64, heapDelta, rssPeak int64, elapsed time.Duration, ok bool) {
	fmt.Printf("memory_measure label=%s pack_bytes=%d objects=%d paths=%d retained_output_bytes=%d heap_end_bytes=%d heap_delta_bytes=%d peak_rss_bytes=%d elapsed_ns=%d ok=%v note=%q\n",
		label, packBytes, objects, paths, retainedOut, heapEnd, heapDelta, rssPeak, elapsed.Nanoseconds(), ok, note)
}

func rssBytes() int64 {
	var ru unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &ru); err != nil {
		return -1
	}
	if runtime.GOOS == "linux" {
		return int64(ru.Maxrss) * 1024
	}
	return int64(ru.Maxrss)
}

func measureDecode(label, note string, packBytes []byte, expectErr bool, afterOK func(pack.IndexedPack) (paths int, err error)) error {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_ = rssBytes()
	start := time.Now()
	ip, err := pack.DecodeIndexed(context.Background(), packBytes)
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	peak := rssBytes()
	paths := 0
	if err == nil && afterOK != nil {
		paths, err = afterOK(ip)
	}
	ok := (err != nil) == expectErr
	if !expectErr && err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if expectErr && err == nil {
		return fmt.Errorf("%s: expected error", label)
	}
	report(label, note, len(packBytes), len(ip.Objects), paths, uint64(ip.RetainedOutputBytes()), after.HeapAlloc,
		int64(after.HeapAlloc)-int64(before.HeapAlloc), peak, elapsed, ok)
	return nil
}

func tinyFixture() error {
	blob := pack.Object{Type: "blob", Data: []byte("tiny-fixture")}
	blob.Hash = pack.HashObject("blob", blob.Data)
	raw, err := pack.Encode([]pack.Object{blob})
	if err != nil {
		return err
	}
	return measureDecode("tiny_fixture", "NOT a production memory bound; sub-KiB sanity only", raw, false, nil)
}

func representativePaths(n int) error {
	if n <= 3475 {
		return fmt.Errorf("representative path count must be >3475, got %d", n)
	}
	blob := pack.Object{Type: "blob", Data: []byte("x")}
	blob.Hash = pack.HashObject("blob", blob.Data)
	entries := make([]tree.TreeEntry, n)
	for i := 0; i < n; i++ {
		entries[i] = tree.TreeEntry{Mode: tree.ModeFile, Name: fmt.Sprintf("p%05d.txt", i), Hash: blob.Hash}
	}
	treeBody, err := tree.EncodeTree(entries)
	if err != nil {
		return err
	}
	treeObj := pack.Object{Type: "tree", Data: treeBody, Hash: pack.HashObject("tree", treeBody)}
	var commitBody bytes.Buffer
	fmt.Fprintf(&commitBody, "tree %s\n", treeObj.Hash)
	commitBody.WriteString("author Mem Probe <mem@probe> 1 +0000\n")
	commitBody.WriteString("committer Mem Probe <mem@probe> 1 +0000\n\npaths\n")
	cb := commitBody.Bytes()
	commitObj := pack.Object{Type: "commit", Data: cb, Hash: pack.HashObject("commit", cb)}
	raw, err := pack.Encode([]pack.Object{blob, treeObj, commitObj})
	if err != nil {
		return err
	}
	return measureDecode("representative_paths", fmt.Sprintf("%d-path tree; MaxObjects=%d", n, bounds.MaxObjects), raw, false,
		func(ip pack.IndexedPack) (int, error) {
			walked, err := tree.Walk(context.Background(), tree.Map(ip.Objects), treeObj.Hash)
			if err != nil {
				return 0, err
			}
			if len(walked) <= 3475 {
				return len(walked), fmt.Errorf("path count %d not >3475", len(walked))
			}
			if int64(ip.PackByteCount) > bounds.MaxRetainedInput {
				return len(walked), fmt.Errorf("pack input exceeds MaxRetainedInput")
			}
			if ip.RetainedOutputBytes() > bounds.MaxRetainedOutput {
				return len(walked), fmt.Errorf("output exceeds MaxRetainedOutput")
			}
			return len(walked), nil
		})
}

func mibDeltaExpand() error {
	const baseSize = 2 << 20
	base := make([]byte, baseSize)
	for i := range base {
		base[i] = byte(i)
	}
	insert := make([]byte, 2<<20)
	for i := range insert {
		insert[i] = byte(255 - i%256)
	}
	targetLen := baseSize + len(insert)
	if int64(targetLen) > bounds.MaxObjectBytes {
		return fmt.Errorf("target %d exceeds MaxObjectBytes", targetLen)
	}
	steps := []pack.DeltaInstruction{{Off: 0, Size: len(base)}}
	// Git delta inserts are at most 127 bytes each.
	for off := 0; off < len(insert); {
		n := 127
		if n > len(insert)-off {
			n = len(insert) - off
		}
		steps = append(steps, pack.DeltaInstruction{Insert: insert[off : off+n]})
		off += n
	}
	delta, err := pack.EncodeDelta(len(base), targetLen, steps)
	if err != nil {
		return err
	}
	zbase, err := zlibCompress(base)
	if err != nil {
		return err
	}
	zdelta, err := zlibCompress(delta)
	if err != nil {
		return err
	}
	body := packHeader(2)
	baseAt := len(body)
	body = putTypeSize(body, 3 /* blob */, uint64(len(base)))
	body = append(body, zbase...)
	at := len(body)
	off, err := pack.EncodeOFS(uint64(at - baseAt))
	if err != nil {
		return err
	}
	body = pack.AppendRawEntry(body, 6 /* ofs delta */, uint64(len(delta)), append(off, zdelta...))
	raw := pack.FinishPack(body)
	return measureDecode("mib_delta_expand", "2MiB base + 2MiB OFS insert; MaxObjectBytes-bounded", raw, false,
		func(ip pack.IndexedPack) (int, error) {
			if ip.RetainedOutputBytes() < int64(targetLen) {
				return 0, fmt.Errorf("retained output %d < target %d", ip.RetainedOutputBytes(), targetLen)
			}
			sim := int64(len(ip.PackBytes)) + ip.RetainedOutputBytes()
			if sim > bounds.MaxRetainedInput+bounds.MaxRetainedOutput {
				return 0, fmt.Errorf("simultaneous retain %d exceeds dual 128MiB budgets", sim)
			}
			return 0, nil
		})
}

func mibMalformedLimit() error {
	body := packHeader(1)
	body = putTypeSize(body, 3, uint64(bounds.MaxObjectBytes)+1)
	z, err := zlibCompress([]byte{1})
	if err != nil {
		return err
	}
	body = append(body, z...)
	raw := pack.FinishPack(body)
	return measureDecode("mib_malformed_limit", "declared size > MaxObjectBytes; fail-closed", raw, true, nil)
}

func packHeader(count uint32) []byte {
	return []byte{'P', 'A', 'C', 'K', 0, 0, 0, 2, byte(count >> 24), byte(count >> 16), byte(count >> 8), byte(count)}
}

func putTypeSize(dst []byte, typ byte, size uint64) []byte {
	b := byte((typ << 4) | byte(size&0x0f))
	size >>= 4
	if size != 0 {
		b |= 0x80
	}
	dst = append(dst, b)
	for size != 0 {
		b = byte(size & 0x7f)
		size >>= 7
		if size != 0 {
			b |= 0x80
		}
		dst = append(dst, b)
	}
	return dst
}

func zlibCompress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
