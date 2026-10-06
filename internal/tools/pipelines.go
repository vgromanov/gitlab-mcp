package tools

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

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
	AddTool(s, d, false, "pipeline", &mcp.Tool{Name: "get_pipeline_job_output", Description: "Get the tail of a job trace/log: the last tail_lines lines (default 200) within max_bytes (default 65536, max 1048576), streamed so large traces stay memory-bounded. Returns trace, truncated, total_bytes. truncate_lines is deprecated: it is now an alias for tail_lines and keeps the last N lines (it used to keep the first N)."}, getPipelineJobOutput)
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
	TailLines     int    `json:"tail_lines,omitempty" jsonschema:"Last N lines to return (default 200)"`
	MaxBytes      int    `json:"max_bytes,omitempty" jsonschema:"Max trace bytes to return (default 65536, max 1048576)"`
	TruncateLines int    `json:"truncate_lines,omitempty" jsonschema:"Deprecated alias for tail_lines"`
}

const (
	defaultTraceTailLines = 200
	defaultTraceMaxBytes  = 64 << 10
	maxTraceMaxBytes      = 1 << 20
)

// tailBuffer is an io.Writer that keeps only the last max bytes written
// (memory stays below 2*max) and counts every byte it saw.
type tailBuffer struct {
	max   int
	buf   []byte
	total int64
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.total += int64(len(p))
	if len(p) >= t.max {
		t.buf = append(t.buf[:0], p[len(p)-t.max:]...)
		return len(p), nil
	}
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*t.max {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

// window returns the last max retained bytes.
func (t *tailBuffer) window() []byte {
	if len(t.buf) > t.max {
		return t.buf[len(t.buf)-t.max:]
	}
	return t.buf
}

// lastLines returns the last n lines of b (ignoring one trailing newline) and
// whether earlier lines were dropped.
func lastLines(b []byte, n int) ([]byte, bool) {
	end := len(b)
	if end > 0 && b[end-1] == '\n' {
		end--
	}
	for i := 0; i < n; i++ {
		j := bytes.LastIndexByte(b[:end], '\n')
		if j < 0 {
			return b, false
		}
		end = j
	}
	return b[end+1:], true
}

func getPipelineJobOutput(ctx context.Context, _ *mcp.CallToolRequest, in getPipelineJobOutputIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	lines, maxBytes := in.TailLines, in.MaxBytes
	if lines <= 0 {
		lines = in.TruncateLines
	}
	if lines <= 0 {
		lines = defaultTraceTailLines
	}
	if maxBytes <= 0 {
		maxBytes = defaultTraceMaxBytes
	}
	maxBytes = min(maxBytes, maxTraceMaxBytes)
	// Jobs.GetTraceFile buffers the whole body inside the client, so stream the
	// raw response into a bounded tail buffer instead.
	path := fmt.Sprintf("projects/%s/jobs/%d/trace", gitlab.PathEscape(pid), in.JobID)
	req, err := d.Client.NewRequest(http.MethodGet, path, nil, []gitlab.RequestOptionFunc{gitlab.WithContext(ctx)})
	if err != nil {
		return nil, nil, err
	}
	tb := &tailBuffer{max: maxBytes}
	if _, err := d.Client.Do(req, tb); err != nil {
		return nil, nil, err
	}
	b := tb.window()
	truncated := tb.total > int64(len(b))
	if i := bytes.IndexByte(b, '\n'); truncated && i >= 0 && i+1 < len(b) {
		b = b[i+1:] // the byte window may start mid-line
	}
	b, cut := lastLines(b, lines)
	return nil, Out(map[string]any{"trace": string(b), "truncated": truncated || cut, "total_bytes": tb.total}), nil
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
