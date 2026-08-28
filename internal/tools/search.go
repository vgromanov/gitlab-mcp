package tools

import (
	"context"
	"fmt"
	"strings"

	glclient "github.com/vgromanov/gitlab-mcp/internal/gitlab"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

const searchCodeToolDesc = `Search code (blob FTS) across the GitLab instance via Search API scope=blobs (Zoekt when exact code search is enabled). ` +
	`Optional search_type: basic|advanced|zoekt. Optional ref pins a branch/tag. ` +
	`Query may include filters e.g. filename:*.go path:internal/ extension:go.`

const searchProjectCodeToolDesc = `Search code (blob FTS) within a project via Search API scope=blobs (Zoekt when exact code search is enabled). ` +
	`Optional search_type: basic|advanced|zoekt. Optional ref pins a branch/tag. ` +
	`Query may include filters e.g. filename:*.go path:internal/ extension:go.`

const searchGroupCodeToolDesc = `Search code (blob FTS) within a group via Search API scope=blobs (Zoekt when exact code search is enabled). ` +
	`Optional search_type: basic|advanced|zoekt. Optional ref pins a branch/tag. ` +
	`Query may include filters e.g. filename:*.go path:internal/ extension:go.`

// RegisterSearch registers code search tools (requires GitLab code search).
func RegisterSearch(s *mcp.Server, d Deps) {
	AddTool(s, d, false, "", &mcp.Tool{Name: "search_code", Description: searchCodeToolDesc}, searchCode)
	AddTool(s, d, false, "", &mcp.Tool{Name: "search_project_code", Description: searchProjectCodeToolDesc}, searchProjectCode)
	AddTool(s, d, false, "", &mcp.Tool{Name: "search_group_code", Description: searchGroupCodeToolDesc}, searchGroupCode)
}

type searchCodeIn struct {
	Query      string  `json:"query" jsonschema:"Search expression; may include filename:/path:/extension: filters"`
	Ref        *string `json:"ref,omitempty" jsonschema:"Branch or tag to search (Search API ref)"`
	SearchType *string `json:"search_type,omitempty" jsonschema:"Search backend: basic, advanced, or zoekt"`
	Pagination
}

func searchCode(ctx context.Context, _ *mcp.CallToolRequest, in searchCodeIn, d Deps) (*mcp.CallToolResult, any, error) {
	opt, reqOpts, err := blobSearchOpts(ctx, in.Ref, in.SearchType, in.Pagination)
	if err != nil {
		return nil, nil, err
	}
	blobs, resp, err := d.Client.Search.Blobs(in.Query, opt, reqOpts...)
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"blobs": blobs, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type searchProjectCodeIn struct {
	ProjectID  string  `json:"project_id" jsonschema:"Project id or URL-encoded path"`
	Query      string  `json:"query" jsonschema:"Search expression; may include filename:/path:/extension: filters"`
	Ref        *string `json:"ref,omitempty" jsonschema:"Branch or tag to search (Search API ref)"`
	SearchType *string `json:"search_type,omitempty" jsonschema:"Search backend: basic, advanced, or zoekt"`
	Pagination
}

func searchProjectCode(ctx context.Context, _ *mcp.CallToolRequest, in searchProjectCodeIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	opt, reqOpts, err := blobSearchOpts(ctx, in.Ref, in.SearchType, in.Pagination)
	if err != nil {
		return nil, nil, err
	}
	blobs, resp, err := d.Client.Search.BlobsByProject(pid, in.Query, opt, reqOpts...)
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"blobs": blobs, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type searchGroupCodeIn struct {
	GroupID    string  `json:"group_id" jsonschema:"Group id or URL-encoded path"`
	Query      string  `json:"query" jsonschema:"Search expression; may include filename:/path:/extension: filters"`
	Ref        *string `json:"ref,omitempty" jsonschema:"Branch or tag to search (Search API ref)"`
	SearchType *string `json:"search_type,omitempty" jsonschema:"Search backend: basic, advanced, or zoekt"`
	Pagination
}

func searchGroupCode(ctx context.Context, _ *mcp.CallToolRequest, in searchGroupCodeIn, d Deps) (*mcp.CallToolResult, any, error) {
	opt, reqOpts, err := blobSearchOpts(ctx, in.Ref, in.SearchType, in.Pagination)
	if err != nil {
		return nil, nil, err
	}
	blobs, resp, err := d.Client.Search.BlobsByGroup(in.GroupID, in.Query, opt, reqOpts...)
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"blobs": blobs, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

func blobSearchOpts(ctx context.Context, ref, searchType *string, p Pagination) (*gitlab.SearchOptions, []gitlab.RequestOptionFunc, error) {
	page, perPage := p.ListOpts()
	opt := &gitlab.SearchOptions{ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)}}
	if ref != nil {
		if r := strings.TrimSpace(*ref); r != "" {
			opt.Ref = gitlab.Ptr(r)
		}
	}
	reqOpts := []gitlab.RequestOptionFunc{gitlab.WithContext(ctx)}
	if searchType != nil {
		st, err := glclient.NormalizeSearchType(*searchType)
		if err != nil {
			return nil, nil, fmt.Errorf("%w", err)
		}
		if st != "" {
			reqOpts = append(reqOpts, glclient.WithSearchType(st))
		}
	}
	return opt, reqOpts, nil
}
