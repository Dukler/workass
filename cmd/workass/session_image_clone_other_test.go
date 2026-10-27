//go:build !darwin

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// On Windows and Linux a candidate is never even opened: an unreadable one
// still reports "unsupported", and the image is written from memory.
func TestCloneIsUnsupportedOffDarwin(t *testing.T) {
	dir := t.TempDir()
	candidate := filepath.Join(dir, "viewed.png")
	if err := os.WriteFile(candidate, []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	outcome, err := cloneRawSessionImage(dir, filepath.Join(dir, "out.raw"), "name", []byte("x"), candidate, sessionImageDurable)
	if outcome != cloneUnsupported || err == nil {
		t.Fatalf("outcome %v, %v", outcome, err)
	}
	if mounts := nonLocalMountPoints(); mounts != nil {
		t.Fatalf("no mount table off darwin: %v", mounts)
	}
}
