package acp

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"workass/internal/procchild"
)

// A job this machine runs for a chat on another machine.
//
// docs/specs/fleet-runtime.md §14e. Where a job runs and who owns it are two
// facts (§6), so its row on this machine is keyed by the job alone and names
// its owner, with no chat here involved: the chat fields stay empty, no chat
// actor sees it, no rail shows it and no provider is looked up for it. It lives
// in the same registry as every other process child, so the list, the read, the
// cancel, the silence notice and the `lost` verdict are the ones that exist.
//
// Its durable record is one file of its own, because the per-tab snapshots and
// receipts that keep a chat's row are exactly what it does not have. A settled
// row stays there, bounded, so a replayed start still finds the job rather than
// running it again and a reader can still ask what became of it.

// maxHostedSettledProcessChildren bounds what a machine remembers about jobs
// that have ended. Its owners hold their own notices; this is only the host's
// memory, so it is a count and not a retention policy.
const maxHostedSettledProcessChildren = 64

func hostedProcessChild(item SpawnedWorkItem) bool {
	return item.Supervised() && strings.TrimSpace(item.TabID) == "" && strings.TrimSpace(item.ChatID) == ""
}

func (m *Manager) hostedProcessChildrenPath() string {
	if strings.TrimSpace(m.opts.StateDir) == "" {
		return ""
	}
	return filepath.Join(m.opts.StateDir, "hosted-process-children.json")
}

// commitProcessChildChange records a change to one process child's row wherever
// that row is held.
func (m *Manager) commitProcessChildChange(tabID, chatID string) {
	if strings.TrimSpace(tabID) != "" || strings.TrimSpace(chatID) != "" {
		m.commitSpawnedWorkChange(tabID, chatID)
		return
	}
	if err := m.persistHostedProcessChildren(); err != nil {
		// Loud, and not a refusal: the job is running and its report was
		// applied, but a restart before the next good write would forget it.
		m.opts.Logf("could not persist the jobs this machine hosts for other machines", map[string]any{
			"error": redactSensitiveText(err.Error()),
		})
	}
}

func (m *Manager) persistHostedProcessChildren() error {
	path := m.hostedProcessChildrenPath()
	if path == "" {
		return nil
	}
	m.spawnedWorkCommitMu.Lock()
	defer m.spawnedWorkCommitMu.Unlock()
	m.spawnedWorkMu.Lock()
	type held struct {
		key  string
		item spawnedWorkSnapshotItem
	}
	running, settled := []held{}, []held{}
	for key, rec := range m.spawnedWork {
		if rec == nil || !hostedProcessChild(rec.Item) {
			continue
		}
		entry := held{key: key, item: spawnedWorkSnapshotItem{
			SpawnedWorkItem: rec.Item, DoneFile: rec.ExternalDoneFile,
			SpoolFile: rec.SpoolFile, SpoolSeq: rec.SpoolSeq,
		}}
		if rec.Item.Status == "running" {
			running = append(running, entry)
		} else {
			settled = append(settled, entry)
		}
	}
	sort.SliceStable(settled, func(i, j int) bool {
		return settled[i].item.FinishedAt > settled[j].item.FinishedAt
	})
	if len(settled) > maxHostedSettledProcessChildren {
		for _, entry := range settled[maxHostedSettledProcessChildren:] {
			delete(m.spawnedWork, entry.key)
		}
		settled = settled[:maxHostedSettledProcessChildren]
	}
	m.spawnedWorkMu.Unlock()
	items := make([]spawnedWorkSnapshotItem, 0, len(running)+len(settled))
	for _, entry := range append(running, settled...) {
		items = append(items, entry.item)
	}
	if len(items) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return writeSpawnedWorkFile(path, data)
}

// loadHostedProcessChildren restores them across a restart. A job whose
// supervisor died while this daemon was down is restored as running and reaches
// `lost` through the ordinary verdict. Unreadable state yields none, loudly.
func (m *Manager) loadHostedProcessChildren() {
	path := m.hostedProcessChildrenPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var items []spawnedWorkSnapshotItem
	if err := json.Unmarshal(data, &items); err != nil {
		m.opts.Logf("the jobs this machine hosts for other machines could not be restored", map[string]any{
			"path": path, "error": redactSensitiveText(err.Error()),
		})
		return
	}
	for _, stored := range items {
		item := stored.SpawnedWorkItem
		taskID := strings.TrimSpace(item.TaskID)
		if !hostedProcessChild(item) || !procchild.ValidID(taskID) || strings.TrimSpace(item.ID) != taskID {
			continue
		}
		if item.OutputFile != "" {
			safe, ok := validateExternalWorkPath(m.opts.StateDir, item.OutputFile)
			if !ok {
				safe = ""
			}
			item.OutputFile = safe
		}
		doneFile := strings.TrimSpace(stored.DoneFile)
		if doneFile != "" {
			safe, ok := validateExternalWorkPath(m.opts.StateDir, doneFile)
			if !ok {
				safe = ""
			}
			doneFile = safe
		}
		rec := &spawnedWorkRecord{
			Item: item, SawPID: item.PID != nil, ExternalDoneFile: doneFile, SpoolSeq: stored.SpoolSeq,
		}
		if item.Status == "running" {
			// Derived rather than read back, like a chat's row: a path from a
			// file is a path somebody could have edited.
			rec.SpoolFile = procchild.OutboxPath(m.opts.StateDir)
		}
		m.spawnedWork[spawnedWorkKey("", "", taskID)] = rec
	}
}

