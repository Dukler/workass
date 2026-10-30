package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"workass/internal/acp"
)

// B1, from the first live P1–P4 test (2026-10-30). The only machine-targeting
// `workass_spawn_subagent` offered was the machine-tagged pair of a chat over
// there, which also changed the OWNER — so a subagent of the calling chat,
// hosted elsewhere, was not expressible and the result read as a separate chat.
func TestHostTargetedSpawnKeepsTheCallingChatAsTheOwner(t *testing.T) {
	machine, remote, err := remoteAgentMethodTarget("agent.spawn", map[string]any{"machine_id": "m-duklerver"})
	if err != nil || !remote || machine != "m-duklerver" {
		t.Fatalf("bare machine_id on a spawn: machine=%q remote=%v err=%v", machine, remote, err)
	}
	// The pair still names an owner, and it still wins when it is given.
	machine, remote, err = remoteAgentMethodTarget("agent.spawn", map[string]any{
		"tab_id": "M~m-duklerver~tab-1", "chat_id": "M~m-duklerver~chat-1",
	})
	if err != nil || !remote || machine != "m-duklerver" {
		t.Fatalf("owner pair on a spawn: machine=%q remote=%v err=%v", machine, remote, err)
	}
	// A host and an owner on two different machines is two hops, and is refused.
	if _, _, err := remoteAgentMethodTarget("agent.spawn", map[string]any{
		"tab_id": "M~m-a~tab-1", "chat_id": "M~m-a~chat-1", "machine_id": "m-b",
	}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("host and owner on different machines: err=%v", err)
	}
	// Nothing else chat-scoped gained a bare machine target.
	if _, _, err := remoteAgentMethodTarget("chat.settle", map[string]any{
		"tab_id": "tab-1", "chat_id": "chat-1", "machine_id": "m-a",
	}); err == nil || !strings.Contains(err.Error(), "machine-tagged tab_id and chat_id") {
		t.Fatalf("chat.settle accepted a bare machine_id: err=%v", err)
	}
}

// A home that will not host a kind says what it lacks, by name (§16.4). These
// two are the creations an agent reaches for next, and a routing error would
// have read like a mistake the caller made.
func TestHostTargetRefusalsNameTheReason(t *testing.T) {
	for method, reason := range remoteAgentHostTargetRefusals {
		_, remote, err := remoteAgentMethodTarget(method, map[string]any{"machine_id": "m-duklerver"})
		if err == nil || remote {
			t.Fatalf("%s accepted a host target: remote=%v err=%v", method, remote, err)
		}
		if err.Error() != reason {
			t.Errorf("%s reason = %q", method, err.Error())
		}
		if remoteAgentHostTargetable[method] {
			t.Errorf("%s is both host-targetable and refused", method)
		}
	}
	for _, method := range []string{"chat.subsession", "fork.create", "fork.run"} {
		if strings.TrimSpace(remoteAgentHostTargetRefusals[method]) == "" {
			t.Errorf("%s has no host-target reason", method)
		}
	}
}

// The host runs it; the chat that asked owns it. A row registered at the host
// is what put the work under a conversation that never asked for it.
func TestHostedSubagentOwnerIsNotAChatHere(t *testing.T) {
	owner, tab := acp.HostedSubagentOwner("m-mac", "app-chat-1")
	if owner == "" || owner != tab {
		t.Fatalf("hosted owner = %q/%q", owner, tab)
	}
	if !acp.IsHostedSubagentOwner(owner) {
		t.Fatal("a hosted owner must be recognisable as naming no chat here")
	}
	if acp.IsHostedSubagentOwner("app-chat-1") {
		t.Fatal("an ordinary chat id was read as a hosted owner")
	}
	if missing, _ := acp.HostedSubagentOwner("", "app-chat-1"); missing != "" {
		t.Fatal("a hosted owner was minted without the machine that asked")
	}
}

// A hosted spawn with no origin has nobody to report to, and a child nobody is
// owed is the dishonesty the fleet runtime exists to remove.
func TestHostedSpawnRefusesWithoutTheChatThatAsked(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	control := harness.handler.agentControl
	if _, err := control.hostedSpawnForOrigin(context.Background(), fleetForwardedWork{Limit: fleetLimitFull}, map[string]any{
		"prompt": "do the thing",
	}); err == nil || !strings.Contains(err.Error(), "needs the chat that asked") {
		t.Fatalf("origin-less hosted spawn: err=%v", err)
	}
	// A fork needs the native session file, which is on the asking machine.
	if _, err := control.hostedSpawnForOrigin(context.Background(), fleetForwardedWork{
		Limit:  fleetLimitFull,
		Origin: fleetCommandOrigin{Machine: "m-mac", ChatID: "app-chat-1", TabID: "app-tab-1"},
	}, map[string]any{"prompt": "do the thing", "fork": true}); err == nil || !strings.Contains(err.Error(), "fleet-runtime §9") {
		t.Fatalf("hosted fork: err=%v", err)
	}
}

