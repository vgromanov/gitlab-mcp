package intentstore

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

const (
	// ApplicationID is the SQLite application_id for this store ("GMIS").
	ApplicationID uint32 = 0x474D4953
	// SchemaName is stored in meta and must match on every open.
	SchemaName = "gitlab-mcp-intent"
	// SchemaVersion is the current user_version.
	SchemaVersion = 1
	// DefaultMaxRows is the row cap when Config.MaxRows is unset.
	DefaultMaxRows = 10000
	// DefaultMaxBytes is the db+wal+shm cap when Config.MaxBytes is unset.
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

const schemaSQL = `
CREATE TABLE meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE intents (
  operation_id TEXT PRIMARY KEY,
  instance_id TEXT NOT NULL,
  actor TEXT NOT NULL,
  project TEXT NOT NULL,
  mr TEXT NOT NULL,
  operation_kind TEXT NOT NULL,
  caller_key TEXT NOT NULL,
  payload_hash TEXT NOT NULL,
  state TEXT NOT NULL,
  expected_head TEXT,
  observed_head TEXT,
  upstream_ids TEXT,
  verification_state TEXT,
  created_unix_nano INTEGER NOT NULL,
  updated_unix_nano INTEGER NOT NULL,
  sending_unix_nano INTEGER,
  finalized_unix_nano INTEGER,
  expires_unix_nano INTEGER,
  compacted INTEGER NOT NULL DEFAULT 0,
  row_epoch TEXT NOT NULL,
  UNIQUE (instance_id, actor, project, mr, operation_kind, caller_key)
);
`
