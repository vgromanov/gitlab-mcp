package sshconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fixture(t *testing.T, user, system string) Input {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".ssh")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	up, sp := filepath.Join(dir, "config"), filepath.Join(home, "system_config")
	for p, data := range map[string]string{up: user, sp: system} {
		if err := os.WriteFile(p, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return Input{Host: "git.example", LocalUser: "local", Home: home, UserConfig: up, SystemConfig: sp, LookupEnv: func(name string) (string, bool) {
		if name == "SSH_AUTH_SOCK" {
			return "/synthetic/default-agent", true
		}
		if name == "TEST_AGENT" {
			return "/synthetic/selected-agent", true
		}
		return "", false
	}}
}
func resolve(t *testing.T, in Input) Config {
	t.Helper()
	c, err := Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMatchHostFirstValueAndIdentities(t *testing.T) {
	in := fixture(t, `Host unrelated
 ProxyCommand forbidden secret-marker
 IdentityFile /wrong
Host *.example !excluded.example
 User git
 IdentityFile "~/.ssh/one key"
Match host git.example originalhost git.example user git localuser local
 Port=7999
 IdentitiesOnly yes
 IdentityAgent $TEST_AGENT
 IdentityFile ~/.ssh/two
Match all
 User ignored
 IdentityFile ~/.ssh/two
`, "Host *\nUser system-ignored\nPort 22\nIdentityFile /system/identity\n")
	c := resolve(t, in)
	if c.User != "git" || c.Port != 7999 || !c.IdentitiesOnly || c.IdentityAgent != "/synthetic/selected-agent" {
		t.Fatal("wrong effective values")
	}
	want := []string{filepath.Join(in.Home, ".ssh/one key"), filepath.Join(in.Home, ".ssh/two"), "/system/identity"}
	if !reflect.DeepEqual(c.IdentityFiles, want) {
		t.Fatalf("identity order mismatch: %v", c.IdentityFiles)
	}
}
func TestHostNegationCaseAndOriginalHost(t *testing.T) {
	in := fixture(t, `Host *.example !git.example
 User wrong
Host GIT.EXAMPLE
 HostName real.example
Match host real.example originalhost git.example
 User git
 Port 7999
Match host !real.example,*
 User wrong
`, "")
	c := resolve(t, in)
	if c.HostName != "real.example" || c.User != "git" || c.Port != 7999 {
		t.Fatal("match substitution failed")
	}
}
func TestDefaultIdentityAndAgentSemantics(t *testing.T) {
	for _, tc := range []struct {
		body, agent string
		files       int
		only        bool
	}{
		{"", "/synthetic/default-agent", 5, false},
		{"IdentityAgent none\nIdentitiesOnly yes\n", "", 5, true},
		{"IdentityAgent SSH_AUTH_SOCK\nIdentityFile none\n", "/synthetic/default-agent", 0, false},
		{"IdentityAgent $TEST_AGENT\nIdentityFile /only\n", "/synthetic/selected-agent", 1, false},
		{"IdentityAgent ${TEST_AGENT}\nIdentityFile none\nIdentityFile /only\n", "/synthetic/selected-agent", 1, false},
	} {
		in := fixture(t, tc.body, "")
		c := resolve(t, in)
		if c.IdentityAgent != tc.agent || len(c.IdentityFiles) != tc.files || c.IdentitiesOnly != tc.only {
			t.Fatal("agent/default identity semantics mismatch")
		}
	}
}
func TestKnownHostsAndTrustPreserved(t *testing.T) {
	in := fixture(t, "StrictHostKeyChecking no\nUpdateHostKeys yes\nUserKnownHostsFile \"~/.ssh/known hosts\" /extra\nGlobalKnownHostsFile none\n", "")
	c := resolve(t, in)
	if c.StrictHostKeyChecking != "no" || c.UpdateHostKeys != "yes" || len(c.GlobalKnownHostsFiles) != 0 || len(c.UserKnownHostsFiles) != 2 {
		t.Fatal("trust silently changed")
	}
	if err := CheckHostKeyUpdateSupport(c); err != nil {
		t.Fatal(err)
	}
	c = resolve(t, fixture(t, "StrictHostKeyChecking yes\nUpdateHostKeys no\n", ""))
	if err := CheckHostKeyUpdateSupport(c); err != nil {
		t.Fatal(err)
	}
	c = resolve(t, fixture(t, "UserKnownHostsFile /custom\n", ""))
	if c.UpdateHostKeys != "no" || c.StrictHostKeyChecking != "ask" {
		t.Fatal("wrong OpenSSH trust defaults")
	}
}
func TestIncludeOrderContextAndCycle(t *testing.T) {
	in := fixture(t, "Host unrelated\nInclude missing/*\nHost *\nInclude conf.d/*\nUser later\n", "")
	dir := filepath.Join(in.Home, ".ssh/conf.d")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"10": "User first\nIdentityFile /one\n", "20": "User second\nIdentityFile /two\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c := resolve(t, in)
	if c.User != "first" || !reflect.DeepEqual(c.IdentityFiles, []string{"/one", "/two"}) {
		t.Fatal("include precedence failed")
	}
	if err := os.WriteFile(filepath.Join(dir, "30"), []byte("Include config\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(in); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle not caught: %v", err)
	}
}
func TestUnsupportedApplicableFailsWithoutEchoingValues(t *testing.T) {
	for _, body := range []string{
		"ProxyCommand run-secret-marker\n",
		"IdentityFile /known\nForwardAgent secret-marker\n",
		"IgnoreUnknown ProxyCommand\nProxyCommand secret-marker\n",
		"Match exec \"secret-marker\"\nUser git\n",
		"Match final\nUser git\n",
		"IdentityAgent $UNSET_SECRET_MARKER\n",
		"IdentityFile ~another/secret-marker\n",
		"IdentityFile /secret-marker/%C\n",
		"CanonicalizeHostname yes\n",
		"secret-marker value\n",
	} {
		c, err := Resolve(fixture(t, body, ""))
		if err == nil {
			t.Fatal("unsupported accepted")
		}
		if !reflect.DeepEqual(c, Config{}) {
			t.Fatal("partial config escaped on error")
		}
		if strings.Contains(err.Error(), "secret-marker") || strings.Contains(err.Error(), "UNSET_SECRET_MARKER") {
			t.Fatal("error exposed setting value")
		}
	}
}
func TestTokenExpansionAndQuoting(t *testing.T) {
	in := fixture(t, "User git\nPort 7999\nHostName %h.internal\nIdentityFile=\"%d/.ssh/%r-%h-%p-%% #key\" # comment\n", "")
	c := resolve(t, in)
	want := filepath.Join(in.Home, ".ssh/git-git.example.internal-7999-% #key")
	if !reflect.DeepEqual(c.IdentityFiles, []string{want}) {
		t.Fatalf("wrong expansion: %v", c.IdentityFiles)
	}
	for _, bad := range []string{"User ==git", "User \"unfinished", "Port 0", "Port 65536", "IdentitiesOnly maybe", "StrictHostKeyChecking maybe", "UpdateHostKeys maybe", "IdentityFile /one /two"} {
		if _, err := Resolve(fixture(t, bad, "")); err == nil {
			t.Fatalf("invalid accepted: %s", bad)
		}
	}
}
func TestMissingOptionalConfigAndBounds(t *testing.T) {
	in := fixture(t, "", "")
	os.Remove(in.UserConfig)
	os.Remove(in.SystemConfig)
	c := resolve(t, in)
	if c.User != "local" || c.Port != 22 {
		t.Fatal("defaults failed")
	}
	if err := os.WriteFile(in.UserConfig, []byte(strings.Repeat("#", maxFileBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(in); err == nil {
		t.Fatal("oversized config accepted")
	}
}
func TestWildcardSSHNotFilesystemGlob(t *testing.T) {
	for _, tc := range []struct {
		s, p string
		want bool
	}{{"abc", "a?c", true}, {"abcdef", "a*f", true}, {"ab", "a[bc]", false}, {"abc", "!abc", false}, {"abc", "*", true}} {
		if wildcard(tc.s, tc.p) != tc.want {
			t.Fatal("wildcard mismatch")
		}
	}
	if matches("host", []string{"!other"}, false) {
		t.Fatal("negation-only pattern matched")
	}
}

// OpenSSH 10.3p1 -G fixture comparison confirmed that an included Host block
// does not change the containing file's active section after Include returns.
func TestIncludeRestoresParentMatchContext(t *testing.T) {
	in := fixture(t, "Host git.example\nInclude child\nUser parent\n", "")
	if err := os.WriteFile(filepath.Join(in.Home, ".ssh/child"), []byte("Host unrelated\nUser child\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if c := resolve(t, in); c.User != "parent" {
		t.Fatal("included Host leaked into parent context")
	}
}

func TestUnrelatedMatchExecNeverEvaluated(t *testing.T) {
	in := fixture(t, "Match host unrelated exec \"touch /should-never-exist\"\nProxyCommand secret-marker\nMatch host git.example\nUser git\n", "")
	if c := resolve(t, in); c.User != "git" {
		t.Fatal("unrelated Match affected target")
	}
}
