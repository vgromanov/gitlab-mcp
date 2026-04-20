package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// RegisterIterationsEvents registers group iterations and event feeds.
func RegisterIterationsEvents(s *mcp.Server, d Deps) {
	AddTool(s, d, false, "", &mcp.Tool{Name: "list_group_iterations", Description: "List iterations for a group"}, listGroupIterations)
	AddTool(s, d, false, "", &mcp.Tool{Name: "list_events", Description: "List contribution events for the current user"}, listEvents)
	AddTool(s, d, false, "", &mcp.Tool{Name: "get_project_events", Description: "List visible events for a project"}, getProjectEvents)
}

type listGroupIterationsIn struct {
	GroupID string `json:"group_id"`
	Pagination
	State *string `json:"state,omitempty"`
}

func listGroupIterations(ctx context.Context, _ *mcp.CallToolRequest, in listGroupIterationsIn, d Deps) (*mcp.CallToolResult, any, error) {
	page, perPage := in.ListOpts()
	opt := &gitlab.ListGroupIterationsOptions{ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)}}
	if in.State != nil {
		opt.State = in.State
	}
	iters, resp, err := d.Client.GroupIterations.ListGroupIterations(in.GroupID, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"iterations": iters, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type listEventsIn struct {
	Pagination
	Action *string `json:"action,omitempty"`
}

func listEvents(ctx context.Context, _ *mcp.CallToolRequest, in listEventsIn, d Deps) (*mcp.CallToolResult, any, error) {
	page, perPage := in.ListOpts()
	opt := &gitlab.ListContributionEventsOptions{ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)}}
	if in.Action != nil {
		v := gitlab.EventTypeValue(*in.Action)
		opt.Action = &v
	}
	ev, resp, err := d.Client.Events.ListCurrentUserContributionEvents(opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"events": ev, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type getProjectEventsIn struct {
	ProjectID string `json:"project_id"`
	Pagination
	Action *string `json:"action,omitempty"`
}

func getProjectEvents(ctx context.Context, _ *mcp.CallToolRequest, in getProjectEventsIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	opt := &gitlab.ListProjectVisibleEventsOptions{ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)}}
	if in.Action != nil {
		v := gitlab.EventTypeValue(*in.Action)
		opt.Action = &v
	}
	ev, resp, err := d.Client.Events.ListProjectVisibleEvents(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"events": ev, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}
