package main

import (
	"runtime"
	"sort"
	"strings"

	"workass/internal/acp"
	"workass/internal/machineid"
)

// daemonWireVersion is the version of the invoke/reply/event contract this
// daemon speaks. A client that cannot speak it refuses the machine by name
// instead of half-understanding its payloads; bump this only when the contract
// changes in a way an older peer cannot read.
const daemonWireVersion = 1

// profileEnvVar names the environment profile a daemon was launched under.
// config/environments/*.env set it; a hand-launched daemon has none, and then
// the field is omitted rather than guessed.
const profileEnvVar = "WORKASS_PROFILE"

// daemonIdentity builds the machine identity merged into /workass/health: who
// this daemon is, what it speaks, where it listens, and what it can actually
// run. Everything here is answerable before a client has paired, so it stays
// free of anything a stranger should not read.
func daemonIdentity(identity machineid.Identity, profile, bind string, port int, manager *acp.Manager, keys fleetKeyIDs, certFingerprint string) map[string]any {
	doc := map[string]any{
		"wireVersion": daemonWireVersion,
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
		"bind":        bind,
		"port":        port,
		// True once this daemon serves its own certificate (E5). Stated on the
		// wire so a client badges an unencrypted machine rather than quietly
		// trusting it — and it decides ws:// vs wss:// on the client, so an
		// encrypted machine needs no client change at all.
		"secure":    strings.TrimSpace(certFingerprint) != "",
		"providers": spawnableProviderIDs(manager),
	}
	if profile = strings.TrimSpace(profile); profile != "" {
		doc["profile"] = profile
	}
	if identity.MachineID != "" {
		doc["machineId"] = identity.MachineID
		doc["displayName"] = identity.DisplayName
		doc["name"] = identity.DisplayName
	}
	// The certificate a client should expect to be talking to. Public because it
	// is a hash of a public certificate — and because a client holding the fleet
	// key can go further and VERIFY it (fleet.CertProof), rather than trusting
	// whichever one it happened to see first.
	if fingerprint := strings.TrimSpace(certFingerprint); fingerprint != "" {
		doc["certFingerprint"] = fingerprint
	}
	// Which fleets this machine will accept an enrolment for. The ids are hashes,
	// never the key, and they are the only reason a client can tell "one of mine,
	// enrol silently" from "someone else's machine on this network".
	if keys != nil {
		if ids := keys.KeyIDs(); len(ids) > 0 {
			doc["fleetIds"] = ids
		}
	}
	return doc
}

// fleetKeyIDs is the sliver of the fleet store this document needs. Taking the
// interface rather than the store keeps the secret out of reach of anything that
// builds a public document.
type fleetKeyIDs interface {
	KeyIDs() []string
}

// spawnableProviderIDs lists the providers this daemon could actually start, so
// a client deciding where to open a chat learns the answer before it tries.
// Disabled providers are omitted: on this machine they cannot be spawned, and a
// list that overstates itself is worse than a short one.
func spawnableProviderIDs(manager *acp.Manager) []string {
	if manager == nil {
		return []string{}
	}
	return filterSpawnableProviderIDs(manager.ProvidersList())
}

func filterSpawnableProviderIDs(providers []map[string]any) []string {
	ids := []string{}
	for _, provider := range providers {
		if enabled, _ := provider["enabled"].(bool); !enabled {
			continue
		}
		id, _ := provider["id"].(string)
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}
