package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"
)

// matrixFixture serves identity + content/mutation endpoints for finite-matrix families.
// Hits on content/mutation paths are counted; identity GetProject/GetGroup are not.
func matrixFixture(t *testing.T, content, mut *int32, groupListHits *int32) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		method := r.Method

		switch {
		case method == http.MethodGet && isProjectIdentityPath(path):
			id := echoNumericID(path, 42)
			_, _ = io.WriteString(w, `{"id":`+itoa(id)+`,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group","full_path":"g","parent_id":0}}`)
		case method == http.MethodGet && isGroupIdentityPath(path):
			id := echoNumericID(path, 10)
			if id == 99 {
				_, _ = io.WriteString(w, `{"id":99,"full_path":"other","parent_id":0}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":`+itoa(id)+`,"full_path":"g","parent_id":0}`)
		case strings.Contains(path, "/groups/") && strings.Contains(path, "/projects"):
			if groupListHits != nil {
				atomic.AddInt32(groupListHits, 1)
			}
			_, _ = io.WriteString(w, `[{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}},{"id":99,"path_with_namespace":"x/p","namespace":{"id":8,"kind":"group"}}]`)
		case strings.Contains(path, "/merge_requests/") && strings.HasSuffix(path, "/diffs"):
			atomic.AddInt32(content, 1)
			_, _ = io.WriteString(w, `[]`)
		case strings.Contains(path, "/merge_requests/") && strings.Contains(path, "/versions"):
			atomic.AddInt32(content, 1)
			_, _ = io.WriteString(w, `[{"id":1}]`)
		case strings.Contains(path, "/merge_requests/") && method == http.MethodGet &&
			!strings.Contains(path, "/notes") && !strings.Contains(path, "/discussions"):
			_, _ = io.WriteString(w, `{"id":1,"iid":1,"project_id":42,"source_project_id":42,"diff_refs":{"base_sha":"a","head_sha":"b","start_sha":"a"}}`)
		case strings.Contains(path, "/discussions") || strings.Contains(path, "/notes"):
			if method == http.MethodGet {
				atomic.AddInt32(content, 1)
				_, _ = io.WriteString(w, `[]`)
			} else {
				atomic.AddInt32(mut, 1)
				_, _ = io.WriteString(w, `{"id":1,"body":"n"}`)
			}
		case strings.Contains(path, "/repository/") || strings.Contains(path, "/repository"):
			if method == http.MethodGet {
				atomic.AddInt32(content, 1)
				if strings.Contains(path, "/tree") {
					_, _ = io.WriteString(w, `[]`)
				} else if strings.Contains(path, "/commits") {
					_, _ = io.WriteString(w, `[]`)
				} else {
					_, _ = io.WriteString(w, `{"content":""}`)
				}
			} else {
				atomic.AddInt32(mut, 1)
				_, _ = io.WriteString(w, `{"id":"abc"}`)
			}
		case strings.Contains(path, "/pipelines") || strings.Contains(path, "/jobs") || strings.Contains(path, "/bridges"):
			if method == http.MethodGet {
				atomic.AddInt32(content, 1)
				if strings.Contains(path, "/bridges") {
					_, _ = io.WriteString(w, `[{"id":1,"downstream_pipeline":{"id":9,"project_id":0,"status":"running","web_url":"http://leak"}}]`)
				} else if strings.Contains(path, "/jobs") && strings.Contains(path, "/trace") {
					w.Header().Set("Content-Type", "text/plain")
					_, _ = io.WriteString(w, "trace")
				} else {
					_, _ = io.WriteString(w, `[]`)
				}
			} else {
				atomic.AddInt32(mut, 1)
				_, _ = io.WriteString(w, `{"id":1}`)
			}
		case path == "/api/v4/projects" || (strings.HasSuffix(path, "/projects") && !strings.Contains(path, "/projects/")):
			atomic.AddInt32(content, 1)
			_, _ = io.WriteString(w, `[{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}},{"id":99,"path_with_namespace":"x/p","namespace":{"id":8,"kind":"group"}}]`)
		case strings.Contains(path, "/merge_requests") && method == http.MethodGet:
			atomic.AddInt32(content, 1)
			_, _ = io.WriteString(w, `[{"id":1,"iid":1,"project_id":42},{"id":2,"iid":2,"project_id":99}]`)
		case method == http.MethodPost || method == http.MethodPut || method == http.MethodDelete:
			atomic.AddInt32(mut, 1)
			_, _ = io.WriteString(w, `{"id":1,"iid":1}`)
		default:
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}}`)
		}
	})
}

func TestAuthzFamilyMatrix_denyZeroContentOrMutation(t *testing.T) {
	type tc struct {
		name   string
		deny   bool
		family string
		run    func(ctx context.Context, d Deps) error
	}
	cases := []tc{
		{
			name: "repo_read_get_file_deny", deny: true, family: "repo",
			run: func(ctx context.Context, d Deps) error {
				_, _, err := getFileContents(ctx, nil, getFileContentsIn{ProjectID: "99", FilePath: "a.go", Ref: "main"}, d)
				return err
			},
		},
		{
			name: "repo_read_get_file_allow", deny: false, family: "repo",
			run: func(ctx context.Context, d Deps) error {
				_, _, err := getFileContents(ctx, nil, getFileContentsIn{ProjectID: "42", FilePath: "a.go", Ref: "main"}, d)
				return err
			},
		},
		{
			name: "repo_write_create_branch_deny", deny: true, family: "repo",
			run: func(ctx context.Context, d Deps) error {
				pid := "99"
				_, _, err := createBranch(ctx, nil, createBranchIn{ProjectID: &pid, Branch: "f", Ref: "main"}, d)
				return err
			},
		},
		{
			name: "repo_write_create_branch_allow", deny: false, family: "repo",
			run: func(ctx context.Context, d Deps) error {
				pid := "42"
				_, _, err := createBranch(ctx, nil, createBranchIn{ProjectID: &pid, Branch: "f", Ref: "main"}, d)
				return err
			},
		},
		{
			name: "discussion_read_deny", deny: true, family: "discussion",
			run: func(ctx context.Context, d Deps) error {
				_, _, err := mrDiscussions(ctx, nil, mrDiscussionsIn{pidMR: pidMR{ProjectID: "99", MergeRequestIID: 1}}, d)
				return err
			},
		},
		{
			name: "discussion_read_allow", deny: false, family: "discussion",
			run: func(ctx context.Context, d Deps) error {
				_, _, err := mrDiscussions(ctx, nil, mrDiscussionsIn{pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1}}, d)
				return err
			},
		},
		{
			name: "discussion_write_deny", deny: true, family: "discussion",
			run: func(ctx context.Context, d Deps) error {
				_, _, err := createMergeRequestNote(ctx, nil, createMergeRequestNoteIn{pidMR: pidMR{ProjectID: "99", MergeRequestIID: 1}, Body: "x"}, d)
				return err
			},
		},
		{
			name: "discussion_write_allow", deny: false, family: "discussion",
			run: func(ctx context.Context, d Deps) error {
				_, _, err := createMergeRequestNote(ctx, nil, createMergeRequestNoteIn{pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1}, Body: "x"}, d)
				return err
			},
		},
		{
			name: "pipeline_read_deny", deny: true, family: "pipeline",
			run: func(ctx context.Context, d Deps) error {
				_, _, err := listPipelines(ctx, nil, listPipelinesIn{ProjectID: "99"}, d)
				return err
			},
		},
		{
			name: "pipeline_read_allow", deny: false, family: "pipeline",
			run: func(ctx context.Context, d Deps) error {
				_, _, err := listPipelines(ctx, nil, listPipelinesIn{ProjectID: "42"}, d)
				return err
			},
		},
		{
			name: "pipeline_trace_deny", deny: true, family: "pipeline",
			run: func(ctx context.Context, d Deps) error {
				_, _, err := getPipelineJobOutput(ctx, nil, getPipelineJobOutputIn{ProjectID: "99", JobID: 1}, d)
				return err
			},
		},
		{
			name: "pipeline_trace_allow", deny: false, family: "pipeline",
			run: func(ctx context.Context, d Deps) error {
				_, _, err := getPipelineJobOutput(ctx, nil, getPipelineJobOutputIn{ProjectID: "42", JobID: 1}, d)
				return err
			},
		},
		{
			name: "pipeline_write_deny", deny: true, family: "pipeline",
			run: func(ctx context.Context, d Deps) error {
				_, _, err := createPipeline(ctx, nil, createPipelineIn{ProjectID: "99", Ref: "main"}, d)
				return err
			},
		},
		{
			name: "pipeline_write_allow", deny: false, family: "pipeline",
			run: func(ctx context.Context, d Deps) error {
				_, _, err := createPipeline(ctx, nil, createPipelineIn{ProjectID: "42", Ref: "main"}, d)
				return err
			},
		},
		{
			name: "mr_mutation_update_deny", deny: true, family: "mr_mut",
			run: func(ctx context.Context, d Deps) error {
				title := "t"
				_, _, err := updateMergeRequest(ctx, nil, updateMergeRequestIn{pidMR: pidMR{ProjectID: "99", MergeRequestIID: 1}, Title: &title}, d)
				return err
			},
		},
		{
			name: "mr_mutation_update_allow", deny: false, family: "mr_mut",
			run: func(ctx context.Context, d Deps) error {
				title := "t"
				_, _, err := updateMergeRequest(ctx, nil, updateMergeRequestIn{pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1}, Title: &title}, d)
				return err
			},
		},
		{
			name: "project_get_deny", deny: true, family: "project",
			run: func(ctx context.Context, d Deps) error {
				_, _, err := getProject(ctx, nil, getProjectIn{ProjectID: "99"}, d)
				return err
			},
		},
		{
			name: "project_get_allow", deny: false, family: "project",
			run: func(ctx context.Context, d Deps) error {
				_, _, err := getProject(ctx, nil, getProjectIn{ProjectID: "42"}, d)
				return err
			},
		},
		{
			name: "draft_notes_legacy_under_group_policy", deny: false, family: "drafts_legacy",
			run: func(ctx context.Context, d Deps) error {
				// Group-only policy must not expand onto draft_notes (resolveLegacy).
				d.Config.AllowedProjectIDs = nil
				d.Config.AllowedGroupIDs = []string{"10"}
				_, err := (pidMR{ProjectID: "42", MergeRequestIID: 1}).resolveLegacy(d)
				return err
			},
		},
		{
			name: "branch_diffs_legacy_string_allowlist", deny: false, family: "branch_diffs_legacy",
			run: func(ctx context.Context, d Deps) error {
				// Out-of-matrix: string match only — "42" allowed, no GetProject policy path required.
				d.Config.AllowedProjectIDs = []string{"42"}
				d.Config.AllowedGroupIDs = nil
				_, _, err := getBranchDiffs(ctx, nil, getBranchDiffsIn{ProjectID: "42", From: "a", To: "b"}, d)
				return err
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var content, mut int32
			d := authzDeps(t, matrixFixture(t, &content, &mut, nil))
			if tc.family != "drafts_legacy" && tc.family != "branch_diffs_legacy" {
				d.Config.AllowedProjectIDs = []string{"42"}
			}
			err := tc.run(context.Background(), d)
			if tc.deny {
				if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) && !strings.Contains(err.Error(), "not allowed") {
					t.Fatalf("want deny, got %v", err)
				}
				if atomic.LoadInt32(&content) != 0 || atomic.LoadInt32(&mut) != 0 {
					t.Fatalf("deny leaked content=%d mut=%d", content, mut)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				switch tc.family {
				case "discussion", "pipeline", "mr_mut", "repo":
					if atomic.LoadInt32(&content)+atomic.LoadInt32(&mut) == 0 && tc.name != "project_get_allow" {
						// project_get may only hit identity; others must touch content/mutation.
						if !strings.Contains(tc.name, "project_get") {
							t.Fatalf("allow expected content/mutation >0, content=%d mut=%d", content, mut)
						}
					}
				}
			}
		})
	}
}

func TestListGroupProjects_canonicalIDAfterAuthzAndDenyBeforeList(t *testing.T) {
	var groupList, groupGet int32
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/groups/") && strings.Contains(path, "/projects"):
			atomic.AddInt32(&groupList, 1)
			if !strings.Contains(path, "/groups/10/") {
				t.Errorf("list used non-canonical group path %s", path)
			}
			_, _ = io.WriteString(w, `[{"id":42,"path_with_namespace":"g/p","namespace":{"id":10,"kind":"group"}}]`)
		case strings.Contains(path, "/groups/"):
			atomic.AddInt32(&groupGet, 1)
			// Path alias and numeric both resolve to canonical id 10.
			_, _ = io.WriteString(w, `{"id":10,"full_path":"g","parent_id":0}`)
		default:
			http.NotFound(w, r)
		}
	}))
	d.Config.AllowedGroupIDs = []string{"moved/old/group"}
	_, _, err := listGroupProjects(context.Background(), nil, listGroupProjectsIn{GroupID: "moved/old/group"}, d)
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&groupGet) < 1 {
		t.Fatal("expected canonical GetGroup before list")
	}
	if atomic.LoadInt32(&groupList) != 1 {
		t.Fatalf("list hits=%d", groupList)
	}

	atomic.StoreInt32(&groupList, 0)
	d2 := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/projects") {
			atomic.AddInt32(&groupList, 1)
		}
		if strings.Contains(r.URL.Path, "/groups/") {
			id := echoNumericID(r.URL.Path, 0)
			if id == 0 {
				id = 99
			}
			_, _ = io.WriteString(w, `{"id":`+itoa(id)+`,"full_path":"x","parent_id":0}`)
			return
		}
		http.NotFound(w, r)
	}))
	d2.Config.AllowedGroupIDs = []string{"10"}
	_, _, err = listGroupProjects(context.Background(), nil, listGroupProjectsIn{GroupID: "99"}, d2)
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
		t.Fatalf("want group deny, got %v", err)
	}
	if atomic.LoadInt32(&groupList) != 0 {
		t.Fatalf("list must not run after deny, hits=%d", groupList)
	}
}

func TestListMergeRequests_groupCanonicalIDAfterAuthz(t *testing.T) {
	var mrList, groupGet int32
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/groups/") && strings.Contains(path, "/merge_requests"):
			atomic.AddInt32(&mrList, 1)
			if !strings.Contains(path, "/groups/10/") {
				t.Errorf("group MR list used non-canonical path %s", path)
			}
			_, _ = io.WriteString(w, `[{"id":1,"iid":1,"project_id":42}]`)
		case strings.Contains(path, "/groups/"):
			atomic.AddInt32(&groupGet, 1)
			_, _ = io.WriteString(w, `{"id":10,"full_path":"g","parent_id":0}`)
		case isProjectIdentityPath(path):
			id := echoNumericID(path, 42)
			_, _ = io.WriteString(w, `{"id":`+itoa(id)+`,"path_with_namespace":"g/p","namespace":{"id":10,"kind":"group"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	d.Config.AllowedGroupIDs = []string{"moved/old/group"}
	gid := "moved/old/group"
	_, _, err := listMergeRequests(context.Background(), nil, listMergeRequestsIn{GroupID: &gid}, d)
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&groupGet) < 1 {
		t.Fatal("expected GetGroup before group MR list")
	}
	if atomic.LoadInt32(&mrList) != 1 {
		t.Fatalf("mr list hits=%d", mrList)
	}

	atomic.StoreInt32(&mrList, 0)
	d2 := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/merge_requests") {
			atomic.AddInt32(&mrList, 1)
		}
		if strings.Contains(r.URL.Path, "/groups/") {
			id := echoNumericID(r.URL.Path, 99)
			_, _ = io.WriteString(w, `{"id":`+itoa(id)+`,"full_path":"x","parent_id":0}`)
			return
		}
		http.NotFound(w, r)
	}))
	d2.Config.AllowedGroupIDs = []string{"10"}
	bad := "99"
	_, _, err = listMergeRequests(context.Background(), nil, listMergeRequestsIn{GroupID: &bad}, d2)
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
		t.Fatalf("want group deny before MR list, got %v", err)
	}
	if atomic.LoadInt32(&mrList) != 0 {
		t.Fatalf("MR list must not run after deny, hits=%d", mrList)
	}
}

