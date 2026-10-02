package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

func authzDeps(t *testing.T, h http.Handler) Deps {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(igl.BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	return Deps{Config: &config.Config{Token: "t"}, Client: cli}
}

func TestAuthorizeCanonical_emptyPolicyAllowAll(t *testing.T) {
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group","full_path":"g","parent_id":0}}`)
	}))
	c, err := AuthorizeCanonicalProject(context.Background(), d, "42")
	if err != nil || c.ID != 42 {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestAuthorizeCanonical_aliasesAndRedirectedAllowlist(t *testing.T) {
	var identity, content int32
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		if strings.Contains(path, "/projects/") {
			atomic.AddInt32(&identity, 1)
			// old path alias and numeric both resolve to id 42
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"new/ns/p","namespace":{"id":7,"kind":"group","full_path":"new/ns","parent_id":0}}`)
			return
		}
		atomic.AddInt32(&content, 1)
		_, _ = io.WriteString(w, `{}`)
	}))
	d.Config.AllowedProjectIDs = []string{"moved/old/path"} // allowlist path token
	// request by numeric — allowlist path resolves to same ID 42
	c, err := AuthorizeCanonicalProject(context.Background(), d, "42")
	if err != nil || c.ID != 42 {
		t.Fatalf("%+v %v", c, err)
	}
	if atomic.LoadInt32(&content) != 0 {
		t.Fatal("content must not run during authz")
	}
	if atomic.LoadInt32(&identity) < 2 {
		t.Fatalf("expected allowlist+request identity resolves, got %d", identity)
	}
}

func TestAuthorizeCanonical_unresolvableStaleFailClosed(t *testing.T) {
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"404"}`, http.StatusNotFound)
	}))
	d.Config.AllowedProjectIDs = []string{"stale/gone"}
	_, err := AuthorizeCanonicalProject(context.Background(), d, "stale/gone")
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeIdentityUnresolved) {
		t.Fatalf("want identity_unresolved, got %v", err)
	}
}

func TestAuthorizeCanonical_groupAncestryAndPrefixUnrelated(t *testing.T) {
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/groups/10") || strings.HasSuffix(path, "/groups/root"):
			_, _ = io.WriteString(w, `{"id":10,"full_path":"acme","parent_id":0}`)
		case strings.Contains(path, "/groups/11"):
			_, _ = io.WriteString(w, `{"id":11,"full_path":"acme/sub","parent_id":10}`)
		case strings.Contains(path, "/groups/99") || strings.HasSuffix(path, "/groups/acme-tools"):
			_, _ = io.WriteString(w, `{"id":99,"full_path":"acme-tools","parent_id":0}`)
		case strings.Contains(path, "/projects/1"):
			_, _ = io.WriteString(w, `{"id":1,"path_with_namespace":"acme/sub/p","namespace":{"id":11,"kind":"group","full_path":"acme/sub","parent_id":10}}`)
		case strings.Contains(path, "/projects/2"):
			_, _ = io.WriteString(w, `{"id":2,"path_with_namespace":"acme-tools/p","namespace":{"id":99,"kind":"group","full_path":"acme-tools","parent_id":0}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	d.Config.AllowedGroupIDs = []string{"10"}
	if _, err := AuthorizeCanonicalProject(context.Background(), d, "1"); err != nil {
		t.Fatalf("subgroup under root should allow: %v", err)
	}
	_, err := AuthorizeCanonicalProject(context.Background(), d, "2")
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
		t.Fatalf("prefix-unrelated must deny: %v", err)
	}
}

func TestAuthorizeCanonical_groupOnlyUserNamespaceDenied(t *testing.T) {
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/groups/") {
			_, _ = io.WriteString(w, `{"id":10,"full_path":"acme","parent_id":0}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":5,"path_with_namespace":"alice/p","namespace":{"id":3,"kind":"user","full_path":"alice","parent_id":0}}`)
	}))
	d.Config.AllowedGroupIDs = []string{"10"}
	_, err := AuthorizeCanonicalProject(context.Background(), d, "5")
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
		t.Fatalf("user ns under group-only must deny: %v", err)
	}
}

