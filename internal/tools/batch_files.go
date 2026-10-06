package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

const (
	batchFilesMaxPaths     = 20
	batchFilesBytesDefault = 32 << 10
	batchFilesBytesLimit   = 256 << 10
	batchFilesBinarySniff  = 8000 // git's own binary heuristic looks at the first 8000 bytes
)

var fullSHARe = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

type batchGetFileContentsIn struct {
	ProjectID       any      `json:"project_id,omitempty" jsonschema:"Project id (number or string) or path; defaults to GITLAB_PROJECT_ID"`
	SHA             string   `json:"sha" jsonschema:"Full 40-character commit SHA; branch and tag names are rejected"`
	Paths           []string `json:"paths" jsonschema:"1 to 20 file paths"`
	MaxBytesPerFile int      `json:"max_bytes_per_file,omitempty" jsonschema:"Content bytes returned per file (default 32768, max 262144, clamped); longer files are cut and flagged truncated"`
}

// batchGetFileContents reads up to 20 files at one exact commit, one sequential
// request per path. A problem with one path is that file's error entry; only
// input problems (sha, path count, project) fail the call, before any request.
func batchGetFileContents(ctx context.Context, _ *mcp.CallToolRequest, in batchGetFileContentsIn, d Deps) (*mcp.CallToolResult, any, error) {
	sha := strings.TrimSpace(in.SHA)
	if !fullSHARe.MatchString(sha) {
		return nil, nil, errInvalid(fmt.Sprintf("invalid sha %q: a full 40-character commit SHA is required (branch and tag names are rejected)", in.SHA))
	}
	if n := len(in.Paths); n < 1 || n > batchFilesMaxPaths {
		return nil, nil, errInvalid(fmt.Sprintf("paths must hold 1 to %d paths, got %d", batchFilesMaxPaths, n))
	}
	pid, err := snapshotProjectID(in.ProjectID, d.Config.DefaultProjectID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return nil, nil, err
	}
	limit := in.MaxBytesPerFile
	if limit < 1 {
		limit = batchFilesBytesDefault
	}
	limit, sha = min(limit, batchFilesBytesLimit), strings.ToLower(sha)

	files := make([]map[string]any, 0, len(in.Paths))
	for _, p := range in.Paths {
		files = append(files, readBatchFile(ctx, d, pid, sha, p, limit))
	}
	return nil, Out(map[string]any{"project_id": pid, "sha": sha, "max_bytes_per_file": limit, "files": files}), nil
}

// readBatchFile returns {path, blob_id, size, binary, truncated, content?} or {path, error}.
// size is the full blob size, so size > len(content) shows what a cut left out.
// A binary file (NUL in the first 8000 bytes, or not valid UTF-8) keeps blob_id
// and size but returns no content.
func readBatchFile(ctx context.Context, d Deps, pid, sha, path string, limit int) map[string]any {
	out := map[string]any{"path": path}
	if strings.TrimSpace(path) == "" {
		out["error"] = "path is empty"
		return out
	}
	f, _, err := d.Client.RepositoryFiles.GetFile(pid, path, &gitlab.GetFileOptions{Ref: gitlab.Ptr(sha)}, gitlab.WithContext(ctx))
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	data, err := base64.StdEncoding.DecodeString(f.Content)
	if err != nil {
		out["error"] = fmt.Sprintf("decode content: %v", err)
		return out
	}
	out["blob_id"], out["size"] = f.BlobID, f.Size
	out["binary"] = bytes.IndexByte(data[:min(len(data), batchFilesBinarySniff)], 0) >= 0 || !utf8.Valid(data)
	out["truncated"] = false
	if out["binary"] == true {
		return out
	}
	if len(data) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(data[cut]) {
			cut-- // never split a multi-byte character
		}
		data, out["truncated"] = data[:cut], true
	}
	out["content"] = string(data)
	return out
}
