package tools

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/vgromanov/gitlab-mcp/internal/config"
	"github.com/vgromanov/gitlab-mcp/internal/testutil"
)

func TestListProjects_requestShape(t *testing.T) {
	var sawPath, sawToken string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawToken = r.Header.Get("Private-Token")
		if r.Method != http.MethodGet {
			t.Errorf("method %s", r.Method)
		}
		q := r.URL.Query()
		if q.Get("page") != "1" || q.Get("per_page") != "20" {
			t.Errorf("query: %v", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	})
	cli, _ := testutil.NewGitLabClient(t, h)
	ctx := context.Background()
	_, out, err := listProjects(ctx, nil, listProjectsIn{}, Deps{
		Config: &config.Config{Token: "secret"},
		Client: cli,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sawPath != "/api/v4/projects" {
		t.Fatalf("path %q", sawPath)
	}
	if sawToken != "test-token" {
		// client-go uses token from NewClient, not from Config in this test path
		t.Fatalf("token header %q", sawToken)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("out type %T", out)
	}
	if _, ok := m["projects"]; !ok {
		t.Fatalf("missing projects: %#v", m)
	}
}
