package acp

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type startupStage uint8

const (
	startupWorker startupStage = iota
	startupControls
	startupPrepared
	startupWritten
	startupUpdate
	startupContent
	startupTool
	startupThought
	startupText
	startupCancelRequested
	startupCancelWritten
	startupCancelWriteFailed
	startupTerminalReply
	startupFinished
	startupContentPublished
	startupStageCount
)

// One bounded, content-free receipt per completed turn. Measuring at the wire
// boundary distinguishes host preparation from provider silence and from a
// provider already thinking before its first tool. No per-chunk log writes.
// Offsets start at Manager.StartJob, not at the controller's Send click.
type turnStartupTiming struct {
	started    time.Time
	stages     [startupStageCount]atomic.Int64
	lastUpdate atomic.Int64
	outcome    atomic.Int32
	detailMu   sync.Mutex
	input      map[string]int64
	runtime    runtimeDiagnosticState
	// Restored receipts are observations from an earlier daemon, never live work.
	restoredElapsed *time.Duration
}

func (s *turnStartupTiming) finish(outcome string) {
	if s == nil {
		return
	}
	code := int32(1)
	switch outcome {
	case "done":
		code = 2
	case "failed":
		code = 3
	case "cancelled":
		code = 4
	case "admission_failed":
		code = 5
	}
	s.outcome.Store(code)
	s.mark(startupFinished)
}

func (s *turnStartupTiming) mark(stage startupStage) {
	if s == nil || s.stages[stage].Load() != 0 {
		return
	}
	s.stages[stage].CompareAndSwap(0, int64(time.Since(s.started))+1)
}

func (s *turnStartupTiming) fields() map[string]any {
	if s == nil {
		return nil
	}
	elapsed := time.Since(s.started)
	if s.restoredElapsed != nil {
		elapsed = *s.restoredElapsed
	}
	if finished := s.stages[startupFinished].Load(); finished != 0 {
		elapsed = time.Duration(finished - 1)
	}
	fields := map[string]any{"managerElapsedMs": elapsed.Milliseconds()}
	if outcome := s.outcome.Load(); outcome > 0 {
		fields["outcome"] = [...]string{"", "finished", "completed", "failed", "cancelled", "admission_failed"}[outcome]
	}
	names := [...]string{"workerStartedMs", "controlsFinishedMs", "promptPreparedMs", "promptWrittenMs", "firstUpdateMs", "firstContentMs", "firstToolMs", "firstThinkingMs", "firstTextMs", "stopRequestedMs", "cancelWrittenMs", "cancelWriteFailedMs", "terminalReplyMs", "finishedMs", "firstContentPublishedMs"}
	for stage, name := range names {
		if offset := s.stages[stage].Load(); offset != 0 {
			fields[name] = time.Duration(offset - 1).Milliseconds()
		}
	}
	if last := s.lastUpdate.Load(); last != 0 {
		fields["lastUpdateMs"] = time.Duration(last - 1).Milliseconds()
		fields["silenceMs"] = max(int64(0), (elapsed - time.Duration(last-1)).Milliseconds())
	}
	s.detailMu.Lock()
	if s.input != nil {
		input := make(map[string]int64, len(s.input))
		for key, value := range s.input {
			input[key] = value
		}
		fields["input"] = input
	}
	if s.runtime.Observed > 0 {
		fields["runtime"] = s.runtime.clone()
	}
	s.detailMu.Unlock()
	if s.restoredElapsed != nil {
		fields["historical"] = true
	}
	return fields
}

const maxTurnDiagnostics = 256

type turnDiagnostic struct {
	tabID, chatID, jobID, providerID, modelID string
	timing                                    *turnStartupTiming
}

func (m *Manager) retainTurnDiagnostic(job *Job) {
	m.diagnosticsMu.Lock()
	defer m.diagnosticsMu.Unlock()
	if len(m.turnDiagnostics) == maxTurnDiagnostics {
		copy(m.turnDiagnostics, m.turnDiagnostics[1:])
		m.turnDiagnostics = m.turnDiagnostics[:maxTurnDiagnostics-1]
	}
	m.turnDiagnostics = append(m.turnDiagnostics, turnDiagnostic{
		tabID: job.TabID, chatID: job.ChatID, jobID: job.ID, providerID: job.ProviderID,
		modelID: job.startOpts.ModelID, timing: job.startupTiming,
	})
}