func TestAuthorizeCanonical_groupOnlyUnknownKindDenied(t *testing.T) {
	for _, kind := range []string{"bogus", "project"} {
		kind := kind
		t.Run(kind, func(t *testing.T) {
			d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(r.URL.Path, "/groups/") {
					_, _ = io.WriteString(w, `{"id":10,"full_path":"acme","parent_id":0}`)
					return
				}
				_, _ = io.WriteString(w, `{"id":5,"path_with_namespace":"x/p","namespace":{"id":10,"kind":"`+kind+`","full_path":"x","parent_id":0}}`)
			}))
			d.Config.AllowedGroupIDs = []string{"10"}
			_, err := AuthorizeCanonicalProject(context.Background(), d, "5")
			if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
				t.Fatalf("kind=%q must deny under group-only: %v", kind, err)
			}
		})
	}
}

func TestAuthorizeCanonical_numericIDMismatchFailClosed(t *testing.T) {
	// Generic fixture returns Project 42 for any lookup — numeric 999 must not
	// be remapped into authorized root 42 (path redirects remain OK).
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}}`)
	}))
	d.Config.AllowedProjectIDs = []string{"999"}
	_, err := AuthorizeCanonicalProject(context.Background(), d, "42")
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeIdentityUnresolved) {
		t.Fatalf("numeric allowlist mismatch must fail-closed identity: %v", err)
	}
	_, err = AuthorizeCanonicalProject(context.Background(), d, "999")
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeIdentityUnresolved) {
		t.Fatalf("numeric request mismatch must fail-closed identity: %v", err)
	}
}

func TestNormalizeIdentityToken_singleDecode(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"group/proj", "group/proj"},
		{"group%2Fproj", "group/proj"},
		{"  group%2Fproj  ", "group/proj"},
		// one decode only: %252F → %2F, not /
		{"group%252Fproj", "group%2Fproj"},
	}
	for _, tc := range cases {
		if got := normalizeIdentityToken(tc.in); got != tc.want {
			t.Fatalf("%q: got %q want %q", tc.in, got, tc.want)
		}
	}
	// Applying twice would collapse double-encoding — callers must not do that.
	once := normalizeIdentityToken("group%252Fproj")
	twice := normalizeIdentityToken(once)
	if once == twice || twice != "group/proj" {
		// once must stay group%2Fproj; a second pass would yield group/proj
		if once != "group%2Fproj" {
			t.Fatalf("once=%q", once)
		}
		if twice != "group/proj" {
			t.Fatalf("twice=%q (documents collapse risk if callers double-decode)", twice)
		}
	}
}

func TestAuthorizeCanonical_encodedPathAliasCfgAndRequest(t *testing.T) {
	// Distinguish SDK PathEscape of "group/proj" (…/group%2Fproj) from
	// PathEscape of post-single-decode "group%2Fproj" (…/group%252Fproj).
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.EscapedPath()
		switch {
		case strings.Contains(path, "/groups/"):
			if strings.Contains(path, "group%252F") {
				_, _ = io.WriteString(w, `{"id":99,"full_path":"group%2Fother","parent_id":0}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":7,"full_path":"group","parent_id":0}`)
		case strings.Contains(path, "/projects/"):
			if strings.Contains(path, "group%252F") {
				// Double-encoded lookup is a different identity, not authorized group/proj.
				_, _ = io.WriteString(w, `{"id":99,"path_with_namespace":"group%2Fproj","namespace":{"id":8,"kind":"group"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"group/proj","namespace":{"id":7,"kind":"group"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	for _, tc := range []struct {
		name, cfg, req string
	}{
		{"raw_cfg_raw_req", "group/proj", "group/proj"},
		{"encoded_cfg_raw_req", "group%2Fproj", "group/proj"},
		{"raw_cfg_encoded_req", "group/proj", "group%2Fproj"},
		{"encoded_cfg_encoded_req", "group%2Fproj", "group%2Fproj"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d.Config.AllowedProjectIDs = []string{tc.cfg}
			c, err := AuthorizeCanonicalProject(context.Background(), d, tc.req)
			if err != nil || c.ID != 42 {
				t.Fatalf("%+v %v", c, err)
			}
		})
	}
}

func TestAuthorizeCanonical_doubleEncodedDoesNotCollapse(t *testing.T) {
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.EscapedPath()
		switch {
		case strings.Contains(path, "/groups/"):
			if strings.Contains(path, "acme%252F") {
				_, _ = io.WriteString(w, `{"id":99,"full_path":"acme%2Ftools","parent_id":0}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":10,"full_path":"acme/tools","parent_id":0}`)
		case strings.Contains(path, "/projects/"):
			if strings.Contains(path, "acme%252F") {
				_, _ = io.WriteString(w, `{"id":99,"path_with_namespace":"acme%2Ftools/p","namespace":{"id":99,"kind":"group"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"acme/tools/p","namespace":{"id":10,"kind":"group"}}`)
		default:
			http.NotFound(w, r)
		}
	}))

	// Authorized path allowlist must not treat %252F as equivalent after one decode.
	d.Config.AllowedProjectIDs = []string{"acme/tools/p"}
	_, err := AuthorizeCanonicalProject(context.Background(), d, "acme%252Ftools%252Fp")
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
		t.Fatalf("double-encoded request must not collapse into authorized project: %v", err)
	}

	// Double-encoded allowlist must not authorize the real path (no second decode).
	d.Config.AllowedProjectIDs = []string{"acme%252Ftools%252Fp"}
	_, err = AuthorizeCanonicalProject(context.Background(), d, "acme/tools/p")
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
		t.Fatalf("double-encoded cfg must not collapse into authorized project: %v", err)
	}

	d.Config.AllowedProjectIDs = nil
	d.Config.AllowedGroupIDs = []string{"acme/tools"}
	_, err = AuthorizeCanonicalGroup(context.Background(), d, "acme%252Ftools")
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
		t.Fatalf("double-encoded group request must not collapse into authorized group: %v", err)
	}

	d.Config.AllowedGroupIDs = []string{"acme%252Ftools"}
	_, err = AuthorizeCanonicalGroup(context.Background(), d, "acme/tools")
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
		t.Fatalf("double-encoded group cfg must not collapse into authorized group: %v", err)
	}
}

