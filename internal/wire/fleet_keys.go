package wire

import (
	"workass/internal/fleet"
)

// The fleet key is the only ceremony the design has — paste it into the next
// machine and that machine is yours (D3). Until now it was readable only by
// running `workass fleet key` in a terminal, against a binary that sits inside
// an application-support directory and is on nobody's PATH. A ceremony you can
// only perform with a shell is not a product, and the Máquinas pane was left
// telling people to run a command they had no way to run.
//
// These four channels put it in the app. The split is the load-bearing part:
//
//   - Listing keys is non-secret — ids, owner, when — so any controller reads it.
//   - Revealing a secret, minting one, or retiring one additionally requires a
//     LOCAL client, so the key still never crosses the network, not even to a
//     device already approved. A phone is a thing you leave in a bar; the fleet
//     key is the one credential that opens every machine.
//
// Controller is checked explicitly rather than through mutatingChannels, for the
// same reason lan:revoke does: acting takes the lease now (f0ec9118), so a
// boundary a device can grant itself is not a boundary.
var fleetAdminChannels = map[string]struct{}{
	"fleet:keys":   {},
	"fleet:reveal": {},
	"fleet:mint":   {},
	"fleet:forget": {},
}

func (h *Hub) handleFleetAdmin(c *client, channel string, args []any) (any, error) {
	store, machineID := h.fleetSnapshot()
	if store == nil {
		return nil, structuredError("fleet:unavailable", "this daemon has no fleet key store", nil)
	}
	local := isLocalIP(c.ip)
	if channel != "fleet:keys" && !local {
		return nil, structuredError("fleet:not-local", "a fleet key is only readable on the machine that holds it", map[string]any{"ip": c.ip})
	}

	switch channel {
	case "fleet:keys":
		return fleetKeysPayload(store, machineID, local), nil

	case "fleet:reveal":
		keyID := stringField(firstMapArg(args), "keyId")
		for _, key := range store.Keys() {
			if keyID != "" && key.KeyID != keyID {
				continue
			}
			// Loud in the record, because reading it is the one moment the secret
			// leaves the daemon and a key read by someone else shows up nowhere else.
			h.logf("[fleet] key %s revealed to %s", key.KeyID, c.ip)
			return map[string]any{
				"keyId": key.KeyID, "secret": key.Secret,
				"owner": key.Owner, "machineId": machineID,
			}, nil
		}
		return nil, structuredError("fleet:unknown-key", "no such key on this machine", nil)

	case "fleet:mint":
		key, minted, err := store.EnsureKey()
		if err != nil {
			return nil, err
		}
		if minted {
			h.logf("[fleet] minted key %s for %s", key.KeyID, c.ip)
			h.broadcastFleetKeys(store, machineID)
		}
		return map[string]any{
			"keyId": key.KeyID, "secret": key.Secret, "owner": key.Owner,
			"minted": minted, "machineId": machineID,
		}, nil

	case "fleet:forget":
		keyID := stringField(firstMapArg(args), "keyId")
		if keyID == "" {
			return nil, structuredError("fleet:malformed", "keyId is required", nil)
		}
		dropped, err := store.Forget(keyID)
		if err != nil {
			return nil, err
		}
		if !dropped {
			return nil, structuredError("fleet:unknown-key", "no such key on this machine", nil)
		}
		// Retiring gates future enrolments only: devices already in keep working,
		// because their tokens were derived once and are independent of the key.
		h.logf("[fleet] key %s retired by %s; devices already enrolled keep working", keyID, c.ip)
		h.broadcastFleetKeys(store, machineID)
		return map[string]any{"ok": true, "keyId": keyID, "remaining": len(store.KeyIDs())}, nil
	}
	return nil, structuredError("fleet:malformed", "unknown fleet channel", nil)
}

// broadcastFleetKeys tells every approved device the set changed. The payload is
// ids only — a broadcast reaches devices that may not read the secret.
func (h *Hub) broadcastFleetKeys(store *fleet.Store, machineID string) {
	h.Broadcast("fleet:keys-changed", map[string]any{
		"machineId": machineID,
		"keyIds":    store.KeyIDs(),
	})
}

func fleetKeysPayload(store *fleet.Store, machineID string, local bool) map[string]any {
	keys := store.Keys()
	rows := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, map[string]any{
			"keyId":     key.KeyID,
			"owner":     key.Owner,
			"label":     key.Label,
			"createdAt": key.CreatedAt,
		})
	}
	return map[string]any{
		"machineId": machineID,
		"keys":      rows,
		// Whether this client may ask for the secret at all, so the app can offer
		// the button or explain instead of failing after the click.
		"canReveal": local,
	}
}
