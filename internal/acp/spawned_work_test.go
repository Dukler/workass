package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMockClaudeProviderForwardsSpawnedWorkWithoutAgentCooperation(t *testing.T) {
	root := repoRoot(t)
	manager := NewManager(Options{
		RootDir: root, StateDir: t.TempDir(), RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour,
		Provider: ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Label: "Workass Mock Claude ACP",
		},
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: "spawn-fixture", ChatID: "chat-spawn-fixture", ProviderID: "mock"})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	job := startAppChatJob(t, manager, session.SessionID, "spawn-fixture", "[mock:spawned-work] passive fixture")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		items := manager.ListSpawnedWork("spawn-fixture", "chat-spawn-fixture")
		if len(items) == 1 && items[0].Status == "exited" {
			if items[0].Kind != "bash" || items[0].OutputFile == "" || items[0].TaskID == "" {
				t.Fatalf("fixture item = %#v", items[0])
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("spawned work never settled; job=%#v items=%#v", job, manager.ListSpawnedWork("spawn-fixture", "chat-spawn-fixture"))
}

func TestMockClaudeProviderKeepsUnnotifiedBackgroundWorkRunningViaOutputOwner(t *testing.T) {
	root := repoRoot(t)
	manager := NewManager(Options{
		RootDir: root, StateDir: t.TempDir(), RuntimeProfile: "dev", SpawnedWorkReconcileInterval: 25 * time.Millisecond,
		Provider: ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Label: "Workass Mock Claude ACP",
		},
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: "spawn-running-fixture", ChatID: "chat-spawn-running-fixture", ProviderID: "mock"})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	startAppChatJob(t, manager, session.SessionID, "spawn-running-fixture", "[mock:spawned-work-running] passive fixture")
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		items := manager.ListSpawnedWork("spawn-running-fixture", "chat-spawn-running-fixture")
		if len(items) == 1 && items[0].Status == "running" && items[0].OutputFile != "" && items[0].PID != nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("unnotified background work did not reconcile: %#v", manager.ListSpawnedWork("spawn-running-fixture", "chat-spawn-running-fixture"))
}

func TestClaudeSteerCancelLeavesSpawnedWorkRunning(t *testing.T) {
	manager, events := newFakeManager(t, "claude-spawned-work-interrupt", Options{
		Provider: ProviderConfig{ID: "claude"},
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	const tabID = "spawn-steer-fixture"
	const chatID = "chat-spawn-steer-fixture"
	session, err := manager.NewSession(ctx, SessionOptions{TabID: tabID, ChatID: chatID, ProviderID: "claude", CWD: repoRoot(t)})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	job := startAppChatJob(t, manager, session.SessionID, tabID, "start background work and wait")
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		items := manager.ListSpawnedWork(tabID, chatID)
		if len(items) == 1 && items[0].Status == "running" && items[0].OutputFile != "" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	items := manager.ListSpawnedWork(tabID, chatID)
	if len(items) != 1 || items[0].Status != "running" {
		t.Fatalf("background work was not running before steer; job=%#v items=%#v", job, items)
	}
	res := manager.Steer(session.SessionID, "redirect parent while background continues", nil, "")
	if res["interrupted"] != true || res["strategy"] != "interrupt-queue" {
		t.Fatalf("claude steer fallback = %#v", res)
	}
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 5*time.Second), "failed", 130, "cancelled")
	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		items = manager.ListSpawnedWork(tabID, chatID)
		if len(items) == 1 && items[0].Status == "running" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("spawned work did not survive steer cancel: %#v", manager.ListSpawnedWork(tabID, chatID))
}

func TestProductionMockProviderCannotInjectSpawnedWork(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir(), RuntimeProfile: "prod", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	if manager.acceptsClaudeSpawnedWorkProvider("mock") {
		t.Fatal("production accepted mock spawned-work provider")
	}
	manager.observeSpawnToolEvent(spawnToolObservation{
		SessionID: "session-prod", TabID: "tab-prod", ChatID: "chat-prod", ProviderID: "mock", ToolCallID: "tool-prod",
		Title: "Bash", RawInput: map[string]any{"run_in_background": true},
	})
	if got := manager.ListSpawnedWork("tab-prod", "chat-prod"); len(got) != 0 {
		t.Fatalf("production accepted mock spawned-work fixture: %#v", got)
	}
}