// TurnDiagnostics reads only bounded timing metadata, including live turns.
// It never attaches a provider, reads a transcript, changes a turn, or polls.
// Completed and failure-checkpoint receipts also survive a daemon restart in
// the bounded diagnostic store; restored observations never imply a live turn.
func (m *Manager) TurnDiagnostics(tabID, chatID string, limit int) map[string]any {
	if limit < 1 || limit > 20 {
		limit = 5
	}
	m.diagnosticsMu.Lock()
	selected := make([]turnDiagnostic, 0, limit)
	for i := len(m.turnDiagnostics) - 1; i >= 0 && len(selected) < limit; i-- {
		d := m.turnDiagnostics[i]
		if d.tabID == tabID && d.chatID == chatID {
			selected = append(selected, d)
		}
	}
	m.diagnosticsMu.Unlock()
	turns := make([]any, 0, len(selected))
	for _, d := range selected {
		fields := d.timing.fields()
		fields["jobId"] = redactSensitiveText(d.jobID)
		fields["providerId"] = redactSensitiveText(d.providerID)
		fields["modelId"] = redactSensitiveText(d.modelID)
		fields["startedAt"] = d.timing.started.UTC().Format(time.RFC3339Nano)
		fields["active"] = fields["finishedMs"] == nil
		phase := "host_preparation"
		if fields["promptWrittenMs"] != nil {
			phase = "waiting_for_provider_activity"
		}
		if fields["firstUpdateMs"] != nil {
			phase = "provider_activity_without_content"
		}
		if fields["firstThinkingMs"] != nil {
			phase = "provider_thinking_observed"
		}
		if fields["firstTextMs"] != nil {
			phase = "provider_text_observed"
		}
		if fields["firstToolMs"] != nil {
			phase = "provider_tool_observed"
		}
		if fields["stopRequestedMs"] != nil {
			phase = "stopping_before_cancel_delivery"
		}
		if fields["cancelWrittenMs"] != nil {
			phase = "waiting_for_provider_stop"
		}
		if fields["cancelWriteFailedMs"] != nil {
			phase = "cancel_delivery_failed"
		}
		if fields["terminalReplyMs"] != nil {
			phase = "host_finalization"
		}
		if fields["finishedMs"] != nil {
			phase = "finished"
		}
		if fields["historical"] == true {
			fields["active"] = false
			if fields["finishedMs"] == nil {
				phase = "previous_daemon_observation"
			}
		}
		fields["phase"] = phase
		delays := map[string]any{}
		for _, interval := range [][3]string{
			{"providerFirstContent", "promptWrittenMs", "firstContentMs"},
			{"contentPublication", "firstContentMs", "firstContentPublishedMs"},
			{"thinkingToFirstTool", "firstThinkingMs", "firstToolMs"},
			{"stopDelivery", "stopRequestedMs", "cancelWrittenMs"},
			{"providerStopReply", "cancelWrittenMs", "terminalReplyMs"},
			{"hostFinalization", "terminalReplyMs", "finishedMs"},
		} {
			from, fromOK := fields[interval[1]].(int64)
			to, toOK := fields[interval[2]].(int64)
			if fromOK && toOK && to >= from {
				delays[interval[0]] = to - from
			}
		}
		fields["intervalsMs"] = delays
		turns = append(turns, fields)
	}
	attachments := m.recentLaneDiagnostics(tabID, chatID, limit)
	return map[string]any{
		"schemaVersion": 1, "tabId": strings.TrimSpace(tabID), "chatId": strings.TrimSpace(chatID),
		"machineId": m.opts.MachineID, "version": m.opts.Version,
		"sampledAt": time.Now().UTC().Format(time.RFC3339Nano), "turns": turns,
		"available": len(turns) > 0 || len(attachments) > 0, "retention": "newest 256 turn observations, at most 20 per exact chat read; completed turns and throttled failure checkpoints survive restart within a 4 MiB store",
		"persistence":     m.turnDiagnosticPersistence(),
		"runtimeMeaning":  "content-free host observations; input sizes measure current Workass/native-host input, not the upstream model request or entire model context; omitted socket details were not exposed by the provider",
		"laneAttachments": attachments, "attachmentRetention": "newest 64 completed create/resume attempts or failed turn preparations in this daemon lifetime; at most 20 returned for this exact chat",
		"timingOrigin":       "Manager.StartJob; excludes controller/network transit and native session creation before admission",
		"phaseMeaning":       "last observed boundary, not an inference about provider internal work; omitted timestamps were not observed",
		"publicationMeaning": "content published by the daemon; does not measure network transit or controller paint",
	}
}
