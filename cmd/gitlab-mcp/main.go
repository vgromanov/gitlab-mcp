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
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache"
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
	// Validate before token/client/server/listener so unknown profiles fail closed
	// without network use (even when a fake token is present).
	if err := cfg.Validate(); err != nil {
		slog.Error("config", "err", err)
		return 1
	}
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
	guarded, err := glclient.NewGuardedClient(cfg)
	if err != nil {
		slog.Error("gitlab guarded client", "err", err)
		return 1
	}

	var cacheSvc *gitcache.Service
	if cfg.GitCacheEnabled {
		cacheSvc, err = gitcache.OpenService(gitcache.ServiceConfig{
			Enabled:             true,
			Token:               cfg.Token,
			Root:                cfg.GitCacheRoot,
			QuotaBytes:          cfg.GitCacheQuotaBytes,
			CAPath:              cfg.GitCacheCACertPath,
			Insecure:            cfg.GitCacheInsecure,
			AllowedInsecureHost: cfg.GitCacheAllowedInsecureHost,
		})
		if err != nil {
			slog.Error("git cache", "err", err)
			return 1
		}
		defer func() {
			_ = cacheSvc.Close(context.Background())
		}()
	}

	opts := []mcpsrv.ServerOption{mcpsrv.WithGuardedClient(guarded)}
	if cacheSvc != nil {
		opts = append(opts, mcpsrv.WithGitCache(cacheSvc))
	}
	srv := mcpsrv.NewServer(cfg, client, log, opts...)
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
