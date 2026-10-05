package gitcache

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/listx"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/origin"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/pack"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/redact"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/single"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/tlsx"
)

// FetchOptions configures one bounded missing-set object fetch.
type FetchOptions struct {
	Token               string
	CAPath              string
	Insecure            bool
	AllowedInsecureHost string
	Timeout             time.Duration
	MaxBytes            int64
	Wants               []plumbing.Hash
	DecodeLimits        *pack.Limits
	preparedTLS         *tls.Config
	Depth               int // must be 1 or 2; never 0 / full-history
	AllowLoopback       bool
	SSH                 listx.Options
}

// FetchResult is one pack acquisition without secrets or path contents.
// Indexed is the sole decode: Objects and PackBytes alias that result.
type FetchResult struct {
	Indexed pack.IndexedPack
	Caps    FetchCaps
}

// Objects returns the single resolved-output map from Indexed.
func (r FetchResult) Objects() map[plumbing.Hash]pack.Object { return r.Indexed.Objects }

// PackBytes returns the pack slice aliased by Indexed (not a copy).
func (r FetchResult) PackBytes() []byte { return r.Indexed.PackBytes }

// FetchCaps records advertisement capabilities that affect authorization.
type FetchCaps struct {
	Shallow        bool
	AllowReachable bool
	AllowTip       bool
	OFSDelta       bool
}

// FetchObjects performs one bounded upload-pack over https or ssh.
// It does not use git.Fetch, the default transport registry, file protocol,
// thin-pack, sideband, or branch wildcards.
func FetchObjects(ctx context.Context, rawURL string, opt FetchOptions) (FetchResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(opt.Wants) == 0 || len(opt.Wants) > 3 {
		return FetchResult{}, ErrLimit
	}
	if err := ValidateDepth(opt.Depth); err != nil {
		return FetchResult{}, err
	}
	target, err := origin.Parse(rawURL, opt.AllowLoopback)
	if err != nil {
		return FetchResult{}, err
	}
	var result FetchResult
	err = single.Do(func() error {
		timeout := opt.Timeout
		if timeout <= 0 || timeout > bounds.Timeout {
			timeout = bounds.Timeout
		}
		maxBytes := opt.MaxBytes
		if maxBytes <= 0 || maxBytes > bounds.MaxPackBytes {
			maxBytes = bounds.MaxPackBytes
		}
		opCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		var opErr error
		switch target.Scheme {
		case "https":
			result, opErr = fetchHTTPS(opCtx, target, opt, timeout, maxBytes)
		case "ssh":
			result, opErr = fetchSSH(opCtx, target, opt, timeout, maxBytes)
		default:
			opErr = ErrScheme
		}
		return opErr
	})
	if e := safeContextError(err); e != nil {
		return FetchResult{}, e
	}
	if target.Scheme == "https" {
		return result, redact.PublicHTTPS(err,
			context.Canceled, context.DeadlineExceeded, ErrLimit, ErrCapability, ErrUnavailable, ErrPartial, ErrBusy, ErrScheme, ErrDenied,
			origin.ErrScheme, origin.ErrUserinfo, origin.ErrHost,
			tlsx.ErrMalformedCA, tlsx.ErrInsecure, single.ErrBusy,
			pack.ErrMalformed, pack.ErrObjectTooLarge, pack.ErrAggregate, pack.ErrDeltaDepth,
			pack.ErrThinDelta, pack.ErrTooManyObjects, pack.ErrChecksum,
		)
	}
	return result, redact.Error(err, opt.Token)
}

