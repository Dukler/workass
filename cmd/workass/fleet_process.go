package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"workass/internal/acp"
	"workass/internal/fleetid"
	"workass/internal/fleetlog"
	"workass/internal/wire"
)

// A process child as a fleet entity: one log, one monitor, one notice path —
// and the same one whether its owner is here or on another machine.
//
// docs/specs/fleet-runtime.md §13 P4, continuing §13's M2 order (subagents
// first, then process children). The whole of P4's reporting is this file plus
// three lines in the home's fan-out, because P3 already built the hard parts:
// a per-entity log at the home, durable monitors at both ends, store-and-
// forward with dedup by seq, and `home_down` for a machine that goes dark.
//
// What P4 adds is the direction P3 did not need. A subagent on another machine
// is spawned BY a chat here, so this daemon learns about it from the spawn
// reply and records its own monitor then (fleetWatcher.WatchRemoteSubagent). A
// process child is registered by `workass run` ON the machine that runs it, so
// the owner never made a call and has nothing to learn from. The home has to
// OFFER the monitor, and `fleet:monitor.offer` is that: the exact inverse of
// `fleet:monitor`, authorized the same way — the target must be homed on the
// peer that is asking, and the watcher must be a chat that really exists here.
//
// A monitor whose watcher is on THIS machine is registered too, and that is
// not redundancy: it is what makes the local path and the remote path the same
// path. The home appends, the fan-out notices the watcher is itself, and the
// event goes to the same projection a peer's event would reach — so a chat
// reads a job on its own machine through the same code that reads one three
// rooms away. §2's claim, as code rather than as prose.

// fleetMonitorOfferChannel is the home telling a watcher's daemon that it is
// owed a monitor it never asked for. Additive to the frozen protocol: a peer
// that never calls it, and a peer that does not serve it, both behave exactly
// as they do today.
const fleetMonitorOfferChannel = "fleet:monitor.offer"

// ObserveProcess is the manager's process-child lifecycle observer.
//
// Every terminal, attention and declared stall becomes an entry in that
// child's own log, which is what makes a process child an entity rather than a
// row in a registry — and what any watcher, here or elsewhere, then reads.
func (h *fleetHome) ObserveProcess(event acp.ProcessLifecycleEvent) {
	if !h.ready() || strings.TrimSpace(event.ChildID) == "" {
		return
	}
	target := h.EntityID(fleetid.Process, event.ChildID)
	if target == "" {
		return
	}
	switch event.Kind {
	case acp.ProcessLifecycleExit:
		payload := map[string]any{
			"outcome": event.Outcome, "label": event.Label, "entity": "process",
			"processReceipt": spawnedWorkReceiptPayload(event.Receipt),
			"resource":       event.Resource, "isolation": event.Isolation,
		}
		if event.ExitCode != nil {
			payload["exitCode"] = *event.ExitCode
		}
		if event.NoticeSuppressed {
			// This machine's own suppression verdict travels with the event, so
			// a watcher elsewhere applies the SAME condition rather than a
			// weaker copy of it. The event is still logged: suppression is
			// about the notice, never about the history.
			payload["noticeSuppressed"] = true
			payload["suppressReason"] = event.SuppressReason
		}
		_, _ = h.Append(target, fleetlog.KindExit, payload)
	case acp.ProcessLifecycleAttention, acp.ProcessLifecycleStalled:
		// A declared stall rides as an attention. §7's monitor stream has one
		// vocabulary — exit, attention, waiting, resumed, progress — and adding
		// a sixth word for "it went quiet and said that would matter" would be
		// a second vocabulary for the same stream.
		_, _ = h.Append(target, fleetlog.KindAttention, map[string]any{
			"attentionKind": event.AttentionKind, "title": event.AttentionText,
			"label": event.Label, "entity": "process", "resource": event.Resource,
		})
	}
}

