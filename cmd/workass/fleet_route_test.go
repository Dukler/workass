package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"workass/internal/fleetid"
)

const (
	fleetTestSelfMachine  = "m-fleet-self"
	fleetTestOtherMachine = "m-fleet-other"
)

// Every addressing param the locality table can consult must also be one this
// daemon resolves by home. A param added there and forgotten here would accept
// a global id and then look it up verbatim, which finds nothing and reports the
// entity as missing rather than as elsewhere.
func TestFleetAddressParamFieldsCoverEveryAddressingParam(t *testing.T) {
	listed := make(map[string]struct{}, len(fleetAddressParamFields))
	for _, field := range fleetAddressParamFields {
		listed[field] = struct{}{}
	}
	for _, kind := range []remoteAgentMethodKind{
		remoteAgentLocalOnly, remoteAgentChatScoped, remoteAgentMachineScoped, remoteAgentForkScoped,
	} {
		for _, field := range remoteAgentAddressParams(kind) {
			if _, ok := listed[field]; !ok {
				t.Errorf("remoteAgentAddressParams names %q, which fleetAddressParamFields does not resolve", field)
			}
		}
	}
}

// route(id) with home == self: all three spellings of one of this daemon's own
// entities reach the same in-process path, with the same params, and none of
// them is treated as remote.
func TestFleetRouteResolvesEverySpellingOfThisMachinesIDsLocally(t *testing.T) {
	handler := &agentControlHandler{selfMachineID: fleetTestSelfMachine}
	router := handler.fleetRouter()
	if router.Self() != fleetTestSelfMachine {
		t.Fatalf("router self = %q", router.Self())
	}
	for _, item := range []struct {
		name  string
		tabID string
		chatI string
	}{
		{"bare local ids", "mcp-tab", "mcp-chat"},
		{"global ids naming this daemon", "wa:" + fleetTestSelfMachine + "/chat/mcp-tab", "wa:" + fleetTestSelfMachine + "/chat/mcp-chat"},
		{"legacy tags naming this daemon", "M~" + fleetTestSelfMachine + "~mcp-tab", "M~" + fleetTestSelfMachine + "~mcp-chat"},
	} {
		params := map[string]any{"tab_id": item.tabID, "chat_id": item.chatI, "limit": 3}
		resolved, err := resolveFleetAddressedParams(router, params)
		if err != nil {
			t.Fatalf("%s: %v", item.name, err)
		}
		if resolved["tab_id"] != "mcp-tab" || resolved["chat_id"] != "mcp-chat" {
			t.Fatalf("%s resolved to tab=%v chat=%v", item.name, resolved["tab_id"], resolved["chat_id"])
		}
		if resolved["limit"] != 3 {
			t.Fatalf("%s changed a non-addressing param: %#v", item.name, resolved)
		}
		machineID, remote, err := remoteAgentMethodTarget("chat.diagnostics", resolved)
		if err != nil || remote || machineID != "" {
			t.Fatalf("%s routed away from this machine: machine=%q remote=%v err=%v", item.name, machineID, remote, err)
		}
	}
}

// A bare local id is this daemon's: migration M0, where every entity that
// already exists is at home here with nothing rewritten. The params a caller
// passed must come back as the same map when nothing needed resolving, so an
// ordinary local call allocates and changes nothing.
func TestFleetRouteLeavesAnUnhomedCallExactlyAsItArrived(t *testing.T) {
	router := fleetid.NewRouter(fleetTestSelfMachine)
	params := map[string]any{"tab_id": "mcp-tab", "chat_id": "mcp-chat", "message": "wa: is a scheme, not an id"}
	resolved, err := resolveFleetAddressedParams(router, params)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(params)
	after, _ := json.Marshal(resolved)
	if string(before) != string(after) {
		t.Fatalf("local params changed: %s -> %s", before, after)
	}
	resolved["tab_id"] = "mutated"
	if params["tab_id"] != "mutated" {
		t.Fatal("an unresolved call copied its params; today's local path must be untouched")
	}
}

