//go:build darwin

package acp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRealOMLXQwenTurn is an opt-in protocol canary for the provider-owned
// oMLX server and Qwen Code ACP harness. Deterministic fixtures remain the
// correctness oracle; this test verifies only discovery, authentication,
// launch, streaming, and terminal protocol state, never response quality.
func TestRealOMLXQwenTurn(t *testing.T) {
	if os.Getenv("WORKASS_REAL_OMLX") != "1" {
		t.Skip("set WORKASS_REAL_OMLX=1 on a Mac running oMLX and Qwen Code")
	}
	root := repoRoot(t)
	stateRoot := t.TempDir()
	providersFile := filepath.Join(stateRoot, "providers.json")
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir:            root,
		StateDir:           filepath.Join(stateRoot, "state"),
		ProviderConfigFile: providersFile,
		Broadcast:          events.Broadcast,
		InitTimeout:        2 * time.Minute,
		RSSSampleInterval:  time.Hour,
		LocalModelEndpoints: []string{
			"http://127.0.0.1:1/v1/models",
			"http://127.0.0.1:1/v1/models",
			defaultOMLXModelsURL,
		},
	})
	t.Cleanup(func() { manager.Reset() })

	manager.DetectProviders(context.Background(), DetectOptions{ProviderID: "qwen"})
	qwen := assertProviderListItem(t, manager.ProvidersList(), "qwen", providerStatusReady, true)
	autoEnv, _ := qwen["autoEnv"].(map[string]string)
	if autoEnv["OPENAI_BASE_URL"] != "http://127.0.0.1:8000/v1" || autoEnv["OPENAI_API_KEY"] != "[redacted]" || autoEnv["QWEN_CODE_SAFE_MODE"] != "true" {
		t.Fatalf("real oMLX-backed Qwen detection did not expose the redacted launch contract")
	}

	session, err := manager.NewSession(context.Background(), SessionOptions{
		TabID: "real-omlx-qwen-tab", ChatID: "real-omlx-qwen-chat", ProviderID: "qwen", CWD: root,
	})
	if err != nil {
		t.Fatalf("real oMLX-backed Qwen session failed: %s", redactSensitiveText(err.Error()))
	}
	job, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, TabID: "real-omlx-qwen-tab",
		ChatID: "real-omlx-qwen-chat", ProviderID: "qwen", CWD: root,
		Prompt: "Reply with one short word. Do not call tools.",
	})
	if err != nil {
		t.Fatalf("real oMLX-backed Qwen turn was not admitted: %s", redactSensitiveText(err.Error()))
	}
	jobID := jobID(job)
	end := events.waitJobEnd(t, jobID, 5*time.Minute)
	assertJobStatus(t, end, "done", 0, "end_turn")
	endJob := jobFromEnd(end)
	if endJob["providerId"] != "qwen" {
		t.Fatalf("real oMLX-backed turn provider=%v, want qwen", endJob["providerId"])
	}
	if chunks := len(events.jobEvents(jobID, "data")); chunks == 0 {
		t.Fatal("real oMLX-backed Qwen turn emitted no streamed data")
	}

	apiKey, err := omlxAPIKey(manager.opts)
	if err != nil || apiKey == "" {
		t.Fatalf("real oMLX credential was unavailable: %v", err)
	}
	persisted, err := os.ReadFile(providersFile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(persisted, []byte(apiKey)) {
		t.Fatal("real oMLX API key crossed into providers.json")
	}
	t.Logf("canary receipt: provider=qwen model=%s streamed=true status=done stopReason=end_turn credentialPersisted=false", autoEnv["OPENAI_MODEL"])
}