func TestListGroupProjects_projectOnlyPolicyNoGroupGet(t *testing.T) {
	var groupGet int32
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if isGroupIdentityPath(r.URL.Path) {
			atomic.AddInt32(&groupGet, 1)
			_, _ = io.WriteString(w, `{"id":10,"full_path":"g","parent_id":0}`)
			return
		}
		if strings.Contains(r.URL.Path, "/groups/") && strings.Contains(r.URL.Path, "/projects") {
			_, _ = io.WriteString(w, `[{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}},{"id":99,"path_with_namespace":"x/p","namespace":{"id":8,"kind":"group"}}]`)
			return
		}
		if isProjectIdentityPath(r.URL.Path) {
			id := echoNumericID(r.URL.Path, 42)
			_, _ = io.WriteString(w, `{"id":`+itoa(id)+`,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}}`)
			return
		}
		http.NotFound(w, r)
	}))
	d.Config.AllowedProjectIDs = []string{"42"} // project-only — no group Get prerequisite
	_, out, err := listGroupProjects(context.Background(), nil, listGroupProjectsIn{GroupID: "alias/g"}, d)
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&groupGet) != 0 {
		t.Fatalf("project-only must not GetGroup, hits=%d", groupGet)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), `"id":99`) {
		t.Fatalf("OOS project leaked: %s", raw)
	}
}

