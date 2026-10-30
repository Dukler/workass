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

// A job id an agent was handed elsewhere reaches its home bare, and one naming a
// third machine is refused rather than stripped into a different job.
func TestAWorkIDIsReducedToTheDestinationsOwnID(t *testing.T) {
	forwarded := map[string]any{
		"work_id": "M~m-duklerver~xwjob00000001",
		"label":   "a label that is not an id",
	}
	if err := stripDestinationMachineTags(forwarded, "m-duklerver"); err != nil {
		t.Fatalf("the destination's own tag must be stripped, got %v", err)
	}
	if forwarded["work_id"] != "xwjob00000001" {
		t.Fatalf("work_id = %#v, want the bare local id", forwarded["work_id"])
	}
	if forwarded["label"] != "a label that is not an id" {
		t.Fatal("only addressing ids are rewritten; a label is not one")
	}
	err := stripDestinationMachineTags(map[string]any{"work_id": "M~m-elsewhere~xwjob00000001"}, "m-duklerver")
	if err == nil || !strings.Contains(err.Error(), "m-elsewhere") {
		t.Fatalf("a third machine's job must be refused by name, got %v", err)
	}
}
