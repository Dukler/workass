package acp

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Work that runs on another machine, as a row in the chat that asked.
//
// docs/specs/fleet-runtime.md §6 and §14f. Where work runs and who owns it are
// two facts, so the asking chat holds the ordinary row every surface already
// renders and the host states what is true about it. This file is both halves:
// the host's reading of its own work, and the owner applying what it is told.
//
// The state is not an event in the entity log. It is the latest value of a row,
// replaced rather than accumulated: a progress line must never grow a log or
// wake anybody, and a value lost to a dropped link is stated again when the link
// comes back (§5). The log keeps what must not be lost, which is the outcome.
//
// A hosted row is read and stopped only through its host. Nothing here probes a
// pid or opens a path from such a row, because both name things on the other
// machine and may mean something else entirely on this one.

// The two kinds of work a host can state.
const (
	HostedWorkSubagent = "subagent"
	HostedWorkProcess  = "process"
)

// HostedWorkState is one host's statement about one piece of its work.
type HostedWorkState struct {
	Kind       string
	WorkID     string
	Label      string
	ProviderID string
	ModelLabel string
	Phase      string
	Activity   string
	Role       string
	Resource   string
	// QueuedAt and ExecStartedAt keep a job's wait apart from its run (§14e).
	QueuedAt      string
	ExecStartedAt string
	StartedAt     string
	QueuePosition int
	OutputFile    string
	PID           *int
	Isolation     string
	Stalled       bool
	Reporting     string

	// Filled in by the owner, never stated by the host.
	HostMachine       string
	HostMachineName   string
	OriginLaneID      string
	OriginOperationID string
	OriginTurnID      string
}

// HostedWorkRouter reaches the machine a hosted row runs on. It is installed by
// the daemon, which owns the links.
type HostedWorkRouter struct {
	Stop func(ctx context.Context, item SpawnedWorkItem) error
	Read func(ctx context.Context, item SpawnedWorkItem, tailBytes int) (string, bool, error)
}

// hostedWorkCallTimeout bounds one stop or output read against a far machine, so
// a dark host answers with an error rather than holding the click open.
const hostedWorkCallTimeout = 20 * time.Second

// SetHostedWorkRouter installs the route to other machines' work.
func (m *Manager) SetHostedWorkRouter(router HostedWorkRouter) {
	if m != nil {
		m.hostedWorkRouter.Store(&router)
	}
}

func hostedWorkItem(item SpawnedWorkItem) bool { return strings.TrimSpace(item.HostMachine) != "" }

// hostedJobItem is a supervised job running elsewhere, which reads its wait and
// its run the same way a supervised job here does.
func hostedJobItem(item SpawnedWorkItem) bool {
	return hostedWorkItem(item) && item.Kind == "external"
}

// IsHostedJob is hostedJobItem for the daemon's projection of a row.
func IsHostedJob(item SpawnedWorkItem) bool { return hostedJobItem(item) }

