package tools

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	glclient "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// RegisterMergeRequests registers merge request tools.
func RegisterMergeRequests(s *mcp.Server, d Deps) {
	AddTool(s, d, true, "", &mcp.Tool{Name: "merge_merge_request", Description: "Accept / merge a merge request"}, mergeMergeRequest)
	AddTool(s, d, true, "", &mcp.Tool{Name: "create_merge_request", Description: "Create a merge request"}, createMergeRequest)
	AddTool(s, d, false, "", &mcp.Tool{Name: "get_merge_request", Description: "Get merge request details"}, getMergeRequest)
	AddTool(s, d, false, "", &mcp.Tool{Name: "get_merge_request_diffs", Description: "Get MR diffs (structured list, first page)"}, getMergeRequestDiffs)
	AddTool(s, d, false, "", &mcp.Tool{Name: "list_merge_request_diffs", Description: "List MR diffs with pagination"}, listMergeRequestDiffs)
	AddTool(s, d, false, "", &mcp.Tool{Name: "get_merge_request_conflicts", Description: "Summarize MR conflicts from diffs and MR flags"}, getMergeRequestConflicts)
	AddTool(s, d, false, "", &mcp.Tool{Name: "list_merge_request_changed_files", Description: "List changed file paths for an MR"}, listMergeRequestChangedFiles)
	AddTool(s, d, false, "", &mcp.Tool{Name: "get_merge_request_file_diff", Description: "Get diffs for specific files in an MR"}, getMergeRequestFileDiff)
	AddTool(s, d, false, "", &mcp.Tool{Name: "list_merge_request_versions", Description: "List MR diff versions"}, listMergeRequestVersions)
	AddTool(s, d, false, "", &mcp.Tool{Name: "get_merge_request_version", Description: "Get a single MR diff version"}, getMergeRequestVersion)
	AddTool(s, d, true, "", &mcp.Tool{Name: "update_merge_request", Description: "Update merge request fields"}, updateMergeRequest)
	AddTool(s, d, false, "", &mcp.Tool{Name: "list_merge_requests", Description: "List merge requests globally or in a project"}, listMergeRequests)
	AddTool(s, d, true, "", &mcp.Tool{Name: "approve_merge_request", Description: "Approve a merge request"}, approveMergeRequest)
	AddTool(s, d, true, "", &mcp.Tool{Name: "unapprove_merge_request", Description: "Remove your approval from an MR"}, unapproveMergeRequest)
	AddTool(s, d, false, "", &mcp.Tool{Name: "get_merge_request_approval_state", Description: "Get MR approval state"}, getMergeRequestApprovalState)
	AddTool(s, d, false, "", &mcp.Tool{Name: "get_merge_request_diff_window", Description: "Read one bounded immutable merge request diff manifest window"}, getMergeRequestDiffWindow)
	registerMergeRequestReviewQueue(s, d)
	registerMergeRequestReviewContext(s, d)
}

type pidMR struct {
	ProjectID       string `json:"project_id"`
	MergeRequestIID int64  `json:"merge_request_iid"`
}

func (p pidMR) resolve(ctx context.Context, d Deps) (string, error) {
	if p.MergeRequestIID < 1 {
		return "", fmt.Errorf("merge_request_iid must be >= 1")
	}
	def := ""
	if d.Config != nil {
		def = d.Config.DefaultProjectID
	}
	if policyActive(d.Config) {
		c, err := AuthorizeCanonicalProject(ctx, d, p.ProjectID)
		if err != nil {
			return "", err
		}
		return strconv.FormatInt(c.ID, 10), nil
	}
	pid, err := ResolveProjectID(p.ProjectID, def)
	if err != nil {
		return "", err
	}
	return pid, nil
}

