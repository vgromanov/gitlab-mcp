package tools

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RegisterAll wires every tool group onto the MCP server.
func RegisterAll(s *mcp.Server, d Deps) {
	RegisterProjects(s, d)
	RegisterRepository(s, d)
	RegisterMergeRequests(s, d)
	RegisterMRNotes(s, d)
	RegisterDraftNotes(s, d)
	RegisterIssues(s, d)
	RegisterIssueNotes(s, d)
	RegisterLabels(s, d)
	RegisterPipelines(s, d)
	RegisterDeployments(s, d)
	RegisterArtifacts(s, d)
	RegisterMilestones(s, d)
	RegisterWiki(s, d)
	RegisterReleases(s, d)
	RegisterIterationsEvents(s, d)
	RegisterMarkdown(s, d)
	RegisterSearch(s, d)
	RegisterWebhooks(s, d)
	RegisterGraphQLTools(s, d)
}