// ApplyHostedWorkState makes the asking chat's row say what its host just said,
// creating the row if this is the first anyone has heard of it. That covers a
// job this chat started, a job started on the other machine with this chat as
// its owner, and a job that was already running when this daemon updated.
func (m *Manager) ApplyHostedWorkState(tabID, chatID string, state HostedWorkState) bool {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	id := normalizeSpawnedWorkTaskID(state.WorkID)
	if m == nil || tabID == "" || chatID == "" || id == "" {
		return false
	}
	key := spawnedWorkKey(tabID, chatID, id)
	m.spawnedWorkMu.Lock()
	_, exists := m.spawnedWork[key]
	m.spawnedWorkMu.Unlock()
	if !exists {
		// A statement that crossed the job's own end must not bring it back.
		if _, settled := m.findSpawnedWorkReceipt(tabID, chatID, id); settled {
			return false
		}
	}
	now := isoNow()
	changed := false
	m.spawnedWorkMu.Lock()
	rec := m.spawnedWork[key]
	if rec == nil {
		item := SpawnedWorkItem{
			ID: id, TaskID: id, TabID: tabID, ChatID: chatID,
			ProviderID: strings.TrimSpace(state.ProviderID),
			Label:      firstNonEmpty(compactText(redactSensitiveText(state.Label), 240), id),
			Status:     "running", UpdatedAt: now,
			StartedAt:         firstNonEmpty(strings.TrimSpace(state.StartedAt), strings.TrimSpace(state.QueuedAt), now),
			HostMachine:       strings.TrimSpace(state.HostMachine),
			HostMachineName:   strings.TrimSpace(state.HostMachineName),
			OriginLaneID:      strings.TrimSpace(state.OriginLaneID),
			OriginOperationID: strings.TrimSpace(state.OriginOperationID),
			OriginTurnID:      strings.TrimSpace(state.OriginTurnID),
		}
		if state.Kind == HostedWorkSubagent {
			item.Kind = trackedSubagentSpawnedWorkKind
		} else {
			item.Kind = "external"
			item.Role = normalizeSpawnedWorkRole(state.Role)
		}
		rec = &spawnedWorkRecord{Item: item}
		m.spawnedWork[key] = rec
		changed = true
	}
	if rec.Item.Status == "running" && hostedWorkItem(rec.Item) {
		if applyHostedWorkFields(&rec.Item, state) {
			changed = true
			rec.Item.UpdatedAt = now
		}
	} else {
		changed = false
	}
	m.spawnedWorkMu.Unlock()
	if changed {
		m.commitSpawnedWorkChange(tabID, chatID)
	}
	return changed
}

// applyHostedWorkFields copies what the host stated onto a row. A field the host
// did not state is left alone, because a statement is about what it mentions.
func applyHostedWorkFields(item *SpawnedWorkItem, state HostedWorkState) bool {
	changed := false
	text := func(target *string, value string, limit int) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		value = compactText(redactSensitiveText(value), limit)
		if *target != value {
			*target = value
			changed = true
		}
	}
	text(&item.Label, state.Label, 240)
	text(&item.ProviderID, state.ProviderID, 120)
	text(&item.ModelLabel, state.ModelLabel, 120)
	text(&item.LastToolName, state.Phase, 120)
	text(&item.Summary, state.Activity, 1000)
	text(&item.Resource, state.Resource, 120)
	text(&item.QueuedAt, state.QueuedAt, 64)
	text(&item.ExecStartedAt, state.ExecStartedAt, 64)
	text(&item.OutputFile, state.OutputFile, 1024)
	text(&item.Isolation, state.Isolation, 64)
	text(&item.HostMachineName, state.HostMachineName, 120)
	if state.PID != nil && (item.PID == nil || *item.PID != *state.PID) {
		value := *state.PID
		item.PID = &value
		changed = true
	}
	if item.QueuePosition != state.QueuePosition {
		item.QueuePosition = state.QueuePosition
		changed = true
	}
	if item.Stalled != state.Stalled {
		item.Stalled = state.Stalled
		changed = true
	}
	if item.Reporting != state.Reporting {
		item.Reporting = state.Reporting
		changed = true
	}
	return changed
}

// SettleHostedProcess ends the asking chat's row from the receipt the job's
// monitor delivered, including a cancel, which is told to nobody but still ends
// the row. The notice is a separate decision and is not made here.
func (m *Manager) SettleHostedProcess(tabID, chatID string, receipt SpawnedWorkReceipt, outcome string) bool {
	id := normalizeSpawnedWorkTaskID(receipt.TaskID)
	if m == nil || id == "" {
		return false
	}
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	changed := false
	m.spawnedWorkMu.Lock()
	rec := m.spawnedWork[spawnedWorkKey(tabID, chatID, id)]
	if rec != nil && hostedJobItem(rec.Item) {
		status := "exited"
		switch normalizeProcessOutcome(outcome) {
		case "failed", "timed_out":
			status = "failed"
		case "lost":
			status = "orphaned"
		}
		if changed = m.settleSpawnedWorkLocked(rec, status, receipt.ExitCode); changed {
			rec.Item.Outcome = strings.TrimSpace(outcome)
			rec.Item.QueuePosition = 0
			if summary := compactText(redactSensitiveText(receipt.Summary), 1000); summary != "" {
				rec.Item.Summary = summary
			}
			if receipt.RunMs > 0 {
				rec.Item.RunMs = receipt.RunMs
			}
			if at := strings.TrimSpace(receipt.StartedAt); at != "" && rec.Item.ExecStartedAt == "" {
				rec.Item.ExecStartedAt = at
			}
			if at := strings.TrimSpace(receipt.FinishedAt); at != "" {
				rec.Item.FinishedAt = at
			}
			rec.Item.UpdatedAt = rec.Item.FinishedAt
		}
	}
	m.spawnedWorkMu.Unlock()
	if changed {
		m.commitSpawnedWorkChange(tabID, chatID)
	}
	return changed
}

