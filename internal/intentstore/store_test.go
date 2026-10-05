package intentstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	plantedBody  = "NOTE-BODY-secret-do-not-store"
	plantedToken = "glpat-PLANTED-TOKEN-VALUE"
)

func privateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func fixedNow(t *testing.T) (Config, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	cfg := Config{
		Path: filepath.Join(privateDir(t), "intent.db"),
		Now:  func() time.Time { return now },
	}
	return cfg, &now
}

func openStore(t *testing.T, cfg Config) *Store {
	t.Helper()
	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func ident(key string) Identity {
	return Identity{
		Instance:  "https://gitlab.example/api/v4",
		Actor:     "reviewer",
		Project:   "group/proj",
		MR:        "7",
		Kind:      "create_note",
		CallerKey: key,
	}
}

func TestPublishingHandlerDisabled(t *testing.T) {
	if PublishingHandlerEnabled() {
		t.Fatal("publishing handler must stay disabled")
	}
	if err := RequireConfigured("review_write", "", false); err != nil {
		t.Fatal(err)
	}
	if err := RequireConfigured("review_write", "", true); err == nil {
		t.Fatal("review_write with a handler requires the intent database")
	}
	if err := RequireConfigured("review_read", "", true); err != nil {
		t.Fatal(err)
	}
	if err := Check(""); err != nil {
		t.Fatal(err)
	}
}

func TestCloseReopen(t *testing.T) {
	cfg, _ := fixedNow(t)
	s := openStore(t, cfg)
	ctx := context.Background()
	hash := PayloadHash([]byte("same-payload"))
	rec, err := s.Begin(ctx, ident("k"), hash, BeginOptions{ExpectedHead: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != StatePrepared || rec.ExpectedHead != "abc123" {
		t.Fatalf("receipt %+v", rec)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := openStore(t, cfg)
	got, err := s2.Get(ctx, rec.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StatePrepared || got.PayloadHash != hash || got.OperationID != rec.OperationID {
		t.Fatalf("reopened %+v", got)
	}
	again, err := s2.Begin(ctx, ident("k"), hash, BeginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if again.OperationID != rec.OperationID {
		t.Fatalf("replay id %s want %s", again.OperationID, rec.OperationID)
	}
}

func TestPayloadConflictAndClaim(t *testing.T) {
	cfg, now := fixedNow(t)
	s := openStore(t, cfg)
	ctx := context.Background()
	hash := PayloadHash([]byte("payload-a"))
	rec, err := s.Begin(ctx, ident("k"), hash, BeginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	published := false
	_, err = s.Begin(ctx, ident("k"), PayloadHash([]byte("payload-b")), BeginOptions{})
	if !errors.Is(err, ErrPayloadConflict) {
		t.Fatalf("conflict: %v", err)
	}
	if published {
		t.Fatal("conflict must not publish")
	}
	got, err := s.GetByIdentity(ctx, ident("k"))
	if err != nil {
		t.Fatal(err)
	}
	if got.PayloadHash != hash || got.OperationID != rec.OperationID {
		t.Fatalf("row changed: %+v", got)
	}
	claimed, err := s.ClaimSending(ctx, rec.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.State != StateSending || claimed.SendingAt.IsZero() {
		t.Fatalf("claim %+v", claimed)
	}
	if _, err := s.ClaimSending(ctx, rec.OperationID); !errors.Is(err, ErrAlreadySending) {
		t.Fatalf("second claim: %v", err)
	}
	done, err := s.RecordOutcome(ctx, rec.OperationID, Outcome{
		State:             StatePublished,
		ObservedHead:      "abc123",
		UpstreamIDs:       []string{"note-1"},
		VerificationState: "verified",
	})
	if err != nil {
		t.Fatal(err)
	}
	if done.State != StatePublished || done.FinalizedAt.IsZero() || done.UpstreamIDs[0] != "note-1" {
		t.Fatalf("outcome %+v", done)
	}
	if _, err := s.ClaimSending(ctx, rec.OperationID); !errors.Is(err, ErrTerminal) {
		t.Fatalf("terminal claim: %v", err)
	}
	_ = now
}

func TestExpiredPreparedCannotDispatch(t *testing.T) {
	cfg, now := fixedNow(t)
	s := openStore(t, cfg)
	ctx := context.Background()
	hash := PayloadHash([]byte("payload-a"))
	exp := now.Add(time.Minute)
	rec, err := s.Begin(ctx, ident("k"), hash, BeginOptions{ExpiresAt: exp})
	if err != nil {
		t.Fatal(err)
	}
	*now = exp
	if _, err := s.ClaimSending(ctx, rec.OperationID); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired claim: %v", err)
	}
	again, err := s.Begin(ctx, ident("k"), hash, BeginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if again.OperationID != rec.OperationID || again.State != StatePrepared {
		t.Fatalf("reuse allocated a new intent: %+v", again)
	}
}

func TestCompactFinalizedOnly(t *testing.T) {
	cfg, now := fixedNow(t)
	cfg.Retention = 30 * 24 * time.Hour
	s := openStore(t, cfg)
	ctx := context.Background()
	pubHash := PayloadHash([]byte("published-body"))
	uncHash := PayloadHash([]byte("uncertain-body"))
	pub, err := s.Begin(ctx, ident("pub"), pubHash, BeginOptions{ExpectedHead: "head-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimSending(ctx, pub.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordOutcome(ctx, pub.OperationID, Outcome{
		State:             StatePublished,
		ObservedHead:      "head-1",
		UpstreamIDs:       []string{"disc-9"},
		VerificationState: "verified",
	}); err != nil {
		t.Fatal(err)
	}
	unc, err := s.Begin(ctx, ident("unc"), uncHash, BeginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimSending(ctx, unc.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordOutcome(ctx, unc.OperationID, Outcome{
		State:       StateUncertain,
		UpstreamIDs: []string{"maybe-1"},
	}); err != nil {
		t.Fatal(err)
	}

	*now = now.Add(29 * 24 * time.Hour)
	n, err := s.Compact(ctx)
	if err != nil || n != 0 {
		t.Fatalf("early compact n=%d err=%v", n, err)
	}
	*now = now.Add(2 * 24 * time.Hour)
	n, err = s.Compact(ctx)
	if err != nil || n != 1 {
		t.Fatalf("compact n=%d err=%v", n, err)
	}
	got, err := s.Get(ctx, pub.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Compacted || got.State != StatePublished || got.PayloadHash != pubHash || got.ObservedHead != "" || len(got.UpstreamIDs) != 0 {
		t.Fatalf("tombstone %+v", got)
	}
	uncGot, err := s.Get(ctx, unc.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if uncGot.Compacted || uncGot.State != StateUncertain || len(uncGot.UpstreamIDs) != 1 {
		t.Fatalf("uncertain compacted: %+v", uncGot)
	}
	if _, err := s.Begin(ctx, ident("pub"), PayloadHash([]byte("other")), BeginOptions{}); !errors.Is(err, ErrPayloadConflict) {
		t.Fatalf("post-compact conflict: %v", err)
	}
	if _, err := s.ClaimSending(ctx, pub.OperationID); !errors.Is(err, ErrTerminal) {
		t.Fatalf("post-compact claim: %v", err)
	}
}

func TestFullReadOnlyCorrupt(t *testing.T) {
	cfg, _ := fixedNow(t)
	cfg.MaxRows = 1
	s := openStore(t, cfg)
	ctx := context.Background()
	if _, err := s.Begin(ctx, ident("one"), PayloadHash([]byte("a")), BeginOptions{}); err != nil {
		t.Fatal(err)
	}
	called := false
	publish := func() error { called = true; return nil }
	_, err := s.Begin(ctx, ident("two"), PayloadHash([]byte("b")), BeginOptions{})
	if !errors.Is(err, ErrFull) {
		t.Fatalf("full: %v", err)
	}
	if called {
		t.Fatal("full store published")
	}
	_ = publish

	path := s.path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.MaxRows = 0
	cfg.MaxBytes = 1
	s2 := openStore(t, cfg)
	if _, err := s2.GetByIdentity(ctx, ident("one")); err != nil {
		t.Fatal(err)
	}
	called = false
	_, err = s2.Begin(ctx, ident("three"), PayloadHash([]byte("c")), BeginOptions{})
	if !errors.Is(err, ErrFull) {
		t.Fatalf("byte cap: %v", err)
	}
	if called {
		t.Fatal("over-capacity store published")
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	called = false
	if _, err := Open(cfg); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only: %v", err)
	}
	if called {
		t.Fatal("read-only store published")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}

	corrupt := filepath.Join(filepath.Dir(path), "corrupt.db")
	if err := os.WriteFile(corrupt, []byte(plantedToken+plantedBody), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Open(Config{Path: corrupt})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt: %v", err)
	}
	if strings.Contains(err.Error(), plantedToken) || strings.Contains(err.Error(), plantedBody) {
		t.Fatalf("corrupt error leaked: %v", err)
	}
}

func TestRejectPermissionsAndUnrelated(t *testing.T) {
	wideParent := privateDir(t)
	wide := filepath.Join(wideParent, "wide")
	if err := os.Mkdir(wide, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(wide, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Config{Path: filepath.Join(wide, "intent.db")}); !errors.Is(err, ErrUnsafePermissions) {
		t.Fatalf("dir mode: %v", err)
	}

	dir := privateDir(t)
	loose := filepath.Join(dir, "loose.db")
	if err := os.WriteFile(loose, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Config{Path: loose}); !errors.Is(err, ErrUnsafePermissions) {
		t.Fatalf("file mode: %v", err)
	}

	real := filepath.Join(dir, "real.db")
	if err := os.WriteFile(real, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Config{Path: link}); !errors.Is(err, ErrSymlink) {
		t.Fatalf("symlink: %v", err)
	}

	other := filepath.Join(dir, "controller.db")
	createUnrelated(t, other)
	_, err := Open(Config{Path: other})
	if !errors.Is(err, ErrUnrelatedDatabase) {
		t.Fatalf("unrelated: %v", err)
	}
	if strings.Contains(err.Error(), plantedBody) {
		t.Fatalf("unrelated error leaked: %v", err)
	}
	if !rawHasTable(t, other, "controller") {
		t.Fatal("unrelated database was modified")
	}
	if rawHasTable(t, other, "intents") {
		t.Fatal("intent schema was applied to an unrelated database")
	}
}

func TestNoSecretLeak(t *testing.T) {
	cfg, _ := fixedNow(t)
	s := openStore(t, cfg)
	ctx := context.Background()
	hash := PayloadHash([]byte(plantedBody + plantedToken))
	if _, err := s.Begin(ctx, ident("k"), plantedToken, BeginOptions{}); !errors.Is(err, ErrInvalidHash) {
		t.Fatalf("token as hash: %v", err)
	}
	if _, err := s.Begin(ctx, ident("k"), hash, BeginOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Begin(ctx, ident("k"), PayloadHash([]byte("different")), BeginOptions{})
	if !errors.Is(err, ErrPayloadConflict) {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	blob := readSidecars(t, cfg.Path)
	blob += err.Error()
	if strings.Contains(blob, plantedBody) || strings.Contains(blob, plantedToken) {
		t.Fatal("planted body or token leaked into the database, sidecars, or error")
	}
}

func TestMigrationRollback(t *testing.T) {
	cfg, _ := fixedNow(t)
	s := openStore(t, cfg)
	ctx := context.Background()
	if _, err := s.Begin(ctx, ident("k"), PayloadHash([]byte("a")), BeginOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := open(ctx, cfg, 2, func(tx *sql.Tx, from, to int) error {
		if from != 1 || to != 2 {
			t.Fatalf("migration range %d -> %d", from, to)
		}
		if _, err := tx.Exec(`ALTER TABLE intents ADD COLUMN boom TEXT`); err != nil {
			return err
		}
		return errors.New("abort migration")
	})
	if !errors.Is(err, ErrMigration) {
		t.Fatalf("migration: %v", err)
	}
	if rawColumn(t, cfg.Path, "boom") {
		t.Fatal("failed migration left a column")
	}
	if rawUserVersion(t, cfg.Path) != 1 {
		t.Fatalf("user_version %d", rawUserVersion(t, cfg.Path))
	}
	s2 := openStore(t, cfg)
	if _, err := s2.Begin(ctx, ident("k2"), PayloadHash([]byte("b")), BeginOptions{}); err != nil {
		t.Fatalf("writes after rolled-back migration: %v", err)
	}
}

func TestEpochResetKeepsTombstones(t *testing.T) {
	cfg, _ := fixedNow(t)
	s := openStore(t, cfg)
	ctx := context.Background()
	hash := PayloadHash([]byte("a"))
	rec, err := s.Begin(ctx, ident("k"), hash, BeginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResetEpoch(ctx, EpochResetConfirmation); !errors.Is(err, ErrDispatchEnabled) {
		t.Fatalf("reset while enabled: %v", err)
	}
	s.DisableDispatch()
	if err := s.ResetEpoch(ctx, "nope"); !errors.Is(err, ErrConfirmation) {
		t.Fatalf("bad confirmation: %v", err)
	}
	if err := s.ResetEpoch(ctx, EpochResetConfirmation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin(ctx, ident("new"), PayloadHash([]byte("b")), BeginOptions{}); !errors.Is(err, ErrWritesDisabled) {
		t.Fatalf("dispatch after disable: %v", err)
	}
	s.dispatch = true
	if _, err := s.ClaimSending(ctx, rec.OperationID); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("stale claim: %v", err)
	}
	if _, err := s.Begin(ctx, ident("k"), PayloadHash([]byte("other")), BeginOptions{}); !errors.Is(err, ErrPayloadConflict) {
		t.Fatalf("old key after epoch reset: %v", err)
	}
	fresh, err := s.Begin(ctx, ident("new"), PayloadHash([]byte("b")), BeginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.EpochCurrent || fresh.Epoch == rec.Epoch {
		t.Fatalf("new epoch row %+v old %s", fresh, rec.Epoch)
	}
}

func TestResetEpochCommitFailureKeepsEpoch(t *testing.T) {
	cfg, _ := fixedNow(t)
	s := openStore(t, cfg)
	ctx := context.Background()
	s.DisableDispatch()
	before := s.epoch
	s.commitBarrier = func() error {
		return errors.New("forced commit failure")
	}
	err := s.ResetEpoch(ctx, EpochResetConfirmation)
	s.commitBarrier = nil
	if err == nil {
		t.Fatal("expected commit failure")
	}
	if s.epoch != before {
		t.Fatalf("memory epoch changed to %s", s.epoch)
	}
	if got := rawMeta(t, cfg.Path, "epoch"); got != before {
		t.Fatalf("persisted epoch %s, want %s", got, before)
	}
	if err := s.ResetEpoch(ctx, EpochResetConfirmation); err != nil {
		t.Fatal(err)
	}
	if s.epoch == before {
		t.Fatal("epoch did not advance after a successful reset")
	}
	if got := rawMeta(t, cfg.Path, "epoch"); got != s.epoch {
		t.Fatalf("persisted %s memory %s", got, s.epoch)
	}
}

func TestForeignEpochBlocksOldWrite(t *testing.T) {
	cfg, _ := fixedNow(t)
	a := openStore(t, cfg)
	b := openStore(t, cfg)
	ctx := context.Background()
	rec, err := b.Begin(ctx, ident("k"), PayloadHash([]byte("a")), BeginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	oldEpoch := rec.Epoch
	a.DisableDispatch()
	if err := a.ResetEpoch(ctx, EpochResetConfirmation); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ClaimSending(ctx, rec.OperationID); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("claim after foreign reset: %v", err)
	}
	if b.epoch != oldEpoch {
		t.Fatalf("foreign store adopted epoch %s", b.epoch)
	}
	if _, err := b.Begin(ctx, ident("new"), PayloadHash([]byte("b")), BeginOptions{}); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("begin after foreign reset: %v", err)
	}
	if _, err := b.ClaimSending(ctx, rec.OperationID); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("second claim: %v", err)
	}
	if _, err := b.GetByIdentity(ctx, ident("new")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old-epoch insert: %v", err)
	}
	got, err := b.Get(ctx, rec.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StatePrepared || got.Epoch != oldEpoch || got.EpochCurrent {
		t.Fatalf("old row changed: %+v", got)
	}
	if b.epoch != oldEpoch || !b.dispatch {
		t.Fatalf("cache epoch %s dispatch %v", b.epoch, b.dispatch)
	}
}

func TestDisabledHandleCannotResetNewerEpoch(t *testing.T) {
	cfg, _ := fixedNow(t)
	a := openStore(t, cfg)
	b := openStore(t, cfg)
	ctx := context.Background()
	old := b.epoch
	a.DisableDispatch()
	b.DisableDispatch()
	if err := a.ResetEpoch(ctx, EpochResetConfirmation); err != nil {
		t.Fatal(err)
	}
	if err := b.ResetEpoch(ctx, EpochResetConfirmation); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("stale reset: %v", err)
	}
	if b.epoch != old {
		t.Fatalf("stale handle adopted %s", b.epoch)
	}
	if got := rawMeta(t, cfg.Path, "epoch"); got != a.epoch {
		t.Fatalf("persisted %s, current %s", got, a.epoch)
	}
	a.dispatch = true
	if _, err := a.Begin(ctx, ident("k"), PayloadHash([]byte("a")), BeginOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestGetCanceledContextIsNotUnrelated(t *testing.T) {
	cfg, _ := fixedNow(t)
	s := openStore(t, cfg)
	ctx := context.Background()
	rec, err := s.Begin(ctx, ident("k"), PayloadHash([]byte("a")), BeginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = s.Get(canceled, rec.OperationID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("get: %v", err)
	}
	if errors.Is(err, ErrUnrelatedDatabase) {
		t.Fatal("canceled get reported an unrelated database")
	}
	_, err = s.GetByIdentity(canceled, ident("k"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("identity: %v", err)
	}
	if errors.Is(err, ErrUnrelatedDatabase) {
		t.Fatal("canceled identity lookup reported an unrelated database")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE meta SET value='' WHERE key='epoch'`); err != nil {
		t.Fatal(err)
	}
	_, err = s.Get(ctx, rec.OperationID)
	if !errors.Is(err, ErrUnrelatedDatabase) {
		t.Fatalf("blank epoch: %v", err)
	}
}

func TestStaleHandleCannotFinalizeCurrentEpoch(t *testing.T) {
	cfg, _ := fixedNow(t)
	current := openStore(t, cfg)
	stale := openStore(t, cfg)
	ctx := context.Background()
	own, err := stale.Begin(ctx, ident("old"), PayloadHash([]byte("old")), BeginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	current.DisableDispatch()
	if err := current.ResetEpoch(ctx, EpochResetConfirmation); err != nil {
		t.Fatal(err)
	}
	current.dispatch = true
	rec, err := current.Begin(ctx, ident("new"), PayloadHash([]byte("new")), BeginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []State{StateRejected, StateUncertain} {
		_, err := stale.RecordOutcome(ctx, rec.OperationID, Outcome{State: state})
		if !errors.Is(err, ErrStaleEpoch) {
			t.Fatalf("stale %s: %v", state, err)
		}
	}
	got, err := current.Get(ctx, rec.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StatePrepared || got.Epoch != rec.Epoch {
		t.Fatalf("current receipt changed: %+v", got)
	}
	if _, err := current.RecordOutcome(ctx, own.OperationID, Outcome{State: StateRejected}); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("new epoch finalizing old receipt: %v", err)
	}
	ownGot, err := stale.RecordOutcome(ctx, own.OperationID, Outcome{State: StateRejected})
	if err != nil {
		t.Fatal(err)
	}
	if ownGot.State != StateRejected || ownGot.Epoch != own.Epoch {
		t.Fatalf("own outcome: %+v", ownGot)
	}
	got, err = current.RecordOutcome(ctx, rec.OperationID, Outcome{State: StateUncertain})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateUncertain || !got.EpochCurrent {
		t.Fatalf("current outcome: %+v", got)
	}
}

func TestRecordOutcomeAfterDisableDispatch(t *testing.T) {
	cfg, _ := fixedNow(t)
	s := openStore(t, cfg)
	ctx := context.Background()
	rec, err := s.Begin(ctx, ident("k"), PayloadHash([]byte("a")), BeginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimSending(ctx, rec.OperationID); err != nil {
		t.Fatal(err)
	}
	s.DisableDispatch()
	if _, err := s.Begin(ctx, ident("new"), PayloadHash([]byte("b")), BeginOptions{}); !errors.Is(err, ErrWritesDisabled) {
		t.Fatalf("begin: %v", err)
	}
	if _, err := s.ClaimSending(ctx, rec.OperationID); !errors.Is(err, ErrWritesDisabled) {
		t.Fatalf("claim: %v", err)
	}
	got, err := s.RecordOutcome(ctx, rec.OperationID, Outcome{State: StatePublished, ObservedHead: "head"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StatePublished {
		t.Fatalf("outcome %+v", got)
	}
}

func TestCompactRespectsDirtyPages(t *testing.T) {
	cfg, now := fixedNow(t)
	s := openStore(t, cfg)
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("k%03d", i)
		rec, err := s.Begin(ctx, ident(key), PayloadHash([]byte(key)), BeginOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimSending(ctx, rec.OperationID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RecordOutcome(ctx, rec.OperationID, Outcome{State: StatePublished, ObservedHead: "head"}); err != nil {
			t.Fatal(err)
		}
	}
	*now = now.Add(31 * 24 * time.Hour)
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	n, err := bytesOnDisk(s.path)
	if err != nil {
		t.Fatal(err)
	}
	s.maxBytes = n + 5000
	_, err = s.Compact(ctx)
	after, sizeErr := bytesOnDisk(s.path)
	if sizeErr != nil {
		t.Fatal(sizeErr)
	}
	if after > s.maxBytes {
		t.Fatalf("compact grew to %d, cap %d, err %v", after, s.maxBytes, err)
	}
	if err == nil {
		t.Fatalf("compact of 100 rows fit in 5000 bytes (%d -> %d)", n, after)
	}
	if !errors.Is(err, ErrFull) {
		t.Fatalf("compact: %v", err)
	}
}

func TestIntermediateSymlinkRejected(t *testing.T) {
	base := privateDir(t)
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(real, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(link, "subdir", "intent.db")
	if _, err := Open(Config{Path: path}); !errors.Is(err, ErrSymlink) {
		t.Fatalf("intermediate symlink: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(real, "subdir", "intent.db")); !os.IsNotExist(err) {
		t.Fatal("database was created through the symlink")
	}
}

func TestUNCPathMatchesWindowsAbsolute(t *testing.T) {
	path := `\\server\share\intent.db`
	if !isAbs(path) {
		t.Fatal("UNC path rejected")
	}
	if parentDir(path) != `\\server\share` {
		t.Fatalf("parent %q", parentDir(path))
	}
	if isAbs(`\\server`) || isAbs(`relative\intent.db`) {
		t.Fatal("incomplete UNC or relative path accepted")
	}
}

func TestByteCapCountsPendingGrowth(t *testing.T) {
	cfg, _ := fixedNow(t)
	s := openStore(t, cfg)
	ctx := context.Background()
	n, err := bytesOnDisk(s.path)
	if err != nil {
		t.Fatal(err)
	}
	s.maxBytes = n + 64
	if _, err := s.Begin(ctx, ident("k"), PayloadHash([]byte("a")), BeginOptions{}); !errors.Is(err, ErrFull) {
		t.Fatalf("pending growth: %v", err)
	}
	if _, err := s.GetByIdentity(ctx, ident("k")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rolled-back insert visible: %v", err)
	}
	s.maxBytes = DefaultMaxBytes
	if _, err := s.Begin(ctx, ident("k"), PayloadHash([]byte("a")), BeginOptions{}); err != nil {
		t.Fatalf("write under cap: %v", err)
	}
}

func TestEncodedIntentPath(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"intent#1.db", "intent%231.db"} {
		t.Run(name, func(t *testing.T) {
			dir := privateDir(t)
			path := filepath.Join(dir, name)
			s, err := Open(Config{Path: path})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			if _, err := s.Begin(ctx, ident("k"), PayloadHash([]byte("a")), BeginOptions{}); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() == 0 || !rawHasTable(t, path, "meta") {
				t.Fatal("sqlite opened a different file than the configured path")
			}
			if name == "intent%231.db" {
				if _, err := os.Lstat(filepath.Join(dir, "intent#1.db")); !os.IsNotExist(err) {
					t.Fatalf("percent-escape opened %v", err)
				}
			}
		})
	}
}

func TestRejectViewOnlyDatabase(t *testing.T) {
	cfg, _ := fixedNow(t)
	db, err := sql.Open("sqlite", cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE VIEW only_view AS SELECT 1 AS n`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg.Path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(cfg); !errors.Is(err, ErrUnrelatedDatabase) {
		t.Fatalf("view-only: %v", err)
	}
	if rawHasTable(t, cfg.Path, "meta") {
		t.Fatal("view-only database was rewritten")
	}
	if !rawHasObject(t, cfg.Path, "view", "only_view") {
		t.Fatal("view was dropped")
	}
}

func TestParentDirIsFilesystemRoot(t *testing.T) {
	root := string(filepath.Separator)
	got := parentDir(root + "intent.db")
	if got != root {
		t.Fatalf("parent of root file = %q, want %q", got, root)
	}
	if runtime.GOOS == "windows" && parentDir(`C:\intent.db`) != `C:\` {
		t.Fatalf("parent of C:\\intent.db = %q", parentDir(`C:\intent.db`))
	}
}

func TestNewStoreHonorsByteCap(t *testing.T) {
	dir := privateDir(t)
	path := filepath.Join(dir, "intent.db")
	_, err := Open(Config{Path: path, MaxBytes: 1})
	if !errors.Is(err, ErrFull) {
		t.Fatalf("open: %v", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, statErr := os.Lstat(path + suffix); !os.IsNotExist(statErr) {
			t.Fatalf("left %s behind: %v", suffix, statErr)
		}
	}
}

func TestBeginStopsAtFlushedSize(t *testing.T) {
	cfg, _ := fixedNow(t)
	s := openStore(t, cfg)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	n, err := bytesOnDisk(s.path)
	if err != nil {
		t.Fatal(err)
	}
	s.maxBytes = 60000
	if n >= s.maxBytes {
		t.Fatalf("fresh store is %d", n)
	}
	_, err = s.Begin(ctx, ident("k"), PayloadHash([]byte("a")), BeginOptions{})
	after, sizeErr := bytesOnDisk(s.path)
	if sizeErr != nil {
		t.Fatal(sizeErr)
	}
	if after > s.maxBytes {
		t.Fatalf("begin grew to %d, cap %d, err %v", after, s.maxBytes, err)
	}
	if !errors.Is(err, ErrFull) {
		t.Fatalf("begin %v (%d -> %d)", err, n, after)
	}
}

func TestInitCapBetweenFlushAndCommit(t *testing.T) {
	// Schema flush is 57496 bytes and commit adds one 4120-byte frame (61616).
	dir := privateDir(t)
	path := filepath.Join(dir, "intent.db")
	_, err := Open(Config{Path: path, MaxBytes: 60000})
	if !errors.Is(err, ErrFull) {
		t.Fatalf("open: %v", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, statErr := os.Lstat(path + suffix); !os.IsNotExist(statErr) {
			t.Fatalf("left %s behind: %v", suffix, statErr)
		}
	}
}

func TestBeginCapBetweenFlushAndCommit(t *testing.T) {
	// On a fresh store, Begin flushes to 73976 and commits at 78096.
	cfg, _ := fixedNow(t)
	cfg.MaxBytes = 75000
	s := openStore(t, cfg)
	ctx := context.Background()
	before, err := bytesOnDisk(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if before >= s.maxBytes {
		t.Fatalf("open size %d already at cap", before)
	}
	_, err = s.Begin(ctx, ident("k"), PayloadHash([]byte("a")), BeginOptions{})
	after, sizeErr := bytesOnDisk(s.path)
	if sizeErr != nil {
		t.Fatal(sizeErr)
	}
	if after > s.maxBytes {
		t.Fatalf("begin grew to %d, cap %d, err %v", after, s.maxBytes, err)
	}
	if !errors.Is(err, ErrFull) {
		t.Fatalf("begin %v (%d -> %d)", err, before, after)
	}
	if _, err := s.GetByIdentity(ctx, ident("k")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rolled-back insert visible: %v", err)
	}
}

func TestClaimCapBetweenFlushAndCommit(t *testing.T) {
	cfg, _ := fixedNow(t)
	s := openStore(t, cfg)
	ctx := context.Background()
	rec, err := s.Begin(ctx, ident("k"), PayloadHash([]byte("a")), BeginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	n, err := bytesOnDisk(s.path)
	if err != nil {
		t.Fatal(err)
	}
	// Flush of the claim is about 4152 bytes; commit adds another 4120-byte frame.
	s.maxBytes = n + 5000
	_, err = s.ClaimSending(ctx, rec.OperationID)
	after, sizeErr := bytesOnDisk(s.path)
	if sizeErr != nil {
		t.Fatal(sizeErr)
	}
	if after > s.maxBytes {
		t.Fatalf("claim grew to %d, cap %d, err %v", after, s.maxBytes, err)
	}
	if !errors.Is(err, ErrFull) {
		t.Fatalf("claim %v (%d -> %d)", err, n, after)
	}
	got, err := s.Get(ctx, rec.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StatePrepared {
		t.Fatalf("state %s", got.State)
	}
}

func TestSqlitePrefixIsLiteral(t *testing.T) {
	cfg, _ := fixedNow(t)
	db, err := sql.Open("sqlite", cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sqlitex_controller(body TEXT)`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(userObjectCountSQL).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("user objects %d", n)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg.Path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(cfg); !errors.Is(err, ErrUnrelatedDatabase) {
		t.Fatalf("lookalike sqlite name: %v", err)
	}
	if rawHasTable(t, cfg.Path, "meta") || rawHasTable(t, cfg.Path, "intents") {
		t.Fatal("unrelated database was rewritten")
	}
	if !rawHasTable(t, cfg.Path, "sqlitex_controller") {
		t.Fatal("sqlitex_controller was dropped")
	}
}

func TestWritableCountsRows(t *testing.T) {
	cfg, _ := fixedNow(t)
	cfg.MaxRows = 1
	s := openStore(t, cfg)
	ctx := context.Background()
	if err := s.Writable(); err != nil {
		t.Fatal(err)
	}
	hash := PayloadHash([]byte("a"))
	if _, err := s.Begin(ctx, ident("k"), hash, BeginOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Writable(); !errors.Is(err, ErrFull) {
		t.Fatalf("writable at max rows: %v", err)
	}
	if _, err := s.Begin(ctx, ident("k"), hash, BeginOptions{}); err != nil {
		t.Fatalf("replay while full: %v", err)
	}
	if _, err := s.Begin(ctx, ident("other"), PayloadHash([]byte("b")), BeginOptions{}); !errors.Is(err, ErrFull) {
		t.Fatalf("new key while full: %v", err)
	}
}

func TestFailedInitDoesNotDeletePeerStore(t *testing.T) {
	cfg, _ := fixedNow(t)
	ctx := context.Background()
	afterPrepare = func(s *Store) error {
		afterPrepare = nil
		peer, err := Open(Config{Path: s.path})
		if err != nil {
			return err
		}
		t.Cleanup(func() { _ = peer.Close() })
		if _, err := peer.Begin(ctx, ident("k"), PayloadHash([]byte("a")), BeginOptions{}); err != nil {
			return err
		}
		return ErrFull
	}
	t.Cleanup(func() { afterPrepare = nil })
	_, err := Open(cfg)
	if !errors.Is(err, ErrFull) {
		t.Fatalf("creator: %v", err)
	}
	if _, err := os.Lstat(cfg.Path); err != nil {
		t.Fatalf("peer database removed: %v", err)
	}
	s := openStore(t, cfg)
	if _, err := s.GetByIdentity(ctx, ident("k")); err != nil {
		t.Fatalf("peer row: %v", err)
	}
}

func TestOpenRejectsAncestorSwap(t *testing.T) {
	base := privateDir(t)
	parent := filepath.Join(base, "parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "intent.db")
	evil := filepath.Join(base, "evil")
	if err := os.Mkdir(evil, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(evil, 0o700); err != nil {
		t.Fatal(err)
	}
	beforeSQLOpen = func(p string) {
		beforeSQLOpen = nil
		if err := os.Rename(parent, parent+".bak"); err != nil {
			t.Errorf("rename: %v", err)
			return
		}
		if err := os.Symlink(evil, parent); err != nil {
			t.Errorf("symlink: %v", err)
		}
	}
	t.Cleanup(func() { beforeSQLOpen = nil })
	_, err := Open(Config{Path: path})
	if !errors.Is(err, ErrSymlink) {
		t.Fatalf("ancestor swap: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(evil, "intent.db")); err == nil {
		t.Fatal("opened through the swapped symlink")
	}
}

func TestCreateRaceOpensExistingFile(t *testing.T) {
	cfg, _ := fixedNow(t)
	beforeCreate = func(path string) {
		beforeCreate = nil
		if err := createExclusive(path); err != nil {
			t.Errorf("hook: %v", err)
		}
	}
	t.Cleanup(func() { beforeCreate = nil })
	s := openStore(t, cfg)
	if _, err := s.Begin(context.Background(), ident("k"), PayloadHash([]byte("a")), BeginOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestNestedParentIsPrivate(t *testing.T) {
	path := filepath.Join(privateDir(t), "nested", "intent.db")
	s, err := Open(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("parent mode %o", info.Mode().Perm())
	}
	if _, err := s.Begin(context.Background(), ident("k"), PayloadHash([]byte("a")), BeginOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestMissingDirThroughSymlinkIsNotCreated(t *testing.T) {
	base := privateDir(t)
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(link, "missing", "intent.db")
	if _, err := Open(Config{Path: path}); !errors.Is(err, ErrSymlink) {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(real, "missing")); !os.IsNotExist(err) {
		t.Fatal("directory was created through the symlink")
	}
}

func TestSchemaTriggerRejectsStaleEpoch(t *testing.T) {
	cfg, _ := fixedNow(t)
	s := openStore(t, cfg)
	_, err := s.db.Exec(`INSERT INTO intents (
		operation_id, instance_id, actor, project, mr, operation_kind, caller_key,
		payload_hash, state, created_unix_nano, updated_unix_nano, compacted, row_epoch
	) VALUES ('x','i','a','p','1','k','c', ?, 'prepared', 1, 1, 0, 'old-epoch')`, PayloadHash([]byte("a")))
	if err == nil || !errors.Is(mapDriver(err), ErrStaleEpoch) {
		t.Fatalf("trigger: %v", err)
	}
}

func TestSQLiteFileURIEncodesReservedBytes(t *testing.T) {
	got := sqliteFileURI("/tmp/a#b%23.db", "mode=ro")
	if strings.Contains(got, "#") || !strings.Contains(got, "a%23b%2523.db") || !strings.Contains(got, "mode=ro") {
		t.Fatal(got)
	}
}

func TestWalAutocheckpointDisabled(t *testing.T) {
	if !strings.Contains(writeDSNQuery, "wal_autocheckpoint(0)") {
		t.Fatal(writeDSNQuery)
	}
	cfg, _ := fixedNow(t)
	s := openStore(t, cfg)
	var n int
	if err := s.db.QueryRow(`PRAGMA wal_autocheckpoint`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("wal_autocheckpoint=%d", n)
	}
}

func TestCheckpointCommitStaysUnderCap(t *testing.T) {
	// 1200 commits cross the default 1000-frame autocheckpoint. With that
	// checkpoint, the main file grows while the WAL stays allocated. A 6 MiB
	// cap sits above one copy of those frames and below db+wal doubled.
	cfg, _ := fixedNow(t)
	cfg.MaxBytes = 6 << 20
	s := openStore(t, cfg)
	ctx := context.Background()
	for i := 0; i < 1200; i++ {
		key := fmt.Sprintf("k%04d", i)
		if _, err := s.Begin(ctx, ident(key), PayloadHash([]byte(key)), BeginOptions{}); err != nil {
			t.Fatalf("begin %s: %v", key, err)
		}
		n, err := bytesOnDisk(s.path)
		if err != nil {
			t.Fatal(err)
		}
		if n > s.maxBytes {
			t.Fatalf("size %d above cap %d after %s", n, s.maxBytes, key)
		}
	}
}

func createUnrelated(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE controller (body TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO controller(body) VALUES (?)`, plantedBody); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func rawHasTable(t *testing.T, path, name string) bool {
	t.Helper()
	return rawHasObject(t, path, "table", name)
}

func rawHasObject(t *testing.T, path, kind, name string) bool {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteFileURI(path, "mode=ro&_query_only=1"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type=? AND name=?`, kind, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func rawMeta(t *testing.T, path, key string) string {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteFileURI(path, "mode=ro&_query_only=1"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func rawColumn(t *testing.T, path, name string) bool {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('intents') WHERE name=?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func rawUserVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func readSidecars(t *testing.T, path string) string {
	t.Helper()
	var b strings.Builder
	for _, p := range sidecarPaths(path) {
		buf, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		b.Write(buf)
	}
	return b.String()
}
