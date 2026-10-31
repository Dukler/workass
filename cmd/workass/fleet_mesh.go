package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"workass/internal/fleetclient"
	"workass/internal/fleetid"
)

// The mesh, at the Agent API edge.
//
// P1 gave every entity a home and made routing a string comparison. This is the
// other branch of that comparison: a command whose home is not this daemon goes
// over a link this daemon dials itself (docs/specs/fleet-runtime.md §4, §6).
// Until now that branch had one implementation — ask the renderer to forward it
// — so with the app closed there was no fleet at all. The renderer relay stays,
// demoted to a fallback for peers that have no mesh yet, because mixed versions
// are a steady state (§12) and not a migration window.
//
// What crosses the link is NOT a second API. It is the agent-control method
// name and its params — exactly what the controller path already rebuilds and
// hands to the ordinary local handler — plus the small fixed envelope §6
// defines. So a new tool reaches the whole fleet the day it exists locally, and
// there is still one place where authorization is written: the home.

// fleetMeshRouter routes one addressed Agent API call to the home that owns it.
type fleetMeshRouter struct {
	mesh  *fleetclient.Mesh
	relay agentChatRemoteRouter
	self  string
	logf  func(string, ...any)
}

// newFleetMeshRouter wraps the renderer relay with the daemon's own links. A
// nil mesh leaves the relay exactly as it was, which is what an installation
// with no fleet key still gets.
func newFleetMeshRouter(mesh *fleetclient.Mesh, relay agentChatRemoteRouter, selfMachineID string, logf func(string, ...any)) *fleetMeshRouter {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &fleetMeshRouter{mesh: mesh, relay: relay, self: strings.TrimSpace(selfMachineID), logf: logf}
}

// Call satisfies agentChatRemoteRouter, so nothing above this file knows which
// transport carried a command.
func (r *fleetMeshRouter) Call(ctx context.Context, method string, params map[string]any) (any, error) {
	if r == nil {
		return nil, errors.New("Workass remote-chat MCP router is unavailable")
	}
	// The mesh carries the agent-control surface and only that. update.targets
	// and update.apply keep their own older machine_id contract, answered by
	// the renderer's transactional updater or by the headless service — never
	// by a peer's agent:control, which would refuse them. Routing them here
	// would break updating another machine from a packaged install, so they go
	// straight to the relay that has always owned them. update.status is
	// carried (2026-10-31): see remoteAgentMethodKinds for why.
	if _, carried := remoteAgentMethodKinds[method]; !carried {
		if r.relay == nil {
			return nil, errors.New("Workass has no local renderer to answer " + method + " for another machine")
		}
		return r.relay.Call(ctx, method, params)
	}
	machineID := strings.TrimSpace(fieldString(params, "machine_id"))
	if machineID == "" || machineID == r.self {
		// A call with no addressee is the fleet-wide read: today that is
		// chat.list, whose answer is the union of every machine's catalog.
		//
		// update.status reaches this branch only through its OWN self-addressed
		// renderer-relay path (answerUpdateLocally's fallback when this machine
		// has no headless updater) — never through forwardRemoteAgentCall, which
		// only ever names a genuinely other machine. It is not a fleet-wide read
		// and must not become one: this machine's own update status is not the
		// union of every peer's.
		if strings.HasPrefix(method, "update.") {
			if r.relay == nil {
				return nil, errors.New("Workass has no local renderer to answer " + method + " for another machine")
			}
			return r.relay.Call(ctx, method, params)
		}
		return r.callEveryPeer(ctx, method, params)
	}
	if r.mesh != nil {
		payload := meshControlPayload(machineID, method, params)
		result, err := r.mesh.Invoke(ctx, machineID, remoteAgentControlChannel, payload)
		if err == nil {
			return result, nil
		}
		// Only a command that never left this machine may be sent again by
		// another route. Anything else already reached the home, where its
		// operation id is what makes a retry a no-op — not a second transport.
		if !errors.Is(err, fleetclient.ErrNotSent) {
			return nil, err
		}
		if r.relay == nil {
			return nil, err
		}
		r.logf("[fleet] no daemon link to %s (%v); falling back to the renderer relay", machineID, err)
	}
	if r.relay == nil {
		return nil, errors.New("Workass has no daemon link to that machine and no local renderer to relay through")
	}
	return r.relay.Call(ctx, method, params)
}

