// Package tlsx builds TLS configs for native cache HTTPS acquisition.
// A CA file augments the system roots. It does not replace them.
// Cache insecure mode is separate from API GITLAB_INSECURE and requires an
// exact configured AllowedInsecureHost match.
package tlsx

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"sync"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/origin"
)

var (
	ErrMalformedCA = errors.New("CA file is malformed or contains no certificates")
	ErrInsecure    = errors.New("cache insecure TLS is only allowed for the configured canonical origin")
)

// maxCABytes is the finite explicit-CA read budget (descriptor-validated).
const maxCABytes int64 = 1 << 20

// Input is the TLS material for one connection.
// Insecure defaults to false. CAPath empty means system roots only.
// AllowedInsecureHost must be set when Insecure is true; empty falls back to
// origin.DefaultInsecureHost only as the named corporate default for tests.
type Input struct {
	ServerName          string
	CAPath              string
	Insecure            bool
	AllowedInsecureHost string
}

// Build returns a tls.Config.
// CAPath, when set, loads PEM certificates and appends them to a copy of the
// system pool. A missing, empty, non-regular, oversize, or non-PEM file fails
// closed and does not fall back to system roots alone. Insecure sets
// InsecureSkipVerify only when ServerName equals AllowedInsecureHost (or
// DefaultInsecureHost when unset).
// One immutable system-root snapshot per process. The random epoch prevents a
// persisted warm key from claiming equivalence across OS trust-store reloads.
var systemOnce sync.Once
var systemPool *x509.CertPool
var systemErr error
var systemEpoch [32]byte

func Build(in Input) (*tls.Config, error) {
	cfg, _, err := Prepare(context.Background(), in)
	return cfg, err
}

// Prepare validates trust before warm lookup and returns the exact immutable
// config used for a cold connection plus a non-secret trust-content fingerprint.
func Prepare(ctx context.Context, in Input) (*tls.Config, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	allowed := strings.TrimSpace(in.AllowedInsecureHost)
	if allowed == "" {
		allowed = origin.DefaultInsecureHost
	}
	if in.Insecure && !origin.InsecureAllowed(in.ServerName, allowed) {
		return nil, "", ErrInsecure
	}
	systemOnce.Do(func() {
		systemPool, systemErr = x509.SystemCertPool()
		if systemErr == nil {
			_, systemErr = rand.Read(systemEpoch[:])
		}
	})
	if systemErr != nil || systemPool == nil {
		return nil, "", errors.New("system trust store is unavailable")
	}
	pool := systemPool.Clone()
	h := sha256.New()
	h.Write(systemEpoch[:])
	fmt.Fprintf(h, "tls12\x00%s\x00%t\x00%s\x00%s\x00", in.ServerName, in.Insecure, allowed, in.CAPath)
	if in.CAPath != "" {
		data, err := readBoundedCA(in.CAPath)
		if err != nil {
			return nil, "", ErrMalformedCA
		}
		count := 0
		for len(bytes.TrimSpace(data)) > 0 {
			if err := ctx.Err(); err != nil {
				return nil, "", err
			}
			data = bytes.TrimSpace(data)
			if !bytes.HasPrefix(data, []byte("-----BEGIN CERTIFICATE-----")) {
				return nil, "", ErrMalformedCA
			}
			block, rest := pem.Decode(data)
			if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
				return nil, "", ErrMalformedCA
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, "", ErrMalformedCA
			}
			pool.AddCert(cert)
			h.Write(cert.Raw)
			count++
			data = rest
		}
		if count == 0 {
			return nil, "", ErrMalformedCA
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, ServerName: in.ServerName, RootCAs: pool, InsecureSkipVerify: in.Insecure}, hex.EncodeToString(h.Sum(nil)), nil
}
