package acp

import (
	"context"
	"errors"
	"sort"
	"time"
)

// "Actualizar agentes solos" (Ajustes · Actualizaciones, owner 2026-11-09):
// the agents' own switch, separate from the Workass one the Mac shell acts on.
// With it on, an agent CLI whose update the hourly check found is installed by
// the daemon itself, through the same path as the "Actualizar" button, once no
// turn of that agent is running. Until then it waits and is looked at again
// every ProviderAutoUpdatePoll. A version that failed to install is not retried
// on its own; a newer version, or the button, tries again.

// SetAutoUpdateProviders stores the owner's choice. Turning it on looks at
// once, so an update already on offer does not wait for the next check.
func (m *Manager) SetAutoUpdateProviders(enabled bool) {
	if m == nil {
		return
	}
	if m.autoUpdateProviders.Swap(enabled) != enabled && enabled {
		m.kickProviderAutoUpdate()
	}
}

// AutoUpdateProviders reports the stored choice.
func (m *Manager) AutoUpdateProviders() bool {
	return m != nil && m.autoUpdateProviders.Load()
}

func (m *Manager) kickProviderAutoUpdate() {
	if m == nil || m.autoUpdateKick == nil || !m.autoUpdateProviders.Load() {
		return
	}
	select {
	case m.autoUpdateKick <- struct{}{}:
	default:
	}
}

func (m *Manager) providerAutoUpdateLoop() {
	ticker := time.NewTicker(m.opts.ProviderAutoUpdatePoll)
	defer ticker.Stop()
	for {
		select {
		case <-m.loopStop:
			return
		case <-ticker.C:
		case <-m.autoUpdateKick:
		}
		if m.autoUpdateProviders.Load() {
			m.autoInstallProviderUpdates()
		}
	}
}

// autoInstallProviderUpdates starts every pending agent update whose agent is
// quiet. It reads only what the last check remembered, so a wait costs no
// network; the install itself confirms the offer before it runs.
func (m *Manager) autoInstallProviderUpdates() {
	for _, update := range m.pendingProviderAutoUpdates() {
		if m.providerHasRunningTurn(update.ProviderID) {
			continue
		}
		_, err := m.StartProviderUpdate(context.Background(), update.ProviderID)
		if m.opts.Logf == nil {
			continue
		}
		fields := map[string]any{"provider": update.ProviderID, "target": update.Latest}
		if err != nil {
			fields["error"] = redactSensitiveText(err.Error())
			var coded chatStructuredError
			if errors.As(err, &coded) {
				fields["code"] = coded.Code
			}
			m.opts.Logf("provider auto-update not started", fields)
			continue
		}
		m.opts.Logf("provider auto-update started", fields)
	}
}

// pendingProviderAutoUpdates is every remembered offer not installing now and
// not already failed at that same version.
func (m *Manager) pendingProviderAutoUpdates() []ProviderUpdate {
	m.updateMu.Lock()
	defer m.updateMu.Unlock()
	out := make([]ProviderUpdate, 0, len(m.providerUpdateLastKnown))
	for id, update := range m.providerUpdateLastKnown {
		if !update.UpdateAvailable || update.Latest == "" {
			continue
		}
		if run := m.providerUpdateRuns[id]; run != nil && run.active() {
			continue
		}
		if failure, failed := m.providerUpdateFailures[id]; failed && (failure.Target == "" || failure.Target == update.Latest) {
			continue
		}
		out = append(out, update)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProviderID < out[j].ProviderID })
	return out
}

// providerHasRunningTurn reports live work of this agent: a running turn (a
// chat turn, a turn the harness started itself, or a subagent; each is a
// running Job), or an engine still holding work past its turn (a background
// command, spawned work, a scheduled wake). Installing under live work could
// pull files from under a process mid-task, so the install waits for both.
func (m *Manager) providerHasRunningTurn(providerID string) bool {
	providerID = normalizeProviderID(providerID)
	m.mu.Lock()
	for _, job := range m.jobs {
		if job != nil && job.Status == "running" && normalizeProviderID(job.ProviderID) == providerID {
			m.mu.Unlock()
			return true
		}
	}
	m.mu.Unlock()
	for _, bridge := range m.allBridges() {
		bridge.mu.Lock()
		mine := normalizeProviderID(bridge.providerID) == providerID
		live := !bridge.closed && bridge.state != StateHibernated && bridge.child != nil
		tabID, chatID := bridge.tabID, bridge.chatID
		bridge.mu.Unlock()
		if mine && live && m.engineHoldsLiveWork(bridge, tabID, chatID) {
			return true
		}
	}
	return false
}
