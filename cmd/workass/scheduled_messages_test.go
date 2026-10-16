package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workass/internal/scheduledmessage"
)

type scheduledClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *scheduledClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *scheduledClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type scheduledFixture struct {
	chats     *chatControlCoordinator
	handler   *agentControlHandler
	scheduler *scheduledmessage.Scheduler
	clock     *scheduledClock
	create    func(*testing.T, string, string)
}

func scheduledOwnerKey(chatID string) string { return "sched-owner-" + chatID }

func newScheduledFixture(t *testing.T) scheduledFixture {
	t.Helper()
	runtime, create := newAgentChatSendActorFixture(t)
	clock := &scheduledClock{now: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	chats := newChatControlCoordinator(nil, nil, runtime)
	scheduler, err := chats.attachScheduledMessages(t.TempDir(), scheduledmessage.Options{Now: clock.Now})
	if err != nil {
		t.Fatalf("attach scheduled messages: %v", err)
	}
	handler := &agentControlHandler{chats: chats, validateOwner: func(ownerKey, chatID, tabID string) bool {
		return ownerKey == scheduledOwnerKey(chatID)
	}}
	return scheduledFixture{chats: chats, handler: handler, scheduler: scheduler, clock: clock, create: create}
}

// tool runs one tool call through the same MCP mapping the daemon serves.
func (f scheduledFixture) tool(t *testing.T, tabID, chatID, ownerKey, name string, arguments map[string]any) (map[string]any, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, toolsPath, nil)
	result, err := callAgentMCPTool(request, browserMCPCallParams{Name: name, Arguments: arguments},
		agentMCPOptions{ChatID: chatID, TabID: tabID, OwnerKey: ownerKey}, f.handler)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	envelope := mapFromAnyMain(result)
	content, _ := envelope["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("%s: unexpected result %#v", name, result)
	}
	text := fieldString(mapFromAnyMain(content[0]), "text")
	if envelope["isError"] == true {
		return nil, text
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatalf("%s: decode %q: %v", name, text, err)
	}
	return decoded, ""
}

func (f scheduledFixture) schedule(t *testing.T, tabID, chatID, operationID string, extra map[string]any) map[string]any {
	t.Helper()
	arguments := map[string]any{"message": "check the build", "operation_id": operationID}
	for key, value := range extra {
		arguments[key] = value
	}
	result, failure := f.tool(t, tabID, chatID, scheduledOwnerKey(chatID), "workass_schedule_message", arguments)
	if failure != "" {
		t.Fatalf("workass_schedule_message failed: %s", failure)
	}
	return mapFromAnyMain(result["scheduledMessage"])
}

func TestScheduledMessageToolsAreInTheAgentCatalog(t *testing.T) {
	tools := map[string]map[string]any{}
	for _, tool := range agentMCPTools() {
		tools[tool["name"].(string)] = tool
	}
	for name, readOnly := range map[string]bool{
		"workass_schedule_message":         false,
		"workass_list_scheduled_messages":  true,
		"workass_cancel_scheduled_message": false,
	} {
		tool, ok := tools[name]
		if !ok {
			t.Fatalf("agent catalog lacks %s", name)
		}
		annotations := tool["annotations"].(map[string]any)
		if annotations["readOnlyHint"] != readOnly {
			t.Fatalf("%s readOnlyHint = %v", name, annotations["readOnlyHint"])
		}
		if workassToolMutates(agentToolKind, name) == readOnly {
			t.Fatalf("%s operation id requirement disagrees with readOnlyHint", name)
		}
		if strings.Contains(strings.ToLower(name+tool["description"].(string)), "wake") {
			t.Fatalf("%s uses the word wake", name)
		}
	}
	properties := tools["workass_schedule_message"]["inputSchema"].(map[string]any)["properties"].(map[string]any)
	for _, key := range []string{"message", "at", "after_seconds", "when_tcp_up", "when_file_exists", "when_pid_exits", "expires_after_seconds", "operation_id"} {
		if _, ok := properties[key]; !ok {
			t.Fatalf("schedule schema lacks %s", key)
		}
	}
	cancelProperties := tools["workass_cancel_scheduled_message"]["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if _, ok := cancelProperties["scheduled_message_id"]; !ok || cancelProperties["operation_id"] == nil {
		t.Fatalf("cancel schema = %#v", cancelProperties)
	}
	childNames := map[string]bool{}
	for _, tool := range agentToolsForChild() {
		childNames[tool["name"].(string)] = true
	}
	if !childNames["workass_schedule_message"] {
		t.Fatal("a delegated agent loses the scheduled message tool")
	}
}

