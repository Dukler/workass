package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildHistoryBlockTruncatesSingleOversizedRecentMessage(t *testing.T) {
	history := []historyMessage{
		{Role: "assistant", Content: strings.Repeat("x", 80000), At: "2026-07-10T00:00:00Z"},
	}

	block := buildHistoryBlock(history, 24000)

	if len(block) > 26000 {
		t.Fatalf("history block length = %d, want bounded near budget", len(block))
	}
	if !strings.Contains(block, "history truncated") {
		t.Fatalf("history block did not mark truncation")
	}
}

func TestPromptHistoryForTabReadsBoundedArchiveWindow(t *testing.T) {
	stateDir := t.TempDir()
	archiveDir := filepath.Join(stateDir, "chat-archive")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatalf("mkdir archive: %v", err)
	}
	file, err := os.Create(filepath.Join(archiveDir, "large-tab.jsonl"))
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	enc := json.NewEncoder(file)
	for i := 0; i < 100; i++ {
		record := map[string]any{
			"role":    "assistant",
			"content": strings.Repeat("x", 5000) + " tail",
			"at":      fmt.Sprintf("2026-07-10T00:00:%02dZ", i),
		}
		if err := enc.Encode(record); err != nil {
			t.Fatalf("encode archive: %v", err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}

	history := promptHistoryForTab(stateDir, JobStartOptions{TabID: "large-tab", HistoryCharBudget: 24000})

	total := 0
	for _, msg := range history {
		total += len(msg.Content)
	}
	if total > archiveReadCharCap(24000)+6000 {
		t.Fatalf("archive history content = %d, want bounded", total)
	}
	if len(history) >= 100 {
		t.Fatalf("history messages = %d, want bounded tail window", len(history))
	}
}

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

func TestEnvironmentBriefCurrentRequestLanguageOverridesRestoredTranscript(t *testing.T) {
	manager, events := newFakeManager(t, "echo-prompt", Options{})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "language-tab")
	history := []any{
		map[string]any{"role": "assistant", "content": "Respuesta anterior en español.", "at": "2026-07-10T00:00:00Z"},
	}

	job := startAppChatJobWithHistory(t, manager, session.SessionID, "language-tab", "Continue this work in English.", history)
	end := events.waitJobEnd(t, jobID(job), 2*time.Second)
	result := jobFromEnd(end)["result"].(string)
	rule := expectedPerTurnLanguageRule
	if !strings.Contains(result, rule) ||
		!strings.Contains(result, "restored transcripts") ||
		!strings.Contains(result, "Respuesta anterior en español.") ||
		!strings.Contains(result, "User request:\nContinue this work in English.") {
		t.Fatalf("language precedence brief missing from restored-history prompt:\n%s", result)
	}
	if strings.Index(result, rule) < strings.Index(result, "Respuesta anterior en español.") ||
		strings.Index(result, rule) > strings.Index(result, "User request:\nContinue this work in English.") {
		t.Fatalf("per-turn language rule must follow restored transcript and immediately govern the current request:\n%s", result)
	}

	spanishSession := newFakeSession(t, manager, "language-tab-spanish")
	spanishHistory := []any{
		map[string]any{"role": "assistant", "content": "Previous assistant response in English.", "at": "2026-07-10T00:00:00Z"},
	}
	spanishJob := startAppChatJobWithHistory(t, manager, spanishSession.SessionID, "language-tab-spanish", "Continuá este trabajo en español.", spanishHistory)
	spanishEnd := events.waitJobEnd(t, jobID(spanishJob), 2*time.Second)
	spanishResult := jobFromEnd(spanishEnd)["result"].(string)
	if !strings.Contains(spanishResult, rule) ||
		!strings.Contains(spanishResult, "Previous assistant response in English.") ||
		!strings.Contains(spanishResult, "User request:\nContinuá este trabajo en español.") {
		t.Fatalf("current Spanish request did not retain dynamic language precedence:\n%s", spanishResult)
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
			ChatID:     "chat-model-identity",
			TabID:      "model-identity-tab",
			ProviderID: "mock",
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
	if !strings.Contains(first, `provider "mock"`) ||
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
		ChatID:    "chat-prune",
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
