package wire

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workass/internal/fleet"
)

// A client that can read lan:devices needs to know which row is ITSELF, or a
// revoke list is a way to tap away your own access. The only identifier it ever
// receives is deviceId on the approved access state, so that field is
// contractual on every path that reaches "approved" — and, more sharply, it must
// be the SAME id after a reconnect. An id that were merely incidental could be
// re-minted per socket and the mark would silently move to another row.
func TestApprovedAccessStateCarriesAStableDeviceIdAcrossReconnect(t *testing.T) {
	manager, _, _ := testLeaseManager(t)
	store, err := fleet.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, minted, err := store.EnsureKey()
	if err != nil || !minted {
		t.Fatalf("mint fleet key: minted=%v err=%v", minted, err)
	}
	const machineID = "m-test-access-identity"

	hub := NewHub(Options{Lease: manager, TrustLocalhost: false})
	hub.SetFleet(store, machineID)
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	// Path 1 — fresh fleet enrolment.
	phone := dialWSPath(t, server.URL, "/?deviceName=phone", "YWNjZXNzLWlkLXBob25lLTAwMQ==")
	if waiting := readEvent(t, phone); waiting.Payload["state"] != "waiting" {
		t.Fatalf("phone parked = %+v", waiting)
	}
	sendInvoke(t, phone, 1, "fleet:challenge")
	challenge, _ := readReply(t, phone).Result.(map[string]any)
	serverNonce, _ := challenge["serverNonce"].(string)
	clientNonce, err := fleet.NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	sendInvoke(t, phone, 2, "fleet:enroll", map[string]any{
		"clientNonce": clientNonce,
		"proof":       fleet.Proof(key.Secret, serverNonce, clientNonce, machineID),
		"name":        "Pixel 9",
	})
	enrolled, events := readReplyFor(t, phone, 2)
	if enrolled.Error != nil {
		t.Fatalf("enrol reply error = %+v", enrolled.Error)
	}
	var approved decodedEvent
	for _, event := range events {
		if event.Channel == "lan:access-state" && event.Payload["state"] == "approved" {
			approved = event
		}
	}
	enrolledID, _ := approved.Payload["deviceId"].(string)
	if enrolledID == "" {
		t.Fatalf("enrolment approved state carried no deviceId: %+v", approved.Payload)
	}
	phone.conn.Close()

	// Path 2 — reconnect carrying the token the client derived for itself.
	token := fleet.Token(key.Secret, serverNonce, clientNonce, machineID)
	again := dialWSPath(t, server.URL, "/?deviceToken="+token+"&deviceName=phone", "YWNjZXNzLWlkLXBob25lLTAwMg==")
	defer again.conn.Close()
	reconnected := readEvent(t, again)
	if reconnected.Payload["state"] != "approved" {
		t.Fatalf("reconnect access state = %+v", reconnected.Payload)
	}
	reconnectedID, _ := reconnected.Payload["deviceId"].(string)
	if reconnectedID != enrolledID {
		t.Fatalf("deviceId moved across reconnect: enrolled=%q reconnected=%q — a client marking itself in a device list would mark the wrong row",
			enrolledID, reconnectedID)
	}

	// And it must name a row the client can actually find in lan:devices, which
	// is the list the mark is drawn onto.
	sendInvoke(t, again, 3, "lan:devices")
	listed, _ := readReply(t, again).Result.(map[string]any)
	rows, _ := listed["devices"].([]any)
	found := false
	for _, row := range rows {
		entry, _ := row.(map[string]any)
		if entry != nil && entry["deviceId"] == enrolledID {
			found = true
		}
	}
	if !found {
		t.Fatalf("deviceId %q is absent from lan:devices (%d rows); the client cannot mark itself", enrolledID, len(rows))
	}
}
