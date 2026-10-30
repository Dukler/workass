package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"workass/internal/chat"
	"workass/internal/fleetclient"
	"workass/internal/fleetid"
	"workass/internal/procchild"
)

// The B6 end-to-end: a supervised job REPORTS, and the chat that asked is told.
//
// This is the test the first live two-machine run should have had. Four jobs
// were started on duklerver from a chat on the Mac, all four returned ok with
// the right owner, all four ran to completion — and not one notice ever
// reached the owning chat, because the only record of what had happened was a
// JSONL spool the daemon never read (it had been written to /tmp/state, which
// was itself only possible because nothing was reporting). The reporting path
// now runs the other way round, and this asserts the whole of it with two real
// daemons and a real TLS link:
//
//	`workass run` on H -> report over the Agent API -> acknowledged by H's
//	daemon -> H's entity log -> the monitor H offered P -> the mesh -> P's
//	watcher -> exactly one notice in the chat on P that asked.
//
// One case per terminal state, because each reaches terminal through a
// different branch and the live test found the refused one missing entirely:
// settled, failed, timed_out, cancelled and lost.

// supervisorReport is one `workass run` report against the daemon on the
// machine that runs the job, through the same tool endpoint the real
// supervisor calls. It carries the registration facts and the batch, and it
// returns the daemon's acknowledgement.
func supervisorReport(
	t *testing.T, harness statelessMCPTestHarness, workID, label, owner string,
	supervisorPID int, events []procchild.Event,
) map[string]any {
	t.Helper()
	// The SUPERVISOR's own payload, built by the supervisor's own constructor
	// rather than by hand here. Hand-built arguments are why this test passed
	// while the live run failed: it sent a payload the daemon accepts and the
	// real `workass run` sent one the argument validator refused whole.
	arguments := procchild.ReportArguments(procchild.Report{
		Spec: procchild.Spec{
			OperationID: "op-" + workID, Label: label, Owner: owner,
			Command: []string{"python", "train.py"},
		},
		ChildID:       workID,
		OutputFile:    harness.manager.StateDir() + "/external-work/" + workID + ".output",
		SupervisorPID: supervisorPID,
		Events:        events,
		Heartbeat:     procchild.HeartbeatInterval,
	})
	status, response := harness.request(t, http.MethodPost, map[string]any{
		"name": "workass_register_process_child", "arguments": arguments,
	})
	if status != http.StatusOK || response["error"] != nil {
		t.Fatalf("report for %s status=%d response=%#v", workID, status, response)
	}
	result := mapFromAnyMain(response["result"])
	if result["ok"] != true {
		t.Fatalf("report for %s = %#v", workID, result)
	}
	return result
}