func fetchHTTPS(ctx context.Context, target origin.Target, opt FetchOptions, timeout time.Duration, maxBytes int64) (FetchResult, error) {
	if opt.Token == "" {
		return FetchResult{}, ErrDenied
	}
	tlsCfg := opt.preparedTLS
	var err error
	if tlsCfg == nil {
		tlsCfg, err = tlsx.Build(tlsx.Input{
			ServerName:          target.Host,
			CAPath:              opt.CAPath,
			Insecure:            opt.Insecure,
			AllowedInsecureHost: opt.AllowedInsecureHost,
		})
	}
	if err != nil {
		return FetchResult{}, err
	}
	dialer := &net.Dialer{Timeout: timeout}
	rt := &http.Transport{
		Proxy:                 func(*http.Request) (*url.URL, error) { return nil, nil },
		DialContext:           dialer.DialContext,
		TLSClientConfig:       tlsCfg,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          1,
	}
	hc := &http.Client{
		Transport: &limitedTransport{base: rt, max: maxBytes, prelude: bounds.MaxPackPrelude},
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirects are disabled")
		},
	}
	client := githttp.NewClientWithOptions(hc, &githttp.ClientOptions{
		RedirectPolicy: githttp.NoFollowRedirects,
	})
	ep, err := transport.NewEndpoint(target.Raw)
	if err != nil {
		return FetchResult{}, ErrScheme
	}
	sess, err := client.NewUploadPackSession(ep, &githttp.BasicAuth{
		Username: "oauth2",
		Password: opt.Token,
	})
	if err != nil {
		return FetchResult{}, ErrUnavailable
	}
	defer sess.Close()
	return uploadPack(ctx, sess, opt, maxBytes)
}

func fetchSSH(ctx context.Context, target origin.Target, opt FetchOptions, timeout time.Duration, maxBytes int64) (FetchResult, error) {
	sshOpt := opt.SSH
	sshOpt.Timeout = timeout
	sshOpt.MaxBytes = maxBytes
	if opt.AllowLoopback {
		sshOpt = sshOpt.WithLoopback()
	}
	sess, closer, err := listx.NewSSHUploadPackSession(ctx, target.Raw, sshOpt)
	if err != nil {
		return FetchResult{}, err
	}
	_ = closer // session Close is invoked exactly once inside finishSSHFetch.
	return finishSSHFetch(ctx, sess, opt, maxBytes)
}

// finishSSHFetch is the production-used SSH upload/close boundary: Close runs
// exactly once after upload and its Sync/Error/Finalize outcome is consumed.
func finishSSHFetch(ctx context.Context, sess transport.UploadPackSession, opt FetchOptions, maxBytes int64) (FetchResult, error) {
	result, opErr := uploadPack(ctx, sess, opt, maxBytes)
	closeErr := sess.Close()
	return composeSSHFetchOutcome(result, opErr, closeErr)
}

// composeSSHFetchOutcome combines upload and Close/trust-finalization errors.
// Either failure yields an empty result. Ordinary non-context errors are joined
// so both safe typed categories remain observable. Cancellation/deadline takes
// bare public precedence (G3) and never exposes a joined context wrapper.
func composeSSHFetchOutcome(result FetchResult, opErr, closeErr error) (FetchResult, error) {
	if opErr == nil && closeErr == nil {
		return result, nil
	}
	joined := errors.Join(opErr, closeErr)
	if e := safeContextError(joined); e != nil {
		return FetchResult{}, e
	}
	return FetchResult{}, joined
}

