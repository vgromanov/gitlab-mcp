package intentstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // register the CGO-free SQLite driver
)

// Config selects the database file and local limits.
type Config struct {
	Path      string
	Now       func() time.Time
	MaxRows   int
	MaxBytes  int64
	Retention time.Duration
}

// Store is a process-local handle on the intent database.
type Store struct {
	mu        sync.Mutex
	db        *sql.DB
	path      string
	epoch     string
	ready     bool
	dispatch  bool
	maxRows   int
	maxBytes  int64
	retention time.Duration
	clock     func() time.Time
	// commitBarrier runs after the statement work and before Commit.
	// Tests use it to fail the commit once the new epoch is staged.
	commitBarrier func() error
	// lastAccepted is db+wal+shm after the last successful open or
	// commit. Flushed frames from a rejected write are not counted
	// against the cap until they become durable.
	lastAccepted int64
}

// beforeCreate runs after a missing database file is observed and before the
// exclusive create. Tests use it to simulate another process winning the race.
var beforeCreate func(path string)

// afterPrepare runs after the file is opened and before initialize.
// Tests use it to let another handle finish initialization, then fail.
var afterPrepare func(*Store) error

// beforeSQLOpen runs after the descriptor walk and before sql.Open.
// Tests use it to swap an ancestor for a symlink in that gap.
var beforeSQLOpen func(path string)

// afterReadEpoch runs after meta.epoch is read inside a Get snapshot
// and before the receipt is loaded. Tests use it to reset the epoch.
var afterReadEpoch func()

// afterDurableCommit runs after Commit and before reclaimWAL.
// Tests use it to open a reader snapshot in that gap.
var afterDurableCommit func()

type migrateFunc func(tx *sql.Tx, from, to int) error

// PublishingHandlerEnabled reports whether this process registers a publisher.
// It stays false until a later issue wires a guarded write handler.
func PublishingHandlerEnabled() bool { return false }

// RequireConfigured fails closed when a publishing handler is enabled on
// review_write without GITLAB_MCP_INTENT_DB.
func RequireConfigured(profile, path string, handlerEnabled bool) error {
	if handlerEnabled && profile == "review_write" && strings.TrimSpace(path) == "" {
		return errors.New("GITLAB_MCP_INTENT_DB is required for review_write when a publishing handler is enabled")
	}
	return nil
}

// Check opens the store when path is set and requires it to be writable.
// An empty path is success.
func Check(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	s, err := Open(Config{Path: path})
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Writable()
}

// Open creates or reopens an intent database at cfg.Path.
func Open(cfg Config) (*Store, error) {
	return open(context.Background(), cfg, 0, nil)
}

func open(ctx context.Context, cfg Config, targetVersion int, migrate migrateFunc) (*Store, error) {
	if targetVersion == 0 {
		targetVersion = SchemaVersion
	}
	s, created, err := prepare(cfg)
	if err != nil {
		return nil, err
	}
	if afterPrepare != nil {
		if err := afterPrepare(s); err != nil {
			return abandonCreated(s, created, err)
		}
	}
	if err := callBusy(func() error { return s.initialize(ctx, targetVersion, migrate) }); err != nil {
		return abandonCreated(s, created, err)
	}
	return s, nil
}

func abandonCreated(s *Store, _ bool, err error) (*Store, error) {
	_ = s.db.Close()
	// Leave a published file in place. application_id is not a safe
	// keep-signal: a peer can be blocked on BeginTx before it commits
	// the schema, and deleting here would make its receipts vanish.
	return nil, err
}

