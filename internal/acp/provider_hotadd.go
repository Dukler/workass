package acp

import (
	"context"
	"os"
	"time"
)

// Adding an ACP agent without restarting Workass (owner 2026-11-19: "can
// adding acps be done without rebooting workass? code this in"). providers.json
// used to be read once at startup; now an entry whose id the daemon does not
// know yet is adopted while it runs: the watcher notices the file changed,
// the new agent joins the registry (existing agents are never touched from
// disk: the daemon owns their state), the list is published and the new agent
// is probed, so it shows in the model picker within seconds. Every save first
// adopts what someone else just added, so a daemon write cannot drop it.

// ProviderFileWatchInterval is how often providers.json is checked.
const ProviderFileWatchInterval = 3 * time.Second

// adoptNewProviderEntriesLocked reads providers.json and adds the agents it
// names that the registry does not have. Callers hold providerConfigMu.
func (m *Manager) adoptNewProviderEntriesLocked() []string {
	m.mu.Lock()
	filePath, rootDir := m.providerConfigFile, m.opts.RootDir
	m.mu.Unlock()
	if filePath == "" {
		return nil
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil
	}
	configs, err := decodeProviderConfigs(data)
	if err != nil {
		return nil
	}
	var added []string
	m.mu.Lock()
	for _, config := range NormalizeProviderCacheConfigs(configs, rootDir) {
		config = normalizeProviderConfig(config, rootDir, config.ID)
		if config.ID == "" || m.providers[config.ID] != nil {
			continue
		}
		if !config.DisabledByUser {
			config.Enabled = true
		}
		runtime := &providerRuntime{Config: config, Status: providerStatusInactive}
		if m.providerRegistry != nil {
			if err := m.providerRegistry.Register(m.providerDefinitionFor(config.ID, runtime)); err != nil {
				continue
			}
		}
		m.providers[config.ID] = runtime
		m.providerOrder = append(m.providerOrder, config.ID)
		added = append(added, config.ID)
	}
	m.mu.Unlock()
	return added
}

// AdoptNewProviders adopts agents added to providers.json, saves the file,
// publishes the list and probes them. It returns the ids it added.
func (m *Manager) AdoptNewProviders(ctx context.Context) []string {
	if m == nil {
		return nil
	}
	m.providerConfigMu.Lock()
	added := m.adoptNewProviderEntriesLocked()
	m.providerConfigMu.Unlock()
	if len(added) == 0 {
		return nil
	}
	_ = m.persistProviderConfigs()
	m.EmitCatalog(ctx)
	m.cacheProviderSnapshotEvent("providers:list", m.ProvidersList())
	go m.runProviderDetectionPass(ctx, added, true, providerDetectionExplicit)
	return added
}

// WatchProviderFile adopts new agents as providers.json changes, until ctx ends.
func (m *Manager) WatchProviderFile(ctx context.Context, every time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	filePath := m.providerConfigFile
	m.mu.Unlock()
	if filePath == "" {
		return
	}
	stamp := func() time.Time {
		info, err := os.Stat(filePath)
		if err != nil {
			return time.Time{}
		}
		return info.ModTime()
	}
	last := stamp()
	ticker := time.NewTicker(every)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if now := stamp(); !now.Equal(last) {
				last = now
				m.AdoptNewProviders(ctx)
			}
		}
	}()
}