func uploadPack(ctx context.Context, sess transport.UploadPackSession, opt FetchOptions, maxBytes int64) (FetchResult, error) {
	if err := ctx.Err(); err != nil {
		return FetchResult{}, err
	}
	adv, err := sess.AdvertisedReferencesContext(ctx)
	if err != nil {
		if e := safeContextError(err); e != nil {
			return FetchResult{}, e
		}
		if ctx.Err() != nil {
			return FetchResult{}, ctx.Err()
		}
		return FetchResult{}, ErrUnavailable
	}
	caps := FetchCaps{
		Shallow:        adv.Capabilities != nil && adv.Capabilities.Supports(capability.Shallow),
		AllowReachable: adv.Capabilities != nil && adv.Capabilities.Supports(capability.AllowReachableSHA1InWant),
		AllowTip:       adv.Capabilities != nil && adv.Capabilities.Supports(capability.AllowTipSHA1InWant),
		OFSDelta:       adv.Capabilities != nil && adv.Capabilities.Supports(capability.OFSDelta),
	}
	if !caps.Shallow {
		return FetchResult{}, ErrCapability
	}
	req := packp.NewUploadPackRequest()
	req.Depth = packp.DepthCommits(opt.Depth)
	_ = req.Capabilities.Set(capability.Shallow)
	if caps.OFSDelta {
		_ = req.Capabilities.Set(capability.OFSDelta)
	}
	if adv.Capabilities != nil && adv.Capabilities.Supports(capability.Agent) {
		_ = req.Capabilities.Set(capability.Agent, "gitlab-mcp-gitcache")
	}
	for _, w := range opt.Wants {
		if err := ctx.Err(); err != nil {
			return FetchResult{}, err
		}
		if !advertisedHash(adv, w) {
			if err := CapabilityGate(true, caps.AllowReachable, caps.Shallow, true); err != nil {
				return FetchResult{}, err
			}
		}
		req.Wants = append(req.Wants, w)
	}
	reader, err := sess.UploadPack(ctx, req)
	if err != nil {
		if e := safeContextError(err); e != nil {
			return FetchResult{}, e
		}
		if ctx.Err() != nil {
			return FetchResult{}, ctx.Err()
		}
		return FetchResult{}, ErrUnavailable
	}
	defer reader.Close()
	raw, err := readLimited(ctx, reader, maxBytes)
	if err != nil {
		if e := safeContextError(err); e != nil {
			return FetchResult{}, e
		}
		if ctx.Err() != nil {
			return FetchResult{}, ctx.Err()
		}
		return FetchResult{}, err
	}
	limits := pack.DefaultLimits()
	if opt.DecodeLimits != nil {
		limits = *opt.DecodeLimits
	}
	ip, err := pack.DecodeIndexedLimits(ctx, raw, limits)
	if err != nil {
		return FetchResult{}, err
	}
	return FetchResult{Indexed: ip, Caps: caps}, nil
}

func advertisedHash(adv *packp.AdvRefs, want plumbing.Hash) bool {
	if adv == nil {
		return false
	}
	if adv.Head != nil && *adv.Head == want {
		return true
	}
	for _, h := range adv.References {
		if h == want {
			return true
		}
	}
	return false
}

func readLimited(ctx context.Context, r io.Reader, max int64) ([]byte, error) {
	var buf bytes.Buffer
	tmp := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := r.Read(tmp)
		if n > 0 {
			if int64(buf.Len()+n) > max {
				return nil, ErrLimit
			}
			buf.Write(tmp[:n])
		}
		if err == io.EOF {
			return buf.Bytes(), nil
		}
		if err != nil {
			if e := safeContextError(err); e != nil {
				return nil, e
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("%w", ErrUnavailable)
		}
	}
}

type limitedTransport struct {
	base    http.RoundTripper
	max     int64
	prelude int64
}

func (t *limitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	limit := t.max
	// Advertised refs stay on max. The upload-pack POST also carries the
	// protocol prelude, which is not part of the raw-pack cap.
	if req != nil && req.Method == http.MethodPost && t.prelude > 0 {
		if t.max > math.MaxInt64-t.prelude {
			limit = math.MaxInt64
		} else {
			limit = t.max + t.prelude
		}
	}
	resp.Body = &limitedBody{ReadCloser: resp.Body, max: limit}
	return resp, nil
}

type limitedBody struct {
	io.ReadCloser
	max int64
	n   int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.n > b.max {
		return 0, ErrLimit
	}
	// A body of exactly max bytes ends with EOF. Probe one extra byte before
	// treating the ceiling as overflow, matching listx.limitedReader.
	if b.n == b.max {
		var one [1]byte
		n, err := b.ReadCloser.Read(one[:])
		if n > 0 {
			b.n += int64(n)
			return 0, ErrLimit
		}
		return 0, err
	}
	if int64(len(p)) > b.max-b.n {
		p = p[:b.max-b.n]
	}
	n, err := b.ReadCloser.Read(p)
	b.n += int64(n)
	return n, err
}

// Return only fixed sentinels; the original transport-controlled wrapper is discarded.
func safeContextError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}
