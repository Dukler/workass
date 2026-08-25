package acp

// AppUpdateReadiness is a bounded, content-free snapshot used only by the
// loopback shell. It intentionally carries counts rather than chat, prompt, or
// task data: the counts record what a user-initiated update interrupts so the
// durable receipt can show it; they never block activation (user law
// 2026-08-25 — updates are entered only by a user click).
type AppUpdateReadiness struct {
	Ready           bool `json:"ready"`
	Admissions      int  `json:"admissions"`
	ForegroundTurns int  `json:"foregroundTurns"`
	BackgroundWork  int  `json:"backgroundWork"`
	ProviderUpdates int  `json:"providerUpdates"`
}

// BeginUpdateDrain atomically closes new-work admission so the handoff window
// cannot admit work that the replacing process could not carry. It never
// refuses a user-initiated update because work is active: any active work is
// counted into the readiness payload and recorded by the updater receipt
// instead. A successful drain remains latched until process exit or
// CancelUpdateDrain.
func (m *Manager) BeginUpdateDrain() AppUpdateReadiness {
	m.updateGateMu.Lock()
	m.updateDraining = true
	result := AppUpdateReadiness{Ready: true, Admissions: m.updateAdmissions}

	m.mu.Lock()
	for _, job := range m.jobs {
		// A tracked subagent owns an internal Job for its provider session, but
		// that same unit of work is reported by the subagent registry below.
		// Counting it here would double-report it in the receipt.
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
		// registry above.
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
