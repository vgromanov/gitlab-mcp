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

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
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

// eofLater returns payload without EOF, then EOF on the next read.
type eofLater struct {
	b []byte
	i int
}

func (r *eofLater) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

func TestLimitedBodyAcceptsExactMax(t *testing.T) {
	const max = 8
	exact := &limitedBody{ReadCloser: io.NopCloser(&eofLater{b: bytes.Repeat([]byte("a"), max)}), max: max}
	got, err := io.ReadAll(exact)
	if err != nil || len(got) != max || string(got) != strings.Repeat("a", max) {
		t.Fatalf("exact max: %q %v", got, err)
	}
	over := &limitedBody{ReadCloser: io.NopCloser(&eofLater{b: bytes.Repeat([]byte("a"), max+1)}), max: max}
	got, err = io.ReadAll(over)
	if !errors.Is(err, ErrLimit) || len(got) != max {
		t.Fatalf("overflow: n=%d err=%v", len(got), err)
	}
	got, err = readLimited(context.Background(), &eofLater{b: bytes.Repeat([]byte("b"), 32)}, 32)
	if err != nil || string(got) != strings.Repeat("b", 32) {
		t.Fatalf("readLimited exact: %q %v", got, err)
	}
}

func TestValidateCloneURLAcceptsGitLabSCP(t *testing.T) {
	if err := ValidateCloneURL("git@gitlab.example:group/proj.git", "https://gitlab.example/api/v4", "group/proj", false); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"git@gitlab.example:group/../proj.git",
		"git@evil.example:group/proj.git",
		"git@gitlab.example:group/proj.git?x=1",
	} {
		if err := ValidateCloneURL(raw, "https://gitlab.example/api/v4", "group/proj", false); !errors.Is(err, ErrAuthz) {
			t.Fatalf("%q: %v", raw, err)
		}
	}
}

func TestUploadPackPostLeavesPreludeHeadroom(t *testing.T) {
	const max = int64(32)
	nak := []byte("0008NAK\n")
	pack := bytes.Repeat([]byte("P"), int(max))
	okBody := append(append([]byte{}, nak...), pack...)
	if int64(len(okBody)) > max+bounds.MaxPackPrelude {
		t.Fatal("fixture exceeds the upload-pack response budget")
	}
	read := func(method string, body []byte) ([]byte, error) {
		t.Helper()
		rt := &limitedTransport{
			base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: 200,
					Body:       io.NopCloser(bytes.NewReader(body)),
					Request:    req,
				}, nil
			}),
			max:     max,
			prelude: bounds.MaxPackPrelude,
		}
		resp, err := rt.RoundTrip(&http.Request{Method: method})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return io.ReadAll(resp.Body)
	}
	got, err := read(http.MethodPost, okBody)
	if err != nil || !bytes.Equal(got, okBody) {
		t.Fatalf("NAK plus pack at cap: n=%d err=%v", len(got), err)
	}
	over := bytes.Repeat([]byte("Q"), int(max+bounds.MaxPackPrelude)+1)
	if _, err := read(http.MethodPost, over); !errors.Is(err, ErrLimit) {
		t.Fatalf("POST over prelude headroom: %v", err)
	}
	if _, err := read(http.MethodGet, bytes.Repeat([]byte("a"), int(max)+1)); !errors.Is(err, ErrLimit) {
		t.Fatalf("GET advertisement used pack headroom: %v", err)
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
