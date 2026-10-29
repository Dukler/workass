package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var agentControlCaseMethod = regexp.MustCompile(`"([a-z_]+\.[a-z_]+)"`)

// Every Agent API method must have an explicit remote verdict. A new tool that
// forgets one used to answer "remote-chat MCP method is not supported" on a
// chat the same agent could already read, which is how workass_configure_chat
// became unreachable on a fleet machine.
func TestEveryAgentControlMethodHasARemoteVerdict(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("agent_control.go"))
	if err != nil {
		t.Fatal(err)
	}
	methods := map[string]struct{}{}
	for _, line := range strings.Split(string(source), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "case \"") {
			continue
		}
		for _, match := range agentControlCaseMethod.FindAllStringSubmatch(trimmed, -1) {
			methods[match[1]] = struct{}{}
		}
	}
	if len(methods) < 30 {
		t.Fatalf("only %d agent control methods were discovered; the scan is wrong", len(methods))
	}
	for method := range methods {
		if method == "chat.list" {
			// chat.list is not an addressed call: it always answers locally and
			// merges in whatever the mounted machines report.
			continue
		}
		if strings.HasPrefix(method, "update.") {
			// update.* keeps its own older machine_id contract and its own
			// local/remote split; see remoteAgentMethodKinds.
			continue
		}
		if _, classified := remoteAgentMethodKinds[method]; !classified {
			t.Errorf("agent control method %q has no entry in remoteAgentMethodKinds", method)
		}
	}
	for method, kind := range remoteAgentMethodKinds {
		if _, exists := methods[method]; !exists {
			t.Errorf("remoteAgentMethodKinds names %q, which agent control does not handle", method)
		}
		if kind != remoteAgentLocalOnly {
			continue
		}
		if strings.TrimSpace(remoteAgentLocalOnlyReasons[method]) == "" {
			t.Errorf("local-only method %q has no reason to report", method)
		}
	}
}

func TestRemoteAgentMethodTargetResolvesEveryClass(t *testing.T) {
	for _, item := range []struct {
		name    string
		method  string
		params  map[string]any
		machine string
		remote  bool
	}{
		{"configure a remote chat", "chat.configure", map[string]any{"tab_id": "M~m-duklerver~tab-1", "chat_id": "M~m-duklerver~chat-1"}, "m-duklerver", true},
		{"configure a local chat", "chat.configure", map[string]any{"tab_id": "tab-1", "chat_id": "chat-1"}, "", false},
		{"create on a named machine", "chat.create", map[string]any{"machine_id": "m-duklerver"}, "m-duklerver", true},
		{"create here", "chat.create", map[string]any{}, "", false},
		{"catalog of a named machine", "agent.catalog", map[string]any{"machine_id": "m-duklerver"}, "m-duklerver", true},
		{"spawn into a remote chat", "agent.spawn", map[string]any{"tab_id": "M~m-duklerver~tab-1", "chat_id": "M~m-duklerver~chat-1"}, "m-duklerver", true},
		{"spawn here", "agent.spawn", map[string]any{}, "", false},
		{"run a remote fork", "fork.run", map[string]any{"fork_point_id": "M~m-duklerver~fp-1"}, "m-duklerver", true},
		{"cancel a remote fork child", "fork.cancel", map[string]any{"run_id": "M~m-duklerver~run-1", "child_id": "M~m-duklerver~child-1"}, "m-duklerver", true},
	} {
		machine, remote, err := remoteAgentMethodTarget(item.method, item.params)
		if err != nil || remote != item.remote || machine != item.machine {
			t.Errorf("%s: machine=%q remote=%v err=%v", item.name, machine, remote, err)
		}
	}
}

func TestRemoteAgentMethodTargetRefusesAmbiguousAddressing(t *testing.T) {
	for _, item := range []struct {
		name     string
		method   string
		params   map[string]any
		contains string
	}{
		{"half-tagged pair", "chat.configure", map[string]any{"tab_id": "M~m-duklerver~tab-1", "chat_id": "chat-1"}, "same machine"},
		{"two machines", "chat.delete", map[string]any{"tab_id": "M~m-a~tab-1", "chat_id": "M~m-b~chat-1"}, "same machine"},
		{"machine_id disagrees", "chat.rename", map[string]any{"tab_id": "M~m-a~tab-1", "chat_id": "M~m-a~chat-1", "machine_id": "m-b"}, "does not match"},
		{"machine_id instead of a pair", "chat.settle", map[string]any{"tab_id": "tab-1", "chat_id": "chat-1", "machine_id": "m-a"}, "machine-tagged tab_id and chat_id"},
		{"remote subagent id without its chat", "agent.wait", map[string]any{"id": "M~m-a~sub-1"}, "also needs the owning chat"},
		{"tagged id on a machine-scoped method", "chat.create", map[string]any{"tab_id": "M~m-a~tab-1", "chat_id": "M~m-a~chat-1"}, "takes machine_id"},
	} {
		if _, _, err := remoteAgentMethodTarget(item.method, item.params); err == nil || !strings.Contains(err.Error(), item.contains) {
			t.Errorf("%s: err=%v", item.name, err)
		}
	}
}

