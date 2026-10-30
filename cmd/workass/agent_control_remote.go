package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"workass/internal/fleetid"
	"workass/internal/wire"
)

const (
	// remoteAgentControlChannel is additive to the frozen invoke/reply/event
	// protocol: no existing channel or payload changes, and a renderer that
	// never calls it behaves exactly as before.
	remoteAgentControlChannel = "agent:control"
	// remoteAgentWaitPollMS bounds a wait that arrives from another machine.
	// Invokes share one ordered lane per client, so a blocking cross-machine
	// wait would stall every other operation against this machine for as long
	// as the child ran. A remote wait therefore polls: it returns the same
	// shape a local wait returns when its window elapses, and the caller
	// re-asks (or uses workass_schedule_message).
	remoteAgentWaitPollMS = 2000
)

// registerAgentControlWireHandler exposes this machine's Agent API control
// surface to a paired controller, so the Workass tools drive a chat here the
// same way whether the agent runs on this machine or on another fleet machine.
//
// Authorization is unchanged in kind: the controller link is the same authority
// that already carries chat:delete and job:start, and every call then passes
// the durable actor fence plus the short-lived actor owner key the local MCP
// path mints for an already-authorized effect. No new listener, port, or
// credential is introduced.
func registerAgentControlWireHandler(hub *wire.Hub, control *agentControlHandler) bool {
	if hub == nil || control == nil {
		return false
	}
	// RegisterWithCaller, not Register: §6's limit is the LESSER of the caller's
	// own ceiling and the grant recorded for the link the command arrived on, and
	// the link is a property of the caller. An invoke that reaches this channel
	// without a socket gets an empty caller, which is the local path and is
	// exactly what it was before.
	hub.RegisterWithCaller("agent:control", func(caller wire.Caller, args []any) (any, error) {
		return control.callAsControllerFor(context.Background(), caller, firstMapArg(args))
	})
	return true
}

func (h *agentControlHandler) callAsController(ctx context.Context, payload map[string]any) (any, error) {
	return h.callAsControllerFor(ctx, wire.Caller{}, payload)
}

func (h *agentControlHandler) callAsControllerFor(ctx context.Context, caller wire.Caller, payload map[string]any) (any, error) {
	if h == nil || h.chats == nil || h.chats.providerChats == nil || h.manager == nil {
		return nil, errors.New("Workass agent control is unavailable")
	}
	if version := intField(payload, "v"); version > fleetEnvelopeVersion {
		// A structured refusal naming the supported range, rather than reading a
		// shape this daemon does not know (§14, version skew).
		return nil, fmt.Errorf("this daemon speaks fleet command envelope v%d; the caller sent v%d — update this machine",
			fleetEnvelopeVersion, version)
	}
	method := strings.TrimSpace(fieldString(payload, "method"))
	// The envelope, evaluated here because the home is where authorization is
	// written (§11). The grant belongs to the link; the limit is the caller's own
	// ceiling; the effective permission is the lesser, and it can only narrow.
	//
	// It is read BEFORE the locality table, so a command that is refused for
	// both reasons is refused for the one the caller can act on: "this link does
	// not carry schedule.*" tells them which capability is missing, where "no
	// remote form" would send them looking for a routing mistake they did not
	// make.
	forwarded := fleetForwardedWork{
		Peer:        strings.TrimSpace(caller.Peer),
		Limit:       fleetEffectiveLimit(caller.Grant, fieldString(payload, "limit")),
		Origin:      fleetOriginFrom(payload["origin"]),
		OperationID: strings.TrimSpace(fieldString(payload, "operation_id")),
	}
	if err := fleetRefuseNarrowedSurface(method, forwarded.Limit, mapFromAnyMain(payload["params"])); err != nil {
		return nil, err
	}
	kind, known := remoteAgentMethodKinds[method]
	if !known || kind == remoteAgentLocalOnly {
		reason := remoteAgentLocalOnlyReasons[method]
		if reason == "" {
			return nil, errors.New("Workass does not accept " + method + " from another machine")
		}
		return nil, errors.New(reason)
	}
	params := copyAnyMap(mapFromAnyMain(payload["params"]))
	// Only ids this machine minted may act here. The router strips its own
	// tags on the way out; anything still tagged is a routing fault, and
	// resolving it locally would address the wrong chat.
	if err := rejectMachineTaggedParams(params); err != nil {
		return nil, err
	}
	delete(params, "machine_id")
	delete(params, "owner_key")
	if forwarded.OperationID != "" && strings.TrimSpace(fieldString(params, "operation_id")) == "" {
		// One operation id for "launch there" and "record here". Carrying the
		// caller's key into the local apply is what makes a replay produce
		// exactly one child here and exactly one row there (§6).
		params["operation_id"] = forwarded.OperationID
	}
	ctx = withFleetForwardedWork(ctx, forwarded)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://127.0.0.1"+toolsPath, nil)
	if err != nil {
		return nil, err
	}
	switch kind {
	case remoteAgentMachineScoped:
		switch method {
		case "chat.create":
			return h.chats.createOnThisMachine(ctx, params)
		case "agent.catalog":
			return h.manager.MachineAgentCatalog(ctx), nil
		case "chat.list":
			// This machine's own catalog, and deliberately not the fleet's: the
			// caller is already asking every peer it knows, so answering with a
			// merged list here would double-count a shared peer and, between two
			// daemons that each know the other, never terminate.
			return h.chats.list()
		}
		return nil, errors.New("unknown machine-scoped agent control method")
	case remoteAgentForkScoped:
		chatID, tabID, ok := h.forkOwnerForController(method, params)
		if !ok {
			return nil, errors.New("unknown fork point or run; call workass_list_forks for exact ids")
		}
		return h.controllerChatCall(request, method, params, tabID, chatID)
	default:
		result, err := h.controllerChatCall(request, method, params, fieldString(params, "tab_id"), fieldString(params, "chat_id"))
		if err != nil || method != "agent.spawn" {
			return result, err
		}
		return h.monitorForwardedSpawn(forwarded, result), nil
	}
}

