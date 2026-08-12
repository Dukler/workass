package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

func stableBackgroundOperationID(kind chat.BackgroundActionKind, tabID, chatID, requestID string) providercontract.OperationID {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"background-v1", string(kind), strings.TrimSpace(tabID), strings.TrimSpace(chatID), strings.TrimSpace(requestID),
	}, "\x00")))
	return providercontract.OperationID(fmt.Sprintf("background:%x", sum[:16]))
}

// executeBackgroundAction is the one adapter from actor-owned background
// commands to runtime mechanics. It contains no provider branches and receives
// no persisted bearer credential.
func (r *providerChatRuntime) executeBackgroundAction(ctx context.Context, action chat.BackgroundAction) (json.RawMessage, error) {
	if r == nil || r.manager == nil {
		return nil, errors.New("background runtime is unavailable")
	}
	result, err := r.manager.WithActorOwner(action.ChatID, action.TabID, func(ownerKey string) (any, error) {
		switch action.Kind {
		case chat.BackgroundSpawnAgent:
			value := action.Spawn
			return r.manager.SpawnSubagent(ctx, acp.SubagentSpawnOptions{
				OwnerKey: ownerKey, ParentChatID: action.ChatID, ParentTabID: action.TabID,
				RootJobIDHint: action.Owner.TurnID, Prompt: value.Prompt, Label: value.Label,
				ProviderID: value.ProviderID, ModelID: value.ModelID, Effort: value.Effort,
				ModeID: value.ModeID, CWD: value.CWD, Profile: value.Profile, PermissionIntent: value.PermissionIntent,
			})
		case chat.BackgroundMessageAgent:
			return r.manager.MessageSubagent(ownerKey, action.ChatID, action.TabID, action.Message.WorkID, action.Message.Message)
		case chat.BackgroundRetryAgent:
			return r.manager.RetrySubagent(ctx, ownerKey, action.ChatID, action.TabID, action.Retry.WorkID, action.Retry.Message)
		case chat.BackgroundCancelAgent:
			ok := r.manager.CancelSubagent(ownerKey, action.ChatID, action.TabID, action.Cancel.WorkID)
			if !ok {
				return nil, errors.New("subagent is no longer cancellable")
			}
			return map[string]any{"ok": true}, nil
		case chat.BackgroundAgentPermission:
			return r.manager.DecideSubagentPermission(ownerKey, action.ChatID, action.TabID, action.Permission.WorkID, action.Permission.Decision)
		case chat.BackgroundRegisterExternal:
			value := action.Register
			return r.manager.RegisterExternalWork(acp.ExternalWorkRegistrationOptions{
				OwnerKey: ownerKey, ParentChatID: action.ChatID, ParentTabID: action.TabID,
				TabID: action.TabID, ChatID: action.ChatID, Label: value.Label, Role: value.Role,
				PID: value.PID, OutputFile: value.OutputFile, DoneFile: value.DoneFile,
				OriginLaneID: string(action.Owner.LaneID), OriginOperationID: string(action.Owner.OperationID), OriginTurnID: action.Owner.TurnID,
			})
		case chat.BackgroundSettleExternal:
			value := action.Settle
			return r.manager.SettleExternalWork(acp.ExternalWorkSettleOptions{
				OwnerKey: ownerKey, ParentChatID: action.ChatID, ParentTabID: action.TabID,
				TabID: action.TabID, ChatID: action.ChatID, WorkID: value.WorkID,
				Status: value.Status, ExitCode: value.ExitCode, Summary: value.Summary,
			})
		case chat.BackgroundStopWork:
			result := r.manager.StopSpawnedWork(action.TabID, action.ChatID, action.Stop.WorkID)
			if result["ok"] != true {
				return nil, errors.New(firstNonEmptyString(fieldString(result, "error"), "spawned work could not be stopped"))
			}
			return result, nil
		default:
			return nil, fmt.Errorf("unsupported actor background action %q", action.Kind)
		}
	})
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode background action receipt: %w", err)
	}
	return raw, nil
}

