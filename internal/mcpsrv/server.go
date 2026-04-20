// Package mcpsrv hosts the MCP server over stdio and streamable HTTP.
package mcpsrv

import (
	"log/slog"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/vgromanov/gitlab-mcp/internal/config"
	"github.com/vgromanov/gitlab-mcp/internal/tools"
	"github.com/vgromanov/gitlab-mcp/internal/version"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// NewServer builds the MCP server with all GitLab tools registered.
func NewServer(cfg *config.Config, client *gitlab.Client, logger *slog.Logger) *mcp.Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	s := mcp.NewServer(&mcp.Implementation{Name: version.Name, Version: version.Version}, &mcp.ServerOptions{
		Logger:       logger,
		Instructions: "GitLab MCP: PAT-authenticated tools for projects, MRs, issues, CI, wiki, releases, and GraphQL.",
	})
	tools.RegisterAll(s, tools.Deps{Config: cfg, Client: client})
	return s
}
