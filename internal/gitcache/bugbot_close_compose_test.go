package gitcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/pack"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/redact"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshtrust"
)

// closeComposeSess exercises the production-used finishSSHFetch boundary.
type closeComposeSess struct {
	advErr    error
	upErr     error
	closeErr  error
	pack      []byte
	head      plumbing.Hash
	noShallow bool
	closes    atomic.Int32
}

func (s *closeComposeSess) AdvertisedReferences() (*packp.AdvRefs, error) {
	return s.AdvertisedReferencesContext(context.Background())
}
func (s *closeComposeSess) AdvertisedReferencesContext(ctx context.Context) (*packp.AdvRefs, error) {
	if s.advErr != nil {
		return nil, s.advErr
	}
	adv := packp.NewAdvRefs()
	h := s.head
	if h.IsZero() {
		h = plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	}
	adv.Head = &h
	if !s.noShallow {
		_ = adv.Capabilities.Set("shallow")
	}
	_ = adv.Capabilities.Add("allow-reachable-sha1-in-want")
	return adv, nil
}
func (s *closeComposeSess) UploadPack(ctx context.Context, req *packp.UploadPackRequest) (*packp.UploadPackResponse, error) {
	if s.upErr != nil {
		return nil, s.upErr
	}
	if len(s.pack) == 0 {
		return nil, errors.New("no pack")
	}
	return packp.NewUploadPackResponseWithPackfile(req, io.NopCloser(bytes.NewReader(s.pack))), nil
}
func (s *closeComposeSess) Close() error {
	s.closes.Add(1)
	return s.closeErr
}

var _ transport.UploadPackSession = (*closeComposeSess)(nil)

func publicFetchBoundary(result FetchResult, err error, token string) (FetchResult, error) {
	if e := safeContextError(err); e != nil {
		return FetchResult{}, e
	}
	return result, redact.Error(err, token)
}

func TestBugbotClose_ConcurrentCapabilityAndTrustCategories(t *testing.T) {
	sess := &closeComposeSess{noShallow: true, closeErr: sshtrust.ErrUpdate}
	result, err := finishSSHFetch(context.Background(), sess, FetchOptions{Depth: 1, Wants: []plumbing.Hash{plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}}, 1<<20)
	if sess.closes.Load() != 1 {
		t.Fatalf("Close count=%d want 1", sess.closes.Load())
	}
	if result.Indexed.PackByteCount != 0 || len(result.Indexed.Objects) != 0 {
		t.Fatalf("non-empty result on failure: %#v", result)
	}
	if !errors.Is(err, ErrCapability) {
		t.Fatalf("capability category lost: %v", err)
	}
	// Concurrent trust/finalization must remain observable (fails on lossy compose).
	if !errors.Is(err, sshtrust.ErrUpdate) {
		t.Fatalf("trust/finalization category lost on concurrent capability failure: %v", err)
	}
	_, pub := publicFetchBoundary(result, err, "tok")
	if !errors.Is(pub, ErrCapability) || !errors.Is(pub, sshtrust.ErrUpdate) {
		t.Fatalf("public sanitization collapsed typed categories: %v", pub)
	}
}

func TestBugbotClose_ConcurrentAdvertisementAndTrustCategories(t *testing.T) {
	sess := &closeComposeSess{
		advErr:   errors.New("connection reset by peer"),
		closeErr: sshtrust.ErrTrustFiles,
	}
	result, err := finishSSHFetch(context.Background(), sess, FetchOptions{Depth: 1, Wants: []plumbing.Hash{plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}}, 1<<20)
	if sess.closes.Load() != 1 {
		t.Fatalf("Close count=%d want 1", sess.closes.Load())
	}
	if result.Indexed.PackByteCount != 0 || len(result.Indexed.Objects) != 0 {
		t.Fatalf("non-empty result: %#v", result)
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("advertisement category: %v", err)
	}
	if !errors.Is(err, sshtrust.ErrTrustFiles) {
		t.Fatalf("trust/finalization category lost on concurrent advertisement failure: %v", err)
	}
}

