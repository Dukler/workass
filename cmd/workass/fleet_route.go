package main

import (
	"strings"

	"workass/internal/fleetid"
)

// Routing by home, for the Agent API edge.
//
// Every Workass tool call that names an entity names it with an id, and until
// now that id could only say WHICH machine in one dialect: the renderer's
// `M~<machine>~<local>` edge tag. The fleet runtime gives every entity a global
// id that carries its home daemon (docs/specs/fleet-runtime.md §3), and routing
// becomes the string comparison in fleetid.Router: home == self takes the
// in-process path this daemon already has.
//
// P1 is local only. There is no mesh and no link yet, so this file does exactly
// two things:
//
//   - accepts a global id anywhere the legacy tag is already accepted, and
//   - resolves a self-homed id — in either dialect — to the bare local id the
//     in-process path has always been given.
//
// Everything else is left byte-identical on purpose. Another home's id is
// handed onward in the legacy tag, which is the "client that predates global
// ids" §3 reserves that tag for: the renderer relay. So no projection, row or
// receipt changes shape, and no caller that only ever spoke local ids notices
// that homes now exist.

// fleetRouter is this daemon's router. It is derived from selfMachineID rather
// than stored beside it, so the daemon keeps one source of truth for its own
// identity (main.go sets it from internal/machineid).
func (h *agentControlHandler) fleetRouter() fleetid.Router {
	if h == nil {
		return fleetid.NewRouter("")
	}
	return fleetid.NewRouter(h.selfMachineID)
}

// fleetAddressParamFields is every Agent API param whose value addresses an
// entity. It is the union of what remoteAgentAddressParams can return, and
// TestFleetAddressParamFieldsCoverEveryAddressingParam holds it to that: a new
// addressing param added to the locality table without being listed here would
// accept a global id and then look it up verbatim, which finds nothing.
//
// Only addressing ids are rewritten. Scanning a whole payload would let message
// text or a model id decide a route — the same reason remoteAgentAddressParams
// is a list and not a heuristic.
var fleetAddressParamFields = []string{
	"tab_id", "chat_id", "fork_point_id", "run_id", "child_id", "work_id", "id", "subagent_id",
}

// resolveFleetAddressedParams rewrites every addressing id into the form the
// rest of this daemon already understands, and reports which home the call
// turned out to be about.
//
// The entity kind is deliberately not asserted per field. §3's type set has no
// `tab`, and `child_id` names a fork child rather than any one kind, so pinning
// a kind here would refuse ids the spec considers well-formed. The home is what
// routing needs; the kind is carried for the reader and checked by the owner of
// the entity, which is the only place that can.
func resolveFleetAddressedParams(router fleetid.Router, params map[string]any) (map[string]any, error) {
	if len(params) == 0 {
		return params, nil
	}
	resolved, copied := params, false
	for _, field := range fleetAddressParamFields {
		value := strings.TrimSpace(fieldString(params, field))
		if value == "" || !fleetid.IsHomed(value) {
			continue
		}
		address, err := router.Address(value, "")
		if err != nil {
			return nil, err
		}
		if address == value {
			continue
		}
		if !copied {
			// The caller's map belongs to the caller: an in-place rewrite would
			// change the params an MCP retry or a receipt still refers to.
			resolved, copied = copyAnyMap(params), true
		}
		resolved[field] = address
	}
	return resolved, nil
}
