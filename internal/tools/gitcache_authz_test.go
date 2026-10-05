package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

const (
	authzHead  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	authzBase  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	authzStart = "cccccccccccccccccccccccccccccccccccccccc"
)

func gitcacheAuthzServer(t *testing.T, mutate func(path string, w http.ResponseWriter) bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		if mutate != nil && mutate(path, w) {
			return
		}
		switch {
		case strings.HasSuffix(path, "/user"):
			_, _ = io.WriteString(w, `{"id":9,"username":"actor"}`)
		case strings.Contains(path, "/repository/commits/"):
			sha := path[strings.LastIndex(path, "/")+1:]
			if len(sha) != 40 {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, `{"id":"`+sha+`"}`)
		case strings.Contains(path, "/projects/1/merge_requests/7"):
			_, _ = io.WriteString(w, `{
				"id":70,"iid":7,"project_id":1,"source_project_id":2,"target_project_id":1,
				"source_branch":"feature","target_branch":"main",
				"diff_refs":{"base_sha":"`+authzBase+`","head_sha":"`+authzHead+`","start_sha":"`+authzStart+`"}
			}`)
		case strings.Contains(path, "/projects/1") || strings.HasSuffix(path, "/projects/g%2Fp"):
			_, _ = io.WriteString(w, `{
				"id":1,"path_with_namespace":"g/p",
				"http_url_to_repo":"https://gitlab.example/g/p.git",
				"ssh_url_to_repo":"ssh://git@gitlab.example/g/p.git",
				"namespace":{"id":10,"kind":"group","full_path":"g","parent_id":0}
			}`)
		case strings.Contains(path, "/projects/2"):
			_, _ = io.WriteString(w, `{
				"id":2,"path_with_namespace":"u/fork",
				"http_url_to_repo":"https://gitlab.example/u/fork.git",
				"ssh_url_to_repo":"ssh://git@gitlab.example/u/fork.git",
				"namespace":{"id":11,"kind":"group","full_path":"u","parent_id":10}
			}`)
		case strings.Contains(path, "/groups/10"):
			_, _ = io.WriteString(w, `{"id":10,"full_path":"g","parent_id":0}`)
		case strings.Contains(path, "/groups/11"):
			_, _ = io.WriteString(w, `{"id":11,"full_path":"u","parent_id":10}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts, &hits
}

func gitcacheAuthzDeps(t *testing.T, ts *httptest.Server) Deps {
	t.Helper()
	cli, err := gitlab.NewClient("tok",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(igl.BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Token:             "tok",
		APIURL:            "https://gitlab.example/api/v4",
		AllowedProjectIDs: []string{"1", "2"},
		AllowedGroupIDs:   []string{"10"},
	}
	return Deps{Config: cfg, Client: cli}
}

func TestGitCacheAuthorizer_freshCanonicalBinding(t *testing.T) {
	ts, _ := gitcacheAuthzServer(t, nil)
	d := gitcacheAuthzDeps(t, ts)
	auth := NewGitCacheAuthorizer(d)
	intent := gitcache.AcquireIntent{AllowLoopback: true, ProjectID: "1", MRIID: 7, Depth: 1, Token: "tok"}
	g, err := auth.ResolveGrant(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if g.ActorID != "9" || g.ProjectID != "1" || g.SourceFork != "2" {
		t.Fatalf("binding %#v", g)
	}
	if g.CanonicalInstance != "https://gitlab.example/api/v4" {
		t.Fatalf("canonical instance %q", g.CanonicalInstance)
	}
	if g.OriginHost != "gitlab.example" {
		t.Fatalf("origin %q", g.OriginHost)
	}
	if g.TargetProjectID != "1" || g.TargetPath != "g/p" || g.SourcePath != "u/fork" {
		t.Fatalf("role repos %#v", g)
	}
	if g.HeadSHA != plumbing.NewHash(authzHead) || g.BaseSHA != plumbing.NewHash(authzBase) || g.StartSHA != plumbing.NewHash(authzStart) {
		t.Fatalf("shas %#v", g)
	}
	wantVer := gitcache.MRVersionFromDiffRefs(g.HeadSHA, g.BaseSHA, g.StartSHA)
	if g.MRVersion != wantVer {
		t.Fatalf("mr version %q want %q", g.MRVersion, wantVer)
	}
	if g.SourceHTTPSURL != "https://gitlab.example/u/fork.git" || g.TargetHTTPSURL != "https://gitlab.example/g/p.git" {
		t.Fatalf("role urls src=%q tgt=%q", g.SourceHTTPSURL, g.TargetHTTPSURL)
	}
	if g.ProjectPath != "g/p" || g.HeadRef != "feature" || g.BaseRef != "main" {
		t.Fatalf("path/refs %#v", g)
	}
	if g.PolicyFP == "" || g.AuthDomain == "" {
		t.Fatal("missing policy/domain")
	}
	alt := g
	alt.MRIID = 8
	if gitcache.GrantFingerprint(alt) == gitcache.GrantFingerprint(g) {
		t.Fatal("fingerprint ignores MRIID")
	}
	alt = g
	alt.CanonicalInstance = "https://gitlab.example:8443/api/v4"
	if gitcache.GrantFingerprint(alt) == gitcache.GrantFingerprint(g) {
		t.Fatal("fingerprint ignores canonical instance port")
	}
}

func TestGitCacheAuthorizer_zeroOwnerProjectIDFailClosed(t *testing.T) {
	ts, _ := gitcacheAuthzServer(t, func(path string, w http.ResponseWriter) bool {
		if strings.Contains(path, "/merge_requests/7") {
			_, _ = io.WriteString(w, `{
				"id":70,"iid":7,"project_id":0,"source_project_id":2,"target_project_id":1,
				"source_branch":"feature","target_branch":"main",
				"diff_refs":{"base_sha":"`+authzBase+`","head_sha":"`+authzHead+`","start_sha":"`+authzStart+`"}
			}`)
			return true
		}
		return false
	})
	d := gitcacheAuthzDeps(t, ts)
	auth := NewGitCacheAuthorizer(d)
	intent := gitcache.AcquireIntent{AllowLoopback: true, ProjectID: "1", MRIID: 7, Depth: 1, Token: "tok"}
	if _, err := auth.ResolveGrant(context.Background(), intent); !errors.Is(err, gitcache.ErrAuthz) {
		t.Fatalf("zero owner project_id accepted: %v", err)
	}
}

func TestGitCacheAuthorizer_rejectsCallerGrantFields(t *testing.T) {
	ts, _ := gitcacheAuthzServer(t, nil)
	d := gitcacheAuthzDeps(t, ts)
	auth := NewGitCacheAuthorizer(d)
	// Intent with expected SHAs that do not match fresh DiffRefs must fail closed.
	intent := gitcache.AcquireIntent{AllowLoopback: true,
		ProjectID:    "1",
		MRIID:        7,
		Depth:        1,
		Token:        "tok",
		ExpectedHead: plumbing.NewHash("dddddddddddddddddddddddddddddddddddddddd"),
	}
	if _, err := auth.ResolveGrant(context.Background(), intent); !errors.Is(err, gitcache.ErrAuthz) {
		t.Fatalf("stale expected head: %v", err)
	}
}

func TestGitCacheAuthorizer_forkDeny(t *testing.T) {
	ts, _ := gitcacheAuthzServer(t, nil)
	d := gitcacheAuthzDeps(t, ts)
	d.Config.AllowedProjectIDs = []string{"1"} // fork project 2 not allowed
	auth := NewGitCacheAuthorizer(d)
	intent := gitcache.AcquireIntent{AllowLoopback: true, ProjectID: "1", MRIID: 7, Depth: 1, Token: "tok"}
	if _, err := auth.ResolveGrant(context.Background(), intent); !errors.Is(err, gitcache.ErrAuthz) {
		t.Fatalf("unallowed fork source accepted: %v", err)
	}
}

func TestGitCacheAuthorizer_groupAncestryRequired(t *testing.T) {
	ts, _ := gitcacheAuthzServer(t, nil)
	d := gitcacheAuthzDeps(t, ts)
	d.Config.AllowedProjectIDs = nil
	d.Config.AllowedGroupIDs = []string{"99"} // unrelated root
	auth := NewGitCacheAuthorizer(d)
	intent := gitcache.AcquireIntent{AllowLoopback: true, ProjectID: "1", MRIID: 7, Depth: 1, Token: "tok"}
	if _, err := auth.ResolveGrant(context.Background(), intent); !errors.Is(err, gitcache.ErrAuthz) {
		t.Fatalf("group ancestry bypass: %v", err)
	}
}

func TestGitCacheAuthorizer_movedDiffRefsFailClosed(t *testing.T) {
	var flip atomic.Bool
	ts, _ := gitcacheAuthzServer(t, func(path string, w http.ResponseWriter) bool {
		if strings.Contains(path, "/merge_requests/7") && flip.Load() {
			moved := map[string]any{
				"id": 70, "iid": 7, "project_id": 1, "source_project_id": 2, "target_project_id": 1,
				"source_branch": "feature", "target_branch": "main",
				"diff_refs": map[string]string{
					"base_sha":  authzBase,
					"head_sha":  "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
					"start_sha": authzStart,
				},
			}
			_ = json.NewEncoder(w).Encode(moved)
			return true
		}
		return false
	})
	d := gitcacheAuthzDeps(t, ts)
	auth := NewGitCacheAuthorizer(d)
	intent := gitcache.AcquireIntent{AllowLoopback: true, ProjectID: "1", MRIID: 7, Depth: 1, Token: "tok"}
	g, err := auth.ResolveGrant(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	flip.Store(true)
	intent.ExpectedHead = g.HeadSHA
	intent.ExpectedMRVersion = g.MRVersion
	if _, err := auth.ResolveGrant(context.Background(), intent); !errors.Is(err, gitcache.ErrAuthz) {
		t.Fatalf("moved head accepted: %v", err)
	}
}

func TestGitCacheAuthorizer_depthZeroRejected(t *testing.T) {
	ts, _ := gitcacheAuthzServer(t, nil)
	d := gitcacheAuthzDeps(t, ts)
	auth := NewGitCacheAuthorizer(d)
	intent := gitcache.AcquireIntent{AllowLoopback: true, ProjectID: "1", MRIID: 7, Depth: 0, Token: "tok"}
	if _, err := auth.ResolveGrant(context.Background(), intent); !errors.Is(err, gitcache.ErrLimit) {
		t.Fatalf("depth 0: %v", err)
	}
}

func TestGitCacheAuthorizer_movedOwnerProjectFailClosed(t *testing.T) {
	ts, _ := gitcacheAuthzServer(t, func(path string, w http.ResponseWriter) bool {
		if strings.Contains(path, "/merge_requests/7") {
			_, _ = io.WriteString(w, `{
				"id":70,"iid":7,"project_id":99,"source_project_id":2,"target_project_id":99,
				"source_branch":"feature","target_branch":"main",
				"diff_refs":{"base_sha":"`+authzBase+`","head_sha":"`+authzHead+`","start_sha":"`+authzStart+`"}
			}`)
			return true
		}
		return false
	})
	d := gitcacheAuthzDeps(t, ts)
	auth := NewGitCacheAuthorizer(d)
	intent := gitcache.AcquireIntent{AllowLoopback: true, ProjectID: "1", MRIID: 7, Depth: 1, Token: "tok"}
	if _, err := auth.ResolveGrant(context.Background(), intent); !errors.Is(err, gitcache.ErrAuthz) {
		t.Fatalf("moved owner project accepted: %v", err)
	}
}

func TestGitCacheAuthorizer_missingDiffRefsFailClosed(t *testing.T) {
	ts, _ := gitcacheAuthzServer(t, func(path string, w http.ResponseWriter) bool {
		if strings.Contains(path, "/merge_requests/7") {
			_, _ = io.WriteString(w, `{
				"id":70,"iid":7,"project_id":1,"source_project_id":2,"target_project_id":1,
				"source_branch":"feature","target_branch":"main",
				"diff_refs":{"base_sha":"short","head_sha":"`+authzHead+`","start_sha":"`+authzStart+`"}
			}`)
			return true
		}
		return false
	})
	d := gitcacheAuthzDeps(t, ts)
	auth := NewGitCacheAuthorizer(d)
	intent := gitcache.AcquireIntent{AllowLoopback: true, ProjectID: "1", MRIID: 7, Depth: 1, Token: "tok"}
	if _, err := auth.ResolveGrant(context.Background(), intent); !errors.Is(err, gitcache.ErrAuthz) {
		t.Fatalf("short diff ref accepted: %v", err)
	}
}

func TestGitCacheAuthorizer_missingTargetProjectIDFailClosed(t *testing.T) {
	ts, _ := gitcacheAuthzServer(t, func(path string, w http.ResponseWriter) bool {
		if strings.Contains(path, "/merge_requests/7") {
			_, _ = io.WriteString(w, `{
				"id":70,"iid":7,"project_id":1,"source_project_id":2,
				"source_branch":"feature","target_branch":"main",
				"diff_refs":{"base_sha":"`+authzBase+`","head_sha":"`+authzHead+`","start_sha":"`+authzStart+`"}
			}`)
			return true
		}
		return false
	})
	d := gitcacheAuthzDeps(t, ts)
	auth := NewGitCacheAuthorizer(d)
	intent := gitcache.AcquireIntent{AllowLoopback: true, ProjectID: "1", MRIID: 7, Depth: 1, Token: "tok"}
	if _, err := auth.ResolveGrant(context.Background(), intent); !errors.Is(err, gitcache.ErrAuthz) {
		t.Fatalf("missing target_project_id accepted: %v", err)
	}
}

func TestGitCacheAuthorizer_sourcePathMismatchFailClosed(t *testing.T) {
	ts, _ := gitcacheAuthzServer(t, func(path string, w http.ResponseWriter) bool {
		if strings.Contains(path, "/projects/2") {
			_, _ = io.WriteString(w, `{
				"id":2,"path_with_namespace":"u/fork",
				"http_url_to_repo":"https://gitlab.example/other/path.git",
				"ssh_url_to_repo":"ssh://git@gitlab.example/other/path.git",
				"namespace":{"id":11,"kind":"group","full_path":"u","parent_id":10}
			}`)
			return true
		}
		return false
	})
	d := gitcacheAuthzDeps(t, ts)
	auth := NewGitCacheAuthorizer(d)
	intent := gitcache.AcquireIntent{AllowLoopback: true, ProjectID: "1", MRIID: 7, Depth: 1, Token: "tok"}
	if _, err := auth.ResolveGrant(context.Background(), intent); !errors.Is(err, gitcache.ErrAuthz) {
		t.Fatalf("clone URL path mismatch accepted: %v", err)
	}
}

func TestGitCacheAuthorizer_fetchTokenMustMatchAPI(t *testing.T) {
	ts, _ := gitcacheAuthzServer(t, nil)
	d := gitcacheAuthzDeps(t, ts)
	auth := NewGitCacheAuthorizer(d)
	intent := gitcache.AcquireIntent{AllowLoopback: true, ProjectID: "1", MRIID: 7, Depth: 1, Token: "different-token"}
	if _, err := auth.ResolveGrant(context.Background(), intent); !errors.Is(err, gitcache.ErrAuthz) {
		t.Fatalf("mismatched API/fetch token accepted: %v", err)
	}
}

func TestGitCacheAuthorizer_originHostMismatch(t *testing.T) {
	ts, _ := gitcacheAuthzServer(t, func(path string, w http.ResponseWriter) bool {
		if strings.Contains(path, "/projects/2") {
			_, _ = io.WriteString(w, `{
				"id":2,"path_with_namespace":"u/fork",
				"http_url_to_repo":"https://evil.example/u/fork.git",
				"ssh_url_to_repo":"ssh://git@evil.example/u/fork.git",
				"namespace":{"id":11,"kind":"group","full_path":"u","parent_id":10}
			}`)
			return true
		}
		return false
	})
	d := gitcacheAuthzDeps(t, ts)
	auth := NewGitCacheAuthorizer(d)
	intent := gitcache.AcquireIntent{AllowLoopback: true, ProjectID: "1", MRIID: 7, Depth: 1, Token: "tok"}
	if _, err := auth.ResolveGrant(context.Background(), intent); !errors.Is(err, gitcache.ErrAuthz) {
		t.Fatalf("origin mismatch accepted: %v", err)
	}
}

func TestGitCacheAuthorizer_canonicalInstancePortAndPrefix(t *testing.T) {
	ts, _ := gitcacheAuthzServer(t, func(path string, w http.ResponseWriter) bool {
		if strings.HasSuffix(path, "/projects/1") || strings.HasSuffix(path, "/projects/2") {
			id, repo, ns := 1, "g/p", `{"id":10,"kind":"group","full_path":"g","parent_id":0}`
			if strings.HasSuffix(path, "/projects/2") {
				id = 2
				repo = "u/fork"
				ns = `{"id":11,"kind":"group","full_path":"u","parent_id":10}`
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "path_with_namespace": repo, "http_url_to_repo": "https://gitlab.example:8443/gitlab/" + repo + ".git", "ssh_url_to_repo": "ssh://git@gitlab.example/" + repo + ".git", "namespace": json.RawMessage(ns)})
			return true
		}
		return false
	})
	d := gitcacheAuthzDeps(t, ts)
	d.Config.APIURL = "https://gitlab.example:8443/gitlab/api/v4"
	auth := NewGitCacheAuthorizer(d)
	intent := gitcache.AcquireIntent{AllowLoopback: true, ProjectID: "1", MRIID: 7, Depth: 1, Token: "tok"}
	g, err := auth.ResolveGrant(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if g.CanonicalInstance != "https://gitlab.example:8443/gitlab/api/v4" {
		t.Fatalf("canonical instance lost port/prefix: %q", g.CanonicalInstance)
	}
	if g.OriginHost != "gitlab.example" {
		t.Fatalf("origin host %q", g.OriginHost)
	}
	alt := g
	alt.CanonicalInstance = "https://gitlab.example/api/v4"
	if gitcache.GrantFingerprint(alt) == gitcache.GrantFingerprint(g) {
		t.Fatal("fingerprint ignores full instance port/prefix")
	}
}

func TestGitCacheAuthorizer_midAPICancelPreservesSentinel(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	ts, _ := gitcacheAuthzServer(t, func(path string, w http.ResponseWriter) bool {
		if strings.HasSuffix(path, "/user") {
			close(started)
			select {
			case <-release:
			case <-time.After(5 * time.Second):
			}
			_, _ = io.WriteString(w, `{"id":9,"username":"actor"}`)
			return true
		}
		return false
	})
	d := gitcacheAuthzDeps(t, ts)
	auth := NewGitCacheAuthorizer(d)
	intent := gitcache.AcquireIntent{AllowLoopback: true, ProjectID: "1", MRIID: 7, Depth: 1, Token: "tok"}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := auth.ResolveGrant(ctx, intent)
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("API call never reached /user")
	}
	cancel()
	err := <-errCh
	close(release)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-API cancel collapsed to ErrAuthz (R8): %v", err)
	}
}
