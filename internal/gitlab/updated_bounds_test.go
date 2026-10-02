package gitlab_test

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	retryablehttp "github.com/hashicorp/go-retryablehttp"
	glclient "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
)

func TestFormatUpdatedBound_preservesFractions(t *testing.T) {
	t.Parallel()
	whole, err := time.Parse(time.RFC3339Nano, "2019-03-15T08:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if got := glclient.FormatUpdatedBound(whole); got != "2019-03-15T08:00:00Z" {
		t.Fatalf("whole: %q", got)
	}
	frac, err := time.Parse(time.RFC3339Nano, "2019-03-15T08:00:00.123456789Z")
	if err != nil {
		t.Fatal(err)
	}
	if got := glclient.FormatUpdatedBound(frac); got != "2019-03-15T08:00:00.123456789Z" {
		t.Fatalf("frac: %q", got)
	}
}

func TestWithUpdatedBounds_setsQuery(t *testing.T) {
	t.Parallel()
	after, _ := time.Parse(time.RFC3339Nano, "2019-03-15T08:00:00.123Z")
	before, _ := time.Parse(time.RFC3339Nano, "2019-03-15T09:00:00Z")
	req, err := retryablehttp.NewRequest(http.MethodGet, "https://example.test/api/v4/merge_requests?state=opened", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := glclient.WithUpdatedBounds(&after, &before)(req); err != nil {
		t.Fatal(err)
	}
	q, err := url.ParseQuery(req.URL.RawQuery)
	if err != nil {
		t.Fatal(err)
	}
	if got := q.Get("updated_after"); got != "2019-03-15T08:00:00.123Z" {
		t.Fatalf("updated_after=%q", got)
	}
	if got := q.Get("updated_before"); got != "2019-03-15T09:00:00Z" {
		t.Fatalf("updated_before=%q", got)
	}
	if got := q.Get("state"); got != "opened" {
		t.Fatalf("state should remain: %q", got)
	}
}
