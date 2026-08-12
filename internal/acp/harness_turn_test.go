package acp

import (
	"testing"
	"time"
)

// The Claude Agent SDK's Stop hook reports the background work still in flight
// and the wakes scheduled to re-invoke the session. These tests pin what the
// daemon is allowed to conclude from that, and — the reason this lane exists —
// that a turn the harness starts on its own gets a job, ends it, and settles
// the obligation instead of vanishing.

func harnessEndedUpdate(promptID string, tasks []any, crons []any, terminalReason string) map[string]any {
	update := map[string]any{
		"phase":           harnessTurnPhaseEnded,
		"promptId":        promptID,
		"terminalReason":  terminalReason,
		"stopReason":      "end_turn",
		"harnessEvidence": true,
	}
	if tasks != nil {
		update["backgroundTasks"] = tasks
	}
	if crons != nil {
		update["sessionCrons"] = crons
	}
	return update
}

func harnessTask(id, kind, status string) any {
	return map[string]any{"id": id, "type": kind, "status": status}
}

func recordHarnessEnd(m *Manager, tabID, chatID, sessionID string, update map[string]any) *harnessTurnEvidence {
	prior := m.harnessTurns.get(sessionID)
	evidence := harnessEvidenceFromUpdate(update)
	evidence.TabID, evidence.ChatID = tabID, chatID
	evidence.Parked, evidence.ParkNote = m.harnessParkEvidence(tabID, chatID, evidence, prior)
	m.harnessTurns.record(sessionID, evidence)
	return evidence
}

// A background task still running IS the park the user waited 76 minutes on. It
// must demote a claimed done, because the harness knows something is coming.
func TestHarnessBackgroundTaskDemotesDeclaredDone(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir()})
	t.Cleanup(func() { manager.Reset() })

	evidence := recordHarnessEnd(manager, "tab-1", "chat-1", "sess-1",
		harnessEndedUpdate("p-1", []any{harnessTask("bg-1", "shell", "running")}, []any{}, "completed"))

	if !evidence.Parked {
		t.Fatal("a running background task is not park evidence; a claimed done would stand and lie")
	}
	if evidence.Quiet() {
		t.Fatal("a session with work in flight reported itself quiet")
	}
}

// Both arrays empty is the harness stating it is genuinely finished. That is
// the only promotion it is allowed, and only from Unknown.
func TestHarnessQuietTurnIsDone(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir()})
	t.Cleanup(func() { manager.Reset() })

	evidence := recordHarnessEnd(manager, "tab-1", "chat-1", "sess-1",
		harnessEndedUpdate("p-1", []any{}, []any{}, "completed"))

	if !evidence.Quiet() || evidence.Parked {
		t.Fatalf("quiet=%v parked=%v, want a quiet, unparked turn", evidence.Quiet(), evidence.Parked)
	}
}

// An older CLI omits the field entirely. Absent is UNKNOWN, not quiet — reading
// silence as "nothing is running" is the false-done this lane removes.
func TestHarnessAbsentListsAreUnknownNotQuiet(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir()})
	t.Cleanup(func() { manager.Reset() })

	evidence := recordHarnessEnd(manager, "tab-1", "chat-1", "sess-1",
		harnessEndedUpdate("p-1", nil, nil, "completed"))

	if evidence.Complete() {
		t.Fatal("a report with no lists claimed to be complete evidence")
	}
	if evidence.Quiet() {
		t.Fatal("an absent list read as proof of quiet — silence is not evidence")
	}
}

// A hook that never ran is not evidence either.
func TestHarnessWithoutHookEvidenceIsIncomplete(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir()})
	t.Cleanup(func() { manager.Reset() })

	update := harnessEndedUpdate("p-1", []any{}, []any{}, "completed")
	update["harnessEvidence"] = false
	evidence := recordHarnessEnd(manager, "tab-1", "chat-1", "sess-1", update)

	if evidence.Complete() || evidence.Quiet() {
		t.Fatal("a turn with no Stop hook produced a verdict instead of falling back")
	}
}

// A never-exiting task — a dev server — must park the chat once, not forever.
// This is the service-classification bug rebuilt one layer up if it regresses.
func TestHarnessRepeatedTaskDoesNotReArmPark(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir()})
	t.Cleanup(func() { manager.Reset() })

	first := recordHarnessEnd(manager, "tab-1", "chat-1", "sess-1",
		harnessEndedUpdate("p-1", []any{harnessTask("bg-1", "shell", "running")}, []any{}, "completed"))
	if !first.Parked {
		t.Fatal("the first report of running work did not park")
	}
	second := recordHarnessEnd(manager, "tab-1", "chat-1", "sess-1",
		harnessEndedUpdate("p-2", []any{harnessTask("bg-1", "shell", "running")}, []any{}, "completed"))
	if second.Parked {
		t.Fatal("an unchanged task re-armed the park; a dev server would pin this chat forever")
	}
}

