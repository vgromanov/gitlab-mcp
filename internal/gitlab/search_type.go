package gitlab

import (
	"fmt"
	"strings"

	retryablehttp "github.com/hashicorp/go-retryablehttp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// Valid Search API search_type values for blob/code search.
var validSearchTypes = map[string]struct{}{
	"basic":    {},
	"advanced": {},
	"zoekt":    {},
}

// NormalizeSearchType trims and lowercases search_type. Empty input is OK
// (caller omits the query param). Non-empty values must be basic, advanced, or zoekt.
func NormalizeSearchType(v string) (string, error) {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" {
		return "", nil
	}
	if _, ok := validSearchTypes[v]; !ok {
		return "", fmt.Errorf("search_type must be one of basic, advanced, zoekt; got %q", v)
	}
	return v, nil
}

// WithSearchType appends search_type to the request query string.
// client-go SearchOptions exposes Ref but not SearchType (v2.20.0).
func WithSearchType(searchType string) gitlab.RequestOptionFunc {
	return func(req *retryablehttp.Request) error {
		q := req.URL.Query()
		q.Set("search_type", searchType)
		req.URL.RawQuery = q.Encode()
		return nil
	}
}
