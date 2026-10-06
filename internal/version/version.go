// Package version exposes build metadata for the MCP server and CLI.
package version

import (
	"runtime/debug"
	"strings"
)

// Name is the MCP implementation / binary name.
const Name = "gitlab-mcp"

// Version is injected at link time via GoReleaser ldflags.
var Version = "0.1.0"

// revisionLen is how many characters of the VCS revision String reports.
const revisionLen = 12

// VCS is the version-control stamp the Go toolchain embeds in a binary.
type VCS struct {
	Revision, Time string // vcs.revision (full commit id), vcs.time (RFC 3339)
	Modified       bool   // vcs.modified: the tree had uncommitted changes
}

// ParseVCS extracts the vcs.* build settings; missing keys stay zero.
func ParseVCS(settings []debug.BuildSetting) VCS {
	var v VCS
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			v.Revision = strings.TrimSpace(s.Value)
		case "vcs.time":
			v.Time = strings.TrimSpace(s.Value)
		case "vcs.modified":
			v.Modified = s.Value == "true"
		}
	}
	return v
}

// Short is the first revisionLen characters of the commit id, or "unknown".
func (v VCS) Short() string {
	if v.Revision == "" {
		return "unknown"
	}
	return v.Revision[:min(len(v.Revision), revisionLen)]
}

// Format returns base+<revision>, with "-dirty" appended for a modified tree
// and "unknown" instead of the revision when there is none.
func (v VCS) Format(base string) string {
	rev := v.Short()
	if v.Modified && v.Revision != "" {
		rev += "-dirty"
	}
	return base + "+" + rev
}

// Build reads the VCS stamp of the running binary (zero when it has none).
func Build() VCS {
	if bi, ok := debug.ReadBuildInfo(); ok {
		return ParseVCS(bi.Settings)
	}
	return VCS{}
}

// String is the version reported to MCP clients and by -version, for example
// "0.1.0+bf02f09abcde" or "0.1.0+bf02f09abcde-dirty".
func String() string { return Build().Format(Version) }
