package main

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

func seedAgentProviderTurn(t *testing.T, engine *chat.Engine, operationID providercontract.OperationID, nativeTurnID string) chat.ProviderActivityOwner {
	t.Helper()
	state := engine.Snapshot()
	if state.ActiveLaneID == "" {
		t.Fatal("agent turn fixture has no active lane")
	}
	if err := engine.Apply(chat.Submit{OperationID: operationID, LaneID: state.ActiveLaneID, Text: "legitimate provider turn"}); err != nil {
		t.Fatalf("seed provider turn: submit: %v", err)
	}
	if err := engine.Apply(chat.TurnAdmitted{
		OperationID: operationID, Accepted: true,
		Turn: providercontract.TurnRef{OperationID: operationID, NativeID: nativeTurnID},
	}); err != nil {
		t.Fatalf("seed provider turn: admission: %v", err)
	}
	state = engine.Snapshot()
	if state.Foreground == nil {
		t.Fatal("seed provider turn did not leave a foreground turn")
	}
	return chat.ProviderActivityOwner{
		LaneID: state.Foreground.LaneID, OperationID: state.Foreground.OperationID,
		TurnID:               state.Foreground.Turn.NativeID,
		ConnectionGeneration: state.Lanes[state.Foreground.LaneID].ConnectionGeneration,
	}
}

func TestProviderChatAgentReadRejectsWrongPairBeforeManagerCapability(t *testing.T) {
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{StateDir: stateDir})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": "agent-read-tab", "chatId": "agent-read-chat", "operationId": "agent-read-create",
		"title": "Actor read gate", "cwd": t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.ListSpawnedWorkForOwner("missing-owner", "agent-read-chat", "stale-tab", "agent-read-chat", "agent-read-tab", 0); err == nil ||
		!strings.Contains(err.Error(), "tab id") {
		t.Fatalf("wrong parent pair was not rejected by actor before manager: %v", err)
	}
	if _, err := runtime.ListSpawnedWorkForOwner("missing-owner", "agent-read-chat", "agent-read-tab", "other-chat", "agent-read-tab", 0); err == nil ||
		!strings.Contains(err.Error(), "exactly match") {
		t.Fatalf("wrong requested pair was not rejected before manager owner lookup: %v", err)
	}
	if _, err := runtime.AgentOwnerCWD("missing-owner", "stale-tab", "agent-read-chat"); err == nil ||
		!strings.Contains(err.Error(), "tab id") {
		t.Fatalf("artifact CWD gate accepted stale pair: %v", err)
	}
}

