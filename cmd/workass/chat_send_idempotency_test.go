package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

func TestAgentChatSendQueuedOwnershipAndImmutableRetries(t *testing.T) {
	runtime, createChat := newAgentChatSendActorFixture(t)
	cases := []struct {
		name, operationID, originalMessage, originalDelivery, retryMessage, retryDelivery string
		assertSafeReceipt                                                                 bool
	}{
		{
			name: "changed message", operationID: "agent-send-message-once",
			originalMessage: "original message", originalDelivery: "queue",
			retryMessage: "changed message", retryDelivery: "queue", assertSafeReceipt: true,
		},
		{
			name: "changed delivery", operationID: "agent-send-delivery-once",
			originalMessage: "same message", originalDelivery: "queue",
			retryMessage: "same message", retryDelivery: "auto",
		},
	}
	for index, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			tabID := fmt.Sprintf("agent-send-tab-%d", index)
			chatID := fmt.Sprintf("agent-send-chat-%d", index)
			createChat(t, tabID, chatID)
			first, err := runtime.QueueAgentMessage(
				context.Background(), tabID, chatID, providercontract.OperationID(test.operationID),
				test.originalMessage, test.originalDelivery,
			)
			if err != nil {
				t.Fatalf("initial chat.send: %v", err)
			}
			if test.assertSafeReceipt {
				if queueID := fieldString(first, "queueId"); queueID == "" || strings.Contains(queueID, test.originalMessage) {
					t.Fatalf("chat.send receipt exposed unsafe queue identity: %#v", first)
				}
				if strings.Contains(strings.ToLower(stringifyAny(first)), test.originalMessage) {
					t.Fatalf("chat.send receipt leaked message content: %#v", first)
				}
			}
			if _, err := runtime.QueueAgentMessage(
				context.Background(), tabID, chatID, providercontract.OperationID(test.operationID),
				test.retryMessage, test.retryDelivery,
			); err == nil || !strings.Contains(err.Error(), "different content") {
				t.Fatalf("changed retry reused chat.send operation: %v", err)
			}
			state, ok := runtime.Snapshot(chatID)
			if !ok {
				t.Fatal("chat actor disappeared after conflicting retry")
			}
			if countAgentSendOperations(state, test.operationID) != 1 {
				t.Fatalf("changed retry created an additional durable operation: %#v", state.Operations)
			}
		})
	}

	t.Run("idle steer owns one FIFO row", func(t *testing.T) {
		const tabID, chatID, operationID = "agent-send-idle-tab", "agent-send-idle-chat", "agent-send-idle-steer"
		createChat(t, tabID, chatID)
		first, err := runtime.QueueAgentMessage(context.Background(), tabID, chatID, operationID, "start this once", "steer")
		if err != nil {
			t.Fatalf("idle steer admission: %v", err)
		}
		queueID := fieldString(first, "queueId")
		if queueID == "" || first["queued"] != true || fieldString(first, "strategy") != "queue" {
			t.Fatalf("idle steer did not return its durable queue receipt: %#v", first)
		}
		state, ok := runtime.Snapshot(chatID)
		if !ok || countAgentSendOperations(state, operationID) != 1 {
			t.Fatalf("idle steer has no exact actor owner: %#v", state)
		}
		durable, found := durableSteerInputForOperation(state, providercontract.OperationID(queueID))
		if !found || durable.Text != "start this once" || durable.Presentation.QueueID != queueID || durable.Presentation.Origin != "agent" {
			t.Fatalf("idle steer changed its immutable FIFO input: found=%v input=%#v", found, durable)
		}

		retry, err := runtime.QueueAgentMessage(context.Background(), tabID, chatID, operationID, "start this once", "steer")
		if err != nil {
			t.Fatalf("idle steer retry: %v", err)
		}
		if fieldString(retry, "queueId") != queueID {
			t.Fatalf("idle steer retry changed queue identity: first=%#v retry=%#v", first, retry)
		}
		state, _ = runtime.Snapshot(chatID)
		if countAgentSendOperations(state, operationID) != 1 {
			t.Fatalf("idle steer retry duplicated actor ownership: %#v", state.Operations)
		}
	})
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
	stopSteerRegressionTurn(t, runtime)
}

func newAgentChatSendActorFixture(t *testing.T) (*providerChatRuntime, func(*testing.T, string, string)) {
	t.Helper()
	root := repoRoot(t)
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "test",
		Provider: acp.ProviderConfig{
			ID: "mock", Name: "Mock", Command: "node",
			Args: []string{filepath.Join(root, "desktop", "acp", "mock-server.mjs")}, CWD: root, Enabled: true,
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	runtime := newProviderChatRuntime(manager, sharedSessionStore(stateDir), stateDir)
	if err := runtime.StartupError(); err != nil {
		manager.Reset()
		t.Fatalf("start actor-only chat.send runtime: %v", err)
	}
	var releaseAdmissions []func()
	t.Cleanup(func() {
		_ = runtime.Close(context.Background())
		for _, release := range releaseAdmissions {
			release()
		}
		manager.Reset()
	})
	createChat := func(t *testing.T, tabID, chatID string) {
		t.Helper()
		if _, err := runtime.CreateRendererChat(map[string]any{
			"tabId": tabID, "chatId": chatID, "operationId": "create-" + chatID,
			"title": "Agent send", "cwd": root, "providerId": "mock", "currentModelId": "mock-deterministic",
		}); err != nil {
			t.Fatalf("create actor-only chat.send actor: %v", err)
		}
		actor, err := runtime.actor(chatID)
		if err != nil {
			t.Fatal(err)
		}
		identity := providercontract.LaneIdentity{
			ChatID: chatID,
			Realm: providercontract.Realm{
				ProviderID: "mock", MachineID: "test-machine", AccountScope: "default", InstallScope: "test",
			},
			WorkspaceEpoch: acp.WorkspaceEpochForRevision(chatID, root, 0),
		}.Normalize()
		if err := actor.engine.Apply(chat.SelectLane{
			Identity: identity, Owner: providercontract.AttachmentOwner{TabID: tabID}, CWD: root,
			ModelID: "mock-deterministic", Creation: providercontract.CreationCapabilities{DeferredUntilInput: true},
		}); err != nil {
			t.Fatalf("select actor-only chat.send lane: %v", err)
		}
		// QueueAgentMessage normally wakes provider execution after its durable
		// receipt. These cases test only immutable actor ownership, so hold the
		// generic claim boundary and close the runtime before releasing it.
		releaseAdmissions = append(releaseAdmissions, actor.coordinator.BeginReplyAdmission())
	}
	return runtime, createChat
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
