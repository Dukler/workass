package acp

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentOwnerCWDUsesExactOwnedRunningWorkspace(t *testing.T) {
	root := t.TempDir()
	workspace := t.TempDir()
	manager := NewManager(Options{RootDir: root})
	t.Cleanup(func() { manager.Reset() })
	manager.mu.Lock()
	manager.bindAgentOwnerLocked("owner-html", "chat-html", "tab-html")
	manager.jobs["job-html"] = &Job{
		ID: "job-html", Status: "running", ChatID: "chat-html", TabID: "tab-html", CWD: workspace,
	}
	manager.mu.Unlock()

	cwd, err := manager.AgentOwnerCWD("owner-html", "chat-html", "tab-html")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.Abs(workspace)
	if cwd != want {
		t.Fatalf("cwd = %q, want %q", cwd, want)
	}
	if _, err := manager.AgentOwnerCWD("owner-html", "another-chat", "tab-html"); err == nil ||
		!strings.Contains(err.Error(), "owns this artifact hosting request") {
		t.Fatalf("retargeted owner error = %v", err)
	}
}

func TestAgentOwnerCWDFallsBackToManagerRootForTurnlessOwner(t *testing.T) {
	root := t.TempDir()
	manager := NewManager(Options{RootDir: root})
	t.Cleanup(func() { manager.Reset() })
	manager.mu.Lock()
	manager.bindAgentOwnerLocked("owner-turnless-html", "chat-html", "tab-html")
	manager.mu.Unlock()

	cwd, err := manager.AgentOwnerCWD("owner-turnless-html", "chat-html", "tab-html")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.Abs(root)
	if cwd != want {
		t.Fatalf("cwd = %q, want %q", cwd, want)
	}
}
