package gitcache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
)

func TestAdvertisedHash(t *testing.T) {
	want := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if advertisedHash(nil, want) {
		t.Fatal("nil adv")
	}
	adv := packp.NewAdvRefs()
	h := want
	adv.Head = &h
	if !advertisedHash(adv, want) {
		t.Fatal("head")
	}
	other := plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	adv.Head = nil
	adv.References = map[string]plumbing.Hash{"refs/heads/main": other}
	if advertisedHash(adv, want) {
		t.Fatal("missing ref")
	}
	adv.References["refs/heads/f"] = want
	if !advertisedHash(adv, want) {
		t.Fatal("ref")
	}
}

func TestReadLimited(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readLimited(ctx, strings.NewReader("x"), 100); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	got, err := readLimited(context.Background(), strings.NewReader("hello"), 100)
	if err != nil || string(got) != "hello" {
		t.Fatalf("read: %q %v", got, err)
	}
	if _, err := readLimited(context.Background(), strings.NewReader(strings.Repeat("a", 50)), 10); !errors.Is(err, ErrLimit) {
		t.Fatalf("limit: %v", err)
	}
	if _, err := readLimited(context.Background(), errReader{err: io.ErrUnexpectedEOF}, 100); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unavailable: %v", err)
	}
}

func TestLimitedBodyAndTransport(t *testing.T) {
	body := &limitedBody{ReadCloser: io.NopCloser(strings.NewReader(strings.Repeat("z", 20))), max: 5}
	buf := make([]byte, 16)
	n, err := body.Read(buf)
	if n != 5 || err != nil {
		t.Fatalf("body first: n=%d err=%v", n, err)
	}
	n, err = body.Read(buf)
	if n != 0 || !errors.Is(err, ErrLimit) {
		t.Fatalf("body at cap: n=%d err=%v", n, err)
	}

	rt := &limitedTransport{
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(bytes.NewReader([]byte("abcdef"))),
				Request:    req,
			}, nil
		}),
		max: 3,
	}
	resp, err := rt.RoundTrip(&http.Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	all, err := io.ReadAll(resp.Body)
	if !errors.Is(err, ErrLimit) && len(all) > 3 {
		t.Fatalf("transport: %q %v", all, err)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