func TestScheduledMessageBindsToTheCallerChatAndRoundTrips(t *testing.T) {
	f := newScheduledFixture(t)
	f.create(t, "sched-tab", "sched-chat")
	f.create(t, "other-tab", "other-chat")

	created := f.schedule(t, "sched-tab", "sched-chat", "sched-op-1", map[string]any{"after_seconds": float64(60)})
	id := fieldString(created, "id")
	if id == "" || created["status"] != "pending" || fieldString(created, "fireAt") == "" || fieldString(created, "createdAt") == "" {
		t.Fatalf("schedule result = %#v", created)
	}
	entries := f.scheduler.List("sched-tab", "sched-chat")
	if len(entries) != 1 || entries[0].ID != id || entries[0].TabID != "sched-tab" || entries[0].ChatID != "sched-chat" {
		t.Fatalf("scheduled message is not bound to the caller chat: %#v", entries)
	}
	if len(f.scheduler.List("other-tab", "other-chat")) != 0 {
		t.Fatal("scheduled message leaked into another chat")
	}

	// A retried call returns the same scheduled message.
	if again := f.schedule(t, "sched-tab", "sched-chat", "sched-op-1", map[string]any{"after_seconds": float64(60)}); fieldString(again, "id") != id {
		t.Fatalf("retry created a second scheduled message: %#v", again)
	}
	f.clock.Advance(time.Second)
	f.schedule(t, "sched-tab", "sched-chat", "sched-op-2", map[string]any{"when_file_exists": "/tmp/workass-sched-never"})

	listed, failure := f.tool(t, "sched-tab", "sched-chat", scheduledOwnerKey("sched-chat"), "workass_list_scheduled_messages", map[string]any{})
	if failure != "" {
		t.Fatal(failure)
	}
	items, _ := listed["scheduledMessages"].([]any)
	if len(items) != 2 || fieldString(mapFromAnyMain(items[0]), "id") != id {
		t.Fatalf("list = %#v", listed)
	}
	second := mapFromAnyMain(items[1])
	if fieldString(second, "expiresAt") == "" || !strings.Contains(fieldString(second, "summary"), "/tmp/workass-sched-never") {
		t.Fatalf("condition scheduled message view = %#v", second)
	}
	if strings.Contains(stringifyAny(listed), "check the build") {
		t.Fatalf("list exposes the stored message: %#v", listed)
	}
	other, failure := f.tool(t, "other-tab", "other-chat", scheduledOwnerKey("other-chat"), "workass_list_scheduled_messages", map[string]any{})
	if failure != "" || len(other["scheduledMessages"].([]any)) != 0 {
		t.Fatalf("another chat sees scheduled messages: %v %#v", failure, other)
	}

	if _, failure := f.tool(t, "other-tab", "other-chat", scheduledOwnerKey("other-chat"), "workass_cancel_scheduled_message",
		map[string]any{"scheduled_message_id": id, "operation_id": "sched-cancel-foreign"}); !strings.Contains(failure, "no scheduled message") {
		t.Fatalf("another chat could cancel: %q", failure)
	}
	cancelled, failure := f.tool(t, "sched-tab", "sched-chat", scheduledOwnerKey("sched-chat"), "workass_cancel_scheduled_message",
		map[string]any{"scheduled_message_id": id, "operation_id": "sched-cancel-1"})
	if failure != "" {
		t.Fatal(failure)
	}
	if view := mapFromAnyMain(cancelled["scheduledMessage"]); view["status"] != "cancelled" || fieldString(view, "settledAt") == "" {
		t.Fatalf("cancel result = %#v", cancelled)
	}
	f.clock.Advance(2 * time.Minute)
	f.scheduler.Tick(context.Background())
	state, _ := f.chats.providerChats.Snapshot("sched-chat")
	if _, delivered := durableSteerInputForOperation(state, scheduledSendOperationID(id)); delivered {
		t.Fatal("a cancelled scheduled message was delivered")
	}
}