func prepare(cfg Config) (store *Store, created bool, err error) {
	path := cfg.Path
	if !isAbs(path) || strings.ContainsAny(path, "?\x00") {
		return nil, false, errors.New("intent store: path must be absolute")
	}
	if err := rejectDotDot(path); err != nil {
		return nil, false, err
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return nil, false, err
	}
	parent := parentDir(path)
	info, err := os.Lstat(parent)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, false, err
		}
		if err := makeParents(parent); err != nil {
			return nil, false, err
		}
		info, err = os.Lstat(parent)
		if err != nil {
			return nil, false, err
		}
	}
	if err := checkDir(parent, info); err != nil {
		return nil, false, err
	}
	info, err = os.Lstat(path)
	switch {
	case err == nil:
		if err := validateExistingFile(path, info); err != nil {
			return nil, false, err
		}
	case os.IsNotExist(err):
		if beforeCreate != nil {
			beforeCreate(path)
		}
		err := createExclusive(path)
		if os.IsExist(err) {
			info, err = os.Lstat(path)
			if err != nil {
				return nil, false, err
			}
			if err := validateExistingFile(path, info); err != nil {
				return nil, false, err
			}
		} else if err != nil {
			return nil, false, err
		} else {
			created = true
			if err := establishPrivate(path, false); err != nil {
				return nil, false, err
			}
		}
	default:
		return nil, false, err
	}
	before, err := existingSidecars(path)
	if err != nil {
		return nil, false, err
	}
	ident, err := identifyFile(path)
	if err != nil {
		return nil, false, err
	}
	if beforeSQLOpen != nil {
		beforeSQLOpen(path)
	}
	dsn := sqliteFileURI(path, writeDSNQuery)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, false, mapDriver(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := callBusy(func() error { return db.Ping() }); err != nil {
		_ = db.Close()
		return nil, false, mapDriver(err)
	}
	opened, err := identifyFile(path)
	if err != nil {
		_ = db.Close()
		return nil, false, err
	}
	if !sameFile(ident, opened) {
		_ = db.Close()
		return nil, false, ErrSymlink
	}
	if err := rejectSymlinkComponents(path); err != nil {
		_ = db.Close()
		return nil, false, err
	}
	if err := lockDownNewSidecars(path, before); err != nil {
		_ = db.Close()
		return nil, false, err
	}
	maxRows := cfg.MaxRows
	if maxRows <= 0 {
		maxRows = DefaultMaxRows
	}
	maxBytes := cfg.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	retention := cfg.Retention
	if retention <= 0 {
		retention = DefaultRetention
	}
	clock := cfg.Now
	if clock == nil {
		clock = time.Now
	}
	return &Store{
		db:        db,
		path:      path,
		dispatch:  true,
		maxRows:   maxRows,
		maxBytes:  maxBytes,
		retention: retention,
		clock:     clock,
	}, created, nil
}

func validateExistingFile(path string, info os.FileInfo) error {
	if err := checkFileMode(path, info); err != nil {
		return err
	}
	if err := classifyHeader(path); err != nil {
		return err
	}
	if info.Size() > 0 {
		return refuseUnrelated(path)
	}
	return nil
}

