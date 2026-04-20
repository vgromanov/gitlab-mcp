package testutil

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPConnect connects an in-memory MCP client to the given server.
func MCPConnect(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cli := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	cs, err := cli.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// ToolNames returns tool names from a list_tools call.
func ToolNames(t *testing.T, cs *mcp.ClientSession) map[string]bool {
	t.Helper()
	ctx := context.Background()
	res, err := cs.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	out := make(map[string]bool)
	for _, x := range res.Tools {
		if x != nil {
			out[x.Name] = true
		}
	}
	return out
}
