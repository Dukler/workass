package acp

import (
	"os"
	"strings"
	"testing"
	"time"

	"workass/internal/procchild"
)

// The two daemon-side halves of the 2026-10-30 live failure.
//
// A supervised job started from another machine reported correctly and was
// refused twice over: once by the argument validator (internal/procchild's
// own test owns that one) and once here, by a registration that demanded a
// LIVE provider binding for a chat nobody was talking in.

// TestAHostChatWithNoLiveSessionStillRegistersItsSupervisedJob is the second
// cause. The host chat on the machine that RUNS a job holds the job's local
// row and nothing else: no agent prompts in it, so after a daemon restart
// there is no session bound to it, so `chatProviders` is empty for it — and
// the registration answered "external work registration is unavailable for
// this Workass ACP provider" to every report four real jobs ever made.
func TestAHostChatWithNoLiveSessionStillRegistersItsSupervisedJob(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir(), RuntimeProfile: "prod", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	installAcceptingSpawnedWorkActorForTest(manager)
	// The owner is bound; the PROVIDER deliberately is not. This is the state a
	// record-holder chat is in, and a prod runtime, because the live failure
	// was on a packaged install.
	manager.mu.Lock()
	manager.bindAgentOwnerLocked("owner-host", "chat-host", "tab-host")
	manager.mu.Unlock()
	if _, err := manager.RegisterProcessChild(processChildRegistrationForTest(t, manager, "op-no-binding", func(opts *ProcessChildRegistrationOptions) {
		opts.OwnerKey, opts.ParentChatID, opts.ParentTabID = "owner-host", "chat-host", "tab-host"
		opts.ProviderID = ""
	})); err == nil {
		t.Fatal("a registration with no provider from either side must still be refused")
	}
	result, err := manager.RegisterProcessChild(processChildRegistrationForTest(t, manager, "op-no-binding", func(opts *ProcessChildRegistrationOptions) {
		opts.OwnerKey, opts.ParentChatID, opts.ParentTabID = "owner-host", "chat-host", "tab-host"
		// What the daemon that started the job read off the chat's durable
		// presentation, which is true whether or not anybody is talking in it.
		opts.ProviderID = "codex"
	}))
	if err != nil {
		t.Fatalf("a record-holder host chat refused its own job's registration: %v", err)
	}
	if result["ok"] != true {
		t.Fatalf("registration = %#v", result)
	}
	if ack, _ := result["ackThrough"].(uint64); ack != 1 {
		t.Fatalf("the report must be acknowledged through seq 1, got %#v", result["ackThrough"])
	}
	rows := manager.ListProcessChildren()
	if len(rows) != 1 || rows[0].Status != "running" {
		t.Fatalf("the job must be a visible running row, got %#v", rows)
	}
}

// TestASupervisorThatNeverReportsGoesSilentWithinOneBeat is the visibility the
// failure had none of. The row now exists from the LAUNCH — written by the
// daemon that started the supervisor — so a supervisor that cannot report is a
// job that says so, within one heartbeat rather than three, and the notice
// names the file holding the supervisor's own complaint.
func TestASupervisorThatNeverReportsGoesSilentWithinOneBeat(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	logPath := manager.opts.StateDir + "/external-work/xw-awaiting.output.supervisor.log"
	start := time.Now().UTC()
	childID, _ := seedProcessChild(t, manager, "op-awaiting", os.Getpid(), func(opts *ProcessChildRegistrationOptions) {
		opts.Events = nil
		opts.HeartbeatMs = procchild.HeartbeatInterval.Milliseconds()
		opts.SupervisorLog = logPath
		opts.AwaitingFirstReport = true
	})
	if rows := manager.ListProcessChildren(); len(rows) != 1 || rows[0].WorkID != childID || rows[0].Reporting != "" {
		t.Fatalf("a launch must be a visible row that has not yet failed to report, got %#v", rows)
	}
	// Half a beat in, nothing is wrong: a supervisor is allowed to be starting.
	manager.reconcileProcessChildren(start.Add(procchild.HeartbeatInterval / 2))
	if rows := manager.ListProcessChildren(); rows[0].Reporting != "" {
		t.Fatalf("a supervisor half a beat old is not silent, got %q", rows[0].Reporting)
	}
	// One beat and a second, and it has never said anything.
	manager.reconcileProcessChildren(start.Add(procchild.HeartbeatInterval + time.Second))
	rows := manager.ListProcessChildren()
	if len(rows) != 1 || rows[0].Reporting != "silent" {
		t.Fatalf("reporting = %#v, want silent", rows)
	}
	notices := 0
	for _, event := range lifecycle.snapshot() {
		if event.Kind != ProcessLifecycleAttention || event.AttentionKind != "not_reporting" {
			continue
		}
		notices++
		if !strings.Contains(event.AttentionText, "never heard from it") {
			t.Fatalf("the notice must say it has never been heard from, got %q", event.AttentionText)
		}
		if !strings.Contains(event.AttentionText, logPath) {
			t.Fatalf("the notice must say where the supervisor's own words are, got %q", event.AttentionText)
		}
	}
	if notices != 1 {
		t.Fatalf("not_reporting notices = %d, want exactly one", notices)
	}
	// One-shot: a second pass does not say it again.
	manager.reconcileProcessChildren(start.Add(4 * procchild.HeartbeatInterval))
	again := 0
	for _, event := range lifecycle.snapshot() {
		if event.Kind == ProcessLifecycleAttention && event.AttentionKind == "not_reporting" {
			again++
		}
	}
	if again != 1 {
		t.Fatalf("not_reporting notices after a second pass = %d, want one", again)
	}
	// And the supervisor's first real report clears it and ends the tighter
	// clock: silence is about this machine's knowledge, never a verdict.
	if _, err := manager.RegisterProcessChild(processChildRegistrationForTest(t, manager, "op-awaiting", func(opts *ProcessChildRegistrationOptions) {
		opts.Events = []procchild.Event{{Seq: 1, Kind: procchild.KindHeartbeat, At: time.Now().UTC()}}
	})); err != nil {
		t.Fatalf("the first real report was refused: %v", err)
	}
	if rows = manager.ListProcessChildren(); rows[0].Reporting != "" {
		t.Fatalf("a report must clear the silence, got %q", rows[0].Reporting)
	}
}

