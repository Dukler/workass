//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// prepareRetire syncs a raw twin through os.Open, a GENERIC_READ handle. On
// Windows File.Sync is FlushFileBuffers, which requires GENERIC_WRITE and
// fails with ERROR_ACCESS_DENIED on such a handle.
func TestAcceptSyncOfAReadOnlyHandleOnWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.raw")
	must(t, os.WriteFile(path, []byte("raw"), 0o600))
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := syncSessionImageDataToDevice(file); err != nil {
		t.Fatalf("sync of a read-only handle: %v (reclaim and collection never complete on Windows)", err)
	}
}

// The whole reclaim on the JSON-backend platform: once the window is over,
// every legacy file goes and the pass finishes (collection runs after it).
func TestAcceptReclaimCompletesOnWindows(t *testing.T) {
	f := newMaintenanceFixture(t, 3)
	f.m.window = 0
	report, err := f.m.runOnce(t.Context())
	if err != nil {
		t.Fatalf("pass stopped: %v (report %+v)", err, report)
	}
	if report.Retired != 3 {
		t.Fatalf("retired %d of 3", report.Retired)
	}
	f.assertImagesIntact(t)
}
