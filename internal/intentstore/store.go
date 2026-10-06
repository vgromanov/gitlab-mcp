package intentstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	bolt "go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"
)

// lockTimeout bounds how long one operation waits for another handle or
// process to release the file lock.
const lockTimeout = 5 * time.Second

// lockSlice is how long one bbolt open waits for the lock before the context
// is checked again.
const lockSlice = 100 * time.Millisecond

// Config selects the database file and local limits.
type Config struct {
	Path      string
	Now       func() time.Time
	MaxRows   int
	MaxBytes  int64
	Retention time.Duration
}

// Store is a process-local handle on the intent database. It keeps no file
// open between calls: each operation opens the file, takes the advisory file
// lock, runs one transaction, and closes it. Handles and processes that
// share a path therefore serialize on the lock and always see committed data.
type Store struct {
	mu        sync.Mutex
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

// beforeCreate runs after a missing database file is observed and before the
// exclusive create. Tests use it to simulate another process winning the race.
var beforeCreate func(path string)

// afterPrepare runs after the file is validated and before initialize.
// Tests use it to let another handle finish initialization, then fail.
var afterPrepare func(*Store) error

// beforeDBOpen runs after the descriptor walk and before the database open.
// Tests use it to swap an ancestor for a symlink in that gap.
var beforeDBOpen func(path string)

// afterReadEpoch runs after meta.epoch is read inside a Get snapshot
// and before the receipt is loaded. Tests use it to race a reset.
var afterReadEpoch func()

type migrateFunc func(tx *bolt.Tx, from, to int) error

// errNoCommit rolls a write transaction back without reporting a failure.
var errNoCommit = errors.New("intent store: nothing to commit")

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
	if afterPrepare != nil {
		if err := afterPrepare(s); err != nil {
			return nil, err
		}
	}
	if err := s.initialize(ctx, targetVersion, migrate); err != nil {
		return nil, err
	}
	return s, nil
}

