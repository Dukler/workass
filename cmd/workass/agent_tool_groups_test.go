package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"workass/internal/acp"
	"workass/internal/wire"
)

// A new Workass tool must be put somewhere: in a group a person can switch off,
// or among the few Workass itself depends on. Neither list may name a tool
// that no longer exists.
func TestEveryWorkassToolHasAGroupOrIsEssential(t *testing.T) {
	names := map[string]bool{}
	for _, tool := range agentToolsCatalog() {
		name := toString(tool["name"])
		names[name] = true
		group := agentToolGroup(name)
		switch {
		case group == "" && !essentialAgentTools[name]:
			t.Errorf("%s is in no tool group and not essential", name)
		case group != "" && essentialAgentTools[name]:
			t.Errorf("%s is both in group %s and essential", name, group)
		case group != "" && !acp.KnownToolGroup(group):
			t.Errorf("%s is in unknown group %s", name, group)
		}
	}
	for name := range agentToolGroups {
		if !names[name] {
			t.Errorf("tool group names %s, which is not a Workass tool", name)
		}
	}
	for name := range essentialAgentTools {
		if !names[name] {
			t.Errorf("essential tools name %s, which is not a Workass tool", name)
		}
	}
	for _, id := range acp.ToolGroupOrder {
		found := false
		for name := range names {
			found = found || agentToolGroup(name) == id
		}
		if !found {
			t.Errorf("group %s has no tools", id)
		}
	}
}

// Switching a group off removes every one of its tools from the listing and
// from the guide, and leaves every other group's tools where they were.
func TestSwitchedOffGroupLeavesTheListAndTheGuide(t *testing.T) {
	manager := acp.NewManager(acp.Options{WorkassToolsOrigin: "https://localhost:8788", StateDir: filepath.Join(t.TempDir(), "state")})
	t.Cleanup(func() { manager.Reset() })
	everyList := toolListText(nil)
	_, everyGuide := manager.WorkassGuide(nil)
	for _, id := range acp.ToolGroupOrder {
		off := acp.ToolGroups{id: true}
		list := toolListText(off)
		_, guide := manager.WorkassGuide(off)
		for _, tool := range agentToolsCatalog() {
			name := toString(tool["name"])
			if internalWorkassTools[name] {
				continue
			}
			quoted := `"` + name + `"`
			if agentToolGroup(name) == id {
				if strings.Contains(list, quoted) {
					t.Errorf("%s off: tools list still has %s", id, name)
				}
				if strings.Contains(guide, name) {
					t.Errorf("%s off: the guide still names %s", id, name)
				}
				continue
			}
			if !strings.Contains(list, quoted) {
				t.Errorf("%s off: tools list lost %s of another group", id, name)
			}
			if strings.Contains(everyGuide, name) && !strings.Contains(guide, name) {
				t.Errorf("%s off: the guide lost %s of another group", id, name)
			}
		}
		if len(list) >= len(everyList) || len(guide) >= len(everyGuide) && id != acp.ToolGroupScheduled {
			// Scheduled messages have no guide sentence of their own.
			t.Errorf("%s off: list %d of %d bytes, guide %d of %d bytes", id, len(list), len(everyList), len(guide), len(everyGuide))
		}
	}
	allOff := acp.ToolGroups{}
	for _, id := range acp.ToolGroupOrder {
		allOff[id] = true
	}
	_, guide := manager.WorkassGuide(allOff)
	for _, kept := range []string{"Workass CLI reference", "workass_agent_catalog", "workass_host_artifact", "Background work, which tool"} {
		if !strings.Contains(guide, kept) {
			t.Errorf("every group off: the guide lost %q:\n%s", kept, guide)
		}
	}
	for _, gone := range []string{"Browser tools:", "list/read/create", "orchestrate tracked subagents", "update state", "timeout_ms=-1", "`workass run`"} {
		if strings.Contains(guide, gone) {
			t.Errorf("every group off: the guide still says %q", gone)
		}
	}
}

