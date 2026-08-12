//go:build windows

package chat

import (
	"path/filepath"
	"testing"
)

func TestWindowsFileStoreSaveDoesNotSyncDirectoryHandle(t *testing.T) {
	state, err := NewState("windows-durable-chat")
	if err != nil {
		t.Fatal(err)
	}
	store := FileStore{Path: filepath.Join(t.TempDir(), "provider-chats", "chat.json")}
	if err := store.Save(state); err != nil {
		t.Fatalf("save Windows actor state: %v", err)
	}
	loaded, found, err := store.Load("windows-durable-chat")
	if err != nil {
		t.Fatalf("reload Windows actor state: %v", err)
	}
	if !found {
		t.Fatal("Windows actor state was not found after save")
	}
	if loaded.ChatID != state.ChatID || loaded.Revision != state.Revision {
		t.Fatalf("Windows actor readback mismatch: got=%#v want=%#v", loaded, state)
	}
}
