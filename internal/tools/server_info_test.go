package tools

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/version"
)

func TestServerInfo_builder(t *testing.T) {
	const full = "bf02f09afdadf019544e6628f3febc9acf0e946b"
	const when = "2026-10-06T13:28:00Z"
	tests := []struct {
		name string
		in   version.VCS
		want serverInfoOut
	}{
		{"clean", version.VCS{Revision: full, Time: when}, serverInfoOut{
			Version: version.Version + "+bf02f09afdad", Revision: full, RevisionShort: "bf02f09afdad", VCSTime: when}},
		{"dirty", version.VCS{Revision: full, Time: when, Modified: true}, serverInfoOut{
			Version: version.Version + "+bf02f09afdad-dirty", Revision: full, RevisionShort: "bf02f09afdad", VCSTime: when, Modified: true}},
		{"missing time", version.VCS{Revision: "bf02f09"}, serverInfoOut{
			Version: version.Version + "+bf02f09", Revision: "bf02f09", RevisionShort: "bf02f09", VCSTime: "unknown"}},
		{"no VCS info", version.VCS{}, serverInfoOut{
			Version: version.Version + "+unknown", Revision: "unknown", RevisionShort: "unknown", VCSTime: "unknown"}},
		{"modified without revision is not reported modified", version.VCS{Modified: true}, serverInfoOut{
			Version: version.Version + "+unknown", Revision: "unknown", RevisionShort: "unknown", VCSTime: "unknown"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.want.Profile, tc.want.ToolCount = "review", 55
			if got := serverInfo(tc.in, "review", 55); got != tc.want {
				t.Fatalf("serverInfo = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// callServerInfo registers the tools over a GitLab stub that fails the test on
// any request, calls get_server_info and returns its structured result.
func callServerInfo(t *testing.T, cfg *config.Config) (map[string]any, int) {
	t.Helper()
	var hits atomic.Int32
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	srv := mcp.NewServer(&mcp.Implementation{Name: "gitlab-mcp-test", Version: "test"}, nil)
	RegisterAll(srv, Deps{Config: cfg, Client: cli})
	cs := testutil.MCPConnect(t, srv)
	listed := len(testutil.ToolNames(t, cs))
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_server_info"})
	if err != nil || res.IsError {
		t.Fatalf("get_server_info: err=%v content=%s", err, contentText(res))
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("get_server_info made %d GitLab request(s), want 0", n)
	}
	m, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content = %#v", res.StructuredContent)
	}
	return m, listed
}

func TestGetServerInfo_noGitLabRequestAndCountsItself(t *testing.T) {
	// An invalid token and a stub that records every request: the tool must
	// answer from the binary alone. The test binary carries no vcs.* settings.
	got, listed := callServerInfo(t, &config.Config{Token: "invalid", ToolProfile: config.ProfileReview})
	if got["profile"] != "review" || int(got["tool_count"].(float64)) != 55 || listed != 55 {
		t.Fatalf("profile/tool_count = %v/%v (tools/list %d), want review/55/55", got["profile"], got["tool_count"], listed)
	}
	if got["version"] != version.String() || got["revision_short"] != version.Build().Short() {
		t.Fatalf("version/revision_short = %v/%v, build says %q/%q", got["version"], got["revision_short"], version.String(), version.Build().Short())
	}
	for _, k := range []string{"revision", "vcs_time"} {
		if s, _ := got[k].(string); s == "" {
			t.Fatalf("%s must always be a non-empty string, got %#v", k, got[k])
		}
	}
	if _, ok := got["modified"].(bool); !ok {
		t.Fatalf("modified must be a bool, got %#v", got["modified"])
	}
}

func TestGetServerInfo_toolCountMatchesRegisteredTools(t *testing.T) {
	cases := []struct {
		name    string
		cfg     *config.Config
		profile string
		want    int // 0: only compare with tools/list
	}{
		{"review", &config.Config{Token: "x", ToolProfile: config.ProfileReview}, "review", 55},
		{"review read-only and disabled", &config.Config{
			Token: "x", ToolProfile: config.ProfileReview, ReadOnly: true,
			DisabledTools: []string{"get_pipeline_job_output", "get_review_queue"}}, "review", 0},
		{"default catalog", &config.Config{Token: "x"}, "default", 0},
		{"daily set plus the tool by name", &config.Config{
			Token: "x", UseDailyTools: true, EnabledTools: []string{"get_server_info"}}, "custom", 42},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, listed := callServerInfo(t, tc.cfg)
			n := int(got["tool_count"].(float64))
			if n != listed || (tc.want != 0 && n != tc.want) || got["profile"] != tc.profile {
				t.Fatalf("tool_count=%d profile=%v; tools/list=%d want=%d profile=%s", n, got["profile"], listed, tc.want, tc.profile)
			}
		})
	}
}

func TestGetServerInfo_absentFromDailySet(t *testing.T) {
	names := registerNames(t, &config.Config{Token: "x", UseDailyTools: true})
	if names["get_server_info"] {
		t.Fatal("daily set must stay unchanged (41 tools) and not carry get_server_info")
	}
}

func TestGetServerInfo_nilClientIsFine(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "gitlab-mcp-test", Version: "test"}, nil)
	RegisterAll(srv, Deps{Config: &config.Config{Token: "x"}}) // Client nil: any use would panic
	cs := testutil.MCPConnect(t, srv)
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_server_info"})
	if err != nil || res.IsError {
		t.Fatalf("get_server_info without a client: err=%v", err)
	}
}
