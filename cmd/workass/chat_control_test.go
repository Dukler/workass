package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
)

func TestT4ModelControlKeysMigrateToBaseAndCompositeCreateValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	snapshot := sessionMirrorFixture("polluted-tab", "polluted-chat", "polluted controls")
	legacyChat := chatFromSnapshot(snapshot, "polluted-tab")
	legacyChat["modelControls"] = map[string]any{"mock": map[string]any{
		"mock-deterministic":       map[string]any{"effort": "low", "modeId": "ask"},
		"mock-deterministic[high]": map[string]any{"effort": "high", "modeId": "bypass"},
		"literal[1m]":              map[string]any{"modeId": "literal"},
	}}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal polluted snapshot: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write polluted snapshot: %v", err)
	}
	var logLines []string
	prevLog := sessionStoreLogLine
	sessionStoreLogLine = func(line string) { logLines = append(logLines, line) }
	t.Cleanup(func() { sessionStoreLogLine = prevLog })

	reloaded := newSessionStore(path)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload polluted controls: %v", err)
	}
	gotChat := chatFromSnapshot(reloaded.Get().(map[string]any), "polluted-tab")
	memory := mapFromAnyMain(mapFromAnyMain(gotChat["modelControls"])["mock"])
	if _, exists := memory["mock-deterministic[high]"]; exists {
		t.Fatalf("composite modelControls key survived migration: %#v", memory)
	}
	base := mapFromAnyMain(memory["mock-deterministic"])
	if fieldString(base, "effort") != "low" || fieldString(base, "modeId") != "ask" {
		t.Fatalf("base controls did not win conflict: %#v", base)
	}
	if _, exists := memory["literal[1m]"]; !exists {
		t.Fatalf("literal bracketed adapter id was migrated away: %#v", memory)
	}
	if len(logLines) != 1 || !strings.Contains(logLines[0], "from=mock-deterministic[high]") || !strings.Contains(logLines[0], "to=mock-deterministic") {
		t.Fatalf("migration logs = %#v", logLines)
	}

	root := repoRoot(t)
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir,
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock",
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, store, manager.StateDir())
	const parentTabID, parentChatID = "composite-parent-tab", "composite-parent-chat"
	if _, err := runtime.CreateRendererChat(map[string]any{
		"operationId": "test:create-composite-parent",
		"tabId":       parentTabID, "chatId": parentChatID,
		"title": "Parent", "cwd": root, "providerId": "mock",
		"currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatalf("create actor-native parent chat: %v", err)
	}
	coordinator := newChatControlCoordinator(manager, func(string, any) {}, runtime)
	createParams := map[string]any{
		"operation_id": "test:create-composite-child",
		"title":        "Composite child", "cwd": stateDir, "provider_id": "mock", "model_id": "mock-deterministic[high]",
	}
	created, err := coordinator.create(context.Background(), parentTabID, parentChatID, createParams)
	if err != nil {
		t.Fatalf("create chat with composite model: %v", err)
	}
	if created["modelId"] != "mock-deterministic" || created["effort"] != "high" || created["resolvedModelId"] != "mock-deterministic[high]" {
		t.Fatalf("created composite controls = %#v", created)
	}
	childState, ok := runtime.Snapshot(fieldString(created, "chatId"))
	if !ok {
		t.Fatal("created child actor is missing")
	}
	if _, err := manager.ToggleProvider(context.Background(), "mock", false); err != nil {
		t.Fatalf("disable provider before lost-reply retry: %v", err)
	}
	retried, err := coordinator.create(context.Background(), parentTabID, parentChatID, createParams)
	if err != nil {
		t.Fatalf("lost-reply create retry consulted disabled provider: %v", err)
	}
	if fieldString(retried, "tabId") != fieldString(created, "tabId") || fieldString(retried, "chatId") != fieldString(created, "chatId") || retried["resolvedModelId"] != created["resolvedModelId"] {
		t.Fatalf("create retry receipt changed: first=%#v retry=%#v", created, retried)
	}
	retriedState, _ := runtime.Snapshot(fieldString(created, "chatId"))
	if retriedState.Revision != childState.Revision {
		t.Fatalf("create retry changed child actor revision: first=%d retry=%d", childState.Revision, retriedState.Revision)
	}
	changed := cloneJSON(createParams).(map[string]any)
	changed["focus"] = true
	if _, err := coordinator.create(context.Background(), parentTabID, parentChatID, changed); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("changed focus reused headless creation operation: %v", err)
	}
}

