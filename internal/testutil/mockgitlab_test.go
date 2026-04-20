package testutil

import (
	"context"
	"io"
	"net/http"
	"testing"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

func TestNewGitLabClient_projectsPath(t *testing.T) {
	var sawPath string
	cli, _ := NewGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	}))
	_, _, err := cli.Projects.ListProjects(&gitlab.ListProjectsOptions{}, gitlab.WithContext(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	if sawPath != "/api/v4/projects" {
		t.Fatalf("path = %q", sawPath)
	}
}