func (s *Store) initialize(ctx context.Context, targetVersion int, migrate migrateFunc) error {
	before, err := existingSidecars(s.path)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return mapDriver(err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var appID int64
	if err := tx.QueryRowContext(ctx, "PRAGMA application_id").Scan(&appID); err != nil {
		return mapDriver(err)
	}
	var userVersion int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&userVersion); err != nil {
		return mapDriver(err)
	}
	switch {
	case appID == 0 && userVersion == 0:
		var n int
		q := userObjectCountSQL
		if err := tx.QueryRowContext(ctx, q).Scan(&n); err != nil {
			return mapDriver(err)
		}
		if n > 0 {
			return ErrUnrelatedDatabase
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id = %d", ApplicationID)); err != nil {
			return mapDriver(err)
		}
		if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
			return mapDriver(err)
		}
		epoch, err := newID()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES ('schema_name', ?), ('epoch', ?)`,
			SchemaName, epoch); err != nil {
			return mapDriver(err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
			return mapDriver(err)
		}
		s.epoch = epoch
		if err := s.enforceCap(ctx, tx); err != nil {
			return err
		}
	case uint32(appID) != ApplicationID:
		return ErrUnrelatedDatabase
	default:
		var name string
		if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_name'`).Scan(&name); err != nil || name != SchemaName {
			return ErrUnrelatedDatabase
		}
		if userVersion > SchemaVersion || (userVersion < targetVersion && targetVersion != SchemaVersion && migrate == nil) {
			return ErrMigration
		}
		if userVersion < targetVersion {
			fn := migrate
			if fn == nil {
				fn = builtinMigrate
			}
			if err := fn(tx, userVersion, targetVersion); err != nil {
				if errors.Is(err, ErrMigration) {
					return err
				}
				return ErrMigration
			}
			if targetVersion != SchemaVersion {
				return ErrMigration
			}
			stmt := fmt.Sprintf("PRAGMA user_version = %d", targetVersion)
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return mapDriver(err)
			}
		}
		epoch, err := readEpoch(ctx, tx)
		if err != nil {
			return err
		}
		s.epoch = epoch
	}
	if err := tx.Commit(); err != nil {
		return mapDriver(err)
	}
	committed = true
	if err := lockDownNewSidecars(s.path, before); err != nil {
		return err
	}
	s.ready = true
	s.noteAccepted()
	return nil
}

// refuseUnrelated inspects an existing SQLite file without converting it to WAL.
func refuseUnrelated(path string) error {
	dsn := sqliteFileURI(path, "mode=ro&_query_only=1")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return mapDriver(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := callBusy(func() error { return db.Ping() }); err != nil {
		return mapDriver(err)
	}
	var appID int64
	if err := db.QueryRow("PRAGMA application_id").Scan(&appID); err != nil {
		return mapDriver(err)
	}
	var n int
	q := userObjectCountSQL
	if err := db.QueryRow(q).Scan(&n); err != nil {
		return mapDriver(err)
	}
	if uint32(appID) == ApplicationID {
		return nil
	}
	if appID != 0 || n > 0 {
		return ErrUnrelatedDatabase
	}
	return nil
}

func builtinMigrate(_ *sql.Tx, from, to int) error {
	if from == to {
		return nil
	}
	return ErrMigration
}

// Close releases the database handle.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ready = false
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// Writable reports whether a new intent could be created.
// A store that already holds MaxRows or MaxBytes reports ErrFull, matching Begin.
func (s *Store) Writable() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableLocked(true, true); err != nil {
		return err
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM intents`).Scan(&n); err != nil {
		return mapDriver(err)
	}
	if n >= s.maxRows {
		return ErrFull
	}
	return nil
}

// DisableDispatch stops new intents and claims. Reads, outcome recording,
// compaction, and epoch reset remain.
func (s *Store) DisableDispatch() {
	s.mu.Lock()
	s.dispatch = false
	s.mu.Unlock()
}

func (s *Store) writeTx(ctx context.Context, requireDispatch bool, fn func(*sql.Tx) error) error {
	return s.commitWrite(ctx, requireDispatch, fn, nil)
}

func (s *Store) commitWrite(ctx context.Context, requireDispatch bool, fn func(*sql.Tx) error, afterCommit func()) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Byte cap is enforced in guardBytes after the statement work, so a
	// Begin replay of an existing identity can return the receipt without
	// inserting — the same as the MaxRows path.
	if err := s.writableLocked(requireDispatch, false); err != nil {
		return err
	}
	before, err := existingSidecars(s.path)
	if err != nil {
		return err
	}
	err = callBusy(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return mapDriver(err)
		}
		committed := false
		defer func() {
			if !committed {
				_ = tx.Rollback()
			}
		}()
		if requireDispatch {
			if err := s.requireCurrentEpoch(ctx, tx); err != nil {
				return err
			}
		}
		var changesBefore int64
		if err := tx.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&changesBefore); err != nil {
			return mapDriver(err)
		}
		if err := fn(tx); err != nil {
			return err
		}
		if requireDispatch {
			if err := s.requireCurrentEpoch(ctx, tx); err != nil {
				return err
			}
		}
		if err := s.guardBytes(ctx, tx, changesBefore); err != nil {
			return err
		}
		if s.commitBarrier != nil {
			if err := s.commitBarrier(); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return mapDriver(err)
		}
		committed = true
		if afterCommit != nil {
			afterCommit()
		}
		if afterDurableCommit != nil {
			afterDurableCommit()
		}
		// Automatic checkpoints are off. Truncate here so WAL frames are
		// copied into the main file only after the cap has been checked.
		s.reclaimWAL()
		s.noteAccepted()
		return lockDownNewSidecars(s.path, before)
	})
	if errors.Is(err, ErrFull) {
		s.reclaimWAL()
	}
	return err
}

// requireCurrentEpoch refuses a dispatch write when meta.epoch is not the
// epoch this store opened or last committed. The cache is not replaced, so a
// later Begin or ClaimSending on the same handle fails the same way.
func (s *Store) requireCurrentEpoch(ctx context.Context, q rowQuery) error {
	epoch, err := readEpoch(ctx, q)
	if err != nil {
		return err
	}
	if epoch != s.epoch {
		return ErrStaleEpoch
	}
	return nil
}

func readEpoch(ctx context.Context, q rowQuery) (string, error) {
	var epoch string
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='epoch'`).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && epoch == "") {
		return "", ErrUnrelatedDatabase
	}
	if err != nil {
		return "", mapDriver(err)
	}
	return epoch, nil
}

