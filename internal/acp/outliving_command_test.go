package acp

import (
	"testing"
	"time"
)

// A top-level Codex chat's command that outlives its turn dies with the engine, so
// idle hibernation waits for it, but not forever: nothing can tell a build from a
// dev server that never ends.
func TestACommandThatOutlivedItsTurnHoldsTheEngineForABoundedTime(t *testing.T) {
	const tabID, chatID, sessionID, taskID = "tab-cmd", "chat-cmd", "session-cmd", "codex-exec-item-1"
	manager := NewManager(Options{
		StateDir: t.TempDir(), RuntimeProfile: "dev", HibernateTTL: time.Minute,
		SpawnedWorkReconcileInterval: time.Hour, RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	bridge := &Bridge{
		manager: manager, sessions: map[string]struct{}{sessionID: {}}, jobsBySession: make(map[string]*Job),
		tabID: tabID, chatID: chatID, state: StateIdle, lastActivity: time.Now().Add(-time.Hour),
	}
	manager.mu.Lock()
	manager.sessionBridge[sessionID] = bridge
	manager.mu.Unlock()
	report := func(kind, status string) {
		manager.observeProviderSpawnedWork(tabID, chatID, sessionID, "codex", providerSpawnedWorkUpdate{Kind: kind, Task: providerSpawnedWorkTask{
			TaskID: taskID, ToolCallID: "item-1", TaskType: "local_bash", Status: status, Backgrounded: true,
		}})
	}
	eligible := func() bool {
		_, ok := manager.hibernateCandidate(bridge, time.Now())
		return ok
	}
	if !eligible() {
		t.Fatal("an idle engine past its TTL should hibernate")
	}

	report("started", "running")
	if eligible() {
		t.Fatal("hibernation would kill a running command")
	}
	if manager.hasRunningSpawnedWorkForChat(tabID, chatID) {
		t.Fatal("a held command must not become a new rail row")
	}

	report("progress", "completed")
	if !eligible() {
		t.Fatal("a finished command still holds the engine")
	}

	report("started", "running")
	// A second report of the same command does not restart its clock.
	bridge.mu.Lock()
	bridge.outlivingCommands[taskID] = time.Now().Add(-manager.opts.SubagentBackgroundCeiling)
	bridge.mu.Unlock()
	report("started", "running")
	if !eligible() {
		t.Fatal("a command that never ends must not hold the engine forever")
	}
}
