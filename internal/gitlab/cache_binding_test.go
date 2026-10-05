package gitlab

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"
)

func TestCacheBindingRejectsWrongCredentialAndSudo(t *testing.T) {
	base, err := url.Parse("https://gitlab.example/api/v4")
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithCacheBinding(context.Background(), base, "tok")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://gitlab.example/api/v4/user", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Private-Token", "wrong")
	if err := checkCacheBinding(req); !errors.Is(err, ErrCacheBinding) {
		t.Fatalf("wrong token: %v", err)
	}
	if CacheBindingObserved(ctx) {
		t.Fatal("observed after failure")
	}

	ctx = WithCacheBinding(context.Background(), base, "tok")
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, "https://gitlab.example/api/v4/user", nil)
	req.Header.Set("Private-Token", "tok")
	req.Header.Set("Sudo", "1")
	if err := checkCacheBinding(req); !errors.Is(err, ErrCacheBinding) {
		t.Fatalf("sudo: %v", err)
	}
}

func TestCacheBindingAcceptsMatchingOriginAndObserves(t *testing.T) {
	base, err := url.Parse("https://gitlab.example/api/v4")
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithCacheBinding(context.Background(), base, "tok")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://gitlab.example/api/v4/projects/1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Private-Token", "tok")
	if err := checkCacheBinding(req); err != nil {
		t.Fatal(err)
	}
	if !CacheBindingObserved(ctx) {
		t.Fatal("expected observation")
	}
}

func TestCacheBindingRejectsWrongHostAndAbsentWithoutContext(t *testing.T) {
	base, _ := url.Parse("https://gitlab.example/api/v4")
	ctx := WithCacheBinding(context.Background(), base, "tok")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://evil.example/api/v4/user", nil)
	req.Header.Set("Private-Token", "tok")
	if err := checkCacheBinding(req); !errors.Is(err, ErrCacheBinding) {
		t.Fatalf("wrong host: %v", err)
	}
	req2, _ := http.NewRequest(http.MethodGet, "https://gitlab.example/api/v4/user", nil)
	if err := checkCacheBinding(req2); err != nil {
		t.Fatalf("no binding context must pass through: %v", err)
	}
	if CacheBindingObserved(context.Background()) {
		t.Fatal("observed without binding")
	}
}
