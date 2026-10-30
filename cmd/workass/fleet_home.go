package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"workass/internal/acp"
	"workass/internal/fleetid"
	"workass/internal/fleetlog"
	"workass/internal/wire"
)

// The home half of P3: logs, subscriptions and the monitors this machine owes.
//
// docs/specs/fleet-runtime.md §5 and §7. Before this, a daemon could ANSWER a
// command from another machine (P2) but could not tell it anything afterwards:
// a child on another machine was watched by re-asking every two seconds
// (`clampControllerWait`), and the child's completion went into the chat on the
// machine that hosted it rather than into the chat that asked (§1). A home with
// no outbound half had no way to say "this finished" to the daemon that cared.
//
// So this file gives the home three things and no more:
//
//   - an append-only log per entity, which is the authoritative history;
//   - a cursor-addressed subscription over the link that already exists, with
//     store-and-forward: a disconnect POSTPONES events, it never drops them,
//     and nothing polls;
//   - a durable record of who is owed what, so a restart on either side owes
//     exactly the events it owed before.
//
// What it deliberately does NOT hold is a subscriber registry. A subscription
// is a cursor on a live socket and dies with it; what survives is the monitor
// (durable, §7) and the log (durable, §5). Conflating them is how a restart
// used to lose an outcome: the process that knew who was listening was the
// process that died.

// The channels one link's monitor life needs. They are additive to the frozen
// invoke/reply/event protocol: new channels only, no existing payload changes,
// and a peer that never calls them behaves exactly as it does today (§12,
// mixed versions are a steady state).
const (
	fleetMonitorChannel     = "fleet:monitor"
	fleetDemonitorChannel   = "fleet:demonitor"
	fleetMonitorSyncChannel = "fleet:monitor.sync"
	fleetLogAckChannel      = "fleet:log.ack"
	// fleetLogEventChannel is the push. The home sends it to exactly one peer —
	// the watcher — because a monitor carries a capability naming the watcher
	// and nothing else may be read through it (§11).
	fleetLogEventChannel = "fleet:log"
)

// fleetHome owns this daemon's entity logs and the monitors it holds for other
// machines' watchers.
type fleetHome struct {
	self     string
	logs     *fleetlog.Store
	monitors *fleetlog.Monitors
	// push admits one event to one peer's sockets. It returns how many sockets
	// it reached, which is how "the peer is not connected" is told from "sent"
	// with no timeout anywhere.
	push func(peerID, channel string, payload any) int
	logf func(string, ...any)

	mu sync.Mutex
}

func newFleetHome(self string, logs *fleetlog.Store, monitors *fleetlog.Monitors, push func(string, string, any) int, logf func(string, ...any)) *fleetHome {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if push == nil {
		push = func(string, string, any) int { return 0 }
	}
	return &fleetHome{self: strings.TrimSpace(self), logs: logs, monitors: monitors, push: push, logf: logf}
}

func (h *fleetHome) ready() bool {
	return h != nil && h.logs != nil && h.monitors != nil && strings.TrimSpace(h.self) != ""
}

// EntityID names one entity this daemon homes, in §3's global form.
func (h *fleetHome) EntityID(kind fleetid.Type, local string) string {
	if !h.ready() {
		return ""
	}
	id, err := fleetid.New(h.self, kind, strings.TrimSpace(local))
	if err != nil {
		return ""
	}
	return id.String()
}

// Append writes one event to an entity's log and hands it to every monitor that
// is owed it. The log write comes FIRST and is durable: a push that never
// arrives is a postponement, because the subscriber will ask from its cursor,
// but an event that was never written is simply gone.
func (h *fleetHome) Append(targetID, kind string, payload map[string]any) (fleetlog.Event, error) {
	if !h.ready() {
		return fleetlog.Event{}, errors.New("this daemon has no entity logs")
	}
	h.mu.Lock()
	event, err := h.logs.Append(targetID, kind, payload)
	h.mu.Unlock()
	if err != nil {
		h.logf("[fleet] could not append %s to %s: %v", kind, targetID, err)
		return fleetlog.Event{}, err
	}
	h.fanOut(event)
	return event, nil
}

