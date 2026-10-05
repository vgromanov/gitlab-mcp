package listx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
)

type nopWriteCloser struct{ bytes.Buffer }

func (nopWriteCloser) Close() error { return nil }

func TestSSHPackStreamIgnoresAdvertisement(t *testing.T) {
	const maxBytes = int64(4096)
	hash := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	adv := packp.NewAdvRefs()
	if err := adv.Capabilities.Set(capability.Shallow); err != nil {
		t.Fatal(err)
	}
	if err := adv.AddReference(plumbing.NewHashReference(plumbing.ReferenceName("refs/heads/main"), hash)); err != nil {
		t.Fatal(err)
	}
	var advBuf bytes.Buffer
	if err := adv.Encode(&advBuf); err != nil {
		t.Fatal(err)
	}
	if int64(advBuf.Len()) >= maxBytes {
		t.Fatalf("advertisement %d is not inside the cap", advBuf.Len())
	}
	nak := []byte("0008NAK\n")
	pack := bytes.Repeat([]byte{0xab}, int(maxBytes))
	if int64(advBuf.Len()+len(nak)+len(pack)) <= maxBytes {
		t.Fatal("fixture does not exceed a shared advertisement and pack limit")
	}

	readPack := func(extra int) ([]byte, error) {
		t.Helper()
		body := append(append(append([]byte{}, advBuf.Bytes()...), nak...), pack...)
		body = append(body, bytes.Repeat([]byte{0xcd}, extra)...)
		s := &sshUploadSession{
			stdout:   &limitedReader{r: bytes.NewReader(body), max: maxBytes},
			stdin:    &nopWriteCloser{},
			maxBytes: maxBytes,
		}
		if _, err := s.AdvertisedReferencesContext(context.Background()); err != nil {
			return nil, err
		}
		req := packp.NewUploadPackRequest()
		req.Wants = []plumbing.Hash{hash}
		resp, err := s.UploadPack(context.Background(), req)
		if err != nil {
			return nil, err
		}
		defer resp.Close()
		return io.ReadAll(resp)
	}

	got, err := readPack(0)
	if err != nil || !bytes.Equal(got, pack) {
		t.Fatalf("pack at cap after advertisement: n=%d err=%v", len(got), err)
	}
	// One byte past the pack cap plus the whole prelude must still fail.
	over := int(bounds.MaxPackPrelude) + 1
	if _, err := readPack(over); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("pack stream over the separate budget: %v", err)
	}
}