// resolveLegacy keeps pre-policy ResolveProjectID + checkAllowedProject for
// out-of-matrix callers (draft notes) that share the pidMR shape only.
func (p pidMR) resolveLegacy(d Deps) (string, error) {
	def := ""
	if d.Config != nil {
		def = d.Config.DefaultProjectID
	}
	pid, err := ResolveProjectID(p.ProjectID, def)
	if err != nil {
		return "", err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return "", err
	}
	if p.MergeRequestIID < 1 {
		return "", fmt.Errorf("merge_request_iid must be >= 1")
	}
	return pid, nil
}

// authorizeMROwnerAndForks authorizes the owner project, loads MR metadata, then
// independently authorizes source/downstream project IDs before content reads.
// When policy is inactive, preserves legacy ResolveProjectID + checkAllowedProject
// without extra GetProject/GetMR (empty-both allow-all compatibility).
func authorizeMROwnerAndForks(ctx context.Context, d Deps, projectID string, mrIID int64) (CanonicalProject, error) {
	owner, _, err := loadAuthorizedMR(ctx, d, projectID, mrIID)
	return owner, err
}

func loadAuthorizedMR(ctx context.Context, d Deps, projectID string, mrIID int64) (CanonicalProject, *gitlab.MergeRequest, error) {
	if mrIID < 1 {
		return CanonicalProject{}, nil, fmt.Errorf("merge_request_iid must be >= 1")
	}
	if !policyActive(d.Config) {
		owner, err := legacyOwnerProjection(d, projectID)
		return owner, nil, err
	}
	owner, err := AuthorizeCanonicalProject(ctx, d, projectID)
	if err != nil {
		return CanonicalProject{}, nil, err
	}
	mr, _, err := d.Client.MergeRequests.GetMergeRequest(owner.ID, mrIID, nil, gitlab.WithContext(ctx))
	if err != nil {
		if passthroughTypedProviderErr(err) {
			return CanonicalProject{}, nil, err
		}
		return CanonicalProject{}, nil, fmt.Errorf("%s: merge request metadata", readmeta.CodeHTTPError)
	}
	if err := requireProvenMRForkProjects(ctx, d, owner, mr); err != nil {
		return CanonicalProject{}, nil, err
	}
	return owner, mr, nil
}

func legacyOwnerProjection(d Deps, projectID string) (CanonicalProject, error) {
	def := ""
	if d.Config != nil {
		def = d.Config.DefaultProjectID
	}
	pid, err := ResolveProjectID(projectID, def)
	if err != nil {
		return CanonicalProject{}, err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return CanonicalProject{}, err
	}
	if id, ok := parseStrictPositiveID(pid); ok {
		return CanonicalProject{ID: id, PathWithNamespace: pid}, nil
	}
	return CanonicalProject{PathWithNamespace: pid}, nil
}

func projectAPIID(c CanonicalProject) string {
	if c.ID > 0 {
		return strconv.FormatInt(c.ID, 10)
	}
	return c.PathWithNamespace
}

// requireProvenMRForkProjects fail-closes when MR metadata is missing or the source
// project identity is unproven (nil MR / SourceProjectID <= 0) under active policy.
func requireProvenMRForkProjects(ctx context.Context, d Deps, owner CanonicalProject, mr *gitlab.MergeRequest) error {
	if mr == nil {
		return identityErr("merge request metadata missing")
	}
	if mr.SourceProjectID <= 0 {
		return identityErr("unproven merge request source project")
	}
	var extra []string
	if mr.SourceProjectID != owner.ID {
		extra = append(extra, strconv.FormatInt(mr.SourceProjectID, 10))
	}
	if mr.ProjectID > 0 && mr.ProjectID != owner.ID {
		extra = append(extra, strconv.FormatInt(mr.ProjectID, 10))
	}
	_, err := AuthorizeAdditionalProjects(ctx, d, extra...)
	return err
}

