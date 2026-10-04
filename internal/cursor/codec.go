// Package cursor implements authenticated opaque v1 pagination cursors.
package cursor

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// SchemaV1 is the locked cursor payload schema version.
	SchemaV1 = "gitlab-mcp-cursor-v1"
	// MaxPayloadBytes is the decoded JSON payload cap.
	MaxPayloadBytes = 8 << 10 // 8 KiB
	// MaxEncodedBytes is the encoded token input cap checked before decode.
	MaxEncodedBytes = 16 << 10 // 16 KiB
	// MinKeyBytes is the minimum raw operator signing key length.
	MinKeyBytes = 32
	// DefaultTTL is the fixed absolute cursor lifetime.
	DefaultTTL = 2 * time.Hour
	// ToolListCommits is the reference tool binding.
	ToolListCommits = "list_commits"
	// SectionListCommits is the reference section binding.
	SectionListCommits = "list_commits"
	// ToolReviewQueue is the canonical review-queue aggregate tool binding.
	ToolReviewQueue = "get_merge_request_review_queue"
	// SectionReviewQueue is the review-queue section binding.
	SectionReviewQueue = "review_queue"
	// QueueContSchemaRQ2 is the locked typed queue continuation schema.
	QueueContSchemaRQ2 = "rq2"
	// ResyncRequired is the uniform fail-closed continuation error token.
	ResyncRequired = "resync_required"
	// MaxImmutableRefs caps plural immutable ref bindings (e.g. MR base+head).
	MaxImmutableRefs = 8
	// MaxQueueCandidates caps signed canonical map entries.
	MaxQueueCandidates = 64
)

// ScopeKind enumerates supported binding scopes.
type ScopeKind string

const (
	ScopeProject    ScopeKind = "project"
	ScopeGroupQueue ScopeKind = "group_queue"
	ScopePipeline   ScopeKind = "pipeline"
)

// ErrResyncRequired is returned for tamper/expiry/rotation/binding/malformed failures.
var ErrResyncRequired = errors.New(ResyncRequired)

// ErrInvalidKey is returned when a present key fails length validation (never echoes key).
var ErrInvalidKey = errors.New("GITLAB_MCP_CURSOR_KEY must be a raw secret of at least 32 bytes")

// Clock abstracts time for absolute expiry checks.
type Clock interface {
	Now() time.Time
}

// RealClock uses wall-clock UTC.
type RealClock struct{}

// Now returns time.Now().UTC().
func (RealClock) Now() time.Time { return time.Now().UTC() }

// FakeClock is a test clock.
type FakeClock struct{ T time.Time }

// Now returns the fixed instant.
func (f *FakeClock) Now() time.Time { return f.T.UTC() }

// Advance moves the fake clock forward.
func (f *FakeClock) Advance(d time.Duration) { f.T = f.T.Add(d) }

// Filters holds normalized selection bound into the cursor.
// RefName/Path/Since/CallerUntil/PerPage are the original caller selection
// (strictly repeated on resume). Until is the discovery upper bound used in
// pin queries (may differ from CallerUntil). List queries use ImmutableRefs
// tip SHA as ref_name, not Filters.RefName.
type Filters struct {
	RefName     string `json:"ref_name"`
	Path        string `json:"path"`
	Since       string `json:"since"`
	CallerUntil string `json:"caller_until"`
	Until       string `json:"until"`
	Order       string `json:"order"`
	Selection   string `json:"selection"`
	PerPage     int    `json:"per_page"`
}

// PageState is signed pagination/guard state for the last returned page.
type PageState struct {
	Page             int    `json:"page"`
	PerPage          int    `json:"per_page"`
	SequenceDigest   string `json:"sequence_digest"`
	LastSHA          string `json:"last_sha"`
	ItemsOnPage      int    `json:"items_on_page"`
	ProviderNextPage int64  `json:"provider_next_page"`
}

// Scope binds project/optional MR, group queue, or project/pipeline.
type Scope struct {
	Kind            ScopeKind `json:"kind"`
	ProjectID       string    `json:"project_id,omitempty"`
	MergeRequestIID *int64    `json:"merge_request_iid,omitempty"`
	GroupID         string    `json:"group_id,omitempty"`
	PipelineID      *int64    `json:"pipeline_id,omitempty"`
}

