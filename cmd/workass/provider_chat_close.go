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
// current actor attachment is the authority that decides whether that id may
// close anything. In particular, an old manager row must not close a newer
// exact-resume attachment merely because both rows refer to one native thread.
//
// Closing an attachment never deletes or changes the immutable ThreadRef. The
// actor records HostLost with the connection generation that was validated at
// admission; a late LaneDetached event from the manager is then harmlessly
// ignored as stale.
func (r *providerChatRuntime) CloseSession(ctx context.Context, sessionID string) bool {
	if r == nil || r.manager == nil {
		return false
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false
	}
	live, ok := r.manager.LiveSessionByID(sessionID)
	if !ok || strings.TrimSpace(live.ChatID) == "" {
		return false
	}
	actor, err := r.actor(live.ChatID)
	if err != nil || actor == nil {
		return false
	}

	actor.mu.Lock()
	defer actor.mu.Unlock()
	state := actor.engine.Snapshot()
	laneID, generation, ok := currentActorAttachmentForClose(state, live, sessionID)
	if !ok {
		return false
	}

	// Serialize the manager detach with actor-side selection/reattachment. The
	// manager call may synchronously emit LaneDetached; the generation check
	// below makes that event idempotent with the explicit HostLost commit.
	closed := r.manager.CloseSession(ctx, sessionID)
	current := actor.engine.Snapshot()
	if lane, exists := current.Lanes[laneID]; exists &&
		lane.ConnectionGeneration == generation &&
		lane.Attachment != nil &&
		strings.TrimSpace(lane.Attachment.ConnectionID) == sessionID {
		// HostLost is deliberately applied after the manager call. If the
		// adapter emitted its detached event first, the generation has already
		// advanced and this becomes a no-op; otherwise this closes the durable
		// attachment immediately instead of relying on an asynchronous event.
		_ = actor.engine.Apply(chat.HostLost{LaneID: laneID, ConnectionGeneration: generation})
	}
	return closed
}

func currentActorAttachmentForClose(state chat.State, live acp.LiveSession, sessionID string) (providercontract.LaneID, uint64, bool) {
	if strings.TrimSpace(live.Info.SessionID) != sessionID || strings.TrimSpace(live.ChatID) != state.ChatID {
		return "", 0, false
	}
	if state.Deleted || strings.TrimSpace(state.Presentation.TabID) == "" ||
		strings.TrimSpace(live.TabID) != strings.TrimSpace(state.Presentation.TabID) {
		return "", 0, false
	}
	providerID := providercontract.NormalizeID(live.Info.ProviderID)
	for laneID, lane := range state.Lanes {
		if lane.Attachment == nil || strings.TrimSpace(lane.Attachment.ConnectionID) != sessionID {
			continue
		}
		if providercontract.NormalizeID(string(lane.Identity.Realm.ProviderID)) != providerID ||
			strings.TrimSpace(lane.Owner.TabID) != strings.TrimSpace(live.TabID) ||
			lane.ConnectionGeneration == 0 {
			return "", 0, false
		}
		return laneID, lane.ConnectionGeneration, true
	}
	return "", 0, false
}
