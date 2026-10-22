// Package store owns the daemon's single SQLite database (state/state.db).
//
// One package opens the file, sets its pragmas, applies the ordered schema
// migrations and records import receipts. Domain packages (internal/chat
// first) keep their own interfaces and talk to the database only through the
// Tx handed to Write/Read, so every statement shares one connection, one
// locking discipline and one retry policy.
//
// Design (docs: .dev/reports/sqlite-state-inventory-2026-10-16.md §3-§5):
//   - Driver: modernc.org/sqlite, pure Go, so the Mac cross-compiles the
//     Windows daemon with CGO_ENABLED=0 and nothing JITs at runtime.
//   - Exactly one process writes the database. The connection runs WAL in
//     EXCLUSIVE locking mode: the wal-index lives in heap memory (no -shm
//     file for an endpoint scanner to map), and a second daemon fails fast
//     with ErrLocked instead of interleaving writes.
//   - synchronous=NORMAL: a commit survives a process crash; a power loss may
//     drop the newest commits but never tears one. Close checkpoints with
//     TRUNCATE so a cleanly stopped daemon leaves one self-contained file.
//   - PRAGMA user_version mirrors schema_migrations; a database written by a
//     newer Workass is refused rather than guessed at.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// FileName is the database file inside the daemon state directory.
const FileName = "state.db"

const (
	defaultBusyTimeout      = 5 * time.Second
	defaultJournalSizeLimit = 32 << 20
	// defaultCacheKiB bounds the page cache per connection. Large chat rows
	// stream through it; nothing here needs the whole history resident.
	defaultCacheKiB = 8 << 10
	maxBusyRetries  = 4
)

var (
	// ErrLocked means another process holds the database (normally a second
	// daemon on the same state directory).
	ErrLocked = errors.New("state database is locked by another process")
	// ErrSchemaTooNew means the file was migrated by a newer Workass build.
	ErrSchemaTooNew = errors.New("state database was written by a newer Workass")
	// ErrSchemaMismatch means the recorded migration history disagrees with
	// this build's ordered migration list.
	ErrSchemaMismatch = errors.New("state database migration history does not match this build")
	// ErrClosed is returned after Close.
	ErrClosed = errors.New("state database is closed")
)

// Synchronous is the PRAGMA synchronous level of the connection.
type Synchronous string

const (
	SynchronousNormal Synchronous = "NORMAL"
	SynchronousFull   Synchronous = "FULL"
)

// Options configures Open. Path is required; zero values elsewhere select
// the production defaults.
type Options struct {
	Path string
	// BusyTimeout is how long a statement waits on a lock held elsewhere
	// before SQLITE_BUSY. Default 5s.
	BusyTimeout time.Duration
	// Synchronous defaults to NORMAL (WAL: durable across process crashes).
	Synchronous Synchronous
	// BinaryVersion is recorded in schema_migrations and receipts.
	BinaryVersion string
	// Migrations overrides the embedded schema; tests only.
	Migrations []Migration
	// BackupDir, when set, receives a VACUUM INTO snapshot before pending
	// migrations are applied to a database that already has a schema.
	BackupDir string
	// QuickCheck runs PRAGMA quick_check after open and fails on any finding.
	QuickCheck bool
}

// DB is the open state database. It is safe for concurrent use; statements
// serialize on the single connection.
type DB struct {
	sql           *sql.DB
	path          string
	binaryVersion string
	version       int

	closeOnce sync.Once
	closeErr  error
	mu        sync.RWMutex
	closed    bool
}

// Tx is the only handle domain code receives. It wraps *sql.Tx so callers
// cannot reach the pool (a nested pool call inside a transaction would wait
// forever on the single connection).
type Tx struct {
	tx *sql.Tx
}

func (t *Tx) Exec(query string, args ...any) (sql.Result, error) {
	return t.tx.Exec(query, args...)
}