func spawnedWorkTestOutput(t *testing.T, taskID string) string {
	t.Helper()
	// validateClaudeTaskOutputPath only trusts os.TempDir()/tmp roots. Under
	// GOTMPDIR (sandboxed gates) t.TempDir() moves elsewhere, so anchor the
	// fixture explicitly inside the allowed root.
	root, err := os.MkdirTemp(os.TempDir(), "spawned-work-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	path := filepath.Join(root, "claude-501", "project", "session", "tasks", taskID+".output")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("ready\napi_key=do-not-expose\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSpawnedWorkPassivelyTracksBackgroundBashAndWritesReceipt(t *testing.T) {
	stateDir := t.TempDir()
	output := spawnedWorkTestOutput(t, "bash-1")
	active := true
	var eventsMu sync.Mutex
	events := 0
	manager := NewManager(Options{
		StateDir: stateDir, SpawnedWorkReconcileInterval: time.Hour,
		SpawnedWorkPIDProbe: func(paths []string) (map[string][]int, bool) {
			if len(paths) != 1 || paths[0] != output {
				t.Fatalf("probe paths = %q, want %q", paths, output)
			}
			if active {
				return map[string][]int{output: {4102}}, true
			}
			return map[string][]int{output: {}}, true
		},
		Broadcast: func(channel string, payload any) {
			if channel == "spawned-work:changed" {
				eventsMu.Lock()
				events++
				eventsMu.Unlock()
			}
		},
	})

	manager.observeSpawnToolEvent(spawnToolObservation{
		SessionID: "session-1", TabID: "tab-1", ChatID: "chat-1", ProviderID: "claude", ToolCallID: "tool-1",
		Title: "Bash", Command: "python3 -m http.server 8737", RawInput: map[string]any{"run_in_background": true},
		Meta: map[string]any{"claudeCode": map[string]any{"toolName": "Bash"}},
	})
	manager.observeSpawnToolEvent(spawnToolObservation{
		SessionID: "session-1", TabID: "tab-1", ChatID: "chat-1", ProviderID: "claude", ToolCallID: "tool-1",
		Title: "Bash", Meta: map[string]any{"claudeCode": map[string]any{"toolName": "Bash"}},
		Output: "Command running in background with ID: bash-1\nOutput is being written to: " + output,
	})

	items := manager.ListSpawnedWork("tab-1", "chat-1")
	if len(items) != 1 || items[0].Kind != "bash" || items[0].Status != "running" || items[0].OutputFile != output {
		t.Fatalf("unexpected background Bash item: %#v", items)
	}
	manager.reconcileSpawnedWork()
	items = manager.ListSpawnedWork("tab-1", "chat-1")
	if items[0].PID == nil || *items[0].PID != 4102 {
		t.Fatalf("pid reconciliation = %#v", items[0].PID)
	}

	active = false
	manager.reconcileSpawnedWork()
	manager.spawnedWorkMu.Lock()
	manager.spawnedWork[spawnedWorkKey("tab-1", "chat-1", "bash-1")].MissingSince = time.Now().Add(-spawnedWorkMissingGrace - time.Second)
	manager.spawnedWorkMu.Unlock()
	manager.reconcileSpawnedWork()
	items = manager.ListSpawnedWork("tab-1", "chat-1")
	if items[0].Status != "exited" || items[0].FinishedAt == "" {
		t.Fatalf("terminal item = %#v", items[0])
	}

	manager.mu.Lock()
	manager.bindAgentOwnerLocked("owner-1", "chat-1", "tab-1")
	manager.mu.Unlock()
	receipts, err := manager.ListSpawnedWorkReceipts("owner-1", "chat-1", "tab-1", "chat-1", "tab-1", 32)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipts = %#v, err = %v", receipts, err)
	}
	if strings.Contains(receipts[0].OutputTail, "do-not-expose") || !strings.Contains(strings.ToLower(receipts[0].OutputTail), "[redacted]") {
		t.Fatalf("receipt tail was not redacted: %q", receipts[0].OutputTail)
	}
	if _, err := manager.ListSpawnedWorkForOwner("owner-1", "chat-1", "tab-1", "other", "tab-1", 0); err == nil {
		t.Fatal("cross-chat spawned-work read was accepted")
	}
	eventsMu.Lock()
	defer eventsMu.Unlock()
	if events < 2 {
		t.Fatalf("spawned-work change events = %d, want at least 2", events)
	}

	restored := NewManager(Options{StateDir: stateDir, SpawnedWorkReconcileInterval: time.Hour})
	restoredItems := restored.ListSpawnedWork("tab-1", "chat-1")
	if len(restoredItems) != 1 || restoredItems[0].Status != "exited" {
		t.Fatalf("restored items = %#v", restoredItems)
	}
}

func TestSpawnedWorkFallbackNormalizesSentencePunctuationAndMergesStructuredRecord(t *testing.T) {
	const taskID = "b7hm20ecb"
	output := spawnedWorkTestOutput(t, taskID)
	manager := NewManager(Options{StateDir: t.TempDir(), SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })

	manager.observeClaudeSpawnedWork("tab-real", "chat-real", "session-real", map[string]any{
		"type": "started", "taskId": taskID, "toolCallId": "tool-real",
		"description": "Run lab/prove feather_dose ladder third attempt", "taskType": "bash",
	})
	manager.observeSpawnToolEvent(spawnToolObservation{
		SessionID: "session-real", TabID: "tab-real", ChatID: "chat-real", ProviderID: "claude", ToolCallID: "tool-real",
		Title: "tool", Meta: map[string]any{"claudeCode": map[string]any{"toolName": "Bash"}},
		Output: "Command running in background with ID: " + taskID + ". Output is being written to: " + output,
	})

	items := manager.ListSpawnedWork("tab-real", "chat-real")
	if len(items) != 1 {
		t.Fatalf("spawned rows = %#v, want one merged record", items)
	}
	item := items[0]
	if item.ID != taskID || item.TaskID != taskID || item.ToolCallID != "tool-real" {
		t.Fatalf("merged identity = %#v", item)
	}
	if item.Label != "Run lab/prove feather_dose ladder third attempt" || item.Status != "running" || item.OutputFile != output {
		t.Fatalf("structured record did not remain authoritative: %#v", item)
	}
}

func TestSpawnedWorkSnapshotReloadHealsLegacyTrailingPunctuationDuplicate(t *testing.T) {
	const taskID = "b7hm20ecb"
	stateDir := t.TempDir()
	output := spawnedWorkTestOutput(t, taskID)
	items := []SpawnedWorkItem{
		{
			ID: taskID, TaskID: taskID, ToolCallID: "tool-real", TabID: "tab-real", ChatID: "chat-real", ProviderID: "claude",
			Kind: "bash", Label: "Run lab/prove feather_dose ladder third attempt", Status: "running",
			StartedAt: "2026-07-15T21:49:00Z", UpdatedAt: "2026-07-15T21:50:00Z",
		},
		{
			ID: taskID + ".", TaskID: taskID + ".", ToolCallID: "tool-real", TabID: "tab-real", ChatID: "chat-real", ProviderID: "claude",
			Kind: "background", Label: "tool", Status: "exited", OutputFile: output,
			StartedAt: "2026-07-15T21:49:01Z", UpdatedAt: "2026-07-15T21:49:05Z", FinishedAt: "2026-07-15T21:49:05Z",
		},
	}
	data, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(stateDir, "spawned-work")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, safeArchiveName("tab-real")+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(Options{StateDir: stateDir, SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	restored := manager.ListSpawnedWork("tab-real", "chat-real")
	if len(restored) != 1 {
		t.Fatalf("restored rows = %#v, want one healed record", restored)
	}
	item := restored[0]
	if item.ID != taskID || item.TaskID != taskID || item.Label != "Run lab/prove feather_dose ladder third attempt" || item.Status != "running" || item.OutputFile != output {
		t.Fatalf("healed record = %#v", item)
	}
}

func TestSpawnedWorkStructuredAgentWorkflowAndSnapshotLifecycle(t *testing.T) {
	manager := NewManager(Options{
		StateDir: t.TempDir(), SpawnedWorkReconcileInterval: time.Hour,
		SpawnedWorkPIDProbe: func([]string) (map[string][]int, bool) { return nil, false },
	})
	manager.observeClaudeSpawnedWork("tab-2", "chat-2", "session-2", map[string]any{
		"type": "started", "taskId": "agent-1", "toolCallId": "tool-agent", "description": "Review UI", "taskType": "agent", "subagentType": "Explore",
	})
	manager.observeClaudeSpawnedWork("tab-2", "chat-2", "session-2", map[string]any{
		"type": "started", "taskId": "flow-1", "description": "Run validation workflow", "taskType": "local_workflow",
	})
	items := manager.ListSpawnedWork("tab-2", "chat-2")
	kinds := map[string]string{}
	for _, item := range items {
		kinds[item.TaskID] = item.Kind
	}
	if len(items) != 2 || kinds["flow-1"] != "workflow" || kinds["agent-1"] != "agent" {
		t.Fatalf("structured items = %#v", items)
	}

	manager.observeClaudeSpawnedWork("tab-2", "chat-2", "session-2", map[string]any{
		"type": "notification", "taskId": "agent-1", "status": "failed", "summary": "review failed",
	})
	manager.observeClaudeSpawnedWork("tab-2", "chat-2", "session-2", map[string]any{
		"type": "snapshot", "tasks": []any{},
	})
	manager.spawnedWorkMu.Lock()
	flow := manager.spawnedWork[spawnedWorkKey("tab-2", "chat-2", "flow-1")]
	flow.MissingSince = time.Now().Add(-spawnedWorkMissingGrace - time.Second)
	manager.spawnedWorkMu.Unlock()
	manager.reconcileSpawnedWork()

	items = manager.ListSpawnedWork("tab-2", "chat-2")
	statuses := map[string]string{}
	for _, item := range items {
		statuses[item.TaskID] = item.Status
	}
	if statuses["agent-1"] != "failed" || statuses["flow-1"] != "exited" {
		t.Fatalf("terminal structured statuses = %#v", statuses)
	}
}

func TestSpawnedWorkRejectsUntrustedOutputPaths(t *testing.T) {
	if _, ok := validateClaudeTaskOutputPath("task-1", "/etc/task-1.output"); ok {
		t.Fatal("accepted output outside Claude temp task directory")
	}
	if _, ok := validateClaudeTaskOutputPath("../task-1", filepath.Join(os.TempDir(), "claude-501", "tasks", "task-1.output")); ok {
		t.Fatal("accepted path traversal task id")
	}
}

func TestSpawnedWorkBatchProbeFindsOpenOutputOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "open.output")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	pidsByPath, supported := spawnedWorkPIDsForOutputs([]string{path})
	if !supported {
		t.Skip("output-file PID probe is unavailable on this platform")
	}
	want := os.Getpid()
	for _, pid := range pidsByPath[path] {
		if pid == want {
			return
		}
	}
	t.Fatalf("open output owners = %#v, want current pid %d", pidsByPath, want)
}

func TestSpawnedWorkRestoredPathlessRecordBecomesOrphanedButLiveSilentRecordStaysRunning(t *testing.T) {
	manager := NewManager(Options{
		RootDir: repoRoot(t), StateDir: t.TempDir(), RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour,
		Provider: ProviderConfig{ID: "claude"},
	})
	t.Cleanup(func() { manager.Reset() })
	const tabID = "pathless-fixture"
	const chatID = "chat-pathless-fixture"
	now := time.Now().UTC()
	mk := func(taskID, sessionID string) *spawnedWorkRecord {
		return &spawnedWorkRecord{
			SessionID: sessionID,
			Item: SpawnedWorkItem{
				ID: taskID, TaskID: taskID, TabID: tabID, ChatID: chatID, ProviderID: "claude",
				Kind: "bash", Label: "ghost " + taskID, Status: "running",
				StartedAt: now.Add(-time.Hour).Format(time.RFC3339Nano),
				UpdatedAt: now.Add(-time.Hour).Format(time.RFC3339Nano),
			},
		}
	}
	ghost := mk("ghost1", "")
	live := mk("live1", "live-session")
	manager.spawnedWorkMu.Lock()
	manager.spawnedWork[spawnedWorkKey(tabID, chatID, "ghost1")] = ghost
	manager.spawnedWork[spawnedWorkKey(tabID, chatID, "live1")] = live
	manager.spawnedWorkMu.Unlock()

	// First reconcile only arms the grace clock; nothing settles.
	manager.reconcileSpawnedWork()
	items := manager.ListSpawnedWork(tabID, chatID)
	for _, item := range items {
		if item.Status != "running" {
			t.Fatalf("record %s settled before the pathless grace elapsed: %#v", item.TaskID, item)
		}
	}
	// Backdate the armed clock past the grace: the restored ownerless record
	// becomes orphaned, while a live-session task remains running regardless
	// of how long it has been silent.
	manager.spawnedWorkMu.Lock()
	ghost.PathlessSince = now.Add(-2 * spawnedWorkPathlessGrace)
	live.PathlessSince = now.Add(-2 * spawnedWorkPathlessGrace)
	manager.spawnedWorkMu.Unlock()
	manager.reconcileSpawnedWork()
	byID := map[string]SpawnedWorkItem{}
	for _, item := range manager.ListSpawnedWork(tabID, chatID) {
		byID[item.TaskID] = item
	}
	if got := byID["ghost1"]; got.Status != "orphaned" || got.Summary == "" {
		t.Fatalf("pathless restored record did not become orphaned with a summary: %#v", got)
	}
	if got := byID["live1"]; got.Status != "running" {
		t.Fatalf("silent record with a live session owner must stay running: %#v", got)
	}
}

func TestTrackedSubagentParentEngineExitDoesNotOrphan(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir(), SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })

	addSpawnedWorkRecordForTest(t, manager, "tab-subagent-engine", "chat-subagent-engine", "child-engine", "subagent", "running")
	if manager.orphanInProcessSpawnedWorkForChat("tab-subagent-engine", "chat-subagent-engine", "engine-reset") {
		t.Fatal("parent engine exit classified tracked subagent as in-process work")
	}
	engineItem := spawnedWorkItemsByTaskID(manager.ListSpawnedWork("tab-subagent-engine", "chat-subagent-engine"))["child-engine"]
	if engineItem.Status != "running" || engineItem.FinishedAt != "" {
		t.Fatalf("parent engine exit settled tracked subagent: %#v", engineItem)
	}
}

