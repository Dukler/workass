package main

import (
	"strings"

	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

// A Workass notice — a job or a subagent that ended, a stall, a machine gone
// dark — is news for the agent that is working NOW. Queued as a fresh turn
// behind the running one, it waited for that whole turn and for every message
// the owner typed meanwhile, and arrived as a stale turn of its own long after
// it mattered (bug-20261116-7ab17943). Claude Code delivers its own
// background-task notifications the other way: folded into the running turn at
// its next tool boundary. So does this.
//
// While the chat's turn is running on a lane that takes live input, the notice
// is admitted as an agent steer of that turn. The steer is durable before any
// provider call and is executed by the coordinator's own drain, so a crash in
// between still delivers it. An agent steer that carries its QueueID is never
// lost: a definite refusal by the provider moves that same input into the FIFO
// once (reduceSteerFailed), which is exactly where it would have gone before.
// With no running turn, the notice starts one, as it always did.

// noticeJoinsRunningTurn reports whether a notice can be folded into the
// chat's running turn instead of waiting for it to end.
func noticeJoinsRunningTurn(state chat.State) bool {
	foreground := state.Foreground
	if foreground == nil || foreground.Status != chat.ForegroundRunning || foreground.Turn.NativeID == "" {
		return false
	}
	if state.PendingCancel != nil {
		// The turn is being stopped; whatever it is told now goes nowhere.
		return false
	}
	lane, ok := state.Lanes[foreground.LaneID]
	return ok && lane.Delivery.LiveSteer
}

// admitNoticeLocked admits one notice with the actor mutex held. The caller
// wakes the coordinator after unlocking, for either path.
func admitNoticeLocked(actor *providerChatActor, state chat.State, operationID providercontract.OperationID,
	laneID providercontract.LaneID, modelID, modeID, originOperationID, message string, presentation providercontract.TurnPresentation,
) error {
	presentation.QueueID = string(operationID)
	presentation.Origin = "agent"
	presentation.NoticeOrigin = strings.TrimSpace(originOperationID)
	if noticeJoinsRunningTurn(state) {
		err := actor.engine.Apply(chat.Steer{OperationID: operationID, Text: message, Presentation: presentation})
		if err == nil {
			return nil
		}
		// Refused before anything was recorded (the turn ended a moment ago, a
		// presentation id is taken): the notice is still owed, so it queues.
		if _, recorded := actor.engine.ReadSnapshot().Operations[operationID]; recorded {
			return err
		}
	}
	return actor.engine.Apply(chat.Submit{
		OperationID: operationID, LaneID: laneID, Text: message, ModelID: modelID, ModeID: modeID,
		Presentation: presentation,
	})
}