// A task whose status genuinely changed is fresh evidence again.
func TestHarnessChangedTaskStatusIsFreshEvidence(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir()})
	t.Cleanup(func() { manager.Reset() })

	recordHarnessEnd(manager, "tab-1", "chat-1", "sess-1",
		harnessEndedUpdate("p-1", []any{harnessTask("bg-1", "shell", "pending")}, []any{}, "completed"))
	second := recordHarnessEnd(manager, "tab-1", "chat-1", "sess-1",
		harnessEndedUpdate("p-2", []any{harnessTask("bg-1", "shell", "running")}, []any{}, "completed"))

	if !second.Parked {
		t.Fatal("a task that changed status was suppressed as a repeat")
	}
}

// A recurring schedule (/loop, CronCreate) is not evidence that THIS request is
// unfinished. Counting it would make done permanently unreachable for a looping
// chat — strictly worse than the bug being fixed.
func TestHarnessRecurringCronDoesNotPark(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir()})
	t.Cleanup(func() { manager.Reset() })

	evidence := recordHarnessEnd(manager, "tab-1", "chat-1", "sess-1",
		harnessEndedUpdate("p-1", []any{}, []any{
			map[string]any{"schedule": "*/20 * * * *", "recurring": true},
		}, "completed"))

	if evidence.Parked {
		t.Fatal("a recurring schedule parked the chat; an hourly loop could never read done")
	}
}

// A one-shot wake is ScheduleWakeup — the self-scheduled park nothing else in
// this system can observe. It must park.
func TestHarnessOneShotCronParks(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir()})
	t.Cleanup(func() { manager.Reset() })

	evidence := recordHarnessEnd(manager, "tab-1", "chat-1", "sess-1",
		harnessEndedUpdate("p-1", []any{}, []any{
			map[string]any{"schedule": "at 19:40", "recurring": false},
		}, "completed"))

	if !evidence.Parked {
		t.Fatal("a one-shot scheduled wake did not park — the invisible-wake case is unfixed")
	}
	if evidence.ParkNote == "" {
		t.Fatal("the park carried no note naming what it waits on")
	}
}

// The clamp's capability rules: the harness may demote a done, and may resolve
// an unknown, but must never overrule a needs_input. A refusal read as done is
// the exact lie this record exists to prevent.
func TestHarnessNeverOverrulesNeedsInput(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir()})
	t.Cleanup(func() { manager.Reset() })

	recordHarnessEnd(manager, "tab-1", "chat-1", "sess-1",
		harnessEndedUpdate("p-1", []any{}, []any{}, "completed"))

	job := &Job{
		TabID: "tab-1", ChatID: "chat-1", SessionID: "sess-1",
		ProviderID: "claude", StopReason: "refusal", Status: "done",
	}
	manager.classifyDispositionForJob(job)
	if job.DispositionState != string(DispositionNeedsInput) {
		t.Fatalf("disposition = %q, want needs_input", job.DispositionState)
	}
}

// The promotion the harness IS allowed: end_turn alone cannot tell done from
// parked, and before v3 this fell through to inference.
func TestHarnessResolvesUnknownToDone(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir()})
	t.Cleanup(func() { manager.Reset() })

	recordHarnessEnd(manager, "tab-1", "chat-1", "sess-1",
		harnessEndedUpdate("p-1", []any{}, []any{}, "completed"))

	job := &Job{
		TabID: "tab-1", ChatID: "chat-1", SessionID: "sess-1",
		ProviderID: "claude", StopReason: "end_turn", Status: "done",
	}
	manager.classifyDispositionForJob(job)
	if job.DispositionState != string(DispositionDone) {
		t.Fatalf("disposition = %q, want done", job.DispositionState)
	}
	if job.DispositionSource != dispositionSourceHarness {
		t.Fatalf("source = %v, want harness", job.DispositionSource)
	}
}

// The demotion, end to end through the clamp: a turn that ended cleanly with
// work still in flight is a park, and the note says what it waits on.
func TestClampDemotesDoneOnHarnessParkEvidence(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir()})
	t.Cleanup(func() { manager.Reset() })

	recordHarnessEnd(manager, "tab-1", "chat-1", "sess-1",
		harnessEndedUpdate("p-1", []any{harnessTask("bg-1", "shell", "running")}, []any{}, "completed"))

	job := &Job{
		TabID: "tab-1", ChatID: "chat-1", SessionID: "sess-1",
		ProviderID: "claude", StopReason: "end_turn", Status: "done",
	}
	manager.classifyDispositionForJob(job)
	if job.DispositionState != string(DispositionParked) {
		t.Fatalf("disposition = %q, want parked", job.DispositionState)
	}
	if job.DispositionSource != dispositionSourceHarness {
		t.Fatalf("source = %v, want harness", job.DispositionSource)
	}
}

