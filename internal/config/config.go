// Package config merges CLI flags and environment into runtime settings.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
)

// Config holds runtime configuration (env + CLI; CLI wins when set).
type Config struct {
	Token              string
	APIURL             string
	ReadOnly           bool
	Wiki               bool
	Milestone          bool
	Pipeline           bool
	UseDailyTools      bool
	Issues             bool
	WorkItems          bool
	Labels             bool
	Drafts             bool
	Webhooks           bool
	Timeline           bool
	EnabledTools       []string
	DisabledTools      []string
	ToolProfile        string // GITLAB_TOOL_PROFILE: "", daily, review_read, review_write
	StreamableHTTP     bool
	Host               string
	Port               string
	DefaultProjectID   string
	AllowedProjectIDs  []string
	AllowedGroupIDs    []string
	CACertPath         string
	InsecureSkipVerify bool
	HTTPProxy          string
	HTTPSProxy         string
	// CursorKey is the raw operator secret from GITLAB_MCP_CURSOR_KEY.
	// Empty allows legacy startup; cursor-dependent paths fail closed at use.
	CursorKey []byte

	// Native object cache (RVG-131). Disabled by default. Separate from API TLS.
	GitCacheEnabled             bool
	GitCacheRoot                string
	GitCacheQuotaBytes          int64
	GitCacheCACertPath          string
	GitCacheInsecure            bool
	GitCacheAllowedInsecureHost string

	// gitCacheQuotaErr is set when an explicit quota is malformed or negative.
	// Unset quota stays zero and later selects the derived default.
	gitCacheQuotaErr error
}

func envBool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envString(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt64(key string, def int64) int64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

// parseExplicitQuota accepts an empty value as unset (zero). A present value
// must be a non-negative integer; malformed and negative values are errors.
func parseExplicitQuota(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("GITLAB_MCP_GIT_CACHE_QUOTA_BYTES must be a non-negative integer")
	}
	return n, nil
}

func parseCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Load parses flags then merges environment. Call from main after flag.Parse().
func Load() *Config {
	c := &Config{
		APIURL:                      envString("GITLAB_API_URL", "https://gitlab.com/api/v4"),
		Token:                       envString("GITLAB_PERSONAL_ACCESS_TOKEN", ""),
		ReadOnly:                    envBool("GITLAB_READ_ONLY_MODE", false),
		Wiki:                        envBool("USE_GITLAB_WIKI", false),
		Milestone:                   envBool("USE_MILESTONE", false),
		Pipeline:                    envBool("USE_PIPELINE", false),
		UseDailyTools:               envBool("USE_DAILY_TOOLS", false),
		Issues:                      envBool("USE_ISSUES", false),
		WorkItems:                   envBool("USE_WORK_ITEMS", false),
		Labels:                      envBool("USE_LABELS", false),
		Drafts:                      envBool("USE_DRAFTS", false),
		Webhooks:                    envBool("USE_WEBHOOKS", false),
		Timeline:                    envBool("USE_TIMELINE", false),
		EnabledTools:                parseCSV(envString("GITLAB_ENABLED_TOOLS", "")),
		DisabledTools:               parseCSV(envString("GITLAB_DISABLED_TOOLS", "")),
		ToolProfile:                 NormalizeToolProfile(envString("GITLAB_TOOL_PROFILE", "")),
		StreamableHTTP:              envBool("STREAMABLE_HTTP", false),
		Host:                        envString("HOST", "127.0.0.1"),
		Port:                        envString("PORT", "3002"),
		DefaultProjectID:            envString("GITLAB_PROJECT_ID", ""),
		CACertPath:                  envString("GITLAB_CA_CERT_PATH", ""),
		InsecureSkipVerify:          envBool("GITLAB_INSECURE", false),
		HTTPProxy:                   envString("HTTP_PROXY", ""),
		HTTPSProxy:                  envString("HTTPS_PROXY", ""),
		GitCacheEnabled:             envBool("GITLAB_MCP_GIT_CACHE", false),
		GitCacheRoot:                envString("GITLAB_MCP_GIT_CACHE_ROOT", ""),
		GitCacheQuotaBytes:          0,
		GitCacheCACertPath:          envString("GITLAB_MCP_GIT_CACHE_CA_CERT_PATH", ""),
		GitCacheInsecure:            envBool("GITLAB_MCP_GIT_CACHE_INSECURE", false),
		GitCacheAllowedInsecureHost: envString("GITLAB_MCP_GIT_CACHE_INSECURE_HOST", "gitlabci.raiffeisen.ru"),
	}
	if raw := envString("GITLAB_ALLOWED_PROJECT_IDS", ""); raw != "" {
		c.AllowedProjectIDs = parseCSV(raw)
	}
	if raw := envString("GITLAB_ALLOWED_GROUP_IDS", ""); raw != "" {
		c.AllowedGroupIDs = parseCSV(raw)
	}
	// Cursor key: raw secret bytes; do not trim (operators may intentionally include spaces).
	if v, ok := os.LookupEnv("GITLAB_MCP_CURSOR_KEY"); ok {
		c.CursorKey = []byte(v)
	}
	quota, quotaErr := parseExplicitQuota(os.Getenv("GITLAB_MCP_GIT_CACHE_QUOTA_BYTES"))
	c.GitCacheQuotaBytes = quota
	c.gitCacheQuotaErr = quotaErr

	var (
		flagToken                = flag.String("token", "", "GitLab PAT (overrides GITLAB_PERSONAL_ACCESS_TOKEN)")
		flagAPIURL               = flag.String("api-url", "", "GitLab API base URL")
		flagReadOnly             = flag.Bool("read-only", false, "Read-only mode")
		flagWiki                 = flag.Bool("use-wiki", false, "Enable wiki tools")
		flagMilestone            = flag.Bool("use-milestone", false, "Enable milestone tools")
		flagPipeline             = flag.Bool("use-pipeline", false, "Enable pipeline tools")
		flagDaily                = flag.Bool("use-daily-tools", false, "Restricted mode: register Aug-2026 daily census tools")
		flagIssues               = flag.Bool("use-issues", false, "Restricted mode: enable issues family (also enters restricted mode)")
		flagWorkItems            = flag.Bool("use-work-items", false, "Restricted mode: enable work items family")
		flagLabels               = flag.Bool("use-labels", false, "Restricted mode: enable labels family")
		flagDrafts               = flag.Bool("use-drafts", false, "Restricted mode: enable MR drafts family")
		flagWebhooks             = flag.Bool("use-webhooks", false, "Restricted mode: enable webhooks family")
		flagTimeline             = flag.Bool("use-timeline", false, "Restricted mode: enable timeline family")
		flagEnabled              = flag.String("enabled-tools", "", "Comma-separated extra tools (enters restricted mode when non-empty)")
		flagDisabled             = flag.String("disabled-tools", "", "Comma-separated tools to exclude")
		flagProfile              = flag.String("tool-profile", "", "Tool profile ceiling: daily|review_read|review_write (empty clears env)")
		flagStreamHTTP           = flag.Bool("streamable-http", false, "Serve streamable HTTP instead of stdio")
		flagHost                 = flag.String("host", "", "HTTP listen host")
		flagPort                 = flag.String("port", "", "HTTP listen port")
		flagDefProject           = flag.String("default-project", "", "Default project id or path")
		flagCACert               = flag.String("ca-cert", "", "Path to PEM CA bundle")
		flagInsecure             = flag.Bool("insecure", false, "Skip TLS verify (dev only)")
		flagGitCache             = flag.Bool("git-cache", false, "Enable native object cache (disabled by default)")
		flagGitCacheRoot         = flag.String("git-cache-root", "", "Dedicated native cache root directory")
		flagGitCacheQuota        = flag.Int64("git-cache-quota-bytes", 0, "Logical disk quota for the cache root")
		flagGitCacheCA           = flag.String("git-cache-ca-cert", "", "Cache-only PEM CA file augmenting system roots")
		flagGitCacheInsecure     = flag.Bool("git-cache-insecure", false, "Cache-only insecure TLS for configured host")
		flagGitCacheInsecureHost = flag.String("git-cache-insecure-host", "", "Hostname allowed for cache-only insecure TLS")
	)
	flag.Parse()

	if *flagToken != "" {
		c.Token = *flagToken
	}
	if *flagAPIURL != "" {
		c.APIURL = *flagAPIURL
	}
	if flagVisited("read-only") {
		c.ReadOnly = *flagReadOnly
	}
	if flagVisited("use-wiki") {
		c.Wiki = *flagWiki
	}
	if flagVisited("use-milestone") {
		c.Milestone = *flagMilestone
	}
	if flagVisited("use-pipeline") {
		c.Pipeline = *flagPipeline
	}
	if flagVisited("use-daily-tools") {
		c.UseDailyTools = *flagDaily
	}
	if flagVisited("use-issues") {
		c.Issues = *flagIssues
	}
	if flagVisited("use-work-items") {
		c.WorkItems = *flagWorkItems
	}
	if flagVisited("use-labels") {
		c.Labels = *flagLabels
	}
	if flagVisited("use-drafts") {
		c.Drafts = *flagDrafts
	}
	if flagVisited("use-webhooks") {
		c.Webhooks = *flagWebhooks
	}
	if flagVisited("use-timeline") {
		c.Timeline = *flagTimeline
	}
	if flagVisited("enabled-tools") {
		c.EnabledTools = parseCSV(*flagEnabled)
	}
	if flagVisited("disabled-tools") {
		c.DisabledTools = parseCSV(*flagDisabled)
	}
	if flagVisited("tool-profile") {
		// Visited empty string clears an env-derived profile.
		applyToolProfileFlag(c, true, *flagProfile)
	}
	if flagVisited("streamable-http") {
		c.StreamableHTTP = *flagStreamHTTP
	}
	if *flagHost != "" {
		c.Host = *flagHost
	}
	if *flagPort != "" {
		c.Port = *flagPort
	}
	if *flagDefProject != "" {
		c.DefaultProjectID = *flagDefProject
	}
	if *flagCACert != "" {
		c.CACertPath = *flagCACert
	}
	if flagVisited("insecure") {
		c.InsecureSkipVerify = *flagInsecure
	}
	if flagVisited("git-cache") {
		c.GitCacheEnabled = *flagGitCache
	}
	if *flagGitCacheRoot != "" {
		c.GitCacheRoot = *flagGitCacheRoot
	}
	if flagVisited("git-cache-quota-bytes") {
		if *flagGitCacheQuota < 0 {
			c.gitCacheQuotaErr = fmt.Errorf("--git-cache-quota-bytes must be a non-negative integer")
		} else {
			c.GitCacheQuotaBytes = *flagGitCacheQuota
			c.gitCacheQuotaErr = nil
		}
	}
	if *flagGitCacheCA != "" {
		c.GitCacheCACertPath = *flagGitCacheCA
	}
	if flagVisited("git-cache-insecure") {
		c.GitCacheInsecure = *flagGitCacheInsecure
	}
	if *flagGitCacheInsecureHost != "" {
		c.GitCacheAllowedInsecureHost = *flagGitCacheInsecureHost
	}

	if c.Token == "" {
		c.Token = envString("GITLAB_PERSONAL_ACCESS_TOKEN", "")
	}
	return c
}

