package tools

import (
	"context"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// labelIDForAPI coerces tool input (always JSON string in schema) to the value GitLab REST expects.
func labelIDForAPI(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Pure decimal string → int id (GitLab accepts int or name string in path).
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && strconv.FormatInt(n, 10) == s {
		return n
	}
	return s
}

// RegisterLabels registers project label tools.
func RegisterLabels(s *mcp.Server, d Deps) {
	AddTool(s, d, false, "", &mcp.Tool{Name: "list_labels", Description: "List labels in a project"}, listLabels)
	AddTool(s, d, false, "", &mcp.Tool{Name: "get_label", Description: "Get a single project label"}, getLabel)
	AddTool(s, d, true, "", &mcp.Tool{Name: "create_label", Description: "Create a project label"}, createLabel)
	AddTool(s, d, true, "", &mcp.Tool{Name: "update_label", Description: "Update a project label"}, updateLabel)
	AddTool(s, d, true, "", &mcp.Tool{Name: "delete_label", Description: "Delete a project label"}, deleteLabel)
}

type listLabelsIn struct {
	ProjectID string `json:"project_id"`
	Pagination
	Search *string `json:"search,omitempty"`
}

func listLabels(ctx context.Context, _ *mcp.CallToolRequest, in listLabelsIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := ResolveProjectID(in.ProjectID, d.Config.DefaultProjectID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	opt := &gitlab.ListLabelsOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
		Search:      in.Search,
	}
	labels, resp, err := d.Client.Labels.ListLabels(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"labels": labels, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type getLabelIn struct {
	ProjectID string `json:"project_id"`
	LabelID   string `json:"label_id" jsonschema:"Label name, or numeric id as decimal digits only"`
}

func getLabel(ctx context.Context, _ *mcp.CallToolRequest, in getLabelIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := ResolveProjectID(in.ProjectID, d.Config.DefaultProjectID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return nil, nil, err
	}
	l, _, err := d.Client.Labels.GetLabel(pid, labelIDForAPI(in.LabelID), gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(l), nil
}

type createLabelIn struct {
	ProjectID   string  `json:"project_id"`
	Name        string  `json:"name"`
	Color       string  `json:"color" jsonschema:"#RRGGBB"`
	Description *string `json:"description,omitempty"`
}

func createLabel(ctx context.Context, _ *mcp.CallToolRequest, in createLabelIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := ResolveProjectID(in.ProjectID, d.Config.DefaultProjectID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return nil, nil, err
	}
	opt := &gitlab.CreateLabelOptions{Name: gitlab.Ptr(in.Name), Color: gitlab.Ptr(in.Color)}
	if in.Description != nil {
		opt.Description = in.Description
	}
	l, _, err := d.Client.Labels.CreateLabel(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(l), nil
}

type updateLabelIn struct {
	ProjectID   string  `json:"project_id"`
	LabelID     string  `json:"label_id" jsonschema:"Label name, or numeric id as decimal digits only"`
	NewName     *string `json:"new_name,omitempty"`
	Color       *string `json:"color,omitempty"`
	Description *string `json:"description,omitempty"`
}

func updateLabel(ctx context.Context, _ *mcp.CallToolRequest, in updateLabelIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := ResolveProjectID(in.ProjectID, d.Config.DefaultProjectID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return nil, nil, err
	}
	opt := &gitlab.UpdateLabelOptions{}
	if in.NewName != nil {
		opt.NewName = in.NewName
	}
	if in.Color != nil {
		opt.Color = in.Color
	}
	if in.Description != nil {
		opt.Description = in.Description
	}
	l, _, err := d.Client.Labels.UpdateLabel(pid, labelIDForAPI(in.LabelID), opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(l), nil
}

type deleteLabelIn struct {
	ProjectID string `json:"project_id"`
	LabelID   string `json:"label_id" jsonschema:"Label name, or numeric id as decimal digits only"`
}

func deleteLabel(ctx context.Context, _ *mcp.CallToolRequest, in deleteLabelIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := ResolveProjectID(in.ProjectID, d.Config.DefaultProjectID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return nil, nil, err
	}
	_, err = d.Client.Labels.DeleteLabel(pid, labelIDForAPI(in.LabelID), nil, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"deleted": true}), nil
}
