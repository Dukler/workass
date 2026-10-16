package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMockClaudeProviderForwardsSpawnedWorkWithoutAgentCooperation(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

func TestRejectedClaudeSteerLeavesForegroundAndSpawnedWorkRunning(t *testing.T) {
	t.Parallel()
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
	if res["ok"] != false || res["unsupported"] != true || res["queued"] != false || res["interrupted"] == true || res["strategy"] != "unsupported" {
		t.Fatalf("Claude steer rejection = %#v", res)
	}
	if _, running := manager.RunningJobForChat(tabID, chatID); !running {
		t.Fatal("rejected steer ended the foreground turn")
	}
	items = manager.ListSpawnedWork(tabID, chatID)
	if len(items) != 1 || items[0].Status != "running" {
		t.Fatalf("rejected steer changed spawned work: %#v", items)
	}
	if cancelled := manager.CancelJobResult(jobID(job)); !cancelled.Cancelled {
		t.Fatalf("explicit cleanup Stop did not cancel foreground turn: %#v", cancelled)
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
	t.Fatalf("spawned work did not survive explicit foreground Stop: %#v", manager.ListSpawnedWork(tabID, chatID))
}

func TestProductionMockProviderCannotInjectSpawnedWork(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{StateDir: t.TempDir(), RuntimeProfile: "prod", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	if manager.acceptsProviderSpawnedWork("mock") {
		t.Fatal("production accepted mock spawned-work provider")
	}
	observeRawSpawnToolFixture(t, manager, spawnToolObservation{
		SessionID: "session-prod", TabID: "tab-prod", ChatID: "chat-prod", ProviderID: "mock", ToolCallID: "tool-prod",
		Title: "Bash",
	}, providerRawToolObservation{Title: "Bash", RawInput: map[string]any{"run_in_background": true}})
	if got := manager.ListSpawnedWork("tab-prod", "chat-prod"); len(got) != 0 {
		t.Fatalf("production accepted mock spawned-work fixture: %#v", got)
	}
}

func observeRawSpawnToolFixture(t *testing.T, manager *Manager, observation spawnToolObservation, raw providerRawToolObservation) {
	t.Helper()
	signal, ok := providerAdapterForID(observation.ProviderID).spawnedWork.DecodeTool(raw)
	if !ok {
		return
	}
	observation.ProviderTool = signal.ProviderTool
	observation.RunsInBackground = signal.RunsInBackground
	observation.FallbackTaskID = signal.FallbackTaskID
	observation.FallbackOutputFile = signal.FallbackOutputFile
	manager.observeSpawnToolEvent(observation)
}

func observeRawSpawnLifecycleFixture(t *testing.T, manager *Manager, tabID, chatID, sessionID, providerID string, raw map[string]any) {
	t.Helper()
	update, ok := providerAdapterForID(providerID).spawnedWork.DecodeLifecycle(raw)
	if !ok {
		t.Fatalf("registered provider did not decode spawned-work fixture: %#v", raw)
	}
	manager.observeProviderSpawnedWork(tabID, chatID, sessionID, providerID, update)
}

func spawnedWorkTestOutput(t *testing.T, taskID string) string {
	t.Helper()
	// The registered provider path policy trusts only os.TempDir()/tmp roots. Under
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
	t.Parallel()
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
	if err := manager.InstallSpawnedWorkObserver(func(_ string, _ string, items []SpawnedWorkItem) (SpawnedWorkActorProjection, error) {
		return SpawnedWorkActorProjection{ActorRevision: 1, Items: items}, nil
	}); err != nil {
		t.Fatalf("install spawned-work actor fixture: %v", err)
	}

	observeRawSpawnToolFixture(t, manager, spawnToolObservation{
		SessionID: "session-1", TabID: "tab-1", ChatID: "chat-1", ProviderID: "claude", ToolCallID: "tool-1",
		Title: "Bash", Command: "python3 -m http.server 8737",
	}, providerRawToolObservation{
		Title: "Bash", Command: "python3 -m http.server 8737", RawInput: map[string]any{"run_in_background": true},
		Meta: map[string]any{"claudeCode": map[string]any{"toolName": "Bash"}},
	})
	observeRawSpawnToolFixture(t, manager, spawnToolObservation{
		SessionID: "session-1", TabID: "tab-1", ChatID: "chat-1", ProviderID: "claude", ToolCallID: "tool-1",
		Title: "Bash",
	}, providerRawToolObservation{
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
	if len(items) != 0 {
		t.Fatalf("settled delivery cache was not compacted after actor acceptance: %#v", items)
	}

	manager.mu.Lock()
	manager.bindAgentOwnerLocked("owner-1", "chat-1", "tab-1")
	manager.mu.Unlock()
	receipts, err := manager.ListSpawnedWorkReceipts("owner-1", "chat-1", "tab-1", "chat-1", "tab-1", 32)
	if err != nil || len(receipts) != 1 || receipts[0].Status != "exited" || receipts[0].FinishedAt == "" {
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
	t.Cleanup(func() { restored.Reset() })
	restoredItems := restored.ListSpawnedWork("tab-1", "chat-1")
	if len(restoredItems) != 0 {
		t.Fatalf("settled delivery cache reappeared after restart: %#v", restoredItems)
	}
}

func TestSpawnedWorkFallbackNormalizesSentencePunctuationAndMergesStructuredRecord(t *testing.T) {
	t.Parallel()
	const taskID = "b7hm20ecb"
	output := spawnedWorkTestOutput(t, taskID)
	manager := NewManager(Options{StateDir: t.TempDir(), SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })

	observeRawSpawnLifecycleFixture(t, manager, "tab-real", "chat-real", "session-real", "claude", map[string]any{
		"type": "started", "taskId": taskID, "toolCallId": "tool-real",
		"description": "Run lab/prove feather_dose ladder third attempt", "taskType": "bash",
	})
	observeRawSpawnToolFixture(t, manager, spawnToolObservation{
		SessionID: "session-real", TabID: "tab-real", ChatID: "chat-real", ProviderID: "claude", ToolCallID: "tool-real",
		Title: "tool",
	}, providerRawToolObservation{
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

func TestSpawnedWorkSnapshotReloadDropsNoncanonicalCacheRow(t *testing.T) {
	t.Parallel()
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
	if len(restored) != 1 || restored[0].TaskID != taskID || restored[0].OutputFile != "" {
		t.Fatalf("noncanonical cache row was merged into actor evidence: %#v", restored)
	}
}

func TestSpawnedWorkStructuredAgentWorkflowAndSnapshotLifecycle(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{
		StateDir: t.TempDir(), SpawnedWorkReconcileInterval: time.Hour,
		SpawnedWorkPIDProbe: func([]string) (map[string][]int, bool) { return nil, false },
	})
	observeRawSpawnLifecycleFixture(t, manager, "tab-2", "chat-2", "session-2", "claude", map[string]any{
		"type": "started", "taskId": "agent-1", "toolCallId": "tool-agent", "description": "Review UI", "taskType": "agent", "subagentType": "Explore",
	})
	observeRawSpawnLifecycleFixture(t, manager, "tab-2", "chat-2", "session-2", "claude", map[string]any{
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

	observeRawSpawnLifecycleFixture(t, manager, "tab-2", "chat-2", "session-2", "claude", map[string]any{
		"type": "notification", "taskId": "agent-1", "status": "failed", "summary": "review failed",
	})
	observeRawSpawnLifecycleFixture(t, manager, "tab-2", "chat-2", "session-2", "claude", map[string]any{
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
	t.Parallel()
	strategy := providerAdapterForID("claude").spawnedWork
	if _, ok := strategy.ValidateOutputPath("task-1", "/etc/task-1.output"); ok {
		t.Fatal("accepted output outside Claude temp task directory")
	}
	if _, ok := strategy.ValidateOutputPath("../task-1", filepath.Join(os.TempDir(), "claude-501", "tasks", "task-1.output")); ok {
		t.Fatal("accepted path traversal task id")
	}
}

func TestSpawnedWorkBatchProbeFindsOpenOutputOwner(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	installAcceptingSpawnedWorkActorForTest(manager)
	manager.commitSpawnedWorkChange(tabID, chatID)

	if _, err := os.Stat(manager.spawnedWorkSnapshotPath(tabID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("terminal executor cache survived actor commit: %v", err)
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	stateDir := t.TempDir()
	manager, _ := newFakeManager(t, "echo-prompt", Options{
		StateDir:                     stateDir,
		LifecycleCheckInterval:       time.Hour,
		RSSSampleInterval:            time.Hour,
		SpawnedWorkReconcileInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	var projected []SpawnedWorkItem
	if err := manager.InstallSpawnedWorkObserver(func(_ string, _ string, items []SpawnedWorkItem) (SpawnedWorkActorProjection, error) {
		projected = append([]SpawnedWorkItem(nil), items...)
		return SpawnedWorkActorProjection{ActorRevision: 1, Items: items}, nil
	}); err != nil {
		t.Fatal(err)
	}

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

	items := spawnedWorkItemsByTaskID(projected)
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
	t.Parallel()
	manager, _ := newFakeManager(t, "echo-prompt", Options{
		LifecycleCheckInterval:       time.Hour,
		RSSSampleInterval:            time.Hour,
		SpawnedWorkReconcileInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	installAcceptingSpawnedWorkActorForTest(manager)

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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	var projected []SpawnedWorkItem
	if err := restored.InstallSpawnedWorkObserver(func(_ string, _ string, items []SpawnedWorkItem) (SpawnedWorkActorProjection, error) {
		projected = append([]SpawnedWorkItem(nil), items...)
		return SpawnedWorkActorProjection{ActorRevision: 1, Items: items}, nil
	}); err != nil {
		t.Fatal(err)
	}
	restored.reconcileSpawnedWork()

	items = projected
	if len(items) != 1 || items[0].Status != "exited" || items[0].ProviderID != "codex" {
		t.Fatalf("Codex handoff after restart items=%#v", items)
	}
}

func TestProductionMockProviderCannotRegisterExternalWork(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

	receipts, err := manager.ListSpawnedWorkReceipts("owner-done", "chat-done", "tab-done", "chat-done", "tab-done", 32)
	if err != nil || len(receipts) != 2 {
		t.Fatalf("external receipts = %#v err=%v", receipts, err)
	}
	byTaskID := make(map[string]SpawnedWorkReceipt, len(receipts))
	for _, receipt := range receipts {
		byTaskID[receipt.TaskID] = receipt
	}
	zero := byTaskID[zeroRaw["workId"].(string)]
	if zero.Status != "exited" || zero.ExitCode == nil || *zero.ExitCode != 0 || zero.Summary != "Done marker written (exit 0)" {
		t.Fatalf("exit zero receipt = %#v", zero)
	}
	three := byTaskID[threeRaw["workId"].(string)]
	if three.Status != "failed" || three.ExitCode == nil || *three.ExitCode != 3 || three.Summary != "Done marker written (exit 3)" {
		t.Fatalf("exit three receipt = %#v", three)
	}
	joinedTails := receipts[0].OutputTail + "\n" + receipts[1].OutputTail
	if strings.Contains(joinedTails, "hide-this") || strings.Contains(joinedTails, "abcdef123456") ||
		!strings.Contains(strings.ToLower(joinedTails), "[redacted]") {
		t.Fatalf("external receipt tails were not redacted: %q", joinedTails)
	}
}

func TestT4ExternalDeadPIDSettlesAfterMissingGrace(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	manager.persistSpawnedWorkSnapshot(tabID)
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
	t.Parallel()
	stateDir := t.TempDir()
	manager := NewManager(Options{StateDir: stateDir, RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	const tabID = "tab-secret-path"
	const chatID = "chat-secret-path"
	var projected []SpawnedWorkItem
	if err := manager.InstallSpawnedWorkObserver(func(_ string, _ string, items []SpawnedWorkItem) (SpawnedWorkActorProjection, error) {
		projected = append([]SpawnedWorkItem(nil), items...)
		return SpawnedWorkActorProjection{ActorRevision: 1, Items: items}, nil
	}); err != nil {
		t.Fatal(err)
	}
	bindExternalWorkOwnerForTest(manager, "owner-secret-path", chatID, tabID, "mock")
	output := externalWorkTestPath(t, "api_key=lane-secret.output")
	if err := os.WriteFile(output, []byte("ready\ntoken=tail-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	addExternalRecordForTest(manager, tabID, chatID, "xw-secret-path", "secret path", nil, output, output+".done", "exited", "pending")
	manager.commitSpawnedWorkChange(tabID, chatID)

	items := projected
	if len(items) != 1 || strings.Contains(items[0].OutputFile, "lane-secret") || !strings.Contains(items[0].OutputFile, "api_key=[redacted]") {
		t.Fatalf("public list leaked output path: %#v", items)
	}
	receipts, err := manager.ListSpawnedWorkReceipts("owner-secret-path", chatID, tabID, chatID, tabID, 8)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipts = %#v err=%v", receipts, err)
	}
	if strings.Contains(receipts[0].OutputFile, "lane-secret") || !strings.Contains(receipts[0].OutputFile, "api_key=[redacted]") {
		t.Fatalf("receipt leaked output path: %#v", receipts[0])
	}
	if strings.Contains(receipts[0].OutputTail, "tail-secret") || !strings.Contains(receipts[0].OutputTail, "token=[redacted]") {
		t.Fatalf("receipt tail leaked raw output: %#v", receipts[0])
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
	installAcceptingSpawnedWorkActorForTest(manager)
	manager.mu.Lock()
	manager.bindAgentOwnerLocked(ownerKey, chatID, tabID)
	manager.bindChatProviderLocked(SessionOptions{TabID: tabID, ChatID: chatID}, providerID)
	manager.mu.Unlock()
}

func installAcceptingSpawnedWorkActorForTest(manager *Manager) {
	if manager == nil {
		panic("spawned-work actor fixture requires a manager")
	}
	manager.spawnedWorkObserverMu.RLock()
	installed := manager.spawnedWorkObserver != nil
	manager.spawnedWorkObserverMu.RUnlock()
	if installed {
		return
	}
	if err := manager.InstallSpawnedWorkObserver(func(_ string, _ string, items []SpawnedWorkItem) (SpawnedWorkActorProjection, error) {
		return SpawnedWorkActorProjection{ActorRevision: 1, Items: items}, nil
	}); err != nil {
		panic(err)
	}
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

func TestNormalizeSpawnedWorkRoleAcceptsOnlyCanonicalRoles(t *testing.T) {
	for _, role := range []string{"server", "daemon", "task", "job"} {
		if got := normalizeSpawnedWorkRole(role); got != "" {
			t.Fatalf("legacy role %q normalized to %q", role, got)
		}
	}
	if normalizeSpawnedWorkRole("service") != spawnedWorkRoleService || normalizeSpawnedWorkRole("work") != spawnedWorkRoleWork {
		t.Fatal("canonical spawned-work roles were not preserved")
	}
}

func TestNativeCodexBackgroundChildOutlivesParentAndRetainsReceipt(t *testing.T) {
	t.Parallel()
	events := newEventCollector()
	stateDir := t.TempDir()
	manager := newCodexCandidateManager(t, stateDir, "native-background-parent", false)
	manager.opts.Broadcast = events.Broadcast
	releaseFile := filepath.Join(stateDir, "release-child")
	manager.providers["codex"].Config.Env["WORKASS_CODEX_FIXTURE_CHILD_RELEASE_FILE"] = releaseFile
	if err := manager.InstallSpawnedWorkObserver(func(_ string, _ string, items []SpawnedWorkItem) (SpawnedWorkActorProjection, error) {
		return SpawnedWorkActorProjection{ActorRevision: 1, Items: items}, nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: "bg-tab", ChatID: "bg-chat", ProviderID: "codex", CWD: repoRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	started, err := manager.StartJob(ctx, JobStartOptions{Kind: "app-chat", SessionID: session.SessionID,
		TabID: "bg-tab", ChatID: "bg-chat", Prompt: "[fixture:native-background]"})
	if err != nil {
		t.Fatal(err)
	}
	assertJobStatus(t, events.waitJobEnd(t, jobID(started), 5*time.Second), "done", 0, "end_turn")
	items := manager.ListSpawnedWork("bg-tab", "bg-chat")
	if len(items) != 1 || items[0].Status != "running" || items[0].Kind != "agent" || items[0].ModelLabel != "gpt-fixture-mini" {
		t.Fatalf("parent terminal lost child lifetime: %#v", items)
	}
	childID := items[0].ID
	if result := manager.StopSpawnedWork("bg-tab", "bg-chat", childID); result["ok"] != false {
		t.Fatalf("unsupported native Stop claimed success: %#v", result)
	}
	if got := manager.ListSpawnedWork("bg-tab", "bg-chat")[0].Status; got != "running" {
		t.Fatalf("unsupported Stop changed status: %s", got)
	}
	bridge := manager.bridgeForSession(session.SessionID, SessionOptions{TabID: "bg-tab", ChatID: "bg-chat", ProviderID: "codex"})
	if bridge == nil {
		t.Fatal("parent completion detached the child's host")
	}
	if err := os.WriteFile(releaseFile, []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.request(ctx, "_workass/codex/rate-limits", map[string]any{}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	events.waitFor(t, 5*time.Second, func(event collectedEvent) bool {
		if event.channel != "spawned-work:changed" {
			return false
		}
		payload, _ := event.payload.(map[string]any)
		items, _ := payload["items"].([]SpawnedWorkItem)
		return len(items) == 1 && items[0].ID == childID && items[0].Status == "failed" && items[0].Summary == "Background review failed"
	})
	if items = manager.ListSpawnedWork("bg-tab", "bg-chat"); len(items) != 0 {
		t.Fatalf("terminal child retained a live runtime record: %#v", items)
	}
	if receipt, ok := manager.findSpawnedWorkReceipt("bg-tab", "bg-chat", childID); !ok || receipt.Status != "failed" {
		t.Fatalf("missing durable child failure receipt: %#v %v", receipt, ok)
	}
	if len(manager.ListSpawnedWork("other-tab", "bg-chat")) != 0 {
		t.Fatal("child crossed exact chat ownership")
	}
}

// 2026-10-11 QA: an engine attached for a chat that was never prompted stayed
// "warm" forever — never idle-hibernated, never age-recycled.
func TestLifecycleHibernatesAnEngineThatWasNeverPrompted(t *testing.T) {
	t.Parallel()
	manager, _ := newFakeManager(t, "echo-prompt", Options{
		HibernateTTL:                 time.Millisecond,
		LifecycleCheckInterval:       time.Hour,
		RSSSampleInterval:            time.Hour,
		SpawnedWorkReconcileInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	const tabID = "never-prompted-tab"
	session := newFakeSession(t, manager, tabID)
	bridge := bridgeForTestSession(t, manager, session.SessionID, tabID, "chat-"+tabID)
	setBridgeLastActivityForTest(bridge, time.Now().Add(-time.Hour))
	manager.SweepLifecycle()
	if !procHasState(manager, StateHibernated) {
		t.Fatalf("a never-prompted engine past its idle TTL was not hibernated: %#v", manager.Processes())
	}
}

// A background shell runs in the engine's process tree, so it dies with it.
// When the engine closes, one whose process is gone is reported as stopped
// right away; one that outlived the engine (a detached process) keeps running.
func TestBridgeCloseReportsBackgroundShellsThatDiedWithTheEngine(t *testing.T) {
	t.Parallel()
	manager, _ := newFakeManager(t, "echo-prompt", Options{
		StateDir:                     t.TempDir(),
		LifecycleCheckInterval:       time.Hour,
		RSSSampleInterval:            time.Hour,
		SpawnedWorkReconcileInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	var projected []SpawnedWorkItem
	if err := manager.InstallSpawnedWorkObserver(func(_ string, _ string, items []SpawnedWorkItem) (SpawnedWorkActorProjection, error) {
		projected = append([]SpawnedWorkItem(nil), items...)
		return SpawnedWorkActorProjection{ActorRevision: 1, Items: items}, nil
	}); err != nil {
		t.Fatal(err)
	}
	const tabID, chatID = "spawn-shell-tab", "spawn-shell-chat"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: tabID, ChatID: chatID})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	bridge := bridgeForTestSession(t, manager, session.SessionID, tabID, chatID)

	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	deadPID, livePID := dead.Process.Pid, os.Getpid()
	for _, shell := range []struct {
		id  string
		pid int
	}{{"shell-dead", deadPID}, {"shell-live", livePID}} {
		addSpawnedWorkRecordForTest(t, manager, tabID, chatID, shell.id, "bash", "running")
		pid := shell.pid
		manager.spawnedWorkMu.Lock()
		rec := manager.spawnedWork[spawnedWorkKey(tabID, chatID, shell.id)]
		rec.Item.OutputFile = "/private/tmp/claude-501/project/session/tasks/" + shell.id + ".output"
		rec.Item.PID = &pid
		manager.spawnedWorkMu.Unlock()
	}

	bridge.Close(true, errors.New("workass update"))

	items := spawnedWorkItemsByTaskID(projected)
	if got := items["shell-dead"]; got.Status != "orphaned" || got.Summary != "Stopped: its process ended with the ACP engine (reason: workass update)" {
		t.Fatalf("a shell that died with the engine was not reported: %#v", got)
	}
	manager.spawnedWorkMu.Lock()
	live := manager.spawnedWork[spawnedWorkKey(tabID, chatID, "shell-live")].Item.Status
	manager.spawnedWorkMu.Unlock()
	if live != "running" {
		t.Fatalf("a shell that outlived the engine was settled: %s", live)
	}
}

// Claude Code names its subagent types "general-purpose" or "Explore"; a task
// with any subagent type is an agent row, not generic background work.
func TestClaudeSubagentTypeMakesAnAgentWithoutAToolCandidate(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{
		StateDir: t.TempDir(), SpawnedWorkReconcileInterval: time.Hour,
		SpawnedWorkPIDProbe: func([]string) (map[string][]int, bool) { return nil, false },
	})
	observeRawSpawnLifecycleFixture(t, manager, "tab-3", "chat-3", "session-3", "claude", map[string]any{
		"type": "started", "taskId": "a7f3544f9748d9adc", "toolCallId": "toolu_agent", "description": "Classify workspace tmp dirs", "subagentType": "general-purpose",
	})
	items := manager.ListSpawnedWork("tab-3", "chat-3")
	if len(items) != 1 || items[0].Kind != "agent" || items[0].Label != "Classify workspace tmp dirs" || items[0].Status != "running" {
		t.Fatalf("native subagent item = %#v", items)
	}
}

// The exact lifecycle the real Claude Code CLI emitted (2026-10-13, through the
// fixed host) for a background Agent-tool subagent. It must become one agent
// item with its description, its output file, and a settled status.
func TestRealClaudeBackgroundAgentLifecycleBecomesAgentWork(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{
		StateDir: t.TempDir(), SpawnedWorkReconcileInterval: time.Hour,
		SpawnedWorkPIDProbe: func([]string) (map[string][]int, bool) { return nil, false },
	})
	output := filepath.Join(os.TempDir(), "claude-501", "-private-tmp-wk-ui-agent-probe", "d60ceb2a-4fd6-4241-80c8-530dd979cc6d", "tasks", "aa48cfacd7844d6ac.output")
	for _, event := range []map[string]any{
		{"type": "started", "taskId": "aa48cfacd7844d6ac", "toolCallId": "toolu_01FXcAcK9iaurFSYtBfr6BaY", "description": "List current directory files", "subagentType": "Explore", "taskType": "local_agent"},
		{"type": "progress", "taskId": "aa48cfacd7844d6ac", "toolCallId": "toolu_01FXcAcK9iaurFSYtBfr6BaY", "description": "Running List files and directories in the working directory", "subagentType": "Explore", "lastToolName": "Bash"},
	} {
		observeRawSpawnLifecycleFixture(t, manager, "tab-real-agent", "chat-real-agent", "session-real-agent", "claude", event)
	}
	running := manager.ListSpawnedWork("tab-real-agent", "chat-real-agent")
	if len(running) != 1 || running[0].Kind != "agent" || running[0].Status != "running" || running[0].LastToolName != "Bash" {
		t.Fatalf("running agent item = %#v", running)
	}
	for _, event := range []map[string]any{
		{"type": "updated", "taskId": "aa48cfacd7844d6ac", "patch": map[string]any{"status": "completed"}},
		{"type": "notification", "taskId": "aa48cfacd7844d6ac", "toolCallId": "toolu_01FXcAcK9iaurFSYtBfr6BaY", "status": "completed", "outputFile": output, "summary": "a.txt\nb.txt"},
	} {
		observeRawSpawnLifecycleFixture(t, manager, "tab-real-agent", "chat-real-agent", "session-real-agent", "claude", event)
	}
	settled := manager.ListSpawnedWork("tab-real-agent", "chat-real-agent")
	if len(settled) != 1 || settled[0].Status != "exited" || settled[0].Label != "List current directory files" {
		t.Fatalf("settled agent item = %#v", settled)
	}
	if settled[0].TaskID != "aa48cfacd7844d6ac" || settled[0].ToolCallID != "toolu_01FXcAcK9iaurFSYtBfr6BaY" {
		t.Fatalf("agent identity lost: %#v", settled[0])
	}
}

// An agent that stopped with its own work running waits for it; if it never
// resumes once that work is done, it settles after the grace instead of
// staying "running" forever.
func TestWaitingAgentSettlesWhenItDoesNotResumeAfterItsOwnWork(t *testing.T) {
	previous := waitingAgentGrace
	waitingAgentGrace = 0
	t.Cleanup(func() { waitingAgentGrace = previous })
	manager := NewManager(Options{
		StateDir: t.TempDir(), SpawnedWorkReconcileInterval: time.Hour,
		SpawnedWorkPIDProbe: func([]string) (map[string][]int, bool) { return nil, false },
	})
	observe := func(event map[string]any) {
		observeRawSpawnLifecycleFixture(t, manager, "tab-wait", "chat-wait", "session-wait", "claude", event)
	}
	observe(map[string]any{"type": "started", "taskId": "agent-w", "toolCallId": "tool-agent", "description": "Waiting agent", "subagentType": "Explore", "taskType": "local_agent"})
	observe(map[string]any{"type": "started", "taskId": "child-w", "toolCallId": "tool-child", "description": "Its own sleep", "taskType": "local_bash", "parentTaskId": "agent-w"})
	observe(map[string]any{"type": "notification", "taskId": "agent-w", "toolCallId": "tool-agent", "status": "completed", "summary": "stopped with background work of its own still running"})
	status := func(id string) string {
		for _, item := range manager.ListSpawnedWork("tab-wait", "chat-wait") {
			if item.TaskID == id {
				return item.Status
			}
		}
		return "absent"
	}
	if status("agent-w") != "running" {
		t.Fatalf("a stopped agent with running work of its own was settled: %s", status("agent-w"))
	}
	manager.reconcileSpawnedWork()
	if status("agent-w") != "running" {
		t.Fatal("a waiting agent settled while its own work still ran")
	}
	observe(map[string]any{"type": "notification", "taskId": "child-w", "toolCallId": "tool-child", "status": "completed"})
	manager.reconcileSpawnedWork() // its last child is done: the grace starts
	manager.reconcileSpawnedWork() // no resume within the grace: settled
	if got := status("agent-w"); got != "exited" {
		t.Fatalf("a waiting agent that never resumed stayed %q", got)
	}
}

// A Monitor watcher is its own kind, and an Agent call's model names the agent.
func TestClaudeMonitorKindAndAgentModelFromToolInput(t *testing.T) {
	t.Parallel()
	if kind := spawnedWorkKind("Monitor", "local_bash", ""); kind != "monitor" {
		t.Fatalf("Monitor kind = %q", kind)
	}
	strategy := claudeProviderSpawnedWorkStrategy{}
	monitor, ok := strategy.DecodeTool(providerRawToolObservation{Title: "Monitor", Meta: map[string]any{"claudeCode": map[string]any{"toolName": "Monitor"}}, RawInput: map[string]any{"command": "tail -f log"}})
	if !ok || !monitor.RunsInBackground {
		t.Fatalf("a Monitor call was not background work: %#v", monitor)
	}
	agent, ok := strategy.DecodeTool(providerRawToolObservation{Title: "Agent", Meta: map[string]any{"claudeCode": map[string]any{"toolName": "Agent"}}, RawInput: map[string]any{"run_in_background": true, "model": "haiku"}})
	if !ok || agent.ModelLabel != "haiku" {
		t.Fatalf("the Agent call's model was lost: %#v", agent)
	}
}

// Claude Code reports every Bash command as a task. One the user interrupts
// never gets its task-ended event, and with no PID or output file nothing else
// could see it end: its row read "running" for as long as the session lived
// (observed 2026-10-13, "Find how Claude Code presents ultracode"). The tool
// call ending ends it. A background shell's call ending does not.
func TestForegroundClaudeBashTaskEndsWithItsToolCall(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{
		StateDir: t.TempDir(), SpawnedWorkReconcileInterval: time.Hour,
		SpawnedWorkPIDProbe: func([]string) (map[string][]int, bool) { return nil, false },
	})
	t.Cleanup(func() { manager.Reset() })
	b := &Bridge{providerID: "claude", manager: manager}
	job := &Job{ID: "job-fg", TabID: "tab-fg", ChatID: "chat-fg", SessionID: "session-fg"}
	observe := func(event map[string]any) {
		observeRawSpawnLifecycleFixture(t, manager, "tab-fg", "chat-fg", "session-fg", "claude", event)
	}
	status := func(taskID string) (string, int) {
		found, count := "absent", 0
		for _, item := range manager.ListSpawnedWork("tab-fg", "chat-fg") {
			if item.TaskID == taskID {
				found, count = item.Status, count+1
			}
		}
		return found, count
	}
	observe(map[string]any{"type": "started", "taskId": "bhboz80wh", "toolCallId": "toolu_fg", "description": "Find how Claude Code presents ultracode", "taskType": "local_bash"})
	observe(map[string]any{"type": "started", "taskId": "bbgshell1", "toolCallId": "toolu_bg", "description": "Run the gate", "taskType": "local_bash", "outputFile": spawnedWorkTestOutput(t, "bbgshell1")})
	if got, _ := status("bhboz80wh"); got != "running" {
		t.Fatalf("foreground task before its call ended = %q", got)
	}

	interrupted := []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "The user doesn't want to proceed with this tool use."}}}
	b.emitToolEvent(job, "tool_call_update", map[string]any{"toolCallId": "toolu_fg", "title": "Find how Claude Code presents ultracode", "status": "failed", "content": interrupted}, "")
	b.emitToolEvent(job, "tool_call_update", map[string]any{"toolCallId": "toolu_bg", "title": "Run the gate", "status": "completed"}, "")

	if got, _ := status("bhboz80wh"); got != "failed" {
		t.Fatalf("an interrupted foreground task stayed %q after its call ended", got)
	}
	if got, _ := status("bbgshell1"); got != "running" {
		t.Fatalf("a background shell was settled by its launching call returning: %q", got)
	}

	// Background shells with no output file yet: one the provider flags as
	// backgrounded, one whose launch said run_in_background while its last
	// update carries no input at all.
	observe(map[string]any{"type": "started", "taskId": "bflagged1", "toolCallId": "toolu_flag", "description": "Flagged", "taskType": "local_bash", "backgrounded": true})
	b.emitToolEvent(job, "tool_call", map[string]any{"toolCallId": "toolu_launch", "title": "Bash", "status": "in_progress", "rawInput": map[string]any{"command": "sleep 40", "run_in_background": true}}, "")
	observe(map[string]any{"type": "started", "taskId": "blaunch1", "toolCallId": "toolu_launch", "description": "Sleep 40 seconds", "taskType": "local_bash"})
	b.emitToolEvent(job, "tool_call_update", map[string]any{"toolCallId": "toolu_flag", "title": "Flagged", "status": "completed"}, "")
	b.emitToolEvent(job, "tool_call_update", map[string]any{"toolCallId": "toolu_launch", "title": "Bash", "status": "completed"}, "")
	for _, id := range []string{"bflagged1", "blaunch1"} {
		if got, _ := status(id); got != "running" {
			t.Fatalf("background shell %s was settled by its call ending: %q", id, got)
		}
	}

	// The provider's own late ending must not revive or duplicate the row.
	observe(map[string]any{"type": "notification", "taskId": "bhboz80wh", "toolCallId": "toolu_fg", "status": "killed"})
	if got, count := status("bhboz80wh"); got == "running" || count != 1 {
		t.Fatalf("late provider ending revived the foreground task: %q ×%d", got, count)
	}
}

// Claude's SendMessage resumes a finished (or restart-orphaned) agent under a
// new call, but the agent's own messages keep naming the Agent call that
// launched it. A fresh row keyed to the resume call left its calls in a second
// group labeled with a raw tool id (DepthVision speed, 2026-10-15). The same
// row must come back, running, under the launch call.
func TestResumedClaudeAgentKeepsItsLaunchCall(t *testing.T) {
	t.Parallel()
	for _, evicted := range []bool{false, true} {
		manager := NewManager(Options{
			StateDir: t.TempDir(), SpawnedWorkReconcileInterval: time.Hour,
			SpawnedWorkPIDProbe: func([]string) (map[string][]int, bool) { return nil, false },
		})
		t.Cleanup(func() { manager.Reset() })
		observe := func(event map[string]any) {
			observeRawSpawnLifecycleFixture(t, manager, "tab-resume", "chat-resume", "session-resume", "claude", event)
		}
		observe(map[string]any{"type": "started", "taskId": "a281f4ed0610bdf38", "toolCallId": "toolu_launch", "description": "Build fast batched CUDA trainer", "subagentType": "general-purpose", "taskType": "local_agent"})
		observe(map[string]any{"type": "notification", "taskId": "a281f4ed0610bdf38", "toolCallId": "toolu_launch", "status": "completed", "summary": "stopped"})
		if evicted {
			// A restart orphans the row and it leaves the live set.
			key := spawnedWorkKey("tab-resume", "chat-resume", "a281f4ed0610bdf38")
			manager.spawnedWorkMu.Lock()
			manager.rememberSettledSpawnedWorkLocked(key, manager.spawnedWork[key].Item)
			delete(manager.spawnedWork, key)
			manager.spawnedWorkMu.Unlock()
		}
		resumedAt := time.Now().UTC()
		observe(map[string]any{"type": "started", "taskId": "a281f4ed0610bdf38", "toolCallId": "toolu_sendmessage", "description": "Build fast batched CUDA trainer", "taskType": "local_agent"})
		items := manager.ListSpawnedWork("tab-resume", "chat-resume")
		if len(items) != 1 {
			t.Fatalf("evicted=%v: a resume made %d rows: %#v", evicted, len(items), items)
		}
		item := items[0]
		if item.Status != "running" || item.ToolCallID != "toolu_launch" || item.Kind != "agent" || item.Label != "Build fast batched CUDA trainer" || item.FinishedAt != "" {
			t.Fatalf("evicted=%v: resumed agent = %#v", evicted, item)
		}
		if started, _ := time.Parse(time.RFC3339Nano, item.StartedAt); started.Before(resumedAt.Add(-time.Second)) {
			t.Fatalf("evicted=%v: a resumed run kept the old clock: %s", evicted, item.StartedAt)
		}
	}
}

// A foreground Agent call returns only when its agent ends, so an interrupt
// that ends the call ends the agent. Its row stayed "running" for 806 minutes
// because only foreground shells were settled this way (2026-10-16).
func TestForegroundClaudeAgentEndsWithItsToolCall(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{
		StateDir: t.TempDir(), SpawnedWorkReconcileInterval: time.Hour,
		SpawnedWorkPIDProbe: func([]string) (map[string][]int, bool) { return nil, false },
	})
	t.Cleanup(func() { manager.Reset() })
	b := &Bridge{providerID: "claude", manager: manager}
	job := &Job{ID: "job-fga", TabID: "tab-fga", ChatID: "chat-fga", SessionID: "session-fga"}
	observe := func(event map[string]any) {
		observeRawSpawnLifecycleFixture(t, manager, "tab-fga", "chat-fga", "session-fga", "claude", event)
	}
	status := func(taskID string) string {
		for _, item := range manager.ListSpawnedWork("tab-fga", "chat-fga") {
			if item.TaskID == taskID {
				return item.Status
			}
		}
		return "absent"
	}
	// Interrupted foreground agent.
	observe(map[string]any{"type": "started", "taskId": "a9144e0e554c20cd4", "toolCallId": "toolu_fg_agent", "description": "Map Sol repair pipeline and outcomes", "subagentType": "Explore", "taskType": "local_agent"})
	// Background agent: its launching call returns at once while it runs.
	b.emitToolEvent(job, "tool_call", map[string]any{"toolCallId": "toolu_bg_agent", "title": "Agent", "status": "in_progress", "rawInput": map[string]any{"description": "Background", "run_in_background": true}}, "")
	observe(map[string]any{"type": "started", "taskId": "abgagent", "toolCallId": "toolu_bg_agent", "description": "Background", "taskType": "local_agent", "backgrounded": true})
	// Foreground agent whose own background shell still runs.
	observe(map[string]any{"type": "started", "taskId": "awaiting", "toolCallId": "toolu_waiting", "description": "Waiting on its shell", "taskType": "local_agent"})
	observe(map[string]any{"type": "started", "taskId": "bchild", "toolCallId": "toolu_child", "description": "Its sleep", "taskType": "local_bash", "parentTaskId": "awaiting", "backgrounded": true})

	rejected := []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "The user doesn't want to proceed with this tool use."}}}
	b.emitToolEvent(job, "tool_call_update", map[string]any{"toolCallId": "toolu_fg_agent", "title": "Map Sol repair pipeline and outcomes", "status": "failed", "content": rejected}, "")
	b.emitToolEvent(job, "tool_call_update", map[string]any{"toolCallId": "toolu_bg_agent", "title": "Agent", "status": "completed"}, "")
	b.emitToolEvent(job, "tool_call_update", map[string]any{"toolCallId": "toolu_waiting", "title": "Agent", "status": "completed"}, "")

	if got := status("a9144e0e554c20cd4"); got == "running" {
		t.Fatal("an interrupted foreground agent stayed running after its call ended")
	}
	if got := status("abgagent"); got != "running" {
		t.Fatalf("a background agent was settled by its launch call returning: %q", got)
	}
	if got := status("awaiting"); got != "running" {
		t.Fatalf("an agent waiting on its own running shell was settled: %q", got)
	}
}

// Claude Code's live list (background_tasks_changed) holds only background
// tasks. A background task missing from it is gone, even when no ended event
// came; a foreground task is never in it and must never be judged by it.
func TestClaudeLiveListSettlesOnlyMissingBackgroundWork(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{
		StateDir: t.TempDir(), SpawnedWorkReconcileInterval: time.Hour,
		SpawnedWorkPIDProbe: func([]string) (map[string][]int, bool) { return nil, false },
	})
	t.Cleanup(func() { manager.Reset() })
	observe := func(event map[string]any) {
		observeRawSpawnLifecycleFixture(t, manager, "tab-live", "chat-live", "session-live", "claude", event)
	}
	observe(map[string]any{"type": "started", "taskId": "abg", "toolCallId": "toolu_abg", "description": "Background agent", "taskType": "local_agent", "backgrounded": true})
	observe(map[string]any{"type": "started", "taskId": "afg", "toolCallId": "toolu_afg", "description": "Foreground agent", "taskType": "local_agent"})
	observe(map[string]any{"type": "snapshot", "tasks": []any{map[string]any{"taskId": "abg", "taskType": "local_agent", "backgrounded": true}}})
	// The background agent dies with no ended event; the list drops it.
	observe(map[string]any{"type": "snapshot", "tasks": []any{}})
	manager.spawnedWorkMu.Lock()
	for _, rec := range manager.spawnedWork {
		if !rec.MissingSince.IsZero() {
			rec.MissingSince = time.Now().Add(-2 * spawnedWorkMissingGrace)
		}
	}
	manager.spawnedWorkMu.Unlock()
	manager.reconcileSpawnedWork()
	status := map[string]string{}
	for _, item := range manager.ListSpawnedWork("tab-live", "chat-live") {
		status[item.TaskID] = item.Status
	}
	if status["abg"] == "running" {
		t.Fatal("a background agent missing from Claude's live list stayed running")
	}
	if status["afg"] != "running" {
		t.Fatalf("a foreground agent was settled by a list it is never in: %q", status["afg"])
	}
}
