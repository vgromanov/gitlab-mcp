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

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	glclient "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/mcpsrv"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	for _, a := range args {
		if a == "-version" || a == "--version" {
			fmt.Printf("%s %s\n", version.Name, version.Version)
			return 0
		}
	}

	return runWithConfig(context.Background(), config.Load())
}

func runWithConfig(parent context.Context, cfg *config.Config) int {
	if cfg.Token == "" {
		slog.Error("GITLAB_PERSONAL_ACCESS_TOKEN or --token is required")
		return 1
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	client, err := glclient.NewClient(cfg)
	if err != nil {
		slog.Error("gitlab client", "err", err)
		return 1
	}

	srv := mcpsrv.NewServer(cfg, client, log)
	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.StreamableHTTP {
		log.Info("streamable HTTP", "addr", net.JoinHostPort(cfg.Host, cfg.Port), "path", "/mcp")
		if err := mcpsrv.RunStreamableHTTP(ctx, srv, cfg.Host, cfg.Port); err != nil {
			slog.Error("http server", "err", err)
			return 1
		}
		return 0
	}
	if err := mcpsrv.RunStdio(ctx, srv); err != nil {
		slog.Error("stdio server", "err", err)
		return 1
	}
	return 0
}
