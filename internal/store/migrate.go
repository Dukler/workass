package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Migration is one ordered schema step. Version starts at 1 and is
// contiguous; a shipped migration is never edited or renumbered, a change is
// a new migration. SQL runs first, then Up (for data steps SQL cannot
// express), in the same transaction as the schema_migrations row and the
// PRAGMA user_version bump.
type Migration struct {
	Version int
	Name    string
	SQL     string
	Up      func(*Tx) error
}

//go:embed migrations/*.sql
var migrationFiles embed.FS

var migrationFileName = regexp.MustCompile(`^([0-9]{4})_([a-z0-9_]+)\.sql$`)

// EmbeddedMigrations returns the schema shipped with this build, ordered.
func EmbeddedMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	migrations := make([]Migration, 0, len(entries))
	for _, entry := range entries {
		match := migrationFileName.FindStringSubmatch(entry.Name())
		if match == nil {
			return nil, fmt.Errorf("migration file %q is not named NNNN_name.sql", entry.Name())
		}
		version, _ := strconv.Atoi(match[1])
		raw, err := migrationFiles.ReadFile(path.Join("migrations", entry.Name()))
		if err != nil {
			return nil, err
		}
		migrations = append(migrations, Migration{Version: version, Name: match[2], SQL: string(raw)})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	if err := validateMigrationList(migrations); err != nil {
		return nil, err
	}
	return migrations, nil
}

func validateMigrationList(migrations []Migration) error {
	seen := make(map[string]struct{}, len(migrations))
	for index, migration := range migrations {
		if migration.Version != index+1 {
			return fmt.Errorf("migration %q has version %d, want %d (versions are contiguous from 1)", migration.Name, migration.Version, index+1)
		}
		if migration.Name == "" {
			return fmt.Errorf("migration %d has no name", migration.Version)
		}
		if _, duplicate := seen[migration.Name]; duplicate {
			return fmt.Errorf("migration name %q is not unique", migration.Name)
		}
		seen[migration.Name] = struct{}{}
		if migration.SQL == "" && migration.Up == nil {
			return fmt.Errorf("migration %d %q is empty", migration.Version, migration.Name)
		}
	}
	return nil
}