// Another home keeps travelling in the dialect its relay speaks. P1 adds no
// transport, so a global id for a chat elsewhere must arrive at the renderer
// router as the machine tag that router has always stripped.
func TestFleetRouteHandsAnotherHomeToTheRelayInTheTagItSpeaks(t *testing.T) {
	remote := &stubAgentChatRemoteRouter{result: map[string]any{"ok": true}}
	handler := &agentControlHandler{selfMachineID: fleetTestSelfMachine, remoteChats: remote}
	router := handler.fleetRouter()
	params := map[string]any{
		"tab_id":  "wa:" + fleetTestOtherMachine + "/chat/tab-1",
		"chat_id": "wa:" + fleetTestOtherMachine + "/chat/chat-1",
	}
	resolved, err := resolveFleetAddressedParams(router, params)
	if err != nil {
		t.Fatal(err)
	}
	if resolved["tab_id"] != "M~"+fleetTestOtherMachine+"~tab-1" || resolved["chat_id"] != "M~"+fleetTestOtherMachine+"~chat-1" {
		t.Fatalf("relay params = %#v", resolved)
	}
	machineID, isRemote, err := remoteAgentMethodTarget("chat.configure", resolved)
	if err != nil || !isRemote || machineID != fleetTestOtherMachine {
		t.Fatalf("remote target = machine=%q remote=%v err=%v", machineID, isRemote, err)
	}
	if _, err := handler.forwardRemoteAgentCall(context.Background(), "chat.configure", machineID, resolved); err != nil {
		t.Fatal(err)
	}
	if remote.params["tab_id"] != "M~"+fleetTestOtherMachine+"~tab-1" {
		t.Fatalf("the relay was handed %#v", remote.params)
	}
}

// Both dialects of a home-bearing id are the same fault on arrival from a peer:
// the sender strips its own home on the way out, so anything still naming one
// would be resolved against the wrong machine's entities.
func TestForwardedCallRefusesAHomeBearingIDFromAPeer(t *testing.T) {
	for _, value := range []string{
		"M~" + fleetTestOtherMachine + "~chat-1",
		"wa:" + fleetTestOtherMachine + "/chat/chat-1",
		"wa:" + fleetTestSelfMachine + "/chat/chat-1",
	} {
		err := rejectMachineTaggedParams(map[string]any{"chat_id": value})
		if err == nil {
			t.Fatalf("a forwarded call carrying %q was accepted", value)
		}
		if !strings.Contains(err.Error(), "chat_id") {
			t.Fatalf("refusal for %q must name the param: %v", value, err)
		}
	}
	// The scan reaches free text too, so it must ask whether a value names a
	// home rather than whether it opens like one. Prose that begins with the
	// scheme is prose.
	for _, params := range []map[string]any{
		{"chat_id": "chat-1", "message": "wa: is a scheme, not an id"},
		{"chat_id": "chat-1", "message": "M is a letter"},
		{"chat_id": "chat-1", "prompt": "wa:nothing"},
	} {
		if err := rejectMachineTaggedParams(params); err != nil {
			t.Fatalf("a forwarded call carrying only local ids was refused: %v (%#v)", err, params)
		}
	}
}

// A malformed global id in an addressing param is reported, not silently taken
// for a local id of that name: looking it up verbatim would answer "no such
// chat" for a chat that exists.
func TestFleetRouteReportsAMalformedGlobalIDInAnAddressingParam(t *testing.T) {
	router := fleetid.NewRouter(fleetTestSelfMachine)
	for _, value := range []string{"wa:", "wa:" + fleetTestSelfMachine, "wa:" + fleetTestSelfMachine + "/tab/mcp-tab"} {
		if _, err := resolveFleetAddressedParams(router, map[string]any{"chat_id": value}); err == nil {
			t.Fatalf("chat_id %q was accepted", value)
		}
	}
}

