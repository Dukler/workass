package acp

import (
	"encoding/json"
	"testing"
)

// A Codex goal reaches the renderer as structured state: the host's private
// update decodes to a typed goal, the manager keeps the latest per chat, and
// both the chat:goal event and chat:goal-get carry it. A cleared goal (null)
// and a frame with an unknown status are told apart.
func TestCodexGoalUpdateReachesChatGoalEventAndGet(t *testing.T) {
	codex := providerAdapterForID("codex").notifications
	frame := map[string]any{
		"sessionUpdate": "_workass_codex_goal",
		"goal": map[string]any{
			"threadId": "thread", "objective": "finish the Gold review", "status": "active",
			"tokensUsed": float64(1200), "tokenBudget": float64(50000), "timeUsedSeconds": float64(720),
		},
	}
	notification, ok := codex.Decode(frame, nil)
	if !ok || notification.Kind != providerNotificationGoal || !notification.GoalSet || notification.Goal == nil {
		t.Fatalf("goal frame = %#v, recognized=%v", notification, ok)
	}
	if goal := notification.Goal; goal.Objective != "finish the Gold review" || goal.Status != "active" || goal.TokensUsed != 1200 || goal.TokenBudget == nil || *goal.TokenBudget != 50000 || goal.TimeUsedSeconds != 720 {
		t.Fatalf("decoded goal = %#v", goal)
	}
	if _, ok := providerAdapterForID("claude").notifications.Decode(frame, nil); ok {
		t.Fatal("Claude adapter consumed a Codex goal frame")
	}
	if _, ok := codex.Decode(map[string]any{"sessionUpdate": "_workass_codex_goal", "goal": map[string]any{"objective": "x", "status": "dancing"}}, nil); ok {
		t.Fatal("a goal with an unknown status was accepted")
	}
	cleared, ok := codex.Decode(map[string]any{"sessionUpdate": "_workass_codex_goal", "goal": nil}, nil)
	if !ok || !cleared.GoalSet || cleared.Goal != nil {
		t.Fatalf("cleared goal frame = %#v, recognized=%v", cleared, ok)
	}

	var events []map[string]any
	manager := NewManager(Options{Broadcast: func(channel string, payload any) {
		if channel != "chat:goal" {
			return
		}
		raw, _ := json.Marshal(payload)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)
		events = append(events, decoded)
	}})
	manager.storeChatGoal("tab", "chat", "session", "codex", notification.Goal)
	if len(events) != 1 || events[0]["chatId"] != "chat" || events[0]["providerId"] != "codex" {
		t.Fatalf("chat:goal events = %#v", events)
	}
	goal, _ := events[0]["goal"].(map[string]any)
	if goal["objective"] != "finish the Gold review" || goal["status"] != "active" || goal["timeUsedSeconds"] != float64(720) {
		t.Fatalf("chat:goal goal = %#v", goal)
	}
	if got := manager.ChatGoal("tab", "chat"); got["goal"] == nil || got["providerId"] != "codex" {
		t.Fatalf("chat:goal-get = %#v", got)
	}
	manager.storeChatGoal("tab", "chat", "session", "codex", nil)
	if got := manager.ChatGoal("tab", "chat"); got["goal"] != nil {
		t.Fatalf("cleared chat:goal-get = %#v", got)
	}
	manager.mu.Lock()
	manager.forgetChatGoalChatLocked("tab")
	manager.mu.Unlock()
	if got := manager.ChatGoal("tab", "chat"); got["providerId"] != nil {
		t.Fatalf("deleted chat kept its goal: %#v", got)
	}
}

// The catalog names the provider a chat with no explicit provider runs on, so
// the composer can show its model instead of a bare "Modelo"; a missing or
// unready default falls back to the first ready provider, as the models do.
func TestCatalogPayloadNamesItsEffectiveDefaultProvider(t *testing.T) {
	ready := func(id string) CatalogGroup {
		return CatalogGroup{ProviderID: id, Status: providerStatusReady, Models: []Model{{ModelID: id + "-model"}}}
	}
	if got := catalogPayload([]CatalogGroup{ready("codex"), ready("claude")}, "claude")["defaultProviderId"]; got != "claude" {
		t.Fatalf("default provider = %v, want claude", got)
	}
	if got := catalogPayload([]CatalogGroup{ready("codex"), ready("claude")}, "gone")["defaultProviderId"]; got != "codex" {
		t.Fatalf("fallback provider = %v, want codex", got)
	}
	if _, ok := catalogPayload(nil, "claude")["defaultProviderId"]; ok {
		t.Fatal("an empty catalog named a default provider")
	}
}