// ApplyHostedSubagentCall puts one tool call of a child running elsewhere into
// the parent turn, the same event a child on this machine produces, so the row's
// activity and its opened detail read the same.
func (m *Manager) ApplyHostedSubagentCall(tabID, chatID, id string, event map[string]any) {
	id = normalizeSpawnedWorkTaskID(id)
	if m == nil || id == "" || len(event) == 0 {
		return
	}
	parentJobID := m.hostedParentJob(strings.TrimSpace(tabID), strings.TrimSpace(chatID), id)
	if parentJobID == "" {
		return
	}
	m.emit("job:event", map[string]any{"type": "acp", "id": parentJobID, "event": event})
}

// hostedParentJob is the turn a hosted child's events belong to: the one that
// spawned it, or after a restart here the chat's running turn.
func (m *Manager) hostedParentJob(tabID, chatID, id string) string {
	m.spawnedWorkMu.Lock()
	parent := ""
	if rec := m.spawnedWork[spawnedWorkKey(tabID, chatID, id)]; rec != nil {
		parent = rec.HostedParentJobID
	}
	m.spawnedWorkMu.Unlock()
	if parent != "" {
		return parent
	}
	if job, running := m.RunningJobForChat(tabID, chatID); running {
		return strings.TrimSpace(asString(job["id"]))
	}
	return ""
}

// StopHostedWork answers a stop for a row whose work runs elsewhere. The second
// result is false for every other row. A success is a wish delivered, and the
// row ends when the host says the work has.
func (m *Manager) StopHostedWork(tabID, chatID, id string) (map[string]any, bool) {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	id = normalizeSpawnedWorkTaskID(id)
	var item SpawnedWorkItem
	found := false
	m.spawnedWorkMu.Lock()
	for _, rec := range m.spawnedWork {
		if rec != nil && rec.Item.TabID == tabID && rec.Item.ChatID == chatID && rec.Item.ID == id && hostedWorkItem(rec.Item) {
			item, found = rec.Item, true
			break
		}
	}
	m.spawnedWorkMu.Unlock()
	if !found {
		return nil, false
	}
	if item.Status != "running" {
		return map[string]any{"ok": true, "id": item.ID, "status": item.Status, "alreadyFinished": true}, true
	}
	router := m.hostedWorkRouter.Load()
	if router == nil || router.Stop == nil {
		return map[string]any{"ok": false, "id": item.ID, "error": "this daemon has no route to the machine running that work"}, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), hostedWorkCallTimeout)
	defer cancel()
	if err := router.Stop(ctx, item); err != nil {
		return map[string]any{"ok": false, "id": item.ID, "error": redactSensitiveText(err.Error())}, true
	}
	return map[string]any{"ok": true, "id": item.ID, "status": "cancelling", "cancelled": true}, true
}

// readHostedWorkTail is a hosted row's output, read where the file is.
func (m *Manager) readHostedWorkTail(item SpawnedWorkItem, tailBytes int) (string, bool, error) {
	router := m.hostedWorkRouter.Load()
	if router == nil || router.Read == nil {
		return "", false, errors.New("this daemon has no route to the machine running that work")
	}
	ctx, cancel := context.WithTimeout(context.Background(), hostedWorkCallTimeout)
	defer cancel()
	return router.Read(ctx, item, tailBytes)
}

// hostedProgressMinGap keeps a chatty child to a few statements a second. A
// change of phase always goes, and the tool calls travel on their own.
const hostedProgressMinGap = 500 * time.Millisecond