func TestBugbotClose_ConcurrentDecodeAndTrustCategories(t *testing.T) {
	want := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	sess := &closeComposeSess{
		pack:     []byte("PACK\x00\x00\x00\x02not-a-real-pack"),
		closeErr: sshtrust.ErrTrustFiles,
	}
	result, err := finishSSHFetch(context.Background(), sess, FetchOptions{Depth: 1, Wants: []plumbing.Hash{want}}, 1<<20)
	if sess.closes.Load() != 1 {
		t.Fatalf("Close count=%d want 1", sess.closes.Load())
	}
	if result.Indexed.PackByteCount != 0 || len(result.Indexed.Objects) != 0 {
		t.Fatalf("non-empty result: %#v", result)
	}
	if err == nil {
		t.Fatal("expected decode failure")
	}
	if !errors.Is(err, pack.ErrMalformed) && !errors.Is(err, pack.ErrChecksum) && !errors.Is(err, pack.ErrTooManyObjects) && !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unexpected decode category: %v", err)
	}
	if !errors.Is(err, sshtrust.ErrTrustFiles) {
		t.Fatalf("trust/finalization category lost on concurrent decode failure: %v", err)
	}
}

func TestBugbotClose_UploadOnlyFailure(t *testing.T) {
	want := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	sess := &closeComposeSess{noShallow: true, closeErr: nil}
	result, err := finishSSHFetch(context.Background(), sess, FetchOptions{Depth: 1, Wants: []plumbing.Hash{want}}, 1<<20)
	if sess.closes.Load() != 1 {
		t.Fatalf("Close count=%d want 1", sess.closes.Load())
	}
	if result.Indexed.PackByteCount != 0 || !errors.Is(err, ErrCapability) {
		t.Fatalf("upload-only: %#v err=%v", result, err)
	}
	if errors.Is(err, sshtrust.ErrUpdate) {
		t.Fatal("invented trust failure on successful close")
	}
}

func TestBugbotClose_ContextPrecedenceWithCloseFailure(t *testing.T) {
	want := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	token := "synthetic-token"
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			wrapped := fmt.Errorf("SYNTHETIC_SECRET https://synthetic-user:synthetic-password@example.test/private: %w", sentinel)
			sess := &closeComposeSess{advErr: wrapped, closeErr: sshtrust.ErrUpdate}
			result, err := finishSSHFetch(context.Background(), sess, FetchOptions{Depth: 1, Wants: []plumbing.Hash{want}}, 1<<20)
			if sess.closes.Load() != 1 {
				t.Fatalf("Close count=%d want 1", sess.closes.Load())
			}
			pubResult, pub := publicFetchBoundary(result, err, token)
			if pubResult.Indexed.PackByteCount != 0 {
				t.Fatalf("non-empty public result: %#v", pubResult)
			}
			if pub != sentinel || pub.Error() != sentinel.Error() || errors.Unwrap(pub) != nil {
				t.Fatalf("public context not bare: matches=%v text=%q unwrap=%v", errors.Is(pub, sentinel), func() string {
					if pub == nil {
						return ""
					}
					return pub.Error()
				}(), errors.Unwrap(pub))
			}
			if bytes.Contains([]byte(pub.Error()), []byte("SYNTHETIC_SECRET")) || bytes.Contains([]byte(pub.Error()), []byte("synthetic-user")) {
				t.Fatalf("unsafe marker present: %v", pub)
			}
			if errors.Is(pub, sshtrust.ErrUpdate) {
				t.Fatalf("joined context wrapper exposed trust category at public boundary: %v", pub)
			}
		})
	}
}

func TestBugbotClose_OrdinaryCombinedPreservesTypedCategories(t *testing.T) {
	op := ErrCapability
	cl := sshtrust.ErrUpdate
	result, err := composeSSHFetchOutcome(FetchResult{Caps: FetchCaps{Shallow: true}}, op, cl)
	if result.Indexed.PackByteCount != 0 || len(result.Indexed.Objects) != 0 {
		t.Fatalf("non-empty: %#v", result)
	}
	if !errors.Is(err, op) || !errors.Is(err, cl) {
		t.Fatalf("ordinary combined lost categories: %v", err)
	}
	_, pub := publicFetchBoundary(result, err, "tok")
	if !errors.Is(pub, op) || !errors.Is(pub, cl) {
		t.Fatalf("public ordinary combined: %v", pub)
	}
}
