//go:build integration

package tools

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/vgromanov/gitlab-mcp/internal/testutil"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

func TestIntegration_listProjects_smoke(t *testing.T) {
	cli, cfg := testutil.GitLabIntegrationClient(t)
	ctx := context.Background()
	_, _, err := listProjects(ctx, nil, listProjectsIn{Pagination: Pagination{Page: 1, PerPage: 5}}, Deps{
		Config: cfg,
		Client: cli,
	})
	if err != nil {
		var er *gitlab.ErrorResponse
		if errors.As(err, &er) && er.Response != nil && er.Response.StatusCode == http.StatusUnauthorized {
			t.Skip("GitLab returned 401 — check GITLAB_PERSONAL_ACCESS_TOKEN and GITLAB_API_URL")
		}
		t.Fatal(err)
	}
}
