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
	history := []any{
		map[string]any{"role": "assistant", "content": "Respuesta anterior en español.", "at": "2026-07-10T00:00:00Z"},
	}

	first := startAppChatJobWithHistory(t, manager, session.SessionID, "language-every-turn", "Continue this work in English.", history)
	firstResult := jobFromEnd(events.waitJobEnd(t, jobID(first), 2*time.Second))["result"].(string)
	assertPerTurnLanguageBoundary(t, firstResult, "Continue this work in English.")

	// The environment/history brief is seeded only once. The language boundary
	// must still be repeated on an already-seeded or provider-resumed session.
	second := startAppChatJobWithHistory(t, manager, session.SessionID, "language-every-turn", "Keep answering in English.", nil)
	secondResult := jobFromEnd(events.waitJobEnd(t, jobID(second), 2*time.Second))["result"].(string)
	assertPerTurnLanguageBoundary(t, secondResult, "Keep answering in English.")
}

func TestWorkassOwnedHistoryScaffoldingIsLanguageNeutral(t *testing.T) {
	block := buildHistoryBlock([]historyMessage{
		{Role: "user", Content: "continue in English"},
		{Role: "assistant", Content: "previous answer"},
	}, 24000)

	for _, want := range []string{"Previous conversation", "User: continue in English", "Assistant: previous answer"} {
		if !strings.Contains(block, want) {
			t.Fatalf("neutral restored-history marker %q missing from %q", want, block)
		}
	}
	for _, forbidden := range []string{"Conversacion previa", "Usuario:", "Devin:", "transcripcion", "historial truncado"} {
		if strings.Contains(block, forbidden) {
			t.Fatalf("Workass-owned restored-history scaffolding still contains %q: %q", forbidden, block)
		}
	}
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

func TestWorkassOwnedCompactionScaffoldingCannotSetReplyLanguage(t *testing.T) {
	seed := buildCompactedSeedBlock(compactedSeed{
		Summary:  "Internal summary.",
		Messages: []historyMessage{{Role: "user", Content: "latest request"}},
	})
	combined := compactionPrompt() + "\n" + seed

	for _, forbidden := range []string{
		"Respondé en español", "Resumí la conversación", "Contexto resembrado",
		"Usuario:", "No respondas", "Instruccion de sistema",
	} {
		if strings.Contains(combined, forbidden) {
			t.Fatalf("Workass-owned compaction scaffolding still contains %q: %q", forbidden, combined)
		}
	}
	if !strings.Contains(combined, "must not determine the language of a later reply") {
		t.Fatalf("compaction prompt does not neutralize its own language: %q", combined)
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
