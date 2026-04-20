// Command gitlab-mcp runs the GitLab Model Context Protocol server.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/vgromanov/gitlab-mcp/internal/config"
	glclient "github.com/vgromanov/gitlab-mcp/internal/gitlab"
	"github.com/vgromanov/gitlab-mcp/internal/mcpsrv"
	"github.com/vgromanov/gitlab-mcp/internal/version"
)

func main() {
	for _, a := range os.Args[1:] {
		if a == "-version" || a == "--version" {
			fmt.Printf("%s %s\n", version.Name, version.Version)
			return
		}
	}

	cfg := config.Load()
	if cfg.Token == "" {
		slog.Error("GITLAB_PERSONAL_ACCESS_TOKEN or --token is required")
		os.Exit(1)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	client, err := glclient.NewClient(cfg)
	if err != nil {
		slog.Error("gitlab client", "err", err)
		os.Exit(1)
	}

	srv := mcpsrv.NewServer(cfg, client, log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.StreamableHTTP {
		log.Info("streamable HTTP", "addr", net.JoinHostPort(cfg.Host, cfg.Port), "path", "/mcp")
		if err := mcpsrv.RunStreamableHTTP(ctx, srv, cfg.Host, cfg.Port); err != nil {
			slog.Error("http server", "err", err)
			os.Exit(1)
		}
		return
	}
	if err := mcpsrv.RunStdio(ctx, srv); err != nil {
		slog.Error("stdio server", "err", err)
		os.Exit(1)
	}
}
