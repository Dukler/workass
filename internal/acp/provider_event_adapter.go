package acp

import (
	"errors"
	"fmt"
	"strings"

	providercontract "workass/internal/provider"
)

func (m *Manager) registerProviderLane(lane *managerLane) {
	if m == nil || lane == nil || strings.TrimSpace(lane.info.SessionID) == "" {
		return
	}
	m.providerLaneMu.Lock()
	m.providerLanesBySession[lane.info.SessionID] = lane
	m.providerLaneMu.Unlock()
}

func (m *Manager) unregisterProviderLane(lane *managerLane) {
	if m == nil || lane == nil {
		return
	}
	m.providerLaneMu.Lock()
	if m.providerLanesBySession[lane.info.SessionID] == lane {
		delete(m.providerLanesBySession, lane.info.SessionID)
	}
	for jobID, owner := range m.providerLanesByJob {
		if owner == lane {
			delete(m.providerLanesByJob, jobID)
			// Host loss is already a terminal actor event. Fence the abandoned
			// attachment immediately so the same operation can never be rebound
			// to a replacement process or resumed behind the user's back.
			m.providerLaneClosedJobs[jobID] = struct{}{}
		}
	}
	m.providerLaneMu.Unlock()
}

func (m *Manager) bindProviderLaneJob(lane *managerLane, jobID string, operationID providercontract.OperationID) {
	if m == nil || lane == nil || strings.TrimSpace(jobID) == "" || operationID == "" {
		return
	}
	if m.providerLaneClosedJob(jobID) {
		return
	}
	lane.mu.Lock()
	lane.jobs[jobID] = operationID
	lane.mu.Unlock()
	m.providerLaneMu.Lock()
	m.providerLanesByJob[jobID] = lane
	m.providerLaneManagedJobs[jobID] = struct{}{}
	m.providerLaneMu.Unlock()
}

func (m *Manager) unbindProviderLaneJob(lane *managerLane, jobID string) {
	if m == nil || lane == nil || strings.TrimSpace(jobID) == "" {
		return
	}
	lane.mu.Lock()
	delete(lane.jobs, jobID)
	lane.mu.Unlock()
	m.providerLaneMu.Lock()
	if m.providerLanesByJob[jobID] == lane {
		delete(m.providerLanesByJob, jobID)
	}
	m.providerLaneMu.Unlock()
}

func (m *Manager) providerLaneManagedJob(jobID string) bool {
	if m == nil {
		return false
	}
	m.providerLaneMu.RLock()
	_, ok := m.providerLaneManagedJobs[strings.TrimSpace(jobID)]
	m.providerLaneMu.RUnlock()
	return ok
}

// closeProviderLaneJob leaves a non-owning tombstone after the active job maps
// are removed. Provider callbacks can outlive the manager Job and bridge maps;
// a closed tombstone keeps their semantic identity fail-closed at the adapter
// boundary instead of letting them fall through to raw frozen-wire publication.
func (m *Manager) closeProviderLaneJob(lane *managerLane, jobID string) {
	if m == nil || lane == nil {
		return
	}
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return
	}
	m.providerLaneMu.Lock()
	m.providerLaneClosedJobs[jobID] = struct{}{}
	m.providerLaneMu.Unlock()
}

func (m *Manager) providerLaneClosedJob(jobID string) bool {
	if m == nil {
		return false
	}
	m.providerLaneMu.RLock()
	_, closed := m.providerLaneClosedJobs[strings.TrimSpace(jobID)]
	m.providerLaneMu.RUnlock()
	return closed
}

func (m *Manager) rejectClosedProviderLaneJob(jobID, eventFamily string) error {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" || !m.providerLaneClosedJob(jobID) {
		return nil
	}
	return fmt.Errorf("late actor-owned %s event for closed provider job %q", eventFamily, jobID)
}

func (m *Manager) forgetProviderLaneManagedJob(jobID string) {
	if m == nil {
		return
	}
	m.providerLaneMu.Lock()
	delete(m.providerLaneManagedJobs, strings.TrimSpace(jobID))
	m.providerLaneMu.Unlock()
}