func TestTrackedSubagentReconcileSilenceDoesNotOrphan(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir(), SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })

	addSpawnedWorkRecordForTest(t, manager, "tab-subagent-reconcile", "chat-subagent-reconcile", "child-reconcile", "subagent", "running")
	manager.spawnedWorkMu.Lock()
	reconcileRec := manager.spawnedWork[spawnedWorkKey("tab-subagent-reconcile", "chat-subagent-reconcile", "child-reconcile")]
	reconcileRec.SessionID = ""
	reconcileRec.PathlessSince = time.Now().Add(-2 * spawnedWorkPathlessGrace)
	manager.spawnedWorkMu.Unlock()
	manager.reconcileSpawnedWork()
	reconcileItem := spawnedWorkItemsByTaskID(manager.ListSpawnedWork("tab-subagent-reconcile", "chat-subagent-reconcile"))["child-reconcile"]
	if reconcileItem.Status != "running" || reconcileItem.FinishedAt != "" {
		t.Fatalf("pathless reconcile silence settled tracked subagent: %#v", reconcileItem)
	}
}

func TestTrackedSubagentSnapshotRestartsAsOrphanedInsteadOfPhantomRunning(t *testing.T) {
	stateDir := t.TempDir()
	const tabID = "tab-subagent-restart"
	const chatID = "chat-subagent-restart"
	const runID = "wa-subagent-restart-1"
	now := time.Now().UTC()
	items := []SpawnedWorkItem{{
		ID: runID, TaskID: runID, TabID: tabID, ChatID: chatID, ProviderID: "codex",
		Kind: "subagent", Label: "restart-owned child", Status: "running",
		StartedAt: now.Add(-time.Minute).Format(time.RFC3339Nano),
		UpdatedAt: now.Add(-time.Second).Format(time.RFC3339Nano),
		Summary:   "Running delegated task", LastToolName: "working",
	}}
	data, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(stateDir, "spawned-work")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, safeArchiveName(tabID)+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(Options{StateDir: stateDir, SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	restored := spawnedWorkItemsByTaskID(manager.ListSpawnedWork(tabID, chatID))[runID]
	if restored.Status != "orphaned" || restored.FinishedAt == "" ||
		!strings.Contains(restored.Summary, "daemon restart") || restored.LastToolName != "orphaned" {
		t.Fatalf("restored tracked subagent remained a phantom: %#v", restored)
	}

	snapshotData, err := os.ReadFile(manager.spawnedWorkSnapshotPath(tabID))
	if err != nil {
		t.Fatalf("read healed snapshot: %v", err)
	}
	if strings.Contains(string(snapshotData), `"status":"running"`) {
		t.Fatalf("healed snapshot retained phantom running status: %s", snapshotData)
	}
	receiptData, err := os.ReadFile(manager.spawnedWorkReceiptPath(tabID))
	if err != nil {
		t.Fatalf("read restart orphan receipt: %v", err)
	}
	if !strings.Contains(string(receiptData), `"status":"orphaned"`) {
		t.Fatalf("restart orphan receipt = %s", receiptData)
	}
}

func TestLifecycleSilentPathlessSpawnedWorkRemainsPinnedBeyondTTL(t *testing.T) {
	manager, events := newFakeManager(t, "echo-prompt", Options{
		HibernateTTL:                 time.Millisecond,
		LifecycleCheckInterval:       time.Hour,
		RSSSampleInterval:            time.Hour,
		SpawnedWorkReconcileInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	const tabID = "spawn-silent-pin-tab"
	chatID := "chat-" + tabID
	session := newFakeSession(t, manager, tabID)
	job := startAppChatJob(t, manager, session.SessionID, tabID, "finish before silent background work")
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 2*time.Second), "done", 0, "end_turn")
	_ = waitProcState(t, manager, StateIdle, 500*time.Millisecond)
	bridge := bridgeForTestSession(t, manager, session.SessionID, tabID, chatID)

	addSpawnedWorkRecordForTest(t, manager, tabID, chatID, "wf-silent", "workflow", "running")
	manager.spawnedWorkMu.Lock()
	silent := manager.spawnedWork[spawnedWorkKey(tabID, chatID, "wf-silent")]
	silent.LastLevelSeen = time.Now().Add(-time.Hour)
	silent.PathlessSince = time.Now().Add(-2 * spawnedWorkPathlessGrace)
	manager.spawnedWorkMu.Unlock()

	manager.reconcileSpawnedWork()
	setBridgeLastActivityForTest(bridge, time.Now().Add(-time.Hour))
	manager.SweepLifecycle()

	item := spawnedWorkItemsByTaskID(manager.ListSpawnedWork(tabID, chatID))["wf-silent"]
	if item.Status != "running" {
		t.Fatalf("silence was treated as a terminal signal: %#v", item)
	}
	if bridgeStateForTest(bridge) == StateHibernated {
		t.Fatalf("silent background work did not pin the bridge beyond the idle TTL: %#v", manager.Processes())
	}
}

func TestLifecycleRunningSpawnedWorkBlocksIdleTTLHibernation(t *testing.T) {
	logs := &spawnedWorkLifecycleLog{}
	manager, events := newFakeManager(t, "echo-prompt", Options{
		HibernateTTL:                 time.Millisecond,
		LifecycleCheckInterval:       time.Hour,
		RSSSampleInterval:            time.Hour,
		SpawnedWorkReconcileInterval: time.Hour,
		Logf:                         logs.Logf,
	})
	t.Cleanup(func() { manager.Reset() })

	const tabID = "spawn-pin-idle-tab"
	chatID := "chat-" + tabID
	session := newFakeSession(t, manager, tabID)
	job := startAppChatJob(t, manager, session.SessionID, tabID, "finish before spawned work pins")
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 2*time.Second), "done", 0, "end_turn")
	_ = waitProcState(t, manager, StateIdle, 500*time.Millisecond)
	bridge := bridgeForTestSession(t, manager, session.SessionID, tabID, chatID)

	addSpawnedWorkRecordForTest(t, manager, tabID, chatID, "wf-pin", "workflow", "running")
	setBridgeLastActivityForTest(bridge, time.Now().Add(-time.Hour))
	manager.SweepLifecycle()

	if procHasState(manager, StateHibernated) {
		t.Fatalf("running spawned workflow did not pin idle bridge: %#v", manager.Processes())
	}
	if !logs.hasSpawnedHibernateAbort(hibernateReasonIdleTTL) {
		t.Fatalf("missing spawned-work hibernate abort log: %#v", logs.entriesSnapshot())
	}
}