// guardBytes rejects a write whose committed db+wal+shm would exceed the cap.
// Dirty pages stay out of the WAL until they are flushed. Automatic WAL
// checkpointing is disabled, so Commit cannot copy those frames into the
// main file while the WAL stays allocated. Commit then appends one more
// frame, so the flushed size alone is not the committed size.
func (s *Store) guardBytes(ctx context.Context, tx *sql.Tx, changesBefore int64) error {
	var changes int64
	if err := tx.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&changes); err != nil {
		return mapDriver(err)
	}
	if changes == changesBefore {
		return nil
	}
	return s.enforceCap(ctx, tx)
}

func (s *Store) enforceCap(ctx context.Context, tx *sql.Tx) error {
	var pageSize int64
	if err := tx.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return mapDriver(err)
	}
	before, err := bytesOnDisk(s.path)
	if err != nil {
		return err
	}
	// Do not flush when even one commit frame would exceed the cap.
	reserve := walCommitReserve(pageSize)
	if before+reserve > s.maxBytes {
		return ErrFull
	}
	if err := flushPages(ctx, tx); err != nil {
		return err
	}
	n, err := bytesOnDisk(s.path)
	if err != nil {
		return err
	}
	// cacheflush has already written the dirty pages. Commit appends one
	// more WAL frame (page plus a 24-byte header) which is not in n yet.
	if n+reserve > s.maxBytes {
		return ErrFull
	}
	return nil
}

// walCommitReserve is the page plus 24-byte header Commit appends after flush.
func walCommitReserve(pageSize int64) int64 {
	if pageSize <= 0 {
		pageSize = 4096
	}
	return pageSize + 24
}

func (s *Store) reclaimWAL() {
	if s == nil || s.db == nil {
		return
	}
	wal, err := fileSize(s.path + "-wal")
	if err != nil || wal == 0 {
		return
	}
	n, err := bytesOnDisk(s.path)
	if err != nil {
		return
	}
	// TRUNCATE copies WAL frames into the main file first. A reader
	// snapshot can then block deleting the WAL, so db+wal+shm grows
	// by that second copy. Skip when the copy would exceed the cap
	// unless this connection can take WAL exclusive (BEGIN IMMEDIATE
	// after locking_mode=EXCLUSIVE). BEGIN EXCLUSIVE alone is only
	// IMMEDIATE in WAL mode and still succeeds while readers exist.
	if n+wal > s.maxBytes {
		s.checkpointIfExclusive()
		return
	}
	_, _ = s.db.ExecContext(context.Background(), `PRAGMA wal_checkpoint(TRUNCATE)`)
}