// The P1 acceptance gate (fleet-runtime.md §13): homes arrive and nothing a
// chat shows changes. For chats, subagents, work and forks, the projection a
// tool answers with must be byte-identical whether the chat was addressed by
// its bare local id, by a global id naming this daemon, or by the legacy tag —
// and no projection may start emitting global ids, because every client that
// reads them still speaks only local ones.
func TestGlobalIDsChangeNoProjectionForChatsSubagentsWorkOrForks(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	h.handler.agentControl.selfMachineID = fleetTestSelfMachine
	fleetSeedForkState(t, h.manager.StateDir(), "mcp-tab", "mcp-chat")
	h.handler.agentControl.forks = newForkService(h.manager.StateDir(), h.manager, nil)

	// A settled subagent, a registered process child and a seeded fork run give
	// all four families something to project. The subagent is waited out so the
	// projections hold still: a running child's elapsed time moves between two
	// reads, which would make "byte-identical" a statement about the clock.
	status, response := h.request(t, http.MethodPost, map[string]any{
		"name": "workass_spawn_subagent",
		"arguments": map[string]any{
			"operation_id": "fleet-parity-spawn", "task": "reply with the deterministic mock result",
			"label": "fleet parity child",
		},
	})
	if status != http.StatusOK || response["error"] != nil {
		t.Fatalf("spawn status=%d response=%#v", status, response)
	}
	subagentID := toString(mapFromAnyMain(response["result"])["id"])
	if subagentID == "" {
		t.Fatalf("spawn result = %#v", response["result"])
	}
	status, response = h.request(t, http.MethodPost, map[string]any{
		"name":      "workass_wait_subagent",
		"arguments": map[string]any{"operation_id": "fleet-parity-wait", "subagent_id": subagentID, "timeout_ms": 8000},
	})
	if status != http.StatusOK || response["error"] != nil {
		t.Fatalf("wait status=%d response=%#v", status, response)
	}
	status, response = h.request(t, http.MethodPost, map[string]any{
		"name": "workass_register_external_work",
		"arguments": map[string]any{
			"operation_id": "fleet-parity-register", "label": "fleet parity lane", "pid": 424242,
		},
	})
	if status != http.StatusOK || response["error"] != nil {
		t.Fatalf("register status=%d response=%#v", status, response)
	}
	workID := toString(mapFromAnyMain(response["result"])["workId"])

	projections := map[string]string{}
	for _, tool := range []string{
		"workass_read_chat", "workass_list_subagents", "workass_list_spawned_work", "workass_list_forks",
	} {
		var baseline string
		for _, spelling := range []struct {
			name  string
			tabID string
			chat  string
		}{
			{"bare local ids", "mcp-tab", "mcp-chat"},
			{"global ids naming this daemon", "wa:" + fleetTestSelfMachine + "/chat/mcp-tab", "wa:" + fleetTestSelfMachine + "/chat/mcp-chat"},
			{"the legacy tag naming this daemon", "M~" + fleetTestSelfMachine + "~mcp-tab", "M~" + fleetTestSelfMachine + "~mcp-chat"},
		} {
			status, response := h.request(t, http.MethodPost, map[string]any{
				"name":      tool,
				"arguments": map[string]any{"tab_id": spelling.tabID, "chat_id": spelling.chat},
			})
			if status != http.StatusOK || response["error"] != nil {
				t.Fatalf("%s addressed with %s: status=%d response=%#v", tool, spelling.name, status, response)
			}
			encoded, err := json.Marshal(response["result"])
			if err != nil {
				t.Fatal(err)
			}
			if baseline == "" {
				baseline = string(encoded)
				projections[tool] = baseline
				continue
			}
			if string(encoded) != baseline {
				t.Fatalf("%s projection differs when addressed with %s:\n local  = %s\n homed  = %s",
					tool, spelling.name, baseline, encoded)
			}
		}
	}

	// Nothing may have started emitting a global id. The renderer, the archives
	// and every persisted draft key off the local ids, so a projection that
	// grew a home would be a wire change, not an addition.
	for tool, encoded := range projections {
		if strings.Contains(encoded, fleetid.Scheme) {
			t.Fatalf("%s emitted a global fleet id: %s", tool, encoded)
		}
		if strings.Contains(encoded, fleetid.LegacyPrefix) {
			t.Fatalf("%s emitted a machine tag for its own entities: %s", tool, encoded)
		}
	}

	// Every id these projections carry must be nameable globally with its local
	// part unchanged — the identifier law, checked against the ids this daemon
	// actually minted rather than against invented samples.
	router := h.handler.agentControl.fleetRouter()
	ids := map[fleetid.Type][]string{
		fleetid.Chat:     {"mcp-tab", "mcp-chat"},
		fleetid.Subagent: {subagentID},
		fleetid.Process:  {workID},
		fleetid.Fork:     {"fp-1793316037099-1"},
		fleetid.ForkRun:  {"fr-1793316037099-2"},
	}
	for kind, locals := range ids {
		for _, local := range locals {
			if strings.TrimSpace(local) == "" {
				t.Fatalf("the fixture produced no %s id", kind)
			}
			global, err := router.ID(kind, local)
			if err != nil {
				t.Fatalf("name %s %q globally: %v", kind, local, err)
			}
			if global.Local != local || global.Home != fleetTestSelfMachine {
				t.Fatalf("global id %#v does not contain %q at home %q", global, local, fleetTestSelfMachine)
			}
			resolved, err := router.Route(global.String(), kind)
			if err != nil {
				t.Fatalf("route %s: %v", global, err)
			}
			if !resolved.Self || resolved.Local != local {
				t.Fatalf("%s resolved to %#v instead of this machine's %q", global, resolved, local)
			}
		}
	}

	// Migration M0 in one assertion: homes cost no data migration. Everything
	// this daemon wrote while serving global ids still holds the same local ids
	// it always did, and not one global id was persisted.
	if err := h.runtime.Close(context.Background()); err != nil {
		t.Fatalf("close the runtime before reading its state: %v", err)
	}
	stored := fleetReadStateFiles(t, h.manager.StateDir())
	if len(stored) == 0 {
		t.Fatal("the fixture persisted no state to check")
	}
	for path, body := range stored {
		if strings.Contains(body, fleetid.Scheme) {
			t.Fatalf("%s stored a global fleet id; P1 rewrites no stored id", path)
		}
		if strings.Contains(body, fleetid.LegacyPrefix) {
			t.Fatalf("%s stored a machine tag for a local entity", path)
		}
	}
	joined := strings.Join(fleetSortedValues(stored), "\n")
	for _, local := range []string{"mcp-chat", subagentID, workID, "fp-1793316037099-1", "fr-1793316037099-2"} {
		if !strings.Contains(joined, local) {
			t.Fatalf("stored state no longer holds the local id %q", local)
		}
	}
}

