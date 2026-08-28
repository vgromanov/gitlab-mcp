package gitlab

import (
	"net/http"
	"net/url"
	"testing"

	retryablehttp "github.com/hashicorp/go-retryablehttp"
)

func TestNormalizeSearchType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"  ", "", false},
		{"zoekt", "zoekt", false},
		{" Zoekt ", "zoekt", false},
		{"basic", "basic", false},
		{"advanced", "advanced", false},
		{"lucene", "", true},
	}
	for _, tc := range cases {
		got, err := NormalizeSearchType(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("NormalizeSearchType(%q): want error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("NormalizeSearchType(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("NormalizeSearchType(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestWithSearchType(t *testing.T) {
	t.Parallel()
	req, err := retryablehttp.NewRequest(http.MethodGet, "https://example.test/api/v4/search?scope=blobs&search=foo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := WithSearchType("zoekt")(req); err != nil {
		t.Fatal(err)
	}
	q, err := url.ParseQuery(req.URL.RawQuery)
	if err != nil {
		t.Fatal(err)
	}
	if q.Get("search_type") != "zoekt" {
		t.Fatalf("search_type=%q", q.Get("search_type"))
	}
	if q.Get("scope") != "blobs" || q.Get("search") != "foo" {
		t.Fatalf("lost existing query: %v", q)
	}
}
