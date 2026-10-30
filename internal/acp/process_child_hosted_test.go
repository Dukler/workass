package acp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"workass/internal/procchild"
)

const hostedTestOwner = "wa:m-owner/chat/asking-chat"

func hostedRegistrationForTest(t *testing.T, manager *Manager, operationID string, mutate func(*ProcessChildRegistrationOptions)) ProcessChildRegistrationOptions {
	t.Helper()
	return processChildRegistrationForTest(t, manager, operationID, func(opts *ProcessChildRegistrationOptions) {
		opts.OwnerKey, opts.ParentChatID, opts.ParentTabID = "", "", ""
		opts.Owner, opts.HostedForOwner = hostedTestOwner, true
		if mutate != nil {
			mutate(opts)
		}
	})
}

func newHostedTestManager(t *testing.T, state string) *Manager {
	t.Helper()
	manager := NewManager(Options{StateDir: state, RuntimeProfile: "prod", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	return manager
}

// No chat, no bound provider, no actor: this manager has none of them, and the
// job is registered and listed all the same.
func TestAHostedJobNeedsNoChatAndNoProvider(t *testing.T) {
	manager := newHostedTestManager(t, t.TempDir())
	result, err := manager.RegisterProcessChild(hostedRegistrationForTest(t, manager, "hosted-no-chat", nil))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if result["ok"] != true || result["created"] != true {
		t.Fatalf("result = %#v", result)
	}
	rows := manager.ListProcessChildren()
	if len(rows) != 1 || rows[0].ChatID != "" || rows[0].TabID != "" || rows[0].Owner != hostedTestOwner {
		t.Fatalf("rows = %#v", rows)
	}
	encoded, _ := json.Marshal(rows[0])
	var wire map[string]any
	_ = json.Unmarshal(encoded, &wire)
	for _, absent := range []string{"chat_id", "tab_id", "started_at"} {
		if _, present := wire[absent]; present {
			t.Fatalf("%s is on the wire: %s", absent, encoded)
		}
	}
	// It is also not somebody's chat row by accident.
	if _, err := manager.RegisterProcessChild(hostedRegistrationForTest(t, manager, "hosted-no-owner", func(opts *ProcessChildRegistrationOptions) {
		opts.Owner = ""
	})); err == nil {
		t.Fatal("a hosted job with no owner was accepted")
	}
}

// A restart loses nothing a job depends on: the row, its cursor, and the verdict
// when its supervisor did not survive either.
func TestAHostedJobSurvivesARestartAndAMissingSupervisorIsStillLost(t *testing.T) {
	state := t.TempDir()
	first := newHostedTestManager(t, state)
	pid := os.Getpid()
	now := time.Now().UTC()
	opts := hostedRegistrationForTest(t, first, "hosted-restart", func(opts *ProcessChildRegistrationOptions) {
		opts.SupervisorPID = &pid
		opts.Events = []procchild.Event{
			{Seq: 1, At: now.Add(-time.Hour), Kind: procchild.KindCreated},
			{Seq: 2, At: now.Add(-time.Minute), Kind: procchild.KindStarted, Payload: map[string]any{"pid": pid}},
		}
	})
	if _, err := first.RegisterProcessChild(opts); err != nil {
		t.Fatal(err)
	}
	first.Reset()

	second := newHostedTestManager(t, state)
	rows := second.ListProcessChildren()
	if len(rows) != 1 || rows[0].Status != "running" || rows[0].StartedAt == "" || rows[0].Owner != hostedTestOwner {
		t.Fatalf("after a restart: %#v", rows)
	}
	// The cursor came back: the same batch, re-sent, is acknowledged and changes
	// nothing, and the row is the same row.
	again, err := second.RegisterProcessChild(opts)
	if err != nil || again["created"] != false || again["ackThrough"] != uint64(2) {
		t.Fatalf("replay after a restart = %#v, %v", again, err)
	}
	var lifecycle []ProcessLifecycleEvent
	var mu sync.Mutex
	second.SetProcessLifecycleObserver(func(event ProcessLifecycleEvent) {
		mu.Lock()
		defer mu.Unlock()
		lifecycle = append(lifecycle, event)
	})
	second.spawnedWorkMu.Lock()
	dead := 1 << 22
	second.spawnedWork[spawnedWorkKey("", "", opts.WorkID)].Item.SupervisorPID = &dead
	second.spawnedWorkMu.Unlock()
	second.ReconcileProcessChildren(time.Now().UTC())
	second.ReconcileProcessChildren(time.Now().UTC().Add(2 * time.Hour))
	mu.Lock()
	defer mu.Unlock()
	if len(lifecycle) != 1 || lifecycle[0].Outcome != procchild.OutcomeLost || lifecycle[0].Owner != hostedTestOwner {
		t.Fatalf("lifecycle = %#v", lifecycle)
	}
	if lifecycle[0].Receipt.RunMs <= 0 || lifecycle[0].Receipt.QueuedMs < int64(50*time.Minute/time.Millisecond) {
		t.Fatalf("a lost job's receipt = %#v", lifecycle[0].Receipt)
	}
	if rows := second.ListProcessChildren(); len(rows) != 0 {
		t.Fatalf("a lost job is still listed: %#v", rows)
	}
	second.Reset()
	third := newHostedTestManager(t, state)
	if view, found := third.ProcessChildByID(opts.WorkID); !found || view.Outcome != procchild.OutcomeLost {
		t.Fatalf("what became of it did not survive: %#v, %v", view, found)
	}
}

// A settled row stays so a replay and a reader can find it, and only so many do.
func TestAHostOnlyRemembersSoManyFinishedJobs(t *testing.T) {
	manager := newHostedTestManager(t, t.TempDir())
	var firstID, lastID string
	for index := 0; index < maxHostedSettledProcessChildren+3; index++ {
		opts := hostedRegistrationForTest(t, manager, fmt.Sprintf("hosted-cap-%d", index), func(opts *ProcessChildRegistrationOptions) {
			opts.Events = []procchild.Event{{Seq: 1, At: time.Now().UTC(), Kind: procchild.KindSettled,
				Payload: map[string]any{"outcome": procchild.OutcomeFailed, "exitCode": 1}}}
		})
		if _, err := manager.RegisterProcessChild(opts); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			firstID = opts.WorkID
		}
		lastID = opts.WorkID
		time.Sleep(time.Millisecond)
	}
	if _, found := manager.ProcessChildByID(lastID); !found {
		t.Fatal("the newest finished job was forgotten")
	}
	if _, found := manager.ProcessChildByID(firstID); found {
		t.Fatal("finished jobs are remembered without bound")
	}
	if rows := manager.ListProcessChildren(); len(rows) != 0 {
		t.Fatalf("finished jobs are listed: %#v", rows)
	}
	data, err := os.ReadFile(filepath.Join(manager.opts.StateDir, "hosted-process-children.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stored []spawnedWorkSnapshotItem
	if err := json.Unmarshal(data, &stored); err != nil || len(stored) != maxHostedSettledProcessChildren {
		t.Fatalf("stored %d rows, %v", len(stored), err)
	}
}

// The credential's durable record, in both shapes.
func TestJobCredentialsSurviveARestartAndOlderChatBoundOnesStillLoad(t *testing.T) {
	state := t.TempDir()
	first := newHostedTestManager(t, state)
	token := first.MintRunToken("xwjob00000001", hostedTestOwner, "op-1")
	first.Reset()
	path := filepath.Join(state, "tool-contexts", "run-tokens.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	if err := json.Unmarshal(data, &records); err != nil {
		t.Fatal(err)
	}
	records = append(records,
		map[string]any{"token": "wrt_older", "chatId": "chat-1", "tabId": "tab-1", "operationId": "op-2", "issuedAt": time.Now().UTC().Format(time.RFC3339)},
		// Names neither a chat nor a job: nothing could ever check it.
		map[string]any{"token": "wrt_nothing", "operationId": "op-3", "issuedAt": time.Now().UTC().Format(time.RFC3339)},
	)
	data, _ = json.Marshal(records)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	second := newHostedTestManager(t, state)
	job, live := second.RunTokenFor(token)
	if !live || !job.JobScoped() || job.WorkID != "xwjob00000001" || job.Owner != hostedTestOwner {
		t.Fatalf("job credential = %#v, %v", job, live)
	}
	if second.ValidateAgentOwner(token, "", "") {
		t.Fatal("a job credential authenticated as a chat")
	}
	older, live := second.RunTokenFor("wrt_older")
	if !live || older.JobScoped() || !second.ValidateAgentOwner("wrt_older", "chat-1", "tab-1") {
		t.Fatalf("older credential = %#v, %v", older, live)
	}
	if _, live := second.RunTokenFor("wrt_nothing"); live {
		t.Fatal("a credential bound to nothing was loaded")
	}
}

func TestAJobReadsAsQueuedUntilExec(t *testing.T) {
	manager := newHostedTestManager(t, t.TempDir())
	pid := os.Getpid()
	queuedAt := time.Now().UTC().Add(-2 * time.Hour)
	opts := hostedRegistrationForTest(t, manager, "hosted-queued", func(opts *ProcessChildRegistrationOptions) {
		opts.SupervisorPID, opts.Resource, opts.StaleAfterMs, opts.MaxHoldMs = &pid, "gpu0", 1, 1
		opts.Events = []procchild.Event{
			{Seq: 1, At: queuedAt, Kind: procchild.KindCreated},
			{Seq: 2, At: queuedAt, Kind: procchild.KindQueued, Payload: map[string]any{"position": 2}},
		}
	})
	if _, err := manager.RegisterProcessChild(opts); err != nil {
		t.Fatal(err)
	}
	rows := manager.ListProcessChildren()
	if len(rows) != 1 {
		t.Fatalf("rows = %#v", rows)
	}
	row := rows[0]
	if row.Status != "queued" || row.StartedAt != "" || row.QueuePosition != 2 || row.Stalled {
		t.Fatalf("a waiting job reads %#v", row)
	}
	if row.QueuedAt != queuedAt.Format(time.RFC3339Nano) {
		t.Fatalf("queued_at = %q, want the event's own time %q", row.QueuedAt, queuedAt)
	}
	if time.Duration(row.QueuedMs)*time.Millisecond < 2*time.Hour {
		t.Fatalf("queued_ms = %d", row.QueuedMs)
	}
	// A report's status is the lifecycle a supervisor reads, in which anything
	// but running means its job has ended and its credential is spent.
	again, err := manager.RegisterProcessChild(hostedRegistrationForTest(t, manager, "hosted-queued", func(o *ProcessChildRegistrationOptions) {
		o.SupervisorPID, o.Resource = &pid, "gpu0"
		o.Events = nil
	}))
	if err != nil || again["status"] != "running" {
		t.Fatalf("a report about a queued job answered %#v, %v", again, err)
	}
	// Nothing the daemon checks on its own can call the wait a stall.
	manager.ReconcileProcessChildren(time.Now().UTC().Add(time.Minute))
	if rows := manager.ListProcessChildren(); rows[0].Stalled {
		t.Fatalf("a waiting job was flagged stalled: %#v", rows[0])
	}

	startedAt := queuedAt.Add(time.Hour)
	if _, err := manager.RegisterProcessChild(hostedRegistrationForTest(t, manager, "hosted-queued", func(o *ProcessChildRegistrationOptions) {
		o.SupervisorPID, o.Resource = &pid, "gpu0"
		o.Events = []procchild.Event{{Seq: 3, At: startedAt, Kind: procchild.KindStarted, Payload: map[string]any{"pid": pid}}}
	})); err != nil {
		t.Fatal(err)
	}
	row = manager.ListProcessChildren()[0]
	if row.Status != "running" || row.StartedAt != startedAt.Format(time.RFC3339Nano) || row.QueuedAt != queuedAt.Format(time.RFC3339Nano) {
		t.Fatalf("once started: %#v", row)
	}
	if row.QueuedMs != time.Hour.Milliseconds() {
		t.Fatalf("queued_ms = %d, want exactly the wait", row.QueuedMs)
	}
}

func TestProcessChildTimesKeepTheWaitOutOfTheRun(t *testing.T) {
	queued := time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC)
	stamp := func(at time.Time) string { return at.Format(time.RFC3339Nano) }
	exec := queued.Add(2*time.Hour + 58*time.Minute + 13*time.Second)
	end := exec.Add(2*time.Hour + 47*time.Minute + 22*time.Second)
	run := (2*time.Hour + 47*time.Minute + 22*time.Second).Milliseconds()
	wait := (2*time.Hour + 58*time.Minute + 13*time.Second).Milliseconds()
	pid := 4242
	for _, test := range []struct {
		name      string
		item      SpawnedWorkItem
		end       time.Time
		startedAt string
		runMs     int64
		queuedMs  int64
	}{{
		name: "the DepthVision job, stamped",
		item: SpawnedWorkItem{QueuedAt: stamp(queued), ExecStartedAt: stamp(exec), StartedAt: stamp(queued), RunMs: run, PID: &pid},
		end:  end, startedAt: stamp(exec), runMs: run, queuedMs: wait,
	}, {
		name: "the same job straddling the update: an older row and its supervisor's own number",
		item: SpawnedWorkItem{StartedAt: stamp(queued), RunMs: run, PID: &pid},
		end:  end, startedAt: stamp(exec), runMs: run, queuedMs: wait,
	}, {
		name: "lost after exec, with no number from its supervisor",
		item: SpawnedWorkItem{QueuedAt: stamp(queued), ExecStartedAt: stamp(exec), PID: &pid},
		end:  end, startedAt: stamp(exec), runMs: run, queuedMs: wait,
	}, {
		name: "cancelled or lost in the queue: it never ran",
		item: SpawnedWorkItem{QueuedAt: stamp(queued), StartedAt: stamp(queued)},
		end:  exec, startedAt: "", runMs: 0, queuedMs: wait,
	}, {
		name: "an older row with a pid and no number: nothing can split it",
		item: SpawnedWorkItem{StartedAt: stamp(queued), PID: &pid},
		end:  end, startedAt: stamp(queued), runMs: 0, queuedMs: 0,
	}} {
		t.Run(test.name, func(t *testing.T) {
			got := ProcessChildTimes(test.item, test.end)
			if got.StartedAt != test.startedAt || got.RunMs != test.runMs || got.QueuedMs != test.queuedMs {
				t.Fatalf("got %+v, want started %q run %d queued %d", got, test.startedAt, test.runMs, test.queuedMs)
			}
		})
	}
}

// A retry of a closing report whose reply was lost, after the chat's actor
// accepted the settled row and it left the live registry. The supervisor has
// nothing to tell it from any other report, and the owner has been told.
func TestARetriedClosingReportDoesNotReviveACompactedJob(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	childID, err := procchild.IDForOperation("op-closing-retry")
	if err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	now := time.Now().UTC()
	options := ProcessChildRegistrationOptions{
		OwnerKey: "owner-proc", ParentChatID: "chat-proc", ParentTabID: "tab-proc",
		WorkID: childID, Label: "closing retry", SupervisorPID: &pid,
		OutputFile: liveOutputPath(t, manager.opts.StateDir, childID),
		Events: []procchild.Event{
			{Seq: 1, At: now, Kind: procchild.KindCreated},
			{Seq: 2, At: now, Kind: procchild.KindSettled, Payload: map[string]any{"outcome": procchild.OutcomeSettled, "exitCode": 0}},
		},
	}
	if _, err := manager.RegisterProcessChild(options); err != nil {
		t.Fatal(err)
	}
	if err := manager.CommitActorSpawnedWorkProjection("tab-proc", "chat-proc", manager.ListSpawnedWork("tab-proc", "chat-proc")); err != nil {
		t.Fatal(err)
	}
	if rows := manager.ListSpawnedWork("tab-proc", "chat-proc"); len(rows) != 0 {
		t.Fatalf("the settled row was not compacted: %#v", rows)
	}

	retry, err := manager.RegisterProcessChild(options)
	if err != nil {
		t.Fatalf("the retry was refused: %v", err)
	}
	if retry["ackThrough"] != uint64(2) || retry["status"] == "running" {
		t.Fatalf("the retry answered %#v", retry)
	}
	if got := lifecycle.kinds(); got != "exit:settled" {
		t.Fatalf("lifecycle = %q; the owner must be told once", got)
	}
	if rows := manager.ListSpawnedWork("tab-proc", "chat-proc"); len(rows) != 0 {
		t.Fatalf("the retry brought a finished job back: %#v", rows)
	}
}

func TestAClosedJobCredentialAnswersOnlyForItsOwnReportAndExpires(t *testing.T) {
	state := t.TempDir()
	manager := newHostedTestManager(t, state)
	token := manager.MintRunToken("xwclosedjob01", "wa:m-away/chat/c", "op-closed")
	if !manager.CloseRunToken(token, "exited", procchild.OutcomeSettled) {
		t.Fatal("the credential did not close")
	}
	binding, ok := manager.RunTokenFor(token)
	if !ok || !binding.Closed() || binding.ClosedStatus != "exited" || binding.ClosedOutcome != procchild.OutcomeSettled {
		t.Fatalf("binding = %#v, %v", binding, ok)
	}
	if replacement := manager.MintRunToken("xwclosedjob01", "wa:m-away/chat/c", "op-closed"); replacement == token {
		t.Fatal("a new start with the same operation was handed the closed credential")
	}
	manager.Reset()

	restarted := newHostedTestManager(t, state)
	if binding, ok := restarted.RunTokenFor(token); !ok || !binding.Closed() {
		t.Fatalf("a restart forgot the job had closed: %#v, %v", binding, ok)
	}
	restarted.mu.Lock()
	aged := restarted.runTokens[token]
	aged.ClosedAt = time.Now().UTC().Add(-2 * runTokenClosedGrace)
	restarted.runTokens[token] = aged
	restarted.persistRunTokensLocked()
	restarted.mu.Unlock()
	if _, ok := restarted.RunTokenFor(token); ok {
		t.Fatal("a closed credential answered past its grace")
	}
	restarted.Reset()
	expired := newHostedTestManager(t, state)
	if _, ok := expired.RunTokenFor(token); ok {
		t.Fatal("a closed credential outlived its grace")
	}
}
