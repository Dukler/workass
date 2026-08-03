package main

import (
	"reflect"
	"runtime"
	"testing"

	"workass/internal/machineid"
)

func TestDaemonIdentityDescribesTheMachineAndWhatItSpeaks(t *testing.T) {
	identity := machineid.Identity{MachineID: "m-abc", DisplayName: "Builder"}
	doc := daemonIdentity(identity, "prod", "lan", 80, nil, testFleetIDs{"fleet-one"}, "")

	for key, want := range map[string]any{
		"machineId":   "m-abc",
		"displayName": "Builder",
		"name":        "Builder",
		"profile":     "prod",
		"wireVersion": daemonWireVersion,
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
		"bind":        "lan",
		"port":        80,
		"secure":      false,
	} {
		if got := doc[key]; got != want {
			t.Errorf("identity[%q] = %v, want %v", key, got, want)
		}
	}
	if providers, ok := doc["providers"].([]string); !ok || providers == nil {
		t.Errorf("identity providers should be a present, non-nil list: %#v", doc["providers"])
	}
}

// A daemon that could not read its identity file still serves its own client.
// It must not invent an id, because a client would then pair with something
// that changes name on the next start.
func TestDaemonIdentityOmitsTheIDWhenIdentityIsUnavailable(t *testing.T) {
	doc := daemonIdentity(machineid.Identity{}, "dev", "localhost", 8788, nil, nil, "")
	for _, key := range []string{"machineId", "displayName", "name"} {
		if _, present := doc[key]; present {
			t.Errorf("identity leaked a %q for a daemon with no identity: %v", key, doc[key])
		}
	}
	if doc["wireVersion"] != daemonWireVersion {
		t.Errorf("wire version should still be answerable: %v", doc["wireVersion"])
	}
}

func TestDaemonIdentityOmitsAnUnsetProfileRatherThanGuessing(t *testing.T) {
	doc := daemonIdentity(machineid.Identity{MachineID: "m-abc"}, "   ", "localhost", 8788, nil, testFleetIDs{}, "")
	if _, present := doc["profile"]; present {
		t.Errorf("a hand-launched daemon claimed a profile: %v", doc["profile"])
	}
}

func TestSpawnableProviderIDsListsOnlyEnabledOnesSorted(t *testing.T) {
	if got := spawnableProviderIDs(nil); !reflect.DeepEqual(got, []string{}) {
		t.Fatalf("nil manager should list nothing, got %#v", got)
	}
	got := filterSpawnableProviderIDs([]map[string]any{
		{"id": "qwen", "enabled": true},
		{"id": "devin", "enabled": false},
		{"id": "claude", "enabled": true},
		{"id": "  ", "enabled": true},
		{"enabled": true},
		{"id": "codex"},
	})
	if want := []string{"claude", "qwen"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("spawnable providers = %#v, want %#v", got, want)
	}
}

// testFleetIDs stands in for the fleet store: the document only ever needs the
// public ids, which is why it takes an interface and not the store.
type testFleetIDs []string

func (ids testFleetIDs) KeyIDs() []string { return ids }

// A machine advertises which fleets it will accept an enrolment for, and that
// has to survive the public-card filter — it is how a client tells one of its
// own machines from a stranger's on the same network.
func TestIdentityAdvertisesFleetIDsOnlyWhenItHasThem(t *testing.T) {
	withKeys := daemonIdentity(machineid.Identity{MachineID: "m-abc"}, "dev", "lan", 8788, nil, testFleetIDs{"a1b2", "c3d4"}, "")
	ids, ok := withKeys["fleetIds"].([]string)
	if !ok || len(ids) != 2 || ids[0] != "a1b2" {
		t.Fatalf("fleetIds = %#v", withKeys["fleetIds"])
	}
	without := daemonIdentity(machineid.Identity{MachineID: "m-abc"}, "dev", "lan", 8788, nil, testFleetIDs{}, "")
	if _, present := without["fleetIds"]; present {
		t.Fatalf("a machine with no fleet key advertised one: %#v", without)
	}
}

// E5: the badge has to be true. A daemon serving plaintext must say so, and one
// serving its own certificate must name the certificate a client should expect,
// because no chain to an IP address can ever be validated the ordinary way.
func TestIdentityReportsSecureOnlyWithACertificate(t *testing.T) {
	plain := daemonIdentity(machineid.Identity{MachineID: "m-abc"}, "dev", "lan", 8788, nil, nil, "")
	if plain["secure"] != false {
		t.Fatalf("plaintext daemon reported secure=%v", plain["secure"])
	}
	if _, present := plain["certFingerprint"]; present {
		t.Fatalf("plaintext daemon advertised a certificate: %#v", plain)
	}
	secure := daemonIdentity(machineid.Identity{MachineID: "m-abc"}, "dev", "lan", 8788, nil, nil, "ab12")
	if secure["secure"] != true || secure["certFingerprint"] != "ab12" {
		t.Fatalf("tls daemon identity = %#v", secure)
	}
}