// The numbers are counted from the text itself: the total is the guide and
// the list, a group's cost is what switching it off removes, and the parts
// add up to the whole.
func TestAgentToolsViewCountsTheTextAgentsReceive(t *testing.T) {
	manager := acp.NewManager(acp.Options{WorkassToolsOrigin: "https://localhost:8788", StateDir: filepath.Join(t.TempDir(), "state")})
	t.Cleanup(func() { manager.Reset() })
	view := agentToolsView(manager, acp.ToolGroups{acp.ToolGroupBrowser: true})
	instructions, guide := manager.WorkassGuide(acp.ToolGroups{acp.ToolGroupBrowser: true})
	if want := approxTokens(len([]rune(instructions + guide))); view["guideTokens"] != want {
		t.Fatalf("guideTokens = %v, want %d", view["guideTokens"], want)
	}
	if want := approxTokens(len([]rune(toolListText(acp.ToolGroups{acp.ToolGroupBrowser: true})))); view["listTokens"] != want {
		t.Fatalf("listTokens = %v, want %d", view["listTokens"], want)
	}
	if view["totalTokens"].(int) < view["guideTokens"].(int)+view["listTokens"].(int)-1 {
		t.Fatalf("total is not guide plus list: %#v", view)
	}
	full := agentToolsView(manager, nil)
	sum := full["essential"].(map[string]any)["tokens"].(int)
	groups := full["groups"].([]any)
	if len(groups) != len(acp.ToolGroupOrder) {
		t.Fatalf("groups = %d", len(groups))
	}
	for index, raw := range groups {
		group := raw.(map[string]any)
		if group["id"] != acp.ToolGroupOrder[index] || group["on"] != true || group["tokens"].(int) <= 0 || group["tools"].(int) <= 0 {
			t.Fatalf("group %d = %#v", index, group)
		}
		sum += group["tokens"].(int)
	}
	if total := full["totalTokens"].(int); sum < total-len(groups) || sum > total+len(groups) {
		t.Fatalf("groups and essential add to %d, total is %d", sum, total)
	}
	for _, raw := range view["groups"].([]any) {
		group := raw.(map[string]any)
		if (group["id"] == acp.ToolGroupBrowser) == (group["on"] == true) {
			t.Fatalf("browser off is not reported off: %#v", group)
		}
	}
}

func TestToolGroupsOffSurviveSettingsNormalization(t *testing.T) {
	got := normalizeSettings(map[string]any{agentToolGroupsOffKey: []any{"machines", "nope", "browser", "machines", 7}})
	if want := []any{"browser", "machines"}; !reflect.DeepEqual(got[agentToolGroupsOffKey], want) {
		t.Fatalf("normalized = %#v, want %#v", got[agentToolGroupsOffKey], want)
	}
	if got := normalizeSettings(nil)[agentToolGroupsOffKey]; !reflect.DeepEqual(got, []any{}) {
		t.Fatalf("default = %#v", got)
	}
	if off := toolGroupsOffFromSettings(got); !off.Off("browser") || !off.Off("machines") || off.Off("pages") {
		t.Fatalf("off = %#v", off)
	}
}

// agent-tools:set changes one key, keeps the rest of the blob, tells other
// windows, and answers with the new view; the manager reads the same setting.
func TestAgentToolsSetKeepsOtherSettingsAndReachesTheManager(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	manager := acp.NewManager(acp.Options{WorkassToolsOrigin: "https://localhost:8788", StateDir: stateDir})
	t.Cleanup(func() { manager.Reset() })
	hub := wire.NewHub(wire.Options{})
	registerDaemonHandlers(hub, t.TempDir(), manager, daemonOptions{StateDir: stateDir})
	state := newDaemonState("", daemonOptions{StateDir: stateDir})
	if _, err := state.setting.set(map[string]any{
		"chatTitles":     map[string]any{"mode": "agent", "providerId": "mock", "model": "m"},
		"modelFavorites": []any{map[string]any{"providerId": "mock", "modelId": "m"}},
	}); err != nil {
		t.Fatal(err)
	}
	reply, err := hub.Invoke("agent-tools:set", []any{map[string]any{"group": "browser", "on": false}})
	if err != nil {
		t.Fatal(err)
	}
	view := reply.(map[string]any)
	if view["groups"].([]any)[0].(map[string]any)["on"] != false {
		t.Fatalf("view after switching browser off = %#v", view["groups"])
	}
	saved := state.setting.get()
	if !reflect.DeepEqual(saved[agentToolGroupsOffKey], []any{"browser"}) {
		t.Fatalf("saved off = %#v", saved[agentToolGroupsOffKey])
	}
	if titles := saved["chatTitles"].(map[string]any); titles["mode"] != "agent" || len(saved["modelFavorites"].([]any)) != 1 {
		t.Fatalf("other settings were lost: %#v", saved)
	}
	if !manager.ToolGroupsOff().Off("browser") {
		t.Fatal("the manager does not read the switched-off group")
	}
	if _, err := hub.Invoke("agent-tools:set", []any{map[string]any{"group": "browser", "on": true}}); err != nil {
		t.Fatal(err)
	}
	if manager.ToolGroupsOff().Off("browser") {
		t.Fatal("switching back on did not reach the manager")
	}
	if _, err := hub.Invoke("agent-tools:set", []any{map[string]any{"group": "nope", "on": false}}); err == nil {
		t.Fatal("an unknown group was accepted")
	}
	if _, err := hub.Invoke("agent-tools:set", []any{map[string]any{"group": "browser"}}); err == nil {
		t.Fatal("a call without on was accepted")
	}
}

