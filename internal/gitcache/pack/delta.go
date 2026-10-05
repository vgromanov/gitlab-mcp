package pack

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
)

func readVarint(b []byte) (uint64, int, error) {
	var v uint64
	var shift uint
	for i, c := range b {
		if shift >= 64 {
			return 0, 0, ErrMalformed
		}
		payload := uint64(c & 0x7f)
		if payload > math.MaxUint64>>shift {
			return 0, 0, ErrMalformed
		}
		v |= payload << shift
		if c&0x80 == 0 {
			return v, i + 1, nil
		}
		if shift > 64-7 {
			return 0, 0, ErrMalformed
		}
		shift += 7
	}
	return 0, 0, ErrMalformed
}

func appendVarint(dst []byte, n uint64) []byte {
	for {
		c := byte(n & 0x7f)
		n >>= 7
		if n > 0 {
			c |= 0x80
		}
		dst = append(dst, c)
		if n == 0 {
			return dst
		}
	}
}

// applyDelta writes the delta result only after the source and target varints
// are within the object limit. It does not allocate the output before that check.
func applyDelta(ctx context.Context, base, delta []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	srcSize, n, err := readVarint(delta)
	if err != nil {
		return nil, ErrMalformed
	}
	tgtSize, m, err := readVarint(delta[n:])
	if err != nil {
		return nil, ErrMalformed
	}
	if srcSize > uint64(bounds.MaxObjectBytes) || tgtSize > uint64(bounds.MaxObjectBytes) {
		return nil, ErrObjectTooLarge
	}
	if srcSize != uint64(len(base)) {
		return nil, ErrMalformed
	}
	out := make([]byte, tgtSize)
	rest := delta[n+m:]
	pos := 0
	step := 0
	for len(rest) > 0 {
		step++
		if step&0x3ff == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		cmd := rest[0]
		rest = rest[1:]
		if cmd == 0 {
			return nil, ErrMalformed
		}
		if cmd&0x80 == 0 {
			n := int(cmd)
			if n > len(rest) || pos+n > len(out) {
				return nil, ErrMalformed
			}
			copy(out[pos:pos+n], rest[:n])
			rest = rest[n:]
			pos += n
			continue
		}
		var off, size int
		for i := 0; i < 4; i++ {
			if cmd&(1<<uint(i)) == 0 {
				continue
			}
			if len(rest) == 0 {
				return nil, ErrMalformed
			}
			off |= int(rest[0]) << (8 * uint(i))
			rest = rest[1:]
		}
		for i := 0; i < 3; i++ {
			if cmd&(0x10<<uint(i)) == 0 {
				continue
			}
			if len(rest) == 0 {
				return nil, ErrMalformed
			}
			size |= int(rest[0]) << (8 * uint(i))
			rest = rest[1:]
		}
		if size == 0 {
			size = 0x10000
		}
		if off < 0 || size < 0 || off+size > len(base) || pos+size > len(out) {
			return nil, ErrMalformed
		}
		copy(out[pos:pos+size], base[off:off+size])
		pos += size
	}
	if pos != len(out) {
		return nil, ErrMalformed
	}
	return out, nil
}

func readOFS(b []byte) (uint64, int, error) {
	if len(b) == 0 {
		return 0, 0, ErrMalformed
	}
	c := b[0]
	i := 1
	offset := uint64(c & 0x7f)
	for c&0x80 != 0 {
		if i >= len(b) || i >= 10 {
			return 0, 0, ErrMalformed
		}
		c = b[i]
		i++
		if offset > (math.MaxUint64>>7)-1 {
			return 0, 0, ErrMalformed
		}
		offset = ((offset + 1) << 7) | uint64(c&0x7f)
	}
	if offset == 0 {
		return 0, 0, ErrMalformed
	}
	return offset, i, nil
}

