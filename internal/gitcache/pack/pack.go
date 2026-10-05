// Package pack decodes a version-2 pack with hard size limits.
//
// This is a custom decoder. It does not call go-git's packfile parser, so it
// does not inherit that parser's compatibility behavior. A REF delta is
// accepted only when its base is an undeltified object in the same pack.
// A REF base that is itself a delta is rejected. OFS chains are limited to
// bounds.MaxDeltaDepth. Delta outputs are allocated only after the base and
// target varints pass the size checks.
package pack

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"

	"github.com/go-git/go-git/v5/plumbing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
)

const (
	kindCommit = 1
	kindTree   = 2
	kindBlob   = 3
	kindTag    = 4
	kindOFS    = 6
	kindREF    = 7
)

var (
	ErrMalformed      = errors.New("malformed pack")
	ErrObjectTooLarge = errors.New("object exceeds the inflated size limit")
	ErrAggregate      = errors.New("inflated objects exceed the aggregate limit")
	ErrDeltaDepth     = errors.New("delta chain exceeds the depth limit")
	ErrThinDelta      = errors.New("thin or external delta base is rejected before expansion")
	ErrTooManyObjects = errors.New("pack object count exceeds the limit")
	ErrChecksum       = errors.New("pack checksum mismatch")
)

// Object is one undeltified git object. Data is the raw content without the
// "type size" header. Hash is the git object id.
type Object struct {
	Type string
	Data []byte
	Hash plumbing.Hash
}

// HashObject returns the git SHA-1 of type and raw content.
func HashObject(kind string, data []byte) plumbing.Hash {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", kind, len(data))
	_, _ = h.Write(data)
	var out plumbing.Hash
	copy(out[:], h.Sum(nil))
	return out
}

type entry struct {
	at         int
	kind       byte
	data       []byte
	baseHash   plumbing.Hash
	baseAt     int
	isDelta    bool
	hash       plumbing.Hash
	headerSize uint64
	baseSize   uint64
	targetSize uint64
}

// Decode inflates a pack under the process limits. Unsafe deltas are rejected
// before their output buffer is allocated. The stock pack parser is not called.
func Decode(ctx context.Context, pack []byte) (map[plumbing.Hash]Object, error) {
	objs, _, _, err := decodeIndexed(ctx, pack)
	return objs, err
}

func decodeIndexed(ctx context.Context, pack []byte) (map[plumbing.Hash]Object, []IndexEntry, plumbing.Hash, error) {
	return decodeIndexedLimits(ctx, pack, DefaultLimits())
}

// Limits admits retained earlier role objects before decoding another role pack.
type Limits struct {
	Objects                 int
	InputBytes, OutputBytes int64
}

