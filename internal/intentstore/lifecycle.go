package intentstore

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	bolt "go.etcd.io/bbolt"
)

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
	err := s.writeTx(ctx, true, func(tx *bolt.Tx) error {
		existing, err := rowByIdentity(tx, id)
		if err == nil {
			if existing.PayloadHash != payloadHash {
				return ErrPayloadConflict
			}
			out = existing.receipt(s.epoch)
			return errNoCommit
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if rowCount(tx) >= s.maxRows {
			return ErrFull
		}
		if err := s.overByteCap(); err != nil {
			return err
		}
		op, err := newID()
		if err != nil {
			return err
		}
		now := s.now().UnixNano()
		r := row{
			OperationID:     op,
			Instance:        id.Instance,
			Actor:           id.Actor,
			Project:         id.Project,
			MR:              id.MR,
			Kind:            id.Kind,
			CallerKey:       id.CallerKey,
			PayloadHash:     payloadHash,
			State:           StatePrepared,
			ExpectedHead:    opts.ExpectedHead,
			CreatedUnixNano: now,
			UpdatedUnixNano: now,
			RowEpoch:        s.epoch,
		}
		if !opts.ExpiresAt.IsZero() {
			r.ExpiresUnixNano = opts.ExpiresAt.UTC().UnixNano()
		}
		if err := putRow(tx, r); err != nil {
			return err
		}
		if err := tx.Bucket(bucketIdentity).Put(identityKey(id), []byte(op)); err != nil {
			return err
		}
		out = r.receipt(s.epoch)
		return nil
	})
	return out, err
}

// ClaimSending atomically commits the sending marker for a prepared intent.
func (s *Store) ClaimSending(ctx context.Context, operationID string) (Receipt, error) {
	if operationID == "" {
		return Receipt{}, ErrNotFound
	}
	var out Receipt
	err := s.writeTx(ctx, true, func(tx *bolt.Tx) error {
		r, err := rowByID(tx, operationID)
		if err != nil {
			return err
		}
		if r.RowEpoch != s.epoch {
			return ErrStaleEpoch
		}
		switch r.State {
		case StateSending:
			return ErrAlreadySending
		case StatePrepared:
		default:
			return ErrTerminal
		}
		now := s.now()
		if r.ExpiresUnixNano != 0 && !now.Before(nano(r.ExpiresUnixNano)) {
			return ErrExpired
		}
		r.State = StateSending
		r.SendingUnixNano = now.UnixNano()
		r.UpdatedUnixNano = now.UnixNano()
		if err := putRow(tx, r); err != nil {
			return err
		}
		out = r.receipt(s.epoch)
		return nil
	})
	return out, err
}

// RecordOutcome stores a reconciliation result without starting a dispatch.
// The write is limited to a row stamped with this handle's epoch, so a store
// that still has the previous epoch cannot finalize a receipt created after reset.
func (s *Store) RecordOutcome(ctx context.Context, operationID string, outcome Outcome) (Receipt, error) {
	if !validHead(outcome.ObservedHead) || !validIDs(outcome.UpstreamIDs) || !validVerification(outcome.VerificationState) {
		return Receipt{}, ErrInvalidOutcome
	}
	var out Receipt
	err := s.writeTx(ctx, false, func(tx *bolt.Tx) error {
		r, err := rowByID(tx, operationID)
		if err != nil {
			return err
		}
		if r.Compacted || isFinalized(r.State) {
			return ErrTerminal
		}
		if !allowedOutcome(r.State, outcome.State) {
			return ErrInvalidState
		}
		if r.RowEpoch != s.epoch {
			return ErrStaleEpoch
		}
		now := s.now().UnixNano()
		if isFinalized(outcome.State) && r.FinalizedUnixNano == 0 {
			r.FinalizedUnixNano = now
		}
		if outcome.UpstreamIDs != nil {
			r.UpstreamIDs = outcome.UpstreamIDs
		}
		if outcome.ObservedHead != "" {
			r.ObservedHead = outcome.ObservedHead
		}
		if outcome.VerificationState != "" {
			r.VerificationState = outcome.VerificationState
		}
		r.State = outcome.State
		r.UpdatedUnixNano = now
		if err := putRow(tx, r); err != nil {
			return err
		}
		out = r.receipt(s.epoch)
		return nil
	})
	return out, err
}

// Get loads one receipt by operation id.
func (s *Store) Get(ctx context.Context, operationID string) (Receipt, error) {
	if err := s.muLock(ctx); err != nil {
		return Receipt{}, err
	}
	defer s.muUnlock()
	return s.receiptSnapshot(ctx, func(tx *bolt.Tx) (row, error) {
		return rowByID(tx, operationID)
	})
}

// GetByIdentity loads the receipt for an idempotency key.
func (s *Store) GetByIdentity(ctx context.Context, id Identity) (Receipt, error) {
	if err := id.validate(); err != nil {
		return Receipt{}, err
	}
	if err := s.muLock(ctx); err != nil {
		return Receipt{}, err
	}
	defer s.muUnlock()
	return s.receiptSnapshot(ctx, func(tx *bolt.Tx) (row, error) {
		return rowByIdentity(tx, id)
	})
}