func TestScheduledMessageRejectsOtherTargetsAndInvalidOwners(t *testing.T) {
	f := newScheduledFixture(t)
	f.create(t, "sched-tab", "sched-chat")
	f.create(t, "other-tab", "other-chat")
	base := map[string]any{"message": "m", "operation_id": "sched-reject-op", "after_seconds": float64(60)}
	with := func(extra map[string]any) map[string]any {
		arguments := map[string]any{}
		for key, value := range base {
			arguments[key] = value
		}
		for key, value := range extra {
			arguments[key] = value
		}
		return arguments
	}
	for name, extra := range map[string]map[string]any{
		"other chat": {"tab_id": "other-tab", "chat_id": "other-chat"},
		"other tab":  {"tab_id": "other-tab"},
		"machine":    {"machine_id": "remote-machine"},
	} {
		if _, failure := f.tool(t, "sched-tab", "sched-chat", scheduledOwnerKey("sched-chat"), "workass_schedule_message", with(extra)); failure == "" {
			t.Fatalf("%s: explicit foreign target accepted", name)
		}
	}
	// The caller's own pair spelled out is fine.
	own := f.schedule(t, "sched-tab", "sched-chat", "sched-own-pair", map[string]any{"after_seconds": float64(60), "tab_id": "sched-tab", "chat_id": "sched-chat"})
	if own["status"] != "pending" {
		t.Fatalf("own explicit pair rejected: %#v", own)
	}
	if _, failure := f.tool(t, "sched-tab", "sched-chat", "wrong-owner", "workass_schedule_message", base); !strings.Contains(failure, "not an owned ACP session") {
		t.Fatalf("invalid owner failure = %q", failure)
	}
	if _, failure := f.tool(t, "sched-tab", "sched-chat", scheduledOwnerKey("other-chat"), "workass_list_scheduled_messages", map[string]any{}); !strings.Contains(failure, "not an owned ACP session") {
		t.Fatalf("another chat's owner key accepted: %q", failure)
	}
	if _, failure := f.tool(t, "wrong-tab", "sched-chat", scheduledOwnerKey("sched-chat"), "workass_schedule_message", base); failure == "" {
		t.Fatal("a tab that does not own the chat was accepted")
	}
	if len(f.scheduler.List("other-tab", "other-chat")) != 0 {
		t.Fatal("a rejected call scheduled something")
	}
}

func TestScheduledMessageIsDeliveredToTheCallerChatExactlyOnce(t *testing.T) {
	f := newScheduledFixture(t)
	f.create(t, "sched-tab", "sched-chat")
	f.create(t, "other-tab", "other-chat")
	created := f.schedule(t, "sched-tab", "sched-chat", "sched-due-op", map[string]any{"message": "the deploy should be finished", "after_seconds": float64(30)})
	id := fieldString(created, "id")
	operationID := scheduledSendOperationID(id)

	f.scheduler.Tick(context.Background())
	if state, _ := f.chats.providerChats.Snapshot("sched-chat"); countAgentSendOperations(state, string(operationID)) != 0 {
		t.Fatal("delivered before it was due")
	}
	f.clock.Advance(31 * time.Second)
	f.scheduler.Tick(context.Background())
	f.scheduler.Tick(context.Background())

	state, _ := f.chats.providerChats.Snapshot("sched-chat")
	if got := countAgentSendOperations(state, string(operationID)); got != 1 {
		t.Fatalf("scheduled message produced %d chat operations, want 1: %#v", got, state.Operations)
	}
	input, found := durableSteerInputForOperation(state, operationID)
	if !found || !strings.Contains(input.Text, "the deploy should be finished") || !strings.Contains(input.Text, id) {
		t.Fatalf("delivered input = %q found=%v", input.Text, found)
	}
	if otherState, _ := f.chats.providerChats.Snapshot("other-chat"); countAgentSendOperations(otherState, string(operationID)) != 0 {
		t.Fatal("delivered to a chat other than the caller's")
	}
	entries := f.scheduler.List("sched-tab", "sched-chat")
	if len(entries) != 1 || entries[0].Status != scheduledmessage.StatusFired {
		t.Fatalf("entry after delivery = %#v", entries)
	}

	// A crash between queueing and saving replays the same send: the queue
	// holds the deterministic operation id, so nothing is delivered twice, even
	// when the replayed text differs (the late marker is added after a restart).
	entry := entries[0]
	if err := f.chats.deliverScheduledMessage(context.Background(), entry, scheduledmessage.Text(entry)); err != nil {
		t.Fatalf("replay with the same text: %v", err)
	}
	entry.Late = true
	if err := f.chats.deliverScheduledMessage(context.Background(), entry, scheduledmessage.Text(entry)); err != nil {
		t.Fatalf("replay with a late marker: %v", err)
	}
	state, _ = f.chats.providerChats.Snapshot("sched-chat")
	if got := countAgentSendOperations(state, string(operationID)); got != 1 {
		t.Fatalf("replay duplicated the delivery: %d operations", got)
	}
}