func TestListMergeRequests_globalAndSearchFilter(t *testing.T) {
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case isProjectIdentityPath(path):
			id := echoNumericID(path, 42)
			_, _ = io.WriteString(w, `{"id":`+itoa(id)+`,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}}`)
		case strings.Contains(path, "/merge_requests"):
			_, _ = io.WriteString(w, `[{"id":1,"iid":1,"project_id":42},{"id":2,"iid":2,"project_id":99}]`)
		case path == "/api/v4/projects" || strings.HasSuffix(path, "/projects"):
			_, _ = io.WriteString(w, `[{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}},{"id":99,"path_with_namespace":"x/p","namespace":{"id":8,"kind":"group"}}]`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	d.Config.AllowedProjectIDs = []string{"42"}
	_, out, err := listMergeRequests(context.Background(), nil, listMergeRequestsIn{}, d)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), `"project_id":99`) {
		t.Fatalf("global MR OOS leak: %s", raw)
	}
	_, out, err = searchRepositories(context.Background(), nil, searchRepositoriesIn{}, d)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(out)
	if strings.Contains(string(raw), `"id":99`) {
		t.Fatalf("search OOS leak: %s", raw)
	}
}

func TestAuthzFamilyMatrix_CallToolAllowDeny(t *testing.T) {
	var content, mut int32
	d := authzDeps(t, matrixFixture(t, &content, &mut, nil))
	d.Config.AllowedProjectIDs = []string{"42"}

	call := func(t *testing.T, name string, args map[string]any) (*mcp.CallToolResult, error) {
		t.Helper()
		srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "t"}, nil)
		RegisterMergeRequests(srv, d)
		RegisterProjects(srv, d)
		RegisterRepository(srv, d)
		RegisterPipelines(srv, d)
		RegisterMRNotes(srv, d)
		cs := testutil.MCPConnect(t, srv)
		return cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	}

	res, err := call(t, "get_project", map[string]any{"project_id": "42"})
	if err != nil || res == nil || res.IsError {
		t.Fatalf("allow get_project: err=%v res=%v", err, res)
	}

	atomic.StoreInt32(&content, 0)
	atomic.StoreInt32(&mut, 0)
	res, err = call(t, "create_merge_request", map[string]any{
		"project_id": "99", "source_branch": "a", "target_branch": "b", "title": "t",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !res.IsError {
		t.Fatal("expected tool error on deny")
	}
	txt := toolErrorText(t, res)
	if !strings.Contains(txt, readmeta.CodeAuthzDenied) {
		t.Fatalf("want authz_denied in CallTool error, got %q", txt)
	}
	if atomic.LoadInt32(&mut) != 0 {
		t.Fatalf("CallTool deny mutation hits=%d", mut)
	}

	atomic.StoreInt32(&content, 0)
	res, err = call(t, "list_merge_request_diffs", map[string]any{
		"project_id": "99", "merge_request_iid": 1, "page": 1, "per_page": 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !res.IsError {
		t.Fatal("expected diffs deny")
	}
	if atomic.LoadInt32(&content) != 0 {
		t.Fatalf("CallTool deny content hits=%d", content)
	}
}

func TestPidOnly_legacyUnaffectedByGroupPolicy(t *testing.T) {
	var identity int32
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isProjectIdentityPath(r.URL.Path) || isGroupIdentityPath(r.URL.Path) {
			atomic.AddInt32(&identity, 1)
		}
		_, _ = io.WriteString(w, `{"id":1}`)
	}))
	d.Config.AllowedGroupIDs = []string{"10"}
	d.Config.AllowedProjectIDs = nil
	// Out-of-matrix wiki-style path via pidOnly: group policy must not force GetProject.
	pid, err := pidOnly(context.Background(), "42", d)
	if err != nil || pid != "42" {
		t.Fatalf("pid=%q err=%v", pid, err)
	}
	if atomic.LoadInt32(&identity) != 0 {
		t.Fatalf("legacy pidOnly must not resolve canonical identity, hits=%d", identity)
	}
}