func TestChatControlMutationPreflightRejectsBeforeManagerSideEffects(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	marker := filepath.Join(stateDir, "catalog-probe.marker")
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "instrumented", Command: "/bin/sh",
			Args: []string{"-c", fmt.Sprintf("printf catalog >> %q; exit 1", marker)},
			CWD:  root, Enabled: true, Label: "Instrumented catalog probe",
		},
		DefaultProviderID: "instrumented", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	const tabID, chatID = "control-preflight-tab", "control-preflight-chat"
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "control-preflight-create",
		"title": "Mutation preflight", "cwd": root, "providerId": "instrumented",
		"currentModelId": "instrumented-model", "currentModeId": "ask",
	}); err != nil {
		t.Fatalf("create actor chat: %v", err)
	}
	coordinator := newChatControlCoordinator(manager, nil, runtime)
	providersBefore := manager.ProvidersList()
	stateBefore, ok := runtime.Snapshot(chatID)
	if !ok {
		t.Fatal("preflight actor is missing")
	}

	configure := func(params map[string]any) error {
		_, err := coordinator.configure(context.Background(), params)
		return err
	}
	mutations := []struct {
		name string
		call func() error
	}{
		{
			name: "configure missing operation",
			call: func() error {
				return configure(map[string]any{
					"tab_id": tabID, "chat_id": chatID, "cwd": "inherit",
					"provider_id": "instrumented", "model_id": "instrumented-model",
				})
			},
		},
		{
			name: "configure stale actor pair",
			call: func() error {
				return configure(map[string]any{
					"operation_id": "control-preflight-stale-pair", "tab_id": "stale-tab", "chat_id": chatID,
					"cwd": "inherit", "provider_id": "instrumented", "model_id": "instrumented-model",
				})
			},
		},
		{
			name: "delete missing operation",
			call: func() error {
				_, err := coordinator.delete(map[string]any{"tab_id": tabID, "chat_id": chatID, "force": true})
				return err
			},
		},
		{
			name: "send missing operation",
			call: func() error {
				_, err := coordinator.send(map[string]any{"tab_id": tabID, "chat_id": chatID, "message": "must not send"})
				return err
			},
		},
		{
			name: "cancel missing operation",
			call: func() error {
				_, err := coordinator.cancel(map[string]any{"tab_id": tabID, "chat_id": chatID})
				return err
			},
		},
		{
			name: "rename missing operation",
			call: func() error {
				_, err := coordinator.rename(map[string]any{"tab_id": tabID, "chat_id": chatID, "title": "must not rename"})
				return err
			},
		},
		{
			name: "focus missing operation",
			call: func() error {
				_, err := coordinator.focus(map[string]any{"tab_id": tabID, "chat_id": chatID})
				return err
			},
		},
		{
			name: "create missing operation",
			call: func() error {
				_, err := coordinator.create(context.Background(), tabID, chatID, map[string]any{"title": "must not create"})
				return err
			},
		},
	}

	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			err := mutation.call()
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "stable operation") && !strings.Contains(strings.ToLower(err.Error()), "tab id") {
				t.Fatalf("invalid mutation error = %v", err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("manager catalog/provider side effect occurred: marker stat=%v", err)
			}
			if got := manager.ProvidersList(); !reflect.DeepEqual(got, providersBefore) {
				t.Fatalf("manager provider catalog changed after fail-first validation: before=%#v after=%#v", providersBefore, got)
			}
			stateAfter, ok := runtime.Snapshot(chatID)
			if !ok || stateAfter.Revision != stateBefore.Revision || stateAfter.Deleted {
				t.Fatalf("invalid mutation changed actor state: before=%#v after=%#v", stateBefore, stateAfter)
			}
		})
	}
}

