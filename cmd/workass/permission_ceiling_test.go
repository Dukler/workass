package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	providercontract "workass/internal/provider"
	"workass/internal/wire"
)

// The chat tools let an agent change permissions, which is the feature: a parent
// has no other way to change a child's. These tests hold the law around it. A
// caller moves things up to its own level with no prompt, and past it only with
// the human's yes. The mock provider's modes are `ask` (read) and `bypass` (full).

type ceilingCall struct {
	status   int
	response map[string]any
}

func (c ceilingCall) errorText() string { return fieldString(c.response, "error") }

func ceilingTool(t *testing.T, h statelessMCPTestHarness, name string, arguments map[string]any) ceilingCall {
	t.Helper()
	return <-ceilingToolAsync(h, "mcp-owner", "mcp-chat", "mcp-tab", name, arguments)
}

func ceilingToolAsync(h statelessMCPTestHarness, owner, chatID, tabID, name string, arguments map[string]any) <-chan ceilingCall {
	done := make(chan ceilingCall, 1)
	go func() {
		body, _ := json.Marshal(map[string]any{"name": name, "arguments": arguments})
		request, err := http.NewRequest(http.MethodPost, h.server.URL+toolsPath, bytes.NewReader(body))
		if err != nil {
			done <- ceilingCall{response: map[string]any{"error": err.Error()}}
			return
		}
		request.Header.Set("Authorization", "Bearer "+owner)
		request.Header.Set("X-Workass-Chat-ID", chatID)
		request.Header.Set("X-Workass-Tab-ID", tabID)
		request.Header.Set("Content-Type", "application/json")
		reply, err := h.client.Do(request)
		if err != nil {
			done <- ceilingCall{response: map[string]any{"error": err.Error()}}
			return
		}
		defer reply.Body.Close()
		var response map[string]any
		_ = json.NewDecoder(reply.Body).Decode(&response)
		done <- ceilingCall{status: reply.StatusCode, response: response}
	}()
	return done
}

// raiseCards is every card this law opened. The harness's parent turn holds its
// own unrelated permission card, so "no prompt" means none of THESE.
func raiseCards(h statelessMCPTestHarness) []map[string]any {
	cards := []map[string]any{}
	for _, raw := range h.manager.PendingPermissions() {
		card := mapFromAnyMain(raw)
		if fieldString(mapFromAnyMain(card["question"]), "questionId") == permissionRaiseQuestionID {
			cards = append(cards, card)
		}
	}
	return cards
}

