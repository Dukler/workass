package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func startTrackedSubagentForTest(manager *Manager, tabID, chatID, id, label string, startedAt time.Time) {
	manager.registerSubagentSpawnedWork(tabID, chatID, SubagentRun{
		ID: id, Label: label, ProviderID: "claude", Status: "running",
		Phase: "working", LatestActivity: "Running delegated task",
		StartedAt: startedAt.UTC().Format(time.RFC3339Nano),
	})
}

func finishTrackedSubagentForTest(manager *Manager, tabID, chatID, id, status, result string) {
	activity := map[string]string{"done": "Completed", "failed": "Failed", "cancelled": "Cancelled"}[status]
	manager.settleSubagentSpawnedWork(tabID, chatID, SubagentRun{
		ID: id, Status: status, Phase: status, LatestActivity: activity,
		ModelLabel: "Opus4.8-xhigh", Result: result,
		FinishedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func TestSettledBackgroundWorkNeverExposesOrSchedulesWakeState(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir(), SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })

	addExternalRecordForTest(manager, "tab-terminal", "chat-terminal", "xw-terminal", "external", nil, "", "", "running", "")
	manager.spawnedWorkMu.Lock()
	if !manager.settleSpawnedWorkLocked(manager.spawnedWork[spawnedWorkKey("tab-terminal", "chat-terminal", "xw-terminal")], "exited", nil) {
		manager.spawnedWorkMu.Unlock()
		t.Fatal("external settle returned false")
	}
	manager.spawnedWorkMu.Unlock()

	startTrackedSubagentForTest(manager, "tab-terminal", "chat-terminal", "wa-terminal", "subagent", time.Now().Add(-time.Minute))
	finishTrackedSubagentForTest(manager, "tab-terminal", "chat-terminal", "wa-terminal", "done", "finished")

	encoded, err := json.Marshal(manager.ListSpawnedWork("tab-terminal", "chat-terminal"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"wake"`) {
		t.Fatalf("settled background work exposed synthetic wake state: %s", encoded)
	}
	for _, item := range manager.ListSpawnedWork("tab-terminal", "chat-terminal") {
		if durableSpawnedWorkPriority(item) {
			t.Fatalf("settled background work blocked an update: %#v", item)
		}
	}
	if manager.chatHasLiveParkEvidence("tab-terminal", "chat-terminal") {
		t.Fatal("settled background work remained live park evidence")
	}
}

func TestLegacyPendingWakeSnapshotIsIgnored(t *testing.T) {
	stateDir := t.TempDir()
	tabID, chatID := "tab-legacy-wake", "chat-legacy-wake"
	dir := filepath.Join(stateDir, "spawned-work")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	raw := []map[string]any{{
		"id": "xw-legacy", "taskId": "xw-legacy", "tabId": tabID, "chatId": chatID,
		"providerId": "codex", "kind": "external", "label": "legacy lane",
		"status": "exited", "startedAt": now, "updatedAt": now, "finishedAt": now,
		"wake": "pending",
	}}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, safeArchiveName(tabID)+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(Options{StateDir: stateDir, SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	items := manager.ListSpawnedWork(tabID, chatID)
	encoded, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || strings.Contains(string(encoded), `"wake"`) || durableSpawnedWorkPriority(items[0]) {
		t.Fatalf("legacy wake state survived load: items=%#v json=%s", items, encoded)
	}
}

func TestTrackedSubagentSettleCarriesModelAndResultOnTheWire(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir(), SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	const tabID, chatID, id = "tab-sub-wire", "chat-sub-wire", "wa-subagent-wire-1"
	bindExternalWorkOwnerForTest(manager, "owner-wire", chatID, tabID, "claude")

	startTrackedSubagentForTest(manager, tabID, chatID, id, "wire subagent", time.Now().Add(-time.Minute))
	finishTrackedSubagentForTest(manager, tabID, chatID, id, "done", "the answer is 42 token=sk-should-not-survive")

	rows, err := manager.ListSpawnedWorkForOwner("owner-wire", chatID, tabID, chatID, tabID, 0)
	if err != nil {
		t.Fatalf("owner spawned-work listing: %v", err)
	}
	var row map[string]any
	for _, candidate := range rows {
		if asString(candidate["taskId"]) == id {
			row = candidate
		}
	}
	if row == nil {
		t.Fatalf("subagent row missing from owner listing: %#v", rows)
	}
	if asString(row["modelLabel"]) != "Opus4.8-xhigh" {
		t.Fatalf("subagent row carried no model label: %#v", row)
	}
	excerpt := asString(row["resultExcerpt"])
	if !strings.Contains(excerpt, "the answer is 42") {
		t.Fatalf("subagent row carried no result excerpt: %#v", row)
	}
	if strings.Contains(excerpt, "sk-should-not-survive") || !strings.Contains(strings.ToLower(excerpt), "[redacted]") {
		t.Fatalf("subagent result excerpt was not redacted: %q", excerpt)
	}
	if len(excerpt) > maxSpawnedWorkResultExcerpt {
		t.Fatalf("subagent result excerpt is unbounded: %d bytes", len(excerpt))
	}
}