const schemaMigrationsDDL = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version        INTEGER PRIMARY KEY,
	name           TEXT NOT NULL,
	applied_at     TEXT NOT NULL,
	binary_version TEXT NOT NULL
)`

// migrate verifies the recorded history and applies pending migrations, one
// transaction each. It returns the resulting user_version.
func migrate(db *DB, migrations []Migration, backupDir string) (int, error) {
	var current int
	if err := db.sql.QueryRow("PRAGMA user_version").Scan(&current); err != nil {
		return 0, classify(err)
	}
	if current > len(migrations) {
		return 0, fmt.Errorf("%w: schema version %d, this build knows %d; update Workass or restore a backup", ErrSchemaTooNew, current, len(migrations))
	}
	if err := verifyHistory(db, migrations, current); err != nil {
		return 0, err
	}
	if current == len(migrations) {
		return current, nil
	}
	backup := ""
	base := current
	if current > 0 && backupDir != "" {
		// An upgrade interrupted after some of its migrations committed
		// resumes from a later version; its marker still names the version it
		// started from, whose backup is the only one an older build can open.
		if marked, ok := readUpgradeBase(backupDir); ok && marked < current {
			base = marked
		} else if err := writeUpgradeBase(backupDir, current); err != nil {
			return 0, fmt.Errorf("record the version this upgrade starts from: %w", err)
		}
		backup = fmt.Sprintf("state-v%d-%s.db", current, time.Now().UTC().Format("20060102T150405.000000000Z"))
		if err := db.Backup(filepath.Join(backupDir, backup)); err != nil {
			return 0, fmt.Errorf("back up state database before migration: %w", err)
		}
	}
	for _, migration := range migrations[current:] {
		migration := migration
		err := db.Write(context.Background(), func(tx *Tx) error {
			if _, err := tx.Exec(schemaMigrationsDDL); err != nil {
				return err
			}
			if migration.SQL != "" {
				if _, err := tx.Exec(migration.SQL); err != nil {
					return err
				}
			}
			if migration.Up != nil {
				if err := migration.Up(tx); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(`INSERT INTO schema_migrations(version, name, applied_at, binary_version) VALUES (?, ?, ?, ?)`,
				migration.Version, migration.Name, nowText(), db.binaryVersion); err != nil {
				return err
			}
			// user_version lives in the database header, which this
			// transaction owns: it commits or rolls back with the schema.
			_, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", migration.Version))
			return err
		})
		if err != nil {
			return 0, fmt.Errorf("apply migration %04d_%s: %w", migration.Version, migration.Name, err)
		}
		current = migration.Version
	}
	if backup != "" {
		// Every migration committed: the backup just written is the state
		// before them, and the newest backup of the version the upgrade
		// started from is the state before an interrupted earlier run of it.
		// Older ones are superseded by those two. The marker goes first: the
		// upgrade is complete, and a crash part way through the prune must
		// not make the next upgrade keep this one's starting backup.
		_ = os.Remove(filepath.Join(backupDir, upgradeBaseFile))
		db.prunedBackups = pruneMigrationBackups(backupDir, backup, base)
	}
	return current, nil
}

// migrationBackupName matches the backups migrate writes (state-v<N>-<UTC
// stamp>.db) and what an interrupted write of one leaves (.partial, and the
// rollback journal SQLite keeps beside it while writing it). The first group
// is the schema version; the second is set on an incomplete write.
var migrationBackupName = regexp.MustCompile(`^state-v([0-9]+)-[0-9]{8}T[0-9]{6}\.[0-9]{9}Z\.db(\.partial(?:-journal|-wal|-shm)?)?$`)

// upgradeBaseFile records, beside the backups, the schema version an upgrade
// started from. It is written before the first backup of an upgrade and
// removed once every migration has committed, so a run that finds it is
// resuming an interrupted upgrade.
const upgradeBaseFile = "upgrade-base"

func readUpgradeBase(dir string) (int, bool) {
	data, err := os.ReadFile(filepath.Join(dir, upgradeBaseFile))
	if err != nil {
		return 0, false
	}
	version, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || version < 1 {
		return 0, false
	}
	return version, true
}

func writeUpgradeBase(dir string, version int) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	partial := filepath.Join(dir, upgradeBaseFile+".partial")
	if err := os.WriteFile(partial, []byte(strconv.Itoa(version)+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(partial, filepath.Join(dir, upgradeBaseFile))
}

// pruneBackupHook, when set (tests), runs before each backup is removed.
var pruneBackupHook func(name string)

// pruneMigrationBackups removes every migration backup in dir except keep,
// the one written before the migrations that just committed, and the newest
// complete backup of version base, the version the upgrade started from when
// an interrupted run of it committed some migrations before this one. It
// returns the names it removed. Each backup is a whole copy of the database
// (a gigabyte on a long-lived install), so keeping every one filled the disk.
//
// It runs only once migrations have committed, in the process that holds the
// database's exclusive lock: no other migration can be writing a backup, a
// migration that failed returned before this point with its backup in place,
// and keep is complete (written and synced, then renamed into place). A crash
// part way leaves some older backups for the next successful migration to
// remove; it can never remove keep. Only files named like a backup are
// touched.
func pruneMigrationBackups(dir, keep string, base int) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	// The stamp sorts names in time order within one version.
	baseBackup := ""
	for _, entry := range entries {
		match := migrationBackupName.FindStringSubmatch(entry.Name())
		if match == nil || match[2] != "" || !entry.Type().IsRegular() {
			continue
		}
		if version, err := strconv.Atoi(match[1]); err == nil && version == base && entry.Name() > baseBackup {
			baseBackup = entry.Name()
		}
	}
	var removed []string
	for _, entry := range entries {
		name := entry.Name()
		if name == keep || name == baseBackup || !entry.Type().IsRegular() || !migrationBackupName.MatchString(name) {
			continue
		}
		if pruneBackupHook != nil {
			pruneBackupHook(name)
		}
		if err := os.Remove(filepath.Join(dir, name)); err == nil {
			removed = append(removed, name)
		}
	}
	return removed
}

func verifyHistory(db *DB, migrations []Migration, current int) error {
	var tables int
	if err := db.sql.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'schema_migrations'`).Scan(&tables); err != nil {
		return classify(err)
	}
	if tables == 0 {
		if current != 0 {
			return fmt.Errorf("%w: user_version %d without schema_migrations", ErrSchemaMismatch, current)
		}
		var others int
		if err := db.sql.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type = 'table'`).Scan(&others); err != nil {
			return classify(err)
		}
		if others != 0 {
			return fmt.Errorf("%w: database has tables but no migration history", ErrSchemaMismatch)
		}
		return nil
	}
	rows, err := db.sql.Query(`SELECT version, name FROM schema_migrations ORDER BY version`)
	if err != nil {
		return classify(err)
	}
	defer rows.Close()
	expected := 1
	for rows.Next() {
		var version int
		var name string
		if err := rows.Scan(&version, &name); err != nil {
			return err
		}
		if version != expected || version > current {
			return fmt.Errorf("%w: recorded migration %d, want %d of %d", ErrSchemaMismatch, version, expected, current)
		}
		if name != migrations[version-1].Name {
			return fmt.Errorf("%w: migration %d is %q here but %q in the database", ErrSchemaMismatch, version, migrations[version-1].Name, name)
		}
		expected++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if expected-1 != current {
		return fmt.Errorf("%w: %d recorded migrations, user_version %d", ErrSchemaMismatch, expected-1, current)
	}
	return nil
}

func nowText() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
