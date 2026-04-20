package tools

import (
	"fmt"

	"github.com/vgromanov/gitlab-mcp/internal/config"
)

func checkAllowedProject(cfg *config.Config, projectID string) error {
	if len(cfg.AllowedProjectIDs) == 0 {
		return nil
	}
	for _, id := range cfg.AllowedProjectIDs {
		if id == projectID {
			return nil
		}
	}
	return fmt.Errorf("project_id %q is not allowed by GITLAB_ALLOWED_PROJECT_IDS", projectID)
}
