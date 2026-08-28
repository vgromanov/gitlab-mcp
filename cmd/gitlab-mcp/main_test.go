package main

import (
	"context"
	"net"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/version"
)

func TestRun_versionFlags(t *testing.T) {
	if code := run([]string{"-version"}); code != 0 {
		t.Fatalf("code %d", code)
	}
	if code := run([]string{"--version"}); code != 0 {
		t.Fatalf("code %d", code)
	}
	if version.Name == "" || version.Version == "" {
		t.Fatal("empty version metadata")
	}
}

func TestRunWithConfig_missingToken(t *testing.T) {
	if code := runWithConfig(context.Background(), &config.Config{}); code != 1 {
		t.Fatalf("code %d", code)
	}
}

func TestRunWithConfig_badClient(t *testing.T) {
	code := runWithConfig(context.Background(), &config.Config{
		Token:  "tok",
		APIURL: "://bad",
	})
	if code != 1 {
		t.Fatalf("code %d", code)
	}
}

func TestRunWithConfig_streamableHTTPBindError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	code := runWithConfig(context.Background(), &config.Config{
		Token:          "tok",
		APIURL:         "https://gitlab.example.invalid/api/v4",
		StreamableHTTP: true,
		Host:           "127.0.0.1",
		Port:           port,
	})
	if code != 1 {
		t.Fatalf("expected bind failure exit 1, got %d", code)
	}
}

func TestRunWithConfig_stdioCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code := runWithConfig(ctx, &config.Config{
		Token:  "tok",
		APIURL: "https://gitlab.example.invalid/api/v4",
	})
	// Cancelled parent should make stdio return an error (exit 1) or succeed quickly (0).
	if code != 0 && code != 1 {
		t.Fatalf("unexpected code %d", code)
	}
}
