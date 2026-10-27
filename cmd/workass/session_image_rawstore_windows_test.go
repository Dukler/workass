//go:build windows

package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

// Windows keeps 0.1.229's store until a real Windows runtime check of the
// raw store passes: the platform default is off, and with it a new image is
// only its base64 text.
func TestWindowsKeepsTheLegacyImageStore(t *testing.T) {
	if sessionImageRawStoreSupported {
		t.Fatal("the raw store must stay off on Windows until it is proven there")
	}
	withRawStore(t, sessionImageRawStoreSupported)
	stateDir := t.TempDir()
	sessions := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	text := base64.StdEncoding.EncodeToString(testImageBytes(t, 50))
	attachments, err := sessions.PersistProviderAttachments([]any{map[string]any{"mimeType": "image/png", "data": text}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sessionImageRawPath(stateDir, attachments[0].Digest)); !os.IsNotExist(err) {
		t.Fatalf("a raw file was written on Windows: %v", err)
	}
	if stored, err := os.ReadFile(sessionImageLegacyPath(stateDir, attachments[0].Digest)); err != nil || string(stored) != text {
		t.Fatalf("legacy file missing or wrong: %v", err)
	}
}
