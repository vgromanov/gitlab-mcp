package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestRunWithConfig_unknownToolProfile(t *testing.T) {
	code := runWithConfig(context.Background(), &config.Config{
		Token:       "fake-token-not-live",
		APIURL:      "https://gitlab.example.invalid/api/v4",
		ToolProfile: "nope",
	})
	if code != 1 {
		t.Fatalf("unknown profile must exit 1 before client/listener, got %d", code)
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

func TestRunWithConfig_badIntentDB(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bad.db")
	planted := []byte("NOTE-BODY-secret-do-not-store glpat-PLANTED-TOKEN-VALUE")
	if err := os.WriteFile(path, planted, 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	code := runWithConfig(context.Background(), &config.Config{
		Token:    "fake-token-not-live",
		APIURL:   srv.URL + "/api/v4",
		IntentDB: path,
	})
	if code != 1 {
		t.Fatalf("corrupt intent db must exit 1 before the GitLab client, got %d", code)
	}
	if hits != 0 {
		t.Fatalf("gitlab client was contacted %d times", hits)
	}
	logged := buf.String()
	if strings.Contains(logged, "NOTE-BODY-secret-do-not-store") || strings.Contains(logged, "glpat-PLANTED-TOKEN-VALUE") {
		t.Fatalf("startup log leaked planted secret: %s", logged)
	}
}
