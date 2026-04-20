package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// RegisterDeployments registers deployment and environment tools.
func RegisterDeployments(s *mcp.Server, d Deps) {
	AddTool(s, d, false, "pipeline", &mcp.Tool{Name: "list_deployments", Description: "List deployments in a project"}, listDeployments)
	AddTool(s, d, false, "pipeline", &mcp.Tool{Name: "get_deployment", Description: "Get a deployment by id"}, getDeployment)
	AddTool(s, d, false, "pipeline", &mcp.Tool{Name: "list_environments", Description: "List environments in a project"}, listEnvironments)
	AddTool(s, d, false, "pipeline", &mcp.Tool{Name: "get_environment", Description: "Get environment by id"}, getEnvironment)
}

type listDeploymentsIn struct {
	ProjectID string `json:"project_id"`
	Pagination
	Status      *string `json:"status,omitempty"`
	Environment *string `json:"environment,omitempty"`
}

func listDeployments(ctx context.Context, _ *mcp.CallToolRequest, in listDeploymentsIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	opt := &gitlab.ListProjectDeploymentsOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
		Status:      in.Status,
		Environment: in.Environment,
	}
	deps, resp, err := d.Client.Deployments.ListProjectDeployments(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"deployments": deps, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type getDeploymentIn struct {
	ProjectID    string `json:"project_id"`
	DeploymentID int64  `json:"deployment_id"`
}

func getDeployment(ctx context.Context, _ *mcp.CallToolRequest, in getDeploymentIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	dep, _, err := d.Client.Deployments.GetProjectDeployment(pid, in.DeploymentID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(dep), nil
}

type listEnvironmentsIn struct {
	ProjectID string `json:"project_id"`
	Pagination
	Search *string `json:"search,omitempty"`
}

func listEnvironments(ctx context.Context, _ *mcp.CallToolRequest, in listEnvironmentsIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	opt := &gitlab.ListEnvironmentsOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
		Search:      in.Search,
	}
	envs, resp, err := d.Client.Environments.ListEnvironments(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"environments": envs, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type getEnvironmentIn struct {
	ProjectID     string `json:"project_id"`
	EnvironmentID int64  `json:"environment_id"`
}

func getEnvironment(ctx context.Context, _ *mcp.CallToolRequest, in getEnvironmentIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	env, _, err := d.Client.Environments.GetEnvironment(pid, in.EnvironmentID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(env), nil
}
