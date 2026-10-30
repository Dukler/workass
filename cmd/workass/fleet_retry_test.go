package main

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/fleetid"
	"workass/internal/fleetlog"
)

// A chat that refuses the first attempts, as a real actor does while it cannot
// admit. Admission is what the pair counts.
func refusingDeliver(refusals *atomic.Int64) func(*fleetWatcherOptions) {
	return func(options *fleetWatcherOptions) {
		deliver := options.Deliver
		options.Deliver = func(notice fleetMonitorNotice) error {
			if refusals.Add(-1) >= 0 {
				return errors.New("the chat that asked has no provider lane for this fleet notice")
			}
			return deliver(notice)
		}
	}
}

func TestARefusedRemoteNoticeKeepsItsCursorAndArrivesOnce(t *testing.T) {
	var refusals atomic.Int64
	refusals.Store(2)
	pair := newFleetTestPair(t, refusingDeliver(&refusals))
	pair.watcher.retryBase = time.Hour // this test asks by hand
	defer pair.watcher.stopRetries()
	pair.setLink(true)
	pair.appendExit(t, "done", "ok")
	for attempt := 0; attempt < 2; attempt++ {
		row, _ := pair.watcher.monitors.Get(pair.ref)
		if row.AppliedSeq != 0 || row.Consumed {
			t.Fatalf("a refused notice moved the cursor: %+v", row)
		}
		if len(pair.notices()) != 0 {
			t.Fatalf("nothing was admitted yet: %+v", pair.notices())
		}
		if held, _ := pair.home.monitors.Get(pair.ref); held.AckedSeq != 0 {
			t.Fatalf("a refused notice was acknowledged to its home: %+v", held)
		}
		if err := pair.watcher.Resync(t.Context(), testHomeMachine); err != nil {
			t.Fatal(err)
		}
	}
	// All of it inside the 60 s window, which must not take a retry for a duplicate.
	if notices := pair.notices(); len(notices) != 1 {
		t.Fatalf("want one notice after the refusals, got %+v", notices)
	}
	row, _ := pair.watcher.monitors.Get(pair.ref)
	if row.AppliedSeq != 1 || !row.Consumed {
		t.Fatalf("an admitted notice must consume its monitor: %+v", row)
	}
	if held, _ := pair.home.monitors.Get(pair.ref); held.AckedSeq != 1 {
		t.Fatalf("an admitted notice must be acknowledged: %+v", held)
	}
	if err := pair.watcher.Resync(t.Context(), testHomeMachine); err != nil {
		t.Fatal(err)
	}
	if notices := pair.notices(); len(notices) != 1 {
		t.Fatalf("a later sync delivered it again: %+v", notices)
	}
}

// selfHomedPair is one daemon that is both home and watcher, wired as the daemon is.
func selfHomedPair(t *testing.T, opts ...func(*fleetWatcherOptions)) *fleetTestPair {
	t.Helper()
	pair := newFleetTestPair(t, append([]func(*fleetWatcherOptions){func(options *fleetWatcherOptions) {
		options.Self = testHomeMachine
	}}, opts...)...)
	pair.home.SetLocalDelivery(pair.watcher.IngestLocalEvent)
	pair.watcher.localHome = pair.home
	return pair
}