// fleetSeedForkState writes one chat's fork state directly, so the fork family
// has a sealed point and a settled run to project without a provider turn.
func fleetSeedForkState(t *testing.T, stateDir, tabID, chatID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(stateDir, "forks"), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"v": 1, "chatId": chatID, "tabId": tabID,
		"points": []any{map[string]any{
			"id": "fp-1793316037099-1", "name": "fleet parity point", "chatId": chatID, "tabId": tabID,
			"providerId": "mock", "status": "ready", "createdAt": "2026-10-29T23:20:37.000000Z",
			"operationId": "fleet-parity-fork-point", "parentSessionId": "mock-session-1",
		}},
		"runs": []any{map[string]any{
			"id": "fr-1793316037099-2", "forkPointId": "fp-1793316037099-1", "chatId": chatID, "tabId": tabID,
			"concurrency": 1, "createdAt": "2026-10-29T23:20:37.000000Z", "operationId": "fleet-parity-fork-run",
			"children": []any{map[string]any{
				"id": "fc-1793316037099-3", "index": 0, "message": "fleet parity child", "status": "completed",
				"sessionId": "mock-session-2", "result": "done", "usage": map[string]any{},
			}},
		}},
		"operations": map[string]any{},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "forks", chatID+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// fleetReadStateFiles reads every text state file a daemon left behind, keyed
// by its path relative to the state dir. SQLite pages are read as bytes too:
// an id written into a row is still a readable string there, which is exactly
// what this check is looking for.
func fleetReadStateFiles(t *testing.T, stateDir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(stateDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		info, statErr := entry.Info()
		if statErr != nil || info.Size() > 8<<20 {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		relative, relErr := filepath.Rel(stateDir, path)
		if relErr != nil {
			relative = path
		}
		out[relative] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func fleetSortedValues(files map[string]string) []string {
	keys := make([]string, 0, len(files))
	for key := range files {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, fmt.Sprintf("--- %s\n%s", key, files[key]))
	}
	return values
}
