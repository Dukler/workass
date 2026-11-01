package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/machinebook"
	"workass/internal/machineid"
	"workass/internal/wire"
)

func machineTestHub(t *testing.T, selfID string) *wire.Hub {
	t.Helper()
	book, err := machinebook.Open(machinebook.Options{StateDir: t.TempDir(), SelfID: selfID, WireVersion: daemonWireVersion})
	if err != nil {
		t.Fatalf("open book: %v", err)
	}
	hub := wire.NewHub()
	identity := machineid.Identity{MachineID: selfID, DisplayName: "this mac"}
	if got := registerMachineHandlers(hub, book, identity); got != machineChannelCount {
		t.Fatalf("registered %d channels, want %d", got, machineChannelCount)
	}
	return hub
}

func fakeDaemonServer(t *testing.T, machineID, name string) string {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != machinebook.HealthPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app":             "workass",
			"version":         daemonVersion,
			"name":            name,
			"displayName":     name,
			"machineId":       machineID,
			"wireVersion":     daemonWireVersion,
			"secure":          true,
			"certFingerprint": "test-certificate-fingerprint",
		})
	}))
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "https://")
}

func invokeMap(t *testing.T, hub *wire.Hub, channel string, args []any) map[string]any {
	t.Helper()
	raw, err := hub.Invoke(channel, args)
	if err != nil {
		t.Fatalf("%s: %v", channel, err)
	}
	result, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("%s returned %T, want a map", channel, raw)
	}
	return result
}

func TestMachinesListStartsWithOnlySelf(t *testing.T) {
	hub := machineTestHub(t, "m-self")

	result := invokeMap(t, hub, "machines:list", nil)
	machines, ok := result["machines"].([]machinebook.Entry)
	if !ok {
		t.Fatalf("machines = %T", result["machines"])
	}
	if len(machines) != 0 {
		t.Fatalf("a fresh book already has %d machines", len(machines))
	}
	// The client's united list has to render the machine it is talking to as
	// well as the ones it learned about, so self rides along with the book.
	self, ok := result["self"].(map[string]any)
	if !ok || self["machineId"] != "m-self" {
		t.Fatalf("self = %+v", result["self"])
	}
}

func TestMachinesAddAndForget(t *testing.T) {
	hub := machineTestHub(t, "m-self")
	address := fakeDaemonServer(t, "m-remote", "builder")

	added := invokeMap(t, hub, "machines:add", []any{map[string]any{"address": address}})
	if added["ok"] != true {
		t.Fatalf("add failed: %+v", added)
	}
	entry, ok := added["machine"].(machinebook.Entry)
	if !ok || entry.MachineID != "m-remote" || entry.Name != "builder" {
		t.Fatalf("machine = %+v", added["machine"])
	}
	if entry.Status != machinebook.StatusOK {
		t.Fatalf("status = %q", entry.Status)
	}

	listed := invokeMap(t, hub, "machines:list", nil)
	if machines := listed["machines"].([]machinebook.Entry); len(machines) != 1 {
		t.Fatalf("listed %d machines after add", len(machines))
	}

	forgotten := invokeMap(t, hub, "machines:forget", []any{map[string]any{"machineId": "m-remote"}})
	if forgotten["ok"] != true {
		t.Fatalf("forget failed: %+v", forgotten)
	}
	if machines := forgotten["machines"].([]machinebook.Entry); len(machines) != 0 {
		t.Fatalf("still listing %d machines after forget", len(machines))
	}
}

