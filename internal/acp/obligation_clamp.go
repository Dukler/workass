package acp

import (
	"strings"
	"time"
)

// chatHasLiveParkEvidence answers one question: is there something alive that
// will resume this chat by itself?
//
// It is deliberately its own function rather than a reuse of
// hasRunningSpawnedWorkForChat, which excludes external and subagent classes
// for bridge-pinning reasons (spawned_work.go:877) and would therefore report
// "nothing alive" for exactly the lanes most likely to be holding a park.
//
// The standard of proof is high in one direction only. Evidence DEMOTES a
// claimed done to parked, and a wrong demotion is the expensive mistake:
// Workass has already been stuck once by a phantom running row vetoing
// recovery, so a row that merely says "running" is not enough where the
// platform can check.
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
		if rec == nil || rec.Item.TabID != tabID || rec.Item.ChatID != chatID {
			continue
		}
		if rec.Item.Status != "running" || isServiceSpawnedWork(rec.Item) {
			continue
		}
		switch spawnedWorkLivenessClassFor(rec.Item) {
		case spawnedWorkLivenessExternal:
			// An external lane is alive until its done marker appears. A lane
			// registered without a PID cannot be disproven, so it counts.
			done, _ := readExternalDoneFile(m.opts.StateDir, rec.ExternalDoneFile)
			if !done && (rec.Item.PID == nil || externalPIDAlive(*rec.Item.PID)) {
				m.spawnedWorkMu.Unlock()
				return true
			}
		case spawnedWorkLivenessSubagent, spawnedWorkLivenessInProcess:
			// Both are owned by an in-memory authority that settles them
			// directly — the subagent registry, or engine exit via
			// orphanInProcessSpawnedWorkForChat. "Running" is that authority
			// speaking, not a stale file.
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
		if _, dup := seen[target.path]; dup {
			continue
		}
		seen[target.path] = struct{}{}
		paths = append(paths, target.path)
	}
	// One synchronous collection scoped to this chat. The ticker cannot help
	// here: it runs on its own schedule and no pass is guaranteed to have run
	// when a turn ends. lsof is already bounded to 1.5s (probe_unix.go:47).
	pidsByPath, supported := m.opts.SpawnedWorkPIDProbe(paths)
	if !supported {
		// Two cases arrive here and both mean the same thing. On Windows the
		// probe does not exist at all (probe_other.go:8), so demanding a
		// confirmed PID would stall every parked lane on the production
		// laptop at 90 minutes while it genuinely runs. A probe that merely
		// failed this tick is equally uninformative. Neither is evidence of
		// absence, so the level authority stands and the row still counts.
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
	// A supported probe that ran and found nothing IS evidence of absence.
	// This is the branch that keeps a stale row from vetoing a real done.
	return false
}

// chatHasPendingPermission reports whether this chat is sitting on a permission
// card. It is a cheap invariant rather than a common case: request_permission
// blocks the prompt, so a turn does not normally END with one outstanding.
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

// settleObligationForJob is the clamp. Order is the specification: something
// blocked on a human outranks everything; otherwise the harness's own report of
// what it still has in flight beats the provider's stop reason, which beats
// inference; and whatever comes out of that is demoted by live evidence at the
// end.
//
// Nothing here asks the model anything. A declared tier existed until the user
// ruled that instructing an agent about its own turn is manipulation it should
// not pay for — and by then the harness answered the same question as fact.
func (m *Manager) settleObligationForJob(job *Job) {
	if job == nil || job.internal || job.SubagentID != "" || job.TabID == "" || job.ChatID == "" {
		return
	}
	signal := completionAdapterFor(job.ProviderID).FromNative(TurnOutcome{
		StopReason:  job.StopReason,
		ExitCode:    job.Code,
		Interrupted: job.Interrupted,
		ErrorText:   job.Error,
	})
	if signal.Disposition == DispositionDeferred {
		// The daemon is tearing down. Leave the record exactly as it is; the
		// boot sweep is what settles it, and a verdict written now may never
		// reach disk anyway.
		return
	}
	if signal.Disposition == DispositionCancelled {
		// A cancelled turn discards its declaration: the agent was interrupted
		// before it could know whether what it claimed was true.
		m.settleObligation(job.TabID, job.ChatID, signal, false)
		return
	}
	if m.chatHasPendingPermission(job.TabID, job.ChatID) {
		signal = CompletionSignal{Disposition: DispositionNeedsInput, Source: dispositionSourceInferred}
	}

	// Harness evidence, applied as capability rules rather than a precedence
	// ladder (gate amendment D4). A blanket "harness outranks native" would mark
	// a refusal — which the native adapter correctly reads as needs_input — as
	// done, the exact lie this whole record exists to prevent. So the harness may
	// only ever: demote a done to parked, and answer a question nothing else
	// could. It never overrules needs_input, and cancellation was already handled
	// above, outside the ladder entirely.
	harness := m.harnessTurns.get(job.SessionID)
	if harness.Complete() {
		switch {
		case harness.Parked && (signal.Disposition == DispositionDone || signal.Disposition == DispositionUnknown):
			signal = CompletionSignal{
				Disposition: DispositionParked,
				Source:      dispositionSourceHarness,
				Note:        harness.ParkNote,
			}
		case harness.Quiet() && signal.Disposition == DispositionUnknown && harness.TerminalReason == "completed":
			// The one promotion the harness is allowed, and only from Unknown:
			// nothing is in flight, nothing is scheduled, and the turn ended
			// normally. Before v3 this fell through to inference.
			signal = CompletionSignal{Disposition: DispositionDone, Source: dispositionSourceHarness}
		}
	}

	// Only a done can be demoted, so only a done pays for the probe.
	evidence := false
	if signal.Disposition == DispositionDone || signal.Disposition == DispositionUnknown {
		evidence = m.chatHasLiveParkEvidence(job.TabID, job.ChatID)
	}
	m.settleObligation(job.TabID, job.ChatID, signal, evidence)

	if settled := m.ObligationFor(job.TabID, job.ChatID); settled != nil {
		job.DispositionState, _ = settled["state"].(string)
		job.DispositionSource, _ = settled["source"].(string)
	}
}

