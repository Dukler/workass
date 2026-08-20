package main

import (
	"context"
	"strings"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

// CloseSession is the actor-owned boundary for the frozen app-chat:close-session
// command. The wire command carries only a disposable session id, so the
// authoritative actor inventory must first resolve exactly one attachment or
// prior close journal. Only then may the manager be consulted as disposable
// executor evidence for that actor-owned target. Manager.CloseSession is
// reached only by the coordinator after the detach journal is durable.
//
// Closing an attachment never deletes or changes the immutable ThreadRef. The
// provider LaneDetached receipt (or the coordinator's HostLost compatibility
// receipt) clears only the disposable attachment and advances the generation;
// a later selection resumes the same native thread.
func (r *providerChatRuntime) CloseSession(ctx context.Context, sessionID string) bool {
	if r == nil || r.manager == nil {
		return false
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false
	}
	// Resolve the actor before reading any transient manager row. Multiple actor
	// matches make the frozen address ambiguous and therefore fail closed.
	actor, state, found := r.findCloseJournal(sessionID)
	if !found || actor == nil {
		return false
	}
	// A lost reply can arrive after the old provider attachment was removed. A
	// completed actor receipt is pure readback and must not touch a newer exact
	// resume that happens to reuse the same native session id.
	if durableDetachReceipt(state, sessionID) {
		return true
	}
	live, ok := r.manager.LiveSessionByID(sessionID)
	if !ok || strings.TrimSpace(live.ChatID) == "" || strings.TrimSpace(live.ChatID) != state.ChatID {
		// Pending/dispatched without exact executor evidence remains fail-closed;
		// guessing another manager session could duplicate a non-idempotent close.
		return false
	}
	return r.closeExactAttachment(ctx, actor, live, sessionID)
}

func (r *providerChatRuntime) closeExactAttachment(ctx context.Context, actor *providerChatActor, live acp.LiveSession, sessionID string) bool {
	if actor == nil {
		return false
	}
	actor.mu.Lock()
	state := actor.engine.Snapshot()
	laneID, generation, ok := currentActorAttachmentForClose(state, live, sessionID)
	if !ok {
		if durableDetachRetryForNewerAttachment(state, live, sessionID) {
			actor.mu.Unlock()
			return true
		}
		actor.mu.Unlock()
		return false
	}
	lane := state.Lanes[laneID]
	operationID := chat.DetachOperationID(state.ChatID, laneID, sessionID, generation)
	err := actor.engine.Apply(chat.DetachTarget{
		OperationID:          operationID,
		LaneID:               laneID,
		Owner:                lane.Owner,
		ConnectionID:         sessionID,
		ConnectionGeneration: generation,
	})
	actor.mu.Unlock()
	if err != nil {
		return false
	}

	// ExecuteDetach claims the exact operation, checks the coordinator's live
	// lane and generation again, and only then reaches provider Lane.Detach
	// (Manager.CloseSession for the ACP lane). It never drains another effect.
	_, executeErr := actor.coordinator.ExecuteDetach(ctx, operationID)
	state = actor.engine.Snapshot()
	if durableDetachReceiptForTarget(state, laneID, sessionID, generation) {
		return true
	}
	return executeErr == nil && durableDetachReceipt(state, sessionID)
}

// currentActorAttachmentForClose is deliberately pure. It is the target
// proof used before the durable command and is also useful to tests that need
// to demonstrate that stale native ids do not authorize a close.
func currentActorAttachmentForClose(state chat.State, live acp.LiveSession, sessionID string) (providercontract.LaneID, uint64, bool) {
	if strings.TrimSpace(live.Info.SessionID) != sessionID || strings.TrimSpace(live.ChatID) != state.ChatID {
		return "", 0, false
	}
	if state.Deleted || strings.TrimSpace(state.Presentation.TabID) == "" ||
		strings.TrimSpace(live.TabID) != strings.TrimSpace(state.Presentation.TabID) {
		return "", 0, false
	}
	providerID := providercontract.NormalizeID(live.Info.ProviderID)
	if providerID == "" {
		return "", 0, false
	}
	var found providercontract.LaneID
	var generation uint64
	for laneID, lane := range state.Lanes {
		if lane.Attachment == nil || strings.TrimSpace(lane.Attachment.ConnectionID) != sessionID {
			continue
		}
		if lane.Identity.ChatID != state.ChatID ||
			providercontract.NormalizeID(string(lane.Identity.Realm.ProviderID)) != providerID ||
			strings.TrimSpace(lane.Owner.TabID) != strings.TrimSpace(live.TabID) ||
			lane.ConnectionGeneration == 0 {
			return "", 0, false
		}
		if attachedProvider := providercontract.NormalizeID(string(lane.Attachment.ProviderID)); attachedProvider != "" && attachedProvider != providerID {
			return "", 0, false
		}
		if live.Info.CWD != "" && strings.TrimSpace(lane.Attachment.CWD) != "" && strings.TrimSpace(live.Info.CWD) != strings.TrimSpace(lane.Attachment.CWD) {
			return "", 0, false
		}
		if found != "" {
			return "", 0, false
		}
		found, generation = laneID, lane.ConnectionGeneration
	}
	if found == "" {
		return "", 0, false
	}
	// The frozen request cannot distinguish a new close from a lost retry when
	// exact resume reuses the same native session id. Any older journal for this
	// id therefore fences the newer generation instead of guessing.
	for _, entry := range state.Outbox {
		if entry.Kind == chat.EffectDetachLane && entry.LaneID == found &&
			strings.TrimSpace(entry.ConnectionID) == sessionID && entry.Generation != generation {
			return "", 0, false
		}
	}
	return found, generation, true
}

func durableDetachReceipt(state chat.State, sessionID string) bool {
	for _, entry := range state.Outbox {
		if entry.Kind == chat.EffectDetachLane && strings.TrimSpace(entry.ConnectionID) == strings.TrimSpace(sessionID) && entry.Status == chat.OutboxCompleted {
			return true
		}
	}
	return false
}

func durableDetachReceiptForTarget(state chat.State, laneID providercontract.LaneID, sessionID string, generation uint64) bool {
	for _, entry := range state.Outbox {
		if entry.Kind == chat.EffectDetachLane && entry.LaneID == laneID &&
			strings.TrimSpace(entry.ConnectionID) == strings.TrimSpace(sessionID) && entry.Generation == generation &&
			entry.Status == chat.OutboxCompleted {
			return true
		}
	}
	return false
}

func durableDetachRetryForNewerAttachment(state chat.State, live acp.LiveSession, sessionID string) bool {
	providerID := providercontract.NormalizeID(live.Info.ProviderID)
	for laneID, lane := range state.Lanes {
		if lane.Attachment == nil || strings.TrimSpace(lane.Attachment.ConnectionID) != strings.TrimSpace(sessionID) ||
			strings.TrimSpace(lane.Owner.TabID) != strings.TrimSpace(live.TabID) ||
			providercontract.NormalizeID(string(lane.Identity.Realm.ProviderID)) != providerID {
			continue
		}
		for _, entry := range state.Outbox {
			if entry.Kind == chat.EffectDetachLane && entry.LaneID == laneID &&
				strings.TrimSpace(entry.ConnectionID) == strings.TrimSpace(sessionID) &&
				entry.Status == chat.OutboxCompleted && entry.Generation < lane.ConnectionGeneration {
				return true
			}
		}
	}
	return false
}

func (r *providerChatRuntime) findCloseJournal(sessionID string) (*providerChatActor, chat.State, bool) {
	if r == nil {
		return nil, chat.State{}, false
	}
	r.mu.Lock()
	ids := make([]string, 0, len(r.known)+len(r.actors))
	seen := make(map[string]struct{}, len(r.known)+len(r.actors))
	actors := make(map[string]*providerChatActor, len(r.actors))
	for chatID, actor := range r.actors {
		actors[chatID] = actor
		seen[chatID] = struct{}{}
		ids = append(ids, chatID)
	}
	for chatID := range r.known {
		if _, exists := seen[chatID]; !exists {
			ids = append(ids, chatID)
		}
	}
	r.mu.Unlock()

	var match *providerChatActor
	var matchState chat.State
	for _, chatID := range ids {
		actor := actors[chatID]
		if actor == nil {
			var err error
			actor, err = r.actor(chatID)
			if err != nil || actor == nil {
				continue
			}
		}
		state := actor.engine.Snapshot()
		candidate := false
		for _, lane := range state.Lanes {
			if lane.Attachment != nil && strings.TrimSpace(lane.Attachment.ConnectionID) == strings.TrimSpace(sessionID) {
				candidate = true
				break
			}
		}
		candidate = candidate || durableDetachReceipt(state, sessionID)
		if !candidate {
			continue
		}
		if match != nil && matchState.ChatID != state.ChatID {
			// The session id is not enough to choose between multiple actor
			// records. Failing closed is safer than asking either manager row to
			// close.
			return nil, chat.State{}, false
		}
		match, matchState = actor, state
	}
	return match, matchState, match != nil
}
