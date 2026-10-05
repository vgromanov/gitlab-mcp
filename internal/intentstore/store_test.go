package intentstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
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

func TestWindowsDriveWalkKeepsAbsolute(t *testing.T) {
	got := windowsPathPrefixes(`C:\private\link\intent.db`)
	want := []string{`C:\private`, `C:\private\link`, `C:\private\link\intent.db`}
	if len(got) != len(want) {
		t.Fatalf("prefixes %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("prefix %d = %q, want %q (filepath.Join(%q, %q) = %q)", i, got[i], want[i], "C:", "private", filepath.Join("C:", "private"))
		}
	}
	slash := windowsPathPrefixes(`C:/private/link/intent.db`)
	if len(slash) != 3 || slash[0] != `C:/private` || slash[1] != `C:/private/link` {
		t.Fatalf("slash prefixes %v", slash)
	}
	unc := windowsPathPrefixes(`\\server\share\private\link\intent.db`)
	if len(unc) < 2 || unc[0] != `\\server\share\private` || unc[1] != `\\server\share\private\link` {
		t.Fatalf("unc prefixes %v", unc)
	}
}

type modeInfo struct {
	os.FileInfo
	mode os.FileMode
}

func (m modeInfo) Mode() os.FileMode { return m.mode }

func (m modeInfo) IsDir() bool { return m.mode.IsDir() }

