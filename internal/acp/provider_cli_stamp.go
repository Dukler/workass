package acp

import (
	"os"
	"path/filepath"
	"time"
)

// A provider CLI update only reaches a chat when its host process starts
// again: a running host keeps the CLI it was started from loaded (Claude's
// streaming query, Codex's app-server, ...). Workass updates the CLI (the
// "Actualizar" button, "Actualizar agentes solos"), the vendor updates itself
// (Claude Code), or the owner runs the vendor's updater in a terminal: in every
// case the executable on disk is replaced while the hosts run on.
//
// So each host remembers the executable it was started from, and the lifecycle
// sweep compares it with what is on disk now. A replaced CLI marks the host to
// recycle at idle, the same path the age and memory limits use: an idle host
// stops now, a running turn finishes first, live background work holds it, and
// the next prompt resumes the exact native thread on the new CLI.

// providerCLIStamp identifies one installed executable: where its launcher
// leads (a native install swaps a versions/ symlink) and the file itself (a
// package manager writes a new file, often with a normalized mtime).
type providerCLIStamp struct {
	real    string
	size    int64
	modTime time.Time
	info    os.FileInfo
}

func (s *providerCLIStamp) same(other *providerCLIStamp) bool {
	if s == nil || other == nil {
		return true // unknown is never a reason to recycle
	}
	return s.real == other.real && s.size == other.size && s.modTime.Equal(other.modTime) && os.SameFile(s.info, other.info)
}

func stampProviderCLI(path string) *providerCLIStamp {
	if path == "" {
		return nil
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil
	}
	info, err := os.Stat(real)
	if err != nil || info.IsDir() {
		return nil
	}
	return &providerCLIStamp{real: real, size: info.Size(), modTime: info.ModTime(), info: info}
}

// currentProviderCLIStamp stamps the executable this provider resolves to now,
// or nil when it has none (a mock, or a CLI mid-install).
func (m *Manager) currentProviderCLIStamp(providerID string) *providerCLIStamp {
	if m == nil {
		return nil
	}
	provider, ok := m.providerSnapshot(normalizeProviderID(providerID))
	if !ok {
		return nil
	}
	resolved, err := resolveProviderExecutable(provider)
	if err != nil {
		return nil
	}
	return stampProviderCLI(resolved)
}

// markBridgesOnReplacedCLI marks every live host whose CLI was replaced since
// it started. A provider seen replaced also gets its version and update card
// read again, so the Agentes rows follow an update made outside Workass.
func (m *Manager) markBridgesOnReplacedCLI() {
	current := map[string]*providerCLIStamp{}
	replaced := map[string]bool{}
	for _, bridge := range m.allBridges() {
		bridge.mu.Lock()
		providerID := bridge.providerID
		started := bridge.cliStamp
		live := !bridge.closed && bridge.state != StateHibernated && bridge.child != nil
		bridge.mu.Unlock()
		if !live || started == nil {
			continue
		}
		now, seen := current[providerID]
		if !seen {
			now = m.currentProviderCLIStamp(providerID)
			current[providerID] = now
		}
		if started.same(now) {
			continue
		}
		bridge.mu.Lock()
		changed := bridge.cliStamp == started && bridge.markRecycleAtIdleLocked(recycleReasonCLIUpdate)
		key := bridge.key
		bridge.mu.Unlock()
		if !changed {
			continue
		}
		replaced[providerID] = true
		if m.opts.Logf != nil {
			m.opts.Logf("acp engine cli replaced", map[string]any{"key": key, "provider": providerID, "from": started.real, "to": now.real})
		}
		m.bridgeChanged(bridge, "recycle-marked")
	}
	if len(replaced) > 0 {
		m.scheduleProviderUpdateCheck()
	}
}
