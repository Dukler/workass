//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsGlobalPresentationCommitWithoutDirectorySyncFailure(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(path)
	if _, err := store.SaveActorGlobalSnapshot(map[string]any{
		globalPresentationOperationField: "windows-global-save",
		globalPresentationRevisionField:  0,
		"theme":                          "dark",
	}); err != nil {
		t.Fatalf("write Windows global presentation state: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Windows global presentation state is missing: %v", err)
	}
}
