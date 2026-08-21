package lease

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewManagerRejectsUnsupportedDeviceStateVersion(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "devices.json"), []byte(`{"version":0,"devices":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewManager(Options{StateDir: stateDir})
	if err == nil || !strings.Contains(err.Error(), "unsupported device state version") {
		t.Fatalf("unsupported device state accepted: %v", err)
	}
}
