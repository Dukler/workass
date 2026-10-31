package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Live bug (2026-10-31, 18:36Z): a real daemon restart left a tracked
// subagent's row "failed" with the right summary, but its owner chat got no
// notice at all. loadSpawnedWorkSnapshots settled the row and published
// spawned-work:changed, which only updates the renderer projection; nothing
// wrote the durable SubagentReceipt that SetChatEnvObserver's startup replay
// turns into the owner's <task-notification>.
func TestSpawnedWorkOrphanedSubagentLeavesAPendingNoticeForItsOwner(t *testing.T) {
	t.Parallel()
	const taskID = "wa-subagent-1793470955160-1"
	stateDir := t.TempDir()
	item := SpawnedWorkItem{
		ID: taskID, TaskID: taskID, TabID: "tab-orphan", ChatID: "chat-orphan",
		ProviderID: "claude", Kind: trackedSubagentSpawnedWorkKind,
		Label: "Map Sol repair pipeline", ModelLabel: "Opus4.8-high", Status: "running",
		StartedAt: "2026-10-31T18:30:00Z", UpdatedAt: "2026-10-31T18:35:00Z",
		OriginLaneID: "lane-fixture", OriginOperationID: "op-fixture",
	}
	data, err := json.Marshal([]SpawnedWorkItem{item})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(stateDir, "spawned-work")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, safeArchiveName("tab-orphan")+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	// NewManager runs loadSpawnedWorkSnapshots synchronously: this is the
	// exact restart-recovery path, not a simulation of it.
	manager := NewManager(Options{StateDir: stateDir, SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })

	restored := manager.ListSpawnedWork("tab-orphan", "chat-orphan")
	if len(restored) != 1 || restored[0].Status != "orphaned" {
		t.Fatalf("restored row = %#v, want exactly one orphaned row", restored)
	}

	pending := manager.pendingSubagentCompletions()
	if len(pending) != 1 {
		t.Fatalf("pending subagent completions = %#v, want exactly one durable receipt owed to the owner chat", pending)
	}
	receipt := pending[0]
	if receipt.ParentTabID != "tab-orphan" || receipt.ParentChatID != "chat-orphan" {
		t.Fatalf("receipt owner = %#v, want tab-orphan/chat-orphan", receipt)
	}
	if receipt.Status != "failed" || !receipt.DeliveryPending {
		t.Fatalf("receipt = %#v, want a pending failed receipt", receipt)
	}

	// SetChatEnvObserver's replay is the only thing that turns a pending
	// receipt into the owner's actual <task-notification>; it must fire
	// exactly once for this run.
	delivered := 0
	manager.SetChatEnvObserver(func(ChatEnvPayload) error { return nil }, func(tabID, chatID string, receipt SubagentReceipt) error {
		delivered++
		if tabID != "tab-orphan" || chatID != "chat-orphan" || receipt.ReceiptID != taskID {
			t.Fatalf("delivered to the wrong owner: tab=%s chat=%s receipt=%#v", tabID, chatID, receipt)
		}
		return nil
	})
	if delivered != 1 {
		t.Fatalf("delivered = %d, want exactly one notice", delivered)
	}
	if again := manager.pendingSubagentCompletions(); len(again) != 0 {
		t.Fatalf("receipt still pending after successful delivery: %#v", again)
	}
}