// loadDiffIdentity always reads canonical owner, merge request, source, and target
// identity for the diff-window tool, including allow-all policy. loadAuthorizedMR
// stays the legacy helper for older callers.
func loadDiffIdentity(ctx context.Context, d Deps, projectID string, mrIID int64) (CanonicalProject, *gitlab.MergeRequest, error) {
	if mrIID < 1 {
		return CanonicalProject{}, nil, fmt.Errorf("merge_request_iid must be >= 1")
	}
	owner, err := AuthorizeCanonicalProject(ctx, d, projectID)
	if err != nil {
		return CanonicalProject{}, nil, err
	}
	mr, _, err := d.Client.MergeRequests.GetMergeRequest(owner.ID, mrIID, nil, gitlab.WithContext(ctx))
	if err != nil {
		if passthroughTypedProviderErr(err) {
			return CanonicalProject{}, nil, err
		}
		return CanonicalProject{}, nil, fmt.Errorf("%s: merge request metadata", readmeta.CodeHTTPError)
	}
	if err := bindDiffMR(owner, mrIID, mr); err != nil {
		return CanonicalProject{}, nil, err
	}
	if err := requireProvenMRForkProjects(ctx, d, owner, mr); err != nil {
		return CanonicalProject{}, nil, err
	}
	return owner, mr, nil
}

func bindDiffMR(owner CanonicalProject, iid int64, mr *gitlab.MergeRequest) error {
	if mr == nil || mr.ID < 1 {
		return identityErr("merge request metadata missing")
	}
	if mr.IID != iid {
		return identityErr("merge request iid mismatch")
	}
	if owner.ID < 1 || mr.ProjectID != owner.ID {
		return identityErr("merge request owner mismatch")
	}
	if mr.SourceProjectID < 1 {
		return identityErr("unproven merge request source project")
	}
	if mr.TargetProjectID < 0 || (mr.TargetProjectID > 0 && mr.TargetProjectID != owner.ID) {
		return identityErr("merge request target mismatch")
	}
	return nil
}

type mergeMergeRequestIn struct {
	pidMR
	MergeCommitMessage       *string `json:"merge_commit_message,omitempty"`
	ShouldRemoveSourceBranch *bool   `json:"should_remove_source_branch,omitempty"`
}

