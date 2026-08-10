package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workass/internal/fleet"
)

func TestFleetQRRefusesToPromiseLANAccessFromLoopbackDaemon(t *testing.T) {
	keys, err := fleet.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, fleetQRPath, nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()

	newFleetQRHandler(keys, 8788, "localhost", nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "listen on the LAN") {
		t.Fatalf("loopback QR response = %d %q", rec.Code, rec.Body.String())
	}
	if ids := keys.KeyIDs(); len(ids) != 0 {
		t.Fatalf("unreachable QR minted a fleet key: %v", ids)
	}
}

func TestFleetQRDrawsOnlyForLocalViewerOfLANDaemon(t *testing.T) {
	keys, err := fleet.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	local := httptest.NewRequest(http.MethodGet, fleetQRPath+"?host=192.168.0.13:8788", nil)
	local.RemoteAddr = "127.0.0.1:54321"
	localRec := httptest.NewRecorder()
	newFleetQRHandler(keys, 8788, "lan", nil).ServeHTTP(localRec, local)
	if localRec.Code != http.StatusOK || localRec.Header().Get("Content-Type") != "image/svg+xml; charset=utf-8" {
		t.Fatalf("LAN QR response = %d type=%q body=%q", localRec.Code, localRec.Header().Get("Content-Type"), localRec.Body.String())
	}
	if !strings.Contains(localRec.Body.String(), "<svg") {
		t.Fatal("LAN QR response was not SVG")
	}

	remote := httptest.NewRequest(http.MethodGet, fleetQRPath, nil)
	remote.RemoteAddr = "192.168.0.44:54321"
	remoteRec := httptest.NewRecorder()
	newFleetQRHandler(keys, 8788, "lan", nil).ServeHTTP(remoteRec, remote)
	if remoteRec.Code != http.StatusForbidden {
		t.Fatalf("remote QR read = %d, want forbidden", remoteRec.Code)
	}
}
