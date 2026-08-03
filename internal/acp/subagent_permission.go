package acp

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// User ruling 2026-07-27: "the subagent permission should be delegated to parent
// agent, who should know if give them permissions or not [...] as a fallback
// parent should handle any permissions request by subagent."
//
// Why this is a tool the parent CALLS rather than an automatic grant against the
// parent's mode: a child that inherited a full-access parent never asks for
// permission at all, so an automatic grant could only ever fire for a child the
// spawner had deliberately narrowed — it would silently turn permission_intent
// into a field with no effect. The parent answering each request one at a time
// keeps the narrowing meaningful and is what the ruling actually asks for.
//
// The boundary: a parent may DENY anything, because denying never widens what
// can happen, and may ALLOW only what its own effective mode already permits
// without asking a human. A parent whose own mode still prompts holds no
// authority to hand down, so its child's card stays the user's to answer —
// which is the same rule as lan:access-decide, where a device cannot grant
// itself access it was never given.
const (
	subagentPermissionAllow = "allow"
	subagentPermissionDeny  = "deny"
)

// DecideSubagentPermission answers one pending permission request raised by a
// tracked subagent, on behalf of the parent agent that spawned it.
func (m *Manager) DecideSubagentPermission(ownerKey, parentChatID, parentTabID, subagentID, decision string) (map[string]any, error) {
	subagentID = strings.TrimSpace(subagentID)
	if subagentID == "" {
		return nil, errors.New("subagent_id is required")
	}
	decision = strings.ToLower(strings.TrimSpace(decision))
	if decision != subagentPermissionAllow && decision != subagentPermissionDeny {
		return nil, errors.New(`decision must be "allow" or "deny"`)
	}
	chatID, tabID, parent, ok := m.subagentOwnerContext(ownerKey, parentChatID, parentTabID)
	if !ok {
		return nil, errors.New("this chat does not own that subagent")
	}

	m.mu.Lock()
	run := m.subagents[subagentID]
	if !m.addressSubagentLocked(run, parent, chatID, tabID) {
		m.mu.Unlock()
		// A run lost to a daemon restart lands here: the registry is in-memory,
		// so the id is simply gone rather than refusing for a reason the caller
		// could act on. Say which, so the parent stops waiting on it.
		return nil, fmt.Errorf("subagent %q is not addressable from this chat; it may have ended or been lost to a daemon restart", subagentID)
	}
	sessionID := strings.TrimSpace(run.SessionID)
	m.mu.Unlock()

	permissionID, optionID, ok := m.pendingSubagentPermission(sessionID, decision)
	if !ok {
		return nil, fmt.Errorf("subagent %q has no permission request waiting", subagentID)
	}
	if decision == subagentPermissionAllow {
		if mode, allowed := m.parentMayGrantSubagentPermission(parent); !allowed {
			return nil, fmt.Errorf("this chat's own permission mode (%s) still asks before acting, so it cannot grant one on a subagent's behalf; the card in this chat is the user's to answer", firstNonEmpty(mode, "unset"))
		}
	}
	m.PermissionDecide(permissionID, optionID)
	return map[string]any{"ok": true, "subagentId": subagentID, "decision": decision}, nil
}

// pendingSubagentPermission finds the request this subagent's session is parked
// on and picks the option matching the decision. Option ids are provider-chosen,
// so the kind/name are what identify an allow from a reject — the same reading
// handlePermissionRequest already does to choose its own fallback.
func (m *Manager) pendingSubagentPermission(sessionID, decision string) (string, string, bool) {
	if strings.TrimSpace(sessionID) == "" {
		return "", "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, rec := range m.permissions {
		if rec == nil || rec.sessionID != sessionID {
			continue
		}
		options, _ := rec.payload["options"].([]any)
		if optionID, ok := permissionOptionForDecision(options, decision); ok {
			return id, optionID, true
		}
		return "", "", false
	}
	return "", "", false
}

// isExitPlanModeToolCall recognises the plan-mode handshake across the shapes a
// provider may send it under. Deliberately generous: a false positive can only
// ever produce "keep planning", which grants nothing, while a false negative
// restores the deadlock this exists to end.
func isExitPlanModeToolCall(toolCall map[string]any) bool {
	label := strings.ToLower(asString(toolCall["title"]) + " " + asString(toolCall["kind"]) + " " + asString(toolCall["toolName"]))
	label = strings.ReplaceAll(strings.ReplaceAll(label, "_", ""), " ", "")
	return strings.Contains(label, "exitplanmode")
}

func permissionOptionForDecision(options []any, decision string) (string, bool) {
	deny := decision == subagentPermissionDeny
	for _, raw := range options {
		opt := mapFromAny(raw)
		kind := asString(opt["kind"])
		label := strings.ToLower(kind + " " + asString(opt["name"]))
		// Exactly the reading handlePermissionRequest uses to pick its fallback.
		// Deliberately not matching a bare "no": "Allow, no confirmation" is an
		// allow, and reading it as a reject would deny what the parent granted.
		rejects := kind == "reject_once" || kind == "reject_always" ||
			strings.Contains(label, "reject") || strings.Contains(label, "deny")
		if rejects == deny {
			if optionID := asString(opt["optionId"]); optionID != "" {
				return optionID, true
			}
		}
	}
	return "", false
}

// parentMayGrantSubagentPermission reports whether the parent's own effective
// mode is the provider's full-access one. Anything narrower still asks a human
// for itself, so it has nothing to delegate.
func (m *Manager) parentMayGrantSubagentPermission(parent *Job) (string, bool) {
	if parent == nil {
		return "", false
	}
	modeID := strings.TrimSpace(m.effectiveSubagentParentMode(parent))
	if modeID == "" {
		return "", false
	}
	providerID := firstNonEmpty(strings.TrimSpace(parent.ProviderID), strings.TrimSpace(parent.startOpts.ProviderID))
	group := m.catalogGroup(context.Background(), providerID)
	full := permissionIntentModes(providerID, group.Modes)["full"]
	return modeID, full != "" && full == modeID
}
