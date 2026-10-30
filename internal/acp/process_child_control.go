package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"workass/internal/procchild"
)

// Listing, reading and cancelling process children.
//
// Three reads and one command, and the shape of each is decided by one line of
// the spec: "local == remote". Every one of these is addressed by the child's
// id on the machine that homes it, so routing takes them to that machine and
// the handler never learns which it was (§6). There is no second
// implementation for "a job over there", which is the whole claim of the fleet
// runtime and the reason the old locality table could not express a job at all:
// a process child is machine-scoped in EXECUTION and chat-scoped in OWNERSHIP,
// and the table had no class for that.
//
// Reading is deliberately not a log: a child's output is a FILE, and a reader
// asks for a bounded tail by byte offset (delegation-fabric.md §3). So progress
// costs nothing, never grows a log and never wakes anybody — which is exactly
// when freshness matters, because a reader is already looking.

// ProcessChildView is one process child, as a reader sees it.
type ProcessChildView struct {
	WorkID  string `json:"work_id"`
	Label   string `json:"label"`
	Status  string `json:"status"`
	Outcome string `json:"outcome,omitempty"`
	Role    string `json:"role,omitempty"`
	Owner   string `json:"owner,omitempty"`
	// ChatID and TabID are absent for a job hosted for another machine: no chat
	// here holds it, and Owner says whose it is.
	ChatID string `json:"chat_id,omitempty"`
	TabID  string `json:"tab_id,omitempty"`
	// QueuedAt is when the job was asked for. StartedAt is when its command
	// started, and is absent while Status is "queued": a job waiting for a
	// resource has not started, however long it has been waiting. QueuedMs is
	// how long it waited, or has waited so far. RunMs is set once it has ended.
	QueuedAt   string `json:"queued_at,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	QueuedMs   int64  `json:"queued_ms,omitempty"`
	RunMs      int64  `json:"run_ms,omitempty"`
	UpdatedAt  string `json:"updated_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	Summary    string `json:"summary,omitempty"`
	OutputFile string `json:"output_file,omitempty"`

	PID           *int   `json:"pid,omitempty"`
	SupervisorPID *int   `json:"supervisor_pid,omitempty"`
	ProcessGroup  int    `json:"process_group,omitempty"`
	Isolation     string `json:"isolation,omitempty"`
	Resource      string `json:"resource,omitempty"`
	QueuePosition int    `json:"queue_position,omitempty"`
	Holding       bool   `json:"holding,omitempty"`
	Stalled       bool   `json:"stalled,omitempty"`
	MaxRuntimeMs  int64  `json:"max_runtime_ms,omitempty"`
	StaleAfterMs  int64  `json:"stale_after_ms,omitempty"`
	MaxHoldMs     int64  `json:"max_hold_ms,omitempty"`
	// StopRequested reports that a cancel has been written and the supervisor
	// has not finished acting on it yet. A cancel is a wish, so saying it is
	// pending is more honest than reporting the job already stopped.
	StopRequested bool `json:"stop_requested,omitempty"`
	// Reporting is empty while the supervisor is reporting, and "silent" when
	// its last report is older than three of its declared heartbeats while its
	// pid is still alive. It is on the view because the operator's rule is
	// that a job nothing has heard from must not READ as healthy — a row that
	// looked fine while the reports were going nowhere is the failure this
	// field exists to make visible.
	Reporting string `json:"reporting,omitempty"`
	// ReportGapMs is the longest stretch the supervisor said it could not
	// reach this daemon. It outlives the outage, so a row that healed still
	// shows that it happened.
	ReportGapMs int64 `json:"report_gap_ms,omitempty"`
	// HeartbeatMs is the cadence the supervisor declared, so a reader can see
	// what silence was measured against.
	HeartbeatMs int64 `json:"heartbeat_ms,omitempty"`
	// SurvivesRestart says whether stopping or updating this machine's daemon would
	// kill the job, measured against its supervisor's real pid when read. Absent
	// for a job that has ended. SurvivalNote says why, either way.
	SurvivesRestart *bool  `json:"survives_restart,omitempty"`
	SurvivalNote    string `json:"survival_note,omitempty"`
}

