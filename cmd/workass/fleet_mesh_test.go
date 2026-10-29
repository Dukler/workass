package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workass/internal/fleet"
	"workass/internal/fleetclient"
	"workass/internal/httpserve"
	"workass/internal/lease"
	"workass/internal/tlscert"
	"workass/internal/wire"
)

// The operator-visible claim of P2, tested the only way it can be: two daemons
// and no renderer process anywhere.
//
// Until now the Electron app was the fleet's only bridge — the daemon could not
// dial, so a remote tool call was broadcast to the renderer, which forwarded it
// over a socket it owned. Closing the app closed the fleet. These tests stand up
// a real receiving daemon (real hub, real device store, real fleet key, real TLS,
// real Agent API control surface) and drive it from a real fleetclient mesh,
// with a relay that FAILS THE TEST if it is used.

// refusingRelay stands in for the renderer that is not running. Any call through
// it is a test failure, which is how "with no renderer anywhere" is asserted
// rather than asserted about.
type refusingRelay struct {
	t      *testing.T
	calls  atomic.Int64
	result any
	err    error
	allow  bool
}

func (r *refusingRelay) Call(ctx context.Context, method string, params map[string]any) (any, error) {
	r.calls.Add(1)
	if !r.allow {
		r.t.Errorf("the renderer relay was used for %s; this path must not need a renderer", method)
		return nil, errors.New("no renderer")
	}
	return r.result, r.err
}

// meshHome serves one harness's Agent API control surface to the fleet.
type meshHome struct {
	machineID string
	address   string
	hub       *wire.Hub
	lease     *lease.Manager
	keys      *fleet.Store
	server    *http.Server
	listener  net.Listener
}

func serveMeshHome(t *testing.T, machineID, secret string, control *agentControlHandler) *meshHome {
	t.Helper()
	stateDir := t.TempDir()
	certificate, err := tlscert.Ensure(stateDir)
	if err != nil {
		t.Fatalf("home certificate: %v", err)
	}
	leases, err := lease.NewManager(lease.Options{StateDir: stateDir, Logf: t.Logf})
	if err != nil {
		t.Fatalf("home device store: %v", err)
	}
	keys, err := fleet.Open(stateDir)
	if err != nil {
		t.Fatalf("home fleet store: %v", err)
	}
	if _, err := keys.Join(secret, fleet.LocalOwner, "test"); err != nil {
		t.Fatalf("home join fleet: %v", err)
	}
	hub := wire.NewHub(wire.Options{Lease: leases, TrustLocalhost: false, Logf: t.Logf})
	hub.SetMachineID(machineID)
	hub.SetFleet(keys, machineID)
	hub.SetTLSFingerprint(certificate.Fingerprint)
	if !registerAgentControlWireHandler(hub, control) {
		t.Fatal("the home did not register its agent control channel")
	}
	server := httpserve.New("", hub, nil)
	server.Version, server.Name = "test", machineID
	server.Identity = func() map[string]any {
		return map[string]any{
			"machineId": machineID, "displayName": machineID, "name": machineID,
			"wireVersion": daemonWireVersion, "secure": true,
			"certFingerprint": certificate.Fingerprint, "fleetIds": keys.KeyIDs(),
		}
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("home listen: %v", err)
	}
	home := &meshHome{
		machineID: machineID, address: raw.Addr().String(), hub: hub, lease: leases, keys: keys,
		listener: tls.NewListener(raw, &tls.Config{
			Certificates: []tls.Certificate{certificate.TLS}, MinVersion: tls.VersionTLS13,
		}),
	}
	home.server = &http.Server{Handler: server, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = home.server.Serve(home.listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = home.server.Shutdown(ctx)
	})
	return home
}

func newMeshDialer(t *testing.T, machineID, secret string) *fleetclient.Mesh {
	t.Helper()
	stateDir := t.TempDir()
	store, err := fleetclient.Open(stateDir)
	if err != nil {
		t.Fatalf("peer store: %v", err)
	}
	keys, err := fleet.Open(stateDir)
	if err != nil {
		t.Fatalf("fleet store: %v", err)
	}
	if _, err := keys.Join(secret, fleet.LocalOwner, "test"); err != nil {
		t.Fatalf("join fleet: %v", err)
	}
	mesh := fleetclient.New(fleetclient.Options{
		SelfMachineID: machineID, SelfName: machineID, Store: store, Keys: keys, Logf: t.Logf,
		Heartbeat: time.Minute, DialTimeout: 10 * time.Second, InvokeTimeout: 20 * time.Second,
	})
	t.Cleanup(mesh.Close)
	return mesh
}

func newFleetTestSecret(t *testing.T) string {
	t.Helper()
	secret, err := fleet.NewSecret()
	if err != nil {
		t.Fatalf("mint a fleet key: %v", err)
	}
	return secret
}

