package main

import (
	"net/http"
	"testing"
	"workass/internal/wire"
)

func TestChatDiagnosticsExactActorAndNoMutation(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	_, err := h.runtime.CreateRendererChat(map[string]any{"tabId": "idle-tab", "chatId": "idle-chat", "operationId": "create-diagnostics-idle"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := h.runtime.Snapshot("idle-chat")
	hub := wire.NewHub()
	registerChatDiagnosticsWire(hub, h.runtime)
	raw, err := hub.Invoke("chat:turn-diagnostics", []any{map[string]any{"tab_id": "idle-tab", "chat_id": "idle-chat", "limit": 2}})
	result := mapFromAnyMain(raw)
	if err != nil || result["available"] != false || result["actor"] == nil {
		t.Fatalf("%v %#v", err, result)
	}
	after, _ := h.runtime.Snapshot("idle-chat")
	if after.Revision != before.Revision {
		t.Fatal("diagnostics mutated the actor")
	}
	for _, params := range []map[string]any{
		{"tab_id": "wrong", "chat_id": "mcp-chat"},
		{"tab_id": "mcp-tab", "chat_id": "unknown-chat"},
		{"tab_id": "mcp-tab", "chat_id": "mcp-chat", "limit": 21},
		{"tab_id": "mcp-tab", "chat_id": "mcp-chat", "limit": 1.5},
	} {
		if _, err := h.runtime.TurnDiagnostics(params); err == nil {
			t.Fatalf("accepted invalid target/limit: %v", params)
		}
	}
}

func TestChatDiagnosticsToolRemoteRoute(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	remote := &stubAgentChatRemoteRouter{result: map[string]any{"machineId": "m-san", "turns": []any{map[string]any{"phase": "waiting_for_provider_activity"}}}}
	h.handler.agentControl.remoteChats = remote
	status, response := h.request(t, http.MethodPost, map[string]any{"name": "workass_get_chat_diagnostics", "arguments": map[string]any{"tab_id": "M~m-san~tab", "chat_id": "M~m-san~chat", "limit": 3}})
	if status != http.StatusOK || response["error"] != nil || remote.method != "chat.diagnostics" || remote.params["machine_id"] != "m-san" {
		t.Fatalf("%d %#v %s", status, response, remote.method)
	}
	if remote.params["owner_key"] != nil || remote.params["parent_chat_id"] != nil {
		t.Fatal("owner capability crossed remote route")
	}
}
