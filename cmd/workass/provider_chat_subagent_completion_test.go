package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
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
