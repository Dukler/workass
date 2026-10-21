package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func openTest(t *testing.T, path string, migrations []Migration) *DB {
	t.Helper()
	db, err := Open(Options{Path: path, BusyTimeout: 100 * time.Millisecond, BinaryVersion: "test", Migrations: migrations})
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func pragmaText(t *testing.T, db *DB, name string) string {
	t.Helper()
	var value string
	if err := db.sql.QueryRow("PRAGMA " + name).Scan(&value); err != nil {
		t.Fatalf("pragma %s: %v", name, err)
	}
	return value
}

func TestOpenAppliesPragmasAndEmbeddedSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", FileName)
	db := openTest(t, path, nil)

	for name, want := range map[string]string{
		"journal_mode": "wal",
		"locking_mode": "exclusive",
		"synchronous":  "1", // NORMAL
		"foreign_keys": "1",
		"auto_vacuum":  "2", // INCREMENTAL
		"busy_timeout": "100",
		"mmap_size":    "0",
	} {
		if got := strings.ToLower(pragmaText(t, db, name)); got != want {
			t.Errorf("PRAGMA %s = %q, want %q", name, got, want)
		}
	}
	embedded, err := EmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if db.SchemaVersion() != len(embedded) || pragmaText(t, db, "user_version") != itoa(len(embedded)) {
		t.Fatalf("schema version %d / user_version %s, want %d", db.SchemaVersion(), pragmaText(t, db, "user_version"), len(embedded))
	}
	var recorded int
	if err := db.sql.QueryRow(`SELECT count(*) FROM schema_migrations WHERE binary_version = 'test'`).Scan(&recorded); err != nil || recorded != len(embedded) {
		t.Fatalf("schema_migrations rows = %d (%v), want %d", recorded, err, len(embedded))
	}
	if info, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("database file mode: %v %v", info.Mode(), err)
	}
	// EXCLUSIVE locking before WAL keeps the wal-index in heap memory.
	if _, err := os.Stat(path + "-shm"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a -shm file exists beside the database: %v", err)
	}
}

