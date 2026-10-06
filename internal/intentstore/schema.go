package intentstore

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

const (
	// SchemaName is stored in meta and must match on every open.
	SchemaName = "gitlab-mcp-intent"
	// SchemaVersion is the current on-disk schema version.
	SchemaVersion = 1
	// DefaultMaxRows is the row cap when Config.MaxRows is unset.
	DefaultMaxRows = 10000
	// DefaultMaxBytes is the data-file size cap when Config.MaxBytes is unset.
	DefaultMaxBytes int64 = 32 << 20
	// DefaultRetention is how long finalized receipt details are kept.
	DefaultRetention = 30 * 24 * time.Hour
	// EpochResetConfirmation is the required maintenance phrase.
	EpochResetConfirmation = "reset-intent-epoch"

	maxIdentLen = 512
	maxHeadLen  = 128
	maxIDs      = 32
	maxIDLen    = 256
	maxVerify   = 64
)

// State is the durable lifecycle of one publication intent.
type State string

const (
	StatePrepared               State = "prepared"
	StateSending                State = "sending"
	StatePublished              State = "published"
	StatePublishedOnChangedHead State = "published_on_changed_head"
	StateAcceptedPending        State = "accepted_pending"
	StateStale                  State = "stale"
	StateRejected               State = "rejected"
	StateUncertain              State = "uncertain"
)

// Identity is the idempotency scope. It never includes a note body or token.
type Identity struct {
	Instance  string
	Actor     string
	Project   string
	MR        string
	Kind      string
	CallerKey string
}

// BeginOptions carries receipt fields known before dispatch.
type BeginOptions struct {
	ExpectedHead string
	ExpiresAt    time.Time
}

// Outcome is a reconciliation update. It cannot authorize another dispatch.
type Outcome struct {
	State             State
	ObservedHead      string
	UpstreamIDs       []string
	VerificationState string
}

// Receipt is the durable record returned to callers.
type Receipt struct {
	OperationID       string
	PayloadHash       string
	Identity          Identity
	State             State
	ExpectedHead      string
	ObservedHead      string
	UpstreamIDs       []string
	VerificationState string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	SendingAt         time.Time
	FinalizedAt       time.Time
	ExpiresAt         time.Time
	Compacted         bool
	Epoch             string
	EpochCurrent      bool
}