// fanOut pushes one event to the watchers whose monitors allow that kind.
//
// One push per watcher HOME, not per monitor: two monitors on the same machine
// share one socket and one ordered stream, which is what makes §7's "one
// ordered stream per watcher" true of the wire rather than only of the model.
func (h *fleetHome) fanOut(event fleetlog.Event) {
	rows := h.monitors.ByTarget(event.Entity)
	if len(rows) == 0 {
		return
	}
	head := h.logs.Head(event.Entity)
	seen := map[string]struct{}{}
	for _, row := range rows {
		if !row.Allows(event.Kind) {
			continue
		}
		home := row.WatcherHome()
		if home == "" || home == h.self {
			continue
		}
		if _, already := seen[home]; already {
			continue
		}
		seen[home] = struct{}{}
		h.push(home, fleetLogEventChannel, map[string]any{
			"targetId": event.Entity, "head": head, "events": []any{eventPayload(event)},
		})
	}
}

// Register exposes the monitor channels to paired peers.
func (h *fleetHome) Register(hub *wire.Hub) bool {
	if hub == nil || !h.ready() {
		return false
	}
	hub.RegisterWithCaller(fleetMonitorChannel, func(caller wire.Caller, args []any) (any, error) {
		return h.handleMonitor(caller, firstMapArg(args))
	})
	hub.RegisterWithCaller(fleetDemonitorChannel, func(caller wire.Caller, args []any) (any, error) {
		return h.handleDemonitor(caller, firstMapArg(args))
	})
	hub.RegisterWithCaller(fleetMonitorSyncChannel, func(caller wire.Caller, args []any) (any, error) {
		return h.handleSync(caller, firstMapArg(args))
	})
	hub.RegisterWithCaller(fleetLogAckChannel, func(caller wire.Caller, args []any) (any, error) {
		return h.handleAck(caller, firstMapArg(args))
	})
	return true
}

// peerOf is the one authorization these channels apply, and §11 is why it is
// this and not more: a link grants nothing by itself, so the watcher a monitor
// names must be homed on the machine that is actually asking. The peer id comes
// from the authenticated socket, never from the payload — a peer that could
// name another machine's watcher could read that machine's work through a
// monitor it registered for it.
func (h *fleetHome) peerOf(caller wire.Caller) (string, error) {
	peer := strings.TrimSpace(caller.Peer)
	if peer == "" {
		return "", errors.New("the fleet monitor channels are for paired daemons; this caller is not one")
	}
	return peer, nil
}

func (h *fleetHome) handleMonitor(caller wire.Caller, params map[string]any) (any, error) {
	peer, err := h.peerOf(caller)
	if err != nil {
		return nil, err
	}
	watcherID := strings.TrimSpace(fieldString(params, "watcher_id"))
	targetID := strings.TrimSpace(fieldString(params, "target_id"))
	row, err := h.register(peer, fleetlog.Registration{
		WatcherID: watcherID, TargetID: targetID,
		Kinds:             stringSliceField(params["kinds"]),
		Label:             fieldString(params, "label"),
		WatcherChatID:     fieldString(params, "watcher_chat_id"),
		WatcherTabID:      fieldString(params, "watcher_tab_id"),
		OriginLaneID:      fieldString(params, "origin_lane_id"),
		OriginOperationID: fieldString(params, "origin_operation_id"),
	})
	if err != nil {
		return nil, err
	}
	slice, err := h.logs.Since(row.TargetID, row.AppliedSeq, 0)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "ref": row.Ref, "targetId": row.TargetID, "slice": slicePayload(slice)}, nil
}

