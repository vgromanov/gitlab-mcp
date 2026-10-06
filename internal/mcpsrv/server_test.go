package mcpsrv

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/version"
)

func TestNewServer(t *testing.T) {
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	}))
	cfg := &config.Config{Token: "t", Wiki: true, Milestone: true, Pipeline: true}
	srv := NewServer(cfg, cli, nil)
	if srv == nil {
		t.Fatal("nil")
	}
	srv2 := NewServer(cfg, cli, slog.Default())
	if srv2 == nil {
		t.Fatal("nil2")
	}
}

func TestNewServer_serverInfoCarriesBuildRevision(t *testing.T) {
	// initialize never calls GitLab, so the fixture handler has no body to write.
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv := NewServer(&config.Config{Token: "t"}, cli, slog.Default())
	cs := testutil.MCPConnect(t, srv)
	info := cs.InitializeResult().ServerInfo
	if info == nil {
		t.Fatal("no serverInfo after initialize")
	}
	if info.Name != version.Name {
		t.Fatalf("name = %q", info.Name)
	}
	if info.Version != version.String() {
		t.Fatalf("version = %q, want %q", info.Version, version.String())
	}
	// The bare link-time constant is no longer what a client sees: the build
	// revision (or an explicit "unknown" without VCS info) follows the "+".
	if !strings.HasPrefix(info.Version, version.Version+"+") || strings.HasSuffix(info.Version, "+") {
		t.Fatalf("version %q lacks the build revision", info.Version)
	}
}

func TestRunStreamableHTTP_shutdown(t *testing.T) {
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	}))
	srv := NewServer(&config.Config{Token: "t"}, cli, slog.Default())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- RunStreamableHTTP(ctx, srv, "127.0.0.1", port) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}
}
