package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
}

type pidMR struct {
	ProjectID       string `json:"project_id"`
	MergeRequestIID int64  `json:"merge_request_iid"`
}

func (p pidMR) resolve(d Deps) (string, error) {
	pid, err := ResolveProjectID(p.ProjectID, d.Config.DefaultProjectID)
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

type mergeMergeRequestIn struct {
	pidMR
	MergeCommitMessage       *string `json:"merge_commit_message,omitempty"`
	ShouldRemoveSourceBranch *bool   `json:"should_remove_source_branch,omitempty"`
}

func mergeMergeRequest(ctx context.Context, _ *mcp.CallToolRequest, in mergeMergeRequestIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
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
	pid, err := ResolveProjectID(in.ProjectID, d.Config.DefaultProjectID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
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
	pid, err := in.resolve(d)
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
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	diffs, _, err := d.Client.MergeRequests.ListMergeRequestDiffs(pid, in.MergeRequestIID, &gitlab.ListMergeRequestDiffsOptions{
		ListOptions: gitlab.ListOptions{PerPage: 100},
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	if in.TruncateLines > 0 {
		for _, df := range diffs {
			if df != nil {
				df.Diff = TruncateLines(df.Diff, in.TruncateLines)
			}
		}
	}
	return nil, Out(map[string]any{"diffs": diffs}), nil
}

type listMergeRequestDiffsIn struct {
	pidMR
	Pagination
	Unidiff *bool `json:"unidiff,omitempty"`
}

func listMergeRequestDiffs(ctx context.Context, _ *mcp.CallToolRequest, in listMergeRequestDiffsIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	opt := &gitlab.ListMergeRequestDiffsOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
		Unidiff:     in.Unidiff,
	}
	diffs, resp, err := d.Client.MergeRequests.ListMergeRequestDiffs(pid, in.MergeRequestIID, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"diffs": diffs, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type getMergeRequestConflictsIn struct {
	pidMR
}

func getMergeRequestConflicts(ctx context.Context, _ *mcp.CallToolRequest, in getMergeRequestConflictsIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	mr, _, err := d.Client.MergeRequests.GetMergeRequest(pid, in.MergeRequestIID, nil, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	diffs, _, err := d.Client.MergeRequests.ListMergeRequestDiffs(pid, in.MergeRequestIID, &gitlab.ListMergeRequestDiffsOptions{
		ListOptions: gitlab.ListOptions{PerPage: 200},
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	var conflictFiles []string
	for _, df := range diffs {
		if df == nil {
			continue
		}
		if strings.Contains(df.Diff, "<<<<<<<") || strings.Contains(df.Diff, ">>>>>>>") {
			conflictFiles = append(conflictFiles, df.NewPath)
		}
	}
	return nil, Out(map[string]any{
		"has_conflicts":         mr.HasConflicts,
		"detailed_merge_status": mr.DetailedMergeStatus,
		"conflict_files":        conflictFiles,
		"merge_request_iid":     mr.IID,
	}), nil
}

type listMergeRequestChangedFilesIn struct {
	pidMR
	ExcludedFilePatterns []string `json:"excluded_file_patterns,omitempty"`
	Pagination
}

func listMergeRequestChangedFiles(ctx context.Context, _ *mcp.CallToolRequest, in listMergeRequestChangedFilesIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
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
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	diffs, _, err := d.Client.MergeRequests.ListMergeRequestDiffs(pid, in.MergeRequestIID, &gitlab.ListMergeRequestDiffsOptions{
		ListOptions: gitlab.ListOptions{PerPage: 200},
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	want := map[string]struct{}{}
	for _, f := range in.Files {
		want[f] = struct{}{}
	}
	var out []*gitlab.MergeRequestDiff
	for _, df := range diffs {
		if df == nil {
			continue
		}
		_, wantNew := want[df.NewPath]
		_, wantOld := want[df.OldPath]
		if wantNew || (df.OldPath != "" && wantOld) {
			if in.TruncateLines > 0 {
				df.Diff = TruncateLines(df.Diff, in.TruncateLines)
			}
			out = append(out, df)
		}
	}
	return nil, Out(map[string]any{"diffs": out}), nil
}

type listMergeRequestVersionsIn struct {
	pidMR
	Pagination
}

func listMergeRequestVersions(ctx context.Context, _ *mcp.CallToolRequest, in listMergeRequestVersionsIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
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
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
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
	pid, err := in.resolve(d)
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
	ProjectID *string `json:"project_id,omitempty"`
	GroupID   *string `json:"group_id,omitempty"`
	State     *string `json:"state,omitempty"`
	AuthorID  *int64  `json:"author_id,omitempty"`
	Pagination
}

func listMergeRequests(ctx context.Context, _ *mcp.CallToolRequest, in listMergeRequestsIn, d Deps) (*mcp.CallToolResult, any, error) {
	page, perPage := in.ListOpts()
	if in.ProjectID != nil && *in.ProjectID != "" {
		pid, err := ResolveProjectID(*in.ProjectID, d.Config.DefaultProjectID)
		if err != nil {
			return nil, nil, err
		}
		if err := checkAllowedProject(d.Config, pid); err != nil {
			return nil, nil, err
		}
		opt := &gitlab.ListProjectMergeRequestsOptions{ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)}}
		if in.State != nil {
			opt.State = in.State
		}
		if in.AuthorID != nil {
			opt.AuthorID = in.AuthorID
		}
		mrs, resp, err := d.Client.MergeRequests.ListProjectMergeRequests(pid, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, nil, err
		}
		return nil, Out(map[string]any{"merge_requests": mrs, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
	}
	if in.GroupID != nil && *in.GroupID != "" {
		opt := &gitlab.ListGroupMergeRequestsOptions{ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)}}
		if in.State != nil {
			opt.State = in.State
		}
		mrs, resp, err := d.Client.MergeRequests.ListGroupMergeRequests(*in.GroupID, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, nil, err
		}
		return nil, Out(map[string]any{"merge_requests": mrs, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
	}
	opt := &gitlab.ListMergeRequestsOptions{ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)}}
	if in.State != nil {
		opt.State = in.State
	}
	if in.AuthorID != nil {
		opt.AuthorID = in.AuthorID
	}
	mrs, resp, err := d.Client.MergeRequests.ListMergeRequests(opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"merge_requests": mrs, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type approveMergeRequestIn struct {
	pidMR
	SHA *string `json:"sha,omitempty"`
}

func approveMergeRequest(ctx context.Context, _ *mcp.CallToolRequest, in approveMergeRequestIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
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
	pid, err := in.resolve(d)
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

func getMergeRequestApprovalState(ctx context.Context, _ *mcp.CallToolRequest, in getMergeRequestApprovalStateIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	st, _, err := d.Client.MergeRequestApprovals.GetApprovalState(pid, in.MergeRequestIID, gitlab.WithContext(ctx))
	if err != nil {
		// Fallback for older GitLab
		app, _, err2 := d.Client.MergeRequests.GetMergeRequestApprovals(pid, in.MergeRequestIID, gitlab.WithContext(ctx))
		if err2 != nil {
			return nil, nil, fmt.Errorf("approval_state: %w; approvals: %v", err, err2)
		}
		return nil, Out(app), nil
	}
	return nil, Out(st), nil
}