// A reopened WAL database must not grow a -shm file either: every read of the
// file, the auto_vacuum step's page count included, comes after EXCLUSIVE.
func TestReopenCreatesNoSharedMemoryFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	for round := 0; round < 3; round++ {
		db, err := Open(Options{Path: path, BinaryVersion: "test", BusyTimeout: 100 * time.Millisecond})
		if err != nil {
			t.Fatalf("open %d: %v", round, err)
		}
		if err := db.Write(context.Background(), func(tx *Tx) error {
			_, err := tx.Exec(`INSERT OR REPLACE INTO migrated_stores(store, status, source, started_at, binary_version) VALUES (?, 'importing', 'src', 'now', 'test')`, fmt.Sprintf("store-%d", round))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path + "-shm"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("open %d: a -shm file exists beside the database: %v", round, err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path + "-shm"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("close %d: a -shm file exists beside the database: %v", round, err)
		}
	}
}

func TestReopenIsIdempotentAndCloseLeavesOneSelfContainedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	db, err := Open(Options{Path: path, BinaryVersion: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Write(context.Background(), func(tx *Tx) error {
		return tx.BeginStoreImport("chats", "provider-chats", "first")
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if info, err := os.Stat(path + "-wal"); err == nil && info.Size() != 0 {
		t.Fatalf("close left a %d-byte WAL", info.Size())
	}
	// Copy only the main file, as the updater's byte copy would.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(dir, "copy", FileName)
	if err := os.MkdirAll(filepath.Dir(copyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, copyPath} {
		reopened := openTest(t, candidate, nil)
		var store MigratedStore
		var ok bool
		if err := reopened.Read(context.Background(), func(tx *Tx) error {
			var err error
			store, ok, err = tx.MigratedStoreStatus("chats")
			return err
		}); err != nil || !ok || store.Status != StoreImporting {
			t.Fatalf("%s: migrated store after reopen = %#v ok=%v err=%v", candidate, store, ok, err)
		}
		var recorded int
		if err := reopened.sql.QueryRow(`SELECT count(*) FROM schema_migrations WHERE binary_version = 'first'`).Scan(&recorded); err != nil || recorded != reopened.SchemaVersion() {
			t.Fatalf("%s: reopen re-applied migrations: recorded=%d err=%v", candidate, recorded, err)
		}
	}
}

func TestOpenRefusesNewerSchemaWithoutTouchingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	db := openTest(t, path, nil)
	known := db.SchemaVersion()
	if _, err := db.sql.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(Options{Path: path, BusyTimeout: 100 * time.Millisecond})
	if !errors.Is(err, ErrSchemaTooNew) || !strings.Contains(err.Error(), "999") || !strings.Contains(err.Error(), itoa(known)) {
		t.Fatalf("newer schema accepted or unclear: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("refusing a newer schema rewrote the database file")
	}
}

func TestOpenRefusesDivergentMigrationHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	first := []Migration{{Version: 1, Name: "alpha", SQL: "CREATE TABLE alpha(x)"}}
	db := openTest(t, path, first)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	renamed := []Migration{{Version: 1, Name: "beta", SQL: "CREATE TABLE alpha(x)"}}
	if _, err := Open(Options{Path: path, Migrations: renamed}); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("renamed migration accepted: %v", err)
	}
}

func TestMigrationsUpgradeInOrderWithBackupAndRollBackAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	v1 := []Migration{{Version: 1, Name: "alpha", SQL: "CREATE TABLE alpha(x INTEGER); INSERT INTO alpha VALUES (1);"}}
	db := openTest(t, path, v1)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	broken := append(append([]Migration(nil), v1...), Migration{Version: 2, Name: "broken", SQL: "CREATE TABLE beta(y); INSERT INTO missing VALUES (1);"})
	if _, err := Open(Options{Path: path, Migrations: broken}); err == nil || !strings.Contains(err.Error(), "0002_broken") {
		t.Fatalf("broken migration did not fail clearly: %v", err)
	}
	check := openTest(t, path, v1)
	var tables int
	if err := check.sql.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name = 'beta'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("failed migration left table beta: %d %v", tables, err)
	}
	if check.SchemaVersion() != 1 {
		t.Fatalf("failed migration moved user_version to %d", check.SchemaVersion())
	}
	if err := check.Close(); err != nil {
		t.Fatal(err)
	}

	v2 := append(append([]Migration(nil), v1...), Migration{
		Version: 2, Name: "beta", SQL: "CREATE TABLE beta(y INTEGER)",
		Up: func(tx *Tx) error {
			_, err := tx.Exec("INSERT INTO beta SELECT x + 1 FROM alpha")
			return err
		},
	})
	backups := filepath.Join(dir, "backups")
	upgraded, err := Open(Options{Path: path, Migrations: v2, BackupDir: backups})
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var y int
	if err := upgraded.sql.QueryRow("SELECT y FROM beta").Scan(&y); err != nil || y != 2 {
		t.Fatalf("Go migration step did not run: y=%d err=%v", y, err)
	}
	entries, err := os.ReadDir(backups)
	if err != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "state-v1-") {
		t.Fatalf("pre-migration backup missing: %v %v", entries, err)
	}
	snapshot := openTest(t, filepath.Join(backups, entries[0].Name()), v1)
	if snapshot.SchemaVersion() != 1 {
		t.Fatalf("backup is not the pre-migration schema: %d", snapshot.SchemaVersion())
	}
}

