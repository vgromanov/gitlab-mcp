package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// RegisterProjects registers project/namespace/user tools.
func RegisterProjects(s *mcp.Server, d Deps) {
	AddTool(s, d, false, "", &mcp.Tool{
		Name:        "list_projects",
		Description: "List projects accessible by the authenticated user",
	}, listProjects)
	AddTool(s, d, false, "", &mcp.Tool{
		Name:        "get_project",
		Description: "Get details of a GitLab project",
	}, getProject)
	AddTool(s, d, false, "", &mcp.Tool{
		Name:        "list_project_members",
		Description: "List members of a GitLab project",
	}, listProjectMembers)
	AddTool(s, d, false, "", &mcp.Tool{
		Name:        "list_group_projects",
		Description: "List projects in a GitLab group",
	}, listGroupProjects)
	AddTool(s, d, false, "", &mcp.Tool{
		Name:        "list_namespaces",
		Description: "List namespaces available to the current user",
	}, listNamespaces)
	AddTool(s, d, false, "", &mcp.Tool{
		Name:        "get_namespace",
		Description: "Get namespace details by id or path",
	}, getNamespace)
	AddTool(s, d, false, "", &mcp.Tool{
		Name:        "verify_namespace",
		Description: "Verify namespace path availability (exists + suggests)",
	}, verifyNamespace)
	AddTool(s, d, false, "", &mcp.Tool{
		Name:        "get_users",
		Description: "Resolve GitLab users by username (batch)",
	}, getUsers)
}

type listProjectsIn struct {
	Pagination
	Search   *string `json:"search,omitempty" jsonschema:"Search filter"`
	Owned    *bool   `json:"owned,omitempty" jsonschema:"Limit to owned projects"`
	Starred  *bool   `json:"starred,omitempty" jsonschema:"Starred only"`
	Archived *bool   `json:"archived,omitempty" jsonschema:"Archived filter"`
}

