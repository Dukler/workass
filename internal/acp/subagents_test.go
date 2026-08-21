package acp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSubagentModelLabelBuildsTurnosChip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, model, effort, want string
	}{
		{"Opus 4.8", "opus[1m]", "xhigh", "Opus4.8-xhigh"},
		{"GPT-5.5", "gpt-5.5-codex", "high", "GPT-5.5-high"},
		{"Sonnet 5", "sonnet-5", "", "Sonnet5"},
		{"", "raw-model-id", "low", "raw-model-id-low"},
		{"", "", "", ""},
	}
	for _, tc := range cases {
		if got := subagentModelLabel(tc.name, tc.model, tc.effort); got != tc.want {
			t.Errorf("subagentModelLabel(%q,%q,%q) = %q, want %q", tc.name, tc.model, tc.effort, got, tc.want)
		}
	}
}

func TestTrackedSubagentAppearsAndUpdatesInOwningSpawnedWorkFeed(t *testing.T) {
	t.Parallel()
	manager, _, session, ownerKey, root, _ := newSubagentLifecycleFixture(t, "spawned-work-feed")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	startSubagentParent(t, manager, session, root, "[mock:active-without-terminal] keep owner alive")

	run, err := manager.SpawnSubagent(ctx, SubagentSpawnOptions{
		OwnerKey: ownerKey, ParentChatID: session.ChatID, ParentTabID: session.TabID,
		Prompt: "[mock:active-without-terminal] stay live for feed progress", Label: "visible-running-child",
		ProviderID: session.Info.ProviderID, ModelID: "mock-deterministic", Effort: "high", ModeID: "ask", CWD: root,
	})
	if err != nil {
		t.Fatalf("spawn running child: %v", err)
	}
	assertRunningSubagentSpawnedWorkItem(t, manager, session, run, "visible-running-child")

	deadline := time.Now().Add(5 * time.Second)
	for {
		manager.updateSubagentActivity(run.ID, "tool", "Inspecting delegated files")
		item := spawnedWorkItemsByTaskID(manager.ListSpawnedWork(session.TabID, session.ChatID))[run.ID]
		if item.Summary == "Inspecting delegated files" && item.LastToolName == "tool" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("subagent progress was not mirrored into spawned work: %#v", item)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !manager.CancelSubagent(ownerKey, session.ChatID, session.TabID, run.ID) {
		t.Fatal("cancel spawned-work child failed")
	}
	waitForSubagentTerminal(t, manager, ownerKey, session, run.ID, 5*time.Second)
}

func TestTrackedSubagentTerminalStatusesSettleWithoutSyntheticWake(t *testing.T) {
	t.Parallel()
	manager, _, session, ownerKey, root, _ := newSubagentLifecycleFixture(t, "spawned-work-terminal")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	startSubagentParent(t, manager, session, root, "[mock:active-without-terminal] keep terminal owner alive")

	t.Run("cancelled maps to exited", func(t *testing.T) {
		run, err := manager.SpawnSubagent(ctx, SubagentSpawnOptions{
			OwnerKey: ownerKey, ParentChatID: session.ChatID, ParentTabID: session.TabID,
			Prompt: "[mock:active-without-terminal] cancel the delegated feed fixture", Label: "visible-cancelled-child",
			ProviderID: session.Info.ProviderID, ModelID: "mock-deterministic", Effort: "high", ModeID: "ask", CWD: root,
		})
		if err != nil {
			t.Fatalf("spawn cancellable child: %v", err)
		}
		if !manager.CancelSubagent(ownerKey, session.ChatID, session.TabID, run.ID) {
			t.Fatal("cancel child failed")
		}
		cancelled := waitForSubagentTerminal(t, manager, ownerKey, session, run.ID, 5*time.Second)
		if cancelled.Status != "cancelled" {
			t.Fatalf("cancelled run = %#v", cancelled)
		}
		assertTerminalSubagentSpawnedWorkItem(t, manager, session, run.ID, "exited", "Cancelled", "cancelled")
	})

	t.Run("done maps to exited", func(t *testing.T) {
		run, err := manager.SpawnSubagent(ctx, SubagentSpawnOptions{
			OwnerKey: ownerKey, ParentChatID: session.ChatID, ParentTabID: session.TabID,
			Prompt: "complete the delegated feed fixture", Label: "visible-completed-child",
			ProviderID: session.Info.ProviderID, ModelID: "mock-deterministic", Effort: "high", ModeID: "ask", CWD: root,
		})
		if err != nil {
			t.Fatalf("spawn completing child: %v", err)
		}
		completed := waitForSubagentTerminal(t, manager, ownerKey, session, run.ID, 8*time.Second)
		if completed.Status != "done" {
			t.Fatalf("completed run = %#v", completed)
		}
		assertTerminalSubagentSpawnedWorkItem(t, manager, session, run.ID, "exited", "Completed", "done")
	})

	t.Run("failure maps to failed", func(t *testing.T) {
		run, err := manager.SpawnSubagent(ctx, SubagentSpawnOptions{
			OwnerKey: ownerKey, ParentChatID: session.ChatID, ParentTabID: session.TabID,
			Prompt: "[mock:error] fail the delegated feed fixture", Label: "visible-failed-child",
			ProviderID: session.Info.ProviderID, ModelID: "mock-deterministic", Effort: "high", ModeID: "ask", CWD: root,
		})
		if err != nil {
			t.Fatalf("spawn failing child: %v", err)
		}
		failed := waitForSubagentTerminal(t, manager, ownerKey, session, run.ID, 8*time.Second)
		if failed.Status != "failed" {
			t.Fatalf("failed run = %#v", failed)
		}
		assertTerminalSubagentSpawnedWorkItem(t, manager, session, run.ID, "failed", "Failed", "failed")
	})
}

func assertRunningSubagentSpawnedWorkItem(t *testing.T, manager *Manager, session subagentParentSession, run SubagentRun, label string) {
	t.Helper()
	items := spawnedWorkItemsByTaskID(manager.ListSpawnedWork(session.TabID, session.ChatID))
	item, ok := items[run.ID]
	if !ok || item.ID != run.ID || item.Kind != "subagent" || item.Status != "running" ||
		item.Label != label || item.ProviderID != run.ProviderID || item.StartedAt == "" || item.UpdatedAt == "" {
		t.Fatalf("running subagent spawned-work item = %#v; all items = %#v", item, items)
	}
}

func assertTerminalSubagentSpawnedWorkItem(t *testing.T, manager *Manager, session subagentParentSession, id, status, summary, phase string) {
	t.Helper()
	item := spawnedWorkItemsByTaskID(manager.ListSpawnedWork(session.TabID, session.ChatID))[id]
	if item.Status != status || item.Summary != summary || item.LastToolName != phase ||
		item.FinishedAt == "" {
		t.Fatalf("terminal subagent spawned-work item = %#v", item)
	}
}

func TestCoordinatedSubagentUsesExplicitSelectionRoutesPermissionAndNamespacesTools(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir:  root,
		StateDir: filepath.Join(t.TempDir(), "state"),
		Provider: ProviderConfig{
			Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Label: "Workass Mock ACP",
		},
		Broadcast: events.Broadcast, PermissionTimeout: 6 * time.Second,
		StdoutFlushInterval: 10 * time.Millisecond, ThoughtFlushInterval: 10 * time.Millisecond,
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{ChatID: "chat-parent", TabID: "tab-parent"})
	if err != nil {
		t.Fatalf("new parent session: %v", err)
	}
	manager.mu.Lock()
	ownerKey := manager.agentOwnerBySession[session.SessionID]
	manager.mu.Unlock()
	if ownerKey == "" {
		t.Fatal("parent session has no agent owner key")
	}
	parent, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, ChatID: "chat-parent", TabID: "tab-parent",
		ProviderID: session.ProviderID, ModelID: "mock-deterministic", ModeID: "ask",
		CWD: root, Prompt: "[mock:slow] parent waits while child runs",
	})
	if err != nil {
		t.Fatalf("start parent: %v", err)
	}
	parentID := asString(parent["id"])

	run, err := manager.SpawnSubagent(ctx, SubagentSpawnOptions{
		OwnerKey: ownerKey, Prompt: "[mock:permission] inspect child path", Label: "permission-child",
		ProviderID: session.ProviderID, ModelID: "mock-deterministic", Effort: "high", ModeID: "ask", CWD: root,
	})
	if err != nil {
		t.Fatalf("spawn subagent: %v", err)
	}
	if run.Status != "running" || run.ID == "" || run.SessionID != "" {
		t.Fatalf("spawn must return immediately with running id: %#v", run)
	}

	waitAttention := make(chan SubagentRun, 1)
	waitAttentionErr := make(chan error, 1)
	go func() {
		attention, waitErr := manager.WaitSubagent(ctx, ownerKey, "", "", run.ID, 4*time.Second)
		waitAttention <- attention
		waitAttentionErr <- waitErr
	}()
	permission := events.waitFor(t, 4*time.Second, func(ev collectedEvent) bool {
		if ev.channel != "chat:permission-request" {
			return false
		}
		payload, _ := ev.payload.(map[string]any)
		return asString(payload["jobId"]) == parentID
	}).payload.(map[string]any)
	attention := <-waitAttention
	if err := <-waitAttentionErr; err != nil {
		t.Fatalf("wait permission attention: %v", err)
	}
	if attention.Status != "running" || attention.Phase != "waiting_permission" || !attention.NeedsAttention ||
		attention.Attention == nil || attention.Attention.Kind != "permission" || !attention.Attention.Active ||
		!strings.Contains(attention.LatestActivity, "Mock permission gate") {
		t.Fatalf("permission attention snapshot = %#v", attention)
	}
	wake, err := manager.WaitSubagents(ctx, ownerKey, "", "", []string{run.ID}, "all", 2*time.Second)
	if err != nil {
		t.Fatalf("wait-many permission attention: %v", err)
	}
	attentionRuns := wake["attention"].([]SubagentRun)
	if wake["needsAttention"] != true || wake["timedOut"] != false || len(attentionRuns) != 1 || attentionRuns[0].ID != run.ID {
		t.Fatalf("wait-many permission attention = %#v", wake)
	}
	if !manager.PermissionDecide(asString(permission["id"]), "allow-once") {
		t.Fatalf("permission decision was not accepted: %#v", permission)
	}

	var finished SubagentRun
	deadline := time.Now().Add(5 * time.Second)
	for {
		finished, err = manager.WaitSubagent(ctx, ownerKey, "", "", run.ID, time.Until(deadline))
		if err != nil {
			t.Fatalf("wait subagent: %v", err)
		}
		if finished.Status != "running" && !finished.NeedsAttention {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("permission attention never settled: %#v", finished)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if finished.Status != "done" || !strings.Contains(finished.Result, "Permission outcome: selected allow-once") {
		t.Fatalf("finished subagent = %#v", finished)
	}

	var headerCount, leafCount int
	for _, ev := range events.jobEvents(parentID, "acp") {
		tool := mapFromAny(ev["event"])
		if asString(tool["subagentId"]) != run.ID {
			continue
		}
		if value, _ := tool["subagentHeader"].(bool); value {
			headerCount++
			continue
		}
		leafCount++
		if id := asString(tool["id"]); id != "" && !strings.HasPrefix(id, run.ID+":") {
			t.Fatalf("child tool id is not namespaced: %q", id)
		}
	}
	if headerCount < 2 || leafCount < 1 {
		t.Fatalf("subagent lifecycle/tool events header=%d leaf=%d events=%#v", headerCount, leafCount, events.jobEvents(parentID, "acp"))
	}
}

func TestSubagentPermissionAttentionStaysUnreadAfterFastResolution(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{})
	run := &SubagentRun{ID: "child", Status: "running", Phase: "working"}
	manager.mu.Lock()
	manager.subagents[run.ID] = run
	manager.mu.Unlock()
	job := &Job{SubagentID: run.ID}

	manager.notifySubagentPermissionForJob(job, "Read a bounded mock file")
	manager.resolveSubagentPermissionForJob(job)

	manager.mu.Lock()
	unread := copySubagentRun(run)
	if run.Attention != nil {
		run.attentionDeliveredSequence = run.Attention.Sequence
	}
	delivered := copySubagentRun(run)
	manager.mu.Unlock()
	if !unread.NeedsAttention || unread.Attention == nil || unread.Attention.Active || unread.Attention.ResolvedAt == "" ||
		!strings.Contains(unread.Attention.Message, "Read a bounded mock file") {
		t.Fatalf("resolved unread attention = %#v", unread)
	}
	if delivered.NeedsAttention {
		t.Fatalf("delivered attention remained unread = %#v", delivered)
	}
}

