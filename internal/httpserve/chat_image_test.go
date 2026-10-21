package httpserve

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workass/internal/lease"
	"workass/internal/wire"
)

func chatImageRequest(t *testing.T, server *Server, method, target, remote string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "http://daemon.test"+target, nil)
	request.RemoteAddr = remote
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	return recorder
}

// The chat-image route checks the device's image key before the handler runs,
// from loopback exactly as from the LAN.
func TestChatImageRouteRequiresAnApprovedDeviceKey(t *testing.T) {
	manager, err := lease.NewManager(lease.Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.ApproveDevice("phone", "192.168.0.9"); err != nil {
		t.Fatal(err)
	}
	server := New("", wire.NewHub(wire.Options{Lease: manager}), nil)
	reached := 0
	server.ChatImages = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached++ })
	target := ChatImagePathPrefix + "tab/" + "ab"
	for _, remote := range []string{"127.0.0.1:1", "192.168.0.9:1"} {
		for _, query := range []string{"", "?k=", "?k=" + manager.Devices()[0].ID + ".00000000000000000000000000000000"} {
			got := chatImageRequest(t, server, http.MethodGet, target+query, remote)
			if got.Code != http.StatusForbidden || got.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("%s %q: status %d cache %q, want an uncacheable 403", remote, query, got.Code, got.Header().Get("Cache-Control"))
			}
		}
	}
	if got := chatImageRequest(t, server, http.MethodPut, target, "127.0.0.1:1"); got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT: status %d, want 405", got.Code)
	}
	if reached != 0 {
		t.Fatalf("an unauthorized request reached the image handler %d times", reached)
	}
}

func TestChatImageRouteDelegatesWhenAuthorizedAndIsAbsentWithoutAHandler(t *testing.T) {
	// A hub without pairing approves every client, as it does on the socket.
	server := New("", wire.NewHub(), nil)
	if got := chatImageRequest(t, server, http.MethodGet, ChatImagePathPrefix+"tab/ab", "127.0.0.1:1"); got.Code != http.StatusNotFound {
		t.Fatalf("no image handler: status %d, want 404", got.Code)
	}
	server.ChatImages = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	if got := chatImageRequest(t, server, http.MethodGet, ChatImagePathPrefix+"tab/ab", "192.168.0.9:1"); got.Code != http.StatusTeapot {
		t.Fatalf("authorized request: status %d, want the handler's answer", got.Code)
	}
}
