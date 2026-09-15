package acp

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

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
