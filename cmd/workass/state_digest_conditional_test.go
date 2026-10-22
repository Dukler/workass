package main

import (
	"path/filepath"
	"testing"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
	"workass/internal/wire"
)

// An idle client reads state:digest on every heartbeat. While nothing changed,
// echoing the digest's etag answers a tiny {unchanged, etag}; any change, and
// any client that sends no argument, still gets the whole digest.
func TestStateDigestAnswersUnchangedToItsOwnEtag(t *testing.T) {
	stateDir := t.TempDir()
	providerChats := &providerChatRuntime{
		manager: &acp.Manager{}, sessions: sharedSessionStore(stateDir), stateDir: stateDir,
		actors: map[string]*providerChatActor{}, known: map[string]struct{}{},
	}
	engine, err := chat.NewEngine("c1")
	if err != nil {
		t.Fatalf("create actor: %v", err)
	}
	if err := engine.Apply(chat.InitializeChat{
		Presentation: chat.PresentationState{TabID: "t1", Title: "Etag", ProviderID: "mock"},
		OperationID:  providercontract.OperationID("etag-create"), Digest: "etag-create",
	}); err != nil {
		t.Fatalf("initialize actor: %v", err)
	}
	providerChats.actors["c1"] = &providerChatActor{engine: engine}
	providerChats.known["c1"] = struct{}{}
	hub := wire.NewHub()
	registerStateDigestHandler(hub, providerChats, nil, newAppSettingsStore(filepath.Join(stateDir, "app-settings.json")))

	raw, err := hub.Invoke("state:digest", nil)
	if err != nil {
		t.Fatalf("state:digest: %v", err)
	}
	full := mapFromAnyMain(raw)
	etag := fieldString(full, "etag")
	if etag == "" || len(anySlice(full["chats"])) != 1 {
		t.Fatalf("full digest = %#v", full)
	}

	raw, err = hub.Invoke("state:digest", []any{map[string]any{"ifNoneMatch": etag}})
	if err != nil {
		t.Fatalf("conditional state:digest: %v", err)
	}
	unchanged := mapFromAnyMain(raw)
	if unchanged["unchanged"] != true || fieldString(unchanged, "etag") != etag || unchanged["chats"] != nil {
		t.Fatalf("unchanged digest = %#v", unchanged)
	}

	raw, err = hub.Invoke("state:digest", []any{map[string]any{"ifNoneMatch": "stale"}})
	if err != nil {
		t.Fatalf("stale conditional state:digest: %v", err)
	}
	if again := mapFromAnyMain(raw); fieldString(again, "etag") != etag || len(anySlice(again["chats"])) != 1 {
		t.Fatalf("a stale etag must receive the full digest: %#v", again)
	}

	if err := engine.Apply(chat.ReplaceStagedQueue{
		OperationID: "etag-queue", Digest: "etag-queue", ExpectedRevision: 0,
		Entries: []chat.StagedQueueEntry{{ID: "q1", Text: "next", Source: "host", Delivery: "queue"}},
	}); err != nil {
		t.Fatalf("change actor: %v", err)
	}
	raw, err = hub.Invoke("state:digest", []any{map[string]any{"ifNoneMatch": etag}})
	if err != nil {
		t.Fatalf("state:digest after a change: %v", err)
	}
	changed := mapFromAnyMain(raw)
	if changed["unchanged"] == true || fieldString(changed, "etag") == etag || len(anySlice(changed["chats"])) != 1 {
		t.Fatalf("a changed digest answered as unchanged: %#v", changed)
	}
}
