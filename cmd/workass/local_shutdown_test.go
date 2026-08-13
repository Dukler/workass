package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

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
