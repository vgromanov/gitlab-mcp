package testutil

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// StreamableMCPConnect serves srv over SDK streamable HTTP on an httptest
// loopback and returns a connected client session. Does not import mcpsrv/tools.
// Existing in-memory MCPConnect is preserved separately.
func StreamableMCPConnect(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	cli := mcp.NewClient(&mcp.Implementation{Name: "testutil-streamable", Version: "v0"}, nil)
	tr := &mcp.StreamableClientTransport{
		Endpoint:             ts.URL,
		DisableStandaloneSSE: true,
	}
	cs, err := cli.Connect(context.Background(), tr, nil)
	if err != nil {
		t.Fatalf("streamable connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// NewSyntheticMCPServer returns a minimal MCP server with one echo tool for
// streamable seam proofs.
func NewSyntheticMCPServer() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "testutil-synthetic", Version: "v0"}, nil)
	type in struct {
		Message string `json:"message"`
	}
	type out struct {
		Echo string `json:"echo"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "echo",
		Description: "echo a message",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args in) (*mcp.CallToolResult, out, error) {
		return nil, out{Echo: args.Message}, nil
	})
	return srv
}
