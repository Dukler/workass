package main

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
