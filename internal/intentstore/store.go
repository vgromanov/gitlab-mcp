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
}

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
	s, err := prepare(cfg)
	if err != nil {
		return nil, err
	}
	if err := callBusy(func() error { return s.initialize(ctx, targetVersion, migrate) }); err != nil {
		_ = s.db.Close()
		return nil, err
	}
	return s, nil
}

func prepare(cfg Config) (*Store, error) {
	path := cfg.Path
	if !isAbs(path) || strings.ContainsAny(path, "?\x00") {
		return nil, errors.New("intent store: path must be absolute")
	}
	parent := parentDir(path)
	info, err := os.Lstat(parent)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return nil, err
		}
		info, err = os.Lstat(parent)
		if err != nil {
			return nil, err
		}
	}
	if err := checkDir(info); err != nil {
		return nil, err
	}
	info, err = os.Lstat(path)
	switch {
	case err == nil:
		if err := checkFileMode(info); err != nil {
			return nil, err
		}
		if err := classifyHeader(path); err != nil {
			return nil, err
		}
		if info.Size() > 0 {
			if err := refuseUnrelated(path); err != nil {
				return nil, err
			}
		}
	case os.IsNotExist(err):
		if err := createExclusive(path); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	before, err := existingSidecars(path)
	if err != nil {
		return nil, err
	}
	dsn := sqliteFileURI(path, "_txlock=immediate&_busy_timeout=5000&_journal_mode=WAL&_synchronous=FULL&_foreign_keys=on")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, mapDriver(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := callBusy(func() error { return db.Ping() }); err != nil {
		_ = db.Close()
		return nil, mapDriver(err)
	}
	if err := lockDownNewSidecars(path, before); err != nil {
		_ = db.Close()
		return nil, err
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
	}, nil
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
		var epoch string
		if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='epoch'`).Scan(&epoch); err != nil || epoch == "" {
			return ErrUnrelatedDatabase
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

// Writable reports whether a new dispatch could be attempted.
func (s *Store) Writable() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writableLocked(true)
}

// DisableDispatch stops new intents and claims. Reads and epoch reset remain.
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
	if err := s.writableLocked(requireDispatch); err != nil {
		return err
	}
	before, err := existingSidecars(s.path)
	if err != nil {
		return err
	}
	return callBusy(func() error {
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
		prior := s.epoch
		if err := s.reloadEpoch(ctx, tx); err != nil {
			return err
		}
		if s.epoch != prior {
			return ErrStaleEpoch
		}
		var changesBefore int64
		if err := tx.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&changesBefore); err != nil {
			return mapDriver(err)
		}
		if err := fn(tx); err != nil {
			return err
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
		return lockDownNewSidecars(s.path, before)
	})
}

// reloadEpoch replaces the cached epoch with the committed meta value.
func (s *Store) reloadEpoch(ctx context.Context, q rowQuery) error {
	epoch, err := readEpoch(ctx, q)
	if err != nil {
		return err
	}
	s.epoch = epoch
	return nil
}

func readEpoch(ctx context.Context, q rowQuery) (string, error) {
	var epoch string
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='epoch'`).Scan(&epoch)
	if err != nil || epoch == "" {
		return "", ErrUnrelatedDatabase
	}
	return epoch, nil
}

// guardBytes rejects a write whose commit would grow db+wal+shm past the cap.
// WAL frames are not on disk until commit, so the check uses the transaction's
// page count plus one frame when an in-place update does not add a page.
func (s *Store) guardBytes(ctx context.Context, tx *sql.Tx, changesBefore int64) error {
	var pages, pageSize, changes int64
	q := `SELECT pc.page_count, ps.page_size, total_changes() FROM pragma_page_count() pc, pragma_page_size() ps`
	if err := tx.QueryRowContext(ctx, q).Scan(&pages, &pageSize, &changes); err != nil {
		return mapDriver(err)
	}
	if changes == changesBefore || pageSize <= 0 {
		return nil
	}
	logical := pages * pageSize
	dbSize, err := fileSize(s.path)
	if err != nil {
		return err
	}
	walSize, err := fileSize(s.path + "-wal")
	if err != nil {
		return err
	}
	shmSize, err := fileSize(s.path + "-shm")
	if err != nil {
		return err
	}
	newPages := int64(1)
	if logical > dbSize {
		grew := (logical - dbSize + pageSize - 1) / pageSize
		if grew > newPages {
			newPages = grew
		}
	}
	extra := newPages * (pageSize + 24)
	if walSize == 0 {
		extra += 32
	}
	if dbSize+walSize+shmSize+extra > s.maxBytes {
		return ErrFull
	}
	return nil
}

func (s *Store) readyLocked() error {
	if s == nil || !s.ready || s.db == nil {
		return ErrNotReady
	}
	return nil
}

func (s *Store) writableLocked(requireDispatch bool) error {
	if err := s.readyLocked(); err != nil {
		return err
	}
	if requireDispatch && !s.dispatch {
		return ErrWritesDisabled
	}
	info, err := os.Lstat(parentDir(s.path))
	if err != nil {
		return err
	}
	if err := checkDir(info); err != nil {
		return err
	}
	info, err = os.Lstat(s.path)
	if err != nil {
		return err
	}
	if err := checkFileMode(info); err != nil {
		return err
	}
	if _, err := existingSidecars(s.path); err != nil {
		return err
	}
	n, err := bytesOnDisk(s.path)
	if err != nil {
		return err
	}
	if n >= s.maxBytes {
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
	return strings.HasPrefix(path, "/") || (len(path) >= 3 && path[1] == ':' && (path[2] == '\\' || path[2] == '/'))
}

func parentDir(path string) string {
	return filepath.Dir(path)
}

// sqliteFileURI encodes path so `#` and `%` stay inside the file name.
// A raw file: concatenation lets SQLite treat those bytes as URI syntax.
func sqliteFileURI(path, rawQuery string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p, RawQuery: rawQuery}).String()
}

const userObjectCountSQL = `SELECT COUNT(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'`
