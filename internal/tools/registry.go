package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// AddTool registers a tool if read-only and feature gates pass.
func AddTool[In, Out any](s *mcp.Server, d Deps, mutating bool, feature string, tool *mcp.Tool, h func(context.Context, *mcp.CallToolRequest, In, Deps) (*mcp.CallToolResult, Out, error)) {
	if mutating && d.Config.ReadOnly {
		return
	}
	if feature != "" && !d.Config.FeatureEnabled(feature) {
		return
	}
	mcp.AddTool(s, tool, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		return h(ctx, req, in, d)
	})
}
