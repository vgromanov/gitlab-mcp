package tools

import (
	"context"
	"fmt"
	"strconv"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// CanonicalProject is a safe identity projection (no raw API payload).
type CanonicalProject struct {
	ID                int64
	PathWithNamespace string
}

func allowedCanonical(d Deps, id int64, path string) error {
	if d.Config == nil || len(d.Config.AllowedProjectIDs) == 0 {
		return nil
	}
	sid := strconv.FormatInt(id, 10)
	for _, a := range d.Config.AllowedProjectIDs {
		if a == sid || a == path {
			return nil
		}
	}
	return fmt.Errorf("%s: project not allowed", readmeta.CodeAuthzDenied)
}

// AuthorizeCanonicalProject resolves project identity then checks allowlist.
// Only a narrow GetProject precedes authorization; raw identity is not returned beyond CanonicalProject.
func AuthorizeCanonicalProject(ctx context.Context, d Deps, projectID string) (CanonicalProject, error) {
	def := ""
	if d.Config != nil {
		def = d.Config.DefaultProjectID
	}
	pid, err := ResolveProjectID(projectID, def)
	if err != nil {
		return CanonicalProject{}, err
	}
	p, _, err := d.Client.Projects.GetProject(pid, nil, gitlab.WithContext(ctx))
	if err != nil {
		return CanonicalProject{}, fmt.Errorf("%s: resolve project identity", readmeta.CodeIdentityUnresolved)
	}
	if p == nil {
		return CanonicalProject{}, fmt.Errorf("%s: empty project identity", readmeta.CodeIdentityUnresolved)
	}
	// Reject malformed zero/nonpositive canonical identity before any content call.
	if p.ID <= 0 {
		return CanonicalProject{}, fmt.Errorf("%s: malformed canonical identity", readmeta.CodeIdentityUnresolved)
	}
	canon := CanonicalProject{ID: p.ID, PathWithNamespace: p.PathWithNamespace}
	if err := allowedCanonical(d, canon.ID, canon.PathWithNamespace); err != nil {
		return CanonicalProject{}, err
	}
	return canon, nil
}

// AuthorizeAdditionalProjects authorizes fork/downstream identities after owner authz.
// Synthetic multi-id fork/downstream cases unit-test this helper (no aggregate MCP tools).
func AuthorizeAdditionalProjects(ctx context.Context, d Deps, projectIDs ...string) ([]CanonicalProject, error) {
	out := make([]CanonicalProject, 0, len(projectIDs))
	seen := map[string]bool{}
	for _, id := range projectIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		c, err := AuthorizeCanonicalProject(ctx, d, id)
		if err != nil {
			return out, err
		}
		out = append(out, c)
	}
	return out, nil
}
