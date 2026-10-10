//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHeadlessServiceInstallationRemoved(t *testing.T) {
	root := t.TempDir()
	err := installHeadlessService(headlessServiceOptions{
		Executable: filepath.Join(root, "workass"),
		StateDir:   filepath.Join(root, "state"),
		Port:       18788,
		Bind:       "localhost",
		Profile:    "test",
	})
	if err == nil || !strings.Contains(err.Error(), "run --headless directly") {
		t.Fatalf("service installation must be refused with direct-start guidance: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("refused installation wrote profile files: %v, %v", entries, err)
	}
}
