package tools

import (
	"io"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/vgromanov/gitlab-mcp/internal/config"
	"github.com/vgromanov/gitlab-mcp/internal/testutil"
)

// Exercise each Register* entrypoint with a live HTTP stub (no import cycles).
func TestRegister_toolGroupsSmoke(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	})
	cli, _ := testutil.NewGitLabClient(t, h)
	cfg := &config.Config{Token: "x", Wiki: true, Milestone: true, Pipeline: true}
	deps := Deps{Config: cfg, Client: cli}

	type reg struct {
		name string
		fn   func(*mcp.Server, Deps)
	}
	regs := []reg{
		{"projects", RegisterProjects},
		{"repository", RegisterRepository},
		{"merge_requests", RegisterMergeRequests},
		{"mr_notes", RegisterMRNotes},
		{"draft_notes", RegisterDraftNotes},
		{"issues", RegisterIssues},
		{"issue_notes", RegisterIssueNotes},
		{"labels", RegisterLabels},
		{"pipelines", RegisterPipelines},
		{"deployments", RegisterDeployments},
		{"artifacts", RegisterArtifacts},
		{"milestones", RegisterMilestones},
		{"wiki", RegisterWiki},
		{"releases", RegisterReleases},
		{"iterations_events", RegisterIterationsEvents},
		{"markdown", RegisterMarkdown},
		{"search", RegisterSearch},
		{"webhooks", RegisterWebhooks},
		{"graphql", RegisterGraphQLTools},
	}
	for _, tc := range regs {
		t.Run(tc.name, func(_ *testing.T) {
			srv := mcp.NewServer(&mcp.Implementation{Name: "gitlab-mcp-test", Version: "test"}, nil)
			tc.fn(srv, deps)
		})
	}
}
