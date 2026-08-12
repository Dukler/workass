package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"workass/internal/chat"
)

func TestAgentChatSendRetryWithChangedMessageConflictsWithoutLeakingReceipt(t *testing.T) {
	runtime, _, _, _, _ := newSteerRegressionFixture(t)
	const tabID, chatID, operationID = "steer-regression-tab", "steer-regression-chat", "agent-send-message-once"
	first, err := runtime.QueueAgentMessage(context.Background(), tabID, chatID, operationID, "original message", "queue")
	if err != nil {
		t.Fatalf("initial chat.send: %v", err)
	}
	if queueID := fieldString(first, "queueId"); queueID == "" || strings.Contains(queueID, "original message") {
		t.Fatalf("chat.send receipt exposed unsafe queue identity: %#v", first)
	}
	if strings.Contains(strings.ToLower(stringifyAny(first)), "original message") {
		t.Fatalf("chat.send receipt leaked message content: %#v", first)
	}
	if _, err := runtime.QueueAgentMessage(context.Background(), tabID, chatID, operationID, "changed message", "queue"); err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("changed message reused chat.send operation: %v", err)
	}
	state, ok := runtime.Snapshot(chatID)
	if !ok {
		t.Fatal("chat actor disappeared after conflicting retry")
	}
	if countAgentSendOperations(state, operationID) != 1 {
		t.Fatalf("changed retry created an additional durable operation: %#v", state.Operations)
	}
}

func TestAgentChatSendRetryWithChangedDeliveryConflicts(t *testing.T) {
	runtime, _, _, _, _ := newSteerRegressionFixture(t)
	const tabID, chatID, operationID = "steer-regression-tab", "steer-regression-chat", "agent-send-delivery-once"
	if _, err := runtime.QueueAgentMessage(context.Background(), tabID, chatID, operationID, "same message", "queue"); err != nil {
		t.Fatalf("initial queued chat.send: %v", err)
	}
	if _, err := runtime.QueueAgentMessage(context.Background(), tabID, chatID, operationID, "same message", "auto"); err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("changed delivery reused chat.send operation: %v", err)
	}
}

func TestAgentChatSendSteerDerivedOperationRejectsChangedMessageAndDelivery(t *testing.T) {
	runtime, _, _, _, info := newSteerRegressionFixture(t)
	startSteerRegressionTurn(t, runtime, info)
	const tabID, chatID, operationID = "steer-regression-tab", "steer-regression-chat", "agent-send-steer-once"
	if _, err := runtime.QueueAgentMessage(context.Background(), tabID, chatID, operationID, "steer original", "steer"); err != nil {
		t.Fatalf("initial steer chat.send: %v", err)
	}
	if _, err := runtime.QueueAgentMessage(context.Background(), tabID, chatID, operationID, "steer changed", "steer"); err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("changed steer message reused chat.send operation: %v", err)
	}
	if _, err := runtime.QueueAgentMessage(context.Background(), tabID, chatID, operationID, "steer original", "queue"); err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("changed steer delivery reused chat.send operation: %v", err)
	}
	state, ok := runtime.Snapshot(chatID)
	if !ok {
		t.Fatal("chat actor disappeared after conflicting steer retries")
	}
	if got := countAgentSendOperations(state, operationID); got != 1 {
		t.Fatalf("steer retry created %d durable operations, want one: %#v", got, state.Operations)
	}
}

func countAgentSendOperations(state chat.State, operationID string) int {
	prefix := "q:" + strings.TrimSpace(operationID) + ":"
	count := 0
	for candidate := range state.Operations {
		if string(candidate) == strings.TrimSpace(operationID) || strings.HasPrefix(string(candidate), prefix) {
			count++
		}
	}
	return count
}

func stringifyAny(value any) string {
	return fmt.Sprintf("%#v", value)
}
