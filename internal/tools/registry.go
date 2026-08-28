package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// AddTool registers a tool if read-only and selection gates pass.
// feature is a family id ("" / "core", or issues|work_items|labels|drafts|
// webhooks|timeline|pipeline|milestone|wiki).
func AddTool[In, Out any](s *mcp.Server, d Deps, mutating bool, feature string, tool *mcp.Tool, h func(context.Context, *mcp.CallToolRequest, In, Deps) (*mcp.CallToolResult, Out, error)) {
	if tool != nil {
		noteToolName(tool.Name)
	}
	if mutating && d.Config != nil && d.Config.ReadOnly {
		return
	}
	if d.Config == nil || !ShouldRegister(d.Config, tool.Name, feature) {
		return
	}
	mcp.AddTool(s, tool, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		return h(ctx, req, in, d)
	})
}