func (m *Manager) providerLaneForSession(sessionID string) *managerLane {
	m.providerLaneMu.RLock()
	lane := m.providerLanesBySession[strings.TrimSpace(sessionID)]
	m.providerLaneMu.RUnlock()
	return lane
}

func (m *Manager) providerLaneForJob(jobID string) *managerLane {
	m.providerLaneMu.RLock()
	lane := m.providerLanesByJob[strings.TrimSpace(jobID)]
	m.providerLaneMu.RUnlock()
	return lane
}

func (m *Manager) providerLaneForChat(tabID, chatID, providerID string) *managerLane {
	_, chatID, providerID = strings.TrimSpace(tabID), strings.TrimSpace(chatID), normalizeProviderID(providerID)
	m.providerLaneMu.RLock()
	defer m.providerLaneMu.RUnlock()
	for _, lane := range m.providerLanesBySession {
		if lane == nil || lane.identity.ChatID != chatID || normalizeProviderID(lane.info.ProviderID) != providerID {
			continue
		}
		return lane
	}
	return nil
}

// ReattachProviderLane updates disposable UI/process routing for an already
// attached immutable lane. It is legal only at an idle boundary and never
// changes ChatID, LaneID, ThreadRef, provider realm, or delivery generation.
func (m *Manager) ReattachProviderLane(selection ProviderLaneSelection) error {
	if m == nil {
		return errors.New("provider manager is unavailable")
	}
	identity := selection.Identity.Normalize()
	thread := selection.Thread.Normalize()
	owner := selection.Owner
	owner.TabID = strings.TrimSpace(owner.TabID)
	if err := identity.Validate(); err != nil {
		return err
	}
	if err := thread.Validate(identity.Realm.ProviderID); err != nil {
		return err
	}
	if owner.TabID == "" {
		return errors.New("provider lane attachment requires a tab id")
	}
	m.providerLaneMu.RLock()
	lane := m.providerLanesBySession[thread.HeadID]
	m.providerLaneMu.RUnlock()
	if lane == nil {
		return &providercontract.Error{Kind: providercontract.ErrorTransientTransport, Message: "provider lane is not attached"}
	}
	lane.mu.Lock()
	if lane.identity != identity || !lane.thread.Equal(thread) {
		lane.mu.Unlock()
		return &providercontract.Error{Kind: providercontract.ErrorNativeIdentityConflict, Message: "provider attachment does not match the immutable lane"}
	}
	if len(lane.jobs) != 0 {
		lane.mu.Unlock()
		return ErrChatBusy
	}
	currentOwner := lane.owner
	if owner.AgentOwnerKey == "" {
		owner.AgentOwnerKey = currentOwner.AgentOwnerKey
	}
	lane.mu.Unlock()

	if m.nativeSessions == nil || !m.nativeSessions.updateAttachment(owner.TabID, identity.ChatID, string(identity.Realm.ProviderID), thread.HeadID) {
		return &providercontract.Error{Kind: providercontract.ErrorProtocolViolation, Message: "could not persist provider lane attachment"}
	}

	m.mu.Lock()
	bridge := m.sessionBridge[thread.HeadID]
	if bridge == nil {
		m.mu.Unlock()
		return &providercontract.Error{Kind: providercontract.ErrorTransientTransport, Message: "provider host attachment disappeared"}
	}
	bridge.mu.Lock()
	if bridge.chatID != identity.ChatID {
		bridge.mu.Unlock()
		m.mu.Unlock()
		return &providercontract.Error{Kind: providercontract.ErrorNativeIdentityConflict, Message: "provider host is attached to another chat"}
	}
	bridge.tabID = owner.TabID
	if bridge.agentOwnerKey != "" {
		owner.AgentOwnerKey = bridge.agentOwnerKey
		m.agentOwners[bridge.agentOwnerKey] = agentOwnerBinding{ChatID: identity.ChatID, TabID: owner.TabID}
	}
	bridge.mu.Unlock()
	m.bindChatProviderLocked(SessionOptions{TabID: owner.TabID, ChatID: identity.ChatID}, string(identity.Realm.ProviderID))
	m.mu.Unlock()

	lane.mu.Lock()
	lane.owner = owner
	lane.mu.Unlock()
	return nil
}