// register is the home-side registration, used by the channel above and
// directly by the spawn path: a subagent spawned from another machine's chat is
// monitored from the instant it exists, so a child that finishes before the
// reply gets home still has somewhere to report.
func (h *fleetHome) register(peer string, row fleetlog.Registration) (fleetlog.Registration, error) {
	if !h.ready() {
		return fleetlog.Registration{}, errors.New("this daemon has no entity logs")
	}
	row.WatcherID, row.TargetID = strings.TrimSpace(row.WatcherID), strings.TrimSpace(row.TargetID)
	if row.WatcherID == "" || row.TargetID == "" {
		return fleetlog.Registration{}, errors.New("a monitor needs a watcher id and a target id")
	}
	if !fleetid.IsGlobal(row.WatcherID) || !fleetid.IsGlobal(row.TargetID) {
		return fleetlog.Registration{}, errors.New("a monitor's watcher and target must be global fleet ids")
	}
	watcherHome, err := fleetid.Parse(row.WatcherID)
	if err != nil {
		return fleetlog.Registration{}, err
	}
	targetHome, err := fleetid.Parse(row.TargetID)
	if err != nil {
		return fleetlog.Registration{}, err
	}
	if targetHome.Home != h.self {
		// One writer per entity, at its home (§8). A monitor on someone else's
		// entity is not this machine's to hold, and holding it would be a second
		// claim on a log this daemon does not own.
		return fleetlog.Registration{}, fmt.Errorf("%s is not homed on this machine", row.TargetID)
	}
	if peer != "" && watcherHome.Home != peer {
		return fleetlog.Registration{}, fmt.Errorf("this link belongs to %s, so it may not register a monitor for a watcher on %s", peer, watcherHome.Home)
	}
	return h.monitors.Add(row)
}

func (h *fleetHome) handleDemonitor(caller wire.Caller, params map[string]any) (any, error) {
	peer, err := h.peerOf(caller)
	if err != nil {
		return nil, err
	}
	ref := strings.TrimSpace(fieldString(params, "ref"))
	row, found := h.monitors.Get(ref)
	if !found {
		// demonitor on a monitor that is already gone is a success, not a fault:
		// it is called on consume and again on reconnect, and the answer to both
		// is the same state.
		return map[string]any{"ok": true, "removed": false}, nil
	}
	if row.WatcherHome() != peer {
		return nil, errors.New("that monitor belongs to another machine's watcher")
	}
	_, removed, err := h.monitors.Remove(ref)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "removed": removed}, nil
}

// handleSync is the one round trip a link needs on the way up: the watcher
// states its cursors, the home answers with the backlog from each of them AND
// with every monitor it holds for that machine.
//
// Both halves matter. The cursors are store-and-forward: a four-day outage is
// delivered from the cursor, in order, with nothing polling in between. The
// enumeration is what keeps the registry honest in both directions — a watcher
// adopts a monitor whose registration reply it never received, and drops one
// whose watcher no longer exists, which is why a dropped watcher leaks no
// registration (§7).
func (h *fleetHome) handleSync(caller wire.Caller, params map[string]any) (any, error) {
	peer, err := h.peerOf(caller)
	if err != nil {
		return nil, err
	}
	held := h.monitors.ByWatcherHome(peer)
	cursors := map[string]uint64{}
	for _, raw := range anySlice(params["cursors"]) {
		entry := mapFromAnyMain(raw)
		target := strings.TrimSpace(fieldString(entry, "target_id"))
		if target == "" {
			continue
		}
		cursors[target] = uint64(intField(entry, "after_seq"))
	}
	monitors := make([]any, 0, len(held))
	slices := make([]any, 0, len(held))
	for _, row := range held {
		monitors = append(monitors, registrationPayload(row))
		after, stated := cursors[row.TargetID]
		if !stated {
			after = row.AckedSeq
		}
		slice, err := h.logs.Since(row.TargetID, after, 0)
		if err != nil {
			continue
		}
		slices = append(slices, slicePayload(slice))
	}
	return map[string]any{"ok": true, "machineId": h.self, "monitors": monitors, "slices": slices}, nil
}

// handleAck advances what this watcher has acknowledged, which is the only
// thing that lets a terminal event leave the log: "nothing is forgotten before
// it is consumed" outranks the retention cap (§5, §16.3c).
func (h *fleetHome) handleAck(caller wire.Caller, params map[string]any) (any, error) {
	peer, err := h.peerOf(caller)
	if err != nil {
		return nil, err
	}
	targetID := strings.TrimSpace(fieldString(params, "target_id"))
	through := uint64(intField(params, "through_seq"))
	acked := 0
	for _, row := range h.monitors.ByTarget(targetID) {
		if row.WatcherHome() != peer {
			continue
		}
		if _, ok := h.monitors.Update(row.Ref, func(entry *fleetlog.Registration) {
			if through > entry.AckedSeq {
				entry.AckedSeq = through
			}
		}); ok {
			acked++
		}
	}
	return map[string]any{"ok": true, "acked": acked}, nil
}

