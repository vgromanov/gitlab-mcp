package tools

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// RegisterIssues registers issue and issue-link tools.
func RegisterIssues(s *mcp.Server, d Deps) {
	AddTool(s, d, false, "", &mcp.Tool{Name: "list_issues", Description: "List issues (global); use scope=all for all visible"}, listIssues)
	AddTool(s, d, false, "", &mcp.Tool{Name: "my_issues", Description: "List issues assigned to the current user"}, myIssues)
	AddTool(s, d, false, "", &mcp.Tool{Name: "list_project_issues", Description: "List issues in a project"}, listProjectIssues)
	AddTool(s, d, false, "", &mcp.Tool{Name: "get_issue", Description: "Get issue by IID in a project"}, getIssue)
	AddTool(s, d, true, "", &mcp.Tool{Name: "create_issue", Description: "Create an issue"}, createIssue)
	AddTool(s, d, true, "", &mcp.Tool{Name: "update_issue", Description: "Update an issue"}, updateIssue)
	AddTool(s, d, true, "", &mcp.Tool{Name: "delete_issue", Description: "Delete an issue"}, deleteIssue)
	AddTool(s, d, false, "", &mcp.Tool{Name: "list_issue_links", Description: "List issue relations/links"}, listIssueLinks)
	AddTool(s, d, false, "", &mcp.Tool{Name: "get_issue_link", Description: "Get a single issue link"}, getIssueLink)
	AddTool(s, d, true, "", &mcp.Tool{Name: "create_issue_link", Description: "Link two issues"}, createIssueLink)
	AddTool(s, d, true, "", &mcp.Tool{Name: "delete_issue_link", Description: "Delete an issue link"}, deleteIssueLink)
}

type pidIssue struct {
	ProjectID string `json:"project_id"`
	IssueIID  int64  `json:"issue_iid"`
}

func (p pidIssue) resolve(d Deps) (string, error) {
	pid, err := ResolveProjectID(p.ProjectID, d.Config.DefaultProjectID)
	if err != nil {
		return "", err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return "", err
	}
	return pid, nil
}

type listIssuesIn struct {
	Pagination
	State  *string `json:"state,omitempty"`
	Scope  *string `json:"scope,omitempty"`
	Search *string `json:"search,omitempty"`
}