func (s *Store) checkpointIfExclusive() {
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `PRAGMA busy_timeout=0`); err != nil {
		return
	}
	defer func() {
		_, _ = s.db.ExecContext(ctx, `PRAGMA locking_mode=NORMAL`)
		_, _ = s.db.ExecContext(ctx, `PRAGMA busy_timeout=5000`)
	}()
	if _, err := s.db.ExecContext(ctx, `PRAGMA locking_mode=EXCLUSIVE`); err != nil {
		return
	}
	if _, err := s.db.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return
	}
	_, _ = s.db.ExecContext(ctx, `ROLLBACK`)
	_, _ = s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
}

func (s *Store) noteAccepted() {
	n, err := bytesOnDisk(s.path)
	if err != nil {
		return
	}
	s.lastAccepted = n
}

func (s *Store) accountedBytes() (int64, error) {
	n, err := bytesOnDisk(s.path)
	if err != nil {
		return 0, err
	}
	if n >= s.maxBytes {
		s.reclaimWAL()
		n, err = bytesOnDisk(s.path)
		if err != nil {
			return 0, err
		}
	}
	if n >= s.maxBytes && s.lastAccepted > 0 && s.lastAccepted < s.maxBytes {
		// Leftover frames from a rejected flush are still on disk because
		// a reader snapshot blocked TRUNCATE. They are not durable.
		return s.lastAccepted, nil
	}
	return n, nil
}

func (s *Store) readyLocked() error {
	if s == nil || !s.ready || s.db == nil {
		return ErrNotReady
	}
	return nil
}

func (s *Store) writableLocked(requireDispatch, checkBytes bool) error {
	if err := s.readyLocked(); err != nil {
		return err
	}
	if requireDispatch && !s.dispatch {
		return ErrWritesDisabled
	}
	if err := rejectSymlinkComponents(s.path); err != nil {
		return err
	}
	info, err := os.Lstat(parentDir(s.path))
	if err != nil {
		return err
	}
	if err := checkDir(parentDir(s.path), info); err != nil {
		return err
	}
	info, err = os.Lstat(s.path)
	if err != nil {
		return err
	}
	if err := checkFileMode(s.path, info); err != nil {
		return err
	}
	if _, err := existingSidecars(s.path); err != nil {
		return err
	}
	n, err := s.accountedBytes()
	if err != nil {
		return err
	}
	if !checkBytes {
		return nil
	}
	var pageSize int64
	if err := s.db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		return mapDriver(err)
	}
	// Same one-frame reserve as enforceCap: Check/Writable must not
	// pass a store whose next Begin cannot persist a commit frame.
	if n+walCommitReserve(pageSize) > s.maxBytes {
		return ErrFull
	}
	return nil
}

func (s *Store) now() time.Time {
	if s.clock == nil {
		return time.Now().UTC()
	}
	return s.clock().UTC()
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func mapDriver(err error) error {
	if err == nil || isSentinel(err) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		return err
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "stale epoch"):
		return ErrStaleEpoch
	case strings.Contains(msg, "full"):
		return ErrFull
	case strings.Contains(msg, "readonly"), strings.Contains(msg, "read-only"):
		return ErrReadOnly
	case strings.Contains(msg, "not a database"), strings.Contains(msg, "malformed"), strings.Contains(msg, "corrupt"):
		return ErrCorrupt
	default:
		return fmt.Errorf("intent store: database error: %w", err)
	}
}

