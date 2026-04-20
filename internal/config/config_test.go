package config

import (
	"strings"
	"testing"
)

func TestFeatureEnabled(t *testing.T) {
	c := &Config{Wiki: true, Milestone: false, Pipeline: true}
	if !c.FeatureEnabled("wiki") || c.FeatureEnabled("milestone") || !c.FeatureEnabled("pipeline") {
		t.Fatal("feature flags mismatch")
	}
	if !c.FeatureEnabled("unknown_should_default_true") {
		t.Fatal("unknown feature should default true")
	}
}

func TestEnvBool(t *testing.T) {
	t.Setenv("MCP_TEST_BOOL", "true")
	if !envBool("MCP_TEST_BOOL", false) {
		t.Fatal("expected true")
	}
	t.Setenv("MCP_TEST_BOOL", "garbage")
	if !envBool("MCP_TEST_BOOL", true) {
		t.Fatal("invalid should return default true")
	}
}

func TestEnvString(t *testing.T) {
	t.Setenv("MCP_TEST_STR", " hello ")
	if envString("MCP_TEST_STR", "def") != "hello" {
		t.Fatalf("got %q", envString("MCP_TEST_STR", "def"))
	}
	if envString("MCP_TEST_STR_MISSING", "def") != "def" {
		t.Fatal("default")
	}
}

func TestAllowedProjectIDs_split(t *testing.T) {
	raw := " 1 , foo/bar , "
	var ids []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			ids = append(ids, p)
		}
	}
	if len(ids) != 2 || ids[0] != "1" || ids[1] != "foo/bar" {
		t.Fatalf("got %#v", ids)
	}
}
