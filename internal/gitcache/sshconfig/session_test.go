package sshconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDormantControlPathDoesNotBlock(t *testing.T) {
	in := fixture(t, "User git\nPort 7999\nControlMaster no\nControlPath ~/.ssh/mux-%h-%p-%r\nControlPersist 10m\n", "")
	c := resolve(t, in)
	if !c.ControlPathDormant || c.ControlPath != filepath.Join(in.Home, ".ssh/mux-git.example-7999-git") {
		t.Fatal("dormant control settings not resolved")
	}
	if err := os.WriteFile(c.ControlPath, []byte("synthetic existing path"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(in); err == nil || !strings.Contains(err.Error(), "existing control path") {
		t.Fatal("existing control path silently bypassed")
	}
}
func TestActiveControlMasterRefused(t *testing.T) {
	for _, mode := range []string{"yes", "auto", "ask", "autoask"} {
		if _, err := Resolve(fixture(t, "ControlMaster "+mode+"\nControlPath none\n", "")); err == nil || !strings.Contains(err.Error(), "connection sharing") {
			t.Fatal("active sharing accepted")
		}
	}
	if _, err := Resolve(fixture(t, "ControlMaster no\nControlPersist invalid\n", "")); err == nil {
		t.Fatal("malformed dormant option accepted")
	}
}
func TestWarningPreferenceDoesNotChangeIdentityOrTrust(t *testing.T) {
	in := fixture(t, "IgnoreUnknown WarnWeakCrypto\nWarnWeakCrypto no\nIdentitiesOnly yes\nStrictHostKeyChecking no\nUpdateHostKeys yes\n", "")
	c := resolve(t, in)
	if c.WarnWeakCrypto != "no" || !c.IdentitiesOnly || c.StrictHostKeyChecking != "no" || c.UpdateHostKeys != "yes" {
		t.Fatal("display option altered behavior")
	}
	if err := CheckHostKeyUpdateSupport(c); err != nil {
		t.Fatal(err)
	}
}
func TestEnvironmentRequestsAndPrecedence(t *testing.T) {
	in := fixture(t, `SetEnv TEST_A=fixed TEST_EMPTY="" TEST_TILDE=~literal TEST_HOST=%h
SetEnv TEST_IGNORED=later
SendEnv LC_* LANG
SendEnv -LC_* TEST_*
`, "SendEnv SYS_*\nSetEnv TEST_IGNORED=system\n")
	c := resolve(t, in)
	got := SessionEnvironment(c, []string{"LC_ALL=excluded", "LANG=locale", "TEST_A=inherited", "TEST_OTHER=inherited-other", "SYS_LANG=system", "UNSELECTED=secret-marker"})
	want := []EnvironmentVariable{{"LANG", "locale"}, {"SYS_LANG", "system"}, {"TEST_A", "fixed"}, {"TEST_EMPTY", ""}, {"TEST_HOST", "git.example"}, {"TEST_OTHER", "inherited-other"}, {"TEST_TILDE", "~literal"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("environment request set or precedence mismatch")
	}
}
func TestEnvironmentErrorsNeverExposeValue(t *testing.T) {
	for _, setting := range []string{"SetEnv NO_EQUALS_secret-marker", "SetEnv TEST=secret-marker%C", "SetEnv TEST=${MISSING_SECRET_MARKER}"} {
		_, err := Resolve(fixture(t, setting, ""))
		if err == nil {
			t.Fatal("malformed environment setting accepted")
		}
		if strings.Contains(err.Error(), "secret-marker") || strings.Contains(err.Error(), "MISSING_SECRET_MARKER") {
			t.Fatal("environment error leaked value")
		}
	}
}
