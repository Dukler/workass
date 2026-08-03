package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"workass/internal/acp"
	"workass/internal/wire"
)

// A client that attaches after the fact — a reloaded renderer, a phone joining
// the LAN — learns the chat's background state by INVOKING spawned-work:list,
// not by waiting for spawned-work:changed. For a chat whose background state is
// quiet that event never fires again, so an obligation carried only by the
// event would be invisible to every client that was not already listening.
func TestSpawnedWorkListCarriesTheObligation(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stateDir, "obligations"), 0o755); err != nil {
		t.Fatalf("mkdir obligations: %v", err)
	}
	// needs_input deliberately: boot reconciliation rewrites working and parked
	// records, so seeding either would test the restart rules instead of the
	// hydration this file is about.
	snapshot := `{"open":[{"tabId":"tab-1","chatId":"chat-1","state":"needs_input","source":"declared",` +
		`"note":"esperando tu respuesta","openedAt":"2026-07-27T06:00:00Z","updatedAt":"2026-07-27T06:10:00Z"}]}`
	if err := os.WriteFile(filepath.Join(stateDir, "obligations", "tab-1.json"), []byte(snapshot), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}

	manager := acp.NewManager(acp.Options{StateDir: stateDir})
	hub := wire.NewHub()
	registerAcpHandlers(hub, manager, stateDir, nil, nil)

	reply := invokeSpawnedWorkList(t, hub, "tab-1", "chat-1")
	obligation, ok := reply["obligation"].(map[string]any)
	if !ok {
		t.Fatalf("spawned-work:list reply = %#v, want an obligation", reply)
	}
	if obligation["state"] != "needs_input" || obligation["source"] != "declared" {
		t.Fatalf("obligation = %#v, want needs_input/declared", obligation)
	}

	// A chat that owes nothing must produce exactly the pre-obligation reply
	// shape: the field is additive, so its absence is what an older daemon
	// sends and what a client falling back to its previous behaviour reads.
	quiet := invokeSpawnedWorkList(t, hub, "tab-1", "chat-none")
	if _, present := quiet["obligation"]; present {
		t.Fatalf("quiet chat reply = %#v, want no obligation key", quiet)
	}
}

func invokeSpawnedWorkList(t *testing.T, hub *wire.Hub, tabID, chatID string) map[string]any {
	t.Helper()
	raw, err := hub.Invoke("spawned-work:list", []any{map[string]any{"tabId": tabID, "chatId": chatID}})
	if err != nil {
		t.Fatalf("spawned-work:list invoke: %v", err)
	}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	var reply map[string]any
	if err := json.Unmarshal(data, &reply); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	return reply
}
