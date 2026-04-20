package tools

import (
	"github.com/vgromanov/gitlab-mcp/internal/config"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// Deps is passed into tool registration closures.
type Deps struct {
	Config *config.Config
	Client *gitlab.Client
}
