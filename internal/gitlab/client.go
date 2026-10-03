// Package gitlab wires an HTTP client and GitLab SDK client from [config.Config].
package gitlab

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/url"
	"os"
	"time"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"

	cleanhttp "github.com/hashicorp/go-cleanhttp"
	retryablehttp "github.com/hashicorp/go-retryablehttp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// Option configures optional production client construction knobs without
// breaking NewClient(cfg) / NewGuardedClient(cfg) call compatibility.
type Option func(*clientOptions)

type clientOptions struct {
	waitMin, waitMax time.Duration
	setWait          bool
	backoff          retryablehttp.Backoff
}

// WithRetryWaitMinMax sets the SDK retry wait window (production seam).
// When both are zero, a zero backoff is also applied so bounded GET retries
// complete promptly under local fixtures.
func WithRetryWaitMinMax(min, max time.Duration) Option {
	return func(o *clientOptions) {
		o.setWait = true
		o.waitMin, o.waitMax = min, max
		if min == 0 && max == 0 {
			o.backoff = func(min, max time.Duration, attemptNum int, resp *http.Response) time.Duration {
				return 0
			}
		}
	}
}

// NewClient builds the production read/fallback GitLab client.
//
// Legacy behavior: the previous constructor used the SDK default retry policy,
// which replayed POST/PUT/PATCH/DELETE (and GraphQL POST) on 429/5xx. NewClient
// now applies safeReadCheckRetry so unsafe methods and raw GraphQL never retry
// even when this client is the sole fallback. Prefer NewGuardedClient (and Deps
// routing) for publication/mutation/GraphQL call paths.
// Always registers a budget interceptor (no-op unless context carries a Budget).
func NewClient(cfg *config.Config, opts ...Option) (*gitlab.Client, error) {
	co := applyOptions(opts...)
	sdkOpts := append(baseClientOptions(cfg), gitlab.WithCustomRetry(safeReadCheckRetry))
	sdkOpts = append(sdkOpts, retryWaitOptions(co)...)
	return gitlab.NewClient(cfg.Token, sdkOpts...)
}

// NewGuardedClient builds a WithoutRetries client for publication, mutation,
// and all GraphQL routes. Share TLS/proxy/redirect/budget construction with
// NewClient; do not hand-edit vendor.
func NewGuardedClient(cfg *config.Config, opts ...Option) (*gitlab.Client, error) {
	co := applyOptions(opts...)
	sdkOpts := append(baseClientOptions(cfg), gitlab.WithoutRetries())
	sdkOpts = append(sdkOpts, retryWaitOptions(co)...)
	return gitlab.NewClient(cfg.Token, sdkOpts...)
}

// NewClients returns the read/fallback client and the guarded no-retry client.
func NewClients(cfg *config.Config, opts ...Option) (read, guarded *gitlab.Client, err error) {
	read, err = NewClient(cfg, opts...)
	if err != nil {
		return nil, nil, err
	}
	guarded, err = NewGuardedClient(cfg, opts...)
	if err != nil {
		return nil, nil, err
	}
	return read, guarded, nil
}

func applyOptions(opts ...Option) clientOptions {
	var co clientOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&co)
		}
	}
	return co
}

func retryWaitOptions(co clientOptions) []gitlab.ClientOptionFunc {
	var out []gitlab.ClientOptionFunc
	if co.setWait {
		out = append(out, gitlab.WithCustomRetryWaitMinMax(co.waitMin, co.waitMax))
	}
	if co.backoff != nil {
		out = append(out, gitlab.WithCustomBackoff(co.backoff))
	}
	return out
}

func baseClientOptions(cfg *config.Config) []gitlab.ClientOptionFunc {
	return []gitlab.ClientOptionFunc{
		gitlab.WithBaseURL(cfg.APIURL),
		gitlab.WithInterceptor(BudgetInterceptor()),
		gitlab.WithHTTPClient(buildHTTPClient(cfg)),
	}
}

// buildHTTPClient always injects CheckRedirect so mutating 307/308 cannot
// replay bodies and cross-origin GitLab credentials are stripped.
func buildHTTPClient(cfg *config.Config) *http.Client {
	tr := cleanhttp.DefaultPooledTransport()
	if cfg.CACertPath != "" || cfg.InsecureSkipVerify || cfg.HTTPProxy != "" || cfg.HTTPSProxy != "" {
		tr.TLSClientConfig = tlsConfig(cfg)
		tr.Proxy = proxyFunc(cfg)
	}
	return &http.Client{
		Transport:     tr,
		CheckRedirect: safeCheckRedirect,
	}
}

func proxyFunc(cfg *config.Config) func(*http.Request) (*url.URL, error) {
	if cfg.HTTPSProxy != "" {
		if u, err := url.Parse(cfg.HTTPSProxy); err == nil {
			return func(*http.Request) (*url.URL, error) { return u, nil }
		}
		return http.ProxyFromEnvironment
	}
	if cfg.HTTPProxy != "" {
		if u, err := url.Parse(cfg.HTTPProxy); err == nil {
			return func(*http.Request) (*url.URL, error) { return u, nil }
		}
		return http.ProxyFromEnvironment
	}
	return http.ProxyFromEnvironment
}

func tlsConfig(cfg *config.Config) *tls.Config {
	tc := &tls.Config{
		InsecureSkipVerify: cfg.InsecureSkipVerify,
		MinVersion:         tls.VersionTLS12,
	}
	if cfg.CACertPath != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if pemData, err := os.ReadFile(cfg.CACertPath); err == nil {
			pool.AppendCertsFromPEM(pemData)
			tc.RootCAs = pool
		}
	}
	return tc
}
