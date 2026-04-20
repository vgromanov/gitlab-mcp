package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// RegisterRepository registers repository / file / branch / commit tools.
func RegisterRepository(s *mcp.Server, d Deps) {
	AddTool(s, d, false, "", &mcp.Tool{Name: "search_repositories", Description: "Search GitLab projects"}, searchRepositories)
	AddTool(s, d, true, "", &mcp.Tool{Name: "create_repository", Description: "Create a new GitLab project"}, createRepository)
	AddTool(s, d, true, "", &mcp.Tool{Name: "fork_repository", Description: "Fork a project"}, forkRepository)
	AddTool(s, d, false, "", &mcp.Tool{Name: "get_file_contents", Description: "Get file or directory from a project repository"}, getFileContents)
	AddTool(s, d, true, "", &mcp.Tool{Name: "create_or_update_file", Description: "Create or update a single file on a branch"}, createOrUpdateFile)
	AddTool(s, d, true, "", &mcp.Tool{Name: "push_files", Description: "Create a commit with multiple file actions"}, pushFiles)
	AddTool(s, d, false, "", &mcp.Tool{Name: "get_repository_tree", Description: "List files and directories in repository"}, getRepositoryTree)
	AddTool(s, d, true, "", &mcp.Tool{Name: "create_branch", Description: "Create a new branch"}, createBranch)
	AddTool(s, d, false, "", &mcp.Tool{Name: "list_commits", Description: "List repository commits"}, listCommits)
	AddTool(s, d, false, "", &mcp.Tool{Name: "get_commit", Description: "Get a single commit"}, getCommit)
	AddTool(s, d, false, "", &mcp.Tool{Name: "get_commit_diff", Description: "Get diff for a commit"}, getCommitDiff)
	AddTool(s, d, false, "", &mcp.Tool{Name: "get_branch_diffs", Description: "Compare two refs (branches/tags/shas)"}, getBranchDiffs)
}

type searchRepositoriesIn struct {
	Pagination
	Search *string `json:"search,omitempty"`
}