func TestDreamSubagentCatalogProgressMessageWaitManyAndDurableReceipt(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	holdRelease := filepath.Join(t.TempDir(), "release")
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir: root, StateDir: stateDir,
		Provider: ProviderConfig{
			Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Label: "Workass Mock ACP",
			Env: map[string]string{"WORKASS_MOCK_ACP_HOLD_FILE": holdRelease},
		},
		Broadcast: events.Broadcast, PermissionTimeout: 2 * time.Second,
		StdoutFlushInterval: 10 * time.Millisecond, ThoughtFlushInterval: 10 * time.Millisecond,
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	manager.SetModelScores(map[string]any{
		"mock": map[string]any{"mock-deterministic": map[string]any{"intelligence": 9, "taste": 8, "cost": 4, "note": "Reliable deterministic fixture"}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{ChatID: "dream-parent", TabID: "dream-tab", CWD: root})
	if err != nil {
		t.Fatalf("new parent session: %v", err)
	}
	manager.mu.Lock()
	ownerKey := manager.agentOwnerBySession[session.SessionID]
	manager.mu.Unlock()
	parent, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, ChatID: "dream-parent", TabID: "dream-tab",
		ProviderID: session.ProviderID, ModelID: "mock-deterministic[high]", ModeID: "ask",
		// Keep the synthetic parent alive past the child so this test exercises
		// current-turn orchestration; cross-turn survival is covered separately.
		CWD: root, Prompt: "[mock:slow] [mock:permission] parent coordinates children",
	})
	if err != nil {
		t.Fatalf("start parent: %v", err)
	}

	catalog, err := manager.AgentCatalog(ctx, ownerKey, "", "")
	if err != nil {
		t.Fatalf("agent catalog: %v", err)
	}
	if catalog["schemaVersion"] != 2 || catalog["models"] != nil || catalog["modes"] != nil {
		t.Fatalf("agent catalog is not normalized v2: %#v", catalog)
	}
	active := mapFromAny(catalog["active"])
	if active["modelId"] != "mock-deterministic" || active["effort"] != "high" || active["cwd"] != root {
		t.Fatalf("normalized active selection = %#v", active)
	}
	limits := mapFromAny(catalog["limits"])
	if limits["maxConcurrentPerTurn"] != maxConcurrentSubagentsPerTurn {
		t.Fatalf("catalog limits = %#v", limits)
	}

	run, err := manager.SpawnSubagent(ctx, SubagentSpawnOptions{
		OwnerKey: ownerKey, Prompt: "[mock:hold-until-steer] [mock:steer] inspect dream path", Label: "dream-child",
		PermissionIntent: "inherit", CWD: "inherit",
	})
	if err != nil {
		t.Fatalf("spawn inherited subagent: %v", err)
	}
	if run.ProviderID != session.ProviderID || run.ModelID != "mock-deterministic" || run.Effort != "high" || run.CWD != root || run.Phase != "starting" {
		t.Fatalf("inherited run = %#v", run)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		listed := manager.ListSubagents(ownerKey, "", "")
		if len(listed) == 1 && listed[0].SessionID != "" && listed[0].JobID != "" && listed[0].Phase == "working" && listed[0].ProgressSequence > 1 && listed[0].LatestActivity != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("subagent never exposed progress: %#v", listed)
		}
		time.Sleep(10 * time.Millisecond)
	}
	manager.mu.Lock()
	childBeforeMessage := copySubagentRun(manager.subagents[run.ID])
	manager.mu.Unlock()
	if childBeforeMessage.Status != "running" {
		t.Fatalf("held subagent finished before live message: %#v", childBeforeMessage)
	}

	delivery, err := manager.MessageSubagent(ownerKey, "", "", run.ID, "focus on the receipt contract")
	if err != nil {
		t.Fatalf("message subagent: %v", err)
	}
	if delivery["delivery"] != "live" {
		t.Fatalf("generic mock message delivery = %#v", delivery)
	}
	if err := os.WriteFile(holdRelease, []byte("release"), 0o600); err != nil {
		t.Fatalf("release mock hold: %v", err)
	}
	waited, err := manager.WaitSubagents(ctx, ownerKey, "", "", []string{run.ID}, "all", 8*time.Second)
	if err != nil {
		t.Fatalf("wait many: %v", err)
	}
	completed := waited["completed"].([]SubagentRun)
	// Delivery is asserted from the protocol acknowledgement above. Do not use
	// model/mock prose as the oracle: a steer can land after the fixture's final
	// text chunk while still being validly accepted by the ACP extension.
	if waited["timedOut"] != false || len(completed) != 1 || completed[0].Status != "done" {
		t.Fatalf("waited result = %#v", waited)
	}
	receipts := manager.ListSubagentReceipts(ownerKey, "", "", 10)
	if len(receipts) != 1 || receipts[0].ReceiptID != run.ID || receipts[0].ResultTruncated {
		t.Fatalf("durable receipts = %#v", receipts)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "subagent-receipts", "dream-tab.jsonl")); err != nil {
		t.Fatalf("receipt file: %v", err)
	}
	retried, err := manager.RetrySubagent(ctx, ownerKey, "", "", run.ID, "verify the corrected receipt")
	if err != nil {
		t.Fatalf("retry subagent: %v", err)
	}
	if retried.RetryOf != run.ID || retried.ProviderID != run.ProviderID || retried.ModelID != run.ModelID || retried.Effort != run.Effort || retried.ModeID != run.ModeID || retried.CWD != run.CWD {
		t.Fatalf("retry did not preserve resolved selection: original=%#v retry=%#v", run, retried)
	}
	retryResult, err := manager.WaitSubagent(ctx, ownerKey, "", "", retried.ID, 5*time.Second)
	if err != nil || retryResult.Status != "done" {
		t.Fatalf("retry result=%#v err=%v", retryResult, err)
	}
	receipts = manager.ListSubagentReceipts(ownerKey, "", "", 10)
	if len(receipts) != 2 || receipts[1].RetryOf != run.ID {
		t.Fatalf("retry receipts = %#v", receipts)
	}
	_ = manager.CancelJob(asString(parent["id"]))
}