func localJobMonitor(t *testing.T, pair *fleetTestPair) (string, fleetlog.Registration) {
	t.Helper()
	watcherID, err := fleetid.New(testHomeMachine, fleetid.Chat, "local-chat-1")
	if err != nil {
		t.Fatal(err)
	}
	target := pair.home.EntityID(fleetid.Process, testProcessLocal)
	row, err := pair.home.register(testHomeMachine, fleetlog.Registration{
		WatcherID: watcherID.String(), TargetID: target, Label: "local job",
		WatcherChatID: "local-chat-1", WatcherTabID: "local-tab-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return target, row
}

func TestARefusedLocalNoticeRetriesByItselfAndArrivesOnce(t *testing.T) {
	var refusals atomic.Int64
	refusals.Store(2)
	pair := selfHomedPair(t, refusingDeliver(&refusals))
	pair.watcher.retryBase = 5 * time.Millisecond
	defer pair.watcher.stopRetries()
	target, row := localJobMonitor(t, pair)
	pair.watcher.WatchLocal(row)
	if _, err := pair.home.Append(target, fleetlog.KindExit,
		processExitPayload(fleetlog.OutcomeSettled, "local job", 0, "Finished")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(pair.notices()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if notices := pair.notices(); len(notices) != 1 {
		t.Fatalf("want one notice with no link and no beat to prompt it, got %+v", notices)
	}
	time.Sleep(40 * time.Millisecond)
	if notices := pair.notices(); len(notices) != 1 {
		t.Fatalf("delivered twice: %+v", notices)
	}
	if held, _ := pair.home.monitors.Get(row.Ref); held.AckedSeq != 1 {
		t.Fatalf("the home was not told: %+v", held)
	}
}

// The first report of a very short job can carry its end, ahead of the monitor.
func TestALocalJobThatEndedBeforeItsMonitorStillTellsItsChat(t *testing.T) {
	pair := selfHomedPair(t)
	target, _ := localJobMonitor(t, pair)
	if _, err := pair.home.Append(target, fleetlog.KindExit,
		processExitPayload(fleetlog.OutcomeSettled, "local job", 0, "Finished")); err != nil {
		t.Fatal(err)
	}
	if len(pair.notices()) != 0 {
		t.Fatalf("no watcher half yet: %+v", pair.notices())
	}
	_, row := localJobMonitor(t, pair)
	pair.watcher.WatchLocal(row)
	// Every later report of the job registers again.
	pair.watcher.WatchLocal(row)
	if notices := pair.notices(); len(notices) != 1 {
		t.Fatalf("want exactly one notice, got %+v", notices)
	}
}

func TestALocalJobsEndDoesNotPassThroughTheHostedRowPath(t *testing.T) {
	settled := 0
	pair := selfHomedPair(t, func(options *fleetWatcherOptions) {
		options.SettleHosted = func(fleetlog.Registration, fleetlog.Event) { settled++ }
	})
	target, row := localJobMonitor(t, pair)
	pair.watcher.WatchLocal(row)
	if _, err := pair.home.Append(target, fleetlog.KindExit,
		processExitPayload(fleetlog.OutcomeSettled, "local job", 0, "Finished")); err != nil {
		t.Fatal(err)
	}
	if settled != 0 {
		t.Fatalf("a local job was settled as if another machine ran it")
	}
}

// What an earlier build left: monitors with only the home's half.
func TestBootAdoptsARunningLocalJobAndRetiresAFinishedOne(t *testing.T) {
	pair := selfHomedPair(t)
	target, row := localJobMonitor(t, pair)
	pair.watcher.AdoptHeldLocal(pair.home)
	if _, watched := pair.watcher.monitors.Get(row.Ref); !watched {
		t.Fatalf("a job still running must be watched from now on")
	}
	if _, err := pair.home.Append(target, fleetlog.KindExit,
		processExitPayload(fleetlog.OutcomeSettled, "local job", 0, "Finished")); err != nil {
		t.Fatal(err)
	}
	if notices := pair.notices(); len(notices) != 1 {
		t.Fatalf("an adopted job tells its chat once, got %+v", notices)
	}

	old := selfHomedPair(t)
	oldTarget, oldRow := localJobMonitor(t, old)
	if _, err := old.home.Append(oldTarget, fleetlog.KindExit,
		processExitPayload(fleetlog.OutcomeSettled, "local job", 0, "Finished")); err != nil {
		t.Fatal(err)
	}
	old.watcher.AdoptHeldLocal(old.home)
	if notices := old.notices(); len(notices) != 0 {
		t.Fatalf("a job that ended before this build must not notify now: %+v", notices)
	}
	if held, _ := old.home.monitors.Get(oldRow.Ref); held.AckedSeq != 1 {
		t.Fatalf("its log entry must be free to leave: %+v", held)
	}
}

func TestAHomeDownNoticeARefusalLosesIsSaidAgain(t *testing.T) {
	var refusals atomic.Int64
	refusals.Store(1)
	pair := newFleetTestPair(t, refusingDeliver(&refusals))
	state := pair.link.States()[0]
	state.Status, state.Since = "down", pair.now()
	pair.advance(fleetHomeDownThreshold + time.Minute)
	pair.watcher.OnLinkState(state)
	if len(pair.notices()) != 0 {
		t.Fatalf("that attempt was refused: %+v", pair.notices())
	}
	pair.watcher.OnLinkState(state)
	pair.watcher.OnLinkState(state)
	if notices := pair.notices(); len(notices) != 1 || notices[0].Kind != fleetMonitorHomeDown {
		t.Fatalf("want it said once more and then latched, got %+v", notices)
	}
}

// Found live: a Mac with no jobs of its own told a person duklerver had none,
// with a job running there. The machine asked about is the one that answers.
func TestAnUpdateStatusSaysWhatRunsOnTheMachineItIsAbout(t *testing.T) {
	handler := &agentControlHandler{manager: acp.NewManager(acp.Options{StateDir: t.TempDir()}), selfMachineID: "m-self"}
	hosted := map[string]any{"total": 1, "summary": "applying an update now WOULD KILL 1 running job(s)"}
	remote := handler.withRunningJobs("m-host", map[string]any{"machineId": "m-host", "runningJobs": hosted})
	jobs := mapFromAnyMain(mapFromAnyMain(remote)["runningJobs"])
	if jobs["total"] != 1 {
		t.Fatalf("this machine's jobs replaced the host's: %#v", remote)
	}
	own := mapFromAnyMain(mapFromAnyMain(handler.withRunningJobs("m-self", map[string]any{"machineId": "m-self"}))["runningJobs"])
	if own["total"] != 0 || own["summary"] == nil {
		t.Fatalf("a machine's own status must state its jobs: %#v", own)
	}
	// A host out there today says it only under diagnostics.
	nested := handler.withRunningJobs("m-host", map[string]any{"machineId": "m-host", "diagnostics": map[string]any{"runningJobs": hosted}})
	if mapFromAnyMain(mapFromAnyMain(nested)["runningJobs"])["total"] != 1 {
		t.Fatalf("the host's own answer was not read: %#v", nested)
	}
	silent := mapFromAnyMain(mapFromAnyMain(handler.withRunningJobs("m-host", map[string]any{"machineId": "m-host"}))["runningJobs"])
	if silent["known"] != false || silent["total"] != nil {
		t.Fatalf("a host that did not say must read as unknown, never as none: %#v", silent)
	}
}
