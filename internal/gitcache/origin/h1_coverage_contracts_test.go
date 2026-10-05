package origin_test

import (
	"errors"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/origin"
)

func TestCoverage_OriginRejectsLoopbackEmptyPathAndHTTP(t *testing.T) {
	if _, err := origin.Parse("https://127.0.0.1/repo.git", false); !errors.Is(err, origin.ErrHost) {
		t.Fatalf("loopback: %v", err)
	}
	if _, err := origin.Parse("https://localhost/repo.git", false); !errors.Is(err, origin.ErrHost) {
		t.Fatalf("localhost: %v", err)
	}
	if _, err := origin.Parse("https://example.com", false); !errors.Is(err, origin.ErrHost) {
		t.Fatalf("empty path: %v", err)
	}
	if _, err := origin.Parse("https://user@example.com/repo.git", false); !errors.Is(err, origin.ErrUserinfo) {
		t.Fatalf("https user: %v", err)
	}
	got, err := origin.Parse("ssh://example.com/repo.git", false)
	if err != nil || got.User != "git" || got.Port != "22" {
		t.Fatalf("ssh defaults: %#v %v", got, err)
	}
	if _, err := origin.Parse("http://example.com/repo.git", false); err == nil {
		t.Fatal("http accepted")
	}
	if _, err := origin.Parse("https://0.0.0.0/repo.git", false); !errors.Is(err, origin.ErrHost) {
		t.Fatalf("unspecified: %v", err)
	}
	loop, err := origin.Parse("https://127.0.0.1/repo.git", true)
	if err != nil || loop.Host != "127.0.0.1" {
		t.Fatalf("loopback allow: %#v %v", loop, err)
	}
}
