package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	backupCrashDirEnv  = "WORKASS_TEST_BACKUP_CRASH_DIR"
	backupCrashWhenEnv = "WORKASS_TEST_BACKUP_CRASH_WHEN"
)

var (
	backupV1 = []Migration{{Version: 1, Name: "alpha", SQL: "CREATE TABLE alpha(x INTEGER); INSERT INTO alpha VALUES (1);"}}
	backupV2 = append(append([]Migration(nil), backupV1...), Migration{Version: 2, Name: "beta", SQL: "CREATE TABLE beta(y INTEGER)"})
	backupV3 = append(append([]Migration(nil), backupV2...), Migration{Version: 3, Name: "gamma", SQL: "CREATE TABLE gamma(z INTEGER)"})
)

// backupFixture is a v1 database with two backups an earlier build left, a
// write of one that was interrupted, and a file that is not a backup.
func backupFixture(t *testing.T) (path, backups string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, FileName)
	db := openTest(t, path, backupV1)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	backups = filepath.Join(dir, "backups")
	if err := os.MkdirAll(backups, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"state-v1-20260901T100000.000000000Z.db",
		"state-v1-20260902T100000.000000000Z.db",
		"state-v1-20260903T100000.000000000Z.db.partial",
		"notes.txt",
	} {
		if err := os.WriteFile(filepath.Join(backups, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path, backups
}

func backupNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

// written are the backups a migration wrote during the test (not fixture ones).
func written(names []string) []string {
	var out []string
	for _, name := range names {
		if migrationBackupName.MatchString(name) && !strings.HasPrefix(name, "state-v1-202609") {
			out = append(out, name)
		}
	}
	return out
}

func assertBackupOf(t *testing.T, path string, version int) {
	t.Helper()
	snapshot, err := Open(Options{Path: path, Migrations: backupV3[:version], BusyTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("backup %s does not open: %v", filepath.Base(path), err)
	}
	defer snapshot.Close()
	if snapshot.SchemaVersion() != version {
		t.Fatalf("backup %s is schema %d, want %d", filepath.Base(path), snapshot.SchemaVersion(), version)
	}
}

func TestSuccessfulMigrationKeepsOnlyItsBackup(t *testing.T) {
	path, backups := backupFixture(t)
	db, err := Open(Options{Path: path, Migrations: backupV2, BackupDir: backups, BusyTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	names := backupNames(t, backups)
	made := written(names)
	if len(made) != 1 || !strings.HasPrefix(made[0], "state-v1-") || strings.HasSuffix(made[0], ".partial") {
		t.Fatalf("backups after the migration: %v", names)
	}
	if want := []string{made[0], "notes.txt"}; strings.Join(names, ",") != strings.Join(sortedCopy(want), ",") {
		t.Fatalf("backups after the migration: %v, want %v", names, sortedCopy(want))
	}
	if pruned := db.PrunedBackups(); len(pruned) != 3 {
		t.Fatalf("pruned %v, want the two older backups and the interrupted one", pruned)
	}
	assertBackupOf(t, filepath.Join(backups, made[0]), 1)
}

func TestFailedMigrationKeepsEveryBackup(t *testing.T) {
	path, backups := backupFixture(t)
	before := backupNames(t, backups)
	broken := append(append([]Migration(nil), backupV1...), Migration{Version: 2, Name: "broken", SQL: "CREATE TABLE beta(y); INSERT INTO missing VALUES (1);"})
	if _, err := Open(Options{Path: path, Migrations: broken, BackupDir: backups, BusyTimeout: 100 * time.Millisecond}); err == nil {
		t.Fatal("the broken migration succeeded")
	}
	after := backupNames(t, backups)
	made := written(after)
	if len(made) != 1 || len(after) != len(before)+1 {
		t.Fatalf("backups after a failed migration: %v (before %v)", after, before)
	}
	for _, name := range before {
		if _, err := os.Stat(filepath.Join(backups, name)); err != nil {
			t.Fatalf("a failed migration removed %s", name)
		}
	}
	assertBackupOf(t, filepath.Join(backups, made[0]), 1)
	// A later successful migration supersedes them all with its own backup.
	db, err := Open(Options{Path: path, Migrations: backupV2, BackupDir: backups, BusyTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	final := written(backupNames(t, backups))
	if len(final) != 1 || final[0] == made[0] {
		t.Fatalf("backups after the retry: %v (failed attempt wrote %s)", backupNames(t, backups), made[0])
	}
}

// TestBackupCrashHelper is the child process of the crash tests: it migrates
// the database in WORKASS_TEST_BACKUP_CRASH_DIR and kills itself either in
// the middle of the migration transaction ("migration") or after removing
// the first older backup ("prune"). No deferred code, rollback or close runs.
func TestBackupCrashHelper(t *testing.T) {
	dir := os.Getenv(backupCrashDirEnv)
	if dir == "" {
		t.Skip("child process of the migration crash tests")
	}
	die := func() {
		self, _ := os.FindProcess(os.Getpid())
		_ = self.Kill()
		time.Sleep(time.Minute)
	}
	migrations := backupV2
	switch os.Getenv(backupCrashWhenEnv) {
	case "migration":
		migrations = append(append([]Migration(nil), backupV1...), Migration{
			Version: 2, Name: "beta", SQL: "CREATE TABLE beta(y INTEGER)",
			Up: func(tx *Tx) error {
				if _, err := tx.Exec("INSERT INTO beta VALUES (2)"); err != nil {
					return err
				}
				die()
				return nil
			},
		})
	case "prune":
		removed := 0
		pruneBackupHook = func(string) {
			if removed++; removed == 2 {
				die()
			}
		}
	}
	_, err := Open(Options{Path: filepath.Join(dir, FileName), Migrations: migrations, BackupDir: filepath.Join(dir, "backups"), BusyTimeout: 100 * time.Millisecond})
	t.Fatalf("child finished without being killed: %v", err)
}

func runBackupCrashChild(t *testing.T, dir, when string) {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^TestBackupCrashHelper$", "-test.count=1")
	child.Env = append(os.Environ(), backupCrashDirEnv+"="+dir, backupCrashWhenEnv+"="+when)
	output, err := child.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.Success() {
		t.Fatalf("child was not killed: %v\n%s", err, output)
	}
}

func TestCrashMidMigrationKeepsItsBackup(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a child process")
	}
	path, backups := backupFixture(t)
	before := backupNames(t, backups)
	runBackupCrashChild(t, filepath.Dir(path), "migration")
	after := backupNames(t, backups)
	made := written(after)
	if len(made) != 1 || len(after) != len(before)+1 {
		t.Fatalf("backups after a crash mid-migration: %v (before %v)", after, before)
	}
	assertBackupOf(t, filepath.Join(backups, made[0]), 1)
	// The database rolled back to v1; the next run migrates and keeps only
	// its own backup.
	db, err := Open(Options{Path: path, Migrations: backupV2, BackupDir: backups, BusyTimeout: 100 * time.Millisecond, QuickCheck: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var rows int
	if err := db.sql.QueryRow("SELECT count(*) FROM beta").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("the crashed migration's row survived: %d %v", rows, err)
	}
	final := written(backupNames(t, backups))
	if len(final) != 1 || final[0] == made[0] {
		t.Fatalf("backups after the rerun: %v", backupNames(t, backups))
	}
}

func TestCrashMidPruneKeepsTheNewestBackup(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a child process")
	}
	path, backups := backupFixture(t)
	runBackupCrashChild(t, filepath.Dir(path), "prune")
	names := backupNames(t, backups)
	made := written(names)
	if len(made) != 1 {
		t.Fatalf("backups after a crash mid-prune: %v", names)
	}
	assertBackupOf(t, filepath.Join(backups, made[0]), 1)
	// The migration committed before the prune began.
	db, err := Open(Options{Path: path, Migrations: backupV2, BackupDir: backups, BusyTimeout: 100 * time.Millisecond, QuickCheck: true})
	if err != nil {
		t.Fatal(err)
	}
	if db.SchemaVersion() != 2 {
		t.Fatalf("schema %d after a crash mid-prune", db.SchemaVersion())
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if len(names) < 3 {
		t.Fatalf("the crash left no older backup to finish: %v", names)
	}
	// The next migration finishes the job.
	next, err := Open(Options{Path: path, Migrations: backupV3, BackupDir: backups, BusyTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	final := written(backupNames(t, backups))
	if all := backupNames(t, backups); len(final) != 1 || !strings.HasPrefix(final[0], "state-v2-") || len(all) != 2 {
		t.Fatalf("backups after the next migration: %v", all)
	}
}

func sortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}
