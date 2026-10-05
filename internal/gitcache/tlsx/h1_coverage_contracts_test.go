package tlsx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/origin"
)

func TestCoverage_PrepareRejectsCanceledInsecureAndMalformedCA(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := Prepare(canceled, Input{ServerName: "example.com"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled prepare: %v", err)
	}
	if _, _, err := Prepare(context.Background(), Input{ServerName: "evil.example", Insecure: true}); !errors.Is(err, ErrInsecure) {
		t.Fatalf("insecure host: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, []byte("-----BEGIN CERTIFICATE-----\nZZZZ\n-----END CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Prepare(context.Background(), Input{ServerName: "example.com", CAPath: path}); !errors.Is(err, ErrMalformedCA) {
		t.Fatalf("malformed ca: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Prepare(context.Background(), Input{ServerName: origin.DefaultInsecureHost, CAPath: path}); !errors.Is(err, ErrMalformedCA) {
		t.Fatalf("empty ca: %v", err)
	}
}