func TestListPipelineTriggerJobs_unknownChildRedactedInHandlerOutput(t *testing.T) {
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/bridges") {
			_, _ = io.WriteString(w, `[{"id":1,"name":"t","downstream_pipeline":{"id":1,"project_id":0,"web_url":"http://secret"}}]`)
			return
		}
		if isProjectIdentityPath(r.URL.Path) {
			id := echoNumericID(r.URL.Path, 42)
			_, _ = io.WriteString(w, `{"id":`+itoa(id)+`,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}}`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	d.Config.AllowedProjectIDs = []string{"42"}
	_, out, err := listPipelineTriggerJobs(context.Background(), nil, listPipelineTriggerJobsIn{ProjectID: "42", PipelineID: 3}, d)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "downstream_pipeline") && !strings.Contains(string(raw), `"downstream_pipeline":null`) {
		// Downstream must be null/omitted — not an object with child fields.
		if strings.Contains(string(raw), "http://secret") || strings.Contains(string(raw), `"project_id":0`) {
			t.Fatalf("unknown child payload leaked: %s", raw)
		}
	}
	if strings.Contains(string(raw), "http://secret") {
		t.Fatalf("leaked: %s", raw)
	}
}

func TestForkRepository_destinationGroupAuthz(t *testing.T) {
	var mut int32
	var lastForkBody string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case r.Method == http.MethodPost && strings.Contains(path, "/fork"):
			atomic.AddInt32(&mut, 1)
			b, _ := io.ReadAll(r.Body)
			lastForkBody = string(b)
			if lastForkBody == "" {
				lastForkBody = r.URL.RawQuery
			}
			_, _ = io.WriteString(w, `{"id":100,"path_with_namespace":"g/forked"}`)
		case strings.Contains(path, "/groups/"):
			seg := path[strings.LastIndex(path, "/")+1:]
			switch {
			case seg == "10" || seg == "g" || strings.HasSuffix(path, "/groups/g"):
				_, _ = io.WriteString(w, `{"id":10,"full_path":"g","parent_id":0}`)
			case seg == "11":
				_, _ = io.WriteString(w, `{"id":11,"full_path":"g/sub","parent_id":10}`)
			case seg == "99" || strings.Contains(path, "/groups/other"):
				_, _ = io.WriteString(w, `{"id":99,"full_path":"other","parent_id":0}`)
			default:
				http.NotFound(w, r)
			}
		case isProjectIdentityPath(path):
			id := echoNumericID(path, 42)
			_, _ = io.WriteString(w, `{"id":`+itoa(id)+`,"path_with_namespace":"g/p","namespace":{"id":10,"kind":"group"}}`)
		default:
			http.NotFound(w, r)
		}
	})

	assertBoundID := func(t *testing.T, want string) {
		t.Helper()
		if !strings.Contains(lastForkBody, "namespace_id") || !strings.Contains(lastForkBody, want) {
			t.Fatalf("fork body/query missing bound namespace_id=%s: %q", want, lastForkBody)
		}
		if strings.Contains(lastForkBody, "namespace_path") {
			t.Fatalf("group-policy fork must not pass namespace_path: %q", lastForkBody)
		}
	}

	t.Run("allow_dest_id", func(t *testing.T) {
		atomic.StoreInt32(&mut, 0)
		lastForkBody = ""
		d := authzDeps(t, handler)
		d.Config.AllowedProjectIDs = []string{"42"}
		d.Config.AllowedGroupIDs = []string{"10"}
		ns := int64(10)
		_, _, err := forkRepository(context.Background(), nil, forkRepositoryIn{
			ProjectID: "42", NamespaceID: &ns, Name: ptr("f"),
		}, d)
		if err != nil {
			t.Fatal(err)
		}
		if atomic.LoadInt32(&mut) != 1 {
			t.Fatalf("mut=%d", mut)
		}
		assertBoundID(t, "10")
	})

	t.Run("allow_dest_path_bound_to_id", func(t *testing.T) {
		atomic.StoreInt32(&mut, 0)
		lastForkBody = ""
		d := authzDeps(t, handler)
		d.Config.AllowedProjectIDs = []string{"42"}
		d.Config.AllowedGroupIDs = []string{"10"}
		path := "g"
		_, _, err := forkRepository(context.Background(), nil, forkRepositoryIn{
			ProjectID: "42", NamespacePath: &path, Name: ptr("f"),
		}, d)
		if err != nil {
			t.Fatal(err)
		}
		if atomic.LoadInt32(&mut) != 1 {
			t.Fatalf("mut=%d", mut)
		}
		assertBoundID(t, "10")
	})

	t.Run("allow_descendant_dest", func(t *testing.T) {
		atomic.StoreInt32(&mut, 0)
		lastForkBody = ""
		d := authzDeps(t, handler)
		d.Config.AllowedProjectIDs = []string{"42"}
		d.Config.AllowedGroupIDs = []string{"10"}
		ns := int64(11)
		_, _, err := forkRepository(context.Background(), nil, forkRepositoryIn{
			ProjectID: "42", NamespaceID: &ns, Name: ptr("f"),
		}, d)
		if err != nil {
			t.Fatal(err)
		}
		if atomic.LoadInt32(&mut) != 1 {
			t.Fatalf("mut=%d", mut)
		}
		assertBoundID(t, "11")
	})

	t.Run("deny_disallowed_dest_zero_mutation", func(t *testing.T) {
		atomic.StoreInt32(&mut, 0)
		d := authzDeps(t, handler)
		d.Config.AllowedProjectIDs = []string{"42"}
		d.Config.AllowedGroupIDs = []string{"10"}
		ns := int64(99)
		_, _, err := forkRepository(context.Background(), nil, forkRepositoryIn{
			ProjectID: "42", NamespaceID: &ns, Name: ptr("f"),
		}, d)
		if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
			t.Fatalf("want deny, got %v", err)
		}
		if atomic.LoadInt32(&mut) != 0 {
			t.Fatalf("mut=%d", mut)
		}
	})

	t.Run("deny_missing_dest_zero_mutation", func(t *testing.T) {
		atomic.StoreInt32(&mut, 0)
		d := authzDeps(t, handler)
		d.Config.AllowedProjectIDs = []string{"42"}
		d.Config.AllowedGroupIDs = []string{"10"}
		_, _, err := forkRepository(context.Background(), nil, forkRepositoryIn{
			ProjectID: "42", Name: ptr("f"),
		}, d)
		if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
			t.Fatalf("want deny missing dest, got %v", err)
		}
		if atomic.LoadInt32(&mut) != 0 {
			t.Fatalf("mut=%d", mut)
		}
	})

	t.Run("deny_unknown_dest_zero_mutation", func(t *testing.T) {
		atomic.StoreInt32(&mut, 0)
		d := authzDeps(t, handler)
		d.Config.AllowedProjectIDs = []string{"42"}
		d.Config.AllowedGroupIDs = []string{"10"}
		path := "missing/ns"
		_, _, err := forkRepository(context.Background(), nil, forkRepositoryIn{
			ProjectID: "42", NamespacePath: &path, Name: ptr("f"),
		}, d)
		if err == nil || !strings.Contains(err.Error(), readmeta.CodeIdentityUnresolved) {
			t.Fatalf("want identity_unresolved, got %v", err)
		}
		if atomic.LoadInt32(&mut) != 0 {
			t.Fatalf("mut=%d", mut)
		}
	})

	t.Run("deny_conflicting_id_and_path", func(t *testing.T) {
		atomic.StoreInt32(&mut, 0)
		d := authzDeps(t, handler)
		d.Config.AllowedProjectIDs = []string{"42"}
		d.Config.AllowedGroupIDs = []string{"10", "99"}
		ns := int64(10)
		path := "other"
		_, _, err := forkRepository(context.Background(), nil, forkRepositoryIn{
			ProjectID: "42", NamespaceID: &ns, NamespacePath: &path, Name: ptr("f"),
		}, d)
		if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
			t.Fatalf("want conflict deny, got %v", err)
		}
		if atomic.LoadInt32(&mut) != 0 {
			t.Fatalf("mut=%d", mut)
		}
	})

	t.Run("project_only_policy_keeps_legacy_dest_passthrough", func(t *testing.T) {
		atomic.StoreInt32(&mut, 0)
		lastForkBody = ""
		d := authzDeps(t, handler)
		d.Config.AllowedProjectIDs = []string{"42"}
		d.Config.AllowedGroupIDs = nil
		path := "g"
		_, _, err := forkRepository(context.Background(), nil, forkRepositoryIn{
			ProjectID: "42", NamespacePath: &path, Name: ptr("f"),
		}, d)
		if err != nil {
			t.Fatal(err)
		}
		if atomic.LoadInt32(&mut) != 1 {
			t.Fatalf("mut=%d", mut)
		}
		if !strings.Contains(lastForkBody, "namespace_path") && !strings.Contains(lastForkBody, "g") {
			t.Fatalf("expected legacy namespace_path passthrough: %q", lastForkBody)
		}
	})
}