// A tool that genuinely has no remote form must say which one it is and what to
// do instead. "not supported" told an agent nothing it could act on.
func TestLocalOnlyAgentToolsReportTheirOwnReason(t *testing.T) {
	for method, kind := range remoteAgentMethodKinds {
		if kind != remoteAgentLocalOnly {
			continue
		}
		_, remote, err := remoteAgentMethodTarget(method, map[string]any{"machine_id": "m-duklerver"})
		if err == nil || remote {
			t.Fatalf("%s accepted a machine target: remote=%v err=%v", method, remote, err)
		}
		if err.Error() != remoteAgentLocalOnlyReasons[method] {
			t.Errorf("%s reason = %q", method, err.Error())
		}
		if _, _, err := remoteAgentMethodTarget(method, map[string]any{}); err != nil {
			t.Errorf("%s rejected an ordinary local call: %v", method, err)
		}
	}
}

func TestRemoteAgentSubagentPairOnlyNamesTheCallingChatLocally(t *testing.T) {
	tabID, chatID, err := remoteAgentSubagentPair(map[string]any{}, "tab-1", "chat-1")
	if err != nil || tabID != "tab-1" || chatID != "chat-1" {
		t.Fatalf("implicit pair = %q/%q err=%v", tabID, chatID, err)
	}
	if _, _, err := remoteAgentSubagentPair(map[string]any{"tab_id": "tab-1", "chat_id": "chat-1"}, "tab-1", "chat-1"); err != nil {
		t.Fatalf("explicit calling pair: %v", err)
	}
	for _, params := range []map[string]any{
		{"tab_id": "tab-2", "chat_id": "chat-2"},
		{"tab_id": "tab-1"},
		{"chat_id": "chat-1"},
	} {
		if _, _, err := remoteAgentSubagentPair(params, "tab-1", "chat-1"); err == nil {
			t.Errorf("sibling or half pair was accepted: %#v", params)
		}
	}
}

func TestClampControllerWaitKeepsTheOrderedLaneMoving(t *testing.T) {
	for _, method := range []string{"agent.wait", "agent.wait_many", "fork.wait"} {
		params := map[string]any{"timeout_ms": 3600000}
		clampControllerWait(method, params)
		if intField(params, "timeout_ms") != remoteAgentWaitPollMS {
			t.Errorf("%s hour-long wait = %v", method, params["timeout_ms"])
		}
		eventOnly := map[string]any{"timeout_ms": -1}
		clampControllerWait(method, eventOnly)
		if intField(eventOnly, "timeout_ms") != remoteAgentWaitPollMS {
			t.Errorf("%s event-only wait = %v", method, eventOnly["timeout_ms"])
		}
		short := map[string]any{"timeout_ms": 1000}
		clampControllerWait(method, short)
		if intField(short, "timeout_ms") != 1000 {
			t.Errorf("%s short wait was raised to %v", method, short["timeout_ms"])
		}
	}
	untouched := map[string]any{"timeout_ms": 3600000}
	clampControllerWait("chat.read", untouched)
	if intField(untouched, "timeout_ms") != 3600000 {
		t.Fatalf("a non-wait method was clamped: %v", untouched["timeout_ms"])
	}
}