// A switched-off group is gone from what the tool endpoint lists, and naming
// or calling one of its tools says it is off rather than unknown.
func TestToolEndpointRefusesASwitchedOffGroup(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	harness.manager.SetToolGroupsSource(func() acp.ToolGroups {
		return acp.ToolGroups{acp.ToolGroupChats: true, acp.ToolGroupBrowser: true}
	})
	get := func(query url.Values) (int, map[string]any) {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, harness.server.URL+toolsPath+"?"+query.Encode(), nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer mcp-owner")
		request.Header.Set("X-Workass-Chat-ID", "mcp-chat")
		request.Header.Set("X-Workass-Tab-ID", "mcp-tab")
		reply, err := harness.client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer reply.Body.Close()
		var response map[string]any
		if err := json.NewDecoder(reply.Body).Decode(&response); err != nil {
			t.Fatal(err)
		}
		return reply.StatusCode, response
	}
	for _, query := range []url.Values{{}, {"full": {"1"}}} {
		status, response := get(query)
		encoded, _ := json.Marshal(response)
		if status != http.StatusOK || strings.Contains(string(encoded), "workass_rename_chat") || strings.Contains(string(encoded), "workass_browser_") {
			t.Fatalf("list %v status %d still has a switched-off tool: %s", query, status, encoded)
		}
		if !strings.Contains(string(encoded), "workass_spawn_subagent") || !strings.Contains(string(encoded), "workass_ask_user_question") {
			t.Fatalf("list %v lost a tool that is on: %s", query, encoded)
		}
	}
	status, response := get(url.Values{"name": {"workass_rename_chat"}})
	if status != http.StatusForbidden || !strings.Contains(toString(response["error"]), "switched off") {
		t.Fatalf("schema of a switched-off tool: %d %#v", status, response)
	}
	status, response = harness.request(t, http.MethodPost, map[string]any{
		"name": "workass_rename_chat", "arguments": map[string]any{"operation_id": "rename-off-1", "title": "Never"},
	})
	if status != http.StatusForbidden || !strings.Contains(toString(response["error"]), "switched off") {
		t.Fatalf("call of a switched-off tool: %d %#v", status, response)
	}
	actor, err := harness.runtime.actor("mcp-chat")
	if err != nil {
		t.Fatal(err)
	}
	if title := actor.engine.Snapshot().Presentation.Title; title != "MCP owner" {
		t.Fatalf("a switched-off tool still ran: title %q", title)
	}
	status, response = harness.request(t, http.MethodPost, map[string]any{
		"name": "workass_list_scheduled_messages", "arguments": map[string]any{},
	})
	if status != http.StatusOK || response["error"] != nil && strings.Contains(toString(response["error"]), "switched off") {
		t.Fatalf("a tool that is on was refused: %d %#v", status, response)
	}
}

// bug-20261113-0bf46145: every Workass tool a delegated child's guide names is
// one its catalog has, with any group switched off, so the guide never sends a
// child to "unknown Workass tool".
func TestADelegatedChildsGuideNamesOnlyToolsItCanCall(t *testing.T) {
	manager := acp.NewManager(acp.Options{WorkassToolsOrigin: "https://localhost:8788", StateDir: filepath.Join(t.TempDir(), "state")})
	t.Cleanup(func() { manager.Reset() })
	named := regexp.MustCompile(`workass_[a-z_]+`)
	for _, id := range append([]string{""}, acp.ToolGroupOrder...) {
		off := acp.ToolGroups{}
		if id != "" {
			off[id] = true
		}
		callable := map[string]bool{}
		for _, tool := range withoutToolGroups(agentToolsForChild(), off) {
			callable[toString(tool["name"])] = true
		}
		instructions, guide := manager.WorkassGuideFor(off, true)
		for _, name := range named.FindAllString(instructions+guide, -1) {
			if !callable[name] {
				t.Errorf("groups off %v: a child's guide names %s, which it cannot call", off, name)
			}
		}
		if id == "" && !strings.Contains(guide, "workass_fork_run") {
			t.Errorf("a child's guide does not offer the fork run it can call:\n%s", guide)
		}
	}
}
