package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

type workassQuestionObservedEvent struct {
	channel string
	payload any
}

func TestWorkassQuestionManagerBridgeUnit(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	events := make(chan workassQuestionObservedEvent, 128)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "test",
		Provider: acp.ProviderConfig{
			ID: "mock", Name: "Mock", Command: "node",
			Args: []string{filepath.Join(root, "desktop", "acp", "mock-server.mjs")}, CWD: root, Enabled: true,
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
		Broadcast: func(channel string, payload any) {
			events <- workassQuestionObservedEvent{channel: channel, payload: payload}
		},
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	const tabID, chatID, ownerKey = "question-tab", "question-chat", "question-owner"
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "create-question-chat",
		"title": "Question owner", "cwd": root, "providerId": "mock", "currentModelId": "mock-deterministic",
	}); err != nil {
		t.Fatalf("create owning actor: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, acp.SessionOptions{
		TabID: tabID, ChatID: chatID, ProviderID: "mock", CWD: root, AgentOwnerKey: ownerKey,
	})
	if err != nil {
		t.Fatalf("attach mock ACP owner: %v", err)
	}
	job, err := manager.StartJob(ctx, acp.JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, TabID: tabID, ChatID: chatID,
		ProviderID: "mock", Prompt: "[mock:hold-until-steer]",
	})
	if err != nil {
		t.Fatalf("start mock ACP fixture turn: %v", err)
	}
	jobID := fieldString(job, "id")
	waitForQuestionCaller(t, manager, ownerKey, chatID, tabID)

	arguments := map[string]any{
		"operation_id": "ask-deploy-target-once", "question_id": "deploy-target",
		"header": "Deploy target", "question": "Which target should I prepare?",
		"options": []any{
			map[string]any{"id": "canary", "label": "Canary", "description": "Lower risk"},
			map[string]any{"id": "production", "label": "Production"},
		},
		"multi_select": true, "allow_free_text": true,
	}
	call := browserMCPCallParams{Name: "workass_ask_user_question", Arguments: arguments}
	control := &agentControlHandler{manager: manager, chats: newChatControlCoordinator(manager, nil, runtime)}
	callCtx, disconnectCaller := context.WithCancel(context.Background())
	defer disconnectCaller()
	callDone := make(chan struct {
		value any
		err   error
	}, 1)
	go func() {
		request := httptest.NewRequest(http.MethodPost, toolsPath, nil).WithContext(callCtx)
		value, callErr := callAgentMCPTool(request, call, agentMCPOptions{
			ChatID: chatID, TabID: tabID, OwnerKey: ownerKey,
		}, control)
		callDone <- struct {
			value any
			err   error
		}{value: value, err: callErr}
	}()
	requestEvent := waitObservedQuestionEvent(t, events, chatID)
	if fieldString(requestEvent, "tabId") != tabID || fieldString(requestEvent, "sessionId") != session.SessionID {
		t.Fatalf("question card escaped exact tab/session ownership: %#v", requestEvent)
	}
	cardQuestion := mapFromAnyMain(requestEvent["question"])
	if cardQuestion["workassTool"] != true || fieldString(cardQuestion, "questionId") != "deploy-target" ||
		fieldString(cardQuestion, "operationId") != "ask-deploy-target-once" || cardQuestion["allowFreeText"] != true {
		t.Fatalf("CLI request did not reach the existing question UI contract: %#v", cardQuestion)
	}
	choices := anySlice(cardQuestion["options"])
	if len(choices) != 2 || fieldString(mapFromAnyMain(choices[0]), "id") != "canary" {
		t.Fatalf("question UI payload lost stable choice ids: %#v", choices)
	}
	question, err := parseWorkassQuestion(map[string]any{
		"operation_id": "ask-deploy-target-once", "question_id": "deploy-target",
		"question": "Which target should I prepare?", "header": "Deploy target",
		"options": arguments["options"], "multi_select": true, "allow_free_text": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if question.Digest == "" || question.OperationID != "ask-deploy-target-once" {
		t.Fatalf("CLI question request lacks its canonical mutation identity: %#v", question)
	}
	answerToken, err := providercontract.EncodeWorkassQuestionAnswer(providercontract.QuestionAnswer{
		Status: "answered", SelectedOptionIDs: []string{"production", "canary"}, FreeText: "retain exact spacing ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !manager.PermissionDecide(fieldString(requestEvent, "id"), answerToken) {
		t.Fatal("question UI reply did not reach the owning manager resolver")
	}
	firstCall := <-callDone
	if firstCall.err != nil {
		t.Fatal(firstCall.err)
	}
	first := decodeQuestionToolResult(t, firstCall.value)
	selected := anySlice(first["selected_options"])
	if first["status"] != "answered" || len(selected) != 2 || fieldString(mapFromAnyMain(selected[0]), "id") != "production" ||
		fieldString(mapFromAnyMain(selected[1]), "id") != "canary" || first["free_text"] != "retain exact spacing " {
		t.Fatalf("CLI answer result lost selected options or free text: %#v", first)
	}
	state, ok := runtime.Snapshot(chatID)
	if !ok {
		t.Fatal("question owner actor disappeared")
	}
	entry, found := externalBrowserMutationEntry(state, providercontract.OperationID("ask-deploy-target-once"))
	if !found || entry.Status != chat.OutboxCompleted || len(entry.Result) == 0 {
		t.Fatalf("question result was not durably receipted: %#v", entry)
	}

	retryValue, retryErr := callAgentMCPTool(httptest.NewRequest(http.MethodPost, toolsPath, nil), call,
		agentMCPOptions{ChatID: chatID, TabID: tabID, OwnerKey: ownerKey}, control)
	if retryErr != nil {
		t.Fatal(retryErr)
	}
	retry := decodeQuestionToolResult(t, retryValue)
	if !reflect.DeepEqual(retry, first) {
		t.Fatalf("stable operation replay changed its answer: first=%#v retry=%#v", first, retry)
	}
	for {
		select {
		case event := <-events:
			if event.channel == "chat:permission-request" && fieldString(mapFromAnyMain(event.payload), "chatId") == chatID {
				t.Fatalf("same operation replay opened a duplicate question: %#v", event.payload)
			}
		default:
			goto noDuplicate
		}
	}
noDuplicate:
	// An explicit caller cancellation settles the same typed question lifecycle
	// and commits a durable cancelled receipt for retries to read.
	cancelledArgs := copyAnyMap(arguments)
	cancelledArgs["operation_id"] = "ask-deploy-target-cancelled"
	cancelledArgs["question_id"] = "deploy-target-cancelled"
	cancelledCall := browserMCPCallParams{Name: call.Name, Arguments: cancelledArgs}
	cancelCtx, cancelQuestionCall := context.WithCancel(context.Background())
	cancelledDone := make(chan struct {
		value any
		err   error
	}, 1)
	go func() {
		request := httptest.NewRequest(http.MethodPost, toolsPath, nil).WithContext(cancelCtx)
		value, callErr := callAgentMCPTool(request, cancelledCall, agentMCPOptions{ChatID: chatID, TabID: tabID, OwnerKey: ownerKey}, control)
		cancelledDone <- struct {
			value any
			err   error
		}{value, callErr}
	}()
	cancelRequest := waitObservedQuestionEvent(t, events, chatID)
	if fieldString(mapFromAnyMain(cancelRequest["question"]), "questionId") != "deploy-target-cancelled" {
		t.Fatalf("cancel test observed another card: %#v", cancelRequest)
	}
	cancelQuestionCall()
	cancelledCallResult := <-cancelledDone
	if cancelledCallResult.err != nil {
		t.Fatalf("cancelled Workass CLI call returned transport error: %v", cancelledCallResult.err)
	}
	cancelledReceipt := decodeQuestionToolResult(t, cancelledCallResult.value)
	if cancelledReceipt["status"] != "cancelled" || cancelledReceipt["reason"] != "caller_cancelled" {
		t.Fatalf("caller cancellation was not explicit in the tool result: %#v", cancelledReceipt)
	}
	replayedCancelled, err := callAgentMCPTool(httptest.NewRequest(http.MethodPost, toolsPath, nil), cancelledCall,
		agentMCPOptions{ChatID: chatID, TabID: tabID, OwnerKey: ownerKey}, control)
	if err != nil || !reflect.DeepEqual(decodeQuestionToolResult(t, replayedCancelled), cancelledReceipt) {
		t.Fatalf("cancelled operation did not replay its durable result: value=%#v err=%v", replayedCancelled, err)
	}
	for {
		select {
		case event := <-events:
			if event.channel == "chat:permission-request" && fieldString(mapFromAnyMain(event.payload), "chatId") == chatID {
				t.Fatalf("cancelled operation replay opened a duplicate question: %#v", event.payload)
			}
		default:
			goto noCancelledDuplicate
		}
	}
noCancelledDuplicate:
	changed := browserMCPCallParams{Name: call.Name, Arguments: copyAnyMap(arguments)}
	changed.Arguments["question"] = "A different question"
	conflictValue, conflictErr := callAgentMCPTool(httptest.NewRequest(http.MethodPost, toolsPath, nil), changed,
		agentMCPOptions{ChatID: chatID, TabID: tabID, OwnerKey: ownerKey}, control)
	if conflictErr != nil || mapFromAnyMain(conflictValue)["isError"] != true {
		t.Fatalf("changed request reused the question operation: result=%#v err=%v", conflictValue, conflictErr)
	}

	manager.CancelJob(jobID)
	waitForMockJobEnd(t, events, jobID)
}

func TestWorkassQuestionSchemaMutationClassificationAndMalformedInputs(t *testing.T) {
	var questionTool map[string]any
	for _, tool := range agentMCPTools() {
		if tool["name"] == "workass_ask_user_question" {
			questionTool = tool
		}
	}
	if questionTool == nil || !workassToolMutates(agentToolKind, "workass_ask_user_question") {
		t.Fatal("question tool is not discoverable and canonically classified as a mutation")
	}
	for _, tool := range agentToolsForChild() {
		if tool["name"] == "workass_ask_user_question" {
			t.Fatal("child catalog exposed a question bound to a different foreground owner")
		}
	}
	if _, err := requiredToolOperationID(agentToolKind, browserMCPCallParams{
		Name: "workass_ask_user_question", Arguments: map[string]any{"question_id": "x"},
	}); err == nil || !strings.Contains(err.Error(), "operation_id") {
		t.Fatalf("question mutation accepted no stable operation id: %v", err)
	}
	base := map[string]any{
		"operation_id": "question-parse-test", "question_id": "selection", "question": "Choose",
		"options": []any{map[string]any{"id": "a", "label": "A"}},
	}
	if _, err := parseWorkassQuestion(base); err != nil {
		t.Fatalf("valid question rejected: %v", err)
	}
	for name, mutation := range map[string]func(map[string]any){
		"unknown field": func(args map[string]any) { args["surprise"] = true },
		"duplicate option": func(args map[string]any) {
			args["options"] = []any{map[string]any{"id": "a", "label": "A"}, map[string]any{"id": "a", "label": "Again"}}
		},
		"partial option": func(args map[string]any) { args["options"] = []any{map[string]any{"id": "a"}} },
		"bad timeout":    func(args map[string]any) { args["timeout_ms"] = 999 },
		"oversized secret shaped prompt": func(args map[string]any) {
			args["question"] = strings.Repeat("Bearer secretvalue ", 30)
		},
	} {
		t.Run(name, func(t *testing.T) {
			args := copyAnyMap(base)
			mutation(args)
			if _, err := parseWorkassQuestion(args); err == nil {
				t.Fatal("malformed question arguments were accepted")
			}
		})
	}
}

func waitForQuestionCaller(t *testing.T, manager *acp.Manager, ownerKey, chatID, tabID string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if err := manager.ValidateWorkassQuestionCaller(ownerKey, chatID, tabID); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("mock ACP turn never became the exact foreground question owner")
}

func waitObservedQuestionEvent(t *testing.T, events <-chan workassQuestionObservedEvent, chatID string) map[string]any {
	t.Helper()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			payload := mapFromAnyMain(event.payload)
			if event.channel == "chat:permission-request" && fieldString(payload, "chatId") == chatID {
				return payload
			}
		case <-timer.C:
			t.Fatal("Workass CLI question never reached the existing question UI event")
		}
	}
}

func waitForMockJobEnd(t *testing.T, events <-chan workassQuestionObservedEvent, jobID string) {
	t.Helper()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			payload := mapFromAnyMain(event.payload)
			if event.channel == "job:event" && payload["type"] == "end" && fieldString(mapFromAnyMain(payload["job"]), "id") == jobID {
				return
			}
		case <-timer.C:
			t.Fatal("mock ACP question owner job did not end after cancellation")
		}
	}
}

func decodeQuestionToolResult(t *testing.T, value any) map[string]any {
	t.Helper()
	result := mapFromAnyMain(value)
	if result["isError"] == true {
		t.Fatalf("Workass question CLI returned error content: %#v", result)
	}
	content := anySlice(result["content"])
	if len(content) != 1 {
		t.Fatalf("Workass question CLI content = %#v", content)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(fieldString(mapFromAnyMain(content[0]), "text")), &decoded); err != nil {
		t.Fatalf("decode Workass question result: %v", err)
	}
	return decoded
}
