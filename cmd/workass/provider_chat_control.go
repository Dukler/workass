package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

// workass_read_chat is returned as JSON inside an MCP text block, so its JSON
// bytes are escaped once more by the stateless MCP response. Keep the inner
// result well below the stdio relay's 4 MiB ceiling. This is a response budget,
// not a persistence limit: the actor retains the complete semantic ledger.
const maxAgentChatReadJSONBytes = 1 * 1024 * 1024

// maxAgentChatReadMessageTextBytes bounds one message's text in a chat read,
// as Claude Code's Read truncates an over-long line instead of dropping the
// file: one huge message no longer spends the whole response budget and hides
// every older row.
const maxAgentChatReadMessageTextBytes = 64 * 1024

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
	// Chat and tab ids become file names (state, archives, spawned-work
	// caches). An unbounded id was accepted here, and later its cache file name
	// was too long to remove, which stopped the daemon from booting at all
	// (2026-10-11 QA). Same character rule as operation ids, bounded short.
	for _, id := range []string{tabID, chatID} {
		if err := validateChatIdentifier(id); err != nil {
			return nil, err
		}
	}
	presentation := chat.PresentationState{
		TabID: tabID, Title: sanitizedChatTitle(fieldString(raw, "title")),
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
	focus := boolFieldValue(raw, "focus")
	actor, err := r.actorForNewChatOperation(chatID, presentation, operationID, focus)
	if err != nil {
		return nil, err
	}
	state := actor.engine.ReadSnapshot()
	globalRevision := uint64(0)
	if focus {
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

// maxStagedQueueRows bounds one chat's editable queue; every row is
// persisted and re-projected on each change.
const maxStagedQueueRows = 100

func (r *providerChatRuntime) ReplaceStagedQueue(tabID, chatID string, operationID providercontract.OperationID, expectedRevision uint64, rawEntries []any) (map[string]any, error) {
	actor, err := r.exactActorFence(tabID, chatID)
	if err != nil {
		return nil, err
	}
	operationID = providercontract.NormalizeOperationID(string(operationID))
	if operationID == "" {
		return nil, errors.New("queue replacement requires a stable operation id")
	}
	if len(rawEntries) > maxStagedQueueRows {
		return nil, fmt.Errorf("a chat queue holds at most %d messages", maxStagedQueueRows)
	}
	actor.mu.Lock()
	defer actor.mu.Unlock()
	state := actor.engine.ReadSnapshot()
	entries, attachmentPlans, err := r.rendererQueueEntries(rawEntries, state.Presentation)
	if err != nil {
		return nil, err
	}
	digest, err := stagedQueueRequestDigest(entries)
	if err != nil {
		return nil, err
	}
	if err := actor.engine.ApplyPrepared(chat.ReplaceStagedQueue{
		OperationID: operationID, Digest: digest, ExpectedRevision: expectedRevision, Entries: entries,
	}, func() error { return materializeProviderAttachmentPlans(attachmentPlans) }); err != nil {
		return nil, err
	}
	state = actor.engine.ReadSnapshot()
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
	actor, err := r.exactActorFence(tabID, chatID)
	if err != nil {
		return nil, err
	}
	operationID = providercontract.NormalizeOperationID(string(operationID))
	if operationID == "" {
		return nil, errors.New("presentation save requires a stable operation id")
	}
	allowed := map[string]struct{}{
		"tabId": {}, "chatId": {}, "operationId": {}, "expectedRevision": {},
		"title": {}, "titleLocked": {}, "group": {}, "draft": {}, "unread": {}, "settled": {}, "settledAt": {}, "pane": {},
	}
	if err := requireOnlyKeys(raw, allowed, "renderer presentation save"); err != nil {
		return nil, err
	}
	actor.mu.Lock()
	defer actor.mu.Unlock()
	state := actor.engine.ReadSnapshot()
	presentation := state.Presentation.Clone()
	presentation.PresentationRevision = expectedRevision
	presentation.Title = sanitizedChatTitle(fieldString(raw, "title"))
	presentation.TitleLocked = boolFieldValue(raw, "titleLocked")
	presentation.Group = optionalStringPointer(raw, "group")
	var draftIntent any
	if draft, exists := raw["draft"]; exists {
		presentation.Draft = redactedSessionString(stringValue(draft))
		draftIntent = presentation.Draft
	}
	presentation.Unread = boolFieldValue(raw, "unread")
	presentation.Settled = fieldString(raw, "settled")
	presentation.SettledAt = int64(max(0, intField(raw, "settledAt")))
	presentation.Pane = optionalStringPointer(raw, "pane")
	// ExpectedRevision is a compare-and-swap precondition, not part of the
	// immutable user intent. If the actor commits this operation and its reply is
	// lost, session:get advances the renderer to the committed revision before it
	// retries the same operation. Keeping the precondition out of the digest lets
	// that retry recover the existing receipt while a changed presentation still
	// fails closed as operation-id reuse.
	digestPayload := struct {
		Title       string
		TitleLocked bool
		Group       *string
		Draft       any
		Unread      bool
		Settled     string
		SettledAt   int64
		Pane        *string
	}{presentation.Title, presentation.TitleLocked, presentation.Group, draftIntent, presentation.Unread, presentation.Settled, presentation.SettledAt, presentation.Pane}
	digestRaw, err := json.Marshal(digestPayload)
	if err != nil {
		return nil, err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(digestRaw))
	if receipt, exists := state.PresentationMutationReceipts[operationID]; exists {
		if receipt.Digest != digest {
			return nil, errors.New("presentation operation id was reused for different content")
		}
		return map[string]any{
			"ok": true, "tabId": tabID, "chatId": chatID, "operationId": string(operationID),
			"presentationRevision": receipt.Revision, "actorRevision": state.Revision,
		}, nil
	}
	if err := actor.engine.Apply(chat.SavePresentation{OperationID: operationID, Digest: digest, Presentation: presentation}); err != nil {
		return nil, err
	}
	state = actor.engine.ReadSnapshot()
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
	actor, err := r.exactActorFence(tabID, chatID)
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
	// An unknown provider would persist and fail every later send. Model and
	// mode stay as given: a stale control is reconciled at the next prompt.
	if r.manager != nil {
		if _, err := r.manager.ProviderDefinition(string(providerID)); err != nil {
			return nil, fmt.Errorf("runtime-control save names an unavailable provider: %w", err)
		}
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
	state := actor.engine.ReadSnapshot()
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

func (r *providerChatRuntime) rendererQueueEntries(rawEntries []any, presentation chat.PresentationState) ([]chat.StagedQueueEntry, []providerAttachmentPlan, error) {
	entries := make([]chat.StagedQueueEntry, 0, len(rawEntries))
	plans := make([]providerAttachmentPlan, 0, len(rawEntries))
	for index, raw := range rawEntries {
		item := mapFromAnyMain(raw)
		allowed := map[string]struct{}{
			"id": {}, "text": {}, "source": {}, "delivery": {}, "queuedAt": {}, "images": {},
			"attachmentNames": {}, "attachmentState": {}, "attachmentError": {},
			// Display-only, owned by the daemon: renderers echo queued rows
			// back whole (an older bundle never strips it), so it is accepted
			// and ignored (review wa-subagent-1792186068644-7, correction 1).
			"notice": {},
		}
		if err := requireOnlyKeys(item, allowed, fmt.Sprintf("renderer queue entry %d", index)); err != nil {
			return nil, nil, err
		}
		plan, err := r.sessions.PlanProviderAttachments(anySlice(item["images"]))
		if err != nil {
			return nil, nil, fmt.Errorf("prepare renderer queue entry %d attachments: %w", index, err)
		}
		attachments := plan.Attachments
		plans = append(plans, plan)
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
	return entries, plans, nil
}

// stagedQueueRequestDigest is the queue replacement receipt digest. It hashes
// the staged entries with attachment pixel sizes cleared: the size is display
// metadata read from bytes the attachment digest already names, and a receipt
// recorded by a build without sizes must still match the same request replayed
// by a newer build instead of failing as reused content.
func stagedQueueRequestDigest(entries []chat.StagedQueueEntry) (string, error) {
	hashed := make([]chat.StagedQueueEntry, len(entries))
	for index, entry := range entries {
		entry.Attachments = providercontract.WithoutImageSize(entry.Attachments)
		hashed[index] = entry
	}
	raw, err := json.Marshal(hashed)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}

func materializeProviderAttachmentPlans(plans []providerAttachmentPlan) error {
	for _, plan := range plans {
		if err := plan.Materialize(); err != nil {
			return err
		}
	}
	return nil
}

// exactActor is the common identity fence for desktop, mobile, LAN, and agent
// control. Once a ChatID is actor-owned, no caller is allowed to fall back to
// the removed mirror merely because its disposable tab attachment is stale.
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
	state := actor.engine.ReadSnapshot()
	if state.Deleted {
		return nil, chat.State{}, errors.New("chat was deleted")
	}
	if strings.TrimSpace(state.Presentation.TabID) != tabID {
		return nil, chat.State{}, errors.New("tab id does not own the requested chat")
	}
	return actor, state, nil
}

// exactActorFence is exactActor for callers that need only the actor: the
// same ownership checks, read without copying the chat's history.
// chatProviderID is the provider a chat is CONFIGURED for, read from its
// durable presentation rather than from a live session binding.
//
// The two differ exactly when it matters here: when nobody is running a turn in
// a chat, the manager has no bound provider for it and a registration that asked
// only the binding was refused. The durable answer is the one that is true
// whether or not anybody is talking in that chat right now.
func (r *providerChatRuntime) chatProviderID(tabID, chatID string) string {
	if r == nil {
		return ""
	}
	_, state, err := r.exactActor(tabID, chatID)
	if err != nil {
		return ""
	}
	return string(providercontract.NormalizeID(string(state.Presentation.ProviderID)))
}

func (r *providerChatRuntime) exactActorFence(tabID, chatID string) (*providerChatActor, error) {
	if r == nil {
		return nil, errors.New("provider chat runtime is unavailable")
	}
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if tabID == "" || chatID == "" {
		return nil, errors.New("exact tab and chat ids are required")
	}
	actor, err := r.actor(chatID)
	if err != nil {
		return nil, err
	}
	identity := actor.engine.IdentitySnapshot()
	if identity.Deleted {
		return nil, errors.New("chat was deleted")
	}
	if strings.TrimSpace(identity.TabID) != tabID {
		return nil, errors.New("tab id does not own the requested chat")
	}
	return actor, nil
}

// exactActorOrSummary is exactActorFence for a read that needs only what a
// closed actor's summary carries (its identity and read projection): a closed
// actor answers from the summary, with the same ownership checks, and is not
// opened. Either the actor or the summary is returned.
func (r *providerChatRuntime) exactActorOrSummary(tabID, chatID string) (*providerChatActor, *chat.ActorSummary, error) {
	if r == nil {
		return nil, nil, errors.New("provider chat runtime is unavailable")
	}
	if summary, closed := r.closedSummary(chatID); closed {
		tabID = strings.TrimSpace(tabID)
		if tabID == "" || strings.TrimSpace(chatID) == "" {
			return nil, nil, errors.New("exact tab and chat ids are required")
		}
		if summary.Deleted {
			return nil, nil, errors.New("chat was deleted")
		}
		if strings.TrimSpace(summary.TabID) != tabID {
			return nil, nil, errors.New("tab id does not own the requested chat")
		}
		return nil, &summary, nil
	}
	actor, err := r.exactActorFence(tabID, chatID)
	return actor, nil, err
}

// ChatGoal reads the chat's latest provider goal (Codex /goal) once the tab is
// proven to own the chat. The goal is live provider state, kept per chat by
// the manager; connected clients already have it from the chat:goal event.
// The proof needs only the chat's identity, so a closed actor stays closed.
func (r *providerChatRuntime) ChatGoal(tabID, chatID string) (map[string]any, error) {
	if _, _, err := r.exactActorOrSummary(tabID, chatID); err != nil {
		return nil, err
	}
	if r.manager == nil {
		return map[string]any{"tabId": tabID, "chatId": chatID, "goal": nil}, nil
	}
	return r.manager.ChatGoal(tabID, chatID), nil
}

// ChatCommands reads the provider capability snapshot from the durable actor.
// The manager's command cache is executor materialization only and is never a
// late-client or restart authority after cutover. A closed actor's summary
// carries its lanes and their attachments as committed, so it answers without
// opening the actor (opening a large one took a second, and the ordered lane
// held this read's neighbours behind it).
func (r *providerChatRuntime) ChatCommands(tabID, chatID string) (map[string]any, error) {
	actor, summary, err := r.exactActorOrSummary(tabID, chatID)
	if err != nil {
		return nil, err
	}
	view := func(read func(chat.State)) { actor.engine.View(read) }
	if summary != nil {
		view = summary.View
	}
	var snapshot *providercontract.LaneAttachmentSnapshot
	view(func(state chat.State) {
		laneID := state.ActiveLaneID
		if laneID == "" {
			laneID = state.DesiredLaneID
		}
		if lane, ok := state.Lanes[laneID]; ok && lane.Attachment != nil {
			attachment := lane.Attachment.Clone()
			snapshot = &attachment
		}
	})
	if snapshot == nil {
		return map[string]any{"supported": false, "live": false, "commandCatalog": nil}, nil
	}
	return map[string]any{
		"supported":      snapshot.CommandCatalogSupported,
		"live":           true,
		"commandCatalog": projectRuntimeCommandCatalog(snapshot.CommandCatalog),
	}, nil
}

func (r *providerChatRuntime) Rewind(ctx context.Context, tabID, chatID string, turnSequence int, operationID providercontract.OperationID) (map[string]any, error) {
	actor, err := r.exactActorFence(tabID, chatID)
	if err != nil {
		return nil, err
	}
	operationID = providercontract.NormalizeOperationID(string(operationID))
	if operationID == "" || turnSequence <= 0 {
		return nil, errors.New("checkpoint restore requires stable operationId and positive turnSeq")
	}
	actor.mu.Lock()
	defer actor.mu.Unlock()
	state := actor.engine.ReadSnapshot()
	var checkpoint json.RawMessage
	var checkpointDigest string
	for _, entry := range state.Outbox {
		if entry.OperationID != operationID {
			continue
		}
		if entry.Kind != chat.EffectCheckpointRestore || entry.TurnSequence != turnSequence {
			return nil, errors.New("checkpoint restore operation conflicts with its durable request")
		}
		checkpoint = append(json.RawMessage(nil), entry.Checkpoint...)
		checkpointDigest = entry.CheckpointDigest
		break
	}
	if len(checkpoint) == 0 {
		checkpoint, checkpointDigest, err = state.CheckpointRestoreTarget(turnSequence)
		if err != nil {
			return nil, err
		}
	}
	if err := actor.engine.Apply(chat.RestoreCheckpoint{
		OperationID: operationID, TurnSequence: turnSequence, ObservedAtUnixMS: time.Now().UnixMilli(),
		Checkpoint: checkpoint, CheckpointDigest: checkpointDigest,
	}); err != nil {
		return nil, err
	}
	if err := actor.coordinator.Drain(ctx); err != nil {
		return nil, err
	}
	state = actor.engine.ReadSnapshot()
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
	state = actor.engine.ReadSnapshot()
	actor.mu.Unlock()
	projected := map[string]any{}
	if err := projectActorChat(projected, state); err != nil {
		return nil, err
	}
	messages := anySlice(projected["messages"])
	messageCount := len(messages)
	truncated := false
	if limit > 0 && len(messages) > limit {
		messages = messages[len(messages)-limit:]
		truncated = true
	}
	copyMessages := make([]any, 0, len(messages))
	eventSources := make([][]any, 0, len(messages))
	eventCount := 0
	for _, raw := range messages {
		message := mapFromAnyMain(cloneJSON(raw))
		events := anySlice(message["events"])
		if includeEvents {
			eventCount += len(events)
			eventSources = append(eventSources, events)
		}
		message["events"] = []any{}
		if text, ok := message["content"].(string); ok && len(text) > maxAgentChatReadMessageTextBytes {
			cut := maxAgentChatReadMessageTextBytes
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
			message["content"] = text[:cut] + fmt.Sprintf("\n…[%d more bytes not shown]", len(text)-cut)
			message["contentTruncated"] = true
			message["contentBytes"] = len(text)
		}
		copyMessages = append(copyMessages, message)
	}
	result := map[string]any{
		"tabId": tabID, "chatId": chatID, "title": fieldString(projected, "title"),
		"cwd": fieldString(projected, "cwd"), "providerId": fieldString(projected, "providerId"),
		"modelId": fieldString(projected, "currentModelId"), "modeId": fieldString(projected, "currentModeId"),
		"messages": copyMessages, "messageCount": messageCount, "truncated": truncated,
	}
	if includeEvents {
		// Reserve the longest count and boolean encodings before adding event
		// bytes. Updating these fields after selection can only shrink the JSON.
		result["eventCount"] = eventCount
		result["includedEventCount"] = eventCount
		result["eventsTruncated"] = false
	}
	if actorForegroundRunning(state.Foreground) {
		result["running"] = true
		result["jobId"] = state.Foreground.Turn.NativeID
	} else {
		result["running"] = false
	}

	result["messages"] = []any{}
	emptyBase, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode bounded chat read: %w", err)
	}
	// Preserve the newest complete transcript rows when ordinary message text
	// alone exceeds the response budget. Event counts continue to describe the
	// caller's requested window, making the omitted data explicit.
	baseSize := len(emptyBase)
	firstMessage := len(copyMessages)
	for index := len(copyMessages) - 1; index >= 0; index-- {
		encodedMessage, err := json.Marshal(copyMessages[index])
		if err != nil {
			return nil, fmt.Errorf("encode bounded chat message: %w", err)
		}
		delta := len(encodedMessage)
		if firstMessage < len(copyMessages) {
			delta++ // comma between adjacent array entries
		}
		if baseSize+delta > maxAgentChatReadJSONBytes {
			break
		}
		firstMessage = index
		baseSize += delta
	}
	if firstMessage > 0 {
		copyMessages = copyMessages[firstMessage:]
		if includeEvents {
			eventSources = eventSources[firstMessage:]
		}
		result["truncated"] = true
	}
	result["messages"] = copyMessages
	baseBytes, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode bounded chat read: %w", err)
	}
	if len(baseBytes) > maxAgentChatReadJSONBytes {
		return nil, errors.New("chat metadata exceeds the bounded read response")
	}
	if !includeEvents {
		return result, nil
	}

	remaining := maxAgentChatReadJSONBytes - len(baseBytes)
	starts := make([]int, len(eventSources))
	for index, events := range eventSources {
		starts[index] = len(events)
	}
	includedEventCount := 0
	selectionFull := true
selectEvents:
	for messageIndex := len(eventSources) - 1; messageIndex >= 0; messageIndex-- {
		events := eventSources[messageIndex]
		for eventIndex := len(events) - 1; eventIndex >= 0; eventIndex-- {
			encodedEvent, err := json.Marshal(events[eventIndex])
			if err != nil {
				return nil, fmt.Errorf("encode bounded chat event: %w", err)
			}
			delta := len(encodedEvent)
			if starts[messageIndex] < len(events) {
				delta++ // comma between adjacent array entries
			}
			if delta > remaining {
				selectionFull = false
				break selectEvents
			}
			starts[messageIndex] = eventIndex
			includedEventCount++
			remaining -= delta
		}
	}
	for index, start := range starts {
		if start >= len(eventSources[index]) {
			continue
		}
		message := mapFromAnyMain(copyMessages[index])
		message["events"] = append([]any(nil), eventSources[index][start:]...)
	}
	result["includedEventCount"] = includedEventCount
	result["eventsTruncated"] = !selectionFull || includedEventCount != eventCount
	encodedResult, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode bounded chat read: %w", err)
	}
	if len(encodedResult) > maxAgentChatReadJSONBytes {
		return nil, errors.New("chat read response exceeded its byte budget")
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
			// The sidebar's explicit settle override: "settled", "active"
			// (reactivated), or "" (never filed). Archiving is a sidebar view
			// over settledAt, not stored state.
			"settled": fieldString(item, "settled"), "settledAt": max(0, intField(item, "settledAt")),
		}
		// A subsession names its parent so agents can find a chat's children.
		if parentChatID := fieldString(item, "parentChatId"); parentChatID != "" {
			listed["parentChatId"] = parentChatID
			listed["parentTabId"] = fieldString(item, "parentTabId")
			listed["forked"] = item["subsessionForked"] == true
		}
		state, ok := r.Snapshot(chatID)
		if ok && actorForegroundRunning(state.Foreground) {
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
	state = actor.engine.ReadSnapshot()
	actor.mu.Unlock()
	if !actorForegroundRunning(state.Foreground) {
		return "", false, nil
	}
	return strings.TrimSpace(state.Foreground.Turn.NativeID), true, nil
}

func (r *providerChatRuntime) CreateChat(title, cwd string, controls resolvedChatControls, focus bool, operationID providercontract.OperationID) (map[string]any, error) {
	operationID = providercontract.NormalizeOperationID(string(operationID))
	if operationID == "" {
		return nil, errors.New("chat creation requires a stable operation id")
	}
	title = normalizedHeadlessChatTitle(title)
	presentation, err := headlessChatPresentation(title, cwd, controls)
	if err != nil {
		return nil, err
	}
	presentation.TabID = stableAgentChatIdentity("tab", operationID)
	digest, err := chatCreationDigest(presentation, focus)
	if err != nil {
		return nil, err
	}
	return r.createChatWithIntentDigest(presentation, focus, operationID, digest, nil)
}

func (r *providerChatRuntime) CreateChatWithIntentDigest(title, cwd string, controls resolvedChatControls, focus bool, operationID providercontract.OperationID, digest string) (map[string]any, error) {
	presentation, err := headlessChatPresentation(normalizedHeadlessChatTitle(title), cwd, controls)
	if err != nil {
		return nil, err
	}
	return r.createChatWithIntentDigest(presentation, focus, operationID, digest, nil)
}

func normalizedHeadlessChatTitle(title string) string {
	title = sanitizedChatTitle(title)
	if title == "" {
		title = "Nuevo chat"
	}
	return title
}

// sanitizedChatTitle is the stored and echoed form of an agent-supplied title.
func sanitizedChatTitle(title string) string {
	return acp.SanitizeDisplayText(title, 200)
}

func headlessChatPresentation(title, cwd string, controls resolvedChatControls) (chat.PresentationState, error) {
	modelControls, err := updateActorModelControls(nil, controls)
	if err != nil {
		return chat.PresentationState{}, err
	}
	presentation := chat.PresentationState{
		Title: title, TitleLocked: true,
		ProviderID:     providercontract.NormalizeID(controls.ProviderID),
		CurrentModelID: hydratableStoredModelID(controls.ModelID), CurrentModeID: strings.TrimSpace(controls.ModeID),
		ModelControls: modelControls,
	}
	if cwd = strings.TrimSpace(cwd); cwd != "" {
		presentation.CWD = &cwd
	}
	return presentation, nil
}

// CreateSubsessionWithIntentDigest creates a FRESH subsession: an ordinary
// new chat whose actor records the chat it lives under (user 2026-10-16).
func (r *providerChatRuntime) CreateSubsessionWithIntentDigest(title, cwd string, controls resolvedChatControls, focus bool, operationID providercontract.OperationID, digest string, parent chat.ParentLink) (map[string]any, error) {
	presentation, err := headlessChatPresentation(normalizedHeadlessChatTitle(title), cwd, controls)
	if err != nil {
		return nil, err
	}
	parent.Forked = false
	return r.createChatWithIntentDigest(presentation, focus, operationID, digest, &parent)
}

func (r *providerChatRuntime) createChatWithIntentDigest(presentation chat.PresentationState, focus bool, operationID providercontract.OperationID, digest string, parent *chat.ParentLink) (map[string]any, error) {
	if r == nil {
		return nil, errors.New("provider chat runtime is unavailable")
	}
	operationID = providercontract.NormalizeOperationID(string(operationID))
	digest = strings.TrimSpace(digest)
	if operationID == "" || digest == "" {
		return nil, errors.New("chat creation requires a stable operation id and immutable intent digest")
	}
	tabID, chatID := stableAgentChatIdentity("tab", operationID), stableAgentChatIdentity("chat", operationID)
	presentation = presentation.Clone()
	presentation.TabID = tabID
	actor, err := r.openActor(chatID, &chat.InitializeChat{Presentation: presentation, OperationID: operationID, Digest: digest, Parent: parent.Clone()})
	if err != nil {
		return nil, err
	}
	if focus {
		if _, err := r.sessions.SaveGlobalActiveTab(tabID, providercontract.OperationID("focus:"+string(operationID))); err != nil {
			return nil, err
		}
	}
	return headlessCreateResult(actor.engine.ReadSnapshot(), operationID, focus), nil
}

func (r *providerChatRuntime) existingHeadlessCreate(operationID providercontract.OperationID, digest string, focus bool) (map[string]any, bool, error) {
	if r == nil {
		return nil, false, errors.New("provider chat runtime is unavailable")
	}
	operationID = providercontract.NormalizeOperationID(string(operationID))
	digest = strings.TrimSpace(digest)
	if operationID == "" || digest == "" {
		return nil, false, errors.New("chat creation requires a stable operation id and immutable intent digest")
	}
	tabID, chatID := stableAgentChatIdentity("tab", operationID), stableAgentChatIdentity("chat", operationID)
	actor, err := r.actor(chatID)
	if errors.Is(err, errActorChatNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	actor.mu.Lock()
	state := actor.engine.ReadSnapshot()
	actor.mu.Unlock()
	if !state.Initialized || state.Deleted || state.Presentation.TabID != tabID || state.CreationOperationID != operationID || state.CreationDigest != digest {
		return nil, true, errors.New("chat creation operation conflicts with the existing durable child")
	}
	if focus {
		if _, err := r.sessions.SaveGlobalActiveTab(tabID, providercontract.OperationID("focus:"+string(operationID))); err != nil {
			return nil, true, err
		}
	}
	return headlessCreateResult(state, operationID, focus), true, nil
}

func headlessCreateResult(state chat.State, operationID providercontract.OperationID, focus bool) map[string]any {
	modelID := strings.TrimSpace(state.Presentation.CurrentModelID)
	baseModel, effort, split := acp.SplitCanonicalEffortSuffix(modelID)
	if !split {
		baseModel = modelID
	}
	return map[string]any{
		"tabId": state.Presentation.TabID, "chatId": state.ChatID, "title": state.Presentation.Title, "active": focus,
		"providerId": string(state.Presentation.ProviderID), "modelId": baseModel, "effort": effort,
		"resolvedModelId": modelID, "modeId": state.Presentation.CurrentModeID, "operationId": string(operationID),
	}
}

func stableAgentChatIdentity(prefix string, operationID providercontract.OperationID) string {
	digest := sha256.Sum256([]byte("agent-chat-create-v1\x00" + strings.TrimSpace(prefix) + "\x00" + string(operationID)))
	return fmt.Sprintf("%s-%x", strings.TrimSpace(prefix), digest[:12])
}

func (r *providerChatRuntime) RenameChat(tabID, chatID, title string, operationID providercontract.OperationID) error {
	title = sanitizedChatTitle(title)
	if title == "" {
		return errors.New("title is required")
	}
	operationID = providercontract.NormalizeOperationID(string(operationID))
	if operationID == "" {
		return errors.New("chat rename requires a stable operation id")
	}
	digestRaw, err := json.Marshal(struct {
		Kind   string `json:"kind"`
		TabID  string `json:"tabId"`
		ChatID string `json:"chatId"`
		Title  string `json:"title"`
	}{Kind: "chat-rename-v1", TabID: strings.TrimSpace(tabID), ChatID: strings.TrimSpace(chatID), Title: title})
	if err != nil {
		return err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(digestRaw))
	actor, err := r.exactActorFence(tabID, chatID)
	if err != nil {
		return err
	}
	actor.mu.Lock()
	defer actor.mu.Unlock()
	state := actor.engine.ReadSnapshot()
	if receipt, exists := state.PresentationMutationReceipts[operationID]; exists {
		if receipt.Digest != digest {
			return errors.New("presentation operation id was reused for different content")
		}
		return nil
	}
	presentation := state.Presentation.Clone()
	presentation.Title = title
	presentation.TitleLocked = true
	if err := actor.engine.Apply(chat.SavePresentation{OperationID: operationID, Digest: digest, Presentation: presentation}); err != nil {
		return err
	}
	receipt, ok := actor.engine.ReadSnapshot().PresentationMutationReceipts[operationID]
	if !ok || receipt.Digest != digest {
		return errors.New("chat rename lost its durable actor receipt")
	}
	return nil
}

// SettleChat is the agent twin of the sidebar Settle/Reactivar action
// (renderer Store.settleChat): it files the chat into the settled shelf or
// back into the live list and touches nothing else. A chat already in the
// requested state keeps its settledAt, so a repeated settle never restarts the
// archive clock; the operation still records a receipt so replays stay exact.
func (r *providerChatRuntime) SettleChat(tabID, chatID string, settled bool, operationID providercontract.OperationID) error {
	operationID = providercontract.NormalizeOperationID(string(operationID))
	if operationID == "" {
		return errors.New("chat settle requires a stable operation id")
	}
	digestRaw, err := json.Marshal(struct {
		Kind    string `json:"kind"`
		TabID   string `json:"tabId"`
		ChatID  string `json:"chatId"`
		Settled bool   `json:"settled"`
	}{Kind: "chat-settle-v1", TabID: strings.TrimSpace(tabID), ChatID: strings.TrimSpace(chatID), Settled: settled})
	if err != nil {
		return err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(digestRaw))
	actor, err := r.exactActorFence(tabID, chatID)
	if err != nil {
		return err
	}
	actor.mu.Lock()
	defer actor.mu.Unlock()
	state := actor.engine.ReadSnapshot()
	if receipt, exists := state.PresentationMutationReceipts[operationID]; exists {
		if receipt.Digest != digest {
			return errors.New("presentation operation id was reused for different content")
		}
		return nil
	}
	presentation := state.Presentation.Clone()
	if settled && presentation.Settled != "settled" {
		presentation.Settled = "settled"
		presentation.SettledAt = time.Now().UnixMilli()
	} else if !settled && presentation.Settled != "active" {
		presentation.Settled = "active"
		presentation.SettledAt = 0
	}
	// Filing a chat away also acknowledges how it ended, as in the sidebar.
	if settled {
		presentation.Unread = false
	}
	if err := actor.engine.Apply(chat.SavePresentation{OperationID: operationID, Digest: digest, Presentation: presentation}); err != nil {
		return err
	}
	receipt, ok := actor.engine.ReadSnapshot().PresentationMutationReceipts[operationID]
	if !ok || receipt.Digest != digest {
		return errors.New("chat settle lost its durable actor receipt")
	}
	return nil
}

func (r *providerChatRuntime) FocusChat(tabID, chatID string, operationID providercontract.OperationID) error {
	if _, err := r.exactActorFence(tabID, chatID); err != nil {
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
	state = actor.engine.ReadSnapshot()
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
	committed := actor.engine.ReadSnapshot()
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
		previous := mapFromAnyMain(providerMemory[canonicalModelControlKey(controls.BaseModel)])
		if tier := fieldString(previous, "serviceTier"); tier == "fast" || tier == "default" {
			entry["serviceTier"] = tier
		}
		if controls.Effort != "" {
			entry["effort"] = controls.Effort
		}
		if controls.ModeID != "" {
			entry["modeId"] = controls.ModeID
		}
		providerMemory[canonicalModelControlKey(controls.BaseModel)] = entry
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
	state := actor.engine.ReadSnapshot()
	if strings.TrimSpace(state.Presentation.TabID) != strings.TrimSpace(tabID) {
		actor.mu.Unlock()
		return errors.New("tab id does not own the requested chat")
	}
	if err := actor.engine.Apply(chat.DeleteChat{OperationID: operationID, Force: force}); err != nil {
		actor.mu.Unlock()
		return err
	}
	if r.scheduled != nil {
		r.scheduled.CancelChat(strings.TrimSpace(tabID), strings.TrimSpace(chatID), "chat deleted")
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
	actor, err := r.exactActorFence(tabID, chatID)
	if err != nil {
		return err
	}
	actor.mu.Lock()
	defer actor.mu.Unlock()
	state := actor.engine.ReadSnapshot()
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

// agentSendQueueIdentity is the bounded durable identity for one stateless
// chat.send request. The caller operation id remains visible in the normal
// actor operation table; the queue identity carries a secret-safe digest of
// every admission input that is not otherwise represented by that table.
//
// The digest is deliberately one-way. In particular, a receipt must never
// echo or persist the message body merely to make a lost reply retryable.
func agentSendQueueIdentity(operationID providercontract.OperationID, tabID, chatID, message, delivery string) string {
	parts := []string{
		"workass-agent-chat-send-v2",
		strings.TrimSpace(string(operationID)), strings.TrimSpace(tabID), strings.TrimSpace(chatID),
		strings.TrimSpace(message), strings.TrimSpace(delivery),
	}
	var input strings.Builder
	for _, part := range parts {
		// Length-prefixing keeps the digest canonical even if a future caller
		// supplies a delimiter-looking identity component.
		fmt.Fprintf(&input, "%d:", len(part))
		input.WriteString(part)
	}
	digest := sha256.Sum256([]byte(input.String()))
	// Keep the caller operation id recoverable for the steer-derived actor
	// operation. OperationID is already bounded and secret-shaped values are
	// rejected at the control boundary.
	return fmt.Sprintf("q:%s:%x", strings.TrimSpace(string(operationID)), digest[:])
}

func normalizeAgentSendDelivery(delivery string) (string, error) {
	delivery = strings.ToLower(strings.TrimSpace(delivery))
	if delivery == "" {
		delivery = "auto"
	}
	switch delivery {
	case "auto", "queue", "steer":
		return delivery, nil
	default:
		return "", errors.New("delivery must be auto, queue, or steer")
	}
}

func agentSendSteerOperationPrefix(operationID providercontract.OperationID) string {
	return "q:" + strings.TrimSpace(string(operationID)) + ":"
}

// agentSendExistingOperation finds either the ordinary caller operation or a
// steer-derived operation created by QueueAgentMessage. The latter is
// intentionally discoverable by the caller operation prefix because the
// existing SteerQueued contract uses its queue id as the actor operation id.
func agentSendExistingOperation(state chat.State, operationID providercontract.OperationID) (providercontract.OperationID, bool, error) {
	operationID = providercontract.NormalizeOperationID(string(operationID))
	var found providercontract.OperationID
	if _, exists := state.Operations[operationID]; exists {
		found = operationID
	}
	prefix := agentSendSteerOperationPrefix(operationID)
	for candidate := range state.Operations {
		if !strings.HasPrefix(string(candidate), prefix) {
			continue
		}
		if found != "" && found != candidate {
			return "", true, errors.New("chat.send operation has multiple durable actor owners")
		}
		found = candidate
	}
	if found == "" {
		return "", false, nil
	}
	return found, true, nil
}

// validateAgentSendRetry compares the immutable actor input before returning
// a prior receipt. The actor's ledger/outbox keeps the semantic message, but
// the generic receipt exposes only queue-safe identity and status.
func validateAgentSendRetry(
	state chat.State,
	actorOperationID providercontract.OperationID,
	callerOperationID providercontract.OperationID,
	tabID, chatID, message, delivery, queueID string,
) error {
	if strings.TrimSpace(state.ChatID) != strings.TrimSpace(chatID) ||
		strings.TrimSpace(state.Presentation.TabID) != strings.TrimSpace(tabID) {
		return errors.New("chat.send operation belongs to a different exact tab/chat pair")
	}
	input, found := durableSteerInputForOperation(state, actorOperationID)
	if !found {
		return errors.New("chat.send operation id already belongs to a different actor command")
	}
	if strings.TrimSpace(input.Text) != strings.TrimSpace(message) ||
		strings.TrimSpace(input.Presentation.QueueID) != strings.TrimSpace(queueID) {
		return errors.New("chat.send operation id was reused for different content or delivery")
	}
	if actorOperationID == callerOperationID && delivery == "steer" {
		return errors.New("chat.send operation id was previously admitted with a different delivery")
	}
	if actorOperationID != callerOperationID && delivery != "steer" {
		return errors.New("chat.send operation id was previously admitted with a different delivery")
	}
	return nil
}

// agentSendReplayReceipt reads back a send's current place: still queued
// (with its position), running, or already delivered. A replay after delivery
// used to claim queued:true at position 0.
func agentSendReplayReceipt(state chat.State, actorOperationID, operationID providercontract.OperationID, queueID string) map[string]any {
	for _, entry := range state.Queue {
		if entry.OperationID == actorOperationID {
			return queuedActorReceipt(state, actorOperationID, queueID)
		}
	}
	for _, entry := range state.StagedQueue {
		if entry.ID == queueID {
			return queuedActorReceipt(state, operationID, queueID)
		}
	}
	receipt := queuedActorReceipt(state, operationID, queueID)
	receipt["queued"] = false
	delete(receipt, "position")
	if state.Foreground != nil && state.Foreground.OperationID == actorOperationID {
		receipt["status"] = "running"
	} else {
		receipt["status"] = "delivered"
	}
	return receipt
}

func (r *providerChatRuntime) QueueAgentMessage(ctx context.Context, tabID, chatID string, operationID providercontract.OperationID, message, delivery string) (map[string]any, error) {
	return r.QueueAgentMessageWithAttachments(ctx, tabID, chatID, operationID, message, delivery, nil)
}

// QueueAgentMessageWithAttachments carries validated local images inside the
// same user message. Their digests join the queue identity, so a retry with
// different images is rejected like a retry with different text.
func (r *providerChatRuntime) QueueAgentMessageWithAttachments(ctx context.Context, tabID, chatID string, operationID providercontract.OperationID, message, delivery string, attachments []acp.ImageAttachment) (map[string]any, error) {
	message = redactedSessionString(message)
	if strings.TrimSpace(message) == "" {
		return nil, errors.New("message is required")
	}
	validatedOperationID, err := providercontract.ValidateOperationID(string(operationID))
	if err != nil {
		return nil, errors.New("chat send requires a valid stable operation id")
	}
	operationID = validatedOperationID
	delivery, err = normalizeAgentSendDelivery(delivery)
	if err != nil {
		return nil, err
	}
	if len(attachments) > 0 && delivery == "steer" {
		return nil, errors.New("image attachments are not supported with steer; use auto or queue")
	}
	actor, err := r.exactActorFence(tabID, chatID)
	if err != nil {
		return nil, err
	}
	// QueueAgentMessage is a stateless retry boundary. The digest-bearing
	// identity is durable in the actor's semantic input, while the public
	// result remains bounded and contains no message text.
	identityText := message
	for _, attachment := range attachments {
		identityText += "\x00attachment:" + attachment.SHA256
	}
	queueID := agentSendQueueIdentity(operationID, tabID, chatID, identityText, delivery)
	var attachmentPlan providerAttachmentPlan
	if len(attachments) > 0 {
		images, err := acp.ImageAttachmentPayloads(attachments)
		if err != nil {
			return nil, err
		}
		for index, image := range images {
			// The file the agent attached is usually the image itself: let the
			// store clone it instead of writing its bytes again.
			if item, ok := image.(map[string]any); ok && index < len(attachments) {
				item[acp.ImageCloneSourceField] = attachments[index].Path
			}
		}
		if attachmentPlan, err = r.sessions.PlanProviderAttachments(images); err != nil {
			return nil, err
		}
	}
	actor.mu.Lock()
	state := actor.engine.ReadSnapshot()
	if state.ChatID != strings.TrimSpace(chatID) || state.Presentation.TabID != strings.TrimSpace(tabID) {
		actor.mu.Unlock()
		return nil, errors.New("chat.send requires the current exact tab/chat pair")
	}
	actorOperationID, exists, lookupErr := agentSendExistingOperation(state, operationID)
	if lookupErr != nil {
		actor.mu.Unlock()
		return nil, lookupErr
	}
	if exists {
		if err := validateAgentSendRetry(state, actorOperationID, operationID, tabID, chatID, message, delivery, queueID); err != nil {
			actor.mu.Unlock()
			return nil, err
		}
		if delivery == "steer" {
			result, replyErr := r.durableSteerReply(state, actorOperationID)
			if replyErr != nil {
				actor.mu.Unlock()
				return nil, replyErr
			}
			result["queueId"] = queueID
			result["operationId"] = string(operationID)
			result["delivery"] = "steer"
			actor.mu.Unlock()
			return result, nil
		}
		receipt := agentSendReplayReceipt(state, actorOperationID, operationID, queueID)
		actor.mu.Unlock()
		return receipt, nil
	}
	if delivery == "steer" {
		action, result, err := r.steerQueuedLocked(actor, tabID, chatID, queueID, message)
		actor.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if action == agentSteerWakeQueue {
			actor.coordinator.Wake()
		}
		if action == agentSteerExecuteLive {
			actorOperationID := providercontract.NormalizeOperationID(queueID)
			if _, err := actor.coordinator.ExecuteSteer(ctx, actorOperationID); err != nil {
				return nil, err
			}
			actor.mu.Lock()
			result, err = r.agentSteerQueuedResultLocked(actor.engine.ReadSnapshot(), actorOperationID)
			actor.mu.Unlock()
			if err != nil {
				return nil, err
			}
		}
		if result == nil {
			return nil, errors.New("chat actor did not own steering")
		}
		result["queueId"] = queueID
		result["operationId"] = string(operationID)
		result["delivery"] = firstNonEmptyString(fieldString(result, "strategy"), "steer")
		return result, nil
	}
	state = actor.engine.ReadSnapshot()
	if state.ChatID != strings.TrimSpace(chatID) || state.Presentation.TabID != strings.TrimSpace(tabID) {
		actor.mu.Unlock()
		return nil, errors.New("chat.send requires the current exact tab/chat pair")
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
		err = actor.engine.ApplyPrepared(chat.Submit{
			OperationID: operationID, LaneID: selection.Identity.ID, Text: message, Attachments: attachmentPlan.Attachments,
			ModelID: selection.ModelID, ModeID: selection.ModeID,
			Presentation: providercontract.TurnPresentation{
				UserMessageID: stableAgentMessageIdentity("u", operationID), AssistantMessageID: stableAgentMessageIdentity("a", operationID), QueueID: queueID,
				PromptText: message, Title: state.Presentation.Title, Origin: "agent", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
			},
		}, attachmentPlan.Materialize)
	}
	if err != nil {
		actor.mu.Unlock()
		return nil, err
	}
	receipt := queuedActorReceipt(actor.engine.ReadSnapshot(), operationID, queueID)
	actor.mu.Unlock()
	// Agent control is headless. The durable queue receipt returns immediately;
	// provider cold-start and turn execution continue from the actor outbox on
	// the coordinator's own effect worker. Its context is cancelled and awaited
	// by Coordinator.Close, so no turn can start a lane or write chat state
	// after the runtime has closed (an untracked Drain on a background context
	// kept writing into the state dir after shutdown).
	actor.coordinator.Wake()
	return receipt, nil
}

// steerQueuedLocked records a steer operation while the exact actor mutex is
// held. It deliberately does not call the coordinator: a provider effect may
// run concurrently and must never execute while the reducer lock is held.
// The action reports whether the caller should execute the provider steer or
// wake an ordinary FIFO turn after unlocking.
type agentSteerAction uint8

const (
	agentSteerNoAction agentSteerAction = iota
	agentSteerExecuteLive
	agentSteerWakeQueue
)

func (r *providerChatRuntime) steerQueuedLocked(actor *providerChatActor, tabID, chatID, queueID, message string) (agentSteerAction, map[string]any, error) {
	if actor == nil || actor.engine == nil {
		return agentSteerNoAction, nil, errors.New("chat actor is unavailable")
	}
	state := actor.engine.ReadSnapshot()
	if strings.TrimSpace(state.ChatID) != strings.TrimSpace(chatID) || strings.TrimSpace(state.Presentation.TabID) != strings.TrimSpace(tabID) {
		return agentSteerNoAction, nil, errors.New("steer tab attachment is stale")
	}
	operationID := providercontract.NormalizeOperationID(queueID)
	if operationID == "" {
		return agentSteerNoAction, nil, errors.New("steer requires a stable operation id")
	}
	if _, exists := state.Operations[operationID]; exists {
		result, err := r.agentSteerQueuedResultLocked(state, operationID)
		return agentSteerNoAction, result, err
	}
	if state.Foreground == nil || state.Foreground.Status != chat.ForegroundRunning {
		laneID := state.DesiredLaneID
		if laneID == "" {
			return agentSteerNoAction, nil, errors.New("agent queue requires a selected provider lane")
		}
		presentation := providercontract.TurnPresentation{
			UserMessageID:      stableAgentMessageIdentity("u", operationID),
			AssistantMessageID: stableAgentMessageIdentity("a", operationID),
			QueueID:            queueID, PromptText: message, Title: state.Presentation.Title,
			Origin: "agent", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
		if err := actor.engine.Apply(chat.Submit{
			OperationID: operationID, LaneID: laneID, Text: message,
			ModelID: state.Presentation.CurrentModelID, ModeID: state.Presentation.CurrentModeID,
			Presentation: presentation,
		}); err != nil {
			return agentSteerNoAction, nil, err
		}
		result := queuedActorReceipt(actor.engine.ReadSnapshot(), operationID, queueID)
		result["ok"] = false
		result["queued"] = true
		result["strategy"] = "queue"
		result["daemonQueued"] = true
		return agentSteerWakeQueue, result, nil
	}
	if err := actor.engine.Apply(chat.Steer{
		OperationID: operationID, Text: message,
		Presentation: providercontract.TurnPresentation{
			UserMessageID:      stableAgentMessageIdentity("u", operationID),
			AssistantMessageID: stableAgentMessageIdentity("a", operationID),
			QueueID:            queueID, PromptText: message, Origin: "agent", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		},
	}); err != nil {
		return agentSteerNoAction, nil, err
	}
	return agentSteerExecuteLive, nil, nil
}

func (r *providerChatRuntime) agentSteerQueuedResultLocked(state chat.State, operationID providercontract.OperationID) (map[string]any, error) {
	operationID = providercontract.NormalizeOperationID(string(operationID))
	for _, entry := range state.Queue {
		if entry.OperationID == operationID {
			return map[string]any{"ok": false, "queued": true, "strategy": "queue", "daemonQueued": true}, nil
		}
	}
	for _, entry := range state.Outbox {
		if entry.Kind != chat.EffectSteerTurn || entry.OperationID != operationID {
			continue
		}
		switch entry.Status {
		case chat.OutboxConsumed:
			return map[string]any{"ok": true, "live": true, "queued": false, "strategy": "generic-live"}, nil
		case chat.OutboxAccepted:
			result := map[string]any{"ok": true, "live": true, "queued": false, "strategy": "generic-live"}
			if pending := state.PendingSteerFor(operationID); pending != nil && pending.AwaitConsumption {
				result["strategy"] = "receipt-live"
				result["receipt"] = true
			}
			return result, nil
		case chat.OutboxAmbiguous:
			return map[string]any{"ok": false, "queued": false, "strategy": "uncertain"}, nil
		case chat.OutboxCompleted:
			if entry.LastError == providercontract.ErrorUnsupportedCapability {
				return map[string]any{"ok": false, "queued": true, "strategy": "queue"}, nil
			}
		case chat.OutboxFailed:
			return map[string]any{"ok": false, "queued": true, "strategy": "queue"}, nil
		}
	}
	return nil, &providercontract.Error{Kind: providercontract.ErrorProtocolViolation, Operation: operationID, Message: "agent steer has no durable disposition"}
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

const maxChatIdentifierLength = 128

func validateChatIdentifier(id string) error {
	if len(id) > maxChatIdentifierLength {
		return fmt.Errorf("chat identifier is longer than %d characters", maxChatIdentifierLength)
	}
	if _, err := providercontract.ValidateOperationID(id); err != nil {
		return fmt.Errorf("invalid chat identifier: %w", err)
	}
	// Narrower than operation ids: no path separators or parent references.
	if strings.ContainsAny(id, "/+=@") || strings.Contains(id, "..") {
		return errors.New("invalid chat identifier: path-shaped characters are not allowed")
	}
	return nil
}