// processChildRegistrationForTest is one registration's options, built the way
// seedProcessChild builds them so the two cannot describe different jobs.
func processChildRegistrationForTest(t *testing.T, manager *Manager, operationID string, mutate func(*ProcessChildRegistrationOptions)) ProcessChildRegistrationOptions {
	t.Helper()
	childID, err := procchild.IDForOperation(operationID)
	if err != nil {
		t.Fatal(err)
	}
	outputDir := manager.opts.StateDir + "/external-work"
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	opts := ProcessChildRegistrationOptions{
		OwnerKey: "owner-proc", ParentChatID: "chat-proc", ParentTabID: "tab-proc",
		WorkID: childID, Label: "training run", Command: "python train.py",
		OutputFile: outputDir + "/" + childID + ".output", SupervisorPID: &pid,
		Events: []procchild.Event{{Seq: 1, Kind: procchild.KindCreated, At: time.Now().UTC()}},
	}
	if mutate != nil {
		mutate(&opts)
	}
	return opts
}

// TestTheLaunchRowNeverResurrectsAJobThatAlreadyFinished is the race the launch
// row introduces and must lose.
//
// The daemon starts the supervisor and THEN writes the row, so a job that
// finishes in milliseconds — `/bin/echo`, or a preflight refusal — can have
// reported, settled and been compacted into its receipt before the launch's
// own registration arrives. Creating a fresh `running` row at that point would
// resurrect a finished job: it would go silent within a beat and `lost` after
// that, and the chat would be told twice about one job in two contradictory
// ways.
func TestTheLaunchRowNeverResurrectsAJobThatAlreadyFinished(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	childID, _ := seedProcessChild(t, manager, "op-fast", os.Getpid(), func(opts *ProcessChildRegistrationOptions) {
		opts.Events = []procchild.Event{
			{Seq: 1, Kind: procchild.KindCreated, At: time.Now().UTC()},
			{Seq: 2, Kind: procchild.KindSettled, At: time.Now().UTC(), Payload: map[string]any{
				"outcome": procchild.OutcomeSettled, "exitCode": 0, "summary": "Finished with exit 0 after 0s"}},
		}
	})
	if rows := manager.ListProcessChildren(); len(rows) != 0 {
		t.Fatalf("a settled job is compacted into its receipt, got %#v", rows)
	}
	// The launch, arriving late.
	result, err := manager.RegisterProcessChild(processChildRegistrationForTest(t, manager, "op-fast", func(opts *ProcessChildRegistrationOptions) {
		opts.Events = nil
		opts.AwaitingFirstReport = true
		opts.SupervisorLog = manager.opts.StateDir + "/external-work/" + childID + ".output.supervisor.log"
	}))
	if err != nil {
		t.Fatalf("a late launch row must be accepted, not refused: %v", err)
	}
	if result["created"] != false || result["status"] != "settled" {
		t.Fatalf("the late launch must resolve to the finished job, got %#v", result)
	}
	if rows := manager.ListProcessChildren(); len(rows) != 0 {
		t.Fatalf("nothing may be resurrected, got %#v", rows)
	}
	// And no amount of reconciling invents a second outcome for it.
	manager.reconcileProcessChildren(time.Now().UTC().Add(10 * procchild.HeartbeatInterval))
	exits := 0
	for _, event := range lifecycle.snapshot() {
		if event.Kind == ProcessLifecycleExit {
			exits++
		}
	}
	if exits != 1 {
		t.Fatalf("exactly one outcome for one job, got %d", exits)
	}
}