func TestSubagentPermissionInheritanceUsesEffectiveLiveModeAndNeverDowngrades(t *testing.T) {
	t.Parallel()
	manager, _ := newFakeManager(t, "codex-controls", Options{Provider: ProviderConfig{ID: "codex"}})
	t.Cleanup(func() { manager.Reset() })
	root := repoRoot(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := manager.NewSession(ctx, SessionOptions{
		ChatID: "permission-parent", TabID: "permission-tab", CWD: root,
		ProviderID: "codex", ModelID: "gpt-5.6-sol[xhigh]", ModeID: "agent-full-access",
	})
	if err != nil {
		t.Fatalf("new full-access parent session: %v", err)
	}
	if _, err := manager.SetMode(ctx, session.SessionID, "agent-full-access"); err != nil {
		t.Fatalf("set live parent mode: %v", err)
	}

	// Reproduce the runtime hole: the provider session knows the effective mode,
	// but a partial/cold job snapshot omitted it. Omitted child permission must
	// recover the live value instead of silently choosing read-only.
	parent := &Job{
		ProviderID: "codex", SessionID: session.SessionID,
		ChatID: "permission-parent", TabID: "permission-tab", CWD: root,
		startOpts: JobStartOptions{ProviderID: "codex", ModelID: "gpt-5.6-sol[xhigh]", CWD: root},
	}
	providerID, modelID, effort, modeID, err := manager.resolveSubagentSelection(ctx, parent, SubagentSpawnOptions{})
	if err != nil {
		t.Fatalf("inherit live full-access mode: %v", err)
	}
	if providerID != "codex" || modelID != "gpt-5.6-sol" || effort != "xhigh" || modeID != "agent-full-access" {
		t.Fatalf("inherited selection = provider=%q model=%q effort=%q mode=%q", providerID, modelID, effort, modeID)
	}
	active := mapFromAny(manager.agentCatalogV2(ctx, parent)["active"])
	if active["modeId"] != "agent-full-access" {
		t.Fatalf("catalog active permission = %#v", active)
	}

	// With no effective parent mode anywhere, inheritance is unknowable. A
	// visible error is safer than an unattended read-only downgrade.
	unknownParent := &Job{
		ProviderID: "codex", CWD: root,
		startOpts: JobStartOptions{ProviderID: "codex", ModelID: "gpt-5.6-sol[xhigh]", CWD: root},
	}
	_, _, _, _, err = manager.resolveSubagentSelection(ctx, unknownParent, SubagentSpawnOptions{})
	if err == nil || !strings.Contains(err.Error(), "cannot inherit") {
		t.Fatalf("unknown permission inheritance error = %v", err)
	}

	_, _, _, explicitMode, err := manager.resolveSubagentSelection(ctx, unknownParent, SubagentSpawnOptions{PermissionIntent: "full"})
	if err != nil || explicitMode != "agent-full-access" {
		t.Fatalf("explicit full permission = mode %q err=%v", explicitMode, err)
	}
}

func TestSubagentWaitDoesNotReportTerminalBeforeReceiptCommit(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	run := &SubagentRun{
		ID: "receipt-pending", ParentJobID: "parent-job", Status: "done",
		ReceiptID: "receipt-pending", done: make(chan struct{}),
	}
	manager.mu.Lock()
	manager.subagents[run.ID] = run
	manager.mu.Unlock()

	completed, running, err := manager.subagentSnapshotsForParent("parent-job", []string{run.ID})
	if err != nil || len(completed) != 0 || len(running) != 1 {
		t.Fatalf("pre-commit snapshot completed=%#v running=%#v err=%v", completed, running, err)
	}
	manager.mu.Lock()
	run.receiptCommitted = true
	manager.mu.Unlock()
	close(run.done)
	completed, running, err = manager.subagentSnapshotsForParent("parent-job", []string{run.ID})
	if err != nil || len(completed) != 1 || len(running) != 0 {
		t.Fatalf("committed snapshot completed=%#v running=%#v err=%v", completed, running, err)
	}
}

func TestSubagentAPIDeniesMissingOrStaleOwner(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	if _, err := manager.AgentCatalog(context.Background(), "missing-owner", "chat", "tab"); err == nil {
		t.Fatal("catalog accepted stale owner")
	}
	if got := manager.ListSubagents("missing-owner", "chat", "tab"); len(got) != 0 {
		t.Fatalf("stale owner listed subagents: %#v", got)
	}
	if manager.CancelSubagent("missing-owner", "chat", "tab", "anything") {
		t.Fatal("stale owner cancelled a subagent")
	}
}

func TestSubagentCancelClearsPermissionAndValidOwnerIsolation(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir: root, StateDir: filepath.Join(t.TempDir(), "state"),
		Provider: ProviderConfig{
			Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Label: "Workass Mock ACP",
		},
		Broadcast: events.Broadcast, PermissionTimeout: 5 * time.Second,
		StdoutFlushInterval: 10 * time.Millisecond, ThoughtFlushInterval: 10 * time.Millisecond,
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	newParent := func(chatID, tabID string) (SessionInfo, string, string) {
		session, err := manager.NewSession(ctx, SessionOptions{ChatID: chatID, TabID: tabID})
		if err != nil {
			t.Fatalf("new parent %s: %v", chatID, err)
		}
		manager.mu.Lock()
		ownerKey := manager.agentOwnerBySession[session.SessionID]
		manager.mu.Unlock()
		job, err := manager.StartJob(context.Background(), JobStartOptions{
			Kind: "app-chat", SessionID: session.SessionID, ChatID: chatID, TabID: tabID,
			ProviderID: session.ProviderID, ModelID: "mock-deterministic", ModeID: "ask",
			CWD: root, Prompt: "[mock:active-without-terminal] keep the owning turn alive",
		})
		if err != nil {
			t.Fatalf("start parent %s: %v", chatID, err)
		}
		return session, ownerKey, asString(job["id"])
	}

	sessionA, ownerA, parentA := newParent("chat-a", "tab-a")
	_, ownerB, _ := newParent("chat-b", "tab-b")
	run, err := manager.SpawnSubagent(ctx, SubagentSpawnOptions{
		OwnerKey: ownerA, Prompt: "[mock:permission] wait until cancelled", Label: "cancel-child",
		ProviderID: sessionA.ProviderID, ModelID: "mock-deterministic", Effort: "high", ModeID: "ask", CWD: root,
	})
	if err != nil {
		t.Fatalf("spawn subagent: %v", err)
	}

	permission := events.waitFor(t, 5*time.Second, func(ev collectedEvent) bool {
		if ev.channel != "chat:permission-request" {
			return false
		}
		payload, _ := ev.payload.(map[string]any)
		return asString(payload["jobId"]) == parentA
	}).payload.(map[string]any)

	if got := manager.ListSubagents(ownerA, "", ""); len(got) != 1 || got[0].ID != run.ID {
		t.Fatalf("owner A subagents = %#v", got)
	}
	if got := manager.ListSubagents(ownerB, "", ""); len(got) != 0 {
		t.Fatalf("owner B saw owner A subagents: %#v", got)
	}
	if manager.CancelSubagent(ownerB, "", "", run.ID) {
		t.Fatal("owner B cancelled owner A subagent")
	}
	if _, err := manager.WaitSubagent(ctx, ownerB, "", "", run.ID, time.Second); err == nil {
		t.Fatal("owner B waited on owner A subagent")
	}
	if !manager.CancelSubagent(ownerA, "", "", run.ID) {
		t.Fatal("owner A could not cancel its subagent")
	}
	for _, raw := range manager.PendingPermissions() {
		pending := mapFromAny(raw)
		if asString(pending["id"]) == asString(permission["id"]) {
			t.Fatalf("cancelled child permission remained pending: %#v", pending)
		}
	}
	var finished SubagentRun
	deadline := time.Now().Add(5 * time.Second)
	for {
		finished, err = manager.WaitSubagent(ctx, ownerA, "", "", run.ID, time.Until(deadline))
		if err != nil {
			t.Fatalf("wait cancelled subagent: %v", err)
		}
		if finished.Status != "running" && !finished.NeedsAttention {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancelled permission attention never settled: %#v", finished)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if finished.Status != "cancelled" || finished.Error != "" {
		t.Fatalf("cancelled subagent = %#v", finished)
	}

	foundCancelledHeader := false
	for _, ev := range events.jobEvents(parentA, "acp") {
		tool := mapFromAny(ev["event"])
		if asString(tool["subagentId"]) == run.ID && tool["subagentHeader"] == true && asString(tool["status"]) == "cancelled" {
			foundCancelledHeader = true
		}
	}
	if !foundCancelledHeader {
		t.Fatalf("no cancelled header for %s; finished=%#v events=%#v", run.ID, finished, events.jobEvents(parentA, "acp"))
	}
}

func TestNestedSubagentOwnershipIsImmediateAndCascadeIsScoped(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{})
	rootJob := &Job{ID: "root-job", Status: "running", ChatID: "root-chat", TabID: "root-tab"}
	childJob := &Job{
		ID: "child-job", Status: "running", ChatID: "subagent:a", TabID: "subagent:a",
		VisibleJobID: "root-job", VisibleSessionID: "root-session",
	}
	manager.mu.Lock()
	manager.jobs[rootJob.ID] = rootJob
	manager.jobs[childJob.ID] = childJob
	manager.agentOwners["root-owner"] = agentOwnerBinding{ChatID: rootJob.ChatID, TabID: rootJob.TabID}
	manager.agentOwners["child-owner"] = agentOwnerBinding{ChatID: childJob.ChatID, TabID: childJob.TabID}
	manager.mu.Unlock()

	newRun := func(id, parentJobID string) (*SubagentRun, <-chan struct{}) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		run := &SubagentRun{
			ID: id, Label: id, Status: "running", ParentJobID: parentJobID,
			RootJobID: rootJob.ID, cancel: cancel, done: done,
		}
		go func() {
			<-ctx.Done()
			manager.mu.Lock()
			if run.Status == "running" {
				run.Status = "cancelled"
			}
			manager.mu.Unlock()
			close(done)
		}()
		return run, ctx.Done()
	}
	runA, _ := newRun("a", rootJob.ID)
	runB, cancelledB := newRun("b", childJob.ID)
	runSibling, cancelledSibling := newRun("sibling", rootJob.ID)
	manager.mu.Lock()
	manager.subagents[runA.ID] = runA
	manager.subagents[runB.ID] = runB
	manager.subagents[runSibling.ID] = runSibling
	manager.mu.Unlock()

	rootChildren := manager.ListSubagents("root-owner", "", "")
	if len(rootChildren) != 2 || rootChildren[0].ID != "a" || rootChildren[1].ID != "sibling" {
		t.Fatalf("root direct children = %#v", rootChildren)
	}
	childChildren := manager.ListSubagents("child-owner", "", "")
	if len(childChildren) != 1 || childChildren[0].ID != "b" {
		t.Fatalf("child direct children = %#v", childChildren)
	}
	if manager.CancelSubagent("child-owner", "", "", runSibling.ID) {
		t.Fatal("nested owner cancelled a root sibling")
	}

	manager.cancelAndDrainSubagentsForOwner(childJob.ID, time.Second)
	select {
	case <-cancelledB:
	default:
		t.Fatal("nested child was not cancelled with its immediate owner")
	}
	select {
	case <-cancelledSibling:
		t.Fatal("owner-scoped cascade cancelled a root sibling")
	default:
	}

	manager.cancelSubagentsForParent(rootJob.ID)
	select {
	case <-cancelledSibling:
	case <-time.After(time.Second):
		t.Fatal("root cancellation did not reach root-attributed children")
	}
}

