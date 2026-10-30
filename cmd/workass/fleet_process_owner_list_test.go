package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/fleetclient"
	"workass/internal/procchild"
)

// The owner's own list, with two real daemons and a real link.
//
// `workass_list_process_children` is the tool for watching a supervised job,
// and `workass_start_process_child` exists to run one somewhere else — so a
// chat whose every job is elsewhere, which is the normal case, asked the one
// tool meant for it and was told it had none. It listed only the rows homed on
// the machine it was called on.
//
// The monitor is what makes the fixed version honest rather than a fleet-wide
// scan: a monitor exists because THIS chat was registered as the watcher of
// THAT job (§7), so the set it enumerates is exactly this chat's own work.
func TestTheOwnersListIncludesTheJobsItOwnsOnOtherMachines(t *testing.T) {
	secret := newFleetTestSecret(t)

	// H — the machine with the pid.
	hosting := newStatelessMCPTestHarness(t)
	home := serveMeshHome(t, "m-home", secret, hosting.handler.agentControl)
	hostingFleet := installTestFleetHome(t, "m-home", hosting, home)
	hosting.manager.SetProcessLifecycleObserver(hostingFleet.ObserveProcess)

	// P — the machine whose chat asked and owns the outcome.
	asking := newStatelessMCPTestHarness(t)
	away := serveMeshHome(t, "m-away", secret, asking.handler.agentControl)
	var watcher *fleetWatcher
	mesh := monitorMeshDialer(t, "m-away", secret, func(peerID, channel string, payload any) {
		watcher.Ingest(peerID, channel, payload)
	})
	watcher = installTestFleetWatcher(t, "m-away", asking, mesh)
	if !watcher.RegisterOfferChannel(away.hub) {
		t.Fatal("the watcher did not register its monitor-offer channel")
	}
	if err := mesh.AddRoute(home.machineID, "home", fleetclient.KindLAN, home.address, fleetclient.SourceManual); err != nil {
		t.Fatalf("add route to the home: %v", err)
	}
	hostingMesh := monitorMeshDialer(t, "m-home", secret, func(string, string, any) {})
	if err := hostingMesh.AddRoute(away.machineID, "away", fleetclient.KindLAN, away.address, fleetclient.SourceManual); err != nil {
		t.Fatalf("add route to the asking machine: %v", err)
	}
	hosting.handler.agentControl.fleetMesh = hostingMesh
	if _, err := mesh.EnsureLink(context.Background(), home.machineID); err != nil {
		t.Fatalf("the asking machine could not link to the home: %v", err)
	}
	// The closed app may not be reached for any of this.
	asking.handler.agentControl.remoteChats = newFleetMeshRouter(mesh, &refusingRelay{t: t}, "m-away", t.Logf)

	// One running job on H, owned by the chat on P. A real pid, so a running
	// row is an honest one.
	live := os.Getpid()
	now := time.Now().UTC()
	registerRemoteProcessChildForTest(t, hosting, "xwownerlist01", "retrain the ranker", "wa:m-away/chat/mcp-chat", live, now)

	// P's own list, addressed to nobody: the question an owner actually asks.
	listed := callProcessControl(t, asking.handler.agentControl, "process.list", map[string]any{
		"owner_key": "mcp-owner", "parent_chat_id": "mcp-chat", "parent_tab_id": "mcp-tab",
	})
	children := anySlice(listed["children"])
	if len(children) != 1 {
		t.Fatalf("the owner's list must contain its one remote job, got %#v", listed)
	}
	row := mapFromAnyMain(children[0])
	if fieldString(row, "work_id") != "xwownerlist01" {
		t.Fatalf("listed row = %#v", row)
	}
	if fieldString(row, "host_machine") != "m-home" {
		t.Fatalf("a remote row must name where it runs, got %#v", row)
	}
	if fieldString(row, "status") != "running" || fieldString(row, "label") != "retrain the ranker" {
		t.Fatalf("the row must carry the home's own facts, got %#v", row)
	}
	// The home's numbers arrive as strings over the wire, and a list whose
	// local rows carry 0 and whose remote rows carry "0" is one nobody can
	// write a comparison against.
	if pid, ok := row["supervisor_pid"].(int64); !ok || pid != int64(live) {
		t.Fatalf("a forwarded row's numbers must be numbers, got %#v", row["supervisor_pid"])
	}
	if listed["unreachable"] != nil {
		t.Fatalf("every home answered, so nothing is unreachable: %#v", listed["unreachable"])
	}

	// And a home that cannot be asked at all is NAMED rather than dropped,
	// because a short answer must never be mistaken for an empty one.
	asking.handler.agentControl.remoteChats = darkRemoteRouter{}
	listed = callProcessControl(t, asking.handler.agentControl, "process.list", map[string]any{
		"owner_key": "mcp-owner", "parent_chat_id": "mcp-chat", "parent_tab_id": "mcp-tab",
	})
	if children = anySlice(listed["children"]); len(children) != 0 {
		t.Fatalf("a home that cannot be asked contributes no rows, got %#v", listed)
	}
	unreachable := anySlice(listed["unreachable"])
	if len(unreachable) != 1 {
		t.Fatalf("a dark home must be named, got %#v", listed)
	}
	entry := mapFromAnyMain(unreachable[0])
	if fieldString(entry, "machine_id") != "m-home" || fieldString(entry, "reason") == "" {
		t.Fatalf("the unreachable entry must name the machine and say why, got %#v", entry)
	}
	if jobs := anySlice(entry["jobs"]); len(jobs) != 1 || jobs[0] != "retrain the ranker" {
		t.Fatalf("it must name the work it owes, got %#v", entry["jobs"])
	}
}

// darkRemoteRouter is a home that cannot be reached at all.
type darkRemoteRouter struct{}

func (darkRemoteRouter) Call(context.Context, string, map[string]any) (any, error) {
	return nil, errors.New("no usable route to that peer")
}

// registerRemoteProcessChildForTest reports one running job into the hosting
// daemon, through the supervisor's own payload, and offers the owner its
// monitor exactly as a real registration does.
func registerRemoteProcessChildForTest(
	t *testing.T, hosting statelessMCPTestHarness, workID, label, owner string, supervisorPID int, now time.Time,
) {
	t.Helper()
	if _, err := procchild.EnsureDir(hosting.manager.StateDir(), workID); err != nil {
		t.Fatal(err)
	}
	events := []procchild.Event{
		{Seq: 1, At: now, Kind: procchild.KindCreated, Payload: map[string]any{"label": label}},
		{Seq: 2, At: now, Kind: procchild.KindStarted, Payload: map[string]any{"pid": supervisorPID}},
	}
	result := supervisorReport(t, hosting, workID, label, owner, supervisorPID, events)
	if result["status"] != "running" {
		t.Fatalf("the job must be running on its home, got %#v", result)
	}
	if _, offered := result["monitor"]; !offered {
		t.Fatalf("the home must offer the owner a monitor, got %#v", result)
	}
	_ = acp.ProcessLifecycleExit
}