// EncodeOFS encodes a pack OFS-delta distance for tests and local fixtures.
func EncodeOFS(n uint64) ([]byte, error) {
	if n == 0 {
		return nil, ErrMalformed
	}
	buf := make([]byte, 10)
	i := len(buf) - 1
	buf[i] = byte(n & 127)
	for n >>= 7; n > 0; n >>= 7 {
		i--
		if i < 0 {
			return nil, ErrMalformed
		}
		n--
		buf[i] = 128 | byte(n&127)
	}
	return buf[i:], nil
}

func putTypeSize(dst []byte, typ byte, size uint64) []byte {
	first := (typ << 4) | byte(size&0x0f)
	size >>= 4
	if size > 0 {
		first |= 0x80
	}
	dst = append(dst, first)
	for size > 0 {
		c := byte(size & 0x7f)
		size >>= 7
		if size > 0 {
			c |= 0x80
		}
		dst = append(dst, c)
	}
	return dst
}

func readTypeSize(b []byte) (typ byte, size uint64, n int, err error) {
	if len(b) == 0 {
		return 0, 0, 0, ErrMalformed
	}
	c := b[0]
	typ = (c >> 4) & 7
	size = uint64(c & 0x0f)
	shift := uint(4)
	i := 1
	for c&0x80 != 0 {
		if i >= len(b) || shift >= 64 {
			return 0, 0, 0, ErrMalformed
		}
		c = b[i]
		i++
		payload := uint64(c & 0x7f)
		if payload > math.MaxUint64>>shift {
			return 0, 0, 0, ErrMalformed
		}
		size |= payload << shift
		if c&0x80 != 0 && shift > 64-7 {
			return 0, 0, 0, ErrMalformed
		}
		shift += 7
	}
	return typ, size, i, nil
}

// DeltaInstruction is a test helper that copies base or inserts bytes.
type DeltaInstruction struct {
	Insert []byte
	Off    int
	Size   int
}

// EncodeDelta builds a delta instruction stream. It records sizes but does not
// allocate the expanded target.
func EncodeDelta(baseLen, targetLen int, steps []DeltaInstruction) ([]byte, error) {
	if baseLen < 0 || targetLen < 0 {
		return nil, ErrMalformed
	}
	var buf []byte
	buf = appendVarint(buf, uint64(baseLen))
	buf = appendVarint(buf, uint64(targetLen))
	for _, step := range steps {
		if step.Insert != nil {
			if len(step.Insert) == 0 || len(step.Insert) > 127 {
				return nil, fmt.Errorf("%w: insert length", ErrMalformed)
			}
			buf = append(buf, byte(len(step.Insert)))
			buf = append(buf, step.Insert...)
			continue
		}
		if step.Size <= 0 || step.Off < 0 {
			return nil, ErrMalformed
		}
		cmd := byte(0x80)
		var extra []byte
		off := step.Off
		for i := 0; i < 4; i++ {
			if off == 0 && i > 0 {
				break
			}
			if i > 0 && off == 0 {
				break
			}
			cmd |= 1 << uint(i)
			extra = append(extra, byte(off))
			off >>= 8
			if off == 0 {
				break
			}
		}
		size := step.Size
		if size != 0x10000 {
			for i := 0; i < 3 && size > 0; i++ {
				cmd |= 0x10 << uint(i)
				extra = append(extra, byte(size))
				size >>= 8
			}
		}
		buf = append(buf, cmd)
		buf = append(buf, extra...)
	}
	return buf, nil
}

func packHeader(count uint32) []byte {
	hdr := make([]byte, 12)
	copy(hdr, "PACK")
	binary.BigEndian.PutUint32(hdr[4:8], 2)
	binary.BigEndian.PutUint32(hdr[8:12], count)
	return hdr
}

// errString is a small wrapper so static sentinels stay unwrap-able.
func reject(sentinel error, why string) error {
	if why == "" {
		return sentinel
	}
	return fmt.Errorf("%w: %s", sentinel, why)
}
