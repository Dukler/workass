package wire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workass/internal/fleet"
)

// The property that decides whether this is safe to run before TLS: the client
// ends up holding a working device token that was never transmitted. It derives
// the same value the daemon derived, from nonces both sides contributed.
func TestFleetEnrolmentApprovesWithoutAHumanAndWithoutSendingTheToken(t *testing.T) {
	manager, _, stateDir := testLeaseManager(t)
	controllerToken := approveTestDevice(t, manager, "controller", "127.0.0.1")
	store, err := fleet.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, minted, err := store.EnsureKey()
	if err != nil || !minted {
		t.Fatalf("mint fleet key: minted=%v err=%v", minted, err)
	}
	const machineID = "m-test-enrolment"

	hub := NewHub(Options{Lease: manager, TrustLocalhost: false})
	hub.SetFleet(store, machineID)
	hub.Register("state:get", func(args []any) (any, error) { return map[string]any{"ok": true}, nil })
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	controller := dialWSPath(t, server.URL, "/?deviceToken="+controllerToken+"&deviceName=controller", "ZmxlZXQtY29udHJvbGxlcg==")
	defer controller.conn.Close()
	if approved := readEvent(t, controller); approved.Payload["state"] != "approved" {
		t.Fatalf("controller access event = %+v", approved)
	}

	phone := dialWSPath(t, server.URL, "/?deviceName=phone", "ZmxlZXQtcGhvbmUtMDAwMQ==")
	defer phone.conn.Close()
	waiting := readEvent(t, phone)
	if waiting.Payload["state"] != "waiting" {
		t.Fatalf("phone parked = %+v", waiting)
	}

	sendInvoke(t, phone, 1, "fleet:challenge")
	challenge, ok := readReply(t, phone).Result.(map[string]any)
	if !ok || challenge["enabled"] != true || challenge["machineId"] != machineID {
		t.Fatalf("challenge = %+v", challenge)
	}
	serverNonce, _ := challenge["serverNonce"].(string)
	if serverNonce == "" {
		t.Fatal("challenge carried no server nonce; without one a captured proof replays")
	}
	keyIDs, _ := challenge["keyIds"].([]any)
	if len(keyIDs) != 1 || keyIDs[0] != key.KeyID {
		t.Fatalf("advertised key ids = %v, want the one this daemon holds", keyIDs)
	}

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
	result, _ := enrolled.Result.(map[string]any)
	if result["ok"] != true || result["keyId"] != key.KeyID || result["owner"] != fleet.LocalOwner {
		t.Fatalf("enrol result = %+v", result)
	}

	// Nothing reusable came back. The reply carries an id and a name; the token
	// is the client's to compute.
	replyJSON, _ := json.Marshal(enrolled.Result)
	derived := fleet.Token(key.Secret, serverNonce, clientNonce, machineID)
	if bytes.Contains(replyJSON, []byte(derived)) || bytes.Contains(replyJSON, []byte(key.Secret)) {
		t.Fatalf("enrol reply leaked a credential: %s", replyJSON)
	}
	approved := decodedEvent{}
	for _, event := range events {
		if event.Channel == "lan:access-state" {
			approved = event
		}
	}
	if approved.Payload["state"] != "approved" {
		t.Fatalf("phone access state after enrol = %+v", approved)
	}
	if token, present := approved.Payload["deviceToken"]; present && token != nil {
		t.Fatalf("access state sent a device token (%v); enrolment must never transmit one", token)
	}

	// The pending request has to be withdrawn, or its timer denies a device that
	// is already approved.
	hub.mu.RLock()
	stillPending := len(hub.pending)
	hub.mu.RUnlock()
	if stillPending != 0 {
		t.Fatalf("pending access requests after enrolment = %d, want 0", stillPending)
	}

	// Silent by design, so it has to be loud in the record.
	notice := readEventOn(t, controller, "fleet:enrolled")
	if notice.Payload["name"] != "Pixel 9" || notice.Payload["keyId"] != key.KeyID || notice.Payload["machineId"] != machineID {
		t.Fatalf("enrolment broadcast = %+v", notice.Payload)
	}

	// The receipt on disk: an enrolled device is an ordinary device, plus the
	// owner and the key that let it in, and never the token itself.
	devices, err := os.ReadFile(filepath.Join(stateDir, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(devices, []byte(derived)) {
		t.Fatal("device state stored the plaintext token")
	}
	if !bytes.Contains(devices, []byte(`"owner": "`+fleet.LocalOwner+`"`)) || !bytes.Contains(devices, []byte(`"keyId": "`+key.KeyID+`"`)) {
		t.Fatalf("device record is missing owner/keyId: %s", devices)
	}

	// The whole point: the token the client computed for itself works.
	reconnected := dialWSPath(t, server.URL, "/?deviceToken="+derived+"&deviceName=phone", "ZmxlZXQtcmVjb25uZWN0MDA=")
	defer reconnected.conn.Close()
	if state := readEvent(t, reconnected); state.Payload["state"] != "approved" {
		t.Fatalf("reconnect with the derived token = %+v", state)
	}
	sendInvoke(t, reconnected, 3, "state:get")
	if reply := readReply(t, reconnected); reply.Error != nil {
		t.Fatalf("state:get after fleet enrolment = %+v", reply.Error)
	}
}

func TestFleetEnrolmentRefusesAnotherFleetAndBurnsTheChallenge(t *testing.T) {
	manager, _, _ := testLeaseManager(t)
	store, err := fleet.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := store.EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	const machineID = "m-test-refusal"
	hub := NewHub(Options{Lease: manager, TrustLocalhost: false})
	hub.SetFleet(store, machineID)
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	client := dialWSPath(t, server.URL, "/?deviceName=stranger", "ZmxlZXQtc3RyYW5nZXItMQ==")
	defer client.conn.Close()
	readEvent(t, client)

	sendInvoke(t, client, 1, "fleet:challenge")
	challenge, _ := readReply(t, client).Result.(map[string]any)
	serverNonce, _ := challenge["serverNonce"].(string)
	clientNonce, err := fleet.NewNonce()
	if err != nil {
		t.Fatal(err)
	}

	otherSecret, err := fleet.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	sendInvoke(t, client, 2, "fleet:enroll", map[string]any{
		"clientNonce": clientNonce,
		"proof":       fleet.Proof(otherSecret, serverNonce, clientNonce, machineID),
	})
	refused := readReply(t, client)
	if errorCode(t, refused) != "fleet:rejected" {
		t.Fatalf("wrong-fleet enrolment = %+v, want fleet:rejected", refused)
	}

	// One challenge, one attempt: the correct proof against the same nonce must
	// not be a second guess.
	sendInvoke(t, client, 3, "fleet:enroll", map[string]any{
		"clientNonce": clientNonce,
		"proof":       fleet.Proof(key.Secret, serverNonce, clientNonce, machineID),
	})
	retry := readReply(t, client)
	if errorCode(t, retry) != "fleet:challenge-expired" {
		t.Fatalf("reused challenge = %+v, want fleet:challenge-expired", retry)
	}

	// A proof bound to another machine is not a proof for this one, even under
	// the right key: it is what stops a relayed challenge from enrolling here.
	sendInvoke(t, client, 4, "fleet:challenge")
	second, _ := readReply(t, client).Result.(map[string]any)
	secondNonce, _ := second["serverNonce"].(string)
	sendInvoke(t, client, 5, "fleet:enroll", map[string]any{
		"clientNonce": clientNonce,
		"proof":       fleet.Proof(key.Secret, secondNonce, clientNonce, "m-some-other-machine"),
	})
	elsewhere := readReply(t, client)
	if errorCode(t, elsewhere) != "fleet:rejected" {
		t.Fatalf("proof for another machine = %+v, want fleet:rejected", elsewhere)
	}
}

// A daemon nobody has joined to a fleet says so rather than failing, so a client
// can tell "wrong key" apart from "no key here yet".
func TestFleetChallengeReportsDisabledWithoutAKey(t *testing.T) {
	manager, _, _ := testLeaseManager(t)
	store, err := fleet.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub(Options{Lease: manager, TrustLocalhost: false})
	hub.SetFleet(store, "m-test-empty")
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	client := dialWSPath(t, server.URL, "/?deviceName=phone", "ZmxlZXQtZW1wdHktMDAwMDA=")
	defer client.conn.Close()
	readEvent(t, client)
	sendInvoke(t, client, 1, "fleet:challenge")
	reply := readReply(t, client)
	result, _ := reply.Result.(map[string]any)
	if reply.Error != nil || result["enabled"] != false {
		t.Fatalf("challenge without a key = %+v err=%+v", result, reply.Error)
	}
	sendInvoke(t, client, 2, "fleet:enroll", map[string]any{"clientNonce": "x", "proof": "y"})
	enrol := readReply(t, client)
	if errorCode(t, enrol) != "fleet:no-key" {
		t.Fatalf("enrol without a key = %+v, want fleet:no-key", enrol)
	}
}

// readReplyFor returns the reply to one invoke plus every event that overtook
// it. A socket being approved receives its new state on the same wire, so a
// client that assumed reply-first would deadlock — and so would this test.
func readReplyFor(t *testing.T, client *testWSClient, id int) (decodedReply, []decodedEvent) {
	t.Helper()
	events := make([]decodedEvent, 0, 4)
	for i := 0; i < 12; i++ {
		raw := client.readText(t)
		var reply decodedReply
		if err := json.Unmarshal(raw, &reply); err == nil && reply.T == "reply" {
			if reply.ID.String() == fmt.Sprint(id) {
				return reply, events
			}
			continue
		}
		var event decodedEvent
		if err := json.Unmarshal(raw, &event); err == nil && event.T == "event" {
			events = append(events, event)
		}
	}
	t.Fatalf("no reply for invoke %d", id)
	return decodedReply{}, nil
}

// errorCode pulls the code out of a structured wire error, which travels as a
// JSON document inside the frozen protocol's string error field.
func errorCode(t *testing.T, reply decodedReply) string {
	t.Helper()
	if reply.Error == nil {
		return ""
	}
	var structured struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(*reply.Error), &structured); err != nil {
		return *reply.Error
	}
	return structured.Code
}

// readEventOn skips the events a socket happens to receive first and returns the
// one under test.
func readEventOn(t *testing.T, client *testWSClient, channel string) decodedEvent {
	t.Helper()
	for i := 0; i < 8; i++ {
		event := readEvent(t, client)
		if event.Channel == channel {
			return event
		}
	}
	t.Fatalf("no %s event arrived", channel)
	return decodedEvent{}
}

// The rotation gate: retiring a key closes future enrolments and strands
// nobody. A device's token does not depend on the key that admitted it, so
// "stop letting new things in" and "throw this device out" stay separate acts.
func TestRetiringAKeyClosesEnrolmentsWithoutStrandingEnrolledDevices(t *testing.T) {
	manager, _, _ := testLeaseManager(t)
	store, err := fleet.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := store.EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	const machineID = "m-test-rotation"
	hub := NewHub(Options{Lease: manager, TrustLocalhost: false})
	hub.SetFleet(store, machineID)
	hub.Register("state:get", func(args []any) (any, error) { return map[string]any{"ok": true}, nil })
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	first := dialWSPath(t, server.URL, "/?deviceName=laptop", "ZmxlZXQtcm90YXRlLW9uZTA=")
	defer first.conn.Close()
	readEvent(t, first)
	sendInvoke(t, first, 1, "fleet:challenge")
	challenge, _ := readReply(t, first).Result.(map[string]any)
	serverNonce, _ := challenge["serverNonce"].(string)
	clientNonce, err := fleet.NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	sendInvoke(t, first, 2, "fleet:enroll", map[string]any{
		"clientNonce": clientNonce,
		"proof":       fleet.Proof(key.Secret, serverNonce, clientNonce, machineID),
	})
	if reply, _ := readReplyFor(t, first, 2); reply.Error != nil {
		t.Fatalf("first enrolment = %+v", reply.Error)
	}
	token := fleet.Token(key.Secret, serverNonce, clientNonce, machineID)

	if dropped, err := store.Forget(key.KeyID); err != nil || !dropped {
		t.Fatalf("forget key: dropped=%v err=%v", dropped, err)
	}

	// Nothing new gets in under it.
	second := dialWSPath(t, server.URL, "/?deviceName=phone", "ZmxlZXQtcm90YXRlLXR3bw==")
	defer second.conn.Close()
	readEvent(t, second)
	sendInvoke(t, second, 1, "fleet:enroll", map[string]any{"clientNonce": clientNonce, "proof": "anything"})
	if code := errorCode(t, readReply(t, second)); code != "fleet:no-key" {
		t.Fatalf("enrolment under a retired key = %q, want fleet:no-key", code)
	}

	// The device that already enrolled is untouched.
	kept := dialWSPath(t, server.URL, "/?deviceToken="+token+"&deviceName=laptop", "ZmxlZXQtcm90YXRlLWtlcHQ=")
	defer kept.conn.Close()
	if state := readEvent(t, kept); state.Payload["state"] != "approved" {
		t.Fatalf("enrolled device after retiring the key = %+v", state)
	}
	sendInvoke(t, kept, 2, "state:get")
	if reply := readReply(t, kept); reply.Error != nil {
		t.Fatalf("enrolled device lost access when its key was retired: %+v", reply.Error)
	}

	// Close and drain the sockets before TempDir cleanup. A disconnect releases
	// the controller lease synchronously after removing the client from the hub;
	// returning while that goroutine is still persisting devices.json can race
	// testing.TempDir's RemoveAll and recreate the directory underneath it.
	_ = kept.conn.Close()
	_ = second.conn.Close()
	_ = first.conn.Close()
	server.Close()
	deadline := time.Now().Add(time.Second)
	for {
		hub.mu.RLock()
		connected := len(hub.clients)
		hub.mu.RUnlock()
		_, hasController := manager.Controller()
		if connected == 0 && !hasController {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fleet rotation clients did not drain; connected=%d controller=%t", connected, hasController)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A non-browser client has no DOM, so the signal it needs has to be in band. The
// reconnect generation is the CLIENT's own counter — the daemon never sees it —
// but a counter cannot tell a reconnect from a daemon that restarted underneath
// it, and only the daemon knows that. So every socket is told who answered, in
// all three outcomes, on the event it already receives the moment it opens.
func TestEverySocketLearnsWhichDaemonAnsweredWithoutADOM(t *testing.T) {
	manager, _, _ := testLeaseManager(t)
	approvedToken := approveTestDevice(t, manager, "desktop", "127.0.0.1")
	store, err := fleet.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	hub := NewHub(Options{Lease: manager, TrustLocalhost: false})
	hub.SetFleet(store, "m-phone-test")
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	// Approved, parked and rejected all learn it — a phone that is waiting for
	// approval still has to know which machine it is waiting on.
	for _, probe := range []struct {
		name  string
		query string
		state string
	}{
		{"approved", "/?deviceToken=" + approvedToken + "&deviceName=desktop", "approved"},
		{"parked", "/?deviceName=phone", "waiting"},
		{"rejected", "/?deviceToken=not-a-real-token&deviceName=phone", "rejected"},
	} {
		client := dialWSPath(t, server.URL, probe.query, "aWRlbnRpdHktcHJvYmUtMDA=")
		event := readEvent(t, client)
		client.conn.Close()
		if event.Channel != "lan:access-state" || event.Payload["state"] != probe.state {
			t.Fatalf("%s: first event = %+v", probe.name, event)
		}
		if event.Payload["machineId"] != "m-phone-test" {
			t.Fatalf("%s: machineId = %v, want the machine that answered", probe.name, event.Payload["machineId"])
		}
		instance, _ := event.Payload["instanceId"].(string)
		if instance != hub.InstanceID() || instance == "" {
			t.Fatalf("%s: instanceId = %q, want this daemon run's id", probe.name, instance)
		}
	}

	// The point of the instance id: a second daemon is a different one, so a
	// client that reconnects into a restarted daemon can tell.
	other := NewHub(Options{Lease: manager, TrustLocalhost: false})
	if other.InstanceID() == hub.InstanceID() || other.InstanceID() == "" {
		t.Fatalf("two daemon runs share an instance id: %q", other.InstanceID())
	}
}
