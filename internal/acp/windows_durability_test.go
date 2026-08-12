//go:build windows

package acp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsSpawnedWorkSnapshotCommitDoesNotSyncDirectoryHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spawned-work", "tab.json")
	if err := writeSpawnedWorkFile(path, []byte(`[]`)); err != nil {
		t.Fatalf("write Windows spawned-work snapshot: %v", err)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != `[]` {
		t.Fatalf("read Windows spawned-work snapshot: raw=%q err=%v", raw, err)
	}
}