func TestConfigureChatRoutesToTheOwningMachineWithoutTheOwnerCapability(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	remote := &stubAgentChatRemoteRouter{result: map[string]any{"ok": true, "machineId": "m-duklerver"}}
	harness.handler.agentControl.remoteChats = remote
	status, response := harness.request(t, http.MethodPost, map[string]any{
		"name": "workass_configure_chat",
		"arguments": map[string]any{
			"tab_id": "M~m-duklerver~tab-codex", "chat_id": "M~m-duklerver~chat-codex",
			"model_id": "mock-deterministic", "permission_intent": "edit", "operation_id": "configure-duklerver-1",
		},
	})
	if status != http.StatusOK || response["error"] != nil {
		t.Fatalf("remote configure status=%d response=%#v", status, response)
	}
	if remote.method != "chat.configure" || fieldString(remote.params, "machine_id") != "m-duklerver" {
		t.Fatalf("remote configure route method=%q params=%#v", remote.method, remote.params)
	}
	if fieldString(remote.params, "permission_intent") != "edit" || fieldString(remote.params, "operation_id") != "configure-duklerver-1" {
		t.Fatalf("remote configure lost its request: %#v", remote.params)
	}
	if remote.params["owner_key"] != nil || remote.params["parent_chat_id"] != nil || remote.params["parent_tab_id"] != nil {
		t.Fatalf("owner capability reached the renderer route: %#v", remote.params)
	}
}

func TestCreateChatOnANamedMachineRoutesAndSelfTargetStaysLocal(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	remote := &stubAgentChatRemoteRouter{result: map[string]any{"ok": true, "tabId": "M~m-duklerver~tab-new", "chatId": "M~m-duklerver~chat-new"}}
	harness.handler.agentControl.remoteChats = remote
	harness.handler.agentControl.selfMachineID = "m-this-mac"

	status, response := harness.request(t, http.MethodPost, map[string]any{
		"name": "workass_create_chat",
		"arguments": map[string]any{
			"operation_id": "create-duklerver-1", "title": "remote lane", "machine_id": "m-duklerver",
		},
	})
	if status != http.StatusOK || response["error"] != nil || remote.method != "chat.create" {
		t.Fatalf("remote create status=%d response=%#v method=%q", status, response, remote.method)
	}

	remote.method = ""
	status, response = harness.request(t, http.MethodPost, map[string]any{
		"name": "workass_create_chat",
		"arguments": map[string]any{
			"operation_id": "create-here-1", "title": "local lane", "machine_id": "m-this-mac",
		},
	})
	if status != http.StatusOK || response["error"] != nil {
		t.Fatalf("self-targeted create status=%d response=%#v", status, response)
	}
	if remote.method != "" {
		t.Fatalf("this machine's own id was routed away as %q", remote.method)
	}
	if fieldString(mapFromAnyMain(response["result"]), "chatId") == "" {
		t.Fatalf("self-targeted create result = %#v", response["result"])
	}
}

func TestRemoteSendRefusesAttachmentsFromAnotherMachine(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	harness.handler.agentControl.remoteChats = &stubAgentChatRemoteRouter{result: map[string]any{"ok": true}}
	_, response := harness.request(t, http.MethodPost, map[string]any{
		"name": "workass_send_chat_message",
		"arguments": map[string]any{
			"tab_id": "M~m-duklerver~tab-1", "chat_id": "M~m-duklerver~chat-1",
			"message": "look at this", "attachments": []any{"/Users/dukler/shot.png"}, "operation_id": "remote-send-1",
		},
	})
	if message := toString(response["error"]); !strings.Contains(message, "machine that runs the chat") {
		t.Fatalf("remote attachment error = %q", message)
	}
}

// The point of the controller channel: a call arriving from another machine runs
// the SAME coordinator, so remote validation cannot drift from local validation.
func TestControllerAgentControlAppliesTheLocalValidation(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	control := harness.handler.agentControl
	ctx := context.Background()

	if _, err := control.callAsController(ctx, map[string]any{
		"method": "chat.configure",
		"params": map[string]any{
			"tab_id": "mcp-tab", "chat_id": "mcp-chat", "model_id": "no-such-model", "operation_id": "controller-bad-model",
		},
	}); err == nil || !strings.Contains(err.Error(), "workass_agent_catalog") {
		t.Fatalf("controller configure accepted an unknown model: %v", err)
	}

	result, err := control.callAsController(ctx, map[string]any{
		"method": "chat.configure",
		"params": map[string]any{
			"tab_id": "mcp-tab", "chat_id": "mcp-chat", "permission_intent": "full", "operation_id": "controller-configure-1",
		},
	})
	if err != nil {
		t.Fatalf("controller configure: %v", err)
	}
	configured := mapFromAnyMain(result)
	if configured["ok"] != true || fieldString(configured, "chatId") != "mcp-chat" || fieldString(configured, "modeId") == "" {
		t.Fatalf("controller configure result = %#v", configured)
	}

	if _, err := control.callAsController(ctx, map[string]any{
		"method": "chat.configure",
		"params": map[string]any{"tab_id": "M~m-other~tab-1", "chat_id": "M~m-other~chat-1", "operation_id": "controller-tagged"},
	}); err == nil || !strings.Contains(err.Error(), "machine-tagged") {
		t.Fatalf("controller accepted a foreign tagged id: %v", err)
	}

	if _, err := control.callAsController(ctx, map[string]any{
		"method": "html.render", "params": map[string]any{"title": "page", "html": "<p>x</p>", "operation_id": "controller-html"},
	}); err == nil || err.Error() != remoteAgentLocalOnlyReasons["html.render"] {
		t.Fatalf("controller accepted a local-only tool: %v", err)
	}

	if _, err := control.callAsController(ctx, map[string]any{
		"method": "chat.configure", "params": map[string]any{"chat_id": "mcp-chat", "operation_id": "controller-no-tab"},
	}); err == nil || !strings.Contains(err.Error(), "exact tab_id and chat_id") {
		t.Fatalf("controller accepted a half target: %v", err)
	}
}

