package pack_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/pack"
)

func TestBuildIndexV2ExactSizeAndRoundTrip(t *testing.T) {
	blob := pack.Object{Type: "blob", Data: []byte("index-v2")}
	blob.Hash = pack.HashObject("blob", blob.Data)
	packBytes, err := pack.Encode([]pack.Object{blob})
	if err != nil {
		t.Fatal(err)
	}
	ip, err := pack.DecodeIndexed(context.Background(), packBytes)
	if err != nil {
		t.Fatal(err)
	}
	want, err := bounds.IndexV2SHA1Bytes(len(ip.Entries))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := pack.BuildIndexV2(context.Background(), ip)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(idx)) != want {
		t.Fatalf("size %d want %d", len(idx), want)
	}
	if !bytes.Equal(idx[:4], []byte{255, 't', 'O', 'c'}) {
		t.Fatalf("missing idx v2 magic: %x", idx[:4])
	}
	dec := idxfile.NewDecoder(bytes.NewReader(idx))
	var mem idxfile.MemoryIndex
	if err := dec.Decode(&mem); err != nil {
		t.Fatalf("decode idx: %v", err)
	}
	off, err := mem.FindOffset(blob.Hash)
	if err != nil || off != int64(ip.Entries[0].Offset) {
		t.Fatalf("offset: %v %d want %d", err, off, ip.Entries[0].Offset)
	}
	crc, err := mem.FindCRC32(blob.Hash)
	if err != nil || crc != ip.Entries[0].CRC32 {
		t.Fatalf("crc: %v %d want %d", err, crc, ip.Entries[0].CRC32)
	}
}

func TestBuildIndexV2RejectsObjectMismatch(t *testing.T) {
	blob := pack.Object{Type: "blob", Data: []byte("a")}
	blob.Hash = pack.HashObject("blob", blob.Data)
	packBytes, err := pack.Encode([]pack.Object{blob})
	if err != nil {
		t.Fatal(err)
	}
	other := pack.Object{Type: "blob", Data: []byte("b")}
	other.Hash = pack.HashObject("blob", other.Data)
	if _, err := pack.BuildIndexV2FromPack(context.Background(), packBytes, map[plumbing.Hash]pack.Object{
		other.Hash: other,
	}); err == nil {
		t.Fatal("mismatch accepted")
	}
}
