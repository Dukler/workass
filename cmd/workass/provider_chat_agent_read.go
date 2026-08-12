package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

// The agent-control read surface is deliberately bounded by the chat actor.
// ACP's manager remains the executor for a live wait and for optional output
// tails, but it is never allowed to decide which chat/work item a caller can
// read. In particular, a manager record must not resurrect a deleted chat or
// let a stale tab address a new attachment.
const (
	actorAgentReceiptDefaultLimit = 32
	actorAgentMaxReceiptLimit     = 256
	actorAgentMaxSubagents        = 256
	actorAgentMaxSpawnedWork      = 256
	actorAgentOutputTailChars     = 12000
)

const agentOwnerReadError = "no running Workass turn owns this subagent request"

func (r *providerChatRuntime) agentReadFence(ownerKey, tabID, chatID string) (chat.State, error) {
	// This call must remain first. Manager owner capabilities are ephemeral; the
	// durable actor pair is the authority for all current-chat reads.
	_, state, err := r.exactActor(tabID, chatID)
	if err != nil {
		return chat.State{}, err
	}
	if !r.agentOwnerAuthorized(ownerKey, tabID, chatID) {
		return chat.State{}, errors.New(agentOwnerReadError)
	}
	return state, nil
}

func (r *providerChatRuntime) agentOwnerAuthorized(ownerKey, tabID, chatID string) bool {
	return r != nil && r.manager != nil && r.manager.ValidateAgentOwner(ownerKey, chatID, tabID)
}

func (r *providerChatRuntime) ListSubagents(ownerKey, tabID, chatID string) ([]acp.SubagentRun, error) {
	state, err := r.agentReadFence(ownerKey, tabID, chatID)
	if err != nil {
		return nil, err
	}
	return actorSubagentRuns(state), nil
}

func (r *providerChatRuntime) ListSubagentReceipts(ownerKey, tabID, chatID string, limit int) ([]acp.SubagentReceipt, error) {
	state, err := r.agentReadFence(ownerKey, tabID, chatID)
	if err != nil {
		return nil, err
	}
	receipts := actorSubagentReceipts(state)
	return trimActorReceipts(receipts, limit), nil
}

func (r *providerChatRuntime) WaitSubagent(ctx context.Context, ownerKey, tabID, chatID, id string, timeout time.Duration) (acp.SubagentRun, error) {
	state, err := r.agentReadFence(ownerKey, tabID, chatID)
	if err != nil {
		return acp.SubagentRun{}, err
	}
	background, ok := actorBackgroundByID(state, id)
	if !ok || !isActorSubagent(background.Event) {
		return acp.SubagentRun{}, errors.New("subagent not found for this owner")
	}
	if background.Event.Status != "running" {
		return actorSubagentRun(state, background), nil
	}
	if r.manager == nil {
		return acp.SubagentRun{}, errors.New("subagent wait runtime is unavailable")
	}
	run, waitErr := r.manager.WaitSubagent(ctx, ownerKey, chatID, tabID, strings.TrimSpace(id), timeout)
	if reconcileErr := r.reconcileAgentBackground(tabID, chatID); waitErr == nil && reconcileErr != nil {
		return acp.SubagentRun{}, fmt.Errorf("reconcile subagent state: %w", reconcileErr)
	}
	return run, waitErr
}

