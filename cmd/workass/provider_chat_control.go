package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

func (r *providerChatRuntime) CreateRendererChat(raw map[string]any) (map[string]any, error) {
	if r == nil {
		return nil, errors.New("chat creation requires the authoritative actor runtime")
	}
	allowed := map[string]struct{}{
		"tabId": {}, "chatId": {}, "operationId": {}, "focus": {}, "title": {}, "titleLocked": {},
		"group": {}, "cwd": {}, "providerId": {}, "currentModelId": {}, "currentModeId": {}, "modelControls": {},
	}
	if err := requireOnlyKeys(raw, allowed, "renderer chat creation"); err != nil {
		return nil, err
	}
	tabID, chatID := strings.TrimSpace(fieldString(raw, "tabId")), strings.TrimSpace(fieldString(raw, "chatId"))
	operationID := providercontract.NormalizeOperationID(fieldString(raw, "operationId"))
	if tabID == "" || chatID == "" || operationID == "" {
		return nil, errors.New("chat creation requires exact tabId, chatId, and stable operationId")
	}
	presentation := chat.PresentationState{
		TabID: tabID, Title: redactedSessionString(fieldString(raw, "title")),
		TitleLocked: boolFieldValue(raw, "titleLocked"), Group: optionalStringPointer(raw, "group"),
		CWD:            stringPointerValueOrNil(fieldString(raw, "cwd")),
		ProviderID:     providercontract.NormalizeID(fieldString(raw, "providerId")),
		CurrentModelID: hydratableStoredModelID(fieldString(raw, "currentModelId")),
		CurrentModeID:  strings.TrimSpace(fieldString(raw, "currentModeId")),
	}
	if presentation.Title == "" {
		presentation.Title = "Nuevo chat"
	}
	if value, exists := raw["modelControls"]; exists && value != nil {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("encode initial model controls: %w", err)
		}
		presentation.ModelControls = encoded
	}
	actor, err := r.actorForNewChatOperation(chatID, presentation, operationID)
	if err != nil {
		return nil, err
	}
	state := actor.engine.Snapshot()
	globalRevision := uint64(0)
	if boolFieldValue(raw, "focus") {
		globalRevision, err = r.sessions.SaveGlobalActiveTab(tabID, providercontract.OperationID("focus:"+string(operationID)))
		if err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"ok": true, "tabId": tabID, "chatId": chatID, "operationId": string(operationID),
		"actorRevision": state.Revision, "presentationRevision": state.Presentation.PresentationRevision,
		globalPresentationRevisionField: globalRevision,
	}, nil
}

func (r *providerChatRuntime) ReplaceStagedQueue(tabID, chatID string, operationID providercontract.OperationID, expectedRevision uint64, rawEntries []any) (map[string]any, error) {
	actor, _, err := r.exactActor(tabID, chatID)
	if err != nil {
		return nil, err
	}
	operationID = providercontract.NormalizeOperationID(string(operationID))
	if operationID == "" {
		return nil, errors.New("queue replacement requires a stable operation id")
	}
	actor.mu.Lock()
	defer actor.mu.Unlock()
	state := actor.engine.Snapshot()
	entries, err := r.rendererQueueEntries(rawEntries, state.Presentation)
	if err != nil {
		return nil, err
	}
	rawDigest, err := json.Marshal(entries)
	if err != nil {
		return nil, err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(rawDigest))
	if err := actor.engine.Apply(chat.ReplaceStagedQueue{
		OperationID: operationID, Digest: digest, ExpectedRevision: expectedRevision, Entries: entries,
	}); err != nil {
		return nil, err
	}
	state = actor.engine.Snapshot()
	receipt, ok := state.QueueMutationReceipts[operationID]
	if !ok || receipt.Digest != digest {
		return nil, errors.New("queue replacement lost its durable actor receipt")
	}
	return map[string]any{
		"ok": true, "tabId": tabID, "chatId": chatID,
		"operationId": string(operationID), "agentQueueRevision": receipt.Revision, "actorRevision": state.Revision,
	}, nil
}

