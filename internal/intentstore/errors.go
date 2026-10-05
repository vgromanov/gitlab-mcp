package intentstore

import "errors"

var (
	// ErrUnrelatedDatabase means the file is SQLite but not this store.
	ErrUnrelatedDatabase = errors.New("intent store: unrelated database")
	// ErrUnsafePermissions means a directory or database file mode is not private.
	ErrUnsafePermissions = errors.New("intent store: unsafe permissions")
	// ErrSymlink means the database path, its parent, or a WAL/SHM sidecar is a symlink.
	ErrSymlink = errors.New("intent store: symlink refused")
	// ErrReadOnly means the database cannot accept a durable write.
	ErrReadOnly = errors.New("intent store: database is read-only")
	// ErrCorrupt means the file is not a readable SQLite database.
	ErrCorrupt = errors.New("intent store: corrupt database")
	// ErrPayloadConflict means the idempotency key already exists with another payload hash.
	ErrPayloadConflict = errors.New("intent store: payload conflict")
	// ErrFull means a row or byte cap would be exceeded.
	ErrFull = errors.New("intent store: store is full")
	// ErrMigration means a schema migration did not commit.
	ErrMigration = errors.New("intent store: migration failed")
	// ErrWritesDisabled means dispatch writes were turned off.
	ErrWritesDisabled = errors.New("intent store: dispatch writes are disabled")
	// ErrNotFound means no intent exists for the id or key.
	ErrNotFound = errors.New("intent store: not found")
	// ErrExpired means a nonterminal intent can no longer start a dispatch.
	ErrExpired = errors.New("intent store: nonterminal intent cannot be reused")
	// ErrAlreadySending means a dispatch marker is already committed.
	ErrAlreadySending = errors.New("intent store: dispatch already started")
	// ErrTerminal means the intent can no longer change or dispatch.
	ErrTerminal = errors.New("intent store: intent is terminal")
	// ErrStaleEpoch means the row belongs to a previous database epoch.
	ErrStaleEpoch = errors.New("intent store: stale database epoch")
	// ErrConfirmation means epoch reset was requested without the exact confirmation.
	ErrConfirmation = errors.New("intent store: epoch reset confirmation rejected")
	// ErrDispatchEnabled means epoch reset was requested while dispatch writes are still on.
	ErrDispatchEnabled = errors.New("intent store: disable dispatch before epoch reset")
	// ErrInvalidHash means the payload hash is not 64 lowercase hex characters.
	ErrInvalidHash = errors.New("intent store: payload hash must be 64 lowercase hex characters")
	// ErrInvalidIdentity means a canonical identity field is missing or too large.
	ErrInvalidIdentity = errors.New("intent store: invalid identity")
	// ErrInvalidState means the requested lifecycle transition is not allowed.
	ErrInvalidState = errors.New("intent store: invalid state transition")
	// ErrNotReady means the store is closed or failed initialization.
	ErrNotReady = errors.New("intent store: database is not ready")
	// ErrInvalidOutcome means a receipt field is too large or not allowed.
	ErrInvalidOutcome = errors.New("intent store: invalid receipt")
)
