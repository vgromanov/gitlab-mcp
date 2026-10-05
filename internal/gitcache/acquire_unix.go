//go:build linux || darwin

package gitcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/listx"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/tlsx"
	"runtime"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/origin"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/pack"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/tree"
)

// Acquire is the single content acquisition path: fresh ResolveGrant before
// warm lookup, warm content return, cold fetch, publication, and cold return.
// Caller-populated Grant fields are never accepted. Depth must be 1 or 2.
func (m *Manager) Acquire(ctx context.Context, intent AcquireIntent, auth Authorizer) (result *AcquireResult, resultErr error) {
	start := time.Now()
	if m == nil {
		return nil, ErrDisabled
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if auth == nil {
		return nil, ErrAuthz
	}
	if err := ValidateDepth(intent.Depth); err != nil {
		return nil, err
	}
	depth := intent.Depth

	m.mu.Lock()
	if m.closed || m.closing {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	if m.acqBusy {
		m.mu.Unlock()
		return nil, ErrBusy
	}
	m.acqBusy = true
	m.wg.Add(1)
	workCtx := m.workCtx
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.acqBusy = false
		m.mu.Unlock()
		m.wg.Done()
	}()

	opCtx, cancel := context.WithTimeout(ctx, bounds.Timeout)
	defer cancel()
	stop := context.AfterFunc(workCtx, cancel)
	defer stop()

	grant, err := resolveBound(opCtx, auth, intent, Grant{})
	if err != nil {
		return nil, preserveCtx(opCtx, err)
	}
	intent, err = prepareAcquisitionTrust(opCtx, grant, intent)
	if err != nil {
		return nil, preserveCtx(opCtx, err)
	}
	grant.TrustProvenance = intent.trustFP
	fp := GrantFingerprint(grant)
	ns := NamespaceID(grant.AuthDomain, grant.CanonicalInstance, grant.ProjectID, grant.SourceFork)

	if warm, err := m.lookupWarm(opCtx, fp, ns); err == nil && warm != nil {
		defer func() {
			if e := warm.Unpin(); e != nil {
				result = nil
				resultErr = errors.Join(resultErr, e)
			}
		}()
		grant, err = resolveBound(opCtx, auth, intent, grant)
		if err != nil {
			return nil, preserveCtx(opCtx, err)
		}
		intent, err = prepareAcquisitionTrust(opCtx, grant, intent)
		if err != nil {
			return nil, preserveCtx(opCtx, err)
		}
		grant.TrustProvenance = intent.trustFP
		if err := proveGrant(opCtx, tree.Map(warm.Objects), grant, depth); err != nil {
			return nil, mapProofErr(opCtx, err)
		}

		grant, err = resolveBound(opCtx, auth, intent, grant)
		if err != nil {
			return nil, preserveCtx(opCtx, err)
		}
		intent, err = prepareAcquisitionTrust(opCtx, grant, intent)
		if err != nil {
			return nil, preserveCtx(opCtx, err)
		}
		grant.TrustProvenance = intent.trustFP
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		return &AcquireResult{
			Warm:         true,
			Objects:      len(warm.Objects),
			HeapAlloc:    mem.HeapAlloc,
			Elapsed:      time.Since(start),
			GenerationID: warm.ID,
			Grant:        grant,
		}, nil
	} else if err != nil && !errors.Is(err, ErrUnavailable) {
		return nil, preserveCtx(opCtx, err)
	}

	grant, err = resolveBound(opCtx, auth, intent, grant)
	if err != nil {
		return nil, preserveCtx(opCtx, err)
	}
	intent, err = prepareAcquisitionTrust(opCtx, grant, intent)
	if err != nil {
		return nil, preserveCtx(opCtx, err)
	}
	grant.TrustProvenance = intent.trustFP

	fres, intent, err := fetchRoleRoots(opCtx, grant, intent, depth)
	if err != nil {
		return nil, preserveCtx(opCtx, err)
	}
	grant, err = resolveBound(opCtx, auth, intent, grant)
	if err != nil {
		return nil, preserveCtx(opCtx, err)
	}
	intent, err = commitAcquisitionTrust(opCtx, grant, intent)
	if err != nil {
		return nil, preserveCtx(opCtx, err)
	}
	grant.TrustProvenance = intent.trustFP
	if err := proveGrant(opCtx, tree.Map(fres.Objects), grant, depth); err != nil {
		return nil, mapProofErr(opCtx, err)
	}
	fp = GrantFingerprint(grant)
	ns = NamespaceID(grant.AuthDomain, grant.CanonicalInstance, grant.ProjectID, grant.SourceFork)
	gen, err := m.publishGeneration(opCtx, fp, ns, grant.HeadSHA, grant.BaseSHA, grant.StartSHA, fres)
	if err != nil {
		return nil, preserveCtx(opCtx, err)
	}
	grant, err = resolveBound(opCtx, auth, intent, grant)
	if err != nil {
		return nil, preserveCtx(opCtx, err)
	}
	intent, err = prepareAcquisitionTrust(opCtx, grant, intent)
	if err != nil {
		return nil, preserveCtx(opCtx, err)
	}
	grant.TrustProvenance = intent.trustFP
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	return &AcquireResult{
		Warm:         false,
		Objects:      len(fres.Objects),
		PackBytes:    fres.PackByteCount,
		HeapAlloc:    mem.HeapAlloc,
		Elapsed:      time.Since(start),
		GenerationID: gen.ID,
		Grant:        grant,
	}, nil
}

func preserveCtx(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if e := safeContextError(err); e != nil {
		return e
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func mapProofErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if e := safeContextError(err); e != nil {
		return e
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, tree.ErrShallow) {
		return ErrPartial
	}
	if errors.Is(err, tree.ErrMissing) {
		return ErrPartial
	}
	return ErrCorrupt
}

func resolveBound(ctx context.Context, auth Authorizer, intent AcquireIntent, prior Grant) (Grant, error) {
	if err := ctx.Err(); err != nil {
		return Grant{}, err
	}
	g, err := auth.ResolveGrant(ctx, intent)
	if err != nil {
		return Grant{}, preserveCtx(ctx, err)
	}
	if err := ValidateGrant(g); err != nil {
		return Grant{}, err
	}
	if err := AuthorizeObject(g, ObjectProof{Hash: g.HeadSHA, Type: "commit", Role: RootHead}); err != nil {
		return Grant{}, err
	}
	if err := AuthorizeObject(g, ObjectProof{Hash: g.BaseSHA, Type: "commit", Role: RootBase}); err != nil {
		return Grant{}, err
	}
	if err := AuthorizeObject(g, ObjectProof{Hash: g.StartSHA, Type: "commit", Role: RootStart}); err != nil {
		return Grant{}, err
	}
	if prior.ProjectID != "" && !sameBinding(prior, g) {
		return Grant{}, ErrAuthz
	}
	return g, nil
}

func sameBinding(a, b Grant) bool {
	return a.CanonicalInstance == b.CanonicalInstance &&
		a.OriginHost == b.OriginHost &&
		a.ProjectID == b.ProjectID &&
		a.ProjectPath == b.ProjectPath &&
		a.SourceFork == b.SourceFork &&
		a.SourcePath == b.SourcePath &&
		a.TargetProjectID == b.TargetProjectID &&
		a.TargetPath == b.TargetPath &&
		a.AuthDomain == b.AuthDomain &&
		a.PolicyFP == b.PolicyFP &&
		a.MRIID == b.MRIID &&
		a.MRVersion == b.MRVersion &&
		a.HeadSHA == b.HeadSHA &&
		a.BaseSHA == b.BaseSHA &&
		a.StartSHA == b.StartSHA &&
		a.HeadRef == b.HeadRef &&
		a.BaseRef == b.BaseRef &&
		a.StartRef == b.StartRef &&
		a.ActorID == b.ActorID &&
		a.SourceHTTPSURL == b.SourceHTTPSURL &&
		a.SourceSSHURL == b.SourceSSHURL &&
		a.TargetHTTPSURL == b.TargetHTTPSURL &&
		a.TargetSSHURL == b.TargetSSHURL
}

// selectURL resolves the source (head) clone URL for transport.
func selectURL(transport string, g Grant, allowLoopback bool) (string, error) {
	return selectRoleURL(transport, g, RootHead, allowLoopback)
}

func selectRoleURL(transport string, g Grant, role RootRole, allowLoopback bool) (string, error) {
	var httpsURL, sshURL string
	switch role {
	case RootHead:
		httpsURL, sshURL = g.SourceHTTPSURL, g.SourceSSHURL
	case RootBase, RootStart:
		httpsURL, sshURL = g.TargetHTTPSURL, g.TargetSSHURL
	default:
		return "", ErrAuthz
	}
	switch transport {
	case "", "https":
		if httpsURL == "" {
			return "", ErrScheme
		}
		t, err := origin.Parse(httpsURL, allowLoopback)
		if err != nil {
			return "", err
		}
		path := g.SourcePath
		if role != RootHead {
			path = g.TargetPath
		}
		if t.Host != g.OriginHost || ValidateCloneURL(t.Raw, g.CanonicalInstance, path, allowLoopback) != nil {
			return "", ErrAuthz
		}
		return httpsURL, nil
	case "ssh":
		if sshURL == "" {
			return "", ErrScheme
		}
		t, err := origin.Parse(sshURL, allowLoopback)
		if err != nil {
			return "", err
		}
		path := g.SourcePath
		if role != RootHead {
			path = g.TargetPath
		}
		if t.Host != g.OriginHost || ValidateCloneURL(t.Raw, g.CanonicalInstance, path, allowLoopback) != nil {
			return "", ErrAuthz
		}
		return sshURL, nil
	default:
		return "", ErrScheme
	}
}

func fetchOpts(intent AcquireIntent, wants []plumbing.Hash, depth int) FetchOptions {
	return FetchOptions{
		Token:               intent.Token,
		CAPath:              intent.CAPath,
		Insecure:            intent.Insecure,
		AllowedInsecureHost: intent.AllowedInsecureHost,
		Wants:               wants,
		Depth:               depth,
		Timeout:             bounds.Timeout,
		AllowLoopback:       intent.AllowLoopback,
		SSH:                 intent.SSH,
		preparedTLS:         intent.preparedTLS,
	}
}

// fetchRoleRoots acquires head from the source repository and, when depth>=2,
// base/start from the target repository when they are distinct or missing.
func fetchRoleRoots(ctx context.Context, grant Grant, intent AcquireIntent, depth int) (pack.IndexedPack, AcquireIntent, error) {
	sourceURL, err := selectRoleURL(intent.Transport, grant, RootHead, intent.AllowLoopback)
	if err != nil {
		return pack.IndexedPack{}, intent, err
	}
	sameRepo := grant.SourceFork == grant.TargetProjectID
	wants := []plumbing.Hash{grant.HeadSHA}
	if sameRepo {
		if grant.BaseSHA != grant.HeadSHA {
			wants = append(wants, grant.BaseSHA)
		}
		if grant.StartSHA != grant.BaseSHA && grant.StartSHA != grant.HeadSHA {
			wants = append(wants, grant.StartSHA)
		}
	}
	opt := fetchOpts(intent, wants, depth)
	if intent.Transport == "ssh" {
		opt.SSH = intent.sourceSSH
	}
	src, err := FetchObjects(ctx, sourceURL, opt)
	if err != nil {
		return pack.IndexedPack{}, intent, err
	}
	if intent.Transport == "ssh" {
		intent.sourceSSH = opt.SSH
		intent.trustTransitions = append(intent.trustTransitions, intent.sourceSSH.TrustTransitions()...)
		intent, err = commitAcquisitionTrust(ctx, grant, intent)
		if err != nil {
			return pack.IndexedPack{}, intent, err
		}
	}
	// Prove each repository's own roots before coalescing; objects appearing in
	// another role's pack never establish that repository's authorization proof.
	if err := tree.ProveCommitRoot(ctx, tree.Map(src.Objects()), grant.HeadSHA); err != nil {
		return pack.IndexedPack{}, intent, mapProofErr(ctx, err)
	}
	if sameRepo {
		if err := proveGrant(ctx, tree.Map(src.Objects()), grant, depth); err != nil {
			return pack.IndexedPack{}, intent, mapProofErr(ctx, err)
		}
		return src.Indexed, intent, nil
	}
	targetURL, err := selectRoleURL(intent.Transport, grant, RootBase, intent.AllowLoopback)
	if err != nil {
		return pack.IndexedPack{}, intent, err
	}
	wants = []plumbing.Hash{grant.BaseSHA}
	if grant.StartSHA != grant.BaseSHA {
		wants = append(wants, grant.StartSHA)
	}
	retained := src.Indexed.RetainedOutputBytes()
	remainingRaw := bounds.MaxPackBytes - int64(src.Indexed.PackByteCount)
	if remainingRaw <= 0 {
		return pack.IndexedPack{}, intent, ErrLimit
	}
	limits := pack.Limits{Objects: bounds.MaxObjects - len(src.Indexed.Entries), InputBytes: bounds.MaxRetainedInput - retained, OutputBytes: bounds.MaxRetainedOutput - retained}
	opt = fetchOpts(intent, wants, depth)
	opt.MaxBytes = remainingRaw
	opt.DecodeLimits = &limits
	if intent.Transport == "ssh" {
		opt.SSH = intent.targetSSH
	}
	tgt, err := FetchObjects(ctx, targetURL, opt)
	if err != nil {
		return pack.IndexedPack{}, intent, err
	}
	if intent.Transport == "ssh" {
		intent.targetSSH = opt.SSH
		intent.trustTransitions = append(intent.trustTransitions, intent.targetSSH.TrustTransitions()...)
	}
	for _, h := range wants {
		if err := tree.ProveCommitRoot(ctx, tree.Map(tgt.Objects()), h); err != nil {
			return pack.IndexedPack{}, intent, mapProofErr(ctx, err)
		}
	}
	objs := src.Objects()
	if err := pack.MergeObjects(ctx, objs, tgt.Objects()); err != nil {
		return pack.IndexedPack{}, intent, err
	}
	// Release role pack buffers and redundant maps before generating one bounded
	// undeltified pack. EncodeIndexed records offsets/CRC without decoding again.
	src = FetchResult{}
	tgt = FetchResult{}
	ip, err := pack.EncodeIndexed(ctx, objs)
	return ip, intent, err
}

func proveGrant(ctx context.Context, g tree.Getter, grant Grant, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := AuthorizeObject(grant, ObjectProof{Hash: grant.HeadSHA, Type: "commit", Role: RootHead}); err != nil {
		return err
	}
	if err := AuthorizeObject(grant, ObjectProof{Hash: grant.BaseSHA, Type: "commit", Role: RootBase}); err != nil {
		return err
	}
	if err := AuthorizeObject(grant, ObjectProof{Hash: grant.StartSHA, Type: "commit", Role: RootStart}); err != nil {
		return err
	}
	if err := tree.ProveHead(ctx, g, grant.HeadSHA, grant.BaseSHA, 2); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if grant.StartSHA != grant.BaseSHA {
		if err := tree.ProveCommitRoot(ctx, g, grant.StartSHA); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
	return nil
}

func prepareAcquisitionTrust(ctx context.Context, g Grant, intent AcquireIntent) (AcquireIntent, error) {
	return bindAcquisitionTrust(ctx, g, intent, false)
}

// commitAcquisitionTrust reloads trust after cold fetch and accepts only owned
// approved transitions (enrollment/update) into the publication binding.
func commitAcquisitionTrust(ctx context.Context, g Grant, intent AcquireIntent) (AcquireIntent, error) {
	return bindAcquisitionTrust(ctx, g, intent, true)
}

func bindAcquisitionTrust(ctx context.Context, g Grant, intent AcquireIntent, afterFetch bool) (AcquireIntent, error) {
	source, err := selectRoleURL(intent.Transport, g, RootHead, intent.AllowLoopback)
	if err != nil {
		return intent, err
	}
	target, err := selectRoleURL(intent.Transport, g, RootBase, intent.AllowLoopback)
	if err != nil {
		return intent, err
	}
	var fp string
	if intent.Transport == "ssh" {
		opts := intent.SSH
		if intent.AllowLoopback {
			opts = opts.WithLoopback()
		}
		var a, b string
		oldSource, oldTarget := intent.sourceSSH, intent.targetSSH
		intent.sourceSSH, a, err = listx.PrepareTrust(ctx, source, opts.ClearPrepared())
		if err != nil {
			return intent, err
		}
		intent.targetSSH, b, err = listx.PrepareTrust(ctx, target, opts.ClearPrepared())
		if err != nil {
			return intent, err
		}
		if afterFetch {
			if err := oldSource.ValidateRefresh(ctx, intent.sourceSSH, intent.trustTransitions); err != nil {
				return intent, preserveCtx(ctx, ErrAuthz)
			}
			if err := oldTarget.ValidateRefresh(ctx, intent.targetSSH, intent.trustTransitions); err != nil {
				return intent, preserveCtx(ctx, ErrAuthz)
			}
			intent.trustTransitions = nil
		}
		fp = a + ":" + b
	} else {
		intent.preparedTLS, fp, err = tlsx.Prepare(ctx, tlsx.Input{ServerName: g.OriginHost, CAPath: intent.CAPath, Insecure: intent.Insecure, AllowedInsecureHost: intent.AllowedInsecureHost})
		if err != nil {
			return intent, err
		}
	}
	sum := sha256.Sum256([]byte(g.CanonicalInstance + "\x00" + source + "\x00" + target + "\x00" + fp))
	fp = hex.EncodeToString(sum[:])
	if intent.trustFP != "" && intent.trustFP != fp {
		if !afterFetch || intent.Transport != "ssh" {
			return intent, ErrAuthz
		}
	}
	intent.trustFP = fp
	return intent, nil
}