func prepare(cfg Config) (*Store, error) {
	path := cfg.Path
	if !isAbs(path) || strings.ContainsAny(path, "?\x00") {
		return nil, errors.New("intent store: path must be absolute")
	}
	if err := rejectDotDot(path); err != nil {
		return nil, err
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return nil, err
	}
	parent := parentDir(path)
	info, err := os.Lstat(parent)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		if err := makeParents(parent); err != nil {
			return nil, err
		}
		info, err = os.Lstat(parent)
		if err != nil {
			return nil, err
		}
	}
	if err := checkDir(parent, info); err != nil {
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
	info, err = os.Lstat(path)
	switch {
	case err == nil:
		if err := validateExistingFile(path, info); err != nil {
			return nil, err
		}
	case os.IsNotExist(err):
		if maxBytes < minContainerBytes {
			return nil, ErrFull
		}
		if beforeCreate != nil {
			beforeCreate(path)
		}
		err := createExclusive(path)
		if os.IsExist(err) {
			info, err = os.Lstat(path)
			if err != nil {
				return nil, err
			}
			if err := validateExistingFile(path, info); err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		} else if err := establishPrivate(path, false); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	return &Store{
		path:      path,
		dispatch:  true,
		maxRows:   maxRows,
		maxBytes:  maxBytes,
		retention: retention,
		clock:     clock,
	}, nil
}

func validateExistingFile(path string, info os.FileInfo) error {
	if err := checkFileMode(path, info); err != nil {
		return err
	}
	return classifyHeader(path)
}

// openChecked opens the database file after the caller's path checks and
// verifies that the opened name is still the file those checks saw. It never
// creates the file: creation goes through createExclusive, which keeps the
// private mode and the no-follow walk.
func openChecked(ctx context.Context, path string, write bool, maxBytes int64) (*bolt.DB, error) {
	before, err := identifyFile(path)
	if err != nil {
		return nil, err
	}
	if beforeDBOpen != nil {
		beforeDBOpen(path)
	}
	opts := &bolt.Options{
		ReadOnly:     !write,
		PageSize:     pageSize,
		NoStatistics: true,
		OpenFile: func(name string, flag int, mode os.FileMode) (*os.File, error) {
			return os.OpenFile(name, flag&^os.O_CREATE, mode)
		},
	}
	if write && maxBytes > 0 {
		opts.MaxSize = int(min(maxBytes, math.MaxInt))
	}
	db, err := openWithLock(ctx, path, opts)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if symErr := rejectSymlinkComponents(path); symErr != nil {
			return nil, symErr
		}
		return nil, mapOpen(err)
	}
	db.AllocSize = allocSize
	opened, err := identifyFile(path)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if !sameFile(before, opened) {
		_ = db.Close()
		return nil, ErrSymlink
	}
	if err := rejectSymlinkComponents(path); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// openWithLock opens the database and acquires its file lock in short
// slices so the caller's context bounds the wait. bbolt can only wait for a
// lock for a fixed time, so the wait is repeated until the context is done
// (returning its error) or lockTimeout has passed (returning bbolt's timeout).
func openWithLock(ctx context.Context, path string, opts *bolt.Options) (*bolt.DB, error) {
	deadline := time.Now().Add(lockTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		slice := min(lockSlice, time.Until(deadline))
		if d, ok := ctx.Deadline(); ok {
			slice = min(slice, time.Until(d))
		}
		if slice <= 0 {
			slice = time.Millisecond
		}
		o := *opts
		o.Timeout = slice
		db, err := bolt.Open(path, 0o600, &o)
		if !errors.Is(err, berrors.ErrTimeout) {
			return db, err
		}
		if !time.Now().Before(deadline) {
			return nil, err
		}
	}
}

// schemaState is what inspect found in an open database.
type schemaState struct {
	fresh   bool
	version int
	epoch   string
}

// inspect classifies the database without changing it. A file whose only
// content is the empty container is fresh. Any other layout than this
// store's meta bucket is unrelated, so a foreign bbolt file is never adopted.
func inspect(tx *bolt.Tx) (schemaState, error) {
	meta := tx.Bucket(bucketMeta)
	if meta == nil {
		fresh := true
		_ = tx.ForEach(func([]byte, *bolt.Bucket) error {
			fresh = false
			return nil
		})
		if fresh {
			return schemaState{fresh: true}, nil
		}
		return schemaState{}, ErrUnrelatedDatabase
	}
	if string(meta.Get(keySchemaName)) != SchemaName {
		return schemaState{}, ErrUnrelatedDatabase
	}
	epoch := string(meta.Get(keyEpoch))
	if epoch == "" {
		return schemaState{}, ErrUnrelatedDatabase
	}
	version, err := strconv.Atoi(string(meta.Get(keySchemaVersion)))
	if err != nil {
		return schemaState{}, ErrUnrelatedDatabase
	}
	if tx.Bucket(bucketIntents) == nil || tx.Bucket(bucketIdentity) == nil {
		return schemaState{}, ErrCorrupt
	}
	return schemaState{version: version, epoch: epoch}, nil
}

func (s *Store) initialize(ctx context.Context, targetVersion int, migrate migrateFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	size, err := fileSize(s.path)
	if err != nil {
		return err
	}
	if size > 0 {
		done, err := s.adoptExisting(ctx, targetVersion)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
	return s.initializeWrite(ctx, targetVersion, migrate)
}

// adoptExisting classifies a non-empty file read-only. It reports done when
// the file is this store at the target version, so a healthy store is opened
// without writing. Foreign files are refused here and never opened for write.
func (s *Store) adoptExisting(ctx context.Context, targetVersion int) (bool, error) {
	db, err := openChecked(ctx, s.path, false, 0)
	if err != nil {
		return false, err
	}
	var st schemaState
	err = db.View(func(tx *bolt.Tx) error {
		var err error
		st, err = inspect(tx)
		return err
	})
	_ = db.Close()
	if err != nil {
		return false, mapBolt(err)
	}
	if st.fresh {
		return false, nil
	}
	if st.version > SchemaVersion {
		return false, ErrMigration
	}
	if st.version != targetVersion {
		return false, nil
	}
	if err := probeWritable(s.path); err != nil {
		return false, err
	}
	s.epoch = st.epoch
	s.ready = true
	return true, nil
}

// probeWritable fails with ErrReadOnly when the file cannot be opened for
// write, matching the first durable write a handle would attempt.
func probeWritable(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return mapOpen(err)
	}
	return f.Close()
}

func (s *Store) initializeWrite(ctx context.Context, targetVersion int, migrate migrateFunc) error {
	size, err := fileSize(s.path)
	if err != nil {
		return err
	}
	if size == 0 && s.maxBytes < minContainerBytes {
		_ = os.Remove(s.path)
		return ErrFull
	}
	db, err := openChecked(ctx, s.path, true, s.maxBytes)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	var epoch string
	err = db.Update(func(tx *bolt.Tx) error {
		st, err := inspect(tx)
		if err != nil {
			return err
		}
		if st.fresh {
			epoch, err = createSchema(tx)
			return err
		}
		epoch = st.epoch
		return migrateSchema(tx, st.version, targetVersion, migrate)
	})
	if err != nil {
		return mapBolt(err)
	}
	s.epoch = epoch
	s.ready = true
	return nil
}

func createSchema(tx *bolt.Tx) (string, error) {
	meta, err := tx.CreateBucket(bucketMeta)
	if err != nil {
		return "", err
	}
	if _, err := tx.CreateBucket(bucketIntents); err != nil {
		return "", err
	}
	if _, err := tx.CreateBucket(bucketIdentity); err != nil {
		return "", err
	}
	epoch, err := newID()
	if err != nil {
		return "", err
	}
	for _, kv := range [][2][]byte{
		{keySchemaName, []byte(SchemaName)},
		{keyEpoch, []byte(epoch)},
		{keySchemaVersion, []byte(strconv.Itoa(SchemaVersion))},
	} {
		if err := meta.Put(kv[0], kv[1]); err != nil {
			return "", err
		}
	}
	return epoch, nil
}

func migrateSchema(tx *bolt.Tx, version, targetVersion int, migrate migrateFunc) error {
	if version > SchemaVersion || (version < targetVersion && targetVersion != SchemaVersion && migrate == nil) {
		return ErrMigration
	}
	if version >= targetVersion {
		return nil
	}
	fn := migrate
	if fn == nil {
		fn = builtinMigrate
	}
	if err := fn(tx, version, targetVersion); err != nil {
		if errors.Is(err, ErrMigration) {
			return err
		}
		return ErrMigration
	}
	if targetVersion != SchemaVersion {
		return ErrMigration
	}
	return tx.Bucket(bucketMeta).Put(keySchemaVersion, []byte(strconv.Itoa(targetVersion)))
}

func builtinMigrate(_ *bolt.Tx, from, to int) error {
	if from == to {
		return nil
	}
	return ErrMigration
}

// muLock serializes operations on one handle. Waiting respects the caller's
// context so a short deadline is not blocked behind another call on the same
// store that is waiting for the file lock.
func (s *Store) muLock(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.mu.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(lockSlice):
		}
	}
}

