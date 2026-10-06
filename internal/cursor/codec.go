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
	// ToolReviewContext is the batch review-context aggregate tool binding.
	ToolReviewContext = "get_merge_request_review_context"
	// SectionReviewContext is the review-context token section binding.
	SectionReviewContext = "review_context"
	// SectionReviewDiscussions is the discussions continuation section binding.
	SectionReviewDiscussions = "discussions"
	// DiscussionsContSchemaDC1 is the locked discussions note-continuation schema.
	DiscussionsContSchemaDC1 = "dc1"
	// ToolDiffWindow is the immutable diff-manifest window tool.
	ToolDiffWindow = "get_merge_request_diff_window"
	// SectionDiffManifest is the diff manifest section binding.
	SectionDiffManifest = "diff_manifest"
	// DiffWindowSchemaDM1 is the window continuation schema.
	DiffWindowSchemaDM1 = "dm1"
	// DiffManifestEvidenceV1 is the only complete-manifest evidence version.
	DiffManifestEvidenceV1 = "diff_manifest.v1"
	// ToolPipelineGraph is the parent/downstream CI graph aggregate.
	ToolPipelineGraph = "get_merge_request_pipeline_graph"
	// SectionPipelineGraph is the pipeline graph section binding.
	SectionPipelineGraph = "pipeline_graph"
	// GraphContSchemaG1 is the locked graph-walk continuation schema.
	GraphContSchemaG1 = "g1"
	// GraphPhaseJobs is the jobs-list phase of a graph node.
	GraphPhaseJobs = "jobs"
	// GraphPhaseBridges is the bridges-list phase of a graph node.
	GraphPhaseBridges = "bridges"
	// MaxGraphVisited caps signed visited+queue entries.
	MaxGraphVisited = 16
	// MaxGraphReachEdges caps bridge/shared reachability entries for a walk
	// that stays within MaxGraphVisited nodes.
	MaxGraphReachEdges = MaxGraphVisited * MaxGraphVisited
	// MaxGraphEvidenceReplayItems caps retained-item replay per completed node.
	MaxGraphEvidenceReplayItems = 1 << 20
	// ReviewWriteFresh is the absolute write-freshness window from retrieved_at.
	ReviewWriteFresh = 5 * time.Minute
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
	ScopeProject       ScopeKind = "project"
	ScopeGroupQueue    ScopeKind = "group_queue"
	ScopePipeline      ScopeKind = "pipeline"
	ScopeReviewContext ScopeKind = "review_context"
)

// ReviewContextSections is the closed section vocabulary for a review-context token.
var ReviewContextSections = []string{
	"metadata",
	"approvals",
	"discussions",
	"pipeline_graph",
	"diff_manifest",
}

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
// LineageMax is used only by the parent pipeline graph. Each entry is
// "<16 hex digits> <job id>" for an FNV-64a fingerprint of a job name and
// the greatest id seen for it. A leading "*" means further names were
// omitted so the payload stays under the size cap. Other tools leave it
// empty, and omitempty keeps their tokens unchanged.
type PageState struct {
	Page             int      `json:"page"`
	PerPage          int      `json:"per_page"`
	SequenceDigest   string   `json:"sequence_digest"`
	LastSHA          string   `json:"last_sha"`
	ItemsOnPage      int      `json:"items_on_page"`
	ProviderNextPage int64    `json:"provider_next_page"`
	LineageMax       []string `json:"lineage_max,omitempty"`
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
	SchemaVersion   string           `json:"schema_version"`
	Instance        string           `json:"instance"`
	ActorID         int64            `json:"actor_id"`
	PolicyFP        string           `json:"policy_fingerprint"`
	Tool            string           `json:"tool"`
	Section         string           `json:"section"`
	Scope           Scope            `json:"scope"`
	Filters         Filters          `json:"filters"`
	ImmutableRefs   []string         `json:"immutable_refs"`
	UpperBound      string           `json:"upper_bound"`
	ExpiresAt       string           `json:"expires_at"`
	PageState       PageState        `json:"page_state"`
	QueueCont       *QueueCont       `json:"queue_cont,omitempty"`
	ContextRef      *ContextRef      `json:"context_ref,omitempty"`
	DiscussionsCont *DiscussionsCont `json:"discussions_cont,omitempty"`
	DiffWindow      *DiffWindowCont  `json:"diff_window,omitempty"`
	GraphCont       *GraphCont       `json:"graph_cont,omitempty"`
}

// GraphCont is the g1 walk continuation for pipeline_graph.
// It stores node identities and policy flags only: no job names or traces.
type GraphCont struct {
	V     string   `json:"v"`
	Phase string   `json:"phase"`
	NP    string   `json:"np"`
	NI    int64    `json:"ni"`
	D     int      `json:"d"`
	Vis   []string `json:"vis,omitempty"`
	Q     []string `json:"q,omitempty"`
	Anc   []string `json:"anc,omitempty"`
	Ev    []string `json:"ev,omitempty"`
	Ei    []string `json:"ei,omitempty"`
	JD    string   `json:"jd,omitempty"`
	BD    string   `json:"bd,omitempty"`
	N     int      `json:"n"`
	Block bool     `json:"block,omitempty"`
	Part  bool     `json:"part,omitempty"`
	Unk   bool     `json:"unk,omitempty"`
	Cov   string   `json:"cov"`
	Cap   string   `json:"cap,omitempty"`
	Inc   bool     `json:"inc,omitempty"`
	Rsn   []string `json:"rsn,omitempty"`
	RP    string   `json:"rp,omitempty"`
	RI    int64    `json:"ri,omitempty"`
	RK    string   `json:"rk,omitempty"`
	Prv   bool     `json:"prv,omitempty"`
	RS    string   `json:"rs,omitempty"`
	Rg    []string `json:"rg,omitempty"`
	// Rgx marks a reachability snapshot shortened to fit the payload cap.
	Rgx bool `json:"rgx,omitempty"`
}