// PayloadHash is the SHA-256 hex digest of canonical payload bytes.
// Callers hash the body elsewhere; the store persists only this digest.
func PayloadHash(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func validHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for _, c := range h {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

func validIdentField(s string, required bool) bool {
	if s == "" {
		return !required
	}
	if len(s) > maxIdentLen {
		return false
	}
	for _, r := range s {
		if r == 0 {
			return false
		}
	}
	return true
}

func (id Identity) validate() error {
	if !validIdentField(id.Instance, true) ||
		!validIdentField(id.Actor, true) ||
		!validIdentField(id.Project, true) ||
		!validIdentField(id.MR, true) ||
		!validIdentField(id.Kind, true) ||
		!validIdentField(id.CallerKey, true) {
		return ErrInvalidIdentity
	}
	return nil
}

func validHead(s string) bool {
	if len(s) > maxHeadLen {
		return false
	}
	for _, r := range s {
		if r == 0 || r == '\n' || r == '\r' {
			return false
		}
	}
	return true
}

func validIDs(ids []string) bool {
	if len(ids) > maxIDs {
		return false
	}
	for _, id := range ids {
		if id == "" || len(id) > maxIDLen {
			return false
		}
		for _, r := range id {
			if r == 0 || r == '\n' || r == '\r' {
				return false
			}
		}
	}
	return true
}

func validVerification(s string) bool {
	if len(s) > maxVerify {
		return false
	}
	for _, r := range s {
		if r == 0 || r == ' ' || r == '\n' || r == '\r' {
			return false
		}
	}
	return true
}

func isFinalized(s State) bool {
	switch s {
	case StatePublished, StatePublishedOnChangedHead, StateStale, StateRejected:
		return true
	default:
		return false
	}
}

func knownState(s State) bool {
	switch s {
	case StatePrepared, StateSending, StatePublished, StatePublishedOnChangedHead,
		StateAcceptedPending, StateStale, StateRejected, StateUncertain:
		return true
	default:
		return false
	}
}

func allowedOutcome(from, to State) bool {
	if !knownState(to) || to == StatePrepared || to == StateSending {
		return false
	}
	switch from {
	case StatePrepared:
		return to == StateRejected || to == StateUncertain
	case StateSending:
		return to == StatePublished || to == StatePublishedOnChangedHead ||
			to == StateAcceptedPending || to == StateStale ||
			to == StateRejected || to == StateUncertain
	case StateAcceptedPending, StateUncertain:
		return to == StatePublished || to == StatePublishedOnChangedHead ||
			to == StateAcceptedPending || to == StateStale ||
			to == StateRejected || to == StateUncertain
	default:
		return false
	}
}

// The store is a single bbolt file. bbolt is pure Go, takes an advisory
// file lock per open handle, and keeps everything in that one file, so
// there are no sidecar files to permission-check or count against the cap.
var (
	bucketMeta     = []byte("meta")
	bucketIntents  = []byte("intents")
	bucketIdentity = []byte("identity")

	keySchemaName    = []byte("schema_name")
	keySchemaVersion = []byte("schema_version")
	keyEpoch         = []byte("epoch")

	// writableProbeCaller prefixes transient probe identities rolled back by Writable.
	writableProbeCaller = "\x00intentstore-writable-probe"
)

const (
	// containerMagic is the bbolt file magic. It sits at headerMagicOffset in
	// the first page header and lets a non-database file be refused before
	// anything opens it for writing.
	containerMagic    uint32 = 0xED0CDAED
	headerMagicOffset        = 16
	headerLen                = headerMagicOffset + 4

	// pageSize is fixed so the file format does not depend on the host page size.
	pageSize = 4096
	// minContainerBytes is the smallest on-disk size bbolt needs to initialize
	// an empty database. Limits below this are rejected before opening.
	minContainerBytes int64 = 4 * pageSize
	// allocSize bounds how far the data file grows past the pages in use.
	allocSize = 256 << 10
)

// row is the persisted form of one intent. Zero timestamps mean "unset".
type row struct {
	OperationID       string   `json:"operation_id"`
	Instance          string   `json:"instance"`
	Actor             string   `json:"actor"`
	Project           string   `json:"project"`
	MR                string   `json:"mr"`
	Kind              string   `json:"kind"`
	CallerKey         string   `json:"caller_key"`
	PayloadHash       string   `json:"payload_hash"`
	State             State    `json:"state"`
	ExpectedHead      string   `json:"expected_head,omitempty"`
	ObservedHead      string   `json:"observed_head,omitempty"`
	UpstreamIDs       []string `json:"upstream_ids,omitempty"`
	VerificationState string   `json:"verification_state,omitempty"`
	CreatedUnixNano   int64    `json:"created"`
	UpdatedUnixNano   int64    `json:"updated"`
	SendingUnixNano   int64    `json:"sending,omitempty"`
	FinalizedUnixNano int64    `json:"finalized,omitempty"`
	ExpiresUnixNano   int64    `json:"expires,omitempty"`
	Compacted         bool     `json:"compacted,omitempty"`
	RowEpoch          string   `json:"row_epoch"`
}

// identityKey joins the identity fields with NUL. validIdentField rejects
// NUL, so distinct identities cannot collide.
func identityKey(id Identity) []byte {
	return []byte(id.Instance + "\x00" + id.Actor + "\x00" + id.Project + "\x00" +
		id.MR + "\x00" + id.Kind + "\x00" + id.CallerKey)
}