func searchRepositories(ctx context.Context, _ *mcp.CallToolRequest, in searchRepositoriesIn, d Deps) (*mcp.CallToolResult, any, error) {
	page, perPage := in.ListOpts()
	opt := &gitlab.ListProjectsOptions{ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)}}
	if in.Search != nil {
		opt.Search = gitlab.Ptr(*in.Search)
	}
	projects, resp, err := d.Client.Projects.ListProjects(opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"projects": projects, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type createRepositoryIn struct {
	Name                 string  `json:"name" jsonschema:"Project name"`
	Path                 *string `json:"path,omitempty"`
	NamespaceID          *int64  `json:"namespace_id,omitempty"`
	Description          *string `json:"description,omitempty"`
	Visibility           *string `json:"visibility,omitempty"`
	InitializeWithReadme *bool   `json:"initialize_with_readme,omitempty"`
}

func createRepository(ctx context.Context, _ *mcp.CallToolRequest, in createRepositoryIn, d Deps) (*mcp.CallToolResult, any, error) {
	opt := &gitlab.CreateProjectOptions{Name: gitlab.Ptr(in.Name)}
	if in.Path != nil {
		opt.Path = in.Path
	}
	if in.NamespaceID != nil {
		opt.NamespaceID = in.NamespaceID
	}
	if in.Description != nil {
		opt.Description = in.Description
	}
	if in.Visibility != nil {
		v := gitlab.VisibilityValue(*in.Visibility)
		opt.Visibility = &v
	}
	if in.InitializeWithReadme != nil {
		opt.InitializeWithReadme = in.InitializeWithReadme
	}
	p, _, err := d.Client.Projects.CreateProject(opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(p), nil
}

type forkRepositoryIn struct {
	ProjectID     string  `json:"project_id"`
	NamespaceID   *int64  `json:"namespace_id,omitempty"`
	NamespacePath *string `json:"namespace_path,omitempty"`
	Name          *string `json:"name,omitempty"`
	Path          *string `json:"path,omitempty"`
}

func forkRepository(ctx context.Context, _ *mcp.CallToolRequest, in forkRepositoryIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := ResolveProjectID(in.ProjectID, d.Config.DefaultProjectID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return nil, nil, err
	}
	opt := &gitlab.ForkProjectOptions{}
	if in.NamespaceID != nil {
		opt.NamespaceID = in.NamespaceID
	}
	if in.NamespacePath != nil {
		opt.NamespacePath = in.NamespacePath
	}
	if in.Name != nil {
		opt.Name = in.Name
	}
	if in.Path != nil {
		opt.Path = in.Path
	}
	p, _, err := d.Client.Projects.ForkProject(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(p), nil
}

type getFileContentsIn struct {
	ProjectID string `json:"project_id"`
	FilePath  string `json:"file_path,omitempty" jsonschema:"Path to file; empty lists repo root via tree"`
	Ref       string `json:"ref,omitempty" jsonschema:"Branch, tag, or commit sha"`
}

func getFileContents(ctx context.Context, _ *mcp.CallToolRequest, in getFileContentsIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := ResolveProjectID(in.ProjectID, d.Config.DefaultProjectID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return nil, nil, err
	}
	ref := strings.TrimSpace(in.Ref)
	if ref == "" {
		ref = "HEAD"
	}
	fp := strings.TrimSpace(in.FilePath)
	if fp == "" {
		nodes, _, err := d.Client.Repositories.ListTree(pid, &gitlab.ListTreeOptions{Ref: gitlab.Ptr(ref)}, gitlab.WithContext(ctx))
		if err != nil {
			return nil, nil, err
		}
		return nil, Out(map[string]any{"type": "tree", "ref": ref, "entries": nodes}), nil
	}
	raw, _, err := d.Client.RepositoryFiles.GetRawFile(pid, fp, &gitlab.GetRawFileOptions{Ref: gitlab.Ptr(ref)}, gitlab.WithContext(ctx))
	if err == nil {
		return nil, Out(map[string]any{"type": "file", "file_path": fp, "ref": ref, "encoding": "utf-8", "content": string(raw)}), nil
	}
	f, _, err2 := d.Client.RepositoryFiles.GetFile(pid, fp, &gitlab.GetFileOptions{Ref: gitlab.Ptr(ref)}, gitlab.WithContext(ctx))
	if err2 != nil {
		return nil, nil, fmt.Errorf("get file: %w (metadata: %v)", err, err2)
	}
	if f.Content != "" {
		dec, decErr := base64.StdEncoding.DecodeString(f.Content)
		if decErr == nil {
			return nil, Out(map[string]any{"type": "file", "file_path": fp, "ref": ref, "encoding": f.Encoding, "size": f.Size, "content": string(dec)}), nil
		}
	}
	return nil, Out(map[string]any{"type": "file", "file_path": fp, "ref": ref, "file": f}), nil
}

type createOrUpdateFileIn struct {
	ProjectID     string `json:"project_id"`
	FilePath      string `json:"file_path"`
	Branch        string `json:"branch"`
	Content       string `json:"content"`
	CommitMessage string `json:"commit_message"`
	Mode          string `json:"mode,omitempty" jsonschema:"create or update (default: create)"`
}

func createOrUpdateFile(ctx context.Context, _ *mcp.CallToolRequest, in createOrUpdateFileIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := ResolveProjectID(in.ProjectID, d.Config.DefaultProjectID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return nil, nil, err
	}
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode == "" {
		mode = "create"
	}
	switch mode {
	case "create":
		fi, _, err := d.Client.RepositoryFiles.CreateFile(pid, in.FilePath, &gitlab.CreateFileOptions{
			Branch:        gitlab.Ptr(in.Branch),
			Content:       gitlab.Ptr(in.Content),
			CommitMessage: gitlab.Ptr(in.CommitMessage),
		}, gitlab.WithContext(ctx))
		if err != nil {
			return nil, nil, err
		}
		return nil, Out(fi), nil
	case "update":
		fi, _, err := d.Client.RepositoryFiles.UpdateFile(pid, in.FilePath, &gitlab.UpdateFileOptions{
			Branch:        gitlab.Ptr(in.Branch),
			Content:       gitlab.Ptr(in.Content),
			CommitMessage: gitlab.Ptr(in.CommitMessage),
		}, gitlab.WithContext(ctx))
		if err != nil {
			return nil, nil, err
		}
		return nil, Out(fi), nil
	default:
		return nil, nil, fmt.Errorf("mode must be create or update, got %q", in.Mode)
	}
}

type fileActionIn struct {
	Action       string  `json:"action"`
	FilePath     string  `json:"file_path"`
	Content      *string `json:"content,omitempty"`
	PreviousPath *string `json:"previous_path,omitempty"`
}

type pushFilesIn struct {
	ProjectID     string         `json:"project_id"`
	Branch        string         `json:"branch"`
	CommitMessage string         `json:"commit_message"`
	Actions       []fileActionIn `json:"actions"`
	StartBranch   *string        `json:"start_branch,omitempty"`
}

func pushFiles(ctx context.Context, _ *mcp.CallToolRequest, in pushFilesIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := ResolveProjectID(in.ProjectID, d.Config.DefaultProjectID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return nil, nil, err
	}
	var acts []*gitlab.CommitActionOptions
	for _, a := range in.Actions {
		av := gitlab.FileActionValue(a.Action)
		c := &gitlab.CommitActionOptions{
			Action:   &av,
			FilePath: gitlab.Ptr(a.FilePath),
		}
		if a.Content != nil {
			c.Content = a.Content
		}
		if a.PreviousPath != nil {
			c.PreviousPath = a.PreviousPath
		}
		acts = append(acts, c)
	}
	opt := &gitlab.CreateCommitOptions{
		Branch:        gitlab.Ptr(in.Branch),
		CommitMessage: gitlab.Ptr(in.CommitMessage),
		Actions:       acts,
	}
	if in.StartBranch != nil {
		opt.StartBranch = in.StartBranch
	}
	cmt, _, err := d.Client.Commits.CreateCommit(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(cmt), nil
}

type getRepositoryTreeIn struct {
	ProjectID string `json:"project_id"`
	Ref       string `json:"ref,omitempty"`
	Path      string `json:"path,omitempty"`
	Recursive *bool  `json:"recursive,omitempty"`
	Pagination
}

func getRepositoryTree(ctx context.Context, _ *mcp.CallToolRequest, in getRepositoryTreeIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := ResolveProjectID(in.ProjectID, d.Config.DefaultProjectID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	opt := &gitlab.ListTreeOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
	}
	if in.Ref != "" {
		opt.Ref = gitlab.Ptr(in.Ref)
	}
	if in.Path != "" {
		opt.Path = gitlab.Ptr(in.Path)
	}
	if in.Recursive != nil {
		opt.Recursive = in.Recursive
	}
	nodes, resp, err := d.Client.Repositories.ListTree(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"tree": nodes, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type createBranchIn struct {
	ProjectID *string `json:"project_id,omitempty"`
	Branch    string  `json:"branch" jsonschema:"New branch name"`
	Ref       string  `json:"ref,omitempty" jsonschema:"Source branch or sha (default default branch)"`
}

func createBranch(ctx context.Context, _ *mcp.CallToolRequest, in createBranchIn, d Deps) (*mcp.CallToolResult, any, error) {
	var pidStr string
	if in.ProjectID != nil {
		pidStr = *in.ProjectID
	}
	pid, err := ResolveProjectID(pidStr, d.Config.DefaultProjectID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return nil, nil, err
	}
	opt := &gitlab.CreateBranchOptions{Branch: gitlab.Ptr(in.Branch)}
	if in.Ref != "" {
		opt.Ref = gitlab.Ptr(in.Ref)
	}
	b, _, err := d.Client.Branches.CreateBranch(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(b), nil
}

type listCommitsIn struct {
	ProjectID string `json:"project_id"`
	RefName   string `json:"ref_name,omitempty"`
	Path      string `json:"path,omitempty"`
	Since     string `json:"since,omitempty"`
	Until     string `json:"until,omitempty"`
	Pagination
}

func listCommits(ctx context.Context, _ *mcp.CallToolRequest, in listCommitsIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := ResolveProjectID(in.ProjectID, d.Config.DefaultProjectID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	opt := &gitlab.ListCommitsOptions{ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)}}
	if in.RefName != "" {
		opt.RefName = gitlab.Ptr(in.RefName)
	}
	if in.Path != "" {
		opt.Path = gitlab.Ptr(in.Path)
	}
	if in.Since != "" {
		t, err := parseCommitTime(in.Since)
		if err != nil {
			return nil, nil, fmt.Errorf("since: %w", err)
		}
		opt.Since = &t
	}
	if in.Until != "" {
		t, err := parseCommitTime(in.Until)
		if err != nil {
			return nil, nil, fmt.Errorf("until: %w", err)
		}
		opt.Until = &t
	}
	commits, resp, err := d.Client.Commits.ListCommits(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"commits": commits, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type getCommitIn struct {
	ProjectID string `json:"project_id"`
	Sha       string `json:"sha"`
}

func getCommit(ctx context.Context, _ *mcp.CallToolRequest, in getCommitIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := ResolveProjectID(in.ProjectID, d.Config.DefaultProjectID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return nil, nil, err
	}
	c, _, err := d.Client.Commits.GetCommit(pid, in.Sha, nil, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(c), nil
}

type getCommitDiffIn struct {
	ProjectID     string `json:"project_id"`
	Sha           string `json:"sha"`
	TruncateLines int    `json:"truncate_lines,omitempty"`
}

func getCommitDiff(ctx context.Context, _ *mcp.CallToolRequest, in getCommitDiffIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := ResolveProjectID(in.ProjectID, d.Config.DefaultProjectID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return nil, nil, err
	}
	diffs, _, err := d.Client.Commits.GetCommitDiff(pid, in.Sha, nil, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	if in.TruncateLines > 0 {
		for _, d := range diffs {
			if d != nil {
				d.Diff = TruncateLines(d.Diff, in.TruncateLines)
			}
		}
	}
	return nil, Out(map[string]any{"diffs": diffs}), nil
}

type getBranchDiffsIn struct {
	ProjectID     string `json:"project_id"`
	From          string `json:"from" jsonschema:"From ref (sha/branch/tag)"`
	To            string `json:"to" jsonschema:"To ref"`
	TruncateLines int    `json:"truncate_lines,omitempty"`
}

func getBranchDiffs(ctx context.Context, _ *mcp.CallToolRequest, in getBranchDiffsIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := ResolveProjectID(in.ProjectID, d.Config.DefaultProjectID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return nil, nil, err
	}
	cmp, _, err := d.Client.Repositories.Compare(pid, &gitlab.CompareOptions{
		From: gitlab.Ptr(in.From),
		To:   gitlab.Ptr(in.To),
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	if in.TruncateLines > 0 && cmp != nil {
		for _, diff := range cmp.Diffs {
			if diff != nil {
				diff.Diff = TruncateLines(diff.Diff, in.TruncateLines)
			}
		}
	}
	return nil, Out(cmp), nil
}

func parseCommitTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	layouts := []string{time.RFC3339, "2006-01-02T15:04:05Z07:00", "2006-01-02"}
	var lastErr error
	for _, layout := range layouts {
		t, err := time.Parse(layout, s)
		if err == nil {
			return t, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return time.Time{}, fmt.Errorf("parse time %q: %w", s, lastErr)
	}
	return time.Time{}, fmt.Errorf("parse time %q", s)
}
