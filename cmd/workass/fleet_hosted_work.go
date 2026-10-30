package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	"workass/internal/fleetid"
	"workass/internal/fleetlog"
	"workass/internal/wire"
)

// A running thing on another machine, as a row in the chat that asked.
//
// docs/specs/fleet-runtime.md §14f. The monitor already carries what must not be
// lost, the outcome. This carries what the row shows while the work runs, which
// is a value and not a history: the home states it on `fleet:state`, the watcher
// makes the row say it, and a statement lost to a dropped link is stated again
// in the answer to the sync the link's return causes. Nothing here is logged and
// nothing here can become a notice.

const (
	// fleetStateChannel is a home stating the current state of one thing it runs.
	fleetStateChannel = "fleet:state"
	// fleetPushChannel carries a push over a socket this home dialled, for a pair
	// whose one link was opened from this side. Additive: an older peer refuses
	// the invoke, which costs a statement and loses nothing the log holds.
	fleetPushChannel = "fleet:push"
)

// hostedStateFor is what the sync answer states for one target, if it is still
// running. Set by the daemon, which owns the manager.
func (h *fleetHome) stateFor(targetID string) (map[string]any, bool) {
	if h == nil || h.states == nil {
		return nil, false
	}
	return h.states(targetID)
}

// pushState states one target's current value to every other machine watching it.
func (h *fleetHome) pushState(targetID string, fields map[string]any) {
	if !h.ready() || targetID == "" {
		return
	}
	seen := map[string]struct{}{}
	for _, row := range h.monitors.ByTarget(targetID) {
		home := row.WatcherHome()
		if home == "" || home == h.self {
			continue
		}
		if _, already := seen[home]; already {
			continue
		}
		seen[home] = struct{}{}
		payload := map[string]any{"targetId": targetID}
		for key, value := range fields {
			payload[key] = value
		}
		h.push(home, fleetStateChannel, payload)
	}
}

// hostedStatePayload is one statement, in the words the wire carries.
func hostedStatePayload(state acp.HostedWorkState) map[string]any {
	payload := map[string]any{
		"kind": state.Kind, "workId": state.WorkID, "label": state.Label,
		"providerId": state.ProviderID, "modelLabel": state.ModelLabel,
		"phase": state.Phase, "activity": state.Activity, "role": state.Role,
		"resource": state.Resource, "queuedAt": state.QueuedAt, "execStartedAt": state.ExecStartedAt,
		"startedAt": state.StartedAt, "queuePosition": state.QueuePosition,
		"outputFile": state.OutputFile, "isolation": state.Isolation, "stalled": state.Stalled,
		"reporting": state.Reporting,
	}
	if state.PID != nil {
		payload["pid"] = *state.PID
	}
	return payload
}

func hostedStateFromPayload(fields map[string]any) acp.HostedWorkState {
	state := acp.HostedWorkState{
		Kind: fieldString(fields, "kind"), WorkID: fieldString(fields, "workId"),
		Label: fieldString(fields, "label"), ProviderID: fieldString(fields, "providerId"),
		ModelLabel: fieldString(fields, "modelLabel"), Phase: fieldString(fields, "phase"),
		Activity: fieldString(fields, "activity"), Role: fieldString(fields, "role"),
		Resource: fieldString(fields, "resource"), QueuedAt: fieldString(fields, "queuedAt"),
		ExecStartedAt: fieldString(fields, "execStartedAt"), StartedAt: fieldString(fields, "startedAt"),
		QueuePosition: intField(fields, "queuePosition"), OutputFile: fieldString(fields, "outputFile"),
		Isolation: fieldString(fields, "isolation"), Stalled: boolFieldValue(fields, "stalled"),
		Reporting: fieldString(fields, "reporting"),
	}
	if value, present := intFieldPresent(fields, "pid"); present {
		state.PID = &value
	}
	return state
}

// ingestState is one statement arriving. It is honoured only for a monitor this
// daemon holds on that target from that machine, which is the same authority an
// event in the log needs (§11).
func (w *fleetWatcher) ingestState(peerID string, fields map[string]any) {
	if !w.ready() || w.hostedState == nil {
		return
	}
	targetID := strings.TrimSpace(fieldString(fields, "targetId"))
	if targetID == "" {
		return
	}
	row, ok := w.monitorFor(peerID, targetID)
	if !ok || row.Consumed {
		return
	}
	w.hostedState(peerID, row, fields)
}

// settleHostedRow ends the asking chat's row from the job or child's exit. It
// runs before the notice decision, so a cancel, which notifies nobody, still
// ends the row.
func (w *fleetWatcher) settleHostedRow(row fleetlog.Registration, event fleetlog.Event) {
	if w.settleHosted != nil && event.Kind == fleetlog.KindExit {
		w.settleHosted(row, event)
	}
}