// RunBackgroundAction journals one authenticated mutation before the manager
// can observe it. Repeating the same OperationID returns the durable receipt;
// a dispatched-but-unconfirmed operation remains ambiguous and is never sent
// again.
func (r *providerChatRuntime) RunBackgroundAction(ctx context.Context, tabID, chatID string, action chat.BackgroundAction) (any, error) {
	actor, state, err := r.exactActor(tabID, chatID)
	if err != nil {
		return nil, err
	}
	action.TabID, action.ChatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	action.OperationID = providercontract.NormalizeOperationID(string(action.OperationID))
	if action.OperationID == "" {
		return nil, errors.New("background mutation requires a stable operation id")
	}
	if target := action.TargetWorkID(); target != "" {
		item, ok := state.Background[target]
		if !ok {
			return nil, fmt.Errorf("background work %q is not actor-owned", target)
		}
		action.Owner = item.Owner
	} else {
		if state.Foreground != nil {
			lane := state.Lanes[state.Foreground.LaneID]
			action.Owner = chat.ProviderActivityOwner{
				LaneID: state.Foreground.LaneID, OperationID: state.Foreground.OperationID,
				TurnID: state.Foreground.Turn.NativeID, ConnectionGeneration: lane.ConnectionGeneration,
			}
		} else {
			for i := len(state.Ledger) - 1; i >= 0; i-- {
				event := state.Ledger[i]
				if event.LaneID == "" || event.OperationID == "" {
					continue
				}
				lane := state.Lanes[event.LaneID]
				action.Owner = chat.ProviderActivityOwner{
					LaneID: event.LaneID, OperationID: event.OperationID, TurnID: event.NativeTurnID,
					ConnectionGeneration: lane.ConnectionGeneration,
				}
				break
			}
			if action.Owner.OperationID == "" {
				return nil, errors.New("new background work requires an actor-owned turn")
			}
		}
	}

	actor.mu.Lock()
	state = actor.engine.Snapshot()
	if _, exists := state.Operations[action.OperationID]; !exists {
		err = actor.engine.Apply(chat.RequestBackgroundAction{Action: action})
	}
	actor.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := actor.coordinator.Drain(ctx); err != nil {
		return nil, err
	}
	state = actor.engine.Snapshot()
	for _, entry := range state.Outbox {
		if entry.Kind != chat.EffectBackground || entry.OperationID != action.OperationID {
			continue
		}
		switch entry.Status {
		case chat.OutboxCompleted:
			var result any
			if len(entry.Result) == 0 {
				return map[string]any{"ok": true}, nil
			}
			decoder := json.NewDecoder(strings.NewReader(string(entry.Result)))
			decoder.UseNumber()
			if err := decoder.Decode(&result); err != nil {
				return nil, errors.New("background action receipt is corrupt")
			}
			return result, nil
		case chat.OutboxAmbiguous:
			return nil, &providercontract.Error{Kind: providercontract.ErrorAcceptanceAmbiguous, Operation: action.OperationID, Message: "background action acceptance is uncertain; it was not resent"}
		case chat.OutboxFailed:
			return nil, &providercontract.Error{Kind: entry.LastError, Operation: action.OperationID, Message: "background action failed"}
		default:
			return nil, &providercontract.Error{Kind: providercontract.ErrorTransientTransport, Operation: action.OperationID, Message: "background action has no terminal receipt"}
		}
	}
	return nil, errors.New("background action has no durable outbox record")
}