func (l *managerLane) operationForJob(jobID string) providercontract.OperationID {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.jobs[strings.TrimSpace(jobID)]
}

func (l *managerLane) currentOperation() (providercontract.OperationID, string) {
	if l == nil {
		return "", ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.jobs) != 1 {
		return "", ""
	}
	for jobID, operationID := range l.jobs {
		return operationID, jobID
	}
	return "", ""
}

// observeProviderLaneEvent is the boundary adapter from the frozen
// renderer/LAN event payloads to the typed provider event union. Raw maps stop
// here; the chat reducer never sees them.
func (m *Manager) observeProviderLaneEvent(channel string, raw any) error {
	payload := mapFromAny(raw)
	switch channel {
	case "job:event":
		return m.observeProviderLaneJobEvent(payload)
	case "chat:permission-request", "chat:permission-resolved":
		return m.observeProviderLanePermission(channel, payload)
	case "spawned-work:changed":
		// Background snapshots cross the dedicated actor observer before Manager
		// publishes this frozen event. Re-adapting them here would create a second
		// owner and would lose origin once the foreground job unbinds.
		return nil
	case "chat:commands":
		return m.verifyProviderLaneCommandCatalog(payload)
	case "agent:apply", "app:update", "chat:catalog", "chat:context-limit", "chat:env", "chat:plan-usage",
		"proc:changed", "providers:list", "providers:update-progress", "providers:updates":
		// Explicitly non-semantic/global or derived notifications. They never
		// mutate the actor transcript, lane identity, queue, or operation journal.
		return nil
	case "chat:checkpoint-restored", "chat:engine-recovered", "chat:session-replaced":
		// These projection-only channels are illegal as semantic ingress for an
		// actor-owned lane. Standalone manager fixtures still exercise the frozen
		// publication surface without constructing the daemon actor runtime.
		if m.payloadBelongsToProviderLane(payload) {
			return fmt.Errorf("actor-owned channel %q bypassed typed provider ingress", channel)
		}
		return nil
	default:
		return fmt.Errorf("unclassified frozen event channel %q", channel)
	}
}

func (m *Manager) payloadBelongsToProviderLane(payload map[string]any) bool {
	if m == nil {
		return false
	}
	if jobID := firstNonEmpty(asString(payload["jobId"]), asString(payload["id"])); jobID != "" {
		if m.providerLaneManagedJob(jobID) || m.providerLaneClosedJob(jobID) {
			return true
		}
	}
	if job := mapFromAny(payload["job"]); len(job) > 0 {
		if jobID := asString(job["id"]); jobID != "" {
			if m.providerLaneManagedJob(jobID) || m.providerLaneClosedJob(jobID) {
				return true
			}
		}
		if m.providerLaneForSession(asString(job["sessionId"])) != nil {
			return true
		}
	}
	if m.providerLaneForSession(asString(payload["sessionId"])) != nil {
		return true
	}
	chatID := strings.TrimSpace(asString(payload["chatId"]))
	if chatID == "" {
		return false
	}
	m.providerLaneMu.RLock()
	defer m.providerLaneMu.RUnlock()
	for _, lane := range m.providerLanesBySession {
		if lane != nil && lane.identity.ChatID == chatID {
			return true
		}
	}
	return false
}

