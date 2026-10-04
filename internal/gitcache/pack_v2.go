package gitcache

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
)

// PackV2EmptyBlob is one PACKv2 SHA-1 pack containing a single empty blob.
// Native index tests write this file and let the pinned Git build the idx.
func PackV2EmptyBlob() ([]byte, error) {
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	if err := zw.Close(); err != nil {
		return nil, err
	}
	var pack bytes.Buffer
	pack.WriteString("PACK")
	if err := binary.Write(&pack, binary.BigEndian, uint32(2)); err != nil {
		return nil, err
	}
	if err := binary.Write(&pack, binary.BigEndian, uint32(1)); err != nil {
		return nil, err
	}
	pack.WriteByte(0x30) // blob, size 0
	pack.Write(zbuf.Bytes())
	sum := sha1.Sum(pack.Bytes())
	pack.Write(sum[:])
	return pack.Bytes(), nil
}