func (r *providerChatRuntime) WaitSubagents(ctx context.Context, ownerKey, tabID, chatID string, ids []string, returnWhen string, timeout time.Duration) (map[string]any, error) {
	state, err := r.agentReadFence(ownerKey, tabID, chatID)
	if err != nil {
		return nil, err
	}
	returnWhen = strings.ToLower(strings.TrimSpace(returnWhen))
	if returnWhen == "" {
		returnWhen = "first"
	}
	if returnWhen != "first" && returnWhen != "all" {
		return nil, errors.New("return_when must be first or all")
	}
	cleanIDs := uniqueAgentIDs(ids)
	if len(cleanIDs) == 0 {
		return nil, errors.New("at least one subagent id is required")
	}
	completed := make([]acp.SubagentRun, 0, len(cleanIDs))
	running := make([]string, 0, len(cleanIDs))
	for _, id := range cleanIDs {
		background, ok := actorBackgroundByID(state, id)
		if !ok || !isActorSubagent(background.Event) {
			return nil, errors.New("subagent not found for this owner")
		}
		if background.Event.Status == "running" {
			running = append(running, id)
		} else {
			completed = append(completed, actorSubagentRun(state, background))
		}
	}
	// A terminal actor row already satisfies the "first" condition. This is
	// important after daemon restart: the in-memory ACP registry may be gone,
	// while the actor still has the authoritative terminal result.
	if returnWhen == "first" && len(completed) > 0 {
		return map[string]any{
			"completed": completed, "running": actorSubagentRunsForIDs(state, running),
			"attention": []acp.SubagentRun{}, "needsAttention": false, "timedOut": false,
		}, nil
	}
	if len(running) == 0 {
		return map[string]any{
			"completed": completed, "running": []acp.SubagentRun{},
			"attention": []acp.SubagentRun{}, "needsAttention": false, "timedOut": false,
		}, nil
	}
	if r.manager == nil {
		return nil, errors.New("subagent wait runtime is unavailable")
	}
	result, waitErr := r.manager.WaitSubagents(ctx, ownerKey, chatID, tabID, running, returnWhen, timeout)
	if reconcileErr := r.reconcileAgentBackground(tabID, chatID); waitErr == nil && reconcileErr != nil {
		return nil, fmt.Errorf("reconcile subagent state: %w", reconcileErr)
	}
	if waitErr != nil {
		return nil, waitErr
	}
	return mergeActorWaitResult(result, completed), nil
}