func (m *Manager) providerLaneForFrozenPayload(payload map[string]any) *managerLane {
	if m == nil {
		return nil
	}
	if jobID := firstNonEmpty(asString(payload["jobId"]), asString(payload["id"])); jobID != "" {
		if lane := m.providerLaneForJob(jobID); lane != nil {
			return lane
		}
		// A closed job is deliberately not returned as a live lane here. The
		// observe error still suppresses raw publication, but it must not try to
		// emit a second protocol-failure event through an attachment whose job
		// owner has already terminally settled.
		if m.providerLaneClosedJob(jobID) {
			return nil
		}
	}
	if job := mapFromAny(payload["job"]); len(job) > 0 {
		jobID := strings.TrimSpace(asString(job["id"]))
		if lane := m.providerLaneForJob(jobID); lane != nil {
			return lane
		}
		if jobID != "" && m.providerLaneClosedJob(jobID) {
			return nil
		}
		if lane := m.providerLaneForSession(asString(job["sessionId"])); lane != nil {
			return lane
		}
	}
	if lane := m.providerLaneForSession(asString(payload["sessionId"])); lane != nil {
		return lane
	}
	chatID := strings.TrimSpace(asString(payload["chatId"]))
	if chatID == "" {
		return nil
	}
	m.providerLaneMu.RLock()
	defer m.providerLaneMu.RUnlock()
	for _, lane := range m.providerLanesBySession {
		if lane != nil && lane.identity.ChatID == chatID {
			return lane
		}
	}
	return nil
}

