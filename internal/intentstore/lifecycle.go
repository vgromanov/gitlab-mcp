package intentstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const receiptCols = `operation_id, instance_id, actor, project, mr, operation_kind, caller_key,
payload_hash, state, expected_head, observed_head, upstream_ids, verification_state,
created_unix_nano, updated_unix_nano, sending_unix_nano, finalized_unix_nano, expires_unix_nano,
compacted, row_epoch`

// Begin persists a prepared intent or returns the existing row for the same key.
func (s *Store) Begin(ctx context.Context, id Identity, payloadHash string, opts BeginOptions) (Receipt, error) {
	if err := id.validate(); err != nil {
		return Receipt{}, err
	}
	if !validHash(payloadHash) {
		return Receipt{}, ErrInvalidHash
	}
	if !validHead(opts.ExpectedHead) {
		return Receipt{}, ErrInvalidOutcome
	}
	var out Receipt
	err := s.writeTx(ctx, true, func(tx *sql.Tx) error {
		existing, err := getByIdentityTx(ctx, tx, id, s.epoch)
		if err == nil {
			if existing.PayloadHash != payloadHash {
				return ErrPayloadConflict
			}
			out = existing
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM intents`).Scan(&n); err != nil {
			return mapDriver(err)
		}
		if n >= s.maxRows {
			return ErrFull
		}
		op, err := newID()
		if err != nil {
			return err
		}
		now := s.now()
		var expires any
		if !opts.ExpiresAt.IsZero() {
			expires = opts.ExpiresAt.UTC().UnixNano()
		}
		var head any
		if opts.ExpectedHead != "" {
			head = opts.ExpectedHead
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO intents (
			operation_id, instance_id, actor, project, mr, operation_kind, caller_key,
			payload_hash, state, expected_head, created_unix_nano, updated_unix_nano, expires_unix_nano,
			compacted, row_epoch
		) SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, value
			FROM meta WHERE key='epoch' AND value=?`,
			op, id.Instance, id.Actor, id.Project, id.MR, id.Kind, id.CallerKey,
			payloadHash, string(StatePrepared), head, now.UnixNano(), now.UnixNano(), expires, s.epoch,
		)
		if err != nil {
			return mapDriver(err)
		}
		inserted, err := res.RowsAffected()
		if err != nil {
			return mapDriver(err)
		}
		if inserted != 1 {
			return ErrStaleEpoch
		}
		out, err = getByIDTx(ctx, tx, op, s.epoch)
		return err
	})
	return out, err
}

