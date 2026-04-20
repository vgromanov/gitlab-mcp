//go:build integration

package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/joho/godotenv"
	"github.com/vgromanov/gitlab-mcp/internal/config"
	glclient "github.com/vgromanov/gitlab-mcp/internal/gitlab"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// RepoRoot returns the repository root (directory containing go.mod).
func RepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/testutil -> repo root
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// LoadDotenv loads .env from repo root (best-effort).
func LoadDotenv(t *testing.T) {
	t.Helper()
	_ = godotenv.Load(filepath.Join(RepoRoot(t), ".env"))
}

// GitLabIntegrationClient returns a real GitLab client or skips the test.
func GitLabIntegrationClient(t *testing.T) (*gitlab.Client, *config.Config) {
	t.Helper()
	LoadDotenv(t)
	token := strings.TrimSpace(os.Getenv("GITLAB_PERSONAL_ACCESS_TOKEN"))
	if token == "" {
		t.Skip("GITLAB_PERSONAL_ACCESS_TOKEN not set")
	}
	apiURL := strings.TrimSpace(os.Getenv("GITLAB_API_URL"))
	if apiURL == "" {
		apiURL = "https://gitlab.com/api/v4"
	}
	cfg := &config.Config{
		Token:     token,
		APIURL:    apiURL,
		Wiki:      os.Getenv("USE_GITLAB_WIKI") == "true",
		Milestone: os.Getenv("USE_MILESTONE") == "true",
		Pipeline:  os.Getenv("USE_PIPELINE") == "true",
	}
	c, err := glclient.NewClient(cfg)
	if err != nil {
		t.Fatalf("gitlab client: %v", err)
	}
	return c, cfg
}
