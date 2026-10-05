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

func TestAcceptsGitLabSCP(t *testing.T) {
	got, err := origin.Parse("git@example.com:group/proj.git", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Scheme != "ssh" || got.User != "git" || got.Host != "example.com" || got.Port != "22" || got.Path != "/group/proj.git" {
		t.Fatalf("%#v", got)
	}
	got, err = origin.Parse("git@[2001:db8::1]:group/proj.git", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != "2001:db8::1" || got.Path != "/group/proj.git" || got.User != "git" || got.Port != "22" {
		t.Fatalf("%#v", got)
	}
	for _, raw := range []string{
		"git@example.com:group/../proj.git",
		"git@example.com:group/./proj.git",
		"git@example.com:group//proj.git",
		"git:token@example.com:group/proj.git",
		"git@[::1]:group/proj.git",
		"user:secret@example.com:group/proj.git",
	} {
		if _, err := origin.Parse(raw, false); err == nil {
			t.Fatalf("accepted %q", raw)
		}
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