func TestScheduledMessageDeliveryToAMissingChatFailsAsChatGone(t *testing.T) {
	f := newScheduledFixture(t)
	entry := scheduledmessage.Entry{ID: "sched-0000000000000001", TabID: "ghost-tab", ChatID: "ghost-chat"}
	err := f.chats.deliverScheduledMessage(context.Background(), entry, "text")
	if !errors.Is(err, scheduledmessage.ErrChatGone) {
		t.Fatalf("missing chat error = %v", err)
	}
	f.create(t, "real-tab", "real-chat")
	entry = scheduledmessage.Entry{ID: "sched-0000000000000002", TabID: "wrong-tab", ChatID: "real-chat"}
	if err := f.chats.deliverScheduledMessage(context.Background(), entry, "text"); !errors.Is(err, scheduledmessage.ErrChatGone) {
		t.Fatalf("foreign tab error = %v", err)
	}
}

func TestDeletingAChatCancelsItsScheduledMessages(t *testing.T) {
	f := newScheduledFixture(t)
	runtime := f.chats.providerChats
	root := repoRoot(t)
	for _, chatID := range []string{"delete-chat", "keep-chat"} {
		if _, err := runtime.CreateRendererChat(map[string]any{
			"tabId": chatID + "-tab", "chatId": chatID, "operationId": "create-" + chatID,
			"title": "Scheduled delete", "cwd": root, "providerId": "mock", "currentModelId": "mock-deterministic",
		}); err != nil {
			t.Fatalf("create %s: %v", chatID, err)
		}
	}
	doomed := fieldString(f.schedule(t, "delete-chat-tab", "delete-chat", "sched-del-1", map[string]any{"after_seconds": float64(600)}), "id")
	kept := fieldString(f.schedule(t, "keep-chat-tab", "keep-chat", "sched-keep-1", map[string]any{"after_seconds": float64(600)}), "id")

	if err := runtime.DeleteChat(context.Background(), "delete-chat-tab", "delete-chat", "delete-chat-op", true); err != nil {
		t.Fatalf("delete chat: %v", err)
	}
	entries := f.scheduler.List("delete-chat-tab", "delete-chat")
	if len(entries) != 1 || entries[0].ID != doomed || entries[0].Status != scheduledmessage.StatusCancelled || entries[0].Error != "chat deleted" {
		t.Fatalf("deleted chat's scheduled messages = %#v", entries)
	}
	if entries := f.scheduler.List("keep-chat-tab", "keep-chat"); len(entries) != 1 || entries[0].ID != kept || entries[0].Status != scheduledmessage.StatusPending {
		t.Fatalf("another chat's scheduled message changed: %#v", entries)
	}
}

func TestScheduledMessageToolsReportUnavailableWithoutAStore(t *testing.T) {
	runtime, create := newAgentChatSendActorFixture(t)
	create(t, "bare-tab", "bare-chat")
	chats := newChatControlCoordinator(nil, nil, runtime)
	handler := &agentControlHandler{chats: chats, validateOwner: func(string, string, string) bool { return true }}
	_, err := handler.call(httptest.NewRequest(http.MethodPost, toolsPath, nil), agentControlRequest{
		Method: "schedule.list",
		Params: map[string]any{"owner_key": "k", "parent_chat_id": "bare-chat", "parent_tab_id": "bare-tab"},
	})
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("list without a store = %v", err)
	}
	// Deleting a chat on a runtime without a store must not panic.
	if runtime.scheduled != nil {
		t.Fatal("runtime has a store it was never given")
	}
}