// ObserveSubagent is the manager's lifecycle observer (§13's M2: subagents
// first). Every terminal and attention event is appended to the subagent's own
// log, which is what makes a subagent an entity rather than a field on a
// struct, and what any watcher — this daemon or another — then reads.
func (h *fleetHome) ObserveSubagent(event acp.SubagentLifecycleEvent) {
	if !h.ready() || strings.TrimSpace(event.SubagentID) == "" {
		return
	}
	target := h.EntityID(fleetid.Subagent, event.SubagentID)
	if target == "" {
		return
	}
	switch event.Kind {
	case "exit":
		payload := map[string]any{
			"outcome": event.Outcome, "label": event.Label,
			"receipt": subagentReceiptPayload(event.Receipt),
		}
		if event.NoticeSuppressed {
			// The home's own suppression verdict travels with the event, so a
			// watcher elsewhere applies the SAME conditions this machine applies
			// (§7) instead of a weaker copy of them. The event is still logged:
			// suppression is about the notice, never about the history.
			payload["noticeSuppressed"] = true
			payload["suppressReason"] = event.SuppressReason
		}
		if origin := strings.TrimSpace(event.OriginMachine); origin != "" {
			payload["originMachine"] = origin
			payload["originChatId"] = event.OriginChatID
			payload["originTabId"] = event.OriginTabID
		}
		_, _ = h.Append(target, fleetlog.KindExit, payload)
	case "attention":
		if event.Attention == nil {
			return
		}
		_, _ = h.Append(target, fleetlog.KindAttention, map[string]any{
			"attentionKind": event.Attention.Kind, "title": event.Attention.Message,
			"label": event.Label, "sequence": event.Attention.Sequence,
			"grantableBy": event.Attention.GrantableBy, "subagentId": event.SubagentID,
		})
	}
}

// WatchSubagentForOrigin registers the home-side monitor for a subagent whose
// parent chat is on another machine, at spawn time.
//
// At spawn rather than at completion, and that is the whole point: a child that
// finishes in the same millisecond its reply is still travelling has somewhere
// to report, because the monitor already exists and the log already holds the
// event. The watcher then pulls from its cursor, so the ORDER of registration
// and exit does not change what is delivered.
func (h *fleetHome) WatchSubagentForOrigin(origin fleetCommandOrigin, subagentID, label, operationID string) (fleetlog.Registration, bool) {
	if !h.ready() {
		return fleetlog.Registration{}, false
	}
	machine := strings.TrimSpace(origin.Machine)
	chatID := strings.TrimSpace(origin.ChatID)
	if machine == "" || machine == h.self || chatID == "" {
		return fleetlog.Registration{}, false
	}
	watcher, err := fleetid.New(machine, fleetid.Chat, chatID)
	if err != nil {
		return fleetlog.Registration{}, false
	}
	target := h.EntityID(fleetid.Subagent, subagentID)
	if target == "" {
		return fleetlog.Registration{}, false
	}
	row, err := h.register(machine, fleetlog.Registration{
		WatcherID: watcher.String(), TargetID: target, Label: label,
		WatcherChatID: chatID, WatcherTabID: strings.TrimSpace(origin.TabID),
		OriginOperationID: firstNonEmptyString(strings.TrimSpace(operationID), strings.TrimSpace(origin.OperationID)),
	})
	if err != nil {
		h.logf("[fleet] could not monitor %s for %s: %v", subagentID, machine, err)
		return fleetlog.Registration{}, false
	}
	return row, true
}

