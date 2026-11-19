package acp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A new ACP agent written to providers.json joins without restarting Workass
// (owner 2026-11-19), and a daemon save in between never drops it.
func TestAnACPAgentAddedToTheFileJoinsWithoutARestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "providers.json")
	manager := NewManager(Options{RootDir: t.TempDir(), ProviderConfigFile: file, Providers: []ProviderConfig{
		{ID: "alpha", Name: "Alpha", Command: "alpha-missing-binary", Args: []string{"acp"}, Enabled: true},
	}})
	t.Cleanup(func() { manager.Reset() })
	if err := manager.persistProviderConfigs(); err != nil {
		t.Fatal(err)
	}
	var on []map[string]any
	data, _ := os.ReadFile(file)
	if err := json.Unmarshal(data, &on); err != nil {
		t.Fatal(err)
	}
	on[0]["name"] = "Alpha renamed on disk"
	on = append(on, map[string]any{"id": "hermes", "name": "Hermes Agent", "command": "hermes-missing-binary", "args": []string{"acp"}, "badge": "agent"})
	data, _ = json.Marshal(on)
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}

	// A daemon save that lands before the watcher keeps the new entry.
	if err := manager.persistProviderConfigs(); err != nil {
		t.Fatal(err)
	}
	cfg, err := manager.providerConfig("hermes")
	if err != nil || cfg.Command != "hermes-missing-binary" || !cfg.Enabled {
		t.Fatalf("hermes = %+v, %v", cfg, err)
	}
	if alpha, _ := manager.providerConfig("alpha"); alpha.Name != "Alpha" {
		t.Fatalf("an existing agent was rewritten from disk: %+v", alpha)
	}
	saved, _ := LoadProviderConfigs(file, "")
	ids := map[string]bool{}
	for _, p := range saved {
		ids[p.ID] = true
	}
	if !ids["alpha"] || !ids["hermes"] {
		t.Fatalf("saved = %v", ids)
	}
	// Adopting again adds nothing.
	if again := manager.AdoptNewProviders(context.Background()); len(again) != 0 {
		t.Fatalf("adopted twice: %v", again)
	}
}

func TestTheProviderFileWatcherAdoptsANewAgent(t *testing.T) {
	file := filepath.Join(t.TempDir(), "providers.json")
	manager := NewManager(Options{RootDir: t.TempDir(), ProviderConfigFile: file, Providers: []ProviderConfig{
		{ID: "alpha", Name: "Alpha", Command: "alpha-missing-binary", Enabled: true},
	}})
	t.Cleanup(func() { manager.Reset() })
	if err := manager.persistProviderConfigs(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.WatchProviderFile(ctx, 20*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	data, _ := os.ReadFile(file)
	var on []map[string]any
	_ = json.Unmarshal(data, &on)
	on = append(on, map[string]any{"id": "hermes", "name": "Hermes Agent", "command": "hermes-missing-binary", "args": []string{"acp"}})
	data, _ = json.Marshal(on)
	// A later mtime than the first save, whatever the file system's clock grain.
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(file, future, future)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := manager.providerConfig("hermes"); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the watcher never adopted the new agent")
}
