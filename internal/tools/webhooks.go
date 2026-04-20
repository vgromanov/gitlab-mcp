package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// RegisterWebhooks registers webhook listing tools.
func RegisterWebhooks(s *mcp.Server, d Deps) {
	AddTool(s, d, false, "", &mcp.Tool{Name: "list_webhooks", Description: "List webhooks for a project or group"}, listWebhooks)
	AddTool(s, d, false, "", &mcp.Tool{Name: "list_webhook_events", Description: "List recent webhook deliveries (not available in client-go; use GitLab UI or REST)"}, listWebhookEvents)
	AddTool(s, d, false, "", &mcp.Tool{Name: "get_webhook_event", Description: "Get webhook delivery details (not available in client-go; use REST)"}, getWebhookEvent)
}

type listWebhooksIn struct {
	ProjectID *string `json:"project_id,omitempty"`
	GroupID   *string `json:"group_id,omitempty"`
	Pagination
}

func listWebhooks(ctx context.Context, _ *mcp.CallToolRequest, in listWebhooksIn, d Deps) (*mcp.CallToolResult, any, error) {
	page, perPage := in.ListOpts()
	if in.ProjectID != nil && *in.ProjectID != "" {
		pid, err := pidOnly(ctx, *in.ProjectID, d)
		if err != nil {
			return nil, nil, err
		}
		hooks, resp, err := d.Client.Projects.ListProjectHooks(pid, &gitlab.ListProjectHooksOptions{
			ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
		}, gitlab.WithContext(ctx))
		if err != nil {
			return nil, nil, err
		}
		return nil, Out(map[string]any{"hooks": hooks, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
	}
	if in.GroupID != nil && *in.GroupID != "" {
		hooks, resp, err := d.Client.Groups.ListGroupHooks(*in.GroupID, &gitlab.ListGroupHooksOptions{
			ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
		}, gitlab.WithContext(ctx))
		if err != nil {
			return nil, nil, err
		}
		return nil, Out(map[string]any{"hooks": hooks, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
	}
	return nil, nil, fmt.Errorf("provide project_id or group_id")
}

type listWebhookEventsIn struct {
	ProjectID string `json:"project_id"`
	HookID    int64  `json:"hook_id"`
}

func listWebhookEvents(ctx context.Context, _ *mcp.CallToolRequest, in listWebhookEventsIn, d Deps) (*mcp.CallToolResult, any, error) {
	_ = ctx
	_ = in
	_ = d
	return nil, nil, fmt.Errorf("list_webhook_events: use GitLab REST GET /projects/:id/hooks/:hook_id/events or execute_graphql; not wrapped in client-go v2.20")
}

type getWebhookEventIn struct {
	ProjectID string `json:"project_id"`
	HookID    int64  `json:"hook_id"`
	EventID   int64  `json:"event_id"`
}

func getWebhookEvent(ctx context.Context, _ *mcp.CallToolRequest, in getWebhookEventIn, d Deps) (*mcp.CallToolResult, any, error) {
	_ = ctx
	_ = in
	_ = d
	return nil, nil, fmt.Errorf("get_webhook_event: use GitLab REST or execute_graphql; not wrapped in client-go v2.20")
}
