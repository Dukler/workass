package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"workass/internal/chat"
	"workass/internal/fleet"
	"workass/internal/fleetclient"
	"workass/internal/fleetid"
	"workass/internal/fleetlog"
)

// The operator-visible claim of P3, tested the only way it can be: two real
// daemons, a real TLS link, a real mock-provider subagent, and no renderer
// process anywhere.
//
// Until now a subagent asked for from a chat HERE and hosted THERE reported its
// completion into the chat on the machine that hosted it
// (docs/specs/fleet-runtime.md §1, `agent_control_remote.go:103`) — the chat
// that asked was never told, because a home had no way to tell it. This test is
// the sentence that was false: a machine-tagged subagent spawned from a chat on
// daemon P and hosted on H reports its completion INTO THE SPAWNING CHAT ON P.

// monitorMeshDialer is newMeshDialer with P3's event half wired: a peer's log
// pushes arrive on OnEvent, which is what P2 deliberately left unset.
func monitorMeshDialer(t *testing.T, machineID, secret string, onEvent func(string, string, any)) *fleetclient.Mesh {
	t.Helper()
	stateDir := t.TempDir()
	store, err := fleetclient.Open(stateDir)
	if err != nil {
		t.Fatalf("peer store: %v", err)
	}
	keys, err := fleet.Open(stateDir)
	if err != nil {
		t.Fatalf("fleet store: %v", err)
	}
	if _, err := keys.Join(secret, fleet.LocalOwner, "test"); err != nil {
		t.Fatalf("join fleet: %v", err)
	}
	mesh := fleetclient.New(fleetclient.Options{
		SelfMachineID: machineID, SelfName: machineID, Store: store, Keys: keys, Logf: t.Logf,
		Heartbeat: time.Minute, DialTimeout: 10 * time.Second, InvokeTimeout: 20 * time.Second,
		OnEvent: onEvent,
	})
	t.Cleanup(mesh.Close)
	return mesh
}

// installTestFleetHome gives one harness the home half of P3: an entity log, a
// held-monitor registry, the four monitor channels on its hub, and the manager
// observer that makes a subagent's life a log.
func installTestFleetHome(t *testing.T, machineID string, harness statelessMCPTestHarness, home *meshHome) *fleetHome {
	t.Helper()
	stateDir := t.TempDir()
	held, err := fleetlog.OpenMonitors(stateDir, fleetlog.MonitorsHeld)
	if err != nil {
		t.Fatalf("held registry: %v", err)
	}
	logs, err := fleetlog.Open(stateDir, fleetlog.Options{Logf: t.Logf, Owed: held.MinAcked})
	if err != nil {
		t.Fatalf("entity logs: %v", err)
	}
	service := newFleetHome(machineID, logs, held, home.hub.SendToPeer, t.Logf)
	if !service.Register(home.hub) {
		t.Fatal("the home did not register its monitor channels")
	}
	harness.handler.agentControl.fleetHome = service
	harness.handler.agentControl.selfMachineID = machineID
	harness.manager.SetSubagentLifecycleObserver(service.ObserveSubagent)
	return service
}

// installTestFleetWatcher gives one harness the watcher half: the watched
// registry, the notice projection into its own chats, and the budget.
func installTestFleetWatcher(t *testing.T, machineID string, harness statelessMCPTestHarness, mesh *fleetclient.Mesh) *fleetWatcher {
	t.Helper()
	watched, err := fleetlog.OpenMonitors(t.TempDir(), fleetlog.MonitorsWatched)
	if err != nil {
		t.Fatalf("watched registry: %v", err)
	}
	watcher := newFleetWatcher(fleetWatcherOptions{
		Self: machineID, Monitors: watched, Mesh: mesh,
		Deliver: harness.runtime.deliverFleetMonitorNotice, Budget: newFleetNoticeBudget(),
		ChatExists: harness.runtime.chatExists, Logf: t.Logf,
	})
	harness.handler.agentControl.fleetWatch = watcher
	harness.handler.agentControl.selfMachineID = machineID
	return watcher
}