// callEveryPeer answers a fleet-wide read by asking every linked peer and the
// relay, and merging what comes back. A peer that is down contributes nothing
// and fails nothing: the caller already reports a partial fleet honestly.
func (r *fleetMeshRouter) callEveryPeer(ctx context.Context, method string, params map[string]any) (any, error) {
	peers := r.linkedPeers()
	if len(peers) == 0 {
		if r.relay == nil {
			return nil, errors.New("Workass has no daemon link to another machine and no local renderer to relay through")
		}
		return r.relay.Call(ctx, method, params)
	}
	type answer struct {
		peer   string
		result any
		err    error
	}
	answers := make([]answer, len(peers))
	var wait sync.WaitGroup
	for i, peerID := range peers {
		wait.Add(1)
		go func(index int, peerID string) {
			defer wait.Done()
			payload := meshControlPayload(peerID, method, params)
			result, err := r.mesh.Invoke(ctx, peerID, remoteAgentControlChannel, payload)
			answers[index] = answer{peer: peerID, result: result, err: err}
		}(i, peerID)
	}
	wait.Wait()

	merged := map[string]any{}
	chats := make([]any, 0, len(peers))
	reached := 0
	for _, entry := range answers {
		if entry.err != nil {
			r.logf("[fleet] %s did not answer %s: %v", entry.peer, method, entry.err)
			continue
		}
		reached++
		chats = append(chats, homedChatRows(entry.peer, entry.result)...)
	}
	if r.relay != nil {
		// A relay-only peer is still part of the fleet. Its rows arrive tagged
		// already, and the caller's merge dedupes by the (tabId, chatId) pair, so
		// a machine reachable both ways is listed once.
		if relayed, err := r.relay.Call(ctx, method, params); err == nil {
			chats = append(chats, anySlice(mapFromAnyMain(relayed)["chats"])...)
		} else if reached == 0 {
			return nil, err
		}
	}
	if reached == 0 && len(chats) == 0 {
		return nil, fmt.Errorf("no fleet machine answered %s", method)
	}
	merged["chats"] = chats
	return merged, nil
}

// linkedPeers is every peer this daemon has a pairing record for.
func (r *fleetMeshRouter) linkedPeers() []string {
	if r.mesh == nil {
		return nil
	}
	out := make([]string, 0, 4)
	for _, state := range r.mesh.States() {
		if state.Peer != "" && state.Peer != r.self {
			out = append(out, state.Peer)
		}
	}
	if len(out) > 0 {
		return out
	}
	return r.mesh.PeerIDs()
}

// homedChatRows tags one peer's chat rows with the machine that owns them.
//
// The tag is the legacy `M~machine~local` dialect rather than a global id on
// purpose: it is what every existing reader of this projection speaks — the
// renderer, the merge below it, and `splitMachineTaggedID` — and §3 keeps that
// spelling as an alias precisely so another home's id can travel to a client
// that predates global ids without anything changing shape.
func homedChatRows(peerID string, result any) []any {
	rows := anySlice(mapFromAnyMain(result)["chats"])
	out := make([]any, 0, len(rows))
	for _, raw := range rows {
		chat := copyAnyMap(mapFromAnyMain(raw))
		if chat == nil {
			continue
		}
		tabID, tabOK := fleetid.New(peerID, fleetid.Chat, fieldString(chat, "tabId"))
		chatID, chatOK := fleetid.New(peerID, fleetid.Chat, fieldString(chat, "chatId"))
		if tabOK != nil || chatOK != nil {
			continue
		}
		chat["tabId"], chat["chatId"] = tabID.Legacy(), chatID.Legacy()
		chat["machineId"] = peerID
		out = append(out, chat)
	}
	return out
}

