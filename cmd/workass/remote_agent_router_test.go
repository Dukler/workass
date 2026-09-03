package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

type stubAgentChatRemoteRouter struct {
	method string
	params map[string]any
	result any
	err    error
}

func (s *stubAgentChatRemoteRouter) Call(_ context.Context, method string, params map[string]any) (any, error) {
	s.method = method
	s.params = copyAnyMap(params)
	return s.result, s.err
}

func TestRendererAgentRouterRoundTripKeepsOwnerCapabilityOutOfRenderer(t *testing.T) {
	router := &rendererAgentRouter{pending: make(map[string]chan remoteAgentRouteReply), timeout: time.Second}
	router.broadcast = func(channel string, raw any) int {
		if channel != remoteAgentRouteRequestChannel {
			t.Fatalf("route channel = %q", channel)
		}
		request := mapFromAnyMain(raw)
		params := mapFromAnyMain(request["params"])
		if params["owner_key"] != nil || params["parent_chat_id"] != nil || params["parent_tab_id"] != nil {
			t.Fatalf("owner capability crossed into renderer params: %#v", params)
		}
		go func() {
			_, _ = router.resolve([]any{map[string]any{
				"requestId": request["requestId"],
				"result":    map[string]any{"ok": true, "machineId": "m-remote"},
			}})
		}()
		return 1
	}
	result, err := router.Call(context.Background(), "chat.read", map[string]any{
		"owner_key": "secret-owner", "parent_chat_id": "parent-chat", "parent_tab_id": "parent-tab",
		"tab_id": "M~m-remote~tab-1", "chat_id": "M~m-remote~chat-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := fieldString(mapFromAnyMain(result), "machineId"); got != "m-remote" {
		t.Fatalf("route result machine = %q", got)
	}
}

func TestRendererAgentRouterFailsImmediatelyWithoutLocalRenderer(t *testing.T) {
	router := &rendererAgentRouter{
		pending: make(map[string]chan remoteAgentRouteReply), timeout: time.Hour,
		broadcast: func(string, any) int { return 0 },
	}
	started := time.Now()
	_, err := router.Call(context.Background(), "chat.list", nil)
	if err == nil || !strings.Contains(err.Error(), "no local renderer") {
		t.Fatalf("missing renderer error = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("missing renderer waited for the route timeout")
	}
}

func TestRemoteAgentChatTargetRequiresOneExactMachine(t *testing.T) {
	machineID, remote, err := remoteAgentChatTarget(map[string]any{
		"tab_id": "M~m-san~tab-1", "chat_id": "M~m-san~chat-1",
	})
	if err != nil || !remote || machineID != "m-san" {
		t.Fatalf("valid remote target = machine=%q remote=%v err=%v", machineID, remote, err)
	}
	for _, target := range []map[string]any{
		{"tab_id": "M~m-san~tab-1", "chat_id": "chat-1"},
		{"tab_id": "M~m-san~tab-1", "chat_id": "M~m-other~chat-1"},
		{"tab_id": "M~broken", "chat_id": "M~broken"},
	} {
		if _, _, err := remoteAgentChatTarget(target); err == nil {
			t.Fatalf("unsafe target was accepted: %#v", target)
		}
	}
}

func TestMergeRemoteAgentChatListAdmitsOnlyTaggedExactPairs(t *testing.T) {
	local := map[string]any{"chats": []any{map[string]any{"tabId": "local-tab", "chatId": "local-chat"}}}
	merged := mergeRemoteAgentChatList(local, map[string]any{"chats": []any{
		map[string]any{"tabId": "M~m-san~tab-1", "chatId": "M~m-san~chat-1", "title": "hello"},
		map[string]any{"tabId": "M~m-san~tab-2", "chatId": "M~m-other~chat-2", "title": "wrong machine"},
		map[string]any{"tabId": "plain-tab", "chatId": "plain-chat", "title": "untagged"},
	}})
	chats := anySlice(merged["chats"])
	if len(chats) != 2 {
		t.Fatalf("merged chats = %#v", chats)
	}
	remote := mapFromAnyMain(chats[1])
	if fieldString(remote, "machineId") != "m-san" || fieldString(remote, "title") != "hello" {
		t.Fatalf("remote chat = %#v", remote)
	}
}

func TestStatelessMCPRoutesTaggedRemoteReadWithoutExposingOwner(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	remote := &stubAgentChatRemoteRouter{result: map[string]any{
		"tabId": "M~m-san~tab-hello", "chatId": "M~m-san~chat-hello",
		"machineId": "m-san", "messages": []any{}, "running": false,
	}}
	harness.handler.agentControl.remoteChats = remote
	status, response := harness.request(t, 501, "tools/call", "workass_read_chat", statelessMCPProtocolVersion, map[string]any{
		"name": "workass_read_chat",
		"arguments": map[string]any{
			"tab_id": "M~m-san~tab-hello", "chat_id": "M~m-san~chat-hello", "limit": 10,
		},
	})
	result := mapFromAnyMain(response["result"])
	if status != http.StatusOK || result["isError"] == true {
		t.Fatalf("remote read status=%d response=%#v", status, response)
	}
	content := result["content"].([]any)
	var read map[string]any
	if err := json.Unmarshal([]byte(toString(mapFromAnyMain(content[0])["text"])), &read); err != nil {
		t.Fatal(err)
	}
	if remote.method != "chat.read" || fieldString(remote.params, "machine_id") != "m-san" || read["machineId"] != "m-san" {
		t.Fatalf("remote route method=%q params=%#v read=%#v", remote.method, remote.params, read)
	}
	if remote.params["owner_key"] != nil || remote.params["parent_chat_id"] != nil || remote.params["parent_tab_id"] != nil {
		t.Fatalf("owner capability reached remote renderer: %#v", remote.params)
	}
}
