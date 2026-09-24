package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

func TestTrackedSubagentCompletionUsesExactActorAndReceiptIdempotency(t *testing.T) {
	runtime, _, _, _, _ := newSteerRegressionFixture(t)
	const tabID, chatID = "steer-regression-tab", "steer-regression-chat"
	receipt := acp.SubagentReceipt{ReceiptID: "receipt-unique", SubagentID: "child-unique", Label: "review", Status: "done", Result: "finished review"}

	// A stale or cross-tab receipt is acknowledged without opening a lane or
	// putting work into another coordinator.
	if err := runtime.deliverSubagentCompletion("other-tab", chatID, receipt); err != nil {
		t.Fatal(err)
	}
	actor, err := runtime.actor(chatID)
	if err != nil {
		t.Fatal(err)
	}
	if len(actor.engine.Snapshot().Operations) != 0 {
		t.Fatal("completion for a different tab entered the actor")
	}

	before := actor.engine.Snapshot().Presentation
	release := actor.coordinator.BeginReplyAdmission()
	defer release()
	if err := runtime.deliverSubagentCompletion(tabID, chatID, receipt); err != nil {
		t.Fatalf("deliver idle parent completion: %v", err)
	}
	operationID := subagentCompletionOperationID(tabID, chatID, receipt.ReceiptID)
	state := actor.engine.Snapshot()
	if _, ok := state.Operations[operationID]; !ok {
		t.Fatalf("receipt-keyed internal input was not durably admitted: %#v", state.Operations)
	}
	if state.Presentation.ProviderID != before.ProviderID || state.Presentation.CurrentModelID != before.CurrentModelID {
		t.Fatalf("completion changed selected provider/model: before=%#v after=%#v", before, state.Presentation)
	}
	if state.Foreground == nil || state.Foreground.OperationID != operationID || state.Foreground.Input.Presentation.Origin != "agent" {
		t.Fatalf("idle completion was not admitted as an agent-origin turn: %#v", state.Foreground)
	}
	if err := runtime.deliverSubagentCompletion(tabID, chatID, receipt); err != nil {
		t.Fatalf("retry same completion: %v", err)
	}
	state = actor.engine.Snapshot()
	if _, ok := state.Operations[operationID]; !ok {
		t.Fatal("idempotent retry lost the original actor operation")
	}
	if got := formatSubagentCompletion(receipt); !strings.Contains(got, "Internal agent completion notice") || !strings.Contains(got, "not a human message") {
		t.Fatalf("completion did not carry internal attribution: %q", got)
	}
}

func TestTrackedSubagentCompletionRunsOnOwningMockCoordinator(t *testing.T) {
	runtime, manager, _, _, info := newSteerRegressionFixture(t)
	const tabID, chatID = "steer-regression-tab", "steer-regression-chat"
	ended := make(chan struct{}, 4)
	manager.SetJobEndFunc(func(tab, chat string) {
		if tab == tabID && chat == chatID {
			ended <- struct{}{}
		}
	})
	start := func(op, prompt string) {
		t.Helper()
		if _, err := runtime.Start(context.Background(), map[string]any{"kind": "app-chat", "tabId": tabID, "chatId": chatID, "sessionId": info.SessionID, "operationId": op, "userMessageId": op + "-u", "assistantMessageId": op + "-a", "prompt": prompt}, "human"); err != nil {
			t.Fatalf("start %s: %v", op, err)
		}
	}
	start("parent-finished", "finish parent turn")
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("parent did not finish")
	}
	actor, err := runtime.actor(chatID)
	if err != nil {
		t.Fatal(err)
	}
	state := actor.engine.Snapshot()
	if state.Foreground != nil {
		t.Fatalf("parent was not idle before child settlement: %#v", state.Foreground)
	}
	receipt := acp.SubagentReceipt{ReceiptID: "actual-mock-child", SubagentID: "actual-mock-child", Label: "child", Status: "done", Result: "FINAL_HANDOFF_MARKER", ParentChatID: chatID, ParentTabID: tabID, OriginLaneID: string(state.DesiredLaneID), DeliveryPending: true}
	if err := runtime.deliverSubagentCompletion(tabID, chatID, receipt); err != nil {
		t.Fatalf("admit settled unwaited child receipt: %v", err)
	}
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("owning coordinator did not execute subagent follow-up")
	}
	if err := runtime.deliverSubagentCompletion(tabID, chatID, receipt); err != nil {
		t.Fatalf("duplicate settlement delivery: %v", err)
	}
	state = actor.engine.Snapshot()
	markerCount := 0
	for _, event := range state.Ledger {
		if event.Role == "assistant" && strings.Contains(event.Text, "FINAL_HANDOFF_MARKER") {
			markerCount++
		}
	}
	if markerCount != 1 {
		t.Fatalf("mock coordinator did not execute exactly one final handoff: marker count=%d ledger=%#v", markerCount, state.Ledger)
	}
}