func TestMachinesNicknameIsPersistedInTheControllerBook(t *testing.T) {
	hub := machineTestHub(t, "m-self")
	address := fakeDaemonServer(t, "m-remote", "builder-hostname")
	if added := invokeMap(t, hub, "machines:add", []any{map[string]any{"address": address}}); added["ok"] != true {
		t.Fatalf("add failed: %+v", added)
	}

	renamed := invokeMap(t, hub, "machines:nickname", []any{map[string]any{
		"machineId": "m-remote",
		"nickname":  "  Taller  ",
	}})
	if renamed["ok"] != true {
		t.Fatalf("nickname failed: %+v", renamed)
	}
	entry, ok := renamed["machine"].(machinebook.Entry)
	if !ok || entry.Name != "builder-hostname" || entry.Nickname != "Taller" {
		t.Fatalf("renamed machine = %+v", renamed["machine"])
	}
	machines := renamed["machines"].([]machinebook.Entry)
	if len(machines) != 1 || machines[0].Nickname != "Taller" {
		t.Fatalf("nickname missing from snapshot: %+v", machines)
	}

	missing := invokeMap(t, hub, "machines:nickname", []any{map[string]any{
		"machineId": "m-gone",
		"nickname":  "Desk",
	}})
	if missing["ok"] != false || !strings.Contains(missing["error"].(string), "no longer") {
		t.Fatalf("missing machine nickname = %+v", missing)
	}
}

// A typed address that does not work is the user's problem to see and fix, so
// it comes back beside the field as a result — never as a wire error.
func TestMachinesAddReportsFailuresAsResults(t *testing.T) {
	hub := machineTestHub(t, "m-self")
	stranger := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>not me</html>"))
	}))
	defer stranger.Close()
	self := fakeDaemonServer(t, "m-self", "this mac")

	cases := []struct {
		name    string
		address string
		want    string
	}{
		{"empty", "  ", "type an address"},
		{"stranger", strings.TrimPrefix(stranger.URL, "https://"), "not a workass daemon"},
		{"self", self, "this machine"},
		{"dead", "127.0.0.1:1", "refused the connection"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := invokeMap(t, hub, "machines:add", []any{map[string]any{"address": tc.address}})
			if result["ok"] != false {
				t.Fatalf("add(%q) reported ok: %+v", tc.address, result)
			}
			message, _ := result["error"].(string)
			if !strings.Contains(message, tc.want) {
				t.Fatalf("add(%q) said %q, want it to mention %q", tc.address, message, tc.want)
			}
		})
	}
}

// A daemon that cannot prove who it is gets no book: it could not tell another
// machine from itself, and nothing it wrote down would name anything stable.
func TestOpenMachineBookNeedsAnIdentity(t *testing.T) {
	logger := log.New(new(strings.Builder), "", 0)
	if book := openMachineBook(t.TempDir(), machineid.Identity{}, logger); book != nil {
		t.Fatal("opened a book for a daemon with no machine id")
	}
	book := openMachineBook(t.TempDir(), machineid.Identity{MachineID: "m-self", DisplayName: "mac"}, logger)
	if book == nil {
		t.Fatal("no book for a daemon that has an identity")
	}
}

func TestSweepPolicyLeavesWindowsExactlyAsItWas(t *testing.T) {
	book, err := machinebook.Open(machinebook.Options{StateDir: t.TempDir(), SelfID: "m-self", WireVersion: 1})
	if err != nil {
		t.Fatalf("open book: %v", err)
	}
	windows := newMachineDiscovery(book, "windows", nil, nil)
	if windows.scanner.Gate != nil || len(windows.scanner.Ports) != 0 {
		t.Fatalf("windows policy changed: gate=%v ports=%v", windows.scanner.Gate != nil, windows.scanner.Ports)
	}
	mac := newMachineDiscovery(book, "darwin", func() bool { return false }, nil)
	if mac.scanner.Gate == nil || len(mac.scanner.Ports) != 2 {
		t.Fatalf("mac policy: gate=%v ports=%v", mac.scanner.Gate != nil, mac.scanner.Ports)
	}
	if mac.scanner.Gate(context.Background()) {
		t.Fatal("an install with no key and no known machine must not sweep by itself")
	}
	joined := newMachineDiscovery(book, "linux", func() bool { return true }, nil)
	if !joined.scanner.Gate(context.Background()) {
		t.Fatal("a machine holding a fleet key and knowing nobody must look for its fleet")
	}
}

