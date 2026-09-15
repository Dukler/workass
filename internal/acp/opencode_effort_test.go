package acp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestACPCatalogDiscoversEffortBeforeFirstPrompt(t *testing.T) {
	for _, providerID := range []string{"opencode", "custom-acp"} {
		t.Run(providerID, func(t *testing.T) {
			root, fixtureDir := repoRoot(t), t.TempDir()
			store := filepath.Join(fixtureDir, "sessions.json")
			trace := filepath.Join(fixtureDir, "prompts.jsonl")
			events := newEventCollector()
			manager := NewManager(Options{
				RootDir: root, StateDir: fixtureDir, InitTimeout: 2 * time.Second, RSSSampleInterval: time.Hour,
				Broadcast: events.Broadcast,
				Providers: []ProviderConfig{{
					ID: providerID, Enabled: true, Command: "node", Args: []string{filepath.Join(root, "desktop", "acp", "mock-server.mjs")},
					Env: map[string]string{
						"WORKASS_MOCK_ACP_MODEL_EFFORT_AXIS": "1",
						"WORKASS_MOCK_ACP_MODEL_OPTION_ID":   "custom_model",
						"WORKASS_MOCK_ACP_CONFIG_NOTIFY":     "1",
						"WORKASS_MOCK_ACP_EFFORT_OPTION_ID":  "custom_reasoning",
						"WORKASS_MOCK_ACP_SESSION_STORE":     store,
						"WORKASS_MOCK_ACP_TRACE_FILE":        trace,
					},
				}},
			})
			t.Cleanup(func() { manager.Reset() })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			groups := manager.Catalog(ctx)["groups"].([]CatalogGroup)
			group := findCatalogGroup(groups, providerID)
			if group == nil || group.Status != providerStatusReady {
				t.Fatalf("catalog unavailable: %#v", group)
			}
			reasoning := findCatalogModel(group.Models, "mock-reasoning")
			if reasoning == nil || !stringSlicesEqual(reasoning.Efforts, []string{"minimal", "low", "medium", "high", "xhigh"}) {
				t.Fatalf("first catalog lacks selectable efforts: %#v", reasoning)
			}
			if plain := findCatalogModel(group.Models, "mock-deterministic"); plain == nil || len(plain.Efforts) != 0 {
				t.Fatalf("discovery invented effort for a plain model: %#v", plain)
			}
			if literal := findCatalogModel(group.Models, "mock-literal"); literal == nil || !stringSlicesEqual(literal.Efforts, []string{"low", "HIGH"}) {
				t.Fatalf("discovery changed literal model variants: %#v", literal)
			}
			if data, err := os.ReadFile(trace); !os.IsNotExist(err) && (err != nil || len(data) != 0) {
				t.Fatalf("catalog discovery sent a prompt or resumed a chat: bytes=%d err=%v", len(data), err)
			}
			data, err := os.ReadFile(store)
			if err != nil {
				t.Fatal(err)
			}
			var stored struct {
				Sessions []struct {
					Model string `json:"model"`
					Turn  int    `json:"turn"`
				} `json:"sessions"`
			}
			if err := json.Unmarshal(data, &stored); err != nil {
				t.Fatal(err)
			}
			if len(stored.Sessions) != 1 || stored.Sessions[0].Model != "mock-deterministic" || stored.Sessions[0].Turn != 0 {
				t.Fatalf("discovery failed to restore its disposable session: %#v", stored)
			}
			for _, event := range events.snapshot() {
				if event.channel == "agent:apply" || event.channel == "chat:catalog" {
					t.Fatalf("catalog probe leaked a live control update: %s", event.channel)
				}
			}
			// A fresh chat can apply an effort selected from that first catalog,
			// including providers with their own model and effort option IDs.
			session, err := manager.NewSession(ctx, SessionOptions{ProviderID: providerID, CWD: fixtureDir})
			if err != nil {
				t.Fatal(err)
			}
			selection := "mock-reasoning[high]"
			result, err := manager.SetModel(ctx, session.SessionID, selection)
			if err != nil || result["appliedModelId"] != selection {
				t.Fatalf("first-turn effort could not be configured: result=%#v err=%v", result, err)
			}
		})
	}
}