func TestLifecycleSettledSpawnedWorkAllowsIdleTTLHibernation(t *testing.T) {
	manager, events := newFakeManager(t, "echo-prompt", Options{
		HibernateTTL:                 time.Millisecond,
		LifecycleCheckInterval:       time.Hour,
		RSSSampleInterval:            time.Hour,
		SpawnedWorkReconcileInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	const tabID = "spawn-settled-idle-tab"
	chatID := "chat-" + tabID
	session := newFakeSession(t, manager, tabID)
	job := startAppChatJob(t, manager, session.SessionID, tabID, "settle before hibernate")
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 2*time.Second), "done", 0, "end_turn")
	_ = waitProcState(t, manager, StateIdle, 500*time.Millisecond)
	bridge := bridgeForTestSession(t, manager, session.SessionID, tabID, chatID)

	addSpawnedWorkRecordForTest(t, manager, tabID, chatID, "wf-settled", "workflow", "running")
	setBridgeLastActivityForTest(bridge, time.Now().Add(-time.Hour))
	manager.SweepLifecycle()
	if procHasState(manager, StateHibernated) {
		t.Fatalf("bridge hibernated while spawned workflow was running: %#v", manager.Processes())
	}

	settleSpawnedWorkRecordForTest(t, manager, tabID, chatID, "wf-settled", "exited")
	setBridgeLastActivityForTest(bridge, time.Now().Add(-time.Hour))
	manager.SweepLifecycle()
	if !procHasState(manager, StateHibernated) {
		t.Fatalf("settled spawned workflow did not allow hibernation: %#v", manager.Processes())
	}
}

