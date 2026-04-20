package tools

import (
	"context"
	"encoding/base64"
	"io"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// RegisterArtifacts registers job artifact tools.
func RegisterArtifacts(s *mcp.Server, d Deps) {
	AddTool(s, d, false, "pipeline", &mcp.Tool{Name: "list_job_artifacts", Description: "Describe artifacts attached to a job (from job metadata)"}, listJobArtifacts)
	AddTool(s, d, false, "pipeline", &mcp.Tool{Name: "download_job_artifacts", Description: "Download job artifacts zip to a local path"}, downloadJobArtifacts)
	AddTool(s, d, false, "pipeline", &mcp.Tool{Name: "get_job_artifact_file", Description: "Download a single file from job artifacts"}, getJobArtifactFile)
}

type listJobArtifactsIn struct {
	ProjectID string `json:"project_id"`
	JobID     int64  `json:"job_id"`
}

func listJobArtifacts(ctx context.Context, _ *mcp.CallToolRequest, in listJobArtifactsIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	j, _, err := d.Client.Jobs.GetJob(pid, in.JobID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"job_id": j.ID, "artifacts": j.Artifacts, "artifacts_file": j.ArtifactsFile}), nil
}

type downloadJobArtifactsIn struct {
	ProjectID string `json:"project_id"`
	JobID     int64  `json:"job_id"`
	LocalPath string `json:"local_path" jsonschema:"Filesystem path to write zip"`
}

func downloadJobArtifacts(ctx context.Context, _ *mcp.CallToolRequest, in downloadJobArtifactsIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	r, _, err := d.Client.Jobs.GetJobArtifacts(pid, in.JobID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Create(in.LocalPath)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(f, r); err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"saved_to": in.LocalPath}), nil
}

type getJobArtifactFileIn struct {
	ProjectID    string `json:"project_id"`
	JobID        int64  `json:"job_id"`
	ArtifactPath string `json:"artifact_path" jsonschema:"Path inside the artifacts archive"`
	AsBase64     bool   `json:"as_base64,omitempty"`
}

func getJobArtifactFile(ctx context.Context, _ *mcp.CallToolRequest, in getJobArtifactFileIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	r, _, err := d.Client.Jobs.DownloadSingleArtifactsFile(pid, in.JobID, in.ArtifactPath, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, err
	}
	if in.AsBase64 {
		return nil, Out(map[string]any{"content_base64": base64.StdEncoding.EncodeToString(b)}), nil
	}
	return nil, Out(map[string]any{"content": string(b)}), nil
}
