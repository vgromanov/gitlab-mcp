package gitcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// F4: transport-returned wrapped cancellation/deadline must survive as typed
// sentinels when the parent context is still live.

type f4Sess struct {
	advErr error
	upErr  error
	reader io.Reader
}

func (s *f4Sess) AdvertisedReferences() (*packp.AdvRefs, error) {
	return s.AdvertisedReferencesContext(context.Background())
}
func (s *f4Sess) AdvertisedReferencesContext(ctx context.Context) (*packp.AdvRefs, error) {
	if s.advErr != nil {
		return nil, s.advErr
	}
	adv := packp.NewAdvRefs()
	h := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	adv.Head = &h
	_ = adv.Capabilities.Set("shallow")
	_ = adv.Capabilities.Add("allow-reachable-sha1-in-want")
	return adv, nil
}
func (s *f4Sess) UploadPack(ctx context.Context, req *packp.UploadPackRequest) (*packp.UploadPackResponse, error) {
	if s.upErr != nil {
		return nil, s.upErr
	}
	if s.reader == nil {
		return nil, errors.New("no reader")
	}
	resp := packp.NewUploadPackResponse(req)
	if err := resp.Decode(io.NopCloser(s.reader)); err != nil {
		return nil, err
	}
	return resp, nil
}
func (s *f4Sess) Close() error { return nil }

var _ transport.UploadPackSession = (*f4Sess)(nil)

func TestF4_AdvertisementWrappedCancelWithLiveParent(t *testing.T) {
	ctx := context.Background() // live parent
	_, err := uploadPack(ctx, &f4Sess{advErr: fmt.Errorf("transport: %w", context.Canceled)}, FetchOptions{Depth: 1, Wants: []plumbing.Hash{plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}}, 1<<20)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("advertisement wrapped cancel lost (F4): %v", err)
	}
}

func TestF4_AdvertisementWrappedDeadlineWithLiveParent(t *testing.T) {
	ctx := context.Background()
	_, err := uploadPack(ctx, &f4Sess{advErr: fmt.Errorf("transport: %w", context.DeadlineExceeded)}, FetchOptions{Depth: 1, Wants: []plumbing.Hash{plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}}, 1<<20)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("advertisement wrapped deadline lost (F4): %v", err)
	}
}

func TestF4_UploadPackWrappedCancelWithLiveParent(t *testing.T) {
	ctx := context.Background()
	want := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	_, err := uploadPack(ctx, &f4Sess{upErr: fmt.Errorf("up: %w", context.Canceled)}, FetchOptions{Depth: 1, Wants: []plumbing.Hash{want}}, 1<<20)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("upload-pack wrapped cancel lost (F4): %v", err)
	}
}

func TestF4_UploadPackWrappedDeadlineWithLiveParent(t *testing.T) {
	ctx := context.Background()
	want := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	_, err := uploadPack(ctx, &f4Sess{upErr: fmt.Errorf("up: %w", context.DeadlineExceeded)}, FetchOptions{Depth: 1, Wants: []plumbing.Hash{want}}, 1<<20)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("upload-pack wrapped deadline lost (F4): %v", err)
	}
}

func TestF4_ReaderWrappedCancelWithLiveParent(t *testing.T) {
	ctx := context.Background()
	r := &f4CancelReader{err: fmt.Errorf("read: %w", context.Canceled)}
	_, err := readLimited(ctx, r, 1<<20)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("reader wrapped cancel lost (F4): %v", err)
	}
}

func TestF4_ReaderWrappedDeadlineWithLiveParent(t *testing.T) {
	ctx := context.Background()
	r := &f4CancelReader{err: fmt.Errorf("read: %w", context.DeadlineExceeded)}
	_, err := readLimited(ctx, r, 1<<20)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reader wrapped deadline lost (F4): %v", err)
	}
}

func TestF4_OrdinaryErrorStillSanitized(t *testing.T) {
	ctx := context.Background()
	_, err := uploadPack(ctx, &f4Sess{advErr: errors.New("connection reset by peer secret=leak")}, FetchOptions{Depth: 1, Wants: []plumbing.Hash{plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}}, 1<<20)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ordinary error not sanitized (F4): %v", err)
	}
	if err != nil && bytes.Contains([]byte(err.Error()), []byte("secret=")) {
		t.Fatalf("payload leaked: %v", err)
	}
}

type f4CancelReader struct{ err error }

func (r *f4CancelReader) Read([]byte) (int, error) { return 0, r.err }