func TestSecondOpenFailsFastWhileTheDatabaseIsHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	held := openTest(t, path, nil)

	started := time.Now()
	second, err := Open(Options{Path: path, BusyTimeout: 100 * time.Millisecond})
	if err == nil {
		_ = second.Close()
		t.Fatal("a second open succeeded while the first connection holds the exclusive lock")
	}
	if !errors.Is(err, ErrLocked) || !IsBusy(err) {
		t.Fatalf("second open error is not ErrLocked: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("second open took %s to fail", elapsed)
	}

	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(Options{Path: path, BusyTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("open after release: %v", err)
	}
	_ = reopened.Close()
}

func TestWriteRetriesBusyAndStopsOnOtherErrors(t *testing.T) {
	db := openTest(t, filepath.Join(t.TempDir(), FileName), nil)
	calls := 0
	err := db.Write(context.Background(), func(tx *Tx) error {
		calls++
		if calls < 3 {
			return ErrLocked
		}
		_, err := tx.Exec(`INSERT INTO migrated_stores(store, status, source, started_at, binary_version) VALUES ('x', 'importing', 's', 't', 'b')`)
		return err
	})
	if err != nil || calls != 3 {
		t.Fatalf("busy write: calls=%d err=%v", calls, err)
	}
	calls = 0
	boom := errors.New("boom")
	if err := db.Write(context.Background(), func(*Tx) error { calls++; return boom }); !errors.Is(err, boom) || calls != 1 {
		t.Fatalf("non-busy error retried: calls=%d err=%v", calls, err)
	}
	calls = 0
	if err := db.Write(context.Background(), func(*Tx) error { calls++; return ErrLocked }); !errors.Is(err, ErrLocked) || calls != maxBusyRetries+1 {
		t.Fatalf("persistent busy: calls=%d err=%v", calls, err)
	}
	// A failed transaction rolled back: only the retried insert exists.
	var rows int
	if err := db.sql.QueryRow(`SELECT count(*) FROM migrated_stores`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("migrated_stores rows = %d %v", rows, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Write(context.Background(), func(*Tx) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after close: %v", err)
	}
}

func TestImportReceiptsAndStoreLifecycle(t *testing.T) {
	db := openTest(t, filepath.Join(t.TempDir(), FileName), nil)
	digest := strings.Repeat("a", 64)
	err := db.Write(context.Background(), func(tx *Tx) error {
		if err := tx.BeginStoreImport("chats", "provider-chats", "test"); err != nil {
			return err
		}
		if err := tx.BeginStoreImport("chats", "provider-chats", "test"); err != nil {
			return err
		}
		if err := tx.BeginStoreImport("chats", "elsewhere", "test"); err == nil {
			return errors.New("a second source was accepted")
		}
		if err := tx.PutImportReceipt(ImportReceipt{Store: "chats", Source: "provider-chats/a.json", SHA256: digest, Size: 3, MTimeNS: 7, ItemKey: "chat-a", Outcome: OutcomeImported}); err != nil {
			return err
		}
		// Re-import replaces the receipt in place.
		return tx.PutImportReceipt(ImportReceipt{Store: "chats", Source: "provider-chats/a.json", SHA256: strings.Repeat("b", 64), Size: 4, MTimeNS: 8, ItemKey: "chat-a", Outcome: OutcomeImported})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Write(context.Background(), func(tx *Tx) error {
		return tx.PutImportReceipt(ImportReceipt{Store: "chats", Source: "x", SHA256: "short", Outcome: OutcomeImported})
	}); err == nil {
		t.Fatal("receipt without a digest accepted")
	}
	err = db.Write(context.Background(), func(tx *Tx) error {
		receipts, err := tx.ImportReceipts("chats")
		if err != nil {
			return err
		}
		if len(receipts) != 1 || receipts[0].SHA256 != strings.Repeat("b", 64) || receipts[0].Size != 4 || receipts[0].BinaryVersion != "" {
			return errors.New("receipt was not replaced in place")
		}
		if err := tx.CompleteStoreImport("chats", 1); err != nil {
			return err
		}
		if err := tx.CompleteStoreImport("chats", 1); err == nil {
			return errors.New("a live store completed twice")
		}
		if err := tx.BeginStoreImport("chats", "provider-chats", "test"); err == nil {
			return errors.New("a live store restarted its import")
		}
		status, ok, err := tx.MigratedStoreStatus("chats")
		if err != nil || !ok || status.Status != StoreLive || status.Items != 1 || status.CompletedAt == "" {
			return errors.Join(err, errors.New("store did not become live"))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestQuickCheckAndBackupOnLiveDatabase(t *testing.T) {
	dir := t.TempDir()
	db := openTest(t, filepath.Join(dir, FileName), nil)
	if err := db.QuickCheck(); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "backups", "snap.db")
	if err := db.Backup(backup); err != nil {
		t.Fatal(err)
	}
	if err := db.Backup(backup); err == nil {
		t.Fatal("backup overwrote an existing file")
	}
	info, err := os.Stat(backup)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("backup file: %v %v", info, err)
	}
}

func TestEmbeddedMigrationsAreContiguousAndNamed(t *testing.T) {
	migrations, err := EmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) == 0 || migrations[0].Name != "store_framework" {
		t.Fatalf("embedded migrations = %#v", migrations)
	}
	if err := validateMigrationList([]Migration{{Version: 2, Name: "x", SQL: "SELECT 1"}}); err == nil {
		t.Fatal("non-contiguous list accepted")
	}
}

func itoa(value int) string { return strconv.Itoa(value) }
