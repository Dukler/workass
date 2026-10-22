package wire

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An approved device receives a read-only image key on lan:access-state. It
// authorizes chat-image requests until the device is revoked, it is not the
// device token, and checking it never writes the device book.
func TestApprovedDeviceReceivesAnImageKeyThatRevocationRetires(t *testing.T) {
	manager, _, stateDir := testLeaseManager(t)
	token := approveTestDevice(t, manager, "phone", "192.168.0.20")
	hub := NewHub(Options{Lease: manager, TrustLocalhost: false})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	client := dialWSPath(t, server.URL, "/?deviceToken="+token, "aW1hZ2Uta2V5LXRlc3Q=")
	defer client.conn.Close()
	approved := readEvent(t, client)
	key, _ := approved.Payload["imageKey"].(string)
	deviceID, _ := approved.Payload["deviceId"].(string)
	if approved.Payload["state"] != "approved" || !strings.HasPrefix(key, deviceID+".") || len(key) != len(deviceID)+1+32 {
		t.Fatalf("approved access state carries image key %q for device %q", key, deviceID)
	}
	if strings.Contains(key, token) {
		t.Fatal("the image key exposes the device token")
	}
	before, err := os.ReadFile(filepath.Join(stateDir, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !hub.AuthorizeChatImageKey(key) {
		t.Fatal("a fresh image key was refused")
	}
	after, err := os.ReadFile(filepath.Join(stateDir, "devices.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("authorizing an image key rewrote the device book (err=%v)", err)
	}
	approveTestDevice(t, manager, "laptop", "192.168.0.21")
	devices := manager.Devices()
	otherID := devices[len(devices)-1].ID
	mac := key[len(deviceID)+1:]
	for _, bad := range []string{"", deviceID, deviceID + ".", key + "0", key[:len(key)-1] + "x", otherID + "." + mac, strings.ToUpper(key)} {
		if hub.AuthorizeChatImageKey(bad) {
			t.Fatalf("image key %q was accepted", bad)
		}
	}
	if !manager.RevokeDevice(deviceID) {
		t.Fatal("revoke failed")
	}
	if hub.AuthorizeChatImageKey(key) {
		t.Fatal("a revoked device's image key still authorizes")
	}
}

// Without device pairing every socket is approved, so every image request is.
func TestHubWithoutPairingAuthorizesImageRequests(t *testing.T) {
	if !NewHub().AuthorizeChatImageKey("") {
		t.Fatal("an unpaired hub refused an image request it would serve over the socket")
	}
	var missing *Hub
	if missing.AuthorizeChatImageKey("x.y") {
		t.Fatal("a nil hub authorized an image request")
	}
}
