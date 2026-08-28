package tools

import (
	"io"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
)

func stubGitLabAPI() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	})
}

func registerNames(t *testing.T, cfg *config.Config) map[string]bool {
	t.Helper()
	cli, _ := testutil.NewGitLabClient(t, stubGitLabAPI())
	srv := mcp.NewServer(&mcp.Implementation{Name: "gitlab-mcp-test", Version: "test"}, nil)
	RegisterAll(srv, Deps{Config: cfg, Client: cli})
	cs := testutil.MCPConnect(t, srv)
	return testutil.ToolNames(t, cs)
}

func TestRegisterAll_coreTools(t *testing.T) {
	cfg := &config.Config{
		Token:     "x",
		Wiki:      true,
		Milestone: true,
		Pipeline:  true,
	}
	names := registerNames(t, cfg)
	if !names["list_projects"] || !names["get_merge_request"] {
		t.Fatalf("missing core tools, have list_projects=%v", names["list_projects"])
	}
}

func TestRegisterAll_readOnlyHidesMutations(t *testing.T) {
	cfg := &config.Config{Token: "x", ReadOnly: true, Wiki: true, Milestone: true, Pipeline: true}
	names := registerNames(t, cfg)
	if names["create_repository"] {
		t.Fatal("create_repository should be hidden in read-only mode")
	}
	if !names["list_projects"] {
		t.Fatal("list_projects should remain")
	}
}

func TestRegisterAll_pipelineGate(t *testing.T) {
	cfg := &config.Config{Token: "x", Pipeline: false, Wiki: true, Milestone: true}
	names := registerNames(t, cfg)
	if names["list_pipelines"] {
		t.Fatal("pipeline tools should be gated off")
	}
}

func TestRegisterAll_wikiGate(t *testing.T) {
	cfg := &config.Config{Token: "x", Wiki: false, Milestone: true, Pipeline: true}
	names := registerNames(t, cfg)
	if names["list_wiki_pages"] {
		t.Fatal("wiki tools should be gated off")
	}
}

func TestRegisterAll_unrestrictedDefaultCount(t *testing.T) {
	// Default catalog: all ungated tools; pipeline/milestone/wiki off.
	names := registerNames(t, &config.Config{Token: "x"})
	if names["list_pipelines"] || names["list_wiki_pages"] || names["list_milestones"] {
		t.Fatal("legacy gated families must stay off by default")
	}
	if !names["list_issues"] || !names["list_projects"] || !names["search_code"] {
		t.Fatal("ungated tools must register in unrestricted mode")
	}
	if n := len(names); n < 80 {
		t.Fatalf("unrestricted default tool count = %d, expected a large catalog", n)
	}
}

func TestRegisterAll_useDailyToolsAlone(t *testing.T) {
	names := registerNames(t, &config.Config{Token: "x", UseDailyTools: true})
	if len(names) != 41 {
		t.Fatalf("USE_DAILY_TOOLS alone registered %d tools, want 41; got %#v", len(names), keys(names))
	}
	for _, name := range RequiredDailySearchTools {
		if !names[name] {
			t.Fatalf("daily catalog missing required search tool %q", name)
		}
	}
	if names["list_issues"] {
		t.Fatal("issues must not register with USE_DAILY_TOOLS alone")
	}
	if names["list_pipelines"] {
		t.Fatal("pipeline must not register with USE_DAILY_TOOLS alone")
	}
}

func TestRegisterAll_dailyUnionIssues(t *testing.T) {
	names := registerNames(t, &config.Config{Token: "x", UseDailyTools: true, Issues: true})
	want := 41 + len(FamilyTools("issues"))
	if len(names) != want {
		t.Fatalf("daily∪issues registered %d, want %d", len(names), want)
	}
	if !names["list_issues"] || !names["search_code"] {
		t.Fatal("expected both daily search and issues tools")
	}
}

func TestRegisterAll_disableRemovesNamed(t *testing.T) {
	names := registerNames(t, &config.Config{
		Token:         "x",
		UseDailyTools: true,
		DisabledTools: []string{"search_repositories", "execute_graphql"},
	})
	if names["search_repositories"] || names["execute_graphql"] {
		t.Fatal("disabled tools must be removed")
	}
	if len(names) != 39 {
		t.Fatalf("got %d tools after disable, want 39", len(names))
	}
}

func TestRegisterAll_legacyPipelineAlone(t *testing.T) {
	names := registerNames(t, &config.Config{Token: "x", Pipeline: true})
	if !names["list_pipelines"] {
		t.Fatal("USE_PIPELINE alone should register pipeline tools")
	}
	if !names["list_projects"] || !names["list_issues"] {
		t.Fatal("USE_PIPELINE alone must stay legacy (core + issues still on)")
	}
	if names["list_wiki_pages"] {
		t.Fatal("wiki should stay off")
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
