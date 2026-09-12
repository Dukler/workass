package acp

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const turnDiagnosticFilename = "turn-diagnostics.json"
const maxTurnDiagnosticStoreBytes = 4 * 1024 * 1024

type storedTurnDiagnostic struct {
	TabID      string                   `json:"tabId"`
	ChatID     string                   `json:"chatId"`
	JobID      string                   `json:"jobId"`
	ProviderID string                   `json:"providerId"`
	ModelID    string                   `json:"modelId"`
	Started    time.Time                `json:"started"`
	Elapsed    time.Duration            `json:"elapsed"`
	Stages     [startupStageCount]int64 `json:"stages"`
	LastUpdate int64                    `json:"lastUpdate"`
	Outcome    int32                    `json:"outcome"`
	Input      map[string]int64         `json:"input,omitempty"`
	Runtime    runtimeDiagnosticState   `json:"runtime"`
}

type turnDiagnosticDisk struct {
	Version int                    `json:"version"`
	Turns   []storedTurnDiagnostic `json:"turns"`
}

func (m *Manager) turnDiagnosticPersistence() map[string]any {
	m.diagnosticStatusMu.Lock()
	defer m.diagnosticStatusMu.Unlock()
	result := map[string]any{"enabled": strings.TrimSpace(m.opts.StateDir) != ""}
	if m.diagnosticPersistenceError != "" {
		result["error"] = m.diagnosticPersistenceError
	}
	return result
}

func (m *Manager) setDiagnosticPersistenceError(value string) {
	m.diagnosticStatusMu.Lock()
	m.diagnosticPersistenceError = value
	m.diagnosticStatusMu.Unlock()
}

func (m *Manager) startDiagnosticWriter() {
	if strings.TrimSpace(m.opts.StateDir) == "" {
		return
	}
	m.diagnosticWake = make(chan struct{}, 1)
	m.diagnosticStop = make(chan struct{})
	m.diagnosticDone = make(chan struct{})
	go func() {
		defer close(m.diagnosticDone)
		var timer *time.Timer
		var due <-chan time.Time
		stopTimer := func() {
			if timer != nil {
				timer.Stop()
			}
			due = nil
		}
		defer stopTimer()
		for {
			select {
			case <-m.diagnosticWake:
				force := m.diagnosticForce.Swap(false)
				m.diagnosticWriteMu.Lock()
				delay := time.Second - time.Since(m.diagnosticLastCheckpoint)
				m.diagnosticWriteMu.Unlock()
				if force || delay <= 0 {
					stopTimer()
					m.persistTurnDiagnostics(force)
				} else if due == nil {
					timer = time.NewTimer(delay)
					due = timer.C
				}
			case <-due:
				due = nil
				m.persistTurnDiagnostics(false)
			case <-m.diagnosticStop:
				m.persistTurnDiagnostics(true)
				return
			}
		}
	}()
}

// A single coalesced wake keeps disk I/O off the provider stdout reader and
// public diagnostics path. No goroutine or unbounded queue per notification.
func (m *Manager) queueTurnDiagnostics(force bool) {
	if m.diagnosticWake == nil {
		return
	}
	if force {
		m.diagnosticForce.Store(true)
	}
	select {
	case m.diagnosticWake <- struct{}{}:
	default:
	}
}

// Reset calls this after all admitted jobs finish, so a final snapshot includes
// their terminal observations without racing removal of the profile directory.
func (m *Manager) stopDiagnosticWriter() {
	if m.diagnosticStop == nil {
		return
	}
	m.diagnosticStopOnce.Do(func() { close(m.diagnosticStop) })
	<-m.diagnosticDone
}

func (d turnDiagnostic) stored() storedTurnDiagnostic {
	s := d.timing
	record := storedTurnDiagnostic{
		TabID: redactSensitiveText(d.tabID), ChatID: redactSensitiveText(d.chatID),
		JobID: redactSensitiveText(d.jobID), ProviderID: redactSensitiveText(d.providerID), ModelID: redactSensitiveText(d.modelID),
		Started: s.started, Elapsed: time.Since(s.started), LastUpdate: s.lastUpdate.Load(), Outcome: s.outcome.Load(),
	}
	if s.restoredElapsed != nil {
		record.Elapsed = *s.restoredElapsed
	}
	for i := range record.Stages {
		record.Stages[i] = s.stages[i].Load()
	}
	if finish := record.Stages[startupFinished]; finish > 0 {
		record.Elapsed = time.Duration(finish - 1)
	}
	s.detailMu.Lock()
	defer s.detailMu.Unlock()
	if s.input != nil {
		record.Input = make(map[string]int64, len(s.input))
		for key, value := range s.input {
			record.Input[key] = value
		}
	}
	record.Runtime = s.runtime.clone()
	return record
}

