package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	"workass/internal/scheduledmessage"
)

const (
	updateTestTab   = "update-tab"
	updateTestChat  = "update-chat"
	updateTestOther = "update-other-chat"
	updateTestSelf  = "m-self"
)

type updatePermissionFixture struct {
	scheduledFixture
	router *stubAgentChatRemoteRouter
}

func newUpdatePermissionFixture(t *testing.T) updatePermissionFixture {
	t.Helper()
	// Real turns on the mock provider, so a message reaches the transcript through
	// the same reducer path it does in production.
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
		t.Fatalf("start chat runtime: %v", err)
	}
	t.Cleanup(func() {
		_ = runtime.Close(context.Background())
		manager.Reset()
	})
	// The scheduler's clock and the message times have to agree here.
	clock := &scheduledClock{now: time.Now().UTC()}
	chats := newChatControlCoordinator(nil, nil, runtime)
	scheduler, err := chats.attachScheduledMessages(t.TempDir(), scheduledmessage.Options{Now: clock.Now})
	if err != nil {
		t.Fatalf("attach scheduled messages: %v", err)
	}
	router := &stubAgentChatRemoteRouter{result: map[string]any{"ok": true}}
	handler := &agentControlHandler{
		chats: chats, remoteChats: router, selfMachineID: updateTestSelf, stateDir: t.TempDir(),
		validateOwner: func(ownerKey, chatID, tabID string) bool { return ownerKey == scheduledOwnerKey(chatID) },
	}
	create := func(t *testing.T, tabID, chatID string) {
		t.Helper()
		if _, err := runtime.CreateRendererChat(map[string]any{
			"tabId": tabID, "chatId": chatID, "operationId": "create-" + chatID,
			"title": "Update owner", "cwd": root, "providerId": "mock", "currentModelId": "mock-deterministic",
		}); err != nil {
			t.Fatalf("create chat: %v", err)
		}
	}
	create(t, updateTestTab, updateTestChat)
	return updatePermissionFixture{
		scheduledFixture: scheduledFixture{chats: chats, handler: handler, scheduler: scheduler, clock: clock, create: create},
		router:           router,
	}
}

// humanSays admits a message exactly as a person's message at the controller is
// admitted: job:start, whose origin the daemon stamps.
func (f updatePermissionFixture) humanSays(t *testing.T, tabID, chatID, messageID, text string) {
	t.Helper()
	admission, err := f.chats.providerChats.AdmitStart(context.Background(), map[string]any{
		"tabId": tabID, "chatId": chatID, "prompt": text, "providerId": "mock",
		"userMessageId": messageID, "assistantMessageId": messageID + "-assistant",
	}, "human")
	if err != nil {
		t.Fatalf("admit %s: %v", messageID, err)
	}
	admission.Dispatch()
	f.waitIdleWith(t, chatID, messageID)
}

