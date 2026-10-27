//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// A clone carries its source's BSD flags. A Finder-locked source (uchg) makes
// an immutable clone that can be neither chmodded, published nor removed, so
// every attempt to clone it leaves a temporary file nobody can delete.
func TestAcceptCloneOfALockedSourceLeavesNothingBehind(t *testing.T) {
	_, storeDir, raw, name := cloneGuardHome(t)
	work := filepath.Join(filepath.Dir(filepath.Dir(storeDir)), "home", "work")
	must(t, os.MkdirAll(work, 0o700))
	source := filepath.Join(work, "locked.png")
	must(t, os.WriteFile(source, raw, 0o600))
	must(t, unix.Chflags(source, unix.UF_IMMUTABLE))
	t.Cleanup(func() {
		_ = unix.Chflags(source, 0)
		entries, _ := os.ReadDir(storeDir)
		for _, entry := range entries {
			_ = unix.Chflags(filepath.Join(storeDir, entry.Name()), 0)
		}
	})
	withLegacyShadow(t, false)
	for attempt := 0; attempt < 3; attempt++ {
		result, err := persistRawSessionImage(storeDir, name, raw, []string{source}, sessionImageDurable)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if attempt == 0 {
			t.Logf("first attempt: %s (attempts %v)", result.method, result.attempts)
		}
		_ = os.Remove(filepath.Join(storeDir, name+sessionImageRawSuffix))
	}
	entries, _ := os.ReadDir(storeDir)
	var leftovers []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			leftovers = append(leftovers, entry.Name())
		}
	}
	if len(leftovers) > 0 {
		t.Fatalf("clone attempts of a locked source left %d undeletable temporaries: %v", len(leftovers), leftovers)
	}
}
