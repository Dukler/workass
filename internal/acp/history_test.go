package acp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	providercontract "workass/internal/provider"
)

func TestEnvironmentBriefIncludesChatArchivePath(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	manager, events := newFakeManager(t, "echo-prompt", Options{StateDir: stateDir, RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "brief-tab")

	job := startAppChatJob(t, manager, session.SessionID, "brief-tab", "read referenced chat")
	end := events.waitJobEnd(t, jobID(job), 2*time.Second)
	result := jobFromEnd(end)["result"].(string)
	archivePath := filepath.Join(stateDir, "chat-archive", "<chatId>.jsonl")
	if !strings.Contains(result, archivePath) ||
		!strings.Contains(result, "{role, content, status, at}") ||
		!strings.Contains(result, "to read another conversation the user references, read that file") ||
		!strings.Contains(result, "reply in the language of the current human-authored user request") ||
		!strings.Contains(result, expectedPerTurnLanguageRule) ||
		!strings.Contains(result, "User request:\nread referenced chat") {
		t.Fatalf("environment brief missing archive discovery paragraph:\n%s", result)
	}
	t.Logf("trace env brief archive path=%s", archivePath)
}

func TestEnvironmentBriefCurrentRequestLanguageUsesHumanRequest(t *testing.T) {
	manager, events := newFakeManager(t, "echo-prompt", Options{})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "language-tab")

	job := startAppChatJob(t, manager, session.SessionID, "language-tab", "Continue this work in English.")
	end := events.waitJobEnd(t, jobID(job), 2*time.Second)
	result := jobFromEnd(end)["result"].(string)
	rule := expectedPerTurnLanguageRule
	if !strings.Contains(result, rule) ||
		!strings.Contains(result, "restored transcripts") ||
		!strings.Contains(result, "User request:\nContinue this work in English.") {
		t.Fatalf("current-request language boundary is missing:\n%s", result)
	}
	if strings.Index(result, rule) > strings.Index(result, "User request:\nContinue this work in English.") {
		t.Fatalf("per-turn language rule must govern the current request:\n%s", result)
	}

	spanishSession := newFakeSession(t, manager, "language-tab-spanish")
	spanishJob := startAppChatJob(t, manager, spanishSession.SessionID, "language-tab-spanish", "Continuá este trabajo en español.")
	spanishEnd := events.waitJobEnd(t, jobID(spanishJob), 2*time.Second)
	spanishResult := jobFromEnd(spanishEnd)["result"].(string)
	if !strings.Contains(spanishResult, rule) ||
		!strings.Contains(spanishResult, "User request:\nContinuá este trabajo en español.") {
		t.Fatalf("current Spanish request lost its language boundary:\n%s", spanishResult)
	}
}

func TestFirstInputInitialContextSeedIsIncludedOnce(t *testing.T) {
	manager, events := newFakeManager(t, "echo-prompt", Options{})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "initial-seed-tab")

	first, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, ChatID: "chat-initial-seed-tab", TabID: "initial-seed-tab",
		ProviderID: session.ProviderID, Prompt: "current request", HumanAuthored: true,
		InitialContextSeed: []providercontract.ContextMessage{
			{LedgerSequence: 1, Role: "user", Text: "earlier question", Inert: true},
			{LedgerSequence: 2, Role: "assistant", Result: "earlier answer", Inert: true},
		},
	})
	if err != nil {
		t.Fatalf("start seeded first turn: %v", err)
	}
	firstEnd := events.waitJobEnd(t, jobID(first), 2*time.Second)
	firstResult := jobFromEnd(firstEnd)["result"].(string)
	if !strings.Contains(firstResult, "one-time restored context seed") ||
		!strings.Contains(firstResult, "User: earlier question") ||
		!strings.Contains(firstResult, "Assistant: earlier answer") ||
		!strings.Contains(firstResult, "User request:\ncurrent request") {
		t.Fatalf("initial context seed was not separated from the current request:\n%s", firstResult)
	}

	second, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, ChatID: "chat-initial-seed-tab", TabID: "initial-seed-tab",
		ProviderID: session.ProviderID, Prompt: "second request", HumanAuthored: true,
	})
	if err != nil {
		t.Fatalf("start ordinary second turn: %v", err)
	}
	secondEnd := events.waitJobEnd(t, jobID(second), 2*time.Second)
	secondResult := jobFromEnd(secondEnd)["result"].(string)
	if strings.Contains(secondResult, "earlier question") || strings.Contains(secondResult, "one-time restored context seed") {
		t.Fatalf("initial context seed replayed on a later turn:\n%s", secondResult)
	}
}

func TestEnvironmentBriefIncludesActiveModelOnEveryTurn(t *testing.T) {
	manager, events := newFakeManager(t, "echo-prompt", Options{})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "model-identity-tab")

	start := func(modelID, prompt string) string {
		job, err := manager.StartJob(context.Background(), JobStartOptions{
			Kind:       "app-chat",
			SessionID:  session.SessionID,
			ChatID:     "chat-model-identity-tab",
			TabID:      "model-identity-tab",
			ProviderID: session.ProviderID,
			ModelID:    modelID,
			Prompt:     prompt,
		})
		if err != nil {
			t.Fatalf("start %s turn: %v", modelID, err)
		}
		end := events.waitJobEnd(t, jobID(job), 2*time.Second)
		return jobFromEnd(end)["result"].(string)
	}

	first := start("model-alpha", "what model are you?")
	if !strings.Contains(first, `provider "`+session.ProviderID+`"`) ||
		!strings.Contains(first, `model "model-alpha"`) ||
		!strings.Contains(first, "answer with this exact Workass runtime identity") ||
		!strings.Contains(first, "User request:\nwhat model are you?") {
		t.Fatalf("first turn missing active runtime identity:\n%s", first)
	}

	second := start("model-beta", "and now?")
	if !strings.Contains(second, `model "model-beta"`) ||
		strings.Contains(second, `model "model-alpha"`) ||
		!strings.Contains(second, "User request:\nand now?") {
		t.Fatalf("second turn kept stale or missing model identity:\n%s", second)
	}
}

func TestCompletedAppChatJobsArePruned(t *testing.T) {
	manager, events := newFakeManager(t, "echo-prompt", Options{})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "prune-tab")

	job, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind:      "app-chat",
		SessionID: session.SessionID,
		ChatID:    "chat-prune-tab",
		TabID:     "prune-tab",
		Prompt:    "prune completed job",
	})
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 2*time.Second), "done", 0, "end_turn")

	manager.mu.Lock()
	_, ok := manager.jobs[jobID(job)]
	manager.mu.Unlock()
	if ok {
		t.Fatalf("completed job %s was retained", jobID(job))
	}
}