// The lane's whole reason for existing: a turn the harness starts on its own
// must get a job, so its words reach the user instead of being discarded at the
// job==nil guards — and that job must END, or every later human prompt queues
// behind a phantom `working` and the stuck-queue incident is rebuilt inside the
// fix for it.
func TestAdoptedHarnessTurnStartsAndEnds(t *testing.T) {
	events := newEventCollector()
	manager := NewManager(Options{StateDir: t.TempDir(), Broadcast: events.Broadcast})
	t.Cleanup(func() { manager.Reset() })
	bridge := &Bridge{
		manager:       manager,
		providerID:    "claude",
		tabID:         "tab-1",
		chatID:        "chat-1",
		opts:          Options{StdoutFlushInterval: time.Hour},
		jobsBySession: map[string]*Job{},
	}

	manager.observeClaudeTurn(bridge, "tab-1", "chat-1", "sess-1", map[string]any{
		"phase": harnessTurnPhaseStarted, "promptId": "p-2", "humanAuthored": false,
	})

	job := bridge.jobForSession("sess-1")
	if job == nil {
		t.Fatal("no job was adopted; the model's words for this turn are discarded")
	}
	if !job.harnessTurn || job.Status != "running" {
		t.Fatalf("adopted job = %+v, want a running harness turn", job)
	}
	manager.observeClaudeTurn(bridge, "tab-1", "chat-1", "sess-1",
		harnessEndedUpdate("p-2", []any{}, []any{}, "completed"))

	if remaining := bridge.jobForSession("sess-1"); remaining != nil {
		t.Fatalf("adopted job survived its turn (%+v); every later prompt queues behind it", remaining)
	}
	if job.DispositionState != string(DispositionDone) {
		t.Fatalf("adopted turn disposition = %q, want done", job.DispositionState)
	}
}

// A human turn is already owned by a real job. Adopting it would give the chat
// two jobs and settle the user's prompt with somebody else's result.
func TestHumanAuthoredTurnIsNotAdopted(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir()})
	t.Cleanup(func() { manager.Reset() })
	bridge := &Bridge{
		manager: manager, providerID: "claude",
		opts: Options{StdoutFlushInterval: time.Hour}, jobsBySession: map[string]*Job{},
	}

	manager.observeClaudeTurn(bridge, "tab-1", "chat-1", "sess-1", map[string]any{
		"phase": harnessTurnPhaseStarted, "promptId": "p-1", "humanAuthored": true,
	})

	if job := bridge.jobForSession("sess-1"); job != nil {
		t.Fatalf("adopted a human turn (%+v)", job)
	}
}

// An engine that dies must not leave an adopted job running: it holds no prompt
// to re-drive, so crash recovery cannot help it.
func TestAdoptedHarnessTurnEndsWhenEngineDies(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir()})
	t.Cleanup(func() { manager.Reset() })
	bridge := &Bridge{
		manager: manager, providerID: "claude", tabID: "tab-1", chatID: "chat-1",
		opts: Options{StdoutFlushInterval: time.Hour}, jobsBySession: map[string]*Job{},
	}

	manager.observeClaudeTurn(bridge, "tab-1", "chat-1", "sess-1", map[string]any{
		"phase": harnessTurnPhaseStarted, "promptId": "p-2", "humanAuthored": false,
	})
	if bridge.jobForSession("sess-1") == nil {
		t.Fatal("no job was adopted")
	}

	manager.abandonAdoptedHarnessTurns(bridge)

	if remaining := bridge.jobForSession("sess-1"); remaining != nil {
		t.Fatalf("adopted job outlived its engine (%+v)", remaining)
	}
}

// A hook firing inside a subagent is the subagent's turn, not the session's.
// Acting on it would end the session's turn on every Agent tool call. The host
// filters it, and this pins the wire shape the daemon may rely on.
func TestSubagentHookIsNotASessionTurn(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir()})
	t.Cleanup(func() { manager.Reset() })
	bridge := &Bridge{
		manager: manager, providerID: "claude",
		opts: Options{StdoutFlushInterval: time.Hour}, jobsBySession: map[string]*Job{},
	}

	// The host never emits this — a payload carrying a subagent's evidence is
	// already dropped there. If one ever arrives, adoption must still be keyed
	// on humanAuthored alone and a second identical start must not re-adopt.
	manager.observeClaudeTurn(bridge, "tab-1", "chat-1", "sess-1", map[string]any{
		"phase": harnessTurnPhaseStarted, "promptId": "p-2", "humanAuthored": false,
	})
	first := bridge.jobForSession("sess-1")
	manager.observeClaudeTurn(bridge, "tab-1", "chat-1", "sess-1", map[string]any{
		"phase": harnessTurnPhaseStarted, "promptId": "p-2", "humanAuthored": false,
	})
	if bridge.jobForSession("sess-1") != first {
		t.Fatal("a repeated start for the same prompt adopted a second job")
	}
}
