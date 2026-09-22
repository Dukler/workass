package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveWorkassToolsCommandUsesTheDedicatedSibling(t *testing.T) {
	dir := t.TempDir()
	daemon := filepath.Join(dir, "workass-daemon.exe")
	tools := filepath.Join(dir, "workass-tools.exe")
	for _, file := range []string{daemon, tools} {
		if err := os.WriteFile(file, []byte("fixture"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	got, err := resolveWorkassToolsCommand(daemon, "", true, "windows")
	if err != nil {
		t.Fatal(err)
	}
	if got != tools {
		t.Fatalf("resolved %q, want %q", got, tools)
	}
}

func TestResolveWorkassToolsCommandFailsClosedAndAllowsOnlyExplicitDevOverride(t *testing.T) {
	dir := t.TempDir()
	daemon := filepath.Join(dir, "workass")
	explicit := filepath.Join(dir, "dev-tools")
	if err := os.WriteFile(daemon, []byte("daemon"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(explicit, []byte("tools"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveWorkassToolsCommand(daemon, "", true, "darwin"); err == nil {
		t.Fatal("production accepted a missing sibling tools executable")
	}
	got, err := resolveWorkassToolsCommand(daemon, explicit, false, "darwin")
	if err != nil {
		t.Fatal(err)
	}
	if got != explicit {
		t.Fatalf("resolved %q, want %q", got, explicit)
	}
	if _, err := resolveWorkassToolsCommand(daemon, daemon, false, "darwin"); err == nil || !strings.Contains(err.Error(), "must not be the daemon") {
		t.Fatalf("daemon override error = %v", err)
	}
}
