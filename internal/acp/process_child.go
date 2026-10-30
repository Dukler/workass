package acp

import (
	"errors"
	"fmt"
	"os"
	"sort"
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
//   - The supervisor writes its transitions to a SPOOL on disk before it
//     reports them, so this daemon can be restarting or mid-self-update and
//     still lose nothing. Ingest is reading one more line of a file.
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

	OriginLaneID      string
	OriginOperationID string
	OriginTurnID      string
}

// RegisterProcessChild records (or re-reads) one supervised child and ingests
// whatever its spool already holds.
//
// Ingesting on every call is what makes the supervisor's closing poke work
// without a second verb: the same registration, repeated with the same id
// after the job ends, delivers the outcome immediately instead of one
// reconcile tick later — and a registration that never gets repeated loses
// nothing, because the tick reads the same file.
func (m *Manager) RegisterProcessChild(opts ProcessChildRegistrationOptions) (map[string]any, error) {
	if err := m.beginWorkAdmission(); err != nil {
		return nil, err
	}
	defer m.endWorkAdmission()
	tabID, chatID, providerID, err := m.resolveExternalWorkOwner(opts.OwnerKey, opts.ParentChatID, opts.ParentTabID, opts.ChatID, opts.TabID)
	if err != nil {
		return nil, err
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
	// The spool path is DERIVED, never taken from the caller: it is a function
	// of the validated id, so a registration cannot point this daemon at an
	// arbitrary file and have its contents applied as a lifecycle.
	spoolFile := procchild.SpoolPath(m.opts.StateDir, workID)
	if spoolFile == "" {
		return nil, errors.New("this machine has no process-child state directory")
	}

	now := time.Now().UTC()
	supervisor := *opts.SupervisorPID
	key := spawnedWorkKey(tabID, chatID, workID)
	m.spawnedWorkMu.Lock()
	rec := m.spawnedWork[key]
	created := rec == nil
	if created {
		rec = &spawnedWorkRecord{
			Item: SpawnedWorkItem{
				ID: workID, TaskID: workID, TabID: tabID, ChatID: chatID, ProviderID: providerID,
				Kind: "external", Label: label, Role: normalizeSpawnedWorkRole(opts.Role), Status: "running",
				StartedAt: now.Format(time.RFC3339Nano), UpdatedAt: now.Format(time.RFC3339Nano),
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
	rec.Item.Owner = compactText(redactSensitiveText(opts.Owner), 240)
	rec.Item.Resource = procchild.NormalizeResource(opts.Resource)
	rec.Item.Isolation = strings.TrimSpace(opts.Isolation)
	rec.Item.MaxRuntimeMs, rec.Item.StaleAfterMs, rec.Item.MaxHoldMs = opts.MaxRuntimeMs, opts.StaleAfterMs, opts.MaxHoldMs
	if opts.ProcessGroup > 1 {
		rec.Item.ProcessGroup = opts.ProcessGroup
	}
	if summary := compactText(redactSensitiveText(opts.Command), 240); summary != "" && rec.Item.LastToolName == "" {
		// The command is shown the way a tracked lane's description already is:
		// free text on the running row, not a tool identity.
		rec.Item.Summary = firstNonEmptySpawnedWorkText(rec.Item.Summary, summary)
	}
	rec.Item.UpdatedAt = now.Format(time.RFC3339Nano)
	events := m.ingestProcessChildSpoolLocked(rec)
	item := rec.Item
	m.spawnedWorkMu.Unlock()

	m.commitSpawnedWorkChange(tabID, chatID)
	m.emitProcessChildLifecycle(events)
	return map[string]any{
		"ok": true, "workId": workID, "taskId": workID, "status": item.Status,
		"outcome": item.Outcome, "outputFile": outputFile, "doneFile": doneFile,
		"spoolFile": spoolFile, "created": created,
	}, nil
}

func firstNonEmptySpawnedWorkText(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// ingestProcessChildSpoolLocked applies every spooled event past this record's
// cursor and returns the lifecycle events that are worth telling a watcher
// about. The caller holds spawnedWorkMu and must emit the returned events
// after releasing it: delivery reaches the chat actor, and holding the registry
// lock across that is how two subsystems deadlock.
func (m *Manager) ingestProcessChildSpoolLocked(rec *spawnedWorkRecord) []ProcessLifecycleEvent {
	if rec == nil || rec.SpoolFile == "" {
		return nil
	}
	events, err := procchild.ReadSpool(rec.SpoolFile)
	if err != nil || len(events) == 0 {
		return nil
	}
	out := make([]ProcessLifecycleEvent, 0, 2)
	for _, event := range events {
		if event.Seq <= rec.SpoolSeq {
			// Dedup by seq, which is the whole of it: re-reading the spool is
			// free, so a crash between applying and persisting the cursor costs
			// one re-application that changes nothing.
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
	case procchild.KindCancelRequested:
		rec.Item.UpdatedAt = event.At.Format(time.RFC3339Nano)
	case procchild.KindSettled:
		outcome := normalizeProcessOutcome(spoolString(payload, "outcome"))
		var exitCode *int
		if value, present := spoolIntPresent(payload, "exitCode"); present {
			exitCode = &value
		}
		summary := compactText(redactSensitiveText(spoolString(payload, "summary")), 1000)
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

// reconcileProcessChildren ingests spools and reaches the `lost` verdict. It
// runs inside the existing reconcile pass rather than on a timer of its own.
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
		events := m.ingestProcessChildSpoolLocked(rec)
		terminal := rec.Item.Status != "running"
		if !terminal && rec.Item.SupervisorPID != nil {
			// The spool has been read to its end and still says running, so the
			// only remaining question is whether the supervisor is there to
			// finish it. Absence is evidence.
			// procchild.PIDAlive rather than this package's externalPIDAlive,
			// and deliberately: the two agree on darwin and linux (kill(pid, 0)
			// with EPERM counting as alive), but externalPIDAlive answers
			// "alive" unconditionally on every other platform, which would make
			// `lost` unreachable on Windows — the one verdict P4 exists for. It
			// is also the probe the resource queue uses, so the two can never
			// disagree about the same pid.
			if procchild.PIDAlive(*rec.Item.SupervisorPID) {
				rec.SupervisorMissingSince = time.Time{}
			} else {
				if rec.SupervisorMissingSince.IsZero() {
					rec.SupervisorMissingSince = now
				}
				if now.Sub(rec.SupervisorMissingSince) >= supervisorMissingGrace {
					if event, ok := m.settleProcessChildLocked(rec, procchild.OutcomeLost, nil,
						"Lost: its supervisor is gone and wrote no outcome, so what the job did is unknown"); ok {
						events = append(events, event)
						terminal = true
					}
				}
			}
		}
		if !terminal && rec.Item.Resource != "" {
			m.reportProcessChildHoldLocked(rec, now, &events)
		}
		spoolFile, workID := rec.SpoolFile, rec.Item.ID
		m.spawnedWorkMu.Unlock()
		if len(events) > 0 {
			m.commitSpawnedWorkChange(candidate.tabID, candidate.chatID)
			m.emitProcessChildLifecycle(events)
		}
		if terminal && spoolFile != "" {
			// The outcome is durable in the receipt and the entity log by now,
			// so the spool has been consumed. Removing it is what keeps a busy
			// week of jobs from leaving thousands of directories behind. The
			// row itself may already be gone: a settled row is compacted out of
			// the live registry once its chat actor has accepted it, and the
			// receipt is the durable record from then on.
			_ = procchild.RemoveSpool(m.opts.StateDir, workID)
			m.spawnedWorkMu.Lock()
			if rec = m.spawnedWork[candidate.key]; rec != nil {
				rec.SpoolFile = ""
			}
			m.spawnedWorkMu.Unlock()
		}
	}
	m.sweepSpentProcessChildSpools()
}

// sweepSpentProcessChildSpools removes spool directories that no row refers to
// and whose supervisor has already recorded a terminal event.
//
// The case it exists for is narrow and real: the commit that retires a settled
// row and the removal of its spool are two steps, so a crash between them would
// leave a directory nothing will ever read again. The condition is deliberately
// strict — a spool with NO terminal event is left alone, because that is a job
// whose supervisor is still running and whose registration simply has not
// reached this daemon yet. Eviction is logged, never silent.
func (m *Manager) sweepSpentProcessChildSpools() {
	root := procchild.Root(m.opts.StateDir)
	if root == "" {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	tracked := map[string]struct{}{}
	m.spawnedWorkMu.Lock()
	for _, rec := range m.spawnedWork {
		if rec != nil && rec.Item.Supervised() {
			tracked[rec.Item.ID] = struct{}{}
		}
	}
	m.spawnedWorkMu.Unlock()
	for _, entry := range entries {
		if !entry.IsDir() || !procchild.ValidID(entry.Name()) {
			continue
		}
		if _, live := tracked[entry.Name()]; live {
			continue
		}
		events, err := procchild.ReadSpool(procchild.SpoolPath(m.opts.StateDir, entry.Name()))
		if err != nil || len(events) == 0 {
			continue
		}
		if !procchild.Terminal(events[len(events)-1].Kind) {
			continue
		}
		if err := procchild.RemoveSpool(m.opts.StateDir, entry.Name()); err == nil {
			m.opts.Logf("removed a spent process-child spool no row refers to", map[string]any{
				"workId": entry.Name(), "events": len(events),
			})
		}
	}
}

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

func spoolInt(payload map[string]any, key string) int {
	value, _ := spoolIntPresent(payload, key)
	return value
}

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