// meshControlPayload is the §6 envelope around one forwarded command.
//
// It is additive to the `agent:control` payload the controller path already
// speaks, so a peer that predates it reads method and params and behaves exactly
// as it does today — which is the whole of §12's "mixed versions are a steady
// state". A peer that understands it narrows by it.
func meshControlPayload(machineID, method string, params map[string]any) map[string]any {
	forwarded := stripFleetTargetTags(machineID, copyAnyMap(params))
	limit := strings.TrimSpace(fieldString(forwarded, fleetLimitParam))
	origin := mapFromAnyMain(forwarded[fleetOriginParam])
	operationID := strings.TrimSpace(fieldString(forwarded, "operation_id"))
	delete(forwarded, fleetLimitParam)
	delete(forwarded, fleetOriginParam)
	payload := map[string]any{
		"v":         fleetEnvelopeVersion,
		"method":    method,
		"params":    forwarded,
		"target_id": fleetTargetID(machineID, forwarded),
		"epoch":     0,
	}
	if limit != "" {
		payload["limit"] = limit
	}
	if len(origin) > 0 {
		payload["origin"] = origin
	}
	if operationID != "" {
		// One key for "launch there" and "record here": the caller minted it
		// once, uses it for the remote apply, and uses the same value for its own
		// local record of that work, so a retry cannot produce a second child on
		// the home or a second row here (§6).
		payload["operation_id"] = operationID
	}
	return payload
}

// stripFleetTargetTags resolves every addressing id to the bare local id the
// home itself uses.
//
// "The router strips its own tags on the way out" is the rule
// `rejectMachineTaggedParams` is written against: a home refuses an id that
// still names a machine, because resolving one locally would address the wrong
// chat. The renderer relay has always done this stripping in JavaScript; a
// daemon link had nobody doing it, so a chat-scoped command addressed the way
// an agent actually reads an id out of workass_list_chats — a machine-tagged
// pair — was refused by the home it was correctly routed to.
//
// Only an id naming THIS target is rewritten. An id naming a third machine is
// left exactly as it is, so the home refuses it by name instead of this router
// silently re-homing someone else's entity.
func stripFleetTargetTags(machineID string, params map[string]any) map[string]any {
	machineID = strings.TrimSpace(machineID)
	if machineID == "" || len(params) == 0 {
		return params
	}
	for _, field := range fleetAddressParamFields {
		value := strings.TrimSpace(fieldString(params, field))
		if value == "" {
			continue
		}
		home, local, tagged, err := fleetid.Split(value, "")
		if err != nil || !tagged || home != machineID || local == "" {
			continue
		}
		params[field] = local
	}
	return params
}

// fleetTargetID names the entity a forwarded command is about, in the global
// form, for the reader of a log line and for P3's per-entity routing. It is
// derived, never trusted: the home is the machine the router resolved.
func fleetTargetID(machineID string, params map[string]any) string {
	for _, field := range []string{"chat_id", "tab_id", "fork_point_id", "run_id", "work_id", "subagent_id", "id"} {
		local := strings.TrimSpace(fieldString(params, field))
		if local == "" {
			continue
		}
		kind := fleetid.Chat
		switch field {
		case "fork_point_id":
			kind = fleetid.Fork
		case "run_id":
			kind = fleetid.ForkRun
		case "work_id":
			kind = fleetid.Process
		case "subagent_id":
			kind = fleetid.Subagent
		}
		if id, err := fleetid.New(machineID, kind, local); err == nil {
			return id.String()
		}
	}
	if id, err := fleetid.New(machineID, fleetid.Peer, machineID); err == nil {
		return id.String()
	}
	return ""
}
