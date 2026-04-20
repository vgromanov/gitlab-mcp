// Package testutil provides mocks and harness helpers for MCP/GitLab tests.
package testutil

import (
	"net/http"
	"net/http/httptest"
	"testing"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// NewGitLabClient returns a GitLab API client pointed at a test HTTP server.
// basePath should normally be "/api/v4" relative to the test server root.
func NewGitLabClient(t *testing.T, h http.Handler) (*gitlab.Client, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	c, err := gitlab.NewClient("test-token", gitlab.WithBaseURL(ts.URL+"/api/v4"))
	if err != nil {
		t.Fatalf("gitlab client: %v", err)
	}
	return c, ts
}
