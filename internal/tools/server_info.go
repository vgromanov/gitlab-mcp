package tools

import (
	"cmp"
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/version"
)

type getServerInfoIn struct{}

// serverInfoOut is flat and always complete: build info the binary lacks is
// the string "unknown" (modified is then false), never an error.
type serverInfoOut struct {
	Version       string `json:"version"`
	Revision      string `json:"revision"`
	RevisionShort string `json:"revision_short"`
	VCSTime       string `json:"vcs_time"`
	Modified      bool   `json:"modified"`
	Profile       string `json:"profile"`
	ToolCount     int    `json:"tool_count"`
}

func serverInfo(v version.VCS, profile string, toolCount int) serverInfoOut {
	return serverInfoOut{
		Version: v.Format(version.Version), Revision: cmp.Or(v.Revision, "unknown"), RevisionShort: v.Short(),
		VCSTime: cmp.Or(v.Time, "unknown"), Modified: v.Modified && v.Revision != "", Profile: profile, ToolCount: toolCount,
	}
}

// RegisterServerInfo adds get_server_info. It never touches d.Client: it answers
// offline and with an invalid token. tool_count is read when called, after
// RegisterAll finished, and includes the tool itself.
func RegisterServerInfo(s *mcp.Server, d Deps) {
	tool := &mcp.Tool{
		Name: "get_server_info",
		Description: "Identity of the serving gitlab-mcp build, answered locally with no GitLab request (works offline and with an invalid token). " +
			"The supported way to verify which build serves you through the mcp-wrapper bridge, which does not forward the child's serverInfo. " +
			"Returns {version (<semver>+<revision_short>[-dirty]), revision (full commit id), revision_short, vcs_time, modified (uncommitted changes), " +
			"profile (review | daily | default | custom), tool_count (tools this instance registered, itself included)}; build info the binary lacks is \"unknown\"",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: new(bool)},
	}
	AddTool(s, d, false, "", tool, func(_ context.Context, _ *mcp.CallToolRequest, _ getServerInfoIn, d Deps) (*mcp.CallToolResult, any, error) {
		n := 0
		if d.registered != nil {
			n = *d.registered
		}
		return nil, Out(serverInfo(version.Build(), d.Config.ProfileName(), n)), nil
	})
}
