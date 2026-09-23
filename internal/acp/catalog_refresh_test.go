package acp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func catalogRefreshTestManager(t *testing.T, events *eventCollector) *Manager {
	t.Helper()
	root := repoRoot(t)
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "mock", Name: "Mock Provider", Command: "node",
			Args: []string{filepath.Join(root, "desktop", "acp", "mock-server.mjs")}, CWD: root,
			Enabled: true,
		}},
		DefaultProviderID:      "mock",
		InitTimeout:            time.Second,
		RSSSampleInterval:      time.Hour,
		LifecycleCheckInterval: time.Hour,
		Broadcast:              events.Broadcast,
	})
	t.Cleanup(func() { manager.Reset() })
	return manager
}

func TestProviderVersionChangeReprobesAndPublishesCatalog(t *testing.T) {
	events := newEventCollector()
	manager := catalogRefreshTestManager(t, events)
	manager.mu.Lock()
	runtime := manager.providers["mock"]
	runtime.Probed = true
	runtime.Status = providerStatusReady
	runtime.Models = []Model{{ModelID: "old-model", Name: "Old model"}}
	runtime.CLIVersion = &CLIVersion{Version: "1.0.0", Raw: "1.0.0"}
	runtime.CatalogCLIVersion = "1.0.0"
	manager.mu.Unlock()

	if !manager.setProviderCLIVersion("mock", &CLIVersion{Version: "1.1.0", Raw: "1.1.0"}) {
		t.Fatal("real CLI version change did not invalidate and refresh the catalog")
	}
	group := findCatalogGroup(manager.CatalogSnapshotGroups(), "mock")
	if group == nil || group.Status != providerStatusReady || len(group.Models) != 1 || group.Models[0].ModelID != "mock-deterministic" {
		t.Fatalf("refreshed catalog = %#v", group)
	}
	event := events.waitChannel(t, "chat:catalog", 5*time.Second)
	payload, _ := event.payload.(map[string]any)
	groups, _ := payload["groups"].([]CatalogGroup)
	group = findCatalogGroup(groups, "mock")
	if group == nil || len(group.Models) != 1 || group.Models[0].ModelID != "mock-deterministic" {
		t.Fatalf("published catalog = %#v", event.payload)
	}
}

func TestAuthoritativeAvailableModelsReplaceRenamedAddedAndRemovedModels(t *testing.T) {
	events := newEventCollector()
	manager := catalogRefreshTestManager(t, events)
	manager.mu.Lock()
	runtime := manager.providers["mock"]
	runtime.Probed = true
	runtime.Status = providerStatusReady
	runtime.Models = []Model{
		{ModelID: "kept", Name: "Old label", Efforts: []string{"low", "high"}},
		{ModelID: "removed", Name: "Removed"},
	}
	runtime.Modes = []Mode{{ID: "ask", Name: "Ask"}}
	manager.mu.Unlock()

	bridge := newBridge("live-mock", Options{Provider: ProviderConfig{ID: "mock", Name: "Mock Provider"}}, manager)
	bridge.models = cloneModels([]Model{
		{ModelID: "kept", Name: "Old label", Efforts: []string{"low", "high"}},
		{ModelID: "removed", Name: "Removed"},
	})
	bridge.agentName = "Fake ACP"
	bridge.applyAvailableModels([]any{
		map[string]any{"id": "kept", "name": "Renamed"},
		map[string]any{"id": "fresh[low]", "name": "Fresh (low)"},
		map[string]any{"id": "fresh[high]", "name": "Fresh (high)"},
	})

	group := findCatalogGroup(manager.CatalogSnapshotGroups(), "mock")
	if group == nil || len(group.Models) != 2 {
		t.Fatalf("authoritative catalog = %#v", group)
	}
	if group.Models[0].ModelID != "kept" || group.Models[0].Name != "Renamed" || len(group.Models[0].Efforts) != 2 {
		t.Fatalf("renamed model and known effort metadata = %#v", group.Models[0])
	}
	if group.Models[1].ModelID != "fresh" || group.Models[1].Name != "Fresh" || len(group.Models[1].Efforts) != 2 {
		t.Fatalf("normalized added model = %#v", group.Models[1])
	}
	if findCatalogModel(group.Models, "removed") != nil {
		t.Fatalf("removed model remained in authoritative catalog: %#v", group.Models)
	}
	event := events.waitChannel(t, "chat:catalog", 5*time.Second)
	payload, _ := event.payload.(map[string]any)
	groups, _ := payload["groups"].([]CatalogGroup)
	if published := findCatalogGroup(groups, "mock"); published == nil || len(published.Models) != 2 {
		t.Fatalf("published authoritative catalog = %#v", event.payload)
	}

	bridge.applyAvailableModels([]any{"replacement", map[string]any{"name": "malformed row"}})
	group = findCatalogGroup(manager.CatalogSnapshotGroups(), "mock")
	if group == nil || len(group.Models) != 2 || findCatalogModel(group.Models, "replacement") != nil {
		t.Fatalf("malformed partial list erased the last complete catalog: %#v", group)
	}
}

func TestProviderCatalogRefreshDoesNotProbeDisabledOrNeedsLoginProvider(t *testing.T) {
	root := repoRoot(t)
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "fake-agent", Name: "Fake Agent", Command: filepath.Join(root, "missing-agent"),
			Enabled: false, NeedsLogin: true,
		}},
		DefaultProviderID:      "fake-agent",
		RSSSampleInterval:      time.Hour,
		LifecycleCheckInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	if manager.refreshProviderCatalogNow(context.Background(), "fake-agent") {
		t.Fatal("needs-login provider unexpectedly launched a catalog probe")
	}
	if groups := manager.CatalogSnapshotGroups(); len(groups) != 0 {
		t.Fatalf("disabled provider appeared in catalog snapshot: %#v", groups)
	}
}

