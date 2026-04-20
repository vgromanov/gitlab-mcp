package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// RegisterWiki registers project and group wiki tools.
func RegisterWiki(s *mcp.Server, d Deps) {
	AddTool(s, d, false, "wiki", &mcp.Tool{Name: "list_wiki_pages", Description: "List project wiki pages"}, listWikiPages)
	AddTool(s, d, false, "wiki", &mcp.Tool{Name: "get_wiki_page", Description: "Get a project wiki page"}, getWikiPage)
	AddTool(s, d, true, "wiki", &mcp.Tool{Name: "create_wiki_page", Description: "Create a project wiki page"}, createWikiPage)
	AddTool(s, d, true, "wiki", &mcp.Tool{Name: "update_wiki_page", Description: "Update a project wiki page"}, updateWikiPage)
	AddTool(s, d, true, "wiki", &mcp.Tool{Name: "delete_wiki_page", Description: "Delete a project wiki page"}, deleteWikiPage)
	AddTool(s, d, false, "wiki", &mcp.Tool{Name: "list_group_wiki_pages", Description: "List group wiki pages"}, listGroupWikiPages)
	AddTool(s, d, false, "wiki", &mcp.Tool{Name: "get_group_wiki_page", Description: "Get a group wiki page"}, getGroupWikiPage)
	AddTool(s, d, true, "wiki", &mcp.Tool{Name: "create_group_wiki_page", Description: "Create a group wiki page"}, createGroupWikiPage)
	AddTool(s, d, true, "wiki", &mcp.Tool{Name: "update_group_wiki_page", Description: "Update a group wiki page"}, updateGroupWikiPage)
	AddTool(s, d, true, "wiki", &mcp.Tool{Name: "delete_group_wiki_page", Description: "Delete a group wiki page"}, deleteGroupWikiPage)
}

type listWikiPagesIn struct {
	ProjectID   string `json:"project_id"`
	WithContent *bool  `json:"with_content,omitempty"`
}

func listWikiPages(ctx context.Context, _ *mcp.CallToolRequest, in listWikiPagesIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	opt := &gitlab.ListWikisOptions{}
	if in.WithContent != nil {
		opt.WithContent = in.WithContent
	}
	pages, _, err := d.Client.Wikis.ListWikis(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"pages": pages}), nil
}

type getWikiPageIn struct {
	ProjectID string  `json:"project_id"`
	Slug      string  `json:"slug"`
	Version   *string `json:"version,omitempty"`
}