// hostedRowOrigin is the actor turn a new hosted row hangs off: the running turn,
// or the last one this chat had. A row with no origin is refused by the actor
// rather than attached to a convenient turn.
func (r *providerChatRuntime) hostedRowOrigin(chatID string) (string, string, string) {
	if r == nil {
		return "", "", ""
	}
	state, ok := r.Snapshot(strings.TrimSpace(chatID))
	if !ok {
		return "", "", ""
	}
	if state.Foreground != nil {
		return string(state.Foreground.LaneID), string(state.Foreground.OperationID), state.Foreground.Turn.NativeID
	}
	return lastLedgerOrigin(state)
}

func lastLedgerOrigin(state chat.State) (string, string, string) {
	for i := len(state.Ledger) - 1; i >= 0; i-- {
		event := state.Ledger[i]
		if event.LaneID != "" && event.OperationID != "" {
			return string(event.LaneID), string(event.OperationID), event.NativeTurnID
		}
	}
	return "", "", ""
}

// applyHostedState makes the watching chat's row say what its host stated.
func (r *providerChatRuntime) applyHostedState(peerID, peerName string, row fleetlog.Registration, fields map[string]any) {
	if r == nil || r.manager == nil {
		return
	}
	chatID := strings.TrimSpace(row.WatcherChatID)
	tabID := strings.TrimSpace(row.WatcherTabID)
	if tabID == "" {
		tabID = r.chatTab(chatID)
	}
	if tabID == "" || !r.chatExists(tabID, chatID) {
		return
	}
	state := hostedStateFromPayload(fields)
	_, local, _, err := fleetid.Split(row.TargetID, fleetid.Process)
	if err != nil || local == "" {
		_, local, _, err = fleetid.Split(row.TargetID, fleetid.Subagent)
	}
	if err != nil || local == "" {
		return
	}
	if state.WorkID != "" && state.WorkID != local {
		return
	}
	state.WorkID = local
	state.Label = firstNonEmptyString(state.Label, row.Label)
	state.HostMachine, state.HostMachineName = peerID, peerName
	state.OriginLaneID, state.OriginOperationID, state.OriginTurnID = r.hostedRowOrigin(chatID)
	r.manager.ApplyHostedWorkState(tabID, chatID, state)
	if call := mapFromAnyMain(fields["call"]); len(call) > 0 {
		r.manager.ApplyHostedSubagentCall(tabID, chatID, local, call)
	}
}

// stopHostedWork answers a stop for a row whose work runs on another machine. The
// second result is false for every other row, which takes the actor's stop.
func (r *providerChatRuntime) stopHostedWork(tabID, chatID, id string) (map[string]any, bool) {
	if r == nil || r.manager == nil {
		return nil, false
	}
	return r.manager.StopHostedWork(tabID, chatID, id)
}

// settleHostedExit ends a hosted row from one exit event in the log.
func (r *providerChatRuntime) settleHostedExit(row fleetlog.Registration, event fleetlog.Event) {
	if r == nil || r.manager == nil {
		return
	}
	chatID := strings.TrimSpace(row.WatcherChatID)
	tabID := strings.TrimSpace(row.WatcherTabID)
	if tabID == "" {
		tabID = r.chatTab(chatID)
	}
	if fieldString(event.Payload, "entity") == "process" {
		receipt := spawnedWorkReceiptFromPayload(mapFromAnyMain(event.Payload["processReceipt"]))
		r.manager.SettleHostedProcess(tabID, chatID, receipt, fieldString(event.Payload, "outcome"))
		return
	}
	receipt := subagentReceiptFromPayload(mapFromAnyMain(event.Payload["receipt"]))
	r.settleHostedSubagentRow(tabID, chatID, receipt)
}

// fleetDisplayName is a peer's name as a person says it. A dialling daemon names
// itself "workass daemon <host> (<machine id>)", which is a useful line in a log
// and a poor one on a row.
func fleetDisplayName(name, machineID string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, "workass daemon")
	name = strings.TrimSpace(name)
	if machineID = strings.TrimSpace(machineID); machineID != "" {
		name = strings.TrimSpace(strings.TrimSuffix(name, "("+machineID+")"))
		if name == machineID {
			return ""
		}
	}
	return name
}

