package acp

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Plan usage asks a provider's account API, and that API rate-limits per
// account: every reader of the same login (the provider's CLI, its usage
// reads, the account-reset reads) spends one shared budget. So:
//
//   - native plan-usage reads of one provider are spaced by
//     Options.PlanUsageMinInterval, whatever asks for them (a turn end, an
//     attach, the tick, a chat switch, the popover, Cuentas), and are held
//     through that provider's rate-limit backoff (account_resets.go);
//   - the last snapshot per provider, and that backoff, are kept in
//     plan-usage.json in the state dir, so after a restart the page paints at
//     once with the age of what it shows instead of opening empty. Only the
//     parsed usage is kept: no raw provider metadata, never a credential.

const (
	planUsageStoreName      = "plan-usage.json"
	planUsageStoreMaxBytes  = 1 << 20
	planUsageStoreVersion   = 1
	planUsageStoreRewriteAt = time.Minute
)

type planUsageGateState struct {
	mu       sync.Mutex
	lastRead map[string]time.Time
	// The last write, so a capture that only moves CapturedAt rewrites the
	// file at most once a minute.
	writtenKey string
	writtenAt  time.Time
}

type planUsageStoreFile struct {
	Version   int                            `json:"version"`
	Snapshots map[string]PlanUsageSnapshot   `json:"snapshots,omitempty"`
	Backoff   map[string]accountResetBackoff `json:"accountResetBackoff,omitempty"`
}

// planUsageReadWait is how long a native plan-usage read of providerID must
// still wait: the spacing since its last read, or the provider's rate-limit
// backoff, whichever ends later.
func (m *Manager) planUsageReadWait(providerID string, now time.Time) time.Duration {
	providerID = normalizeProviderID(providerID)
	var wait time.Duration
	if spacing := m.opts.PlanUsageMinInterval; spacing > 0 {
		m.planUsageGate.mu.Lock()
		last := m.planUsageGate.lastRead[providerID]
		m.planUsageGate.mu.Unlock()
		if !last.IsZero() {
			wait = last.Add(spacing).Sub(now)
		}
	}
	if until := m.accountResetRateLimitedUntil(providerID); until.Sub(now) > wait {
		wait = until.Sub(now)
	}
	if wait < 0 {
		return 0
	}
	return wait
}

// notePlanUsageRead records that the provider's account API was just asked.
func (m *Manager) notePlanUsageRead(providerID string, at time.Time) {
	providerID = normalizeProviderID(providerID)
	m.planUsageGate.mu.Lock()
	defer m.planUsageGate.mu.Unlock()
	if m.planUsageGate.lastRead == nil {
		m.planUsageGate.lastRead = make(map[string]time.Time)
	}
	if at.After(m.planUsageGate.lastRead[providerID]) {
		m.planUsageGate.lastRead[providerID] = at
	}
}

func (m *Manager) planUsageStorePath() string {
	if dir := strings.TrimSpace(m.opts.StateDir); dir != "" {
		return filepath.Join(dir, planUsageStoreName)
	}
	return ""
}

// loadPersistedPlanUsage restores the snapshots and backoffs a previous
// daemon kept. A snapshot dates its own reads, so it also seeds the spacing:
// a restart does not ask the account API sooner than it would have.
func (m *Manager) loadPersistedPlanUsage() {
	path := m.planUsageStorePath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > planUsageStoreMaxBytes {
		return
	}
	var stored planUsageStoreFile
	if json.Unmarshal(data, &stored) != nil || stored.Version != planUsageStoreVersion {
		return
	}
	readAt := map[string]time.Time{}
	m.mu.Lock()
	for id, snapshot := range stored.Snapshots {
		id = normalizeProviderID(id)
		if id == "" || normalizeProviderID(snapshot.ProviderID) != id {
			continue
		}
		snapshot.ProviderID = id
		snapshot.Raw = nil
		// A snapshot saved before a window was renamed holds it twice; the
		// later entry is the current name (mergePlanUsageEntries appends).
		snapshot.Entries = collapseRenamedPlanUsageWindows(snapshot.Entries)
		if _, live := m.planUsageByProvider[id]; live {
			continue
		}
		m.planUsageByProvider[id] = snapshot
		if at, err := time.Parse(time.RFC3339Nano, snapshot.CapturedAt); err == nil {
			readAt[id] = at
		}
	}
	m.mu.Unlock()
	for id, at := range readAt {
		m.notePlanUsageRead(id, at)
	}
	now := time.Now()
	m.accountResets.mu.Lock()
	for id, backoff := range stored.Backoff {
		if id = normalizeProviderID(id); id != "" && backoff.RetryAt.After(now) {
			if m.accountResets.backoff == nil {
				m.accountResets.backoff = make(map[string]accountResetBackoff)
			}
			m.accountResets.backoff[id] = backoff
		}
	}
	m.accountResets.mu.Unlock()
}

// persistPlanUsage writes the snapshots (without raw metadata) and the
// active backoffs. A write that would only move CapturedAt happens at most
// once a minute; any other change is written at once.
func (m *Manager) persistPlanUsage() {
	path := m.planUsageStorePath()
	if path == "" {
		return
	}
	m.planUsageGate.mu.Lock()
	defer m.planUsageGate.mu.Unlock()
	m.mu.Lock()
	snapshots := make(map[string]PlanUsageSnapshot, len(m.planUsageByProvider))
	undated := make(map[string]PlanUsageSnapshot, len(m.planUsageByProvider))
	for id, snapshot := range m.planUsageByProvider {
		snapshot.Raw = nil
		snapshots[id] = snapshot
		snapshot.CapturedAt = ""
		undated[id] = snapshot
	}
	m.mu.Unlock()
	backoff := m.accountResetBackoffs(time.Now())
	key, err := json.Marshal(planUsageStoreFile{Version: planUsageStoreVersion, Snapshots: undated, Backoff: backoff})
	if err != nil {
		return
	}
	if string(key) == m.planUsageGate.writtenKey && time.Since(m.planUsageGate.writtenAt) < planUsageStoreRewriteAt {
		return
	}
	data, err := json.MarshalIndent(planUsageStoreFile{Version: planUsageStoreVersion, Snapshots: snapshots, Backoff: backoff}, "", "  ")
	if err != nil || len(data) > planUsageStoreMaxBytes {
		return
	}
	if err := writePlanUsageStore(path, data); err != nil {
		m.opts.Logf("plan usage store write failed", map[string]any{"error": redactSensitiveText(err.Error())})
		return
	}
	m.planUsageGate.writtenKey = string(key)
	m.planUsageGate.writtenAt = time.Now()
}

// writePlanUsageStore replaces the file atomically. It never creates the
// state dir: a manager whose dir is gone has nothing to keep.
func writePlanUsageStore(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".plan-usage-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, writeErr := tmp.Write(append(bytes.TrimSpace(data), '\n'))
	closeErr := tmp.Close()
	if writeErr == nil {
		writeErr = closeErr
	}
	if writeErr == nil {
		writeErr = os.Chmod(name, 0o600)
	}
	if writeErr == nil {
		writeErr = os.Rename(name, path)
	}
	if writeErr != nil {
		_ = os.Remove(name)
	}
	return writeErr
}
