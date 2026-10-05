package gitcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/origin"
)

// RootRole identifies which authorized immutable root a hash must be reachable from.
type RootRole string

const (
	RootHead  RootRole = "head"
	RootBase  RootRole = "base"
	RootStart RootRole = "start"
)

// Grant is an authoritative binding produced only by Authorizer.ResolveGrant
// from fresh canonical API reads. Caller-populated Grant fields cannot authorize.
type Grant struct {
	// CanonicalInstance is the full credential-safe API instance binding
	// (scheme://host[:port][/path]), never hostname-only.
	CanonicalInstance string
	OriginHost        string // hostname extracted for URL host checks
	ProjectID         string // owner project id (must be non-zero proven)
	ProjectPath       string // owner path
	SourceFork        string // source project id
	SourcePath        string // source path_with_namespace
	TargetProjectID   string // target project id (must be non-zero proven)
	TargetPath        string // target path_with_namespace
	AuthDomain        string
	PolicyFP          string
	MRIID             int
	MRVersion         string
	HeadSHA           plumbing.Hash
	BaseSHA           plumbing.Hash
	StartSHA          plumbing.Hash
	HeadRef           string
	BaseRef           string
	StartRef          string
	ActorID           string
	// Role-specific clone URLs. Head is fetched from source; base/start from target.
	SourceHTTPSURL string
	SourceSSHURL   string
	TargetHTTPSURL string
	TargetSSHURL   string
	// HTTPSURL/SSHURL mirror source URLs for older call sites; prefer Source*.
	HTTPSURL string
	SSHURL   string
	// TrustProvenance is a non-secret effective TLS/SSH trust binding set at
	// acquisition (never credentials or reusable token digests).
	TrustProvenance string
}

// ValidateGrant checks structural completeness of an already-resolved grant.
func ValidateGrant(g Grant) error {
	if g.OriginHost == "" || !canonicalInstanceOK(g.CanonicalInstance, g.OriginHost) {
		return ErrAuthz
	}
	if g.ProjectID == "" || g.ProjectID == "0" || g.ProjectPath == "" {
		return ErrAuthz
	}
	if g.AuthDomain == "" || g.ActorID == "" {
		return ErrAuthz
	}
	if g.SourceFork == "" || g.SourceFork == "0" || g.SourcePath == "" {
		return ErrAuthz
	}
	if g.TargetProjectID == "" || g.TargetProjectID == "0" || g.TargetPath == "" {
		return ErrAuthz
	}
	if g.MRIID <= 0 || g.MRVersion == "" {
		return ErrAuthz
	}
	if g.HeadSHA == plumbing.ZeroHash || g.BaseSHA == plumbing.ZeroHash || g.StartSHA == plumbing.ZeroHash {
		return ErrAuthz
	}
	if g.SourceHTTPSURL == "" && g.SourceSSHURL == "" {
		return ErrAuthz
	}
	if g.TargetHTTPSURL == "" && g.TargetSSHURL == "" {
		return ErrAuthz
	}
	return nil
}

// canonicalInstanceOK requires a full scheme://host[:port][/path] binding that
// matches OriginHost. Hostname-only values are rejected.
func canonicalInstanceOK(inst, originHost string) bool {
	inst = strings.TrimSpace(inst)
	if inst == "" || inst == originHost || !strings.Contains(inst, "://") {
		return false
	}
	u, err := url.Parse(inst)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || !strings.EqualFold(host, originHost) {
		return false
	}
	return true
}

// ObjectProof is an independently validated object against an authorized root.
type ObjectProof struct {
	Hash plumbing.Hash
	Type string
	Role RootRole
}

// AuthorizeObject requires the hash/type to match the grant root for Role.
func AuthorizeObject(g Grant, proof ObjectProof) error {
	if err := ValidateGrant(g); err != nil {
		return err
	}
	var want plumbing.Hash
	switch proof.Role {
	case RootHead:
		want = g.HeadSHA
	case RootBase:
		want = g.BaseSHA
	case RootStart:
		want = g.StartSHA
	default:
		return ErrAuthz
	}
	if proof.Hash == plumbing.ZeroHash || proof.Hash != want {
		return ErrAuthz
	}
	if proof.Type != "" && proof.Type != "commit" {
		return ErrAuthz
	}
	return nil
}

// Authorizer resolves an authoritative Grant from fresh canonical API state.
// Implementations must ignore any caller-supplied Grant fields.
type Authorizer interface {
	ResolveGrant(ctx context.Context, intent AcquireIntent) (Grant, error)
}

// StaticAuthorizer is a hermetic test adapter. It still rejects intents that
// fail the Allow predicate; it does not accept pre-populated Grants.
type StaticAuthorizer struct {
	Grant Grant
	Allow func(AcquireIntent) error
}