func TestSubagentSurvivesCancelledParentSettlesAndWritesReceipt(t *testing.T) {
	t.Parallel()
	manager, events, session, ownerKey, root, stateDir := newSubagentLifecycleFixture(t, "cancel-survival")
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	parent := startSubagentParent(t, manager, session, root, "[mock:active-without-terminal] parent is explicitly cancelled")
	run, err := manager.SpawnSubagent(ctx, SubagentSpawnOptions{
		OwnerKey: ownerKey, ParentChatID: session.ChatID, ParentTabID: session.TabID,
		Prompt: "[mock:slow] child survives parent cancellation", Label: "cancel-survivor",
		ProviderID: session.Info.ProviderID, ModelID: "mock-deterministic", Effort: "high", ModeID: "ask", CWD: root,
	})
	if err != nil {
		t.Fatalf("spawn child: %v", err)
	}
	// Cancel an actually active provider turn. StartJob publishes its receipt
	// before session/prompt is dispatched, so cancelling immediately here races
	// the fixture's request queue and can cancel "nothing" before the prompt is
	// active. The fixture's completed plan is emitted synchronously immediately
	// before it parks the prompt, making it the deterministic protocol boundary
	// this test needs.
	parentID := asString(parent["id"])
	activeDeadline := time.Now().Add(4 * time.Second)
	for {
		providerParked := false
		for _, payload := range events.jobEvents(parentID, "acp") {
			event := mapFromAny(payload["event"])
			if asString(event["kind"]) != "plan" {
				continue
			}
			entries, _ := event["entries"].([]any)
			if len(entries) == 2 && asString(mapFromAny(entries[0])["status"]) == "completed" && asString(mapFromAny(entries[1])["status"]) == "completed" {
				providerParked = true
				break
			}
		}
		if providerParked {
			break
		}
		if time.Now().After(activeDeadline) {
			t.Fatalf("parent provider turn did not reach its deterministic parked boundary before cancellation")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !manager.CancelJob(parentID) {
		t.Fatal("parent cancellation was not accepted")
	}
	waitForSubagentAdoption(t, manager, run.ID, 4*time.Second)
	manager.mu.Lock()
	status := manager.subagents[run.ID].Status
	manager.mu.Unlock()
	if status != "running" {
		t.Fatalf("cancelled parent terminated child: status=%s", status)
	}
	finished, err := manager.WaitSubagent(ctx, ownerKey, session.ChatID, session.TabID, run.ID, 8*time.Second)
	if err != nil || finished.Status != "done" || !finished.Adopted {
		t.Fatalf("turnless wait after parent cancellation = %#v err=%v", finished, err)
	}
	receipts := manager.ListSubagentReceipts(ownerKey, session.ChatID, session.TabID, 10)
	if len(receipts) != 1 || receipts[0].SubagentID != run.ID || receipts[0].Status != "done" {
		t.Fatalf("survivor receipt = %#v", receipts)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "subagent-receipts", safeArchiveName(session.TabID)+".jsonl")); err != nil {
		t.Fatalf("survivor receipt file: %v", err)
	}
	if len(events.jobEvents(asString(parent["id"]), "acp")) == 0 {
		t.Fatal("surviving child lost its existing visible parent route")
	}

	// Normal completion is the same adoption boundary with a different parent
	// terminal reason. Reuse the established provider/session instead of paying
	// for another parent engine and catalog handshake.
	startSubagentParent(t, manager, session, root, "[mock:slow] parent completes normally")
	normalRun, err := manager.SpawnSubagent(ctx, SubagentSpawnOptions{
		OwnerKey: ownerKey, ParentChatID: session.ChatID, ParentTabID: session.TabID,
		Prompt: "[mock:slow] [mock:active-without-terminal] child remains active", Label: "normal-survivor",
		ProviderID: session.Info.ProviderID, ModelID: "mock-deterministic", Effort: "high", ModeID: "ask", CWD: root,
	})
	if err != nil {
		t.Fatalf("spawn normal-completion survivor: %v", err)
	}
	waitForSubagentAdoption(t, manager, normalRun.ID, 6*time.Second)
	manager.mu.Lock()
	normalSnapshot := copySubagentRun(manager.subagents[normalRun.ID])
	manager.mu.Unlock()
	if normalSnapshot.Status != "running" || !normalSnapshot.Adopted || normalSnapshot.AdoptedAt == "" {
		t.Fatalf("normally completed parent did not leave child running: %#v", normalSnapshot)
	}
	if !manager.CancelSubagent(ownerKey, session.ChatID, session.TabID, normalRun.ID) {
		t.Fatal("normal-completion survivor cleanup failed")
	}
	waitForSubagentTerminal(t, manager, ownerKey, session, normalRun.ID, 5*time.Second)
}

func TestRejectedSteerDoesNotEndParentOrPrematurelyAdoptRunningSubagents(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		mode       string
		providerID string
		strategy   string
	}{
		{name: "claude-unsupported", mode: "interruptible-prompt", providerID: "claude", strategy: "unsupported"},
		{name: "codex-rejected-native", mode: "codex-steer-rejected", providerID: "codex", strategy: "rejected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manager, events := newFakeManager(t, tc.mode, Options{
				Provider: ProviderConfig{ID: tc.providerID},
			})
			t.Cleanup(func() { manager.Reset() })
			tabID := "steer-adopt-" + tc.name
			chatID := "chat-" + tabID
			root := repoRoot(t)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			session, err := manager.NewSession(ctx, SessionOptions{TabID: tabID, ChatID: chatID, CWD: root})
			if err != nil {
				t.Fatalf("new session: %v", err)
			}
			manager.mu.Lock()
			ownerKey := manager.agentOwnerBySession[session.SessionID]
			manager.mu.Unlock()
			if ownerKey == "" {
				t.Fatal("parent session has no owner key")
			}
			parent := startAppChatJob(t, manager, session.SessionID, tabID, "parent waits for steer")
			_ = events.waitJobType(t, jobID(parent), "acp", 2*time.Second)

			spawnCtx, spawnCancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer spawnCancel()
			run, err := manager.SpawnSubagent(spawnCtx, SubagentSpawnOptions{
				OwnerKey: ownerKey, ParentChatID: chatID, ParentTabID: tabID,
				Prompt: "child keeps running after parent steer", Label: "steer-survivor",
				ProviderID: session.ProviderID, ModelID: "fake-model", ModeID: "ask", CWD: root,
			})
			if err != nil {
				t.Fatalf("spawn child: %v", err)
			}
			if run.Status != "running" {
				t.Fatalf("spawned child = %#v", run)
			}

			res := manager.Steer(session.SessionID, "redirect parent", nil, "")
			if res["ok"] != false || res["queued"] != false || res["interrupted"] == true || res["strategy"] != tc.strategy {
				t.Fatalf("steer rejection result = %#v", res)
			}
			if _, running := manager.RunningJobForChat(tabID, chatID); !running {
				t.Fatal("rejected steer ended the parent turn")
			}
			manager.mu.Lock()
			beforeCancel := copySubagentRun(manager.subagents[run.ID])
			manager.mu.Unlock()
			if beforeCancel.Status != "running" || beforeCancel.Adopted {
				t.Fatalf("rejected steer prematurely adopted child: %#v", beforeCancel)
			}
			if cancelled := manager.CancelJobResult(jobID(parent)); !cancelled.Cancelled {
				t.Fatalf("explicit cleanup Stop did not cancel parent: %#v", cancelled)
			}
			assertJobStatus(t, events.waitJobEnd(t, jobID(parent), 2*time.Second), "failed", 130, "cancelled")
			waitForSubagentAdoption(t, manager, run.ID, 4*time.Second)

			manager.mu.Lock()
			snapshot := copySubagentRun(manager.subagents[run.ID])
			manager.mu.Unlock()
			if snapshot.Status != "running" || !snapshot.Adopted || snapshot.ParentJobID != jobID(parent) {
				t.Fatalf("explicitly cancelled parent did not adopt child: %#v", snapshot)
			}
		})
	}
}

