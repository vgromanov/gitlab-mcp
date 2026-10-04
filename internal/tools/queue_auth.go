package tools

import (
	"context"
	"errors"
	"strconv"
	"strings"

	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
	glbudget "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
)

// Queue-local identity helpers. Shared getProjectSafe/getGroupSafe/groupAncestryContains
// keep collapsing transport budgets for legacy callers; the review queue must not.

func queueBudgetOrContext(err error) error {
	if err == nil {
		return nil
	}
	if isTypedBudget(err) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return glbudget.ErrBudgetElapsed
	}
	if errors.Is(err, context.Canceled) {
		return errQueueCancelled
	}
	return nil
}

func queueContextStop(ctx context.Context) error {
	return queueBudgetOrContext(ctx.Err())
}

var errQueueCancelled = errors.New(igl.CodeCancelled)

func queueGetProject(ctx context.Context, d Deps, pid string) (*gitlab.Project, error) {
	tok := normalizeIdentityToken(pid)
	if tok == "" {
		return nil, identityErr("resolve project identity")
	}
	wantNumeric, isNumeric := parseStrictPositiveID(tok)
	p, _, err := d.Client.Projects.GetProject(tok, nil, gitlab.WithContext(ctx))
	if err != nil {
		if kept := queueBudgetOrContext(err); kept != nil {
			return nil, kept
		}
		return nil, identityErr("resolve project identity")
	}
	if p == nil || p.ID <= 0 {
		return nil, identityErr("malformed canonical identity")
	}
	if isNumeric && p.ID != wantNumeric {
		return nil, identityErr("numeric project identity mismatch")
	}
	return p, nil
}

