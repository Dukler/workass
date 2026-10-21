package main

import (
	"path/filepath"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/wire"
)

// A renderer whose catalog hash diverged from state:digest (a missed
// chat:catalog) heals by reading providers:catalog. That only converges if the
// read returns exactly the groups the digest hashes, so a renderer hashing the
// reply lands on the digest's own value.
func TestProvidersCatalogReadMatchesTheDigestCatalogHash(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	store := sharedSessionStore(stateDir)
	runtime := newProviderChatRuntime(manager, store, stateDir)
	if err := runtime.StartupError(); err != nil {
		t.Fatalf("start actor runtime: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(t.Context()) })

	hub := wire.NewHub()
	registerAcpHandlers(hub, manager, stateDir, store, nil, runtime)
	registerStateDigestHandler(hub, runtime, manager, newAppSettingsStore(filepath.Join(stateDir, "app-settings.json")))

	raw, err := hub.Invoke("providers:catalog", nil)
	if err != nil {
		t.Fatalf("providers:catalog: %v", err)
	}
	payload, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("providers:catalog reply = %#v, want the chat:catalog payload", raw)
	}
	groups, ok := payload["groups"].([]acp.CatalogGroup)
	if !ok || len(groups) == 0 {
		t.Fatalf("providers:catalog groups = %#v", payload["groups"])
	}
	if _, ok := payload["models"]; !ok {
		t.Fatalf("providers:catalog reply lacks the chat:catalog models field: %#v", payload)
	}

	digestRaw, err := hub.Invoke("state:digest", nil)
	if err != nil {
		t.Fatalf("state:digest: %v", err)
	}
	digest := mapFromAnyMain(digestRaw)
	hashes, ok := digest["catalogHash"].(map[string]string)
	if !ok || len(hashes) != len(groups) {
		t.Fatalf("digest catalogHash = %#v for %d groups", digest["catalogHash"], len(groups))
	}
	for _, group := range groups {
		if got, want := stateDigestHash(group), hashes[group.ProviderID]; got != want {
			t.Fatalf("providers:catalog group %q hashes %s, digest says %s", group.ProviderID, got, want)
		}
	}
}

// The renderer hashes the JSON it received with the same encoding Go uses
// (desktop/renderer2/src/store/digest-hash.ts). json.Marshal escapes <, > and
// &; a renderer that did not escape them hashed every such group differently
// for good.
// The same literal is asserted by tests/digest-hash.test.ts.
const digestParityHash = "099b0cc7cebd6ca886d1127a911b9e8c4e6797a64ba5f46ca8236366797798fc"

func TestStateDigestHashParityFixture(t *testing.T) {
	latency := int64(812)
	group := acp.CatalogGroup{
		ProviderID:        "mock",
		ProviderName:      "Mock <beta> & co",
		Models:            []acp.Model{{ModelID: "m1", Name: "Model \u2028 one", Efforts: []string{"low"}}},
		Modes:             []acp.Mode{},
		PermissionIntents: map[string]string{"write": "default", "read": "plan"},
		Status:            "error",
		LatencyMs:         &latency,
		Error:             "exec: a -> b && c > d é",
	}
	if got := stateDigestHash(group); got != digestParityHash {
		t.Fatalf("parity fixture hash = %s, want %s (update both this test and tests/digest-hash.test.ts)", got, digestParityHash)
	}
}
