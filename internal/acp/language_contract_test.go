package acp

import (
	"strings"
	"testing"
)

func TestWorkassSetsNoReplyLanguage(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })

	first := manager.buildAppChatPrompt(JobStartOptions{HumanAuthored: true}, "Continue this work in English.")
	second := buildUserRequestBlock("Keep answering in English.", true)
	for _, prompt := range []string{first, second} {
		if strings.Contains(strings.ToLower(prompt), "response language") {
			t.Fatalf("the prompt sets a reply language: %q", prompt)
		}
	}
	if !strings.HasSuffix(first, "User request:\nContinue this work in English.") ||
		!strings.HasSuffix(second, "User request:\nKeep answering in English.") {
		t.Fatalf("current request is not delimited: %q / %q", first, second)
	}
}

func TestLocalizedWorkassToolCardIsIsolatedFromHumanProse(t *testing.T) {
	t.Parallel()
	request := "Usar una herramienta\n" +
		"mcp.workass-agent.workass_agent_catalog\n" +
		"0.0s\n" +
		"0.0s\n" +
		"Workass agent coordinator is unavailable\n\n" +
		"???? what is this why"
	prompt := buildUserRequestBlock(request, true)
	cardAt := strings.Index(prompt, "<workass_tool_card>\nUsar una herramienta")
	humanAt := strings.Index(prompt, "User request:\n???? what is this why")
	if cardAt < 0 || humanAt < 0 || cardAt > humanAt {
		t.Fatalf("localized tool card was not isolated from human prose: %q", prompt)
	}
	if strings.Count(prompt, "Usar una herramienta") != 1 ||
		strings.Count(prompt, "Workass agent coordinator is unavailable") != 1 {
		t.Fatalf("tool-card evidence was lost or duplicated: %q", prompt)
	}
}

func TestOrdinaryMCPMentionIsNotMisclassifiedAsToolCard(t *testing.T) {
	t.Parallel()
	request := "Please inspect mcp.workass-agent.workass_agent_catalog and answer in English."
	prompt := buildUserRequestBlock(request, true)
	if strings.Contains(prompt, "<workass_tool_card>") || !strings.Contains(prompt, "User request:\n"+request) {
		t.Fatalf("ordinary request was rewritten as a tool card: %q", prompt)
	}
}