// monitorForwardedSpawn registers the home-side monitor for a subagent spawned
// from a chat on another machine, and hands the watcher the reference.
//
// At spawn, not at completion. A child that finishes while this very reply is
// still on the wire has somewhere to report, because the monitor already exists
// and its log already holds the event — and the watcher pulls from a cursor, so
// the order those two things happened in changes nothing about what is
// delivered. This is the whole of §1's "children on another machine report into
// THAT machine's chat, never to the chat that asked", answered.
//
// The result is additive: a `monitor` block beside the fields agent.spawn
// already returns. A caller that predates it reads the spawn exactly as it does
// today, which is §12's mixed fleet.
func (h *agentControlHandler) monitorForwardedSpawn(forwarded fleetForwardedWork, result any) any {
	if h == nil || !h.fleetHome.ready() || forwarded.Origin.empty() {
		return result
	}
	fields := mapFromAnyMain(result)
	subagentID := strings.TrimSpace(fieldString(fields, "id"))
	if subagentID == "" {
		return result
	}
	row, ok := h.fleetHome.WatchSubagentForOrigin(forwarded.Origin, subagentID, fieldString(fields, "label"), forwarded.OperationID)
	if !ok {
		return result
	}
	slice, err := h.fleetHome.logs.Since(row.TargetID, 0, 0)
	if err != nil {
		return result
	}
	merged := copyAnyMap(fields)
	if merged == nil {
		merged = map[string]any{}
	}
	merged["monitor"] = map[string]any{
		"ref": row.Ref, "targetId": row.TargetID, "label": row.Label, "slice": slicePayload(slice),
	}
	return merged
}

func (h *agentControlHandler) forkOwnerForController(method string, params map[string]any) (string, string, bool) {
	if h.forks == nil {
		return "", "", false
	}
	if method == "fork.run" {
		return h.forks.PointOwner(fieldString(params, "fork_point_id"))
	}
	return h.forks.RunOwner(fieldString(params, "run_id"))
}

func (h *agentControlHandler) controllerChatCall(r *http.Request, method string, params map[string]any, tabID, chatID string) (any, error) {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if tabID == "" || chatID == "" {
		return nil, errors.New(method + " requires the exact tab_id and chat_id of a chat on this machine")
	}
	if err := h.fenceActor(tabID, chatID); err != nil {
		return nil, err
	}
	// The acting chat IS the addressed chat: a tool called for a chat on this
	// machine acts for that chat, exactly as a delegated agent acts for the
	// chat that spawned it.
	params["parent_tab_id"], params["parent_chat_id"] = tabID, chatID
	clampControllerWait(method, params)
	return h.manager.WithActorOwner(chatID, tabID, func(ownerKey string) (any, error) {
		params["owner_key"] = ownerKey
		return h.call(r, agentControlRequest{Method: method, Params: params})
	})
}

func clampControllerWait(method string, params map[string]any) {
	switch method {
	case "agent.wait", "agent.wait_many", "fork.wait":
	default:
		return
	}
	requested := intField(params, "timeout_ms")
	if requested > 0 && requested < remoteAgentWaitPollMS {
		return
	}
	params["timeout_ms"] = remoteAgentWaitPollMS
}

func rejectMachineTaggedParams(params map[string]any) error {
	for key, value := range params {
		text, ok := value.(string)
		// Either dialect of a home-bearing id is the same fault here: the router
		// strips its own tags on the way out, so anything still naming a home is
		// a routing error, and resolving it locally would address the wrong chat.
		// This scan reaches every param, message text included, so it asks
		// whether the value NAMES a home rather than whether it merely opens
		// like one.
		if !ok || !fleetid.NamesHome(text) {
			continue
		}
		return errors.New("a forwarded Workass tool call must carry this machine's own ids; " + key + " is machine-tagged")
	}
	return nil
}
