package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"workass/internal/procchild"
)

// Process children, daemon side: the record, the spool, and the one verdict
// only this machine can reach.
//
// docs/specs/fleet-runtime.md §13 P4. The supervisor (`workass run`,
// internal/procchild) owns the process; this file owns what the chat knows
// about it. The division is deliberate and is the answer to every failure the
// WIP note catalogued:
//
//   - The supervisor REPORTS its transitions here, over the authenticated
//     Agent API, and this daemon ACKNOWLEDGES each one by seq. The ack is what
//     makes an event delivered, so nothing is lost to a daemon that was
//     restarting: the supervisor's outbox holds the batch and the next report
//     delivers it in order, exactly once.
//   - This daemon never DISCOVERS a job by scanning anything. It learns about
//     one from its report. It reads the outbox for exactly one case — a child
//     it already holds a row for, whose supervisor has died with events still
//     undelivered — and it looks that up by id.
//   - This daemon never spawns the child and is never its parent (§9), so a
//     daemon restart does not touch it. Re-attaching is the snapshot plus the
//     spool cursor.
//   - A supervisor pid that is gone with no `settled` in the spool is `lost`.
//     That is a verdict, not a guess, and it is the exact inverse of the
//     marker-file design where absence meant "still running" and a finished
//     job sat unnoticed for five hours.
//
// Everything here is additive to the existing spawned-work registry rather
// than a second store beside it. A process child IS today's external work with
// a supervisor, so the rail row, the obligation, the receipt and the actor
// fence are the ones that already exist — which is what makes the migration's
// projection-parity gate something that holds by construction rather than by
// a comparison nobody will re-run.

// supervisorMissingGrace is how long a vanished supervisor is given before its
// child is called `lost`. It is externalWorkMissingGrace, deliberately the
// same number: both are "a pid this machine cannot find any more", and two
// graces for one question is how they drift apart.
const supervisorMissingGrace = externalWorkMissingGrace

// ProcessChildRegistrationOptions is one `workass run` registration.
//
// It is idempotent by WorkID, which the supervisor derives from its operation
// id — so a retry after an uncertain reply resolves to the row that already
// exists. §6: one operation_id for both "launch there" and "record here".
type ProcessChildRegistrationOptions struct {
	OwnerKey     string
	ParentChatID string
	ParentTabID  string
	TabID        string
	ChatID       string

	WorkID       string
	Label        string
	Role         string
	Command      string
	Owner        string
	Resource     string
	OutputFile   string
	DoneFile     string
	SpoolFile    string
	Isolation    string
	ProcessGroup int
	// SupervisorPID is required: a registration with no supervisor is
	// evidence-less, and a row whose liveness nothing can answer is exactly
	// the stale `running` P4 exists to make impossible.
	SupervisorPID *int
	MaxRuntimeMs  int64
	StaleAfterMs  int64
	MaxHoldMs     int64
	// Events is this report's batch: every event the supervisor has not been
	// acknowledged for, in seq order. Applying them is idempotent by seq.
	Events []procchild.Event
	// HeartbeatMs is the cadence the supervisor reports at, so this daemon
	// decides what silence is from what the supervisor declared rather than
	// from a second constant of its own.
	HeartbeatMs int64
	// ReportGapMs is how long the supervisor says it was unable to reach this
	// daemon immediately before this batch.
	ReportGapMs int64

	OriginLaneID      string
	OriginOperationID string
	OriginTurnID      string

	// ProviderID is the chat's DURABLE provider, read by the caller that can read
	// it. It is the fallback behind this manager's live session binding, which a
	// chat with no running turn does not have (resolveExternalWorkOwner says what
	// that cost).
	ProviderID string
	// SupervisorLog is where the supervisor's own stream — its diagnostics and
	// its closing receipt — is being captured, set only when THIS daemon
	// started the supervisor and therefore owns that file. It is what a notice
	// about a silent job can point a reader at.
	SupervisorLog string
	// AwaitingFirstReport marks the registration this daemon writes for a
	// supervisor IT started, before that supervisor has said anything.
	//
	// The row has to exist from the start: every way this product notices a
	// job that cannot report — the silence notice, the `lost` verdict, the
	// drain of a dead supervisor's outbox — hangs off a row, so a supervisor
	// whose every report was refused produced no row, no notice and no trace.
	// It also means the silence is measured against ONE beat rather than
	// three: a supervisor reports at startup, so a minute of nothing from a
	// process this daemon launched itself is already news.
	AwaitingFirstReport bool
	// HostedForOwner is a job this machine runs for a chat on another machine.
	// No chat here holds it: the row is keyed by the job alone and names its
	// owner, and the caller has already authorized it by the job's own
	// credential, or by being the daemon that launched it. The chat fields above
	// stay empty (process_child_hosted.go).
	HostedForOwner bool
}