// ResolveGrant implements Authorizer.
func (s StaticAuthorizer) ResolveGrant(ctx context.Context, intent AcquireIntent) (Grant, error) {
	if err := ctx.Err(); err != nil {
		return Grant{}, err
	}
	if s.Allow != nil {
		if err := s.Allow(intent); err != nil {
			if ctx.Err() != nil {
				return Grant{}, ctx.Err()
			}
			return Grant{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Grant{}, err
	}
	g := s.Grant
	if intent.MRIID > 0 {
		g.MRIID = intent.MRIID
	}
	if err := matchExpected(intent, g); err != nil {
		return Grant{}, err
	}
	if err := ValidateGrant(g); err != nil {
		return Grant{}, err
	}
	return g, nil
}

func matchExpected(intent AcquireIntent, g Grant) error {
	if intent.ExpectedHead != plumbing.ZeroHash && intent.ExpectedHead != g.HeadSHA {
		return ErrAuthz
	}
	if intent.ExpectedBase != plumbing.ZeroHash && intent.ExpectedBase != g.BaseSHA {
		return ErrAuthz
	}
	if intent.ExpectedStart != plumbing.ZeroHash && intent.ExpectedStart != g.StartSHA {
		return ErrAuthz
	}
	if intent.ExpectedMRVersion != "" && intent.ExpectedMRVersion != g.MRVersion {
		return ErrAuthz
	}
	return nil
}

// ValidateDepth requires a bounded shallow depth of 1 or 2. Depth 0 (full
// history) and any other value are rejected.
func ValidateDepth(depth int) error {
	if depth != 1 && depth != 2 {
		return ErrLimit
	}
	return nil
}

// CapabilityGate rejects fetches that cannot satisfy the exact object contract.
func CapabilityGate(requireShallow, allowReachable, hasShallow bool, needUnadvertised bool) error {
	if requireShallow && !hasShallow {
		return ErrCapability
	}
	if needUnadvertised && !allowReachable {
		return ErrCapability
	}
	return nil
}

// TrustProvenanceFromIntent describes intent for legacy fixtures. It does not
// validate trust and is never used to authorize production warm acquisition.
func TrustProvenanceFromIntent(intent AcquireIntent) string {
	h := sha256.New()
	write := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	write(intent.Transport)
	write(intent.CAPath)
	if intent.Insecure {
		write("insecure=1")
	} else {
		write("insecure=0")
	}
	write(intent.AllowedInsecureHost)
	if intent.SSH.SSHConfig != nil {
		write("sshcfg=1")
	} else {
		write("sshcfg=0")
	}
	write(intent.SSH.KnownHosts)
	return hex.EncodeToString(h.Sum(nil))
}

// GrantFingerprint is a non-secret identity of the immutable grant binding.
func GrantFingerprint(g Grant) string {
	h := sha256.New()
	writeFP := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	writeFP(g.CanonicalInstance)
	writeFP(g.OriginHost)
	writeFP(g.ProjectID)
	writeFP(g.ProjectPath)
	writeFP(g.SourceFork)
	writeFP(g.SourcePath)
	writeFP(g.TargetProjectID)
	writeFP(g.TargetPath)
	writeFP(g.AuthDomain)
	writeFP(g.PolicyFP)
	writeFP(fmt.Sprintf("%d", g.MRIID))
	writeFP(g.MRVersion)
	writeFP(g.HeadRef)
	writeFP(g.BaseRef)
	writeFP(g.StartRef)
	writeFP(g.SourceHTTPSURL)
	writeFP(g.SourceSSHURL)
	writeFP(g.TargetHTTPSURL)
	writeFP(g.TargetSSHURL)
	writeFP(g.TrustProvenance)
	writeFP(g.ActorID)
	_, _ = h.Write(g.HeadSHA[:])
	_, _ = h.Write(g.BaseSHA[:])
	_, _ = h.Write(g.StartSHA[:])
	return hex.EncodeToString(h.Sum(nil))
}

// ParseSHA requires a full 40-hex object id.
func ParseSHA(s string) (plumbing.Hash, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if len(s) != 40 {
		return plumbing.ZeroHash, errors.New("gitcache: sha")
	}
	if _, err := hex.DecodeString(s); err != nil {
		return plumbing.ZeroHash, ErrAuthz
	}
	return plumbing.NewHash(s), nil
}

// MRVersionFromDiffRefs builds the immutable MR version binding from DiffRefs SHAs.
func MRVersionFromDiffRefs(head, base, start plumbing.Hash) string {
	return head.String() + ":" + base.String() + ":" + start.String()
}

// ValidateCloneURL binds the exact repo path and HTTPS scheme/port/relative root
// to the authoritative instance. SSH uses its independently approved port and
// native configuration, but still binds the exact project path and instance host.
func ValidateCloneURL(raw, instance, projectPath string, allowLoopback bool) error {
	t, err := origin.Parse(raw, allowLoopback)
	if err != nil {
		return ErrAuthz
	}
	u, err := url.Parse(instance)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ErrAuthz
	}
	ru, err := url.Parse(raw)
	if err != nil || ru.RawQuery != "" || ru.Fragment != "" || ru.RawPath != "" {
		return ErrAuthz
	}
	if !strings.EqualFold(t.Host, u.Hostname()) || projectPath == "" || strings.HasPrefix(projectPath, "/") {
		return ErrAuthz
	}
	prefix := strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/api/v4")
	expected := "/" + projectPath + ".git"
	if t.Scheme == "https" {
		port := u.Port()
		if port == "" {
			port = "443"
		}
		if u.Scheme != "https" || t.Port != port {
			return ErrAuthz
		}
		expected = prefix + expected
	}
	if t.Path != expected {
		return ErrAuthz
	}
	return nil
}
