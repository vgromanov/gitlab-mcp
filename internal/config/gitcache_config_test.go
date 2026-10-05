package config

import "testing"

func TestGitCacheValidateRequiresRootWhenEnabled(t *testing.T) {
	c := &Config{GitCacheEnabled: true}
	if err := c.Validate(); err == nil {
		t.Fatal("expected root required")
	}
	c.GitCacheRoot = "/tmp/cache"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.GitCacheInsecure = true
	c.GitCacheAllowedInsecureHost = ""
	if err := c.Validate(); err == nil {
		t.Fatal("expected insecure host required")
	}
}

func TestGitCacheDefaultDisabled(t *testing.T) {
	c := &Config{}
	if c.GitCacheEnabled {
		t.Fatal("enabled by default")
	}
}
