package acp

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestPruneSpawnedWorkForMigrationPreservesSharedTabSurvivorAndRestart(t *testing.T) {
	stateDir := t.TempDir()
	manager := NewManager(Options{StateDir: stateDir, RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour})
	defer manager.Reset()

	const tabID = "reused-tab"
	const staleChatID = "stale-chat"
	const currentChatID = "current-chat"
	addExternalRecordForTest(manager, tabID, staleChatID, "stale-work", "stale", nil, "", "", "running", "")
	addExternalRecordForTest(manager, tabID, currentChatID, "current-work", "current", nil, "", "", "running", "")
	if err := manager.persistSpawnedWorkSnapshot(tabID, staleChatID); err != nil {
		t.Fatalf("persist shared tab snapshot: %v", err)
	}

	receiptPath := manager.spawnedWorkReceiptPath(tabID)
	if err := os.MkdirAll(filepath.Dir(receiptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	currentReceipt, err := json.Marshal(SpawnedWorkReceipt{
		ReceiptID: "current-receipt", TaskID: "current-work", TabID: tabID, ChatID: currentChatID,
		Status: "exited",
	})
	if err != nil {
		t.Fatal(err)
	}
	staleReceipt, err := json.Marshal(SpawnedWorkReceipt{
		ReceiptID: "stale-receipt", TaskID: "stale-work", TabID: tabID, ChatID: staleChatID,
		Status: "exited",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receiptPath, append(append(currentReceipt, '\n'), append(staleReceipt, '\n')...), 0o600); err != nil {
		t.Fatal(err)
	}

	// A receipt can outlive a malformed/empty executor snapshot. The migration
	// inventory must still find and remove its exact pair.
	receiptOnlyTab := "receipt-only-old-tab"
	receiptOnlyPath := filepath.Join(stateDir, "spawned-work-receipts", receiptOnlyTab+".jsonl")
	receiptOnly, err := json.Marshal(SpawnedWorkReceipt{
		ReceiptID: "receipt-only", TaskID: "receipt-only", TabID: receiptOnlyTab, ChatID: staleChatID,
		Status: "exited",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(receiptOnlyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receiptOnlyPath, append(receiptOnly, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	pairs := manager.LegacySpawnedWorkPairsForMigration()
	if !hasSpawnedWorkPair(pairs, tabID, staleChatID) || !hasSpawnedWorkPair(pairs, tabID, currentChatID) ||
		!hasSpawnedWorkPair(pairs, receiptOnlyTab, staleChatID) {
		t.Fatalf("migration inventory omitted a loaded/saved pair: %#v", pairs)
	}
	if err := manager.PruneSpawnedWorkForMigration(tabID, staleChatID); err != nil {
		t.Fatalf("prune stale shared-tab pair: %v", err)
	}
	if err := manager.PruneSpawnedWorkForMigration(receiptOnlyTab, staleChatID); err != nil {
		t.Fatalf("prune receipt-only pair: %v", err)
	}

	if got := manager.ListSpawnedWork(tabID, staleChatID); len(got) != 0 {
		t.Fatalf("stale in-memory executor record survived: %#v", got)
	}
	if got := manager.ListSpawnedWork(tabID, currentChatID); len(got) != 1 || got[0].TaskID != "current-work" {
		t.Fatalf("current in-memory executor record was not preserved: %#v", got)
	}
	snapshot, err := os.ReadFile(manager.spawnedWorkSnapshotPath(tabID))
	if err != nil {
		t.Fatalf("read surviving shared-tab snapshot: %v", err)
	}
	var stored []spawnedWorkSnapshotItem
	if err := json.Unmarshal(snapshot, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].ChatID != currentChatID || stored[0].TaskID != "current-work" {
		t.Fatalf("shared-tab snapshot lost or retained the wrong record: %#v", stored)
	}
	receipts, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatalf("read surviving receipt file: %v", err)
	}
	if string(receipts) == "" || string(receipts) == string(staleReceipt)+"\n" ||
		containsReceiptID(receipts, "stale-receipt") || !containsReceiptID(receipts, "current-receipt") {
		t.Fatalf("receipt filter did not preserve only the survivor: %s", receipts)
	}
	if _, err := os.Stat(receiptOnlyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("receipt-only stale file survived: %v", err)
	}

	manager.Reset()
	restarted := NewManager(Options{StateDir: stateDir, RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour})
	defer restarted.Reset()
	if got := restarted.ListSpawnedWork(tabID, staleChatID); len(got) != 0 {
		t.Fatalf("stale executor record returned after restart: %#v", got)
	}
	if got := restarted.ListSpawnedWork(tabID, currentChatID); len(got) != 1 || got[0].TaskID != "current-work" {
		t.Fatalf("current executor record did not survive restart: %#v", got)
	}
}

func TestPruneSpawnedWorkForMigrationRacesLateCommitWithoutRecreatingStaleFile(t *testing.T) {
	stateDir := t.TempDir()
	manager := NewManager(Options{StateDir: stateDir, RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour})
	defer manager.Reset()
	const tabID = "race-old-tab"
	const chatID = "race-old-chat"
	addExternalRecordForTest(manager, tabID, chatID, "race-work", "race", nil, "", "", "running", "")
	if err := manager.persistSpawnedWorkSnapshot(tabID, chatID); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			manager.commitSpawnedWorkChange(tabID, chatID)
		}()
		go func() {
			defer wg.Done()
			if err := manager.PruneSpawnedWorkForMigration(tabID, chatID); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent stale-pair prune failed: %v", err)
	}
	if err := manager.PruneSpawnedWorkForMigration(tabID, chatID); err != nil {
		t.Fatal(err)
	}
	if got := manager.ListSpawnedWork(tabID, chatID); len(got) != 0 {
		t.Fatalf("stale record was recreated in memory after concurrent cleanup: %#v", got)
	}
	if _, err := os.Stat(manager.spawnedWorkSnapshotPath(tabID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale snapshot was recreated after concurrent cleanup: %v", err)
	}
}

func TestPruneSpawnedWorkForMigrationRemovesReceiptOnlyPairWithoutSnapshotDirectory(t *testing.T) {
	stateDir := t.TempDir()
	manager := NewManager(Options{StateDir: stateDir, RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour})
	defer manager.Reset()

	const tabID = "receipt-only-tab"
	const chatID = "receipt-only-chat"
	receiptPath := manager.spawnedWorkReceiptPath(tabID)
	if err := os.MkdirAll(filepath.Dir(receiptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	receipt, err := json.Marshal(SpawnedWorkReceipt{
		ReceiptID: "receipt-only", TaskID: "receipt-only", TabID: tabID, ChatID: chatID, Status: "exited",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receiptPath, append(receipt, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := manager.PruneSpawnedWorkForMigration(tabID, chatID); err != nil {
		t.Fatalf("prune receipt-only pair without a snapshot directory: %v", err)
	}
	if _, err := os.Stat(receiptPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("receipt-only stale file survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "spawned-work")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pruning an absent snapshot must not create the snapshot directory: %v", err)
	}
}

func hasSpawnedWorkPair(pairs []SpawnedWorkPair, tabID, chatID string) bool {
	for _, pair := range pairs {
		if pair.TabID == tabID && pair.ChatID == chatID {
			return true
		}
	}
	return false
}

func containsReceiptID(data []byte, receiptID string) bool {
	for _, line := range boundedSpawnedReceiptLines(data) {
		var receipt SpawnedWorkReceipt
		if json.Unmarshal(line, &receipt) == nil && receipt.ReceiptID == receiptID {
			return true
		}
	}
	return false
}
