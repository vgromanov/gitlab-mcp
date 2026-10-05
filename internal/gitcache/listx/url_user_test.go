package listx

import (
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/origin"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshconfig"
)

func TestSSHInputUsesURLUser(t *testing.T) {
	target := origin.Target{Host: "gitlab.example", User: "git", Port: "7999"}
	base := sshconfig.Input{LocalUser: "osuser", RemoteUser: "osuser", RemotePort: 22, Home: t.TempDir()}
	got := sshInputForTarget(target, Options{SSHConfig: &base})
	if got.RemoteUser != "git" || got.Host != "gitlab.example" || got.LocalUser != "osuser" || got.RemotePort != 7999 {
		t.Fatalf("copied input %#v", got)
	}
	if base.RemoteUser != "osuser" {
		t.Fatal("caller SSH config was mutated")
	}
	got = sshInputForTarget(target, Options{})
	if got.RemoteUser != "git" || got.Host != "gitlab.example" || got.RemotePort != 7999 {
		t.Fatalf("empty input %#v", got)
	}
}