// measureSurvival fills in one active row's survival, for the moment it is read.
func measureSurvival(view *ProcessChildView) {
	if view == nil || !view.active() || view.SupervisorPID == nil {
		return
	}
	survival := procchild.SurvivalOf(*view.SupervisorPID)
	protected := survival.Protected
	view.SurvivesRestart = &protected
	view.SurvivalNote = firstNonEmpty(survival.How, survival.Why)
}

// JobSurvival is one running job as the updater reports it.
type JobSurvival struct {
	WorkID    string `json:"workId"`
	Label     string `json:"label"`
	Owner     string `json:"owner,omitempty"`
	Protected bool   `json:"protected"`
	Note      string `json:"note"`
}

// RunningJobSurvival is every active job on this machine and whether it would
// outlive this daemon. It is the fact an update has to be applied in view of.
func (m *Manager) RunningJobSurvival() []JobSurvival {
	out := []JobSurvival{}
	for _, row := range m.ListProcessChildren() {
		if !row.active() || row.SurvivesRestart == nil {
			continue
		}
		out = append(out, JobSurvival{
			WorkID: row.WorkID, Label: row.Label, Owner: row.Owner,
			Protected: *row.SurvivesRestart, Note: row.SurvivalNote,
		})
	}
	return out
}

// ListProcessChildren reports every supervised child on this machine, newest
// running first, with the live resource queue folded in.
//
// Machine-scoped rather than chat-scoped, because a resource queue is a
// property of the machine: a job waiting behind another job needs to see the
// job it is waiting behind, which may belong to a different chat. Every row
// still names its owner, so nothing about who asked is lost.
func (m *Manager) ListProcessChildren() []ProcessChildView {
	m.spawnedWorkMu.Lock()
	rows := make([]ProcessChildView, 0, 8)
	resources := map[string]struct{}{}
	for _, rec := range m.spawnedWork {
		if rec == nil || !rec.Item.Supervised() {
			continue
		}
		if rec.Item.Status != "running" && hostedProcessChild(rec.Item) {
			// Kept so a read or a replayed start still finds it, and past this
			// list exactly as a chat's settled row is once compacted.
			continue
		}
		rows = append(rows, processChildView(rec.Item))
		if rec.Item.Resource != "" {
			resources[rec.Item.Resource] = struct{}{}
		}
	}
	m.spawnedWorkMu.Unlock()
	holders := map[string]procchild.QueueEntry{}
	for resource := range resources {
		for _, entry := range procchild.Queue(m.opts.StateDir, resource) {
			holders[entry.ChildID] = entry
		}
	}
	for i := range rows {
		if entry, ok := holders[rows[i].WorkID]; ok {
			rows[i].QueuePosition, rows[i].Holding = entry.Position, entry.Holding
		}
		rows[i].StopRequested = procchild.StopRequested(m.opts.StateDir, rows[i].WorkID)
		measureSurvival(&rows[i])
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].active() != rows[j].active() {
			return rows[i].active()
		}
		return firstNonEmpty(rows[i].StartedAt, rows[i].QueuedAt) > firstNonEmpty(rows[j].StartedAt, rows[j].QueuedAt)
	})
	return rows
}

// ResourceQueues is the visible FIFO, for every resource with waiters.
func (m *Manager) ResourceQueues() []procchild.QueueEntry {
	out := make([]procchild.QueueEntry, 0, 4)
	for _, resource := range procchild.Resources(m.opts.StateDir) {
		out = append(out, procchild.Queue(m.opts.StateDir, resource)...)
	}
	return out
}

