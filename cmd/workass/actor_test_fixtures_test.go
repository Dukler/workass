package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// sessionMirrorFixture is migration input only. New-chat tests must use the
// actor-native chat:create boundary instead of seeding this retired shape.
func sessionMirrorFixture(tabID, chatID, prompt string) map[string]any {
	return map[string]any{
		"v": json.Number("1"), "activeId": tabID, "seq": json.Number("2"),
		"theme": "dark", "panes": map[string]any{}, "mode": "chats",
		"chats": []any{map[string]any{
			"id": tabID, "chatId": chatID, "title": "Offline", "titleLocked": true,
			"group": nil, "cwd": nil, "currentModelId": nil, "currentModeId": nil,
			"draft": "", "providerId": "mock",
			"messages": []any{
				map[string]any{"id": "client-u", "role": "user", "content": prompt, "status": "done", "at": "2026-07-11T10:00:00Z", "events": []any{}},
				map[string]any{"id": "client-a", "role": "assistant", "content": "", "status": "running", "at": nil, "events": []any{}},
			},
		}},
	}
}

// chatFromSnapshot is a test-only lookup for migration/session fixtures.
func chatFromSnapshot(snapshot map[string]any, tabID string) map[string]any {
	for _, raw := range anySlice(snapshot["chats"]) {
		chat := mapFromAnyMain(raw)
		if fieldString(chat, "id") == tabID {
			return chat
		}
	}
	return nil
}

func sessionAssistant(t *testing.T, snapshot map[string]any, tabID string) map[string]any {
	t.Helper()
	chat := chatFromSnapshot(snapshot, tabID)
	if chat == nil {
		t.Fatalf("chat %q missing from %#v", tabID, snapshot)
	}
	for i := len(messageSlice(chat)) - 1; i >= 0; i-- {
		message := mapFromAnyMain(messageSlice(chat)[i])
		if message["role"] == "assistant" {
			return message
		}
	}
	t.Fatalf("assistant missing from chat %#v", chat)
	return nil
}

// writeLegacySessionSnapshot seeds pre-cutover input before the session store
// is opened. Migration fixtures must not use the retired runtime Save method.
func writeLegacySessionSnapshot(t *testing.T, stateDir string, snapshot map[string]any) {
	t.Helper()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create legacy state directory: %v", err)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal legacy session snapshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, sessionStateFilename), raw, 0o600); err != nil {
		t.Fatalf("write legacy session snapshot: %v", err)
	}
}
