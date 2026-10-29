package wire

import (
	"errors"
	"strings"
	"testing"
	"time"

	"workass/internal/lease"
)

// The receiving side's half of a peer link: one record per linked daemon,
// listable and revocable on its own, and a revocation that outlives the socket.

func TestPeerLinksAreListedAndRevokedOneAtATime(t *testing.T) {
	rig := fleetAdminHub(t)
	hub, manager := rig.hub, rig.manager

	for _, peer := range []string{"m-first", "m-second"} {
		if _, err := manager.EnrollPeerDevice("daemon "+peer, "192.168.0.9", "token-"+peer, "local", "key-1", peer, ""); err != nil {
			t.Fatalf("enrol %s: %v", peer, err)
		}
	}
	listed := hub.peerLinksPayload()
	rows, _ := listed["links"].([]any)
	if len(rows) != 2 {
		t.Fatalf("fleet:links shows %d links, want two: %#v", len(rows), listed)
	}
	// Empty means full, which is what fleet membership has always meant. The
	// listing says so rather than leaving a blank where a permission belongs.
	if grant := stringField(rows[0].(map[string]any), "grant"); grant != "full" {
		t.Fatalf("an ungranted link lists its grant as %q", grant)
	}

	found, err := hub.RevokePeerLink("m-first", "test")
	if err != nil || !found {
		t.Fatalf("revoke one link: found=%v err=%v", found, err)
	}
	remaining := manager.PeerLinks()
	if len(remaining) != 1 || remaining[0].Peer != "m-second" {
		t.Fatalf("revoking one link left %#v", remaining)
	}
	// A revoked peer stays revoked. The fleet key is deliberately untouched, so
	// without this the peer would re-enrol itself on its next dial and quietly
	// undo the decision.
	if _, err := manager.EnrollPeerDevice("daemon m-first", "192.168.0.9", "token-again", "local", "key-1", "m-first", ""); !errors.Is(err, lease.ErrPeerRevoked) {
		t.Fatalf("a revoked peer re-enrolled: %v", err)
	}
	if _, err := manager.EnrollPeerDevice("daemon m-third", "192.168.0.9", "token-third", "local", "key-1", "m-third", ""); err != nil {
		t.Fatalf("revoking one peer blocked another: %v", err)
	}
	lifted, err := manager.AllowPeer("m-first")
	if err != nil || !lifted {
		t.Fatalf("re-approve one link: lifted=%v err=%v", lifted, err)
	}
	if _, err := manager.EnrollPeerDevice("daemon m-first", "192.168.0.9", "token-again", "local", "key-1", "m-first", ""); err != nil {
		t.Fatalf("a re-approved peer could not link: %v", err)
	}
}