func (t *Tx) Query(query string, args ...any) (*sql.Rows, error) {
	return t.tx.Query(query, args...)
}

func (t *Tx) QueryRow(query string, args ...any) *sql.Row {
	return t.tx.QueryRow(query, args...)
}

// Prepare returns a statement bound to this transaction; the caller closes it.
func (t *Tx) Prepare(query string) (*sql.Stmt, error) {
	return t.tx.Prepare(query)
}

// Open opens (creating if needed) the database at opts.Path, applies the
// connection pragmas, takes the exclusive lock, verifies the schema history
// and applies pending migrations.
func Open(opts Options) (*DB, error) {
	path := strings.TrimSpace(opts.Path)
	if path == "" {
		return nil, errors.New("state database path is empty")
	}
	if strings.ContainsRune(path, '?') {
		// The driver splits its DSN at '?'; a literal one would be misread.
		return nil, fmt.Errorf("state database path %q contains '?'", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Owner-only, like every other state file. SQLite gives the -wal file
	// the database file's permissions, so setting them here covers both.
	file, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	busy := opts.BusyTimeout
	if busy <= 0 {
		busy = defaultBusyTimeout
	}
	syncMode := opts.Synchronous
	if syncMode == "" {
		syncMode = SynchronousNormal
	}
	if syncMode != SynchronousNormal && syncMode != SynchronousFull {
		return nil, fmt.Errorf("unsupported synchronous mode %q", syncMode)
	}
	migrations := opts.Migrations
	if migrations == nil {
		var err error
		migrations, err = EmbeddedMigrations()
		if err != nil {
			return nil, err
		}
	}
	if err := validateMigrationList(migrations); err != nil {
		return nil, err
	}

	// A plain path (no "file:" prefix) reaches sqlite3_open_v2 verbatim, so
	// spaces ("Application Support") and Windows drive letters need no URI
	// escaping. _txlock=immediate makes every transaction take the write lock
	// at BEGIN, which busy_timeout can wait out, instead of upgrading later.
	base, err := sqlite.NewConnector(path + "?_txlock=immediate")
	if err != nil {
		return nil, err
	}
	connector := &pragmaConnector{base: base, pragmas: connectionPragmas(busy, syncMode)}
	pool := sql.OpenDB(connector)
	// One connection: EXCLUSIVE locking mode admits no second one, and one
	// writer is the design anyway. It never idles out, so the lock and the
	// heap wal-index live as long as the daemon.
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	pool.SetConnMaxLifetime(0)
	pool.SetConnMaxIdleTime(0)

	db := &DB{sql: pool, path: path, binaryVersion: strings.TrimSpace(opts.BinaryVersion)}
	// Refuse a newer schema before the first write transaction, so a file
	// from a newer build is left byte-for-byte as it was.
	if err := refuseNewerSchema(pool, len(migrations)); err != nil {
		_ = pool.Close()
		return nil, err
	}
	if err := db.acquire(); err != nil {
		_ = pool.Close()
		return nil, err
	}
	if opts.QuickCheck {
		if err := db.QuickCheck(); err != nil {
			_ = pool.Close()
			return nil, err
		}
	}
	version, err := migrate(db, migrations, opts.BackupDir)
	if err != nil {
		_ = pool.Close()
		return nil, err
	}
	db.version = version
	return db, nil
}

func connectionPragmas(busy time.Duration, syncMode Synchronous) []string {
	// Order matters and is why these are not DSN _pragma values (the driver
	// sorts those): locking_mode must precede every read of the file, so that
	// the wal-index is created in heap memory and no -shm file ever exists.
	// On a reopened WAL database even the page_count check of the auto_vacuum
	// step is such a read: before EXCLUSIVE it created a -shm file.
	return []string{
		fmt.Sprintf("PRAGMA busy_timeout = %d", busy.Milliseconds()),
		"PRAGMA locking_mode = EXCLUSIVE",
		newFileAutoVacuum,
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = " + string(syncMode),
		"PRAGMA foreign_keys = ON",
		fmt.Sprintf("PRAGMA journal_size_limit = %d", defaultJournalSizeLimit),
		"PRAGMA checkpoint_fullfsync = ON",
		fmt.Sprintf("PRAGMA cache_size = -%d", defaultCacheKiB),
		"PRAGMA mmap_size = 0",
	}
}

// newFileAutoVacuum is applied only to a file with no pages yet: auto_vacuum
// is fixed once page 1 exists, and re-issuing it on an existing file dirties
// the header (a write, even when refusing a newer schema).
const newFileAutoVacuum = "PRAGMA auto_vacuum = INCREMENTAL"

// pragmaConnector applies the connection pragmas in a fixed order to every
// connection database/sql opens, including a replacement after a bad conn.
type pragmaConnector struct {
	base    driver.Connector
	pragmas []string
}

func (c *pragmaConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	execer, ok := conn.(driver.ExecerContext)
	if !ok {
		_ = conn.Close()
		return nil, errors.New("sqlite driver connection cannot execute pragmas")
	}
	for _, pragma := range c.pragmas {
		if pragma == newFileAutoVacuum {
			pages, err := queryInt(ctx, conn, "PRAGMA page_count")
			if err != nil {
				_ = conn.Close()
				return nil, classify(err)
			}
			if pages != 0 {
				continue
			}
		}
		if _, err := execer.ExecContext(ctx, pragma, nil); err != nil {
			_ = conn.Close()
			return nil, classify(fmt.Errorf("%s: %w", pragma, err))
		}
	}
	return conn, nil
}

func (c *pragmaConnector) Driver() driver.Driver { return c.base.Driver() }

func queryInt(ctx context.Context, conn driver.Conn, query string) (int64, error) {
	queryer, ok := conn.(driver.QueryerContext)
	if !ok {
		return 0, errors.New("sqlite driver connection cannot query")
	}
	rows, err := queryer.QueryContext(ctx, query, nil)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	values := make([]driver.Value, len(rows.Columns()))
	if err := rows.Next(values); err != nil {
		return 0, err
	}
	value, ok := values[0].(int64)
	if !ok {
		return 0, fmt.Errorf("%s returned %T", query, values[0])
	}
	return value, nil
}

func refuseNewerSchema(pool *sql.DB, known int) error {
	var current int
	if err := pool.QueryRow("PRAGMA user_version").Scan(&current); err != nil {
		return classify(err)
	}
	if current > known {
		// Fail closed: this build cannot know what the newer schema means.
		return fmt.Errorf("%w: schema version %d, this build knows %d; update Workass or restore a backup", ErrSchemaTooNew, current, known)
	}
	return nil
}

// acquire forces the first write so the EXCLUSIVE lock is taken at Open, and
// verifies the journal mode actually became WAL.
func (db *DB) acquire() error {
	var mode string
	if err := db.sql.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		return classify(err)
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("state database journal mode is %q, want wal", mode)
	}
	return db.Write(context.Background(), func(tx *Tx) error {
		// An empty write transaction still takes the write lock, which
		// locking_mode=EXCLUSIVE then keeps.
		_, err := tx.Exec("PRAGMA user_version")
		return err
	})
}

// Path is the database file path.
func (db *DB) Path() string { return db.path }

// SchemaVersion is the user_version after migrations.
func (db *DB) SchemaVersion() int { return db.version }

// BinaryVersion is the build identity recorded in receipts.
func (db *DB) BinaryVersion() string { return db.binaryVersion }

// Write runs fn in one IMMEDIATE transaction. SQLITE_BUSY (another process
// briefly holding the file, e.g. a scanner or a backup) is retried with
// backoff; fn must therefore be safe to run again from the start.
func (db *DB) Write(ctx context.Context, fn func(*Tx) error) error {
	return db.run(ctx, fn)
}

// Read runs fn in a transaction that only reads. It shares the single
// connection, so it observes every committed write.
func (db *DB) Read(ctx context.Context, fn func(*Tx) error) error {
	return db.run(ctx, fn)
}

func (db *DB) run(ctx context.Context, fn func(*Tx) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return ErrClosed
	}
	var err error
	for attempt := 0; ; attempt++ {
		err = db.runOnce(ctx, fn)
		if err == nil || !IsBusy(err) || attempt >= maxBusyRetries {
			return classify(err)
		}
		delay := time.Duration(25<<attempt) * time.Millisecond
		select {
		case <-ctx.Done():
			return errors.Join(classify(err), ctx.Err())
		case <-time.After(delay):
		}
	}
}