// chatNotices reads every admitted notice row out of one chat's durable state,
// which is where a notice has to be if it is to survive a restart.
func chatNotices(t *testing.T, harness statelessMCPTestHarness, chatID string) []chat.LedgerEvent {
	t.Helper()
	state, ok := harness.runtime.Snapshot(chatID)
	if !ok {
		t.Fatalf("chat %s has no snapshot", chatID)
	}
	out := make([]chat.LedgerEvent, 0, 2)
	for _, event := range state.Ledger {
		if event.NoticeKind != "" {
			out = append(out, event)
		}
	}
	for _, entry := range state.Queue {
		if entry.Presentation.NoticeKind != "" {
			out = append(out, chat.LedgerEvent{
				Role: "user", Text: entry.Text, OperationID: entry.OperationID, LaneID: entry.LaneID,
				NoticeKind: entry.Presentation.NoticeKind, NoticeTitle: entry.Presentation.NoticeTitle,
				NoticeStatus: entry.Presentation.NoticeStatus,
			})
		}
	}
	if state.Foreground != nil && state.Foreground.Input.Presentation.NoticeKind != "" {
		entry := state.Foreground.Input
		out = append(out, chat.LedgerEvent{
			Role: "user", Text: entry.Text, OperationID: entry.OperationID, LaneID: entry.LaneID,
			NoticeKind: entry.Presentation.NoticeKind, NoticeTitle: entry.Presentation.NoticeTitle,
			NoticeStatus: entry.Presentation.NoticeStatus,
		})
	}
	return out
}

