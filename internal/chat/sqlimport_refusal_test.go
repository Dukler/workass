package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"workass/internal/store"
)

func setMaxPageCount(t *testing.T, db *store.DB, pages int64) {
	t.Helper()
	if err := db.Write(context.Background(), func(tx *store.Tx) error {
		var applied int64
		return tx.QueryRow(fmt.Sprintf(`PRAGMA max_page_count = %d`, pages)).Scan(&applied)
	}); err != nil {
		t.Fatal(err)
	}
}

func pageCount(t *testing.T, db *store.DB) int64 {
	t.Helper()
	var pages int64
	if err := db.Read(context.Background(), func(tx *store.Tx) error {
		return tx.QueryRow(`PRAGMA page_count`).Scan(&pages)
	}); err != nil {
		t.Fatal(err)
	}
	return pages
}

// Only a check of the chat file refuses it: the refusal receipt and its "fix
// or remove the file" wording are for that alone. A failure of the database
// or the disk while importing stops the import just the same (no receipt for
// the file, the store stays importing, the JSON untouched), but names its
// real cause and blames no file; once the cause is gone the same files
// import.
func TestImportRefusesOnlyAnInvalidChatFile(t *testing.T) {
	cases := []struct {
		name string
		// inject sets the failure up; the returned func removes it.
		inject func(t *testing.T, actors *SQLActors, actorDir string, options *ImportOptions) func()
		// failed is the chat whose file stops the import.
		failed  string
		refusal bool
		cause   string
		is      error
	}{
		{
			name: "invalid chat file",
			inject: func(t *testing.T, _ *SQLActors, actorDir string, _ *ImportOptions) func() {
				path := writeInvalidSequenceActor(t, actorDir, "bad-sequence")
				return func() {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
			},
			failed:  "bad-sequence",
			refusal: true,
			cause:   "ledger sequence 5 is not contiguous",
		},
		{
			// A page limit just above the database's size: the first
			// actor's rows fail with SQLITE_FULL inside the step that reads
			// and checks the file.
			name: "disk full",
			inject: func(t *testing.T, actors *SQLActors, _ string, _ *ImportOptions) func() {
				setMaxPageCount(t, actors.db, pageCount(t, actors.db)+2)
				return func() { setMaxPageCount(t, actors.db, 1<<30) }
			},
			failed: "big-chat",
			cause:  "the disk is full",
		},
		{
			// What the store reports, after its retries, while another
			// process holds the database.
			name: "database busy",
			inject: func(_ *testing.T, _ *SQLActors, _ string, options *ImportOptions) func() {
				options.afterRows = func(string, int) error { return store.ErrLocked }
				return func() { options.afterRows = nil }
			},
			failed: "big-chat",
			cause:  "the state database is busy",
			is:     store.ErrLocked,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			actorDir := filepath.Join(stateDir, "provider-chats")
			if err := os.MkdirAll(actorDir, 0o700); err != nil {
				t.Fatal(err)
			}
			const chatID = "big-chat"
			if err := (FileStore{Path: filepath.Join(actorDir, actorFileName(chatID))}).Save(richLedgerState(t, chatID, 40, 0, 0)); err != nil {
				t.Fatal(err)
			}
			actors := openTestActors(t)
			options := corpusImportOptions(stateDir)
			var logged []string
			options.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
			restore := tc.inject(t, actors, actorDir, &options)
			before := snapshotDir(t, actorDir)

			result, err := ImportFileActors(context.Background(), actors.db, options)
			if err == nil || result.Live {
				t.Fatalf("the import went on: %+v %v", result, err)
			}
			if !strings.Contains(err.Error(), tc.cause) || (tc.is != nil && !errors.Is(err, tc.is)) {
				t.Fatalf("the error does not name its cause %q: %v", tc.cause, err)
			}
			blames := strings.Contains(err.Error(), "fixed or removed")
			refusalPath := filepath.Join(stateDir, ImportRefusalFileName)
			raw, statErr := os.ReadFile(refusalPath)
			if tc.refusal {
				var refusal ImportRefusal
				if !blames || statErr != nil || json.Unmarshal(raw, &refusal) != nil || !strings.Contains(refusal.Reason, tc.cause) {
					t.Fatalf("a refused file: blames=%v receipt=%s (%v); error %v", blames, raw, statErr, err)
				}
			} else {
				if blames || !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("a %s blamed the chat file: receipt=%s (%v); error %v", tc.name, raw, statErr, err)
				}
				for _, line := range logged {
					if strings.Contains(line, "refused chat actor") {
						t.Fatalf("a %s was logged as a refusal: %q", tc.name, line)
					}
				}
			}
			var status store.MigratedStore
			var receipts int
			if err := actors.db.Read(context.Background(), func(tx *store.Tx) error {
				var err error
				if status, _, err = tx.MigratedStoreStatus(ActorStoreName); err != nil {
					return err
				}
				return tx.QueryRow(`SELECT count(*) FROM import_receipts WHERE item_key = ?`, tc.failed).Scan(&receipts)
			}); err != nil {
				t.Fatal(err)
			}
			if status.Status != store.StoreImporting || receipts != 0 {
				t.Fatalf("after the failure: status %q, %d receipts for %s", status.Status, receipts, tc.failed)
			}
			if after := snapshotDir(t, actorDir); !reflect.DeepEqual(before, after) {
				t.Fatal("the failed import changed the JSON directory")
			}

			restore()
			if result, err := ImportFileActors(context.Background(), actors.db, options); err != nil || !result.Live {
				t.Fatalf("import once the cause is gone: %+v %v", result, err)
			}
			if loaded, found, err := actors.Store(chatID).Load(chatID); err != nil || !found || len(loaded.Ledger) != 40 {
				t.Fatalf("the imported chat: found=%v err=%v", found, err)
			}
			if _, err := os.Stat(refusalPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a refusal receipt outlived the completed import: %v", err)
			}
		})
	}
}