// WatchProcess records the home-side monitor for one process child, and offers
// it to the owner's daemon when the owner is elsewhere.
//
// At registration rather than at completion, and that is the point: a job that
// finishes in the same second its registration reply is still travelling has
// somewhere to report, because the monitor already exists and the log already
// holds the event. The watcher pulls from its cursor, so the ORDER of the two
// does not change what is delivered.
func (h *fleetHome) WatchProcess(ctx context.Context, mesh fleetMonitorInvoker, owner processOwner, childID, label string) (fleetlog.Registration, bool) {
	if !h.ready() {
		return fleetlog.Registration{}, false
	}
	machine := strings.TrimSpace(owner.Machine)
	if machine == "" {
		machine = h.self
	}
	chatID := strings.TrimSpace(owner.ChatID)
	if chatID == "" {
		return fleetlog.Registration{}, false
	}
	watcher, err := fleetid.New(machine, fleetid.Chat, chatID)
	if err != nil {
		return fleetlog.Registration{}, false
	}
	target := h.EntityID(fleetid.Process, childID)
	if target == "" {
		return fleetlog.Registration{}, false
	}
	row, err := h.register(machine, fleetlog.Registration{
		WatcherID: watcher.String(), TargetID: target, Label: label,
		WatcherChatID: chatID, WatcherTabID: strings.TrimSpace(owner.TabID),
		OriginLaneID: strings.TrimSpace(owner.LaneID), OriginOperationID: strings.TrimSpace(owner.OperationID),
	})
	if err != nil {
		h.logf("[fleet] could not monitor process child %s for %s: %v", childID, machine, err)
		return fleetlog.Registration{}, false
	}
	if machine != h.self && mesh != nil {
		h.offerMonitor(ctx, mesh, machine, row)
	}
	return row, true
}

// offerMonitor tells the owner's daemon what it is owed, with the backlog from
// the cursor it answers with.
//
// A failure here is a delay, not a loss: the owner's next `fleet:monitor.sync`
// on link up enumerates every monitor this home holds for it and adopts the
// ones it does not know, which is the path P3 already built for a registration
// reply that never arrived.
func (h *fleetHome) offerMonitor(ctx context.Context, mesh fleetMonitorInvoker, machine string, row fleetlog.Registration) {
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := mesh.Invoke(callCtx, machine, fleetMonitorOfferChannel, registrationPayload(row)); err != nil {
		h.logf("[fleet] %s was not told it is owed %s yet (%v); its next sync will adopt it", machine, row.TargetID, err)
	}
}

// AckLocal advances what a watcher on THIS machine has consumed. It is the
// in-process form of handleAck, and it is what finally lets a terminal event
// leave the log: nothing is forgotten before it is consumed.
func (h *fleetHome) AckLocal(targetID string, through uint64) {
	if !h.ready() || through == 0 {
		return
	}
	for _, row := range h.monitors.ByTarget(strings.TrimSpace(targetID)) {
		if row.WatcherHome() != h.self {
			continue
		}
		_, _ = h.monitors.Update(row.Ref, func(entry *fleetlog.Registration) {
			if through > entry.AckedSeq {
				entry.AckedSeq = through
			}
		})
	}
}

// processOwner is the chat or turn owed a process child's outcome.
type processOwner struct {
	Machine     string
	ChatID      string
	TabID       string
	LaneID      string
	OperationID string
}

// resolveProcessOwner reads the --owner a supervisor declared, falling back to
// the chat that holds the credential.
//
// A declared owner may name another machine, which is the whole point: a job
// run on the box with the GPU reports into the chat that asked for it. An
// owner naming a chat is accepted; one naming a turn is reduced to its chat,
// because a notice is delivered to a chat and the turn travels separately as
// the origin stamp.
func resolveProcessOwner(declared, selfMachine, callerChatID, callerTabID, laneID, operationID string) (processOwner, error) {
	owner := processOwner{
		Machine: strings.TrimSpace(selfMachine), ChatID: strings.TrimSpace(callerChatID),
		TabID: strings.TrimSpace(callerTabID), LaneID: strings.TrimSpace(laneID),
		OperationID: strings.TrimSpace(operationID),
	}
	declared = strings.TrimSpace(declared)
	if declared == "" {
		return owner, nil
	}
	if !fleetid.IsHomed(declared) {
		return processOwner{}, errors.New("--owner must be a global fleet id, for example wa:m-7f3ad2/chat/app-chat-1792187130561")
	}
	id, err := fleetid.Parse(declared)
	if err != nil {
		// A legacy machine-tagged id still resolves, per the identifier law.
		home, local, homed, splitErr := fleetid.Split(declared, fleetid.Chat)
		if splitErr != nil || !homed || strings.TrimSpace(local) == "" {
			return processOwner{}, err
		}
		owner.Machine, owner.ChatID = home, local
		if owner.Machine == strings.TrimSpace(selfMachine) {
			return owner, nil
		}
		owner.forgetLocalContext()
		return owner, nil
	}
	switch id.Type {
	case fleetid.Chat:
		owner.ChatID = id.Local
	case fleetid.Turn:
		// A turn names the chat that runs it; the turn itself becomes the
		// origin stamp, which is what binds the notice to the right lane.
		owner.OperationID = id.Local
	default:
		return processOwner{}, fmt.Errorf("--owner must name a chat or a turn, not a %s", string(id.Type))
	}
	owner.Machine = id.Home
	if owner.Machine != strings.TrimSpace(selfMachine) {
		owner.forgetLocalContext()
	}
	if strings.TrimSpace(owner.ChatID) == "" {
		return processOwner{}, errors.New("--owner names a turn but no chat; give the chat's global id")
	}
	return owner, nil
}