func (s *Store) muUnlock() {
	s.mu.Unlock()
}

// Close marks the handle unusable. No file stays open between operations.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ready = false
	return nil
}

// Writable reports whether a new intent could be created.
// A store that already holds MaxRows or MaxBytes reports ErrFull, matching Begin.
func (s *Store) Writable() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableLocked(true); err != nil {
		return err
	}
	if err := s.overByteCap(); err != nil {
		return err
	}
	return s.view(context.Background(), func(tx *bolt.Tx) error {
		if rowCount(tx) >= s.maxRows {
			return ErrFull
		}
		return nil
	})
}

// DisableDispatch stops new intents and claims. Reads, outcome recording,
// compaction, and epoch reset remain.
func (s *Store) DisableDispatch() {
	s.mu.Lock()
	s.dispatch = false
	s.mu.Unlock()
}

func (s *Store) writeTx(ctx context.Context, requireDispatch bool, fn func(*bolt.Tx) error) error {
	return s.commitWrite(ctx, requireDispatch, fn, nil)
}

// commitWrite runs fn in one exclusive transaction. fn returning errNoCommit
// rolls back and reports success. The byte cap is enforced by the database
// itself: a write that needs the file to grow past MaxBytes fails before
// anything is committed, so a rejected write is never durable.
func (s *Store) commitWrite(ctx context.Context, requireDispatch bool, fn func(*bolt.Tx) error, afterCommit func()) error {
	if err := s.muLock(ctx); err != nil {
		return err
	}
	defer s.muUnlock()
	if err := s.writableLocked(requireDispatch); err != nil {
		return err
	}
	db, err := openChecked(ctx, s.path, true, s.maxBytes)
	if err != nil {
		return err
	}
	defer db.Close()
	err = db.Update(func(tx *bolt.Tx) error {
		if requireDispatch {
			if err := s.requireCurrentEpoch(tx); err != nil {
				return err
			}
		}
		if err := fn(tx); err != nil {
			return err
		}
		if s.commitBarrier != nil {
			if err := s.commitBarrier(); err != nil {
				return err
			}
		}
		return ctx.Err()
	})
	if errors.Is(err, errNoCommit) {
		return nil
	}
	if err != nil {
		return mapBolt(err)
	}
	if afterCommit != nil {
		afterCommit()
	}
	return nil
}

