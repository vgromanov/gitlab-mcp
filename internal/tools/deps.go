package tools

import (
	"time"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// Deps is passed into tool registration closures.
type Deps struct {
	Config *config.Config
	Client *gitlab.Client
	// Guarded is the WithoutRetries publication/mutation/GraphQL client.
	// Nil keeps legacy single-client wiring; NewClient itself still denies
	// unsafe-method retries when used as the sole fallback.
	Guarded *gitlab.Client
	// Clock is optional; nil uses real UTC time (cursor expiry / upper bound).
	Clock cursor.Clock
	// GitCache is the optional native object cache service. Nil/disabled means
	// ordinary API tools keep working without Git. No public cache tool is
	// registered from this dependency.
	GitCache *gitcache.Service
}

func (d Deps) now() time.Time {
	if d.Clock != nil {
		return d.Clock.Now().UTC()
	}
	return time.Now().UTC()
}
