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
	t.Parallel()
	stateDir := filepath.Join(t.TempDir(), "state")
	manager := NewManager(Options{StateDir: stateDir})
	t.Cleanup(func() { manager.Reset() })
	result := manager.buildAppChatPrompt(JobStartOptions{HumanAuthored: true}, "read referenced chat")
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
	t.Parallel()
	result := buildUserRequestBlock("Continue this work in English.", true)
	rule := expectedPerTurnLanguageRule
	if !strings.Contains(result, rule) ||
		!strings.Contains(result, "restored transcripts") ||
		!strings.Contains(result, "User request:\nContinue this work in English.") {
		t.Fatalf("current-request language boundary is missing:\n%s", result)
	}
	if strings.Index(result, rule) > strings.Index(result, "User request:\nContinue this work in English.") {
		t.Fatalf("per-turn language rule must govern the current request:\n%s", result)
	}

	spanishResult := buildUserRequestBlock("Continuá este trabajo en español.", true)
	if !strings.Contains(spanishResult, rule) ||
		!strings.Contains(spanishResult, "User request:\nContinuá este trabajo en español.") {
		t.Fatalf("current Spanish request lost its language boundary:\n%s", spanishResult)
	}
}

func TestFirstInputInitialContextSeedIsIncludedOnce(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	firstResult := manager.buildAppChatPrompt(JobStartOptions{
		HumanAuthored: true,
		InitialContextSeed: []providercontract.ContextMessage{
			{LedgerSequence: 1, Role: "user", Text: "earlier question", Inert: true},
			{LedgerSequence: 2, Role: "assistant", Result: "earlier answer", Inert: true},
		},
	}, "current request")
	if !strings.Contains(firstResult, "one-time restored context seed") ||
		!strings.Contains(firstResult, "User: earlier question") ||
		!strings.Contains(firstResult, "Assistant: earlier answer") ||
		!strings.Contains(firstResult, "User request:\ncurrent request") {
		t.Fatalf("initial context seed was not separated from the current request:\n%s", firstResult)
	}

	secondResult := buildUserRequestBlock("second request", true)
	if strings.Contains(secondResult, "earlier question") || strings.Contains(secondResult, "one-time restored context seed") {
		t.Fatalf("initial context seed replayed on a later turn:\n%s", secondResult)
	}
}

func TestEnvironmentBriefIncludesActiveModelOnEveryTurn(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	bridge := newBridge("model-identity", Options{Provider: ProviderConfig{ID: "mock", Name: "Mock Provider"}}, manager)

	start := func(modelID, prompt string) string {
		return buildTurnRuntimeIdentity(bridge, "mock", modelID) + buildUserRequestBlock(prompt, true)
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
	t.Parallel()
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

func TestContextDeltaPromptPreservesSessionAndCurrentRequestBoundary(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	prompt := manager.buildAppChatPrompt(JobStartOptions{HumanAuthored: true, ContextDelta: []providercontract.ContextMessage{
		{LedgerSequence: 113, Role: "user", Text: "missing question", Inert: true},
		{LedgerSequence: 116, Role: "assistant", Text: "missing answer", Inert: true},
	}}, "current request")
	if !strings.Contains(prompt, "same native session") || !strings.Contains(prompt, "User: missing question") ||
		!strings.Contains(prompt, "Assistant: missing answer") || !strings.Contains(prompt, "User request:\ncurrent request") ||
		strings.Contains(prompt, "newly created") || strings.Contains(prompt, "one-time") || strings.Contains(prompt, "history was omitted") {
		t.Fatal("delta prompt misrepresented its session or historical context")
	}
	next := manager.buildAppChatPrompt(JobStartOptions{HumanAuthored: true}, "next request")
	if strings.Contains(next, "missing question") || strings.Contains(next, "conversation_transcript") {
		t.Fatal("delta leaked to another turn")
	}
}

func TestContextDeltaTraversesRuntimeIntoExactResumedMockThread(t *testing.T) {
	t.Parallel()
	fixture := newPersistentMockFixture(t, "resume")
	first, events := fixture.newManager()
	session := fixture.newSession(t, first)
	fixture.runTurn(t, first, events, session.SessionID, "original-native-history")
	binding, ok := first.nativeSessions.get("native-tab", "native-chat", "mock")
	if !ok {
		t.Fatal("missing original binding")
	}
	first.Reset()
	manager, _ := fixture.newManager()
	t.Cleanup(func() { manager.Reset() })
	lane, err := (managerLaneFactory{manager: manager, providerID: "mock"}).Resume(context.Background(), providercontract.ResumeLaneRequest{
		Identity: bindingLaneIdentity(binding), Thread: bindingThreadRef(binding), Owner: providercontract.AttachmentOwner{TabID: "native-tab"}, CWD: binding.CWD,
	})
	if err != nil {
		t.Fatal(err)
	}
	managed := lane.(*managerLane)
	terminal := make(chan providercontract.Event, 2)
	go func() {
		for event := range lane.Events() {
			managed.AcknowledgeDurableEvent(event.Identity.Sequence, nil)
			if event.Kind == providercontract.EventTurnTerminal {
				terminal <- event
			}
		}
	}()
	for i, op := range []string{"delta-turn", "plain-turn"} {
		input := providercontract.TurnInput{OperationID: providercontract.OperationID(op), Text: op}
		if i == 0 {
			input.ContextDelta = []providercontract.ContextMessage{{EventID: "missing", LedgerSequence: 3, Role: "user", Text: "only-missing-history", Inert: true}}
		}
		if _, err := lane.Delivery().StartTurn(context.Background(), input); err != nil {
			t.Fatal(err)
		}
		select {
		case event := <-terminal:
			if event.Terminal == nil || event.Terminal.Status != "completed" {
				t.Fatalf("mock terminal: %#v", event.Terminal)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("resumed mock did not finish")
		}
	}
	if !lane.Thread().Equal(bindingThreadRef(binding)) || persistentMockSessionCount(t, fixture.sessionFile) != 1 {
		t.Fatal("delta replaced the native thread")
	}
	trace := strings.Join(readNativeMockTrace(t, fixture.traceFile), "\n")
	if !strings.Contains(trace, "session/resume") || strings.Count(trace, "only-missing-history") != 1 {
		t.Fatalf("missing delta was not delivered exactly once: %s", trace)
	}
	if err := lane.Detach(context.Background()); err != nil {
		t.Fatal(err)
	}
}