func TestCancelledCatalogRefreshWaiterDoesNotRecurse(t *testing.T) {
	manager := catalogRefreshTestManager(t, newEventCollector())
	done := make(chan struct{}) // the first caller owns a probe that has not completed
	manager.mu.Lock()
	runtime := manager.providers["mock"]
	runtime.CatalogRefreshing = true
	runtime.CatalogRefreshDone = done
	manager.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	finished := make(chan bool, 1)
	go func() { finished <- manager.refreshProviderCatalogNow(ctx, "mock") }()
	select {
	case result := <-finished:
		if result {
			t.Fatal("cancelled waiter claimed to refresh the catalog")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter did not exit while another probe remained in flight")
	}
}

func TestCatalogReadExpiryRefreshesRemoteModelsWithoutCLIVersionChange(t *testing.T) {
	manager := catalogRefreshTestManager(t, newEventCollector())
	manager.mu.Lock()
	runtime := manager.providers["mock"]
	runtime.Probed = true
	runtime.Status = providerStatusReady
	runtime.Models = []Model{{ModelID: "old-remote-model", Name: "Old remote model"}}
	runtime.CatalogCLIVersion = providerVersionIdentity(runtime.CLIVersion)
	runtime.CatalogRefreshedAt = time.Now().Add(-providerCatalogReadExpiry - time.Second)
	manager.mu.Unlock()

	group := findCatalogGroupFromPayload(manager.Catalog(context.Background()), "mock")
	if group == nil || len(group.Models) != 1 || group.Models[0].ModelID != "mock-deterministic" {
		t.Fatalf("expired same-version remote catalog was not refreshed: %#v", group)
	}
}

func TestOlderBridgeCannotReplaceLaterAuthoritativeProbe(t *testing.T) {
	manager := catalogRefreshTestManager(t, newEventCollector())
	manager.mu.Lock()
	runtime := manager.providers["mock"]
	runtime.Probed = true
	runtime.Status = providerStatusReady
	runtime.Models = []Model{{ModelID: "authoritative-new", Name: "New"}}
	runtime.CatalogRevision = 3
	manager.mu.Unlock()

	bridge := newBridge("old-live-mock", Options{Provider: ProviderConfig{ID: "mock", Name: "Mock Provider"}}, manager)
	bridge.models = []Model{{ModelID: "stale-old", Name: "Stale"}}
	bridge.applyAvailableModels([]any{map[string]any{"id": "stale-renamed", "name": "Stale renamed"}})
	group := findCatalogGroup(manager.CatalogSnapshotGroups(), "mock")
	if group == nil || len(group.Models) != 1 || group.Models[0].ModelID != "authoritative-new" {
		t.Fatalf("old bridge replaced newer authoritative catalog: %#v", group)
	}
}

func TestInitialCatalogAuthenticationFailureKeepsClaudeLoginPolicy(t *testing.T) {
	root := repoRoot(t)
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "claude", Name: "Claude Code", Command: os.Args[0],
			Args: []string{"-test.run=TestFakeACPHelper", "--"}, CWD: root,
			Env:     map[string]string{"WORKASS_FAKE_ACP": "1", "WORKASS_FAKE_ACP_MODE": "auth-on-session"},
			Enabled: true,
		}},
		DefaultProviderID:      "claude",
		InitTimeout:            time.Second,
		RSSSampleInterval:      time.Hour,
		LifecycleCheckInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	manager.Catalog(context.Background())
	item := assertProviderListItem(t, manager.ProvidersList(), "claude", providerStatusNeedsLogin, false)
	if item["fixHint"] != "Ejecuta `claude auth login`" {
		t.Fatalf("initial catalog auth failure lost Claude login hint: %#v", item)
	}
}

func TestCatalogRefreshAuthenticationFailurePreservesUsableLastGoodCatalog(t *testing.T) {
	root := repoRoot(t)
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "devin", Name: "Devin", Command: os.Args[0],
			Args: []string{"-test.run=TestFakeACPHelper", "--"}, CWD: root,
			Env:     map[string]string{"WORKASS_FAKE_ACP": "1", "WORKASS_FAKE_ACP_MODE": "auth-on-session"},
			Enabled: true,
		}},
		DefaultProviderID:      "devin",
		InitTimeout:            time.Second,
		RSSSampleInterval:      time.Hour,
		LifecycleCheckInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	manager.mu.Lock()
	runtime := manager.providers["devin"]
	runtime.Probed = true
	runtime.Status = providerStatusReady
	runtime.Models = []Model{{ModelID: "last-good", Name: "Last good"}}
	runtime.CatalogCLIVersion = providerVersionIdentity(runtime.CLIVersion)
	runtime.CatalogRefreshedAt = time.Now().Add(-providerCatalogReadExpiry - time.Second)
	manager.mu.Unlock()

	group := findCatalogGroupFromPayload(manager.Catalog(context.Background()), "devin")
	if group == nil || group.Status != providerStatusReady || len(group.Models) != 1 || group.Models[0].ModelID != "last-good" {
		t.Fatalf("failed refresh erased usable last-good catalog: %#v", group)
	}
	assertProviderListItem(t, manager.ProvidersList(), "devin", providerStatusReady, true)
}

func findCatalogGroupFromPayload(payload map[string]any, providerID string) *CatalogGroup {
	groups, _ := payload["groups"].([]CatalogGroup)
	return findCatalogGroup(groups, providerID)
}