func getWikiPage(ctx context.Context, _ *mcp.CallToolRequest, in getWikiPageIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	opt := &gitlab.GetWikiPageOptions{}
	if in.Version != nil {
		opt.Version = in.Version
	}
	w, _, err := d.Client.Wikis.GetWikiPage(pid, in.Slug, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(w), nil
}

type createWikiPageIn struct {
	ProjectID string  `json:"project_id"`
	Title     string  `json:"title"`
	Content   string  `json:"content"`
	Format    *string `json:"format,omitempty"`
}

func createWikiPage(ctx context.Context, _ *mcp.CallToolRequest, in createWikiPageIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	opt := &gitlab.CreateWikiPageOptions{
		Title:   gitlab.Ptr(in.Title),
		Content: gitlab.Ptr(in.Content),
	}
	if in.Format != nil {
		v := gitlab.WikiFormatValue(*in.Format)
		opt.Format = &v
	}
	w, _, err := d.Client.Wikis.CreateWikiPage(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(w), nil
}

type updateWikiPageIn struct {
	ProjectID string  `json:"project_id"`
	Slug      string  `json:"slug"`
	Title     *string `json:"title,omitempty"`
	Content   *string `json:"content,omitempty"`
	Format    *string `json:"format,omitempty"`
}

func updateWikiPage(ctx context.Context, _ *mcp.CallToolRequest, in updateWikiPageIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	opt := &gitlab.EditWikiPageOptions{}
	if in.Title != nil {
		opt.Title = in.Title
	}
	if in.Content != nil {
		opt.Content = in.Content
	}
	if in.Format != nil {
		v := gitlab.WikiFormatValue(*in.Format)
		opt.Format = &v
	}
	w, _, err := d.Client.Wikis.EditWikiPage(pid, in.Slug, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(w), nil
}

type deleteWikiPageIn struct {
	ProjectID string `json:"project_id"`
	Slug      string `json:"slug"`
}

func deleteWikiPage(ctx context.Context, _ *mcp.CallToolRequest, in deleteWikiPageIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	_, err = d.Client.Wikis.DeleteWikiPage(pid, in.Slug, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"deleted": true}), nil
}

type listGroupWikiPagesIn struct {
	GroupID     string `json:"group_id"`
	WithContent *bool  `json:"with_content,omitempty"`
}

func listGroupWikiPages(ctx context.Context, _ *mcp.CallToolRequest, in listGroupWikiPagesIn, d Deps) (*mcp.CallToolResult, any, error) {
	opt := &gitlab.ListGroupWikisOptions{}
	if in.WithContent != nil {
		opt.WithContent = in.WithContent
	}
	pages, _, err := d.Client.GroupWikis.ListGroupWikis(in.GroupID, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"pages": pages}), nil
}

type getGroupWikiPageIn struct {
	GroupID string  `json:"group_id"`
	Slug    string  `json:"slug"`
	Version *string `json:"version,omitempty"`
}

func getGroupWikiPage(ctx context.Context, _ *mcp.CallToolRequest, in getGroupWikiPageIn, d Deps) (*mcp.CallToolResult, any, error) {
	opt := &gitlab.GetGroupWikiPageOptions{}
	if in.Version != nil {
		opt.Version = in.Version
	}
	w, _, err := d.Client.GroupWikis.GetGroupWikiPage(in.GroupID, in.Slug, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(w), nil
}

type createGroupWikiPageIn struct {
	GroupID string  `json:"group_id"`
	Title   string  `json:"title"`
	Content string  `json:"content"`
	Format  *string `json:"format,omitempty"`
}

func createGroupWikiPage(ctx context.Context, _ *mcp.CallToolRequest, in createGroupWikiPageIn, d Deps) (*mcp.CallToolResult, any, error) {
	opt := &gitlab.CreateGroupWikiPageOptions{Title: gitlab.Ptr(in.Title), Content: gitlab.Ptr(in.Content)}
	if in.Format != nil {
		v := gitlab.WikiFormatValue(*in.Format)
		opt.Format = &v
	}
	w, _, err := d.Client.GroupWikis.CreateGroupWikiPage(in.GroupID, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(w), nil
}

type updateGroupWikiPageIn struct {
	GroupID string  `json:"group_id"`
	Slug    string  `json:"slug"`
	Title   *string `json:"title,omitempty"`
	Content *string `json:"content,omitempty"`
	Format  *string `json:"format,omitempty"`
}

func updateGroupWikiPage(ctx context.Context, _ *mcp.CallToolRequest, in updateGroupWikiPageIn, d Deps) (*mcp.CallToolResult, any, error) {
	opt := &gitlab.EditGroupWikiPageOptions{}
	if in.Title != nil {
		opt.Title = in.Title
	}
	if in.Content != nil {
		opt.Content = in.Content
	}
	if in.Format != nil {
		v := gitlab.WikiFormatValue(*in.Format)
		opt.Format = &v
	}
	w, _, err := d.Client.GroupWikis.EditGroupWikiPage(in.GroupID, in.Slug, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(w), nil
}

type deleteGroupWikiPageIn struct {
	GroupID string `json:"group_id"`
	Slug    string `json:"slug"`
}

func deleteGroupWikiPage(ctx context.Context, _ *mcp.CallToolRequest, in deleteGroupWikiPageIn, d Deps) (*mcp.CallToolResult, any, error) {
	_, err := d.Client.GroupWikis.DeleteGroupWikiPage(in.GroupID, in.Slug, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"deleted": true}), nil
}
