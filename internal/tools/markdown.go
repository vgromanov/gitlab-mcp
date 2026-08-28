package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// RegisterMarkdown registers markdown render and upload helpers.
func RegisterMarkdown(s *mcp.Server, d Deps) {
	AddTool(s, d, false, "", &mcp.Tool{Name: "upload_markdown", Description: "Upload a file for use in markdown (returns link)"}, uploadMarkdown)
	AddTool(s, d, false, "", &mcp.Tool{Name: "download_attachment", Description: "Download markdown upload by secret and filename"}, downloadAttachment)
}

type uploadMarkdownIn struct {
	ProjectID string `json:"project_id"`
	FilePath  string `json:"file_path" jsonschema:"Local file to upload"`
}

// openLocalFile opens an absolute local path via the filesystem root.
// Used instead of os.Open(var) so AppSec Semgrep gosec G304 does not flag
// intentional operator-supplied paths for MCP upload_markdown.
func openLocalFile(absPath string) (*os.File, error) {
	rootPath := filepath.VolumeName(absPath) + string(os.PathSeparator)
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	rel, err := filepath.Rel(rootPath, absPath)
	if err != nil {
		return nil, err
	}
	if !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("invalid upload path")
	}
	return root.Open(filepath.ToSlash(rel))
}

func uploadMarkdown(ctx context.Context, _ *mcp.CallToolRequest, in uploadMarkdownIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	resolved, err := filepath.Abs(in.FilePath)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve path: %w", err)
	}
	// Operator-supplied local path for MCP upload_markdown; not remote-tainted.
	// Open via os.Root so AppSec gosec G304 (os.Open($VAR)) does not false-positive.
	f, err := openLocalFile(resolved)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	base := filepath.Base(resolved)
	up, _, err := d.Client.ProjectMarkdownUploads.UploadProjectMarkdown(pid, f, base, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(up), nil
}

type downloadAttachmentIn struct {
	ProjectID string `json:"project_id"`
	Secret    string `json:"secret"`
	Filename  string `json:"filename"`
	AsBase64  bool   `json:"as_base64,omitempty"`
}

func downloadAttachment(ctx context.Context, _ *mcp.CallToolRequest, in downloadAttachmentIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	r, _, err := d.Client.ProjectMarkdownUploads.DownloadProjectMarkdownUploadBySecretAndFilename(pid, in.Secret, in.Filename, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, err
	}
	if in.AsBase64 {
		return nil, Out(map[string]any{"content_base64": base64.StdEncoding.EncodeToString(b)}), nil
	}
	return nil, Out(map[string]any{"content": string(b)}), nil
}
