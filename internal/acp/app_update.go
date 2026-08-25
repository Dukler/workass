package acp

// AppUpdateReadiness is a bounded, content-free snapshot used only by the
// loopback shell. It intentionally carries counts rather than chat, prompt, or
// task data: update safety needs to know that work exists, not what it says.
type AppUpdateReadiness struct {
	Ready           bool `json:"ready"`
	Admissions      int  `json:"admissions"`
	ForegroundTurns int  `json:"foregroundTurns"`
	BackgroundWork  int  `json:"backgroundWork"`
	ProviderUpdates int  `json:"providerUpdates"`
}

// BeginUpdateDrain atomically closes new-work admission and proves the daemon
// is quiescent. A successful drain remains latched until process exit. A busy
// result re-opens admission immediately, so merely checking for an update can
// never wedge normal chat activity.
func (m *Manager) BeginUpdateDrain() AppUpdateReadiness {
	m.updateGateMu.Lock()
	m.updateDraining = true
	result := AppUpdateReadiness{Admissions: m.updateAdmissions}
	if result.Admissions > 0 {
		m.updateDraining = false
		m.updateGateMu.Unlock()
		return result
	}

	m.mu.Lock()
	for _, job := range m.jobs {
		// A tracked subagent owns an internal Job for its provider session, but
		// that same unit of work is reported by the subagent registry below.
		// Counting it here labels background work as a foreground chat turn and
		// then counts it a second time as background work.
		if job != nil && job.Status == "running" && job.Kind != "subagent" {
			result.ForegroundTurns++
		}
	}
	for _, run := range m.subagents {
		if run != nil && run.Status == "running" {
			result.BackgroundWork++
		}
	}
	m.mu.Unlock()

	m.spawnedWorkMu.Lock()
	for _, record := range m.spawnedWork {
		if record == nil || record.Item.Status != "running" || isServiceSpawnedWork(record.Item) {
			continue
		}
		// Tracked subagents are already counted from the authoritative subagent
		// registry above. Other spawned/background work has its own liveness
		// oracle and must independently block activation.
		if record.Item.Kind != trackedSubagentSpawnedWorkKind {
			result.BackgroundWork++
		}
	}
	m.spawnedWorkMu.Unlock()

	m.updateMu.Lock()
	for _, run := range m.providerUpdateRuns {
		if run != nil && run.active() {
			result.ProviderUpdates++
		}
	}
	m.updateMu.Unlock()

	result.Ready = result.ForegroundTurns == 0 && result.BackgroundWork == 0 && result.ProviderUpdates == 0
	if !result.Ready {
		m.updateDraining = false
	}
	m.updateGateMu.Unlock()
	return result
}

// CancelUpdateDrain re-opens admission only when the shell could not arm its
// external updater worker after a successful prepare. Once commit shuts the
// process down this method is no longer reachable.
func (m *Manager) CancelUpdateDrain() {
	m.updateGateMu.Lock()
	m.updateDraining = false
	m.updateGateMu.Unlock()
}