// The row the asking chat was missing for the whole life of the child, and the
// settle that follows the monitor's exit. One row per child, replay included.
func TestHostedSubagentRowLivesInTheChatThatAsked(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	manager := harness.handler.agentControl.manager
	// The same origin the production caller stamps: the asking chat's own lane
	// and running turn, which is what lets the actor own the row at all.
	laneID, operationID := harness.runtime.watcherOrigin("mcp-tab", "mcp-chat")
	if laneID == "" && operationID == "" {
		t.Fatal("the asking chat has no lane or turn to own a hosted row")
	}
	manager.RegisterHostedSubagent("mcp-tab", "mcp-chat", acp.HostedSubagentRow{
		SubagentID: "wa-subagent-1-1", Label: "Build the thing", ModelLabel: "mock [high]",
		ProviderID: "claude", HostMachine: "m-duklerver", HostMachineName: "duklerver",
		OriginLaneID: laneID, OriginOperationID: operationID,
	})
	manager.RegisterHostedSubagent("mcp-tab", "mcp-chat", acp.HostedSubagentRow{
		SubagentID: "wa-subagent-1-1", Label: "Build the thing", HostMachine: "m-duklerver",
		OriginLaneID: laneID, OriginOperationID: operationID,
	})
	rows := manager.ListSpawnedWork("mcp-tab", "mcp-chat")
	hosted := []acp.SpawnedWorkItem{}
	for _, row := range rows {
		if row.HostMachine != "" {
			hosted = append(hosted, row)
		}
	}
	if len(hosted) != 1 {
		t.Fatalf("hosted rows = %d, want exactly one per child: %#v", len(hosted), rows)
	}
	row := hosted[0]
	if row.ChatID != "mcp-chat" || row.TabID != "mcp-tab" {
		t.Fatalf("hosted row owner = %s/%s", row.TabID, row.ChatID)
	}
	if row.Status != "running" || row.HostMachine != "m-duklerver" || row.HostMachineName != "duklerver" {
		t.Fatalf("hosted row = %#v", row)
	}
	// What it is doing, like any other row. The machine is a fact of its own and
	// never takes the activity's place.
	if strings.Contains(row.Summary, "duklerver") || row.LastToolName != "starting" {
		t.Fatalf("hosted row put the machine where the activity goes: %#v", row)
	}
	// The actor is the authority the UI reads, and the host travels with the
	// row, so the chat shows the work AND where it is running.
	live, ok := harness.runtime.Snapshot("mcp-chat")
	if !ok {
		t.Fatal("the asking chat has no snapshot")
	}
	owned, tracked := live.Background["wa-subagent-1-1"]
	if !tracked || owned.Event.Status != "running" || owned.Event.HostMachineName != "duklerver" {
		t.Fatalf("the asking chat's actor did not own the hosted row: tracked=%v event=%#v", tracked, owned.Event)
	}
	manager.SettleHostedSubagent("mcp-tab", "mcp-chat", acp.SubagentReceipt{
		SubagentID: "wa-subagent-1-1", Status: "done", Label: "Build the thing", Result: "built",
	})
	settled, ok := harness.runtime.Snapshot("mcp-chat")
	if !ok {
		t.Fatal("the asking chat lost its snapshot")
	}
	finished, tracked := settled.Background["wa-subagent-1-1"]
	if !tracked {
		t.Fatal("the settled hosted row disappeared from the chat that asked")
	}
	if finished.Event.Status == "running" {
		t.Fatalf("hosted row stayed running after its monitor exit: %#v", finished.Event)
	}
	if finished.Event.HostMachineName != "duklerver" {
		t.Fatalf("settled hosted row lost the machine that ran it: %#v", finished.Event)
	}
}

// A local spawn is untouched: the machine_id that names THIS machine takes the
// ordinary in-process path, with no host anywhere on the row.
func TestLocalSpawnIsUnchangedByHostTargeting(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	harness.handler.agentControl.selfMachineID = "m-self"
	status, response := harness.request(t, http.MethodPost, map[string]any{
		"name": "workass_spawn_subagent",
		"arguments": map[string]any{
			"task": "summarize the mock fixture", "operation_id": "local-spawn-1",
			"machine_id": "m-self", "permission_intent": "read",
		},
	})
	if status != http.StatusOK {
		t.Fatalf("local spawn HTTP=%d response=%#v", status, response)
	}
	// The spawn itself may fail on fixture grounds; what must never happen is a
	// routing refusal that mentions another machine, and no row may carry a host.
	if failure := strings.ToLower(fieldString(response, "error")); strings.Contains(failure, "machine-tagged") {
		t.Fatalf("a self-addressed spawn was routed as remote: %s", failure)
	}
	for _, row := range harness.manager.ListSpawnedWork("mcp-tab", "mcp-chat") {
		if row.HostMachine != "" {
			t.Fatalf("a local spawn was recorded as hosted elsewhere: %#v", row)
		}
	}
}
