package sshconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReviewUnsafeUserConfigAndInclude(t *testing.T) {
	for _, include := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "include"}[include], func(t *testing.T) {
			in := fixture(t, "Host *\nUser git\n", "")
			p := in.UserConfig
			if include {
				p = filepath.Join(in.Home, "included")
				os.WriteFile(p, []byte("User git\n"), 0600)
				os.WriteFile(in.UserConfig, []byte("Include "+p+"\n"), 0600)
			}
			if err := os.Chmod(p, 0666); err != nil {
				t.Fatal(err)
			}
			if _, err := Resolve(in); err == nil {
				t.Fatal("writable user configuration accepted")
			}
		})
	}
}
func TestSystemConfigurationDoesNotInheritUserModeRule(t *testing.T) {
	in := fixture(t, "", "User system\n")
	if e := os.Chmod(in.SystemConfig, 0666); e != nil {
		t.Fatal(e)
	}
	if _, e := Resolve(in); e != nil {
		t.Fatal(e)
	}
}
func TestSystemIncludeStillChecksPermissions(t *testing.T) {
	in := fixture(t, "", "")
	p := filepath.Join(in.Home, "system-include")
	os.WriteFile(p, []byte("User system\n"), 0600)
	os.Chmod(p, 0666)
	os.WriteFile(in.SystemConfig, []byte("Include "+p+"\n"), 0600)
	if _, e := Resolve(in); e == nil {
		t.Fatal("unsafe system include accepted")
	}
}