func TestDiscoverMachinesToolReportsWhatTheSweepAdded(t *testing.T) {
	address := fakeDaemonServer(t, "m-found", "found")
	book, err := machinebook.Open(machinebook.Options{StateDir: t.TempDir(), SelfID: "m-self", WireVersion: 1,
		HTTPClient: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, address)
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}}})
	if err != nil {
		t.Fatalf("open book: %v", err)
	}
	discovery := newMachineDiscovery(book, "darwin", nil, nil)
	discovery.scanner.Candidates = []string{"192.168.50.20:8788"}
	discovery.scanner.ProbeTimeout = time.Second

	first := discovery.discoverMachines(context.Background())
	found, _ := first["newly_found"].([]map[string]any)
	if len(found) != 1 || found[0]["machine_id"] != "m-found" || found[0]["address"] != "192.168.50.20:8788" {
		t.Fatalf("first discovery: %#v", first)
	}
	second := discovery.discoverMachines(context.Background())
	again, _ := second["newly_found"].([]map[string]any)
	machines, _ := second["machines"].([]map[string]any)
	if len(again) != 0 || len(machines) != 1 {
		t.Fatalf("second discovery must report a known machine as known: %#v", second)
	}
}

func TestDiscoverMachinesToolAnswersItsOwnerAndNobodyElse(t *testing.T) {
	root := repoRoot(t)
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: filepath.Join(t.TempDir(), "state"),
		Provider:          acp.ProviderConfig{ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}},
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, store, manager.StateDir())
	if _, err := runtime.CreateRendererChat(map[string]any{
		"operationId": "test:create-discovery-parent",
		"tabId":       "discovery-tab", "chatId": "discovery-chat",
		"title": "Discovery parent", "cwd": root, "providerId": "mock",
		"currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := manager.NewSession(ctx, acp.SessionOptions{
		ChatID: "discovery-chat", TabID: "discovery-tab", CWD: root, AgentOwnerKey: "discovery-owner",
	}); err != nil {
		t.Fatalf("new session: %v", err)
	}
	address := fakeDaemonServer(t, "m-found", "found")
	book, err := machinebook.Open(machinebook.Options{StateDir: t.TempDir(), SelfID: "m-self", WireVersion: 1,
		HTTPClient: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, address)
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}}})
	if err != nil {
		t.Fatalf("open book: %v", err)
	}
	discovery := newMachineDiscovery(book, "darwin", nil, nil)
	discovery.scanner.Candidates = []string{"192.168.50.20:8788"}
	discovery.scanner.ProbeTimeout = time.Second
	handler := &agentControlHandler{manager: manager, chats: newChatControlCoordinator(manager, nil, runtime), machineDiscovery: discovery}
	request := httptest.NewRequest(http.MethodPost, toolsPath, nil)
	call := func(owner string) (any, error) {
		return handler.call(request, agentControlRequest{Method: "machines.discover", Params: map[string]any{
			"owner_key": owner, "parent_chat_id": "discovery-chat", "parent_tab_id": "discovery-tab",
		}})
	}
	if _, err := call("someone-else"); err == nil {
		t.Fatal("a caller that owns no chat ran a network sweep")
	}
	if entries := book.List(); len(entries) != 0 {
		t.Fatalf("a refused caller still swept: %#v", entries)
	}
	result, err := call("discovery-owner")
	if err != nil {
		t.Fatalf("owned call: %v", err)
	}
	found, _ := mapFromAnyMain(result)["newly_found"].([]map[string]any)
	if len(found) != 1 || found[0]["machine_id"] != "m-found" {
		t.Fatalf("tool result: %#v", result)
	}
	handler.machineDiscovery = nil
	if _, err := call("discovery-owner"); err == nil || !strings.Contains(err.Error(), "machine book") {
		t.Fatalf("no book must say so, got %v", err)
	}
}