func TestBridgeCloseOrphansInProcessSpawnedWork(t *testing.T) {
	stateDir := t.TempDir()
	manager, _ := newFakeManager(t, "echo-prompt", Options{
		StateDir:                     stateDir,
		LifecycleCheckInterval:       time.Hour,
		RSSSampleInterval:            time.Hour,
		SpawnedWorkReconcileInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	const tabID = "spawn-orphan-tab"
	const chatID = "spawn-orphan-chat"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: tabID, ChatID: chatID})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	bridge := bridgeForTestSession(t, manager, session.SessionID, tabID, chatID)
	addSpawnedWorkRecordForTest(t, manager, tabID, chatID, "wf-orphan", "workflow", "running")
	addSpawnedWorkRecordForTest(t, manager, tabID, chatID, "agent-orphan", "agent", "running")
	addSpawnedWorkRecordForTest(t, manager, tabID, chatID, "bash-orphan", "bash", "running")

	bridge.Close(true, errors.New("test forced close"))

	items := spawnedWorkItemsByTaskID(manager.ListSpawnedWork(tabID, chatID))
	wantSummary := "Orphaned: the ACP engine exited while this ran in-process (reason: test forced close)"
	for _, taskID := range []string{"wf-orphan", "agent-orphan", "bash-orphan"} {
		item := items[taskID]
		if item.Status != "orphaned" || item.FinishedAt == "" || item.Summary != wantSummary {
			t.Fatalf("%s was not orphaned with summary: %#v", taskID, item)
		}
	}

	data, err := os.ReadFile(manager.spawnedWorkReceiptPath(tabID))
	if err != nil {
		t.Fatalf("read orphan receipts: %v", err)
	}
	receiptStatus := map[string]string{}
	for _, line := range boundedSpawnedReceiptLines(data) {
		var receipt SpawnedWorkReceipt
		if err := json.Unmarshal(line, &receipt); err != nil {
			t.Fatalf("decode receipt: %v", err)
		}
		receiptStatus[receipt.TaskID] = receipt.Status
	}
	if receiptStatus["wf-orphan"] != "orphaned" || receiptStatus["agent-orphan"] != "orphaned" || receiptStatus["bash-orphan"] != "orphaned" {
		t.Fatalf("orphan receipts = %#v", receiptStatus)
	}
}

func TestCommitSpawnedWorkChangeAdvancesBridgeLastActivity(t *testing.T) {
	manager, _ := newFakeManager(t, "echo-prompt", Options{
		LifecycleCheckInterval:       time.Hour,
		RSSSampleInterval:            time.Hour,
		SpawnedWorkReconcileInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	const tabID = "spawn-activity-tab"
	const chatID = "spawn-activity-chat"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: tabID, ChatID: chatID})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	bridge := bridgeForTestSession(t, manager, session.SessionID, tabID, chatID)
	before := time.Now().Add(-time.Hour)
	setBridgeLastActivityForTest(bridge, before)

	manager.commitSpawnedWorkChange(tabID, chatID)

	if got := bridgeLastActivityForTest(bridge); !got.After(before) {
		t.Fatalf("lastActivity = %s, want after %s", got.Format(time.RFC3339Nano), before.Format(time.RFC3339Nano))
	}
}

func TestLifecycleRecycleAtIdleBlockedBySpawnedWork(t *testing.T) {
	logs := &spawnedWorkLifecycleLog{}
	manager, events := newFakeManager(t, "echo-prompt", Options{
		HibernateTTL:                 time.Hour,
		LifecycleCheckInterval:       time.Hour,
		RSSSampleInterval:            time.Hour,
		SpawnedWorkReconcileInterval: time.Hour,
		Logf:                         logs.Logf,
	})
	t.Cleanup(func() { manager.Reset() })

	const tabID = "spawn-recycle-tab"
	chatID := "chat-" + tabID
	session := newFakeSession(t, manager, tabID)
	job := startAppChatJob(t, manager, session.SessionID, tabID, "finish before recycle mark")
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 2*time.Second), "done", 0, "end_turn")
	_ = waitProcState(t, manager, StateIdle, 500*time.Millisecond)
	bridge := bridgeForTestSession(t, manager, session.SessionID, tabID, chatID)

	addSpawnedWorkRecordForTest(t, manager, tabID, chatID, "wf-recycle", "workflow", "running")
	bridge.mu.Lock()
	bridge.markRecycleAtIdleLocked(recycleReasonRSS)
	lastActivity := bridge.lastActivity
	bridge.mu.Unlock()

	if hibernated := manager.hibernateBridgeIfEligible(bridge, recycleReasonRSS, 0, lastActivity, time.Now(), nil); hibernated {
		t.Fatalf("recycle-at-idle hibernated with running spawned workflow: %#v", manager.Processes())
	}
	if procHasState(manager, StateHibernated) {
		t.Fatalf("recycle-at-idle left bridge hibernated: %#v", manager.Processes())
	}
	if !logs.hasSpawnedHibernateAbort(recycleReasonRSS) {
		t.Fatalf("missing recycle spawned-work abort log: %#v", logs.entriesSnapshot())
	}
}