func (m *Manager) observeProviderLaneJobEvent(payload map[string]any) error {
	switch strings.TrimSpace(asString(payload["type"])) {
	case "start":
		job := mapFromAny(payload["job"])
		jobID := strings.TrimSpace(asString(job["id"]))
		if err := m.rejectClosedProviderLaneJob(jobID, "start"); err != nil {
			return err
		}
		lane := m.providerLaneForSession(asString(job["sessionId"]))
		if lane == nil || lane.owner.TabID != strings.TrimSpace(asString(job["tabId"])) || lane.identity.ChatID != strings.TrimSpace(asString(job["chatId"])) {
			if m.providerLaneManagedJob(jobID) {
				return errors.New("actor-owned start event lost its provider lane")
			}
			return nil
		}
		// Actor-owned admissions bind the immutable operation before the public
		// start event. Preserve it; UserMessageID is only the visible row id.
		if lane.operationForJob(jobID) == "" {
			operationID := providercontract.NormalizeOperationID(asString(job["userMessageId"]))
			m.bindProviderLaneJob(lane, jobID, operationID)
		}
		return nil
	case "data":
		jobID := strings.TrimSpace(asString(payload["id"]))
		if err := m.rejectClosedProviderLaneJob(jobID, "data"); err != nil {
			return err
		}
		lane := m.providerLaneForJob(jobID)
		if lane == nil {
			if m.providerLaneManagedJob(jobID) {
				return errors.New("actor-owned data event lost its provider lane")
			}
			return nil
		}
		stream := strings.TrimSpace(asString(payload["stream"]))
		if stream != "stdout" {
			if stream == "stderr" || stream == "system" {
				return nil
			}
			return fmt.Errorf("actor-owned data event has unknown stream %q", stream)
		}
		operationID := lane.operationForJob(jobID)
		rawPhase := strings.TrimSpace(asString(payload["phase"]))
		phase := providercontract.AssistantPhaseContent
		switch rawPhase {
		case "commentary":
			phase = providercontract.AssistantPhaseCommentary
		case "final_answer":
			phase = providercontract.AssistantPhaseFinal
		}
		return lane.emit(providercontract.Event{
			Kind:      providercontract.EventAssistantChunk,
			Identity:  providercontract.EventIdentity{OperationID: operationID, TurnID: jobID},
			Assistant: &providercontract.AssistantEvent{Phase: phase, Text: asString(payload["chunk"]), TypedPhase: rawPhase != ""},
		})
	case "assistant-media":
		jobID := strings.TrimSpace(asString(payload["id"]))
		if err := m.rejectClosedProviderLaneJob(jobID, "assistant-media"); err != nil {
			return err
		}
		lane := m.providerLaneForJob(jobID)
		if lane == nil {
			if m.providerLaneManagedJob(jobID) {
				return errors.New("actor-owned media event lost its provider lane")
			}
			return nil
		}
		attachments, err := m.persistProviderEventAttachments(providerEventSlice(payload["images"]))
		if err != nil {
			return err
		}
		if len(attachments) == 0 {
			return nil
		}
		return lane.emit(providercontract.Event{
			Kind: providercontract.EventAssistantMedia,
			Identity: providercontract.EventIdentity{
				OperationID: lane.operationForJob(jobID), TurnID: jobID,
			},
			Media: &providercontract.AssistantMediaEvent{Attachments: attachments},
		})
	case "acp":
		return m.observeProviderLaneACPEvent(payload)
	case "usage":
		lane := m.providerLaneForSession(asString(payload["sessionId"]))
		if lane == nil {
			return nil
		}
		operationID, jobID := lane.currentOperation()
		return lane.emit(providercontract.Event{
			Kind:     providercontract.EventUsageUpdated,
			Identity: providercontract.EventIdentity{OperationID: operationID, TurnID: jobID},
			Usage: &providercontract.UsageEvent{
				Used: numberOrZero(payload["used"]), Size: numberOrZero(payload["size"]),
				InputTokens: numberOrZero(payload["inputTokens"]), OutputTokens: numberOrZero(payload["outputTokens"]),
			},
		})
	case "end":
		job := mapFromAny(payload["job"])
		jobID := strings.TrimSpace(asString(job["id"]))
		if m.providerLaneClosedJob(jobID) && boolFromAny(job["crashInterrupted"]) {
			// LaneDetached already terminalized the durable actor. This manager
			// receipt only closes the frozen UI job card; feeding it back into the
			// actor would create a second turn owner.
			m.forgetProviderLaneManagedJob(jobID)
			return nil
		}
		if err := m.rejectClosedProviderLaneJob(jobID, "terminal"); err != nil {
			return err
		}
		lane := m.providerLaneForJob(jobID)
		if lane == nil {
			lane = m.providerLaneForSession(asString(job["sessionId"]))
		}
		if lane == nil {
			if m.providerLaneManagedJob(jobID) {
				return errors.New("actor-owned terminal event lost its provider lane")
			}
			return nil
		}
		m.closeProviderLaneJob(lane, jobID)
		defer func() {
			m.unbindProviderLaneJob(lane, jobID)
			m.forgetProviderLaneManagedJob(jobID)
		}()
		operationID := lane.operationForJob(jobID)
		if operationID == "" {
			operationID = providercontract.NormalizeOperationID(asString(job["userMessageId"]))
		}
		attachments, persistErr := m.persistProviderEventAttachments(providerEventSlice(job["images"]))
		if persistErr != nil {
			return persistErr
		}
		consumedSteerIDs := make([]providercontract.OperationID, 0)
		for _, rawID := range providerEventSlice(job["consumedSteerIds"]) {
			if id := providercontract.NormalizeOperationID(asString(rawID)); id != "" {
				consumedSteerIDs = append(consumedSteerIDs, id)
			}
		}
		status := "completed"
		code := intPointerFromAny(job["code"])
		if code != nil && *code == 130 || strings.TrimSpace(asString(job["stopReason"])) == "cancelled" {
			status = "cancelled"
		} else if strings.TrimSpace(asString(job["status"])) != "done" {
			status = "failed"
		}
		disposition := mapFromAny(job["disposition"])
		err := lane.emit(providercontract.Event{
			Kind:     providercontract.EventTurnTerminal,
			Identity: providercontract.EventIdentity{OperationID: operationID, TurnID: jobID},
			Terminal: &providercontract.TerminalEvent{
				Status: status, StopReason: asString(job["stopReason"]), Result: asString(job["result"]),
				Error: asString(job["error"]), FinishedAt: asString(job["finishedAt"]), Code: code,
				Interrupted: boolFromAny(job["interrupted"]), CrashInterrupted: boolFromAny(job["crashInterrupted"]),
				DispositionState: asString(disposition["state"]), DispositionSource: asString(disposition["source"]),
				DispositionNote:  asString(disposition["note"]),
				ConsumedSteerIDs: consumedSteerIDs, Attachments: attachments,
			},
		})
		return err
	case "turn-heartbeat":
		// Explicitly transient liveness. It is never persisted or reconstructed.
		return nil
	}
	jobID := strings.TrimSpace(firstNonEmpty(asString(payload["id"]), asString(mapFromAny(payload["job"])["id"])))
	if err := m.rejectClosedProviderLaneJob(jobID, "unknown"); err != nil {
		return err
	}
	if m.providerLaneManagedJob(jobID) {
		return fmt.Errorf("actor-owned job event has unknown type %q", asString(payload["type"]))
	}
	return nil
}

