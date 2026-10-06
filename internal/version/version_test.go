package version

import (
	"regexp"
	"runtime/debug"
	"strings"
	"testing"
)

func settings(kv ...string) []debug.BuildSetting {
	var out []debug.BuildSetting
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, debug.BuildSetting{Key: kv[i], Value: kv[i+1]})
	}
	return out
}

func TestVersionConstants(t *testing.T) {
	if Name == "" || Version == "" {
		t.Fatal("empty")
	}
}

func TestVCSFormat(t *testing.T) {
	const full = "bf02f09afdadf019544e6628f3febc9acf0e946b"
	const when = "2026-10-06T13:28:00Z"
	tests := []struct {
		name     string
		settings []debug.BuildSetting
		want     string
		wantTime string
	}{
		{"clean", settings("vcs.revision", full, "vcs.time", when, "vcs.modified", "false"), "0.1.0+bf02f09afdad", when},
		{"modified", settings("vcs.revision", full, "vcs.time", when, "vcs.modified", "true"), "0.1.0+bf02f09afdad-dirty", when},
		{"modified key missing is clean", settings("vcs.revision", full), "0.1.0+bf02f09afdad", ""},
		{"missing vcs.time", settings("vcs.revision", full, "vcs.modified", "false"), "0.1.0+bf02f09afdad", ""},
		{"short revision kept", settings("vcs.revision", "bf02f09"), "0.1.0+bf02f09", ""},
		{"exactly 12 chars", settings("vcs.revision", full[:12]), "0.1.0+bf02f09afdad", ""},
		{"revision is trimmed", settings("vcs.revision", " "+full+"\n"), "0.1.0+bf02f09afdad", ""},
		{"no settings", nil, "0.1.0+unknown", ""},
		{"unrelated settings only", settings("-compiler", "gc", "CGO_ENABLED", "0"), "0.1.0+unknown", ""},
		{"blank revision", settings("vcs.revision", "  ", "vcs.modified", "true"), "0.1.0+unknown", ""},
		{"modified without revision is not dirty", settings("vcs.modified", "true", "vcs.time", when), "0.1.0+unknown", when},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := ParseVCS(tc.settings)
			if got := v.Format("0.1.0"); got != tc.want {
				t.Fatalf("Format = %q, want %q", got, tc.want)
			}
			if v.Time != tc.wantTime {
				t.Fatalf("Time = %q, want %q", v.Time, tc.wantTime)
			}
		})
	}
}

var stampSuffix = regexp.MustCompile(`^(unknown|[0-9a-f]{1,12}(-dirty)?)$`)

func TestString_usesLinkTimeBase(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })
	Version = "9.9.9"
	// A test binary normally has no VCS stamp (unknown); a stamped one must
	// still match the documented shape. Either way the base is kept.
	base, suffix, ok := strings.Cut(String(), "+")
	if !ok || base != "9.9.9" || !stampSuffix.MatchString(suffix) {
		t.Fatalf("String = %q", String())
	}
}

func TestShort(t *testing.T) {
	for rev, want := range map[string]string{
		"": "unknown", "bf02f09": "bf02f09", "bf02f09afdadf019544e6628f3febc9acf0e946b": "bf02f09afdad",
	} {
		if got := (VCS{Revision: rev}).Short(); got != want {
			t.Fatalf("Short(%q) = %q, want %q", rev, got, want)
		}
	}
}