// §13 P3: "the remote-subagent-reports-to-my-chat case with no renderer".
func TestARemoteSubagentReportsIntoTheSpawningChatWithNoRendererAnywhere(t *testing.T) {
	secret := newFleetTestSecret(t)

	// H — the machine that will run the child.
	hosting := newStatelessMCPTestHarness(t)
	home := serveMeshHome(t, "m-home", secret, hosting.handler.agentControl)
	hostingFleet := installTestFleetHome(t, "m-home", hosting, home)

	// P — the machine whose chat asks. Its ids are deliberately the SAME local
	// ids as H's: a global id is its home plus its local id, so two chats named
	// `mcp-chat` on two machines are two entities, and anything that confused
	// them would deliver this child's outcome to the wrong chat.
	asking := newStatelessMCPTestHarness(t)
	var watcher *fleetWatcher
	mesh := monitorMeshDialer(t, "m-away", secret, func(peerID, channel string, payload any) {
		watcher.Ingest(peerID, channel, payload)
	})
	watcher = installTestFleetWatcher(t, "m-away", asking, mesh)
	if err := mesh.AddRoute(home.machineID, "home", fleetclient.KindLAN, home.address, fleetclient.SourceManual); err != nil {
		t.Fatalf("add route: %v", err)
	}
	// The renderer relay FAILS the test if it is used, which is how "with no
	// renderer anywhere" is asserted rather than asserted about.
	relay := &refusingRelay{t: t}
	asking.handler.agentControl.remoteChats = newFleetMeshRouter(mesh, relay, "m-away", t.Logf)

	// The ask: a chat on P spawns a subagent on H, naming H's chat the way an
	// agent reads it out of workass_list_chats.
	status, response := asking.request(t, http.MethodPost, map[string]any{
		"name": "workass_spawn_subagent",
		"arguments": map[string]any{
			"operation_id": "p3-remote-spawn-1",
			"tab_id":       "M~m-home~mcp-tab", "chat_id": "M~m-home~mcp-chat",
			"task": "reply with the deterministic mock result", "label": "remote reviewer",
		},
	})
	if status != http.StatusOK || response["error"] != nil {
		t.Fatalf("remote spawn status=%d response=%#v", status, response)
	}
	spawned := mapFromAnyMain(response["result"])
	childID := strings.TrimSpace(toString(spawned["id"]))
	if childID == "" {
		t.Fatalf("remote spawn result = %#v", spawned)
	}
	// The home answered with the monitor it had already registered, in the
	// global form, so the watcher's record names an entity rather than a string
	// that happens to look like one.
	monitor := mapFromAnyMain(spawned["monitor"])
	targetID := strings.TrimSpace(fieldString(monitor, "targetId"))
	expected, err := fleetid.New("m-home", fleetid.Subagent, childID)
	if err != nil {
		t.Fatal(err)
	}
	if targetID != expected.String() {
		t.Fatalf("the spawn's monitor target = %q, want %q", targetID, expected.String())
	}
	if held := hostingFleet.monitorsHeldFor("m-away"); len(held) != 1 || held[0].TargetID != targetID {
		t.Fatalf("the home holds %#v", held)
	}
	if watched := watcher.monitors.All(); len(watched) != 1 || watched[0].TargetID != targetID {
		t.Fatalf("the asking machine watches %#v", watched)
	}
	if watched := watcher.monitors.All(); watched[0].WatcherID != "wa:m-away/chat/mcp-chat" {
		t.Fatalf("the monitor's watcher = %q, which is not the chat that asked", watched[0].WatcherID)
	}

	// The child runs on H and settles there. Its outcome arrives HERE.
	deadline := time.Now().Add(20 * time.Second)
	var notices []chat.LedgerEvent
	for {
		notices = chatNotices(t, asking, "mcp-chat")
		if len(notices) > 0 {
			break
		}
		if time.Now().After(deadline) {
			state, _ := hosting.runtime.Snapshot("mcp-chat")
			t.Fatalf("the remote child never reported into the chat that asked; the host's own chat has %d ledger row(s)", len(state.Ledger))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(notices) != 1 {
		t.Fatalf("the chat that asked got %d notice(s): %#v", len(notices), notices)
	}
	notice := notices[0]
	// Never a user message: it is an agent-origin notice row with the subagent
	// notice card, exactly as a local child's completion is (the 2026-10-16 law
	// and the first of the five aa53975e anti-regressions).
	if notice.NoticeKind != "subagent" || notice.NoticeStatus != "done" || notice.NoticeTitle != "remote reviewer" {
		t.Fatalf("the notice = kind %q status %q title %q", notice.NoticeKind, notice.NoticeStatus, notice.NoticeTitle)
	}
	if !strings.HasPrefix(notice.Text, "<task-notification>") || !strings.Contains(notice.Text, "<status>completed</status>") {
		t.Fatalf("the notice body is not a task notification:\n%s", notice.Text)
	}
	// No imperative, no instruction, no language direction — the second
	// anti-regression, asserted on the exact bytes the model receives.
	lowered := strings.ToLower(notice.Text)
	for _, imperative := range []string{"you must", "you should", "please ", "continue", "respond in", "reply in", "answer in"} {
		if strings.Contains(lowered, imperative) {
			t.Fatalf("the notice instructs the model (%q):\n%s", imperative, notice.Text)
		}
	}
	if !strings.Contains(notice.Text, childID) {
		t.Fatalf("the notice does not name the child that settled:\n%s", notice.Text)
	}

	// The projection-parity gate for the migrated subsystem (§12's M2): the row
	// a monitor admits is the row the local completion path admits, to the byte
	// and to the operation id. It is not a copy of that path — it IS that path,
	// so the only thing worth asserting is that nothing was transformed on the
	// way across the link.
	hostRecord, err := hostingFleet.logs.Since(targetID, 0, 0)
	if err != nil {
		t.Fatalf("read the home's log: %v", err)
	}
	last := hostRecord.Events[len(hostRecord.Events)-1]
	replayed := subagentReceiptFromPayload(mapFromAnyMain(last.Payload["receipt"]))
	replayed.OriginLaneID, replayed.OriginOperationID = string(notice.LaneID), ""
	if want := formatSubagentCompletion(replayed); notice.Text != want {
		t.Fatalf("the admitted notice is not byte-identical to the local projection:\n got %q\nwant %q", notice.Text, want)
	}
	if want := subagentCompletionOperationID("mcp-tab", "mcp-chat", childID); notice.OperationID != want {
		t.Fatalf("the admission key = %q, want the local path's %q", notice.OperationID, want)
	}

	// The host's own chat is NOT told: the outcome is owed to the chat that
	// asked, and delivering it twice would be the old bug with a new transport.
	if hostNotices := chatNotices(t, hosting, "mcp-chat"); len(hostNotices) != 0 {
		t.Fatalf("the hosting chat was also notified: %#v", hostNotices)
	}
	// Exactly once, across a redelivery: a resync from the cursor adds nothing.
	if err := watcher.Resync(context.Background(), "m-home"); err != nil {
		t.Fatalf("resync: %v", err)
	}
	if after := chatNotices(t, asking, "mcp-chat"); len(after) != 1 {
		t.Fatalf("a resync re-delivered the outcome: %#v", after)
	}
	// The home's log is the authoritative history — suppressing the host's own
	// notice is about the notice, never about the record.
	slice, err := hostingFleet.logs.Since(targetID, 0, 0)
	if err != nil {
		t.Fatalf("read the home's log: %v", err)
	}
	if len(slice.Events) == 0 || slice.Events[len(slice.Events)-1].Kind != fleetlog.KindExit {
		t.Fatalf("the home's log = %#v", slice.Events)
	}
	logged := mapFromAnyMain(slice.Events[len(slice.Events)-1].Payload["receipt"])
	if fieldString(logged, "label") != "remote reviewer" || strings.TrimSpace(fieldString(logged, "result")) == "" {
		t.Fatalf("the host's own terminal record = %#v", logged)
	}
	if origin := fieldString(slice.Events[len(slice.Events)-1].Payload, "originMachine"); origin != "m-away" {
		t.Fatalf("the exit event's origin stamp = %q", origin)
	}
	// And the watcher acknowledged exactly what it applied, which is what lets
	// that terminal event eventually leave the home's log.
	ackDeadline := time.Now().Add(10 * time.Second)
	for {
		acked, monitored := hostingFleet.monitors.MinAcked(targetID)
		if monitored && acked == slice.Head {
			break
		}
		if time.Now().After(ackDeadline) {
			t.Fatalf("the home is still owed through %d of %d", acked, slice.Head)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if relay.calls.Load() != 0 {
		t.Fatalf("the renderer relay was used %d time(s)", relay.calls.Load())
	}

	// The cursor the asking daemon would read back after a restart: durable, at
	// the head, and marked consumed, which is what makes the delivery
	// exactly-once rather than at-least-once.
	restored := watcher.monitors.All()[0]
	if restored.AppliedSeq != slice.Head || !restored.Consumed {
		t.Fatalf("the durable cursor after delivery = %#v", restored)
	}
}

// The envelope carried the origin, and the origin is what the host wrote into
// its own durable record of the child — so a replay of the same spawn produces
// one child, one monitor and one notice.
func TestOneOperationIDReplayedProducesOneRemoteChildAndOneNotice(t *testing.T) {
	secret := newFleetTestSecret(t)
	hosting := newStatelessMCPTestHarness(t)
	home := serveMeshHome(t, "m-home", secret, hosting.handler.agentControl)
	installTestFleetHome(t, "m-home", hosting, home)

	asking := newStatelessMCPTestHarness(t)
	var watcher *fleetWatcher
	mesh := monitorMeshDialer(t, "m-away", secret, func(peerID, channel string, payload any) {
		watcher.Ingest(peerID, channel, payload)
	})
	watcher = installTestFleetWatcher(t, "m-away", asking, mesh)
	if err := mesh.AddRoute(home.machineID, "home", fleetclient.KindLAN, home.address, fleetclient.SourceManual); err != nil {
		t.Fatalf("add route: %v", err)
	}
	relay := &refusingRelay{t: t}
	asking.handler.agentControl.remoteChats = newFleetMeshRouter(mesh, relay, "m-away", t.Logf)

	arguments := map[string]any{
		"operation_id": "p3-replayed-spawn",
		"tab_id":       "M~m-home~mcp-tab", "chat_id": "M~m-home~mcp-chat",
		"task": "reply with the deterministic mock result", "label": "replayed reviewer",
	}
	ids := map[string]struct{}{}
	for attempt := 0; attempt < 2; attempt++ {
		status, response := asking.request(t, http.MethodPost, map[string]any{
			"name": "workass_spawn_subagent", "arguments": arguments,
		})
		if status != http.StatusOK || response["error"] != nil {
			t.Fatalf("attempt %d status=%d response=%#v", attempt, status, response)
		}
		ids[toString(mapFromAnyMain(response["result"])["id"])] = struct{}{}
	}
	if len(ids) != 1 {
		t.Fatalf("a replayed operation id produced %d child(ren): %#v", len(ids), ids)
	}

	deadline := time.Now().Add(20 * time.Second)
	for {
		notices := chatNotices(t, asking, "mcp-chat")
		if len(notices) == 1 {
			break
		}
		if len(notices) > 1 {
			encoded, _ := json.Marshal(notices)
			t.Fatalf("a replayed spawn notified twice: %s", encoded)
		}
		if time.Now().After(deadline) {
			t.Fatal("the replayed remote child never reported")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if watched := watcher.monitors.All(); len(watched) != 1 {
		t.Fatalf("a replayed spawn produced %d monitor(s)", len(watched))
	}
	if relay.calls.Load() != 0 {
		t.Fatalf("the renderer relay was used %d time(s)", relay.calls.Load())
	}
}

// The two monitor events that have no local equivalent, admitted through a real
// chat actor: an `attention` raised by a subagent on another machine, and the
// `home_down` only the watching daemon can synthesize.
func TestAttentionAndHomeDownAreAdmittedAsNoticesAndNeverAsOutcomes(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	laneID, parentOperationID := harness.runtime.watcherOrigin("mcp-tab", "mcp-chat")
	if laneID == "" {
		t.Fatal("the fixture chat has no lane to admit a notice on")
	}
	attention := fleetMonitorNotice{
		Kind: fleetlog.KindAttention, TargetID: "wa:m-home/subagent/wa-subagent-7",
		ChatID: "mcp-chat", TabID: "mcp-tab", LaneID: laneID, OriginOperationID: parentOperationID,
		Label: "remote reviewer", AttentionKind: "permission", AttentionText: "Permission required: write a file",
	}
	if err := harness.runtime.deliverFleetMonitorNotice(attention); err != nil {
		t.Fatalf("admit an attention: %v", err)
	}
	dark := fleetMonitorNotice{
		Kind: fleetMonitorHomeDown, Home: "m-home", Since: time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC),
		TargetID: "wa:m-home/subagent/wa-subagent-7", ChatID: "mcp-chat", TabID: "mcp-tab",
		LaneID: laneID, OriginOperationID: parentOperationID, Label: "remote reviewer",
		LastKnown: []string{"remote reviewer", "second child"},
	}
	if err := harness.runtime.deliverFleetMonitorNotice(dark); err != nil {
		t.Fatalf("admit a home_down: %v", err)
	}
	notices := chatNotices(t, harness, "mcp-chat")
	if len(notices) != 2 {
		t.Fatalf("the chat got %d notice(s): %#v", len(notices), notices)
	}
	byStatus := map[string]chat.LedgerEvent{}
	for _, notice := range notices {
		byStatus[notice.NoticeStatus] = notice
	}
	raised, ok := byStatus["attention"]
	if !ok {
		t.Fatalf("no attention notice: %#v", notices)
	}
	if raised.NoticeKind != "subagent" || raised.NoticeTitle != "remote reviewer" {
		t.Fatalf("the attention row = %#v", raised)
	}
	if !strings.Contains(raised.Text, "<status>attention</status>") || !strings.Contains(raised.Text, "needs a decision") {
		t.Fatalf("the attention body:\n%s", raised.Text)
	}
	unreachable, ok := byStatus[fleetNoticeStatusHomeDown]
	if !ok {
		t.Fatalf("no home_down notice: %#v", notices)
	}
	// The rule: unknown is not an outcome. Not in the status the renderer reads,
	// and not in the words the model reads.
	if unreachable.NoticeStatus == "done" || unreachable.NoticeStatus == "failed" {
		t.Fatalf("home_down was admitted as an outcome: %q", unreachable.NoticeStatus)
	}
	if !strings.Contains(unreachable.Text, "<status>unknown</status>") {
		t.Fatalf("the home_down body:\n%s", unreachable.Text)
	}
	if !strings.Contains(unreachable.Text, "second child") {
		t.Fatalf("the home_down body does not list what is unknown:\n%s", unreachable.Text)
	}

	// Replay is a no-op: the admission key derives from durable facts, so a
	// redelivery after a crash cannot double-deliver.
	for _, notice := range []fleetMonitorNotice{attention, dark} {
		if err := harness.runtime.deliverFleetMonitorNotice(notice); err != nil {
			t.Fatalf("replay: %v", err)
		}
	}
	if after := chatNotices(t, harness, "mcp-chat"); len(after) != 2 {
		t.Fatalf("a replayed notice was admitted twice: %#v", after)
	}

	// Neither is ever a user message, and neither instructs the model.
	for _, notice := range notices {
		if notice.Role != "user" {
			continue
		}
		lowered := strings.ToLower(notice.Text)
		for _, imperative := range []string{"you must", "you should", "please ", "respond in", "reply in"} {
			if strings.Contains(lowered, imperative) {
				t.Fatalf("a fleet notice instructs the model (%q):\n%s", imperative, notice.Text)
			}
		}
	}

	// A chat that is gone acknowledges and drops rather than being recreated —
	// the engine-death anti-regression, applied to a notice from another machine.
	missing := attention
	missing.ChatID, missing.TargetID = "no-such-chat", "wa:m-home/subagent/wa-subagent-9"
	if err := harness.runtime.deliverFleetMonitorNotice(missing); err == nil {
		t.Fatal("a notice for a chat that does not exist was admitted")
	}
	// home_up is not a row: the backlog that follows it is the news.
	if err := harness.runtime.deliverFleetMonitorNotice(fleetMonitorNotice{
		Kind: fleetMonitorHomeUp, Home: "m-home", ChatID: "mcp-chat", TabID: "mcp-tab",
	}); err != nil {
		t.Fatalf("home_up: %v", err)
	}
	if after := chatNotices(t, harness, "mcp-chat"); len(after) != 2 {
		t.Fatalf("home_up became a row: %#v", after)
	}
}
