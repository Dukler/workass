package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

const sessionProjectionMessageTail = 60

type actorHistoryProjection uint8

const (
	actorHistoryFull actorHistoryProjection = iota
	actorHistoryTail
	actorHistoryMetadataOnly
)

// StateDigest is the lean actor-derived heartbeat projection. It intentionally
// does not consult session-state.json chat rows: after cutover that file contains
// daemon-global UI preferences only, and using it here would manufacture a
// permanent session:get refresh loop for every real actor-owned chat.
func (r *providerChatRuntime) StateDigest(catalogHashes map[string]string, settingsRevision, procHash string) (map[string]any, error) {
	if r == nil {
		return nil, errors.New("provider chat digest is unavailable")
	}
	ids, err := r.knownChatIDs()
	if err != nil {
		return nil, err
	}
	chats := make([]any, 0, len(ids))
	for _, chatID := range ids {
		actor, err := r.actor(chatID)
		if err != nil {
			return nil, err
		}
		digest := actor.engine.DigestSnapshot()
		if digest.Deleted {
			continue
		}
		projected := map[string]any{
			"tabId": digest.TabID, "chatId": digest.ChatID,
			"actorRevision":           digest.ActorRevision,
			presentationRevisionField: digest.PresentationRevision,
			"runningJobId":            nullableDigestString(digest.RunningJobID),
			"lastMessageId":           nullableDigestString(digest.LastMessageID),
			"messageCount":            digest.MessageCount, "queueLen": digest.QueueLen,
			"queueHeadId":               nullableDigestString(digest.QueueHeadID),
			agentQueueRevisionField:     int(digest.AgentQueueRevision),
			runtimeControlRevisionField: int(digest.RuntimeControlRevision),
			"providerId":                nullableDigestString(digest.ProviderID),
			"currentModelId":            nullableDigestString(digest.CurrentModelID),
			"currentModeId":             nullableDigestString(digest.CurrentModeID),
			"pendingPermissionIds":      digest.PendingPermissionIDs,
		}
		chats = append(chats, projected)
	}
	if catalogHashes == nil {
		catalogHashes = map[string]string{}
	}
	globalRevision := 0
	if r.sessions != nil {
		globalRevision = intValue(r.sessions.GlobalSnapshot()[globalPresentationRevisionField])
	}
	return map[string]any{
		"chats": chats, globalPresentationRevisionField: globalRevision, "catalogHash": catalogHashes,
		"settingsRevision": settingsRevision, "procHash": procHash,
	}, nil
}

// ProjectSession is the pure actor -> frozen Mirror-v1 boundary. The session
// store contributes daemon-global application preferences only. The selected
// chat and any running chat carry a bounded actor tail; idle, unselected chats
// carry metadata only. Opening a chat obtains its full ledger through the frozen
// chat:archive-load read method. Bounding each chat independently was not a
// bounded session: a machine with many chats still emitted a 90 MiB first
// hydration and repeatedly lost the receiving WebSocket.
func (r *providerChatRuntime) ProjectSession() (map[string]any, error) {
	if r == nil || r.sessions == nil {
		return nil, errors.New("provider chat projection is unavailable")
	}
	known, err := r.knownChatIDs()
	if err != nil {
		return nil, err
	}
	root := r.sessions.GlobalSnapshot()
	states := make([]chat.State, 0, len(known))
	for _, chatID := range known {
		actor, err := r.actor(chatID)
		if err != nil {
			return nil, err
		}
		state := actor.engine.Snapshot()
		if !state.Deleted {
			states = append(states, state)
		}
	}
	states = orderActorChatStates(states, root["chatOrder"])
	activeID := strings.TrimSpace(fieldString(root, "activeId"))
	activeExists := false
	firstTabID := ""
	for _, state := range states {
		tabID := state.Presentation.TabID
		if firstTabID == "" {
			firstTabID = tabID
		}
		if tabID == activeID {
			activeExists = true
		}
	}
	if !activeExists {
		activeID = firstTabID
	}
	if activeID == "" {
		root["activeId"] = nil
	} else {
		root["activeId"] = activeID
	}

	projectedChats := make([]any, 0, len(states))
	for _, state := range states {
		projected := map[string]any{}
		history := sessionHistoryProjection(state, activeID)
		if err := projectActorChatWithHistory(projected, state, history); err != nil {
			return nil, fmt.Errorf("project actor-native chat %q: %w", state.ChatID, err)
		}
		projectedChats = append(projectedChats, projected)
	}
	root["chats"] = projectedChats
	if err := rehydrateExternalSessionImages(root, filepath.Dir(r.sessions.path)); err != nil {
		return nil, err
	}
	return root, nil
}