// forwardHostedSubagentProgress states a hosted child's phase and activity to
// the chat that asked.
func (m *Manager) forwardHostedSubagentProgress(id, phase, activity string) {
	state, ok := m.HostedWorkStateFor(HostedWorkSubagent, id)
	if !ok {
		return
	}
	now := time.Now()
	m.hostedProgressMu.Lock()
	if m.hostedProgress == nil {
		m.hostedProgress = map[string]hostedProgressMark{}
	}
	last := m.hostedProgress[id]
	if (last.phase == phase && last.activity == activity) || (last.phase == phase && now.Sub(last.at) < hostedProgressMinGap) {
		m.hostedProgressMu.Unlock()
		return
	}
	m.hostedProgress[id] = hostedProgressMark{phase: phase, activity: activity, at: now}
	m.hostedProgressMu.Unlock()
	m.emitSubagentLifecycle(SubagentLifecycleEvent{Kind: SubagentLifecycleProgress, SubagentID: id, State: &state})
}

type hostedProgressMark struct {
	phase, activity string
	at              time.Time
}

// forwardHostedSubagentCall states one tool call of a hosted child.
func (m *Manager) forwardHostedSubagentCall(id string, event map[string]any) {
	id = strings.TrimSpace(id)
	m.mu.Lock()
	run := m.subagents[id]
	hosted := run != nil && strings.TrimSpace(run.originMachine) != ""
	m.mu.Unlock()
	if !hosted {
		return
	}
	call := make(map[string]any, len(event))
	for key, value := range event {
		if text, isText := value.(string); isText {
			value = redactSensitiveText(text)
		}
		call[key] = value
	}
	delete(call, "images")
	m.emitSubagentLifecycle(SubagentLifecycleEvent{Kind: SubagentLifecycleCall, SubagentID: id, Call: call})
}

// HostedWorkStateFor is this machine's statement about work it runs for another
// machine, while that work is still running.
func (m *Manager) HostedWorkStateFor(kind, id string) (HostedWorkState, bool) {
	id = normalizeSpawnedWorkTaskID(id)
	if m == nil || id == "" {
		return HostedWorkState{}, false
	}
	if kind == HostedWorkSubagent {
		m.mu.Lock()
		defer m.mu.Unlock()
		run := m.subagents[id]
		if run == nil || run.Status != "running" || strings.TrimSpace(run.originMachine) == "" {
			return HostedWorkState{}, false
		}
		return HostedWorkState{
			Kind: HostedWorkSubagent, WorkID: id, Label: run.Label, ProviderID: run.ProviderID,
			ModelLabel: run.ModelLabel, Phase: run.Phase, Activity: run.LatestActivity, StartedAt: run.StartedAt,
		}, true
	}
	view, found := m.ProcessChildByID(id)
	if !found || !view.active() {
		return HostedWorkState{}, false
	}
	return hostedWorkStateFromView(view), true
}

func hostedWorkStateFromView(view ProcessChildView) HostedWorkState {
	return HostedWorkState{
		Kind: HostedWorkProcess, WorkID: view.WorkID, Label: view.Label, Role: view.Role,
		Activity: view.Summary, Resource: view.Resource, QueuedAt: view.QueuedAt,
		ExecStartedAt: view.StartedAt, StartedAt: view.QueuedAt, QueuePosition: view.QueuePosition,
		OutputFile: view.OutputFile, PID: view.PID, Isolation: view.Isolation,
		Stalled: view.Stalled, Reporting: view.Reporting,
	}
}

// CancelHostedSubagent stops a child this machine runs for another machine, on
// the word of the machine it runs for and of no other.
func (m *Manager) CancelHostedSubagent(id, peer string) (map[string]any, error) {
	id = normalizeSpawnedWorkTaskID(id)
	peer = strings.TrimSpace(peer)
	m.mu.Lock()
	run := m.subagents[id]
	origin := ""
	if run != nil {
		origin = strings.TrimSpace(run.originMachine)
	}
	m.mu.Unlock()
	if origin == "" {
		return nil, errors.New("no subagent with that id runs on this machine for another machine")
	}
	if peer != "" && origin != peer {
		return nil, errors.New("that subagent runs for another machine, so this one may not stop it")
	}
	if !m.cancelSubagentByID(id, "") {
		return map[string]any{"ok": true, "already": true}, nil
	}
	return map[string]any{"ok": true, "cancelled": true}, nil
}
