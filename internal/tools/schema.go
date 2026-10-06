// Package tools registers GitLab-backed MCP tool handlers.
package tools

import (
	"fmt"
	"strconv"
	"strings"
)

// ResolveProjectID returns explicit project or default from config.
func ResolveProjectID(explicit string, defaultPID string) (string, error) {
	pid := strings.TrimSpace(explicit)
	if pid == "" {
		pid = strings.TrimSpace(defaultPID)
	}
	if pid == "" {
		return "", fmt.Errorf("project_id is required (or set GITLAB_PROJECT_ID / --default-project)")
	}
	return pid, nil
}

// ProjectGID builds a GitLab GraphQL global id for a project numeric id.
func ProjectGID(numericID int64) string {
	return fmt.Sprintf("gid://gitlab/Project/%d", numericID)
}

// Pagination holds list pagination with clamped per_page.
type Pagination struct {
	Page    int `json:"page,omitempty" jsonschema:"Page number (1-based, default 1)"`
	PerPage int `json:"per_page,omitempty" jsonschema:"Items per page (max 100)"`
}

// ListOpts returns gitlab ListOptions with defaults.
func (p Pagination) ListOpts() (page, perPage int) { return p.listOptsDefault(20) }

// listOptsDefault is ListOpts with a caller-chosen per_page default.
func (p Pagination) listOptsDefault(def int) (page, perPage int) {
	page = p.Page
	if page < 1 {
		page = 1
	}
	perPage = p.PerPage
	if perPage < 1 {
		perPage = def
	}
	if perPage > 100 {
		perPage = 100
	}
	return page, perPage
}

// pageInfo reports what a single list request returned. complete is true only
// when GitLab sent no next page; the server never pages past what was asked.
func pageInfo(page, perPage int, nextPage int64) map[string]any {
	return map[string]any{"page": page, "per_page": perPage, "next_page": nextPage, "complete": nextPage == 0}
}

// TruncateLines limits diff/trace text (best-effort line split).
func TruncateLines(s string, maxLines int) string {
	if maxLines <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}
	return strings.Join(lines[:maxLines], "\n") + fmt.Sprintf("\n... truncated (%d more lines)", len(lines)-maxLines)
}

// ParseID parses string or number project / entity ids for logging.
func ParseID(s string) string {
	return strings.TrimSpace(s)
}

// IntFromAny coerces json number / string to int (for IIDs).
func IntFromAny(v any) (int, error) {
	switch x := v.(type) {
	case nil:
		return 0, fmt.Errorf("missing value")
	case float64:
		return int(x), nil
	case int:
		return x, nil
	case int64:
		return int(x), nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		if err != nil {
			return 0, fmt.Errorf("invalid integer: %q", x)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("unsupported type %T", v)
	}
}
