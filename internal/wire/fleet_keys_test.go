package wire

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workass/internal/fleet"
	"workass/internal/lease"
)

type fleetAdminRig struct {
	hub             *Hub
	store           *fleet.Store
	manager         *lease.Manager
	controllerToken string
	server          *httptest.Server
}

func fleetAdminHub(t *testing.T) fleetAdminRig {
	t.Helper()
	manager, _, _ := testLeaseManager(t)
	controllerToken := approveTestDevice(t, manager, "mac", "127.0.0.1")
	store, err := fleet.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub(Options{Lease: manager, TrustLocalhost: false})
	hub.SetFleet(store, "m-admin-test")
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	t.Cleanup(server.Close)
	return fleetAdminRig{hub: hub, store: store, manager: manager, controllerToken: controllerToken, server: server}
}

// structuredErrorCode reads the code out of the JSON body structuredError mints,
// for the handler-level calls that never go through a reply frame.
func structuredErrorCode(err error) string {
	var structured struct {
		Code string `json:"code"`
	}
	if json.Unmarshal([]byte(err.Error()), &structured) != nil {
		return err.Error()
	}
	return structured.Code
}

// The point of the channels: a human with the app and no terminal can see the
// key, which is the only thing they have to carry to the next machine.
func TestFleetKeyIsReadableFromTheAppWithoutATerminal(t *testing.T) {
	rig := fleetAdminHub(t)
	store, controllerToken, server := rig.store, rig.controllerToken, rig.server

	mac := dialWSPath(t, server.URL, "/?deviceToken="+controllerToken+"&deviceName=mac", "ZmxlZXQtYWRtaW4tbWFjMQ==")
	defer mac.conn.Close()
	if approved := readEvent(t, mac); approved.Payload["state"] != "approved" {
		t.Fatalf("controller access event = %+v", approved)
	}

	// A daemon nobody has joined to a fleet has no key, and says so rather than
	// inventing one behind your back.
	sendInvoke(t, mac, 1, "fleet:keys")
	emptyReply, _ := readReplyFor(t, mac, 1)
	empty, _ := emptyReply.Result.(map[string]any)
	if keys, _ := empty["keys"].([]any); len(keys) != 0 {
		t.Fatalf("fresh daemon already had keys: %+v", empty)
	}
	if empty["canReveal"] != true {
		t.Fatalf("a local client must be allowed to reveal: %+v", empty)
	}

	sendInvoke(t, mac, 2, "fleet:mint")
	mintedReply, _ := readReplyFor(t, mac, 2)
	minted, _ := mintedReply.Result.(map[string]any)
	if minted["minted"] != true {
		t.Fatalf("first mint should report minting: %+v", minted)
	}
	secret, _ := minted["secret"].(string)
	if secret == "" || secret != store.Keys()[0].Secret {
		t.Fatalf("mint did not return the key the daemon stored: %+v", minted)
	}

	// Idempotent: pressing the button twice must not put a second fleet on this
	// machine, or half the fleet ends up on a key the other half never heard of.
	sendInvoke(t, mac, 3, "fleet:mint")
	againReply, _ := readReplyFor(t, mac, 3)
	again, _ := againReply.Result.(map[string]any)
	if again["minted"] != false || again["secret"] != secret {
		t.Fatalf("second mint changed the key: %+v", again)
	}

	sendInvoke(t, mac, 4, "fleet:reveal")
	revealedReply, _ := readReplyFor(t, mac, 4)
	revealed, _ := revealedReply.Result.(map[string]any)
	if revealed["secret"] != secret {
		t.Fatalf("reveal = %+v, want the minted secret", revealed)
	}

	// The list itself never carries the secret: it reaches devices that are not
	// allowed to read one.
	sendInvoke(t, mac, 5, "fleet:keys")
	listedReply, _ := readReplyFor(t, mac, 5)
	listed, _ := listedReply.Result.(map[string]any)
	rows, _ := listed["keys"].([]any)
	if len(rows) != 1 {
		t.Fatalf("keys = %+v", listed)
	}
	row, _ := rows[0].(map[string]any)
	if row["keyId"] != minted["keyId"] {
		t.Fatalf("listed key id = %+v", row)
	}
	if _, leaked := row["secret"]; leaked {
		t.Fatal("the non-secret list carried the secret")
	}

	keyID, _ := minted["keyId"].(string)
	sendInvoke(t, mac, 6, "fleet:forget", map[string]any{"keyId": keyID})
	forgottenReply, _ := readReplyFor(t, mac, 6)
	forgotten, _ := forgottenReply.Result.(map[string]any)
	if forgotten["ok"] != true || len(store.KeyIDs()) != 0 {
		t.Fatalf("forget = %+v, store still holds %v", forgotten, store.KeyIDs())
	}
}

// The fleet key opens every machine, so it is the one credential that must not
// travel to a device you merely approved — a phone is a thing you leave in a bar.
func TestFleetSecretNeverLeavesTheMachineThatHoldsIt(t *testing.T) {
	rig := fleetAdminHub(t)
	hub, store := rig.hub, rig.store
	if _, _, err := store.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	phone := &client{ip: "192.168.0.208"}

	listed, err := hub.handleFleetAdmin(phone, "fleet:keys", nil)
	if err != nil {
		t.Fatalf("a remote controller may still read the non-secret list: %v", err)
	}
	payload, _ := listed.(map[string]any)
	if payload["canReveal"] != false {
		t.Fatalf("a remote client must be told it cannot reveal: %+v", payload)
	}

	for _, channel := range []string{"fleet:reveal", "fleet:mint", "fleet:forget"} {
		if _, err := hub.handleFleetAdmin(phone, channel, nil); err == nil {
			t.Fatalf("%s from a remote device was allowed", channel)
		} else if code := structuredErrorCode(err); code != "fleet:not-local" {
			t.Fatalf("%s error code = %q, want fleet:not-local", channel, code)
		}
	}
}

// Reading or retiring the key that admits every device is the same class of
// boundary as revoking one, so it is checked explicitly: acting takes the lease
// now, and a boundary a device can grant itself is not a boundary.
func TestFleetKeyAdminRequiresTheController(t *testing.T) {
	rig := fleetAdminHub(t)
	controllerToken, server := rig.controllerToken, rig.server
	otherToken := approveTestDevice(t, rig.manager, "phone", "192.168.0.208")

	mac := dialWSPath(t, server.URL, "/?deviceToken="+controllerToken+"&deviceName=mac", "ZmxlZXQtYWRtaW4tbWFjMg==")
	defer mac.conn.Close()
	if approved := readEvent(t, mac); approved.Payload["state"] != "approved" {
		t.Fatalf("controller access event = %+v", approved)
	}

	other := dialWSPath(t, server.URL, "/?deviceToken="+otherToken+"&deviceName=phone", "ZmxlZXQtYWRtaW4tcGgxMg==")
	defer other.conn.Close()
	if approved := readEvent(t, other); approved.Payload["state"] != "approved" {
		t.Fatalf("second device access event = %+v", approved)
	}

	sendInvoke(t, other, 1, "fleet:reveal")
	if code := errorCode(t, readReply(t, other)); code != "lan:not-controller" {
		t.Fatalf("non-controller reveal error = %q, want lan:not-controller", code)
	}
}