func (r *providerChatRuntime) ListSpawnedWorkForOwner(ownerKey, parentChatID, parentTabID, requestedChatID, requestedTabID string, tailBytes int) ([]map[string]any, error) {
	_, state, err := r.exactActor(parentTabID, parentChatID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(requestedChatID) != strings.TrimSpace(parentChatID) || strings.TrimSpace(requestedTabID) != strings.TrimSpace(parentTabID) {
		return nil, errors.New("tab_id + chat_id must exactly match the owning Workass chat")
	}
	if !r.agentOwnerAuthorized(ownerKey, parentTabID, parentChatID) {
		return nil, errors.New(agentOwnerReadError)
	}
	if tailBytes < 0 || tailBytes > actorAgentOutputTailChars {
		return nil, errors.New("spawned work tail_chars must be between 0 and 12000")
	}
	rows := actorSpawnedWorkRows(state)
	if tailBytes == 0 || len(rows) == 0 {
		return rows, nil
	}
	if r.manager == nil {
		return nil, errors.New("spawned work output runtime is unavailable")
	}
	for _, row := range rows {
		id := firstNonEmptyString(fieldString(row, "id"), fieldString(row, "taskId"))
		live := r.manager.ReadSpawnedWork(parentTabID, parentChatID, id, tailBytes)
		// The actor row remains the response authority. Only the bounded output
		// tail is borrowed from the executor, and it is okay for a terminal file
		// to have disappeared since the actor committed its receipt.
		if live["ok"] == true {
			row["tail"] = live["tail"]
			row["tailLimited"] = live["tailLimited"]
		} else {
			row["tail"] = ""
			row["tailLimited"] = false
		}
	}
	return rows, nil
}

func (r *providerChatRuntime) ListSpawnedWorkReceipts(ownerKey, parentChatID, parentTabID, requestedChatID, requestedTabID string, limit int) ([]acp.SpawnedWorkReceipt, error) {
	_, state, err := r.exactActor(parentTabID, parentChatID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(requestedChatID) != strings.TrimSpace(parentChatID) || strings.TrimSpace(requestedTabID) != strings.TrimSpace(parentTabID) {
		return nil, errors.New("tab_id + chat_id must exactly match the owning Workass chat")
	}
	if !r.agentOwnerAuthorized(ownerKey, parentTabID, parentChatID) {
		return nil, errors.New(agentOwnerReadError)
	}
	receipts := actorSpawnedWorkReceipts(state)
	if r.manager != nil && len(receipts) > 0 {
		// Receipt fields come from the actor. Reading an output tail is an
		// executor-only enrichment and is intentionally best effort after the
		// durable actor fence.
		for i := range receipts {
			live := r.manager.ReadSpawnedWork(parentTabID, parentChatID, receipts[i].TaskID, actorAgentOutputTailChars)
			if live["ok"] == true {
				receipts[i].OutputTail = strings.TrimSpace(toString(live["tail"]))
				if limited, ok := live["tailLimited"].(bool); ok {
					receipts[i].TailLimited = limited
				}
			}
		}
	}
	return trimSpawnedWorkReceipts(receipts, limit), nil
}

// AgentOwnerCWD resolves the workspace from the actor presentation/lane
// snapshot. Manager.AgentOwnerCWD is intentionally not used: it is a live
// executor capability lookup and cannot be a chat-state authority or a
// fallback for a stale/deleted actor attachment.
func (r *providerChatRuntime) AgentOwnerCWD(ownerKey, tabID, chatID string) (string, error) {
	_, state, err := r.exactActor(tabID, chatID)
	if err != nil {
		return "", err
	}
	cwd := ""
	if state.Presentation.CWD != nil {
		cwd = strings.TrimSpace(*state.Presentation.CWD)
	}
	if cwd == "" {
		laneID := state.ActiveLaneID
		if laneID == "" {
			laneID = state.DesiredLaneID
		}
		if lane, ok := state.Lanes[laneID]; ok {
			cwd = strings.TrimSpace(lane.CWD)
		}
	}
	if cwd == "" {
		return "", errors.New("actor chat has no readable working directory")
	}
	absolute, err := filepath.Abs(filepath.Clean(cwd))
	if err != nil {
		return "", errors.New("actor chat working directory is invalid")
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		return "", errors.New("actor chat has no readable working directory")
	}
	// Validate the short-lived live capability only after the actor has
	// selected and checked the authoritative workspace.
	if !r.agentOwnerAuthorized(ownerKey, tabID, chatID) {
		return "", errors.New("no Workass agent session owns this artifact hosting request")
	}
	return absolute, nil
}

// reconcileAgentBackground is deliberately the existing observer ingress, not
// a second persistence path. A live wait can settle/advance manager runtime
// state; re-reading its exact pair feeds that evidence back through the same
// actor reducer used by ordinary spawned-work callbacks.
func (r *providerChatRuntime) reconcileAgentBackground(tabID, chatID string) error {
	if r == nil || r.manager == nil {
		return errors.New("background reconciliation runtime is unavailable")
	}
	_, err := r.applySpawnedWorkSnapshot(tabID, chatID, r.manager.ListSpawnedWork(tabID, chatID))
	return err
}

func actorBackgroundByID(state chat.State, id string) (chat.BackgroundState, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return chat.BackgroundState{}, false
	}
	for key, item := range state.Background {
		if key == id || strings.TrimSpace(item.Event.WorkID) == id || strings.TrimSpace(item.Event.TaskID) == id {
			return item, true
		}
	}
	return chat.BackgroundState{}, false
}

func isActorSubagent(event providercontract.BackgroundEvent) bool {
	return strings.EqualFold(strings.TrimSpace(event.Kind), "subagent")
}

func actorSubagentRuns(state chat.State) []acp.SubagentRun {
	out := make([]acp.SubagentRun, 0)
	for _, item := range state.Background {
		if isActorSubagent(item.Event) {
			out = append(out, actorSubagentRun(state, item))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StartedAt == out[j].StartedAt {
			return out[i].ID < out[j].ID
		}
		return out[i].StartedAt < out[j].StartedAt
	})
	if len(out) > actorAgentMaxSubagents {
		out = out[len(out)-actorAgentMaxSubagents:]
	}
	return out
}

func actorSubagentRunsForIDs(state chat.State, ids []string) []acp.SubagentRun {
	out := make([]acp.SubagentRun, 0, len(ids))
	for _, id := range ids {
		if item, ok := actorBackgroundByID(state, id); ok && isActorSubagent(item.Event) {
			out = append(out, actorSubagentRun(state, item))
		}
	}
	return out
}

func actorSubagentRun(state chat.State, item chat.BackgroundState) acp.SubagentRun {
	event := item.Event
	id := firstNonEmptyString(event.WorkID, event.TaskID)
	status := strings.TrimSpace(event.Status)
	switch status {
	case "exited":
		status = "done"
	case "orphaned":
		status = "failed"
	}
	providerID := ""
	if lane, ok := state.Lanes[item.Owner.LaneID]; ok {
		providerID = string(lane.Identity.Realm.ProviderID)
	}
	finished := strings.TrimSpace(event.FinishedAt)
	end := time.Now().UTC()
	if parsed, parseErr := time.Parse(time.RFC3339Nano, finished); parseErr == nil {
		end = parsed
	}
	started, parseErr := time.Parse(time.RFC3339Nano, event.StartedAt)
	elapsed := int64(0)
	if parseErr == nil && !end.Before(started) {
		elapsed = end.Sub(started).Milliseconds()
	}
	result := acp.RedactSensitiveText(strings.TrimSpace(event.ResultExcerpt))
	run := acp.SubagentRun{
		ID: id, Label: acp.RedactSensitiveText(event.Title), Status: status,
		ProviderID: providerID, ModelLabel: acp.RedactSensitiveText(event.ModelLabel),
		Phase: acp.RedactSensitiveText(event.LastToolName), LatestActivity: acp.RedactSensitiveText(event.Summary),
		LastActivityAt: event.UpdatedAt, ElapsedMs: elapsed, StartedAt: event.StartedAt, FinishedAt: finished,
		Result: result, RootJobID: item.Owner.TurnID, ReceiptID: id,
	}
	if status == "failed" {
		run.Error = result
		run.Result = ""
	}
	return run
}

func actorSubagentReceipts(state chat.State) []acp.SubagentReceipt {
	out := make([]acp.SubagentReceipt, 0)
	for _, item := range state.Background {
		if !isActorSubagent(item.Event) || strings.TrimSpace(item.Event.Status) == "running" {
			continue
		}
		run := actorSubagentRun(state, item)
		out = append(out, acp.SubagentReceipt{
			ReceiptID: run.ReceiptID, SubagentID: run.ID, Label: run.Label, Status: run.Status,
			ProviderID: run.ProviderID, ModelID: run.ModelID, Effort: run.Effort, ModelLabel: run.ModelLabel,
			ModeID: run.ModeID, Profile: run.Profile, StartedAt: run.StartedAt, FinishedAt: run.FinishedAt,
			ElapsedMs: run.ElapsedMs, Result: run.Result, Error: run.Error,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].FinishedAt == out[j].FinishedAt {
			return out[i].ReceiptID < out[j].ReceiptID
		}
		return out[i].FinishedAt < out[j].FinishedAt
	})
	return out
}

func trimActorReceipts(receipts []acp.SubagentReceipt, limit int) []acp.SubagentReceipt {
	if limit <= 0 || limit > actorAgentMaxReceiptLimit {
		limit = actorAgentReceiptDefaultLimit
	}
	if len(receipts) > limit {
		return receipts[len(receipts)-limit:]
	}
	return receipts
}

func uniqueAgentIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func mergeActorWaitResult(result map[string]any, completed []acp.SubagentRun) map[string]any {
	if result == nil {
		result = map[string]any{}
	}
	existing := make(map[string]struct{}, len(completed))
	for _, run := range completed {
		existing[run.ID] = struct{}{}
	}
	managerCompleted, _ := result["completed"].([]acp.SubagentRun)
	merged := append([]acp.SubagentRun(nil), completed...)
	for _, run := range managerCompleted {
		if _, ok := existing[run.ID]; ok {
			continue
		}
		merged = append(merged, run)
	}
	result["completed"] = merged
	return result
}

type actorSpawnedWorkProjection struct {
	item acp.SpawnedWorkItem
}

func actorSpawnedWorkItems(state chat.State) []actorSpawnedWorkProjection {
	items := make([]actorSpawnedWorkProjection, 0, len(state.Background))
	for _, background := range state.Background {
		items = append(items, actorSpawnedWorkProjection{item: actorBackgroundWorkItem(state, background)})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if (items[i].item.Status == "running") != (items[j].item.Status == "running") {
			return items[i].item.Status == "running"
		}
		if items[i].item.StartedAt == items[j].item.StartedAt {
			return items[i].item.ID < items[j].item.ID
		}
		return items[i].item.StartedAt > items[j].item.StartedAt
	})
	if len(items) > actorAgentMaxSpawnedWork {
		items = items[:actorAgentMaxSpawnedWork]
	}
	return items
}

