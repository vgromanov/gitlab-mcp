package testutil

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestStreamableMCPConnect_ListToolsAndCallTool(t *testing.T) {
	srv := NewSyntheticMCPServer()
	cs := StreamableMCPConnect(t, srv)

	names := ToolNames(t, cs)
	if !names["echo"] {
		t.Fatalf("tools=%v missing echo", names)
	}

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "ping"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || res.IsError {
		t.Fatalf("tool result=%+v", res)
	}
	blob := ""
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			blob += tc.Text
		}
	}
	if res.StructuredContent != nil {
		blob += strings.TrimSpace(strings.ReplaceAll(
			strings.ReplaceAll(toStringy(res.StructuredContent), " ", ""),
			"\n", "",
		))
	}
	if !strings.Contains(blob, "ping") && res.StructuredContent == nil {
		t.Fatalf("echo payload missing ping; content=%v structured=%v", res.Content, res.StructuredContent)
	}
	_ = io.Discard
}

func toStringy(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case map[string]any:
		if e, ok := x["echo"].(string); ok {
			return e
		}
	}
	return ""
}

func TestMCPConnect_preserved(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	cs := MCPConnect(t, srv)
	if cs == nil {
		t.Fatal("nil session")
	}
	if ToolNames(t, cs) == nil {
		t.Fatal("nil names")
	}
}
