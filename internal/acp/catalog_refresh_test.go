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

func TestSessionAttachPublishesChangedModelOptions(t *testing.T) {
	events := newEventCollector()
	manager := catalogRefreshTestManager(t, events)
	manager.mu.Lock()
	runtime := manager.providers["mock"]
	runtime.Probed = true
	runtime.Status = providerStatusReady
	runtime.Models = []Model{{ModelID: "removed-model", Name: "Removed model"}}
	manager.mu.Unlock()

	bridge := newBridge("live-mock", Options{Provider: ProviderConfig{ID: "mock", Name: "Mock Provider"}}, manager)
	bridge.agentName = "Mock ACP"
	bridge.applyConfigOptionsForSession("resumed-session", []any{map[string]any{
		"id": "model", "category": "model", "currentValue": "mock-deterministic",
		"options": []any{map[string]any{"value": "mock-deterministic", "name": "Mock deterministic"}},
	}}, false, false)

	event := events.waitChannel(t, "chat:catalog", 5*time.Second)
	payload, _ := event.payload.(map[string]any)
	groups, _ := payload["groups"].([]CatalogGroup)
	group := findCatalogGroup(groups, "mock")
	if group == nil || len(group.Models) != 1 || group.Models[0].ModelID != "mock-deterministic" {
		t.Fatalf("attached host did not publish its current models: %#v", group)
	}
}

func TestSessionAttachRefreshesCatalogRevisionForLiveBridge(t *testing.T) {
	events := newEventCollector()
	manager := catalogRefreshTestManager(t, events)
	manager.mu.Lock()
	providerConfig := manager.providers["mock"].Config
	manager.mu.Unlock()
	bridge := newBridge("live-mock", Options{Provider: providerConfig}, manager)
	bridge.catalogRevision = 1
	bridge.agentName = "Mock ACP"

	manager.mu.Lock()
	runtime := manager.providers["mock"]
	runtime.Probed = true
	runtime.Status = providerStatusReady
	runtime.Models = []Model{{ModelID: "removed-model", Name: "Removed model"}}
	runtime.CLIVersion = &CLIVersion{Version: "1.0.0", Raw: "1.0.0"}
	runtime.CatalogCLIVersion = "1.0.0"
	runtime.CatalogRevision = 2 // a same-version probe committed after bridge construction
	runtime.CatalogRefreshedAt = time.Now().Add(-time.Second)
	manager.mu.Unlock()

	// The bridge object predates provider detection, but its process starts after
	// the latest probe and uses the same launch configuration, so its session
	// options belong to the current host generation.
	bridge.startedAt = time.Now()
	_, err := bridge.attachSession("resumed-session", repoRoot(t), SessionOptions{TabID: "mock-tab", ChatID: "mock-chat"}, map[string]any{
		"configOptions": []any{map[string]any{
			"id": "model", "category": "model", "currentValue": "mock-deterministic",
			"options": []any{map[string]any{"value": "mock-deterministic", "name": "Mock deterministic"}},
		}},
	}, "session-resume")
	if err != nil {
		t.Fatalf("attach session: %v", err)
	}

	group := findCatalogGroup(manager.CatalogSnapshotGroups(), "mock")
	if group == nil || len(group.Models) != 1 || group.Models[0].ModelID != "mock-deterministic" {
		t.Fatalf("fresh attached-host catalog was fenced by a completed probe: %#v", group)
	}
}

func TestSessionAttachFromOlderSameVersionProcessRetainsCatalogRevisionFence(t *testing.T) {
	manager := catalogRefreshTestManager(t, newEventCollector())
	manager.mu.Lock()
	runtime := manager.providers["mock"]
	runtime.Probed = true
	runtime.Status = providerStatusReady
	runtime.Models = []Model{{ModelID: "renamed-model", Name: "Renamed model"}}
	runtime.CLIVersion = &CLIVersion{Version: "1.0.0", Raw: "1.0.0"}
	runtime.CatalogCLIVersion = "1.0.0"
	runtime.CatalogRevision = 2
	runtime.CatalogRefreshedAt = time.Now()
	providerConfig := runtime.Config
	manager.mu.Unlock()

	bridge := newBridge("same-version-old-live-mock", Options{Provider: providerConfig}, manager)
	bridge.catalogRevision = 1
	bridge.startedAt = time.Now().Add(-time.Minute)
	bridge.agentName = "Older Mock ACP"
	_, err := bridge.attachSession("old-same-version-session", repoRoot(t), SessionOptions{TabID: "mock-tab", ChatID: "mock-chat"}, map[string]any{
		"configOptions": []any{map[string]any{
			"id": "model", "category": "model", "currentValue": "old-model",
			"options": []any{map[string]any{"value": "old-model", "name": "Old model"}},
		}},
	}, "session-resume")
	if err != nil {
		t.Fatalf("attach older same-version session: %v", err)
	}

	group := findCatalogGroup(manager.CatalogSnapshotGroups(), "mock")
	if group == nil || len(group.Models) != 1 || group.Models[0].ModelID != "renamed-model" {
		t.Fatalf("older same-version host replaced the later provider probe: %#v", group)
	}
}

func TestSessionAttachCannotCrossUnprobedCLIVersionInvalidation(t *testing.T) {
	manager := catalogRefreshTestManager(t, newEventCollector())
	manager.mu.Lock()
	runtime := manager.providers["mock"]
	runtime.Probed = false
	runtime.Status = providerStatusReady
	runtime.Models = []Model{{ModelID: "last-probe-model", Name: "Last probe model"}}
	runtime.CLIVersion = &CLIVersion{Version: "2.0.0", Raw: "2.0.0"}
	runtime.CatalogCLIVersion = "1.0.0"
	runtime.CatalogRevision = 2
	runtime.CatalogRefreshedAt = time.Now().Add(-time.Minute)
	providerConfig := runtime.Config
	manager.mu.Unlock()

	bridge := newBridge("pre-version-invalidation-mock", Options{Provider: providerConfig}, manager)
	bridge.catalogRevision = 1
	bridge.startedAt = time.Now().Add(-time.Second) // after the prior probe, before invalidation
	bridge.agentName = "Pre-invalidation Mock ACP"
	_, err := bridge.attachSession("pre-invalidation-session", repoRoot(t), SessionOptions{TabID: "mock-tab", ChatID: "mock-chat"}, map[string]any{
		"configOptions": []any{map[string]any{
			"id": "model", "category": "model", "currentValue": "stale-model",
			"options": []any{map[string]any{"value": "stale-model", "name": "Stale model"}},
		}},
	}, "session-resume")
	if err != nil {
		t.Fatalf("attach pre-invalidation session: %v", err)
	}

	group := findCatalogGroup(manager.CatalogSnapshotGroups(), "mock")
	if group == nil || len(group.Models) != 1 || group.Models[0].ModelID != "last-probe-model" {
		t.Fatalf("bridge crossed an unprobed CLI-version invalidation: %#v", group)
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