func TestProviderChatAgentReadProjectsActorBackgroundState(t *testing.T) {
	root := repoRoot(t)
	workspace := t.TempDir()
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join(root, "desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	const tabID, chatID, ownerKey = "agent-read-tab", "agent-read-chat", "agent-read-owner"
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "agent-read-create",
		"title": "Actor read state", "cwd": workspace, "providerId": "mock",
		"currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := runtime.Select(ctx, acp.SessionOptions{
		TabID: tabID, ChatID: chatID, ProviderID: "mock", CWD: workspace, AgentOwnerKey: ownerKey,
	}); err != nil {
		t.Fatal(err)
	}
	actor, err := runtime.actor(chatID)
	if err != nil {
		t.Fatal(err)
	}
	owner := seedAgentProviderTurn(t, actor.engine, "agent-read-turn", "agent-read-native-turn")
	finished := "2026-08-11T12:00:01Z"
	if err := actor.engine.Apply(chat.ReconcileBackgroundSnapshot{Items: []chat.BackgroundState{{
		Owner: owner,
		Event: providercontract.BackgroundEvent{
			WorkID: "child-1", TaskID: "child-1", Kind: "subagent", Title: "Review child",
			Status: "exited", StartedAt: "2026-08-11T12:00:00Z", UpdatedAt: finished, FinishedAt: finished,
			ModelLabel: "Mock-high", ResultExcerpt: "actor result",
		},
	}}}); err != nil {
		t.Fatal(err)
	}
	runs, err := runtime.ListSubagents(ownerKey, tabID, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != "child-1" || runs[0].Status != "done" || runs[0].Result != "actor result" {
		t.Fatalf("actor subagent projection = %#v", runs)
	}
	receipts, err := runtime.ListSubagentReceipts(ownerKey, tabID, chatID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 || receipts[0].SubagentID != "child-1" || receipts[0].Status != "done" {
		t.Fatalf("actor subagent receipts = %#v", receipts)
	}
	items, err := runtime.ListSpawnedWorkForOwner(ownerKey, chatID, tabID, chatID, tabID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || fieldString(items[0], "id") != "child-1" || fieldString(items[0], "status") != "exited" {
		t.Fatalf("actor spawned work projection = %#v", items)
	}
	workReceipts, err := runtime.ListSpawnedWorkReceipts(ownerKey, chatID, tabID, chatID, tabID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(workReceipts) != 1 || workReceipts[0].TaskID != "child-1" || workReceipts[0].Status != "exited" {
		t.Fatalf("actor spawned work receipts = %#v", workReceipts)
	}
	if cwd, err := runtime.AgentOwnerCWD(ownerKey, tabID, chatID); err != nil || cwd != workspace {
		t.Fatalf("actor workspace = %q, %v", cwd, err)
	}
}

func newAgentWaitReceiptFixture(t *testing.T) (*providerChatRuntime, *acp.Manager, string, string, string, string) {
	t.Helper()
	root := repoRoot(t)
	stateDir := t.TempDir()
	const tabID, chatID, ownerKey, workID = "agent-wait-tab", "agent-wait-chat", "agent-wait-owner", "agent-wait-child"
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "test",
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join(root, "desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "agent-wait-create",
		"title": "Agent wait receipt", "cwd": root, "providerId": "mock",
		"currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := runtime.Select(ctx, acp.SessionOptions{
		TabID: tabID, ChatID: chatID, ProviderID: "mock", CWD: root, AgentOwnerKey: ownerKey,
	}); err != nil {
		t.Fatal(err)
	}
	actor, err := runtime.actor(chatID)
	if err != nil {
		t.Fatal(err)
	}
	owner := seedAgentProviderTurn(t, actor.engine, "agent-wait-owner-turn", "agent-wait-native-turn")
	finished := "2026-08-12T12:00:01Z"
	if err := actor.engine.Apply(chat.ReconcileBackgroundSnapshot{Items: []chat.BackgroundState{{
		Owner: owner,
		Event: providercontract.BackgroundEvent{
			WorkID: workID, TaskID: workID, Kind: "subagent", Title: "Wait child",
			Status: "exited", StartedAt: "2026-08-12T12:00:00Z", UpdatedAt: finished, FinishedAt: finished,
			ResultExcerpt: "completed actor row",
		},
	}}}); err != nil {
		t.Fatal(err)
	}
	return runtime, manager, tabID, chatID, ownerKey, workID
}

func TestProviderChatAgentWaitUsesDurableObservationReceipt(t *testing.T) {
	runtime, _, tabID, chatID, ownerKey, workID := newAgentWaitReceiptFixture(t)
	ctx := context.Background()
	const operationID providercontract.OperationID = "agent-wait-observation-once"
	before, ok := runtime.Snapshot(chatID)
	if !ok {
		t.Fatal("agent wait actor disappeared")
	}
	first, err := runtime.WaitSubagent(ctx, ownerKey, tabID, chatID, operationID, workID, time.Second)
	if err != nil || first.Status != "done" {
		t.Fatalf("terminal actor wait = %#v err=%v", first, err)
	}
	after, _ := runtime.Snapshot(chatID)
	receipt, exists := after.AgentWaitObservationReceipts[operationID]
	if !exists || receipt.Digest != agentWaitObservationDigest("single", tabID, chatID, []string{workID}, "single", time.Second) {
		t.Fatalf("wait observation receipt = %#v", after.AgentWaitObservationReceipts)
	}
	if after.Revision <= before.Revision {
		t.Fatalf("wait receipt did not commit an actor revision: before=%d after=%d", before.Revision, after.Revision)
	}

	retried, err := runtime.WaitSubagent(ctx, ownerKey, tabID, chatID, operationID, workID, time.Second)
	if err != nil || retried.Status != "done" {
		t.Fatalf("same terminal actor wait retry = %#v err=%v", retried, err)
	}
	readback, _ := runtime.Snapshot(chatID)
	if readback.Revision != after.Revision {
		t.Fatalf("same wait retry mutated actor revision: before=%d after=%d", after.Revision, readback.Revision)
	}

	if _, err := runtime.WaitSubagent(ctx, ownerKey, tabID, chatID, operationID, workID, 2*time.Second); err == nil || !strings.Contains(err.Error(), "different request") {
		t.Fatalf("changed timeout reused wait operation: %v", err)
	}
}

func TestProviderChatAgentWaitChangedIntentWinsWhenTargetIsMissing(t *testing.T) {
	runtime, _, tabID, chatID, _, workID := newAgentWaitReceiptFixture(t)
	actor, err := runtime.actor(chatID)
	if err != nil {
		t.Fatal(err)
	}
	const operationID providercontract.OperationID = "agent-wait-disappearing-target"
	originalDigest := agentWaitObservationDigest("single", tabID, chatID, []string{workID}, "single", time.Second)
	if _, err := runtime.recordAgentWaitObservation(actor, tabID, chatID, operationID, originalDigest, []string{workID}); err != nil {
		t.Fatalf("record original wait observation: %v", err)
	}

	missingTarget := "agent-wait-disappeared-child"
	changedDigest := agentWaitObservationDigest("single", tabID, chatID, []string{missingTarget}, "single", 2*time.Second)
	if _, err := runtime.recordAgentWaitObservation(actor, tabID, chatID, operationID, changedDigest, []string{missingTarget}); err == nil || !strings.Contains(err.Error(), "different request") {
		t.Fatalf("changed wait intent with a missing target was accepted or misclassified: %v", err)
	}
}

func TestProviderChatAgentWaitManyUsesTerminalActorRows(t *testing.T) {
	runtime, _, tabID, chatID, ownerKey, workID := newAgentWaitReceiptFixture(t)
	const operationID providercontract.OperationID = "agent-wait-many-terminal"
	result, err := runtime.WaitSubagents(context.Background(), ownerKey, tabID, chatID, operationID, []string{workID, workID}, "all", time.Second)
	if err != nil {
		t.Fatalf("terminal actor wait_many = %#v err=%v", result, err)
	}
	completed, ok := result["completed"].([]acp.SubagentRun)
	if !ok || len(completed) != 1 || completed[0].ID != workID {
		t.Fatalf("terminal actor wait_many completed = %#v", result["completed"])
	}
	state, _ := runtime.Snapshot(chatID)
	receipt, exists := state.AgentWaitObservationReceipts[operationID]
	if !exists || receipt.Digest != agentWaitObservationDigest("many", tabID, chatID, []string{workID}, "all", time.Second) {
		t.Fatalf("wait_many observation receipt = %#v", state.AgentWaitObservationReceipts)
	}
	revision := state.Revision
	if _, err := runtime.WaitSubagents(context.Background(), ownerKey, tabID, chatID, operationID, []string{workID}, "all", time.Second); err != nil {
		t.Fatalf("same terminal actor wait_many retry: %v", err)
	}
	if retried, _ := runtime.Snapshot(chatID); retried.Revision != revision {
		t.Fatalf("same wait_many retry mutated actor revision: before=%d after=%d", revision, retried.Revision)
	}
	if _, err := runtime.WaitSubagents(context.Background(), ownerKey, tabID, chatID, operationID, []string{workID}, "first", time.Second); err == nil || !strings.Contains(err.Error(), "different request") {
		t.Fatalf("changed return_when reused wait_many operation: %v", err)
	}
}

func TestProviderChatAgentWaitObservationRaceReservesOneReceipt(t *testing.T) {
	runtime, _, tabID, chatID, ownerKey, workID := newAgentWaitReceiptFixture(t)
	const operationID providercontract.OperationID = "agent-wait-observation-race"
	before, _ := runtime.Snapshot(chatID)
	const callers = 16
	errs := make(chan error, callers)
	var group sync.WaitGroup
	group.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer group.Done()
			_, err := runtime.WaitSubagent(context.Background(), ownerKey, tabID, chatID, operationID, workID, time.Second)
			errs <- err
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent exact wait failed: %v", err)
		}
	}
	after, _ := runtime.Snapshot(chatID)
	if len(after.AgentWaitObservationReceipts) != 1 || after.Revision <= before.Revision {
		t.Fatalf("concurrent wait receipts/revision = %d/%d, want 1 and a committed revision after %d", len(after.AgentWaitObservationReceipts), after.Revision, before.Revision)
	}
}

func TestProviderChatAgentWaitFencesStalePairBeforeOwnerManager(t *testing.T) {
	runtime, _, _, chatID, ownerKey, workID := newAgentWaitReceiptFixture(t)
	// The exact actor fence runs before the transient owner capability check;
	// a stale tab therefore fails without needing a live Manager owner row.
	if _, err := runtime.WaitSubagent(context.Background(), ownerKey, "stale-tab", chatID, "agent-wait-stale", workID, time.Second); err == nil || !strings.Contains(err.Error(), "tab id") {
		t.Fatalf("stale wait pair was accepted: %v", err)
	}
}
