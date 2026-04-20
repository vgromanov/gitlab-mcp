package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// RegisterSearch registers code search tools (requires GitLab code search).
func RegisterSearch(s *mcp.Server, d Deps) {
	AddTool(s, d, false, "", &mcp.Tool{Name: "search_code", Description: "Search code across the GitLab instance"}, searchCode)
	AddTool(s, d, false, "", &mcp.Tool{Name: "search_project_code", Description: "Search code within a project"}, searchProjectCode)
	AddTool(s, d, false, "", &mcp.Tool{Name: "search_group_code", Description: "Search code within a group"}, searchGroupCode)
}

type searchCodeIn struct {
	Query string `json:"query"`
	Pagination
}

func searchCode(ctx context.Context, _ *mcp.CallToolRequest, in searchCodeIn, d Deps) (*mcp.CallToolResult, any, error) {
	page, perPage := in.ListOpts()
	opt := &gitlab.SearchOptions{ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)}}
	blobs, resp, err := d.Client.Search.Blobs(in.Query, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"blobs": blobs, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type searchProjectCodeIn struct {
	ProjectID string `json:"project_id"`
	Query     string `json:"query"`
	Pagination
}

func searchProjectCode(ctx context.Context, _ *mcp.CallToolRequest, in searchProjectCodeIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	opt := &gitlab.SearchOptions{ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)}}
	blobs, resp, err := d.Client.Search.BlobsByProject(pid, in.Query, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"blobs": blobs, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type searchGroupCodeIn struct {
	GroupID string `json:"group_id"`
	Query   string `json:"query"`
	Pagination
}

func searchGroupCode(ctx context.Context, _ *mcp.CallToolRequest, in searchGroupCodeIn, d Deps) (*mcp.CallToolResult, any, error) {
	page, perPage := in.ListOpts()
	opt := &gitlab.SearchOptions{ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)}}
	blobs, resp, err := d.Client.Search.BlobsByGroup(in.GroupID, in.Query, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"blobs": blobs, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}
