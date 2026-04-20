// Package version exposes build metadata for the MCP server and CLI.
package version

// Name is the MCP implementation / binary name.
const Name = "gitlab-mcp"

// Version is injected at link time via GoReleaser ldflags.
var Version = "0.1.0"