func TestTrackedSubagentCompletionDoesNotResurrectDeletedChat(t *testing.T) {
	runtime, _, _, _, _ := newSteerRegressionFixture(t)
	const tabID, chatID = "steer-regression-tab", "steer-regression-chat"
	if err := runtime.DeleteChat(context.Background(), tabID, chatID, "delete-completion-owner", true); err != nil {
		t.Fatalf("delete completion owner: %v", err)
	}
	if err := runtime.deliverSubagentCompletion(tabID, chatID, acp.SubagentReceipt{
		ReceiptID: "receipt-after-delete", SubagentID: "child", Status: "failed", Error: "late failure",
	}); err != nil {
		t.Fatalf("discard completion for deleted chat: %v", err)
	}
	actor, err := runtime.actor(chatID)
	if err != nil {
		t.Fatal(err)
	}
	state := actor.engine.Snapshot()
	if !state.Deleted {
		t.Fatalf("deleted actor was resurrected or received work: deleted=%v operations=%#v", state.Deleted, state.Operations)
	}
	if _, ok := state.Operations[subagentCompletionOperationID(tabID, chatID, "receipt-after-delete")]; ok {
		t.Fatalf("deleted actor received late completion input: %#v", state.Operations)
	}
}

func TestTrackedSubagentCompletionQueuesBehindUnrelatedForegroundTurn(t *testing.T) {
	runtime, _, _, _, info := newSteerRegressionFixture(t)
	const tabID, chatID = "steer-regression-tab", "steer-regression-chat"
	started, err := runtime.Start(context.Background(), map[string]any{
		"kind": "app-chat", "tabId": tabID, "chatId": chatID, "sessionId": info.SessionID,
		"operationId": "unrelated-active-turn", "userMessageId": "unrelated-user", "assistantMessageId": "unrelated-assistant",
		"prompt": "[mock:active-without-terminal] keep this foreground turn open",
	}, "human")
	if err != nil {
		t.Fatal(err)
	}
	state, ok := runtime.Snapshot(chatID)
	if !ok || state.Foreground == nil || state.Foreground.OperationID != "unrelated-active-turn" {
		t.Fatalf("foreground fixture was not admitted: %#v", state)
	}
	receipt := acp.SubagentReceipt{ReceiptID: "while-busy", SubagentID: "child", Label: "late review", Status: "failed", Error: "bounded failure"}
	if err := runtime.deliverSubagentCompletion(tabID, chatID, receipt); err != nil {
		t.Fatalf("queue completion during active turn: %v", err)
	}
	state, _ = runtime.Snapshot(chatID)
	if state.Foreground == nil || state.Foreground.OperationID != "unrelated-active-turn" {
		t.Fatalf("completion steered or replaced the active turn: %#v", state.Foreground)
	}
	completionID := subagentCompletionOperationID(tabID, chatID, receipt.ReceiptID)
	found := false
	for _, queued := range state.Queue {
		if queued.OperationID == completionID && queued.Presentation.Origin == "agent" {
			found = true
		}
	}
	if !found {
		t.Fatalf("completion was not durably queued behind the foreground turn: %#v", state.Queue)
	}
	jobID := strings.TrimSpace(fieldString(started, "jobId"))
	if jobID != "" {
		_, _, _ = runtime.Cancel(context.Background(), jobID)
	}
}