// sweepStalledObligations turns a park nobody is behind into something the user
// can act on. It runs at the end of reconciliation, after service
// classification, so a row promoted to "service" on this same tick is already
// excluded from the evidence it would otherwise have supplied.
func (m *Manager) sweepStalledObligations(now time.Time) {
	type candidate struct{ tabID, chatID, parkedSince string }
	candidates := []candidate{}
	m.obligationMu.Lock()
	for _, rec := range m.obligations {
		if rec != nil && rec.State == obligationParked {
			candidates = append(candidates, candidate{rec.TabID, rec.ChatID, rec.ParkedSince})
		}
	}
	m.obligationMu.Unlock()

	stamp := now.UTC().Format(time.RFC3339Nano)
	for _, item := range candidates {
		// The grace existed because a self-scheduled wake was invisible to the
		// daemon. It is not invisible to the harness, which reports both its
		// in-flight work and its scheduled wakes at every turn end. When that
		// report says the session is quiet, waiting 90 minutes to say so adds
		// nothing — it only delays the truth, which is what cost the user an
		// hour. Any scheduled wake at all, recurring or not, keeps the grace as
		// the outer bound: a schedule can still die with the engine.
		//
		// There is deliberately no switch to turn this off. Stalling early is
		// self-healing: the wake that was supposedly missed reopens the
		// obligation on its next turn, so the cost is one wrong pill for one
		// tick. Stalling late costs an hour of somebody's day. A flag here
		// would only have kept the grace alive under another name.
		immediate := false
		note := "The work this chat parked on is no longer running."
		if ev := m.harnessTurns.getByChat(item.tabID, item.chatID); ev.Quiet() {
			immediate = true
			note = "Nothing is running and nothing is scheduled to wake this chat."
		}
		parkedAt, err := time.Parse(time.RFC3339Nano, item.parkedSince)
		if !immediate && (err != nil || now.Sub(parkedAt) < stalledGrace) {
			continue
		}
		// Unchanged and load-bearing: the harness cannot see the daemon's own
		// wake machinery, so a chat parked on a registered external lane reports
		// a quiet SDK session while genuinely waiting. This is the guard that
		// keeps the blessed spawn-tracked-lane workflow from false-alarming.
		if m.chatHasLiveParkEvidence(item.tabID, item.chatID) {
			continue
		}
		changed := false
		m.obligationMu.Lock()
		if rec := m.obligations[obligationKey(item.tabID, item.chatID)]; rec != nil && rec.State == obligationParked {
			changed = m.markObligationStalled(rec, stamp)
			if changed {
				rec.Note = note
			}
		}
		m.obligationMu.Unlock()
		if changed {
			m.persistObligations(item.tabID)
			// A stall happens with no turn running, so nothing else would tell
			// the client. This is the one transition the user is meant to act
			// on that nobody announced.
			m.commitSpawnedWorkChange(item.tabID, item.chatID)
		}
	}
}

// reconcileObligationsAtBoot settles what the previous process could not. A
// turn cannot survive the daemon, so a persisted "working" record on a fresh
// process is a phantom — the same shape as the running row that once vetoed a
// stuck chat's recovery, and it must never be allowed to sit there looking
// busy. A persisted park whose evidence did not reload died with the old
// process and is news the user has otherwise no way to discover.
func (m *Manager) reconcileObligationsAtBoot() {
	stamp := isoNow()
	type pending struct{ tabID, chatID, state string }
	items := []pending{}
	m.obligationMu.Lock()
	for _, rec := range m.obligations {
		if rec == nil {
			continue
		}
		if rec.State == obligationWorking || rec.State == obligationParked {
			items = append(items, pending{rec.TabID, rec.ChatID, rec.State})
		}
	}
	m.obligationMu.Unlock()

	touched := map[string]struct{}{}
	for _, item := range items {
		if item.state == obligationParked && m.chatHasLiveParkEvidence(item.tabID, item.chatID) {
			continue
		}
		note := "The work this chat parked on did not survive the daemon restart."
		if item.state == obligationWorking {
			note = "This turn was still running when the daemon restarted, so it never finished."
		}
		m.obligationMu.Lock()
		if rec := m.obligations[obligationKey(item.tabID, item.chatID)]; rec != nil {
			if m.markObligationStalled(rec, stamp) {
				rec.Note = note
				touched[item.tabID] = struct{}{}
			}
		}
		m.obligationMu.Unlock()
	}
	for tabID := range touched {
		m.persistObligations(tabID)
	}
}
