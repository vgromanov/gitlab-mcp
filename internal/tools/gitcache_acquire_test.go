package tools

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
)

func openAcquireTestService(t *testing.T, token string) *gitcache.Service {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	svc, err := gitcache.OpenService(gitcache.ServiceConfig{
		Enabled: true,
		Root:    root,
		Token:   token,
	})
	if errors.Is(err, gitcache.ErrPlatform) {
		t.Skip(err.Error())
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close(context.Background()) })
	return svc
}

func TestAcquireMergeRequestObjectsSkipsDisabledCache(t *testing.T) {
	got, err := acquireMergeRequestObjects(context.Background(), Deps{}, "1", 7)
	if err != nil || got != nil {
		t.Fatalf("disabled cache ran: %#v %v", got, err)
	}
}

func TestAcquireMergeRequestObjectsRejectsNilConfig(t *testing.T) {
	svc := openAcquireTestService(t, "tok")
	_, err := acquireMergeRequestObjects(context.Background(), Deps{GitCache: svc}, "1", 7)
	if !errors.Is(err, gitcache.ErrAuthz) {
		t.Fatalf("nil config: %v", err)
	}
}

func TestMergeRequestCacheResultSummary(t *testing.T) {
	mr := &gitlab.MergeRequest{}
	if mergeRequestCacheResult(mr, nil) != mr {
		t.Fatal("disabled result replaced the merge request")
	}
	out := mergeRequestCacheResult(mr, &gitcache.AcquireResult{
		Warm:         true,
		Objects:      4,
		GenerationID: "gen",
		Grant:        gitcache.Grant{ActorID: "secret-actor"},
	})
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("summary type %T", out)
	}
	if m["merge_request"] != mr {
		t.Fatal("summary dropped the merge request")
	}
	summary, ok := m["git_cache"].(mrCacheSummary)
	if !ok || !summary.Warm || summary.Objects != 4 || summary.GenerationID != "gen" {
		t.Fatalf("summary %#v", m["git_cache"])
	}
}

func TestGetMergeRequestDisabledCacheDoesNotAcquire(t *testing.T) {
	var userHits atomic.Int32
	ts, _ := gitcacheAuthzServer(t, func(path string, w http.ResponseWriter) bool {
		if strings.HasSuffix(path, "/user") {
			userHits.Add(1)
		}
		return false
	})
	d := gitcacheAuthzDeps(t, ts)
	_, out, err := getMergeRequest(context.Background(), nil, getMergeRequestIn{
		pidMR: pidMR{ProjectID: "1", MergeRequestIID: 7},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	if userHits.Load() != 0 {
		t.Fatalf("disabled cache called the authorizer %d times", userHits.Load())
	}
	payload, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("payload %T", out)
	}
	if _, exists := payload["git_cache"]; exists {
		t.Fatal("disabled cache added git_cache")
	}
	if payload["iid"] != float64(7) {
		t.Fatalf("iid %#v", payload["iid"])
	}
}

func TestGetMergeRequestEnabledCacheAcquires(t *testing.T) {
	var (
		svc      *gitcache.Service
		d        Deps
		nested   error
		once     sync.Once
		userHits atomic.Int32
	)
	ts, _ := gitcacheAuthzServer(t, func(path string, w http.ResponseWriter) bool {
		if !strings.HasSuffix(path, "/user") {
			return false
		}
		userHits.Add(1)
		once.Do(func() {
			_, nested = svc.Acquire(context.Background(), gitcache.AcquireIntent{
				ProjectID: "1", MRIID: 7, Depth: 1, Token: "tok",
			}, NewGitCacheAuthorizer(d))
		})
		return false
	})
	cli, err := gitlab.NewClient("tok",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(igl.BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	svc = openAcquireTestService(t, "tok")
	d = Deps{
		Config: &config.Config{
			Token:             "tok",
			APIURL:            ts.URL + "/api/v4",
			AllowedProjectIDs: []string{"1", "2"},
			AllowedGroupIDs:   []string{"10"},
		},
		Client:   cli,
		GitCache: svc,
	}
	_, _, err = getMergeRequest(context.Background(), nil, getMergeRequestIn{
		pidMR: pidMR{ProjectID: "1", MergeRequestIID: 7},
	}, d)
	if !errors.Is(err, gitcache.ErrAuthz) {
		t.Fatalf("enabled get_merge_request: %v", err)
	}
	if userHits.Load() == 0 {
		t.Fatal("enabled cache did not construct the merge-request authorizer")
	}
	if !errors.Is(nested, gitcache.ErrBusy) {
		t.Fatalf("acquisition was not inside Service.Acquire: %v", nested)
	}
}