// Model-specific controls may be absent until the model is selected. Exercise
// that protocol through both OpenCode and an unregistered ACP adapter: wire
// selection must not depend on a built-in provider policy.
func TestACPEffortSelectionBeforeAxisDiscovery(t *testing.T) {
	for _, providerID := range []string{"opencode", "custom-acp"} {
		for _, effortConfigID := range []string{"effort", "custom_reasoning"} {
			t.Run(providerID+"/"+effortConfigID, func(t *testing.T) {
				testEffortSelectionBeforeAxisDiscovery(t, providerID, effortConfigID)
			})
		}
	}
}

func testEffortSelectionBeforeAxisDiscovery(t *testing.T, providerID, effortConfigID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root := repoRoot(t)
	fixtureDir := t.TempDir()
	opts := Options{
		RootDir: root, StateDir: fixtureDir, RSSSampleInterval: time.Hour,
		Provider: ProviderConfig{ID: providerID, Command: "node", Args: []string{filepath.Join(root, "desktop", "acp", "mock-server.mjs")},
			Env: map[string]string{
				"WORKASS_MOCK_ACP_MODEL_EFFORT_AXIS": "1",
				"WORKASS_MOCK_ACP_EFFORT_OPTION_ID":  effortConfigID,
				"WORKASS_MOCK_ACP_SESSION_STORE":     filepath.Join(fixtureDir, "sessions.json"),
			}},
	}
	manager := NewManager(opts)
	t.Cleanup(func() { manager.Reset() })
	sessionOpts := SessionOptions{ProviderID: providerID, CWD: fixtureDir, Ephemeral: true}
	var savedID string
	for _, effort := range []string{"high", "xhigh"} {
		t.Run(effort, func(t *testing.T) {
			bridge := newBridge(providerID+"-effort-"+effort, opts, manager)
			defer bridge.Close(true, nil)
			var info SessionInfo
			var err error
			if savedID == "" {
				info, err = bridge.NewSession(ctx, sessionOpts)
			} else {
				info, _, err = bridge.RestoreSession(ctx, nativeSessionBinding{SessionID: savedID, ProviderSessionID: savedID, CWD: fixtureDir}, sessionOpts)
			}
			if err != nil {
				t.Fatal(err)
			}
			if savedID != "" && info.SessionID != savedID {
				t.Fatal("resume replaced the provider thread")
			}
			savedID = info.SessionID
			bridge.mu.Lock()
			_, axisKnown := bridge.axisEffortsByModel["mock-reasoning"]
			bridge.mu.Unlock()
			if axisKnown {
				t.Fatal("fixture already knew the target model's effort axis")
			}
			selection := "mock-reasoning[" + effort + "]"
			result, err := bridge.SetModel(ctx, info.SessionID, selection)
			if err != nil {
				t.Fatalf("set model before effort discovery: %v", err)
			}
			if result["currentModelId"] != selection || result["appliedModelId"] != selection {
				t.Fatalf("selected effort was lost: %#v", result)
			}
			if result, err := bridge.Prompt(ctx, info.SessionID, "[mock:quick] verify admission"); err != nil || result.StopReason != "end_turn" {
				t.Fatalf("configured mock turn did not complete: stop=%s err=%v", result.StopReason, err)
			}
			// A provider can expose literal model variants alongside a separate
			// effort axis. Discovering the latter must not rewrite literal IDs.
			for _, literal := range []string{"mock-literal[low]", "mock-literal[HIGH]"} {
				result, err := bridge.SetModel(ctx, info.SessionID, literal)
				if err != nil || result["appliedModelId"] != literal {
					t.Fatalf("literal model %q was rewritten: result=%#v err=%v", literal, result, err)
				}
			}
			// A stale effort on a model that has no effort axis is resolved
			// from that model's own controls, not another model's capabilities.
			result, err = bridge.SetModel(ctx, info.SessionID, "mock-deterministic[high]")
			if err != nil || result["currentModelId"] != "mock-deterministic" || result["modelWritebackReason"] != "unsupported-effort" {
				t.Fatalf("unsupported effort was not reconciled: result=%#v err=%v", result, err)
			}
			result, err = bridge.SetModel(ctx, info.SessionID, selection)
			if err != nil || result["appliedModelId"] != selection {
				t.Fatalf("switch back lost the selected effort: result=%#v err=%v", result, err)
			}
			// Resume a non-reasoning model in a fresh bridge, so the second
			// selection must discover the target's effort axis again.
			if _, err := bridge.SetModel(ctx, info.SessionID, "mock-deterministic"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