func TestIsSymlinkRejectsIrregularReparse(t *testing.T) {
	// Go 1.25 reports a Windows directory junction as ModeIrregular.
	if !isSymlink(modeInfo{mode: os.ModeDir | os.ModeIrregular}) {
		t.Fatal("junction-like ModeIrregular accepted")
	}
	if !isSymlink(modeInfo{mode: os.ModeSymlink}) {
		t.Fatal("ModeSymlink accepted")
	}
	if isSymlink(modeInfo{mode: os.ModeDir | 0o700}) {
		t.Fatal("ordinary directory treated as a reparse point")
	}
	dir := privateDir(t)
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkDir(dir, modeInfo{FileInfo: info, mode: os.ModeDir | os.ModeIrregular}); !errors.Is(err, ErrSymlink) {
		t.Fatalf("checkDir irregular: %v", err)
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

func TestOpenRejectsDotDot(t *testing.T) {
	dir := privateDir(t)
	sep := string(filepath.Separator)
	// filepath.Join would Clean this to dir/intent.db. Keep the `..`
	// so validation and the database open cannot diverge across a symlink.
	path := dir + sep + "nested" + sep + ".." + sep + "intent.db"
	if _, err := Open(Config{Path: path}); err == nil {
		t.Fatal("accepted path with ..")
	}
	s, err := Open(Config{Path: filepath.Join(dir, "intent.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
}

func TestNewStoreHonorsByteCap(t *testing.T) {
	dir := privateDir(t)
	path := filepath.Join(dir, "intent.db")
	_, err := Open(Config{Path: path, MaxBytes: 1})
	if !errors.Is(err, ErrFull) {
		t.Fatalf("open: %v", err)
	}
	s, err := Open(Config{Path: path})
	if err != nil {
		t.Fatalf("reopen after failed init: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
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
	blob, readErr := os.ReadFile(cfg.Path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	blob = append(blob, err.Error()...)
	if bytes.Contains(blob, []byte(plantedBody)) || bytes.Contains(blob, []byte(plantedToken)) {
		t.Fatal("planted body or token leaked into the database or error")
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
	createForeign(t, other, func(tx *bolt.Tx) error {
		b, err := tx.CreateBucket([]byte("controller"))
		if err != nil {
			return err
		}
		return b.Put([]byte("body"), []byte(plantedBody))
	})
	before, err := os.ReadFile(other)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(Config{Path: other})
	if !errors.Is(err, ErrUnrelatedDatabase) {
		t.Fatalf("unrelated: %v", err)
	}
	if strings.Contains(err.Error(), plantedBody) {
		t.Fatalf("unrelated error leaked: %v", err)
	}
	after, err := os.ReadFile(other)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("unrelated database was modified")
	}
	if !rawHasBucket(t, other, "controller") || rawHasBucket(t, other, "intents") {
		t.Fatal("unrelated database layout changed")
	}
}

func TestForeignContainersAreNotAdopted(t *testing.T) {
	cases := map[string]func(*bolt.Tx) error{
		"other bucket": func(tx *bolt.Tx) error {
			_, err := tx.CreateBucket([]byte("controller"))
			return err
		},
		"meta without schema name": func(tx *bolt.Tx) error {
			_, err := tx.CreateBucket(bucketMeta)
			return err
		},
		"meta with another schema name": func(tx *bolt.Tx) error {
			b, err := tx.CreateBucket(bucketMeta)
			if err != nil {
				return err
			}
			return b.Put(keySchemaName, []byte("other"))
		},
		"schema name without epoch": func(tx *bolt.Tx) error {
			b, err := tx.CreateBucket(bucketMeta)
			if err != nil {
				return err
			}
			return b.Put(keySchemaName, []byte(SchemaName))
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(privateDir(t), "foreign.db")
			createForeign(t, path, build)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Open(Config{Path: path}); !errors.Is(err, ErrUnrelatedDatabase) {
				t.Fatalf("open: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("foreign database was modified")
			}
		})
	}
}

func TestEmptyContainerIsInitialized(t *testing.T) {
	path := filepath.Join(privateDir(t), "skeleton.db")
	createForeign(t, path, func(*bolt.Tx) error { return nil })
	s, err := Open(Config{Path: path})
	if err != nil {
		t.Fatalf("open skeleton: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.Begin(context.Background(), ident("k"), PayloadHash([]byte("a")), BeginOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptContainerIsRefused(t *testing.T) {
	path := filepath.Join(privateDir(t), "garbage.db")
	page := make([]byte, 2*pageSize)
	page[headerMagicOffset] = 0xED
	page[headerMagicOffset+1] = 0xDA
	page[headerMagicOffset+2] = 0x0C
	page[headerMagicOffset+3] = 0xED
	copy(page[100:], plantedToken)
	if err := os.WriteFile(path, page, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(Config{Path: path})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("garbage with magic: %v", err)
	}
	if strings.Contains(err.Error(), plantedToken) {
		t.Fatalf("corrupt error leaked: %v", err)
	}
}

func TestNewerSchemaVersionIsRefused(t *testing.T) {
	cfg, _ := fixedNow(t)
	s := openStore(t, cfg)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	rawWrite(t, cfg.Path, func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(keySchemaVersion, []byte("99"))
	})
	if _, err := Open(cfg); !errors.Is(err, ErrMigration) {
		t.Fatalf("newer schema: %v", err)
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
	_, err := open(ctx, cfg, 2, func(tx *bolt.Tx, from, to int) error {
		if from != 1 || to != 2 {
			t.Fatalf("migration range %d -> %d", from, to)
		}
		if _, err := tx.CreateBucket([]byte("boom")); err != nil {
			return err
		}
		return errors.New("abort migration")
	})
	if !errors.Is(err, ErrMigration) {
		t.Fatalf("migration: %v", err)
	}
	if rawHasBucket(t, cfg.Path, "boom") {
		t.Fatal("failed migration left a bucket")
	}
	if got := rawMeta(t, cfg.Path, "schema_version"); got != "1" {
		t.Fatalf("schema_version %q", got)
	}
	s2 := openStore(t, cfg)
	if _, err := s2.Begin(ctx, ident("k2"), PayloadHash([]byte("b")), BeginOptions{}); err != nil {
		t.Fatalf("writes after rolled-back migration: %v", err)
	}
}

func TestGetEpochAndReceiptShareSnapshot(t *testing.T) {
	cfg, _ := fixedNow(t)
	s := openStore(t, cfg)
	ctx := context.Background()
	rec, err := s.Begin(ctx, ident("k"), PayloadHash([]byte("a")), BeginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	peer := openStore(t, cfg)
	resetDone := make(chan error, 1)
	var raced bool
	afterReadEpoch = func() {
		afterReadEpoch = nil
		raced = true
		peer.DisableDispatch()
		go func() { resetDone <- peer.ResetEpoch(ctx, EpochResetConfirmation) }()
		select {
		case err := <-resetDone:
			t.Errorf("reset committed while a read held the snapshot: %v", err)
		case <-time.After(300 * time.Millisecond):
		}
	}
	t.Cleanup(func() { afterReadEpoch = nil })
	got, err := s.Get(ctx, rec.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if !raced {
		t.Fatal("hook did not run")
	}
	if !got.EpochCurrent {
		t.Fatalf("snapshot mixed epochs: %+v", got)
	}
	if err := <-resetDone; err != nil {
		t.Fatalf("reset after read: %v", err)
	}
	later, err := s.Get(ctx, rec.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if later.EpochCurrent {
		t.Fatalf("row still current after reset: %+v", later)
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
	if _, err := s.Begin(canceled, ident("k2"), PayloadHash([]byte("b")), BeginOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled begin: %v", err)
	}
	if _, err := s.GetByIdentity(ctx, ident("k2")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("canceled begin was durable: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	rawWrite(t, cfg.Path, func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(keyEpoch, nil)
	})
	s2 := &Store{path: cfg.Path, ready: true, dispatch: true, clock: cfg.Now}
	if _, err := s2.Get(ctx, rec.OperationID); !errors.Is(err, ErrUnrelatedDatabase) {
		t.Fatalf("blank epoch: %v", err)
	}
}

func TestCompactNeverGrowsPastCap(t *testing.T) {
	cfg, now := fixedNow(t)
	s := openStore(t, cfg)
	ctx := context.Background()
	for i := 0; i < 40; i++ {
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
	n := fileBytes(t, s.path)
	s.maxBytes = n
	_, err := s.Compact(ctx)
	if after := fileBytes(t, s.path); after > s.maxBytes {
		t.Fatalf("compact grew to %d, cap %d, err %v", after, s.maxBytes, err)
	}
	if err != nil && !errors.Is(err, ErrFull) {
		t.Fatalf("compact: %v", err)
	}
	s.maxBytes = DefaultMaxBytes
	if _, err := s.Compact(ctx); err != nil {
		t.Fatalf("compact under a normal cap: %v", err)
	}
	got, err := s.GetByIdentity(ctx, ident("k000"))
	if err != nil || !got.Compacted {
		t.Fatalf("tombstone: %+v %v", got, err)
	}
}

func TestEncodedIntentPath(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"intent#1.db", "intent%231.db", "intent?x.db"} {
		t.Run(name, func(t *testing.T) {
			if strings.Contains(name, "?") {
				if _, err := Open(Config{Path: filepath.Join(privateDir(t), name)}); err == nil {
					t.Fatal("path with ? accepted")
				}
				return
			}
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
			if !rawHasBucket(t, path, "meta") {
				t.Fatal("the store wrote a different file than the configured path")
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatalf("directory holds %d entries, want only the database", len(entries))
			}
		})
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
	beforeDBOpen = func(p string) {
		beforeDBOpen = nil
		if err := os.Rename(parent, parent+".bak"); err != nil {
			t.Errorf("rename: %v", err)
			return
		}
		if err := os.Symlink(evil, parent); err != nil {
			t.Errorf("symlink: %v", err)
		}
	}
	t.Cleanup(func() { beforeDBOpen = nil })
	_, err := Open(Config{Path: path})
	if !errors.Is(err, ErrSymlink) {
		t.Fatalf("ancestor swap: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(evil, "intent.db")); !os.IsNotExist(err) {
		t.Fatalf("a database appeared through the swapped symlink: %v", err)
	}
}

func TestByteCapStopsGrowthBeforeCommit(t *testing.T) {
	cfg, _ := fixedNow(t)
	s := openStore(t, cfg)
	ctx := context.Background()
	if _, err := s.Begin(ctx, ident("seed"), PayloadHash([]byte("seed")), BeginOptions{}); err != nil {
		t.Fatal(err)
	}
	s.maxBytes = fileBytes(t, s.path) + 192<<10
	var accepted []string
	var full error
	for i := 0; i < 5000 && full == nil; i++ {
		key := fmt.Sprintf("k%04d", i)
		_, err := s.Begin(ctx, ident(key), PayloadHash([]byte(key)), BeginOptions{ExpectedHead: strings.Repeat("h", 100)})
		switch {
		case err == nil:
			accepted = append(accepted, key)
		case errors.Is(err, ErrFull):
			full = err
			if _, err := s.GetByIdentity(ctx, ident(key)); !errors.Is(err, ErrNotFound) {
				t.Fatalf("rejected insert %s is durable: %v", key, err)
			}
		default:
			t.Fatalf("begin %s: %v", key, err)
		}
		if n := fileBytes(t, s.path); n > s.maxBytes {
			t.Fatalf("file is %d bytes after %s, cap %d", n, key, s.maxBytes)
		}
	}
	if full == nil || len(accepted) == 0 {
		t.Fatalf("cap never reached: accepted %d, full %v", len(accepted), full)
	}
	for _, key := range accepted {
		if _, err := s.GetByIdentity(ctx, ident(key)); err != nil {
			t.Fatalf("accepted %s lost: %v", key, err)
		}
	}
	if _, err := s.Begin(ctx, ident(accepted[0]), PayloadHash([]byte(accepted[0])), BeginOptions{}); err != nil {
		t.Fatalf("replay at the cap: %v", err)
	}
	rec, err := s.GetByIdentity(ctx, ident("seed"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordOutcome(ctx, rec.OperationID, Outcome{State: StateRejected}); err != nil && !errors.Is(err, ErrFull) {
		t.Fatalf("outcome at the cap: %v", err)
	}
	s.maxBytes = DefaultMaxBytes
	if _, err := s.Begin(ctx, ident("after"), PayloadHash([]byte("after")), BeginOptions{}); err != nil {
		t.Fatalf("begin after the cap was raised: %v", err)
	}
}

func TestOverCapStoreRejectsNewRowsOnly(t *testing.T) {
	cfg, _ := fixedNow(t)
	s := openStore(t, cfg)
	ctx := context.Background()
	hash := PayloadHash([]byte("a"))
	if _, err := s.Begin(ctx, ident("k"), hash, BeginOptions{}); err != nil {
		t.Fatal(err)
	}
	s.maxBytes = fileBytes(t, s.path) - 1
	if err := s.Writable(); !errors.Is(err, ErrFull) {
		t.Fatalf("writable: %v", err)
	}
	if _, err := s.Begin(ctx, ident("other"), PayloadHash([]byte("b")), BeginOptions{}); !errors.Is(err, ErrFull) {
		t.Fatalf("new key: %v", err)
	}
	if _, err := s.Begin(ctx, ident("k"), hash, BeginOptions{}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if _, err := s.GetByIdentity(ctx, ident("k")); err != nil {
		t.Fatalf("read: %v", err)
	}
}

func TestPeerWithHigherCapGrowthIsHonored(t *testing.T) {
	cfg, _ := fixedNow(t)
	low := openStore(t, cfg)
	ctx := context.Background()
	if _, err := low.Begin(ctx, ident("seed"), PayloadHash([]byte("s")), BeginOptions{}); err != nil {
		t.Fatal(err)
	}
	low.maxBytes = fileBytes(t, low.path)
	if err := low.Writable(); err != nil {
		t.Fatalf("at cap: %v", err)
	}

	peer := openStore(t, cfg)
	peer.maxBytes = DefaultMaxBytes
	for i := 0; i < 40; i++ {
		if _, err := peer.Begin(ctx, ident(fmt.Sprintf("peer-%d", i)), PayloadHash([]byte{byte(i)}), BeginOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if fileBytes(t, low.path) <= low.maxBytes {
		t.Fatal("peer did not grow the file past the lower cap")
	}
	if err := low.Writable(); !errors.Is(err, ErrFull) {
		t.Fatalf("writable after peer growth: %v", err)
	}
	if _, err := low.Begin(ctx, ident("next"), PayloadHash([]byte("n")), BeginOptions{}); !errors.Is(err, ErrFull) {
		t.Fatalf("begin after peer growth: %v", err)
	}
}

func TestHandlesShareOneFileWithoutSidecars(t *testing.T) {
	cfg, _ := fixedNow(t)
	const handles = 6
	stores := make([]*Store, handles)
	for i := range stores {
		stores[i] = openStore(t, cfg)
	}
	ctx := context.Background()
	hash := PayloadHash([]byte("shared"))
	ops := make([]string, handles)
	var wg sync.WaitGroup
	errs := make(chan error, handles*2)
	for i, s := range stores {
		wg.Add(1)
		go func(i int, s *Store) {
			defer wg.Done()
			rec, err := s.Begin(ctx, ident("same"), hash, BeginOptions{})
			if err != nil {
				errs <- err
				return
			}
			ops[i] = rec.OperationID
			if _, err := s.Begin(ctx, ident(fmt.Sprintf("own-%d", i)), PayloadHash([]byte{byte(i)}), BeginOptions{}); err != nil {
				errs <- err
			}
		}(i, s)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for _, op := range ops {
		if op != ops[0] {
			t.Fatalf("handles allocated different operations: %v", ops)
		}
	}
	rows := 0
	if err := stores[0].view(ctx, func(tx *bolt.Tx) error {
		rows = rowCount(tx)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rows != handles+1 {
		t.Fatalf("rows=%d want %d", rows, handles+1)
	}
	entries, err := os.ReadDir(filepath.Dir(cfg.Path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("sidecar or temp files next to the database: %v", entries)
	}
}

func TestBeginReplaysUnderRowCap(t *testing.T) {
	cfg, _ := fixedNow(t)
	cfg.MaxRows = 1
	s := openStore(t, cfg)
	ctx := context.Background()
	hash := PayloadHash([]byte("a"))
	first, err := s.Begin(ctx, ident("k"), hash, BeginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Begin(ctx, ident("k"), hash, BeginOptions{})
	if err != nil || again.OperationID != first.OperationID {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if _, err := s.Begin(ctx, ident("j"), hash, BeginOptions{}); !errors.Is(err, ErrFull) {
		t.Fatalf("second row: %v", err)
	}
}

func TestOperationsRejectClosedHandle(t *testing.T) {
	cfg, _ := fixedNow(t)
	s := openStore(t, cfg)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.Begin(ctx, ident("k"), PayloadHash([]byte("a")), BeginOptions{}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("begin: %v", err)
	}
	if _, err := s.Get(ctx, "x"); !errors.Is(err, ErrNotReady) {
		t.Fatalf("get: %v", err)
	}
}

func TestWritableRefusesLoosenedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	cfg, _ := fixedNow(t)
	s := openStore(t, cfg)
	if err := os.Chmod(cfg.Path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Writable(); !errors.Is(err, ErrUnsafePermissions) {
		t.Fatalf("loosened file: %v", err)
	}
}

func createForeign(t *testing.T, path string, build func(*bolt.Tx) error) {
	t.Helper()
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(build); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func rawView(t *testing.T, path string, fn func(*bolt.Tx) error) {
	t.Helper()
	db, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.View(fn); err != nil {
		t.Fatal(err)
	}
}

func rawWrite(t *testing.T, path string, fn func(*bolt.Tx) error) {
	t.Helper()
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Update(fn); err != nil {
		t.Fatal(err)
	}
}

func rawHasBucket(t *testing.T, path, name string) bool {
	t.Helper()
	found := false
	rawView(t, path, func(tx *bolt.Tx) error {
		found = tx.Bucket([]byte(name)) != nil
		return nil
	})
	return found
}

func rawMeta(t *testing.T, path, key string) string {
	t.Helper()
	var v string
	rawView(t, path, func(tx *bolt.Tx) error {
		v = string(tx.Bucket(bucketMeta).Get([]byte(key)))
		return nil
	})
	return v
}

func fileBytes(t *testing.T, path string) int64 {
	t.Helper()
	n, err := fileSize(path)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