func orderActorChatStates(states []chat.State, rawOrder any) []chat.State {
	if len(states) < 2 {
		return states
	}
	byTab := make(map[string]chat.State, len(states))
	for _, state := range states {
		byTab[state.Presentation.TabID] = state
	}
	ordered := make([]chat.State, 0, len(states))
	seen := make(map[string]struct{}, len(states))
	for _, rawTabID := range normalizedActorChatOrder(rawOrder) {
		tabID, _ := rawTabID.(string)
		state, exists := byTab[tabID]
		if !exists {
			continue
		}
		ordered = append(ordered, state)
		seen[tabID] = struct{}{}
	}
	// Actors unknown to the saved order are appended in knownChatIDs order,
	// which is deterministic. A later renderer save folds them into chatOrder.
	for _, state := range states {
		if _, exists := seen[state.Presentation.TabID]; exists {
			continue
		}
		ordered = append(ordered, state)
	}
	return ordered
}

func sessionHistoryProjection(state chat.State, activeTabID string) actorHistoryProjection {
	if state.Presentation.TabID == activeTabID || state.Foreground != nil {
		return actorHistoryTail
	}
	return actorHistoryMetadataOnly
}

func (r *providerChatRuntime) ProjectSessionRaw() ([]byte, error) {
	root, err := r.ProjectSession()
	if err != nil {
		return nil, err
	}
	return json.Marshal(root)
}

// ProjectArchiveByTab derives the frozen archive payload from the same actor
// ledger as session:get. After eager cutover, found=false means the tab is not
// an actor-owned chat; callers must never fall back to deleted JSONL storage.
func (r *providerChatRuntime) ProjectArchiveByTab(tabID string) ([]any, bool, error) {
	matched, found, err := r.actorByTab(tabID)
	if err != nil || !found {
		return nil, found, err
	}
	if matched.engine.Snapshot().Deleted {
		// A tombstone owns this historical tab. Never recreate its transcript.
		return []any{}, true, nil
	}
	projected := map[string]any{}
	if err := projectActorChat(projected, matched.engine.Snapshot()); err != nil {
		return nil, false, err
	}
	messages := anySlice(projected["messages"])
	if err := rehydrateExternalSessionImages(messages, filepath.Dir(r.sessions.path)); err != nil {
		return nil, false, err
	}
	return messages, true, nil
}

func (r *providerChatRuntime) actorByTab(tabID string) (*providerChatActor, bool, error) {
	tabID = strings.TrimSpace(tabID)
	if r == nil || tabID == "" {
		return nil, false, nil
	}
	ids, err := r.knownChatIDs()
	if err != nil {
		return nil, false, err
	}
	var matched *providerChatActor
	for _, chatID := range ids {
		actor, err := r.actor(chatID)
		if err != nil {
			return nil, false, err
		}
		if strings.TrimSpace(actor.engine.Snapshot().Presentation.TabID) != tabID {
			continue
		}
		if matched != nil {
			return nil, false, fmt.Errorf("multiple actor chats claim tab id %q", tabID)
		}
		matched = actor
	}
	if matched == nil {
		return nil, false, nil
	}
	return matched, true, nil
}

// ApplyRendererSnapshot translates the frozen session:save payload into typed
// actor commands. Renderer-authored messages, provider runtime fields, lane
// bindings, permissions, usage and outbox state are ignored; after the actor
// commit, the global presentation store receives only daemon-wide UI fields.
func (r *providerChatRuntime) ApplyRendererSnapshot(snapshot any) (bool, error) {
	_, err := r.applyRendererSnapshot(snapshot)
	return err == nil, err
}

func (r *providerChatRuntime) applyRendererSnapshot(snapshot any) (globalPresentationSaveResult, error) {
	if r == nil || r.sessions == nil {
		return globalPresentationSaveResult{}, errors.New("provider chat projection is unavailable")
	}
	detached := mapFromAnyMain(cloneJSON(redactSessionValue(snapshot)))
	if detached == nil {
		return globalPresentationSaveResult{}, errors.New("renderer session snapshot is not an object")
	}
	// Chat rows are deliberately ignored here. Each chat presentation and queue
	// mutation, including deletion, has its own stable-id actor command;
	// session:save persists daemon-global UI state only.
	result, err := r.sessions.SaveActorGlobalSnapshot(detached)
	if err != nil {
		return globalPresentationSaveResult{}, fmt.Errorf("persist daemon-global presentation state: %w", err)
	}
	detached[globalPresentationRevisionField] = int(result.Revision)
	return result, nil
}

func projectActorChat(out map[string]any, state chat.State) error {
	return projectActorChatWithHistory(out, state, actorHistoryFull)
}

