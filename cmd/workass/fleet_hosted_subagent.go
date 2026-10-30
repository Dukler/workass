package main

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

// Where it runs, and who owns it.
//
// docs/specs/fleet-runtime.md §6 states these as two facts. Until this file
// they were one, and the consequence was not subtle: the only machine-targeting
// `workass_spawn_subagent` offered was the machine-tagged pair of a chat on the
// other machine, so a coordinator asking for a subagent OF ITS OWN CHAT, hosted
// on another box, got a child of the other box's chat. The home registered the
// row, the home's chat showed the work, and the chat that asked showed nothing
// until the completion notice arrived. The operator's reading of the first live
// P1–P4 test was exact: "not a subagent, not even a child chat" (2026-10-30).
//
// With a host target the two halves live where they belong:
//
//   - the HOST runs the child with its own defaults, registers no row, and
//     appends the child's events to the child's log, as P3 already does;
//   - the OWNER — the calling chat, on its own machine — holds the row, the
//     monitor and the notice.
//
// Both halves share one operation id, so a retried spawn produces one child
// there and one row here (§6).

// hostedSpawnForOrigin is `agent.spawn` arriving from another machine with a
// host target and no chat here to own it.
//
// It deliberately does NOT go through the chat actor: there is no chat actor to
// go through, and inventing one would recreate the orphan this path exists to
// remove. The child's durable record is its log at this home; the asking chat's
// durable record is its own row and monitor at its own home. One writer each.
func (h *agentControlHandler) hostedSpawnForOrigin(ctx context.Context, forwarded fleetForwardedWork, params map[string]any) (any, error) {
	if h == nil || h.manager == nil {
		return nil, errors.New("subagent spawn requires the ACP manager")
	}
	if forwarded.Origin.empty() || strings.TrimSpace(forwarded.Origin.ChatID) == "" {
		// Without an origin there is nobody to report to, and a child nobody is
		// owed is the exact dishonesty the fleet runtime exists to remove.
		return nil, errors.New("a hosted subagent needs the chat that asked: pass machine_id from a Workass agent on another fleet machine, or name a chat on this machine with its machine-tagged tab_id + chat_id")
	}
	if boolFieldValue(params, "fork") {
		// §9: a native session fork must run where the session file is. The
		// asking chat's session is on the asking machine.
		return nil, errors.New("fork=true forks the calling chat's native provider session, which lives on the calling machine; a subagent hosted on another machine starts fresh (fleet-runtime §9)")
	}
	return h.manager.SpawnSubagent(ctx, acp.SubagentSpawnOptions{
		HostedForOrigin: true,
		OriginMachine:   forwarded.Origin.Machine,
		OriginChatID:    forwarded.Origin.ChatID,
		OriginTabID:     forwarded.Origin.TabID,
		// One operation id for "launch there" and "record here" (§6).
		OriginOperationID: forwarded.OperationID,
		// The caller's own ceiling, which this child can only narrow (§6, §11).
		LimitIntent:      forwarded.Limit,
		Prompt:           fieldString(params, "prompt"),
		Label:            acp.SanitizeDisplayText(fieldString(params, "label"), 80),
		ProviderID:       fieldString(params, "provider_id"),
		ModelID:          fieldString(params, "model_id"),
		Effort:           fieldString(params, "effort"),
		ModeID:           fieldString(params, "mode_id"),
		CWD:              fieldString(params, "cwd"),
		Profile:          fieldString(params, "profile"),
		PermissionIntent: fieldString(params, "permission_intent"),
	})
}

// hostedSpawnIsForThisMachine reports a forwarded `agent.spawn` that named this
// machine as the HOST and no chat here as the owner.
func hostedSpawnIsForThisMachine(method string, params map[string]any) bool {
	return method == "agent.spawn" &&
		strings.TrimSpace(fieldString(params, "tab_id")) == "" &&
		strings.TrimSpace(fieldString(params, "chat_id")) == ""
}