// DropMonitorsForMissingWatchers is the leak guard, run from the watcher's own
// side of the link: the home cannot know whether a chat on another machine
// still exists, and the machine that can is the one that owns it.
func (h *fleetHome) monitorsHeldFor(peer string) []fleetlog.Registration {
	if !h.ready() {
		return nil
	}
	return h.monitors.ByWatcherHome(strings.TrimSpace(peer))
}

func eventPayload(event fleetlog.Event) map[string]any {
	return map[string]any{
		"entity": event.Entity, "seq": event.Seq, "kind": event.Kind,
		"at": event.At.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), "payload": event.Payload,
	}
}

func slicePayload(slice fleetlog.Slice) map[string]any {
	events := make([]any, 0, len(slice.Events))
	for _, event := range slice.Events {
		events = append(events, eventPayload(event))
	}
	out := map[string]any{"targetId": slice.Entity, "events": events, "head": slice.Head, "firstSeq": slice.FirstSeq}
	if slice.Truncated {
		out["truncated"] = true
		out["truncatedTo"] = slice.TruncatedTo
	}
	return out
}

func registrationPayload(row fleetlog.Registration) map[string]any {
	return map[string]any{
		"ref": row.Ref, "watcherId": row.WatcherID, "targetId": row.TargetID,
		"kinds": row.Kinds, "label": row.Label,
		"watcherChatId": row.WatcherChatID, "watcherTabId": row.WatcherTabID,
		"originLaneId": row.OriginLaneID, "originOperationId": row.OriginOperationID,
		"ackedSeq": row.AckedSeq,
	}
}

// subagentReceiptPayload carries the bounded terminal receipt across a link in
// exactly the fields the local projection reads, so the notice a remote chat
// renders is byte-identical to the one the hosting chat would have rendered.
// Text was redacted before it was persisted and is redacted again on arrival.
func subagentReceiptPayload(receipt acp.SubagentReceipt) map[string]any {
	return map[string]any{
		"receiptId": receipt.ReceiptID, "subagentId": receipt.SubagentID, "label": receipt.Label,
		"status": receipt.Status, "providerId": receipt.ProviderID, "modelId": receipt.ModelID,
		"effort": receipt.Effort, "modelLabel": receipt.ModelLabel, "modeId": receipt.ModeID,
		"startedAt": receipt.StartedAt, "finishedAt": receipt.FinishedAt, "elapsedMs": receipt.ElapsedMs,
		"stopReason": receipt.StopReason, "result": receipt.Result, "error": receipt.Error,
		"resultTruncated": receipt.ResultTruncated, "errorTruncated": receipt.ErrorTruncated,
		"originLaneId": receipt.OriginLaneID, "originOperationId": receipt.OriginOperationID,
		"originMachine": receipt.OriginMachine, "originChatId": receipt.OriginChatID,
		"originTabId": receipt.OriginTabID,
	}
}

func subagentReceiptFromPayload(payload map[string]any) acp.SubagentReceipt {
	return acp.SubagentReceipt{
		ReceiptID: fieldString(payload, "receiptId"), SubagentID: fieldString(payload, "subagentId"),
		Label: fieldString(payload, "label"), Status: fieldString(payload, "status"),
		ProviderID: fieldString(payload, "providerId"), ModelID: fieldString(payload, "modelId"),
		Effort: fieldString(payload, "effort"), ModelLabel: fieldString(payload, "modelLabel"),
		ModeID: fieldString(payload, "modeId"), StartedAt: fieldString(payload, "startedAt"),
		FinishedAt: fieldString(payload, "finishedAt"), ElapsedMs: int64(intField(payload, "elapsedMs")),
		StopReason: fieldString(payload, "stopReason"), Result: fieldString(payload, "result"),
		Error:           fieldString(payload, "error"),
		ResultTruncated: boolFieldValue(payload, "resultTruncated"), ErrorTruncated: boolFieldValue(payload, "errorTruncated"),
		OriginLaneID: fieldString(payload, "originLaneId"), OriginOperationID: fieldString(payload, "originOperationId"),
		OriginMachine: fieldString(payload, "originMachine"), OriginChatID: fieldString(payload, "originChatId"),
		OriginTabID: fieldString(payload, "originTabId"),
	}
}
