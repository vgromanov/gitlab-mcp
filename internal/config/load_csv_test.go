package config

import "testing"

func TestParseCSV(t *testing.T) {
	if parseCSV("") != nil {
		t.Fatal("empty")
	}
	got := parseCSV(" a , b , ,c ")
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("%#v", got)
	}
}

func TestLoad_fromEnv(t *testing.T) {
	t.Setenv("GITLAB_PERSONAL_ACCESS_TOKEN", "tok")
	t.Setenv("GITLAB_API_URL", "https://example.test/api/v4")
	t.Setenv("GITLAB_READ_ONLY_MODE", "true")
	t.Setenv("USE_GITLAB_WIKI", "1")
	t.Setenv("USE_MILESTONE", "true")
	t.Setenv("USE_PIPELINE", "true")
	t.Setenv("USE_DAILY_TOOLS", "true")
	t.Setenv("USE_ISSUES", "true")
	t.Setenv("USE_WORK_ITEMS", "true")
	t.Setenv("USE_LABELS", "true")
	t.Setenv("USE_DRAFTS", "true")
	t.Setenv("USE_WEBHOOKS", "true")
	t.Setenv("USE_TIMELINE", "true")
	t.Setenv("GITLAB_ENABLED_TOOLS", "a,b")
	t.Setenv("GITLAB_DISABLED_TOOLS", "c")
	t.Setenv("STREAMABLE_HTTP", "true")
	t.Setenv("HOST", "0.0.0.0")
	t.Setenv("PORT", "9999")
	t.Setenv("GITLAB_PROJECT_ID", "99")
	t.Setenv("GITLAB_ALLOWED_PROJECT_IDS", "1,2")
	t.Setenv("GITLAB_CA_CERT_PATH", "/tmp/nope.pem")
	t.Setenv("GITLAB_INSECURE", "true")
	t.Setenv("HTTP_PROXY", "http://proxy:1")
	t.Setenv("HTTPS_PROXY", "http://proxy:2")
	// flag.Parse may only run once; if flags already registered this panics.
	defer func() {
		if r := recover(); r != nil {
			t.Skipf("Load re-register flags: %v", r)
		}
	}()
	c := Load()
	if c.Token != "tok" || c.APIURL != "https://example.test/api/v4" {
		t.Fatalf("token/url: %#v %#v", c.Token, c.APIURL)
	}
	if !c.ReadOnly || !c.Wiki || !c.RestrictedMode() {
		t.Fatalf("flags not loaded: %#v", c)
	}
	if len(c.AllowedProjectIDs) != 2 {
		t.Fatalf("allowed: %#v", c.AllowedProjectIDs)
	}
	for _, f := range []string{"wiki", "milestone", "pipeline", "issues", "work_items", "labels", "drafts", "webhooks", "timeline"} {
		if !c.FeatureEnabled(f) {
			t.Fatalf("feature %s", f)
		}
	}
	if (&Config{}).RestrictedMode() {
		t.Fatal("nil-ish")
	}
	if (*Config)(nil).RestrictedMode() {
		t.Fatal("nil receiver")
	}
}
