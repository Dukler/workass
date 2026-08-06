package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRepairStartupStatePreservesMalformedFilesAndLeavesValidData(t *testing.T) {
	stateDir := t.TempDir()
	validIdentity := []byte(`{"machineId":"m-stable","displayName":"Stable"}`)
	if err := os.WriteFile(filepath.Join(stateDir, "machine-id.json"), validIdentity, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "machines.json"), []byte(`{broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "daemon-cert.pem"), []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "daemon-key.pem"), []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := repairStartupState(stateDir)
	if err != nil {
		t.Fatalf("repair startup state: %v", err)
	}
	if len(report.Moved) != 3 {
		t.Fatalf("preserved files = %#v", report.Moved)
	}
	if data, err := os.ReadFile(filepath.Join(stateDir, "machine-id.json")); err != nil || string(data) != string(validIdentity) {
		t.Fatalf("valid identity changed: %q err=%v", data, err)
	}
	for _, name := range []string{"machines.json", "daemon-cert.pem", "daemon-key.pem"} {
		if _, err := os.Stat(filepath.Join(stateDir, name)); !os.IsNotExist(err) {
			t.Fatalf("malformed %s remains after repair: %v", name, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(stateDir, "recovery"))
	if err != nil || len(entries) != 3 {
		t.Fatalf("recovery copies = %d err=%v", len(entries), err)
	}
}

func TestLocalRecoveryShutdownHandlerAcceptsOnlyLoopbackPost(t *testing.T) {
	stopped := make(chan struct{})
	handler := localRecoveryShutdownHandler(func() { close(stopped) })
	remote := httptest.NewRequest(http.MethodPost, "https://workass.local/workass/recovery/shutdown", nil)
	remote.RemoteAddr = "192.0.2.10:3000"
	remoteRecorder := httptest.NewRecorder()
	handler.ServeHTTP(remoteRecorder, remote)
	if remoteRecorder.Code != http.StatusForbidden {
		t.Fatalf("remote recovery status = %d", remoteRecorder.Code)
	}

	local := httptest.NewRequest(http.MethodPost, "https://workass.local/workass/recovery/shutdown", nil)
	local.RemoteAddr = "127.0.0.1:3000"
	localRecorder := httptest.NewRecorder()
	handler.ServeHTTP(localRecorder, local)
	if localRecorder.Code != http.StatusAccepted {
		t.Fatalf("local recovery status = %d", localRecorder.Code)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("local recovery did not request shutdown")
	}
}