// Payload is the authenticated v1 cursor body (never includes tokens/keys/notes).
// ImmutableRefs is plural so future MR adapters can bind base+head (and similar)
// while list_commits pins a single tip SHA as a one-element slice.
// QueueCont is omitempty and valid only for group_queue + review-queue tool/section.
type Payload struct {
	SchemaVersion string     `json:"schema_version"`
	Instance      string     `json:"instance"`
	ActorID       int64      `json:"actor_id"`
	PolicyFP      string     `json:"policy_fingerprint"`
	Tool          string     `json:"tool"`
	Section       string     `json:"section"`
	Scope         Scope      `json:"scope"`
	Filters       Filters    `json:"filters"`
	ImmutableRefs []string   `json:"immutable_refs"`
	UpperBound    string     `json:"upper_bound"`
	ExpiresAt     string     `json:"expires_at"`
	PageState     PageState  `json:"page_state"`
	QueueCont     *QueueCont `json:"queue_cont,omitempty"`
}

// QueueCont is the typed rq2 continuation for the review-queue aggregate.
type QueueCont struct {
	V     string            `json:"v"`
	Phase string            `json:"phase"`
	KI    int               `json:"ki"`
	Kinds []string          `json:"kinds"`
	KP    []QueueKindProg   `json:"kp"`
	OG    *QueueOngoingProg `json:"og"`
	CM    []QueueCandidate  `json:"cm"`
	EI    int               `json:"ei"`
	// Lim persists safe limitation codes across resumes (never note text).
	Lim []string `json:"lim,omitempty"`
	// Term is true when continuation must not be minted (capacity/provider terminal).
	Term bool `json:"term,omitempty"`
}

// QueueKindProg is per kind×state list-stream progress (reviewer/authored).
type QueueKindProg struct {
	Kind  string `json:"kind"`
	State string `json:"state"`
	P     int    `json:"p"`
	N     int64  `json:"n"`
	E     bool   `json:"e"`
	CN    int    `json:"cn"`
	PD    string `json:"pd"`
	PSz   int    `json:"psz"`
}

// QueueOngoingProg is sorted-seed discussion progress.
type QueueOngoingProg struct {
	SI  int    `json:"si"`
	DP  int    `json:"dp"`
	CN  int    `json:"cn"`
	PD  string `json:"pd"`
	PSz int    `json:"psz"`
	E   bool   `json:"e"`
}

// QueueCandidate is a confirmed canonical map entry.
type QueueCandidate struct {
	K string  `json:"k"`
	B int     `json:"b"`
	U string  `json:"u"`
	H *string `json:"h"`
}

// ValidateKey reports whether key meets the minimum raw length. Empty key is allowed (legacy).
func ValidateKey(key []byte) error {
	if len(key) == 0 {
		return nil
	}
	if len(key) < MinKeyBytes {
		return ErrInvalidKey
	}
	return nil
}

// Encode signs and serializes a payload. key must be at least MinKeyBytes.
func Encode(key []byte, p Payload) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	if len(key) == 0 {
		return "", fmt.Errorf("cursor signing key is not configured")
	}
	if err := validatePayload(&p); err != nil {
		return "", err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", ErrResyncRequired
	}
	if len(raw) > MaxPayloadBytes {
		return "", ErrResyncRequired
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(raw)
	sum := mac.Sum(nil)
	token := "v1." + base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(sum)
	if len(token) > MaxEncodedBytes {
		return "", ErrResyncRequired
	}
	return token, nil
}