// recordHostedProcess is the row for a job this chat just started elsewhere, so
// it shows from the reply rather than from the first statement after it.
func (h *agentControlHandler) recordHostedProcess(machineID string, params map[string]any, result any) {
	if h == nil || h.chats == nil || h.chats.providerChats == nil || h.manager == nil {
		return
	}
	tabID, chatID := strings.TrimSpace(fieldString(params, "parent_tab_id")), strings.TrimSpace(fieldString(params, "parent_chat_id"))
	fields := mapFromAnyMain(result)
	workID := strings.TrimSpace(fieldString(fields, "workId"))
	if tabID == "" || chatID == "" || workID == "" {
		return
	}
	lane, operation, turn := h.chats.providerChats.hostedRowOrigin(chatID)
	h.manager.ApplyHostedWorkState(tabID, chatID, acp.HostedWorkState{
		Kind: acp.HostedWorkProcess, WorkID: workID, Label: fieldString(params, "label"),
		Role: fieldString(params, "role"), Resource: fieldString(params, "resource"),
		QueuedAt: fieldString(fields, "queuedAt"), StartedAt: fieldString(fields, "queuedAt"),
		OutputFile:  fieldString(fields, "outputFile"),
		HostMachine: strings.TrimSpace(machineID), HostMachineName: h.fleetPeerName(machineID),
		OriginLaneID: lane, OriginOperationID: operation, OriginTurnID: turn,
	})
}

// hostedWorkRouter is how a hosted row's stop and output reach its host.
func (h *agentControlHandler) hostedWorkRouter() acp.HostedWorkRouter {
	return acp.HostedWorkRouter{
		Stop: func(ctx context.Context, item acp.SpawnedWorkItem) error {
			params := map[string]any{"parent_tab_id": item.TabID, "parent_chat_id": item.ChatID}
			method := "process.cancel"
			if item.Kind == "subagent" {
				method = "agent.cancel_hosted"
				params["id"] = item.ID
			} else {
				params["work_id"] = item.ID
				params["reason"] = "stopped from the chat that owns it"
			}
			_, err := h.forwardRemoteAgentCall(ctx, method, item.HostMachine, params)
			return err
		},
		Read: func(ctx context.Context, item acp.SpawnedWorkItem, tailBytes int) (string, bool, error) {
			answer, err := h.forwardRemoteAgentCall(ctx, "process.read", item.HostMachine, map[string]any{
				"parent_tab_id": item.TabID, "parent_chat_id": item.ChatID,
				"work_id": item.ID, "tail_chars": tailBytes,
			})
			if err != nil {
				return "", false, err
			}
			fields := mapFromAnyMain(answer)
			return fieldString(fields, "output_tail"), boolFieldValue(fields, "output_truncated"), nil
		},
	}
}

// fleetPusher reaches a watcher over whichever socket the pair has. A home's push
// went only to a peer that dialled it, so for a pair linked from this side every
// push was dropped and the outcome waited for the next sync.
type fleetPusher struct {
	send func(peerID, channel string, payload any) int
	link func(peerID string) (func(ctx context.Context, channel string, args ...any) (any, error), bool)
	logf func(string, ...any)

	once  sync.Once
	queue chan fleetPush
}

type fleetPush struct {
	peer, channel string
	payload       any
}

// fleetPushQueue bounds what waits on a slow link. A statement that does not fit
// is replaced by the next one, and an event stays owed in the log.
const fleetPushQueue = 256

func (p *fleetPusher) push(peerID, channel string, payload any) int {
	if delivered := p.send(peerID, channel, payload); delivered > 0 {
		return delivered
	}
	if p.link == nil {
		return 0
	}
	if _, linked := p.link(peerID); !linked {
		return 0
	}
	p.once.Do(func() {
		p.queue = make(chan fleetPush, fleetPushQueue)
		go p.drain()
	})
	select {
	case p.queue <- fleetPush{peer: peerID, channel: channel, payload: payload}:
		return 1
	default:
		return 0
	}
}

func (p *fleetPusher) drain() {
	for item := range p.queue {
		invoke, linked := p.link(item.peer)
		if !linked {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_, err := invoke(ctx, fleetPushChannel, map[string]any{"channel": item.channel, "payload": item.payload})
		cancel()
		if err != nil && p.logf != nil && item.channel != fleetStateChannel {
			p.logf("[fleet] a push to %s did not arrive and stays owed in the log: %v", item.peer, err)
		}
	}
}

// RegisterPushChannel accepts a push from a peer that this daemon's own socket
// does not reach. The peer id comes from the authenticated socket, and the
// channels are the two the watcher already takes from a peer's socket.
func (w *fleetWatcher) RegisterPushChannel(hub *wire.Hub) {
	if hub == nil || !w.ready() {
		return
	}
	hub.RegisterWithCaller(fleetPushChannel, func(caller wire.Caller, args []any) (any, error) {
		peer := strings.TrimSpace(caller.Peer)
		if peer == "" {
			return nil, errors.New("the fleet push channel is for paired daemons; this caller is not one")
		}
		fields := firstMapArg(args)
		channel := strings.TrimSpace(fieldString(fields, "channel"))
		if channel != fleetLogEventChannel && channel != fleetStateChannel {
			return nil, errors.New("the fleet push channel carries monitor events only")
		}
		w.Ingest(peer, channel, fields["payload"])
		return map[string]any{"ok": true}, nil
	})
}