func TestChatControlInvalidOperationCannotCancelOrDeleteRunningTurn(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join(root, "desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
			Env: map[string]string{"WORKASS_MOCK_ACP_DELAY_MS": "1000"},
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	const tabID, chatID = "control-running-tab", "control-running-chat"
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "control-running-create",
		"title": "Running mutation preflight", "cwd": root, "providerId": "mock",
		"currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatalf("create running actor chat: %v", err)
	}
	info, err := runtime.Select(context.Background(), acp.SessionOptions{
		TabID: tabID, ChatID: chatID, ProviderID: "mock", CWD: root,
	})
	if err != nil {
		t.Fatalf("attach running provider lane: %v", err)
	}
	if _, err := runtime.Start(context.Background(), map[string]any{
		"kind": "app-chat", "tabId": tabID, "chatId": chatID, "sessionId": info.SessionID,
		"operationId": "control-running-turn", "userMessageId": "control-running-user",
		"assistantMessageId": "control-running-assistant", "prompt": "[mock:slow] remain active",
	}, "agent"); err != nil {
		t.Fatalf("start running provider turn: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state, ok := runtime.Snapshot(chatID)
		if ok && state.Foreground != nil && strings.TrimSpace(state.Foreground.Turn.NativeID) != "" &&
			(state.Foreground.Status == chat.ForegroundDispatching || state.Foreground.Status == chat.ForegroundRunning) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	state, ok := runtime.Snapshot(chatID)
	if !ok || state.Foreground == nil {
		t.Fatalf("provider turn did not become foreground: %#v", state)
	}
	jobID := state.Foreground.Turn.NativeID
	coordinator := newChatControlCoordinator(manager, nil, runtime)

	mutations := []struct {
		name string
		call func() error
	}{
		{
			name: "delete missing operation",
			call: func() error {
				_, err := coordinator.delete(map[string]any{"tab_id": tabID, "chat_id": chatID, "force": true})
				return err
			},
		},
		{
			name: "cancel missing operation",
			call: func() error {
				_, err := coordinator.cancel(map[string]any{"tab_id": tabID, "chat_id": chatID})
				return err
			},
		},
		{
			name: "delete stale actor pair",
			call: func() error {
				_, err := coordinator.delete(map[string]any{
					"operation_id": "control-running-stale-delete", "tab_id": "stale-tab", "chat_id": chatID, "force": true,
				})
				return err
			},
		},
		{
			name: "cancel stale actor pair",
			call: func() error {
				_, err := coordinator.cancel(map[string]any{
					"operation_id": "control-running-stale-cancel", "tab_id": "stale-tab", "chat_id": chatID,
				})
				return err
			},
		},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			err := mutation.call()
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "stable operation") && !strings.Contains(strings.ToLower(err.Error()), "tab id") {
				t.Fatalf("invalid running-turn mutation error = %v", err)
			}
			state, ok := runtime.Snapshot(chatID)
			if !ok || state.Foreground == nil || state.Foreground.Turn.NativeID != jobID ||
				(state.Foreground.Status != chat.ForegroundDispatching && state.Foreground.Status != chat.ForegroundRunning) {
				t.Fatalf("invalid mutation touched foreground/provider execution: %#v", state.Foreground)
			}
		})
	}

	const cancelOperationID = "control-cancel-receipt"
	firstCancel, err := coordinator.cancel(map[string]any{
		"operation_id": cancelOperationID, "tab_id": tabID, "chat_id": chatID,
	})
	if err != nil {
		t.Fatalf("actor-backed cancel: %v", err)
	}
	firstCancelled, _ := boolField(firstCancel, "cancelled")
	if !firstCancelled || fieldString(firstCancel, "operationId") != cancelOperationID || fieldString(firstCancel, "jobId") != jobID {
		t.Fatalf("first cancel receipt = %#v", firstCancel)
	}
	waitProviderChatIdle(t, runtime, chatID, 5*time.Second)
	state, ok = runtime.Snapshot(chatID)
	if !ok {
		t.Fatal("actor disappeared after cancellation")
	}
	cancelReceipt, hasCancelReceipt := cancelReceiptForOperation(state, cancelOperationID)
	if !hasCancelReceipt {
		t.Fatalf("actor has no durable cancel receipt: %#v", state.Outbox)
	}
	if _, done := cancelResultFromReceipt(cancelReceipt); !done {
		t.Fatalf("cancel receipt is not terminal after idle boundary: %#v", cancelReceipt)
	}
	revisionBeforeRetry := state.Revision
	retryCancel, err := coordinator.cancel(map[string]any{
		"operation_id": cancelOperationID, "tab_id": tabID, "chat_id": chatID,
	})
	if err != nil {
		t.Fatalf("lost-reply cancel retry: %v", err)
	}
	retryCancelled, _ := boolField(retryCancel, "cancelled")
	if !retryCancelled || fieldString(retryCancel, "operationId") != cancelOperationID || fieldString(retryCancel, "jobId") != jobID {
		t.Fatalf("lost-reply cancel receipt = %#v", retryCancel)
	}
	state, _ = runtime.Snapshot(chatID)
	if state.Revision != revisionBeforeRetry {
		t.Fatalf("lost-reply receipt readback mutated actor revision: before=%d after=%d", revisionBeforeRetry, state.Revision)
	}

	const idleCancelOperationID = "control-cancel-idle-receipt"
	idleCancel, err := coordinator.cancel(map[string]any{
		"operation_id": idleCancelOperationID, "tab_id": tabID, "chat_id": chatID,
	})
	if err != nil || fieldString(idleCancel, "reason") != "idle" || fieldString(idleCancel, "operationId") != idleCancelOperationID {
		t.Fatalf("idle cancel receipt = %#v, err=%v", idleCancel, err)
	}

	if _, err := runtime.Start(context.Background(), map[string]any{
		"kind": "app-chat", "tabId": tabID, "chatId": chatID, "sessionId": info.SessionID,
		"operationId": "control-running-turn-again", "userMessageId": "control-running-user-again",
		"assistantMessageId": "control-running-assistant-again", "prompt": "[mock:slow] remain active again",
	}, "agent"); err != nil {
		t.Fatalf("start second provider turn: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state, ok = runtime.Snapshot(chatID)
		if ok && state.Foreground != nil && strings.TrimSpace(state.Foreground.Turn.NativeID) != "" &&
			(state.Foreground.Status == chat.ForegroundDispatching || state.Foreground.Status == chat.ForegroundRunning) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	state, ok = runtime.Snapshot(chatID)
	if !ok || state.Foreground == nil || strings.TrimSpace(state.Foreground.Turn.NativeID) == "" {
		t.Fatalf("second provider turn did not become foreground: %#v", state)
	}
	secondJobID := state.Foreground.Turn.NativeID
	if secondJobID == jobID {
		t.Fatalf("provider reused native job id across turns: %s", secondJobID)
	}
	revisionBeforeIdleRetry := state.Revision
	idleRetry, err := coordinator.cancel(map[string]any{
		"operation_id": idleCancelOperationID, "tab_id": tabID, "chat_id": chatID,
	})
	if err != nil || fieldString(idleRetry, "reason") != "idle" || fieldString(idleRetry, "operationId") != idleCancelOperationID {
		t.Fatalf("lost-reply idle cancel receipt = %#v, err=%v", idleRetry, err)
	}
	state, ok = runtime.Snapshot(chatID)
	if !ok || state.Revision != revisionBeforeIdleRetry || state.Foreground == nil || state.Foreground.Turn.NativeID != secondJobID {
		t.Fatalf("lost-reply idle receipt touched later foreground: %#v", state)
	}
	revisionBeforeChangedOperation := state.Revision
	if _, err := coordinator.cancel(map[string]any{
		"operation_id": cancelOperationID, "tab_id": tabID, "chat_id": chatID,
	}); err == nil || !strings.Contains(strings.ToLower(err.Error()), "different foreground turn") {
		t.Fatalf("changed-operation cancel error = %v", err)
	}
	state, ok = runtime.Snapshot(chatID)
	if !ok || state.Revision != revisionBeforeChangedOperation || state.Foreground == nil || state.Foreground.Turn.NativeID != secondJobID {
		t.Fatalf("changed-operation cancel touched current turn: %#v", state)
	}
	secondCancel, err := coordinator.cancel(map[string]any{
		"operation_id": "control-cancel-second", "tab_id": tabID, "chat_id": chatID,
	})
	if err != nil {
		t.Fatalf("cleanup second actor-backed cancel: %v", err)
	}
	secondCancelled, _ := boolField(secondCancel, "cancelled")
	if !secondCancelled {
		t.Fatalf("second cancel receipt = %#v", secondCancel)
	}
	waitProviderChatIdle(t, runtime, chatID, 5*time.Second)
}