func TestControllerAgentControlReadsAndDrivesThisMachinesChat(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	control := harness.handler.agentControl
	ctx := context.Background()

	read, err := control.callAsController(ctx, map[string]any{
		"method": "chat.read", "params": map[string]any{"tab_id": "mcp-tab", "chat_id": "mcp-chat", "limit": 5},
	})
	if err != nil {
		t.Fatalf("controller read: %v", err)
	}
	if fieldString(mapFromAnyMain(read), "chatId") != "mcp-chat" {
		t.Fatalf("controller read = %#v", read)
	}

	renamed, err := control.callAsController(ctx, map[string]any{
		"method": "chat.rename",
		"params": map[string]any{"tab_id": "mcp-tab", "chat_id": "mcp-chat", "title": "driven from another machine", "operation_id": "controller-rename-1"},
	})
	if err != nil {
		t.Fatalf("controller rename: %v", err)
	}
	if fieldString(mapFromAnyMain(renamed), "title") != "driven from another machine" {
		t.Fatalf("controller rename = %#v", renamed)
	}

	subagents, err := control.callAsController(ctx, map[string]any{
		"method": "agent.list", "params": map[string]any{"tab_id": "mcp-tab", "chat_id": "mcp-chat"},
	})
	if err != nil {
		t.Fatalf("controller subagent list: %v", err)
	}
	if _, ok := mapFromAnyMain(subagents)["subagents"]; !ok {
		t.Fatalf("controller subagent list = %#v", subagents)
	}
}

func TestControllerChatCreateUsesThisMachinesOwnCatalogDefault(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	control := harness.handler.agentControl
	ctx := context.Background()

	created, err := control.callAsController(ctx, map[string]any{
		"method": "chat.create", "params": map[string]any{"operation_id": "controller-create-1", "title": "from the Mac"},
	})
	if err != nil {
		t.Fatalf("controller create: %v", err)
	}
	result := mapFromAnyMain(created)
	chatID, providerID := fieldString(result, "chatId"), fieldString(result, "providerId")
	if chatID == "" || providerID != "mock" {
		t.Fatalf("controller create = %#v", result)
	}
	// Replaying the same operation is the same chat, not a second one.
	replay, err := control.callAsController(ctx, map[string]any{
		"method": "chat.create", "params": map[string]any{"operation_id": "controller-create-1", "title": "from the Mac"},
	})
	if err != nil || fieldString(mapFromAnyMain(replay), "chatId") != chatID {
		t.Fatalf("controller create replay = %#v err=%v", replay, err)
	}
	if _, err := control.callAsController(ctx, map[string]any{
		"method": "chat.create", "params": map[string]any{"operation_id": "controller-create-2", "title": "bad cwd", "cwd": "inherit"},
	}); err == nil || !strings.Contains(err.Error(), "across machines") {
		t.Fatalf("controller create accepted cwd inherit: %v", err)
	}
}

func TestMachineAgentCatalogReportsWhatThisMachineCanRun(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	catalog := harness.manager.MachineAgentCatalog(context.Background())
	groups, _ := catalog["groups"].([]map[string]any)
	if len(groups) == 0 {
		t.Fatalf("machine catalog = %#v", catalog)
	}
	found := false
	for _, group := range groups {
		if fieldString(group, "providerId") != "mock" {
			continue
		}
		found = true
		if models, _ := group["models"].([]map[string]any); len(models) == 0 {
			t.Fatalf("mock provider group = %#v", group)
		}
	}
	if !found {
		t.Fatalf("machine catalog has no mock provider: %#v", groups)
	}
}
