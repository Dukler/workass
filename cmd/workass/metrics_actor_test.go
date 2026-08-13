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
	// A noncanonical renderer file deliberately describes a different chat. It
	// must not affect actor-derived metrics.
	if err := os.WriteFile(filepath.Join(stateDir, sessionStateFilename), []byte(`{"chats":[{"id":"stale","messages":[{"content":"stale"}]}]}`), 0600); err != nil {
		t.Fatalf("write stale session mirror: %v", err)
	}

	engine, err := chat.NewEngine("metrics-chat")
	if err != nil {
		t.Fatalf("create actor: %v", err)
	}
	apply := func(command chat.Command) {
		t.Helper()
		if err := engine.Apply(command); err != nil {
			t.Fatalf("seed actor with %T: %v", command, err)
		}
	}
	identity := providercontract.LaneIdentity{ChatID: "metrics-chat", WorkspaceEpoch: "workspace", Realm: providercontract.Realm{
		ProviderID: "codex", MachineID: "machine", AccountScope: "account", InstallScope: "install", Verified: true,
	}}.Normalize()
	apply(chat.InitializeChat{Presentation: chat.PresentationState{TabID: "metrics-tab"}, OperationID: "metrics-create", Digest: "metrics-create-digest"})
	apply(chat.SelectLane{Identity: identity, Owner: providercontract.AttachmentOwner{TabID: "metrics-tab"}})
	apply(chat.LaneOpened{LaneID: identity.ID, Thread: providercontract.ThreadRef{ProviderID: "codex", RootID: "thread", HeadID: "thread", Lineage: 1}, ConnectionGeneration: 1, Context: providercontract.ContextCapabilities{ExactResume: true}})
	apply(chat.Submit{OperationID: "metrics-turn", Text: "hello", Presentation: providercontract.TurnPresentation{UserMessageID: "metrics-user", AssistantMessageID: "metrics-assistant"}})
	apply(chat.TurnAdmitted{OperationID: "metrics-turn", Accepted: true, Turn: providercontract.TurnRef{OperationID: "metrics-turn", NativeID: "native-turn"}})
	apply(chat.ProviderEventReceived{ConnectionGeneration: 1, Event: providercontract.Event{
		Kind:     providercontract.EventThinkingUpdate,
		Identity: providercontract.EventIdentity{ChatID: "metrics-chat", LaneID: identity.ID, OperationID: "metrics-turn", TurnID: "native-turn", Sequence: 1},
		Thinking: &providercontract.ThinkingEvent{Text: "thinking"},
	}})
	apply(chat.InputConsumed{OperationID: "metrics-turn"})
	apply(chat.TurnCompleted{OperationID: "metrics-turn", Assistant: "world"})

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