func projectActorChatWithHistory(out map[string]any, state chat.State, history actorHistoryProjection) error {
	if out == nil {
		return errors.New("chat projection target is nil")
	}
	if !state.Initialized {
		return errors.New("chat actor initialization is incomplete")
	}
	if state.Deleted {
		return errors.New("deleted chat cannot be projected")
	}
	p := state.Presentation
	out["id"] = p.TabID
	out["chatId"] = state.ChatID
	out["actorRevision"] = state.Revision
	out["title"] = p.Title
	out["titleLocked"] = p.TitleLocked
	setOptionalProjectionString(out, "group", p.Group)
	setOptionalProjectionString(out, "cwd", p.CWD)
	out["draft"] = p.Draft
	out["unread"] = p.Unread
	out[presentationRevisionField] = p.PresentationRevision
	setOptionalProjectionString(out, "pane", p.Pane)
	setStringOrNil(out, "providerId", string(p.ProviderID))
	setStringOrNil(out, "currentModelId", p.CurrentModelID)
	setStringOrNil(out, "currentModeId", p.CurrentModeID)
	if err := setRawProjection(out, "modelControls", p.ModelControls); err != nil {
		return err
	}
	if err := projectActorUsage(out, state); err != nil {
		return err
	}
	if p.Settled == "" {
		delete(out, "settled")
	} else {
		out["settled"] = p.Settled
	}
	if p.SettledAt == 0 {
		delete(out, "settledAt")
	} else {
		out["settledAt"] = p.SettledAt
	}
	if lastActivityAt := actorLastActivityAt(state); lastActivityAt > 0 {
		out["lastActivityAt"] = lastActivityAt
	} else {
		delete(out, "lastActivityAt")
	}
	if p.WorkspaceRevision == 0 {
		delete(out, workspaceRevisionField)
	} else {
		out[workspaceRevisionField] = p.WorkspaceRevision
	}
	if p.AgentQueueRevision == 0 {
		delete(out, agentQueueRevisionField)
	} else {
		out[agentQueueRevisionField] = p.AgentQueueRevision
	}
	if p.RuntimeControlRevision == 0 {
		delete(out, runtimeControlRevisionField)
	} else {
		out[runtimeControlRevisionField] = p.RuntimeControlRevision
	}
	if len(p.PlanLatest) == 0 {
		out["planLatest"] = []any{}
	} else {
		entries := make([]any, 0, len(p.PlanLatest))
		for _, entry := range p.PlanLatest {
			entries = append(entries, map[string]any{"status": entry.Status, "content": entry.Text})
		}
		out["planLatest"] = entries
	}
	if p.PlanLatestMessageID == "" {
		delete(out, "planLatestMessageId")
	} else {
		out["planLatestMessageId"] = p.PlanLatestMessageID
	}

	messages, messageCount, err := projectActorMessages(state, history)
	if err != nil {
		return err
	}
	out["messages"] = messages
	if history != actorHistoryFull {
		out["messageCount"] = messageCount
		out["historyComplete"] = len(messages) == messageCount
	}
	queue, err := projectActorQueue(state.StagedQueue, state.Queue)
	if err != nil {
		return err
	}
	out["queue"] = queue
	out["pending"] = actorForegroundRunning(state.Foreground)
	delete(out, "serverAuthored")
	delete(out, "liveSession")
	delete(out, "sessionId")
	delete(out, "sessionProviderId")
	delete(out, "sessionError")
	if lane, ok := state.Lanes[state.ActiveLaneID]; ok {
		out["sessionProviderId"] = string(lane.Identity.Realm.ProviderID)
		if !lane.Thread.IsZero() {
			out["sessionId"] = lane.Thread.HeadID
		}
		if lane.Attachment != nil {
			out["liveSession"] = projectLaneAttachment(*lane.Attachment, lane.Delivery)
		}
	}
	if lane, ok := state.Lanes[state.DesiredLaneID]; ok && (lane.Phase == chat.LaneBlocked || lane.Phase == chat.LaneBroken) {
		out["sessionError"] = string(lane.LastError)
	}
	return nil
}

func actorForegroundRunning(foreground *chat.ForegroundTurn) bool {
	if foreground == nil {
		return false
	}
	switch foreground.Status {
	case chat.ForegroundDispatching, chat.ForegroundRunning, chat.ForegroundReconciling:
		return true
	default:
		return false
	}
}

// actorLastActivityAt projects the lifecycle clock independently of transcript
// residency. Ledger order is canonical, so the newest parseable row is the
// latest committed activity; a foreground turn may be newer still.
func actorLastActivityAt(state chat.State) int64 {
	latest := int64(0)
	for index := len(state.Ledger) - 1; index >= 0; index-- {
		at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(state.Ledger[index].At))
		if err != nil {
			continue
		}
		latest = at.UnixMilli()
		break
	}
	if foreground := state.Foreground; foreground != nil {
		if at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(foreground.StartedAt)); err == nil && at.UnixMilli() > latest {
			latest = at.UnixMilli()
		}
	}
	return latest
}