func actorBackgroundWorkItem(state chat.State, background chat.BackgroundState) acp.SpawnedWorkItem {
	event := background.Event
	id := firstNonEmptyString(event.WorkID, event.TaskID)
	providerID := ""
	if lane, ok := state.Lanes[background.Owner.LaneID]; ok {
		providerID = string(lane.Identity.Realm.ProviderID)
	}
	return acp.SpawnedWorkItem{
		ID: id, TaskID: firstNonEmptyString(event.TaskID, id), ToolCallID: event.ToolCallID,
		TabID: state.Presentation.TabID, ChatID: state.ChatID, ProviderID: providerID,
		Kind: event.Kind, Label: acp.RedactSensitiveText(event.Title), Role: event.Role,
		Status: event.Status, StartedAt: event.StartedAt, UpdatedAt: event.UpdatedAt, FinishedAt: event.FinishedAt,
		OutputFile: acp.RedactSensitiveText(event.OutputFile), PID: cloneIntPointer(event.PID), ExitCode: cloneIntPointer(event.ExitCode),
		Summary: acp.RedactSensitiveText(event.Summary), LastToolName: acp.RedactSensitiveText(event.LastToolName),
		ModelLabel: acp.RedactSensitiveText(event.ModelLabel), ResultExcerpt: acp.RedactSensitiveText(event.ResultExcerpt),
		OriginLaneID: string(background.Owner.LaneID), OriginOperationID: string(background.Owner.OperationID), OriginTurnID: background.Owner.TurnID,
	}
}

