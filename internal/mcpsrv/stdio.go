package mcpsrv

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RunStdio runs the MCP server on stdio (JSON-RPC over stdin/stdout).
func RunStdio(ctx context.Context, srv *mcp.Server) error {
	return srv.Run(ctx, &mcp.StdioTransport{})
}
