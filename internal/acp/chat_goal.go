package acp

import (
	"strings"
	"time"
)

// A provider's standing goal for a chat (Codex /goal): the objective it keeps
// working toward across turns, its status, and how much it has used. The host
// reports every change, idle or not, as a private session update; the daemon
// keeps the latest per chat in memory and announces it on the additive
// chat:goal event (chat:goal-get answers late clients). Runtime presentation
// only: it never enters the actor transcript, and the provider stays the
// authority (a restarted daemon learns it again when the session resumes).
type providerGoalUpdate struct {
	Objective       string  `json:"objective"`
	Status          string  `json:"status"`
	TokensUsed      int64   `json:"tokensUsed"`
	TokenBudget     *int64  `json:"tokenBudget"`
	TimeUsedSeconds float64 `json:"timeUsedSeconds"`
}

var providerGoalStatuses = map[string]struct{}{
	"active": {}, "paused": {}, "blocked": {}, "usageLimited": {}, "budgetLimited": {}, "complete": {},
}

type chatGoalEntry struct {
	sessionID  string
	providerID string
	goal       *providerGoalUpdate
	observedAt int64
}

// decodeProviderGoal accepts a goal object or null. Anything else (unknown
// status, missing objective) is rejected rather than shown half-understood.
func decodeProviderGoal(raw any) (*providerGoalUpdate, bool) {
	if raw == nil {
		return nil, true
	}
	fields := mapFromAny(raw)
	if len(fields) == 0 {
		return nil, false
	}
	status := strings.TrimSpace(asString(fields["status"]))
	objective, ok := fields["objective"].(string)
	if _, known := providerGoalStatuses[status]; !known || !ok {
		return nil, false
	}
	goal := &providerGoalUpdate{
		Objective: redactSensitiveText(strings.TrimSpace(objective)),
		Status:    status,
	}
	if value, ok := goalNumber(fields["tokensUsed"]); ok && value >= 0 {
		goal.TokensUsed = int64(value)
	}
	if value, ok := goalNumber(fields["tokenBudget"]); ok && value >= 0 {
		budget := int64(value)
		goal.TokenBudget = &budget
	}
	if value, ok := goalNumber(fields["timeUsedSeconds"]); ok && value >= 0 {
		goal.TimeUsedSeconds = value
	}
	return goal, true
}

func goalNumber(raw any) (float64, bool) {
	switch value := raw.(type) {
	case float64:
		return value, true
	case int:
		return float64(value), true
	case int64:
		return float64(value), true
	}
	return 0, false
}

func (m *Manager) storeChatGoal(tabID, chatID, sessionID, providerID string, goal *providerGoalUpdate) {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if tabID == "" && chatID == "" {
		return
	}
	entry := &chatGoalEntry{sessionID: sessionID, providerID: providerID, goal: goal, observedAt: time.Now().UnixMilli()}
	m.mu.Lock()
	m.chatGoals[commandCatalogKey(tabID, chatID)] = entry
	m.mu.Unlock()
	m.emit("chat:goal", chatGoalPayload(tabID, chatID, entry))
}

func chatGoalPayload(tabID, chatID string, entry *chatGoalEntry) map[string]any {
	payload := map[string]any{"tabId": nullableString(tabID), "chatId": nullableString(chatID), "goal": nil}
	if entry == nil {
		return payload
	}
	payload["providerId"] = entry.providerID
	payload["observedAt"] = entry.observedAt
	if entry.goal != nil {
		payload["goal"] = entry.goal
	}
	return payload
}

// ChatGoal answers the additive chat:goal-get invoke with the chat's latest
// known goal (goal null when none is set or none was ever reported).
func (m *Manager) ChatGoal(tabID, chatID string) map[string]any {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	m.mu.Lock()
	entry := m.chatGoals[commandCatalogKey(tabID, chatID)]
	m.mu.Unlock()
	return chatGoalPayload(tabID, chatID, entry)
}

// forgetChatGoalSessionLocked / forgetChatGoalChatLocked mirror the command
// catalog cache: a closed session or a deleted chat drops its goal. Caller
// holds m.mu.
func (m *Manager) forgetChatGoalSessionLocked(sessionID string) {
	for key, entry := range m.chatGoals {
		if entry != nil && entry.sessionID == sessionID {
			delete(m.chatGoals, key)
		}
	}
}

func (m *Manager) forgetChatGoalChatLocked(tabID string) {
	tabID = strings.TrimSpace(tabID)
	for key := range m.chatGoals {
		if entryTab, _, ok := strings.Cut(key, "\x00"); ok && entryTab == tabID {
			delete(m.chatGoals, key)
		}
	}
}