// RegisterProcessChild records one supervised child and applies the batch of
// events its supervisor is reporting.
//
// It is one call rather than two because the row and the events are one fact:
// a daemon that restarted and lost its in-memory row rebuilds it from the same
// message that delivers the backlog, and a job that finishes in the same second
// its first report is still travelling needs no second verb for the outcome.
// The answer carries `ackThrough`, which is the only thing that lets the
// supervisor retire an event from its outbox.
func (m *Manager) RegisterProcessChild(opts ProcessChildRegistrationOptions) (map[string]any, error) {
	if err := m.beginWorkAdmission(); err != nil {
		return nil, err
	}
	defer m.endWorkAdmission()
	tabID, chatID, providerID := "", "", ""
	if opts.HostedForOwner {
		if strings.TrimSpace(opts.Owner) == "" {
			return nil, errors.New("a job hosted for another machine must name its owner; no chat here is owed its outcome")
		}
	} else {
		var err error
		tabID, chatID, providerID, err = m.resolveExternalWorkOwner(opts.OwnerKey, opts.ParentChatID, opts.ParentTabID, opts.ChatID, opts.TabID, opts.ProviderID)
		if err != nil {
			return nil, err
		}
	}
	workID := strings.TrimSpace(opts.WorkID)
	if !procchild.ValidID(workID) {
		return nil, errors.New("a process child needs the work id its supervisor derived from the operation id")
	}
	if opts.SupervisorPID == nil || *opts.SupervisorPID <= 1 {
		// Refusal of evidence-less registration (§13 P4). Without a supervisor
		// pid there is nothing that could ever settle this row honestly.
		return nil, errors.New("a process child registration must carry its supervisor's pid")
	}
	label := compactText(redactSensitiveText(opts.Label), 240)
	if label == "" {
		return nil, errors.New("process child label is required")
	}
	outputFile, ok := validateExternalWorkPath(m.opts.StateDir, opts.OutputFile)
	if !ok {
		return nil, errors.New("process child output_file is not an allowed absolute path")
	}
	doneFile := strings.TrimSpace(opts.DoneFile)
	if doneFile == "" {
		doneFile = outputFile + ".done"
	}
	if doneFile, ok = validateExternalWorkPath(m.opts.StateDir, doneFile); !ok {
		return nil, errors.New("process child done_file is not an allowed absolute path")
	}
	// The outbox path is DERIVED, never taken from the caller: it is this
	// machine's one outbox, so a report cannot point this daemon at an
	// arbitrary file and have its contents applied as a lifecycle.
	spoolFile := procchild.OutboxPath(m.opts.StateDir)
	if spoolFile == "" {
		return nil, errors.New("this machine has no process-child state directory")
	}
	events, err := normalizeReportedEvents(opts.Events)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	supervisor := *opts.SupervisorPID
	key := spawnedWorkKey(tabID, chatID, workID)
	if opts.AwaitingFirstReport && !opts.HostedForOwner {
		// The launch row loses every race it can lose. The daemon starts the
		// supervisor and then writes the row, so a job that finishes in
		// milliseconds may already have reported, been settled, and had its
		// row compacted into a receipt before this call arrives — and creating
		// a fresh `running` row then would resurrect a finished job, which
		// would go silent in a minute and `lost` after that. A receipt for
		// this work id is proof the launch has nothing left to record.
		if _, settled := m.findSpawnedWorkReceipt(tabID, chatID, workID); settled {
			return map[string]any{
				"ok": true, "workId": workID, "taskId": workID, "status": "settled",
				"created": false, "ackThrough": uint64(0),
			}, nil
		}
	}
	if !opts.HostedForOwner && reportCarriesSettle(events) {
		// A closing report arriving for a job with no row and a receipt is a retry
		// whose reply was lost: the actor accepted the settled row and it was
		// compacted. Taken as new, it would bring back a finished job and tell its
		// owner a second time.
		m.spawnedWorkMu.Lock()
		_, live := m.spawnedWork[key]
		m.spawnedWorkMu.Unlock()
		if !live {
			if receipt, settled := m.findSpawnedWorkReceipt(tabID, chatID, workID); settled {
				return map[string]any{
					"ok": true, "workId": workID, "taskId": workID, "status": receipt.Status,
					"outcome": receipt.Outcome, "created": false, "replayed": true,
					"ackThrough": procchild.HighestSeq(events),
				}, nil
			}
		}
	}
	m.spawnedWorkMu.Lock()
	rec := m.spawnedWork[key]
	created := rec == nil
	if created {
		rec = &spawnedWorkRecord{
			Item: SpawnedWorkItem{
				ID: workID, TaskID: workID, TabID: tabID, ChatID: chatID, ProviderID: providerID,
				Kind: "external", Label: label, Role: normalizeSpawnedWorkRole(opts.Role), Status: "running",
				StartedAt: now.Format(time.RFC3339Nano), UpdatedAt: now.Format(time.RFC3339Nano),
				QueuedAt:     now.Format(time.RFC3339Nano),
				OutputFile:   outputFile,
				OriginLaneID: strings.TrimSpace(opts.OriginLaneID), OriginOperationID: strings.TrimSpace(opts.OriginOperationID),
				OriginTurnID: strings.TrimSpace(opts.OriginTurnID),
			},
			ExternalDoneFile: doneFile, SpoolFile: spoolFile,
		}
		m.spawnedWork[key] = rec
	}
	// Facts the supervisor is authoritative about are refreshed on every call;
	// the row's identity and its origin stamp are immutable (the origin is what
	// binds later settlement to the turn that asked, and rebinding it would let
	// an outcome land on whichever turn happened to be live).
	rec.Item.SupervisorPID = &supervisor
	// STATED facts overwrite; omitted ones are left alone. The difference
	// matters now that a report arrives many times over a job's life rather
	// than two or three: a later report that did not mention the resource must
	// not erase which GPU the job is holding.
	if owner := compactText(redactSensitiveText(opts.Owner), 240); owner != "" {
		rec.Item.Owner = owner
	}
	if resource := procchild.NormalizeResource(opts.Resource); resource != "" {
		rec.Item.Resource = resource
	}
	if isolation := strings.TrimSpace(opts.Isolation); isolation != "" && isolation != "none" {
		rec.Item.Isolation = isolation
	}
	if opts.MaxRuntimeMs > 0 {
		rec.Item.MaxRuntimeMs = opts.MaxRuntimeMs
	}
	if opts.StaleAfterMs > 0 {
		rec.Item.StaleAfterMs = opts.StaleAfterMs
	}
	if opts.MaxHoldMs > 0 {
		rec.Item.MaxHoldMs = opts.MaxHoldMs
	}
	if opts.ProcessGroup > 1 {
		rec.Item.ProcessGroup = opts.ProcessGroup
	}
	if summary := compactText(redactSensitiveText(opts.Command), 240); summary != "" && rec.Item.LastToolName == "" {
		// The command is shown the way a tracked lane's description already is:
		// free text on the running row, not a tool identity.
		rec.Item.Summary = firstNonEmptySpawnedWorkText(rec.Item.Summary, summary)
	}
	if created || reportCarriesLifecycle(events) {
		// A report of nothing but liveness does not change the row. One whose
		// timestamp moved every minute would make every quiet job look busy in
		// every surface that sorts or ages by it.
		rec.Item.UpdatedAt = now.Format(time.RFC3339Nano)
	}
	if opts.HeartbeatMs > 0 {
		rec.Item.HeartbeatMs = opts.HeartbeatMs
	}
	if opts.ReportGapMs > rec.Item.ReportGapMs {
		rec.Item.ReportGapMs = opts.ReportGapMs
	}
	// Heard from, which is the fact the silence verdict is read against. It is
	// set for an EMPTY report too: a supervisor with nothing to say still said
	// it, and that is the whole point of the heartbeat.
	rec.LastReportAt = now
	if opts.AwaitingFirstReport {
		// Not a report at all: this daemon writing down the supervisor it has
		// just launched. The clock starts here, and nothing is claimed to have
		// been heard.
		if created {
			rec.AwaitingFirstReport = true
		}
		if log := strings.TrimSpace(opts.SupervisorLog); log != "" {
			rec.SupervisorLog = log
		}
	} else {
		rec.AwaitingFirstReport = false
	}
	if rec.Item.Reporting == processReportingSilent {
		rec.Item.Reporting = ""
		rec.ReportedSilent = false
	}
	lifecycle := m.applyReportedEventsLocked(rec, events)
	item, ackThrough := rec.Item, rec.SpoolSeq
	m.spawnedWorkMu.Unlock()

	m.commitProcessChildChange(tabID, chatID)
	m.emitProcessChildLifecycle(lifecycle)
	if item.Status != "running" {
		// Retired here rather than only on the next reconcile tick, because a
		// settled row is compacted out of the live registry once its chat actor
		// has accepted it — so the tick may find nothing to clean up after, and
		// every finished job would leave its directory behind forever. Only
		// DELIVERED events go; anything unacknowledged is the only copy of
		// something nobody has been told.
		m.forgetProcessChildOutbox(workID)
	}
	return map[string]any{
		"ok": true, "workId": workID, "taskId": workID, "status": item.Status,
		"outcome": item.Outcome, "outputFile": outputFile, "doneFile": doneFile,
		"spoolFile": spoolFile, "created": created, "ackThrough": ackThrough,
	}, nil
}