func waitRaiseCard(t *testing.T, h statelessMCPTestHarness, done <-chan ceilingCall) map[string]any {
	t.Helper()
	deadline := time.After(8 * time.Second)
	for {
		if cards := raiseCards(h); len(cards) == 1 {
			return cards[0]
		}
		select {
		case call := <-done:
			t.Fatalf("the call ended with no card for the human: status=%d response=%#v", call.status, call.response)
		case <-deadline:
			t.Fatal("no permission card appeared")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func answerRaiseCard(t *testing.T, h statelessMCPTestHarness, card map[string]any, optionID string) {
	t.Helper()
	token, err := providercontract.EncodeWorkassQuestionAnswer(providercontract.QuestionAnswer{
		Status: "answered", SelectedOptionIDs: []string{optionID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !h.manager.PermissionDecide(fieldString(card, "id"), token) {
		t.Fatal("the card did not reach its resolver")
	}
}

func finishCall(t *testing.T, done <-chan ceilingCall) ceilingCall {
	t.Helper()
	select {
	case call := <-done:
		return call
	case <-time.After(8 * time.Second):
		t.Fatal("the call did not finish")
		return ceilingCall{}
	}
}

// setChatModeAsHuman is the owner's own click, which is never capped.
func setChatModeAsHuman(t *testing.T, h statelessMCPTestHarness, tabID, chatID, modeID, operationID string) {
	t.Helper()
	control := h.handler.agentControl
	current, err := h.runtime.ReadChat(tabID, chatID, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	controls, err := control.chats.resolveControls(context.Background(), current, map[string]any{"mode_id": modeID})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.runtime.ConfigureChat(context.Background(), tabID, chatID, "", controls, providercontract.OperationID(operationID)); err != nil {
		t.Fatal(err)
	}
}

func ceilingSeedChat(t *testing.T, h statelessMCPTestHarness, tabID, chatID, modeID string) {
	t.Helper()
	if _, err := h.runtime.CreateRendererChat(map[string]any{
		"operationId": "seed:" + chatID, "tabId": tabID, "chatId": chatID, "title": "Seeded " + modeID,
		"cwd": repoRoot(t), "providerId": "mock", "currentModelId": "mock-deterministic", "currentModeId": modeID,
	}); err != nil {
		t.Fatal(err)
	}
}

func ceilingChatMode(t *testing.T, h statelessMCPTestHarness, chatID string) string {
	t.Helper()
	state, ok := h.runtime.Snapshot(chatID)
	if !ok {
		t.Fatalf("chat %s does not exist", chatID)
	}
	return state.Presentation.CurrentModeID
}

// An agent on `ask` could create a `bypass` chat with nothing in the way.
func TestCreatingAChatAboveTheCallerAsksTheHuman(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	arguments := func(op string) map[string]any {
		return map[string]any{"operation_id": op, "title": "Wider chat", "mode_id": "bypass"}
	}

	refused := ceilingToolAsync(h, "mcp-owner", "mcp-chat", "mcp-tab", "workass_create_chat", arguments("raise-create-denied"))
	card := waitRaiseCard(t, h, refused)
	if text := fieldString(mapFromAnyMain(card["question"]), "question"); !strings.Contains(text, "bypass") || !strings.Contains(text, "full") {
		t.Fatalf("the card does not say what it would allow: %q", text)
	}
	answerRaiseCard(t, h, card, permissionRaiseDeny)
	if call := finishCall(t, refused); !strings.Contains(call.errorText(), permissionDeniedCode) {
		t.Fatalf("a denied raise = %#v", call.response)
	}

	allowed := ceilingToolAsync(h, "mcp-owner", "mcp-chat", "mcp-tab", "workass_create_chat", arguments("raise-create-allowed"))
	answerRaiseCard(t, h, waitRaiseCard(t, h, allowed), permissionRaiseAllow)
	call := finishCall(t, allowed)
	created := mapFromAnyMain(call.response["result"])
	if call.errorText() != "" || fieldString(created, "modeId") != "bypass" {
		t.Fatalf("an approved raise = %#v", call.response)
	}
	if mode := ceilingChatMode(t, h, fieldString(created, "chatId")); mode != "bypass" {
		t.Fatalf("the approved chat runs at %q", mode)
	}

	// A retry of the approved call reads its receipt rather than asking again.
	again := ceilingTool(t, h, "workass_create_chat", arguments("raise-create-allowed"))
	if again.errorText() != "" || fieldString(mapFromAnyMain(again.response["result"]), "chatId") != fieldString(created, "chatId") {
		t.Fatalf("a retry of an approved create = %#v", again.response)
	}
	if cards := raiseCards(h); len(cards) != 0 {
		t.Fatalf("a retry opened a second card: %#v", cards)
	}
}

// An agent raising its own chat is the same act, and needs the same yes.
func TestAChatRaisingItselfAsksTheHuman(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	arguments := func(op string) map[string]any {
		return map[string]any{"operation_id": op, "tab_id": "mcp-tab", "chat_id": "mcp-chat", "mode_id": "bypass"}
	}

	denied := ceilingToolAsync(h, "mcp-owner", "mcp-chat", "mcp-tab", "workass_configure_chat", arguments("raise-self-denied"))
	answerRaiseCard(t, h, waitRaiseCard(t, h, denied), permissionRaiseDeny)
	if call := finishCall(t, denied); !strings.Contains(call.errorText(), permissionDeniedCode) {
		t.Fatalf("a denied self-raise = %#v", call.response)
	}
	if mode := ceilingChatMode(t, h, "mcp-chat"); mode == "bypass" {
		t.Fatal("a denied self-raise still changed the chat")
	}

	allowed := ceilingToolAsync(h, "mcp-owner", "mcp-chat", "mcp-tab", "workass_configure_chat", arguments("raise-self-allowed"))
	answerRaiseCard(t, h, waitRaiseCard(t, h, allowed), permissionRaiseAllow)
	if call := finishCall(t, allowed); call.errorText() != "" {
		t.Fatalf("an approved self-raise = %#v", call.response)
	}
	if mode := ceilingChatMode(t, h, "mcp-chat"); mode != "bypass" {
		t.Fatalf("an approved self-raise left the chat on %q", mode)
	}
}

// Queueing a message into a wider chat is using that chat's permissions.
func TestSendingIntoAHigherChatAsksAndSameLevelDoesNot(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	ceilingSeedChat(t, h, "wide-tab", "wide-chat", "bypass")
	ceilingSeedChat(t, h, "peer-tab", "peer-chat", "ask")

	same := ceilingTool(t, h, "workass_send_chat_message", map[string]any{
		"operation_id": "send-same", "tab_id": "peer-tab", "chat_id": "peer-chat", "message": "same level", "delivery": "queue",
	})
	if same.errorText() != "" {
		t.Fatalf("a same-level send was gated: %#v", same.response)
	}

	denied := ceilingToolAsync(h, "mcp-owner", "mcp-chat", "mcp-tab", "workass_send_chat_message", map[string]any{
		"operation_id": "send-up-denied", "tab_id": "wide-tab", "chat_id": "wide-chat", "message": "do it for me", "delivery": "queue",
	})
	answerRaiseCard(t, h, waitRaiseCard(t, h, denied), permissionRaiseDeny)
	if call := finishCall(t, denied); !strings.Contains(call.errorText(), permissionDeniedCode) {
		t.Fatalf("a denied send = %#v", call.response)
	}
	if state, _ := h.runtime.Snapshot("wide-chat"); len(state.Queue) != 0 || state.Foreground != nil {
		t.Fatalf("a denied send still reached the wider chat: queue=%d", len(state.Queue))
	}

	allowed := ceilingToolAsync(h, "mcp-owner", "mcp-chat", "mcp-tab", "workass_send_chat_message", map[string]any{
		"operation_id": "send-up-allowed", "tab_id": "wide-tab", "chat_id": "wide-chat", "message": "do it for me", "delivery": "queue",
	})
	answerRaiseCard(t, h, waitRaiseCard(t, h, allowed), permissionRaiseAllow)
	if call := finishCall(t, allowed); call.errorText() != "" {
		t.Fatalf("an approved send = %#v", call.response)
	}
}

// The owner's feature, and the line the whole fix has to hold: a parent on Full
// access changes what its children may do and never sees a prompt for it.
func TestFullAccessParentChangesChildPermissionsWithNoPrompt(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	setChatModeAsHuman(t, h, "mcp-tab", "mcp-chat", "bypass", "human-parent-full")
	ceilingSeedChat(t, h, "child-tab", "child-chat", "ask")

	mustSucceed := func(name string, arguments map[string]any) map[string]any {
		t.Helper()
		call := ceilingTool(t, h, name, arguments)
		if call.errorText() != "" {
			t.Fatalf("%s from a full-access parent = %#v", name, call.response)
		}
		if cards := raiseCards(h); len(cards) != 0 {
			t.Fatalf("%s from a full-access parent opened a card: %#v", name, cards)
		}
		return mapFromAnyMain(call.response["result"])
	}

	mustSucceed("workass_configure_chat", map[string]any{"operation_id": "full-raise-child", "tab_id": "child-tab", "chat_id": "child-chat", "mode_id": "bypass"})
	if mode := ceilingChatMode(t, h, "child-chat"); mode != "bypass" {
		t.Fatalf("raising the child left it on %q", mode)
	}
	mustSucceed("workass_configure_chat", map[string]any{"operation_id": "full-lower-child", "tab_id": "child-tab", "chat_id": "child-chat", "mode_id": "ask"})
	if mode := ceilingChatMode(t, h, "child-chat"); mode != "ask" {
		t.Fatalf("lowering the child left it on %q", mode)
	}
	created := mustSucceed("workass_create_chat", map[string]any{"operation_id": "full-create", "title": "Wide", "permission_intent": "full"})
	if fieldString(created, "modeId") != "bypass" {
		t.Fatalf("created = %#v", created)
	}
	subsession := mustSucceed("workass_create_subsession", map[string]any{"operation_id": "full-subsession", "title": "Wide child", "mode_id": "bypass"})
	if fieldString(subsession, "chatId") == "" {
		t.Fatalf("subsession = %#v", subsession)
	}
	mustSucceed("workass_send_chat_message", map[string]any{"operation_id": "full-send", "tab_id": fieldString(created, "tabId"), "chat_id": fieldString(created, "chatId"), "message": "go", "delivery": "queue"})
	spawned := mustSucceed("workass_spawn_subagent", map[string]any{"operation_id": "full-spawn", "task": "reply with the deterministic mock result", "label": "wide child", "permission_intent": "full"})
	if fieldString(spawned, "modeId") != "bypass" {
		t.Fatalf("spawned = %#v", spawned)
	}
}

// Lowering never needs a yes, whoever the caller is, and neither does a change
// that leaves the level where it was.
func TestLoweringAndKeepingALevelNeverAsk(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	ceilingSeedChat(t, h, "wide-tab", "wide-chat", "bypass")

	kept := ceilingTool(t, h, "workass_configure_chat", map[string]any{
		"operation_id": "keep-wide", "tab_id": "wide-tab", "chat_id": "wide-chat", "effort": "high",
	})
	if kept.errorText() != "" || len(raiseCards(h)) != 0 || ceilingChatMode(t, h, "wide-chat") != "bypass" {
		t.Fatalf("keeping a level = %#v, cards %d", kept.response, len(raiseCards(h)))
	}
	lowered := ceilingTool(t, h, "workass_configure_chat", map[string]any{
		"operation_id": "lower-wide", "tab_id": "wide-tab", "chat_id": "wide-chat", "mode_id": "ask",
	})
	if lowered.errorText() != "" || len(raiseCards(h)) != 0 || ceilingChatMode(t, h, "wide-chat") != "ask" {
		t.Fatalf("lowering a chat above the caller = %#v, cards %d", lowered.response, len(raiseCards(h)))
	}
}

func TestSpawningAboveTheCallerAsksTheHuman(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	for name, arguments := range map[string]map[string]any{
		"intent": {"operation_id": "spawn-up-intent", "task": "reply", "label": "wide", "permission_intent": "full"},
		"mode":   {"operation_id": "spawn-up-mode", "task": "reply", "label": "wide", "mode_id": "bypass"},
	} {
		done := ceilingToolAsync(h, "mcp-owner", "mcp-chat", "mcp-tab", "workass_spawn_subagent", arguments)
		answerRaiseCard(t, h, waitRaiseCard(t, h, done), permissionRaiseDeny)
		if call := finishCall(t, done); !strings.Contains(call.errorText(), permissionDeniedCode) {
			t.Fatalf("%s: a denied spawn = %#v", name, call.response)
		}
	}
	listed := ceilingTool(t, h, "workass_list_subagents", map[string]any{})
	if subagents := anySlice(mapFromAnyMain(listed.response["result"])["subagents"]); len(subagents) != 0 {
		t.Fatalf("a denied spawn still started a child: %#v", subagents)
	}
	inherit := ceilingTool(t, h, "workass_spawn_subagent", map[string]any{"operation_id": "spawn-same", "task": "reply with the deterministic mock result", "label": "same"})
	if inherit.errorText() != "" || len(raiseCards(h)) != 0 {
		t.Fatalf("inheriting the caller's level = %#v", inherit.response)
	}
}

// Fork children run at the fork point's mode, so that is what is being granted.
func TestForkPointAboveTheCallerAsksAndARunOfOneIsGated(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	ceilingSeedChat(t, h, "wide-tab", "wide-chat", "bypass")
	h.handler.agentControl.forks = newForkService(h.manager.StateDir(), h.manager, nil)
	if _, err := h.runtime.Select(context.Background(), acp.SessionOptions{
		TabID: "wide-tab", ChatID: "wide-chat", ProviderID: "mock", CWD: repoRoot(t), AgentOwnerKey: "wide-owner",
	}); err != nil {
		t.Fatal(err)
	}

	done := ceilingToolAsync(h, "mcp-owner", "mcp-chat", "mcp-tab", "workass_create_fork_point", map[string]any{
		"operation_id": "fork-up", "tab_id": "wide-tab", "chat_id": "wide-chat", "name": "wide point",
	})
	answerRaiseCard(t, h, waitRaiseCard(t, h, done), permissionRaiseDeny)
	if call := finishCall(t, done); !strings.Contains(call.errorText(), permissionDeniedCode) {
		t.Fatalf("a denied fork point = %#v", call.response)
	}

	// A narrowed forwarded command cannot reach a wider fork point's children.
	h.handler.agentControl.forks = fleetSeedWideForkPoint(t, h, "mcp-tab", "mcp-chat", "bypass")
	_, err := h.handler.agentControl.callAsControllerFor(context.Background(), wire.Caller{Peer: "m-away"}, map[string]any{
		"v": 1, "method": "fork.run", "limit": "read",
		"params": map[string]any{
			"fork_point_id": "fp-1793316037099-1", "operation_id": "fork-run-up",
			"children": []any{map[string]any{"label": "c", "message": "go"}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), permissionCeilingCode) {
		t.Fatalf("a read-limited fork.run over a full fork point = %v", err)
	}
}

// Every forwarded command answers to the stated limit on the machine it reaches.
func TestForwardedCommandsAnswerToTheStatedLimit(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	control := h.handler.agentControl
	control.selfMachineID = "m-home"
	ceilingSeedChat(t, h, "wide-tab", "wide-chat", "bypass")
	ceilingSeedChat(t, h, "peer-tab", "peer-chat", "ask")
	origin := map[string]any{"machine": "m-away", "chat_id": "asking-chat", "tab_id": "asking-tab"}
	forward := func(method, limit string, params map[string]any) (any, error) {
		return control.callAsControllerFor(context.Background(), wire.Caller{Peer: "m-away"}, map[string]any{
			"v": 1, "method": method, "limit": limit, "origin": origin, "params": params,
		})
	}

	for name, attempt := range map[string]struct {
		method string
		params map[string]any
	}{
		"create":            {"chat.create", map[string]any{"operation_id": "fwd-create", "title": "x", "cwd": repoRoot(t), "mode_id": "bypass"}},
		"create intent":     {"chat.create", map[string]any{"operation_id": "fwd-create-intent", "title": "x", "cwd": repoRoot(t), "provider_id": "mock", "permission_intent": "full"}},
		"configure":         {"chat.configure", map[string]any{"operation_id": "fwd-configure", "tab_id": "peer-tab", "chat_id": "peer-chat", "mode_id": "bypass"}},
		"send":              {"chat.send", map[string]any{"operation_id": "fwd-send", "tab_id": "wide-tab", "chat_id": "wide-chat", "message": "go", "delivery": "queue"}},
		"spawn in a chat":   {"agent.spawn", map[string]any{"operation_id": "fwd-spawn", "tab_id": "mcp-tab", "chat_id": "mcp-chat", "prompt": "go", "mode_id": "bypass"}},
		"spawn hosted mode": {"agent.spawn", map[string]any{"operation_id": "fwd-hosted", "prompt": "go", "provider_id": "mock", "mode_id": "bypass", "cwd": repoRoot(t)}},
		"process.start":     {"process.start", map[string]any{"operation_id": "fwd-process", "label": "x", "cwd": repoRoot(t), "command": []any{"true"}}},
	} {
		if _, err := forward(attempt.method, "read", attempt.params); err == nil || !strings.Contains(err.Error(), "full") {
			t.Fatalf("%s under a read limit = %v", name, err)
		} else if attempt.method != "process.start" && !strings.Contains(err.Error(), "ermission") {
			t.Fatalf("%s under a read limit was refused for another reason: %v", name, err)
		}
	}
	if state, _ := h.runtime.Snapshot("peer-chat"); state.Presentation.CurrentModeID != "ask" {
		t.Fatalf("a refused configure changed the chat to %q", state.Presentation.CurrentModeID)
	}
	if state, _ := h.runtime.Snapshot("wide-chat"); len(state.Queue) != 0 || state.Foreground != nil {
		t.Fatal("a refused send reached the wider chat")
	}

	// Inside the limit nothing changes, and a command stating no mode starts at it.
	if _, err := forward("chat.send", "read", map[string]any{"operation_id": "fwd-send-peer", "tab_id": "peer-tab", "chat_id": "peer-chat", "message": "go", "delivery": "queue"}); err != nil {
		t.Fatalf("a same-level forwarded send = %v", err)
	}
	if _, err := forward("chat.configure", "read", map[string]any{"operation_id": "fwd-lower", "tab_id": "wide-tab", "chat_id": "wide-chat", "mode_id": "ask"}); err != nil {
		t.Fatalf("a forwarded lowering = %v", err)
	}
	if _, err := forward("chat.configure", "full", map[string]any{"operation_id": "fwd-full", "tab_id": "peer-tab", "chat_id": "peer-chat", "mode_id": "bypass"}); err != nil {
		t.Fatalf("a full-limit forwarded raise = %v", err)
	}
	// A peer that states nothing is still a peer that predates the envelope.
	if _, err := forward("chat.configure", "", map[string]any{"operation_id": "fwd-unstated", "tab_id": "wide-tab", "chat_id": "wide-chat", "mode_id": "bypass"}); err != nil {
		t.Fatalf("an unstated limit = %v", err)
	}
}

// The renderer relay forwards params and has no envelope, so the limit a caller
// stated rides inside them. Dropping it there made every relayed command full.
func TestALimitThatCameOverTheRendererRelayStillBinds(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	ceilingSeedChat(t, h, "peer-tab", "peer-chat", "ask")
	_, err := h.handler.agentControl.callAsControllerFor(context.Background(), wire.Caller{Peer: "m-away"}, map[string]any{
		"method": "chat.configure",
		"params": map[string]any{
			"operation_id": "relay-configure", "tab_id": "peer-tab", "chat_id": "peer-chat", "mode_id": "bypass",
			fleetLimitParam: "read",
		},
	})
	if err == nil || !strings.Contains(err.Error(), permissionCeilingCode) {
		t.Fatalf("a relayed read-limited raise = %v", err)
	}
}

func TestAForwardedDecisionCannotAllowBelowFullAccess(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	setChatModeAsHuman(t, h, "mcp-tab", "mcp-chat", "bypass", "human-parent-full-decide")
	_, err := h.handler.agentControl.callAsControllerFor(context.Background(), wire.Caller{Peer: "m-away"}, map[string]any{
		"v": 1, "method": "agent.decide_permission", "limit": "read",
		"params": map[string]any{"operation_id": "fwd-decide", "tab_id": "mcp-tab", "chat_id": "mcp-chat", "subagent_id": "wa-subagent-1", "decision": "allow"},
	})
	if err == nil || !strings.Contains(err.Error(), permissionCeilingCode) {
		t.Fatalf("a read-limited allow = %v", err)
	}
}

// The edge states the real level. `ask` translated to no intent at all, which
// the home read as "unstated", which is full.
func TestAForwardedCommandStatesTheCallersRealLevel(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	control := h.handler.agentControl
	remote := &stubAgentChatRemoteRouter{result: map[string]any{"ok": true}}
	control.remoteChats = remote

	call := ceilingTool(t, h, "workass_configure_chat", map[string]any{
		"operation_id": "edge-limit", "tab_id": "M~m-work~tab", "chat_id": "M~m-work~chat", "mode_id": "bypass",
	})
	if call.errorText() != "" {
		t.Fatalf("forward = %#v", call.response)
	}
	if got := fieldString(remote.params, fleetLimitParam); got != fleetLimitRead {
		t.Fatalf("an ask chat stated limit %q", got)
	}

	setChatModeAsHuman(t, h, "mcp-tab", "mcp-chat", "bypass", "human-edge-full")
	call = ceilingTool(t, h, "workass_configure_chat", map[string]any{
		"operation_id": "edge-limit-full", "tab_id": "M~m-work~tab", "chat_id": "M~m-work~chat", "mode_id": "bypass",
	})
	if call.errorText() != "" {
		t.Fatalf("forward = %#v", call.response)
	}
	if got := fieldString(remote.params, fleetLimitParam); got != fleetLimitFull {
		t.Fatalf("a full-access chat stated limit %q", got)
	}
}

// chat.send to another machine keeps a renderer-native route with no envelope at
// all, so the sender's own machine reads the target and asks there.
func TestARemoteSendIntoAHigherChatIsGatedAtTheCaller(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	remote := &remoteSendRouter{listed: []any{map[string]any{
		"tabId": "M~m-work~tab", "chatId": "M~m-work~chat", "providerId": "mock", "modeId": "bypass", "title": "Wide there",
	}}}
	h.handler.agentControl.remoteChats = remote
	arguments := func(op string) map[string]any {
		return map[string]any{"operation_id": op, "tab_id": "M~m-work~tab", "chat_id": "M~m-work~chat", "message": "go", "delivery": "queue"}
	}

	denied := ceilingToolAsync(h, "mcp-owner", "mcp-chat", "mcp-tab", "workass_send_chat_message", arguments("remote-send-denied"))
	answerRaiseCard(t, h, waitRaiseCard(t, h, denied), permissionRaiseDeny)
	if call := finishCall(t, denied); !strings.Contains(call.errorText(), permissionDeniedCode) {
		t.Fatalf("a denied remote send = %#v", call.response)
	}
	if remote.sent != nil {
		t.Fatalf("a denied remote send was forwarded: %#v", remote.sent)
	}

	allowed := ceilingToolAsync(h, "mcp-owner", "mcp-chat", "mcp-tab", "workass_send_chat_message", arguments("remote-send-allowed"))
	answerRaiseCard(t, h, waitRaiseCard(t, h, allowed), permissionRaiseAllow)
	if call := finishCall(t, allowed); call.errorText() != "" {
		t.Fatalf("an approved remote send = %#v", call.response)
	}
	// What the human approved is what the home is told, so it does not refuse it.
	if fieldString(remote.sent, fleetLimitParam) != fleetLimitFull {
		t.Fatalf("an approved remote send stated %#v", remote.sent)
	}
}

type remoteSendRouter struct {
	listed []any
	sent   map[string]any
}

func (r *remoteSendRouter) Call(_ context.Context, method string, params map[string]any) (any, error) {
	if method == "chat.list" {
		return map[string]any{"chats": r.listed}, nil
	}
	r.sent = copyAnyMap(params)
	return map[string]any{"ok": true}, nil
}

// A delegated agent's tools act FOR its parent chat, so reading the parent's
// level would hand a narrowed child its parent's full access. It answers to its
// own, and it has no card to ask with.
func TestASubagentAnswersToItsOwnLevelNotItsParents(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	setChatModeAsHuman(t, h, "mcp-tab", "mcp-chat", "bypass", "human-parent-full-child")
	ceilingSeedChat(t, h, "wide-tab", "wide-chat", "bypass")
	ceilingSeedChat(t, h, "peer-tab", "peer-chat", "ask")
	spawned := ceilingTool(t, h, "workass_spawn_subagent", map[string]any{
		"operation_id": "narrow-child", "task": "reply with the deterministic mock result", "label": "narrow", "mode_id": "ask",
	})
	childID := fieldString(mapFromAnyMain(spawned.response["result"]), "id")
	if spawned.errorText() != "" || childID == "" {
		t.Fatalf("spawn = %#v", spawned.response)
	}
	asChild := func(name string, arguments map[string]any) ceilingCall {
		t.Helper()
		var call ceilingCall
		childChat := subagentTabPrefix + childID
		if _, err := h.manager.WithActorOwner(childChat, childChat, func(owner string) (any, error) {
			call = <-ceilingToolAsync(h, owner, childChat, childChat, name, arguments)
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		return call
	}
	for name, arguments := range map[string]map[string]any{
		"workass_create_chat":       {"operation_id": "child-create", "title": "x", "mode_id": "bypass"},
		"workass_configure_chat":    {"operation_id": "child-configure", "tab_id": "peer-tab", "chat_id": "peer-chat", "mode_id": "bypass"},
		"workass_send_chat_message": {"operation_id": "child-send", "tab_id": "wide-tab", "chat_id": "wide-chat", "message": "go", "delivery": "queue"},
		"workass_create_subsession": {"operation_id": "child-fork", "title": "x", "fork": true},
	} {
		call := asChild(name, arguments)
		if !strings.Contains(call.errorText(), permissionCeilingCode) || !strings.Contains(call.errorText(), "parent") {
			t.Fatalf("%s from a narrowed child = %#v", name, call.response)
		}
	}
	if cards := raiseCards(h); len(cards) != 0 {
		t.Fatalf("a child opened a human card: %#v", cards)
	}

	// Unstated, a child's new chat starts at the child's own level.
	created := asChild("workass_create_chat", map[string]any{"operation_id": "child-create-plain", "title": "x"})
	result := mapFromAnyMain(created.response["result"])
	if created.errorText() != "" || fieldString(result, "modeId") != "ask" {
		t.Fatalf("a child's plain create = %#v", created.response)
	}
}

// With no running turn there is nowhere to show a card, and the refusal says
// exactly what to ask for instead of leaving the agent at a dead end.
func TestWithNoTurnToShowACardTheRefusalSaysWhatToAsk(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	ceilingSeedChat(t, h, "idle-tab", "idle-chat", "ask")
	ceilingSeedChat(t, h, "peer-tab", "peer-chat", "ask")
	var call ceilingCall
	if _, err := h.manager.WithActorOwner("idle-chat", "idle-tab", func(owner string) (any, error) {
		call = <-ceilingToolAsync(h, owner, "idle-chat", "idle-tab", "workass_configure_chat", map[string]any{
			"operation_id": "idle-raise", "tab_id": "peer-tab", "chat_id": "peer-chat", "mode_id": "bypass",
		})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	text := call.errorText()
	if !strings.Contains(text, permissionCeilingCode) || !strings.Contains(text, "Ask the user") || !strings.Contains(text, "bypass") {
		t.Fatalf("a refusal with no card to show = %q", text)
	}
	if mode := ceilingChatMode(t, h, "peer-chat"); mode != "ask" {
		t.Fatalf("the refused chat is on %q", mode)
	}
}

// The owner's own action reaches the actor through the renderer's channels, and
// nothing here stands in the way of a click on any chat.
func TestAHumanActionIsNeverCapped(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	ceilingSeedChat(t, h, "peer-tab", "peer-chat", "ask")
	setChatModeAsHuman(t, h, "peer-tab", "peer-chat", "bypass", "human-click")
	if mode := ceilingChatMode(t, h, "peer-chat"); mode != "bypass" {
		t.Fatalf("a human raise left the chat on %q", mode)
	}
	if _, err := h.runtime.Start(context.Background(), map[string]any{
		"tabId": "peer-tab", "chatId": "peer-chat", "providerId": "mock", "cwd": repoRoot(t),
		"prompt": "typed by the owner", "userMessageId": "human-typed", "assistantMessageId": "human-typed-reply",
	}, "human"); err != nil {
		t.Fatalf("the owner typing into a wider chat = %v", err)
	}
	if cards := raiseCards(h); len(cards) != 0 {
		t.Fatalf("a human action opened a card: %#v", cards)
	}
}

// fleetSeedWideForkPoint writes one ready fork point at a stated mode.
func fleetSeedWideForkPoint(t *testing.T, h statelessMCPTestHarness, tabID, chatID, modeID string) *acp.ForkService {
	t.Helper()
	dir := filepath.Join(h.manager.StateDir(), "forks")
	fleetSeedForkState(t, h.manager.StateDir(), tabID, chatID)
	path := filepath.Join(dir, chatID+".json")
	rewriteForkPointMode(t, path, modeID)
	return newForkService(h.manager.StateDir(), h.manager, nil)
}

func rewriteForkPointMode(t *testing.T, path, modeID string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	mapFromAnyMain(anySlice(state["points"])[0])["modeId"] = modeID
	raw, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The whole road, over a real daemon link: the level a chat holds is the limit
// the other machine holds it to, and the owner's click is what changes that.
func TestAJobOnAnotherMachineTakesAChatAtFullAccessOverARealLink(t *testing.T) {
	fleet := newHostedJobFleet(t)
	setChatModeAsHuman(t, fleet.asking, "mcp-tab", "mcp-chat", "ask", "owner-lowers-to-ask")
	arguments := map[string]any{
		"machine_id": "m-home", "operation_id": "job-from-ask", "label": "from ask",
		"cwd": repoRoot(t), "command": []any{"true"},
	}
	status, response := fleet.asking.request(t, http.MethodPost, map[string]any{"name": "workass_start_process_child", "arguments": arguments})
	if status == http.StatusOK && response["error"] == nil {
		t.Fatalf("an ask chat started a job on another machine: %#v", response)
	}
	if text := fieldString(response, "error"); !strings.Contains(text, "process.start") || !strings.Contains(text, "workass_configure_chat") {
		t.Fatalf("the refusal does not say what to do: %q", text)
	}

	setChatModeAsHuman(t, fleet.asking, "mcp-tab", "mcp-chat", "bypass", "owner-raises-to-full")
	if started := fleet.start(t, map[string]any{
		"operation_id": "job-from-full", "label": "from full", "cwd": repoRoot(t), "command": []any{"true"},
	}); started["ok"] != true {
		t.Fatalf("a full-access chat's job = %#v", started)
	}
}