func (m *Manager) observeProviderLaneACPEvent(payload map[string]any) error {
	jobID := strings.TrimSpace(asString(payload["id"]))
	if err := m.rejectClosedProviderLaneJob(jobID, "ACP"); err != nil {
		return err
	}
	lane := m.providerLaneForJob(jobID)
	if lane == nil {
		if m.providerLaneManagedJob(jobID) {
			return errors.New("actor-owned ACP event lost its provider lane")
		}
		return nil
	}
	event := mapFromAny(payload["event"])
	operationID := lane.operationForJob(jobID)
	switch strings.TrimSpace(asString(event["kind"])) {
	case "turn-heartbeat":
		// Frozen ACP hosts carry this liveness pulse inside an `acp` envelope.
		// It is intentionally transient and names no semantic state; the actor's
		// durable obligation/background receipts remain the restart authority.
		return nil
	case "thinking":
		return lane.emit(providercontract.Event{
			Kind: providercontract.EventThinkingUpdate,
			Identity: providercontract.EventIdentity{
				OperationID: operationID, TurnID: jobID,
			},
			Thinking: &providercontract.ThinkingEvent{Text: asString(event["text"])},
		})
	case "input-consumed", "steer-consumed":
		consumedID := providercontract.NormalizeOperationID(asString(event["clientUserMessageId"]))
		if consumedID == "" {
			return errors.New("actor-owned input consumption event is missing operation id")
		}
		var thread *providercontract.ThreadRef
		if strings.TrimSpace(asString(event["kind"])) == "input-consumed" {
			thread = lane.threadCreationReceipt(consumedID)
		}
		err := lane.emit(providercontract.Event{
			Kind:     providercontract.EventInputConsumed,
			Identity: providercontract.EventIdentity{OperationID: consumedID, TurnID: jobID},
			Input:    &providercontract.InputEvent{OperationID: consumedID, Thread: thread},
		})
		if err == nil && thread != nil {
			lane.clearThreadCreationReceipt(consumedID)
		}
		return err
	case "tool":
		attachments, err := m.persistProviderEventAttachments(providerEventSlice(event["images"]))
		if err != nil {
			return err
		}
		return lane.emit(providercontract.Event{
			Kind:     providercontract.EventToolUpdate,
			Identity: providercontract.EventIdentity{OperationID: operationID, TurnID: jobID},
			Tool: &providercontract.ToolEvent{
				ToolCallID: firstNonEmpty(asString(event["id"]), asString(event["terminalId"])),
				ToolKind:   asString(event["toolKind"]), Title: asString(event["title"]), Status: asString(event["status"]),
				Command: asString(event["command"]), TerminalID: asString(event["terminalId"]), Input: asString(event["input"]),
				Output: asString(event["output"]), Location: asString(event["location"]), Attachments: attachments,
				SubagentID: asString(event["subagentId"]), SubagentLabel: asString(event["subagentLabel"]),
				SubagentProvider: asString(event["subagentProvider"]), SubagentModel: asString(event["subagentModel"]),
				SubagentHeader: boolFromAny(event["subagentHeader"]),
			},
		})
	case "plan":
		entries := make([]providercontract.PlanEntry, 0)
		for index, rawEntry := range providerEventSlice(event["entries"]) {
			entry := mapFromAny(rawEntry)
			entries = append(entries, providercontract.PlanEntry{
				ID: fmt.Sprintf("%s:%d", jobID, index), Text: asString(entry["content"]), Status: asString(entry["status"]),
			})
		}
		return lane.emit(providercontract.Event{
			Kind:     providercontract.EventPlanUpdate,
			Identity: providercontract.EventIdentity{OperationID: operationID, TurnID: jobID},
			Plan:     &providercontract.PlanEvent{Entries: entries},
		})
	case "compaction":
		return emitProviderLaneCompaction(lane, operationID, jobID, event)
	}
	return fmt.Errorf("actor-owned ACP event has unknown kind %q", asString(event["kind"]))
}