func listIssues(ctx context.Context, _ *mcp.CallToolRequest, in listIssuesIn, d Deps) (*mcp.CallToolResult, any, error) {
	page, perPage := in.ListOpts()
	opt := &gitlab.ListIssuesOptions{ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)}}
	if in.State != nil {
		opt.State = in.State
	}
	if in.Scope != nil {
		opt.Scope = in.Scope
	}
	if in.Search != nil {
		opt.Search = in.Search
	}
	issues, resp, err := d.Client.Issues.ListIssues(opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"issues": issues, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type myIssuesIn struct {
	Pagination
	State *string `json:"state,omitempty"`
}

func myIssues(ctx context.Context, _ *mcp.CallToolRequest, in myIssuesIn, d Deps) (*mcp.CallToolResult, any, error) {
	page, perPage := in.ListOpts()
	scope := "assigned_to_me"
	opt := &gitlab.ListIssuesOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
		Scope:       gitlab.Ptr(scope),
	}
	if in.State != nil {
		opt.State = in.State
	}
	issues, resp, err := d.Client.Issues.ListIssues(opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"issues": issues, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type listProjectIssuesIn struct {
	ProjectID string `json:"project_id"`
	Pagination
	State  *string `json:"state,omitempty"`
	Labels string  `json:"labels,omitempty"`
	Search *string `json:"search,omitempty"`
}

func listProjectIssues(ctx context.Context, _ *mcp.CallToolRequest, in listProjectIssuesIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := ResolveProjectID(in.ProjectID, d.Config.DefaultProjectID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	opt := &gitlab.ListProjectIssuesOptions{ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)}}
	if in.State != nil {
		opt.State = in.State
	}
	if strings.TrimSpace(in.Labels) != "" {
		parts := strings.Split(in.Labels, ",")
		var lo gitlab.LabelOptions
		for _, p := range parts {
			if t := strings.TrimSpace(p); t != "" {
				lo = append(lo, t)
			}
		}
		opt.Labels = &lo
	}
	if in.Search != nil {
		opt.Search = in.Search
	}
	issues, resp, err := d.Client.Issues.ListProjectIssues(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"issues": issues, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type getIssueIn struct {
	pidIssue
}

func getIssue(ctx context.Context, _ *mcp.CallToolRequest, in getIssueIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	iss, _, err := d.Client.Issues.GetIssue(pid, in.IssueIID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(iss), nil
}

type createIssueIn struct {
	ProjectID   string   `json:"project_id"`
	Title       string   `json:"title"`
	Description *string  `json:"description,omitempty"`
	Labels      []string `json:"labels,omitempty"`
}

func createIssue(ctx context.Context, _ *mcp.CallToolRequest, in createIssueIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := ResolveProjectID(in.ProjectID, d.Config.DefaultProjectID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return nil, nil, err
	}
	opt := &gitlab.CreateIssueOptions{Title: gitlab.Ptr(in.Title)}
	if in.Description != nil {
		opt.Description = in.Description
	}
	if len(in.Labels) > 0 {
		lo := gitlab.LabelOptions(in.Labels)
		opt.Labels = &lo
	}
	iss, _, err := d.Client.Issues.CreateIssue(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(iss), nil
}

type updateIssueIn struct {
	pidIssue
	Title       *string `json:"title,omitempty"`
	Description *string `json:"description,omitempty"`
	StateEvent  *string `json:"state_event,omitempty"`
}

func updateIssue(ctx context.Context, _ *mcp.CallToolRequest, in updateIssueIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	opt := &gitlab.UpdateIssueOptions{}
	if in.Title != nil {
		opt.Title = in.Title
	}
	if in.Description != nil {
		opt.Description = in.Description
	}
	if in.StateEvent != nil {
		opt.StateEvent = in.StateEvent
	}
	iss, _, err := d.Client.Issues.UpdateIssue(pid, in.IssueIID, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(iss), nil
}

type deleteIssueIn struct {
	pidIssue
}

func deleteIssue(ctx context.Context, _ *mcp.CallToolRequest, in deleteIssueIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	_, err = d.Client.Issues.DeleteIssue(pid, in.IssueIID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"deleted": true}), nil
}

type listIssueLinksIn struct {
	pidIssue
}

func listIssueLinks(ctx context.Context, _ *mcp.CallToolRequest, in listIssueLinksIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	rel, _, err := d.Client.IssueLinks.ListIssueRelations(pid, in.IssueIID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"relations": rel}), nil
}

type getIssueLinkIn struct {
	pidIssue
	IssueLinkID int64 `json:"issue_link_id"`
}

func getIssueLink(ctx context.Context, _ *mcp.CallToolRequest, in getIssueLinkIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	link, _, err := d.Client.IssueLinks.GetIssueLink(pid, in.IssueIID, in.IssueLinkID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(link), nil
}

type createIssueLinkIn struct {
	pidIssue
	TargetProjectID string  `json:"target_project_id"`
	TargetIssueIID  string  `json:"target_issue_iid"`
	LinkType        *string `json:"link_type,omitempty"`
}

func createIssueLink(ctx context.Context, _ *mcp.CallToolRequest, in createIssueLinkIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	opt := &gitlab.CreateIssueLinkOptions{
		TargetProjectID: gitlab.Ptr(in.TargetProjectID),
		TargetIssueIID:  gitlab.Ptr(in.TargetIssueIID),
	}
	if in.LinkType != nil {
		opt.LinkType = in.LinkType
	}
	link, _, err := d.Client.IssueLinks.CreateIssueLink(pid, in.IssueIID, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(link), nil
}

type deleteIssueLinkIn struct {
	pidIssue
	IssueLinkID int64 `json:"issue_link_id"`
}

func deleteIssueLink(ctx context.Context, _ *mcp.CallToolRequest, in deleteIssueLinkIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	link, _, err := d.Client.IssueLinks.DeleteIssueLink(pid, in.IssueIID, in.IssueLinkID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(link), nil
}