func TestAuthorizeMROwnerAndForks_metadataErrorSafe(t *testing.T) {
	secret := "SECRET_TOKEN_do_not_leak"
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/merge_requests/") {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"message":"`+secret+`"}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}}`)
	}))
	d.Config.AllowedProjectIDs = []string{"42"}
	_, err := authorizeMROwnerAndForks(context.Background(), d, "42", 1)
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeHTTPError) {
		t.Fatalf("want safe http_error, got %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("raw SDK/body leaked: %v", err)
	}
}

func TestAuthorizeCanonical_intersection(t *testing.T) {
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/groups/"):
			_, _ = io.WriteString(w, `{"id":10,"full_path":"acme","parent_id":0}`)
		case strings.Contains(path, "/projects/1"):
			_, _ = io.WriteString(w, `{"id":1,"path_with_namespace":"acme/p","namespace":{"id":10,"kind":"group","full_path":"acme","parent_id":0}}`)
		case strings.Contains(path, "/projects/2"):
			_, _ = io.WriteString(w, `{"id":2,"path_with_namespace":"acme/other","namespace":{"id":10,"kind":"group","full_path":"acme","parent_id":0}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	d.Config.AllowedProjectIDs = []string{"1"}
	d.Config.AllowedGroupIDs = []string{"10"}
	if _, err := AuthorizeCanonicalProject(context.Background(), d, "1"); err != nil {
		t.Fatal(err)
	}
	_, err := AuthorizeCanonicalProject(context.Background(), d, "2")
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
		t.Fatalf("intersection must deny project 2: %v", err)
	}
}

func TestGetMergeRequestDiffs_forkDenyZeroContent(t *testing.T) {
	var contentHits int32
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/diffs"):
			atomic.AddInt32(&contentHits, 1)
			_, _ = io.WriteString(w, `[]`)
		case strings.Contains(path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":1,"iid":1,"project_id":42,"source_project_id":99,"diff_refs":{}}`)
		case strings.Contains(path, "/projects/42"):
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}}`)
		case strings.Contains(path, "/projects/99"):
			_, _ = io.WriteString(w, `{"id":99,"path_with_namespace":"fork/p","namespace":{"id":8,"kind":"group"}}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	d.Config.AllowedProjectIDs = []string{"42"}
	_, _, err := getMergeRequestDiffs(context.Background(), nil, getMergeRequestDiffsIn{
		pidMR: pidMR{ProjectID: "42", MergeRequestIID: 1},
	}, d)
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
		t.Fatalf("want fork deny, got %v", err)
	}
	if atomic.LoadInt32(&contentHits) != 0 {
		t.Fatalf("content hits=%d want 0", contentHits)
	}
}