func mergeMergeRequest(ctx context.Context, _ *mcp.CallToolRequest, in mergeMergeRequestIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(ctx, d)
	if err != nil {
		return nil, nil, err
	}
	opt := &gitlab.AcceptMergeRequestOptions{}
	if in.MergeCommitMessage != nil {
		opt.MergeCommitMessage = in.MergeCommitMessage
	}
	if in.ShouldRemoveSourceBranch != nil {
		opt.ShouldRemoveSourceBranch = in.ShouldRemoveSourceBranch
	}
	mr, _, err := d.Client.MergeRequests.AcceptMergeRequest(pid, in.MergeRequestIID, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(mr), nil
}

type createMergeRequestIn struct {
	ProjectID          string  `json:"project_id"`
	SourceBranch       string  `json:"source_branch"`
	TargetBranch       string  `json:"target_branch"`
	Title              string  `json:"title"`
	Description        *string `json:"description,omitempty"`
	RemoveSourceBranch *bool   `json:"remove_source_branch,omitempty"`
}

func createMergeRequest(ctx context.Context, _ *mcp.CallToolRequest, in createMergeRequestIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := resolveProjectAuthz(ctx, d, in.ProjectID)
	if err != nil {
		return nil, nil, err
	}
	opt := &gitlab.CreateMergeRequestOptions{
		Title:        gitlab.Ptr(in.Title),
		SourceBranch: gitlab.Ptr(in.SourceBranch),
		TargetBranch: gitlab.Ptr(in.TargetBranch),
	}
	if in.Description != nil {
		opt.Description = in.Description
	}
	if in.RemoveSourceBranch != nil {
		opt.RemoveSourceBranch = in.RemoveSourceBranch
	}
	mr, _, err := d.Client.MergeRequests.CreateMergeRequest(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(mr), nil
}

type getMergeRequestIn struct {
	pidMR
	IncludeRebaseInProgress *bool `json:"include_rebase_in_progress,omitempty"`
}

func getMergeRequest(ctx context.Context, _ *mcp.CallToolRequest, in getMergeRequestIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(ctx, d)
	if err != nil {
		return nil, nil, err
	}
	opt := &gitlab.GetMergeRequestsOptions{}
	if in.IncludeRebaseInProgress != nil {
		opt.IncludeRebaseInProgress = in.IncludeRebaseInProgress
	}
	mr, _, err := d.Client.MergeRequests.GetMergeRequest(pid, in.MergeRequestIID, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(mr), nil
}

type getMergeRequestDiffsIn struct {
	pidMR
	TruncateLines int `json:"truncate_lines,omitempty"`
}

func getMergeRequestDiffs(ctx context.Context, _ *mcp.CallToolRequest, in getMergeRequestDiffsIn, d Deps) (*mcp.CallToolResult, any, error) {
	ctx, release := ensureLegacyInvocationBudget(ctx, 100)
	defer release()
	owner, err := authorizeMROwnerAndForks(ctx, d, in.ProjectID, in.MergeRequestIID)
	if err != nil {
		return nil, nil, err
	}
	page, err := fetchLegacyMRDiffPage(ctx, d, legacyMRDiffFetchOpts{
		Owner:           owner,
		MergeRequestIID: in.MergeRequestIID,
		PerPage:         100,
		TruncateLines:   in.TruncateLines,
	})
	if err != nil {
		return nil, nil, err
	}
	// Preserve existing object field "diffs"; add honesty pagination + section.
	return nil, Out(map[string]any{
		"diffs":      page.Diffs,
		"pagination": map[string]any{"next_page": page.Pagination.NextPage},
		"section":    page.Section,
	}), nil
}

type listMergeRequestDiffsIn struct {
	pidMR
	Pagination
	Unidiff *bool `json:"unidiff,omitempty"`
}

// listMergeRequestDiffs implementation lives in mr_diffs_envelope.go (envelope adapter).

type getMergeRequestConflictsIn struct {
	pidMR
}

func getMergeRequestConflicts(ctx context.Context, _ *mcp.CallToolRequest, in getMergeRequestConflictsIn, d Deps) (*mcp.CallToolResult, any, error) {
	ctx, release := ensureLegacyInvocationBudget(ctx, 200)
	defer release()
	owner, err := authorizeMROwnerAndForks(ctx, d, in.ProjectID, in.MergeRequestIID)
	if err != nil {
		return nil, nil, err
	}
	page, err := fetchLegacyMRDiffPage(ctx, d, legacyMRDiffFetchOpts{
		Owner:           owner,
		MergeRequestIID: in.MergeRequestIID,
		PerPage:         200,
	})
	if err != nil {
		return nil, nil, err
	}
	annotateConflictScanCoverage(&page.Section)
	conflictFiles := scanConflictFiles(page.Diffs)
	mr := page.MR
	var hasConflicts bool
	var detailedStatus string
	var iid int64
	if mr != nil {
		hasConflicts = mr.HasConflicts
		detailedStatus = mr.DetailedMergeStatus
		iid = mr.IID
	}
	// Preserve authoritative mergeability fields; heuristic scan never overrides them.
	return nil, Out(map[string]any{
		"has_conflicts":         hasConflicts,
		"detailed_merge_status": detailedStatus,
		"conflict_files":        conflictFiles,
		"merge_request_iid":     iid,
		"pagination":            map[string]any{"next_page": page.Pagination.NextPage},
		"section":               page.Section,
	}), nil
}

type listMergeRequestChangedFilesIn struct {
	pidMR
	ExcludedFilePatterns []string `json:"excluded_file_patterns,omitempty"`
	Pagination
}

func listMergeRequestChangedFiles(ctx context.Context, _ *mcp.CallToolRequest, in listMergeRequestChangedFilesIn, d Deps) (*mcp.CallToolResult, any, error) {
	owner, err := authorizeMROwnerAndForks(ctx, d, in.ProjectID, in.MergeRequestIID)
	if err != nil {
		return nil, nil, err
	}
	pid := projectAPIID(owner)
	page, perPage := in.ListOpts()
	diffs, resp, err := d.Client.MergeRequests.ListMergeRequestDiffs(pid, in.MergeRequestIID, &gitlab.ListMergeRequestDiffsOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	var paths []string
outer:
	for _, df := range diffs {
		if df == nil {
			continue
		}
		p := df.NewPath
		if p == "" {
			p = df.OldPath
		}
		for _, pat := range in.ExcludedFilePatterns {
			if matched, _ := pathMatch(pat, p); matched {
				continue outer
			}
		}
		paths = append(paths, p)
	}
	return nil, Out(map[string]any{"files": paths, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

// minimal glob: * suffix
func pathMatch(pattern, path string) (bool, error) {
	if pattern == "" {
		return false, nil
	}
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.HasPrefix(path, prefix), nil
	}
	return path == pattern, nil
}

type getMergeRequestFileDiffIn struct {
	pidMR
	Files         []string `json:"files" jsonschema:"Paths relative to repo (new_path)"`
	TruncateLines int      `json:"truncate_lines,omitempty"`
}

func getMergeRequestFileDiff(ctx context.Context, _ *mcp.CallToolRequest, in getMergeRequestFileDiffIn, d Deps) (*mcp.CallToolResult, any, error) {
	ctx, release := ensureLegacyInvocationBudget(ctx, 200)
	defer release()
	owner, err := authorizeMROwnerAndForks(ctx, d, in.ProjectID, in.MergeRequestIID)
	if err != nil {
		return nil, nil, err
	}
	page, err := fetchLegacyMRDiffPage(ctx, d, legacyMRDiffFetchOpts{
		Owner:           owner,
		MergeRequestIID: in.MergeRequestIID,
		PerPage:         200,
		TruncateLines:   in.TruncateLines,
		FilterFiles:     true,
		WantFiles:       in.Files,
	})
	if err != nil {
		return nil, nil, err
	}
	// Preserve existing object field "diffs"; missing requested paths are unobserved.
	return nil, Out(map[string]any{
		"diffs":      page.Diffs,
		"pagination": map[string]any{"next_page": page.Pagination.NextPage},
		"section":    page.Section,
	}), nil
}

type listMergeRequestVersionsIn struct {
	pidMR
	Pagination
}

func listMergeRequestVersions(ctx context.Context, _ *mcp.CallToolRequest, in listMergeRequestVersionsIn, d Deps) (*mcp.CallToolResult, any, error) {
	owner, err := authorizeMROwnerAndForks(ctx, d, in.ProjectID, in.MergeRequestIID)
	if err != nil {
		return nil, nil, err
	}
	pid := projectAPIID(owner)
	page, perPage := in.ListOpts()
	vers, resp, err := d.Client.MergeRequests.GetMergeRequestDiffVersions(pid, in.MergeRequestIID, &gitlab.GetMergeRequestDiffVersionsOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"versions": vers, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type getMergeRequestVersionIn struct {
	pidMR
	VersionID int64 `json:"version_id"`
}

func getMergeRequestVersion(ctx context.Context, _ *mcp.CallToolRequest, in getMergeRequestVersionIn, d Deps) (*mcp.CallToolResult, any, error) {
	owner, err := authorizeMROwnerAndForks(ctx, d, in.ProjectID, in.MergeRequestIID)
	if err != nil {
		return nil, nil, err
	}
	pid := projectAPIID(owner)
	v, _, err := d.Client.MergeRequests.GetSingleMergeRequestDiffVersion(pid, in.MergeRequestIID, in.VersionID, nil, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(v), nil
}

type updateMergeRequestIn struct {
	pidMR
	Title        *string `json:"title,omitempty"`
	Description  *string `json:"description,omitempty"`
	TargetBranch *string `json:"target_branch,omitempty"`
	StateEvent   *string `json:"state_event,omitempty"`
}

func updateMergeRequest(ctx context.Context, _ *mcp.CallToolRequest, in updateMergeRequestIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(ctx, d)
	if err != nil {
		return nil, nil, err
	}
	opt := &gitlab.UpdateMergeRequestOptions{}
	if in.Title != nil {
		opt.Title = in.Title
	}
	if in.Description != nil {
		opt.Description = in.Description
	}
	if in.TargetBranch != nil {
		opt.TargetBranch = in.TargetBranch
	}
	if in.StateEvent != nil {
		opt.StateEvent = in.StateEvent
	}
	mr, _, err := d.Client.MergeRequests.UpdateMergeRequest(pid, in.MergeRequestIID, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(mr), nil
}

type listMergeRequestsIn struct {
	ProjectID     *string `json:"project_id,omitempty" jsonschema:"Project id or path; mutually exclusive with group_id"`
	GroupID       *string `json:"group_id,omitempty" jsonschema:"Group id or path; mutually exclusive with project_id"`
	State         *string `json:"state,omitempty" jsonschema:"MR state filter (opened, closed, locked, merged, all)"`
	AuthorID      *int64  `json:"author_id,omitempty" jsonschema:"Positive GitLab user id of the author; omit when unused"`
	ReviewerID    *int64  `json:"reviewer_id,omitempty" jsonschema:"Positive GitLab user id of a reviewer; use with scope=all for discovery"`
	Scope         *string `json:"scope,omitempty" jsonschema:"created_by_me, assigned_to_me, reviews_for_me, or all; omit for legacy GitLab default"`
	UpdatedAfter  *string `json:"updated_after,omitempty" jsonschema:"Strict RFC3339/RFC3339Nano instant (2-digit hour, dot fractions only, legal offset); at most 9 fractional digits (nanosecond); lossless on the wire within that limit"`
	UpdatedBefore *string `json:"updated_before,omitempty" jsonschema:"Strict RFC3339/RFC3339Nano instant (2-digit hour, dot fractions only, legal offset); at most 9 fractional digits (nanosecond); lossless on the wire within that limit"`
	OrderBy       *string `json:"order_by,omitempty" jsonschema:"created_at, updated_at, label_priority, priority, milestone_due, popularity, or title"`
	Sort          *string `json:"sort,omitempty" jsonschema:"asc or desc"`
	Pagination
}

var listMRScopes = map[string]struct{}{
	"created_by_me":  {},
	"assigned_to_me": {},
	"reviews_for_me": {},
	"all":            {},
}

var listMROrderBy = map[string]struct{}{
	"created_at":     {},
	"updated_at":     {},
	"label_priority": {},
	"priority":       {},
	"milestone_due":  {},
	"popularity":     {},
	"title":          {},
}

type listMRParsedBounds struct {
	after  *time.Time
	before *time.Time
}

func validateListMergeRequestsIn(in listMergeRequestsIn) (listMRParsedBounds, error) {
	var bounds listMRParsedBounds
	proj := in.ProjectID != nil && strings.TrimSpace(*in.ProjectID) != ""
	grp := in.GroupID != nil && strings.TrimSpace(*in.GroupID) != ""
	if proj && grp {
		return bounds, fmt.Errorf("project_id and group_id are mutually exclusive")
	}
	if err := requirePositiveUserID("author_id", in.AuthorID); err != nil {
		return bounds, err
	}
	if err := requirePositiveUserID("reviewer_id", in.ReviewerID); err != nil {
		return bounds, err
	}
	if in.Scope != nil {
		s := strings.TrimSpace(*in.Scope)
		if _, ok := listMRScopes[s]; !ok {
			return bounds, fmt.Errorf("scope must be created_by_me, assigned_to_me, reviews_for_me, or all; got %q", *in.Scope)
		}
	}
	if in.OrderBy != nil {
		o := strings.TrimSpace(*in.OrderBy)
		if _, ok := listMROrderBy[o]; !ok {
			return bounds, fmt.Errorf("order_by must be one of created_at, updated_at, label_priority, priority, milestone_due, popularity, title; got %q", *in.OrderBy)
		}
	}
	if in.Sort != nil {
		s := strings.TrimSpace(*in.Sort)
		if s != "asc" && s != "desc" {
			return bounds, fmt.Errorf("sort must be asc or desc; got %q", *in.Sort)
		}
	}
	after, err := parseListMRTime("updated_after", in.UpdatedAfter)
	if err != nil {
		return bounds, err
	}
	before, err := parseListMRTime("updated_before", in.UpdatedBefore)
	if err != nil {
		return bounds, err
	}
	if after != nil && before != nil && after.After(*before) {
		return bounds, fmt.Errorf("updated_after must be <= updated_before")
	}
	bounds.after = after
	bounds.before = before
	return bounds, nil
}

func requirePositiveUserID(name string, id *int64) error {
	if id == nil {
		return nil
	}
	if *id <= 0 {
		return fmt.Errorf("%s must be a positive integer", name)
	}
	return nil
}

// Strict RFC3339 / RFC3339Nano (Z or ±HH:MM). Rejects Go time.Parse leniency
// (1-digit hour, comma fractions, illegal offsets) and >9 fractional digits
// so we never accept then silently truncate sub-nanosecond precision.
var listMRTimeRe = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?(Z|[+-](\d{2}):(\d{2}))$`)

func parseListMRTime(name string, raw *string) (*time.Time, error) {
	if raw == nil {
		return nil, nil
	}
	s := strings.TrimSpace(*raw)
	if s == "" {
		return nil, fmt.Errorf("%s must be a non-empty RFC3339 timestamp", name)
	}
	m := listMRTimeRe.FindStringSubmatch(s)
	if m == nil {
		return nil, fmt.Errorf("%s must be strict RFC3339/RFC3339Nano (2-digit hour, '.' fractional seconds with at most 9 digits, zone Z or ±HH:MM with HH 00-23 and MM 00-59)", name)
	}
	hour, _ := strconv.Atoi(m[4])
	min, _ := strconv.Atoi(m[5])
	sec, _ := strconv.Atoi(m[6])
	if hour > 23 || min > 59 || sec > 60 {
		return nil, fmt.Errorf("%s has out-of-range clock fields", name)
	}
	if m[9] != "" { // numeric ±HH:MM offset (empty when zone is Z)
		zh, _ := strconv.Atoi(m[9])
		zm, _ := strconv.Atoi(m[10])
		if zh > 23 || zm > 59 {
			return nil, fmt.Errorf("%s has out-of-range timezone offset", name)
		}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil, fmt.Errorf("%s must be a valid calendar RFC3339/RFC3339Nano instant: %w", name, err)
	}
	return &t, nil
}

func listMRRequestOpts(ctx context.Context, bounds listMRParsedBounds) []gitlab.RequestOptionFunc {
	opts := []gitlab.RequestOptionFunc{gitlab.WithContext(ctx)}
	if bounds.after != nil || bounds.before != nil {
		opts = append(opts, glclient.WithUpdatedBounds(bounds.after, bounds.before))
	}
	return opts
}

func listMergeRequests(ctx context.Context, _ *mcp.CallToolRequest, in listMergeRequestsIn, d Deps) (*mcp.CallToolResult, any, error) {
	bounds, err := validateListMergeRequestsIn(in)
	if err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	reqOpts := listMRRequestOpts(ctx, bounds)

	if in.ProjectID != nil && strings.TrimSpace(*in.ProjectID) != "" {
		pid, err := resolveProjectAuthz(ctx, d, *in.ProjectID)
		if err != nil {
			return nil, nil, err
		}
		opt := &gitlab.ListProjectMergeRequestsOptions{ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)}}
		applyListMRCommon(in, &opt.State, &opt.AuthorID, &opt.ReviewerID, &opt.Scope, &opt.OrderBy, &opt.Sort)
		mrs, resp, err := d.Client.MergeRequests.ListProjectMergeRequests(pid, opt, reqOpts...)
		if err != nil {
			return nil, nil, err
		}
		return nil, Out(map[string]any{"merge_requests": mrs, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
	}
	if in.GroupID != nil && strings.TrimSpace(*in.GroupID) != "" {
		gid := *in.GroupID
		if d.Config != nil && len(d.Config.AllowedGroupIDs) > 0 {
			g, err := AuthorizeCanonicalGroup(ctx, d, *in.GroupID)
			if err != nil {
				return nil, nil, err
			}
			gid = strconv.FormatInt(g.ID, 10)
		}
		opt := &gitlab.ListGroupMergeRequestsOptions{ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)}}
		applyListMRCommon(in, &opt.State, &opt.AuthorID, &opt.ReviewerID, &opt.Scope, &opt.OrderBy, &opt.Sort)
		mrs, resp, err := d.Client.MergeRequests.ListGroupMergeRequests(gid, opt, reqOpts...)
		if err != nil {
			return nil, nil, err
		}
		mrs, err = filterMergeRequestsByPolicy(ctx, d, mrs)
		if err != nil {
			return nil, nil, err
		}
		return nil, Out(map[string]any{"merge_requests": mrs, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
	}
	opt := &gitlab.ListMergeRequestsOptions{ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)}}
	applyListMRCommon(in, &opt.State, &opt.AuthorID, &opt.ReviewerID, &opt.Scope, &opt.OrderBy, &opt.Sort)
	mrs, resp, err := d.Client.MergeRequests.ListMergeRequests(opt, reqOpts...)
	if err != nil {
		return nil, nil, err
	}
	mrs, err = filterMergeRequestsByPolicy(ctx, d, mrs)
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"merge_requests": mrs, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

func applyListMRCommon(
	in listMergeRequestsIn,
	state **string,
	authorID **int64,
	reviewerID **gitlab.ReviewerIDValue,
	scope **string,
	orderBy **string,
	sort **string,
) {
	if in.State != nil {
		*state = in.State
	}
	if in.AuthorID != nil {
		*authorID = in.AuthorID
	}
	if in.ReviewerID != nil {
		*reviewerID = gitlab.ReviewerID(*in.ReviewerID)
	}
	if in.Scope != nil {
		s := strings.TrimSpace(*in.Scope)
		*scope = &s
	}
	if in.OrderBy != nil {
		o := strings.TrimSpace(*in.OrderBy)
		*orderBy = &o
	}
	if in.Sort != nil {
		s := strings.TrimSpace(*in.Sort)
		*sort = &s
	}
}

type approveMergeRequestIn struct {
	pidMR
	SHA *string `json:"sha,omitempty"`
}

func approveMergeRequest(ctx context.Context, _ *mcp.CallToolRequest, in approveMergeRequestIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(ctx, d)
	if err != nil {
		return nil, nil, err
	}
	opt := &gitlab.ApproveMergeRequestOptions{}
	if in.SHA != nil {
		opt.SHA = in.SHA
	}
	a, _, err := d.Client.MergeRequestApprovals.ApproveMergeRequest(pid, in.MergeRequestIID, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(a), nil
}

type unapproveMergeRequestIn struct {
	pidMR
}

func unapproveMergeRequest(ctx context.Context, _ *mcp.CallToolRequest, in unapproveMergeRequestIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(ctx, d)
	if err != nil {
		return nil, nil, err
	}
	_, err = d.Client.MergeRequestApprovals.UnapproveMergeRequest(pid, in.MergeRequestIID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"unapproved": true}), nil
}

type getMergeRequestApprovalStateIn struct {
	pidMR
}

func getMergeRequestApprovalState(ctx context.Context, _ *mcp.CallToolRequest, in getMergeRequestApprovalStateIn, d Deps) (*mcp.CallToolResult, mrApprovalReadResult, error) {
	ctx, _, release := ensureApprovalInvocationBudget(ctx)
	defer release()

	owner, err := authorizeAndVerifyApprovalMR(ctx, d, in.ProjectID, in.MergeRequestIID)
	if err != nil {
		return nil, mrApprovalReadResult{}, err
	}
	out, err := readNormalizedApprovals(ctx, d, owner, in.MergeRequestIID)
	if err != nil {
		return nil, mrApprovalReadResult{}, err
	}
	return nil, out, nil
}
