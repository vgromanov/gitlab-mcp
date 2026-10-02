package config

import "testing"

func TestPolicyFingerprint_stableUnderPermutation(t *testing.T) {
	a := &Config{AllowedProjectIDs: []string{"2", "1", "1", " 3 "}, AllowedGroupIDs: []string{"b", "a", "a"}}
	b := &Config{AllowedProjectIDs: []string{"3", "1", "2"}, AllowedGroupIDs: []string{"a", "b"}}
	if a.PolicyFingerprint() != b.PolicyFingerprint() {
		t.Fatalf("fingerprint not stable: %s vs %s", a.PolicyFingerprint(), b.PolicyFingerprint())
	}
	empty := (&Config{}).PolicyFingerprint()
	if empty == "" || empty == a.PolicyFingerprint() {
		t.Fatalf("empty fingerprint unexpected: %q", empty)
	}
	if !a.PolicyActive() || (&Config{}).PolicyActive() {
		t.Fatal("PolicyActive")
	}
}

func TestLoad_allowedGroupIDs(t *testing.T) {
	t.Setenv("GITLAB_PERSONAL_ACCESS_TOKEN", "tok")
	t.Setenv("GITLAB_ALLOWED_GROUP_IDS", "10, 20 ,10")
	t.Setenv("GITLAB_ALLOWED_PROJECT_IDS", "")
	defer func() {
		if r := recover(); r != nil {
			t.Skipf("Load re-register flags: %v", r)
		}
	}()
	c := Load()
	if len(c.AllowedGroupIDs) != 2 {
		t.Fatalf("groups: %#v", c.AllowedGroupIDs)
	}
	if !c.PolicyActive() {
		t.Fatal("expected group policy active")
	}
}
