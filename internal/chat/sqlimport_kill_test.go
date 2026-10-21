package chat

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"workass/internal/store"
)

const (
	importKillStateEnv = "WORKASS_TEST_IMPORT_KILL_STATE"
	importKillAtEnv    = "WORKASS_TEST_IMPORT_KILL_AT"
)

// TestImportKillHelper is the child process of
// TestImportResumesConsistentlyAfterAKillMidTransaction. It imports and kills
// itself inside the Nth actor's transaction, after that actor's rows were
// written: no rollback, no close, no deferred code.
func TestImportKillHelper(t *testing.T) {
	stateDir := os.Getenv(importKillStateEnv)
	if stateDir == "" {
		t.Skip("child process of the kill-during-import test")
	}
	killAt, _ := strconv.Atoi(os.Getenv(importKillAtEnv))
	db, err := store.Open(store.Options{Path: filepath.Join(stateDir, store.FileName), BinaryVersion: "kill-child"})
	if err != nil {
		t.Fatal(err)
	}
	options := corpusImportOptions(stateDir)
	seen := 0
	options.afterRows = func(string, int) error {
		seen++
		if seen == killAt {
			self, _ := os.FindProcess(os.Getpid())
			_ = self.Kill()
			time.Sleep(time.Minute)
		}
		return nil
	}
	_, err = ImportFileActors(context.Background(), db, options)
	t.Fatalf("child import finished without being killed: %v", err)
}

func TestImportResumesConsistentlyAfterAKillMidTransaction(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a child process")
	}
	stateDir := t.TempDir()
	actorDir := filepath.Join(stateDir, "provider-chats")
	if err := os.MkdirAll(actorDir, 0o700); err != nil {
		t.Fatal(err)
	}
	corpus := buildActorCorpus(t, actorDir)
	killAt := 4
	child := exec.Command(os.Args[0], "-test.run=^TestImportKillHelper$", "-test.count=1")
	child.Env = append(os.Environ(), importKillStateEnv+"="+stateDir, importKillAtEnv+"="+strconv.Itoa(killAt))
	output, err := child.CombinedOutput()
	if err == nil {
		t.Fatalf("child was not killed:\n%s", output)
	}
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.Success() {
		t.Fatalf("child did not die from the kill: %v\n%s", err, output)
	}

	// The killed process left an uncommitted transaction in the WAL. Opening
	// recovers to the last commit: killAt-1 actors with receipts, nothing of
	// the actor that was mid-write.
	db, err := store.Open(store.Options{Path: filepath.Join(stateDir, store.FileName), BinaryVersion: "resume", QuickCheck: true})
	if err != nil {
		t.Fatalf("reopen after kill: %v", err)
	}
	defer db.Close()
	var receipts, chats, orphanRows int
	var status store.MigratedStore
	if err := db.Read(context.Background(), func(tx *store.Tx) error {
		var err error
		if status, _, err = tx.MigratedStoreStatus(ActorStoreName); err != nil {
			return err
		}
		if err := tx.QueryRow(`SELECT count(*) FROM import_receipts`).Scan(&receipts); err != nil {
			return err
		}
		if err := tx.QueryRow(`SELECT count(*) FROM chats`).Scan(&chats); err != nil {
			return err
		}
		return tx.QueryRow(`SELECT count(*) FROM messages WHERE chat_id NOT IN (SELECT item_key FROM import_receipts)`).Scan(&orphanRows)
	}); err != nil {
		t.Fatal(err)
	}
	if status.Status != store.StoreImporting || receipts != killAt-1 || chats != killAt-1 || orphanRows != 0 {
		t.Fatalf("after kill: status=%q receipts=%d chats=%d orphan rows=%d, want importing/%d/%d/0", status.Status, receipts, chats, orphanRows, killAt-1, killAt-1)
	}

	result, err := ImportFileActors(context.Background(), db, corpusImportOptions(stateDir))
	if err != nil {
		t.Fatalf("resume after kill: %v", err)
	}
	if !result.Live || result.Unchanged != killAt-1 || result.Imported != len(corpus)-(killAt-1) {
		t.Fatalf("resume result = %+v", result)
	}
	actors := NewSQLActors(db)
	for _, actor := range corpus {
		fileState, _, err := (FileStore{Path: filepath.Join(actorDir, actorFileName(actor.chatID))}).Load(actor.chatID)
		if err != nil {
			t.Fatal(err)
		}
		sqlState, found, err := actors.Store(actor.chatID).Load(actor.chatID)
		if err != nil || !found {
			t.Fatalf("%s after kill+resume: found=%v err=%v", actor.shape, found, err)
		}
		assertSameDurableState(t, actor.shape+" after kill+resume", fileState, sqlState, strings.Contains(actor.shape, "journal"))
	}
	if err := db.QuickCheck(); err != nil {
		t.Fatal(err)
	}
}