// The phase's whole point: a tool call reaches a chat on another machine with
// the app closed on both ends.
func TestAToolCallReachesAnotherHomeWithNoRendererAnywhere(t *testing.T) {
	secret := newFleetTestSecret(t)
	harness := newStatelessMCPTestHarness(t)
	home := serveMeshHome(t, "m-home", secret, harness.handler.agentControl)

	mesh := newMeshDialer(t, "m-away", secret)
	if err := mesh.AddRoute(home.machineID, "home", fleetclient.KindLAN, home.address, fleetclient.SourceManual); err != nil {
		t.Fatalf("add route: %v", err)
	}
	relay := &refusingRelay{t: t}
	router := newFleetMeshRouter(mesh, relay, "m-away", t.Logf)

	read, err := router.Call(context.Background(), "chat.read", map[string]any{
		"machine_id": home.machineID, "tab_id": "mcp-tab", "chat_id": "mcp-chat", "limit": 1,
	})
	if err != nil {
		t.Fatalf("read a chat on another home: %v", err)
	}
	if chatID := fieldString(mapFromAnyMain(read), "chatId"); chatID != "mcp-chat" {
		t.Fatalf("the home answered %#v", read)
	}
	if relay.calls.Load() != 0 {
		t.Fatalf("the renderer relay was used %d time(s)", relay.calls.Load())
	}
	// The receiving side recorded exactly one revocable pairing record for the
	// machine that called it — no human was asked anything.
	links := home.lease.PeerLinks()
	if len(links) != 1 || links[0].Peer != "m-away" {
		t.Fatalf("the home's peer links = %#v", links)
	}

	// A mutation travels the same way, through the same authorization.
	renamed, err := router.Call(context.Background(), "chat.rename", map[string]any{
		"machine_id": home.machineID, "tab_id": "mcp-tab", "chat_id": "mcp-chat",
		"title": "renamed from another machine", "operation_id": "mesh-rename-1",
	})
	if err != nil {
		t.Fatalf("rename a chat on another home: %v", err)
	}
	if !boolFieldValue(mapFromAnyMain(renamed), "ok") {
		t.Fatalf("remote rename = %#v", renamed)
	}
	// One operation id, replayed: exactly one effect, because the caller's key
	// travels with the command instead of being minted again at the home (§6).
	replay, err := router.Call(context.Background(), "chat.rename", map[string]any{
		"machine_id": home.machineID, "tab_id": "mcp-tab", "chat_id": "mcp-chat",
		"title": "renamed from another machine", "operation_id": "mesh-rename-1",
	})
	if err != nil {
		t.Fatalf("replay a remote rename: %v", err)
	}
	if !boolFieldValue(mapFromAnyMain(replay), "ok") {
		t.Fatalf("replayed remote rename = %#v", replay)
	}
	after, err := router.Call(context.Background(), "chat.read", map[string]any{
		"machine_id": home.machineID, "tab_id": "mcp-tab", "chat_id": "mcp-chat", "limit": 1,
	})
	if err != nil {
		t.Fatalf("read after replay: %v", err)
	}
	if title := fieldString(mapFromAnyMain(after), "title"); title != "renamed from another machine" {
		t.Fatalf("title after one replayed operation = %q", title)
	}
}

// A peer with no mesh is reached through the relay, which is what keeps a mixed
// fleet working as a steady state rather than as a migration window (§12).
func TestAnUnlinkedPeerStillGoesThroughTheRelay(t *testing.T) {
	secret := newFleetTestSecret(t)
	mesh := newMeshDialer(t, "m-away", secret)
	relay := &refusingRelay{t: t, allow: true, result: map[string]any{"ok": true, "via": "relay"}}
	router := newFleetMeshRouter(mesh, relay, "m-away", t.Logf)

	result, err := router.Call(context.Background(), "chat.rename", map[string]any{
		"machine_id": "m-old-peer", "tab_id": "tab", "chat_id": "chat", "title": "x",
	})
	if err != nil {
		t.Fatalf("relay fallback: %v", err)
	}
	if fieldString(mapFromAnyMain(result), "via") != "relay" {
		t.Fatalf("fallback answered %#v", result)
	}
	if relay.calls.Load() != 1 {
		t.Fatalf("the relay was called %d time(s)", relay.calls.Load())
	}
}