func listProjects(ctx context.Context, _ *mcp.CallToolRequest, in listProjectsIn, d Deps) (*mcp.CallToolResult, any, error) {
	page, perPage := in.ListOpts()
	opt := &gitlab.ListProjectsOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
	}
	if in.Search != nil {
		opt.Search = gitlab.Ptr(*in.Search)
	}
	if in.Owned != nil {
		opt.Owned = gitlab.Ptr(*in.Owned)
	}
	if in.Starred != nil {
		opt.Starred = gitlab.Ptr(*in.Starred)
	}
	if in.Archived != nil {
		opt.Archived = gitlab.Ptr(*in.Archived)
	}
	projects, resp, err := d.Client.Projects.ListProjects(opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	out, err := ToJSONTree(map[string]any{
		"projects":   projects,
		"pagination": map[string]any{"page": page, "per_page": perPage, "next_page": resp.NextPage},
	})
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

type getProjectIn struct {
	ProjectID string `json:"project_id" jsonschema:"Numeric id or URL-encoded path"`
}

func getProject(ctx context.Context, _ *mcp.CallToolRequest, in getProjectIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := ResolveProjectID(in.ProjectID, d.Config.DefaultProjectID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return nil, nil, err
	}
	p, _, err := d.Client.Projects.GetProject(pid, nil, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(p), nil
}

type listProjectMembersIn struct {
	ProjectID string `json:"project_id" jsonschema:"Project id or path"`
	Pagination
	Query *string `json:"query,omitempty" jsonschema:"Search query"`
}

func listProjectMembers(ctx context.Context, _ *mcp.CallToolRequest, in listProjectMembersIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := ResolveProjectID(in.ProjectID, d.Config.DefaultProjectID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	opt := &gitlab.ListProjectMembersOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
	}
	if in.Query != nil {
		opt.Query = gitlab.Ptr(*in.Query)
	}
	members, resp, err := d.Client.ProjectMembers.ListProjectMembers(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	out, err := ToJSONTree(map[string]any{
		"members":    members,
		"pagination": map[string]any{"page": page, "per_page": perPage, "next_page": resp.NextPage},
	})
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

type listGroupProjectsIn struct {
	GroupID string `json:"group_id" jsonschema:"Group id or URL-encoded path"`
	Pagination
	IncludeSubGroups *bool   `json:"include_subgroups,omitempty"`
	Search           *string `json:"search,omitempty"`
	Archived         *bool   `json:"archived,omitempty"`
}

func listGroupProjects(ctx context.Context, _ *mcp.CallToolRequest, in listGroupProjectsIn, d Deps) (*mcp.CallToolResult, any, error) {
	page, perPage := in.ListOpts()
	opt := &gitlab.ListGroupProjectsOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
	}
	if in.IncludeSubGroups != nil {
		opt.IncludeSubGroups = gitlab.Ptr(*in.IncludeSubGroups)
	}
	if in.Search != nil {
		opt.Search = gitlab.Ptr(*in.Search)
	}
	if in.Archived != nil {
		opt.Archived = gitlab.Ptr(*in.Archived)
	}
	projects, resp, err := d.Client.Groups.ListGroupProjects(in.GroupID, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	out, err := ToJSONTree(map[string]any{
		"projects":   projects,
		"pagination": map[string]any{"page": page, "per_page": perPage, "next_page": resp.NextPage},
	})
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

type listNamespacesIn struct {
	Pagination
	Search *string `json:"search,omitempty"`
}

func listNamespaces(ctx context.Context, _ *mcp.CallToolRequest, in listNamespacesIn, d Deps) (*mcp.CallToolResult, any, error) {
	page, perPage := in.ListOpts()
	opt := &gitlab.ListNamespacesOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
	}
	if in.Search != nil {
		opt.Search = gitlab.Ptr(*in.Search)
	}
	ns, resp, err := d.Client.Namespaces.ListNamespaces(opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	out, err := ToJSONTree(map[string]any{
		"namespaces": ns,
		"pagination": map[string]any{"page": page, "per_page": perPage, "next_page": resp.NextPage},
	})
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

type getNamespaceIn struct {
	NamespaceID string `json:"namespace_id" jsonschema:"Namespace id or path"`
}

func getNamespace(ctx context.Context, _ *mcp.CallToolRequest, in getNamespaceIn, d Deps) (*mcp.CallToolResult, any, error) {
	ns, _, err := d.Client.Namespaces.GetNamespace(in.NamespaceID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(ns), nil
}

type verifyNamespaceIn struct {
	Path     string `json:"path" jsonschema:"Namespace path to verify"`
	ParentID *int64 `json:"parent_id,omitempty"`
}

func verifyNamespace(ctx context.Context, _ *mcp.CallToolRequest, in verifyNamespaceIn, d Deps) (*mcp.CallToolResult, any, error) {
	opt := &gitlab.NamespaceExistsOptions{}
	if in.ParentID != nil {
		opt.ParentID = in.ParentID
	}
	ex, _, err := d.Client.Namespaces.NamespaceExists(in.Path, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(ex), nil
}

type getUsersIn struct {
	Usernames []string `json:"usernames" jsonschema:"GitLab usernames to resolve"`
}

func getUsers(ctx context.Context, _ *mcp.CallToolRequest, in getUsersIn, d Deps) (*mcp.CallToolResult, any, error) {
	var users []*gitlab.User
	for _, u := range in.Usernames {
		u = trim(u)
		if u == "" {
			continue
		}
		list, _, err := d.Client.Users.ListUsers(&gitlab.ListUsersOptions{
			Username:    gitlab.Ptr(u),
			ListOptions: gitlab.ListOptions{PerPage: 5},
		}, gitlab.WithContext(ctx))
		if err != nil {
			return nil, nil, err
		}
		for _, user := range list {
			if user != nil && user.Username == u {
				users = append(users, user)
				break
			}
		}
	}
	return nil, Out(map[string]any{"users": users}), nil
}

func trim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	return s
}
