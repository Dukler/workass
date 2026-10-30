package main

import (
	"strings"
	"testing"
)

// B7 and B9 from the first live two-machine test: two dead ends an agent could
// not get out of, both in the Agent API's own surface rather than in anything
// the fleet does.

// B7. `process.list`, `process.read` and `process.cancel` were machine-scoped
// in the one routing authority from the day they existed — and the inbound
// switch that serves machine-scoped methods did not implement them. So the
// route was computed, forwarded, authorized, and then answered "unknown
// machine-scoped agent control method": the exact shape of failure a single
// routing authority exists to make impossible.
func TestTheProcessChildReadsAndStopAreMachineScopedAndServedForAPeer(t *testing.T) {
	for _, method := range []string{"process.list", "process.read", "process.cancel", "process.start"} {
		if remoteAgentMethodKinds[method] != remoteAgentMachineScoped {
			t.Fatalf("%s must be machine-scoped: a job's home is the machine with the pid", method)
		}
		machineID, remote, err := remoteAgentMethodTarget(method, map[string]any{"machine_id": "m-duklerver"})
		if err != nil || !remote || machineID != "m-duklerver" {
			t.Fatalf("%s with machine_id must route there, got %q/%v/%v", method, machineID, remote, err)
		}
	}
	// Served, not merely routed. The handler is reached with no chat here to
	// act for — a process child's home is the machine with the pid, its owner
	// is a global chat id on the registration, and §6 holds those are two
	// facts — so a missing manager is the only thing that can refuse it.
	handler := &agentControlHandler{selfMachineID: "m-duklerver"}
	for _, method := range []string{"process.list", "process.read", "process.cancel"} {
		if _, err := handler.processChildForController(method, map[string]any{}); err == nil ||
			strings.Contains(err.Error(), "unknown machine-scoped") {
			t.Fatalf("%s must be served for a peer, got %v", method, err)
		}
	}
	if _, err := handler.processChildForController("process.nonsense", map[string]any{}); err == nil {
		t.Fatal("an unknown method must still be refused by name")
	}
}

// A job id read out of a remote listing is a machine-tagged id, and it names
// the machine the job is on. Refusing it while asking for a machine_id the
// caller would have to extract from that same id by hand is a dead end dressed
// as a contract.
func TestATaggedJobIdNamesItsOwnMachineOnTheProcessReads(t *testing.T) {
	for _, method := range []string{"process.read", "process.cancel"} {
		machineID, remote, err := remoteAgentMethodTarget(method, map[string]any{
			"work_id": "M~m-duklerver~xwABCDEF12345",
		})
		if err != nil || !remote || machineID != "m-duklerver" {
			t.Fatalf("%s with a tagged work_id must route to its home, got %q/%v/%v", method, machineID, remote, err)
		}
	}
	// And a tag that disagrees with an explicit machine_id is a real conflict,
	// not something to pick a winner for.
	if _, _, err := remoteAgentMethodTarget("process.read", map[string]any{
		"work_id": "M~m-duklerver~xwABCDEF12345", "machine_id": "m-other",
	}); err == nil {
		t.Fatal("a tagged id that disagrees with machine_id must be refused")
	}
	// A method that names no entity keeps saying so: a tag there is a caller
	// mistake worth naming rather than a home to route to.
	if _, _, err := remoteAgentMethodTarget("chat.create", map[string]any{
		"work_id": "M~m-duklerver~xwABCDEF12345",
	}); err == nil {
		t.Fatal("chat.create names no entity, so a tagged id in it must be refused")
	}
}

// Stopping another machine's job is not reading it. A narrowed link may watch
// the fleet's work and may not end it.
func TestAPeerBelowFullAccessMayReadAnotherMachinesJobsAndNotStopThem(t *testing.T) {
	for _, limit := range []string{fleetLimitRead, fleetLimitEdit} {
		if err := fleetRefuseNarrowedSurface("process.list", limit, nil); err != nil {
			t.Fatalf("%s access must still be able to READ jobs: %v", limit, err)
		}
		if err := fleetRefuseNarrowedSurface("process.read", limit, nil); err != nil {
			t.Fatalf("%s access must still be able to read one job: %v", limit, err)
		}
		err := fleetRefuseNarrowedSurface("process.cancel", limit, nil)
		if err == nil {
			t.Fatalf("%s access must not be able to stop a job", limit)
		}
		if !strings.Contains(err.Error(), "process.cancel") {
			// A structural refusal names the capability, always.
			t.Fatalf("the refusal must name what is missing, got %v", err)
		}
	}
	if err := fleetRefuseNarrowedSurface("process.cancel", fleetLimitFull, nil); err != nil {
		t.Fatalf("full access stops jobs: %v", err)
	}
}