// Failure checkpoints are limited to one write per second across all chats.
// Completion always flushes. Only bounded, content-free receipts are written;
// no transcript read, provider request, or polling is needed.
func (m *Manager) persistTurnDiagnostics(force bool) {
	if strings.TrimSpace(m.opts.StateDir) == "" {
		return
	}
	m.diagnosticWriteMu.Lock()
	defer m.diagnosticWriteMu.Unlock()
	if !force && time.Since(m.diagnosticLastCheckpoint) < time.Second {
		return
	}
	m.diagnosticLastCheckpoint = time.Now()
	m.diagnosticsMu.Lock()
	diagnostics := append([]turnDiagnostic(nil), m.turnDiagnostics...)
	m.diagnosticsMu.Unlock()
	disk := turnDiagnosticDisk{Version: 1, Turns: make([]storedTurnDiagnostic, 0, len(diagnostics))}
	for _, d := range diagnostics {
		if d.timing != nil {
			disk.Turns = append(disk.Turns, d.stored())
		}
	}
	data, err := json.Marshal(disk)
	for err == nil && len(data) > maxTurnDiagnosticStoreBytes && len(disk.Turns) > 0 {
		disk.Turns = disk.Turns[1:]
		data, err = json.Marshal(disk)
	}
	if err != nil {
		m.setDiagnosticPersistenceError("encode_failed")
		return
	}
	if err = os.MkdirAll(m.opts.StateDir, 0o700); err != nil {
		m.setDiagnosticPersistenceError("write_failed")
		return
	}
	// Reuse one exact staging name. A killed writer can leave at most this
	// single bounded artifact; never follow a planted staging symlink.
	tmpName := filepath.Join(m.opts.StateDir, ".turn-diagnostics.pending")
	if stat, statErr := os.Lstat(tmpName); statErr == nil {
		if !stat.Mode().IsRegular() || os.Remove(tmpName) != nil {
			m.setDiagnosticPersistenceError("write_failed")
			return
		}
	} else if !os.IsNotExist(statErr) {
		m.setDiagnosticPersistenceError("write_failed")
		return
	}
	tmp, err := os.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		m.setDiagnosticPersistenceError("write_failed")
		return
	}
	defer os.Remove(tmpName)
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(m.opts.StateDir, turnDiagnosticFilename))
	}
	if err != nil {
		m.setDiagnosticPersistenceError("write_failed")
		return
	}
	m.setDiagnosticPersistenceError("")
}

func restoreDiagnosticEvent(raw map[string]any) map[string]any {
	event := sanitizeRuntimeDiagnostic(raw)
	if event == nil {
		return nil
	}
	for _, key := range []string{"sequence", "atMs"} {
		if n, ok := diagnosticNumber(raw[key]); ok {
			event[key] = n
		}
	}
	return event
}

func (record storedTurnDiagnostic) restore() (turnDiagnostic, bool) {
	for _, id := range []string{record.TabID, record.ChatID, record.JobID, record.ProviderID, record.ModelID} {
		if len(id) > 512 || redactSensitiveText(id) != id || strings.ContainsAny(id, "\r\n\x00") {
			return turnDiagnostic{}, false
		}
	}
	if record.TabID == "" || record.ChatID == "" || record.JobID == "" || record.Started.IsZero() || record.Elapsed < 0 || record.Outcome < 0 || record.Outcome > 5 || record.LastUpdate < 0 {
		return turnDiagnostic{}, false
	}
	s := &turnStartupTiming{started: record.Started, restoredElapsed: &record.Elapsed}
	for i, stage := range record.Stages {
		if stage < 0 {
			return turnDiagnostic{}, false
		}
		s.stages[i].Store(stage)
	}
	s.outcome.Store(record.Outcome)
	s.lastUpdate.Store(record.LastUpdate)
	if record.Input != nil {
		s.input = map[string]int64{}
		for _, key := range []string{"preparedTextBytes", "requestTextBytes", "initialSeedMessages", "contextDeltaMessages", "imageCount", "toolBriefBytes"} {
			if n, ok := record.Input[key]; ok && n >= 0 {
				s.input[key] = n
			}
		}
	}
	r := record.Runtime
	for _, n := range []int64{r.Observed, r.Retries, r.Errors, r.HostErrors, r.Compactions, r.Fallbacks, r.DroppedEvents} {
		if n < 0 {
			return turnDiagnostic{}, false
		}
	}
	if len(r.Events) > maxRuntimeDiagnosticEvents {
		return turnDiagnostic{}, false
	}
	r.Input, r.Usage = restoreDiagnosticEvent(r.Input), restoreDiagnosticEvent(r.Usage)
	r.Events = nil
	for _, raw := range record.Runtime.Events {
		if event := restoreDiagnosticEvent(raw); event != nil {
			r.Events = append(r.Events, event)
		}
	}
	s.runtime = r
	return turnDiagnostic{tabID: record.TabID, chatID: record.ChatID, jobID: record.JobID, providerID: record.ProviderID, modelID: record.ModelID, timing: s}, true
}

func (m *Manager) loadTurnDiagnostics() {
	if strings.TrimSpace(m.opts.StateDir) == "" {
		return
	}
	name := filepath.Join(m.opts.StateDir, turnDiagnosticFilename)
	stat, err := os.Lstat(name)
	if os.IsNotExist(err) {
		return
	}
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > maxTurnDiagnosticStoreBytes {
		m.setDiagnosticPersistenceError("unreadable_store")
		return
	}
	f, err := os.Open(name)
	if err != nil {
		m.setDiagnosticPersistenceError("unreadable_store")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxTurnDiagnosticStoreBytes+1))
	if err != nil || len(data) > maxTurnDiagnosticStoreBytes {
		m.setDiagnosticPersistenceError("unreadable_store")
		return
	}
	var disk turnDiagnosticDisk
	if json.Unmarshal(data, &disk) != nil || disk.Version != 1 || len(disk.Turns) > maxTurnDiagnostics {
		m.setDiagnosticPersistenceError("invalid_store")
		return
	}
	for _, record := range disk.Turns {
		d, ok := record.restore()
		if !ok {
			m.setDiagnosticPersistenceError("invalid_store")
			continue
		}
		m.turnDiagnostics = append(m.turnDiagnostics, d)
	}
}