func (r *providerChatRuntime) SavePresentation(tabID, chatID string, operationID providercontract.OperationID, expectedRevision uint64, raw map[string]any) (map[string]any, error) {
	actor, _, err := r.exactActor(tabID, chatID)
	if err != nil {
		return nil, err
	}
	operationID = providercontract.NormalizeOperationID(string(operationID))
	if operationID == "" {
		return nil, errors.New("presentation save requires a stable operation id")
	}
	allowed := map[string]struct{}{
		"tabId": {}, "chatId": {}, "operationId": {}, "expectedRevision": {},
		"title": {}, "titleLocked": {}, "group": {}, "draft": {}, "unread": {}, "settled": {}, "pane": {},
	}
	if err := requireOnlyKeys(raw, allowed, "renderer presentation save"); err != nil {
		return nil, err
	}
	actor.mu.Lock()
	defer actor.mu.Unlock()
	state := actor.engine.Snapshot()
	presentation := state.Presentation.Clone()
	presentation.PresentationRevision = expectedRevision
	presentation.Title = redactedSessionString(fieldString(raw, "title"))
	presentation.TitleLocked = boolFieldValue(raw, "titleLocked")
	presentation.Group = optionalStringPointer(raw, "group")
	presentation.Draft = redactedSessionString(stringValue(raw["draft"]))
	presentation.Unread = boolFieldValue(raw, "unread")
	presentation.Settled = fieldString(raw, "settled")
	presentation.Pane = optionalStringPointer(raw, "pane")
	digestPayload := struct {
		ExpectedRevision uint64
		Title            string
		TitleLocked      bool
		Group            *string
		Draft            string
		Unread           bool
		Settled          string
		Pane             *string
	}{expectedRevision, presentation.Title, presentation.TitleLocked, presentation.Group, presentation.Draft, presentation.Unread, presentation.Settled, presentation.Pane}
	digestRaw, err := json.Marshal(digestPayload)
	if err != nil {
		return nil, err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(digestRaw))
	if err := actor.engine.Apply(chat.SavePresentation{OperationID: operationID, Digest: digest, Presentation: presentation}); err != nil {
		return nil, err
	}
	state = actor.engine.Snapshot()
	receipt, ok := state.PresentationMutationReceipts[operationID]
	if !ok || receipt.Digest != digest {
		return nil, errors.New("presentation save lost its durable actor receipt")
	}
	return map[string]any{
		"ok": true, "tabId": tabID, "chatId": chatID, "operationId": string(operationID),
		"presentationRevision": receipt.Revision, "actorRevision": state.Revision,
	}, nil
}

