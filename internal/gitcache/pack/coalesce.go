package pack

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"hash/crc32"
	"sort"

	"github.com/go-git/go-git/v5/plumbing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
)

// MergeObjects checks collisions before discarding the duplicate. Payloads alias
// their sole bounded decode; the role aggregate is admitted before retention.
func MergeObjects(ctx context.Context, dst, src map[plumbing.Hash]Object) error {
	var n int64
	for _, o := range dst {
		n += int64(len(o.Data))
	}
	for h, o := range src {
		if err := ctx.Err(); err != nil {
			return err
		}
		if old, ok := dst[h]; ok {
			if old.Hash != h || o.Hash != h || old.Type != o.Type || !bytes.Equal(old.Data, o.Data) {
				return ErrChecksum
			}
			continue
		}
		if o.Hash != h || HashObject(o.Type, o.Data) != h {
			return ErrChecksum
		}
		if len(dst) >= bounds.MaxObjects || int64(len(o.Data)) > bounds.MaxObjectBytes {
			return ErrTooManyObjects
		}
		n += int64(len(o.Data))
		if n > bounds.MaxRetainedOutput {
			return ErrAggregate
		}
		dst[h] = o
	}
	return nil
}

// EncodeIndexed encodes already validated objects once without another decode or
// inflated payload copy. All compressed writes and local work honor ctx and the
// raw-pack ceiling, including the trailer.
func EncodeIndexed(ctx context.Context, objs map[plumbing.Hash]Object) (IndexedPack, error) {
	if len(objs) == 0 || len(objs) > bounds.MaxObjects {
		return IndexedPack{}, ErrTooManyObjects
	}
	keys := make([]plumbing.Hash, 0, len(objs))
	var total int64
	for h, o := range objs {
		if err := ctx.Err(); err != nil {
			return IndexedPack{}, err
		}
		if o.Hash != h || HashObject(o.Type, o.Data) != h {
			return IndexedPack{}, ErrChecksum
		}
		if int64(len(o.Data)) > bounds.MaxObjectBytes {
			return IndexedPack{}, ErrObjectTooLarge
		}
		total += int64(len(o.Data))
		if total > bounds.MaxRetainedOutput {
			return IndexedPack{}, ErrAggregate
		}
		keys = append(keys, h)
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i][:], keys[j][:]) < 0 })
	b := &limitedBuffer{max: bounds.MaxPackBytes - 20}
	if _, err := b.Write(packHeader(uint32(len(keys)))); err != nil {
		return IndexedPack{}, err
	}
	entries := make([]IndexEntry, 0, len(keys))
	for _, h := range keys {
		if err := ctx.Err(); err != nil {
			return IndexedPack{}, err
		}
		o := objs[h]
		typ, err := nameKind(o.Type)
		if err != nil {
			return IndexedPack{}, err
		}
		offset := b.buf.Len()
		if _, err = b.Write(putTypeSize(nil, typ, uint64(len(o.Data)))); err != nil {
			return IndexedPack{}, err
		}
		z := zlib.NewWriter(b)
		for pos := 0; pos < len(o.Data); {
			if err := ctx.Err(); err != nil {
				_ = z.Close()
				return IndexedPack{}, err
			}
			end := pos + 32768
			if end > len(o.Data) {
				end = len(o.Data)
			}
			if _, err := z.Write(o.Data[pos:end]); err != nil {
				_ = z.Close()
				return IndexedPack{}, err
			}
			pos = end
		}
		if err := z.Close(); err != nil {
			return IndexedPack{}, err
		}
		entries = append(entries, IndexEntry{h, uint64(offset), crc32.ChecksumIEEE(b.buf.Bytes()[offset:])})
	}
	sum := sha1.Sum(b.buf.Bytes())
	b.max = bounds.MaxPackBytes
	if _, err := b.Write(sum[:]); err != nil {
		return IndexedPack{}, err
	}
	return IndexedPack{Objects: objs, Entries: entries, PackChecksum: plumbing.Hash(sum), PackBytes: b.buf.Bytes(), PackByteCount: b.buf.Len()}, nil
}
