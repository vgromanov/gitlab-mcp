package redact

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestCoverage_RedactErrorAndPublicHTTPSContracts(t *testing.T) {
	if Error(nil, "secret") != nil {
		t.Fatal("nil error")
	}
	secret := "super-secret-token"
	err := errors.New("failed with " + secret + " and " + url.QueryEscape(secret))
	out := Error(err, "", secret)
	if out == nil || strings.Contains(out.Error(), secret) || strings.Contains(out.Error(), url.QueryEscape(secret)) {
		t.Fatalf("secret leaked: %v", out)
	}
	clean := errors.New("no secrets here")
	if got := Error(clean, "zzz"); got != clean {
		t.Fatalf("unchanged should return original: %v", got)
	}
	if PublicHTTPS(nil) != nil {
		t.Fatal("nil public")
	}
	if !errors.Is(PublicHTTPS(context.Canceled), context.Canceled) {
		t.Fatal("cancel")
	}
	if !errors.Is(PublicHTTPS(context.DeadlineExceeded), context.DeadlineExceeded) {
		t.Fatal("deadline")
	}
	keep := errors.New("local-sentinel")
	if !errors.Is(PublicHTTPS(keep, keep), keep) {
		t.Fatal("keep sentinel")
	}
	if !errors.Is(PublicHTTPS(errors.New("remote boom"), keep), ErrHTTPS) {
		t.Fatal("https category")
	}
	if HostFrame("missing").Text == "" || HostFrame("mismatch").Text == "" || HostFrame("revoked").Text == "" || HostFrame("other").Text == "" {
		t.Fatal("host frames")
	}
}
