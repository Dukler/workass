package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveWorkassToolsCommandUsesDaemonWithoutSibling(t *testing.T) {
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
