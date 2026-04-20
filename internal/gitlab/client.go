// Package gitlab wires an HTTP client and GitLab SDK client from [config.Config].
package gitlab

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/url"
	"os"

	"github.com/vgromanov/gitlab-mcp/internal/config"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// NewClient builds an authenticated GitLab REST/GraphQL client.
func NewClient(cfg *config.Config) (*gitlab.Client, error) {
	opts := []gitlab.ClientOptionFunc{
		gitlab.WithBaseURL(cfg.APIURL),
	}

	if cfg.HTTPProxy != "" || cfg.HTTPSProxy != "" || cfg.CACertPath != "" || cfg.InsecureSkipVerify {
		tr := &http.Transport{
			TLSClientConfig: tlsConfig(cfg),
		}
		if cfg.HTTPSProxy != "" {
			if u, err := url.Parse(cfg.HTTPSProxy); err == nil {
				tr.Proxy = func(*http.Request) (*url.URL, error) { return u, nil }
			} else {
				tr.Proxy = http.ProxyFromEnvironment
			}
		} else if cfg.HTTPProxy != "" {
			if u, err := url.Parse(cfg.HTTPProxy); err == nil {
				tr.Proxy = func(*http.Request) (*url.URL, error) { return u, nil }
			} else {
				tr.Proxy = http.ProxyFromEnvironment
			}
		} else {
			tr.Proxy = http.ProxyFromEnvironment
		}
		opts = append(opts, gitlab.WithHTTPClient(&http.Client{Transport: tr}))
	}

	return gitlab.NewClient(cfg.Token, opts...)
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