// ClaimSending atomically commits the sending marker for a prepared intent.
func (s *Store) ClaimSending(ctx context.Context, operationID string) (Receipt, error) {
	if operationID == "" {
		return Receipt{}, ErrNotFound
	}
	var out Receipt
	err := s.writeTx(ctx, true, func(tx *sql.Tx) error {
		rec, err := getByIDTx(ctx, tx, operationID, s.epoch)
		if err != nil {
			return err
		}
		if !rec.EpochCurrent {
			return ErrStaleEpoch
		}
		switch rec.State {
		case StateSending:
			return ErrAlreadySending
		case StatePrepared:
		default:
			return ErrTerminal
		}
		if !rec.ExpiresAt.IsZero() && !s.now().Before(rec.ExpiresAt) {
			return ErrExpired
		}
		now := s.now()
		res, err := tx.ExecContext(ctx, `UPDATE intents SET state=?, sending_unix_nano=?, updated_unix_nano=?
			WHERE operation_id=? AND state=?
			  AND row_epoch=(SELECT value FROM meta WHERE key='epoch')
			  AND row_epoch=?`,
			string(StateSending), now.UnixNano(), now.UnixNano(), operationID, string(StatePrepared), s.epoch)
		if err != nil {
			return mapDriver(err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return mapDriver(err)
		}
		if n != 1 {
			current, err := getByIDTx(ctx, tx, operationID, s.epoch)
			if err != nil {
				return err
			}
			if !current.EpochCurrent {
				return ErrStaleEpoch
			}
			return ErrAlreadySending
		}
		out, err = getByIDTx(ctx, tx, operationID, s.epoch)
		return err
	})
	return out, err
}

// RecordOutcome stores a reconciliation result without starting a dispatch.
func (s *Store) RecordOutcome(ctx context.Context, operationID string, outcome Outcome) (Receipt, error) {
	if !validHead(outcome.ObservedHead) || !validIDs(outcome.UpstreamIDs) || !validVerification(outcome.VerificationState) {
		return Receipt{}, ErrInvalidOutcome
	}
	var out Receipt
	err := s.writeTx(ctx, false, func(tx *sql.Tx) error {
		rec, err := getByIDTx(ctx, tx, operationID, s.epoch)
		if err != nil {
			return err
		}
		if rec.Compacted || isFinalized(rec.State) {
			return ErrTerminal
		}
		if !allowedOutcome(rec.State, outcome.State) {
			return ErrInvalidState
		}
		now := s.now()
		finalized := rec.FinalizedAt
		if isFinalized(outcome.State) && finalized.IsZero() {
			finalized = now
		}
		ids := rec.UpstreamIDs
		if outcome.UpstreamIDs != nil {
			ids = outcome.UpstreamIDs
		}
		raw, err := json.Marshal(ids)
		if err != nil {
			return ErrInvalidOutcome
		}
		observed := rec.ObservedHead
		if outcome.ObservedHead != "" {
			observed = outcome.ObservedHead
		}
		verify := rec.VerificationState
		if outcome.VerificationState != "" {
			verify = outcome.VerificationState
		}
		var observedArg, verifyArg, finalizedArg, upstreamArg any
		if observed != "" {
			observedArg = observed
		}
		if verify != "" {
			verifyArg = verify
		}
		if ids != nil {
			upstreamArg = string(raw)
		}
		if !finalized.IsZero() {
			finalizedArg = finalized.UTC().UnixNano()
		}
		_, err = tx.ExecContext(ctx, `UPDATE intents SET state=?, observed_head=?, upstream_ids=?, verification_state=?,
			updated_unix_nano=?, finalized_unix_nano=? WHERE operation_id=?`,
			string(outcome.State), observedArg, upstreamArg, verifyArg, now.UnixNano(), finalizedArg, operationID)
		if err != nil {
			return mapDriver(err)
		}
		out, err = getByIDTx(ctx, tx, operationID, s.epoch)
		return err
	})
	return out, err
}

// Get loads one receipt by operation id.
func (s *Store) Get(ctx context.Context, operationID string) (Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readyLocked(); err != nil {
		return Receipt{}, err
	}
	epoch, err := readEpoch(ctx, s.db)
	if err != nil {
		return Receipt{}, err
	}
	return getByIDTx(ctx, s.db, operationID, epoch)
}

// GetByIdentity loads the receipt for an idempotency key.
func (s *Store) GetByIdentity(ctx context.Context, id Identity) (Receipt, error) {
	if err := id.validate(); err != nil {
		return Receipt{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readyLocked(); err != nil {
		return Receipt{}, err
	}
	epoch, err := readEpoch(ctx, s.db)
	if err != nil {
		return Receipt{}, err
	}
	return getByIdentityTx(ctx, s.db, id, epoch)
}

// Compact drops finalized receipt details older than the retention window.
// Tombstones keep the key, payload hash, and outcome. In-flight and uncertain
// rows are left intact.
func (s *Store) Compact(ctx context.Context) (int, error) {
	var n int64
	err := s.writeTx(ctx, false, func(tx *sql.Tx) error {
		now := s.now()
		cutoff := now.Add(-s.retention).UnixNano()
		res, err := tx.ExecContext(ctx, `UPDATE intents SET
			expected_head=NULL, observed_head=NULL, upstream_ids=NULL, verification_state=NULL,
			compacted=1, updated_unix_nano=?
			WHERE compacted=0
			  AND state IN (?, ?, ?, ?)
			  AND finalized_unix_nano IS NOT NULL
			  AND finalized_unix_nano <= ?`,
			now.UnixNano(),
			string(StatePublished), string(StatePublishedOnChangedHead), string(StateStale), string(StateRejected),
			cutoff)
		if err != nil {
			return mapDriver(err)
		}
		n, err = res.RowsAffected()
		return err
	})
	return int(n), err
}

// ResetEpoch records a new epoch. Dispatch must already be disabled.
// Existing rows, including tombstones, are not deleted. The cached epoch
// changes only after the meta update commits. Begin and ClaimSending reload
// meta.epoch inside the same immediate transaction and stamp or claim a row
// only when that value is still this store's epoch. The schema triggers
// abort a commit that would tag a prepared or sending row with any other epoch.
func (s *Store) ResetEpoch(ctx context.Context, confirmation string) error {
	if confirmation != EpochResetConfirmation {
		return ErrConfirmation
	}
	var next string
	return s.commitWrite(ctx, false, func(tx *sql.Tx) error {
		if s.dispatch {
			return ErrDispatchEnabled
		}
		epoch, err := newID()
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE meta SET value=? WHERE key='epoch'`, epoch)
		if err != nil {
			return mapDriver(err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return mapDriver(err)
		}
		if affected != 1 {
			return ErrUnrelatedDatabase
		}
		next = epoch
		return nil
	}, func() {
		s.epoch = next
	})
}

type rowQuery interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getByIDTx(ctx context.Context, q rowQuery, operationID, epoch string) (Receipt, error) {
	row := q.QueryRowContext(ctx, `SELECT `+receiptCols+` FROM intents WHERE operation_id=?`, operationID)
	rec, err := scanReceipt(row, epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, ErrNotFound
	}
	return rec, err
}

func getByIdentityTx(ctx context.Context, q rowQuery, id Identity, epoch string) (Receipt, error) {
	row := q.QueryRowContext(ctx, `SELECT `+receiptCols+` FROM intents WHERE
		instance_id=? AND actor=? AND project=? AND mr=? AND operation_kind=? AND caller_key=?`,
		id.Instance, id.Actor, id.Project, id.MR, id.Kind, id.CallerKey)
	rec, err := scanReceipt(row, epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, ErrNotFound
	}
	return rec, err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanReceipt(row scanner, epoch string) (Receipt, error) {
	var rec Receipt
	var state, upstream sql.NullString
	var expected, observed, verify sql.NullString
	var created, updated int64
	var sending, finalized, expires sql.NullInt64
	var compacted int
	var rowEpoch string
	err := row.Scan(
		&rec.OperationID, &rec.Identity.Instance, &rec.Identity.Actor, &rec.Identity.Project, &rec.Identity.MR,
		&rec.Identity.Kind, &rec.Identity.CallerKey, &rec.PayloadHash, &state, &expected, &observed, &upstream, &verify,
		&created, &updated, &sending, &finalized, &expires, &compacted, &rowEpoch,
	)
	if err != nil {
		return Receipt{}, mapDriver(err)
	}
	rec.State = State(state.String)
	rec.ExpectedHead = expected.String
	rec.ObservedHead = observed.String
	rec.VerificationState = verify.String
	rec.CreatedAt = nano(created)
	rec.UpdatedAt = nano(updated)
	rec.SendingAt = nullNano(sending)
	rec.FinalizedAt = nullNano(finalized)
	rec.ExpiresAt = nullNano(expires)
	rec.Compacted = compacted != 0
	rec.Epoch = rowEpoch
	rec.EpochCurrent = rowEpoch == epoch
	if upstream.Valid && upstream.String != "" && upstream.String != "null" {
		if err := json.Unmarshal([]byte(upstream.String), &rec.UpstreamIDs); err != nil {
			return Receipt{}, ErrCorrupt
		}
	}
	return rec, nil
}

func nano(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

func nullNano(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return nano(n.Int64)
}
