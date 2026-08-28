package mcpsrv

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
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
