package gitlab

import (
	"time"

	retryablehttp "github.com/hashicorp/go-retryablehttp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// FormatUpdatedBound formats an instant for GitLab updated_after/updated_before
// query parameters. Uses RFC3339Nano so fractional seconds are preserved;
// Go strips trailing fractional zeros (whole seconds match RFC3339).
func FormatUpdatedBound(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// WithUpdatedBounds sets updated_after and/or updated_before on the request
// query after client-go encodes API options. Prefer this over *time.Time on
// List*MergeRequestsOptions: go-querystring formats time.Time as RFC3339 and
// drops fractional seconds.
func WithUpdatedBounds(after, before *time.Time) gitlab.RequestOptionFunc {
	return func(req *retryablehttp.Request) error {
		q := req.URL.Query()
		if after != nil {
			q.Set("updated_after", FormatUpdatedBound(*after))
		}
		if before != nil {
			q.Set("updated_before", FormatUpdatedBound(*before))
		}
		req.URL.RawQuery = q.Encode()
		return nil
	}
}
