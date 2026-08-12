package acp

import "strings"

// chatHasLiveParkEvidence answers one executor-only question: is something
// alive that will resume this chat by itself? The result is evidence supplied
// to the actor; it is never persisted as a second chat state machine.
func (m *Manager) chatHasLiveParkEvidence(tabID, chatID string) bool {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if tabID == "" || chatID == "" {
		return false
	}
	type probeTarget struct {
		key  string
		path string
	}
	targets := []probeTarget{}
	m.spawnedWorkMu.Lock()
	for key, rec := range m.spawnedWork {
		if rec == nil || rec.Item.TabID != tabID || rec.Item.ChatID != chatID || rec.Item.Status != "running" || isServiceSpawnedWork(rec.Item) {
			continue
		}
		switch spawnedWorkLivenessClassFor(rec.Item) {
		case spawnedWorkLivenessExternal:
			done, _ := readExternalDoneFile(m.opts.StateDir, rec.ExternalDoneFile)
			if !done && (rec.Item.PID == nil || externalPIDAlive(*rec.Item.PID)) {
				m.spawnedWorkMu.Unlock()
				return true
			}
		case spawnedWorkLivenessSubagent, spawnedWorkLivenessInProcess:
			m.spawnedWorkMu.Unlock()
			return true
		default:
			if rec.Item.OutputFile != "" {
				targets = append(targets, probeTarget{key: key, path: rec.Item.OutputFile})
			} else if rec.MissingSince.IsZero() {
				m.spawnedWorkMu.Unlock()
				return true
			}
		}
	}
	m.spawnedWorkMu.Unlock()
	if len(targets) == 0 {
		return false
	}
	paths := make([]string, 0, len(targets))
	seen := map[string]struct{}{}
	for _, target := range targets {
		if _, duplicate := seen[target.path]; duplicate {
			continue
		}
		seen[target.path] = struct{}{}
		paths = append(paths, target.path)
	}
	pidsByPath, supported := m.opts.SpawnedWorkPIDProbe(paths)
	if !supported {
		m.spawnedWorkMu.Lock()
		defer m.spawnedWorkMu.Unlock()
		for _, target := range targets {
			if rec := m.spawnedWork[target.key]; rec != nil && rec.MissingSince.IsZero() {
				return true
			}
		}
		return false
	}
	for _, target := range targets {
		if len(pidsByPath[target.path]) > 0 {
			return true
		}
	}
	return false
}

func (m *Manager) chatHasPendingPermission(tabID, chatID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rec := range m.permissions {
		if rec == nil {
			continue
		}
		job := m.jobs[rec.jobID]
		if job != nil && job.TabID == tabID && job.ChatID == chatID {
			return true
		}
	}
	return false
}

// classifyDispositionForJob combines provider, harness, permission, and
// executor-liveness evidence into one typed terminal receipt. The chat actor is
// the only owner that applies and persists the resulting obligation state.
func (m *Manager) classifyDispositionForJob(job *Job) {
	if job == nil || job.internal || job.SubagentID != "" || job.TabID == "" || job.ChatID == "" {
		return
	}
	signal := completionAdapterFor(job.ProviderID).FromNative(TurnOutcome{
		StopReason: job.StopReason, ExitCode: job.Code, Interrupted: job.Interrupted, ErrorText: job.Error,
	})
	if signal.Disposition == DispositionDeferred || signal.Disposition == DispositionCancelled {
		job.DispositionState = string(signal.Disposition)
		job.DispositionSource = signal.Source
		job.DispositionNote = compactText(signal.Note, maxDispositionNote)
		return
	}
	if m.chatHasPendingPermission(job.TabID, job.ChatID) {
		signal = CompletionSignal{Disposition: DispositionNeedsInput, Source: dispositionSourceInferred}
	}
	harness := m.harnessTurns.get(job.SessionID)
	if harness.Complete() {
		switch {
		case harness.Parked && (signal.Disposition == DispositionDone || signal.Disposition == DispositionUnknown):
			signal = CompletionSignal{Disposition: DispositionParked, Source: dispositionSourceHarness, Note: harness.ParkNote}
		case harness.Quiet() && signal.Disposition == DispositionUnknown && harness.TerminalReason == "completed":
			signal = CompletionSignal{Disposition: DispositionDone, Source: dispositionSourceHarness}
		}
	}
	evidence := false
	if signal.Disposition == DispositionDone || signal.Disposition == DispositionUnknown {
		evidence = m.chatHasLiveParkEvidence(job.TabID, job.ChatID)
	}
	if signal.Disposition == DispositionUnknown {
		if evidence {
			signal.Disposition = DispositionParked
		} else {
			signal.Disposition = DispositionDone
		}
		signal.Source = dispositionSourceInferred
	} else if signal.Disposition == DispositionDone && evidence {
		signal.Disposition = DispositionParked
		signal.Source = dispositionSourceInferred
	}
	job.DispositionState = string(signal.Disposition)
	job.DispositionSource = signal.Source
	job.DispositionNote = compactText(signal.Note, maxDispositionNote)
}

// ObligationEvidence is a transient executor observation. The actor applies it
// under a serialized ReconcileObligation command.
type ObligationEvidence struct {
	Live         bool
	HarnessQuiet bool
}

func (m *Manager) ChatObligationEvidence(tabID, chatID string) ObligationEvidence {
	if m == nil {
		return ObligationEvidence{}
	}
	return ObligationEvidence{
		Live:         m.chatHasLiveParkEvidence(tabID, chatID),
		HarnessQuiet: m.harnessTurns.getByChat(tabID, chatID).Quiet(),
	}
}