// forgetLocalContext drops everything about the owner that is only true HERE.
//
// A tab, a lane and a running turn are ids in THIS machine's chat. Exported to
// a chat on another machine they name nothing — and they are not merely
// useless, they are wrong in a way that fails silently: the notice admission
// fence compares the tab exactly and drops a mismatch, and it refuses a lane
// it cannot find. That is how four live jobs with a declared remote owner
// reported perfectly and told nobody. The owner's own machine fills these in
// from what it knows (RegisterOffer), and an absent lane falls through to the
// chat's desired lane, exactly as a local completion with no origin lane does.
func (o *processOwner) forgetLocalContext() {
	o.TabID, o.LaneID, o.OperationID = "", "", ""
}

// RegisterOffer is the watcher's side of fleet:monitor.offer.
//
// Authorization is the mirror of the home's: the target must be homed on the
// peer that is offering — a peer cannot make this daemon watch a third
// machine's entity — and the watcher must be a chat that exists here, because
// a monitor for a chat that is gone is a leak the home cannot detect.
func (w *fleetWatcher) RegisterOffer(peer string, params map[string]any) (any, error) {
	if !w.ready() {
		return nil, errors.New("this daemon holds no fleet monitors")
	}
	peer = strings.TrimSpace(peer)
	targetID := strings.TrimSpace(fieldString(params, "targetId"))
	watcherID := strings.TrimSpace(fieldString(params, "watcherId"))
	if targetID == "" || watcherID == "" {
		return nil, errors.New("a monitor offer needs a watcher id and a target id")
	}
	target, err := fleetid.Parse(targetID)
	if err != nil {
		return nil, err
	}
	if target.Home != peer {
		return nil, fmt.Errorf("%s is homed on %s, so %s may not offer a monitor on it", targetID, target.Home, peer)
	}
	watcher, err := fleetid.Parse(watcherID)
	if err != nil {
		return nil, err
	}
	if watcher.Home != w.self {
		return nil, fmt.Errorf("%s is not a watcher on this machine", watcherID)
	}
	chatID := strings.TrimSpace(fieldString(params, "watcherChatId"))
	tabID := strings.TrimSpace(fieldString(params, "watcherTabId"))
	if chatID == "" {
		chatID = watcher.Local
	}
	if tabID == "" && w.chatTab != nil {
		// The offering machine could not know this chat's tab, and the notice
		// admission fence compares it exactly: an empty tab matched nothing and
		// was dropped in silence. This machine owns the chat, so it answers.
		tabID = strings.TrimSpace(w.chatTab(chatID))
	}
	if w.chatExists != nil && !w.chatExists(tabID, chatID) {
		if tabID != "" {
			// The offering machine may not know this chat's tab; try the chat
			// alone before refusing, because refusing would strand real work.
			if !w.chatExists("", chatID) {
				return nil, fmt.Errorf("no chat %q on this machine owns that work", chatID)
			}
			tabID = ""
		} else {
			return nil, fmt.Errorf("no chat %q on this machine owns that work", chatID)
		}
	}
	row, err := w.Record(fleetlog.Registration{
		WatcherID: watcherID, TargetID: targetID, Label: fieldString(params, "label"),
		WatcherChatID: chatID, WatcherTabID: tabID,
		OriginLaneID: fieldString(params, "originLaneId"), OriginOperationID: fieldString(params, "originOperationId"),
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "ref": row.Ref, "afterSeq": row.AppliedSeq}, nil
}

// RegisterOfferChannel exposes the one watcher-side channel to paired peers.
func (w *fleetWatcher) RegisterOfferChannel(hub *wire.Hub) bool {
	if hub == nil || !w.ready() {
		return false
	}
	hub.RegisterWithCaller(fleetMonitorOfferChannel, func(caller wire.Caller, args []any) (any, error) {
		peer := strings.TrimSpace(caller.Peer)
		if peer == "" {
			return nil, errors.New("the fleet monitor channels are for paired daemons; this caller is not one")
		}
		return w.RegisterOffer(peer, firstMapArg(args))
	})
	return true
}

// IngestLocalEvent applies one event whose watcher is this daemon itself, and
// reports the sequence it reached so the home can retire it.
//
// This is the local half of "local == remote": the same project() decides what
// becomes a notice, the same dedup by seq makes redelivery free, and the same
// durable monitor means a crash between applying and acknowledging costs one
// re-application that changes nothing.
func (w *fleetWatcher) IngestLocalEvent(event fleetlog.Event, head uint64) uint64 {
	if !w.ready() {
		return 0
	}
	row, ok := w.monitorFor(w.self, event.Entity)
	if !ok {
		return 0
	}
	w.applySlice(context.Background(), w.self, row.Ref, map[string]any{
		"targetId": event.Entity, "head": head,
		"events": []any{eventPayload(event)},
	})
	if updated, found := w.monitors.Get(row.Ref); found {
		return updated.AppliedSeq
	}
	return 0
}

// spawnedWorkReceiptPayload carries a process child's bounded terminal receipt
// across a link in exactly the fields the local projection reads, so the notice
// a remote chat renders is byte-identical to the one the hosting chat renders.
// Text was redacted before it was persisted and is redacted again on arrival.
func spawnedWorkReceiptPayload(receipt acp.SpawnedWorkReceipt) map[string]any {
	return map[string]any{
		"receiptId": receipt.ReceiptID, "taskId": receipt.TaskID, "toolCallId": receipt.ToolCallID,
		"tabId": receipt.TabID, "chatId": receipt.ChatID, "kind": receipt.Kind, "label": receipt.Label,
		"role": receipt.Role, "status": receipt.Status, "startedAt": receipt.StartedAt,
		"finishedAt": receipt.FinishedAt, "elapsedMs": receipt.ElapsedMs,
		"outputFile": receipt.OutputFile, "pid": intPointerPayload(receipt.PID),
		"exitCode": intPointerPayload(receipt.ExitCode), "summary": receipt.Summary,
		"outputTail": receipt.OutputTail, "tailLimited": receipt.TailLimited,
		"modelLabel": receipt.ModelLabel,
	}
}

func spawnedWorkReceiptFromPayload(payload map[string]any) acp.SpawnedWorkReceipt {
	receipt := acp.SpawnedWorkReceipt{
		ReceiptID: fieldString(payload, "receiptId"), TaskID: fieldString(payload, "taskId"),
		ToolCallID: fieldString(payload, "toolCallId"), TabID: fieldString(payload, "tabId"),
		ChatID: fieldString(payload, "chatId"), Kind: fieldString(payload, "kind"),
		Label: fieldString(payload, "label"), Role: fieldString(payload, "role"),
		Status: fieldString(payload, "status"), StartedAt: fieldString(payload, "startedAt"),
		FinishedAt: fieldString(payload, "finishedAt"), ElapsedMs: int64(intField(payload, "elapsedMs")),
		OutputFile: fieldString(payload, "outputFile"), Summary: fieldString(payload, "summary"),
		OutputTail:  redactedSessionString(fieldString(payload, "outputTail")),
		TailLimited: boolFieldValue(payload, "tailLimited"), ModelLabel: fieldString(payload, "modelLabel"),
	}
	if value, present := intFieldPresent(payload, "pid"); present {
		receipt.PID = &value
	}
	if value, present := intFieldPresent(payload, "exitCode"); present {
		receipt.ExitCode = &value
	}
	return receipt
}

// intPointerPayload keeps an absent pid or exit code ABSENT across a link. A
// nil that travelled as 0 would claim an exit code of success for a job whose
// code nobody ever read.
func intPointerPayload(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

// remoteProcessListTimeout bounds asking one home for its rows.
const remoteProcessListTimeout = 15 * time.Second

// watchedRemoteProcessChildren lists the supervised jobs one chat owns on other
// machines, by asking the homes it already holds monitors on.
//
// The monitor is the authority on which jobs those are, and that is the whole
// reason this is honest rather than a fleet-wide scan: a monitor exists because
// THIS chat was registered as the watcher of THAT job (§7), so the set is
// exactly this chat's own work and nothing else's. Each home is asked once,
// through the ordinary route, and answers with its own rows; a row that the
// home no longer holds is a settled job that has been compacted into its
// receipt, so it is absent rather than invented here.
func (h *agentControlHandler) watchedRemoteProcessChildren(
	ctx context.Context, tabID, chatID string, params map[string]any,
) ([]any, []any) {
	targets := h.watchedRemoteProcessTargets(tabID, chatID)
	if len(targets) == 0 {
		return nil, nil
	}
	machines := make([]string, 0, len(targets))
	for machine := range targets {
		machines = append(machines, machine)
	}
	sort.Strings(machines)
	rows := []any{}
	unreachable := []any{}
	for _, machine := range machines {
		wanted := targets[machine]
		// Bounded per home. A read that was local before this must not be
		// held open by one dark machine: the answer names it as unreachable
		// and the rest of the list arrives.
		askCtx, cancel := context.WithTimeout(ctx, remoteProcessListTimeout)
		answer, err := h.forwardRemoteAgentCall(askCtx, "process.list", machine, params)
		cancel()
		if err != nil {
			labels := make([]string, 0, len(wanted))
			for _, label := range wanted {
				labels = append(labels, label)
			}
			sort.Strings(labels)
			jobs := make([]any, 0, len(labels))
			for _, label := range labels {
				jobs = append(jobs, label)
			}
			unreachable = append(unreachable, map[string]any{
				"machine_id": machine, "jobs": jobs,
				"reason": acp.RedactSensitiveText(err.Error()),
			})
			continue
		}
		for _, raw := range anySlice(mapFromAnyMain(answer)["children"]) {
			row := mapFromAnyMain(cloneJSON(raw))
			if row == nil {
				continue
			}
			if _, ours := wanted[strings.TrimSpace(fieldString(row, "work_id"))]; !ours {
				// Another chat's job on the same machine. The home answers
				// machine-scoped because a resource queue is a machine's fact;
				// what this chat is owed is still only its own work.
				continue
			}
			// Where it runs, which is a different fact from who owns it (§6)
			// and the one thing a merged list must not leave the reader to
			// guess at.
			row["host_machine"] = machine
			normalizeRemoteProcessNumbers(row)
			rows = append(rows, row)
		}
	}
	return rows, unreachable
}

// watchedRemoteProcessTargets maps each other machine to the process ids this
// chat watches there, with the label the monitor carries.
func (h *agentControlHandler) watchedRemoteProcessTargets(tabID, chatID string) map[string]map[string]string {
	if h == nil || h.fleetWatch == nil || h.fleetWatch.monitors == nil {
		return nil
	}
	chatID, tabID = strings.TrimSpace(chatID), strings.TrimSpace(tabID)
	if chatID == "" {
		return nil
	}
	targets := map[string]map[string]string{}
	for _, row := range h.fleetWatch.monitors.All() {
		if row.Consumed || strings.TrimSpace(row.WatcherChatID) != chatID {
			continue
		}
		if tab := strings.TrimSpace(row.WatcherTabID); tab != "" && tabID != "" && tab != tabID {
			continue
		}
		machine, local, homed, err := fleetid.Split(row.TargetID, fleetid.Process)
		if err != nil || !homed || local == "" {
			continue
		}
		if machine == "" || machine == strings.TrimSpace(h.selfMachineID) {
			continue
		}
		if targets[machine] == nil {
			targets[machine] = map[string]string{}
		}
		label := strings.TrimSpace(row.Label)
		if label == "" {
			label = local
		}
		targets[machine][local] = label
	}
	return targets
}

// remoteProcessNumberFields are the fields of a listed job that are numbers.
//
// They are named because a forwarded row's numbers are whatever the transport
// made of them — a JSON number on one path, a decimal STRING on another — and
// a merged list whose local rows carry 0 and whose remote rows carry "0" is a
// list nobody can write one comparison against. The string encoding already
// cost every exit code of every job reported over a link once.
var remoteProcessNumberFields = []string{
	"pid", "supervisor_pid", "process_group", "exit_code", "queue_position",
	"max_runtime_ms", "stale_after_ms", "max_hold_ms", "heartbeat_ms", "report_gap_ms",
}

// normalizeRemoteProcessNumbers puts one forwarded row's numbers back in the
// shape the local rows use. An absent field stays absent: a nil exit code that
// arrived as nothing must not become a zero, which would claim success for a
// job whose code nobody read.
func normalizeRemoteProcessNumbers(row map[string]any) {
	for _, field := range remoteProcessNumberFields {
		value, present := row[field]
		if !present {
			continue
		}
		text := ""
		switch typed := value.(type) {
		case string:
			text = strings.TrimSpace(typed)
			if text == "" {
				// An empty string is not a zero. It is a field the other side
				// had no value for, and a zero exit code is a claim of success.
				delete(row, field)
				continue
			}
		case json.Number:
			text = typed.String()
		default:
			continue
		}
		if number, err := strconv.ParseInt(text, 10, 64); err == nil {
			row[field] = number
		}
	}
}