func isSentinel(err error) bool {
	switch {
	case errors.Is(err, ErrUnrelatedDatabase),
		errors.Is(err, ErrUnsafePermissions),
		errors.Is(err, ErrSymlink),
		errors.Is(err, ErrReadOnly),
		errors.Is(err, ErrCorrupt),
		errors.Is(err, ErrPayloadConflict),
		errors.Is(err, ErrFull),
		errors.Is(err, ErrMigration),
		errors.Is(err, ErrWritesDisabled),
		errors.Is(err, ErrNotFound),
		errors.Is(err, ErrExpired),
		errors.Is(err, ErrAlreadySending),
		errors.Is(err, ErrTerminal),
		errors.Is(err, ErrStaleEpoch),
		errors.Is(err, ErrConfirmation),
		errors.Is(err, ErrDispatchEnabled),
		errors.Is(err, ErrInvalidHash),
		errors.Is(err, ErrInvalidIdentity),
		errors.Is(err, ErrInvalidState),
		errors.Is(err, ErrNotReady),
		errors.Is(err, ErrInvalidOutcome):
		return true
	default:
		return false
	}
}

func isBusy(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "database is locked")
}

func callBusy(fn func() error) error {
	deadline := time.Now().Add(5 * time.Second)
	var err error
	for {
		err = fn()
		if err == nil || !isBusy(err) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func isAbs(path string) bool {
	if filepath.IsAbs(path) {
		return true
	}
	if len(path) >= 3 && path[1] == ':' && (path[2] == '\\' || path[2] == '/') {
		return true
	}
	return uncShare(path) != ""
}

func parentDir(path string) string {
	if share := uncShare(path); share != "" {
		rest := path[len(share):]
		if rest == "" || rest == `\` || rest == `/` {
			return share
		}
		sep := `\`
		if strings.Contains(rest, `/`) && !strings.Contains(rest, `\`) {
			sep = `/`
		}
		i := strings.LastIndex(rest, sep)
		if i <= 0 {
			return share
		}
		return share + rest[:i]
	}
	return filepath.Dir(path)
}

// uncShare returns `\\server\share` or `//server/share` when path is a UNC
// path. filepath.IsAbs accepts these on Windows and rejects the backslash
// form on other GOOS, so the check is explicit.
func uncShare(path string) string {
	switch {
	case strings.HasPrefix(path, `\\`):
		rest := strings.TrimPrefix(path, `\\`)
		server, rest, ok := strings.Cut(rest, `\`)
		if !ok || server == "" {
			return ""
		}
		share, _, _ := strings.Cut(rest, `\`)
		if share == "" {
			return ""
		}
		return `\\` + server + `\` + share
	case strings.HasPrefix(path, `//`):
		rest := strings.TrimPrefix(path, `//`)
		server, rest, ok := strings.Cut(rest, `/`)
		if !ok || server == "" {
			return ""
		}
		share, _, _ := strings.Cut(rest, `/`)
		if share == "" {
			return ""
		}
		return `//` + server + `/` + share
	default:
		return ""
	}
}

// writeDSNQuery disables the 1000-frame automatic checkpoint. Commit would
// otherwise copy WAL pages into the main file while leaving the WAL
// allocated, so db+wal+shm can finish above MaxBytes after the cap check.
const writeDSNQuery = `_txlock=immediate&_busy_timeout=5000&_journal_mode=WAL&_synchronous=FULL&_foreign_keys=on&_pragma=wal_autocheckpoint(0)`

// sqliteFileURI encodes path so `#` and `%` stay inside the file name.
// A raw file: concatenation lets SQLite treat those bytes as URI syntax.
func sqliteFileURI(path, rawQuery string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p, RawQuery: rawQuery}).String()
}

// Literal "sqlite_" prefix. LIKE would treat "_" as a single-character wildcard
// and ignore names such as sqlitex_controller.
const userObjectCountSQL = `SELECT COUNT(*) FROM sqlite_master WHERE substr(name, 1, 7) != 'sqlite_'`
