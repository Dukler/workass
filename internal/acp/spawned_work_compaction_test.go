package acp

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestActorSpawnedWorkProjectionPreservesAcceptedRunningAndSharedTabRows(t *testing.T) {
	stateDir := t.TempDir()
	manager := NewManager(Options{StateDir: stateDir, RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour})
	defer manager.Reset()

	const tabID = "shared-tab"
	const chatID = "chat"
	const otherChatID = "other-chat"
	addExternalRecordForTest(manager, tabID, chatID, "running-work", "running", nil, "", "", "running", "")
	addExternalRecordForTest(manager, tabID, chatID, "orphan-work", "orphan", nil, "", "", "running", "")
	addExternalRecordForTest(manager, tabID, chatID, "settled-work", "settled", nil, "", "", "exited", "")
	addExternalRecordForTest(manager, tabID, otherChatID, "other-work", "other", nil, "", "", "running", "")
	if err := manager.persistSpawnedWorkSnapshot(tabID); err != nil {
		t.Fatal(err)
	}
	accepted := []SpawnedWorkItem{{ID: "running-work", TaskID: "running-work", TabID: tabID, ChatID: chatID, Status: "running"}}
	if err := manager.CommitActorSpawnedWorkProjection(tabID, chatID, accepted); err != nil {
		t.Fatal(err)
	}
	if got := manager.ListSpawnedWork(tabID, chatID); len(got) != 1 || got[0].TaskID != "running-work" {
		t.Fatalf("target cache after compaction = %#v", got)
	}
	if got := manager.ListSpawnedWork(tabID, otherChatID); len(got) != 1 || got[0].TaskID != "other-work" {
		t.Fatalf("shared-tab survivor after compaction = %#v", got)
	}
	raw, err := os.ReadFile(manager.spawnedWorkSnapshotPath(tabID))
	if err != nil {
		t.Fatal(err)
	}
	var stored []spawnedWorkSnapshotItem
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("durable running cache = %#v", stored)
	}

	manager.Reset()
	restarted := NewManager(Options{StateDir: stateDir, RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour})
	defer restarted.Reset()
	if got := restarted.ListSpawnedWork(tabID, chatID); len(got) != 1 || got[0].TaskID != "running-work" {
		t.Fatalf("compacted cache after restart = %#v", got)
	}
}

func TestSpawnedWorkCommitCompactsOnlyAfterActorAcceptsSnapshot(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir(), RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour})
	defer manager.Reset()
	const tabID = "tab"
	const chatID = "chat"
	addExternalRecordForTest(manager, tabID, chatID, "settled-work", "settled", nil, "", "", "exited", "")

	observed := 0
	if err := manager.InstallSpawnedWorkObserver(func(gotTabID, gotChatID string, items []SpawnedWorkItem) (SpawnedWorkActorProjection, error) {
		if gotTabID != tabID || gotChatID != chatID || len(items) != 1 || items[0].TaskID != "settled-work" {
			t.Fatalf("actor snapshot = tab:%q chat:%q items:%#v", gotTabID, gotChatID, items)
		}
		observed++
		return SpawnedWorkActorProjection{ActorRevision: 7, Items: items}, nil
	}); err != nil {
		t.Fatal(err)
	}
	manager.commitSpawnedWorkChange(tabID, chatID)
	if observed != 1 {
		t.Fatalf("actor observations = %d", observed)
	}
	if got := manager.ListSpawnedWork(tabID, chatID); len(got) != 0 {
		t.Fatalf("settled delivery cache survived actor commit: %#v", got)
	}
	raw, err := os.ReadFile(manager.spawnedWorkReceiptPath(tabID))
	if err != nil {
		t.Fatal(err)
	}
	if !containsSpawnedWorkReceipt(raw, "spawned-settled-work") {
		t.Fatalf("settled receipt was not durable before compaction: %s", raw)
	}
}

func TestSpawnedWorkCommitDoesNotPersistBeforeActorAcceptance(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir(), RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour})
	defer manager.Reset()
	const tabID = "tab"
	const chatID = "chat"
	addExternalRecordForTest(manager, tabID, chatID, "unaccepted-work", "unaccepted", nil, "", "", "running", "")
	if err := manager.InstallSpawnedWorkObserver(func(string, string, []SpawnedWorkItem) (SpawnedWorkActorProjection, error) {
		return SpawnedWorkActorProjection{}, os.ErrInvalid
	}); err != nil {
		t.Fatal(err)
	}
	manager.commitSpawnedWorkChange(tabID, chatID)
	if _, err := os.Stat(manager.spawnedWorkSnapshotPath(tabID)); !os.IsNotExist(err) {
		t.Fatalf("executor cache was persisted before actor acceptance: %v", err)
	}
}

func containsSpawnedWorkReceipt(data []byte, receiptID string) bool {
	for _, line := range boundedSpawnedReceiptLines(data) {
		var receipt SpawnedWorkReceipt
		if json.Unmarshal(line, &receipt) == nil && receipt.ReceiptID == receiptID {
			return true
		}
	}
	return false
}
