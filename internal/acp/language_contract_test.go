package acp

import (
	"strings"
	"testing"
	"time"
)

const expectedPerTurnLanguageRule = "Response language for this turn: use the language of the current human-authored user request below."

func TestWorkassLanguageRuleIsAdjacentToEveryTurn(t *testing.T) {
	manager, events := newFakeManager(t, "echo-prompt", Options{})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "language-every-turn")

	first := startAppChatJob(t, manager, session.SessionID, "language-every-turn", "Continue this work in English.")
	firstResult := jobFromEnd(events.waitJobEnd(t, jobID(first), 2*time.Second))["result"].(string)
	assertPerTurnLanguageBoundary(t, firstResult, "Continue this work in English.")

	// The language boundary must repeat on an already-used provider session.
	second := startAppChatJob(t, manager, session.SessionID, "language-every-turn", "Keep answering in English.")
	secondResult := jobFromEnd(events.waitJobEnd(t, jobID(second), 2*time.Second))["result"].(string)
	assertPerTurnLanguageBoundary(t, secondResult, "Keep answering in English.")
}

func TestLocalizedWorkassToolCardCannotSelectHumanReplyLanguage(t *testing.T) {
	request := "Usar una herramienta\n" +
		"mcp.workass-agent.workass_agent_catalog\n" +
		"0.0s\n" +
		"0.0s\n" +
		"Workass agent coordinator is unavailable\n\n" +
		"???? what is this why"
	prompt := buildUserRequestBlock(request, true)
	ruleAt := strings.Index(prompt, expectedPerTurnLanguageRule)
	cardAt := strings.Index(prompt, "<workass_tool_card>\nUsar una herramienta")
	humanAt := strings.Index(prompt, "User request:\n???? what is this why")
	if ruleAt < 0 || cardAt < 0 || humanAt < 0 || ruleAt > cardAt || cardAt > humanAt {
		t.Fatalf("localized tool card was not isolated from human prose: %q", prompt)
	}
	if strings.Count(prompt, "Usar una herramienta") != 1 ||
		strings.Count(prompt, "Workass agent coordinator is unavailable") != 1 {
		t.Fatalf("tool-card evidence was lost or duplicated: %q", prompt)
	}
	if !strings.Contains(prompt[ruleAt:humanAt], "not human-authored language selection") ||
		!strings.Contains(prompt[ruleAt:humanAt], "use the language of that prose") {
		t.Fatalf("language boundary does not neutralize generated card text: %q", prompt[ruleAt:humanAt])
	}
}

func TestOrdinaryMCPMentionIsNotMisclassifiedAsToolCard(t *testing.T) {
	request := "Please inspect mcp.workass-agent.workass_agent_catalog and answer in English."
	prompt := buildUserRequestBlock(request, true)
	if strings.Contains(prompt, "<workass_tool_card>") || !strings.Contains(prompt, "User request:\n"+request) {
		t.Fatalf("ordinary request was rewritten as a tool card: %q", prompt)
	}
}

func assertPerTurnLanguageBoundary(t *testing.T, prompt, request string) {
	t.Helper()
	ruleAt := strings.LastIndex(prompt, expectedPerTurnLanguageRule)
	requestAt := strings.LastIndex(prompt, "User request:\n"+request)
	if ruleAt < 0 || requestAt < 0 || ruleAt > requestAt {
		t.Fatalf("per-turn language boundary is missing or misplaced: rule=%d request=%d prompt=%q", ruleAt, requestAt, prompt)
	}
	if strings.Contains(prompt[ruleAt:requestAt], "Previous conversation") {
		t.Fatalf("restored history appears between the language rule and current request: %q", prompt[ruleAt:])
	}
}