// recordHostedSubagent is the caller's half: the row, in the chat that asked,
// for a child running on another machine.
//
// It runs beside the monitor registration and for the same reason. The monitor
// makes the OUTCOME survive this daemon restarting; the row makes the child
// VISIBLE for as long as it runs. Without it the asking conversation showed no
// sign of work it had started until that work was already over.
func (h *agentControlHandler) recordHostedSubagent(machineID, tabID, chatID, laneID, operationID string, result any) {
	if h == nil || h.manager == nil {
		return
	}
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	fields := mapFromAnyMain(result)
	subagentID := strings.TrimSpace(fieldString(fields, "id"))
	if tabID == "" || chatID == "" || subagentID == "" {
		return
	}
	h.manager.RegisterHostedSubagent(tabID, chatID, acp.HostedSubagentRow{
		SubagentID:        subagentID,
		Label:             fieldString(fields, "label"),
		ModelLabel:        fieldString(fields, "modelLabel"),
		ProviderID:        fieldString(fields, "providerId"),
		HostMachine:       strings.TrimSpace(machineID),
		HostMachineName:   h.fleetPeerName(machineID),
		StartedAt:         fieldString(fields, "startedAt"),
		Phase:             fieldString(fields, "phase"),
		Activity:          fieldString(fields, "latestActivity"),
		OriginLaneID:      laneID,
		OriginOperationID: operationID,
		OriginTurnID:      fieldString(fields, "rootJobId"),
	})
}

// fleetPeerName is the human name of a peer machine, as the mesh knows it. It
// is cosmetic: an unnamed peer is shown by its machine id, never guessed at.
func (h *agentControlHandler) fleetPeerName(machineID string) string {
	machineID = strings.TrimSpace(machineID)
	if h == nil || h.fleetMesh == nil || machineID == "" {
		return ""
	}
	for _, state := range h.fleetMesh.States() {
		if strings.TrimSpace(state.Peer) == machineID {
			return fleetDisplayName(state.Name, machineID)
		}
	}
	return ""
}

// settleHostedSubagentRow settles the asking chat's row from a monitor exit.
// The notice is delivered by the ordinary completion path; this is the row that
// notice is about, which would otherwise stay `running` forever.
func (r *providerChatRuntime) settleHostedSubagentRow(tabID, chatID string, receipt acp.SubagentReceipt) {
	if r == nil || r.manager == nil {
		return
	}
	r.manager.SettleHostedSubagent(tabID, chatID, receipt)
	r.settleHostedSubagentCard(tabID, chatID, receipt)
}

// settleHostedSubagentCard makes the child's card in the turn that started it say
// how the child ended.
//
// A child on another machine can outlive the turn and this daemon. A restart here
// ends that turn as interrupted and every open tool in it as failed, the child's
// card with them, and the live event that would correct it has no lane to travel
// on: the row and the notice were right and the transcript said "falló" for good.
func (r *providerChatRuntime) settleHostedSubagentCard(tabID, chatID string, receipt acp.SubagentReceipt) {
	id := strings.TrimSpace(receipt.SubagentID)
	// The monitor recorded the same turn when the child started, so the end still
	// finds its card if the row is gone.
	operationID := providercontract.NormalizeOperationID(firstNonEmptyString(r.manager.HostedSubagentOriginOperation(tabID, chatID, id), receipt.OriginOperationID))
	if operationID == "" {
		return
	}
	status := "completed"
	switch strings.TrimSpace(receipt.Status) {
	case "failed":
		status = "failed"
	case "cancelled":
		status = "cancelled"
	}
	actor, err := r.actor(strings.TrimSpace(chatID))
	if err != nil {
		return
	}
	actor.mu.Lock()
	defer actor.mu.Unlock()
	current, found := chat.SubagentCardStatus(actor.engine.ReadSnapshot(), operationID, id)
	if !found || current == status {
		return
	}
	tool := providercontract.ToolEvent{ToolCallID: id, Status: status}
	if at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(receipt.FinishedAt)); err == nil {
		tool.EndedAtUnixMS = at.UnixMilli()
	}
	if err := actor.engine.Apply(chat.SettleSubagentCard{OperationID: operationID, Tool: tool}); err != nil {
		log.Printf("[fleet] could not correct the card of %s: %v", id, err)
	}
}