func queueGetGroup(ctx context.Context, d Deps, gid string) (*gitlab.Group, error) {
	tok := normalizeIdentityToken(gid)
	if tok == "" {
		return nil, identityErr("resolve group identity")
	}
	wantNumeric, isNumeric := parseStrictPositiveID(tok)
	g, _, err := d.Client.Groups.GetGroup(tok, nil, gitlab.WithContext(ctx))
	if err != nil {
		if kept := queueBudgetOrContext(err); kept != nil {
			return nil, kept
		}
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

func queueResolvePrincipal(ctx context.Context, d Deps) (int64, error) {
	u, _, err := d.Client.Users.CurrentUser(gitlab.WithContext(ctx))
	if err != nil {
		if kept := queueBudgetOrContext(err); kept != nil {
			return 0, kept
		}
		return 0, identityErr("authenticated actor")
	}
	if u == nil || u.ID < 1 {
		return 0, identityErr("authenticated actor")
	}
	return u.ID, nil
}

func queueResolveDiscoveryActor(ctx context.Context, d Deps, id int64) (int64, error) {
	if id < 1 {
		return 0, identityErr("discovery actor")
	}
	u, _, err := d.Client.Users.GetUser(id, nil, gitlab.WithContext(ctx))
	if err != nil {
		if kept := queueBudgetOrContext(err); kept != nil {
			return 0, kept
		}
		return 0, identityErr("discovery actor")
	}
	if u == nil || u.ID != id {
		return 0, identityErr("discovery actor")
	}
	return u.ID, nil
}

func queueGroupAncestryContains(ctx context.Context, d Deps, startNamespaceID int64, allowed map[int64]struct{}) (bool, error) {
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
		g, _, err := d.Client.Groups.GetGroup(strconv.FormatInt(cur, 10), nil, gitlab.WithContext(ctx))
		if err != nil {
			if kept := queueBudgetOrContext(err); kept != nil {
				return false, kept
			}
			// User namespace or non-group: not a member. Budget never takes this path.
			return false, nil
		}
		if g == nil || g.ID <= 0 {
			return false, nil
		}
		// A numeric ancestry lookup must return that same group. A rewritten id
		// is unresolved identity, not membership in the caller's allowlist.
		if g.ID != cur {
			return false, identityErr("numeric group identity mismatch")
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

func (st *queueRuntime) queueAllowedProjects(ctx context.Context) (map[int64]struct{}, error) {
	if st.allowedProjects != nil {
		return st.allowedProjects, nil
	}
	out := map[int64]struct{}{}
	if st.d.Config == nil {
		st.allowedProjects = out
		return out, nil
	}
	for _, tok := range st.d.Config.AllowedProjectIDs {
		if strings.TrimSpace(tok) == "" {
			continue
		}
		p, err := queueGetProject(ctx, st.d, tok)
		if err != nil {
			return nil, err
		}
		out[p.ID] = struct{}{}
	}
	st.allowedProjects = out
	return out, nil
}

func (st *queueRuntime) queueAllowedGroups(ctx context.Context) (map[int64]struct{}, error) {
	if st.allowedGroups != nil {
		return st.allowedGroups, nil
	}
	out := map[int64]struct{}{}
	if st.d.Config == nil {
		st.allowedGroups = out
		return out, nil
	}
	for _, tok := range st.d.Config.AllowedGroupIDs {
		if strings.TrimSpace(tok) == "" {
			continue
		}
		g, err := queueGetGroup(ctx, st.d, tok)
		if err != nil {
			return nil, err
		}
		out[g.ID] = struct{}{}
	}
	st.allowedGroups = out
	return out, nil
}

func (st *queueRuntime) queueProjectSatisfiesPolicy(ctx context.Context, canon CanonicalProject) error {
	if !policyActive(st.d.Config) {
		return nil
	}
	needProject := st.d.Config != nil && len(st.d.Config.AllowedProjectIDs) > 0
	needGroup := st.d.Config != nil && len(st.d.Config.AllowedGroupIDs) > 0
	if needProject {
		allowed, err := st.queueAllowedProjects(ctx)
		if err != nil {
			return err
		}
		if _, ok := allowed[canon.ID]; !ok {
			return authzDenied("project not allowed")
		}
	}
	if needGroup {
		allowed, err := st.queueAllowedGroups(ctx)
		if err != nil {
			return err
		}
		if canon.NamespaceKind != "group" {
			return authzDenied("project namespace not under allowed group")
		}
		ok, err := queueGroupAncestryContains(ctx, st.d, canon.NamespaceID, allowed)
		if err != nil {
			return err
		}
		if !ok {
			return authzDenied("project not under allowed group")
		}
	}
	return nil
}

func (st *queueRuntime) queueAuthorizeProject(ctx context.Context, projectID string) (CanonicalProject, error) {
	def := ""
	if st.d.Config != nil {
		def = st.d.Config.DefaultProjectID
	}
	pid, err := ResolveProjectID(projectID, def)
	if err != nil {
		return CanonicalProject{}, err
	}
	p, err := queueGetProject(ctx, st.d, pid)
	if err != nil {
		return CanonicalProject{}, err
	}
	canon := projectFromAPI(p)
	if err := st.queueProjectSatisfiesPolicy(ctx, canon); err != nil {
		return CanonicalProject{}, err
	}
	return canon, nil
}

func (st *queueRuntime) queueAuthorizeAdditional(ctx context.Context, projectIDs ...string) error {
	seen := map[string]struct{}{}
	for _, id := range projectIDs {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if _, err := st.queueAuthorizeProject(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func queueAuthorizeCanonicalGroup(ctx context.Context, d Deps, groupID string) (CanonicalGroup, error) {
	if strings.TrimSpace(groupID) == "" {
		return CanonicalGroup{}, identityErr("group_id required")
	}
	g, err := queueGetGroup(ctx, d, groupID)
	if err != nil {
		return CanonicalGroup{}, err
	}
	canon := CanonicalGroup{ID: g.ID, FullPath: g.FullPath}
	if d.Config == nil || len(d.Config.AllowedGroupIDs) == 0 {
		return canon, nil
	}
	allowed := map[int64]struct{}{}
	for _, tok := range d.Config.AllowedGroupIDs {
		if strings.TrimSpace(tok) == "" {
			continue
		}
		ag, err := queueGetGroup(ctx, d, tok)
		if err != nil {
			// The requested group is already known. A budget or cancel here has
			// not observed a provider page, so the caller can keep that group
			// and resume without treating the stop as a malformed page.
			if isTypedBudget(err) || errors.Is(err, errQueueCancelled) {
				return canon, err
			}
			return CanonicalGroup{}, err
		}
		allowed[ag.ID] = struct{}{}
	}
	if _, ok := allowed[canon.ID]; ok {
		return canon, nil
	}
	ok, err := queueGroupAncestryContains(ctx, d, canon.ID, allowed)
	if err != nil {
		if isTypedBudget(err) || errors.Is(err, errQueueCancelled) {
			return canon, err
		}
		return CanonicalGroup{}, err
	}
	if !ok {
		return CanonicalGroup{}, authzDenied("group not allowed")
	}
	return canon, nil
}

// canonicalSelectionID resolves a requested project token (numeric or path) once per invocation.
func (st *queueRuntime) canonicalSelectionID(ctx context.Context, token string) (int64, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return 0, identityErr("project selection")
	}
	if st.selectionCache == nil {
		st.selectionCache = map[string]int64{}
	}
	if id, ok := st.selectionCache[token]; ok {
		return id, nil
	}
	p, err := queueGetProject(ctx, st.d, token)
	if err != nil {
		return 0, err
	}
	st.selectionCache[token] = p.ID
	return p.ID, nil
}

// projectSelected is the single requested-project gate for reviewer, authored, and ongoing.
// An empty project_ids list selects every policy-permitted project.
func (st *queueRuntime) projectSelected(ctx context.Context, projectID int64) (bool, error) {
	if len(st.norm.projects) == 0 {
		return true, nil
	}
	if !st.selectionReady {
		ids := make(map[int64]struct{}, len(st.norm.projects))
		for _, tok := range st.norm.projects {
			id, err := st.canonicalSelectionID(ctx, tok)
			if err != nil {
				return false, err
			}
			ids[id] = struct{}{}
		}
		st.selectionIDs = ids
		st.selectionReady = true
	}
	_, ok := st.selectionIDs[projectID]
	return ok, nil
}