func (r *providerChatRuntime) applySpawnedWorkSnapshot(tabID, chatID string, items []acp.SpawnedWorkItem) (acp.SpawnedWorkActorProjection, error) {
	actor, state, err := r.exactActor(tabID, chatID)
	if err != nil {
		return acp.SpawnedWorkActorProjection{}, err
	}
	background := make([]chat.BackgroundState, 0, len(items))
	for _, item := range items {
		workID := firstNonEmptyString(strings.TrimSpace(item.ID), strings.TrimSpace(item.TaskID))
		if workID == "" {
			return acp.SpawnedWorkActorProjection{}, errors.New("runtime background snapshot contains an item without identity")
		}
		owner, ok := exactBackgroundOwner(state, item)
		if !ok {
			return acp.SpawnedWorkActorProjection{}, fmt.Errorf("runtime background work %q has no exact actor owner", workID)
		}
		background = append(background, chat.BackgroundState{Owner: owner, Event: backgroundEvent(item, workID)})
	}
	actor.mu.Lock()
	err = actor.engine.Apply(chat.ReconcileBackgroundSnapshot{
		Items: background, AuthoritativeAbsence: true, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err == nil {
		evidence := r.manager.ChatObligationEvidence(tabID, chatID)
		err = actor.engine.Apply(chat.ReconcileObligation{
			ObservedAt:   time.Now().UTC().Format(time.RFC3339Nano),
			LiveEvidence: evidence.Live, HarnessQuiet: evidence.HarnessQuiet,
		})
	}
	state = actor.engine.Snapshot()
	actor.mu.Unlock()
	if err != nil {
		return acp.SpawnedWorkActorProjection{}, err
	}
	return acp.SpawnedWorkActorProjection{
		ActorRevision: state.Revision, Obligation: actorObligationProjection(state.Obligation),
	}, nil
}

func actorObligationProjection(value *chat.ObligationState) *acp.ChatObligationProjection {
	if value == nil {
		return nil
	}
	return &acp.ChatObligationProjection{
		State: value.State, Source: value.Source, Note: value.Note, PromptID: value.PromptID,
	}
}

// syncSpawnedWorkSnapshots performs the one startup reconciliation required
// because the manager loads its process/liveness records before chat actors are
// opened. Actor state remains authoritative: an absent executor snapshot never
// erases already-durable actor rows.
func (r *providerChatRuntime) syncSpawnedWorkSnapshots() error {
	ids, err := r.knownChatIDs()
	if err != nil {
		return err
	}
	for _, chatID := range ids {
		actor, err := r.actor(chatID)
		if err != nil {
			return err
		}
		state := actor.engine.Snapshot()
		tabID := strings.TrimSpace(state.Presentation.TabID)
		if tabID == "" {
			return fmt.Errorf("background reconciliation chat %q has no tab attachment", chatID)
		}
		items := r.manager.ListSpawnedWork(tabID, chatID)
		if _, err := r.applySpawnedWorkSnapshot(tabID, chatID, items); err != nil {
			return fmt.Errorf("reconcile background work for chat %q: %w", chatID, err)
		}
	}
	return nil
}

func exactBackgroundOwner(state chat.State, item acp.SpawnedWorkItem) (chat.ProviderActivityOwner, bool) {
	workID := firstNonEmptyString(strings.TrimSpace(item.ID), strings.TrimSpace(item.TaskID))
	if existing, ok := state.Background[workID]; ok {
		return existing.Owner, true
	}
	if tool := state.Tools[strings.TrimSpace(item.ToolCallID)]; strings.TrimSpace(item.ToolCallID) != "" && tool.Owner.OperationID != "" {
		return tool.Owner, true
	}
	laneID := providercontract.LaneID(strings.TrimSpace(item.OriginLaneID))
	operationID := providercontract.NormalizeOperationID(item.OriginOperationID)
	if lane, ok := state.Lanes[laneID]; ok && operationID != "" {
		return chat.ProviderActivityOwner{LaneID: laneID, OperationID: operationID, TurnID: strings.TrimSpace(item.OriginTurnID), ConnectionGeneration: lane.ConnectionGeneration}, true
	}
	turnID := strings.TrimSpace(item.OriginTurnID)
	// A ledger scan is legal only when the runtime supplied an immutable origin
	// identity. Empty origins must never attach an item to the first historical
	// row merely because one exists.
	if operationID == "" && turnID == "" {
		return chat.ProviderActivityOwner{}, false
	}
	if state.Foreground != nil &&
		(operationID == "" || state.Foreground.OperationID == operationID) &&
		(turnID == "" || state.Foreground.Turn.NativeID == turnID) {
		lane := state.Lanes[state.Foreground.LaneID]
		providerID := providercontract.NormalizeID(item.ProviderID)
		if providerID == "" || lane.Identity.Realm.ProviderID == providerID {
			return chat.ProviderActivityOwner{
				LaneID: state.Foreground.LaneID, OperationID: state.Foreground.OperationID,
				TurnID: state.Foreground.Turn.NativeID, ConnectionGeneration: lane.ConnectionGeneration,
			}, true
		}
	}
	for _, event := range state.Ledger {
		if (operationID != "" && event.OperationID != operationID) || (turnID != "" && event.NativeTurnID != turnID) {
			continue
		}
		if event.LaneID != "" && event.OperationID != "" {
			lane := state.Lanes[event.LaneID]
			return chat.ProviderActivityOwner{LaneID: event.LaneID, OperationID: event.OperationID, TurnID: event.NativeTurnID, ConnectionGeneration: lane.ConnectionGeneration}, true
		}
	}
	return chat.ProviderActivityOwner{}, false
}

func backgroundEvent(item acp.SpawnedWorkItem, workID string) providercontract.BackgroundEvent {
	return providercontract.BackgroundEvent{
		WorkID: workID, TaskID: item.TaskID, ToolCallID: item.ToolCallID, Title: item.Label,
		Kind: item.Kind, Role: item.Role, Status: item.Status, StartedAt: item.StartedAt,
		UpdatedAt: item.UpdatedAt, FinishedAt: item.FinishedAt, ExitCode: item.ExitCode,
		Summary: item.Summary, OutputFile: item.OutputFile, PID: item.PID, LastToolName: item.LastToolName,
		ModelLabel: item.ModelLabel, ResultExcerpt: item.ResultExcerpt,
	}
}

func (r *providerChatRuntime) ListBackground(tabID, chatID string) ([]map[string]any, error) {
	_, state, err := r.exactActor(tabID, chatID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(state.Background))
	for id := range state.Background {
		ids = append(ids, id)
	}
	sort.SliceStable(ids, func(i, j int) bool {
		left, right := state.Background[ids[i]].Event, state.Background[ids[j]].Event
		if (left.Status == "running") != (right.Status == "running") {
			return left.Status == "running"
		}
		return left.StartedAt > right.StartedAt
	})
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		item := state.Background[id].Event
		row := map[string]any{
			"id": item.WorkID, "taskId": item.TaskID, "tabId": state.Presentation.TabID, "chatId": state.ChatID,
			"kind": item.Kind, "label": item.Title, "role": item.Role, "status": item.Status,
			"startedAt": item.StartedAt, "updatedAt": item.UpdatedAt,
		}
		lane := state.Lanes[state.Background[id].Owner.LaneID]
		row["providerId"] = string(lane.Identity.Realm.ProviderID)
		for key, value := range map[string]any{
			"toolCallId": item.ToolCallID, "finishedAt": item.FinishedAt, "exitCode": item.ExitCode,
			"summary": item.Summary, "outputFile": item.OutputFile, "pid": item.PID,
			"lastToolName": item.LastToolName, "modelLabel": item.ModelLabel, "resultExcerpt": item.ResultExcerpt,
		} {
			if value != nil && fmt.Sprint(value) != "" {
				row[key] = value
			}
		}
		out = append(out, row)
	}
	return out, nil
}

func (r *providerChatRuntime) Obligation(tabID, chatID string) (*acp.ChatObligationProjection, error) {
	_, state, err := r.exactActor(tabID, chatID)
	if err != nil {
		return nil, err
	}
	return actorObligationProjection(state.Obligation), nil
}

func (r *providerChatRuntime) ReadBackground(tabID, chatID, id string, tailBytes int) (map[string]any, error) {
	_, state, err := r.exactActor(tabID, chatID)
	if err != nil {
		return nil, err
	}
	if _, ok := state.Background[strings.TrimSpace(id)]; !ok {
		return map[string]any{"ok": false, "error": "spawned work item not found"}, nil
	}
	return r.manager.ReadSpawnedWork(tabID, chatID, id, tailBytes), nil
}
