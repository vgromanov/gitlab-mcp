package tools

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

const maxGroupAncestryDepth = 32

// CanonicalProject is a safe identity projection (no raw API payload).
type CanonicalProject struct {
	ID                int64
	PathWithNamespace string
	NamespaceID       int64
	NamespaceKind     string
}

// CanonicalGroup is a safe group identity projection.
type CanonicalGroup struct {
	ID       int64
	FullPath string
}

func policyActive(cfg *config.Config) bool {
	return cfg != nil && cfg.PolicyActive()
}

func identityErr(detail string) error {
	return fmt.Errorf("%s: %s", readmeta.CodeIdentityUnresolved, detail)
}

func authzDenied(detail string) error {
	return fmt.Errorf("%s: %s", readmeta.CodeAuthzDenied, detail)
}

// normalizeIdentityToken trims and applies a single PathUnescape at the identity
// lookup boundary (getProjectSafe/getGroupSafe only). Callers must pass tokens
// through without a prior decode so once-encoded and double-encoded forms stay distinct.
func normalizeIdentityToken(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "%") {
		return s
	}
	dec, err := url.PathUnescape(s)
	if err != nil {
		return s
	}
	return dec
}

func parseStrictPositiveID(tok string) (int64, bool) {
	id, err := strconv.ParseInt(tok, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func getProjectSafe(ctx context.Context, d Deps, pid string) (*gitlab.Project, error) {
	tok := normalizeIdentityToken(pid)
	if tok == "" {
		return nil, identityErr("resolve project identity")
	}
	wantNumeric, isNumeric := parseStrictPositiveID(tok)
	p, _, err := d.Client.Projects.GetProject(tok, nil, gitlab.WithContext(ctx))
	if err != nil {
		return nil, identityErr("resolve project identity")
	}
	if p == nil || p.ID <= 0 {
		return nil, identityErr("malformed canonical identity")
	}
	// Numeric lookups must return the same ID (path redirects may remap path→ID).
	if isNumeric && p.ID != wantNumeric {
		return nil, identityErr("numeric project identity mismatch")
	}
	return p, nil
}

func getGroupSafe(ctx context.Context, d Deps, gid string) (*gitlab.Group, error) {
	tok := normalizeIdentityToken(gid)
	if tok == "" {
		return nil, identityErr("resolve group identity")
	}
	wantNumeric, isNumeric := parseStrictPositiveID(tok)
	g, _, err := d.Client.Groups.GetGroup(tok, nil, gitlab.WithContext(ctx))
	if err != nil {
		return nil, identityErr("resolve group identity")
	}
	if g == nil || g.ID <= 0 {
		return nil, identityErr("malformed canonical group identity")
	}
	if isNumeric && g.ID != wantNumeric {
		return nil, identityErr("numeric group identity mismatch")
	}
	return g, nil
}

func projectFromAPI(p *gitlab.Project) CanonicalProject {
	c := CanonicalProject{ID: p.ID, PathWithNamespace: p.PathWithNamespace}
	if p.Namespace != nil {
		c.NamespaceID = p.Namespace.ID
		c.NamespaceKind = strings.ToLower(strings.TrimSpace(p.Namespace.Kind))
	}
	return c
}

// resolveAllowedProjectIDSet resolves configured project allowlist tokens to numeric IDs.
// Path tokens that redirect to an ID are included; unresolvable tokens fail-closed.
// Tokens are passed raw; single PathUnescape happens only inside getProjectSafe.
func resolveAllowedProjectIDSet(ctx context.Context, d Deps) (map[int64]struct{}, error) {
	out := map[int64]struct{}{}
	if d.Config == nil {
		return out, nil
	}
	for _, tok := range d.Config.AllowedProjectIDs {
		if strings.TrimSpace(tok) == "" {
			continue
		}
		p, err := getProjectSafe(ctx, d, tok)
		if err != nil {
			return nil, err
		}
		out[p.ID] = struct{}{}
	}
	return out, nil
}

// resolveAllowedGroupIDSet resolves configured group allowlist tokens to canonical group IDs.
// Tokens are passed raw; single PathUnescape happens only inside getGroupSafe.
func resolveAllowedGroupIDSet(ctx context.Context, d Deps) (map[int64]struct{}, error) {
	out := map[int64]struct{}{}
	if d.Config == nil {
		return out, nil
	}
	for _, tok := range d.Config.AllowedGroupIDs {
		if strings.TrimSpace(tok) == "" {
			continue
		}
		g, err := getGroupSafe(ctx, d, tok)
		if err != nil {
			return nil, err
		}
		out[g.ID] = struct{}{}
	}
	return out, nil
}

func groupAncestryContains(ctx context.Context, d Deps, startNamespaceID int64, allowed map[int64]struct{}) (bool, error) {
	if startNamespaceID <= 0 {
		return false, nil
	}
	if _, ok := allowed[startNamespaceID]; ok {
		return true, nil
	}
	seen := map[int64]struct{}{}
	cur := startNamespaceID
	for depth := 0; depth < maxGroupAncestryDepth; depth++ {
		if _, ok := seen[cur]; ok {
			return false, authzDenied("group ancestry cycle")
		}
		seen[cur] = struct{}{}
		g, err := getGroupSafe(ctx, d, strconv.FormatInt(cur, 10))
		if err != nil {
			// Namespace may be a user namespace (Groups.GetGroup fails) — not a group member.
			return false, nil
		}
		if _, ok := allowed[g.ID]; ok {
			return true, nil
		}
		if g.ParentID <= 0 {
			return false, nil
		}
		cur = g.ParentID
	}
	return false, authzDenied("group ancestry depth exceeded")
}

func projectSatisfiesPolicy(ctx context.Context, d Deps, canon CanonicalProject) error {
	if !policyActive(d.Config) {
		return nil
	}
	needProject := d.Config != nil && len(d.Config.AllowedProjectIDs) > 0
	needGroup := d.Config != nil && len(d.Config.AllowedGroupIDs) > 0

	if needProject {
		allowed, err := resolveAllowedProjectIDSet(ctx, d)
		if err != nil {
			return err
		}
		if _, ok := allowed[canon.ID]; !ok {
			return authzDenied("project not allowed")
		}
	}
	if needGroup {
		allowed, err := resolveAllowedGroupIDSet(ctx, d)
		if err != nil {
			return err
		}
		// Group membership only for canonical group namespaces (exact kind).
		if canon.NamespaceKind != "group" {
			return authzDenied("project namespace not under allowed group")
		}
		ok, err := groupAncestryContains(ctx, d, canon.NamespaceID, allowed)
		if err != nil {
			return err
		}
		if !ok {
			return authzDenied("project not under allowed group")
		}
	}
	return nil
}

// AuthorizeCanonicalProject resolves project identity then checks allowlists.
// Only narrow identity lookups precede authorization; raw API objects are not returned.
func AuthorizeCanonicalProject(ctx context.Context, d Deps, projectID string) (CanonicalProject, error) {
	def := ""
	if d.Config != nil {
		def = d.Config.DefaultProjectID
	}
	pid, err := ResolveProjectID(projectID, def)
	if err != nil {
		return CanonicalProject{}, err
	}
	p, err := getProjectSafe(ctx, d, pid)
	if err != nil {
		return CanonicalProject{}, err
	}
	canon := projectFromAPI(p)
	if err := projectSatisfiesPolicy(ctx, d, canon); err != nil {
		return CanonicalProject{}, err
	}
	return canon, nil
}

// AuthorizeAdditionalProjects authorizes fork/downstream identities after owner authz.
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

// AuthorizeCanonicalGroup resolves a group identity and checks group allowlist.
// When group policy is unset, resolution still returns a safe projection (caller may filter projects separately).
// groupID is passed raw; single PathUnescape happens only inside getGroupSafe.
func AuthorizeCanonicalGroup(ctx context.Context, d Deps, groupID string) (CanonicalGroup, error) {
	if strings.TrimSpace(groupID) == "" {
		return CanonicalGroup{}, identityErr("group_id required")
	}
	g, err := getGroupSafe(ctx, d, groupID)
	if err != nil {
		return CanonicalGroup{}, err
	}
	canon := CanonicalGroup{ID: g.ID, FullPath: g.FullPath}
	if d.Config == nil || len(d.Config.AllowedGroupIDs) == 0 {
		return canon, nil
	}
	allowed, err := resolveAllowedGroupIDSet(ctx, d)
	if err != nil {
		return CanonicalGroup{}, err
	}
	if _, ok := allowed[canon.ID]; ok {
		return canon, nil
	}
	ok, err := groupAncestryContains(ctx, d, canon.ID, allowed)
	if err != nil {
		return CanonicalGroup{}, err
	}
	if !ok {
		return CanonicalGroup{}, authzDenied("group not allowed")
	}
	return canon, nil
}

// ProjectAllowedByPolicy reports whether a known project identity is in policy
// without returning raw API objects. Used for discovery filtering.
func ProjectAllowedByPolicy(ctx context.Context, d Deps, id int64, pathWithNamespace string, namespaceID int64, namespaceKind string) (bool, error) {
	if !policyActive(d.Config) {
		return true, nil
	}
	if id <= 0 {
		return false, identityErr("malformed project identity")
	}
	canon := CanonicalProject{
		ID:                id,
		PathWithNamespace: pathWithNamespace,
		NamespaceID:       namespaceID,
		NamespaceKind:     strings.ToLower(strings.TrimSpace(namespaceKind)),
	}
	if err := projectSatisfiesPolicy(ctx, d, canon); err != nil {
		if strings.HasPrefix(err.Error(), readmeta.CodeAuthzDenied) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// FilterProjectsByPolicy drops out-of-policy projects before MCP publish.
func FilterProjectsByPolicy(ctx context.Context, d Deps, projects []*gitlab.Project) ([]*gitlab.Project, error) {
	if !policyActive(d.Config) || len(projects) == 0 {
		return projects, nil
	}
	out := make([]*gitlab.Project, 0, len(projects))
	for _, p := range projects {
		if p == nil {
			continue
		}
		nsID, nsKind := int64(0), ""
		if p.Namespace != nil {
			nsID = p.Namespace.ID
			nsKind = p.Namespace.Kind
		}
		ok, err := ProjectAllowedByPolicy(ctx, d, p.ID, p.PathWithNamespace, nsID, nsKind)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// resolveProjectAuthz resolves project id with canonical policy when active;
// when policy is inactive keeps legacy ResolveProjectID (no extra GetProject).
func resolveProjectAuthz(ctx context.Context, d Deps, projectID string) (string, error) {
	def := ""
	if d.Config != nil {
		def = d.Config.DefaultProjectID
	}
	if policyActive(d.Config) {
		c, err := AuthorizeCanonicalProject(ctx, d, projectID)
		if err != nil {
			return "", err
		}
		return strconv.FormatInt(c.ID, 10), nil
	}
	return ResolveProjectID(projectID, def)
}

func filterMergeRequestsByPolicy(ctx context.Context, d Deps, mrs []*gitlab.BasicMergeRequest) ([]*gitlab.BasicMergeRequest, error) {
	if !policyActive(d.Config) || len(mrs) == 0 {
		return mrs, nil
	}
	out := make([]*gitlab.BasicMergeRequest, 0, len(mrs))
	for _, mr := range mrs {
		if mr == nil {
			continue
		}
		_, err := AuthorizeCanonicalProject(ctx, d, strconv.FormatInt(mr.ProjectID, 10))
		if err != nil {
			msg := err.Error()
			if strings.HasPrefix(msg, readmeta.CodeAuthzDenied) || strings.HasPrefix(msg, readmeta.CodeIdentityUnresolved) {
				continue
			}
			return nil, err
		}
		out = append(out, mr)
	}
	return out, nil
}
