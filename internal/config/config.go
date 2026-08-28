// Package config merges CLI flags and environment into runtime settings.
package config

import (
	"flag"
	"os"
	"strconv"
	"strings"
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
	StreamableHTTP     bool
	Host               string
	Port               string
	DefaultProjectID   string
	AllowedProjectIDs  []string
	CACertPath         string
	InsecureSkipVerify bool
	HTTPProxy          string
	HTTPSProxy         string
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
		APIURL:             envString("GITLAB_API_URL", "https://gitlab.com/api/v4"),
		Token:              envString("GITLAB_PERSONAL_ACCESS_TOKEN", ""),
		ReadOnly:           envBool("GITLAB_READ_ONLY_MODE", false),
		Wiki:               envBool("USE_GITLAB_WIKI", false),
		Milestone:          envBool("USE_MILESTONE", false),
		Pipeline:           envBool("USE_PIPELINE", false),
		UseDailyTools:      envBool("USE_DAILY_TOOLS", false),
		Issues:             envBool("USE_ISSUES", false),
		WorkItems:          envBool("USE_WORK_ITEMS", false),
		Labels:             envBool("USE_LABELS", false),
		Drafts:             envBool("USE_DRAFTS", false),
		Webhooks:           envBool("USE_WEBHOOKS", false),
		Timeline:           envBool("USE_TIMELINE", false),
		EnabledTools:       parseCSV(envString("GITLAB_ENABLED_TOOLS", "")),
		DisabledTools:      parseCSV(envString("GITLAB_DISABLED_TOOLS", "")),
		StreamableHTTP:     envBool("STREAMABLE_HTTP", false),
		Host:               envString("HOST", "127.0.0.1"),
		Port:               envString("PORT", "3002"),
		DefaultProjectID:   envString("GITLAB_PROJECT_ID", ""),
		CACertPath:         envString("GITLAB_CA_CERT_PATH", ""),
		InsecureSkipVerify: envBool("GITLAB_INSECURE", false),
		HTTPProxy:          envString("HTTP_PROXY", ""),
		HTTPSProxy:         envString("HTTPS_PROXY", ""),
	}
	if raw := envString("GITLAB_ALLOWED_PROJECT_IDS", ""); raw != "" {
		c.AllowedProjectIDs = parseCSV(raw)
	}

	var (
		flagToken      = flag.String("token", "", "GitLab PAT (overrides GITLAB_PERSONAL_ACCESS_TOKEN)")
		flagAPIURL     = flag.String("api-url", "", "GitLab API base URL")
		flagReadOnly   = flag.Bool("read-only", false, "Read-only mode")
		flagWiki       = flag.Bool("use-wiki", false, "Enable wiki tools")
		flagMilestone  = flag.Bool("use-milestone", false, "Enable milestone tools")
		flagPipeline   = flag.Bool("use-pipeline", false, "Enable pipeline tools")
		flagDaily      = flag.Bool("use-daily-tools", false, "Restricted mode: register Aug-2026 daily census tools")
		flagIssues     = flag.Bool("use-issues", false, "Restricted mode: enable issues family (also enters restricted mode)")
		flagWorkItems  = flag.Bool("use-work-items", false, "Restricted mode: enable work items family")
		flagLabels     = flag.Bool("use-labels", false, "Restricted mode: enable labels family")
		flagDrafts     = flag.Bool("use-drafts", false, "Restricted mode: enable MR drafts family")
		flagWebhooks   = flag.Bool("use-webhooks", false, "Restricted mode: enable webhooks family")
		flagTimeline   = flag.Bool("use-timeline", false, "Restricted mode: enable timeline family")
		flagEnabled    = flag.String("enabled-tools", "", "Comma-separated extra tools (enters restricted mode when non-empty)")
		flagDisabled   = flag.String("disabled-tools", "", "Comma-separated tools to exclude")
		flagStreamHTTP = flag.Bool("streamable-http", false, "Serve streamable HTTP instead of stdio")
		flagHost       = flag.String("host", "", "HTTP listen host")
		flagPort       = flag.String("port", "", "HTTP listen port")
		flagDefProject = flag.String("default-project", "", "Default project id or path")
		flagCACert     = flag.String("ca-cert", "", "Path to PEM CA bundle")
		flagInsecure   = flag.Bool("insecure", false, "Skip TLS verify (dev only)")
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

// RestrictedMode is on when USE_DAILY_TOOLS, any new family flag, or a non-empty
// GITLAB_ENABLED_TOOLS list is set. Legacy USE_PIPELINE / USE_MILESTONE /
// USE_GITLAB_WIKI alone do not enter restricted mode.
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