// ProcessChildByID reads back a process child by id, however it is held, so a
// replayed start can answer with the job that already exists.
func (m *Manager) ProcessChildByID(workID string) (ProcessChildView, bool) {
	workID = normalizeSpawnedWorkTaskID(workID)
	m.spawnedWorkMu.Lock()
	defer m.spawnedWorkMu.Unlock()
	for _, rec := range m.spawnedWork {
		if rec != nil && rec.Item.ID == workID && rec.Item.Supervised() {
			return processChildView(rec.Item), true
		}
	}
	return ProcessChildView{}, false
}

// ProcessStatusQueued is what a reader is told about a job whose command has not
// started: it may be waiting for a resource, or in the moment before exec. It is
// a word for readers only. The row's own Status stays the closed lifecycle every
// surface reads, in which anything not "running" has ended.
const ProcessStatusQueued = "queued"

// ProcessTimes is one job's queue and run time, kept apart.
type ProcessTimes struct {
	QueuedAt  string
	StartedAt string
	QueuedMs  int64
	RunMs     int64
	// Unsplit is a job that started at a moment nobody recorded, whose wait and run
	// are one number.
	Unsplit bool
}

// ProcessChildTimes separates a job's wait from its run. end is when it ended,
// and zero while it has not.
//
// A row written before these fields existed has only StartedAt, which was when
// the row was first written: that reads as when it was asked for, and it counts
// as having started once it holds a child pid. Such a job's supervisor still
// reports its run time when it settles, so a job that straddles an update gets
// the right numbers too.
func ProcessChildTimes(item SpawnedWorkItem, end time.Time) ProcessTimes {
	out := ProcessTimes{QueuedAt: firstNonEmpty(strings.TrimSpace(item.QueuedAt), strings.TrimSpace(item.StartedAt))}
	queued, _ := time.Parse(time.RFC3339Nano, out.QueuedAt)
	exec, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(item.ExecStartedAt))
	switch {
	case !exec.IsZero():
		out.StartedAt = strings.TrimSpace(item.ExecStartedAt)
	case item.PID != nil && item.RunMs > 0 && !end.IsZero():
		exec = end.Add(-time.Duration(item.RunMs) * time.Millisecond)
		out.StartedAt = exec.UTC().Format(time.RFC3339Nano)
	case item.PID != nil:
		// Started, at a moment nobody recorded. The row's first write is the
		// nearest thing known, and the wait cannot be told from the run, so no run
		// time is claimed: a figure that includes the queue is not one. A receipt
		// with none reports <elapsed>, which is what that number always was.
		exec = queued
		out.StartedAt = out.QueuedAt
		out.Unsplit = true
	}
	out.RunMs = item.RunMs
	if out.RunMs <= 0 && !out.Unsplit && !exec.IsZero() && !end.IsZero() && end.After(exec) {
		out.RunMs = end.Sub(exec).Milliseconds()
	}
	if out.Unsplit {
		return out
	}
	waitedUntil := exec
	if waitedUntil.IsZero() {
		waitedUntil = end
	}
	if waitedUntil.IsZero() {
		waitedUntil = time.Now().UTC()
	}
	if !queued.IsZero() && waitedUntil.After(queued) {
		out.QueuedMs = waitedUntil.Sub(queued).Milliseconds()
	}
	return out
}

// earlierProcessStamp keeps the earlier of a stored stamp and an event's own
// time: the launch row and the supervisor's first event describe one moment, and
// a report can arrive long after the event it carries.
func earlierProcessStamp(stored string, at time.Time) string {
	if at.IsZero() {
		return stored
	}
	if current, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(stored)); err == nil && !current.After(at) {
		return stored
	}
	return at.UTC().Format(time.RFC3339Nano)
}