// Decode verifies and parses a token. All failures collapse to ErrResyncRequired.
// JSON is strict: unknown fields and trailing payload content are rejected.
func Decode(key []byte, token string, now time.Time) (Payload, error) {
	var zero Payload
	if err := ValidateKey(key); err != nil {
		return zero, ErrResyncRequired
	}
	if len(key) == 0 {
		return zero, ErrResyncRequired
	}
	if len(token) == 0 || len(token) > MaxEncodedBytes {
		return zero, ErrResyncRequired
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return zero, ErrResyncRequired
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(raw) == 0 || len(raw) > MaxPayloadBytes {
		return zero, ErrResyncRequired
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != sha256.Size {
		return zero, ErrResyncRequired
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(raw)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return zero, ErrResyncRequired
	}
	var p Payload
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return zero, ErrResyncRequired
	}
	if err := decodeExactEnd(dec); err != nil {
		return zero, ErrResyncRequired
	}
	if err := validatePayload(&p); err != nil {
		return zero, ErrResyncRequired
	}
	exp, err := time.Parse(time.RFC3339, p.ExpiresAt)
	if err != nil {
		return zero, ErrResyncRequired
	}
	if !now.UTC().Before(exp.UTC()) {
		return zero, ErrResyncRequired
	}
	return p, nil
}

func decodeExactEnd(dec *json.Decoder) error {
	if dec.More() {
		return ErrResyncRequired
	}
	if _, err := dec.Token(); err != io.EOF {
		return ErrResyncRequired
	}
	return nil
}

func validatePayload(p *Payload) error {
	if p == nil {
		return ErrResyncRequired
	}
	if p.SchemaVersion != SchemaV1 {
		return ErrResyncRequired
	}
	if strings.TrimSpace(p.Instance) == "" || p.ActorID < 1 {
		return ErrResyncRequired
	}
	if strings.TrimSpace(p.PolicyFP) == "" || strings.TrimSpace(p.Tool) == "" || strings.TrimSpace(p.Section) == "" {
		return ErrResyncRequired
	}
	if strings.TrimSpace(p.UpperBound) == "" || strings.TrimSpace(p.ExpiresAt) == "" {
		return ErrResyncRequired
	}
	if _, err := time.Parse(time.RFC3339, p.UpperBound); err != nil {
		return ErrResyncRequired
	}
	if _, err := time.Parse(time.RFC3339, p.ExpiresAt); err != nil {
		return ErrResyncRequired
	}
	if p.Filters.PerPage < 1 || p.Filters.PerPage > 50 {
		return ErrResyncRequired
	}

	isQueue := p.Scope.Kind == ScopeGroupQueue && p.Tool == ToolReviewQueue && p.Section == SectionReviewQueue
	if p.QueueCont != nil && !isQueue {
		return ErrResyncRequired
	}
	if isQueue {
		if len(p.ImmutableRefs) != 0 {
			return ErrResyncRequired
		}
		if err := validateQueuePageStateEmpty(p.PageState); err != nil {
			return err
		}
		if p.QueueCont == nil {
			return ErrResyncRequired
		}
		if err := validateQueueCont(p.QueueCont, p.Filters.PerPage); err != nil {
			return err
		}
	} else {
		if err := validateImmutableRefs(p.ImmutableRefs); err != nil {
			return err
		}
		if err := validatePageState(p.PageState, p.Filters.PerPage); err != nil {
			return err
		}
	}

	switch p.Scope.Kind {
	case ScopeProject:
		if strings.TrimSpace(p.Scope.ProjectID) == "" {
			return ErrResyncRequired
		}
		if p.Scope.GroupID != "" || p.Scope.PipelineID != nil {
			return ErrResyncRequired
		}
		if p.Scope.MergeRequestIID != nil && *p.Scope.MergeRequestIID < 1 {
			return ErrResyncRequired
		}
	case ScopeGroupQueue:
		if strings.TrimSpace(p.Scope.GroupID) == "" {
			return ErrResyncRequired
		}
		if p.Scope.ProjectID != "" || p.Scope.MergeRequestIID != nil || p.Scope.PipelineID != nil {
			return ErrResyncRequired
		}
		if !isQueue {
			// group_queue without review-queue tool/section is not a valid wired token.
			return ErrResyncRequired
		}
	case ScopePipeline:
		if strings.TrimSpace(p.Scope.ProjectID) == "" || p.Scope.PipelineID == nil || *p.Scope.PipelineID < 1 {
			return ErrResyncRequired
		}
		if p.Scope.GroupID != "" || p.Scope.MergeRequestIID != nil {
			return ErrResyncRequired
		}
	default:
		return ErrResyncRequired
	}
	return nil
}

func validateQueuePageStateEmpty(ps PageState) error {
	if ps.Page != 0 || ps.PerPage != 0 || ps.ItemsOnPage != 0 || ps.ProviderNextPage != 0 {
		return ErrResyncRequired
	}
	if ps.LastSHA != "" || ps.SequenceDigest != "" {
		return ErrResyncRequired
	}
	return nil
}

func validateQueueCont(qc *QueueCont, filterPerPage int) error {
	if qc == nil || qc.V != QueueContSchemaRQ2 {
		return ErrResyncRequired
	}
	if qc.Phase != "discover" && qc.Phase != "emit" {
		return ErrResyncRequired
	}
	if len(qc.Kinds) < 1 || len(qc.Kinds) > 3 {
		return ErrResyncRequired
	}
	seenKind := map[string]struct{}{}
	wantOngoing := false
	for i, k := range qc.Kinds {
		if k != "reviewer" && k != "ongoing" && k != "authored" {
			return ErrResyncRequired
		}
		if _, dup := seenKind[k]; dup {
			return ErrResyncRequired
		}
		seenKind[k] = struct{}{}
		if i > 0 && qc.Kinds[i-1] >= k {
			return ErrResyncRequired // must be unique sorted
		}
		if k == "ongoing" {
			wantOngoing = true
		}
	}
	if qc.KI < 0 || qc.KI > len(qc.KP) {
		return ErrResyncRequired
	}
	if len(qc.CM) > MaxQueueCandidates {
		return ErrResyncRequired
	}
	if qc.EI < 0 || qc.EI > len(qc.CM) {
		return ErrResyncRequired
	}
	if qc.Phase == "discover" && qc.EI != 0 {
		return ErrResyncRequired
	}
	seenKey := map[string]struct{}{}
	wantBits := 0
	for _, k := range qc.Kinds {
		switch k {
		case "reviewer":
			wantBits |= 1
		case "ongoing":
			wantBits |= 2
		case "authored":
			wantBits |= 4
		}
	}
	for _, c := range qc.CM {
		if err := validateQueueCandidate(c); err != nil {
			return err
		}
		if c.B&^wantBits != 0 {
			return ErrResyncRequired // bits must be subset of requested kinds
		}
		if _, dup := seenKey[c.K]; dup {
			return ErrResyncRequired
		}
		seenKey[c.K] = struct{}{}
	}
	seenStream := map[string]struct{}{}
	for _, kp := range qc.KP {
		if kp.Kind != "reviewer" && kp.Kind != "authored" {
			return ErrResyncRequired
		}
		if _, ok := seenKind[kp.Kind]; !ok {
			return ErrResyncRequired
		}
		if kp.State != "opened" && kp.State != "closed" && kp.State != "merged" {
			return ErrResyncRequired
		}
		sk := kp.Kind + "|" + kp.State
		if _, dup := seenStream[sk]; dup {
			return ErrResyncRequired
		}
		seenStream[sk] = struct{}{}
		if err := validateReplayProg(kp.P, kp.N, kp.E, kp.CN, kp.PD, kp.PSz, filterPerPage); err != nil {
			return err
		}
	}
	// Every non-ongoing requested kind must have exactly the requested state streams present
	// (validated loosely here: kp kinds subset of requested; stream count checked by tool).
	if wantOngoing {
		if qc.OG == nil {
			return ErrResyncRequired
		}
		if qc.OG.SI < 0 || qc.OG.DP < 1 {
			return ErrResyncRequired
		}
		if err := validateReplayProg(qc.OG.DP, 0, qc.OG.E, qc.OG.CN, qc.OG.PD, qc.OG.PSz, filterPerPage); err != nil {
			return err
		}
	} else if qc.OG != nil {
		return ErrResyncRequired
	}
	for _, code := range qc.Lim {
		if strings.TrimSpace(code) == "" {
			return ErrResyncRequired
		}
	}
	return nil
}

func validateReplayProg(page int, next int64, exhausted bool, cn int, pd string, psz, filterPerPage int) error {
	if page < 1 || psz < 1 || psz > 50 || psz != filterPerPage {
		return ErrResyncRequired
	}
	if cn < 0 || cn > psz {
		return ErrResyncRequired
	}
	if next < 0 {
		return ErrResyncRequired
	}
	if cn == 0 {
		if pd != "" {
			return ErrResyncRequired
		}
	} else if !isHexSHA256(pd) {
		return ErrResyncRequired
	}
	if exhausted && next != 0 {
		return ErrResyncRequired
	}
	_ = exhausted
	return nil
}

func validateQueueCandidate(c QueueCandidate) error {
	if !validQueueKey(c.K) {
		return ErrResyncRequired
	}
	if c.B <= 0 || c.B > 7 {
		return ErrResyncRequired
	}
	if _, err := time.Parse(time.RFC3339Nano, c.U); err != nil {
		if _, err2 := time.Parse(time.RFC3339, c.U); err2 != nil {
			return ErrResyncRequired
		}
	}
	if c.H != nil {
		h := strings.TrimSpace(*c.H)
		if len(h) != 40 {
			return ErrResyncRequired
		}
		for i := 0; i < 40; i++ {
			ch := h[i]
			switch {
			case ch >= '0' && ch <= '9', ch >= 'a' && ch <= 'f':
			default:
				return ErrResyncRequired
			}
		}
	}
	return nil
}

func validQueueKey(k string) bool {
	parts := strings.Split(k, ":")
	if len(parts) != 2 {
		return false
	}
	pid, err1 := strconv.ParseInt(parts[0], 10, 64)
	iid, err2 := strconv.ParseInt(parts[1], 10, 64)
	return err1 == nil && err2 == nil && pid > 0 && iid > 0
}

func validateImmutableRefs(refs []string) error {
	if len(refs) < 1 || len(refs) > MaxImmutableRefs {
		return ErrResyncRequired
	}
	seen := map[string]struct{}{}
	for _, r := range refs {
		s := strings.TrimSpace(r)
		if s == "" {
			return ErrResyncRequired
		}
		if _, dup := seen[s]; dup {
			return ErrResyncRequired
		}
		seen[s] = struct{}{}
	}
	return nil
}

// validatePageState rejects skip/loop/jump and incomplete resumable guard state.
func validatePageState(ps PageState, filterPerPage int) error {
	if ps.Page < 1 || ps.PerPage != filterPerPage || ps.PerPage < 1 || ps.PerPage > 50 {
		return ErrResyncRequired
	}
	if ps.ItemsOnPage < 0 || ps.ItemsOnPage > ps.PerPage {
		return ErrResyncRequired
	}
	if ps.ProviderNextPage < 0 {
		return ErrResyncRequired
	}
	if ps.ProviderNextPage == 0 {
		// Terminal page: when items present, digest/boundary required.
		if ps.ItemsOnPage > 0 {
			if strings.TrimSpace(ps.LastSHA) == "" || !isHexSHA256(ps.SequenceDigest) {
				return ErrResyncRequired
			}
		} else if ps.SequenceDigest != "" || ps.LastSHA != "" {
			return ErrResyncRequired
		}
		return nil
	}
	// Resumable: strict sequential next page only (no repeat/skip/jump).
	if ps.ProviderNextPage != int64(ps.Page)+1 {
		return ErrResyncRequired
	}
	if ps.ItemsOnPage < 1 {
		return ErrResyncRequired
	}
	if strings.TrimSpace(ps.LastSHA) == "" || !isHexSHA256(ps.SequenceDigest) {
		return ErrResyncRequired
	}
	return nil
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// SequenceDigest returns hex(SHA256) over provider-ordered commit SHAs joined by '\n'.
func SequenceDigest(shas []string) string {
	h := sha256.New()
	for i, s := range shas {
		if i > 0 {
			_, _ = h.Write([]byte{'\n'})
		}
		_, _ = h.Write([]byte(strings.TrimSpace(s)))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// MatchBinding compares resume request bindings against a decoded payload.
func MatchBinding(p Payload, instance string, actorID int64, policyFP, tool, section string, scope Scope, filters Filters, immutableRefs []string, upperBound string) error {
	if p.Instance != instance || p.ActorID != actorID || p.PolicyFP != policyFP {
		return ErrResyncRequired
	}
	if p.Tool != tool || p.Section != section {
		return ErrResyncRequired
	}
	if p.UpperBound != upperBound {
		return ErrResyncRequired
	}
	if !stringSliceEq(p.ImmutableRefs, immutableRefs) {
		return ErrResyncRequired
	}
	if p.Scope.Kind != scope.Kind || p.Scope.ProjectID != scope.ProjectID || p.Scope.GroupID != scope.GroupID {
		return ErrResyncRequired
	}
	if !int64PtrEq(p.Scope.MergeRequestIID, scope.MergeRequestIID) || !int64PtrEq(p.Scope.PipelineID, scope.PipelineID) {
		return ErrResyncRequired
	}
	if p.Filters != filters {
		return ErrResyncRequired
	}
	return nil
}

func stringSliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func int64PtrEq(a, b *int64) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// TipRef returns the primary immutable tip ref (index 0) for single-tip tools.
func TipRef(refs []string) string {
	if len(refs) == 0 {
		return ""
	}
	return refs[0]
}

// ErrInvalidInstance is returned when an API URL cannot be canonicalized.
// It never includes the raw URL (which may carry userinfo/query secrets).
var ErrInvalidInstance = errors.New("invalid GitLab API instance URL")

// CanonicalInstance returns a credential-safe instance binding identity:
// scheme + lowercased host + non-default port + path, with no userinfo, query,
// or fragment. Default ports (http/80, https/443) are omitted. Trailing slashes
// on the path are stripped. The raw input is never echoed in errors.
func CanonicalInstance(apiURL string) (string, error) {
	raw := strings.TrimSpace(apiURL)
	if raw == "" {
		return "", ErrInvalidInstance
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", ErrInvalidInstance
	}
	scheme := strings.ToLower(strings.TrimSpace(u.Scheme))
	if scheme != "http" && scheme != "https" {
		return "", ErrInvalidInstance
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "" {
		return "", ErrInvalidInstance
	}
	// Intentionally drop u.User, u.RawQuery, u.Fragment — never serialize credentials.
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	hostPort := host
	if port != "" {
		hostPort = net.JoinHostPort(host, port)
	}
	path := u.EscapedPath()
	if path == "" {
		path = u.Path
	}
	path = strings.TrimRight(path, "/")
	if path == "/" {
		path = ""
	}
	return scheme + "://" + hostPort + path, nil
}