// B9. `workass_list_chats` hands an agent machine-TAGGED ids, because that is
// how every other tool addresses a chat elsewhere. Passing those straight back
// to `process.start` was refused with "a forwarded Workass tool call must
// carry this machine's own ids" — true of the forwarded call and useless to the
// caller, since the tagged pair was the only pair it had ever been given.
func TestTheHostChatPairAcceptsTheTaggedIdsAnAgentIsActuallyGiven(t *testing.T) {
	// On the way out: the router strips the destination's own tag, which is the
	// law it already applied to the addressing pair.
	forwarded := map[string]any{
		"host_tab_id":  "M~m-duklerver~tab-duklerver-codex-1",
		"host_chat_id": "wa:m-duklerver/chat/conv-duklerver-codex-1",
		"label":        "a label that is not an id",
	}
	if err := stripDestinationMachineTags(forwarded, "m-duklerver"); err != nil {
		t.Fatalf("the destination's own tags must be stripped, got %v", err)
	}
	if forwarded["host_tab_id"] != "tab-duklerver-codex-1" {
		t.Fatalf("host_tab_id = %#v, want the bare local id", forwarded["host_tab_id"])
	}
	if forwarded["host_chat_id"] != "conv-duklerver-codex-1" {
		t.Fatalf("host_chat_id = %#v, want the bare local id", forwarded["host_chat_id"])
	}
	if forwarded["label"] != "a label that is not an id" {
		t.Fatal("only addressing ids are rewritten; a label is not one")
	}
	// A third machine is not a tag to strip but a different machine named, and
	// no stripping could make it true.
	third := map[string]any{"host_chat_id": "M~m-elsewhere~conv-1"}
	err := stripDestinationMachineTags(third, "m-duklerver")
	if err == nil || !strings.Contains(err.Error(), "m-elsewhere") {
		t.Fatalf("a third machine's chat must be refused by name, got %v", err)
	}

	// And at the destination, for a call that arrives with the tags still on:
	// a tag naming THIS machine is accepted, one naming another is not.
	tabID, chatID, err := processStartHostPair(map[string]any{
		"host_tab_id": "M~m-duklerver~tab-1", "host_chat_id": "M~m-duklerver~conv-1",
	}, "m-duklerver")
	if err != nil || tabID != "tab-1" || chatID != "conv-1" {
		t.Fatalf("this machine's own tagged pair must be accepted, got %q/%q/%v", tabID, chatID, err)
	}
	if _, _, err := processStartHostPair(map[string]any{
		"host_tab_id": "M~m-elsewhere~tab-1", "host_chat_id": "M~m-elsewhere~conv-1",
	}, "m-duklerver"); err == nil {
		t.Fatal("a chat on a third machine cannot hold the local row for a job running here")
	}
	// Bare ids are what the local path always took, unchanged.
	tabID, chatID, err = processStartHostPair(map[string]any{
		"host_tab_id": "tab-1", "host_chat_id": "conv-1",
	}, "m-duklerver")
	if err != nil || tabID != "tab-1" || chatID != "conv-1" {
		t.Fatalf("bare ids must still work, got %q/%q/%v", tabID, chatID, err)
	}
	// Half-tagged is unresolvable and says which half.
	if _, _, err := processStartHostPair(map[string]any{
		"host_tab_id": "M~m-duklerver~tab-1", "host_chat_id": "conv-1",
	}, "m-duklerver"); err == nil {
		t.Fatal("a half-tagged pair must be refused rather than guessed at")
	}
	// And the guidance survives for a caller that named neither.
	if _, _, err := processStartHostPair(map[string]any{}, "m-duklerver"); err == nil ||
		!strings.Contains(err.Error(), "workass_list_chats") {
		t.Fatalf("a missing pair must say where to get one, got %v", err)
	}
}
