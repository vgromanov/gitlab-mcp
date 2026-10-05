package pack_test

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/pack"
)

func TestRetainedOutputAndBuildIndexEdges(t *testing.T) {
	blob := pack.Object{Type: "blob", Data: []byte("idx-retain")}
	blob.Hash = pack.HashObject("blob", blob.Data)
	raw, err := pack.Encode([]pack.Object{blob})
	if err != nil {
		t.Fatal(err)
	}
	ip, err := pack.DecodeIndexed(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if ip.RetainedOutputBytes() != int64(len(blob.Data)) {
		t.Fatalf("retained %d", ip.RetainedOutputBytes())
	}
	if ip.PackByteCount != len(raw) || len(ip.PackBytes) != len(raw) {
		t.Fatal("pack alias")
	}
	idx, err := pack.BuildIndexV2(context.Background(), ip)
	if err != nil || len(idx) == 0 {
		t.Fatal(err)
	}
	want, err := bounds.IndexV2SHA1Bytes(len(ip.Entries))
	if err != nil || int64(len(idx)) != want {
		t.Fatalf("size %d want %d", len(idx), want)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pack.BuildIndexV2(ctx, ip); err == nil {
		t.Fatal("canceled build")
	}
	empty := pack.IndexedPack{Objects: map[plumbing.Hash]pack.Object{}, PackByteCount: 12}
	if _, err := pack.BuildIndexV2(context.Background(), empty); err == nil {
		t.Fatal("empty entries")
	}
	// mismatch objs vs pack
	other := pack.Object{Type: "blob", Data: []byte("nope")}
	other.Hash = pack.HashObject("blob", other.Data)
	if _, err := pack.BuildIndexV2FromPack(context.Background(), raw, map[plumbing.Hash]pack.Object{other.Hash: other}); err == nil {
		t.Fatal("mismatch")
	}
}
