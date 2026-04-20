package tools

import (
	"context"
	"io"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// RegisterPipelines registers CI pipeline and job tools (gated by USE_PIPELINE).
func RegisterPipelines(s *mcp.Server, d Deps) {
	AddTool(s, d, false, "pipeline", &mcp.Tool{Name: "list_pipelines", Description: "List pipelines in a project"}, listPipelines)
	AddTool(s, d, false, "pipeline", &mcp.Tool{Name: "get_pipeline", Description: "Get a pipeline by id"}, getPipeline)
	AddTool(s, d, false, "pipeline", &mcp.Tool{Name: "list_pipeline_jobs", Description: "List jobs in a pipeline"}, listPipelineJobs)
	AddTool(s, d, false, "pipeline", &mcp.Tool{Name: "list_pipeline_trigger_jobs", Description: "List bridge/trigger jobs in a pipeline"}, listPipelineTriggerJobs)
	AddTool(s, d, false, "pipeline", &mcp.Tool{Name: "get_pipeline_job", Description: "Get a pipeline job"}, getPipelineJob)
	AddTool(s, d, false, "pipeline", &mcp.Tool{Name: "get_pipeline_job_output", Description: "Get job trace/log output"}, getPipelineJobOutput)
	AddTool(s, d, true, "pipeline", &mcp.Tool{Name: "create_pipeline", Description: "Create a pipeline for a ref"}, createPipeline)
	AddTool(s, d, true, "pipeline", &mcp.Tool{Name: "retry_pipeline", Description: "Retry failed/canceled jobs in a pipeline"}, retryPipeline)
	AddTool(s, d, true, "pipeline", &mcp.Tool{Name: "cancel_pipeline", Description: "Cancel a pipeline"}, cancelPipeline)
	AddTool(s, d, true, "pipeline", &mcp.Tool{Name: "play_pipeline_job", Description: "Run a manual job"}, playPipelineJob)
	AddTool(s, d, true, "pipeline", &mcp.Tool{Name: "retry_pipeline_job", Description: "Retry a single job"}, retryPipelineJob)
	AddTool(s, d, true, "pipeline", &mcp.Tool{Name: "cancel_pipeline_job", Description: "Cancel a running job"}, cancelPipelineJob)
}

func pidOnly(_ context.Context, projectID string, d Deps) (string, error) {
	pid, err := ResolveProjectID(projectID, d.Config.DefaultProjectID)
	if err != nil {
		return "", err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return "", err
	}
	return pid, nil
}

type listPipelinesIn struct {
	ProjectID string `json:"project_id"`
	Pagination
	Ref    *string `json:"ref,omitempty"`
	Status *string `json:"status,omitempty"`
}

func listPipelines(ctx context.Context, _ *mcp.CallToolRequest, in listPipelinesIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	opt := &gitlab.ListProjectPipelinesOptions{ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)}}
	if in.Ref != nil {
		opt.Ref = in.Ref
	}
	if in.Status != nil {
		v := gitlab.BuildStateValue(*in.Status)
		opt.Status = &v
	}
	pipes, resp, err := d.Client.Pipelines.ListProjectPipelines(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"pipelines": pipes, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type getPipelineIn struct {
	ProjectID  string `json:"project_id"`
	PipelineID int64  `json:"pipeline_id"`
}

func getPipeline(ctx context.Context, _ *mcp.CallToolRequest, in getPipelineIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	p, _, err := d.Client.Pipelines.GetPipeline(pid, in.PipelineID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(p), nil
}

type listPipelineJobsIn struct {
	ProjectID  string `json:"project_id"`
	PipelineID int64  `json:"pipeline_id"`
	Pagination
}

func listPipelineJobs(ctx context.Context, _ *mcp.CallToolRequest, in listPipelineJobsIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	jobs, resp, err := d.Client.Jobs.ListPipelineJobs(pid, in.PipelineID, &gitlab.ListJobsOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"jobs": jobs, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type listPipelineTriggerJobsIn struct {
	ProjectID  string `json:"project_id"`
	PipelineID int64  `json:"pipeline_id"`
	Pagination
}

func listPipelineTriggerJobs(ctx context.Context, _ *mcp.CallToolRequest, in listPipelineTriggerJobsIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	bridges, resp, err := d.Client.Jobs.ListPipelineBridges(pid, in.PipelineID, &gitlab.ListJobsOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"bridges": bridges, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type getPipelineJobIn struct {
	ProjectID string `json:"project_id"`
	JobID     int64  `json:"job_id"`
}

func getPipelineJob(ctx context.Context, _ *mcp.CallToolRequest, in getPipelineJobIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	j, _, err := d.Client.Jobs.GetJob(pid, in.JobID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(j), nil
}

type getPipelineJobOutputIn struct {
	ProjectID     string `json:"project_id"`
	JobID         int64  `json:"job_id"`
	TruncateLines int    `json:"truncate_lines,omitempty"`
}

func getPipelineJobOutput(ctx context.Context, _ *mcp.CallToolRequest, in getPipelineJobOutputIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	r, _, err := d.Client.Jobs.GetTraceFile(pid, in.JobID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, err
	}
	out := string(b)
	if in.TruncateLines > 0 {
		out = TruncateLines(out, in.TruncateLines)
	}
	return nil, Out(map[string]any{"trace": out}), nil
}

type createPipelineIn struct {
	ProjectID string            `json:"project_id"`
	Ref       string            `json:"ref"`
	Variables map[string]string `json:"variables,omitempty"`
}

func createPipeline(ctx context.Context, _ *mcp.CallToolRequest, in createPipelineIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	opt := &gitlab.CreatePipelineOptions{Ref: gitlab.Ptr(in.Ref)}
	if len(in.Variables) > 0 {
		var vars []*gitlab.PipelineVariableOptions
		for k, v := range in.Variables {
			kk, vv := k, v
			vars = append(vars, &gitlab.PipelineVariableOptions{Key: gitlab.Ptr(kk), Value: gitlab.Ptr(vv)})
		}
		opt.Variables = &vars
	}
	p, _, err := d.Client.Pipelines.CreatePipeline(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(p), nil
}

type retryPipelineIn struct {
	ProjectID  string `json:"project_id"`
	PipelineID int64  `json:"pipeline_id"`
}

func retryPipeline(ctx context.Context, _ *mcp.CallToolRequest, in retryPipelineIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	p, _, err := d.Client.Pipelines.RetryPipelineBuild(pid, in.PipelineID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(p), nil
}

type cancelPipelineIn struct {
	ProjectID  string `json:"project_id"`
	PipelineID int64  `json:"pipeline_id"`
}

func cancelPipeline(ctx context.Context, _ *mcp.CallToolRequest, in cancelPipelineIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	p, _, err := d.Client.Pipelines.CancelPipelineBuild(pid, in.PipelineID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(p), nil
}

type playPipelineJobIn struct {
	ProjectID string `json:"project_id"`
	JobID     int64  `json:"job_id"`
}

func playPipelineJob(ctx context.Context, _ *mcp.CallToolRequest, in playPipelineJobIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	j, _, err := d.Client.Jobs.PlayJob(pid, in.JobID, nil, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(j), nil
}

type retryPipelineJobIn struct {
	ProjectID string `json:"project_id"`
	JobID     int64  `json:"job_id"`
}

func retryPipelineJob(ctx context.Context, _ *mcp.CallToolRequest, in retryPipelineJobIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	j, _, err := d.Client.Jobs.RetryJob(pid, in.JobID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(j), nil
}

type cancelPipelineJobIn struct {
	ProjectID string `json:"project_id"`
	JobID     int64  `json:"job_id"`
}

func cancelPipelineJob(ctx context.Context, _ *mcp.CallToolRequest, in cancelPipelineJobIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	j, _, err := d.Client.Jobs.CancelJob(pid, in.JobID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(j), nil
}
