package main

import (
	"testing"
)

// chatFromSnapshot is a test-only lookup for actor-derived session projections.
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
