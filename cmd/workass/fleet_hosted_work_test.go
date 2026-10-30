package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/fleetclient"
	"workass/internal/fleetid"
	"workass/internal/fleetlog"
	"workass/internal/procchild"
)

// The rail reads spawned-work:list, which is ListBackground, and nothing else. So
// every assertion here is on that row: a field that reached the manager and not
// the list is a field the operator cannot see.
func railRow(t *testing.T, harness statelessMCPTestHarness, workID string) map[string]any {
	t.Helper()
	rows, err := harness.runtime.ListBackground("mcp-tab", "mcp-chat")
	if err != nil {
		t.Fatalf("spawned-work:list: %v", err)
	}
	for _, row := range rows {
		if fieldString(row, "id") == workID {
			return row
		}
	}
	return nil
}

func awaitRailRow(t *testing.T, harness statelessMCPTestHarness, workID string, ready func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		if last = railRow(t, harness, workID); last != nil && ready(last) {
			return last
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the rail never showed %s as expected; last row: %#v", workID, last)
	return nil
}

func hostedRowOwnerOrigin(harness statelessMCPTestHarness) (string, string, string) {
	return harness.runtime.hostedRowOrigin("mcp-chat")
}

// A subagent on another machine reads like one here: what it is doing, in the two
// fields a local row uses, changing as it runs, with the machine beside it.
func TestARemoteSubagentRowCarriesTheHostsLiveActivity(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	row := fleetlog.Registration{
		WatcherChatID: "mcp-chat", WatcherTabID: "mcp-tab",
		TargetID: "wa:m-host/subagent/wa-subagent-9-1", Label: "remote test",
	}
	lane, operation, _ := hostedRowOwnerOrigin(harness)
	harness.handler.agentControl.manager.RegisterHostedSubagent("mcp-tab", "mcp-chat", acp.HostedSubagentRow{
		SubagentID: "wa-subagent-9-1", Label: "remote test", ModelLabel: "GPT-6.1-Sol-low", ProviderID: "codex",
		HostMachine: "m-host", HostMachineName: fleetDisplayName("workass daemon duklerver (m-host)", "m-host"),
		OriginLaneID: lane, OriginOperationID: operation,
	})
	started := railRow(t, harness, "wa-subagent-9-1")
	if started == nil {
		t.Fatal("no rail row for a subagent running on another machine")
	}
	if fieldString(started, "hostMachineName") != "duklerver" || fieldString(started, "hostMachine") != "m-host" {
		t.Fatalf("the machine did not reach the rail: %#v", started)
	}
	if fieldString(started, "lastToolName") != "starting" || fieldString(started, "summary") != "Starting subagent" {
		t.Fatalf("the row opened with something other than a local row's words: %#v", started)
	}
	if fieldString(started, "modelLabel") != "GPT-6.1-Sol-low" || fieldString(started, "assistantBrand") != "gpt" {
		t.Fatalf("the model did not reach the rail: %#v", started)
	}
	for _, step := range []struct{ phase, activity string }{
		{"working", "Working"},
		{"tool", "Read a file"},
	} {
		harness.runtime.applyHostedState("m-host", "duklerver", row, hostedStatePayload(acp.HostedWorkState{
			Kind: acp.HostedWorkSubagent, WorkID: "wa-subagent-9-1", Phase: step.phase, Activity: step.activity,
		}))
		live := railRow(t, harness, "wa-subagent-9-1")
		if fieldString(live, "lastToolName") != step.phase || fieldString(live, "summary") != step.activity {
			t.Fatalf("after %q the rail row reads %#v", step.phase, live)
		}
		if fieldString(live, "hostMachineName") != "duklerver" {
			t.Fatalf("the activity pushed out the machine: %#v", live)
		}
	}
	// A cancel notifies nobody, and that must not leave the row running.
	harness.runtime.settleHostedExit(row, fleetlog.Event{Kind: fleetlog.KindExit, Payload: map[string]any{
		"outcome": "cancelled", "noticeSuppressed": true,
		"receipt": subagentReceiptPayload(acp.SubagentReceipt{SubagentID: "wa-subagent-9-1", Status: "cancelled", Label: "remote test"}),
	}})
	if ended := railRow(t, harness, "wa-subagent-9-1"); ended == nil || fieldString(ended, "status") == "running" {
		t.Fatalf("a cancelled remote subagent stayed running: %#v", ended)
	}
}

// The stop button on a remote row reaches the machine running the work, and the
// row ends when that machine says it has, not when the click happens.
func TestStoppingARemoteRowGoesToItsHost(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	manager := harness.handler.agentControl.manager
	lane, operation, _ := hostedRowOwnerOrigin(harness)
	manager.RegisterHostedSubagent("mcp-tab", "mcp-chat", acp.HostedSubagentRow{
		SubagentID: "wa-subagent-9-2", Label: "stop me", HostMachine: "m-host",
		OriginLaneID: lane, OriginOperationID: operation,
	})
	var asked acp.SpawnedWorkItem
	failure := errors.New("machine m-host is dark")
	manager.SetHostedWorkRouter(acp.HostedWorkRouter{Stop: func(_ context.Context, item acp.SpawnedWorkItem) error {
		asked = item
		return failure
	}})
	result, hosted := manager.StopHostedWork("mcp-tab", "mcp-chat", "wa-subagent-9-2")
	if !hosted || result["ok"] == true {
		t.Fatalf("a stop that could not reach the host answered %#v", result)
	}
	if asked.HostMachine != "m-host" || asked.Kind != "subagent" {
		t.Fatalf("the stop went to %#v", asked)
	}
	// Not a stable operation stuck on its first failure: the next click tries again.
	failure = nil
	if result, _ := manager.StopHostedWork("mcp-tab", "mcp-chat", "wa-subagent-9-2"); result["ok"] != true || result["status"] != "cancelling" {
		t.Fatalf("the retried stop answered %#v", result)
	}
	if row := railRow(t, harness, "wa-subagent-9-2"); fieldString(row, "status") != "running" {
		t.Fatalf("the row ended on the click rather than on the host's word: %#v", row)
	}
	if result, hosted := manager.StopHostedWork("mcp-tab", "mcp-chat", "not-hosted"); hosted {
		t.Fatalf("a row that is not hosted was taken for one: %#v", result)
	}
}

// Only the machine a child runs for may stop it.
func TestAHostedSubagentStopsOnlyForItsOwnMachine(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	if _, err := harness.handler.agentControl.manager.CancelHostedSubagent("wa-subagent-missing-1", "m-away"); err == nil {
		t.Fatal("a stop for a child this machine does not host was accepted")
	}
}

// Two real daemons and a real link: a job on H is a row in the chat on P from its
// first report, says queued and then running, names its machine, and ends.
func TestARemoteJobIsARowInTheChatThatOwnsIt(t *testing.T) {
	secret := newFleetTestSecret(t)
	hosting := newStatelessMCPTestHarness(t)
	home := serveMeshHome(t, "m-home", secret, hosting.handler.agentControl)
	hostingFleet := installTestFleetHome(t, "m-home", hosting, home)
	hosting.manager.SetProcessLifecycleObserver(hostingFleet.ObserveProcess)

	asking := newStatelessMCPTestHarness(t)
	away := serveMeshHome(t, "m-away", secret, asking.handler.agentControl)
	var watcher *fleetWatcher
	mesh := monitorMeshDialer(t, "m-away", secret, func(peerID, channel string, payload any) {
		watcher.Ingest(peerID, channel, payload)
	})
	watcher = installTestFleetWatcher(t, "m-away", asking, mesh)
	watcher.hostedState = func(peerID string, row fleetlog.Registration, fields map[string]any) {
		asking.runtime.applyHostedState(peerID, "duklerver", row, fields)
	}
	watcher.settleHosted = asking.runtime.settleHostedExit
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

	owner := "wa:m-away/chat/mcp-chat"
	workID, err := procchild.IDForOperation("rail-job-operation")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	live := os.Getpid()
	supervisorReport(t, hosting, workID, "train on the box", owner, live, []procchild.Event{
		{Seq: 1, Kind: procchild.KindCreated, At: now},
		{Seq: 2, Kind: procchild.KindQueued, At: now, Payload: map[string]any{"position": 2}},
	})
	awaitWatchedMonitor(t, watcher, workID)
	// The offer can land after the first report's statement. The next report, a
	// heartbeat at the latest, states it again.
	supervisorReport(t, hosting, workID, "train on the box", owner, live, []procchild.Event{
		{Seq: 3, Kind: procchild.KindHeartbeat, At: now},
	})
	queued := awaitRailRow(t, asking, workID, func(map[string]any) bool { return true })
	if fieldString(queued, "status") != "running" || fieldString(queued, "kind") != "external" {
		t.Fatalf("the job's row is not an ordinary running row: %#v", queued)
	}
	if fieldString(queued, "queuedAt") == "" || fieldString(queued, "execStartedAt") != "" {
		t.Fatalf("a job waiting its turn does not read as queued: %#v", queued)
	}
	if fieldString(queued, "hostMachine") != "m-home" || fieldString(queued, "hostMachineName") != "duklerver" {
		t.Fatalf("the row does not say where the job runs: %#v", queued)
	}
	started := now.Add(time.Second)
	supervisorReport(t, hosting, workID, "train on the box", owner, live, []procchild.Event{
		{Seq: 4, Kind: procchild.KindStarted, At: started, Payload: map[string]any{"pid": live, "pgid": live, "group": "process_group"}},
	})
	awaitRailRow(t, asking, workID, func(row map[string]any) bool { return fieldString(row, "execStartedAt") != "" })
	supervisorReport(t, hosting, workID, "train on the box", owner, live, []procchild.Event{
		{Seq: 5, Kind: procchild.KindSettled, At: started.Add(time.Minute), Payload: map[string]any{
			"outcome": procchild.OutcomeSettled, "exitCode": 0, "summary": "Finished with exit 0 after 1m0s", "elapsedMs": 60000,
		}},
	})
	ended := awaitRailRow(t, asking, workID, func(row map[string]any) bool { return fieldString(row, "status") != "running" })
	if fieldString(ended, "status") != "exited" || intField(ended, "runMs") != 60000 {
		t.Fatalf("the job's end did not reach the row: %#v", ended)
	}
	awaitOneProcessNotice(t, asking, "train on the box")
}

// A row stands for its target whichever kind it is, by the id the monitor holds.
func TestHostedStateRefusesAWorkIDItsMonitorDoesNotName(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	target, err := fleetid.New("m-host", fleetid.Process, "job-a")
	if err != nil {
		t.Fatal(err)
	}
	harness.runtime.applyHostedState("m-host", "duklerver", fleetlog.Registration{
		WatcherChatID: "mcp-chat", WatcherTabID: "mcp-tab", TargetID: target.String(),
	}, map[string]any{"workId": "job-b", "kind": acp.HostedWorkProcess})
	if row := railRow(t, harness, "job-b"); row != nil {
		t.Fatalf("a statement named a job its monitor is not about: %#v", row)
	}
}

func TestDisplayNameOfAPeer(t *testing.T) {
	for _, testCase := range []struct{ raw, id, want string }{
		{"workass daemon duklerver (m-03)", "m-03", "duklerver"},
		{"workass daemon m-03", "m-03", ""},
		{"duklerver", "m-03", "duklerver"},
		{"", "m-03", ""},
	} {
		if got := fleetDisplayName(testCase.raw, testCase.id); got != testCase.want {
			t.Fatalf("fleetDisplayName(%q) = %q, want %q", testCase.raw, got, testCase.want)
		}
	}
}

// The live failure: a child failed in 187ms, its exit reached its home's log, and
// the push beat the spawn reply to the owner, who had no monitor to land it on. The
// owner still showed it running eighteen minutes later and had been told nothing.
func TestAnExitAlreadyInTheLogReachesTheOwnerAsARowAndANotice(t *testing.T) {
	secret := newFleetTestSecret(t)
	hosting := newStatelessMCPTestHarness(t)
	home := serveMeshHome(t, "m-home", secret, hosting.handler.agentControl)
	hostingFleet := installTestFleetHome(t, "m-home", hosting, home)

	asking := newStatelessMCPTestHarness(t)
	var watcher *fleetWatcher
	mesh := monitorMeshDialer(t, "m-away", secret, func(peerID, channel string, payload any) {
		watcher.Ingest(peerID, channel, payload)
	})
	watcher = installTestFleetWatcher(t, "m-away", asking, mesh)
	watcher.settleHosted = asking.runtime.settleHostedExit
	if err := mesh.AddRoute(home.machineID, "home", fleetclient.KindLAN, home.address, fleetclient.SourceManual); err != nil {
		t.Fatalf("add route to the home: %v", err)
	}
	if _, err := mesh.EnsureLink(context.Background(), home.machineID); err != nil {
		t.Fatalf("link: %v", err)
	}
	const childID = "wa-subagent-187-1"
	target, err := fleetid.New("m-home", fleetid.Subagent, childID)
	if err != nil {
		t.Fatal(err)
	}
	watcherID, err := fleetid.New("m-away", fleetid.Chat, "mcp-chat")
	if err != nil {
		t.Fatal(err)
	}
	row, err := hostingFleet.register("m-away", fleetlog.Registration{
		WatcherID: watcherID.String(), TargetID: target.String(), Label: "fails at once",
		WatcherChatID: "mcp-chat", WatcherTabID: "mcp-tab",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The child fails, and the push finds no monitor on the owner yet.
	if _, err := hostingFleet.Append(target.String(), fleetlog.KindExit, map[string]any{
		"outcome": "failed", "label": "fails at once",
		"receipt": subagentReceiptPayload(acp.SubagentReceipt{
			ReceiptID: "subagent-" + childID, SubagentID: childID, Label: "fails at once", Status: "failed",
			Error: "model control: Unsupported Codex model: gpt-6.1-sol",
		}),
	}); err != nil {
		t.Fatal(err)
	}
	lane, operation, _ := hostedRowOwnerOrigin(asking)
	asking.handler.agentControl.manager.RegisterHostedSubagent("mcp-tab", "mcp-chat", acp.HostedSubagentRow{
		SubagentID: childID, Label: "fails at once", HostMachine: "m-home", OriginLaneID: lane, OriginOperationID: operation,
	})
	// Only now does the owner record the monitor, from a reply with no slice in it.
	watcher.WatchRemoteSubagent(context.Background(), "m-home", map[string]any{
		"monitor": map[string]any{"targetId": target.String(), "ref": row.Ref, "label": "fails at once"},
	}, "mcp-tab", "mcp-chat", lane, operation)

	ended := awaitRailRow(t, asking, childID, func(row map[string]any) bool { return fieldString(row, "status") != "running" })
	if fieldString(ended, "status") != "failed" {
		t.Fatalf("the row: %#v", ended)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(chatNotices(t, asking, "mcp-chat")) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if notices := chatNotices(t, asking, "mcp-chat"); len(notices) != 1 {
		t.Fatalf("notices = %d, want exactly one", len(notices))
	}
	// And a monitor still owed a sync on an up link is asked again on a beat.
	watcher.mu.Lock()
	if watcher.lastResync == nil {
		watcher.lastResync = map[string]time.Time{}
	}
	watcher.lastResync["m-home"] = time.Now().Add(-time.Hour)
	watcher.mu.Unlock()
	watcher.OnLinkState(fleetclient.State{Peer: "m-home", Status: fleetclient.StatusUp})
	time.Sleep(fleetCatchUpDelay * 3)
	if notices := chatNotices(t, asking, "mcp-chat"); len(notices) != 1 {
		t.Fatalf("a later sync delivered it again: %d notices", len(notices))
	}
}

// A push reaches a watcher over a link the home itself dialled.
func TestAPushGoesOverTheLinkThisSideDialled(t *testing.T) {
	sent := []string{}
	pusher := &fleetPusher{
		send: func(string, string, any) int { return 0 },
		link: func(string) (func(context.Context, string, ...any) (any, error), bool) {
			return func(_ context.Context, channel string, args ...any) (any, error) {
				sent = append(sent, channel+":"+fieldString(firstMapArg(args), "channel"))
				return map[string]any{"ok": true}, nil
			}, true
		},
	}
	if got := pusher.push("m-away", fleetLogEventChannel, map[string]any{}); got != 1 {
		t.Fatalf("push = %d", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(sent) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if len(sent) != 1 || sent[0] != fleetPushChannel+":"+fleetLogEventChannel {
		t.Fatalf("sent = %v", sent)
	}
}

// Found on the live rig: the spawn answered with the run itself, the monitor path
// read that as a map with no id, and no remote subagent ever had a monitor. The
// exit stayed in its home's log and nobody was told.
func TestAHostTargetedSpawnGetsAMonitorFromTheRunItReturns(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	secret := newFleetTestSecret(t)
	home := serveMeshHome(t, "m-home", secret, harness.handler.agentControl)
	installTestFleetHome(t, "m-home", harness, home)
	answer := harness.handler.agentControl.monitorForwardedSpawn(fleetForwardedWork{
		Origin: fleetCommandOrigin{Machine: "m-away", ChatID: "mcp-chat", TabID: "mcp-tab"},
	}, acp.SubagentRun{ID: "wa-subagent-5-1", Label: "hosted"})
	monitor := mapFromAnyMain(mapFromAnyMain(answer)["monitor"])
	if !strings.Contains(fieldString(monitor, "targetId"), "wa-subagent-5-1") {
		t.Fatalf("a host-targeted spawn has no monitor: %#v", answer)
	}
}

// Found live: a link-up resync and a catch-up ran together and one exit was
// delivered twice.
func TestConcurrentSyncsDeliverOneExitOnce(t *testing.T) {
	asking := newStatelessMCPTestHarness(t)
	watched, err := fleetlog.OpenMonitors(t.TempDir(), fleetlog.MonitorsWatched)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	delivered := 0
	watcher := newFleetWatcher(fleetWatcherOptions{
		Self: "m-away", Monitors: watched, ChatExists: asking.runtime.chatExists, ChatTab: asking.runtime.chatTab,
		Deliver: func(fleetMonitorNotice) error {
			mu.Lock()
			delivered++
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			return nil
		},
	})
	row, err := watcher.Record(fleetlog.Registration{
		WatcherID: "wa:m-away/chat/mcp-chat", TargetID: "wa:m-home/subagent/wa-subagent-2-1",
		WatcherChatID: "mcp-chat", WatcherTabID: "mcp-tab",
	})
	if err != nil {
		t.Fatal(err)
	}
	slice := map[string]any{"events": []any{map[string]any{
		"seq": 1, "kind": fleetlog.KindExit, "payload": map[string]any{
			"receipt": subagentReceiptPayload(acp.SubagentReceipt{SubagentID: "wa-subagent-2-1", ReceiptID: "r", Status: "done"}),
		},
	}}}
	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			watcher.applySlice(context.Background(), "m-home", row.Ref, slice)
		}()
	}
	wait.Wait()
	if delivered != 1 {
		t.Fatalf("delivered %d times", delivered)
	}
}

// Found on the live rig: a sync adopted a job's monitor before the local record,
// with the empty tab the home has for a chat on another machine, and every notice
// through it was dropped by the admission fence. The row settled and nobody was told.
func TestANoticeThroughAMonitorAdoptedWithNoTabStillArrives(t *testing.T) {
	asking := newStatelessMCPTestHarness(t)
	watched, err := fleetlog.OpenMonitors(t.TempDir(), fleetlog.MonitorsWatched)
	if err != nil {
		t.Fatal(err)
	}
	watcher := newFleetWatcher(fleetWatcherOptions{
		Self: "m-away", Monitors: watched, Deliver: asking.runtime.deliverFleetMonitorNotice,
		ChatExists: asking.runtime.chatExists, ChatTab: asking.runtime.chatTab,
	})
	row, err := watcher.Record(fleetlog.Registration{
		WatcherID: "wa:m-away/chat/mcp-chat", TargetID: "wa:m-home/process/xwNoTabJob0001", WatcherChatID: "mcp-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	watcher.applySlice(context.Background(), "m-home", row.Ref, map[string]any{"events": []any{map[string]any{
		"seq": 1, "kind": fleetlog.KindExit, "payload": map[string]any{
			"entity": "process", "outcome": "failed", "label": "no tab",
			"processReceipt": spawnedWorkReceiptPayload(acp.SpawnedWorkReceipt{ReceiptID: "spawned-xwNoTabJob0001", TaskID: "xwNoTabJob0001", Label: "no tab", Status: "failed"}),
		},
	}}})
	if notices := chatNotices(t, asking, "mcp-chat"); len(notices) != 1 {
		t.Fatalf("notices = %d, want one", len(notices))
	}
}
