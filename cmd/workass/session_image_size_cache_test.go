package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Images are content-addressed and immutable: once a sidecar's header was
// read, later projections take its size from the cache without touching the
// file system at all.
func TestImagePixelSizeCacheHitNeedsNoFileSystem(t *testing.T) {
	stateDir := t.TempDir()
	ref := writeSessionImageSidecar(t, stateDir, "data:image/png;base64,"+sizedPNGBase64(t, 7, 5))
	path := filepath.Join(stateDir, ref)
	if width, height := sessionImageRefSize(ref, stateDir); width != 7 || height != 5 {
		t.Fatalf("first read = %dx%d", width, height)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if width, height := sessionImageRefSize(ref, stateDir); width != 7 || height != 5 {
		t.Fatalf("a cached size went back to the file system: %dx%d", width, height)
	}
}
