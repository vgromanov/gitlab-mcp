package testutil

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPConnectAndToolNames(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	cs := MCPConnect(t, srv)
	names := ToolNames(t, cs)
	if names == nil {
		t.Fatal("nil names")
	}
}
