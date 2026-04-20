package tools

import (
	"io"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/vgromanov/gitlab-mcp/internal/config"
	"github.com/vgromanov/gitlab-mcp/internal/testutil"
)

func stubGitLabAPI() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	})
}

func TestRegisterAll_coreTools(t *testing.T) {
	cli, _ := testutil.NewGitLabClient(t, stubGitLabAPI())
	cfg := &config.Config{
		Token:     "x",
		Wiki:      true,
		Milestone: true,
		Pipeline:  true,
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "gitlab-mcp-test", Version: "test"}, nil)
	RegisterAll(srv, Deps{Config: cfg, Client: cli})
	cs := testutil.MCPConnect(t, srv)
	names := testutil.ToolNames(t, cs)
	if !names["list_projects"] || !names["get_merge_request"] {
		t.Fatalf("missing core tools, have list_projects=%v", names["list_projects"])
	}
}

func TestRegisterAll_readOnlyHidesMutations(t *testing.T) {
	cli, _ := testutil.NewGitLabClient(t, stubGitLabAPI())
	cfg := &config.Config{Token: "x", ReadOnly: true, Wiki: true, Milestone: true, Pipeline: true}
	srv := mcp.NewServer(&mcp.Implementation{Name: "gitlab-mcp-test", Version: "test"}, nil)
	RegisterAll(srv, Deps{Config: cfg, Client: cli})
	cs := testutil.MCPConnect(t, srv)
	names := testutil.ToolNames(t, cs)
	if names["create_repository"] {
		t.Fatal("create_repository should be hidden in read-only mode")
	}
	if !names["list_projects"] {
		t.Fatal("list_projects should remain")
	}
}

func TestRegisterAll_pipelineGate(t *testing.T) {
	cli, _ := testutil.NewGitLabClient(t, stubGitLabAPI())
	cfg := &config.Config{Token: "x", Pipeline: false, Wiki: true, Milestone: true}
	srv := mcp.NewServer(&mcp.Implementation{Name: "gitlab-mcp-test", Version: "test"}, nil)
	RegisterAll(srv, Deps{Config: cfg, Client: cli})
	cs := testutil.MCPConnect(t, srv)
	names := testutil.ToolNames(t, cs)
	if names["list_pipelines"] {
		t.Fatal("pipeline tools should be gated off")
	}
}

func TestRegisterAll_wikiGate(t *testing.T) {
	cli, _ := testutil.NewGitLabClient(t, stubGitLabAPI())
	cfg := &config.Config{Token: "x", Wiki: false, Milestone: true, Pipeline: true}
	srv := mcp.NewServer(&mcp.Implementation{Name: "gitlab-mcp-test", Version: "test"}, nil)
	RegisterAll(srv, Deps{Config: cfg, Client: cli})
	cs := testutil.MCPConnect(t, srv)
	names := testutil.ToolNames(t, cs)
	if names["list_wiki_pages"] {
		t.Fatal("wiki tools should be gated off")
	}
}
