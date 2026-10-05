package tools

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache"
)

// F1: conflicting positive target_project_id must fail closed before role-root
// grant construction, even when the foreign project and commits pass policy.

func TestF1_ConflictingPositiveTargetRejected(t *testing.T) {
	var project3Hits atomic.Int32
	var commitHits atomic.Int32
	ts, _ := gitcacheAuthzServer(t, func(path string, w http.ResponseWriter) bool {
		switch {
		case strings.Contains(path, "/projects/1/merge_requests/7"):
			_, _ = io.WriteString(w, `{
				"id":70,"iid":7,"project_id":1,"source_project_id":2,"target_project_id":3,
				"source_branch":"feature","target_branch":"main",
				"diff_refs":{"base_sha":"`+authzBase+`","head_sha":"`+authzHead+`","start_sha":"`+authzStart+`"}
			}`)
			return true
		case strings.Contains(path, "/repository/commits/"):
			// Must precede /projects/3 — commit URLs contain that prefix.
			commitHits.Add(1)
			sha := path[strings.LastIndex(path, "/")+1:]
			_, _ = io.WriteString(w, `{"id":"`+sha+`"}`)
			return true
		case path == "/api/v4/projects/3" || strings.HasSuffix(path, "/projects/3"):
			project3Hits.Add(1)
			_, _ = io.WriteString(w, `{
				"id":3,"path_with_namespace":"other/p",
				"http_url_to_repo":"https://gitlab.example/other/p.git",
				"ssh_url_to_repo":"ssh://git@gitlab.example/other/p.git",
				"namespace":{"id":12,"kind":"group","full_path":"other","parent_id":10}
			}`)
			return true
		case strings.Contains(path, "/groups/12"):
			_, _ = io.WriteString(w, `{"id":12,"full_path":"other","parent_id":10}`)
			return true
		default:
			return false
		}
	})
	d := gitcacheAuthzDeps(t, ts)
	d.Config.AllowedProjectIDs = []string{"1", "2", "3"}
	auth := NewGitCacheAuthorizer(d)
	intent := gitcache.AcquireIntent{AllowLoopback: true, ProjectID: "1", MRIID: 7, Depth: 1, Token: "tok"}
	g, err := auth.ResolveGrant(context.Background(), intent)
	if err == nil {
		t.Fatalf("conflicting positive target accepted (F1): grant=%#v", g)
	}
	if !errors.Is(err, gitcache.ErrAuthz) {
		t.Fatalf("conflicting positive target wrong err (F1): %v", err)
	}
	// Allowlist resolution may touch project 3; role-root commit proof / grant binding must not.
	if commitHits.Load() != 0 {
		t.Fatalf("role-root commit acquisition began after target mismatch (F1): commits=%d project3_hits=%d", commitHits.Load(), project3Hits.Load())
	}
	if g.TargetProjectID == "3" || g.TargetHTTPSURL != "" || g.SourceHTTPSURL != "" {
		t.Fatalf("foreign target role binding leaked (F1): %#v", g)
	}
}

func TestF1_NilMRIdentityRejected(t *testing.T) {
	ts, _ := gitcacheAuthzServer(t, func(path string, w http.ResponseWriter) bool {
		if strings.Contains(path, "/merge_requests/7") {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `null`)
			return true
		}
		return false
	})
	d := gitcacheAuthzDeps(t, ts)
	auth := NewGitCacheAuthorizer(d)
	intent := gitcache.AcquireIntent{AllowLoopback: true, ProjectID: "1", MRIID: 7, Depth: 1, Token: "tok"}
	if _, err := auth.ResolveGrant(context.Background(), intent); !errors.Is(err, gitcache.ErrAuthz) {
		t.Fatalf("nil MR identity accepted (F1): %v", err)
	}
}

func TestF1_MalformedMRIdentityRejected(t *testing.T) {
	ts, _ := gitcacheAuthzServer(t, func(path string, w http.ResponseWriter) bool {
		if strings.Contains(path, "/merge_requests/7") {
			_, _ = io.WriteString(w, `{
				"id":70,"iid":8,"project_id":1,"source_project_id":2,"target_project_id":1,
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
		t.Fatalf("malformed MR iid accepted (F1): %v", err)
	}
}

func TestF1_ValidForkOwnerTargetAccepted(t *testing.T) {
	ts, _ := gitcacheAuthzServer(t, nil)
	d := gitcacheAuthzDeps(t, ts)
	auth := NewGitCacheAuthorizer(d)
	intent := gitcache.AcquireIntent{AllowLoopback: true, ProjectID: "1", MRIID: 7, Depth: 1, Token: "tok"}
	g, err := auth.ResolveGrant(context.Background(), intent)
	if err != nil {
		t.Fatalf("valid fork/owner target rejected (F1 control): %v", err)
	}
	if g.TargetProjectID != "1" || g.SourceFork != "2" || g.ProjectID != "1" {
		t.Fatalf("binding %#v", g)
	}
}
