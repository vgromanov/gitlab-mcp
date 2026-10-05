// Package mcpsrv hosts the MCP server over stdio and streamable HTTP.
package mcpsrv

import (
	"log/slog"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/version"
)

// ServerOption configures optional NewServer behavior without breaking the
// existing (cfg, client, logger) call signature.
type ServerOption func(*serverOptions)

type serverOptions struct {
	guarded  *gitlab.Client
	gitCache *gitcache.Service
}

// WithGuardedClient supplies the WithoutRetries publication/mutation/GraphQL client.
func WithGuardedClient(c *gitlab.Client) ServerOption {
	return func(o *serverOptions) {
		o.guarded = c
	}
}

// WithGitCache wires the optional native object cache service into tool Deps.
// No public cache tool is registered. When the service is enabled,
// get_merge_request acquires that merge request's authorized objects.
func WithGitCache(s *gitcache.Service) ServerOption {
	return func(o *serverOptions) {
		o.gitCache = s
	}
}

// NewServer builds the MCP server with all GitLab tools registered.
// Optional WithGuardedClient wires Deps.Guarded for mutating and GraphQL tools.
func NewServer(cfg *config.Config, client *gitlab.Client, logger *slog.Logger, opts ...ServerOption) *mcp.Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	var so serverOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&so)
		}
	}
	s := mcp.NewServer(&mcp.Implementation{Name: version.Name, Version: version.Version}, &mcp.ServerOptions{
		Logger:       logger,
		Instructions: "GitLab MCP: PAT-authenticated tools for projects, MRs, issues, CI, wiki, releases, and GraphQL.",
	})
	tools.RegisterAll(s, tools.Deps{Config: cfg, Client: client, Guarded: so.guarded, GitCache: so.gitCache})
	tools.WarnUnknownSelectionTools(cfg, logger)
	return s
}
