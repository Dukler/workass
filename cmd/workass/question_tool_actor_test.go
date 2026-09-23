package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	"workass/internal/httpserve"
	"workass/internal/lease"
	providercontract "workass/internal/provider"
	"workass/internal/wire"
)

func TestWorkassQuestionActorWireAnswerUnicodeIsolationAndReplay(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><body></body>"), 0o644); err != nil {
		t.Fatalf("write renderer entry: %v", err)
	}
	leaseManager, err := lease.NewManager(lease.Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("create lease manager: %v", err)
	}
	controllerDevice, controllerToken, err := leaseManager.ApproveDevice("question-controller", "127.0.0.1")
	if err != nil {
		t.Fatalf("approve controller: %v", err)
	}
	viewerDevice, viewerToken, err := leaseManager.ApproveDevice("question-viewer", "127.0.0.1")
	if err != nil {
		t.Fatalf("approve viewer: %v", err)
	}
	hub := wire.NewHub(wire.Options{Lease: leaseManager, TrustLocalhost: false})
	sessionState := sharedSessionStore(stateDir)
	var requestEventsMu sync.Mutex
	requestEvents := 0
	broadcast := daemonEventBroadcaster(sessionState, hub.Broadcast)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "test",
		Provider: acp.ProviderConfig{
			ID: "mock", Name: "Mock", Command: "node",
			Args: []string{filepath.Join(root, "desktop", "acp", "mock-server.mjs")}, CWD: root, Enabled: true,
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
		Broadcast: func(channel string, payload any) {
			if channel == "chat:permission-request" {
				requestEventsMu.Lock()
				requestEvents++
				requestEventsMu.Unlock()
			}
			broadcast(channel, payload)
		},
	})
	runtime := newProviderChatRuntime(manager, sessionState, stateDir)
	t.Cleanup(func() {
		_ = runtime.Close(context.Background())
		manager.Reset()
	})
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: runtime})
	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	controller := dialTestWSPath(t, server.URL, "/?deviceToken="+controllerToken+"&deviceName=question-controller")
	defer controller.conn.Close()
	controllerState := mapFromAnyMain(controller.waitChannelEvent(t, "lan:access-state", 2*time.Second).Payload)
	if controllerState["controller"] != true {
		t.Fatalf("question UI connection does not own the controller lease: %#v", controllerState)
	}
	viewer := dialTestWSPath(t, server.URL, "/?deviceToken="+viewerToken+"&deviceName=question-viewer")
	defer viewer.conn.Close()
	viewerState := mapFromAnyMain(viewer.waitChannelEvent(t, "lan:access-state", 2*time.Second).Payload)
	if viewerState["controller"] == true {
		t.Fatalf("second approved device unexpectedly owns the controller lease: %#v", viewerState)
	}

	const tabID, chatID, otherTab, otherChat, ownerKey = "question-tab", "question-chat", "other-tab", "other-chat", "question-owner"
	createWireActorChat(t, controller, 101, tabID, chatID, "mock")
	createWireActorChat(t, controller, 102, otherTab, otherChat, "mock")
	selection, err := runtime.Select(context.Background(), acp.SessionOptions{
		TabID: tabID, ChatID: chatID, ProviderID: "mock", CWD: root,
		OperationID: "question-lane-selection", AgentOwnerKey: ownerKey,
	})
	if err != nil || strings.TrimSpace(selection.SessionID) == "" {
		t.Fatalf("attach actor-owned mock lane: session=%#v err=%v", selection, err)
	}
	if !manager.ValidateAgentOwner(ownerKey, chatID, tabID) {
		state, _ := runtime.Snapshot(chatID)
		lane := state.Lanes[state.ActiveLaneID]
		t.Fatalf("selected lane did not retain its exact agent owner capability: session=%q laneOwner=%q activeLane=%q", selection.SessionID, lane.Owner.AgentOwnerKey, state.ActiveLaneID)
	}
	// Seed the exact owner-bound lane selection receipt that an established
	// agent-owned lane carries into its next foreground turn. The frozen
	// renderer job:start request intentionally does not carry ACP credentials.
	actor, err := runtime.actor(chatID)
	if err != nil {
		t.Fatalf("open selected actor: %v", err)
	}
	actor.mu.Lock()
	selectedState := actor.engine.Snapshot()
	selectedLane := selectedState.Lanes[selectedState.ActiveLaneID]
	startArg := map[string]any{
		"kind": "app-chat", "tabId": tabID, "chatId": chatID, "sessionId": selection.SessionID,
		"providerId": "mock", "prompt": "[mock:hold-until-steer] stay available for the question tool",
		"operationId": "question-wire-turn", "userMessageId": "question-wire-user", "assistantMessageId": "question-wire-assistant",
	}
	turnSelectionOptions := effectiveSelectionOptions(selectedState, parseSessionOptions(startArg))
	turnSelectionDigest, err := selectionRequestDigest(turnSelectionOptions)
	if err == nil {
		err = actor.engine.Apply(chat.CommitLaneSelection{
			OperationID: "question-wire-turn", Digest: turnSelectionDigest,
			Identity: selectedLane.Identity, Thread: selectedLane.Thread, Owner: selectedLane.Owner,
			CWD: selectedLane.CWD, ModelID: selectedLane.ModelID, ModeID: selectedLane.ModeID,
			Context: selectedLane.Context, Delivery: selectedLane.Delivery, Creation: selectedLane.Creation,
			Established: !selectedLane.Thread.IsZero(),
			Update: chat.UpdateRuntimeControls{
				ProviderID: selectedLane.Identity.Realm.ProviderID, ModelID: selectedLane.ModelID, ModeID: selectedLane.ModeID,
				ReplaceModelID: true, ReplaceModeID: true,
				ExpectedRevision: selectedState.Presentation.RuntimeControlRevision, RequireRevision: true,
			},
		})
	}
	actor.mu.Unlock()
	if err != nil {
		t.Fatalf("commit exact agent-owned lane for renderer turn: %v", err)
	}
	stateAfterOwnerSeed := actor.engine.Snapshot()
	checkOptions := effectiveSelectionOptions(stateAfterOwnerSeed, parseSessionOptions(startArg))
	checkDigest, err := selectionRequestDigest(checkOptions)
	if err != nil || checkDigest != turnSelectionDigest {
		t.Fatalf("test fixture lane selection digest changed: before=%q after=%q err=%v options=%#v", turnSelectionDigest, checkDigest, err, checkOptions)
	}
	controller.invoke(t, 103, "job:start", startArg)
	startReply := controller.waitReply(t, 103, 5*time.Second)
	if startReply.Error != nil {
		t.Fatalf("start actor-backed foreground turn: %s", *startReply.Error)
	}
	job := mapFromAnyMain(startReply.Result)
	jobID := fieldString(job, "id")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if manager.ValidateWorkassQuestionCaller(ownerKey, chatID, tabID) == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := manager.ValidateWorkassQuestionCaller(ownerKey, chatID, tabID); err != nil {
		running, runningOK := manager.RunningJobForChat(tabID, chatID)
		state, _ := runtime.Snapshot(chatID)
		lane := state.Lanes[state.ActiveLaneID]
		t.Fatalf("mock foreground did not become the exact question owner: ownerBound=%t selectionSession=%q running=%#v runningOK=%t laneOwner=%q foreground=%#v job=%#v reason=%v",
			manager.ValidateAgentOwner(ownerKey, chatID, tabID), selection.SessionID, running, runningOK, lane.Owner.AgentOwnerKey, state.Foreground, job, err)
	}

	questionArgs := map[string]any{
		"operation_id": "ask-deploy-target-once", "question_id": "deploy-target",
		"header": "Destino", "question": "¿Qué destino preparo?",
		"options": []any{
			map[string]any{"id": "canary", "label": "Canary", "description": "Prueba con menor riesgo"},
			map[string]any{"id": "production", "label": "Producción"},
		},
		"multi_select": true, "allow_free_text": true,
	}
	call := browserMCPCallParams{Name: "workass_ask_user_question", Arguments: questionArgs}
	control := &agentControlHandler{manager: manager, chats: newChatControlCoordinator(manager, nil, runtime)}
	wrongChatValue, wrongChatErr := callAgentMCPTool(httptest.NewRequest(http.MethodPost, toolsPath, nil), call,
		agentMCPOptions{ChatID: otherChat, TabID: otherTab, OwnerKey: ownerKey}, control)
	if wrongChatErr != nil || mapFromAnyMain(wrongChatValue)["isError"] != true {
		t.Fatalf("agent owner was allowed to retarget a question to another chat: result=%#v err=%v", wrongChatValue, wrongChatErr)
	}

	callerCtx, disconnectCaller := context.WithCancel(context.Background())
	defer disconnectCaller()
	firstDone := make(chan struct {
		value any
		err   error
	}, 1)
	go func() {
		request := httptest.NewRequest(http.MethodPost, toolsPath, nil).WithContext(callerCtx)
		value, callErr := callAgentMCPTool(request, call, agentMCPOptions{ChatID: chatID, TabID: tabID, OwnerKey: ownerKey}, control)
		firstDone <- struct {
			value any
			err   error
		}{value, callErr}
	}()
	var request map[string]any
	for {
		event := controller.waitChannelEvent(t, "chat:permission-request", 8*time.Second)
		request = mapFromAnyMain(event.Payload)
		if fieldString(request, "chatId") == chatID {
			break
		}
	}
	if fieldString(request, "tabId") != tabID || fieldString(request, "jobId") != jobID || fieldString(request, "sessionId") != selection.SessionID {
		t.Fatalf("question was routed away from its exact actor/turn: %#v", request)
	}
	question := mapFromAnyMain(request["question"])
	if question["workassTool"] != true || fieldString(question, "questionId") != "deploy-target" ||
		fieldString(question, "operationId") != "ask-deploy-target-once" || question["allowFreeText"] != true {
		t.Fatalf("provider-neutral CLI schema did not reach the existing question card: %#v", question)
	}
	choices := anySlice(question["options"])
	if len(choices) != 2 || fieldString(mapFromAnyMain(choices[0]), "id") != "canary" {
		t.Fatalf("question card lost stable option ids: %#v", choices)
	}

	freeText := strings.Repeat("界🙂", 500) // 1000 Unicode code points, more than 1000 UTF-8 bytes.
	answerToken, err := providercontract.EncodeWorkassQuestionAnswer(providercontract.QuestionAnswer{
		Status: "answered", SelectedOptionIDs: []string{"production", "canary"}, FreeText: freeText,
	})
	if err != nil {
		t.Fatalf("encode long Unicode answer: %v", err)
	}
	// Under the frozen wire's existing mutation rule, an approved viewer taking
	// an answer action first acquires the controller lease. A malformed answer
	// must leave the card pending; the current controller then retries it.
	viewer.invoke(t, 201, "chat:permission-decide", map[string]any{"id": request["id"], "optionId": "not-a-structured-answer"})
	viewerReply := viewer.waitReply(t, 201, 2*time.Second)
	malformedState, _ := runtime.Snapshot(chatID)
	malformedPermission := malformedState.Permissions[fieldString(request, "id")]
	if viewerReply.Error == nil || malformedPermission.Event.Status != "pending" ||
		malformedPermission.Event.Question == nil || malformedPermission.Event.Question.Answer != nil {
		t.Fatalf("malformed structured answer should be rejected without resolving the card: reply=%+v permission=%#v", viewerReply, malformedPermission)
	}
	if !leaseManager.IsController(viewerDevice.ID) || leaseManager.IsController(controllerDevice.ID) {
		t.Fatal("question answer route did not honor the wire's exact controller lease transition")
	}

	secondStarted := make(chan struct{})
	secondDone := make(chan struct {
		value any
		err   error
	}, 1)
	go func() {
		close(secondStarted)
		value, callErr := callAgentMCPTool(httptest.NewRequest(http.MethodPost, toolsPath, nil), call,
			agentMCPOptions{ChatID: chatID, TabID: tabID, OwnerKey: ownerKey}, control)
		secondDone <- struct {
			value any
			err   error
		}{value, callErr}
	}()
	<-secondStarted
	select {
	case <-secondDone:
		t.Fatal("same-operation question replay completed before its original answer")
	case <-time.After(20 * time.Millisecond):
	}
	viewer.invoke(t, 202, "chat:permission-decide", map[string]any{"id": request["id"], "optionId": answerToken})
	decisionReply := viewer.waitReply(t, 202, 5*time.Second)
	if decisionReply.Error != nil || mapFromAnyMain(decisionReply.Result)["ok"] != true {
		t.Fatalf("current controller question reply route rejected the structured answer: %+v", decisionReply)
	}
	first := <-firstDone
	second := <-secondDone
	if first.err != nil || second.err != nil {
		t.Fatalf("CLI question calls failed: first=%v second=%v", first.err, second.err)
	}
	firstResult := decodeQuestionToolResult(t, first.value)
	secondResult := decodeQuestionToolResult(t, second.value)
	if !reflect.DeepEqual(firstResult, secondResult) || firstResult["free_text"] != freeText || firstResult["status"] != "answered" {
		t.Fatalf("Unicode answer or stable operation replay changed: first=%#v second=%#v", firstResult, secondResult)
	}
	state, ok := runtime.Snapshot(chatID)
	if !ok {
		t.Fatal("question owning actor disappeared")
	}
	permission := state.Permissions[fieldString(request, "id")]
	if permission.Event.Question == nil || permission.Event.Question.Answer == nil || permission.Event.Question.Answer.FreeText != freeText {
		t.Fatalf("actor did not durably retain the exact user answer: %#v", permission.Event.Question)
	}
	var answerOperationID string
	for _, entry := range state.Outbox {
		if entry.Kind == chat.EffectPermission && entry.RequestID == fieldString(request, "id") {
			answerOperationID = string(entry.OperationID)
		}
	}
	if !strings.HasPrefix(answerOperationID, "permission-answer-v1:") || len(answerOperationID) > 256 || strings.Contains(answerOperationID, freeText) || strings.Contains(answerOperationID, answerToken) {
		t.Fatalf("structured answer leaked into or overflowed its durable operation identity: %q", answerOperationID)
	}
	entry, found := externalBrowserMutationEntry(state, providercontract.OperationID("ask-deploy-target-once"))
	if !found || entry.Status != chat.OutboxCompleted || len(entry.Result) == 0 {
		t.Fatalf("question result was not durably receipted: %#v", entry)
	}
	var durableResult map[string]any
	if err := json.Unmarshal(entry.Result, &durableResult); err != nil || durableResult["free_text"] != freeText {
		t.Fatalf("durable Unicode question receipt did not round trip: result=%#v err=%v", durableResult, err)
	}
	requestEventsMu.Lock()
	gotRequestEvents := requestEvents
	requestEventsMu.Unlock()
	if gotRequestEvents != 1 {
		t.Fatalf("same operation opened %d question cards, want exactly one", gotRequestEvents)
	}
	if pending := manager.PendingPermissions(); len(pending) != 0 {
		t.Fatalf("resolved Workass question remained pending after reconnect/replay: %#v", pending)
	}
}
