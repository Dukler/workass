package main

import (
	"encoding/json"
	"errors"
	"strings"

	"workass/internal/wire"
)

func (r *providerChatRuntime) TurnDiagnostics(params map[string]any) (map[string]any, error) {
	if r == nil || r.manager == nil {
		return nil, errors.New("chat diagnostics runtime is unavailable")
	}
	tabID, chatID := strings.TrimSpace(fieldString(params, "tab_id")), strings.TrimSpace(fieldString(params, "chat_id"))
	if tabID == "" || chatID == "" {
		return nil, errors.New("chat diagnostics require exact tab_id and chat_id")
	}
	r.mu.Lock()
	actor := r.actors[chatID]
	r.mu.Unlock()
	if actor == nil {
		return nil, errors.New("chat diagnostics unavailable: exact chat is not resident in this daemon; no history was loaded")
	}
	// The body-free digest validates ownership without cloning long transcripts.
	state := actor.engine.DigestSnapshot()
	if state.Deleted || state.TabID != tabID {
		return nil, errors.New("diagnostics target is deleted or tab_id does not own chat_id")
	}
	limit := 5
	if raw, ok := params["limit"]; ok {
		var value float64
		switch n := raw.(type) {
		case json.Number:
			parsed, err := n.Int64()
			if err != nil {
				return nil, errors.New("diagnostics limit must be an integer from 1 to 20")
			}
			value = float64(parsed)
		case int:
			value = float64(n)
		case float64:
			value = n
		default:
			return nil, errors.New("diagnostics limit must be an integer from 1 to 20")
		}
		if value < 1 || value > 20 || value != float64(int(value)) {
			return nil, errors.New("diagnostics limit must be an integer from 1 to 20")
		}
		limit = int(value)
	}
	result := r.manager.TurnDiagnostics(tabID, chatID, limit)
	result["actor"] = map[string]any{
		"running": state.RunningJobID != "", "runningJobId": state.RunningJobID,
		"queuedMessages": state.QueueLen, "pendingPermissions": len(state.PendingPermissionIDs),
		"messageCount": state.MessageCount, "revision": state.ActorRevision,
	}
	return result, nil
}

func registerChatDiagnosticsWire(hub *wire.Hub, runtime *providerChatRuntime) {
	// Diagnostics must not sit behind an unrelated slow mutation on a LAN link.
	// This uses the unchanged invoke/reply protocol and normal peer auth.
	hub.RegisterOutOfBandRead("chat:turn-diagnostics", func(args []any) (any, error) {
		return runtime.TurnDiagnostics(firstMapArg(args))
	})
}