func (m *Manager) observeProviderLaneCompaction(sessionID string, event map[string]any) {
	lane := m.providerLaneForSession(sessionID)
	if lane == nil {
		return
	}
	operationID, jobID := lane.currentOperation()
	_ = emitProviderLaneCompaction(lane, operationID, jobID, event)
}

func emitProviderLaneCompaction(lane *managerLane, operationID providercontract.OperationID, jobID string, event map[string]any) error {
	if lane == nil {
		return nil
	}
	return lane.emit(providercontract.Event{
		Kind:     firstCompactionKind(asString(event["phase"])),
		Identity: providercontract.EventIdentity{OperationID: operationID, TurnID: jobID},
		Compaction: &providercontract.CompactionEvent{
			CheckpointID: asString(event["checkpointId"]), Coverage: uint64(numberOrZero(event["coverage"])), Digest: asString(event["digest"]),
		},
	})
}

func firstCompactionKind(phase string) providercontract.EventKind {
	if strings.TrimSpace(phase) == "started" {
		return providercontract.EventCompactionStarted
	}
	return providercontract.EventCompactionCheckpoint
}

func (m *Manager) observeProviderLanePermission(channel string, payload map[string]any) error {
	jobID := strings.TrimSpace(asString(payload["jobId"]))
	lane := m.providerLaneForJob(jobID)
	if lane == nil {
		if m.providerLaneManagedJob(jobID) {
			return errors.New("actor-owned permission event lost its provider lane")
		}
		return nil
	}
	operationID := lane.operationForJob(jobID)
	options := make([]string, 0)
	details := make([]providercontract.PermissionOption, 0)
	for _, rawOption := range providerEventSlice(payload["options"]) {
		option := mapFromAny(rawOption)
		if id := strings.TrimSpace(firstNonEmpty(asString(option["optionId"]), asString(option["id"]))); id != "" {
			options = append(options, id)
			details = append(details, providercontract.PermissionOption{
				ID: id, Name: asString(option["name"]), Kind: asString(option["kind"]),
			})
		}
	}
	var question *providercontract.PermissionQuestion
	if rawQuestion := mapFromAny(payload["question"]); len(rawQuestion) > 0 {
		parsed := providercontract.PermissionQuestion{
			Question: asString(rawQuestion["question"]), Header: asString(rawQuestion["header"]),
			MultiSelect: boolFromAny(rawQuestion["multiSelect"]),
		}
		for _, rawOption := range providerEventSlice(rawQuestion["options"]) {
			option := mapFromAny(rawOption)
			parsed.Options = append(parsed.Options, providercontract.PermissionQuestionOption{
				Label: asString(option["label"]), Description: asString(option["description"]),
			})
		}
		question = &parsed
	}
	status := "pending"
	if channel == "chat:permission-resolved" {
		status = "resolved"
	}
	return lane.emit(providercontract.Event{
		Kind:     map[bool]providercontract.EventKind{true: providercontract.EventPermissionResolved, false: providercontract.EventPermissionRequested}[status == "resolved"],
		Identity: providercontract.EventIdentity{OperationID: operationID, TurnID: jobID},
		Permission: &providercontract.PermissionEvent{
			RequestID: asString(payload["id"]), Title: asString(payload["title"]), Kind: asString(payload["kind"]),
			Status: status, Options: options, OptionDetails: details, Question: question,
			ResolvedOptionID: asString(payload["optionId"]),
		},
	})
}

func providerEventSlice(value any) []any {
	switch items := value.(type) {
	case []any:
		return items
	default:
		return nil
	}
}

func boolFromAny(value any) bool {
	result, _ := value.(bool)
	return result
}

func intPointerFromAny(value any) *int {
	if value == nil {
		return nil
	}
	result := numberOrZero(value)
	return &result
}
