package main

import (
	"os"
	"path/filepath"
	"testing"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

func TestDaemonMetricsUsesAuthoritativeActorInventory(t *testing.T) {
	stateDir := t.TempDir()
	// The retired renderer mirror deliberately describes a different chat. It
	// must not affect the metrics response after actor cutover.
	if err := os.WriteFile(filepath.Join(stateDir, sessionStateFilename), []byte(`{"chats":[{"id":"stale","messages":[{"content":"stale"}]}]}`), 0600); err != nil {
		t.Fatalf("write stale session mirror: %v", err)
	}

	engine, err := chat.NewEngine("metrics-chat")
	if err != nil {
		t.Fatalf("create actor: %v", err)
	}
	if err := engine.Apply(chat.MigrateLegacyChat{
		Version: 2, Digest: "metrics-digest",
		Presentation: chat.PresentationState{TabID: "metrics-tab"},
		Messages: []chat.LegacyMessage{
			{MessageID: "metrics-user", OperationID: "metrics-turn", Role: "user", Text: "hello", Status: "done"},
			{MessageID: "metrics-assistant", OperationID: "metrics-turn", Role: "assistant", Text: "world", Status: "done", Timeline: []chat.TimelineEntry{{
				Key:      "metrics-thinking",
				At:       0,
				Kind:     providercontract.EventThinkingUpdate,
				Thinking: &providercontract.ThinkingEvent{Text: "thinking"},
			}}},
		},
	}); err != nil {
		t.Fatalf("seed actor: %v", err)
	}

	chatID := "metrics-chat"
	runtime := &providerChatRuntime{
		manager:  &acp.Manager{},
		sessions: &sessionStore{},
		stateDir: stateDir,
		actors: map[string]*providerChatActor{
			chatID: {engine: engine},
		},
		known: map[string]struct{}{chatID: {}},
	}

	metrics := daemonMetrics(runtime, stateDir, nil, nil)
	session, ok := metrics["session"].(map[string]any)
	if !ok {
		t.Fatalf("session metrics = %#v", metrics["session"])
	}
	if got := session["chats"]; got != 1 {
		t.Fatalf("actor chat count = %#v, want 1", got)
	}
	if got := session["messages"]; got != 2 {
		t.Fatalf("actor message count = %#v, want 2", got)
	}
	if got := session["events"]; got != 1 {
		t.Fatalf("actor event count = %#v, want 1", got)
	}
	if got := session["messageBytes"]; got != len("hello")+len("world") {
		t.Fatalf("actor message bytes = %#v, want %d", got, len("hello")+len("world"))
	}
	if got := session["snapshotBytes"]; got == int64(0) {
		t.Fatal("session snapshot bytes did not report the global file")
	}
}