// receiptSnapshot loads meta.epoch and the receipt from one read
// transaction. The read holds the file lock, so a reset cannot commit
// between the two lookups and mark an old row current or a new row stale.
func (s *Store) receiptSnapshot(ctx context.Context, load func(*bolt.Tx) (row, error)) (Receipt, error) {
	var out Receipt
	err := s.view(ctx, func(tx *bolt.Tx) error {
		epoch, err := readEpoch(tx)
		if err != nil {
			return err
		}
		if afterReadEpoch != nil {
			afterReadEpoch()
		}
		r, err := load(tx)
		if err != nil {
			return err
		}
		out = r.receipt(epoch)
		return nil
	})
	return out, err
}

// Compact drops finalized receipt details older than the retention window.
// Tombstones keep the key, payload hash, and outcome. In-flight and uncertain
// rows are left intact.
func (s *Store) Compact(ctx context.Context) (int, error) {
	n := 0
	err := s.writeTx(ctx, false, func(tx *bolt.Tx) error {
		now := s.now()
		cutoff := now.Add(-s.retention).UnixNano()
		var due []row
		err := tx.Bucket(bucketIntents).ForEach(func(_, v []byte) error {
			var r row
			if err := json.Unmarshal(v, &r); err != nil {
				return ErrCorrupt
			}
			if !r.Compacted && isFinalized(r.State) && r.FinalizedUnixNano != 0 && r.FinalizedUnixNano <= cutoff {
				due = append(due, r)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if len(due) == 0 {
			return errNoCommit
		}
		for _, r := range due {
			r.ExpectedHead = ""
			r.ObservedHead = ""
			r.UpstreamIDs = nil
			r.VerificationState = ""
			r.Compacted = true
			r.UpdatedUnixNano = now.UnixNano()
			if err := putRow(tx, r); err != nil {
				return err
			}
		}
		n = len(due)
		return nil
	})
	return n, err
}

// ResetEpoch records a new epoch. Dispatch must already be disabled.
// Existing rows, including tombstones, are not deleted. The cached epoch
// changes only after the meta update commits. The update requires
// meta.epoch to still equal this handle's epoch, so a disabled store
// cannot reset a newer epoch written by another process. Begin and
// ClaimSending read meta.epoch inside the same exclusive transaction and
// stamp or claim a row only when that value is still this store's epoch.
func (s *Store) ResetEpoch(ctx context.Context, confirmation string) error {
	if confirmation != EpochResetConfirmation {
		return ErrConfirmation
	}
	var next string
	return s.commitWrite(ctx, false, func(tx *bolt.Tx) error {
		if s.dispatch {
			return ErrDispatchEnabled
		}
		if err := s.requireCurrentEpoch(tx); err != nil {
			return err
		}
		epoch, err := newID()
		if err != nil {
			return err
		}
		if err := tx.Bucket(bucketMeta).Put(keyEpoch, []byte(epoch)); err != nil {
			return err
		}
		next = epoch
		return nil
	}, func() {
		s.epoch = next
	})
}

func putRow(tx *bolt.Tx, r row) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return ErrInvalidOutcome
	}
	return tx.Bucket(bucketIntents).Put([]byte(r.OperationID), raw)
}

func decodeRow(raw []byte) (row, error) {
	var r row
	if err := json.Unmarshal(raw, &r); err != nil {
		return row{}, ErrCorrupt
	}
	return r, nil
}

func rowByID(tx *bolt.Tx, operationID string) (row, error) {
	if operationID == "" {
		return row{}, ErrNotFound
	}
	raw := tx.Bucket(bucketIntents).Get([]byte(operationID))
	if raw == nil {
		return row{}, ErrNotFound
	}
	return decodeRow(raw)
}

func rowByIdentity(tx *bolt.Tx, id Identity) (row, error) {
	op := tx.Bucket(bucketIdentity).Get(identityKey(id))
	if op == nil {
		return row{}, ErrNotFound
	}
	return rowByID(tx, string(op))
}

func (r row) receipt(epoch string) Receipt {
	rec := Receipt{
		OperationID: r.OperationID,
		PayloadHash: r.PayloadHash,
		Identity: Identity{
			Instance:  r.Instance,
			Actor:     r.Actor,
			Project:   r.Project,
			MR:        r.MR,
			Kind:      r.Kind,
			CallerKey: r.CallerKey,
		},
		State:             r.State,
		ExpectedHead:      r.ExpectedHead,
		ObservedHead:      r.ObservedHead,
		VerificationState: r.VerificationState,
		CreatedAt:         nano(r.CreatedUnixNano),
		UpdatedAt:         nano(r.UpdatedUnixNano),
		SendingAt:         nano(r.SendingUnixNano),
		FinalizedAt:       nano(r.FinalizedUnixNano),
		ExpiresAt:         nano(r.ExpiresUnixNano),
		Compacted:         r.Compacted,
		Epoch:             r.RowEpoch,
		EpochCurrent:      r.RowEpoch == epoch,
	}
	if len(r.UpstreamIDs) > 0 {
		rec.UpstreamIDs = append([]string(nil), r.UpstreamIDs...)
	}
	return rec
}

func nano(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}
