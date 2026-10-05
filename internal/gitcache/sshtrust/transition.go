//go:build linux || darwin

package sshtrust

import (
	"bytes"
	"context"
	"reflect"
)

// Transition is an immutable process-local receipt. Its states are private and
// originate only in an authenticated writer. Diagnostics are not ownership proof.
type Transition struct {
	before, after snapshot
	owner         string
	finalized     bool
}

// Finalize makes successful writer receipts available only after session Sync,
// close and update-worker joining have all completed without an error.
func (m *Manager) Finalize(err error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.finished {
		return m.finalErr
	}
	m.finished = true
	if err == nil && m.ctx.Err() != nil {
		err = m.ctx.Err()
	}
	m.finalized = err == nil
	m.finalErr = err
	if m.finalized {
		m.committed = append([]Transition(nil), m.pending...)
	}
	return err
}

func (m *Manager) Transitions() []Transition {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.finalized {
		return nil
	}
	out := append([]Transition(nil), m.committed...)
	for i := range out {
		if out[i].owner != m.Provenance() || len(m.files) == 0 || !sameSnapshot(out[i].before, m.files[0]) {
			return nil
		}
		out[i].finalized = true
	}
	return out
}

func sameSnapshot(a, b snapshot) bool {
	return a.path == b.path && a.exists == b.exists && bytes.Equal(a.data, b.data) &&
		(!a.exists || sameMetadata(a.info, b.info) && reflect.DeepEqual(a.attrs, b.attrs))
}

// ValidateRefresh accepts only the exact sequential result of finalized owned
// writes. Unrelated files, metadata, policy and endpoint remain bound.
func (m *Manager) ValidateRefresh(ctx context.Context, fresh *Manager, receipts []Transition) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || fresh == nil || m.addr != fresh.addr ||
		m.cfg.User != fresh.cfg.User || m.cfg.StrictHostKeyChecking != fresh.cfg.StrictHostKeyChecking ||
		m.cfg.UpdateHostKeys != fresh.cfg.UpdateHostKeys || m.cfg.HashKnownHosts != fresh.cfg.HashKnownHosts ||
		!reflect.DeepEqual(m.cfg.UserKnownHostsFiles, fresh.cfg.UserKnownHostsFiles) ||
		!reflect.DeepEqual(m.cfg.GlobalKnownHostsFiles, fresh.cfg.GlobalKnownHostsFiles) {
		return ErrTrustFiles
	}
	before := append(append([]snapshot(nil), m.files...), m.globals...)
	after := append(append([]snapshot(nil), fresh.files...), fresh.globals...)
	if len(before) != len(after) {
		return ErrTrustFiles
	}
	for _, receipt := range receipts {
		if !receipt.finalized || receipt.owner == "" || receipt.before.path != receipt.after.path {
			return ErrTrustFiles
		}
		for i := range before {
			if before[i].path != receipt.before.path {
				continue
			}
			if !sameSnapshot(before[i], receipt.before) {
				return ErrTrustFiles
			}
			before[i] = receipt.after
		}
	}
	for i := range before {
		if !sameSnapshot(before[i], after[i]) {
			return ErrTrustFiles
		}
	}
	return ctx.Err()
}