// ReadProcessChild is one child, its bounded output tail, and the spooled
// transitions past a cursor the reader states.
func (m *Manager) ReadProcessChild(workID string, tailBytes int, afterSeq uint64) (map[string]any, error) {
	workID = normalizeSpawnedWorkTaskID(workID)
	if !procchild.ValidID(workID) {
		return nil, errors.New("a process child id is required")
	}
	m.spawnedWorkMu.Lock()
	var item SpawnedWorkItem
	spoolFile, found := "", false
	for _, rec := range m.spawnedWork {
		if rec == nil || rec.Item.ID != workID || !rec.Item.Supervised() {
			continue
		}
		item, spoolFile, found = rec.Item, rec.SpoolFile, true
		break
	}
	m.spawnedWorkMu.Unlock()
	if !found {
		// Not "unknown": a chat's settled row is compacted into its receipt, and a
		// job must stay readable after it ends, which is when somebody asks.
		if receipt, settled := m.findProcessChildReceipt(workID); settled {
			return processChildReceiptAnswer(receipt, tailBytes, m), nil
		}
		return nil, fmt.Errorf("no process child %q is tracked on this machine", workID)
	}
	view := processChildView(item)
	measureSurvival(&view)
	view.StopRequested = procchild.StopRequested(m.opts.StateDir, workID)
	if item.Resource != "" {
		for _, entry := range procchild.Queue(m.opts.StateDir, item.Resource) {
			if entry.ChildID == workID {
				view.QueuePosition, view.Holding = entry.Position, entry.Holding
				break
			}
		}
	}
	tail, truncated, size := readProcessChildTail(item.OutputFile, tailBytes)
	out := map[string]any{
		"ok": true, "child": view, "output_tail": tail,
		"output_bytes": size, "output_truncated": truncated,
	}
	if spoolFile != "" {
		// The transitions a reader asks for come from the outbox, which holds
		// what the supervisor REPORTED — delivered and not. Showing the
		// undelivered ones is the point rather than an accident: a job whose
		// reports are not arriving is exactly the job somebody is reading
		// about, and its queue is the evidence of what it has been doing.
		if events := m.readProcessChildTransitions(workID, afterSeq); events != nil {
			out["transitions"] = events
		}
	}
	return out, nil
}

// readProcessChildTransitions is one child's reported transitions past a
// cursor, by id.
func (m *Manager) readProcessChildTransitions(workID string, afterSeq uint64) []map[string]any {
	outbox := m.processOutbox()
	if outbox == nil {
		return nil
	}
	events, err := outbox.All(workID, afterSeq, 0)
	if err != nil {
		return nil
	}
	transitions := make([]map[string]any, 0, len(events))
	for _, event := range events {
		transitions = append(transitions, map[string]any{
			"seq": event.Seq, "kind": event.Kind,
			"at": event.At.UTC().Format(time.RFC3339Nano),
		})
	}
	return transitions
}

// CancelProcessChild asks one child to stop, gracefully first.
//
// The stop-file is written FIRST and is the authoritative record of the wish
// (§4: "cancel is stored as desired state... a cancel issued while the host is
// unreachable is not lost"). The supervisor then runs the sequence it owns:
// the stop file, SIGTERM to the whole group where there is such a thing, and a
// kill after the grace. This daemon only reaches for the group itself when the
// supervisor is already gone — because then nobody else will.
func (m *Manager) CancelProcessChild(workID, reason string) (map[string]any, error) {
	workID = normalizeSpawnedWorkTaskID(workID)
	if !procchild.ValidID(workID) {
		return nil, errors.New("a process child id is required")
	}
	m.spawnedWorkMu.Lock()
	var item SpawnedWorkItem
	found := false
	for _, rec := range m.spawnedWork {
		if rec == nil || rec.Item.ID != workID || !rec.Item.Supervised() {
			continue
		}
		item, found = rec.Item, true
		break
	}
	m.spawnedWorkMu.Unlock()
	if !found {
		return nil, fmt.Errorf("no process child %q is tracked on this machine", workID)
	}
	if item.Status != "running" {
		return map[string]any{"ok": true, "already": true, "status": item.Status, "outcome": item.Outcome}, nil
	}
	reason = compactText(redactSensitiveText(reason), 200)
	if reason == "" {
		reason = "cancelled by its owner"
	}
	if err := procchild.RequestStop(m.opts.StateDir, workID, reason); err != nil {
		return nil, err
	}
	supervised := item.SupervisorPID != nil && procchild.PIDAlive(*item.SupervisorPID)
	signalled := []int{}
	if !supervised {
		// No supervisor is left to act on the wish, so the only thing that can
		// stop the work is this daemon reaching the pids directly. It settles
		// nothing: the row becomes `lost` through the ordinary verdict if the
		// supervisor never comes back, which is the honest answer — this daemon
		// did not see the job end.
		if item.PID != nil {
			if signal, _ := m.terminateSpawnedWorkPIDs([]int{*item.PID}); len(signal) > 0 {
				signalled = append(signalled, signal...)
			}
		}
	}
	return map[string]any{
		"ok": true, "already": false, "workId": workID,
		"supervisorAlive": supervised, "signalled": signalled,
		"stopFile": procchild.StopPath(m.opts.StateDir, workID),
	}, nil
}

