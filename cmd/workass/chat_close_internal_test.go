package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Closing a chat that still has a job trace must stay closed. The job-trace
// restore loop in mergeAuthoritativeTurnsLocked used to re-append the server
// row on the very same save, so the × silently did nothing: the renderer's
// post-save verification saw the chat present and retried the delete forever.
func TestClosingChatWithLiveJobTraceStaysClosed(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), sessionStateFilename)
	store := newSessionStore(statePath)

	snapshot := sessionMirrorFixture("tab-close-me", "conv-close-me", "hold this chat open")
	if !store.Save(snapshot) {
		t.Fatal("seed save rejected")
	}
	// A running job pinned to that tab is exactly the condition that resurrected it.
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start",
		"job": map[string]any{
			"id": "job-close-me", "tabId": "tab-close-me", "chatId": "conv-close-me",
			"title": "hold this chat open", "startedAt": "2026-07-24T10:00:00Z",
		},
	})

	closed := map[string]any{
		"v": json.Number("1"), "activeId": nil, "seq": json.Number("3"),
		"theme": "dark", "panes": map[string]any{}, "mode": "chats",
		"chats":                  []any{},
		"_workassSave":           "lean-payload-v2",
		"_workassDeletedChatIds": []any{"tab-close-me"},
	}
	if !store.Save(closed) {
		t.Fatal("close save rejected")
	}
	if chat := chatFromSnapshot(store.Get().(map[string]any), "tab-close-me"); chat != nil {
		t.Fatalf("closed chat came back with a live job trace: %#v", chat)
	}
}

// A subagent's ACP session carries chat/tab id "subagent:<runID>". It is
// internal plumbing and must never mint a user-visible chat; each spawned lane
// used to leave an untitled "Nuevo chat" row behind, persisted across restarts.
func TestSubagentSessionNeverCreatesVisibleChat(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), sessionStateFilename)
	store := newSessionStore(statePath)

	internalTab := subagentTabPrefix + "wa-subagent-1-1"
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start",
		"job": map[string]any{
			"id": "subagent-job-1", "tabId": internalTab, "chatId": internalTab,
			"startedAt": "2026-07-24T10:00:00Z",
		},
	})
	if !store.UpdateChatControls(internalTab, internalTab, "codex", "gpt-5.6-sol", "agent") {
		t.Log("UpdateChatControls declined the internal tab, as expected")
	}
	for _, chat := range anySlice(store.Get().(map[string]any)["chats"]) {
		if id := fieldString(mapFromAnyMain(chat), "id"); isInternalTabID(id) {
			t.Fatalf("subagent session created a visible chat %q", id)
		}
	}

	// Rows persisted by an older build are pruned instead of resurfacing.
	stale := map[string]any{
		"v": json.Number("1"), "activeId": nil, "seq": json.Number("2"),
		"theme": "dark", "panes": map[string]any{}, "mode": "chats",
		"chats": []any{map[string]any{
			"id": internalTab, "chatId": internalTab, "title": "Nuevo chat",
			"titleLocked": true, "messages": []any{},
		}},
	}
	if !store.Save(stale) {
		t.Fatal("save with a stale internal row rejected")
	}
	if chat := chatFromSnapshot(store.Get().(map[string]any), internalTab); chat != nil {
		t.Fatalf("stale internal chat survived the prune: %#v", chat)
	}
}

func TestLegacySubagentRowIsPrunedAndRewrittenOnLoad(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), sessionStateFilename)
	internalTab := subagentTabPrefix + "wa-subagent-1-1"
	legacy := map[string]any{
		"v": json.Number("1"), "activeId": nil, "seq": json.Number("2"),
		"theme": "dark", "panes": map[string]any{}, "mode": "chats",
		"chats": []any{map[string]any{
			"id": internalTab, "chatId": internalTab, "title": "Nuevo chat",
			"titleLocked": true, "messages": []any{},
		}},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy snapshot: %v", err)
	}
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatalf("write legacy snapshot: %v", err)
	}

	loaded := newSessionStore(statePath)
	if err := loaded.LoadError(); err != nil {
		t.Fatalf("load legacy snapshot: %v", err)
	}
	if chat := chatFromSnapshot(loaded.Get().(map[string]any), internalTab); chat != nil {
		t.Fatalf("legacy internal chat survived disk load: %#v", chat)
	}

	reloaded := newSessionStore(statePath)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload rewritten snapshot: %v", err)
	}
	if chat := chatFromSnapshot(reloaded.Get().(map[string]any), internalTab); chat != nil {
		t.Fatalf("rewritten snapshot resurrected internal chat: %#v", chat)
	}
	persisted, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read rewritten snapshot: %v", err)
	}
	if string(persisted) == string(data) {
		t.Fatal("clean legacy snapshot was not rewritten after internal-row pruning")
	}
}