func TestListPipelineTriggerJobs_redactsDownstream(t *testing.T) {
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/bridges"):
			_, _ = io.WriteString(w, `[{"id":1,"name":"trigger","downstream_pipeline":{"id":9,"project_id":99,"status":"success"}},{"id":2,"name":"ok","downstream_pipeline":{"id":8,"project_id":42,"status":"success"}}]`)
		case strings.Contains(path, "/projects/42"):
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}}`)
		case strings.Contains(path, "/projects/99"):
			_, _ = io.WriteString(w, `{"id":99,"path_with_namespace":"other/p","namespace":{"id":8,"kind":"group"}}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	d.Config.AllowedProjectIDs = []string{"42"}
	_, out, err := listPipelineTriggerJobs(context.Background(), nil, listPipelineTriggerJobsIn{
		ProjectID: "42", PipelineID: 1,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), `"project_id":99`) || strings.Contains(string(raw), `"project_id": 99`) {
		t.Fatalf("leaked downstream project 99: %s", raw)
	}
	if !strings.Contains(string(raw), `"project_id":42`) && !strings.Contains(string(raw), `"project_id": 42`) {
		// in-policy downstream may remain
		t.Logf("payload=%s", raw)
	}
}

func TestListProjects_filtersDiscovery(t *testing.T) {
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/projects") && !strings.Contains(r.URL.Path, "/projects/") {
			_, _ = io.WriteString(w, `[{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}},{"id":99,"path_with_namespace":"x/p","namespace":{"id":8,"kind":"group"}}]`)
			return
		}
		if strings.Contains(r.URL.Path, "/projects/42") {
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}}`)
			return
		}
		if strings.Contains(r.URL.Path, "/projects/99") {
			_, _ = io.WriteString(w, `{"id":99,"path_with_namespace":"x/p","namespace":{"id":8,"kind":"group"}}`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	d.Config.AllowedProjectIDs = []string{"42"}
	_, out, err := listProjects(context.Background(), nil, listProjectsIn{}, d)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), `"id":99`) {
		t.Fatalf("OOS project leaked: %s", raw)
	}
	if !strings.Contains(string(raw), `"id":42`) {
		t.Fatalf("missing allowed project: %s", raw)
	}
}

func TestAuthorizeCanonicalGroup_explicitDeny(t *testing.T) {
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/groups/10") {
			_, _ = io.WriteString(w, `{"id":10,"full_path":"acme","parent_id":0}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":99,"full_path":"other","parent_id":0}`)
	}))
	d.Config.AllowedGroupIDs = []string{"10"}
	if _, err := AuthorizeCanonicalGroup(context.Background(), d, "10"); err != nil {
		t.Fatal(err)
	}
	_, err := AuthorizeCanonicalGroup(context.Background(), d, "99")
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
		t.Fatalf("want deny, got %v", err)
	}
}

func TestCreateMergeRequest_denyZeroMutation(t *testing.T) {
	var mut int32
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			atomic.AddInt32(&mut, 1)
			_, _ = io.WriteString(w, `{"iid":1}`)
			return
		}
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/projects/99"):
			_, _ = io.WriteString(w, `{"id":99,"path_with_namespace":"other/p","namespace":{"id":8,"kind":"group"}}`)
		case strings.Contains(path, "/projects/42"):
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	d.Config.AllowedProjectIDs = []string{"99"}
	_, _, err := createMergeRequest(context.Background(), nil, createMergeRequestIn{
		ProjectID: "42", SourceBranch: "a", TargetBranch: "b", Title: "t",
	}, d)
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
		t.Fatalf("want deny got %v", err)
	}
	if atomic.LoadInt32(&mut) != 0 {
		t.Fatalf("mutation hits=%d", mut)
	}
}
