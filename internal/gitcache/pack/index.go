package pack

import (
	"bytes"
	"context"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
)

// IndexEntry is one pack-index row: object hash, pack offset, and CRC32 of the
// packed object bytes (header through compressed payload), matching git idx v2.
type IndexEntry struct {
	Hash   plumbing.Hash
	Offset uint64
	CRC32  uint32
}

// IndexedPack is a bounded decode result plus the data required to build a
// real pack index v2 without re-entering the stock packfile parser.
// PackBytes aliases the caller slice (not a copy). Objects is the sole
// retained resolved-output map for this decode.
type IndexedPack struct {
	Objects       map[plumbing.Hash]Object
	Entries       []IndexEntry
	PackChecksum  plumbing.Hash
	PackBytes     []byte
	PackByteCount int
}

// RetainedOutputBytes sums resolved object payloads currently held in Objects.
func (ip IndexedPack) RetainedOutputBytes() int64 {
	var n int64
	for _, o := range ip.Objects {
		n += int64(len(o.Data))
	}
	return n
}

// DecodeIndexed decodes pack bytes under the process limits and records
// validated offsets/CRCs for index-v2 construction. One Objects map only.
func DecodeIndexed(ctx context.Context, packBytes []byte) (IndexedPack, error) {
	return DecodeIndexedLimits(ctx, packBytes, DefaultLimits())
}

func DecodeIndexedLimits(ctx context.Context, packBytes []byte, limits Limits) (IndexedPack, error) {
	objs, entries, sum, err := decodeIndexedLimits(ctx, packBytes, limits)
	if err != nil {
		return IndexedPack{}, err
	}
	return IndexedPack{
		Objects:       objs,
		Entries:       entries,
		PackChecksum:  sum,
		PackBytes:     packBytes,
		PackByteCount: len(packBytes),
	}, nil
}

// BuildIndexV2 writes a git pack index v2 using go-git's idxfile Writer and
// Encoder. It admits the exact encoded size from IndexV2SHA1Bytes before
// allocating the output buffer. Stock packfile.Parser is not used for size
// enforcement; offsets/CRCs come from the bounded decoder.
func BuildIndexV2(ctx context.Context, ip IndexedPack) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(ip.Entries) == 0 || len(ip.Entries) > bounds.MaxObjects {
		return nil, ErrTooManyObjects
	}
	unique := make([]IndexEntry, 0, len(ip.Objects))
	seen := make(map[plumbing.Hash]struct{}, len(ip.Objects))
	for _, e := range ip.Entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		obj, ok := ip.Objects[e.Hash]
		if !ok || obj.Hash != e.Hash {
			return nil, ErrChecksum
		}
		if e.Offset >= uint64(ip.PackByteCount) {
			return nil, ErrMalformed
		}
		if _, dup := seen[e.Hash]; dup {
			continue
		}
		seen[e.Hash] = struct{}{}
		unique = append(unique, e)
	}
	if len(unique) != len(ip.Objects) {
		return nil, ErrChecksum
	}
	// Packs are capped at MaxPackBytes << 2^31, so no offset64 table is used.
	want, err := bounds.IndexV2SHA1Bytes(len(unique))
	if err != nil {
		return nil, ErrTooManyObjects
	}
	if want > bounds.MaxIndexBytes {
		return nil, ErrAggregate
	}

	w := new(idxfile.Writer)
	if err := w.OnHeader(uint32(len(unique))); err != nil {
		return nil, err
	}
	for _, e := range unique {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		w.Add(e.Hash, e.Offset, e.CRC32)
	}
	if err := w.OnFooter(ip.PackChecksum); err != nil {
		return nil, err
	}
	idx, err := w.Index()
	if err != nil {
		return nil, err
	}

	// Admit exact size, then encode into a pre-sized buffer (no grow past want).
	lw := &limitedBuffer{max: want}
	lw.buf.Grow(int(want))
	enc := idxfile.NewEncoder(lw)
	wrote, err := enc.Encode(idx)
	if err != nil {
		return nil, err
	}
	if int64(wrote) != want || int64(lw.buf.Len()) != want {
		return nil, fmt.Errorf("%w: index size %d want %d", ErrAggregate, wrote, want)
	}
	return lw.buf.Bytes(), nil
}

// BuildIndexV2FromPack is a test/helper that decodes once then builds index v2.
// Production acquire/publish must call DecodeIndexed once and BuildIndexV2 on
// that IndexedPack — never decode again while retaining a prior Objects map.
// The objs argument, when non-nil, is an integrity check against the single
// decode result (not a second retained output set used for indexing).
func BuildIndexV2FromPack(ctx context.Context, packBytes []byte, objs map[plumbing.Hash]Object) ([]byte, error) {
	ip, err := DecodeIndexed(ctx, packBytes)
	if err != nil {
		return nil, err
	}
	if objs != nil {
		if len(objs) != len(ip.Objects) {
			return nil, ErrChecksum
		}
		for h, o := range objs {
			got, ok := ip.Objects[h]
			if !ok || got.Type != o.Type || !bytes.Equal(got.Data, o.Data) || got.Hash != o.Hash {
				return nil, ErrChecksum
			}
		}
	}
	// Drop the caller's objs reference from the live set before indexing so
	// this helper does not itself keep two output maps; callers that still
	// hold objs must not use this on the hot publish path.
	return BuildIndexV2(ctx, ip)
}

type limitedBuffer struct {
	buf bytes.Buffer
	max int64
	n   int64
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.n+int64(len(p)) > l.max {
		return 0, ErrAggregate
	}
	n, err := l.buf.Write(p)
	l.n += int64(n)
	return n, err
}

var _ io.Writer = (*limitedBuffer)(nil)

func objectCRC(packBytes []byte, start, end int) (uint32, error) {
	if start < 0 || end < start || end > len(packBytes) {
		return 0, ErrMalformed
	}
	return crc32.ChecksumIEEE(packBytes[start:end]), nil
}
