package tools

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"
)

// GitCacheAuthorizer resolves authoritative Grants from fresh GitLab API reads
// using the same canonical project/group authorization as other tools.
// Caller-populated Grant fields are ignored; only AcquireIntent is consulted.
type GitCacheAuthorizer struct {
	d Deps
}

// NewGitCacheAuthorizer builds the production acquisition authorizer.
// It does not register a public cache tool and does not persist credentials.
func NewGitCacheAuthorizer(d Deps) *GitCacheAuthorizer {
	return &GitCacheAuthorizer{d: d}
}

func authzAPIErr(ctx context.Context, err error) error {
	if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return err
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return gitcache.ErrAuthz
}

// ResolveGrant implements gitcache.Authorizer.
func (a *GitCacheAuthorizer) ResolveGrant(ctx context.Context, intent gitcache.AcquireIntent) (gitcache.Grant, error) {
	if a == nil || a.d.Client == nil || a.d.Config == nil {
		return gitcache.Grant{}, gitcache.ErrAuthz
	}
	if err := ctx.Err(); err != nil {
		return gitcache.Grant{}, err
	}
	if intent.MRIID < 1 || strings.TrimSpace(intent.ProjectID) == "" {
		return gitcache.Grant{}, gitcache.ErrAuthz
	}
	if err := gitcache.ValidateDepth(intent.Depth); err != nil {
		return gitcache.Grant{}, err
	}

	token := a.d.Config.Token
	if token == "" || (intent.Token != "" && intent.Token != token) {
		return gitcache.Grant{}, gitcache.ErrAuthz
	}
	configured, err := cursor.CanonicalInstance(a.d.Config.APIURL)
	if err != nil {
		return gitcache.Grant{}, gitcache.ErrAuthz
	}
	actual, err := cursor.CanonicalInstance(a.d.Client.BaseURL().String())
	if err != nil {
		return gitcache.Grant{}, gitcache.ErrAuthz
	}
	ip := net.ParseIP(a.d.Client.BaseURL().Hostname())
	fixture := intent.AllowLoopback && (a.d.Client.BaseURL().Hostname() == "localhost" || (ip != nil && ip.IsLoopback()))
	effectiveConfigured := configured
	if !strings.HasSuffix(effectiveConfigured, "/api/v4") {
		effectiveConfigured += "/api/v4"
	}
	if actual != effectiveConfigured && !fixture {
		return gitcache.Grant{}, gitcache.ErrAuthz
	}
	ctx = igl.WithCacheBinding(ctx, a.d.Client.BaseURL(), token)
	u, _, err := a.d.Client.Users.CurrentUser(gitlab.WithContext(ctx))
	if err != nil || u == nil || u.ID <= 0 {
		return gitcache.Grant{}, authzAPIErr(ctx, err)
	}
	actorID := strconv.FormatInt(u.ID, 10)

	owner, err := AuthorizeCanonicalProject(ctx, a.d, intent.ProjectID)
	if err != nil || owner.ID <= 0 {
		return gitcache.Grant{}, authzAPIErr(ctx, err)
	}

	mr, _, err := a.d.Client.MergeRequests.GetMergeRequest(owner.ID, int64(intent.MRIID), nil, gitlab.WithContext(ctx))
	if err != nil || mr == nil {
		return gitcache.Grant{}, authzAPIErr(ctx, err)
	}
	if mr.IID != int64(intent.MRIID) {
		return gitcache.Grant{}, gitcache.ErrAuthz
	}
	// Owner project id must be positively proven; zero is not tolerated.
	if mr.ProjectID <= 0 || mr.ProjectID != owner.ID {
		return gitcache.Grant{}, gitcache.ErrAuthz
	}
	if err := requireProvenMRForkProjects(ctx, a.d, owner, mr); err != nil {
		return gitcache.Grant{}, authzAPIErr(ctx, err)
	}
	if mr.TargetProjectID <= 0 {
		return gitcache.Grant{}, gitcache.ErrAuthz
	}
	// Positive target must equal the canonical MR owner. A different allowed
	// project is not the MR target (same shape as bindDiffMR, but require >0).
	if mr.TargetProjectID != owner.ID {
		return gitcache.Grant{}, gitcache.ErrAuthz
	}
	if mr.SourceProjectID <= 0 {
		return gitcache.Grant{}, gitcache.ErrAuthz
	}
	targetID := strconv.FormatInt(mr.TargetProjectID, 10)

	headRaw, ok := readmeta.ObservedHeadSHA(mr.DiffRefs.HeadSha)
	if !ok {
		return gitcache.Grant{}, gitcache.ErrAuthz
	}
	baseRaw, ok := readmeta.ObservedHeadSHA(mr.DiffRefs.BaseSha)
	if !ok {
		return gitcache.Grant{}, gitcache.ErrAuthz
	}
	startRaw, ok := readmeta.ObservedHeadSHA(mr.DiffRefs.StartSha)
	if !ok {
		return gitcache.Grant{}, gitcache.ErrAuthz
	}
	head := plumbing.NewHash(headRaw)
	base := plumbing.NewHash(baseRaw)
	start := plumbing.NewHash(startRaw)
	mrVersion := gitcache.MRVersionFromDiffRefs(head, base, start)

	sourceID := strconv.FormatInt(mr.SourceProjectID, 10)
	if _, err := AuthorizeCanonicalProject(ctx, a.d, sourceID); err != nil {
		return gitcache.Grant{}, authzAPIErr(ctx, err)
	}
	srcProj, err := getProjectSafe(ctx, a.d, sourceID)
	if err != nil || srcProj == nil || srcProj.ID != mr.SourceProjectID {
		return gitcache.Grant{}, authzAPIErr(ctx, err)
	}
	tgtProj, err := getProjectSafe(ctx, a.d, targetID)
	if err != nil || tgtProj == nil || tgtProj.ID != mr.TargetProjectID {
		return gitcache.Grant{}, authzAPIErr(ctx, err)
	}

	canonicalInst, err := cursor.CanonicalInstance(a.d.Config.APIURL)
	if err != nil {
		return gitcache.Grant{}, gitcache.ErrAuthz
	}
	originHost, err := originHostFromCanonical(canonicalInst)
	if err != nil {
		return gitcache.Grant{}, gitcache.ErrAuthz
	}

	srcHTTPS := strings.TrimSpace(srcProj.HTTPURLToRepo)
	srcSSH := strings.TrimSpace(srcProj.SSHURLToRepo)
	srcPath := strings.TrimSpace(srcProj.PathWithNamespace)
	if srcPath == "" || (srcHTTPS == "" && srcSSH == "") {
		return gitcache.Grant{}, gitcache.ErrAuthz
	}
	if srcHTTPS != "" {
		if err := assertRepoURL(srcHTTPS, canonicalInst, srcPath); err != nil {
			return gitcache.Grant{}, err
		}
	}
	if srcSSH != "" {
		if err := assertRepoURL(srcSSH, canonicalInst, srcPath); err != nil {
			return gitcache.Grant{}, err
		}
	}

	tgtHTTPS := strings.TrimSpace(tgtProj.HTTPURLToRepo)
	tgtSSH := strings.TrimSpace(tgtProj.SSHURLToRepo)
	tgtPath := strings.TrimSpace(tgtProj.PathWithNamespace)
	if tgtPath == "" || (tgtHTTPS == "" && tgtSSH == "") {
		return gitcache.Grant{}, gitcache.ErrAuthz
	}
	if tgtHTTPS != "" {
		if err := assertRepoURL(tgtHTTPS, canonicalInst, tgtPath); err != nil {
			return gitcache.Grant{}, err
		}
	}
	if tgtSSH != "" {
		if err := assertRepoURL(tgtSSH, canonicalInst, tgtPath); err != nil {
			return gitcache.Grant{}, err
		}
	}

	// Independently establish each immutable root in its own authorized repository.
	for _, root := range []struct {
		project string
		sha     plumbing.Hash
	}{{sourceID, head}, {targetID, base}, {targetID, start}} {
		commit, _, err := a.d.Client.Commits.GetCommit(root.project, root.sha.String(), nil, gitlab.WithContext(ctx))
		if err != nil || commit == nil || commit.ID != root.sha.String() {
			return gitcache.Grant{}, authzAPIErr(ctx, err)
		}
	}
	if !igl.CacheBindingObserved(ctx) {
		return gitcache.Grant{}, gitcache.ErrAuthz
	}
	var domainKey string
	if a.d.GitCache != nil {
		domainKey, err = a.d.GitCache.Domain().Bind(actorID, token)
	} else {
		var local gitcache.AuthDomain
		domainKey, err = local.Bind(actorID, token)
	}
	if err != nil {
		return gitcache.Grant{}, err
	}
	policyFP := ""
	if a.d.Config.PolicyActive() {
		policyFP = a.d.Config.PolicyFingerprint()
	}

	g := gitcache.Grant{
		CanonicalInstance: canonicalInst,
		OriginHost:        originHost,
		ProjectID:         strconv.FormatInt(owner.ID, 10),
		ProjectPath:       owner.PathWithNamespace,
		SourceFork:        sourceID,
		SourcePath:        srcPath,
		TargetProjectID:   targetID,
		TargetPath:        tgtPath,
		AuthDomain:        domainKey,
		PolicyFP:          policyFP,
		MRIID:             intent.MRIID,
		MRVersion:         mrVersion,
		HeadSHA:           head,
		BaseSHA:           base,
		StartSHA:          start,
		HeadRef:           strings.TrimSpace(mr.SourceBranch),
		BaseRef:           strings.TrimSpace(mr.TargetBranch),
		StartRef:          strings.TrimSpace(mr.TargetBranch),
		ActorID:           actorID,
		SourceHTTPSURL:    srcHTTPS,
		SourceSSHURL:      srcSSH,
		TargetHTTPSURL:    tgtHTTPS,
		TargetSSHURL:      tgtSSH,
		HTTPSURL:          srcHTTPS,
		SSHURL:            srcSSH,
	}
	if err := matchIntentExpectations(intent, g); err != nil {
		return gitcache.Grant{}, err
	}
	if err := gitcache.ValidateGrant(g); err != nil {
		return gitcache.Grant{}, err
	}
	return g, nil
}

func matchIntentExpectations(intent gitcache.AcquireIntent, g gitcache.Grant) error {
	if intent.ExpectedHead != plumbing.ZeroHash && intent.ExpectedHead != g.HeadSHA {
		return gitcache.ErrAuthz
	}
	if intent.ExpectedBase != plumbing.ZeroHash && intent.ExpectedBase != g.BaseSHA {
		return gitcache.ErrAuthz
	}
	if intent.ExpectedStart != plumbing.ZeroHash && intent.ExpectedStart != g.StartSHA {
		return gitcache.ErrAuthz
	}
	if intent.ExpectedMRVersion != "" && intent.ExpectedMRVersion != g.MRVersion {
		return gitcache.ErrAuthz
	}
	return nil
}

func originHostFromCanonical(inst string) (string, error) {
	u, err := url.Parse(inst)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("gitcache: origin host")
	}
	return strings.ToLower(u.Hostname()), nil
}

func assertRepoURL(raw, canonicalInstance, projectPath string) error {
	return gitcache.ValidateCloneURL(raw, canonicalInstance, projectPath, false)
}
