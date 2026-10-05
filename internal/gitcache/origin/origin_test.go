package origin_test

import (
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/origin"
)

func TestRejectsFileAndCredentialURLs(t *testing.T) {
	for _, raw := range []string{
		"file:///tmp/repo.git",
		"ext::ssh -p 22 host",
		"git://example.com/repo.git",
		"https://user:pass@example.com/repo.git",
		"https://oauth2:token@example.com/repo.git",
	} {
		if _, err := origin.Parse(raw, false); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestAcceptsHTTPSAndSSH(t *testing.T) {
	if _, err := origin.Parse("https://example.com/group/proj.git", false); err != nil {
		t.Fatal(err)
	}
	got, err := origin.Parse("ssh://git@example.com:7999/group/proj.git", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.User != "git" || got.Port != "7999" {
		t.Fatalf("got %#v", got)
	}
}

func TestInsecureAllowedExactHost(t *testing.T) {
	if !origin.InsecureAllowed("gitlabci.raiffeisen.ru", origin.DefaultInsecureHost) {
		t.Fatal("expected allow")
	}
	if origin.InsecureAllowed("evil.example", origin.DefaultInsecureHost) {
		t.Fatal("widened origin")
	}
}
