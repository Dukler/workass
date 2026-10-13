package acp

import "sort"

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

// UpdateWorkChat is one chat's share of the work an update would stop.
type UpdateWorkChat struct {
	TabID           string `json:"tabId"`
	ForegroundTurns int    `json:"foregroundTurns,omitempty"`
	BackgroundWork  int    `json:"backgroundWork,omitempty"`
}

// UpdateWork is the read-only answer to app-update:work: what a user-initiated
// update would stop right now, so the update button can warn BEFORE the click
// rather than only recording it in the receipt. Unlike BeginUpdateDrain it
// never closes admission. External lanes are left out because they are
// detached processes that outlive the daemon and are re-settled after it;
// services are left out because they are restarted, not lost.
type UpdateWork struct {
	ForegroundTurns int              `json:"foregroundTurns"`
	BackgroundWork  int              `json:"backgroundWork"`
	Chats           []UpdateWorkChat `json:"chats"`
}

func (m *Manager) UpdateWorkInProgress() UpdateWork {
	byTab := map[string]*UpdateWorkChat{}
	order := []string{}
	chat := func(tabID string) *UpdateWorkChat {
		if entry, ok := byTab[tabID]; ok {
			return entry
		}
		entry := &UpdateWorkChat{TabID: tabID}
		byTab[tabID] = entry
		order = append(order, tabID)
		return entry
	}
	result := UpdateWork{Chats: []UpdateWorkChat{}}

	m.mu.Lock()
	for _, job := range m.jobs {
		// A tracked subagent's Job is counted through its spawned-work row below.
		if job != nil && job.Status == "running" && job.Kind != "subagent" {
			result.ForegroundTurns++
			chat(job.TabID).ForegroundTurns++
		}
	}
	m.mu.Unlock()

	m.spawnedWorkMu.Lock()
	for _, record := range m.spawnedWork {
		if record == nil || record.Item.Status != "running" || isServiceSpawnedWork(record.Item) || record.Item.Kind == "external" {
			continue
		}
		result.BackgroundWork++
		chat(record.Item.TabID).BackgroundWork++
	}
	m.spawnedWorkMu.Unlock()

	sort.Strings(order)
	for _, tabID := range order {
		result.Chats = append(result.Chats, *byTab[tabID])
	}
	return result
}