func actorSpawnedWorkRows(state chat.State) []map[string]any {
	items := actorSpawnedWorkItems(state)
	rows := make([]map[string]any, 0, len(items))
	for _, projection := range items {
		encoded, _ := json.Marshal(projection.item)
		var row map[string]any
		if json.Unmarshal(encoded, &row) == nil {
			rows = append(rows, row)
		}
	}
	return rows
}

func actorSpawnedWorkReceipts(state chat.State) []acp.SpawnedWorkReceipt {
	items := actorSpawnedWorkItems(state)
	receipts := make([]acp.SpawnedWorkReceipt, 0, len(items))
	for _, projection := range items {
		item := projection.item
		if strings.TrimSpace(item.Status) == "running" {
			continue
		}
		started, startErr := time.Parse(time.RFC3339Nano, item.StartedAt)
		finished, finishErr := time.Parse(time.RFC3339Nano, item.FinishedAt)
		elapsed := int64(0)
		if startErr == nil && finishErr == nil && !finished.Before(started) {
			elapsed = finished.Sub(started).Milliseconds()
		}
		receipts = append(receipts, acp.SpawnedWorkReceipt{
			ReceiptID: "spawned-" + item.TaskID, TaskID: item.TaskID, ToolCallID: item.ToolCallID,
			TabID: item.TabID, ChatID: item.ChatID, Kind: item.Kind, Label: item.Label, Role: item.Role,
			Status: item.Status, StartedAt: item.StartedAt, FinishedAt: item.FinishedAt, ElapsedMs: elapsed,
			OutputFile: item.OutputFile, PID: cloneIntPointer(item.PID), ExitCode: cloneIntPointer(item.ExitCode),
			Summary: item.Summary,
		})
	}
	sort.SliceStable(receipts, func(i, j int) bool {
		if receipts[i].FinishedAt == receipts[j].FinishedAt {
			return receipts[i].ReceiptID < receipts[j].ReceiptID
		}
		return receipts[i].FinishedAt < receipts[j].FinishedAt
	})
	return receipts
}

func trimSpawnedWorkReceipts(receipts []acp.SpawnedWorkReceipt, limit int) []acp.SpawnedWorkReceipt {
	if limit <= 0 || limit > actorAgentMaxReceiptLimit {
		limit = actorAgentReceiptDefaultLimit
	}
	if len(receipts) > limit {
		return receipts[len(receipts)-limit:]
	}
	return receipts
}

func cloneIntPointer(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