// owner42 + source99 from MR metadata — not owner99 denial. All seven fork-sensitive handlers.
func TestForkSensitiveHandlers_sourceDenyZeroContent(t *testing.T) {
	type tc struct {
		name string
		run  func(ctx context.Context, d Deps) error
	}
	cases := []tc{
		{"get_merge_request_diffs", func(ctx context.Context, d Deps) error {
			_, _, err := getMergeRequestDiffs(ctx, nil, getMergeRequestDiffsIn{pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1}}, d)
			return err
		}},
		{"list_merge_request_diffs", func(ctx context.Context, d Deps) error {
			_, _, err := listMergeRequestDiffs(ctx, nil, listMergeRequestDiffsIn{pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1}}, d)
			return err
		}},
		{"get_merge_request_conflicts", func(ctx context.Context, d Deps) error {
			_, _, err := getMergeRequestConflicts(ctx, nil, getMergeRequestConflictsIn{pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1}}, d)
			return err
		}},
		{"list_merge_request_changed_files", func(ctx context.Context, d Deps) error {
			_, _, err := listMergeRequestChangedFiles(ctx, nil, listMergeRequestChangedFilesIn{pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1}}, d)
			return err
		}},
		{"get_merge_request_file_diff", func(ctx context.Context, d Deps) error {
			_, _, err := getMergeRequestFileDiff(ctx, nil, getMergeRequestFileDiffIn{pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1}, Files: []string{"a.go"}}, d)
			return err
		}},
		{"list_merge_request_versions", func(ctx context.Context, d Deps) error {
			_, _, err := listMergeRequestVersions(ctx, nil, listMergeRequestVersionsIn{pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1}}, d)
			return err
		}},
		{"get_merge_request_version", func(ctx context.Context, d Deps) error {
			_, _, err := getMergeRequestVersion(ctx, nil, getMergeRequestVersionIn{pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1}, VersionID: 5}, d)
			return err
		}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var content, identity int32
			d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				path := r.URL.Path
				switch {
				case strings.HasSuffix(path, "/diffs") || strings.Contains(path, "/versions") || strings.Contains(path, "/compare"):
					atomic.AddInt32(&content, 1)
					_, _ = io.WriteString(w, `[]`)
				case strings.Contains(path, "/merge_requests/") && r.Method == http.MethodGet:
					// Owner 42 allowed; fork source 99 denied from metadata.
					_, _ = io.WriteString(w, `{"id":1,"iid":1,"project_id":42,"source_project_id":99,"diff_refs":{"base_sha":"a","head_sha":"b","start_sha":"a"},"has_conflicts":false}`)
				case isProjectIdentityPath(path):
					atomic.AddInt32(&identity, 1)
					id := echoNumericID(path, 42)
					ns := `{"id":7,"kind":"group"}`
					if id == 99 {
						ns = `{"id":8,"kind":"group"}`
					}
					_, _ = io.WriteString(w, `{"id":`+itoa(id)+`,"path_with_namespace":"g/p","namespace":`+ns+`}`)
				default:
					_, _ = io.WriteString(w, `{}`)
				}
			}))
			d.Config.AllowedProjectIDs = []string{"42"}
			err := tc.run(context.Background(), d)
			if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
				t.Fatalf("want source-fork authz_denied, got %v", err)
			}
			if atomic.LoadInt32(&content) != 0 {
				t.Fatalf("content hits=%d want 0 (no diffs/versions/compare after deny)", content)
			}
		})
	}
}

