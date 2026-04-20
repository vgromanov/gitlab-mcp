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

// Load parses flags then merges environment. Call from main after flag.Parse().
func Load() *Config {
	c := &Config{
		APIURL:             envString("GITLAB_API_URL", "https://gitlab.com/api/v4"),
		Token:              envString("GITLAB_PERSONAL_ACCESS_TOKEN", ""),
		ReadOnly:           envBool("GITLAB_READ_ONLY_MODE", false),
		Wiki:               envBool("USE_GITLAB_WIKI", false),
		Milestone:          envBool("USE_MILESTONE", false),
		Pipeline:           envBool("USE_PIPELINE", false),
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
		for _, p := range strings.Split(raw, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				c.AllowedProjectIDs = append(c.AllowedProjectIDs, p)
			}
		}
	}

	var (
		flagToken      = flag.String("token", "", "GitLab PAT (overrides GITLAB_PERSONAL_ACCESS_TOKEN)")
		flagAPIURL     = flag.String("api-url", "", "GitLab API base URL")
		flagReadOnly   = flag.Bool("read-only", false, "Read-only mode")
		flagWiki       = flag.Bool("use-wiki", false, "Enable wiki tools")
		flagMilestone  = flag.Bool("use-milestone", false, "Enable milestone tools")
		flagPipeline   = flag.Bool("use-pipeline", false, "Enable pipeline tools")
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

// FeatureEnabled reports gated feature flags.
func (c *Config) FeatureEnabled(name string) bool {
	switch name {
	case "wiki":
		return c.Wiki
	case "milestone":
		return c.Milestone
	case "pipeline":
		return c.Pipeline
	default:
		return true
	}
}