func TestEveryTerminalStateOfARemoteJobProducesExactlyOneNoticeInTheChatThatAsked(t *testing.T) {
	secret := newFleetTestSecret(t)

	// H — the machine with the files, the pids and the job.
	hosting := newStatelessMCPTestHarness(t)
	home := serveMeshHome(t, "m-home", secret, hosting.handler.agentControl)
	hostingFleet := installTestFleetHome(t, "m-home", hosting, home)
	hosting.manager.SetProcessLifecycleObserver(hostingFleet.ObserveProcess)

	// P — the machine whose chat asked, and which is owed every outcome. It
	// serves a home of its own, because this direction is the one P3 did not
	// need: nobody on P ever CALLED for these jobs, so H has to offer P the
	// monitor, and an offer is an inbound call on P.
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
	// And H dials P, which is how the offer gets there.
	hostingMesh := monitorMeshDialer(t, "m-home", secret, func(string, string, any) {})
	if err := hostingMesh.AddRoute(away.machineID, "away", fleetclient.KindLAN, away.address, fleetclient.SourceManual); err != nil {
		t.Fatalf("add route to the asking machine: %v", err)
	}
	hosting.handler.agentControl.fleetMesh = hostingMesh
	// And P dials H, which is the direction the EVENTS take: a home pushes to
	// the peers connected to it, so the pair really does need its duplex link
	// up — the same link §4 requires and the same one B8 was about.
	if _, err := mesh.EnsureLink(context.Background(), home.machineID); err != nil {
		t.Fatalf("the asking machine could not link to the home: %v", err)
	}
	// The renderer relay FAILS the test if it is used: the app is closed, which
	// is the only condition under which the fleet runtime is worth having.
	relay := &refusingRelay{t: t}
	asking.handler.agentControl.remoteChats = newFleetMeshRouter(mesh, relay, "m-away", t.Logf)

	owner := "wa:m-away/chat/mcp-chat"
	now := time.Now().UTC()
	// A supervisor that is really there, so a running row is honest.
	live := os.Getpid()

	for _, testCase := range []struct {
		name     string
		workID   string
		label    string
		events   []procchild.Event
		lost     bool
		status   string
		exitCode int
		contains string
	}{{
		name: "a job that finished", workID: "xwe2eok000001", label: "e2e settled",
		status: "done", exitCode: 0, contains: "exit 0",
		events: []procchild.Event{
			{Seq: 1, At: now, Kind: procchild.KindCreated, Payload: map[string]any{"label": "e2e settled"}},
			{Seq: 2, At: now, Kind: procchild.KindStarted, Payload: map[string]any{"pid": 4242}},
			{Seq: 3, At: now, Kind: procchild.KindSettled, Payload: map[string]any{
				"outcome": procchild.OutcomeSettled, "exitCode": 0, "summary": "Finished with exit 0 after 4s"}},
		},
	}, {
		// The case the live test had NO path for. A refusal starts nothing, so
		// the old design told nobody — which from the asking chat's side is
		// indistinguishable from a job that vanished.
		name: "a job refused at preflight", workID: "xwe2eref00001", label: "e2e refused",
		status: "failed", exitCode: procchild.ExitPreflightRefused, contains: "preflight refused",
		events: []procchild.Event{
			{Seq: 1, At: now, Kind: procchild.KindCreated, Payload: map[string]any{"label": "e2e refused"}},
			{Seq: 2, At: now, Kind: procchild.KindSettled, Payload: map[string]any{
				"outcome": procchild.OutcomeFailed, "exitCode": procchild.ExitPreflightRefused,
				"summary": "preflight refused: inputs: /srv/in.bin is missing"}},
		},
	}, {
		name: "a job stopped at its own ceiling", workID: "xwe2etmo00001", label: "e2e timed out",
		status: "timed_out", exitCode: procchild.ExitTimedOut, contains: "timed out",
		events: []procchild.Event{
			{Seq: 1, At: now, Kind: procchild.KindCreated, Payload: map[string]any{"label": "e2e timed out"}},
			{Seq: 2, At: now, Kind: procchild.KindStarted, Payload: map[string]any{"pid": 4243}},
			{Seq: 3, At: now, Kind: procchild.KindCancelRequested, Payload: map[string]any{
				"by": "max_runtime", "outcome": procchild.OutcomeTimedOut}},
			{Seq: 4, At: now, Kind: procchild.KindSettled, Payload: map[string]any{
				"outcome": procchild.OutcomeTimedOut, "exitCode": procchild.ExitTimedOut,
				"summary": "Timed out after 1h0m0s"}},
		},
	}, {
		// The supervisor is gone and reported no outcome, so this machine
		// reaches the one verdict only it can. Absence is evidence.
		name: "a job whose supervisor vanished", workID: "xwe2elost0001", label: "e2e lost",
		lost: true, status: "lost", contains: "unknown",
		events: []procchild.Event{
			{Seq: 1, At: now, Kind: procchild.KindCreated, Payload: map[string]any{"label": "e2e lost"}},
			{Seq: 2, At: now, Kind: procchild.KindStarted, Payload: map[string]any{"pid": 4244}},
		},
	}} {
		t.Run(testCase.name, func(t *testing.T) {
			pid := live
			if testCase.lost {
				pid = deadSupervisorPIDForTest(t)
			}
			// The supervisor reports its creation first and is acknowledged,
			// which is what registers the row and offers P the monitor. A job
			// that finishes while that first reply is still travelling still
			// has somewhere to report, because the monitor already exists.
			first := supervisorReport(t, hosting, testCase.workID, testCase.label, owner, pid, testCase.events[:1])
			if ackThroughOf(first) != 1 {
				t.Fatalf("the creation must be acknowledged, got %#v", first["ackThrough"])
			}
			// The monitor exists at BOTH ends before the job can possibly end,
			// which is what gives a job that finishes in the same second
			// somewhere to report.
			if held := hostingFleet.monitorsHeldFor("m-away"); len(held) == 0 {
				t.Fatalf("the home holds no monitor for the asking machine: %#v", held)
			}
			awaitWatchedMonitor(t, watcher, testCase.workID)
			rest := supervisorReport(t, hosting, testCase.workID, testCase.label, owner, pid, testCase.events)
			if ackThroughOf(rest) != len(testCase.events) {
				t.Fatalf("the batch must be acknowledged through %d, got %#v", len(testCase.events), rest["ackThrough"])
			}
			if testCase.lost {
				// Nothing more will ever be reported, so the verdict comes
				// from the reconcile pass and the pid, past the grace.
				hosting.manager.ReconcileProcessChildren(time.Now().UTC())
				hosting.manager.ReconcileProcessChildren(time.Now().UTC().Add(2 * time.Hour))
			}

			// The event is at the HOME first: one entity log, which is what a
			// watcher then reads. Asserted separately from the notice so a
			// failure says which half broke.
			target := hostingFleet.EntityID(fleetid.Process, testCase.workID)
			slice, err := hostingFleet.logs.Since(target, 0, 0)
			if err != nil {
				t.Fatalf("the home's entity log for %s: %v", target, err)
			}
			if len(slice.Events) == 0 {
				t.Fatalf("the home recorded nothing for %s; a job's end has to be an entry in its own log", target)
			}
			notice := awaitOneProcessNotice(t, asking, testCase.label)
			if notice.NoticeStatus != testCase.status {
				t.Fatalf("notice status = %q, want %q", notice.NoticeStatus, testCase.status)
			}
			if notice.NoticeTitle != testCase.label {
				// The label is the only name the chat ever shows.
				t.Fatalf("notice title = %q, want %q", notice.NoticeTitle, testCase.label)
			}
			if !strings.Contains(strings.ToLower(notice.Text), testCase.contains) {
				t.Fatalf("the notice does not say what happened (%q):\n%s", testCase.contains, notice.Text)
			}
			if testCase.exitCode != 0 && !strings.Contains(notice.Text, "exit") {
				// The exit code is what a finished job is judged by, and it
				// crosses the wire as a string. The first version of the
				// payload reader dropped every one of them in silence.
				t.Fatalf("the notice must carry the exit evidence:\n%s", notice.Text)
			}
			// A completion reaches a chat as a task notification and never as a
			// user message (the 2026-10-16 law).
			if !strings.HasPrefix(notice.Text, "<task-notification>") {
				t.Fatalf("the notice is not a task notification:\n%s", notice.Text)
			}
		})
	}

	if relay.calls.Load() != 0 {
		t.Fatalf("the renderer relay was used %d time(s); the whole point is that the app is closed", relay.calls.Load())
	}
}

