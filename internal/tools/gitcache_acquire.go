package tools

import (
	"context"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache"
)

// mrCacheSummary is the non-secret result of one merge-request acquisition.
// It has no object bytes and no comparison diff.
type mrCacheSummary struct {
	Warm         bool   `json:"warm"`
	Objects      int    `json:"objects"`
	GenerationID string `json:"generation_id"`
}

// acquireMergeRequestObjects runs the production MR acquisition when the
// cache service is enabled. A nil or disabled service returns no result and
// does no cache filesystem or network work. The intent identifies the MR
// only: depth is 2, transport is HTTPS, and AllowLoopback stays false.
func acquireMergeRequestObjects(ctx context.Context, d Deps, projectID string, mrIID int64) (*gitcache.AcquireResult, error) {
	if d.GitCache == nil || !d.GitCache.Enabled() {
		return nil, nil
	}
	if d.Config == nil || mrIID < 1 {
		return nil, gitcache.ErrAuthz
	}
	intent := gitcache.AcquireIntent{
		ProjectID: projectID,
		MRIID:     int(mrIID),
		Depth:     2,
		Token:     d.Config.Token,
		Transport: "https",
	}
	return d.GitCache.Acquire(ctx, intent, NewGitCacheAuthorizer(d))
}

// mergeRequestCacheResult keeps the GitLab merge request as the tool result
// when the cache did not run. A completed acquisition adds a summary beside
// that merge request.
func mergeRequestCacheResult(mr *gitlab.MergeRequest, acquired *gitcache.AcquireResult) any {
	if acquired == nil {
		return mr
	}
	return map[string]any{
		"merge_request": mr,
		"git_cache": mrCacheSummary{
			Warm:         acquired.Warm,
			Objects:      acquired.Objects,
			GenerationID: acquired.GenerationID,
		},
	}
}