func TestAuthorizeMROwnerAndForks_unprovenSourceFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"nil_mr_empty_object_zero_source", `{"id":1,"iid":1,"project_id":42,"source_project_id":0}`},
		{"negative_source", `{"id":1,"iid":1,"project_id":42,"source_project_id":-3}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var content int32
			d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/diffs") || strings.Contains(r.URL.Path, "/versions") {
					atomic.AddInt32(&content, 1)
					_, _ = io.WriteString(w, `[]`)
					return
				}
				if strings.Contains(r.URL.Path, "/merge_requests/") {
					_, _ = io.WriteString(w, tc.body)
					return
				}
				if isProjectIdentityPath(r.URL.Path) {
					id := echoNumericID(r.URL.Path, 42)
					_, _ = io.WriteString(w, `{"id":`+itoa(id)+`,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}}`)
					return
				}
				_, _ = io.WriteString(w, `{}`)
			}))
			d.Config.AllowedProjectIDs = []string{"42"}
			_, err := authorizeMROwnerAndForks(context.Background(), d, "42", 1)
			if err == nil || !strings.Contains(err.Error(), readmeta.CodeIdentityUnresolved) {
				t.Fatalf("want identity_unresolved, got %v", err)
			}
			_, _, err = listMergeRequestDiffs(context.Background(), nil, listMergeRequestDiffsIn{
				pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
			}, d)
			if err == nil || !strings.Contains(err.Error(), readmeta.CodeIdentityUnresolved) {
				t.Fatalf("list_merge_request_diffs want identity_unresolved, got %v", err)
			}
			if atomic.LoadInt32(&content) != 0 {
				t.Fatalf("content=%d", content)
			}
		})
	}
}

func TestForkSensitiveHandlers_emptyPolicyNoExtraIdentity(t *testing.T) {
	var identity, content int32
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		if isProjectIdentityPath(path) || isGroupIdentityPath(path) {
			atomic.AddInt32(&identity, 1)
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}}`)
			return
		}
		if strings.HasSuffix(path, "/diffs") || strings.Contains(path, "/versions") {
			atomic.AddInt32(&content, 1)
			_, _ = io.WriteString(w, `[{"old_path":"a.go","new_path":"a.go","diff":""}]`)
			return
		}
		if strings.Contains(path, "/merge_requests/") {
			_, _ = io.WriteString(w, `{"id":1,"iid":1,"project_id":42,"source_project_id":0,"has_conflicts":false,"diff_refs":{}}`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	// Empty both allowlists — six non-020 handlers must not GetProject before content.
	_, _, err := getMergeRequestDiffs(context.Background(), nil, getMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&identity) != 0 {
		t.Fatalf("empty policy get_merge_request_diffs must not GetProject, identity=%d", identity)
	}
	if atomic.LoadInt32(&content) == 0 {
		t.Fatal("expected content")
	}
}