// awaitWatchedMonitor waits for the OFFER to land: H tells P what it is owed,
// because nobody on P ever called for this job and so P has nothing to learn
// from a reply.
func awaitWatchedMonitor(t *testing.T, watcher *fleetWatcher, workID string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		for _, row := range watcher.monitors.All() {
			if strings.Contains(row.TargetID, workID) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the asking machine was never offered a monitor for %s; it watches %#v", workID, watcher.monitors.All())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ackThroughOf reads the acknowledgement off a report's answer. It goes
// through JSON on the way, so the number arrives as a float — which is exactly
// what the real supervisor's client has to cope with too.
func ackThroughOf(result map[string]any) int { return intField(result, "ackThrough") }

// deadSupervisorPIDForTest is a pid that is certainly gone: this test's own
// child, started and reaped, so the number is real and the process is not.
func deadSupervisorPIDForTest(t *testing.T) int {
	t.Helper()
	process, err := os.StartProcess("/usr/bin/true", []string{"true"}, &os.ProcAttr{})
	if err != nil {
		t.Skipf("cannot start a throwaway process on this host: %v", err)
	}
	if _, err := process.Wait(); err != nil {
		t.Fatal(err)
	}
	return process.Pid
}

// awaitOneProcessNotice waits for exactly one notice naming one job, and fails
// if a second one arrives after it.
func awaitOneProcessNotice(t *testing.T, harness statelessMCPTestHarness, label string) chat.LedgerEvent {
	t.Helper()
	// Matched on the LABEL, because the label is the only name the chat ever
	// shows and therefore the only thing a reader could match on either.
	matches := func() []chat.LedgerEvent {
		found := make([]chat.LedgerEvent, 0, 2)
		for _, notice := range chatNotices(t, harness, "mcp-chat") {
			if notice.NoticeTitle == label {
				found = append(found, notice)
			}
		}
		return found
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		matching := matches()
		if len(matching) > 1 {
			encoded, _ := json.Marshal(matching)
			t.Fatalf("%q notified %d times: %s", label, len(matching), encoded)
		}
		if len(matching) == 1 {
			// Held briefly, because "exactly one" is a claim about what happens
			// next as much as about what has happened: a second delivery of one
			// event is the regression this guards.
			time.Sleep(300 * time.Millisecond)
			if again := matches(); len(again) != 1 {
				t.Fatalf("%q notified %d times once settled", label, len(again))
			}
			return matching[0]
		}
		if time.Now().After(deadline) {
			encoded, _ := json.Marshal(chatNotices(t, harness, "mcp-chat"))
			t.Fatalf("%q never reported into the chat that asked; it holds: %s", label, encoded)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