// view runs fn in one read transaction. The shared file lock is held until
// fn returns, so a reset in another handle cannot commit mid-read and the
// epoch and the row come from the same state. The caller holds s.mu.
func (s *Store) view(ctx context.Context, fn func(*bolt.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.readyLocked(); err != nil {
		return err
	}
	if err := s.guardPath(); err != nil {
		return err
	}
	db, err := openChecked(ctx, s.path, false, 0)
	if err != nil {
		return err
	}
	defer db.Close()
	return mapBolt(db.View(fn))
}

// requireCurrentEpoch refuses a dispatch write when meta.epoch is not the
// epoch this store opened or last committed. The cache is not replaced, so a
// later Begin or ClaimSending on the same handle fails the same way.
func (s *Store) requireCurrentEpoch(tx *bolt.Tx) error {
	epoch, err := readEpoch(tx)
	if err != nil {
		return err
	}
	if epoch != s.epoch {
		return ErrStaleEpoch
	}
	return nil
}

func readEpoch(tx *bolt.Tx) (string, error) {
	meta := tx.Bucket(bucketMeta)
	if meta == nil {
		return "", ErrUnrelatedDatabase
	}
	epoch := string(meta.Get(keyEpoch))
	if epoch == "" {
		return "", ErrUnrelatedDatabase
	}
	return epoch, nil
}

func rowCount(tx *bolt.Tx) int {
	b := tx.Bucket(bucketIntents)
	if b == nil {
		return 0
	}
	return b.Stats().KeyN
}

func (s *Store) readyLocked() error {
	if s == nil || !s.ready {
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
	return s.guardPath()
}

// guardPath re-runs the path, directory, and file checks before each
// operation opens the file.
func (s *Store) guardPath() error {
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
	return checkFileMode(s.path, info)
}

// overByteCap reports ErrFull when the data file is already larger than
// MaxBytes, for example after the cap was lowered. A file below the cap is
// bounded by the database's own size limit when it grows.
func (s *Store) overByteCap() error {
	n, err := fileSize(s.path)
	if err != nil {
		return err
	}
	if n > s.maxBytes {
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

// mapOpen translates a failure to open the file. Errors that mean the bytes
// are not a usable container become ErrCorrupt without echoing file content.
func mapOpen(err error) error {
	switch {
	case err == nil || isSentinel(err):
		return err
	case errors.Is(err, berrors.ErrInvalid),
		errors.Is(err, berrors.ErrVersionMismatch),
		errors.Is(err, berrors.ErrChecksum),
		strings.Contains(err.Error(), "file size too small"):
		return ErrCorrupt
	default:
		return mapBolt(err)
	}
}

func mapBolt(err error) error {
	switch {
	case err == nil || isSentinel(err):
		return err
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, berrors.ErrMaxSizeReached):
		return ErrFull
	case errors.Is(err, berrors.ErrDatabaseReadOnly),
		errors.Is(err, fs.ErrPermission),
		errors.Is(err, syscall.EROFS):
		return ErrReadOnly
	case errors.Is(err, berrors.ErrInvalid),
		errors.Is(err, berrors.ErrVersionMismatch),
		errors.Is(err, berrors.ErrChecksum):
		return ErrCorrupt
	case errors.Is(err, berrors.ErrTimeout):
		return fmt.Errorf("intent store: database is locked by another handle: %w", err)
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
		errors.Is(err, ErrInvalidOutcome),
		errors.Is(err, errNoCommit):
		return true
	default:
		return false
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
