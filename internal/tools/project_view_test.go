package tools

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
)

// leakyProject is a project as GitLab returns it to a maintainer: the allowed
// fields plus secrets and settings that must never reach an MCP client.
const leakyProject = `{
	"id": 7, "name": "p", "path": "p", "path_with_namespace": "g/p", "name_with_namespace": "G / p",
	"description": "d", "default_branch": "main", "visibility": "private",
	"web_url": "http://x/g/p", "http_url_to_repo": "http://x/g/p.git", "ssh_url_to_repo": "git@x:g/p.git",
	"topics": ["a"], "archived": false, "empty_repo": false, "created_at": "2026-01-01T00:00:00Z",
	"runners_token": "SECRET-RUNNERS", "ci_config_path": ".gitlab-ci.yml", "owner": {"id": 1, "username": "u"},
	"creator_id": 1, "permissions": {"project_access": {"access_level": 40}}, "service_desk_address": "sd@x",
	"import_url": "http://user:pw@x/r.git", "open_issues_count": 3, "star_count": 1,
	"namespace": {"id": 2, "name": "G", "path": "g", "kind": "group", "full_path": "g", "web_url": "http://x/g", "avatar_url": "a", "parent_id": 0},
	"forked_from_project": {"id": 5, "name": "up", "path_with_namespace": "u/p", "web_url": "http://x/u/p", "repository_storage": "default"}
}`

// projectTools is every tool whose output contains project objects. Add a tool
// here when it starts returning projects.
var projectTools = []struct {
	name string
	args map[string]any
	list bool // output is {"projects": [...]}
}{
	{"get_project", map[string]any{"project_id": "7"}, false},
	{"list_projects", map[string]any{}, true},
	{"list_group_projects", map[string]any{"group_id": "g"}, true},
	{"search_repositories", map[string]any{"search": "p"}, true},
	{"create_repository", map[string]any{"name": "p"}, false},
	{"fork_repository", map[string]any{"project_id": "7"}, false},
}

func TestProjectTools_allowlist(t *testing.T) {
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path != "/api/v4/projects/7" {
			writeFixture(w, "["+leakyProject+"]")
			return
		}
		_, _ = fmt.Fprint(w, leakyProject)
	}))
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "test"}, nil)
	d := Deps{Config: &config.Config{}, Client: cli}
	RegisterProjects(srv, d)
	RegisterRepository(srv, d)
	cs := testutil.MCPConnect(t, srv)
	registered := testutil.ToolNames(t, cs)

	for _, tc := range projectTools {
		t.Run(tc.name, func(t *testing.T) {
			if !registered[tc.name] {
				t.Fatalf("%s is not registered", tc.name)
			}
			out := callDiffTool(t, cs, tc.name, tc.args)
			p := out
			if tc.list {
				ps, _ := out["projects"].([]any)
				if len(ps) != 1 {
					t.Fatalf("projects = %v", out["projects"])
				}
				p, _ = ps[0].(map[string]any)
			}
			assertAllowed(t, p, projectFields)
			assertAllowed(t, p["namespace"], projectNamespaceFields)
			assertAllowed(t, p["forked_from_project"], projectForkFields)
			if p["id"] != float64(7) || p["path_with_namespace"] != "g/p" || p["default_branch"] != "main" || p["web_url"] != "http://x/g/p" {
				t.Fatalf("allowed fields were dropped: %v", p)
			}
			if _, ok := p["runners_token"]; ok {
				t.Fatal("runners_token leaked")
			}
		})
	}
}

func assertAllowed(t *testing.T, v any, allowed map[string]bool) {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not an object: %#v", v)
	}
	for k := range m {
		if !allowed[k] {
			t.Errorf("non-allowlisted field %q in output", k)
		}
	}
}

func TestProjectView(t *testing.T) {
	if got := projectView(nil); got != nil {
		t.Fatalf("nil -> %#v", got)
	}
	if got, _ := projectView([]map[string]any{}).([]any); got == nil || len(got) != 0 {
		t.Fatalf("empty list -> %#v", got)
	}
	one, _ := projectView(map[string]any{"id": 1, "runners_token": "x", "namespace": "not-an-object"}).(map[string]any)
	if len(one) != 2 || one["id"] != float64(1) || one["namespace"] != "not-an-object" {
		t.Fatalf("view = %#v", one)
	}
}