func (r *providerChatRuntime) SaveRuntimeControls(tabID, chatID string, operationID providercontract.OperationID, expectedRevision uint64, raw map[string]any) (map[string]any, error) {
	actor, _, err := r.exactActor(tabID, chatID)
	if err != nil {
		return nil, err
	}
	operationID = providercontract.NormalizeOperationID(string(operationID))
	if operationID == "" {
		return nil, errors.New("runtime-control save requires a stable operation id")
	}
	allowed := map[string]struct{}{
		"tabId": {}, "chatId": {}, "operationId": {}, "expectedRevision": {},
		"providerId": {}, "currentModelId": {}, "currentModeId": {}, "modelControls": {},
	}
	if err := requireOnlyKeys(raw, allowed, "renderer runtime-control save"); err != nil {
		return nil, err
	}
	providerID := providercontract.NormalizeID(fieldString(raw, "providerId"))
	if providerID == "" {
		return nil, errors.New("runtime-control save requires a provider id")
	}
	var modelControls json.RawMessage
	if value, exists := raw["modelControls"]; exists && value != nil {
		modelControls, err = json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("encode runtime model controls: %w", err)
		}
	}
	update := chat.UpdateRuntimeControls{
		ProviderID:     providerID,
		ModelID:        hydratableStoredModelID(fieldString(raw, "currentModelId")),
		ModeID:         strings.TrimSpace(fieldString(raw, "currentModeId")),
		ReplaceModelID: true,
		ReplaceModeID:  true,
		ModelControls:  modelControls, ReplaceModelControls: true,
		ExpectedRevision: expectedRevision, RequireRevision: true,
	}
	digestPayload := struct {
		ExpectedRevision uint64
		ProviderID       providercontract.ID
		ModelID          string
		ModeID           string
		ModelControls    json.RawMessage
	}{expectedRevision, update.ProviderID, update.ModelID, update.ModeID, update.ModelControls}
	digestRaw, err := json.Marshal(digestPayload)
	if err != nil {
		return nil, err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(digestRaw))

	actor.mu.Lock()
	defer actor.mu.Unlock()
	if err := actor.engine.Apply(chat.SaveRuntimeControls{OperationID: operationID, Digest: digest, Update: update}); err != nil {
		return nil, err
	}
	state := actor.engine.Snapshot()
	receipt, ok := state.RuntimeControlMutationReceipts[operationID]
	if !ok || receipt.Digest != digest {
		return nil, errors.New("runtime-control save lost its durable actor receipt")
	}
	result := map[string]any{
		"ok": true, "tabId": tabID, "chatId": chatID, "operationId": string(operationID),
		"runtimeControlRevision": receipt.Revision, "actorRevision": state.Revision,
		"providerId":     string(state.Presentation.ProviderID),
		"currentModelId": nullableDigestString(state.Presentation.CurrentModelID),
		"currentModeId":  nullableDigestString(state.Presentation.CurrentModeID),
	}
	if err := setRawProjection(result, "modelControls", state.Presentation.ModelControls); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *providerChatRuntime) rendererQueueEntries(rawEntries []any, presentation chat.PresentationState) ([]chat.StagedQueueEntry, error) {
	entries := make([]chat.StagedQueueEntry, 0, len(rawEntries))
	for index, raw := range rawEntries {
		item := mapFromAnyMain(raw)
		allowed := map[string]struct{}{
			"id": {}, "text": {}, "source": {}, "delivery": {}, "queuedAt": {}, "images": {},
			"attachmentNames": {}, "attachmentState": {}, "attachmentError": {},
		}
		if err := requireOnlyKeys(item, allowed, fmt.Sprintf("renderer queue entry %d", index)); err != nil {
			return nil, err
		}
		attachments, err := r.sessions.PersistProviderAttachments(anySlice(item["images"]))
		if err != nil {
			return nil, fmt.Errorf("persist renderer queue entry %d attachments: %w", index, err)
		}
		names := make([]string, 0, len(anySlice(item["attachmentNames"])))
		for _, rawName := range anySlice(item["attachmentNames"]) {
			if name := strings.TrimSpace(stringValue(rawName)); name != "" {
				names = append(names, name)
			}
		}
		entry := chat.StagedQueueEntry{
			ID: fieldString(item, "id"), Text: redactedSessionString(fieldString(item, "text")),
			Source: fieldString(item, "source"), Delivery: fieldString(item, "delivery"), QueuedAt: fieldString(item, "queuedAt"),
			Attachments: attachments, AttachmentNames: names, AttachmentState: fieldString(item, "attachmentState"),
			AttachmentError:  redactedSessionString(fieldString(item, "attachmentError")),
			TargetProviderID: presentation.ProviderID, ModelID: presentation.CurrentModelID, ModeID: presentation.CurrentModeID,
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// exactActor is the common identity fence for desktop, mobile, LAN, and agent
// control. Once a ChatID is actor-owned, no caller is allowed to fall back to
// the legacy mirror merely because its disposable tab attachment is stale.
func (r *providerChatRuntime) exactActor(tabID, chatID string) (*providerChatActor, chat.State, error) {
	if r == nil {
		return nil, chat.State{}, errors.New("provider chat runtime is unavailable")
	}
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if tabID == "" || chatID == "" {
		return nil, chat.State{}, errors.New("exact tab and chat ids are required")
	}
	actor, err := r.actor(chatID)
	if err != nil {
		return nil, chat.State{}, err
	}
	state := actor.engine.Snapshot()
	if state.Deleted {
		return nil, chat.State{}, errors.New("chat was deleted")
	}
	if strings.TrimSpace(state.Presentation.TabID) != tabID {
		return nil, chat.State{}, errors.New("tab id does not own the requested chat")
	}
	return actor, state, nil
}

// ChatCommands reads the provider capability snapshot from the durable actor.
// The manager's command cache is executor materialization only and is never a
// late-client or restart authority after cutover.
func (r *providerChatRuntime) ChatCommands(tabID, chatID string) (map[string]any, error) {
	_, state, err := r.exactActor(tabID, chatID)
	if err != nil {
		return nil, err
	}
	laneID := state.ActiveLaneID
	if laneID == "" {
		laneID = state.DesiredLaneID
	}
	lane, ok := state.Lanes[laneID]
	if !ok || lane.Attachment == nil {
		return map[string]any{"supported": false, "live": false, "commandCatalog": nil}, nil
	}
	snapshot := lane.Attachment
	return map[string]any{
		"supported":      snapshot.CommandCatalogSupported,
		"live":           true,
		"commandCatalog": projectRuntimeCommandCatalog(snapshot.CommandCatalog),
	}, nil
}

func (r *providerChatRuntime) Rewind(ctx context.Context, tabID, chatID string, turnSequence int, operationID providercontract.OperationID) (map[string]any, error) {
	actor, _, err := r.exactActor(tabID, chatID)
	if err != nil {
		return nil, err
	}
	operationID = providercontract.NormalizeOperationID(string(operationID))
	if operationID == "" || turnSequence <= 0 {
		return nil, errors.New("checkpoint restore requires stable operationId and positive turnSeq")
	}
	actor.mu.Lock()
	defer actor.mu.Unlock()
	if err := actor.engine.Apply(chat.RestoreCheckpoint{
		OperationID: operationID, TurnSequence: turnSequence, ObservedAtUnixMS: time.Now().UnixMilli(),
	}); err != nil {
		return nil, err
	}
	if err := actor.coordinator.Drain(ctx); err != nil {
		return nil, err
	}
	state := actor.engine.Snapshot()
	for _, entry := range state.Outbox {
		if entry.Kind != chat.EffectCheckpointRestore || entry.OperationID != operationID {
			continue
		}
		switch entry.Status {
		case chat.OutboxCompleted:
			var result map[string]any
			if err := json.Unmarshal(entry.Result, &result); err != nil {
				return nil, fmt.Errorf("decode checkpoint restore receipt: %w", err)
			}
			result["operationId"] = string(operationID)
			result["actorRevision"] = state.Revision
			return result, nil
		case chat.OutboxAmbiguous:
			return nil, &providercontract.Error{Kind: providercontract.ErrorAcceptanceAmbiguous, Message: "checkpoint restore may have executed; Workass will not repeat it"}
		case chat.OutboxFailed:
			return nil, &providercontract.Error{Kind: entry.LastError, Message: "checkpoint restore failed before a durable receipt"}
		default:
			return nil, fmt.Errorf("checkpoint restore is still %s", entry.Status)
		}
	}
	return nil, errors.New("checkpoint restore lost its durable actor receipt")
}

func (r *providerChatRuntime) ReadChat(tabID, chatID string, limit int, includeEvents bool) (map[string]any, error) {
	actor, state, err := r.exactActor(tabID, chatID)
	if err != nil {
		return nil, err
	}
	actor.mu.Lock()
	state = actor.engine.Snapshot()
	actor.mu.Unlock()
	projected := map[string]any{}
	if err := projectActorChat(projected, state); err != nil {
		return nil, err
	}
	messages := anySlice(projected["messages"])
	truncated := false
	if limit > 0 && len(messages) > limit {
		messages = messages[len(messages)-limit:]
		truncated = true
	}
	copyMessages := make([]any, 0, len(messages))
	for _, raw := range messages {
		message := mapFromAnyMain(cloneJSON(raw))
		if !includeEvents {
			message["events"] = []any{}
		}
		copyMessages = append(copyMessages, message)
	}
	result := map[string]any{
		"tabId": tabID, "chatId": chatID, "title": fieldString(projected, "title"),
		"cwd": fieldString(projected, "cwd"), "providerId": fieldString(projected, "providerId"),
		"modelId": fieldString(projected, "currentModelId"), "modeId": fieldString(projected, "currentModeId"),
		"messages": copyMessages, "truncated": truncated,
	}
	if state.Foreground != nil {
		result["running"] = true
		result["jobId"] = state.Foreground.Turn.NativeID
	} else {
		result["running"] = false
	}
	return result, nil
}

func (r *providerChatRuntime) ListChats() ([]map[string]any, error) {
	root, err := r.ProjectSession()
	if err != nil {
		return nil, err
	}
	result := make([]map[string]any, 0, len(anySlice(root["chats"])))
	for _, raw := range anySlice(root["chats"]) {
		item := mapFromAnyMain(raw)
		tabID, chatID := fieldString(item, "id"), fieldString(item, "chatId")
		listed := map[string]any{
			"tabId": tabID, "chatId": chatID, "title": fieldString(item, "title"),
			"cwd": fieldString(item, "cwd"), "providerId": fieldString(item, "providerId"),
			"modelId": fieldString(item, "currentModelId"), "modeId": fieldString(item, "currentModeId"),
			"active": fieldString(root, "activeId") == tabID,
		}
		state, ok := r.Snapshot(chatID)
		if ok && state.Foreground != nil {
			listed["running"] = true
			listed["jobId"] = state.Foreground.Turn.NativeID
		} else {
			listed["running"] = false
		}
		result = append(result, listed)
	}
	return result, nil
}

func (r *providerChatRuntime) Foreground(tabID, chatID string) (string, bool, error) {
	actor, state, err := r.exactActor(tabID, chatID)
	if err != nil {
		return "", false, err
	}
	actor.mu.Lock()
	state = actor.engine.Snapshot()
	actor.mu.Unlock()
	if state.Foreground == nil {
		return "", false, nil
	}
	return strings.TrimSpace(state.Foreground.Turn.NativeID), true, nil
}

func (r *providerChatRuntime) CreateChat(title, cwd string, controls resolvedChatControls, focus bool, operationID providercontract.OperationID) (map[string]any, error) {
	if r == nil {
		return nil, errors.New("provider chat runtime is unavailable")
	}
	operationID = providercontract.NormalizeOperationID(string(operationID))
	if operationID == "" {
		return nil, errors.New("chat creation requires a stable operation id")
	}
	title = redactedSessionString(title)
	if title == "" {
		title = "Nuevo chat"
	}
	if len(title) > 200 {
		title = title[:200]
	}
	tabID, chatID := stableAgentChatIdentity("tab", operationID), stableAgentChatIdentity("chat", operationID)
	modelControls, err := updateActorModelControls(nil, controls)
	if err != nil {
		return nil, err
	}
	presentation := chat.PresentationState{
		TabID: tabID, Title: title, TitleLocked: true,
		ProviderID:     providercontract.NormalizeID(controls.ProviderID),
		CurrentModelID: hydratableStoredModelID(controls.ModelID), CurrentModeID: strings.TrimSpace(controls.ModeID),
		ModelControls: modelControls,
	}
	if cwd = strings.TrimSpace(cwd); cwd != "" {
		presentation.CWD = &cwd
	}
	if _, err := r.actorForNewChatOperation(chatID, presentation, operationID); err != nil {
		return nil, err
	}
	if focus {
		if _, err := r.sessions.SaveGlobalActiveTab(tabID, providercontract.OperationID("focus:"+string(operationID))); err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"tabId": tabID, "chatId": chatID, "title": title, "active": focus,
		"providerId": string(presentation.ProviderID), "modelId": controls.BaseModel, "modeId": presentation.CurrentModeID,
		"operationId": string(operationID),
	}, nil
}

func stableAgentChatIdentity(prefix string, operationID providercontract.OperationID) string {
	digest := sha256.Sum256([]byte("agent-chat-create-v1\x00" + strings.TrimSpace(prefix) + "\x00" + string(operationID)))
	return fmt.Sprintf("%s-%x", strings.TrimSpace(prefix), digest[:12])
}

func (r *providerChatRuntime) RenameChat(tabID, chatID, title string, operationID providercontract.OperationID) error {
	title = redactedSessionString(title)
	if title == "" {
		return errors.New("title is required")
	}
	if len(title) > 200 {
		title = title[:200]
	}
	_, state, err := r.exactActor(tabID, chatID)
	if err != nil {
		return err
	}
	_, err = r.SavePresentation(tabID, chatID, operationID, state.Presentation.PresentationRevision, map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": string(operationID),
		"expectedRevision": state.Presentation.PresentationRevision,
		"title":            title, "titleLocked": true, "group": optionalPointerValue(state.Presentation.Group),
		"draft": state.Presentation.Draft, "unread": state.Presentation.Unread,
		"settled": state.Presentation.Settled, "pane": optionalPointerValue(state.Presentation.Pane),
	})
	return err
}

func optionalPointerValue(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func (r *providerChatRuntime) FocusChat(tabID, chatID string, operationID providercontract.OperationID) error {
	if _, _, err := r.exactActor(tabID, chatID); err != nil {
		return err
	}
	_, err := r.sessions.SaveGlobalActiveTab(strings.TrimSpace(tabID), operationID)
	return err
}

func (r *providerChatRuntime) ConfigureChat(ctx context.Context, tabID, chatID, cwd string, controls resolvedChatControls, operationID providercontract.OperationID) error {
	operationID = providercontract.NormalizeOperationID(string(operationID))
	if operationID == "" {
		return errors.New("chat configuration requires a stable operation id")
	}
	actor, state, err := r.exactActor(tabID, chatID)
	if err != nil {
		return err
	}
	cwd = strings.TrimSpace(cwd)
	currentCWD := ""
	if state.Presentation.CWD != nil {
		currentCWD = strings.TrimSpace(*state.Presentation.CWD)
	}
	if cwd != "" && cwd != currentCWD {
		moveResult, moveErr := r.MoveWorkspace(ctx, map[string]any{
			"tabId": tabID, "chatId": chatID, "cwd": cwd,
			"operationId": string(operationID), "workspaceRebind": true,
			"providerId": controls.ProviderID, "modelId": controls.ModelID, "modeId": controls.ModeID,
			"expectedWorkspaceRevision": int(state.Presentation.WorkspaceRevision),
		})
		if moveErr != nil {
			return moveErr
		}
		if fieldString(moveResult, "error") != "" || moveResult["workspaceCommitted"] != true || moveResult["workspaceRebound"] != true {
			return fmt.Errorf("workspace move did not commit: %s", firstNonEmptyString(fieldString(moveResult, "error"), "unknown workspace move failure"))
		}
		// A ConfigureChat retry may arrive after the workspace receipt committed
		// but before this controls command was persisted. MoveWorkspace reads that
		// receipt and never closes/recreates a provider host a second time.
		if moveResult["operationId"] != string(operationID) {
			return errors.New("workspace move receipt returned the wrong operation id")
		}
	}
	actor.mu.Lock()
	defer actor.mu.Unlock()
	state = actor.engine.Snapshot()
	modelControls, err := updateActorModelControls(state.Presentation.ModelControls, controls)
	if err != nil {
		return err
	}
	update := chat.UpdateRuntimeControls{
		ProviderID: providercontract.NormalizeID(controls.ProviderID), ModelID: controls.ModelID, ModeID: controls.ModeID,
		ReplaceModelID: true, ReplaceModeID: true,
		ModelControls: modelControls, ReplaceModelControls: true,
		ExpectedRevision: state.Presentation.RuntimeControlRevision, RequireRevision: true,
	}
	// ConfigureChat is a compound logical action. Its retry may re-read the
	// actor after the workspace half has committed, so the daemon-issued CAS
	// revision can legitimately be newer than on the first attempt. Digest only
	// the requested controls, not that transient expected revision; the reducer
	// still enforces the current revision on a first application.
	digestUpdate := update
	digestUpdate.ExpectedRevision = 0
	digestUpdate.RequireRevision = false
	digestRaw, err := json.Marshal(digestUpdate)
	if err != nil {
		return err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(digestRaw))
	if err := actor.engine.Apply(chat.SaveRuntimeControls{
		OperationID: operationID, Digest: digest, Update: update,
	}); err != nil {
		return err
	}
	committed := actor.engine.Snapshot()
	receipt, ok := committed.RuntimeControlMutationReceipts[operationID]
	if !ok || receipt.Digest != digest {
		return errors.New("chat configuration lost its durable controls receipt")
	}
	return nil
}

func updateActorModelControls(raw json.RawMessage, controls resolvedChatControls) (json.RawMessage, error) {
	var memory map[string]any
	if len(raw) != 0 && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &memory); err != nil {
			return nil, err
		}
	}
	if memory == nil {
		memory = map[string]any{}
	}
	if controls.ProviderID != "" && controls.BaseModel != "" {
		providerMemory := mapFromAnyMain(memory[controls.ProviderID])
		entry := map[string]any{}
		if controls.Effort != "" {
			entry["effort"] = controls.Effort
		}
		if controls.ModeID != "" {
			entry["modeId"] = controls.ModeID
		}
		providerMemory[modelControlBaseKey(controls.BaseModel)] = entry
		memory[controls.ProviderID] = providerMemory
	}
	return json.Marshal(memory)
}

func (r *providerChatRuntime) DeleteChat(ctx context.Context, tabID, chatID string, operationID providercontract.OperationID, force bool) error {
	if r == nil {
		return errors.New("provider chat runtime is unavailable")
	}
	actor, err := r.actor(chatID)
	if err != nil {
		return err
	}
	actor.mu.Lock()
	state := actor.engine.Snapshot()
	if strings.TrimSpace(state.Presentation.TabID) != strings.TrimSpace(tabID) {
		actor.mu.Unlock()
		return errors.New("tab id does not own the requested chat")
	}
	if err := actor.engine.Apply(chat.DeleteChat{OperationID: operationID, Force: force}); err != nil {
		actor.mu.Unlock()
		return err
	}
	drainErr := actor.coordinator.Drain(ctx)
	closeErr := actor.coordinator.Close(ctx)
	actor.mu.Unlock()
	if root := r.sessions.GlobalSnapshot(); fieldString(root, "activeId") == tabID {
		_, _ = r.sessions.SaveGlobalActiveTab("", providercontract.OperationID("delete-focus:"+string(operationID)))
	}
	return errors.Join(drainErr, closeErr)
}

func (r *providerChatRuntime) ApplySessionRefresh(payload map[string]any) error {
	if r == nil || fieldString(payload, "action") != "session-refresh" {
		return nil
	}
	tabID, chatID, sessionID := fieldString(payload, "tabId"), fieldString(payload, "chatId"), fieldString(payload, "sessionId")
	if tabID == "" || chatID == "" || sessionID == "" {
		return errors.New("session refresh requires exact tab, chat, and provider connection ids")
	}
	actor, _, err := r.exactActor(tabID, chatID)
	if err != nil {
		return err
	}
	actor.mu.Lock()
	defer actor.mu.Unlock()
	state := actor.engine.Snapshot()
	laneID := state.ActiveLaneID
	if state.Foreground != nil {
		laneID = state.Foreground.LaneID
	} else if laneID == "" {
		laneID = state.DesiredLaneID
	}
	lane, ok := state.Lanes[laneID]
	if !ok || lane.Attachment == nil || strings.TrimSpace(lane.Attachment.ConnectionID) != sessionID {
		return errors.New("session refresh belongs to a stale provider attachment")
	}
	providerID := providercontract.NormalizeID(fieldString(payload, "providerId"))
	if providerID != "" && providerID != lane.Identity.Realm.ProviderID {
		return errors.New("session refresh provider does not own the current lane")
	}
	return actor.engine.Apply(chat.UpdateRuntimeControls{
		ProviderID: lane.Identity.Realm.ProviderID,
		ModelID:    firstNonEmptyString(fieldString(payload, "modelId"), fieldString(payload, "currentModelId")),
		ModeID:     firstNonEmptyString(fieldString(payload, "modeId"), fieldString(payload, "currentModeId")),
	})
}

func (r *providerChatRuntime) QueueAgentMessage(ctx context.Context, tabID, chatID string, operationID providercontract.OperationID, message, delivery string) (map[string]any, error) {
	message = redactedSessionString(message)
	if strings.TrimSpace(message) == "" {
		return nil, errors.New("message is required")
	}
	operationID = providercontract.NormalizeOperationID(string(operationID))
	if operationID == "" {
		return nil, errors.New("chat send requires a stable operation id")
	}
	actor, _, err := r.exactActor(tabID, chatID)
	if err != nil {
		return nil, err
	}
	queueID := stableAgentMessageIdentity("q", operationID)
	if delivery == "steer" {
		result, handled, err := r.SteerQueued(ctx, tabID, chatID, queueID, message)
		if err != nil {
			return nil, err
		}
		if !handled {
			return nil, errors.New("chat actor did not own steering")
		}
		result["queueId"] = queueID
		result["operationId"] = string(operationID)
		result["delivery"] = firstNonEmptyString(fieldString(result, "strategy"), "steer")
		return result, nil
	}
	actor.mu.Lock()
	state := actor.engine.Snapshot()
	if _, exists := state.Operations[operationID]; exists {
		receipt := queuedActorReceipt(state, operationID, queueID)
		actor.mu.Unlock()
		return receipt, nil
	}
	opts := acp.SessionOptions{TabID: tabID, ChatID: chatID}
	if state.Presentation.CWD != nil {
		opts.CWD = strings.TrimSpace(*state.Presentation.CWD)
	}
	opts.ProviderID = string(state.Presentation.ProviderID)
	opts.ModelID = state.Presentation.CurrentModelID
	opts.ModeID = state.Presentation.CurrentModeID
	opts.OperationID = operationID
	selection, err := r.resolveSelectionLocked(ctx, actor, opts)
	if err == nil {
		err = r.commitLaneSelectionLocked(actor, selection, opts)
	}
	if err == nil {
		err = actor.engine.Apply(chat.Submit{
			OperationID: operationID, LaneID: selection.Identity.ID, Text: message,
			ModelID: selection.ModelID, ModeID: selection.ModeID,
			Presentation: providercontract.TurnPresentation{
				UserMessageID: stableAgentMessageIdentity("u", operationID), AssistantMessageID: stableAgentMessageIdentity("a", operationID), QueueID: queueID,
				PromptText: message, Title: state.Presentation.Title, Origin: "agent", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
			},
		})
	}
	if err != nil {
		actor.mu.Unlock()
		return nil, err
	}
	receipt := queuedActorReceipt(actor.engine.Snapshot(), operationID, queueID)
	actor.mu.Unlock()
	// Agent control is headless. The durable queue receipt returns immediately;
	// provider cold-start and turn execution continue from the actor outbox.
	go func() { _ = actor.coordinator.Drain(context.Background()) }()
	return receipt, nil
}

func stableAgentMessageIdentity(prefix string, operationID providercontract.OperationID) string {
	digest := sha256.Sum256([]byte("agent-chat-v1\x00" + strings.TrimSpace(prefix) + "\x00" + string(operationID)))
	return fmt.Sprintf("%s-%x", strings.TrimSpace(prefix), digest[:12])
}

func (r *providerChatRuntime) RuntimeControls(tabID, chatID string) (string, string, string, bool, error) {
	_, state, err := r.exactActor(tabID, chatID)
	if err != nil {
		if errors.Is(err, errActorChatNotFound) {
			return "", "", "", false, nil
		}
		return "", "", "", false, err
	}
	return string(state.Presentation.ProviderID), state.Presentation.CurrentModelID, state.Presentation.CurrentModeID, true, nil
}

func (r *providerChatRuntime) MostRecentAssistantJobID(tabID, chatID string) (string, error) {
	_, state, err := r.exactActor(tabID, chatID)
	if err != nil {
		return "", err
	}
	if state.Foreground != nil && strings.TrimSpace(state.Foreground.Turn.NativeID) != "" {
		return strings.TrimSpace(state.Foreground.Turn.NativeID), nil
	}
	for index := len(state.Ledger) - 1; index >= 0; index-- {
		if state.Ledger[index].Role == "assistant" && strings.TrimSpace(state.Ledger[index].NativeTurnID) != "" {
			return strings.TrimSpace(state.Ledger[index].NativeTurnID), nil
		}
	}
	return "", nil
}