func (db *DB) runOnce(ctx context.Context, fn func(*Tx) error) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// A panic inside fn must not leave the only connection inside an open
	// transaction: every later caller would wait on it forever.
	committed := false
	defer func() {
		if !committed {
			if recovered := recover(); recovered != nil {
				_ = tx.Rollback()
				panic(recovered)
			}
		}
	}()
	if err := fn(&Tx{tx: tx}); err != nil {
		return errors.Join(err, ignoreDone(tx.Rollback()))
	}
	committed = true
	if err := tx.Commit(); err != nil {
		return errors.Join(err, ignoreDone(tx.Rollback()))
	}
	return nil
}

func ignoreDone(err error) error {
	if errors.Is(err, sql.ErrTxDone) {
		return nil
	}
	return err
}

// QuickCheck runs PRAGMA quick_check and reports any finding as an error.
func (db *DB) QuickCheck() error {
	rows, err := db.sql.Query("PRAGMA quick_check")
	if err != nil {
		return classify(err)
	}
	defer rows.Close()
	var findings []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return err
		}
		if line != "ok" {
			findings = append(findings, line)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(findings) > 0 {
		return fmt.Errorf("state database quick_check: %s", strings.Join(findings, "; "))
	}
	return nil
}

// Backup writes a consistent single-file snapshot (no WAL) to dst with
// VACUUM INTO. dst must not exist.
func (db *DB) Backup(dst string) error {
	dst = strings.TrimSpace(dst)
	if dst == "" {
		return errors.New("backup path is empty")
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("backup %q already exists", dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return ErrClosed
	}
	if _, err := db.sql.Exec("VACUUM INTO ?", dst); err != nil {
		return classify(err)
	}
	return os.Chmod(dst, 0o600)
}

// Close checkpoints the WAL into the main file, truncates it and closes the
// connection, releasing the exclusive lock.
func (db *DB) Close() error {
	if db == nil {
		return nil
	}
	db.closeOnce.Do(func() {
		db.mu.Lock()
		defer db.mu.Unlock()
		db.closed = true
		_, checkpointErr := db.sql.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
		db.closeErr = errors.Join(classify(checkpointErr), db.sql.Close())
	})
	return db.closeErr
}

// Code returns the primary SQLite result code of err, or 0.
func Code(err error) int {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code() & 0xff
	}
	return 0
}

// IsBusy reports SQLITE_BUSY / SQLITE_LOCKED.
func IsBusy(err error) bool {
	if errors.Is(err, ErrLocked) {
		return true
	}
	switch Code(err) {
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
		return true
	}
	return false
}

// IsConstraint reports a constraint violation (UNIQUE, CHECK, FK...).
func IsConstraint(err error) bool {
	return Code(err) == sqlite3.SQLITE_CONSTRAINT
}

// IsFull reports SQLITE_FULL: the disk (or the database's page limit) is
// full.
func IsFull(err error) bool {
	return Code(err) == sqlite3.SQLITE_FULL
}

func classify(err error) error {
	if err == nil || errors.Is(err, ErrLocked) {
		return err
	}
	switch Code(err) {
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
		return fmt.Errorf("%w (is another Workass daemon using this state directory?): %v", ErrLocked, err)
	}
	return err
}
