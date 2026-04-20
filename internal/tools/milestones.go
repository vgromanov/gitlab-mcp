package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// RegisterMilestones registers milestone tools (gated by USE_MILESTONE).
func RegisterMilestones(s *mcp.Server, d Deps) {
	AddTool(s, d, false, "milestone", &mcp.Tool{Name: "list_milestones", Description: "List milestones in a project"}, listMilestones)
	AddTool(s, d, false, "milestone", &mcp.Tool{Name: "get_milestone", Description: "Get a milestone"}, getMilestone)
	AddTool(s, d, true, "milestone", &mcp.Tool{Name: "create_milestone", Description: "Create a milestone"}, createMilestone)
	AddTool(s, d, true, "milestone", &mcp.Tool{Name: "edit_milestone", Description: "Update a milestone"}, editMilestone)
	AddTool(s, d, true, "milestone", &mcp.Tool{Name: "delete_milestone", Description: "Delete a milestone"}, deleteMilestone)
	AddTool(s, d, false, "milestone", &mcp.Tool{Name: "get_milestone_issue", Description: "List issues assigned to a milestone"}, getMilestoneIssues)
	AddTool(s, d, false, "milestone", &mcp.Tool{Name: "get_milestone_merge_requests", Description: "List MRs assigned to a milestone"}, getMilestoneMergeRequests)
	AddTool(s, d, true, "milestone", &mcp.Tool{Name: "promote_milestone", Description: "Promote milestone to parent group (Premium)"}, promoteMilestone)
	AddTool(s, d, false, "milestone", &mcp.Tool{Name: "get_milestone_burndown_events", Description: "Burndown chart events (group milestones only in this build)"}, getMilestoneBurndownEvents)
}

type listMilestonesIn struct {
	ProjectID string `json:"project_id"`
	Pagination
	State *string `json:"state,omitempty"`
}

func listMilestones(ctx context.Context, _ *mcp.CallToolRequest, in listMilestonesIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	opt := &gitlab.ListMilestonesOptions{ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)}}
	if in.State != nil {
		opt.State = in.State
	}
	ms, resp, err := d.Client.Milestones.ListMilestones(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"milestones": ms, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type getMilestoneIn struct {
	ProjectID   string `json:"project_id"`
	MilestoneID int64  `json:"milestone_id"`
}

func getMilestone(ctx context.Context, _ *mcp.CallToolRequest, in getMilestoneIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	m, _, err := d.Client.Milestones.GetMilestone(pid, in.MilestoneID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(m), nil
}

type createMilestoneIn struct {
	ProjectID   string  `json:"project_id"`
	Title       string  `json:"title"`
	Description *string `json:"description,omitempty"`
	DueDate     *string `json:"due_date,omitempty"`
}

func createMilestone(ctx context.Context, _ *mcp.CallToolRequest, in createMilestoneIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	opt := &gitlab.CreateMilestoneOptions{Title: gitlab.Ptr(in.Title)}
	if in.Description != nil {
		opt.Description = in.Description
	}
	if in.DueDate != nil {
		s := strings.TrimSpace(*in.DueDate)
		if s != "" {
			dt, err := gitlab.ParseISOTime(s)
			if err != nil {
				return nil, nil, fmt.Errorf("due_date: %w", err)
			}
			t := dt
			opt.DueDate = &t
		}
	}
	m, _, err := d.Client.Milestones.CreateMilestone(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(m), nil
}

type editMilestoneIn struct {
	ProjectID   string  `json:"project_id"`
	MilestoneID int64   `json:"milestone_id"`
	Title       *string `json:"title,omitempty"`
	Description *string `json:"description,omitempty"`
	DueDate     *string `json:"due_date,omitempty"`
	StateEvent  *string `json:"state_event,omitempty"`
}

func editMilestone(ctx context.Context, _ *mcp.CallToolRequest, in editMilestoneIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	opt := &gitlab.UpdateMilestoneOptions{}
	if in.Title != nil {
		opt.Title = in.Title
	}
	if in.Description != nil {
		opt.Description = in.Description
	}
	if in.DueDate != nil {
		s := strings.TrimSpace(*in.DueDate)
		if s != "" {
			dt, err := gitlab.ParseISOTime(s)
			if err != nil {
				return nil, nil, fmt.Errorf("due_date: %w", err)
			}
			t := dt
			opt.DueDate = &t
		}
	}
	if in.StateEvent != nil {
		opt.StateEvent = in.StateEvent
	}
	m, _, err := d.Client.Milestones.UpdateMilestone(pid, in.MilestoneID, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(m), nil
}

type deleteMilestoneIn struct {
	ProjectID   string `json:"project_id"`
	MilestoneID int64  `json:"milestone_id"`
}

func deleteMilestone(ctx context.Context, _ *mcp.CallToolRequest, in deleteMilestoneIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	_, err = d.Client.Milestones.DeleteMilestone(pid, in.MilestoneID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"deleted": true}), nil
}

type getMilestoneIssuesIn struct {
	ProjectID   string `json:"project_id"`
	MilestoneID int64  `json:"milestone_id"`
	Pagination
}

func getMilestoneIssues(ctx context.Context, _ *mcp.CallToolRequest, in getMilestoneIssuesIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	issues, resp, err := d.Client.Milestones.GetMilestoneIssues(pid, in.MilestoneID, &gitlab.GetMilestoneIssuesOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"issues": issues, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type getMilestoneMergeRequestsIn struct {
	ProjectID   string `json:"project_id"`
	MilestoneID int64  `json:"milestone_id"`
	Pagination
}

func getMilestoneMergeRequests(ctx context.Context, _ *mcp.CallToolRequest, in getMilestoneMergeRequestsIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	mrs, resp, err := d.Client.Milestones.GetMilestoneMergeRequests(pid, in.MilestoneID, &gitlab.GetMilestoneMergeRequestsOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"merge_requests": mrs, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type promoteMilestoneIn struct {
	ProjectID   string `json:"project_id"`
	MilestoneID int64  `json:"milestone_id"`
}

func promoteMilestone(ctx context.Context, _ *mcp.CallToolRequest, _ promoteMilestoneIn, d Deps) (*mcp.CallToolResult, any, error) {
	_ = ctx
	_ = d
	return nil, nil, fmt.Errorf("promote_milestone is not exposed in client-go v2; use execute_graphql with the milestonePromote mutation for your GitLab version")
}

type getMilestoneBurndownEventsIn struct {
	GroupID     string `json:"group_id" jsonschema:"Required for burndown chart API"`
	MilestoneID int64  `json:"milestone_id"`
	Pagination
}

func getMilestoneBurndownEvents(ctx context.Context, _ *mcp.CallToolRequest, in getMilestoneBurndownEventsIn, d Deps) (*mcp.CallToolResult, any, error) {
	if in.GroupID == "" {
		return nil, nil, fmt.Errorf("group_id is required (project milestone burndown uses group milestone API)")
	}
	page, perPage := in.ListOpts()
	ev, resp, err := d.Client.GroupMilestones.GetGroupMilestoneBurndownChartEvents(in.GroupID, in.MilestoneID, &gitlab.GetGroupMilestoneBurndownChartEventsOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"events": ev, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}