func TestExplicitParentStopDropsOnlyItsQueuedSubagentCompletion(t *testing.T) {
	runtime, _, _, stateDir, info := newSteerRegressionFixture(t)
	const tabID, chatID, parentOperation = "steer-regression-tab", "steer-regression-chat", "stopped-parent-operation"
	started, err := runtime.Start(context.Background(), map[string]any{
		"kind": "app-chat", "tabId": tabID, "chatId": chatID, "sessionId": info.SessionID,
		"operationId": parentOperation, "userMessageId": "parent-user", "assistantMessageId": "parent-assistant",
		"prompt": "[mock:active-without-terminal] keep parent open",
	}, "human")
	if err != nil {
		t.Fatal(err)
	}
	actor, err := runtime.actor(chatID)
	if err != nil {
		t.Fatal(err)
	}
	state := actor.engine.Snapshot()
	if err := actor.engine.Apply(chat.Submit{
		OperationID: "human-waiting", LaneID: state.DesiredLaneID, Text: "human queued work",
		Presentation: providercontract.TurnPresentation{UserMessageID: "human-waiting-user", Origin: "human"},
	}); err != nil {
		t.Fatalf("queue human work: %v", err)
	}
	receipt := acp.SubagentReceipt{
		ReceiptID: "child-receipt-before-stop", SubagentID: "child", Label: "child", Status: "done",
		Result: "must not run after explicit Stop", ParentTabID: tabID, ParentChatID: chatID, OriginOperationID: parentOperation,
	}
	path := filepath.Join(stateDir, "subagent-receipts", tabID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runtime.deliverSubagentCompletion(tabID, chatID, receipt); err != nil {
		t.Fatalf("admit queued child completion: %v", err)
	}
	completionID := subagentCompletionOperationID(tabID, chatID, receipt.ReceiptID)
	state = actor.engine.Snapshot()
	if len(state.Queue) != 2 {
		t.Fatalf("expected human work and child completion queued: %#v", state.Queue)
	}
	jobID := ""
	if state.Foreground != nil {
		jobID = state.Foreground.Turn.NativeID
	}
	if jobID == "" {
		t.Fatalf("fixture parent has no native job identity: %#v (%v)", state.Foreground, started)
	}
	result, handled, err := runtime.Cancel(context.Background(), jobID)
	if err != nil || !handled || !result.Cancelled {
		t.Fatalf("explicit parent Stop = %#v handled=%v err=%v", result, handled, err)
	}
	// Re-deliver the callback after Stop has committed, as happens when its
	// manager callback was already in flight while Stop held actor admission.
	if err := runtime.deliverSubagentCompletion(tabID, chatID, receipt); err != nil {
		t.Fatalf("late callback after committed Stop: %v", err)
	}
	state = actor.engine.Snapshot()
	if state.Foreground != nil && state.Foreground.OperationID == completionID {
		t.Fatalf("stopped parent's completion became the foreground turn: %#v", state.Foreground)
	}
	for _, queued := range state.Queue {
		if queued.OperationID == completionID {
			t.Fatalf("stopped parent's queued completion survived Stop: %#v", state.Queue)
		}
	}
	humanRetained := state.Foreground != nil && state.Foreground.OperationID == "human-waiting"
	for _, queued := range state.Queue {
		humanRetained = humanRetained || queued.OperationID == "human-waiting"
	}
	if !humanRetained {
		t.Fatalf("unrelated human work was removed by Stop: foreground=%#v queue=%#v", state.Foreground, state.Queue)
	}
	// Reopen the durable actor and replay the pending receipt. The same actor
	// fence must survive restart, while a receipt from a different parent stays
	// eligible.
	runtime.mu.Lock()
	delete(runtime.actors, chatID)
	runtime.mu.Unlock()
	restartedActor, err := runtime.actor(chatID)
	if err != nil {
		t.Fatalf("reopen stopped actor: %v", err)
	}
	if err := runtime.deliverSubagentCompletion(tabID, chatID, receipt); err != nil {
		t.Fatalf("replay stopped-parent receipt: %v", err)
	}
	state = restartedActor.engine.Snapshot()
	completionLive := state.Foreground != nil && state.Foreground.OperationID == completionID
	for _, queued := range state.Queue {
		completionLive = completionLive || queued.OperationID == completionID
	}
	if completionLive {
		t.Fatalf("replayed stopped-parent receipt resurrected after actor reload: queue=%#v foreground=%#v", state.Queue, state.Foreground)
	}
	otherParentReceipt := acp.SubagentReceipt{
		ReceiptID: "different-parent-receipt", SubagentID: "other-child", Label: "other child", Status: "done",
		Result: "belongs to an unrelated parent", ParentTabID: tabID, ParentChatID: chatID,
		OriginLaneID: string(state.DesiredLaneID), OriginOperationID: "other-parent-operation",
	}
	if err := runtime.deliverSubagentCompletion(tabID, chatID, otherParentReceipt); err != nil {
		t.Fatalf("admit unrelated parent's completion: %v", err)
	}
	otherCompletionID := subagentCompletionOperationID(tabID, chatID, otherParentReceipt.ReceiptID)
	state = restartedActor.engine.Snapshot()
	if !actorHasSubagentCompletion(state, otherCompletionID, providercontract.LaneID(state.DesiredLaneID), formatSubagentCompletion(otherParentReceipt)) {
		t.Fatalf("Stop suppressed a completion from an unrelated parent: queue=%#v foreground=%#v", state.Queue, state.Foreground)
	}
	humanRetained = false
	for _, queued := range state.Queue {
		humanRetained = humanRetained || queued.OperationID == "human-waiting"
	}
	if !humanRetained {
		t.Fatalf("late callback disturbed unrelated human queue after restart: %#v", state.Queue)
	}
}