// active reports a job that has not ended, whether or not it has started.
func (v ProcessChildView) active() bool {
	return v.Status == "running" || v.Status == ProcessStatusQueued
}

func processChildView(item SpawnedWorkItem) ProcessChildView {
	end, _ := time.Parse(time.RFC3339Nano, item.FinishedAt)
	times := ProcessChildTimes(item, end)
	status := item.Status
	if status == "running" && times.StartedAt == "" {
		status = ProcessStatusQueued
	}
	runMs := int64(0)
	if item.Status != "running" {
		runMs = times.RunMs
	}
	return ProcessChildView{
		WorkID: item.ID, Label: item.Label, Status: status, Outcome: item.Outcome,
		Role: item.Role, Owner: item.Owner, ChatID: item.ChatID, TabID: item.TabID,
		QueuedAt: times.QueuedAt, StartedAt: times.StartedAt, QueuedMs: times.QueuedMs, RunMs: runMs,
		UpdatedAt: item.UpdatedAt, FinishedAt: item.FinishedAt,
		ExitCode: item.ExitCode, Summary: item.Summary, OutputFile: item.OutputFile,
		PID: item.PID, SupervisorPID: item.SupervisorPID, ProcessGroup: item.ProcessGroup,
		Isolation: item.Isolation, Resource: item.Resource, QueuePosition: item.QueuePosition,
		Stalled: item.Stalled, MaxRuntimeMs: item.MaxRuntimeMs,
		StaleAfterMs: item.StaleAfterMs, MaxHoldMs: item.MaxHoldMs,
		Reporting: item.Reporting, ReportGapMs: item.ReportGapMs, HeartbeatMs: item.HeartbeatMs,
	}
}

// findProcessChildReceipt looks through every tab's receipts for one ended job. A
// read by id names no chat, and the row that named one has been compacted away.
func (m *Manager) findProcessChildReceipt(workID string) (SpawnedWorkReceipt, bool) {
	stateDir := strings.TrimSpace(m.opts.StateDir)
	if stateDir == "" {
		return SpawnedWorkReceipt{}, false
	}
	paths, _ := filepath.Glob(filepath.Join(stateDir, "spawned-work-receipts", "*.jsonl"))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lines := boundedSpawnedReceiptLines(data)
		for i := len(lines) - 1; i >= 0; i-- {
			var receipt SpawnedWorkReceipt
			if json.Unmarshal(lines[i], &receipt) == nil && normalizeSpawnedWorkTaskID(receipt.TaskID) == workID {
				return receipt, true
			}
		}
	}
	return SpawnedWorkReceipt{}, false
}

// processChildReceiptAnswer is the shape a read of a live job has, from a receipt.
func processChildReceiptAnswer(receipt SpawnedWorkReceipt, tailBytes int, m *Manager) map[string]any {
	view := ProcessChildView{
		WorkID: receipt.TaskID, Label: receipt.Label, Status: receipt.Status, Outcome: receipt.Outcome,
		Role: receipt.Role, ChatID: receipt.ChatID, TabID: receipt.TabID,
		QueuedAt: receipt.QueuedAt, StartedAt: receipt.StartedAt, QueuedMs: receipt.QueuedMs, RunMs: receipt.RunMs,
		UpdatedAt: receipt.FinishedAt, FinishedAt: receipt.FinishedAt, ExitCode: receipt.ExitCode,
		Summary: receipt.Summary, OutputFile: receipt.OutputFile, PID: receipt.PID,
	}
	tail, truncated, size := readProcessChildTail(receipt.OutputFile, tailBytes)
	if tail == "" {
		// The file may have been cleaned up since. The receipt kept its own tail.
		tail, truncated = receipt.OutputTail, receipt.TailLimited
	}
	return map[string]any{
		"ok": true, "child": view, "output_tail": tail, "output_bytes": size,
		"output_truncated": truncated, "from_receipt": true,
	}
}