// projectActorMessages scans stable ids for the full ledger but materializes
// only the history requested by the projection policy. Full archive reads
// carry every row; metadata-only session rows retain just live foreground rows
// that have not reached the ledger yet.
func projectActorMessages(state chat.State, history actorHistoryProjection) ([]any, int, error) {
	seenMessage := make(map[string]struct{}, len(state.Ledger)+4)
	for _, event := range state.Ledger {
		if _, duplicate := seenMessage[event.MessageID]; duplicate {
			return nil, 0, fmt.Errorf("actor ledger contains duplicate message id %q", event.MessageID)
		}
		seenMessage[event.MessageID] = struct{}{}
	}

	extra := make([]map[string]any, 0, 4)
	if foreground := state.Foreground; foreground != nil {
		if !foreground.UserConsumed {
			userID := strings.TrimSpace(foreground.Input.Presentation.UserMessageID)
			if userID == "" {
				userID = fmt.Sprintf("message:%s:user", foreground.OperationID)
			}
			if _, exists := seenMessage[userID]; !exists {
				userStatus := "pending"
				if foreground.Status == chat.ForegroundUncertain {
					userStatus = "done"
				}
				user := map[string]any{
					"id": userID, "role": "user", "content": foreground.Input.Text,
					"status": userStatus, "at": nilIfEmpty(foreground.StartedAt), "events": []any{},
				}
				if queueID := strings.TrimSpace(foreground.Input.Presentation.QueueID); queueID != "" {
					user[agentQueueMessageField] = queueID
				}
				images, err := projectionAttachments(foreground.Input.Attachments)
				if err != nil {
					return nil, 0, err
				}
				if len(images) > 0 {
					user["images"] = images
				}
				extra = append(extra, user)
				seenMessage[userID] = struct{}{}
			}
		}

		assistantID := strings.TrimSpace(foreground.CurrentAssistantMessageID)
		if _, exists := seenMessage[assistantID]; !exists {
			events, err := projectTimeline(foreground.Timeline)
			if err != nil {
				return nil, 0, err
			}
			assistantStatus := "running"
			assistantContent := foreground.AssistantContent
			if foreground.Status == chat.ForegroundUncertain {
				assistantStatus = "failed"
				if strings.TrimSpace(assistantContent) == "" {
					assistantContent = "Workass could not confirm whether the provider accepted this turn. It will not resend it."
				}
			}
			assistant := map[string]any{
				"id": assistantID, "role": "assistant", "content": assistantContent,
				"status": assistantStatus, "at": nil, "events": events,
			}
			if foreground.Status == chat.ForegroundUncertain {
				assistant["interrupted"] = true
			}
			if startedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(foreground.StartedAt)); err == nil {
				assistant["turnStartedAt"] = startedAt.UnixMilli()
			}
			if foreground.AssistantResult != "" {
				assistant["result"] = foreground.AssistantResult
			}
			images, err := projectionAttachments(foreground.AssistantAttachments)
			if err != nil {
				return nil, 0, err
			}
			if len(images) > 0 {
				assistant["images"] = images
			}
			if permission := projectPermission(foreground.Permission); permission != nil {
				assistant["permission"] = permission
			}
			if foreground.Turn.NativeID != "" {
				assistant["jobId"] = foreground.Turn.NativeID
			}
			if assistantID != foreground.RootAssistantMessageID {
				assistant["turnRootId"] = foreground.RootAssistantMessageID
				assistant["turnTerminal"] = true
			}
			extra = append(extra, assistant)
			seenMessage[assistantID] = struct{}{}
		}

		steerRows, err := projectPendingSteer(state.PendingSteer, foreground)
		if err != nil {
			return nil, 0, err
		}
		for _, row := range steerRows {
			id := fieldString(row, "id")
			if _, duplicate := seenMessage[id]; duplicate {
				return nil, 0, fmt.Errorf("pending steer duplicates message id %q", id)
			}
			seenMessage[id] = struct{}{}
			extra = append(extra, row)
		}
	}

	total := len(state.Ledger) + len(extra)
	first := 0
	switch history {
	case actorHistoryMetadataOnly:
		first = len(state.Ledger)
	case actorHistoryTail:
		if total > sessionProjectionMessageTail {
			first = total - sessionProjectionMessageTail
		}
	case actorHistoryFull:
	default:
		return nil, 0, fmt.Errorf("unknown actor history projection %d", history)
	}
	messages := make([]any, 0, total-first)
	ledgerStart := min(first, len(state.Ledger))
	for index := ledgerStart; index < len(state.Ledger); index++ {
		message, err := projectLedgerMessage(state.Ledger[index])
		if err != nil {
			return nil, 0, err
		}
		messages = append(messages, message)
	}
	extraStart := max(0, first-len(state.Ledger))
	for index := extraStart; index < len(extra); index++ {
		messages = append(messages, extra[index])
	}
	return messages, total, nil
}

// projectReconciledTerminalJob keeps the frozen live job:end contract after a
// host crash without reviving Manager as chat authority. The terminal ledger
// row and lane binding have already been committed by the actor; this function
// is a pure presentation projection of those bytes.
func projectReconciledTerminalJob(state chat.State, operationID providercontract.OperationID, turn providercontract.TurnRef) (map[string]any, error) {
	return projectActorTerminalJob(state, operationID, turn)
}

