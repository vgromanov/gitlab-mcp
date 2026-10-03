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

// TestLoad_allowedGroupIDs asserts legacy Load/parseCSV preserves duplicate raw
// CSV tokens. Pre-RVG-126 this fixture expected len==2 and was masked by
// panic-to-Skip on flag.CommandLine re-registration; primary race suite under
// isolated Load hygiene exposed the stale expectation (coordinator log
// coordinator-primary-suite-failure-v1.log). Production parseCSV must NOT
// globally dedupe — PolicyFingerprint normalization dedupes for policy identity.
func TestLoad_allowedGroupIDs(t *testing.T) {
	clearKnownConfigEnv(t)
	t.Setenv("GITLAB_PERSONAL_ACCESS_TOKEN", "tok")
	t.Setenv("GITLAB_ALLOWED_GROUP_IDS", "10, 20 ,10")
	t.Setenv("GITLAB_ALLOWED_PROJECT_IDS", "")
	withIsolatedFlagCommandLine(t, nil)
	c := Load()

	wantRaw := []string{"10", "20", "10"}
	if len(c.AllowedGroupIDs) != len(wantRaw) {
		t.Fatalf("raw groups len=%d want %d; got %#v", len(c.AllowedGroupIDs), len(wantRaw), c.AllowedGroupIDs)
	}
	for i, w := range wantRaw {
		if c.AllowedGroupIDs[i] != w {
			t.Fatalf("raw groups[%d]=%q want %q; full %#v", i, c.AllowedGroupIDs[i], w, c.AllowedGroupIDs)
		}
	}
	if !c.PolicyActive() {
		t.Fatal("expected group policy active")
	}

	// Canonical policy identity matches deduped [10,20] (order-normalized).
	canonical := &Config{AllowedGroupIDs: []string{"10", "20"}}
	if c.PolicyFingerprint() != canonical.PolicyFingerprint() {
		t.Fatalf("fingerprint after raw duplicates must equal canonical [10,20]: got %s want %s",
			c.PolicyFingerprint(), canonical.PolicyFingerprint())
	}
	if c.PolicyFingerprint() == (&Config{}).PolicyFingerprint() {
		t.Fatal("group allowlist must change fingerprint vs empty")
	}
}