func TestUnsupportedSubagentSteerKeepsOneDurableFollowupWithoutInterrupting(t *testing.T) {
	t.Parallel()
	manager, events := newFakeManager(t, "interruptible-prompt", Options{
		Provider: ProviderConfig{ID: "claude"},
	})
	t.Cleanup(func() { manager.Reset() })
	const tabID = "subagent-unsupported-steer-tab"
	const chatID = "chat-subagent-unsupported-steer-tab"
	session := newFakeSession(t, manager, tabID)
	manager.mu.Lock()
	ownerKey := manager.agentOwnerBySession[session.SessionID]
	manager.mu.Unlock()
	if ownerKey == "" {
		t.Fatal("parent session has no owner key")
	}
	parent := startAppChatJob(t, manager, session.SessionID, tabID, "parent keeps running")
	_ = events.waitJobType(t, jobID(parent), "acp", 2*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	run, err := manager.SpawnSubagent(ctx, SubagentSpawnOptions{
		OwnerKey: ownerKey, ParentChatID: chatID, ParentTabID: tabID,
		Prompt: "child keeps running for a durable follow-up", Label: "unsupported-steer-child",
		ProviderID: session.ProviderID, ModelID: "fake-model", ModeID: "ask", CWD: repoRoot(t),
	})
	if err != nil {
		t.Fatalf("spawn child: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		manager.mu.Lock()
		child := manager.subagents[run.ID]
		ready := child != nil && child.Status == "running" && child.acceptingMessages && child.SessionID != "" && child.JobID != ""
		manager.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child never became ready for coordinator feedback")
		}
		time.Sleep(10 * time.Millisecond)
	}

	delivery, err := manager.MessageSubagent(ownerKey, chatID, tabID, run.ID, "persist this exact follow-up")
	if err != nil || delivery["ok"] != true || delivery["delivery"] != "followup" || delivery["strategy"] != "unsupported" {
		t.Fatalf("unsupported subagent delivery = %#v err=%v", delivery, err)
	}
	manager.mu.Lock()
	child := copySubagentRun(manager.subagents[run.ID])
	followupCount := len(manager.subagents[run.ID].followups)
	manager.mu.Unlock()
	if child.Status != "running" || followupCount != 1 {
		t.Fatalf("unsupported steer interrupted child or lost its owner: child=%#v followups=%d", child, followupCount)
	}
	if !manager.CancelSubagent(ownerKey, chatID, tabID, run.ID) {
		t.Fatal("cleanup child cancellation failed")
	}
	waitForSubagentTerminal(t, manager, ownerKey, subagentParentSession{Info: session, ChatID: chatID, TabID: tabID}, run.ID, 5*time.Second)
	deadline = time.Now().Add(5 * time.Second)
	for {
		if receipts := manager.ListSubagentReceipts(ownerKey, chatID, tabID, 10); len(receipts) == 1 && receipts[0].SubagentID == run.ID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cleanup child receipt was not durably committed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	manager.CancelJob(jobID(parent))
}

func TestNextTurnListsAndWaitsOnAdoptedSubagent(t *testing.T) {
	t.Parallel()
	manager, events, session, ownerKey, root, _ := newSubagentLifecycleFixture(t, "next-turn")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	startSubagentParent(t, manager, session, root, "[mock:slow] first parent completes")
	run, err := manager.SpawnSubagent(ctx, SubagentSpawnOptions{
		OwnerKey: ownerKey, ParentChatID: session.ChatID, ParentTabID: session.TabID,
		Prompt: "[mock:spawned-work] [mock:slow] adopted bounded result", Label: "adopted-wait",
		ProviderID: session.Info.ProviderID, ModelID: "mock-deterministic", Effort: "high", ModeID: "ask", CWD: root,
	})
	if err != nil {
		t.Fatalf("spawn child: %v", err)
	}
	waitForSubagentAdoption(t, manager, run.ID, 6*time.Second)
	next := startSubagentParent(t, manager, session, root, "[mock:active-without-terminal] adopting parent")
	listed := manager.ListSubagents(ownerKey, session.ChatID, session.TabID)
	if len(listed) != 1 || listed[0].ID != run.ID || !listed[0].Adopted || listed[0].ParentJobID != asString(next["id"]) || listed[0].RootJobID != asString(next["id"]) {
		t.Fatalf("next-turn adopted list = %#v", listed)
	}
	finished, err := manager.WaitSubagent(ctx, ownerKey, session.ChatID, session.TabID, run.ID, 8*time.Second)
	if err != nil || finished.Status != "done" || len(finished.Result) == 0 || len(finished.Result) > 12000 {
		t.Fatalf("next-turn adopted wait = %#v err=%v", finished, err)
	}
	nextID := asString(next["id"])
	if !manager.CancelJob(nextID) {
		t.Fatal("cancel adopting parent failed")
	}
	assertJobStatus(t, events.waitJobEnd(t, nextID, 2*time.Second), "failed", 130, "cancelled")

	// Addressability after adoption uses the same owner/session boundary. Run it
	// after the completed wait instead of creating another provider fixture.
	startSubagentParent(t, manager, session, root, "[mock:slow] parent completes before message")
	messageRun, err := manager.SpawnSubagent(ctx, SubagentSpawnOptions{
		OwnerKey: ownerKey, ParentChatID: session.ChatID, ParentTabID: session.TabID,
		Prompt: "[mock:slow] [mock:steer] [mock:active-without-terminal] adopted message target", Label: "adopted-message",
		ProviderID: session.Info.ProviderID, ModelID: "mock-deterministic", Effort: "high", ModeID: "ask", CWD: root,
	})
	if err != nil {
		t.Fatalf("spawn adopted message target: %v", err)
	}
	waitForSubagentAdoption(t, manager, messageRun.ID, 6*time.Second)
	delivery, err := manager.MessageSubagent(ownerKey, session.ChatID, session.TabID, messageRun.ID, "continue with the adopted correction")
	if err != nil || delivery["ok"] != true {
		t.Fatalf("turnless message adopted child = %#v err=%v", delivery, err)
	}
	controller := startSubagentParent(t, manager, session, root, "[mock:active-without-terminal] adopting controller")
	if !manager.CancelSubagent(ownerKey, session.ChatID, session.TabID, messageRun.ID) {
		t.Fatal("cancel adopted child failed")
	}
	cancelledRun := waitForSubagentTerminal(t, manager, ownerKey, session, messageRun.ID, 5*time.Second)
	if cancelledRun.Status != "cancelled" || cancelledRun.ParentJobID != asString(controller["id"]) {
		t.Fatalf("cancelled adopted child = %#v", cancelledRun)
	}
	manager.CancelJob(asString(controller["id"]))
}

func TestSubagentLatchedPermissionAttentionSurfacesToAdoptingTurn(t *testing.T) {
	t.Parallel()
	manager, events, session, ownerKey, root, _ := newSubagentLifecycleFixture(t, "permission-adoption")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	parent := startSubagentParent(t, manager, session, root, "[mock:slow] parent completes around permission")
	run, err := manager.SpawnSubagent(ctx, SubagentSpawnOptions{
		OwnerKey: ownerKey, ParentChatID: session.ChatID, ParentTabID: session.TabID,
		Prompt: "[mock:permission] [mock:slow] adopted permission", Label: "adopted-permission",
		ProviderID: session.Info.ProviderID, ModelID: "mock-deterministic", Effort: "high", ModeID: "ask", CWD: root,
	})
	if err != nil {
		t.Fatalf("spawn child: %v", err)
	}
	permission := events.waitFor(t, 5*time.Second, func(ev collectedEvent) bool {
		payload, _ := ev.payload.(map[string]any)
		return ev.channel == "chat:permission-request" && asString(payload["jobId"]) == asString(parent["id"])
	}).payload.(map[string]any)
	waitForSubagentAdoption(t, manager, run.ID, 6*time.Second)
	next := startSubagentParent(t, manager, session, root, "[mock:active-without-terminal] permission adopter")
	attention, err := manager.WaitSubagent(ctx, ownerKey, session.ChatID, session.TabID, run.ID, 3*time.Second)
	if err != nil || !attention.Adopted || !attention.NeedsAttention || attention.Attention == nil || attention.Attention.Kind != "permission" || attention.ParentJobID != asString(next["id"]) {
		t.Fatalf("adopting wait attention = %#v err=%v", attention, err)
	}
	pending := false
	for _, raw := range manager.PendingPermissions() {
		if asString(mapFromAny(raw)["id"]) == asString(permission["id"]) {
			pending = true
		}
	}
	if !pending {
		t.Fatal("adopted child permission stopped flowing to the controller")
	}
	manager.PermissionDecide(asString(permission["id"]), "allow-once")
	finished := waitForSubagentTerminal(t, manager, ownerKey, session, run.ID, 6*time.Second)
	if finished.Status != "done" {
		t.Fatalf("permission child did not settle: %#v", finished)
	}
	manager.CancelJob(asString(next["id"]))
}

func TestPruneSubagentsNeverEvictsRunningAdoptedRun(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	manager.mu.Lock()
	manager.subagents["running-adopted"] = &SubagentRun{ID: "running-adopted", Status: "running", Adopted: true, AdoptedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	for index := 0; index < maxCompletedSubagentHistory; index++ {
		id := fmt.Sprintf("settled-%03d", index)
		manager.subagents[id] = &SubagentRun{ID: id, Status: "done", FinishedAt: fmt.Sprintf("2026-01-01T00:00:%02dZ", index%60)}
	}
	manager.pruneSubagentsLocked()
	_, present := manager.subagents["running-adopted"]
	manager.mu.Unlock()
	if !present {
		t.Fatal("pruning evicted a running adopted subagent")
	}
}

func TestSubagentTurnlessOwnerListsWaitsAndSpawnsBornAdoptedWithOptionalVisibleHint(t *testing.T) {
	t.Parallel()
	manager, events, session, ownerKey, root, _ := newSubagentLifecycleFixture(t, "turnless")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	withHint, err := manager.SpawnSubagent(ctx, SubagentSpawnOptions{
		OwnerKey: ownerKey, ParentChatID: session.ChatID, ParentTabID: session.TabID, RootJobIDHint: "visible-assistant-job",
		Prompt: "born adopted with anchor", Label: "turnless-hinted",
		ProviderID: session.Info.ProviderID, ModelID: "mock-deterministic", Effort: "high", ModeID: "ask", CWD: root,
	})
	if err != nil || !withHint.Adopted || withHint.ParentJobID != "" || withHint.RootJobID != "visible-assistant-job" || withHint.AdoptedAt == "" {
		t.Fatalf("turnless hinted spawn = %#v err=%v", withHint, err)
	}
	listed := manager.ListSubagents(ownerKey, session.ChatID, session.TabID)
	if len(listed) != 1 || listed[0].ID != withHint.ID || !listed[0].Adopted {
		t.Fatalf("turnless list = %#v", listed)
	}
	wake, err := manager.WaitSubagents(ctx, ownerKey, session.ChatID, session.TabID, []string{withHint.ID}, "all", 8*time.Second)
	if err != nil || wake["timedOut"] != false || len(wake["completed"].([]SubagentRun)) != 1 {
		t.Fatalf("turnless wait-many = %#v err=%v", wake, err)
	}
	hintedDone, err := manager.WaitSubagent(ctx, ownerKey, session.ChatID, session.TabID, withHint.ID, time.Second)
	if err != nil || hintedDone.Status != "done" {
		t.Fatalf("turnless hinted wait = %#v err=%v", hintedDone, err)
	}
	if headers := subagentHeadersFor(events, "visible-assistant-job", withHint.ID); len(headers) < 2 {
		t.Fatalf("hinted spawn did not route lifecycle headers: %#v", headers)
	}
	retried, err := manager.RetrySubagent(ctx, ownerKey, session.ChatID, session.TabID, withHint.ID, "turnless retry")
	if err != nil || !retried.Adopted || retried.ParentJobID != "" || retried.RetryOf != withHint.ID {
		t.Fatalf("turnless retry = %#v err=%v", retried, err)
	}
	retriedDone, err := manager.WaitSubagent(ctx, ownerKey, session.ChatID, session.TabID, retried.ID, 8*time.Second)
	if err != nil || retriedDone.Status != "done" {
		t.Fatalf("turnless retry wait = %#v err=%v", retriedDone, err)
	}

	withoutHint, err := manager.SpawnSubagent(ctx, SubagentSpawnOptions{
		OwnerKey: ownerKey, ParentChatID: session.ChatID, ParentTabID: session.TabID,
		Prompt: "born adopted without anchor", Label: "turnless-unanchored",
		ProviderID: session.Info.ProviderID, ModelID: "mock-deterministic", Effort: "high", ModeID: "ask", CWD: root,
	})
	if err != nil || !withoutHint.Adopted || withoutHint.ParentJobID != "" || withoutHint.RootJobID != "" {
		t.Fatalf("turnless unanchored spawn = %#v err=%v", withoutHint, err)
	}
	unanchoredDone, err := manager.WaitSubagent(ctx, ownerKey, session.ChatID, session.TabID, withoutHint.ID, 8*time.Second)
	if err != nil || unanchoredDone.Status != "done" {
		t.Fatalf("turnless unanchored wait = %#v err=%v", unanchoredDone, err)
	}
	for _, event := range events.snapshot() {
		if event.channel != "job:event" {
			continue
		}
		payload, _ := event.payload.(map[string]any)
		tool := mapFromAny(payload["event"])
		if asString(tool["subagentId"]) == withoutHint.ID {
			t.Fatalf("unanchored spawn emitted visible header/tool event: %#v", payload)
		}
	}
	receipts := manager.ListSubagentReceipts(ownerKey, session.ChatID, session.TabID, 10)
	if len(receipts) != 3 || receipts[1].RetryOf != withHint.ID || receipts[2].SubagentID != withoutHint.ID {
		t.Fatalf("turnless spawn receipts = %#v", receipts)
	}
}

func TestExplicitChatDeletionStillCancelsAdoptedSubagents(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	run := &SubagentRun{
		ID: "delete-adopted", Status: "running", Adopted: true, ParentJobID: "old-parent", RootJobID: "old-root",
		parentChatID: "delete-chat", parentTabID: "delete-tab", cancel: cancel, done: done,
	}
	go func() {
		<-ctx.Done()
		manager.mu.Lock()
		run.Status = "cancelled"
		manager.mu.Unlock()
		close(done)
	}()
	manager.mu.Lock()
	manager.subagents[run.ID] = run
	manager.mu.Unlock()
	manager.ForgetChat(context.Background(), "delete-tab", "delete-chat", "delete-chat-test-operation")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("explicit chat deletion did not cancel adopted subagent")
	}
	manager.mu.Lock()
	status := run.Status
	manager.mu.Unlock()
	if status != "cancelled" {
		t.Fatalf("deleted chat subagent status = %q", status)
	}
}

type subagentParentSession struct {
	Info   SessionInfo
	ChatID string
	TabID  string
}

func newSubagentLifecycleFixture(t *testing.T, name string) (*Manager, *eventCollector, subagentParentSession, string, string, string) {
	t.Helper()
	root := repoRoot(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir: root, StateDir: stateDir,
		Provider:  ProviderConfig{Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Label: "Workass Mock ACP"},
		Broadcast: events.Broadcast, PermissionTimeout: 8 * time.Second,
		StdoutFlushInterval: 10 * time.Millisecond, ThoughtFlushInterval: 10 * time.Millisecond,
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	chatID, tabID := "chat-"+name, "tab-"+name
	info, err := manager.NewSession(ctx, SessionOptions{ChatID: chatID, TabID: tabID, CWD: root})
	if err != nil {
		t.Fatalf("new parent session: %v", err)
	}
	manager.mu.Lock()
	ownerKey := manager.agentOwnerBySession[info.SessionID]
	manager.mu.Unlock()
	if ownerKey == "" {
		t.Fatal("parent session has no owner key")
	}
	return manager, events, subagentParentSession{Info: info, ChatID: chatID, TabID: tabID}, ownerKey, root, stateDir
}

func startSubagentParent(t *testing.T, manager *Manager, session subagentParentSession, cwd, prompt string) map[string]any {
	t.Helper()
	job, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: session.Info.SessionID, ChatID: session.ChatID, TabID: session.TabID,
		ProviderID: session.Info.ProviderID, ModelID: "mock-deterministic", ModeID: "ask", CWD: cwd, Prompt: prompt,
	})
	if err != nil {
		t.Fatalf("start parent: %v", err)
	}
	return job
}

func waitForSubagentAdoption(t *testing.T, manager *Manager, id string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		manager.mu.Lock()
		run := manager.subagents[id]
		adopted := run != nil && run.Adopted && run.AdoptedAt != ""
		manager.mu.Unlock()
		if adopted {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("subagent %s was not adopted within %s", id, timeout)
}

func waitForSubagentTerminal(t *testing.T, manager *Manager, ownerKey string, session subagentParentSession, id string, timeout time.Duration) SubagentRun {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		run, err := manager.WaitSubagent(context.Background(), ownerKey, session.ChatID, session.TabID, id, time.Until(deadline))
		if err != nil {
			t.Fatalf("wait subagent %s: %v", id, err)
		}
		if run.Status != "running" && !run.NeedsAttention {
			return run
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("subagent %s did not settle within %s", id, timeout)
	return SubagentRun{}
}

func subagentHeadersFor(events *eventCollector, jobID, subagentID string) []map[string]any {
	out := []map[string]any{}
	for _, payload := range events.jobEvents(jobID, "acp") {
		tool := mapFromAny(payload["event"])
		if tool["subagentHeader"] == true && asString(tool["subagentId"]) == subagentID {
			out = append(out, tool)
		}
	}
	return out
}