// Revocation is never silence. Every watcher is told link_revoked on the same
// broadcast, with the peer named, so nothing has to infer a cut link from a
// socket that stopped answering.
func TestRevokingALinkTellsEveryWatcher(t *testing.T) {
	rig := fleetAdminHub(t)
	hub, manager, server, controllerToken := rig.hub, rig.manager, rig.server, rig.controllerToken
	if _, err := manager.EnrollPeerDevice("daemon m-peer", "192.168.0.9", "token-peer", "local", "key-1", "m-peer", ""); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	watcher := dialWSPath(t, server.URL, "/?deviceToken="+controllerToken+"&deviceName=mac", "cGVlci1saW5rLXdhdGNoZXIxMg==")
	defer watcher.conn.Close()
	if approved := readEvent(t, watcher); approved.Payload["state"] != "approved" {
		t.Fatalf("controller access event = %+v", approved)
	}

	observed := make(chan PeerLinkEvent, 1)
	hub.SetPeerLinks(func(event PeerLinkEvent) { observed <- event })
	if _, err := hub.RevokePeerLink("m-peer", "test"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	announced := readEventOn(t, watcher, "fleet:link-revoked")
	if announced.Payload["event"] != "link_revoked" || announced.Payload["peer"] != "m-peer" {
		t.Fatalf("link_revoked payload = %+v", announced.Payload)
	}
	select {
	case event := <-observed:
		if event.Kind != "revoked" || event.Peer != "m-peer" {
			t.Fatalf("the daemon was told %#v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the daemon was never told its own link was revoked, so its outbound socket would survive the decision")
	}
}

// A peer link reuses the device store, and everything downstream of it cannot
// tell a daemon from a browser — except where it must: the kind, the peer id,
// and the revocation that is keyed by machine rather than by device.
func TestAPeerLinkIsADeviceOfKindPeer(t *testing.T) {
	manager, _, _ := testLeaseManager(t)
	browser, err := manager.EnrollDevice("phone", "192.168.0.5", "token-phone", "local", "key-1")
	if err != nil {
		t.Fatalf("enrol a browser: %v", err)
	}
	daemon, err := manager.EnrollPeerDevice("daemon m-peer", "192.168.0.9", "token-peer", "local", "key-1", "m-peer", "edit")
	if err != nil {
		t.Fatalf("enrol a peer: %v", err)
	}
	if browser.IsPeer() {
		t.Fatalf("a browser was recorded as a peer: %#v", browser)
	}
	if !daemon.IsPeer() || daemon.Peer != "m-peer" || daemon.Grant != "edit" {
		t.Fatalf("peer record = %#v", daemon)
	}
	if _, ok := manager.AuthenticateToken("token-peer", "192.168.0.9"); !ok {
		t.Fatal("a peer link does not authenticate like every other device")
	}
	// Revoking the peer leaves the browser alone, and vice versa.
	if _, found, err := manager.RevokePeerLink("m-peer"); err != nil || !found {
		t.Fatalf("revoke the peer: found=%v err=%v", found, err)
	}
	if _, ok := manager.Device(browser.ID); !ok {
		t.Fatal("revoking a peer link removed a browser")
	}
	if links := manager.PeerLinks(); len(links) != 0 {
		t.Fatalf("peer links after revocation = %#v", links)
	}
}

// The heartbeat is the cheapest authenticated round trip there is, and it
// refuses to answer a socket that claims to be a different peer than its record
// says: a link that answered both would be a link to nobody in particular.
func TestHeartbeatRefusesAMismatchedPeerClaim(t *testing.T) {
	rig := fleetAdminHub(t)
	hub, manager, server := rig.hub, rig.manager, rig.server
	if _, err := manager.EnrollPeerDevice("daemon m-peer", "127.0.0.1", "token-peer", "local", "key-1", "m-peer", ""); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	link := dialWSPath(t, server.URL, "/?deviceToken=token-peer&deviceName=daemon", "cGVlci1saW5rLWJlYXQxMjM0NQ==")
	defer link.conn.Close()
	if approved := readEvent(t, link); approved.Payload["state"] != "approved" {
		t.Fatalf("peer access event = %+v", approved)
	}

	link.sendText(t, `{"t":"invoke","id":1,"channel":"`+PeerLinkChannel+`","args":[{"machineId":"m-peer"}]}`)
	beat := readReply(t, link)
	if beat.Error != nil {
		t.Fatalf("heartbeat refused its own peer: %v", *beat.Error)
	}
	payload, _ := beat.Result.(map[string]any)
	if payload["peer"] != "m-peer" || payload["machineId"] != "m-admin-test" {
		t.Fatalf("heartbeat answered %#v", payload)
	}
	link.sendText(t, `{"t":"invoke","id":2,"channel":"`+PeerLinkChannel+`","args":[{"machineId":"m-somebody-else"}]}`)
	impostor := readReply(t, link)
	if impostor.Error == nil || !strings.Contains(*impostor.Error, "this link belongs to m-peer") {
		t.Fatalf("heartbeat accepted a mismatched peer claim: %#v", impostor)
	}
	_ = hub
}