// DiffWindowCont binds one diff-manifest window. It stores no patch text.
type DiffWindowCont struct {
	V          string `json:"v"`
	Mode       string `json:"mode"`
	VersionID  int64  `json:"version_id,omitempty"`
	BaseSHA    string `json:"base_sha,omitempty"`
	StartSHA   string `json:"start_sha,omitempty"`
	HeadSHA    string `json:"head_sha,omitempty"`
	FromSHA    string `json:"from_sha,omitempty"`
	ToSHA      string `json:"to_sha,omitempty"`
	Straight   bool   `json:"straight,omitempty"`
	Total      int    `json:"total"`
	Offset     int    `json:"offset"`
	FullDigest string `json:"full_digest"`
	PerPage    int    `json:"per_page"`
}

// DiscussionsCont is the dc1 note continuation for review-context discussions.
// Coordinates and prefix hashes only: no note bodies, timestamps, or unread ids.
type DiscussionsCont struct {
	V   string `json:"v"`
	P   int    `json:"p"`
	DI  int    `json:"di"`
	NI  int    `json:"ni"`
	DID string `json:"did,omitempty"`
	DP  string `json:"dp,omitempty"`
	ND  string `json:"nd,omitempty"`
}

// ContextRef is one merge request's signed review evidence.
// It is not a batch scope and does not claim a sibling was read.
type ContextRef struct {
	OwnerProjectID    int64             `json:"owner_project_id"`
	SourceProjectID   int64             `json:"source_project_id"`
	TargetProjectID   int64             `json:"target_project_id"`
	SourceBranch      string            `json:"source_branch"`
	TargetBranch      string            `json:"target_branch"`
	SourceSHA         string            `json:"source_sha"`
	TargetSHA         string            `json:"target_sha"`
	VersionID         int64             `json:"version_id"`
	VersionHead       string            `json:"version_head"`
	VersionBase       string            `json:"version_base"`
	VersionStart      string            `json:"version_start"`
	Requested         []string          `json:"requested"`
	Complete          []string          `json:"complete"`
	Excluded          []string          `json:"excluded"`
	Digests           map[string]string `json:"digests"`
	Evidence          map[string]string `json:"evidence,omitempty"`
	RetrievedAt       string            `json:"retrieved_at"`
	WriteFreshUntil   string            `json:"write_fresh_until"`
	BracketConsistent bool              `json:"bracket_consistent"`
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
	if err := rejectDuplicateJSONKeys(raw); err != nil {
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
	exp, err := time.Parse(time.RFC3339Nano, p.ExpiresAt)
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
	// Pinned until and expires_at may carry fractional seconds. RFC3339Nano accepts both forms.
	if _, err := time.Parse(time.RFC3339Nano, p.UpperBound); err != nil {
		return ErrResyncRequired
	}
	if _, err := time.Parse(time.RFC3339Nano, p.ExpiresAt); err != nil {
		return ErrResyncRequired
	}
	if p.Filters.PerPage < 1 || p.Filters.PerPage > 50 {
		return ErrResyncRequired
	}

	isQueue := p.Scope.Kind == ScopeGroupQueue && p.Tool == ToolReviewQueue && p.Section == SectionReviewQueue
	isReview := p.Scope.Kind == ScopeReviewContext && p.Tool == ToolReviewContext && p.Section == SectionReviewContext
	isDisc := p.Tool == ToolReviewContext && p.Section == SectionReviewDiscussions
	isDiff := p.Tool == ToolDiffWindow && p.Section == SectionDiffManifest
	isGraph := p.Tool == ToolPipelineGraph && p.Section == SectionPipelineGraph
	if p.QueueCont != nil && !isQueue {
		return ErrResyncRequired
	}
	if p.DiffWindow != nil && !isDiff {
		return ErrResyncRequired
	}
	if p.ContextRef != nil && !isReview {
		return ErrResyncRequired
	}
	if p.DiscussionsCont != nil && !isDisc {
		return ErrResyncRequired
	}
	if p.GraphCont != nil && !isGraph {
		return ErrResyncRequired
	}
	if isReview {
		if len(p.ImmutableRefs) != 0 || p.Filters.PerPage != 1 {
			return ErrResyncRequired
		}
		if err := validateQueuePageStateEmpty(p.PageState); err != nil {
			return err
		}
		if err := validateContextRef(p); err != nil {
			return err
		}
	} else if isQueue {
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
	} else if isDisc {
		if err := validateDiscussionsCursor(p); err != nil {
			return err
		}
	} else if isGraph {
		if p.GraphCont == nil || p.ContextRef != nil || p.QueueCont != nil || p.DiscussionsCont != nil || p.DiffWindow != nil {
			return ErrResyncRequired
		}
		if err := validateImmutableRefs(p.ImmutableRefs); err != nil {
			return err
		}
		if err := validatePageState(p.PageState, p.Filters.PerPage); err != nil {
			return err
		}
		if err := validateGraphCont(p.GraphCont); err != nil {
			return err
		}
	} else if isDiff {
		if p.DiffWindow == nil || p.ContextRef != nil || p.QueueCont != nil || p.DiscussionsCont != nil || p.GraphCont != nil {
			return ErrResyncRequired
		}
		if p.Scope.Kind != ScopeProject || strings.TrimSpace(p.Scope.ProjectID) == "" || p.Scope.MergeRequestIID == nil || *p.Scope.MergeRequestIID < 1 {
			return ErrResyncRequired
		}
		if err := validateImmutableRefs(p.ImmutableRefs); err != nil {
			return err
		}
		if err := validatePageState(p.PageState, p.Filters.PerPage); err != nil {
			return err
		}
		if err := validateDiffWindow(p); err != nil {
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
	case ScopeReviewContext:
		if !isReview {
			return ErrResyncRequired
		}
		if strings.TrimSpace(p.Scope.ProjectID) == "" || p.Scope.MergeRequestIID == nil || *p.Scope.MergeRequestIID < 1 {
			return ErrResyncRequired
		}
		if p.Scope.GroupID != "" || p.Scope.PipelineID != nil {
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
	if qc.Phase == "emit" {
		if qc.KI != len(qc.KP) {
			return ErrResyncRequired
		}
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
		if kp.E && (kp.CN != 0 || kp.PD != "" || kp.N != 0) {
			return ErrResyncRequired
		}
	}
	for i := 0; i < qc.KI && i < len(qc.KP); i++ {
		if !qc.KP[i].E {
			return ErrResyncRequired
		}
	}
	if qc.Phase == "emit" {
		for _, kp := range qc.KP {
			if !kp.E {
				return ErrResyncRequired
			}
		}
	}
	// Exact kind×state product is checked again by the tool against the normalized request.
	// Here, streams must be sorted and each non-ongoing kind must share one state set.
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
		if qc.OG.E && (qc.OG.CN != 0 || qc.OG.PD != "") {
			return ErrResyncRequired
		}
		if qc.Phase == "emit" && !qc.OG.E {
			return ErrResyncRequired
		}
	} else if qc.OG != nil {
		return ErrResyncRequired
	}
	if err := validateQueueLim(qc.Lim); err != nil {
		return err
	}
	if err := validateQueueStreamOrder(qc.KP); err != nil {
		return err
	}
	return nil
}

func validateQueueStreamOrder(kp []QueueKindProg) error {
	for i := 1; i < len(kp); i++ {
		if kp[i-1].Kind > kp[i].Kind {
			return ErrResyncRequired
		}
		if kp[i-1].Kind == kp[i].Kind && kp[i-1].State >= kp[i].State {
			return ErrResyncRequired
		}
	}
	return nil
}

// queueLimitationCodes is the closed set persisted in rq2 lim. It mirrors the
// readmeta code list so the codec can fail closed without importing tools.
var queueLimitationCodes = map[string]struct{}{
	"inaccessible":            {},
	"unsupported":             {},
	"partial":                 {},
	"inconsistent":            {},
	"collapsed":               {},
	"too_large":               {},
	"budget_items":            {},
	"budget_bytes":            {},
	"budget_elapsed":          {},
	"budget_requests":         {},
	"http_error":              {},
	"cancelled":               {},
	"unknown_count":           {},
	"authz_denied":            {},
	"identity_unresolved":     {},
	"dedupe_capacity":         {},
	"membership_incomplete":   {},
	"cursor_capacity":         {},
	"provider_page_ambiguous": {},
}

func validateQueueLim(codes []string) error {
	seen := map[string]struct{}{}
	for _, code := range codes {
		if _, ok := queueLimitationCodes[code]; !ok {
			return ErrResyncRequired
		}
		if _, dup := seen[code]; dup {
			return ErrResyncRequired
		}
		seen[code] = struct{}{}
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
	if exhausted && (next != 0 || cn != 0 || pd != "") {
		return ErrResyncRequired
	}
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
	if err1 != nil || err2 != nil || pid <= 0 || iid <= 0 {
		return false
	}
	// Reject padded numerics (042:1) so distinct spellings cannot bypass duplicate detection.
	return parts[0] == strconv.FormatInt(pid, 10) && parts[1] == strconv.FormatInt(iid, 10)
}

func rejectDuplicateJSONKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return walkJSONNoDup(dec)
}

func walkJSONNoDup(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyTok.(string)
			if !ok {
				return ErrResyncRequired
			}
			if _, dup := seen[key]; dup {
				return ErrResyncRequired
			}
			seen[key] = struct{}{}
			if err := walkJSONNoDup(dec); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := end.(json.Delim); !ok || d != '}' {
			return ErrResyncRequired
		}
		return nil
	case '[':
		for dec.More() {
			if err := walkJSONNoDup(dec); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := end.(json.Delim); !ok || d != ']' {
			return ErrResyncRequired
		}
		return nil
	default:
		return ErrResyncRequired
	}
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

func validateContextRef(p *Payload) error {
	if p == nil || p.ContextRef == nil {
		return ErrResyncRequired
	}
	ref := p.ContextRef
	if ref.OwnerProjectID < 1 || ref.SourceProjectID < 1 || ref.TargetProjectID < 1 || ref.VersionID < 1 {
		return ErrResyncRequired
	}
	if p.Scope.ProjectID != strconv.FormatInt(ref.OwnerProjectID, 10) {
		return ErrResyncRequired
	}
	if strings.TrimSpace(ref.SourceBranch) == "" || ref.SourceBranch != strings.TrimSpace(ref.SourceBranch) {
		return ErrResyncRequired
	}
	if strings.TrimSpace(ref.TargetBranch) == "" || ref.TargetBranch != strings.TrimSpace(ref.TargetBranch) {
		return ErrResyncRequired
	}
	if !isGitSHA(ref.SourceSHA) || !isGitSHA(ref.TargetSHA) || !isGitSHA(ref.VersionHead) || !isGitSHA(ref.VersionBase) || !isGitSHA(ref.VersionStart) {
		return ErrResyncRequired
	}
	if ref.SourceSHA != ref.VersionHead {
		return ErrResyncRequired
	}
	if !ref.BracketConsistent || len(ref.Complete) == 0 {
		return ErrResyncRequired
	}
	if !validSectionMask(ref.Requested) || !validCompleteMask(ref.Complete) || !validSectionMask(ref.Excluded) {
		return ErrResyncRequired
	}
	seen := map[string]struct{}{}
	for _, name := range ref.Complete {
		if _, ok := seen[name]; ok {
			return ErrResyncRequired
		}
		seen[name] = struct{}{}
	}
	for _, name := range ref.Excluded {
		if _, ok := seen[name]; ok {
			return ErrResyncRequired
		}
		seen[name] = struct{}{}
	}
	if len(seen) != len(ref.Requested) {
		return ErrResyncRequired
	}
	for _, name := range ref.Requested {
		if _, ok := seen[name]; !ok {
			return ErrResyncRequired
		}
		delete(seen, name)
	}
	if len(seen) != 0 {
		return ErrResyncRequired
	}
	if len(ref.Digests) != len(ref.Complete) {
		return ErrResyncRequired
	}
	for _, name := range ref.Complete {
		sum, ok := ref.Digests[name]
		if !ok || !isHexSHA256(sum) {
			return ErrResyncRequired
		}
	}
	for name := range ref.Digests {
		if !containsString(ref.Complete, name) {
			return ErrResyncRequired
		}
	}
	ret, err := time.Parse(time.RFC3339, ref.RetrievedAt)
	if err != nil {
		return ErrResyncRequired
	}
	fresh, err := time.Parse(time.RFC3339, ref.WriteFreshUntil)
	if err != nil {
		return ErrResyncRequired
	}
	exp, err := time.Parse(time.RFC3339, p.ExpiresAt)
	if err != nil {
		return ErrResyncRequired
	}
	if p.UpperBound != ref.RetrievedAt || p.Filters.Selection != strings.Join(ref.Requested, ",") {
		return ErrResyncRequired
	}
	if !fresh.Equal(ret.Add(ReviewWriteFresh)) || !exp.Equal(ret.Add(DefaultTTL)) || !fresh.Before(exp) {
		return ErrResyncRequired
	}
	return validateDiffManifestEvidence(ref)
}

func validateDiffManifestEvidence(ref *ContextRef) error {
	if ref == nil {
		return ErrResyncRequired
	}
	inComplete := containsString(ref.Complete, "diff_manifest")
	inExcluded := containsString(ref.Excluded, "diff_manifest")
	if inComplete && inExcluded {
		return ErrResyncRequired
	}
	if !inComplete {
		if len(ref.Evidence) != 0 {
			return ErrResyncRequired
		}
		return nil
	}
	if len(ref.Evidence) != 1 || ref.Evidence["diff_manifest"] != DiffManifestEvidenceV1 {
		return ErrResyncRequired
	}
	return nil
}

func validateDiffWindow(p *Payload) error {
	if p == nil || p.DiffWindow == nil {
		return ErrResyncRequired
	}
	w := p.DiffWindow
	if w.V != DiffWindowSchemaDM1 || w.PerPage != p.Filters.PerPage || w.PerPage < 1 || w.PerPage > 50 {
		return ErrResyncRequired
	}
	if w.Total < 1 || w.Offset < 1 || w.Offset >= w.Total || w.Offset%w.PerPage != 0 {
		return ErrResyncRequired
	}
	if !isHexSHA256(w.FullDigest) || p.PageState.SequenceDigest != p.PageState.LastSHA || !isHexSHA256(p.PageState.SequenceDigest) {
		return ErrResyncRequired
	}
	if p.PageState.Page < 1 || w.Offset != p.PageState.Page*w.PerPage {
		return ErrResyncRequired
	}
	if p.PageState.ItemsOnPage != w.PerPage || p.PageState.ProviderNextPage != int64(p.PageState.Page)+1 {
		return ErrResyncRequired
	}
	if p.Filters.Selection != diffWindowSelection(w) || p.Filters.Until != p.UpperBound {
		return ErrResyncRequired
	}
	ub, err := time.Parse(time.RFC3339, p.UpperBound)
	if err != nil {
		return ErrResyncRequired
	}
	exp, err := time.Parse(time.RFC3339, p.ExpiresAt)
	if err != nil || !exp.Equal(ub.Add(DefaultTTL)) {
		return ErrResyncRequired
	}
	switch w.Mode {
	case "full_version":
		if w.VersionID < 1 || w.BaseSHA != "" || w.StartSHA != "" || w.HeadSHA != "" || w.FromSHA != "" || w.ToSHA != "" || w.Straight {
			return ErrResyncRequired
		}
	case "full_tuple":
		if w.VersionID != 0 || !isGitSHA(w.BaseSHA) || !isGitSHA(w.StartSHA) || !isGitSHA(w.HeadSHA) || w.FromSHA != "" || w.ToSHA != "" || w.Straight {
			return ErrResyncRequired
		}
	case "incremental":
		if w.VersionID != 0 || w.BaseSHA != "" || w.StartSHA != "" || w.HeadSHA != "" || !isGitSHA(w.FromSHA) || !isGitSHA(w.ToSHA) || !w.Straight {
			return ErrResyncRequired
		}
	default:
		return ErrResyncRequired
	}
	return nil
}

func diffWindowSelection(w *DiffWindowCont) string {
	if w == nil {
		return ""
	}
	switch w.Mode {
	case "full_version":
		return fmt.Sprintf("version:%d", w.VersionID)
	case "full_tuple":
		return "tuple:" + w.BaseSHA + ":" + w.StartSHA + ":" + w.HeadSHA
	case "incremental":
		return "inc:" + w.FromSHA + ":" + w.ToSHA
	default:
		return ""
	}
}

func validSectionMask(names []string) bool {
	return validNameMask(names, reviewSectionName)
}

func validCompleteMask(names []string) bool {
	return validNameMask(names, reviewCompleteEvidence)
}

func validNameMask(names []string, allow func(string) bool) bool {
	if names == nil {
		return false
	}
	for i, name := range names {
		if !allow(name) {
			return false
		}
		if i > 0 && names[i] <= names[i-1] {
			return false
		}
	}
	return true
}

func reviewSectionName(name string) bool {
	switch name {
	case "metadata", "approvals", "discussions", "pipeline_graph", "diff_manifest":
		return true
	default:
		return false
	}
}

func reviewCompleteEvidence(name string) bool {
	return name == "metadata" || name == "approvals" || name == "discussions" || name == "diff_manifest" || name == "pipeline_graph"
}

func validateDiscussionsCursor(p *Payload) error {
	if p == nil || p.DiscussionsCont == nil || p.Scope.Kind != ScopeProject {
		return ErrResyncRequired
	}
	if strings.TrimSpace(p.Scope.ProjectID) == "" || p.Scope.GroupID != "" || p.Scope.PipelineID != nil {
		return ErrResyncRequired
	}
	if p.Scope.MergeRequestIID == nil || *p.Scope.MergeRequestIID < 1 {
		return ErrResyncRequired
	}
	if p.ContextRef != nil || p.QueueCont != nil {
		return ErrResyncRequired
	}
	if err := validateQueuePageStateEmpty(p.PageState); err != nil {
		return err
	}
	if p.Filters.PerPage != 20 || p.Filters.Order != "provider" {
		return ErrResyncRequired
	}
	if p.Filters.Selection != "semantic" && p.Filters.Selection != "all" {
		return ErrResyncRequired
	}
	if p.Filters.Until == "" || p.Filters.Until != p.UpperBound {
		return ErrResyncRequired
	}
	if p.Filters.RefName != "" || p.Filters.Path != "" || p.Filters.Since != "" || p.Filters.CallerUntil != "" {
		return ErrResyncRequired
	}
	if len(p.ImmutableRefs) != 5 || p.ImmutableRefs[0] != p.ImmutableRefs[2] {
		return ErrResyncRequired
	}
	for _, ref := range p.ImmutableRefs {
		if !isGitSHA(ref) {
			return ErrResyncRequired
		}
	}
	c := p.DiscussionsCont
	if c.V != DiscussionsContSchemaDC1 || c.P < 1 || c.DI < 0 || c.NI < 0 {
		return ErrResyncRequired
	}
	if c.DI == 0 {
		if c.DP != "" {
			return ErrResyncRequired
		}
	} else if !isHexSHA256(c.DP) {
		return ErrResyncRequired
	}
	if c.NI == 0 {
		if c.ND != "" || c.DID != "" {
			return ErrResyncRequired
		}
	} else if c.DID == "" || !isHexSHA256(c.ND) {
		return ErrResyncRequired
	}
	return nil
}

func validateGraphCont(c *GraphCont) error {
	if c == nil || c.V != GraphContSchemaG1 {
		return ErrResyncRequired
	}
	if c.Phase != GraphPhaseJobs && c.Phase != GraphPhaseBridges {
		return ErrResyncRequired
	}
	if strings.TrimSpace(c.NP) == "" || c.NP != strings.TrimSpace(c.NP) || c.NI < 1 || c.D < 0 {
		return ErrResyncRequired
	}
	if c.N < 1 || c.N > MaxGraphVisited {
		return ErrResyncRequired
	}
	if c.Cov != "unknown" && c.Cov != "partial" && c.Cov != "complete" {
		return ErrResyncRequired
	}
	if c.Cap != "" && c.Cap != "bridges" && c.Cap != "unknown" {
		return ErrResyncRequired
	}
	if err := validateGraphKeyList(c.Vis, false); err != nil {
		return err
	}
	if err := validateGraphKeyList(c.Q, true); err != nil {
		return err
	}
	if len(c.Vis)+len(c.Q) > MaxGraphVisited {
		return ErrResyncRequired
	}
	if err := validateGraphAncestry(c.Anc, c.D); err != nil {
		return err
	}
	if err := validateGraphEvidence(c); err != nil {
		return err
	}
	if err := validateGraphReach(c.Rg); err != nil {
		return err
	}
	if c.RI < 0 || (c.RI == 0 && c.RP != "") {
		return ErrResyncRequired
	}
	if c.RI > 0 && !validGraphVisitKey(c.RP+":"+strconv.FormatInt(c.RI, 10)) {
		return ErrResyncRequired
	}
	if c.D > 0 && c.RI < 1 {
		return ErrResyncRequired
	}
	switch c.RK {
	case "", "merged_result", "branch", "merge_request_head", "unproven":
	default:
		return ErrResyncRequired
	}
	switch c.RS {
	case "", "equal", "different", "unknown":
	default:
		return ErrResyncRequired
	}
	return validateGraphReasons(c.Rsn)
}

func validGraphDigest(s string) bool {
	if s == "" {
		return true
	}
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// FormatGraphEvidence encodes the jobs, bridges, and pipeline-metadata digests
// of one completed node.
func FormatGraphEvidence(key, jobs, bridges, meta string) (string, error) {
	if !validGraphVisitKey(key) || !validGraphDigest(jobs) || !validGraphDigest(bridges) || !validGraphDigest(meta) {
		return "", ErrResyncRequired
	}
	return key + "=" + jobs + "=" + bridges + "=" + meta, nil
}

// FormatGraphEvidenceReplay records how many retained items certification must
// re-read for one completed node.
func FormatGraphEvidenceReplay(key string, items int) (string, error) {
	if !validGraphVisitKey(key) || items < 0 || items > MaxGraphEvidenceReplayItems {
		return "", ErrResyncRequired
	}
	return key + "=" + strconv.Itoa(items), nil
}

// ParseGraphEvidenceReplay decodes a FormatGraphEvidenceReplay item.
func ParseGraphEvidenceReplay(item string) (key string, items int, ok bool) {
	j := strings.LastIndexByte(item, '=')
	if j < 0 {
		return "", 0, false
	}
	key = item[:j]
	n, err := strconv.Atoi(item[j+1:])
	if err != nil || !validGraphVisitKey(key) || n < 0 || n > MaxGraphEvidenceReplayItems {
		return "", 0, false
	}
	return key, n, true
}

// ParseGraphEvidence decodes a FormatGraphEvidence item.
func ParseGraphEvidence(item string) (key, jobs, bridges, meta string, ok bool) {
	rest := item
	fields := [3]string{}
	for i := 2; i >= 0; i-- {
		j := strings.LastIndexByte(rest, '=')
		if j < 0 {
			return "", "", "", "", false
		}
		fields[i] = rest[j+1:]
		rest = rest[:j]
	}
	key = rest
	jobs, bridges, meta = fields[0], fields[1], fields[2]
	if !validGraphVisitKey(key) || !validGraphDigest(jobs) || !validGraphDigest(bridges) || !validGraphDigest(meta) {
		return "", "", "", "", false
	}
	return key, jobs, bridges, meta, true
}

// FormatGraphReachEdge encodes one bridge/shared edge for cycle detection on resume.
func FormatGraphReachEdge(from, to, kind string) (string, error) {
	if !validGraphVisitKey(from) || !validGraphVisitKey(to) || (kind != "bridge" && kind != "shared") {
		return "", ErrResyncRequired
	}
	return from + ">" + to + ">" + kind, nil
}

// ParseGraphReachEdge decodes a FormatGraphReachEdge item.
func ParseGraphReachEdge(item string) (from, to, kind string, ok bool) {
	parts := strings.Split(item, ">")
	if len(parts) != 3 {
		return "", "", "", false
	}
	from, to, kind = parts[0], parts[1], parts[2]
	if !validGraphVisitKey(from) || !validGraphVisitKey(to) || (kind != "bridge" && kind != "shared") {
		return "", "", "", false
	}
	return from, to, kind, true
}

func validateGraphReach(items []string) error {
	if len(items) > MaxGraphReachEdges {
		return ErrResyncRequired
	}
	seen := map[string]struct{}{}
	var prev string
	havePrev := false
	for _, item := range items {
		if _, _, _, ok := ParseGraphReachEdge(item); !ok {
			return ErrResyncRequired
		}
		if _, dup := seen[item]; dup {
			return ErrResyncRequired
		}
		seen[item] = struct{}{}
		if havePrev && item < prev {
			return ErrResyncRequired
		}
		prev, havePrev = item, true
	}
	return nil
}

func validateGraphEvidence(c *GraphCont) error {
	if !validGraphDigest(c.JD) || !validGraphDigest(c.BD) || len(c.Ev) > MaxGraphVisited {
		return ErrResyncRequired
	}
	seen := map[string]struct{}{}
	for _, item := range c.Ev {
		key, _, _, _, ok := ParseGraphEvidence(item)
		if !ok {
			return ErrResyncRequired
		}
		if _, dup := seen[key]; dup {
			return ErrResyncRequired
		}
		seen[key] = struct{}{}
	}
	if len(c.Ei) > len(c.Ev) {
		return ErrResyncRequired
	}
	replay := map[string]int{}
	for _, item := range c.Ei {
		key, n, ok := ParseGraphEvidenceReplay(item)
		if !ok {
			return ErrResyncRequired
		}
		if _, ok := seen[key]; !ok {
			return ErrResyncRequired
		}
		if _, dup := replay[key]; dup {
			return ErrResyncRequired
		}
		replay[key] = n
	}
	return nil
}

// validateGraphAncestry checks the active node's root-to-parent path.
// Order is meaningful, so unlike Vis it is not sorted.
func validateGraphAncestry(items []string, depth int) error {
	if len(items) > depth || len(items) > MaxGraphVisited {
		return ErrResyncRequired
	}
	seen := map[string]struct{}{}
	for _, item := range items {
		if item != strings.TrimSpace(item) || !validGraphVisitKey(item) {
			return ErrResyncRequired
		}
		if _, dup := seen[item]; dup {
			return ErrResyncRequired
		}
		seen[item] = struct{}{}
	}
	return nil
}

func validateGraphReasons(items []string) error {
	if len(items) > MaxGraphVisited {
		return ErrResyncRequired
	}
	seen := map[string]struct{}{}
	for _, item := range items {
		if item == "" || item != strings.TrimSpace(item) || len(item) > 64 {
			return ErrResyncRequired
		}
		for i := 0; i < len(item); i++ {
			c := item[i]
			if c >= 'a' && c <= 'z' {
				continue
			}
			if i > 0 && ((c >= '0' && c <= '9') || c == '_') {
				continue
			}
			return ErrResyncRequired
		}
		if _, dup := seen[item]; dup {
			return ErrResyncRequired
		}
		seen[item] = struct{}{}
	}
	return nil
}

func validateGraphKeyList(items []string, queued bool) error {
	if len(items) > MaxGraphVisited {
		return ErrResyncRequired
	}
	seen := map[string]struct{}{}
	var prev string
	havePrev := false
	for _, item := range items {
		if item == "" || item != strings.TrimSpace(item) {
			return ErrResyncRequired
		}
		if _, dup := seen[item]; dup {
			return ErrResyncRequired
		}
		if queued {
			if !validGraphQueueKey(item) {
				return ErrResyncRequired
			}
		} else {
			if !validGraphVisitKey(item) {
				return ErrResyncRequired
			}
			if havePrev && item <= prev {
				return ErrResyncRequired
			}
			prev = item
			havePrev = true
		}
		seen[item] = struct{}{}
	}
	return nil
}

func validGraphVisitKey(item string) bool {
	proj, id, ok := splitGraphKey(item)
	return ok && proj != "" && id > 0
}

func validGraphQueueKey(item string) bool {
	_, _, _, _, _, _, ok := ParseGraphQueueItem(item)
	return ok
}

// FormatGraphQueueItem encodes one signed walk-queue entry.
// Shape: project:pipeline:depth:sha:bridge:ancestor,ancestor
// sha is 40-hex or "-". bridge is a positive id or "-".
// The ancestor field is always present and may be empty.
func FormatGraphQueueItem(project string, pipeline int64, depth int, sha string, bridge int64, ancestors []string) (string, error) {
	if depth < 1 || pipeline < 1 || strings.TrimSpace(project) == "" || project != strings.TrimSpace(project) {
		return "", ErrResyncRequired
	}
	if !validGraphVisitKey(project + ":" + strconv.FormatInt(pipeline, 10)) {
		return "", ErrResyncRequired
	}
	if sha != "-" && !isGitSHA(sha) {
		return "", ErrResyncRequired
	}
	if bridge < 0 {
		return "", ErrResyncRequired
	}
	bridgeField := "-"
	if bridge > 0 {
		bridgeField = strconv.FormatInt(bridge, 10)
	}
	seen := map[string]struct{}{}
	for _, a := range ancestors {
		if !validGraphVisitKey(a) {
			return "", ErrResyncRequired
		}
		if _, dup := seen[a]; dup {
			return "", ErrResyncRequired
		}
		seen[a] = struct{}{}
	}
	return project + ":" + strconv.FormatInt(pipeline, 10) + ":" + strconv.Itoa(depth) + ":" + sha + ":" + bridgeField + ":" + strings.Join(ancestors, ","), nil
}

// ParseGraphQueueItem decodes a signed walk-queue entry.
func ParseGraphQueueItem(item string) (project string, pipeline int64, depth int, sha string, bridge int64, ancestors []string, ok bool) {
	if item == "" || item != strings.TrimSpace(item) {
		return "", 0, 0, "", 0, nil, false
	}
	parts := strings.SplitN(item, ":", 6)
	if len(parts) != 6 {
		return "", 0, 0, "", 0, nil, false
	}
	project = parts[0]
	pipeline, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || pipeline < 1 {
		return "", 0, 0, "", 0, nil, false
	}
	depth, err = strconv.Atoi(parts[2])
	if err != nil || depth < 1 {
		return "", 0, 0, "", 0, nil, false
	}
	sha = parts[3]
	if sha != "-" && !isGitSHA(sha) {
		return "", 0, 0, "", 0, nil, false
	}
	if parts[4] != "-" {
		bridge, err = strconv.ParseInt(parts[4], 10, 64)
		if err != nil || bridge < 1 {
			return "", 0, 0, "", 0, nil, false
		}
	}
	if !validGraphVisitKey(project + ":" + parts[1]) {
		return "", 0, 0, "", 0, nil, false
	}
	if parts[5] != "" {
		seen := map[string]struct{}{}
		for _, a := range strings.Split(parts[5], ",") {
			if !validGraphVisitKey(a) {
				return "", 0, 0, "", 0, nil, false
			}
			if _, dup := seen[a]; dup {
				return "", 0, 0, "", 0, nil, false
			}
			seen[a] = struct{}{}
			ancestors = append(ancestors, a)
		}
	}
	return project, pipeline, depth, sha, bridge, ancestors, true
}

func splitGraphKey(item string) (string, int64, bool) {
	i := strings.LastIndexByte(item, ':')
	if i <= 0 || i == len(item)-1 {
		return "", 0, false
	}
	id, err := strconv.ParseInt(item[i+1:], 10, 64)
	if err != nil || id < 1 {
		return "", 0, false
	}
	proj := item[:i]
	if proj == "" || proj != strings.TrimSpace(proj) {
		return "", 0, false
	}
	return proj, id, true
}

// ReviewLiveRefs is an independently observed provenance tuple.
// A zero value is not an observation and fails closed.
type ReviewLiveRefs struct {
	OwnerProjectID  int64
	SourceProjectID int64
	TargetProjectID int64
	SourceBranch    string
	TargetBranch    string
	SourceSHA       string
	TargetSHA       string
	VersionID       int64
	VersionHead     string
	VersionBase     string
	VersionStart    string
}

func (r ReviewLiveRefs) proved() bool {
	if r.OwnerProjectID < 1 || r.SourceProjectID < 1 || r.TargetProjectID < 1 || r.VersionID < 1 {
		return false
	}
	if r.SourceBranch == "" || r.SourceBranch != strings.TrimSpace(r.SourceBranch) {
		return false
	}
	if r.TargetBranch == "" || r.TargetBranch != strings.TrimSpace(r.TargetBranch) {
		return false
	}
	if !isGitSHA(r.SourceSHA) || !isGitSHA(r.TargetSHA) || !isGitSHA(r.VersionHead) || !isGitSHA(r.VersionBase) || !isGitSHA(r.VersionStart) {
		return false
	}
	return r.SourceSHA == r.VersionHead
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func isGitSHA(s string) bool {
	if len(s) != 40 {
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

// VerifyContextBinding checks a decoded review-context token against the live
// caller binding, an independently supplied provenance tuple, and the demanded
// complete sections. It performs no I/O. A missing observation fails closed.
// A metadata-only token fails a demand for approvals.
func VerifyContextBinding(p Payload, instance string, actorID int64, policyFP, tool, section string, scope Scope, filters Filters, upperBound string, live ReviewLiveRefs, demand []string) error {
	if err := MatchBinding(p, instance, actorID, policyFP, tool, section, scope, filters, nil, upperBound); err != nil {
		return err
	}
	if p.ContextRef == nil || !p.ContextRef.BracketConsistent || len(demand) == 0 || !live.proved() {
		return ErrResyncRequired
	}
	ref := p.ContextRef
	if ref.OwnerProjectID != live.OwnerProjectID || ref.SourceProjectID != live.SourceProjectID || ref.TargetProjectID != live.TargetProjectID ||
		ref.SourceBranch != live.SourceBranch || ref.TargetBranch != live.TargetBranch ||
		ref.SourceSHA != live.SourceSHA || ref.TargetSHA != live.TargetSHA ||
		ref.VersionID != live.VersionID || ref.VersionHead != live.VersionHead || ref.VersionBase != live.VersionBase || ref.VersionStart != live.VersionStart {
		return ErrResyncRequired
	}
	for _, name := range demand {
		if !containsString(ref.Complete, name) {
			return ErrResyncRequired
		}
	}
	return nil
}

// ContextWriteFresh reports whether now is still inside the token's absolute write window.
func ContextWriteFresh(p Payload, now time.Time) bool {
	if p.ContextRef == nil {
		return false
	}
	fresh, err := time.Parse(time.RFC3339, p.ContextRef.WriteFreshUntil)
	if err != nil {
		return false
	}
	return now.UTC().Before(fresh.UTC())
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