func projectActorTerminalJob(state chat.State, operationID providercontract.OperationID, turn providercontract.TurnRef) (map[string]any, error) {
	var user *chat.LedgerEvent
	var assistant *chat.LedgerEvent
	for index := range state.Ledger {
		event := &state.Ledger[index]
		if event.OperationID != operationID {
			continue
		}
		if event.Role == "user" && user == nil {
			user = event
		}
		if event.Role == "assistant" && event.Terminal != nil {
			assistant = event
		}
	}
	if assistant == nil || assistant.Terminal == nil {
		return nil, errors.New("reconciled terminal operation is missing its actor ledger receipt")
	}
	terminal := assistant.Terminal
	lane, ok := state.Lanes[assistant.LaneID]
	if !ok {
		return nil, errors.New("terminal operation lost its exact provider lane")
	}
	jobID := firstNonEmptyString(turn.NativeID, assistant.NativeTurnID, providercontract.DeriveJobID(state.ChatID, operationID))
	if jobID == "" {
		return nil, errors.New("reconciled terminal operation is missing its native turn id")
	}
	status := strings.ToLower(strings.TrimSpace(terminal.Status))
	jobStatus := "failed"
	code := any(1)
	if status == "completed" || status == "done" {
		jobStatus = "done"
		code = 0
	} else if status == "cancelled" || strings.EqualFold(terminal.StopReason, "cancelled") {
		code = 130
	}
	if terminal.Code != nil {
		code = *terminal.Code
	}
	startedAt := ""
	promptText := ""
	userMessageID := ""
	if user != nil {
		startedAt, promptText, userMessageID = user.At, user.Text, user.MessageID
	}
	finishedAt := firstNonEmptyString(terminal.FinishedAt, assistant.At)
	if finishedAt == "" {
		finishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	result := firstNonEmptyString(terminal.Result, assistant.Result, assistant.Text)
	consumedSteers := make([]string, 0, len(terminal.ConsumedSteerIDs))
	for _, id := range terminal.ConsumedSteerIDs {
		consumedSteers = append(consumedSteers, string(id))
	}
	job := map[string]any{
		"id": jobID, "kind": "app-chat", "key": nil, "title": state.Presentation.Title,
		"status": jobStatus, "startedAt": startedAt, "finishedAt": finishedAt, "code": code,
		"permissionMode": "", "chatId": state.ChatID, "tabId": nullableString(state.Presentation.TabID),
		"sessionId": nullableString(lane.Thread.HeadID), "providerId": string(lane.Identity.Realm.ProviderID),
		"userMessageId": nullableString(userMessageID), "assistantMessageId": nullableString(assistant.MessageID),
		"promptText": promptText, "result": nullableString(result), "error": nullableString(terminal.Error),
		"stopReason": nullableString(terminal.StopReason), "crashInterrupted": terminal.CrashInterrupted,
		"interrupted": terminal.Interrupted, "consumedSteerIds": consumedSteers,
	}
	if strings.TrimSpace(terminal.DispositionState) != "" {
		disposition := map[string]any{"state": terminal.DispositionState}
		if strings.TrimSpace(terminal.DispositionSource) != "" {
			disposition["source"] = terminal.DispositionSource
		}
		job["disposition"] = disposition
	}
	return map[string]any{"type": "end", "job": job}, nil
}

func projectLaneAttachment(snapshot providercontract.LaneAttachmentSnapshot, delivery providercontract.DeliveryCapabilities) map[string]any {
	models := make([]any, 0, len(snapshot.Models))
	for _, model := range snapshot.Models {
		models = append(models, map[string]any{
			"modelId": model.ID, "name": model.Name, "efforts": append([]string(nil), model.Efforts...),
		})
	}
	modes := make([]any, 0, len(snapshot.Modes))
	for _, mode := range snapshot.Modes {
		modes = append(modes, map[string]any{"id": mode.ID, "name": mode.Name})
	}
	projected := map[string]any{
		"sessionId": snapshot.ConnectionID, "cwd": snapshot.CWD, "agent": snapshot.Agent,
		"providerId": string(snapshot.ProviderID), "providerName": snapshot.ProviderName,
		"models": models, "currentModelId": nullableDigestString(snapshot.CurrentModelID),
		"modes": modes, "currentModeId": nullableDigestString(snapshot.CurrentModeID),
		"imageSupport":       snapshot.ImageSupport,
		"planUsageSupported": snapshot.PlanUsageSupported, "planUsageResetSupported": snapshot.PlanUsageResetSupported,
		"commandCatalogSupported": snapshot.CommandCatalogSupported,
		"deliveryCapabilities":    projectDeliveryCapabilities(delivery),
	}
	if snapshot.CommandCatalog != nil {
		projected["commandCatalog"] = projectRuntimeCommandCatalog(snapshot.CommandCatalog)
	}
	return projected
}

func projectDeliveryCapabilities(capabilities providercontract.DeliveryCapabilities) map[string]any {
	return map[string]any{
		"stableInputIdentity":     capabilities.StableInputIdentity,
		"liveSteer":               capabilities.LiveSteer,
		"steerConsumptionReceipt": capabilities.SteerConsumptionReceipt,
		"consumptionReceipt":      capabilities.ConsumptionReceipt,
		"turnReadback":            capabilities.TurnReadback,
	}
}

func projectRuntimeCommandCatalog(catalog *providercontract.RuntimeCommandCatalog) any {
	if catalog == nil {
		return nil
	}
	commands := make([]any, 0, len(catalog.Commands))
	for _, command := range catalog.Commands {
		commands = append(commands, map[string]any{
			"name": command.Name, "description": command.Description,
			"argumentHint": command.ArgumentHint, "aliases": append([]string(nil), command.Aliases...),
		})
	}
	agents := make([]any, 0, len(catalog.Agents))
	for _, agent := range catalog.Agents {
		agents = append(agents, map[string]any{"name": agent.Name, "description": agent.Description, "model": agent.Model})
	}
	return map[string]any{
		"commands": commands, "agents": agents, "outputStyle": catalog.OutputStyle,
		"availableOutputStyles": append([]string(nil), catalog.AvailableOutputStyles...),
		"commandsTruncated":     catalog.CommandsTruncated, "agentsTruncated": catalog.AgentsTruncated,
		"stylesTruncated": catalog.StylesTruncated, "asOf": catalog.AsOf,
	}
}

func projectLedgerMessage(event chat.LedgerEvent) (map[string]any, error) {
	message := map[string]any{}
	message["id"] = event.MessageID
	message["role"] = event.Role
	message["content"] = event.Text
	if event.Result == "" {
		delete(message, "result")
	} else {
		message["result"] = event.Result
	}
	status := event.Status
	if status == "" {
		status = "done"
	}
	message["status"] = status
	if event.At == "" {
		message["at"] = nil
	} else {
		message["at"] = event.At
	}
	if len(event.Timeline) > 0 {
		events, err := projectTimeline(event.Timeline)
		if err != nil {
			return nil, err
		}
		message["events"] = events
	} else if _, exists := message["events"]; !exists {
		message["events"] = []any{}
	}
	images, err := projectionAttachments(event.Attachments)
	if err != nil {
		return nil, err
	}
	if len(images) > 0 {
		message["images"] = images
	}
	if event.SteerState != "" {
		message["steerState"] = event.SteerState
	} else {
		delete(message, "steerState")
	}
	if event.SteerBoundary != "" {
		message["steerBoundary"] = event.SteerBoundary
	}
	if event.SteerContinuationID != "" {
		message["steerContinuationId"] = event.SteerContinuationID
	}
	if event.SteerContinuationFor != "" {
		message["steerContinuationFor"] = event.SteerContinuationFor
	}
	if event.TurnRootID != "" {
		message["turnRootId"] = event.TurnRootID
	} else {
		delete(message, "turnRootId")
	}
	if event.TurnTerminal != nil {
		message["turnTerminal"] = *event.TurnTerminal
	} else {
		delete(message, "turnTerminal")
	}
	if permission := projectPermission(event.Permission); permission != nil {
		message["permission"] = permission
	} else {
		delete(message, "permission")
	}
	if event.NativeTurnID != "" {
		message["jobId"] = event.NativeTurnID
	}
	if event.TurnStartedAt > 0 {
		message["turnStartedAt"] = event.TurnStartedAt
	}
	if event.QueueID != "" {
		message[agentQueueMessageField] = event.QueueID
	} else {
		delete(message, agentQueueMessageField)
	}
	if event.Interrupted {
		message["interrupted"] = true
	} else {
		delete(message, "interrupted")
	}
	if event.RetryPrompt != "" {
		message["retryPrompt"] = event.RetryPrompt
	} else {
		delete(message, "retryPrompt")
	}
	return message, nil
}

func projectPendingSteer(pending *chat.PendingSteer, foreground *chat.ForegroundTurn) ([]map[string]any, error) {
	if pending == nil || foreground == nil {
		return nil, nil
	}
	userID := strings.TrimSpace(pending.Presentation.UserMessageID)
	if userID == "" {
		userID = fmt.Sprintf("message:%s:user", pending.OperationID)
	}
	continuationID := strings.TrimSpace(pending.Presentation.AssistantMessageID)
	if continuationID == "" {
		continuationID = foreground.RootAssistantMessageID + "~after~" + string(pending.OperationID)
	}
	steerState := "sending"
	status := "pending"
	switch pending.Status {
	case chat.SteerAccepted:
		steerState, status = "accepted", "done"
	case chat.SteerUncertain:
		steerState, status = "uncertain", "done"
	}
	images, err := projectionAttachments(pending.Attachments)
	if err != nil {
		return nil, err
	}
	user := map[string]any{
		"id": userID, "role": "user", "content": pending.Text, "status": status,
		"at": nilIfEmpty(pending.Presentation.StartedAt), "events": []any{},
		"steerState": steerState, "turnRootId": foreground.RootAssistantMessageID,
	}
	if queueID := strings.TrimSpace(pending.Presentation.QueueID); queueID != "" {
		user[agentQueueMessageField] = queueID
	}
	if len(images) > 0 {
		user["images"] = images
	}
	assistant := map[string]any{
		"id": continuationID, "role": "assistant", "content": "", "status": "pending", "at": nil,
		"events": []any{}, "jobId": foreground.Turn.NativeID,
		"turnRootId": foreground.RootAssistantMessageID, "turnTerminal": true,
	}
	if pending.AwaitConsumption || pending.Status == chat.SteerDispatching {
		user["steerBoundary"] = "waiting"
		user["steerContinuationId"] = continuationID
		assistant["steerBoundary"] = "waiting"
		assistant["steerContinuationFor"] = userID
	}
	return []map[string]any{user, assistant}, nil
}

func projectTimeline(entries []chat.TimelineEntry) ([]any, error) {
	out := make([]any, 0, len(entries))
	for _, entry := range entries {
		item := map[string]any{"key": entry.Key, "at": entry.At}
		switch entry.Kind {
		case providercontract.EventThinkingUpdate:
			if entry.Thinking == nil {
				return nil, errors.New("thinking timeline entry lost its typed payload")
			}
			item["kind"], item["text"] = "thinking", entry.Thinking.Text
		case providercontract.EventPlanUpdate:
			if entry.Plan == nil {
				return nil, errors.New("plan timeline entry lost its typed payload")
			}
			projected := make([]any, 0, len(entry.Plan.Entries))
			for _, planEntry := range entry.Plan.Entries {
				projected = append(projected, map[string]any{"status": planEntry.Status, "content": planEntry.Text})
			}
			item["kind"], item["entries"] = "plan", projected
		case providercontract.EventToolUpdate:
			if entry.Tool == nil {
				return nil, errors.New("tool timeline entry lost its typed payload")
			}
			tool := entry.Tool
			item["kind"] = "tool"
			item["id"] = nullableString(tool.ToolCallID)
			item["toolKind"] = nullableString(tool.ToolKind)
			item["title"], item["status"] = tool.Title, tool.Status
			item["command"] = nullableString(tool.Command)
			item["terminalId"] = nullableString(tool.TerminalID)
			item["input"] = nullableString(tool.Input)
			item["output"] = nullableString(tool.Output)
			item["location"] = nullableString(tool.Location)
			images, err := projectionAttachments(tool.Attachments)
			if err != nil {
				return nil, err
			}
			if len(images) > 0 {
				item["images"] = images
			}
			if tool.StartedAtUnixMS > 0 {
				item["startedAt"] = tool.StartedAtUnixMS
			}
			if tool.EndedAtUnixMS > 0 {
				item["endedAt"] = tool.EndedAtUnixMS
			}
			if tool.SubagentID != "" {
				item["subagentId"] = tool.SubagentID
			}
			if tool.SubagentLabel != "" {
				item["subagentLabel"] = tool.SubagentLabel
			}
			if tool.SubagentProvider != "" {
				item["subagentProvider"] = tool.SubagentProvider
			}
			if tool.SubagentModel != "" {
				item["subagentModel"] = tool.SubagentModel
			}
			if tool.SubagentHeader {
				item["subagentHeader"] = true
			}
		case providercontract.EventCompactionStarted, providercontract.EventCompactionCheckpoint:
			if entry.Compaction == nil {
				return nil, errors.New("compaction timeline entry lost its typed payload")
			}
			item["kind"] = "compaction"
		case providercontract.EventCheckpointRestored:
			if entry.Restored == nil || entry.Restored.TurnSequence <= 0 {
				return nil, errors.New("checkpoint restore timeline entry lost its typed payload")
			}
			item["kind"], item["turnSeq"] = "restored", entry.Restored.TurnSequence
		default:
			return nil, fmt.Errorf("unsupported actor timeline event %q", entry.Kind)
		}
		out = append(out, item)
	}
	return out, nil
}

func projectPermission(permission *providercontract.PermissionEvent) map[string]any {
	if permission == nil {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(permission.Status)) {
	case "resolved", "failed":
		return nil
	}
	options := make([]any, 0, max(len(permission.OptionDetails), len(permission.Options)))
	if len(permission.OptionDetails) > 0 {
		for _, option := range permission.OptionDetails {
			options = append(options, map[string]any{"optionId": option.ID, "name": option.Name, "kind": option.Kind})
		}
	} else {
		for _, optionID := range permission.Options {
			options = append(options, map[string]any{"optionId": optionID, "name": optionID, "kind": ""})
		}
	}
	out := map[string]any{
		"id": permission.RequestID, "title": permission.Title,
		"kind": nullableString(permission.Kind), "options": options,
	}
	if permission.Question != nil {
		questionOptions := make([]any, 0, len(permission.Question.Options))
		for _, option := range permission.Question.Options {
			questionOptions = append(questionOptions, map[string]any{"label": option.Label, "description": option.Description})
		}
		out["question"] = map[string]any{
			"question": permission.Question.Question, "header": permission.Question.Header,
			"options": questionOptions, "multiSelect": permission.Question.MultiSelect,
		}
	}
	return out
}

func projectActorQueue(staged []chat.StagedQueueEntry, queue []chat.QueueEntry) ([]any, error) {
	out := make([]any, 0, len(staged)+len(queue))
	for _, entry := range staged {
		item := map[string]any{"id": entry.ID, "text": entry.Text}
		if entry.Source != "" {
			item["source"] = entry.Source
		}
		if entry.Delivery != "" {
			item["delivery"] = entry.Delivery
		}
		if entry.QueuedAt != "" {
			item["queuedAt"] = entry.QueuedAt
		}
		images, err := projectionAttachments(entry.Attachments)
		if err != nil {
			return nil, err
		}
		if len(images) > 0 {
			item["images"] = images
		}
		if len(entry.AttachmentNames) > 0 {
			item["attachmentNames"] = append([]string(nil), entry.AttachmentNames...)
		}
		if entry.AttachmentState != "" {
			item["attachmentState"] = entry.AttachmentState
		}
		if entry.AttachmentError != "" {
			item["attachmentError"] = entry.AttachmentError
		}
		out = append(out, item)
	}
	for _, entry := range queue {
		id := strings.TrimSpace(entry.Presentation.QueueID)
		if id == "" {
			id = string(entry.OperationID)
		}
		item := map[string]any{
			"id": id, "text": entry.Text, "delivery": "queue",
		}
		if entry.Presentation.Origin != "" {
			source := entry.Presentation.Origin
			if source == "human" {
				source = "host"
			}
			item["source"] = source
		}
		images, err := projectionAttachments(entry.Attachments)
		if err != nil {
			return nil, err
		}
		if len(images) > 0 {
			item["images"] = images
		}
		out = append(out, item)
	}
	return out, nil
}

func nilIfEmpty(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func projectionAttachments(attachments []providercontract.Attachment) ([]any, error) {
	out := make([]any, 0, len(attachments))
	for index, attachment := range attachments {
		ref := strings.TrimPrefix(strings.TrimSpace(attachment.Ref), providerSessionImageRefPrefix)
		if ref == "" || ref == strings.TrimSpace(attachment.Ref) {
			return nil, fmt.Errorf("actor attachment %d is missing a durable session image reference", index)
		}
		item := map[string]any{
			"mimeType": attachment.MIMEType, "name": attachment.Name,
			sessionImageDataRefField: ref,
		}
		out = append(out, item)
	}
	return out, nil
}

func setOptionalProjectionString(target map[string]any, key string, value *string) {
	if value == nil {
		target[key] = nil
		return
	}
	target[key] = *value
}

func setStringOrNil(target map[string]any, key, value string) {
	if strings.TrimSpace(value) == "" {
		target[key] = nil
		return
	}
	target[key] = value
}

func setRawProjection(target map[string]any, key string, raw json.RawMessage) error {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		delete(target, key)
		return nil
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("actor presentation field %q is invalid: %w", key, err)
	}
	target[key] = value
	return nil
}

func projectActorUsage(target map[string]any, state chat.State) error {
	byProvider := make(map[string]any)
	if raw := state.Presentation.ContextUsageByProvider; len(raw) > 0 && strings.TrimSpace(string(raw)) != "null" {
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.UseNumber()
		if err := decoder.Decode(&byProvider); err != nil {
			return fmt.Errorf("actor presentation field %q is invalid: %w", "contextUsageByProvider", err)
		}
		if byProvider == nil {
			byProvider = make(map[string]any)
		}
	}
	type observedUsage struct {
		laneID string
		value  providercontract.UsageEvent
	}
	latest := make(map[string]observedUsage)
	for laneID, usage := range state.Usage {
		lane, ok := state.Lanes[laneID]
		if !ok {
			return fmt.Errorf("actor usage belongs to unknown lane %q", laneID)
		}
		providerID := strings.TrimSpace(string(lane.Identity.Realm.ProviderID))
		used := usage.Used
		if used <= 0 {
			used = usage.InputTokens + usage.OutputTokens
		}
		if providerID == "" || used < 0 || usage.Size <= 0 {
			continue
		}
		prior, exists := latest[providerID]
		if exists && (prior.value.ObservedAtUnixMS > usage.ObservedAtUnixMS ||
			(prior.value.ObservedAtUnixMS == usage.ObservedAtUnixMS && prior.laneID >= string(laneID))) {
			continue
		}
		latest[providerID] = observedUsage{laneID: string(laneID), value: usage}
	}
	for providerID, observed := range latest {
		usage := observed.value
		used := usage.Used
		if used <= 0 {
			used = usage.InputTokens + usage.OutputTokens
		}
		projected := map[string]any{"used": used, "size": usage.Size}
		if usage.ObservedAtUnixMS > 0 {
			projected["updatedAt"] = time.UnixMilli(usage.ObservedAtUnixMS).UTC().Format(time.RFC3339Nano)
		}
		byProvider[providerID] = projected
	}
	if len(byProvider) == 0 {
		delete(target, "contextUsageByProvider")
	} else {
		target["contextUsageByProvider"] = byProvider
	}
	selected := strings.TrimSpace(string(state.Presentation.ProviderID))
	if selected != "" {
		if value, ok := byProvider[selected]; ok {
			target["usage"] = cloneJSON(value)
			return nil
		}
	}
	delete(target, "usage")
	return nil
}
