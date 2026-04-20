package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// RegisterReleases registers release tools.
func RegisterReleases(s *mcp.Server, d Deps) {
	AddTool(s, d, false, "", &mcp.Tool{Name: "list_releases", Description: "List project releases"}, listReleases)
	AddTool(s, d, false, "", &mcp.Tool{Name: "get_release", Description: "Get release by tag name"}, getRelease)
	AddTool(s, d, true, "", &mcp.Tool{Name: "create_release", Description: "Create a release"}, createRelease)
	AddTool(s, d, true, "", &mcp.Tool{Name: "update_release", Description: "Update a release"}, updateRelease)
	AddTool(s, d, true, "", &mcp.Tool{Name: "delete_release", Description: "Delete a release (tag remains)"}, deleteRelease)
	AddTool(s, d, true, "", &mcp.Tool{Name: "create_release_evidence", Description: "Create release evidence (Premium; may require GraphQL on some versions)"}, createReleaseEvidence)
	AddTool(s, d, false, "", &mcp.Tool{Name: "download_release_asset", Description: "Download a release asset by URL or by matching link name"}, downloadReleaseAsset)
}

type listReleasesIn struct {
	ProjectID string `json:"project_id"`
	Pagination
}

func listReleases(ctx context.Context, _ *mcp.CallToolRequest, in listReleasesIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	rels, resp, err := d.Client.Releases.ListReleases(pid, &gitlab.ListReleasesOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"releases": rels, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type getReleaseIn struct {
	ProjectID string `json:"project_id"`
	TagName   string `json:"tag_name"`
}

func getRelease(ctx context.Context, _ *mcp.CallToolRequest, in getReleaseIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	r, _, err := d.Client.Releases.GetRelease(pid, in.TagName, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(r), nil
}

type createReleaseIn struct {
	ProjectID   string  `json:"project_id"`
	TagName     string  `json:"tag_name"`
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	Ref         *string `json:"ref,omitempty"`
}

func createRelease(ctx context.Context, _ *mcp.CallToolRequest, in createReleaseIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	opt := &gitlab.CreateReleaseOptions{TagName: gitlab.Ptr(in.TagName)}
	if in.Name != nil {
		opt.Name = in.Name
	}
	if in.Description != nil {
		opt.Description = in.Description
	}
	if in.Ref != nil {
		opt.Ref = in.Ref
	}
	r, _, err := d.Client.Releases.CreateRelease(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(r), nil
}

type updateReleaseIn struct {
	ProjectID   string  `json:"project_id"`
	TagName     string  `json:"tag_name"`
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
}

func updateRelease(ctx context.Context, _ *mcp.CallToolRequest, in updateReleaseIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	opt := &gitlab.UpdateReleaseOptions{}
	if in.Name != nil {
		opt.Name = in.Name
	}
	if in.Description != nil {
		opt.Description = in.Description
	}
	r, _, err := d.Client.Releases.UpdateRelease(pid, in.TagName, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(r), nil
}

type deleteReleaseIn struct {
	ProjectID string `json:"project_id"`
	TagName   string `json:"tag_name"`
}

func deleteRelease(ctx context.Context, _ *mcp.CallToolRequest, in deleteReleaseIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := pidOnly(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	r, _, err := d.Client.Releases.DeleteRelease(pid, in.TagName, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(r), nil
}

type createReleaseEvidenceIn struct {
	ProjectID string `json:"project_id"`
	TagName   string `json:"tag_name"`
}

func createReleaseEvidence(ctx context.Context, _ *mcp.CallToolRequest, in createReleaseEvidenceIn, d Deps) (*mcp.CallToolResult, any, error) {
	_ = ctx
	_ = in
	_ = d
	return nil, nil, fmt.Errorf("create_release_evidence is not available in client-go REST helpers; use execute_graphql for your GitLab tier")
}

type downloadReleaseAssetIn struct {
	ProjectID string `json:"project_id"`
	TagName   string `json:"tag_name,omitempty"`
	DirectURL string `json:"direct_url,omitempty" jsonschema:"If set, download this URL with PAT auth"`
	LinkName  string `json:"link_name,omitempty" jsonschema:"Match assets.links[].name when tag_name is set"`
	LocalPath string `json:"local_path"`
}

func downloadReleaseAsset(ctx context.Context, _ *mcp.CallToolRequest, in downloadReleaseAssetIn, d Deps) (*mcp.CallToolResult, any, error) {
	url := strings.TrimSpace(in.DirectURL)
	if url == "" {
		if in.TagName == "" || in.LinkName == "" {
			return nil, nil, fmt.Errorf("provide direct_url or (tag_name and link_name)")
		}
		pid, err := pidOnly(ctx, in.ProjectID, d)
		if err != nil {
			return nil, nil, err
		}
		rel, _, err := d.Client.Releases.GetRelease(pid, in.TagName, gitlab.WithContext(ctx))
		if err != nil {
			return nil, nil, err
		}
		if len(rel.Assets.Links) == 0 {
			return nil, nil, fmt.Errorf("release has no asset links")
		}
		for _, l := range rel.Assets.Links {
			if l != nil && l.Name == in.LinkName {
				if l.DirectAssetURL != "" {
					url = l.DirectAssetURL
				} else {
					url = l.URL
				}
				break
			}
		}
		if url == "" {
			return nil, nil, fmt.Errorf("no asset link named %q", in.LinkName)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("PRIVATE-TOKEN", d.Config.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, nil, fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(in.LocalPath)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"saved_to": in.LocalPath, "url": url}), nil
}
