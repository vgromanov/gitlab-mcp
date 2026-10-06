package tools

import (
	"io"
	"net/http"
	"slices"
	"sort"
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

func sortedKeys(m map[string]bool) []string {
	out := keys(m)
	sort.Strings(out)
	return out
}

func TestRegisterAll_reviewProfileExactSet(t *testing.T) {
	want := []string{
		"get_merge_request_discussion", "create_merge_request_discussion_note", "resolve_merge_request_thread",
		"list_pipelines", "get_pipeline", "list_pipeline_jobs", "list_pipeline_trigger_jobs",
		"get_pipeline_job", "get_pipeline_job_output", "get_review_queue", "get_review_snapshot",
		"batch_get_file_contents",
	}
	want = append(want, DailyTools()...)
	sort.Strings(want)

	// Other enable sources must not widen the closed profile.
	cfg := &config.Config{
		Token: "x", ToolProfile: config.ProfileReview,
		Pipeline: true, Issues: true, EnabledTools: []string{"create_pipeline", "list_issues"},
	}
	got := sortedKeys(registerNames(t, cfg))
	if !slices.Equal(got, want) {
		t.Fatalf("review tools/list mismatch\n got: %v\nwant: %v", got, want)
	}
	if len(got) != 53 {
		t.Fatalf("review catalog size = %d, want 53", len(got))
	}
	if !slices.Equal(sortedKeysOf(ReviewTools()), want) {
		t.Fatal("ReviewTools() must match the registered review set")
	}
	names := registerNames(t, cfg)
	for _, w := range []string{
		"create_pipeline", "retry_pipeline", "cancel_pipeline", "play_pipeline_job",
		"retry_pipeline_job", "cancel_pipeline_job",
	} {
		if names[w] {
			t.Fatalf("pipeline write tool %q exposed in review profile", w)
		}
	}
}

func sortedKeysOf(in []string) []string {
	out := slices.Clone(in)
	sort.Strings(out)
	return out
}

func TestRegisterAll_reviewProfileReadOnlyAndDisable(t *testing.T) {
	names := registerNames(t, &config.Config{
		Token: "x", ToolProfile: config.ProfileReview, ReadOnly: true,
		DisabledTools: []string{"get_pipeline_job_output"},
	})
	for _, n := range []string{"create_merge_request_discussion_note", "resolve_merge_request_thread", "get_pipeline_job_output"} {
		if names[n] {
			t.Fatalf("%q must be subtracted by read-only / disabled list", n)
		}
	}
	if !names["get_merge_request_discussion"] || !names["list_pipelines"] {
		t.Fatal("read tools must remain")
	}
}

func TestRegisterAll_dailyAndDefaultUnchangedByProfileWork(t *testing.T) {
	daily := registerNames(t, &config.Config{Token: "x", UseDailyTools: true})
	if !slices.Equal(sortedKeys(daily), sortedKeysOf(DailyTools())) {
		t.Fatal("USE_DAILY_TOOLS must register exactly DailyTools()")
	}
	for _, n := range reviewExtraTools {
		if daily[n] {
			t.Fatalf("review-only tool %q leaked into daily set", n)
		}
	}
	def := registerNames(t, &config.Config{Token: "x"})
	for _, n := range []string{"list_pipelines", "create_pipeline", "get_pipeline_job_output"} {
		if def[n] {
			t.Fatalf("default catalog must keep pipeline family off, got %q", n)
		}
	}
	for _, n := range []string{"get_merge_request_discussion", "resolve_merge_request_thread", "list_issues", "get_review_queue", "get_review_snapshot", "batch_get_file_contents"} {
		if !def[n] {
			t.Fatalf("default catalog lost %q", n)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