func flagVisited(name string) bool {
	visited := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			visited = true
		}
	})
	return visited
}

// PolicyActive reports whether project and/or group allowlists are configured.
// Empty both lists is legacy allow-all and is not a security boundary.
func (c *Config) PolicyActive() bool {
	if c == nil {
		return false
	}
	return len(c.AllowedProjectIDs) > 0 || len(c.AllowedGroupIDs) > 0
}

// PolicyFingerprint returns hex(SHA256) of versioned raw allowlist config.
// Tokens are trimmed, empty dropped, deduped, and sorted. Equivalent aliases that
// differ as raw strings may produce different fingerprints (not semantic-canonical).
// Format: gitlab-mcp-policy-v1\nprojects=<csv>\ngroups=<csv>\n
func (c *Config) PolicyFingerprint() string {
	projects, groups := []string(nil), []string(nil)
	if c != nil {
		projects = normalizeFingerprintList(c.AllowedProjectIDs)
		groups = normalizeFingerprintList(c.AllowedGroupIDs)
	}
	var b strings.Builder
	b.WriteString("gitlab-mcp-policy-v1\nprojects=")
	b.WriteString(strings.Join(projects, ","))
	b.WriteString("\ngroups=")
	b.WriteString(strings.Join(groups, ","))
	b.WriteString("\n")
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func normalizeFingerprintList(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, raw := range in {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// NormalizeToolProfile trims and lowercases a GITLAB_TOOL_PROFILE value.
func NormalizeToolProfile(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// applyToolProfileFlag applies CLI --tool-profile when visited (empty clears).
func applyToolProfileFlag(c *Config, visited bool, raw string) {
	if c == nil || !visited {
		return
	}
	c.ToolProfile = NormalizeToolProfile(raw)
}

// Validate reports configuration errors that must fail closed at startup.
// Unknown ToolProfile values are rejected; empty profile is legacy behavior.
// Missing GITLAB_MCP_CURSOR_KEY is allowed (legacy tools); a present invalid key fails without echo.
func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("config is nil")
	}
	switch c.ToolProfile {
	case "", "daily", "review_read", "review_write":
	default:
		return fmt.Errorf("unknown GITLAB_TOOL_PROFILE %q; allowed: unset, daily, review_read, review_write", c.ToolProfile)
	}
	if err := cursor.ValidateKey(c.CursorKey); err != nil {
		return err
	}
	if c.gitCacheQuotaErr != nil {
		return c.gitCacheQuotaErr
	}
	if c.GitCacheEnabled {
		if strings.TrimSpace(c.GitCacheRoot) == "" {
			return fmt.Errorf("GITLAB_MCP_GIT_CACHE_ROOT is required when the native git cache is enabled")
		}
		if c.GitCacheInsecure && strings.TrimSpace(c.GitCacheAllowedInsecureHost) == "" {
			return fmt.Errorf("GITLAB_MCP_GIT_CACHE_INSECURE_HOST is required when cache insecure TLS is enabled")
		}
	}
	return nil
}

// CursorSigningEnabled reports whether a usable cursor signing key is configured.
func (c *Config) CursorSigningEnabled() bool {
	return c != nil && len(c.CursorKey) >= cursor.MinKeyBytes
}

// CursorTTL returns the fixed absolute cursor lifetime (2h).
func (c *Config) CursorTTL() time.Duration {
	return cursor.DefaultTTL
}

// RestrictedMode is on when USE_DAILY_TOOLS, any new family flag, or a non-empty
// GITLAB_ENABLED_TOOLS list is set. Legacy USE_PIPELINE / USE_MILESTONE /
// USE_GITLAB_WIKI alone do not enter restricted mode.
// Named ToolProfile selection is handled separately in tools.ShouldRegister and
// does not redefine this predicate for unset-profile legacy paths.
func (c *Config) RestrictedMode() bool {
	if c == nil {
		return false
	}
	return c.UseDailyTools ||
		len(c.EnabledTools) > 0 ||
		c.Issues || c.WorkItems || c.Labels ||
		c.Drafts || c.Webhooks || c.Timeline
}

// FeatureEnabled reports gated feature flags (legacy + new families).
func (c *Config) FeatureEnabled(name string) bool {
	switch name {
	case "wiki":
		return c.Wiki
	case "milestone":
		return c.Milestone
	case "pipeline":
		return c.Pipeline
	case "issues":
		return c.Issues
	case "work_items":
		return c.WorkItems
	case "labels":
		return c.Labels
	case "drafts":
		return c.Drafts
	case "webhooks":
		return c.Webhooks
	case "timeline":
		return c.Timeline
	default:
		return true
	}
}