func DefaultLimits() Limits {
	return Limits{bounds.MaxObjects, bounds.MaxRetainedInput, bounds.MaxRetainedOutput}
}
func decodeIndexedLimits(ctx context.Context, pack []byte, limits Limits) (map[plumbing.Hash]Object, []IndexEntry, plumbing.Hash, error) {
	if limits.Objects < 0 || limits.Objects > bounds.MaxObjects || limits.InputBytes < 0 || limits.InputBytes > bounds.MaxRetainedInput || limits.OutputBytes < 0 || limits.OutputBytes > bounds.MaxRetainedOutput {
		return nil, nil, plumbing.ZeroHash, ErrAggregate
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var zero plumbing.Hash
	if err := ctx.Err(); err != nil {
		return nil, nil, zero, err
	}
	if int64(len(pack)) > bounds.MaxPackBytes {
		return nil, nil, zero, ErrAggregate
	}
	if len(pack) < 32 || string(pack[:4]) != "PACK" {
		return nil, nil, zero, reject(ErrMalformed, "signature")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 {
		return nil, nil, zero, reject(ErrMalformed, "version")
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	if count < 0 || count > limits.Objects {
		return nil, nil, zero, ErrTooManyObjects
	}
	sum := sha1.Sum(pack[:len(pack)-20])
	if !bytes.Equal(sum[:], pack[len(pack)-20:]) {
		return nil, nil, zero, ErrChecksum
	}
	var packSum plumbing.Hash
	copy(packSum[:], pack[len(pack)-20:])
	body := pack[:len(pack)-20]
	entries := make([]entry, 0, count)
	var inflated int64
	pos := 12
	for len(entries) < count {
		if err := ctx.Err(); err != nil {
			return nil, nil, zero, err
		}
		if pos >= len(body) {
			return nil, nil, zero, reject(ErrMalformed, "truncated")
		}
		typ, size, n, err := readTypeSize(body[pos:])
		if err != nil {
			return nil, nil, zero, err
		}
		if size > uint64(bounds.MaxObjectBytes) {
			return nil, nil, zero, ErrObjectTooLarge
		}
		if inflated+int64(size) > limits.InputBytes {
			return nil, nil, zero, ErrAggregate
		}
		at := pos
		pos += n
		e := entry{at: at, kind: typ, headerSize: size}
		switch typ {
		case kindOFS:
			off, n, err := readOFS(body[pos:])
			if err != nil {
				return nil, nil, zero, err
			}
			if uint64(at) < off {
				return nil, nil, zero, ErrThinDelta
			}
			e.baseAt = at - int(off)
			e.isDelta = true
			pos += n
		case kindREF:
			if pos+20 > len(body) {
				return nil, nil, zero, reject(ErrMalformed, "ref delta")
			}
			copy(e.baseHash[:], body[pos:pos+20])
			e.isDelta = true
			pos += 20
		case kindCommit, kindTree, kindBlob, kindTag:
		default:
			return nil, nil, zero, reject(ErrMalformed, "object type")
		}
		payload, consumed, err := inflateAt(ctx, body[pos:], int(size))
		if err != nil {
			return nil, nil, zero, err
		}
		if uint64(len(payload)) != size {
			return nil, nil, zero, reject(ErrMalformed, "inflated size")
		}
		inflated += int64(len(payload))
		if inflated > limits.InputBytes {
			return nil, nil, zero, ErrAggregate
		}
		e.data = payload
		pos += consumed
		entries = append(entries, e)
	}
	if pos != len(body) {
		return nil, nil, zero, reject(ErrMalformed, "trailing bytes")
	}
	if len(entries) != count {
		return nil, nil, zero, reject(ErrMalformed, "object count")
	}
	byAt := make(map[int]int, len(entries))
	byHash := make(map[plumbing.Hash]int, len(entries))
	for i := range entries {
		if err := ctx.Err(); err != nil {
			return nil, nil, zero, err
		}
		byAt[entries[i].at] = i
		if entries[i].isDelta {
			continue
		}
		h := HashObject(kindName(entries[i].kind), entries[i].data)
		entries[i].hash = h
		if _, exists := byHash[h]; !exists {
			byHash[h] = i
		}
	}
	for i := range entries {
		if err := ctx.Err(); err != nil {
			return nil, nil, zero, err
		}
		if !entries[i].isDelta {
			continue
		}
		src, tgt, err := deltaSizes(entries[i].data)
		if err != nil {
			return nil, nil, zero, err
		}
		if src > uint64(bounds.MaxObjectBytes) || tgt > uint64(bounds.MaxObjectBytes) {
			return nil, nil, zero, ErrObjectTooLarge
		}
		entries[i].baseSize = src
		entries[i].targetSize = tgt
		if entries[i].kind == kindREF {
			if _, ok := byHash[entries[i].baseHash]; !ok {
				return nil, nil, zero, ErrThinDelta
			}
		} else if _, ok := byAt[entries[i].baseAt]; !ok {
			return nil, nil, zero, ErrThinDelta
		}
	}
	var finalSum int64
	for i := range entries {
		if err := ctx.Err(); err != nil {
			return nil, nil, zero, err
		}
		if entries[i].isDelta {
			finalSum += int64(entries[i].targetSize)
		} else {
			finalSum += int64(len(entries[i].data))
		}
	}
	if finalSum > limits.OutputBytes {
		return nil, nil, zero, ErrAggregate
	}

	resolved := make([]Object, len(entries))
	done := make([]bool, len(entries))
	depths := make([]int, len(entries))
	for i := range entries {
		if _, _, err := resolve(ctx, entries, i, byAt, byHash, resolved, done, depths, map[int]bool{}); err != nil {
			return nil, nil, zero, err
		}
	}
	out := make(map[plumbing.Hash]Object, len(resolved))
	for _, obj := range resolved {
		if prev, ok := out[obj.Hash]; ok && (prev.Type != obj.Type || !bytes.Equal(prev.Data, obj.Data)) {
			return nil, nil, zero, ErrChecksum
		}
		out[obj.Hash] = obj
	}

	idxEntries := make([]IndexEntry, 0, len(entries))
	for i := range entries {
		if err := ctx.Err(); err != nil {
			return nil, nil, zero, err
		}
		end := len(body)
		if i+1 < len(entries) {
			end = entries[i+1].at
		}
		crc, err := objectCRC(pack, entries[i].at, end)
		if err != nil {
			return nil, nil, zero, err
		}
		idxEntries = append(idxEntries, IndexEntry{
			Hash:   resolved[i].Hash,
			Offset: uint64(entries[i].at),
			CRC32:  crc,
		})
	}
	return out, idxEntries, packSum, nil
}

func deltaSizes(data []byte) (uint64, uint64, error) {
	src, n, err := readVarint(data)
	if err != nil {
		return 0, 0, err
	}
	tgt, _, err := readVarint(data[n:])
	if err != nil {
		return 0, 0, err
	}
	return src, tgt, nil
}

func resolve(ctx context.Context, entries []entry, i int, byAt map[int]int, byHash map[plumbing.Hash]int, resolved []Object, done []bool, depths []int, stack map[int]bool) (Object, int, error) {
	if err := ctx.Err(); err != nil {
		return Object{}, 0, err
	}
	if done[i] {
		return resolved[i], depths[i], nil
	}
	if stack[i] {
		return Object{}, 0, reject(ErrMalformed, "delta cycle")
	}
	e := entries[i]
	if !e.isDelta {
		obj := Object{Type: kindName(e.kind), Data: e.data, Hash: e.hash}
		resolved[i] = obj
		done[i] = true
		return obj, 0, nil
	}
	var baseIdx int
	var ok bool
	if e.kind == kindREF {
		baseIdx, ok = byHash[e.baseHash]
	} else {
		baseIdx, ok = byAt[e.baseAt]
	}
	if !ok {
		return Object{}, 0, ErrThinDelta
	}
	stack[i] = true
	base, baseDepth, err := resolve(ctx, entries, baseIdx, byAt, byHash, resolved, done, depths, stack)
	delete(stack, i)
	if err != nil {
		return Object{}, 0, err
	}
	if baseDepth+1 > bounds.MaxDeltaDepth {
		return Object{}, 0, ErrDeltaDepth
	}
	if uint64(len(base.Data)) != e.baseSize {
		return Object{}, 0, ErrMalformed
	}
	if int64(e.targetSize) > bounds.MaxObjectBytes {
		return Object{}, 0, ErrObjectTooLarge
	}
	expanded, err := applyDelta(ctx, base.Data, e.data)
	if err != nil {
		return Object{}, 0, err
	}
	if uint64(len(expanded)) != e.targetSize {
		return Object{}, 0, ErrMalformed
	}
	obj := Object{Type: base.Type, Data: expanded, Hash: HashObject(base.Type, expanded)}
	resolved[i] = obj
	depths[i] = baseDepth + 1
	done[i] = true
	return obj, depths[i], nil
}

func kindName(k byte) string {
	switch k {
	case kindCommit:
		return "commit"
	case kindTree:
		return "tree"
	case kindBlob:
		return "blob"
	case kindTag:
		return "tag"
	default:
		return ""
	}
}

func inflateAt(ctx context.Context, b []byte, declared int) ([]byte, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if declared < 0 || int64(declared) > bounds.MaxObjectBytes {
		return nil, 0, ErrObjectTooLarge
	}
	br := bytes.NewReader(b)
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, ErrMalformed
	}
	defer zr.Close()
	dst := make([]byte, 0, declared)
	buf := make([]byte, 32*1024)
	reads := 0
	for {
		reads++
		if reads&0x0f == 0 {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
		}
		n, rerr := zr.Read(buf)
		if n > 0 {
			if int64(len(dst)+n) > bounds.MaxObjectBytes || len(dst)+n > declared {
				return nil, 0, ErrObjectTooLarge
			}
			dst = append(dst, buf[:n]...)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, 0, ErrMalformed
		}
	}
	consumed := len(b) - br.Len()
	if consumed <= 0 {
		return nil, 0, ErrMalformed
	}
	return dst, consumed, nil
}

// Encode writes an undeltified version-2 pack. It is used by local fixtures.
func Encode(objs []Object) ([]byte, error) {
	if len(objs) > bounds.MaxObjects {
		return nil, ErrTooManyObjects
	}
	var body []byte
	body = append(body, packHeader(uint32(len(objs)))...)
	for _, obj := range objs {
		if int64(len(obj.Data)) > bounds.MaxObjectBytes {
			return nil, ErrObjectTooLarge
		}
		typ, err := nameKind(obj.Type)
		if err != nil {
			return nil, err
		}
		body = putTypeSize(body, typ, uint64(len(obj.Data)))
		comp, err := zlibCompress(obj.Data)
		if err != nil {
			return nil, err
		}
		body = append(body, comp...)
	}
	sum := sha1.Sum(body)
	body = append(body, sum[:]...)
	if int64(len(body)) > bounds.MaxPackBytes {
		return nil, ErrAggregate
	}
	return body, nil
}

func nameKind(name string) (byte, error) {
	switch name {
	case "commit":
		return kindCommit, nil
	case "tree":
		return kindTree, nil
	case "blob":
		return kindBlob, nil
	case "tag":
		return kindTag, nil
	default:
		return 0, ErrMalformed
	}
}

func zlibCompress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// AppendRawEntry appends one already-typed pack entry, used by tests that
// build delta objects. The caller supplies the on-wire bytes after the header
// size (OFS/REF prefix plus zlib payload).
func AppendRawEntry(dst []byte, typ byte, declared uint64, rest []byte) []byte {
	dst = putTypeSize(dst, typ, declared)
	return append(dst, rest...)
}

// FinishPack appends the SHA-1 trailer.
func FinishPack(body []byte) []byte {
	sum := sha1.Sum(body)
	return append(body, sum[:]...)
}