func TestT1ExternalWorkRegisterDesignatesOutputAndPersistsSnapshot(t *testing.T) {
	stateDir := t.TempDir()
	manager := NewManager(Options{StateDir: stateDir, RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	bindExternalWorkOwnerForTest(manager, "owner-ext", "chat-ext", "tab-ext", "mock")

	result, err := manager.RegisterExternalWork(ExternalWorkRegistrationOptions{
		OwnerKey: "owner-ext", ParentChatID: "chat-ext", ParentTabID: "tab-ext", Label: "Lane register api_key=hidden",
	})
	if err != nil {
		t.Fatalf("register external work: %v", err)
	}
	workID, outputFile, doneFile := result["workId"].(string), result["outputFile"].(string), result["doneFile"].(string)
	if result["taskId"] != workID || !strings.HasPrefix(workID, "xw") || doneFile != outputFile+".done" {
		t.Fatalf("register result = %#v", result)
	}
	wantRoot := filepath.Join(stateDir, "external-work") + string(filepath.Separator)
	if !strings.HasPrefix(outputFile, wantRoot) {
		t.Fatalf("designated output path = %q, want under %q", outputFile, wantRoot)
	}
	info, err := os.Stat(outputFile)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("designated output file info=%#v err=%v", info, err)
	}
	items := manager.ListSpawnedWork("tab-ext", "chat-ext")
	if len(items) != 1 || items[0].TaskID != workID || items[0].Kind != "external" || items[0].Status != "running" ||
		items[0].ToolCallID != "" || items[0].Label != "Lane register api_key=[redacted]" {
		t.Fatalf("registered item = %#v", items)
	}
	manager.spawnedWorkMu.Lock()
	rec := manager.spawnedWork[spawnedWorkKey("tab-ext", "chat-ext", workID)]
	if rec == nil || rec.SessionID != "" || rec.ExternalDoneFile != doneFile {
		t.Fatalf("registered record = %#v", rec)
	}
	manager.spawnedWorkMu.Unlock()

	snapshotData, err := os.ReadFile(manager.spawnedWorkSnapshotPath("tab-ext"))
	if err != nil || !strings.Contains(string(snapshotData), workID) || !strings.Contains(string(snapshotData), `"doneFile"`) {
		t.Fatalf("snapshot data = %s err=%v", snapshotData, err)
	}
	restored := NewManager(Options{StateDir: stateDir, RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { restored.Reset() })
	restoredItems := restored.ListSpawnedWork("tab-ext", "chat-ext")
	if len(restoredItems) != 1 || restoredItems[0].TaskID != workID || restoredItems[0].Kind != "external" || restoredItems[0].Status != "running" {
		t.Fatalf("restored external items = %#v", restoredItems)
	}
	restored.spawnedWorkMu.Lock()
	restoredDone := restored.spawnedWork[spawnedWorkKey("tab-ext", "chat-ext", workID)].ExternalDoneFile
	restored.spawnedWorkMu.Unlock()
	if restoredDone != doneFile {
		t.Fatalf("restored done file = %q, want %q", restoredDone, doneFile)
	}
}

func TestExternalWorkRegistrationIsProviderNeutralAndSettlementSurvivesRestart(t *testing.T) {
	stateDir := t.TempDir()
	outputFile := externalWorkTestPath(t, "codex-handoff.output")
	manager := NewManager(Options{StateDir: stateDir, RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour})
	bindExternalWorkOwnerForTest(manager, "owner-codex-handoff", "chat-codex-handoff", "tab-codex-handoff", "codex")

	registered, err := manager.RegisterExternalWork(ExternalWorkRegistrationOptions{
		OwnerKey: "owner-codex-handoff", ParentChatID: "chat-codex-handoff", ParentTabID: "tab-codex-handoff",
		Label: "production daemon handoff", OutputFile: outputFile,
	})
	if err != nil {
		manager.Reset()
		t.Fatalf("register Codex external work: %v", err)
	}
	workID := asString(registered["workId"])
	doneFile := asString(registered["doneFile"])
	if workID == "" || doneFile == "" {
		manager.Reset()
		t.Fatalf("Codex registration result = %#v", registered)
	}
	items := manager.ListSpawnedWork("tab-codex-handoff", "chat-codex-handoff")
	if len(items) != 1 || items[0].ProviderID != "codex" || items[0].Status != "running" {
		manager.Reset()
		t.Fatalf("Codex external item = %#v", items)
	}
	manager.Reset()

	if err := os.WriteFile(doneFile, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	restored := NewManager(Options{StateDir: stateDir, RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { restored.Reset() })
	restored.reconcileSpawnedWork()

	items = restored.ListSpawnedWork("tab-codex-handoff", "chat-codex-handoff")
	if len(items) != 1 || items[0].Status != "exited" || items[0].ProviderID != "codex" {
		t.Fatalf("Codex handoff after restart items=%#v", items)
	}
}

func TestProductionMockProviderCannotRegisterExternalWork(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir(), RuntimeProfile: "prod", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	bindExternalWorkOwnerForTest(manager, "owner-prod-mock", "chat-prod-mock", "tab-prod-mock", "mock")
	if _, err := manager.RegisterExternalWork(ExternalWorkRegistrationOptions{
		OwnerKey: "owner-prod-mock", ParentChatID: "chat-prod-mock", ParentTabID: "tab-prod-mock", Label: "must reject",
	}); err == nil {
		t.Fatal("production accepted mock external-work registration")
	}
}

func TestT2RunningExternalWorkDoesNotPinIdleTTLHibernationButBashStillDoes(t *testing.T) {
	logs := &spawnedWorkLifecycleLog{}
	manager, events := newFakeManager(t, "echo-prompt", Options{
		HibernateTTL:                 time.Millisecond,
		LifecycleCheckInterval:       time.Hour,
		RSSSampleInterval:            time.Hour,
		SpawnedWorkReconcileInterval: time.Hour,
		Logf:                         logs.Logf,
	})
	t.Cleanup(func() { manager.Reset() })

	const externalTab = "external-no-pin-tab"
	externalChat := "chat-" + externalTab
	externalSession := newFakeSession(t, manager, externalTab)
	externalJob := startAppChatJob(t, manager, externalSession.SessionID, externalTab, "finish before external")
	assertJobStatus(t, events.waitJobEnd(t, jobID(externalJob), 2*time.Second), "done", 0, "end_turn")
	_ = waitProcState(t, manager, StateIdle, 500*time.Millisecond)
	externalBridge := bridgeForTestSession(t, manager, externalSession.SessionID, externalTab, externalChat)
	addSpawnedWorkRecordForTest(t, manager, externalTab, externalChat, "xw-no-pin", "external", "running")
	setBridgeLastActivityForTest(externalBridge, time.Now().Add(-time.Hour))
	manager.SweepLifecycle()
	if !procHasState(manager, StateHibernated) {
		t.Fatalf("running external work pinned idle bridge: %#v", manager.Processes())
	}

	const bashTab = "external-bash-pin-tab"
	bashChat := "chat-" + bashTab
	bashSession := newFakeSession(t, manager, bashTab)
	bashJob := startAppChatJob(t, manager, bashSession.SessionID, bashTab, "finish before bash")
	assertJobStatus(t, events.waitJobEnd(t, jobID(bashJob), 2*time.Second), "done", 0, "end_turn")
	_ = waitProcState(t, manager, StateIdle, 500*time.Millisecond)
	bashBridge := bridgeForTestSession(t, manager, bashSession.SessionID, bashTab, bashChat)
	addSpawnedWorkRecordForTest(t, manager, bashTab, bashChat, "bash-pin", "bash", "running")
	setBridgeLastActivityForTest(bashBridge, time.Now().Add(-time.Hour))
	manager.SweepLifecycle()
	if bridgeStateForTest(bashBridge) == StateHibernated {
		t.Fatalf("running in-process bash did not pin idle bridge: %#v", manager.Processes())
	}
	if !logs.hasSpawnedHibernateAbort(hibernateReasonIdleTTL) {
		t.Fatalf("missing bash spawned-work abort log: %#v", logs.entriesSnapshot())
	}
}

func TestT3ExternalDoneFileSettlesAndWritesRedactedReceiptTail(t *testing.T) {
	stateDir := t.TempDir()
	manager := NewManager(Options{StateDir: stateDir, RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	bindExternalWorkOwnerForTest(manager, "owner-done", "chat-done", "tab-done", "mock")

	outputZero := externalWorkTestPath(t, "done-zero.output")
	zeroRaw, err := manager.RegisterExternalWork(ExternalWorkRegistrationOptions{
		OwnerKey: "owner-done", ParentChatID: "chat-done", ParentTabID: "tab-done", Label: "exit zero", OutputFile: outputZero,
	})
	if err != nil {
		t.Fatalf("register exit zero: %v", err)
	}
	if err := os.WriteFile(outputZero, []byte("ok\npassword=hide-this\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zeroRaw["doneFile"].(string), []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager.reconcileSpawnedWork()
	zero := spawnedWorkItemsByTaskID(manager.ListSpawnedWork("tab-done", "chat-done"))[zeroRaw["workId"].(string)]
	if zero.Status != "exited" || zero.ExitCode == nil || *zero.ExitCode != 0 || zero.Summary != "Done marker written (exit 0)" {
		t.Fatalf("exit zero item = %#v", zero)
	}

	outputThree := externalWorkTestPath(t, "done-three.output")
	threeRaw, err := manager.RegisterExternalWork(ExternalWorkRegistrationOptions{
		OwnerKey: "owner-done", ParentChatID: "chat-done", ParentTabID: "tab-done", Label: "exit three", OutputFile: outputThree,
	})
	if err != nil {
		t.Fatalf("register exit three: %v", err)
	}
	if err := os.WriteFile(outputThree, []byte("bad\nbearer abcdef123456\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(threeRaw["doneFile"].(string), []byte("3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager.reconcileSpawnedWork()
	three := spawnedWorkItemsByTaskID(manager.ListSpawnedWork("tab-done", "chat-done"))[threeRaw["workId"].(string)]
	if three.Status != "failed" || three.ExitCode == nil || *three.ExitCode != 3 || three.Summary != "Done marker written (exit 3)" {
		t.Fatalf("exit three item = %#v", three)
	}

	receipts, err := manager.ListSpawnedWorkReceipts("owner-done", "chat-done", "tab-done", "chat-done", "tab-done", 32)
	if err != nil || len(receipts) != 2 {
		t.Fatalf("external receipts = %#v err=%v", receipts, err)
	}
	joinedTails := receipts[0].OutputTail + "\n" + receipts[1].OutputTail
	if strings.Contains(joinedTails, "hide-this") || strings.Contains(joinedTails, "abcdef123456") ||
		!strings.Contains(strings.ToLower(joinedTails), "[redacted]") {
		t.Fatalf("external receipt tails were not redacted: %q", joinedTails)
	}
	read := manager.ReadSpawnedWork("tab-done", "chat-done", zero.TaskID, 12000)
	if strings.Contains(asString(read["tail"]), "hide-this") || !strings.Contains(strings.ToLower(asString(read["tail"])), "[redacted]") {
		t.Fatalf("external read tail was not redacted: %#v", read)
	}
}

func TestT4ExternalDeadPIDSettlesAfterMissingGrace(t *testing.T) {
	deadPID := 99999999
	if externalPIDAlive(deadPID) {
		t.Skip("platform reports the test pid alive or unknown")
	}
	manager := NewManager(Options{StateDir: t.TempDir(), RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	pid := deadPID
	now := time.Now().UTC()
	output := externalWorkTestPath(t, "pid.output")
	addExternalRecordForTest(manager, "tab-pid", "chat-pid", "xw-pid", "pid lane", &pid, output, output+".done", "running", "")

	manager.reconcileSpawnedWork()
	item := spawnedWorkItemsByTaskID(manager.ListSpawnedWork("tab-pid", "chat-pid"))["xw-pid"]
	if item.Status != "running" {
		t.Fatalf("pid record settled before grace: %#v", item)
	}
	manager.spawnedWorkMu.Lock()
	manager.spawnedWork[spawnedWorkKey("tab-pid", "chat-pid", "xw-pid")].MissingSince = now.Add(-externalWorkMissingGrace - time.Second)
	manager.spawnedWorkMu.Unlock()
	manager.reconcileSpawnedWork()
	item = spawnedWorkItemsByTaskID(manager.ListSpawnedWork("tab-pid", "chat-pid"))["xw-pid"]
	if item.Status != "exited" || item.Summary != "Process exited without a done marker" {
		t.Fatalf("pid record did not settle after grace: %#v", item)
	}
}

func TestT9ExternalWorkPathValidation(t *testing.T) {
	stateDir := t.TempDir()
	if _, ok := validateExternalWorkPath(stateDir, "relative.output"); ok {
		t.Fatal("accepted relative external path")
	}
	if _, ok := validateExternalWorkPath(stateDir, filepath.Clean(os.TempDir())+"/workass/../traversal.output"); ok {
		t.Fatal("accepted unclean traversal external path")
	}
	if _, ok := validateExternalWorkPath(stateDir, "/etc/workass-external.output"); ok {
		t.Fatal("accepted external path outside allowed roots")
	}
	target := externalWorkTestPath(t, "target.output")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := externalWorkTestPath(t, "link.output")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, ok := validateExternalWorkPath(stateDir, link); ok {
		t.Fatal("accepted symlinked external path")
	}
	tmpAccepted := externalWorkTestPath(t, "accepted.output")
	if _, ok := validateExternalWorkPath(stateDir, tmpAccepted); !ok {
		t.Fatalf("rejected tmp external path %q", tmpAccepted)
	}
	stateAccepted := filepath.Join(stateDir, "external-work", "accepted.output")
	if _, ok := validateExternalWorkPath(stateDir, stateAccepted); !ok {
		t.Fatalf("rejected state external path %q", stateAccepted)
	}
}

func TestT10ExternalSnapshotCapPreservesAllRunningRecords(t *testing.T) {
	stateDir := t.TempDir()
	manager := NewManager(Options{StateDir: stateDir, RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	const tabID = "tab-cap"
	const chatID = "chat-cap"
	for i := 0; i < maxSpawnedWorkPerChat+3; i++ {
		taskID := fmt.Sprintf("xw-run-%03d", i)
		addExternalRecordForTest(manager, tabID, chatID, taskID, taskID, nil, "", "", "running", "")
	}
	addExternalRecordForTest(manager, tabID, chatID, "xw-done", "terminal lane", nil, "", "", "exited", "delivered")

	listed := manager.ListSpawnedWork(tabID, chatID)
	if len(listed) != maxSpawnedWorkPerChat+3 {
		t.Fatalf("listed rows = %d, want every running row", len(listed))
	}
	for _, item := range listed {
		if item.Status != "running" {
			t.Fatalf("terminal row displaced a running row in list: %#v", item)
		}
	}
	manager.persistSpawnedWorkSnapshot(tabID, chatID)
	manager.Reset()

	restored := NewManager(Options{StateDir: stateDir, RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { restored.Reset() })
	restoredItems := restored.ListSpawnedWork(tabID, chatID)
	if len(restoredItems) != maxSpawnedWorkPerChat+3 {
		t.Fatalf("restored rows = %d, want every running row", len(restoredItems))
	}
	for _, item := range restoredItems {
		if item.Status != "running" {
			t.Fatalf("restored terminal row displaced a running row: %#v", item)
		}
	}
}

func TestT11ExternalWorkPublicPayloadsRedactSecretShapedOutputPath(t *testing.T) {
	stateDir := t.TempDir()
	manager := NewManager(Options{StateDir: stateDir, RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	const tabID = "tab-secret-path"
	const chatID = "chat-secret-path"
	bindExternalWorkOwnerForTest(manager, "owner-secret-path", chatID, tabID, "mock")
	output := externalWorkTestPath(t, "api_key=lane-secret.output")
	if err := os.WriteFile(output, []byte("ready\ntoken=tail-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	addExternalRecordForTest(manager, tabID, chatID, "xw-secret-path", "secret path", nil, output, output+".done", "exited", "pending")
	manager.commitSpawnedWorkChange(tabID, chatID)

	items := manager.ListSpawnedWork(tabID, chatID)
	if len(items) != 1 || strings.Contains(items[0].OutputFile, "lane-secret") || !strings.Contains(items[0].OutputFile, "api_key=[redacted]") {
		t.Fatalf("public list leaked output path: %#v", items)
	}
	read := manager.ReadSpawnedWork(tabID, chatID, "xw-secret-path", 12000)
	readItem, _ := read["item"].(SpawnedWorkItem)
	if strings.Contains(readItem.OutputFile, "lane-secret") || !strings.Contains(readItem.OutputFile, "api_key=[redacted]") {
		t.Fatalf("public read leaked output path: %#v", read)
	}
	if tail := asString(read["tail"]); strings.Contains(tail, "tail-secret") || !strings.Contains(tail, "token=[redacted]") {
		t.Fatalf("public read tail was not redacted from raw path: %#v", read)
	}

	receipts, err := manager.ListSpawnedWorkReceipts("owner-secret-path", chatID, tabID, chatID, tabID, 8)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipts = %#v err=%v", receipts, err)
	}
	if strings.Contains(receipts[0].OutputFile, "lane-secret") || !strings.Contains(receipts[0].OutputFile, "api_key=[redacted]") {
		t.Fatalf("receipt leaked output path: %#v", receipts[0])
	}
	receiptBytes, err := os.ReadFile(manager.spawnedWorkReceiptPath(tabID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(receiptBytes), "lane-secret") {
		t.Fatalf("receipt file leaked output path: %s", receiptBytes)
	}

}

func bindExternalWorkOwnerForTest(manager *Manager, ownerKey, chatID, tabID, providerID string) {
	manager.mu.Lock()
	manager.bindAgentOwnerLocked(ownerKey, chatID, tabID)
	manager.bindChatProviderLocked(SessionOptions{TabID: tabID, ChatID: chatID}, providerID)
	manager.mu.Unlock()
}

func externalWorkTestPath(t *testing.T, name string) string {
	t.Helper()
	root, err := os.MkdirTemp(os.TempDir(), "workass-external-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	return filepath.Join(root, name)
}

func addExternalRecordForTest(manager *Manager, tabID, chatID, taskID, label string, pid *int, outputFile, doneFile, status, _ string) {
	now := time.Now().UTC()
	if label == "" {
		label = taskID
	}
	item := SpawnedWorkItem{
		ID: taskID, TaskID: taskID, TabID: tabID, ChatID: chatID, ProviderID: "claude",
		Kind: "external", Label: label, Status: status,
		StartedAt:  now.Add(-time.Minute).Format(time.RFC3339Nano),
		UpdatedAt:  now.Format(time.RFC3339Nano),
		OutputFile: outputFile, PID: pid,
	}
	if item.Status == "" {
		item.Status = "running"
	}
	if item.Status != "running" {
		item.FinishedAt = now.Format(time.RFC3339Nano)
	}
	manager.spawnedWorkMu.Lock()
	manager.spawnedWork[spawnedWorkKey(tabID, chatID, taskID)] = &spawnedWorkRecord{Item: item, ExternalDoneFile: doneFile, SawPID: pid != nil}
	manager.spawnedWorkMu.Unlock()
}

type spawnedWorkLifecycleLogEntry struct {
	message string
	fields  map[string]any
}

type spawnedWorkLifecycleLog struct {
	mu      sync.Mutex
	entries []spawnedWorkLifecycleLogEntry
}

func (l *spawnedWorkLifecycleLog) Logf(message string, fields map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	copied := make(map[string]any, len(fields))
	for key, value := range fields {
		copied[key] = value
	}
	l.entries = append(l.entries, spawnedWorkLifecycleLogEntry{message: message, fields: copied})
}

func (l *spawnedWorkLifecycleLog) hasSpawnedHibernateAbort(reason string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, entry := range l.entries {
		if entry.message == "acp hibernate aborted" && asString(entry.fields["reason"]) == reason && entry.fields["spawnedWork"] == true {
			return true
		}
	}
	return false
}

func (l *spawnedWorkLifecycleLog) entriesSnapshot() []spawnedWorkLifecycleLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]spawnedWorkLifecycleLogEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

func bridgeForTestSession(t *testing.T, manager *Manager, sessionID, tabID, chatID string) *Bridge {
	t.Helper()
	bridge := manager.bridgeForSession(sessionID, SessionOptions{TabID: tabID, ChatID: chatID, SessionID: sessionID})
	if bridge == nil {
		t.Fatalf("bridge missing for session %s", sessionID)
	}
	return bridge
}

func addSpawnedWorkRecordForTest(t *testing.T, manager *Manager, tabID, chatID, taskID, kind, status string) {
	t.Helper()
	if status == "" {
		status = "running"
	}
	now := time.Now().UTC()
	rec := &spawnedWorkRecord{
		SessionID: "session-" + taskID,
		Item: SpawnedWorkItem{
			ID: taskID, TaskID: taskID, TabID: tabID, ChatID: chatID, ProviderID: "claude",
			Kind: kind, Label: taskID, Status: status,
			StartedAt: now.Add(-time.Minute).Format(time.RFC3339Nano),
			UpdatedAt: now.Format(time.RFC3339Nano),
		},
	}
	if status != "running" {
		rec.Item.FinishedAt = now.Format(time.RFC3339Nano)
	}
	manager.spawnedWorkMu.Lock()
	manager.spawnedWork[spawnedWorkKey(tabID, chatID, taskID)] = rec
	manager.spawnedWorkMu.Unlock()
}

func settleSpawnedWorkRecordForTest(t *testing.T, manager *Manager, tabID, chatID, taskID, status string) {
	t.Helper()
	manager.spawnedWorkMu.Lock()
	defer manager.spawnedWorkMu.Unlock()
	rec := manager.spawnedWork[spawnedWorkKey(tabID, chatID, taskID)]
	if !manager.settleSpawnedWorkLocked(rec, status, nil) {
		t.Fatalf("spawned work %s did not settle", taskID)
	}
}

func spawnedWorkItemsByTaskID(items []SpawnedWorkItem) map[string]SpawnedWorkItem {
	out := make(map[string]SpawnedWorkItem, len(items))
	for _, item := range items {
		out[item.TaskID] = item
	}
	return out
}

func setBridgeLastActivityForTest(bridge *Bridge, lastActivity time.Time) {
	bridge.mu.Lock()
	bridge.lastActivity = lastActivity
	bridge.mu.Unlock()
}

func bridgeLastActivityForTest(bridge *Bridge) time.Time {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	return bridge.lastActivity
}

func bridgeStateForTest(bridge *Bridge) EngineState {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	return bridge.state
}