// A command that REACHED the home and failed there is never sent again by
// another transport. Retrying it would run it twice, which is exactly the
// "launch there, record here" failure the one operation id exists to end.
func TestACommandThatReachedTheHomeIsNeverRelayedAgain(t *testing.T) {
	secret := newFleetTestSecret(t)
	harness := newStatelessMCPTestHarness(t)
	home := serveMeshHome(t, "m-home", secret, harness.handler.agentControl)
	mesh := newMeshDialer(t, "m-away", secret)
	if err := mesh.AddRoute(home.machineID, "home", fleetclient.KindLAN, home.address, fleetclient.SourceManual); err != nil {
		t.Fatalf("add route: %v", err)
	}
	relay := &refusingRelay{t: t}
	router := newFleetMeshRouter(mesh, relay, "m-away", t.Logf)

	_, err := router.Call(context.Background(), "chat.rename", map[string]any{
		"machine_id": home.machineID, "tab_id": "no-such-tab", "chat_id": "no-such-chat", "title": "x",
	})
	if err == nil {
		t.Fatal("renaming a chat that does not exist on the home succeeded")
	}
	if relay.calls.Load() != 0 {
		t.Fatalf("a command the home already refused was relayed again %d time(s)", relay.calls.Load())
	}
}

// Narrowing, the no-chaining rule, and refusals that name the capability.
func TestForwardedLimitsNarrowAndNameWhatIsMissing(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	control := harness.handler.agentControl
	control.selfMachineID = "m-home"
	ctx := context.Background()

	// A limit wider than the link's grant is narrowed to the grant: the caller
	// claims full, the link was given edit, and the effective permission is edit
	// — so a workspace change is refused by name.
	_, err := control.callAsControllerFor(ctx, wire.Caller{Peer: "m-away", Grant: "edit"}, map[string]any{
		"v": 1, "method": "chat.configure", "limit": "full",
		"params": map[string]any{
			"tab_id": "mcp-tab", "chat_id": "mcp-chat", "cwd": "/tmp", "operation_id": "narrowed-configure-1",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "workspace mutation") || !strings.Contains(err.Error(), "edit") {
		t.Fatalf("a wider limit was not narrowed to the link's grant: %v", err)
	}

	// Below full access, the settings/schedule/update surfaces are refused with
	// the capability named rather than with a generic denial (§16.4).
	for method, capability := range map[string]string{
		"schedule.create": "schedule.*",
		"update.apply":    "update.*",
		"chat.delete":     "chat.delete",
	} {
		_, err := control.callAsControllerFor(ctx, wire.Caller{Peer: "m-away"}, map[string]any{
			"v": 1, "method": method, "limit": "read",
			"params": map[string]any{"tab_id": "mcp-tab", "chat_id": "mcp-chat"},
		})
		if err == nil || !strings.Contains(err.Error(), capability) {
			t.Fatalf("%s under a read limit was refused as %v, which does not name %s", method, err, capability)
		}
	}

	// Full access is unchanged: the same command, on the same link, works.
	if _, err := control.callAsControllerFor(ctx, wire.Caller{Peer: "m-away", Grant: "full"}, map[string]any{
		"v": 1, "method": "chat.rename", "limit": "full",
		"params": map[string]any{
			"tab_id": "mcp-tab", "chat_id": "mcp-chat", "title": "full access still works",
			"operation_id": "full-rename-1",
		},
	}); err != nil {
		t.Fatalf("a full-access forwarded command was refused: %v", err)
	}

	// And an envelope from the future is refused by name rather than misread.
	if _, err := control.callAsControllerFor(ctx, wire.Caller{Peer: "m-away"}, map[string]any{
		"v": fleetEnvelopeVersion + 1, "method": "chat.read",
		"params": map[string]any{"tab_id": "mcp-tab", "chat_id": "mcp-chat"},
	}); err == nil || !strings.Contains(err.Error(), "envelope") {
		t.Fatalf("a newer envelope was read anyway: %v", err)
	}
}

// The rule that replaced max_depth: narrowed work may not use the receiving
// daemon's own links, so a grant cannot be widened by a third machine.
func TestNarrowedWorkMayNotTakeASecondHop(t *testing.T) {
	ctx := withFleetForwardedWork(context.Background(), fleetForwardedWork{Peer: "m-away", Limit: "edit"})
	if err := fleetRefuseSecondHop(ctx, "chat.send"); err == nil || !strings.Contains(err.Error(), "third machine") {
		t.Fatalf("narrowed work was allowed a second hop: %v", err)
	}
	full := withFleetForwardedWork(context.Background(), fleetForwardedWork{Peer: "m-away", Limit: "full"})
	if err := fleetRefuseSecondHop(full, "chat.send"); err != nil {
		t.Fatalf("full-access work was refused a second hop: %v", err)
	}
	if err := fleetRefuseSecondHop(context.Background(), "chat.send"); err != nil {
		t.Fatalf("a local call was treated as a forwarded one: %v", err)
	}
}

// min(link grant, caller limit), with silence meaning "not stated" rather than
// either extreme.
func TestFleetEffectiveLimitOnlyEverNarrows(t *testing.T) {
	cases := []struct{ grant, limit, want string }{
		{"", "", "full"},
		{"full", "full", "full"},
		{"edit", "full", "edit"},
		{"full", "edit", "edit"},
		{"read", "edit", "read"},
		{"edit", "read", "read"},
		{"", "read", "read"},
		{"read", "", "read"},
		{"nonsense", "read", "read"},
	}
	for _, test := range cases {
		if got := fleetEffectiveLimit(test.grant, test.limit); got != test.want {
			t.Errorf("fleetEffectiveLimit(%q, %q) = %q, want %q", test.grant, test.limit, got, test.want)
		}
	}
}

// The envelope is additive and derived, never trusted: the home in a target id
// is the machine the router resolved, and the reserved edge params never reach
// a provider.
func TestTheForwardedEnvelopeIsAdditiveAndCarriesNoEdgeParams(t *testing.T) {
	payload := meshControlPayload("m-home", "chat.send", map[string]any{
		"tab_id": "tab-1", "chat_id": "chat-1", "message": "hello",
		"operation_id": "send-1", fleetLimitParam: "edit",
		fleetOriginParam: map[string]any{"machine": "m-away", "chat_id": "chat-away"},
	})
	if payload["method"] != "chat.send" || payload["v"] != fleetEnvelopeVersion {
		t.Fatalf("envelope = %#v", payload)
	}
	if payload["limit"] != "edit" || payload["operation_id"] != "send-1" {
		t.Fatalf("envelope lost its limit or its operation id: %#v", payload)
	}
	if origin := mapFromAnyMain(payload["origin"]); fieldString(origin, "machine") != "m-away" {
		t.Fatalf("envelope origin = %#v", origin)
	}
	params := mapFromAnyMain(payload["params"])
	if params[fleetLimitParam] != nil || params[fleetOriginParam] != nil {
		t.Fatalf("an edge-only param reached the forwarded params: %#v", params)
	}
	if fieldString(params, "message") != "hello" {
		t.Fatalf("the forwarded params lost the command: %#v", params)
	}
	if target := fieldString(payload, "target_id"); target != "wa:m-home/chat/chat-1" {
		t.Fatalf("target id = %q", target)
	}
}

// workass_list_chats across the fleet, with no renderer. This was the one tool
// whose fleet-wide answer the renderer used to assemble from its own sockets,
// so it is the one that proves the daemon no longer needs the app.
func TestTheFleetChatListIsAnsweredOverLinksWithNoRenderer(t *testing.T) {
	secret := newFleetTestSecret(t)
	harness := newStatelessMCPTestHarness(t)
	home := serveMeshHome(t, "m-home", secret, harness.handler.agentControl)
	mesh := newMeshDialer(t, "m-away", secret)
	if err := mesh.AddRoute(home.machineID, "home", fleetclient.KindLAN, home.address, fleetclient.SourceManual); err != nil {
		t.Fatalf("add route: %v", err)
	}
	// A fleet-wide read asks the relay too, because a peer without mesh support
	// is still part of the fleet (§12). With no renderer running that call fails,
	// and the list must still be answered from the links rather than fail with it.
	relay := &refusingRelay{t: t, allow: true, err: errors.New("Workass remote-chat MCP router has no local renderer")}
	router := newFleetMeshRouter(mesh, relay, "m-away", t.Logf)

	listed, err := router.Call(context.Background(), "chat.list", map[string]any{})
	if err != nil {
		t.Fatalf("list the fleet's chats: %v", err)
	}
	rows := anySlice(mapFromAnyMain(listed)["chats"])
	if len(rows) == 0 {
		t.Fatalf("the fleet chat list is empty: %#v", listed)
	}
	found := false
	for _, raw := range rows {
		chat := mapFromAnyMain(raw)
		if fieldString(chat, "machineId") != "m-home" {
			continue
		}
		// Another home's rows arrive named by their home, in the dialect every
		// existing reader of this projection already speaks.
		machineID, _, tagged, err := splitMachineTaggedID(fieldString(chat, "chatId"))
		if err != nil || !tagged || machineID != "m-home" {
			t.Fatalf("remote chat row is not named by its home: %#v (%v)", chat, err)
		}
		found = true
	}
	if !found {
		t.Fatalf("no row from m-home in %#v", rows)
	}
	if relay.calls.Load() != 1 {
		t.Fatalf("the fleet chat list asked the relay %d time(s), want exactly one attempt", relay.calls.Load())
	}

	// And it merges into this machine's own catalog without colliding with it.
	merged := mergeRemoteAgentChatList(map[string]any{"chats": []any{
		map[string]any{"tabId": "local-tab", "chatId": "local-chat"},
	}}, listed)
	if len(anySlice(merged["chats"])) != len(rows)+1 {
		t.Fatalf("merged list = %#v", merged["chats"])
	}
}