func (f updatePermissionFixture) waitIdleWith(t *testing.T, chatID, messageID string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		state, ok := f.chats.providerChats.Snapshot(chatID)
		if ok && state.Foreground == nil && len(state.Queue) == 0 {
			if _, found := state.UserMessageRecord(messageID); found {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("message %s never settled: %#v", messageID, state.Foreground)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f updatePermissionFixture) apply(t *testing.T, arguments map[string]any) (map[string]any, string) {
	t.Helper()
	return f.tool(t, updateTestTab, updateTestChat, scheduledOwnerKey(updateTestChat), "workass_apply_update", arguments)
}

func updateApplyArguments(machine, target, operation, message string) map[string]any {
	arguments := map[string]any{
		"machine_id": machine, "expected_current_version": "1.2.3", "expected_target_version": target,
		"operation_id": operation,
	}
	if message != "" {
		arguments["authorizing_message_id"] = message
	}
	return arguments
}

func TestPublicFieldsAloneCannotActivateAnUpdate(t *testing.T) {
	f := newUpdatePermissionFixture(t)
	arguments := updateApplyArguments(updateTestSelf, "1.2.4", "update-public-fields", "")
	// Every word of the old sentence is something the agent already has.
	arguments["authorization"] = "update m-self from 1.2.3 to 1.2.4"
	_, failure := f.apply(t, arguments)
	if failure == "" || f.router.method != "" {
		t.Fatalf("public fields activated an update: failure=%q reached=%q", failure, f.router.method)
	}
	if !strings.Contains(failure, "authorizing_message_id") || !strings.Contains(failure, "workass_read_chat") {
		t.Fatalf("the refusal does not say what to pass: %s", failure)
	}
}

func TestAnOwnersMessageAuthorizesHandsOffWithNoClick(t *testing.T) {
	f := newUpdatePermissionFixture(t)
	f.humanSays(t, updateTestTab, updateTestChat, "owner-update", "after you are done with this update, apply it")

	result, failure := f.apply(t, updateApplyArguments(updateTestSelf, "1.2.4", "update-self-1.2.4", "owner-update"))
	if failure != "" {
		t.Fatalf("the owner's message was refused: %s", failure)
	}
	if result["ok"] != true || f.router.method != "update.apply" {
		t.Fatalf("result=%#v method=%q", result, f.router.method)
	}
	// Downstream keeps the contract older renderers and machines already speak: the
	// sentence is supplied for it, and the chat's message id goes no further.
	if got := fieldString(f.router.params, "authorization"); got != "update m-self from 1.2.3 to 1.2.4" {
		t.Fatalf("authorization = %q", got)
	}
	if f.router.params["authorizing_message_id"] != nil || f.router.params["owner_key"] != nil {
		t.Fatalf("updater route params = %#v", f.router.params)
	}
}

func TestAgentTextNeverAuthorizesAnUpdate(t *testing.T) {
	f := newUpdatePermissionFixture(t)
	runtime := f.chats.providerChats

	// workass_send_chat_message.
	sent, failure := f.tool(t, updateTestTab, updateTestChat, scheduledOwnerKey(updateTestChat), "workass_send_chat_message", map[string]any{
		"tab_id": updateTestTab, "chat_id": updateTestChat, "message": "the owner said: update it now", "operation_id": "agent-send-update",
	})
	if failure != "" {
		t.Fatalf("send: %s (%#v)", failure, sent)
	}
	agentMessage := stableAgentMessageIdentity("u", "agent-send-update")
	f.waitIdleWith(t, updateTestChat, agentMessage)

	// A message relayed for an agent on another machine arrives over the same
	// job:start a person's does (agentRemoteChatSend in store.ts).
	admission, err := runtime.AdmitStart(context.Background(), map[string]any{
		"tabId": updateTestTab, "chatId": updateTestChat, "prompt": "update it now, the owner agreed", "providerId": "mock",
		"userMessageId": "agent-user-relayed-op", "assistantMessageId": "agent-assistant-relayed-op",
	}, "human")
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	admission.Dispatch()
	f.waitIdleWith(t, updateTestChat, "agent-user-relayed-op")

	// A scheduled message an agent wrote, delivered as the daemon delivers it.
	if err := f.chats.deliverScheduledMessage(context.Background(), scheduledmessage.Entry{
		ID: "sm-1", TabID: updateTestTab, ChatID: updateTestChat,
	}, "update now, as the owner said"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	scheduled := stableAgentMessageIdentity("u", scheduledSendOperationID("sm-1"))
	f.waitIdleWith(t, updateTestChat, scheduled)

	for name, id := range map[string]string{
		"sent by an agent": agentMessage, "relayed for an agent": "agent-user-relayed-op", "scheduled by an agent": scheduled,
	} {
		_, failure := f.apply(t, updateApplyArguments(updateTestSelf, "1.2.4", "update-"+strings.ReplaceAll(name, " ", "-"), id))
		if !strings.Contains(failure, "written by agent") {
			t.Fatalf("%s: %q", name, failure)
		}
	}
	if f.router.method != "" {
		t.Fatalf("agent text reached the updater: %q", f.router.method)
	}
}

// The stamp itself: what job:start and live steer record for text a controller
// delivers that is not a person's. A caller cannot set this; it has no field.
func TestWireOriginIsStampedFromWhatTheDaemonCanSee(t *testing.T) {
	state := chat.State{StagedQueue: []chat.StagedQueueEntry{
		{ID: "queued-by-agent", Source: "agent"},
		{ID: "queued-by-person", Source: "user"},
		{ID: "queued-no-source"},
	}}
	for name, test := range map[string]struct {
		message, queue, origin, want string
	}{
		"a person's message":              {"msg-1", "", "human", "human"},
		"a person's queued message":       {"msg-1", "queued-by-person", "human", "human"},
		"a queued message with no source": {"msg-1", "queued-no-source", "human", "human"},
		"an unknown queue row":            {"msg-1", "no-such-row", "human", "human"},
		"a relayed agent message":         {"agent-user-op-1", "", "human", "agent"},
		"an agent's queued row":           {"msg-1", "queued-by-agent", "human", "agent"},
		"already an agent":                {"msg-1", "", "agent", "agent"},
		"internal stays internal":         {"agent-user-op-1", "queued-by-agent", "internal", "internal"},
	} {
		if got := wireTurnOrigin(state, test.message, test.queue, test.origin); got != test.want {
			t.Errorf("%s = %q, want %q", name, got, test.want)
		}
	}
}

func TestTheMessageMustBeInTheCallingChat(t *testing.T) {
	f := newUpdatePermissionFixture(t)
	f.create(t, "update-other-tab", updateTestOther)
	f.humanSays(t, "update-other-tab", updateTestOther, "owner-elsewhere", "please update this machine")

	_, failure := f.apply(t, updateApplyArguments(updateTestSelf, "1.2.4", "update-elsewhere", "owner-elsewhere"))
	if !strings.Contains(failure, "not in this chat") || f.router.method != "" {
		t.Fatalf("failure=%q reached=%q", failure, f.router.method)
	}
}

func TestTheOwnersMessageMustMentionAnUpdate(t *testing.T) {
	f := newUpdatePermissionFixture(t)
	f.humanSays(t, updateTestTab, updateTestChat, "owner-other", "please fix the login page")
	_, failure := f.apply(t, updateApplyArguments(updateTestSelf, "1.2.4", "update-off-topic", "owner-other"))
	if !strings.Contains(failure, "does not mention an update") || f.router.method != "" {
		t.Fatalf("failure=%q reached=%q", failure, f.router.method)
	}
	for _, text := range []string{
		"update", "Update it", "after you are done with this update", "updates are fine", "go ahead, updating is OK",
		"upgrade this machine", "(update)", "actualizá la app", "actualizalo cuando termines",
		"Actualización permitida", "que se actualice cuando termines", "actualiza", "updatealo nomás",
	} {
		if !mentionsAnUpdate(text) {
			t.Errorf("%q was not read as mentioning an update", text)
		}
	}
	for _, text := range []string{"", "fix the login page", "outdated docs", "actualmente no", "la actualidad", "preupdate hook"} {
		if mentionsAnUpdate(text) {
			t.Errorf("%q was read as mentioning an update", text)
		}
	}
}

func TestTheOwnersMessageMustBeRecent(t *testing.T) {
	f := newUpdatePermissionFixture(t)
	f.humanSays(t, updateTestTab, updateTestChat, "owner-old", "update when you can")
	state, _ := f.chats.providerChats.Snapshot(updateTestChat)
	record, _ := state.UserMessageRecord("owner-old")
	written, err := time.Parse(time.RFC3339Nano, record.At)
	if err != nil {
		t.Fatalf("message time %q: %v", record.At, err)
	}

	f.handler.updatePermissionNow = func() time.Time { return written.Add(updatePermissionMaxAge + time.Minute) }
	_, failure := f.apply(t, updateApplyArguments(updateTestSelf, "1.2.4", "update-too-late", "owner-old"))
	if !strings.Contains(failure, "too old") || !strings.Contains(failure, "8 days") || f.router.method != "" {
		t.Fatalf("failure=%q reached=%q", failure, f.router.method)
	}
	f.handler.updatePermissionNow = func() time.Time { return written.Add(updatePermissionMaxAge - time.Minute) }
	if _, failure := f.apply(t, updateApplyArguments(updateTestSelf, "1.2.4", "update-in-time", "owner-old")); failure != "" {
		t.Fatalf("inside the bound: %s", failure)
	}
}

func TestOneMessageIsOneActivationPerMachineAndVersion(t *testing.T) {
	f := newUpdatePermissionFixture(t)
	f.humanSays(t, updateTestTab, updateTestChat, "owner-once", "update all of them")

	if _, failure := f.apply(t, updateApplyArguments(updateTestSelf, "1.2.4", "update-first", "owner-once")); failure != "" {
		t.Fatalf("first: %s", failure)
	}

	// A second operation for the same permission never reaches the updater, and
	// is told how to read the first one's receipt.
	f.router.method = ""
	_, failure := f.apply(t, updateApplyArguments(updateTestSelf, "1.2.4", "update-second", "owner-once"))
	if !strings.Contains(failure, `already authorized operation "update-first"`) || f.router.method != "" {
		t.Fatalf("failure=%q reached=%q", failure, f.router.method)
	}

	// Replaying the first reaches the updater, whose durable receipt answers it, and
	// it still does past the age limit: this is reading, not a new activation.
	f.handler.updatePermissionNow = func() time.Time { return time.Now().Add(30 * 24 * time.Hour) }
	if _, failure := f.apply(t, updateApplyArguments(updateTestSelf, "1.2.4", "update-first", "owner-once")); failure != "" {
		t.Fatalf("replay: %s", failure)
	}
	if f.router.method != "update.apply" || fieldString(f.router.params, "operation_id") != "update-first" {
		t.Fatalf("replay route method=%q params=%#v", f.router.method, f.router.params)
	}
	f.handler.updatePermissionNow = nil

	// The same message may name the next version, and another machine: the owner's
	// permission is scoped by what the message says, which the agent answers for.
	if _, failure := f.apply(t, updateApplyArguments(updateTestSelf, "1.2.5", "update-next-version", "owner-once")); failure != "" {
		t.Fatalf("next version: %s", failure)
	}
	if _, failure := f.apply(t, updateApplyArguments("m-other", "1.2.4", "update-other-machine", "owner-once")); failure != "" {
		t.Fatalf("other machine: %s", failure)
	}

	// A reused operation id cannot be pointed at something else.
	_, failure = f.apply(t, updateApplyArguments("m-third", "1.2.4", "update-first", "owner-once"))
	if !strings.Contains(failure, "reused") {
		t.Fatalf("reused operation: %q", failure)
	}

	// All of it survives a restart: the record is on disk, not in this handler.
	restarted := &agentControlHandler{chats: f.chats, validateOwner: f.handler.validateOwner, remoteChats: f.router,
		selfMachineID: updateTestSelf, stateDir: f.handler.stateDir}
	f.router.method = ""
	second := f.scheduledFixture
	second.handler = restarted
	_, failure = second.tool(t, updateTestTab, updateTestChat, scheduledOwnerKey(updateTestChat), "workass_apply_update",
		updateApplyArguments(updateTestSelf, "1.2.4", "update-after-restart", "owner-once"))
	if !strings.Contains(failure, "already authorized") || f.router.method != "" {
		t.Fatalf("after restart: failure=%q reached=%q", failure, f.router.method)
	}
}

// A refusal below the check spends nothing: a machine that was briefly unreachable
// is not a used-up permission, and the same operation may try again.
func TestAFailedRouteDoesNotLoseThePermission(t *testing.T) {
	f := newUpdatePermissionFixture(t)
	f.humanSays(t, updateTestTab, updateTestChat, "owner-retry", "update it")
	f.router.err = fmt.Errorf("remote machine m-other is unavailable")
	if _, failure := f.apply(t, updateApplyArguments("m-other", "1.2.4", "update-retry-op", "owner-retry")); !strings.Contains(failure, "unavailable") {
		t.Fatalf("failure=%q", failure)
	}
	f.router.err = nil
	if _, failure := f.apply(t, updateApplyArguments("m-other", "1.2.4", "update-retry-op", "owner-retry")); failure != "" {
		t.Fatalf("retry of the same operation: %s", failure)
	}
}

func TestARemoteMachineIsCheckedOnTheSideThatHoldsTheChat(t *testing.T) {
	f := newUpdatePermissionFixture(t)
	if _, failure := f.apply(t, updateApplyArguments("m-work", "1.2.4", "update-remote-none", "")); failure == "" || f.router.method != "" {
		t.Fatalf("a remote update with no permission: failure=%q reached=%q", failure, f.router.method)
	}
	f.humanSays(t, updateTestTab, updateTestChat, "owner-remote", "actualizá m-work cuando termines")
	if _, failure := f.apply(t, updateApplyArguments("m-work", "1.2.4", "update-remote", "owner-remote")); failure != "" {
		t.Fatalf("remote: %s", failure)
	}
	if f.router.method != "update.apply" || fieldString(f.router.params, "machine_id") != "m-work" ||
		fieldString(f.router.params, "authorization") != "update m-work from 1.2.3 to 1.2.4" ||
		f.router.params["authorizing_message_id"] != nil {
		t.Fatalf("route method=%q params=%#v", f.router.method, f.router.params)
	}
}

func TestAProgrammedUpdateKeepsTheOwnersOriginalMessage(t *testing.T) {
	f := newUpdatePermissionFixture(t)
	f.humanSays(t, updateTestTab, updateTestChat, "owner-later", "update when the research is done")

	entry := f.schedule(t, updateTestTab, updateTestChat, "schedule-update", map[string]any{
		"message":       `The owner said: "update when the research is done". Apply the update now.`,
		"after_seconds": 3600, "authorizing_message_id": "owner-later",
	})
	if entry["authorizingMessageId"] != "owner-later" {
		t.Fatalf("entry = %#v", entry)
	}

	// Due: the daemon delivers it as agent text, naming the owner's message.
	id := fieldString(entry, "id")
	var delivered scheduledmessage.Entry
	for _, candidate := range f.scheduler.List(updateTestTab, updateTestChat) {
		if candidate.ID == id {
			delivered = candidate
		}
	}
	text := scheduledmessage.Text(delivered)
	if !strings.Contains(text, "authorizing_message_id for workass_apply_update: owner-later") {
		t.Fatalf("delivered text = %q", text)
	}
	if err := f.chats.deliverScheduledMessage(context.Background(), delivered, text); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	deliveredID := stableAgentMessageIdentity("u", scheduledSendOperationID(id))
	f.waitIdleWith(t, updateTestChat, deliveredID)

	// The delivered message itself authorizes nothing; the owner's original does.
	if _, failure := f.apply(t, updateApplyArguments(updateTestSelf, "1.2.4", "update-from-schedule-text", deliveredID)); !strings.Contains(failure, "written by agent") {
		t.Fatalf("scheduled text: %q", failure)
	}
	if _, failure := f.apply(t, updateApplyArguments(updateTestSelf, "1.2.4", "update-programmed", "owner-later")); failure != "" {
		t.Fatalf("the programmed update was refused: %s", failure)
	}
}

func TestSchedulingChecksThePermissionWhileTheOwnerIsReachable(t *testing.T) {
	f := newUpdatePermissionFixture(t)
	f.humanSays(t, updateTestTab, updateTestChat, "owner-soon", "update tomorrow")
	schedule := func(extra map[string]any) string {
		arguments := map[string]any{"message": "apply it", "operation_id": fmt.Sprintf("schedule-%d", time.Now().UnixNano())}
		for key, value := range extra {
			arguments[key] = value
		}
		_, failure := f.tool(t, updateTestTab, updateTestChat, scheduledOwnerKey(updateTestChat), "workass_schedule_message", arguments)
		return failure
	}
	if failure := schedule(map[string]any{"after_seconds": 60, "authorizing_message_id": "no-such-message"}); !strings.Contains(failure, "not in this chat") {
		t.Fatalf("unknown id: %q", failure)
	}
	// Still a week out with the longest condition the scheduler allows.
	if failure := schedule(map[string]any{"after_seconds": 7 * 24 * 3600, "authorizing_message_id": "owner-soon"}); failure != "" {
		t.Fatalf("the longest schedule: %s", failure)
	}
	// One it would be refused at when it came due is refused now instead.
	f.clock.now = f.clock.now.Add(2 * 24 * time.Hour)
	if failure := schedule(map[string]any{"after_seconds": 7 * 24 * 3600, "authorizing_message_id": "owner-soon"}); !strings.Contains(failure, "too old") {
		t.Fatalf("past the bound: %q", failure)
	}
	// No id is the ordinary scheduled message it always was.
	if failure := schedule(map[string]any{"after_seconds": 60}); failure != "" {
		t.Fatalf("ordinary: %s", failure)
	}
}

// Fail closed, naming what to do: a row with no recorded origin is a message from
// before this was recorded, or a shape that cannot say. It is never assumed human.
func TestAnUnknownOriginFailsClosedAndSaysWhatToDo(t *testing.T) {
	now := time.Now().UTC()
	at := now.Format(time.RFC3339Nano)
	ledger := func(event chat.LedgerEvent) chat.State {
		event.Role = "user"
		event.MessageID = "m"
		event.Text = "update it"
		if event.At == "" {
			event.At = at
		}
		return chat.State{ChatID: "c", Ledger: []chat.LedgerEvent{event}}
	}
	for name, test := range map[string]struct {
		state chat.State
		want  string
	}{
		"no origin recorded":  {ledger(chat.LedgerEvent{}), "Ask the owner to send a new message"},
		"internal":            {ledger(chat.LedgerEvent{Origin: "internal"}), "written by internal"},
		"a notice":            {ledger(chat.LedgerEvent{Origin: "human", NoticeKind: "subagent"}), "Workass notice"},
		"copied by a fork":    {ledger(chat.LedgerEvent{Origin: "human", Inherited: true}), "copied into this chat"},
		"excluded by a fork":  {ledger(chat.LedgerEvent{Origin: "human", ContextExcluded: true}), "copied into this chat"},
		"stopped before sent": {ledger(chat.LedgerEvent{Origin: "human", TerminalState: "cancelled_before_admission"}), "stopped message"},
		"no usable time":      {ledger(chat.LedgerEvent{Origin: "human", At: "yesterday"}), "no usable time"},
		"a time far ahead":    {ledger(chat.LedgerEvent{Origin: "human", At: now.Add(48 * time.Hour).Format(time.RFC3339Nano)}), "too old"},
		"nothing in the chat": {chat.State{ChatID: "c"}, "not in this chat"},
	} {
		_, err := verifyOwnerUpdateMessage(test.state, "m", now)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := verifyOwnerUpdateMessage(ledger(chat.LedgerEvent{Origin: "human"}), "m", now); err != nil {
		t.Fatalf("a good message: %v", err)
	}
}

func TestADamagedPermissionRecordRefusesRatherThanForgets(t *testing.T) {
	f := newUpdatePermissionFixture(t)
	f.humanSays(t, updateTestTab, updateTestChat, "owner-damaged", "update it")
	if err := os.WriteFile(filepath.Join(f.handler.stateDir, updateAuthorizationsFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, failure := f.apply(t, updateApplyArguments(updateTestSelf, "1.2.4", "update-damaged", "owner-damaged"))
	if !strings.Contains(failure, "damaged") || f.router.method != "" {
		t.Fatalf("failure=%q reached=%q", failure, f.router.method)
	}
}

func TestAWrongSentenceIsStillRefusedButNotRequired(t *testing.T) {
	f := newUpdatePermissionFixture(t)
	f.humanSays(t, updateTestTab, updateTestChat, "owner-sentence", "update it")
	arguments := updateApplyArguments(updateTestSelf, "1.2.4", "update-wrong-sentence", "owner-sentence")
	arguments["authorization"] = "please update"
	if _, failure := f.apply(t, arguments); !strings.Contains(failure, "does not exactly match") || f.router.method != "" {
		t.Fatalf("failure=%q reached=%q", failure, f.router.method)
	}
	arguments["authorization"] = "update m-self from 1.2.3 to 1.2.4"
	if _, failure := f.apply(t, arguments); failure != "" {
		t.Fatalf("the exact sentence with a real message: %s", failure)
	}
}

func TestADelegatedAgentHasNoUpdateTool(t *testing.T) {
	for _, tool := range agentToolsForChild() {
		if tool["name"] == "workass_apply_update" {
			t.Fatal("a delegated agent can apply an update")
		}
	}
	found := false
	for _, tool := range agentMCPTools() {
		if tool["name"] == "workass_apply_update" {
			found = true
			description := toString(tool["description"])
			for _, want := range []string{"authorizing_message_id", "workass_read_chat", "AGENTS.md"} {
				if !strings.Contains(description, want) {
					t.Errorf("description lacks %q: %s", want, description)
				}
			}
		}
	}
	if !found {
		t.Fatal("the chat's own agent lost the update tool")
	}
}

func TestAgentReadShowsWhoWroteEachInput(t *testing.T) {
	f := newUpdatePermissionFixture(t)
	f.humanSays(t, updateTestTab, updateTestChat, "owner-read", "update when you can")
	f.create(t, "update-other-tab", updateTestOther)

	// An agent that has never been told its own ids finds its chat first.
	listed, failure := f.tool(t, updateTestTab, updateTestChat, scheduledOwnerKey(updateTestChat), "workass_list_chats", map[string]any{})
	if failure != "" {
		t.Fatalf("list: %s", failure)
	}
	marked := map[string]bool{}
	for _, raw := range anySlice(listed["chats"]) {
		item := mapFromAnyMain(raw)
		marked[fieldString(item, "chatId")] = item["callingChat"] == true
	}
	if !marked[updateTestChat] || marked[updateTestOther] || len(marked) != 2 {
		t.Fatalf("calling chat marks = %#v", marked)
	}

	result, err := f.chats.providerChats.ReadChat(updateTestTab, updateTestChat, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, raw := range anySlice(result["messages"]) {
		message := mapFromAnyMain(raw)
		if fieldString(message, "id") == "owner-read" {
			found = true
			if message["origin"] != "human" {
				t.Fatalf("message = %#v", message)
			}
		}
		if fieldString(message, "role") == "assistant" && message["origin"] != nil {
			t.Fatalf("an assistant row got an origin: %#v", message)
		}
	}
	if !found {
		t.Fatalf("no owner message in %#v", result["messages"])
	}
}
