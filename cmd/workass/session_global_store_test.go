package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGlobalSessionStoreRejectsChatRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	if err := os.WriteFile(path, []byte(`{"v":1,"chats":[{"id":"foreign-tab","chatId":"foreign-chat"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newSessionStore(path)
	if err := store.LoadError(); err == nil || !strings.Contains(err.Error(), "requires canonical actor storage") {
		t.Fatalf("chat-bearing global state load error = %v", err)
	}
	if got := store.Get(); got != nil {
		t.Fatalf("chat-bearing global state was published: %#v", got)
	}
}
