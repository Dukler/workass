package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveWorkassToolsCommandUsesDaemonForLegacyWindowsLayouts(t *testing.T) {
	dir := t.TempDir()
	daemon := filepath.Join(dir, "workass-daemon.exe")
	tools := filepath.Join(dir, "workass-tools.exe")
	if err := os.WriteFile(daemon, []byte("fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, present := range []bool{false, true} {
		if present {
			if err := os.WriteFile(tools, []byte("unused helper"), 0755); err != nil {
				t.Fatal(err)
			}
		}
		for _, prod := range []bool{false, true} {
			got, err := resolveWorkassToolsCommand(daemon, "", prod, "windows")
			if err != nil || got != daemon {
				t.Fatalf("helper present=%v, prod=%v: resolved %q, %v; want %q", present, prod, got, err, daemon)
			}
		}
	}
}

func TestResolveWorkassToolsCommandUsesSignedNodeLauncherOnlyForCompleteWindowsLayout(t *testing.T) {
	dir := t.TempDir()
	daemon := filepath.Join(dir, "workass-daemon.exe")
	if err := os.WriteFile(daemon, []byte("daemon"), 0755); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(dir, windowsToolsLauncher.launcher)
	client := filepath.Join(dir, windowsToolsLauncher.client)
	node := filepath.Join(dir, windowsToolsLauncher.node)
	for _, file := range []string{launcher, client, node} {
		if got, err := resolveWorkassToolsCommand(daemon, "", true, "windows"); err != nil || got != daemon {
			t.Fatalf("incomplete bundle selected %q, %v; want legacy daemon %q", got, err, daemon)
		}
		if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("fixture"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, prod := range []bool{false, true} {
		got, err := resolveWorkassToolsCommand(daemon, "", prod, "windows")
		if err != nil || got != launcher {
			t.Fatalf("complete bundle prod=%v resolved %q, %v; want launcher %q", prod, got, err, launcher)
		}
	}
	explicit := filepath.Join(dir, "dev-tools")
	if err := os.WriteFile(explicit, []byte("dev"), 0755); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveWorkassToolsCommand(daemon, explicit, false, "windows"); err != nil || got != explicit {
		t.Fatalf("explicit dev override resolved %q, %v; want %q", got, err, explicit)
	}
	if got, err := resolveWorkassToolsCommand(daemon, explicit, true, "windows"); err != nil || got != launcher {
		t.Fatalf("production override resolved %q, %v; want launcher %q", got, err, launcher)
	}
	if err := os.Remove(client); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveWorkassToolsCommand(daemon, "", true, "windows"); err != nil || got != daemon {
		t.Fatalf("bundle missing client resolved %q, %v; want legacy daemon %q", got, err, daemon)
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
	for _, platform := range []string{"darwin", "linux"} {
		for _, override := range []string{"", explicit, "relative", filepath.Join(dir, "missing"), dir} {
			got, err := resolveWorkassToolsCommand(daemon, override, true, platform)
			if err != nil || got != daemon {
				t.Fatalf("production override %q: resolved %q, %v; want daemon", override, got, err)
			}
		}
	}
	got, err := resolveWorkassToolsCommand(daemon, explicit, false, "darwin")
	if err != nil {
		t.Fatal(err)
	}
	if got != explicit {
		t.Fatalf("resolved %q, want %q", got, explicit)
	}
	if got, err := resolveWorkassToolsCommand(daemon, daemon, false, "darwin"); err != nil || got != daemon {
		t.Fatalf("daemon override resolved %q, %v", got, err)
	}
	for _, override := range []string{"relative", filepath.Join(dir, "missing"), dir} {
		if _, err := resolveWorkassToolsCommand(daemon, override, false, "darwin"); err == nil {
			t.Fatalf("accepted invalid dev override %q", override)
		}
	}
	for _, invalid := range []string{"", " ", "relative"} {
		if _, err := resolveWorkassToolsCommand(invalid, "", true, "darwin"); err == nil {
			t.Fatalf("accepted invalid daemon path %q", invalid)
		}
	}
}
