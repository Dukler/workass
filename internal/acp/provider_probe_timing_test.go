package acp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
)

const virtualProbeStageDelay = 2600 * time.Millisecond

func TestProviderDetectionAllowsFullInitializeAndSessionBudgets(t *testing.T) {
	root := repoRoot(t)
	pathDir := t.TempDir()
	installFakeAgentWrapper(t, pathDir, "devin", "echo-prompt")
	t.Setenv("PATH", pathDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ASSISTANT_DEVIN", filepath.Join(pathDir, "devin"))

	runVirtualProviderDetection(t, root, virtualProbeStageDelay, virtualProbeStageDelay, true)
}

func TestProviderDetectionStillEnforcesEachVirtualRequestBudget(t *testing.T) {
	root := repoRoot(t)
	pathDir := t.TempDir()
	installFakeAgentWrapper(t, pathDir, "devin", "echo-prompt")
	t.Setenv("PATH", pathDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ASSISTANT_DEVIN", filepath.Join(pathDir, "devin"))

	runVirtualProviderDetection(t, root, 5200*time.Millisecond, 0, false)
}

func runVirtualProviderDetection(t *testing.T, root string, initDelay, sessionDelay time.Duration, wantReady bool) {
	t.Helper()
	originalRegistration := providerRegistrations["devin"]
	registration := originalRegistration
	registration.ProbeTimeout = 5 * time.Second
	providerRegistrations["devin"] = registration
	t.Cleanup(func() { providerRegistrations["devin"] = originalRegistration })
	synctest.Test(t, func(t *testing.T) {
		manager := NewManager(Options{
			RootDir:           root,
			InitTimeout:       5 * time.Second,
			RSSSampleInterval: time.Hour,
			catalogProbeBridge: func(key string, opts Options, manager *Manager) *Bridge {
				bridge := newBridge(key, opts, manager)
				bridge.child = &exec.Cmd{}
				bridge.stdin = &virtualProbeWriter{bridge: bridge, initDelay: initDelay, sessionDelay: sessionDelay}
				return bridge
			},
		})
		defer manager.Reset()

		manager.DetectProviders(context.Background(), DetectOptions{ProviderID: "devin"})
		if wantReady {
			item := assertProviderListItem(t, manager.ProvidersList(), "devin", providerStatusReady, true)
			if got := item["latencyMs"]; got != int64(5200) {
				t.Fatalf("virtual catalog probe latency = %#v, want 5200ms across two independent request budgets", got)
			}
			manager.mu.Lock()
			agentName := manager.providers["devin"].AgentName
			manager.mu.Unlock()
			if agentName != "Fake devin" {
				t.Fatalf("detected agent name = %q", agentName)
			}
			groups, _ := manager.Catalog(context.Background())["groups"].([]CatalogGroup)
			group := findCatalogGroup(groups, "devin")
			if group == nil || group.Status != providerStatusReady || len(group.Models) != 1 || group.Models[0].ModelID != "virtual-model" {
				t.Fatalf("detected virtual catalog group = %#v", group)
			}
			return
		}
		item := assertProviderListItem(t, manager.ProvidersList(), "devin", providerStatusError, false)
		if got := item["latencyMs"]; got != int64(5000) {
			t.Fatalf("timed-out virtual initialize probe latency = %#v, want 5000ms", got)
		}
		// Let the deliberately late initialize reply arrive so the bubble has no
		// outstanding response goroutine when the manager is reset.
		time.Sleep(250 * time.Millisecond)
	})
}

type virtualProbeWriter struct {
	bridge       *Bridge
	initDelay    time.Duration
	sessionDelay time.Duration
}

func (w *virtualProbeWriter) Write(data []byte) (int, error) {
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(data, &request); err != nil {
		return 0, err
	}
	if len(request.ID) == 0 {
		return len(data), nil
	}
	var delay time.Duration
	var result any = map[string]any{}
	switch request.Method {
	case "initialize":
		delay = w.initDelay
		result = map[string]any{"protocolVersion": 1, "agentInfo": map[string]any{"name": "Fake devin"}}
	case "session/new":
		delay = w.sessionDelay
		result = map[string]any{"sessionId": "virtual-session", "availableModels": []any{map[string]any{"modelId": "virtual-model", "name": "Virtual Model"}}}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return 0, err
	}
	bridge := w.bridge
	id := append(json.RawMessage(nil), request.ID...)
	go func() {
		time.Sleep(delay)
		bridge.handleResponse(id, encoded, nil)
	}()
	return len(data), nil
}

func (w *virtualProbeWriter) Close() error { return nil }