// reportCarriesSettle reports whether a batch carries the job's outcome.
func reportCarriesSettle(events []procchild.Event) bool {
	for _, event := range events {
		if event.Kind == procchild.KindSettled {
			return true
		}
	}
	return false
}

// reportCarriesLifecycle reports whether a batch says anything about the job
// as opposed to about the supervisor.
func reportCarriesLifecycle(events []procchild.Event) bool {
	for _, event := range events {
		if event.Kind != procchild.KindHeartbeat {
			return true
		}
	}
	return false
}

// processReportingSilent is the one non-empty value of a row's Reporting field.
const processReportingSilent = "silent"

// maxReportedEventsPerCall bounds one batch. A supervisor that has been unable
// to report for a day has a day of heartbeats queued, and it sends them in
// order across as many calls as it takes rather than in one unbounded request.
// MaxReportedEventsPerCall is the batch ceiling, exported because the tool
// schema states it: an agent-facing bound that the validator and the
// declaration disagreed about would be a refusal with no visible cause.
const MaxReportedEventsPerCall = 512

const maxReportedEventsPerCall = MaxReportedEventsPerCall

// normalizeReportedEvents validates one batch and puts it in seq order.
//
// A batch is refused outright rather than partially applied: the events are a
// SEQUENCE, and applying the readable half of a malformed report would advance
// the cursor past something nobody looked at.
func normalizeReportedEvents(events []procchild.Event) ([]procchild.Event, error) {
	if len(events) == 0 {
		return nil, nil
	}
	if len(events) > maxReportedEventsPerCall {
		return nil, fmt.Errorf("a supervised-job report carries at most %d events; send the backlog in order across several reports", maxReportedEventsPerCall)
	}
	out := make([]procchild.Event, 0, len(events))
	seen := make(map[uint64]struct{}, len(events))
	for _, event := range events {
		if event.Seq == 0 {
			return nil, errors.New("every reported supervised-job event needs its outbox seq")
		}
		if strings.TrimSpace(event.Kind) == "" {
			return nil, errors.New("every reported supervised-job event needs a kind")
		}
		if _, duplicate := seen[event.Seq]; duplicate {
			return nil, fmt.Errorf("supervised-job event seq %d appears twice in one report", event.Seq)
		}
		seen[event.Seq] = struct{}{}
		out = append(out, event)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

func firstNonEmptySpawnedWorkText(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// applyReportedEventsLocked applies every reported event past this record's
// acknowledgement cursor and returns the lifecycle events that are worth
// telling a watcher about. The caller holds spawnedWorkMu and must emit the
// returned events after releasing it: delivery reaches the chat actor, and
// holding the registry lock across that is how two subsystems deadlock.
//
// Dedup by seq is the whole of exactly-once. A supervisor that crashed between
// its insert and its send, or that never got an ack it was owed, re-sends; the
// cursor makes the re-application change nothing, so the retry is free and the
// supervisor never has to decide whether it is safe.
func (m *Manager) applyReportedEventsLocked(rec *spawnedWorkRecord, events []procchild.Event) []ProcessLifecycleEvent {
	if rec == nil || len(events) == 0 {
		return nil
	}
	out := make([]ProcessLifecycleEvent, 0, 2)
	for _, event := range events {
		if event.Seq <= rec.SpoolSeq {
			continue
		}
		rec.SpoolSeq = event.Seq
		if lifecycle, ok := m.applySpoolEventLocked(rec, event); ok {
			out = append(out, lifecycle)
		}
	}
	return out
}

func (m *Manager) applySpoolEventLocked(rec *spawnedWorkRecord, event procchild.Event) (ProcessLifecycleEvent, bool) {
	payload := event.Payload
	switch event.Kind {
	case procchild.KindCreated:
		if rec.Item.Resource == "" {
			rec.Item.Resource = procchild.NormalizeResource(spoolString(payload, "resource"))
		}
		if rec.Item.Owner == "" {
			rec.Item.Owner = compactText(redactSensitiveText(spoolString(payload, "owner")), 240)
		}
		rec.Item.QueuedAt = earlierProcessStamp(rec.Item.QueuedAt, event.At)
		rec.Item.UpdatedAt = event.At.Format(time.RFC3339Nano)
	case procchild.KindQueued:
		rec.Item.QueuePosition = spoolInt(payload, "position")
		rec.Item.UpdatedAt = event.At.Format(time.RFC3339Nano)
	case procchild.KindStarted:
		if pid := spoolInt(payload, "pid"); pid > 1 {
			value := pid
			rec.Item.PID = &value
			rec.SawPID = true
		}
		if pgid := spoolInt(payload, "pgid"); pgid > 1 {
			rec.Item.ProcessGroup = pgid
		}
		if isolation := strings.TrimSpace(spoolString(payload, "group")); isolation != "" {
			rec.Item.Isolation = isolation
		}
		rec.Item.QueuePosition = 0
		rec.Item.ExecStartedAt = event.At.UTC().Format(time.RFC3339Nano)
		rec.MissingSince = time.Time{}
		rec.Item.UpdatedAt = event.At.Format(time.RFC3339Nano)
	case procchild.KindStalled:
		if rec.Item.Stalled || rec.Item.Status != "running" {
			return ProcessLifecycleEvent{}, false
		}
		// One notice however long the silence lasts. The supervisor already
		// emits this once per child; the flag here is the second guard, for a
		// spool re-read after a restart.
		rec.Item.Stalled = true
		rec.Item.UpdatedAt = event.At.Format(time.RFC3339Nano)
		return m.processLifecycleLocked(rec, ProcessLifecycleEvent{
			Kind: ProcessLifecycleStalled, AttentionKind: "stalled",
			AttentionText: compactText(redactSensitiveText(spoolString(payload, "evidence")), 400),
		}), true
	case procchild.KindAttention:
		if rec.Item.Status != "running" {
			return ProcessLifecycleEvent{}, false
		}
		return m.processLifecycleLocked(rec, ProcessLifecycleEvent{
			Kind:          ProcessLifecycleAttention,
			AttentionKind: firstNonEmptySpawnedWorkText(spoolString(payload, "attentionKind"), "attention"),
			AttentionText: compactText(redactSensitiveText(spoolString(payload, "title")), 400),
		}), true
	case procchild.KindHeartbeat:
		// Deliberately nothing but the fact that it arrived, which the caller
		// already recorded as LastReportAt. It is not a notice, not an entry in
		// the entity log, and not even an UpdatedAt bump: a row whose timestamp
		// moved every minute would make every quiet job look busy.
	case procchild.KindCancelRequested:
		rec.Item.UpdatedAt = event.At.Format(time.RFC3339Nano)
	case procchild.KindSettled:
		outcome := normalizeProcessOutcome(spoolString(payload, "outcome"))
		var exitCode *int
		if value, present := spoolIntPresent(payload, "exitCode"); present {
			exitCode = &value
		}
		summary := compactText(redactSensitiveText(spoolString(payload, "summary")), 1000)
		if ms, present := spoolIntPresent(payload, "elapsedMs"); present && ms > 0 && rec.Item.Status == "running" {
			rec.Item.RunMs = int64(ms)
		}
		return m.settleProcessChildLocked(rec, outcome, exitCode, summary)
	}
	return ProcessLifecycleEvent{}, false
}

// settleProcessChildLocked is the one place a supervised child becomes
// terminal, whatever decided it.
//
// Two vocabularies meet here. Status stays the closed presentation set every
// surface already reads — a timed-out job presents as failed, a lost one as
// orphaned, which is the word the rail has always shown for "no process" — and
// Outcome carries the fleet runtime's verdict unchanged. Collapsing them would
// either lie to the chat or change the meaning of a field.
func (m *Manager) settleProcessChildLocked(rec *spawnedWorkRecord, outcome string, exitCode *int, summary string) (ProcessLifecycleEvent, bool) {
	if rec == nil || rec.Item.Status != "running" {
		return ProcessLifecycleEvent{}, false
	}
	status := "exited"
	switch outcome {
	case procchild.OutcomeFailed, procchild.OutcomeTimedOut:
		status = "failed"
	case procchild.OutcomeLost:
		status = "orphaned"
	}
	if !m.settleSpawnedWorkLocked(rec, status, exitCode) {
		return ProcessLifecycleEvent{}, false
	}
	rec.Item.Outcome = outcome
	if summary != "" {
		rec.Item.Summary = summary
	}
	rec.Item.QueuePosition = 0
	rec.Item.UpdatedAt = rec.Item.FinishedAt
	event := m.processLifecycleLocked(rec, ProcessLifecycleEvent{Kind: ProcessLifecycleExit, Outcome: outcome})
	if outcome == procchild.OutcomeCancelled {
		// A cancelled child never notifies: the party that would be told is the
		// party that asked (§7). It is still logged — suppression is about the
		// notice, never about the history.
		event.NoticeSuppressed = true
		event.SuppressReason = "the owner cancelled this job"
	}
	return event, true
}

func normalizeProcessOutcome(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case procchild.OutcomeSettled:
		return procchild.OutcomeSettled
	case procchild.OutcomeTimedOut:
		return procchild.OutcomeTimedOut
	case procchild.OutcomeCancelled:
		return procchild.OutcomeCancelled
	case procchild.OutcomeLost:
		return procchild.OutcomeLost
	default:
		return procchild.OutcomeFailed
	}
}

// reconcileProcessChildren reaches the two verdicts only this daemon can:
// a supervisor that has stopped REPORTING, and a supervisor that is GONE.
//
// It runs inside the existing reconcile pass rather than on a timer of its own,
// and it scans nothing: the rows it considers are the rows it already holds.
// ReconcileProcessChildren runs one pass at a stated time. Exported so a test
// can reach a time-based verdict — the `lost` grace, the silence threshold —
// without waiting out a real grace, and so the reconcile loop and a test take
// exactly the same path to it.
func (m *Manager) ReconcileProcessChildren(now time.Time) { m.reconcileProcessChildren(now) }

func (m *Manager) reconcileProcessChildren(now time.Time) {
	type pending struct {
		key, tabID, chatID string
	}
	supervised := []pending{}
	m.spawnedWorkMu.Lock()
	for key, rec := range m.spawnedWork {
		if rec == nil || !rec.Item.Supervised() {
			continue
		}
		if rec.Item.Status != "running" && rec.SpoolFile == "" {
			continue
		}
		supervised = append(supervised, pending{key: key, tabID: rec.Item.TabID, chatID: rec.Item.ChatID})
	}
	m.spawnedWorkMu.Unlock()
	sort.SliceStable(supervised, func(i, j int) bool { return supervised[i].key < supervised[j].key })
	for _, candidate := range supervised {
		m.spawnedWorkMu.Lock()
		rec := m.spawnedWork[candidate.key]
		if rec == nil {
			m.spawnedWorkMu.Unlock()
			continue
		}
		workID := rec.Item.ID
		cursor := rec.SpoolSeq
		supervisorGone := rec.Item.Status == "running" && rec.Item.SupervisorPID != nil &&
			!procchild.PIDAlive(*rec.Item.SupervisorPID)
		m.spawnedWorkMu.Unlock()

		// The one read of the outbox this daemon performs, and only for the one
		// case the report path cannot cover: the supervisor is dead, so nobody
		// is left to re-send what it queued. A supervisor that crashed between
		// writing its settle and delivering it is NOT lost, and reading the
		// queue it left is the difference between the truth and a guess.
		drained := []procchild.Event{}
		if supervisorGone {
			drained = m.drainProcessChildOutbox(workID, cursor)
		}

		m.spawnedWorkMu.Lock()
		if rec = m.spawnedWork[candidate.key]; rec == nil {
			m.spawnedWorkMu.Unlock()
			continue
		}
		events := m.applyReportedEventsLocked(rec, drained)
		terminal := rec.Item.Status != "running"
		if !terminal && rec.Item.SupervisorPID != nil {
			if procchild.PIDAlive(*rec.Item.SupervisorPID) {
				rec.SupervisorMissingSince = time.Time{}
				m.reportProcessChildSilenceLocked(rec, now, &events)
			} else {
				// Its queue has been drained and still says running, so the
				// only remaining question is whether the supervisor is there to
				// finish it. Absence is evidence.
				// procchild.PIDAlive rather than this package's externalPIDAlive,
				// and deliberately: the two agree on darwin and linux (kill(pid, 0)
				// with EPERM counting as alive), but externalPIDAlive answers
				// "alive" unconditionally on every other platform, which would make
				// `lost` unreachable on Windows — the one verdict P4 exists for. It
				// is also the probe the resource queue uses, so the two can never
				// disagree about the same pid.
				if rec.SupervisorMissingSince.IsZero() {
					rec.SupervisorMissingSince = now
				}
				if now.Sub(rec.SupervisorMissingSince) >= supervisorMissingGrace {
					if event, ok := m.settleProcessChildLocked(rec, procchild.OutcomeLost, nil,
						"Lost: its supervisor is gone and reported no outcome, so what the job did is unknown"); ok {
						events = append(events, event)
						terminal = true
					}
				}
			}
		}
		if !terminal && rec.Item.Resource != "" {
			m.reportProcessChildHoldLocked(rec, now, &events)
		}
		hadOutbox := rec.SpoolFile != ""
		m.spawnedWorkMu.Unlock()
		if len(events) > 0 {
			m.commitProcessChildChange(candidate.tabID, candidate.chatID)
			m.emitProcessChildLifecycle(events)
		}
		if terminal && hadOutbox {
			// The outcome is durable in the receipt and the entity log by now,
			// so this child's delivered events have served their purpose. The
			// row itself may already be gone: a settled row is compacted out of
			// the live registry once its chat actor has accepted it, and the
			// receipt is the durable record from then on.
			m.forgetProcessChildOutbox(workID)
			m.spawnedWorkMu.Lock()
			if rec = m.spawnedWork[candidate.key]; rec != nil {
				rec.SpoolFile = ""
			}
			m.spawnedWorkMu.Unlock()
		}
	}
	m.pruneProcessChildOutbox(now)
}

// reportProcessChildSilenceLocked raises ONE notice for a supervisor that is
// alive and has stopped reporting.
//
// This is the operator's rule where it is enforceable. A job's record must
// never look healthy when nothing here has heard from it: three missed
// heartbeats from a process that still exists means the reports are not
// arriving, and that is news — not a verdict about the work, which may be going
// perfectly. It settles nothing and it is one-shot; the next report clears it.
func (m *Manager) reportProcessChildSilenceLocked(rec *spawnedWorkRecord, now time.Time, events *[]ProcessLifecycleEvent) {
	if rec == nil || rec.Item.Status != "running" || rec.ReportedSilent {
		return
	}
	if rec.LastReportAt.IsZero() {
		// Nothing has been heard yet in this daemon's lifetime, which after a
		// restart is every job at once. The clock starts now rather than at a
		// zero time, so a restart raises no alarm it has not earned.
		rec.LastReportAt = now
		return
	}
	threshold := procchild.SilenceThreshold
	if rec.Item.HeartbeatMs > 0 {
		declared := 3 * time.Duration(rec.Item.HeartbeatMs) * time.Millisecond
		if declared > 0 {
			threshold = declared
		}
	}
	if rec.AwaitingFirstReport {
		// One beat, not three. A supervisor reports as it starts, so for a
		// process this daemon launched itself there is nothing to be patient
		// about: a minute with no first report means the reports are not
		// arriving, and the live failure this tightening exists for stayed
		// invisible for the whole of four jobs.
		threshold = procchild.HeartbeatInterval
	}
	silent := now.Sub(rec.LastReportAt)
	if silent < threshold {
		return
	}
	rec.Item.Reporting = processReportingSilent
	rec.ReportedSilent = true
	rec.Item.UpdatedAt = now.Format(time.RFC3339Nano)
	*events = append(*events, m.processLifecycleLocked(rec, ProcessLifecycleEvent{
		Kind: ProcessLifecycleAttention, AttentionKind: "not_reporting",
		// Longer than the other two attentions on purpose: this one carries an
		// absolute path a reader has to be able to open, and a notice whose
		// action is truncated is a notice that costs more than it gives.
		AttentionText: compactText(processSilenceText(rec, silent), maxSilenceNoticeText),
	}))
}

// maxSilenceNoticeText bounds the silence notice. It is wider than the 400 the
// stall and attention notices use because this one must still contain a usable
// absolute path after the sentence that explains it.
const maxSilenceNoticeText = 900

// processSilenceText says which silence this is.
//
// A job that reported and went quiet and a job that has never said anything at
// all are different news: the first is a supervisor that stopped answering,
// the second is usually a supervisor that CANNOT answer — and in that case
// this daemon started it, so it knows where the supervisor's own complaint was
// written and says so instead of leaving a reader to find it.
func processSilenceText(rec *spawnedWorkRecord, silent time.Duration) string {
	if !rec.AwaitingFirstReport {
		return fmt.Sprintf(
			"Not reporting: its supervisor (pid %d) is still running but this machine has heard nothing from it for %s, so its progress and its outcome are unknown until it reports again.",
			supervisorPIDOf(rec), silent.Round(time.Second))
	}
	text := fmt.Sprintf(
		"Not reporting: this machine started its supervisor (pid %d) %s ago and has never heard from it, so this job's progress and its outcome are unknown here. The work may be running perfectly; nothing on this side can say.",
		supervisorPIDOf(rec), silent.Round(time.Second))
	if log := strings.TrimSpace(rec.SupervisorLog); log != "" {
		text += fmt.Sprintf(" The supervisor's own diagnostics and receipt are at %s.", log)
	}
	return text
}

func supervisorPIDOf(rec *spawnedWorkRecord) int {
	if rec == nil || rec.Item.SupervisorPID == nil {
		return 0
	}
	return *rec.Item.SupervisorPID
}

// processOutbox opens this machine's supervised-job outbox, once, lazily.
//
// Lazily because a daemon on a machine that has never run a supervised job has
// no outbox and should not create one, and once because the handle is a SQLite
// connection: opening it per reconcile tick would be a file open per tick.
func (m *Manager) processOutbox() *procchild.Outbox {
	m.processOutboxMu.Lock()
	defer m.processOutboxMu.Unlock()
	if m.processOutboxHandle != nil {
		return m.processOutboxHandle
	}
	if m.processOutboxFailed {
		return nil
	}
	outbox, err := procchild.OpenOutboxForReading(m.opts.StateDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// Loud once, never per tick. An unusable outbox means a dead
			// supervisor's queued outcome cannot be recovered here, which is a
			// real loss of evidence and not a detail.
			m.processOutboxFailed = true
			m.opts.Logf("this machine's supervised-job outbox could not be opened", map[string]any{
				"path": procchild.OutboxPath(m.opts.StateDir), "error": redactSensitiveText(err.Error()),
			})
		}
		return nil
	}
	m.processOutboxHandle = outbox
	return outbox
}

// drainProcessChildOutbox reads one child's undelivered events past a cursor.
// By id, never by listing: this daemon learns that a job exists from its
// report, and this read only recovers what a dead supervisor left behind.
func (m *Manager) drainProcessChildOutbox(workID string, cursor uint64) []procchild.Event {
	outbox := m.processOutbox()
	if outbox == nil {
		return nil
	}
	events, err := outbox.All(workID, cursor, 0)
	if err != nil {
		m.opts.Logf("could not read a supervised job's outbox", map[string]any{
			"workId": workID, "error": redactSensitiveText(err.Error()),
		})
		return nil
	}
	return events
}

// closeProcessOutbox releases the outbox handle. It matters beyond tidiness:
// the handle is a SQLite connection with a WAL, and on Windows an open file is
// a locked file — a manager that was reset without closing it would keep a
// supervisor from writing.
func (m *Manager) closeProcessOutbox() {
	m.processOutboxMu.Lock()
	outbox := m.processOutboxHandle
	m.processOutboxHandle, m.processOutboxFailed = nil, false
	m.processOutboxMu.Unlock()
	if outbox != nil {
		_ = outbox.Close()
	}
}

func (m *Manager) forgetProcessChildOutbox(workID string) {
	if outbox := m.processOutbox(); outbox != nil {
		_ = outbox.Forget(workID)
	}
	// And the child's own directory, which now holds only its stop file. One
	// empty directory per job, kept forever, is the litter the spool's removal
	// used to prevent.
	_ = procchild.RemoveDir(m.opts.StateDir, workID)
}

// pruneProcessChildOutbox applies the retention window. It is the whole of
// what replaced the old directory sweep: no listing, no per-child files, and
// an undelivered event is never removed however old it is.
func (m *Manager) pruneProcessChildOutbox(now time.Time) {
	m.processOutboxMu.Lock()
	due := m.processOutboxPrunedAt.IsZero() || now.Sub(m.processOutboxPrunedAt) >= processOutboxPruneInterval
	if due {
		m.processOutboxPrunedAt = now
	}
	m.processOutboxMu.Unlock()
	if !due {
		return
	}
	outbox := m.processOutbox()
	if outbox == nil {
		return
	}
	if removed, err := outbox.Prune(now); err == nil && removed > 0 {
		m.opts.Logf("pruned delivered supervised-job events past the retention window", map[string]any{
			"events": removed,
		})
	}
}

// processOutboxPruneInterval keeps retention off the hot path: once an hour is
// far more often than a seven-day window needs.
const processOutboxPruneInterval = time.Hour

// reportProcessChildHoldLocked raises ONE attention for a holder past the hold
// it declared. §16.7: max_hold is report-only — a host never kills a holder to
// be fair, because killing somebody's eight-hour training run to be fair is a
// worse outcome than saying who is holding the GPU.
func (m *Manager) reportProcessChildHoldLocked(rec *spawnedWorkRecord, now time.Time, events *[]ProcessLifecycleEvent) {
	if rec.Item.MaxHoldMs <= 0 || rec.Item.Stalled {
		return
	}
	for _, entry := range procchild.Queue(m.opts.StateDir, rec.Item.Resource) {
		if entry.ChildID != rec.Item.ID || !entry.Holding {
			continue
		}
		if entry.HeldForMs <= rec.Item.MaxHoldMs {
			return
		}
		// The stall flag is reused as the one-shot latch: a row raises one
		// non-terminal attention, whichever of the two declared thresholds it
		// crossed first, so a long hold cannot become a notice per tick.
		rec.Item.Stalled = true
		*events = append(*events, m.processLifecycleLocked(rec, ProcessLifecycleEvent{
			Kind: ProcessLifecycleAttention, AttentionKind: "max_hold",
			AttentionText: fmt.Sprintf("still holding %s after %s, past the %s it declared",
				rec.Item.Resource, time.Duration(entry.HeldForMs)*time.Millisecond,
				time.Duration(rec.Item.MaxHoldMs)*time.Millisecond),
		}))
		return
	}
}

func spoolString(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	value, _ := payload[key].(string)
	return value
}

// spoolInt is spoolIntPresent where absence and zero mean the same thing.
func spoolInt(payload map[string]any, key string) int {
	value, _ := spoolIntPresent(payload, key)
	return value
}

// spoolIntPresent reads one number out of an event payload, in whatever shape
// the journey here left it in.
//
// The shapes are not hypothetical. A payload built in process carries `int`; a
// payload decoded from the report's JSON carries `float64` or `json.Number`
// depending on the decoder; and the wire encodes numbers as strings, which is
// how the FIRST version of this function silently dropped every exit code of
// every job reported over a link — the one fact a finished job is judged by,
// absent, with nothing anywhere saying so.
func spoolIntPresent(payload map[string]any, key string) (int, bool) {
	if payload == nil {
		return 0, false
	}
	switch value := payload[key].(type) {
	case float64:
		return int(value), true
	case int:
		return value, true
	case int64:
		return int(value), true
	case json.Number:
		parsed, err := value.Int64()
		return int(parsed), err == nil
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		return parsed, err == nil
	}
	return 0, false
}

// ReadProcessChildOutput is the bounded tail a reader asks for by byte offset.
//
// Output is not events (delegation-fabric.md §3): a child's stdout is a file,
// and a reader asks for a bounded tail rather than having it copied into a log.
// So progress costs nothing, never grows the log and never wakes anybody.
func readProcessChildTail(path string, limit int) (string, bool, int64) {
	if strings.TrimSpace(path) == "" {
		return "", false, 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", false, 0
	}
	if limit <= 0 {
		limit = defaultSpawnedWorkTailBytes
	}
	if limit > maxSpawnedWorkTailBytes {
		limit = maxSpawnedWorkTailBytes
	}
	size := info.Size()
	offset, truncated := int64(0), false
	if size > int64(limit) {
		offset, truncated = size-int64(limit), true
	}
	file, err := os.Open(path)
	if err != nil {
		return "", false, size
	}
	defer file.Close()
	buf := make([]byte, limit)
	read, err := file.ReadAt(buf, offset)
	if read <= 0 && err != nil {
		return "", truncated, size
	}
	return redactSensitiveText(string(buf[:read])), truncated, size
}
