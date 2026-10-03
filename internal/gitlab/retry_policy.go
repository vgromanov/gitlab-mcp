package gitlab

import (
	"context"
	"net/http"
)

// safeReadCheckRetry is the production NewClient retry policy.
//
// Legacy SDK default (retryHTTPCheck) retried 429/5xx for every HTTP method,
// including POST/PUT/PATCH/DELETE and therefore raw GraphQL POST. This policy
// only retries GET/HEAD when the method is known from resp.Request; if the
// method is unavailable (typical ambiguous transport failure), it fails closed.
func safeReadCheckRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	method := ""
	if resp != nil && resp.Request != nil {
		method = resp.Request.Method
	}
	if method == "" {
		// Fail closed: never assume a safe read on method-less transport errors.
		return false, nil
	}
	switch method {
	case http.MethodGet, http.MethodHead:
		// eligible for bounded retry below
	default:
		// Unsafe methods and GraphQL POST never retry on the NewClient fallback.
		return false, nil
	}

	if err != nil {
		// Method is known GET/HEAD: allow retry of transport failures.
		return true, nil
	}
	if resp == nil {
		return false, nil
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return true, nil
	}
	if resp.StatusCode == 0 || (resp.StatusCode >= 500 && resp.StatusCode != http.StatusNotImplemented) {
		return true, nil
	}
	return false, nil
}
