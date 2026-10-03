package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// AddTool registers a tool if read-only and selection gates pass.
// feature is a family id ("" / "core", or issues|work_items|labels|drafts|
// webhooks|timeline|pipeline|milestone|wiki).
//
// When mutating is true and Deps.Guarded is set, the closed-over Deps uses
// Guarded as Client so handlers that call d.Client hit the no-retry path.
//
// Tool descriptors are copied before annotation attach so static callers are
// not mutated across registrations.
func AddTool[In, Out any](s *mcp.Server, d Deps, mutating bool, feature string, tool *mcp.Tool, h func(context.Context, *mcp.CallToolRequest, In, Deps) (*mcp.CallToolResult, Out, error)) {
	if tool != nil {
		noteToolName(tool.Name)
	}
	if mutating && d.Config != nil && d.Config.ReadOnly {
		return
	}
	if d.Config == nil || tool == nil || !ShouldRegister(d.Config, tool.Name, feature) {
		return
	}
	local := d
	if mutating && d.Guarded != nil {
		local.Client = d.Guarded
	}
	reg := applyToolAnnotations(tool, mutating)
	mcp.AddTool(s, reg, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		return h(ctx, req, in, local)
	})
}
