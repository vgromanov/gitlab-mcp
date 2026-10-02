package tools

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"
)

// getProjectOut is the allowlisted get_project MCP output (explicit DTO; no SDK embed).
type getProjectOut struct {
	ID                int64                   `json:"id" jsonschema:"Canonical project id"`
	Name              string                  `json:"name" jsonschema:"Project name"`
	Path              string                  `json:"path" jsonschema:"Project path"`
	PathWithNamespace string                  `json:"path_with_namespace" jsonschema:"Full path with namespace"`
	DefaultBranch     string                  `json:"default_branch" jsonschema:"Default branch name"`
	Visibility        string                  `json:"visibility" jsonschema:"Visibility (private/internal/public)"`
	WebURL            string                  `json:"web_url,omitempty" jsonschema:"Safe absolute HTTP(S) project web URL when present"`
	Archived          bool                    `json:"archived" jsonschema:"Whether the project is archived"`
	Namespace         *getProjectNamespaceOut `json:"namespace,omitempty" jsonschema:"Allowlisted namespace projection"`
}

// getProjectNamespaceOut is the nested namespace allowlist for get_project.
type getProjectNamespaceOut struct {
	ID       int64  `json:"id" jsonschema:"Namespace id"`
	Name     string `json:"name" jsonschema:"Namespace name"`
	Path     string `json:"path" jsonschema:"Namespace path"`
	FullPath string `json:"full_path" jsonschema:"Namespace full path"`
	Kind     string `json:"kind" jsonschema:"Namespace kind"`
}

// usableGetProjectResponse rejects nil/nonpositive IDs and numeric pid/response
// mismatches (post-authz content GetProject must match the canonical numeric
// pid from resolveProjectAuthz). Fixed safe error only — no raw SDK object.
func usableGetProjectResponse(p *gitlab.Project, expectPID string) error {
	if p == nil || p.ID <= 0 {
		return fmt.Errorf("%s: backend request failed", readmeta.CodeHTTPError)
	}
	if want, ok := parseStrictPositiveID(expectPID); ok && p.ID != want {
		return fmt.Errorf("%s: backend request failed", readmeta.CodeHTTPError)
	}
	return nil
}

func projectMetadataFromSDK(p *gitlab.Project) getProjectOut {
	out := getProjectOut{
		ID:                p.ID,
		Name:              p.Name,
		Path:              p.Path,
		PathWithNamespace: p.PathWithNamespace,
		DefaultBranch:     p.DefaultBranch,
		Visibility:        string(p.Visibility),
		Archived:          p.Archived,
	}
	if safe, ok := safeProjectWebURL(p.WebURL); ok {
		out.WebURL = safe
	}
	if p.Namespace != nil {
		out.Namespace = &getProjectNamespaceOut{
			ID:       p.Namespace.ID,
			Name:     p.Namespace.Name,
			Path:     p.Namespace.Path,
			FullPath: p.Namespace.FullPath,
			Kind:     p.Namespace.Kind,
		}
	}
	return out
}

// safeProjectWebURL keeps only well-formed absolute HTTP(S) URLs with a nonempty
// Hostname() (not merely Host — rejects hostless forms like https://:443/p), no
// userinfo, no control characters in the raw string or relevant parsed parts
// (scheme, hostname, decoded Path, RawPath), strict path percent-encoding, and
// no query or fragment (including empty "?" / "#"). Matching URLs are returned
// unchanged; all others are omitted. No DNS or network lookup is performed.
func safeProjectWebURL(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	if hasURLControls(raw) {
		return "", false
	}
	// Reject before parse so empty "?" / "#" cannot evade via RawQuery/Fragment.
	if strings.Contains(raw, "?") || strings.Contains(raw, "#") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return "", false
	}
	if !u.IsAbs() || u.Opaque != "" || u.User != nil {
		return "", false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	// Host may be ":443" with empty hostname — require an actual hostname/IP.
	if u.Hostname() == "" {
		return "", false
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	// Controls may appear only after percent-decoding (e.g. %0A in Path).
	if hasURLControls(u.Scheme) || hasURLControls(u.Hostname()) || hasURLControls(u.Path) || hasURLControls(u.RawPath) {
		return "", false
	}
	if !strictPathPercentEncoding(u) {
		return "", false
	}
	return raw, true
}

func hasURLControls(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return r == unicode.ReplacementChar || r < 0x20 || r == 0x7f
	})
}

// strictPathPercentEncoding rejects incomplete or invalid % sequences in the
// escaped path (net/url PathUnescape semantics; no network I/O).
func strictPathPercentEncoding(u *url.URL) bool {
	esc := u.RawPath
	if esc == "" {
		esc = u.EscapedPath()
	}
	if !strings.Contains(esc, "%") {
		return true
	}
	_, err := url.PathUnescape(esc)
	return err == nil
}
