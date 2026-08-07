package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProviderTypedMessagePhaseBoundarySurvivesStdoutCoalescing(t *testing.T) {
	events := newEventCollector()
	manager := NewManager(Options{Broadcast: events.Broadcast, StdoutFlushInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	bridge := newBridge("phase-test", Options{StdoutFlushInterval: time.Hour}, manager)
	job := &Job{ID: "phase-job"}

	commentary := map[string]any{"_meta": map[string]any{"workassAssistantPhase": "commentary"}}
	final := map[string]any{"_meta": map[string]any{"workassAssistantPhase": "final_answer"}}
	if got := agentMessagePhase(commentary); got != "commentary" {
		t.Fatalf("commentary phase = %q", got)
	}
	if got := agentMessagePhase(final); got != "final_answer" {
		t.Fatalf("final phase = %q", got)
	}
	if got := agentMessagePhase(map[string]any{"_meta": map[string]any{"workassAssistantPhase": "future"}}); got != "" {
		t.Fatalf("unknown phase = %q, want legacy empty phase", got)
	}
	if got := agentMessagePhase(map[string]any{"_meta": map[string]any{"codex": map[string]any{"phase": "final_answer"}}}); got != "final_answer" {
		t.Fatalf("Codex compatibility phase = %q", got)
	}
	if got := agentMessagePhase(map[string]any{"providerId": "codex", "result": "looks final"}); got != "" {
		t.Fatalf("untyped provider output synthesized phase = %q", got)
	}

	bridge.queueStdout(job, "working notes", agentMessagePhase(commentary))
	bridge.queueStdout(job, "final report", agentMessagePhase(final))
	bridge.flushJobBuffers(job)
	data := events.jobEvents(job.ID, "data")
	if len(data) != 2 {
		t.Fatalf("phase data events = %d, want 2: %#v", len(data), data)
	}
	if data[0]["chunk"] != "working notes" || data[0]["phase"] != "commentary" {
		t.Fatalf("commentary event = %#v", data[0])
	}
	if data[1]["chunk"] != "final report" || data[1]["phase"] != "final_answer" {
		t.Fatalf("final event = %#v", data[1])
	}
	if got := job.output.String(); got != "working notesfinal report" {
		t.Fatalf("combined provider history = %q", got)
	}
}

func TestMockProviderTypedPhasesAreExplicitAndPhaseLessTurnsStayPlain(t *testing.T) {
	root := repoRoot(t)
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir:             root,
		Provider:            ProviderConfig{Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Label: "Workass Mock ACP"},
		Broadcast:           events.Broadcast,
		StdoutFlushInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: "provider-phase-tab"})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	typed := startAppChatJob(t, manager, session.SessionID, "provider-phase-tab", "[mock:phases] provider-authored final")
	assertJobStatus(t, events.waitJobEnd(t, jobID(typed), 5*time.Second), "done", 0, "end_turn")
	typedData := events.jobEvents(jobID(typed), "data")
	if len(typedData) != 2 || typedData[0]["phase"] != "commentary" || typedData[1]["phase"] != "final_answer" {
		t.Fatalf("provider-typed data = %#v", typedData)
	}

	plain := startAppChatJob(t, manager, session.SessionID, "provider-phase-tab", "ordinary provider response")
	assertJobStatus(t, events.waitJobEnd(t, jobID(plain), 5*time.Second), "done", 0, "end_turn")
	plainData := events.jobEvents(jobID(plain), "data")
	if len(plainData) != 1 {
		t.Fatalf("phase-less data events = %#v", plainData)
	}
	if _, exists := plainData[0]["phase"]; exists {
		t.Fatalf("phase-less provider was assigned a result phase: %#v", plainData[0])
	}
}

func TestMockInitializeSessionPromptCancelErrorAndReuse(t *testing.T) {
	root := repoRoot(t)
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir:              root,
		Provider:             ProviderConfig{Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Label: "Workass Mock ACP"},
		Broadcast:            events.Broadcast,
		StdoutFlushInterval:  50 * time.Millisecond,
		ThoughtFlushInterval: 40 * time.Millisecond,
	})
	t.Cleanup(func() { manager.Reset() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	init, err := manager.InitializeBridge(ctx, "mock-tab")
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if init.ProtocolVersion != 1 || init.AgentName != "Workass Mock ACP" {
		t.Fatalf("init = %+v", init)
	}

	session, err := manager.NewSession(ctx, SessionOptions{TabID: "mock-tab"})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if session.SessionID == "" || session.Agent != "Workass Mock ACP" || len(session.Models) != 1 || len(session.Modes) != 2 {
		t.Fatalf("session = %+v", session)
	}

	job := startAppChatJob(t, manager, session.SessionID, "mock-tab", "hello from mock")
	start := events.waitJobType(t, jobID(job), "start", 2*time.Second)
	startJob := start["job"].(map[string]any)
	// Renderer fixture: desktop/renderer/app.js:4901-4903 stores chatJobs by chatId,
	// app.js:4932-4941 consumes job.chatId and sets the assistant status to "running",
	// and app.js:4577-4578 is the tabRunning() predicate that drives the composer.
	if startJob["chatId"] != "chat-mock-tab" || startJob["tabId"] != "mock-tab" || startJob["sessionId"] != session.SessionID || startJob["status"] != "running" {
		t.Fatalf("start running-state job shape = %#v", startJob)
	}
	end := events.waitJobEnd(t, jobID(job), 5*time.Second)
	assertJobStatus(t, end, "done", 0, "end_turn")
	dataEvents := events.jobEvents(jobID(job), "data")
	if len(dataEvents) != 1 {
		t.Fatalf("stdout chunks = %d, want one coalesced chunk; events=%#v", len(dataEvents), dataEvents)
	}
	assertDataChunkShape(t, dataEvents[0], jobID(job))
	chunk := dataEvents[0]["chunk"].(string)
	if !strings.Contains(chunk, `Mock ACP turn 1: Active Workass runtime for this turn: provider "custom"`) ||
		!strings.Contains(chunk, "Workass context:") || !strings.Contains(chunk, "User request:\nhello from mock") {
		t.Fatalf("stdout chunk = %q", chunk)
	}
	if !events.jobEventKindsInOrder(jobID(job), []string{"start", "acp", "data", "end"}) {
		t.Fatalf("job events were not ordered as expected: %#v", events.snapshot())
	}

	slow := startAppChatJob(t, manager, session.SessionID, "mock-tab", "[mock:slow] cancel this")
	events.waitJobType(t, jobID(slow), "acp", 2*time.Second)
	if !manager.CancelJob(jobID(slow)) {
		t.Fatalf("cancel returned false")
	}
	slowEnd := events.waitJobEnd(t, jobID(slow), 5*time.Second)
	assertJobStatus(t, slowEnd, "failed", 130, "cancelled")

	afterCancel := startAppChatJob(t, manager, session.SessionID, "mock-tab", "after cancel")
	assertJobStatus(t, events.waitJobEnd(t, jobID(afterCancel), 5*time.Second), "done", 0, "end_turn")

	failed := startAppChatJob(t, manager, session.SessionID, "mock-tab", "[mock:error]")
	failedEnd := events.waitJobEnd(t, jobID(failed), 5*time.Second)
	assertJobStatus(t, failedEnd, "failed", 1, "")
	if errText := jobFromEnd(failedEnd)["error"].(string); !strings.Contains(errText, "Deterministic mock failure") {
		t.Fatalf("error text = %q", errText)
	}

	afterError := startAppChatJob(t, manager, session.SessionID, "mock-tab", "after error")
	assertJobStatus(t, events.waitJobEnd(t, jobID(afterError), 5*time.Second), "done", 0, "end_turn")
}

func TestMockLostTerminalEventuallyEnds(t *testing.T) {
	root := repoRoot(t)
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir: root,
		Provider: ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID:       "mock",
		Broadcast:               events.Broadcast,
		StdoutFlushInterval:     5 * time.Millisecond,
		ThoughtFlushInterval:    5 * time.Millisecond,
		PromptReconcileInterval: 25 * time.Millisecond,
		PromptReconcileTimeout:  100 * time.Millisecond,
		PromptTerminalGrace:     100 * time.Millisecond,
	})
	t.Cleanup(func() { manager.Reset() })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: "lost-terminal-tab", ChatID: "chat-lost-terminal-tab"})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	job := startAppChatJob(t, manager, session.SessionID, "lost-terminal-tab", "[mock:lost-terminal] complete visibly")
	_ = events.waitJobType(t, jobID(job), "data", time.Second)
	end := events.waitJobEnd(t, jobID(job), 750*time.Millisecond)
	assertJobStatus(t, end, "done", 0, "end_turn")
}

func TestMockLostTerminalCannotLeaveJobRunningWhenAdapterDoesNotReleasePrompt(t *testing.T) {
	root := repoRoot(t)
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir: root,
		Provider: ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID:       "mock",
		Broadcast:               events.Broadcast,
		StdoutFlushInterval:     5 * time.Millisecond,
		ThoughtFlushInterval:    5 * time.Millisecond,
		PromptReconcileInterval: 25 * time.Millisecond,
		PromptReconcileTimeout:  100 * time.Millisecond,
		PromptTerminalGrace:     50 * time.Millisecond,
		CrashRecoveryBackoff:    time.Millisecond,
		CrashRecoveryWindow:     time.Second,
	})
	t.Cleanup(func() { manager.Reset() })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: "unreleased-terminal-tab", ChatID: "chat-unreleased-terminal-tab"})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	job := startAppChatJob(t, manager, session.SessionID, "unreleased-terminal-tab", "[mock:lost-terminal-unreleased] complete visibly")
	_ = events.waitJobType(t, jobID(job), "data", time.Second)
	end := events.waitJobEnd(t, jobID(job), 3*time.Second)
	assertJobStatus(t, end, "failed", 1, "engine-crash")
}

func TestMockPromptSilenceDoesNotCompleteAnAuthoritativelyActiveTurn(t *testing.T) {
	root := repoRoot(t)
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir: root,
		Provider: ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID:       "mock",
		Broadcast:               events.Broadcast,
		StdoutFlushInterval:     5 * time.Millisecond,
		ThoughtFlushInterval:    5 * time.Millisecond,
		PromptReconcileInterval: 20 * time.Millisecond,
		PromptReconcileTimeout:  100 * time.Millisecond,
		PromptTerminalGrace:     50 * time.Millisecond,
	})
	t.Cleanup(func() { manager.Reset() })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: "active-silent-tab", ChatID: "chat-active-silent-tab"})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	job := startAppChatJob(t, manager, session.SessionID, "active-silent-tab", "[mock:active-without-terminal] still active")
	_ = events.waitJobType(t, jobID(job), "data", time.Second)
	time.Sleep(150 * time.Millisecond)
	if running, ok := manager.RunningJobForChat("active-silent-tab", "chat-active-silent-tab"); !ok || running["id"] != jobID(job) {
		t.Fatalf("authoritatively active silent turn was terminalized: running=%#v ok=%v", running, ok)
	}
	if !manager.CancelJob(jobID(job)) {
		t.Fatal("cancel active silent turn")
	}
	end := events.waitJobEnd(t, jobID(job), time.Second)
	assertJobStatus(t, end, "failed", 130, "cancelled")
}

func TestStartJobStaleSessionIDCannotDriveAnotherChat(t *testing.T) {
	manager, events := newFakeManager(t, "echo-prompt", Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	chatA, err := manager.NewSession(ctx, SessionOptions{TabID: "owner-tab-a", ChatID: "owner-chat-a"})
	if err != nil {
		t.Fatal(err)
	}
	chatB, err := manager.NewSession(ctx, SessionOptions{TabID: "owner-tab-b", ChatID: "owner-chat-b"})
	if err != nil {
		t.Fatal(err)
	}
	job, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: chatA.SessionID, // deliberately stale/wrong
		TabID: "owner-tab-b", ChatID: "owner-chat-b", Prompt: "belongs only to chat B",
	})
	if err != nil {
		t.Fatalf("stale id should recover the requested owner, not fail or cross-wire: %v", err)
	}
	end := events.waitJobEnd(t, jobID(job), 5*time.Second)
	ended := jobFromEnd(end)
	if ended["sessionId"] != chatB.SessionID {
		t.Fatalf("job used session %v, want chat B session %s (chat A was %s)", ended["sessionId"], chatB.SessionID, chatA.SessionID)
	}
	if liveA, ok := manager.LiveSession(chatA.SessionID); !ok || liveA.TabID != "owner-tab-a" || liveA.ChatID != "owner-chat-a" {
		t.Fatalf("chat A ownership was disturbed: %#v ok=%v", liveA, ok)
	}
}

func TestMockPermissionMarkerRoundTrip(t *testing.T) {
	root := repoRoot(t)
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir:             root,
		Provider:            ProviderConfig{Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Label: "Workass Mock ACP"},
		Broadcast:           events.Broadcast,
		StdoutFlushInterval: 30 * time.Millisecond,
	})
	t.Cleanup(func() { manager.Reset() })
	session := newMockSession(t, manager, "perm-mock-tab")
	job := startAppChatJob(t, manager, session.SessionID, "perm-mock-tab", "[mock:permission] please ask")
	req := events.waitChannel(t, "chat:permission-request", 3*time.Second).payload.(map[string]any)
	if req["jobId"] != jobID(job) || req["sessionId"] != session.SessionID || req["title"] != "Mock permission gate" || req["kind"] != "execute" {
		t.Fatalf("mock permission request = %#v", req)
	}
	if !manager.PermissionDecide(req["id"].(string), "allow-once") {
		t.Fatalf("permission decide returned false")
	}
	end := events.waitJobEnd(t, jobID(job), 5*time.Second)
	assertJobStatus(t, end, "done", 0, "end_turn")
	if result := jobFromEnd(end)["result"].(string); !strings.Contains(result, "Permission outcome: selected allow-once.") {
		t.Fatalf("permission result = %q", result)
	}
}

func TestMockSteerMidSlowTurnReflectedInOutput(t *testing.T) {
	root := repoRoot(t)
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir:             root,
		Provider:            ProviderConfig{Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Label: "Workass Mock ACP"},
		Broadcast:           events.Broadcast,
		StdoutFlushInterval: 20 * time.Millisecond,
	})
	t.Cleanup(func() { manager.Reset() })
	session := newMockSession(t, manager, "steer-mock-tab")
	job := startAppChatJob(t, manager, session.SessionID, "steer-mock-tab", "[mock:slow] [mock:steer] base")
	_ = events.waitJobType(t, jobID(job), "acp", 3*time.Second)

	res := manager.Steer(session.SessionID, "follow-up steer", nil, "")
	if res["ok"] != true || res["live"] != true || res["queued"] != false {
		t.Fatalf("steer result = %#v", res)
	}
	end := events.waitJobEnd(t, jobID(job), 5*time.Second)
	assertJobStatus(t, end, "done", 0, "end_turn")
	if result := jobFromEnd(end)["result"].(string); !strings.Contains(result, "Steer input: follow-up steer.") {
		t.Fatalf("steered result = %q", result)
	}
}

func TestSteerUnsupportedKeepsClientQueue(t *testing.T) {
	manager, events := newFakeManager(t, "slow-prompt", Options{})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "steer-unsupported-tab")
	job := startAppChatJob(t, manager, session.SessionID, "steer-unsupported-tab", "unsupported steer")

	res := manager.Steer(session.SessionID, "local queue should stay", nil, "")
	if res["ok"] != false || res["unsupported"] != true || res["queued"] != false {
		t.Fatalf("unsupported steer result = %#v", res)
	}
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 2*time.Second), "done", 0, "end_turn")
}

func TestSteerRejectsInternalMaintenancePrompt(t *testing.T) {
	manager, _ := newFakeManager(t, "echo-prompt", Options{Provider: ProviderConfig{ID: "codex"}})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "internal-steer-tab")
	bridge := manager.bridgeForSession(session.SessionID, SessionOptions{TabID: "internal-steer-tab", SessionID: session.SessionID})
	if bridge == nil {
		t.Fatal("missing fake bridge")
	}
	maintenance := &Job{Status: "running", SessionID: session.SessionID, internal: true}
	bridge.setJobForSession(session.SessionID, maintenance)
	defer bridge.clearJobForSession(session.SessionID, maintenance)

	res := manager.Steer(session.SessionID, "must become a user follow-up", nil, "u-internal")
	if res["ok"] != false || res["queued"] != false {
		t.Fatalf("internal maintenance steer result = %#v", res)
	}
}

func TestCodexSteerUsesAcknowledgedNativeRequestWithoutCancellingRunningTurn(t *testing.T) {
	manager, events := newFakeManager(t, "codex-steer", Options{
		Provider: ProviderConfig{ID: "codex"},
	})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "codex-steer-tab")
	job := startAppChatJob(t, manager, session.SessionID, "codex-steer-tab", "base turn")
	_ = events.waitJobType(t, jobID(job), "acp", 2*time.Second)

	const clientUserMessageID = "u-steer-receipt"
	res := manager.Steer(session.SessionID, "change direction", nil, clientUserMessageID)
	if res["ok"] != true || res["live"] != true || res["queued"] != false || res["strategy"] != "codex-live" {
		t.Fatalf("codex steer result = %#v", res)
	}
	if res["receipt"] != true {
		t.Fatalf("codex steer did not advertise its consumption receipt: %#v", res)
	}
	if res["turnId"] != "fake-active-turn" {
		t.Fatalf("codex steer acknowledgement missing active turn id: %#v", res)
	}
	end := events.waitJobEnd(t, jobID(job), 2*time.Second)
	assertJobStatus(t, end, "done", 0, "end_turn")
	consumed, _ := jobFromEnd(end)["consumedSteerIds"].([]string)
	if len(consumed) != 1 || consumed[0] != clientUserMessageID {
		t.Fatalf("consumed steer receipts = %#v", jobFromEnd(end)["consumedSteerIds"])
	}
	if result := asString(jobFromEnd(end)["result"]); !strings.Contains(result, "codex steered: change direction") {
		t.Fatalf("codex steered result = %q", result)
	}
}

func TestCodexSteerAlreadyFinishedNativeTurnInterruptsOnlyStaleWrapperForNextTurn(t *testing.T) {
	manager, events := newFakeManager(t, "codex-steer-next-turn", Options{
		Provider: ProviderConfig{ID: "codex"},
	})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "codex-steer-next-turn-tab")
	job := startAppChatJob(t, manager, session.SessionID, "codex-steer-next-turn-tab", "base turn")
	_ = events.waitJobType(t, jobID(job), "acp", 2*time.Second)

	res := manager.Steer(session.SessionID, "start this next", nil, "u-next-turn")
	if res["ok"] != false || res["strategy"] != "interrupt-queue" || res["interrupted"] != true || res["reason"] != "no-active-turn" {
		t.Fatalf("finished-turn steer disposition = %#v", res)
	}
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 2*time.Second), "failed", 130, "cancelled")
}

func TestCodexSteerNonSteerableReviewQueuesWithoutCancellingReview(t *testing.T) {
	manager, events := newFakeManager(t, "codex-steer-nonsteerable", Options{
		Provider: ProviderConfig{ID: "codex"},
	})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "codex-steer-review-tab")
	job := startAppChatJob(t, manager, session.SessionID, "codex-steer-review-tab", "review turn")
	_ = events.waitJobType(t, jobID(job), "acp", 2*time.Second)

	res := manager.Steer(session.SessionID, "after review", nil, "u-after-review")
	if res["ok"] != false || res["strategy"] != "queue" || res["interrupted"] == true || res["reason"] != "active-turn-not-steerable" {
		t.Fatalf("non-steerable review disposition = %#v", res)
	}
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 2*time.Second), "done", 0, "end_turn")
}

func TestCodexSteerDuplicateConsumptionReceiptIsIdempotent(t *testing.T) {
	manager, events := newFakeManager(t, "codex-steer-duplicate-receipt", Options{
		Provider: ProviderConfig{ID: "codex"},
	})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "codex-steer-duplicate-tab")
	job := startAppChatJob(t, manager, session.SessionID, "codex-steer-duplicate-tab", "base turn")
	_ = events.waitJobType(t, jobID(job), "acp", 2*time.Second)

	const clientUserMessageID = "u-duplicate-receipt"
	res := manager.Steer(session.SessionID, "one direction", nil, clientUserMessageID)
	if res["ok"] != true || res["turnId"] != "fake-active-turn" || res["receipt"] != true {
		t.Fatalf("duplicate-receipt steer acknowledgement = %#v", res)
	}
	end := events.waitJobEnd(t, jobID(job), 2*time.Second)
	assertJobStatus(t, end, "done", 0, "end_turn")
	consumed, _ := jobFromEnd(end)["consumedSteerIds"].([]string)
	if len(consumed) != 1 || consumed[0] != clientUserMessageID {
		t.Fatalf("duplicate receipt was not deduplicated: %#v", jobFromEnd(end)["consumedSteerIds"])
	}
}

func TestClaudeSteerInterruptsCurrentTurnForImmediateFIFOFollowup(t *testing.T) {
	manager, events := newFakeManager(t, "interruptible-prompt", Options{
		Provider: ProviderConfig{ID: "claude"},
	})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "claude-queue-tab")
	job := startAppChatJob(t, manager, session.SessionID, "claude-queue-tab", "finish current turn")

	res := manager.Steer(session.SessionID, "next Claude turn", nil, "")
	if res["ok"] != false || res["unsupported"] != true || res["interrupted"] != true || res["queued"] != false || res["strategy"] != "interrupt-queue" {
		t.Fatalf("claude interrupt+queue result = %#v", res)
	}
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 2*time.Second), "failed", 130, "cancelled")
}

func TestClaudeNativeSteerUsesAcknowledgedLiveRequestWithoutCancellingRunningTurn(t *testing.T) {
	methodLog := filepath.Join(t.TempDir(), "methods.log")
	manager, events := newFakeManager(t, "claude-steer", Options{
		Provider: ProviderConfig{
			ID:  "claude",
			Env: map[string]string{"WORKASS_FAKE_ACP_METHOD_LOG": methodLog},
		},
	})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "claude-live-steer-tab")
	job := startAppChatJob(t, manager, session.SessionID, "claude-live-steer-tab", "base turn")
	_ = events.waitJobType(t, jobID(job), "acp", 2*time.Second)

	const clientUserMessageID = "u-claude-steer-receipt"
	res := manager.Steer(session.SessionID, "change direction live", nil, clientUserMessageID)
	if res["ok"] != true || res["live"] != true || res["queued"] != false || res["strategy"] != "claude-live" {
		t.Fatalf("claude native steer result = %#v", res)
	}
	if res["turnId"] != "fake-claude-steer" || res["receipt"] != true {
		t.Fatalf("claude native steer acknowledgement = %#v", res)
	}
	end := events.waitJobEnd(t, jobID(job), 2*time.Second)
	assertJobStatus(t, end, "done", 0, "end_turn")
	if result := asString(jobFromEnd(end)["result"]); !strings.Contains(result, "claude steered: change direction live") {
		t.Fatalf("claude steered result = %q", result)
	}
	consumed, _ := jobFromEnd(end)["consumedSteerIds"].([]string)
	if len(consumed) != 1 || consumed[0] != clientUserMessageID {
		t.Fatalf("claude consumed steer receipts = %#v", jobFromEnd(end)["consumedSteerIds"])
	}
	methods, err := os.ReadFile(methodLog)
	if err != nil {
		t.Fatalf("read fake method log: %v", err)
	}
	if strings.Contains(string(methods), "session/cancel") {
		t.Fatalf("native Claude steer emitted session/cancel:\n%s", methods)
	}
	if strings.Count(string(methods), "_workass/claude/steer") != 1 {
		t.Fatalf("native Claude steer method log =\n%s", methods)
	}
}

func TestUnpatchedCodexAdapterFallsBackToInterruptQueueWithoutHanging(t *testing.T) {
	manager, events := newFakeManager(t, "interruptible-prompt", Options{
		Provider: ProviderConfig{ID: "codex"},
	})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "codex-unpatched-steer-tab")
	job := startAppChatJob(t, manager, session.SessionID, "codex-unpatched-steer-tab", "base turn")
	_ = events.waitJobType(t, jobID(job), "acp", 2*time.Second)

	res := manager.Steer(session.SessionID, "redirect without native extension", nil, "")
	if res["ok"] != false || res["interrupted"] != true || res["strategy"] != "interrupt-queue" {
		t.Fatalf("unpatched codex fallback = %#v", res)
	}
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 2*time.Second), "failed", 130, "cancelled")
}

func TestCloseSessionEmitsProcChanged(t *testing.T) {
	root := repoRoot(t)
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir: root,
		Provider: ProviderConfig{
			Command: "node",
			Args:    []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD:     root,
			Label:   "Workass Mock ACP",
		},
		Broadcast: events.Broadcast,
	})
	t.Cleanup(func() { manager.Reset() })
	session := newMockSession(t, manager, "close-proc-tab")
	if !manager.CloseSession(context.Background(), session.SessionID) {
		t.Fatalf("close session returned false")
	}
	ev := events.waitFor(t, 3*time.Second, func(ev collectedEvent) bool {
		if ev.channel != "proc:changed" {
			return false
		}
		payload, _ := ev.payload.(map[string]any)
		processes, _ := payload["processes"].([]map[string]any)
		for _, proc := range processes {
			if proc["status"] == "failed" && proc["finishedAt"] != nil {
				return true
			}
		}
		rawProcesses, _ := payload["processes"].([]any)
		for _, raw := range rawProcesses {
			proc, _ := raw.(map[string]any)
			if proc["status"] == "failed" && proc["finishedAt"] != nil {
				return true
			}
		}
		return false
	})
	t.Logf("trace event proc:changed after close payload=%#v", ev.payload)
}

func TestFakePermissionDecideRoundTrip(t *testing.T) {
	manager, events := newFakeManager(t, "permission", Options{PermissionTimeout: 2 * time.Second})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "perm-tab")
	job := startAppChatJob(t, manager, session.SessionID, "perm-tab", "needs permission")

	ev := events.waitChannel(t, "chat:permission-request", 2*time.Second)
	payload := ev.payload.(map[string]any)
	if payload["id"] == "" || payload["jobId"] != jobID(job) || payload["sessionId"] != session.SessionID || payload["title"] != "Run fake tool" || payload["kind"] != "execute" {
		t.Fatalf("permission payload = %#v", payload)
	}
	options := payload["options"].([]any)
	if len(options) != 2 {
		t.Fatalf("permission options = %#v", options)
	}
	first := options[0].(map[string]any)
	if first["optionId"] != "allow" || first["name"] != "Allow once" || first["kind"] != "allow_once" {
		t.Fatalf("first option = %#v", first)
	}
	if !manager.PermissionDecide(payload["id"].(string), "allow") {
		t.Fatalf("permission decide returned false")
	}

	end := events.waitJobEnd(t, jobID(job), 2*time.Second)
	assertJobStatus(t, end, "done", 0, "end_turn")
	if result := jobFromEnd(end)["result"].(string); !strings.Contains(result, "permission selected allow") {
		t.Fatalf("result = %q", result)
	}
}

func TestPermissionWaitSuppressesPromptReconciliationKill(t *testing.T) {
	manager, events := newFakeManager(t, "permission-reconcile-hang", Options{
		PromptReconcileInterval: 15 * time.Millisecond,
		PromptReconcileTimeout:  15 * time.Millisecond,
		PromptTerminalGrace:     20 * time.Millisecond,
		PermissionTimeout:       time.Second,
		CrashRecoveryBackoff:    time.Millisecond,
		CrashRecoveryWindow:     time.Second,
	})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "permission-reconcile-hang-tab")
	job := startAppChatJob(t, manager, session.SessionID, "permission-reconcile-hang-tab", "needs a long decision")
	permission := events.waitChannel(t, "chat:permission-request", 2*time.Second).payload.(map[string]any)

	// Three reconciliation timeouts would recycle the bridge in under 100ms if
	// a healthy user decision wait were misclassified as provider silence.
	time.Sleep(140 * time.Millisecond)
	if running, ok := manager.RunningJobForChat("permission-reconcile-hang-tab", "chat-permission-reconcile-hang-tab"); !ok || running["id"] != jobID(job) {
		t.Fatalf("permission-waiting job was killed by reconciliation: running=%#v ok=%v", running, ok)
	}
	if !manager.PermissionDecide(asString(permission["id"]), "allow") {
		t.Fatal("permission decision was not accepted")
	}
	end := events.waitJobEnd(t, jobID(job), 2*time.Second)
	assertJobStatus(t, end, "done", 0, "end_turn")
}

func TestFakePermissionTimeoutUsesFallbackDeny(t *testing.T) {
	manager, events := newFakeManager(t, "permission", Options{PermissionTimeout: 60 * time.Millisecond})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "timeout-tab")
	job := startAppChatJob(t, manager, session.SessionID, "timeout-tab", "timeout permission")

	_ = events.waitChannel(t, "chat:permission-request", 2*time.Second)
	end := events.waitJobEnd(t, jobID(job), 2*time.Second)
	assertJobStatus(t, end, "done", 0, "end_turn")
	if result := jobFromEnd(end)["result"].(string); !strings.Contains(result, "permission selected deny") {
		t.Fatalf("result = %q", result)
	}
}

func TestProviderConfigFileDefaultsAndConfiguredRegistry(t *testing.T) {
	root := repoRoot(t)
	file := filepath.Join(t.TempDir(), "providers.json")
	defaults, err := LoadProviderConfigs(file, root)
	if err != nil {
		t.Fatalf("load absent providers: %v", err)
	}
	if _, ok := providerFromSlice(defaults, "mock"); !ok || len(defaults) < 5 {
		t.Fatalf("built-in providers = %#v", defaults)
	}
	mock, _ := providerFromSlice(defaults, "mock")
	if !mock.Enabled || mock.Command != "node" || len(mock.Args) == 0 {
		t.Fatalf("mock defaults = %#v", mock)
	}

	configured := []byte(`[{"id":"fake-agent","name":"Fake Agent","command":"fake-acp","args":["--serve"],"enabled":true}]`)
	if err := os.WriteFile(file, configured, 0o600); err != nil {
		t.Fatalf("write providers: %v", err)
	}
	providers, err := LoadProviderConfigs(file, root)
	if err != nil {
		t.Fatalf("load configured providers: %v", err)
	}
	fake, ok := providerFromSlice(providers, "fake-agent")
	if !ok || fake.Command != "fake-acp" || !fake.Enabled {
		t.Fatalf("configured providers = %#v", providers)
	}
	if _, ok := providerFromSlice(providers, "codex"); !ok || len(providers) < 7 {
		t.Fatalf("configured cache did not append built-ins: %#v", providers)
	}
}

func TestProviderRegistryCatalogToggleFailureAndConcurrentIsolation(t *testing.T) {
	root := repoRoot(t)
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{
			{ID: "mock", Name: "Mock Provider", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Enabled: true},
			{ID: "fake-agent", Name: "Fake Agent", Command: os.Args[0], Args: []string{"-test.run=TestFakeACPHelper", "--"}, CWD: root, Env: map[string]string{"WORKASS_FAKE_ACP": "1", "WORKASS_FAKE_ACP_MODE": "slow-prompt"}, Enabled: true},
			{ID: "disabled-agent", Name: "Disabled Agent", Command: os.Args[0], Args: []string{"-test.run=TestFakeACPHelper", "--"}, CWD: root, Env: map[string]string{"WORKASS_FAKE_ACP": "1", "WORKASS_FAKE_ACP_MODE": "echo-prompt"}, Enabled: false},
			{ID: "failing-agent", Name: "Failing Agent", Command: os.Args[0], Args: []string{"-test.run=TestFakeACPHelper", "--"}, CWD: root, Env: map[string]string{"WORKASS_FAKE_ACP": "1", "WORKASS_FAKE_ACP_MODE": "crash-stderr"}, Enabled: true},
		},
		DefaultProviderID:    "mock",
		Broadcast:            events.Broadcast,
		InitTimeout:          400 * time.Millisecond,
		StdoutFlushInterval:  10 * time.Millisecond,
		ThoughtFlushInterval: 10 * time.Millisecond,
		RSSSampleInterval:    time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	catalog := manager.Catalog(context.Background())
	groups, _ := catalog["groups"].([]CatalogGroup)
	if len(groups) != 3 || catalog["models"] == nil || catalog["modes"] == nil {
		t.Fatalf("catalog = %#v", catalog)
	}
	assertCatalogGroup(t, groups, "mock", providerStatusReady, true)
	assertCatalogEfforts(t, findCatalogGroup(groups, "mock"), "mock-deterministic", []string{"low", "high"})
	assertCatalogGroup(t, groups, "fake-agent", providerStatusReady, true)
	assertCatalogGroup(t, groups, "failing-agent", providerStatusError, false)
	if findCatalogGroup(groups, "disabled-agent") != nil {
		t.Fatalf("disabled provider leaked into catalog groups: %#v", groups)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mockSession, err := manager.NewSession(ctx, SessionOptions{TabID: "tab-a", ChatID: "chat-a", ProviderID: "mock"})
	if err != nil {
		t.Fatalf("mock session: %v", err)
	}
	fakeSession, err := manager.NewSession(ctx, SessionOptions{TabID: "tab-b", ChatID: "chat-b", ProviderID: "fake-agent"})
	if err != nil {
		t.Fatalf("fake session: %v", err)
	}
	if mockSession.ProviderID != "mock" || fakeSession.ProviderID != "fake-agent" {
		t.Fatalf("provider-bound sessions mock=%#v fake=%#v", mockSession, fakeSession)
	}
	mockBridge := manager.bridgeForSession(mockSession.SessionID, SessionOptions{SessionID: mockSession.SessionID})
	fakeBridge := manager.bridgeForSession(fakeSession.SessionID, SessionOptions{SessionID: fakeSession.SessionID})
	if mockBridge == nil || fakeBridge == nil || mockBridge == fakeBridge || !strings.HasPrefix(mockBridge.Key(), "mock:") || !strings.HasPrefix(fakeBridge.Key(), "fake-agent:") {
		t.Fatalf("bridge isolation mock=%v fake=%v", bridgeKey(mockBridge), bridgeKey(fakeBridge))
	}

	mockJob, err := manager.StartJob(context.Background(), JobStartOptions{Kind: "app-chat", SessionID: mockSession.SessionID, ChatID: "chat-a", TabID: "tab-a", Prompt: "[mock:slow] concurrent mock"})
	if err != nil {
		t.Fatalf("mock job: %v", err)
	}
	fakeJob, err := manager.StartJob(context.Background(), JobStartOptions{Kind: "app-chat", SessionID: fakeSession.SessionID, ChatID: "chat-b", TabID: "tab-b", Prompt: "concurrent fake"})
	if err != nil {
		t.Fatalf("fake job: %v", err)
	}
	if mockJob["providerId"] != "mock" || fakeJob["providerId"] != "fake-agent" {
		t.Fatalf("job provider ids mock=%#v fake=%#v", mockJob, fakeJob)
	}
	mockEnd := events.waitJobEnd(t, jobID(mockJob), 5*time.Second)
	fakeEnd := events.waitJobEnd(t, jobID(fakeJob), 5*time.Second)
	assertJobStatus(t, mockEnd, "done", 0, "end_turn")
	assertJobStatus(t, fakeEnd, "done", 0, "end_turn")
	if result := jobFromEnd(mockEnd)["result"].(string); !strings.Contains(result, "Mock ACP turn") {
		t.Fatalf("mock result routed to wrong provider: %q", result)
	}
	if result := jobFromEnd(fakeEnd)["result"].(string); !strings.Contains(result, "Workass context:") || !strings.Contains(result, "User request:\nconcurrent fake") {
		t.Fatalf("fake result routed to wrong provider: %q", result)
	}

	if _, err := manager.ToggleProvider(context.Background(), "fake-agent", false); err != nil {
		t.Fatalf("toggle fake-agent: %v", err)
	}
	toggleEvent := events.waitChannel(t, "chat:catalog", 2*time.Second).payload.(map[string]any)
	toggleGroups, _ := toggleEvent["groups"].([]CatalogGroup)
	if findCatalogGroup(toggleGroups, "fake-agent") != nil {
		t.Fatalf("disabled toggled provider leaked into catalog: %#v", toggleGroups)
	}
}

func TestSetModelPassesEffortSuffixedIDUnchanged(t *testing.T) {
	manager, _ := newFakeManager(t, "echo-prompt", Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	session, err := manager.NewSession(ctx, SessionOptions{TabID: "effort-tab", ChatID: "effort-chat"})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	want := "fake-model[high]"
	result, err := manager.SetModel(ctx, session.SessionID, want)
	if err != nil {
		t.Fatalf("set suffixed model: %v", err)
	}
	if result["currentModelId"] != want {
		t.Fatalf("set-model result = %#v, want currentModelId %q", result, want)
	}
	bridge := manager.bridgeForSession(session.SessionID, SessionOptions{SessionID: session.SessionID})
	if bridge == nil {
		t.Fatal("session bridge missing")
	}
	bridge.mu.Lock()
	current := copyStringPtr(bridge.currentModel)
	bridge.mu.Unlock()
	if current == nil || *current != want {
		t.Fatalf("bridge currentModel = %v, want %q", current, want)
	}
}

func TestTurnReappliesPersistedModelAndPermissionMode(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "turn-controls.log")
	t.Setenv("WORKASS_FAKE_ACP_CONFIG_LOG", logPath)
	manager, events := newFakeManager(t, "echo-prompt", Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := manager.NewSession(ctx, SessionOptions{TabID: "controls-tab", ChatID: "controls-chat"})
	if err != nil {
		t.Fatalf("new controls session: %v", err)
	}
	job, err := manager.StartJob(ctx, JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, TabID: "controls-tab", ChatID: "controls-chat",
		ProviderID: session.ProviderID, ModelID: "fake-model[high]", ModeID: "bypass", Prompt: "use this chat's controls",
	})
	if err != nil {
		t.Fatalf("start controlled turn: %v", err)
	}
	end := events.waitJobEnd(t, jobID(job), 4*time.Second)
	assertJobStatus(t, end, "done", 0, "end_turn")
	calls := readConfigCalls(t, logPath)
	if !containsString(calls, "model=fake-model[high]") || !containsString(calls, "mode=bypass") {
		t.Fatalf("turn did not restore model+mode before prompt, calls = %#v", calls)
	}
	t.Logf("trace restored turn controls session=%s calls=%v", session.SessionID, calls)
}

func TestTurnTranslatesLegacyPermissionAndRoutesCodexEffortAxis(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "codex-controls.log")
	t.Setenv("WORKASS_FAKE_ACP_CONFIG_LOG", logPath)
	manager, events := newFakeManager(t, "codex-controls", Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := manager.NewSession(ctx, SessionOptions{TabID: "codex-controls-tab", ChatID: "codex-controls-chat"})
	if err != nil {
		t.Fatalf("new Codex controls session: %v", err)
	}
	if session.CurrentModelID == nil || *session.CurrentModelID != "gpt-5.6-sol[xhigh]" {
		t.Fatalf("initialized composite model = %v", session.CurrentModelID)
	}
	job, err := manager.StartJob(ctx, JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, TabID: "codex-controls-tab", ChatID: "codex-controls-chat",
		ProviderID: session.ProviderID, ModelID: "gpt-5.6-sol[high]", ModeID: "bypassPermissions", Prompt: "restore compatible controls",
	})
	if err != nil {
		t.Fatalf("start Codex controlled turn: %v", err)
	}
	end := events.waitJobEnd(t, jobID(job), 4*time.Second)
	assertJobStatus(t, end, "done", 0, "end_turn")
	calls := readConfigCalls(t, logPath)
	for _, want := range []string{"model=gpt-5.6-sol", "reasoning_effort=high", "mode=agent-full-access"} {
		if !containsString(calls, want) {
			t.Fatalf("missing compatible Codex control %q in calls %#v", want, calls)
		}
	}
	if containsString(calls, "model=gpt-5.6-sol[high]") || containsString(calls, "mode=bypassPermissions") {
		t.Fatalf("adapter received renderer composite/stale mode: %#v", calls)
	}
}

func TestTurnRoutesCodexCrossModelRestoreThroughSeparateEffortAxis(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "codex-cross-model-controls.log")
	t.Setenv("WORKASS_FAKE_ACP_CONFIG_LOG", logPath)
	manager, events := newFakeManager(t, "codex-controls", Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := manager.NewSession(ctx, SessionOptions{TabID: "codex-cross-model-tab", ChatID: "codex-cross-model-chat"})
	if err != nil {
		t.Fatalf("new Codex cross-model session: %v", err)
	}
	if session.CurrentModelID == nil || *session.CurrentModelID != "gpt-5.6-sol[xhigh]" {
		t.Fatalf("initialized composite model = %v", session.CurrentModelID)
	}
	job, err := manager.StartJob(ctx, JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, TabID: "codex-cross-model-tab", ChatID: "codex-cross-model-chat",
		ProviderID: session.ProviderID, ModelID: "gpt-5.6-luna[max]", ModeID: "agent-full-access", Prompt: "restore a different Codex base model",
	})
	if err != nil {
		t.Fatalf("start cross-model Codex turn: %v", err)
	}
	end := events.waitJobEnd(t, jobID(job), 4*time.Second)
	assertJobStatus(t, end, "done", 0, "end_turn")
	calls := readConfigCalls(t, logPath)
	for _, want := range []string{"model=gpt-5.6-luna", "reasoning_effort=max", "mode=agent-full-access"} {
		if !containsString(calls, want) {
			t.Fatalf("missing cross-model Codex control %q in calls %#v", want, calls)
		}
	}
	if containsString(calls, "model=gpt-5.6-luna[max]") {
		t.Fatalf("adapter received renderer composite as model value: %#v", calls)
	}
}

func TestTurnControlRestoreRejectionFallsBackToPrompt(t *testing.T) {
	manager, events := newFakeManager(t, "control-reject", Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := manager.NewSession(ctx, SessionOptions{TabID: "control-reject-tab", ChatID: "control-reject-chat"})
	if err != nil {
		t.Fatalf("new control-reject session: %v", err)
	}
	job, err := manager.StartJob(ctx, JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, TabID: "control-reject-tab", ChatID: "control-reject-chat",
		ProviderID: session.ProviderID, ModelID: "fake-model[high]", ModeID: "bypass", Prompt: "continue despite stale controls",
	})
	if err != nil {
		t.Fatalf("start control-reject turn: %v", err)
	}
	end := events.waitJobEnd(t, jobID(job), 4*time.Second)
	assertJobStatus(t, end, "done", 0, "end_turn")
	endedJob, _ := end["job"].(map[string]any)
	if result := strings.TrimSpace(asString(endedJob["result"])); result != "ok" {
		t.Fatalf("fallback prompt result = %q, want ok; job=%#v", result, endedJob)
	}
}

func TestClaudeEffortAxisSurfacesAndRoutesSeparately(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "config-calls.log")
	t.Setenv("WORKASS_FAKE_ACP_CONFIG_LOG", logPath)
	manager, events := newFakeManager(t, "claude-effort", Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := manager.NewSession(ctx, SessionOptions{TabID: "claude-eff-tab", ChatID: "claude-eff-chat"})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	// The separate thought_level option must surface as per-model effort stops so
	// the renderer shows the effort slider for Claude at all.
	bridge := manager.bridgeForSession(session.SessionID, SessionOptions{SessionID: session.SessionID})
	if bridge == nil {
		t.Fatal("session bridge missing")
	}
	wantEfforts := []string{"low", "medium", "high", "xhigh", "max"}
	if opus := findCatalogModel(session.Models, "opus[1m]"); opus == nil || !stringSlicesEqual(opus.Efforts, wantEfforts) {
		t.Fatalf("session opus model efforts = %#v in %#v", opus, session.Models)
	}
	_ = events

	// Picking effort hands the daemon a Codex-style composite id. The adapter only
	// knows the bare model, so the daemon must send the base model on the model axis
	// and the effort on the dedicated effort axis — never the composite.
	want := "opus[1m][high]"
	result, err := manager.SetModel(ctx, session.SessionID, want)
	if err != nil {
		t.Fatalf("set composite model: %v", err)
	}
	if result["currentModelId"] != want {
		t.Fatalf("set-model result = %#v, want currentModelId %q", result, want)
	}

	bridge.mu.Lock()
	gotModel := bridge.currentModelSelectionLocked()
	gotEffort := copyStringPtr(bridge.currentEffort)
	bridge.mu.Unlock()
	if gotModel == nil || *gotModel != want {
		t.Fatalf("bridge currentModel = %v, want %q", gotModel, want)
	}
	if gotEffort == nil || *gotEffort != "high" {
		t.Fatalf("bridge currentEffort = %v, want %q", gotEffort, "high")
	}

	calls := readConfigCalls(t, logPath)
	if !containsString(calls, "model=opus[1m]") {
		t.Fatalf("expected model axis to receive base id, calls = %#v", calls)
	}
	if !containsString(calls, "effort=high") {
		t.Fatalf("expected effort axis to receive high, calls = %#v", calls)
	}
	if containsString(calls, "model=opus[1m][high]") {
		t.Fatalf("composite id must never reach the model axis, calls = %#v", calls)
	}
}

func TestClaudeSyntheticDefaultInitialSessionUsesExplicitModel(t *testing.T) {
	manager, _ := newFakeManager(t, "claude-effort-default", Options{
		RSSSampleInterval: time.Hour,
		Provider:          ProviderConfig{ID: "claude"},
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := manager.NewSession(ctx, SessionOptions{
		TabID: "claude-default-tab", ChatID: "claude-default-chat",
	})
	if err != nil {
		t.Fatalf("new Claude default session: %v", err)
	}
	if session.CurrentModelID == nil || *session.CurrentModelID != "opus[1m]" {
		t.Fatalf("initial current model = %v, want explicit Opus alias", session.CurrentModelID)
	}
	if model := findCatalogModel(session.Models, "default"); model != nil {
		t.Fatalf("synthetic default remained visible: %#v in %#v", model, session.Models)
	}
	wantEfforts := []string{"low", "medium", "high", "xhigh", "max"}
	if opus := findCatalogModel(session.Models, "opus[1m]"); opus == nil || !stringSlicesEqual(opus.Efforts, wantEfforts) {
		t.Fatalf("explicit Opus efforts = %#v, want %#v in %#v", opus, wantEfforts, session.Models)
	}

	bridge := manager.bridgeForSession(session.SessionID, SessionOptions{SessionID: session.SessionID})
	if bridge == nil {
		t.Fatal("session bridge missing")
	}
	bridge.mu.Lock()
	opusEfforts, opusKnown := bridge.axisEffortsByModel["opus[1m]"]
	_, defaultKnown := bridge.axisEffortsByModel["default"]
	bridge.mu.Unlock()
	if !opusKnown || !stringSlicesEqual(opusEfforts, wantEfforts) || defaultKnown {
		t.Fatalf("effort capability keys = %#v, want explicit Opus only", bridge.axisEffortsByModel)
	}

	live, ok := manager.LiveSession(session.SessionID)
	if !ok || live.Info.CurrentModelID == nil || *live.Info.CurrentModelID != "opus[1m]" {
		t.Fatalf("live current model = %#v, want explicit Opus alias", live)
	}
	if got := nativeBindingModel(t, manager, "claude-default-tab", "claude-default-chat", "claude"); got != "opus[1m]" {
		t.Fatalf("native binding model = %q, want explicit Opus alias", got)
	}
}

func TestClaudePassiveSyntheticDefaultWinsAsExplicitAlias(t *testing.T) {
	manager, events := newFakeManager(t, "claude-effort-default", Options{
		RSSSampleInterval: time.Hour,
		Provider:          ProviderConfig{ID: "claude"},
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := manager.NewSession(ctx, SessionOptions{
		TabID: "claude-passive-default-tab", ChatID: "claude-passive-default-chat",
	})
	if err != nil {
		t.Fatalf("new Claude default session: %v", err)
	}
	explicit := "claude-fable-5[1m][high]"
	result, err := manager.SetModel(ctx, session.SessionID, explicit)
	if err != nil {
		t.Fatalf("set different explicit Claude model: %v", err)
	}
	if result["currentModelId"] != explicit {
		t.Fatalf("explicit set-model result = %#v, want %q", result, explicit)
	}

	bridge := manager.bridgeForSession(session.SessionID, SessionOptions{SessionID: session.SessionID})
	if bridge == nil {
		t.Fatal("session bridge missing")
	}
	bridge.applyConfigOptionsForSession(
		session.SessionID,
		fakeClaudeEffortConfigOptions("default", "default", "default"),
		true,
		false,
	)

	evt := events.waitChannel(t, "agent:apply", 2*time.Second).payload.(map[string]any)
	if evt["action"] != "session-refresh" || evt["modelId"] != "opus[1m]" || evt["currentModelId"] != "opus[1m]" {
		t.Fatalf("passive default capture event = %#v, want explicit Opus alias", evt)
	}
	if got := nativeBindingModel(t, manager, "claude-passive-default-tab", "claude-passive-default-chat", "claude"); got != "opus[1m]" {
		t.Fatalf("passive default native binding = %q, want explicit Opus alias", got)
	}
	live, ok := manager.LiveSession(session.SessionID)
	if !ok || live.Info.CurrentModelID == nil || *live.Info.CurrentModelID != "opus[1m]" {
		t.Fatalf("passive default live selection = %#v, want explicit Opus alias", live)
	}
}

func TestClaudeEffortCapabilityStaysModelSpecificWhenSwitchingToHaiku(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "config-calls.log")
	t.Setenv("WORKASS_FAKE_ACP_CONFIG_LOG", logPath)
	manager, _ := newFakeManager(t, "claude-effort", Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := manager.NewSession(ctx, SessionOptions{TabID: "claude-haiku-tab", ChatID: "claude-haiku-chat"})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	wantEfforts := []string{"low", "medium", "high", "xhigh", "max"}
	if opus := findCatalogModel(session.Models, "opus[1m]"); opus == nil || !stringSlicesEqual(opus.Efforts, wantEfforts) {
		t.Fatalf("initial opus efforts = %#v in %#v", opus, session.Models)
	}
	if haiku := findCatalogModel(session.Models, "haiku"); haiku == nil || len(haiku.Efforts) != 0 {
		t.Fatalf("Haiku must not inherit Opus effort controls: %#v in %#v", haiku, session.Models)
	}

	// A stale renderer can still restore the previously advertised haiku[low]
	// composite. The daemon must apply the valid base model, must not write the
	// now-absent effort option, and must return the canonical selection it really
	// applied so both persistence owners can repair themselves.
	result, err := manager.SetModel(ctx, session.SessionID, "haiku[low]")
	if err != nil {
		t.Fatalf("set stale Haiku composite: %v", err)
	}
	if result["currentModelId"] != "haiku" {
		t.Fatalf("Haiku set-model result = %#v, want canonical base", result)
	}
	bridge := manager.bridgeForSession(session.SessionID, SessionOptions{SessionID: session.SessionID})
	if bridge == nil {
		t.Fatal("session bridge missing")
	}
	bridge.mu.Lock()
	current := bridge.currentModelSelectionLocked()
	models := append([]Model(nil), bridge.models...)
	bridge.mu.Unlock()
	if current == nil || *current != "haiku" {
		t.Fatalf("bridge current selection = %v, want haiku", current)
	}
	if haiku := findCatalogModel(models, "haiku"); haiku == nil || len(haiku.Efforts) != 0 {
		t.Fatalf("live Haiku catalog leaked efforts: %#v in %#v", haiku, models)
	}
	if opus := findCatalogModel(models, "opus[1m]"); opus == nil || !stringSlicesEqual(opus.Efforts, wantEfforts) {
		t.Fatalf("switching to Haiku forgot Opus capabilities: %#v in %#v", opus, models)
	}
	calls := readConfigCalls(t, logPath)
	if !containsString(calls, "model=haiku") || containsString(calls, "effort=low") || containsString(calls, "model=haiku[low]") {
		t.Fatalf("stale Haiku selection routed incorrectly: %#v", calls)
	}

	// Returning to an effort-capable model must still use the separate axis.
	result, err = manager.SetModel(ctx, session.SessionID, "opus[1m][high]")
	if err != nil {
		t.Fatalf("restore Opus effort selection: %v", err)
	}
	if result["currentModelId"] != "opus[1m][high]" {
		t.Fatalf("restored Opus result = %#v", result)
	}
	calls = readConfigCalls(t, logPath)
	if !containsString(calls, "model=opus[1m]") || !containsString(calls, "effort=high") {
		t.Fatalf("Opus effort routing regressed after Haiku: %#v", calls)
	}
}

func TestT1cUndiscoveredBaseRowStillRoutesEffortComposite(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "cold-row-controls.log")
	t.Setenv("WORKASS_FAKE_ACP_CONFIG_LOG", logPath)
	manager, _ := newFakeManager(t, "claude-cold-effort", Options{
		RSSSampleInterval: time.Hour,
		Provider:          ProviderConfig{ID: "claude"},
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := manager.NewSession(ctx, SessionOptions{TabID: "cold-row-tab", ChatID: "cold-row-chat"})
	if err != nil {
		t.Fatalf("new cold session: %v", err)
	}
	bridge := manager.bridgeForSession(session.SessionID, SessionOptions{SessionID: session.SessionID})
	if bridge == nil {
		t.Fatal("session bridge missing")
	}
	// The prod shape (2026-07-27 22:48): a restore-time startup apply raced
	// catalog discovery, so the composite's base row was missing from b.models
	// and the raw composite went to the adapter, which refused it.
	bridge.mu.Lock()
	bridge.models = nil
	bridge.mu.Unlock()

	requested := "claude-fable-5[1m][max]"
	result, err := manager.SetModel(ctx, session.SessionID, requested)
	if err != nil {
		t.Fatalf("set composite with undiscovered base row: %v", err)
	}
	if result["appliedModelId"] != "claude-fable-5[1m]" {
		t.Fatalf("cold-row set-model result = %#v, want applied base", result)
	}
	if result["currentModelId"] != requested {
		t.Fatalf("cold-row durable selection = %#v, want %q preserved", result, requested)
	}
	calls := readConfigCalls(t, logPath)
	if !containsString(calls, "model=claude-fable-5[1m]") || containsString(calls, "model=claude-fable-5[1m][max]") {
		t.Fatalf("undiscovered base row routed incorrectly: %#v", calls)
	}
}

func TestClaudeProviderSessionAdoptionAndHeartbeatForwarding(t *testing.T) {
	manager, events := newFakeManager(t, "claude-effort", Options{
		RSSSampleInterval: time.Hour,
		Provider:          ProviderConfig{ID: "claude"},
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := manager.NewSession(ctx, SessionOptions{TabID: "fork-adopt-tab", ChatID: "fork-adopt-chat"})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	job, err := manager.StartJob(ctx, JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID,
		ChatID: "fork-adopt-chat", TabID: "fork-adopt-tab",
		Prompt: "[fake:claude-updates] go",
	})
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	events.waitJobEnd(t, jobID(job), 5*time.Second)

	var heartbeat map[string]any
	for _, event := range events.snapshot() {
		if event.channel != "job:event" {
			continue
		}
		payload, _ := event.payload.(map[string]any)
		inner, _ := payload["event"].(map[string]any)
		if asString(inner["kind"]) == "turn-heartbeat" {
			heartbeat = inner
		}
	}
	if heartbeat == nil || asString(heartbeat["phase"]) != "thinking" {
		t.Fatalf("turn-heartbeat job event missing or wrong: %#v", heartbeat)
	}

	binding, ok := manager.nativeSessions.get("fork-adopt-tab", "fork-adopt-chat", "claude")
	if !ok || binding.ProviderSessionID != session.SessionID+"-forked-1" {
		t.Fatalf("provider session not adopted: ok=%v binding=%#v", ok, binding)
	}
}

func TestT1ColdBridgePreservesUndiscoveredEffortSelection(t *testing.T) {
	manager, _ := newFakeManager(t, "claude-cold-effort", Options{
		RSSSampleInterval: time.Hour,
		Provider:          ProviderConfig{ID: "claude"},
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := manager.NewSession(ctx, SessionOptions{TabID: "cold-effort-tab", ChatID: "cold-effort-chat"})
	if err != nil {
		t.Fatalf("new cold session: %v", err)
	}
	requested := "claude-fable-5[1m][high]"
	result, err := manager.SetModel(ctx, session.SessionID, requested)
	if err != nil {
		t.Fatalf("set cold composite: %v", err)
	}
	if result["currentModelId"] != requested || result["appliedModelId"] != "claude-fable-5[1m]" {
		t.Fatalf("cold set-model result = %#v", result)
	}
	if got := nativeBindingModel(t, manager, "cold-effort-tab", "cold-effort-chat", "claude"); got != requested {
		t.Fatalf("native persisted model = %q, want %q", got, requested)
	}

	bridge := manager.bridgeForSession(session.SessionID, SessionOptions{SessionID: session.SessionID})
	if bridge == nil {
		t.Fatal("session bridge missing")
	}
	bridge.applyConfigOptionsForSession(session.SessionID, fakeClaudeEffortConfigOptions("claude-fable-5[1m]", "default", "default"), false, true)
	result, err = manager.SetModel(ctx, session.SessionID, requested)
	if err != nil {
		t.Fatalf("retry composite after discovery: %v", err)
	}
	if result["currentModelId"] != requested || result["appliedModelId"] != requested {
		t.Fatalf("retry set-model result = %#v, want durable and applied %q", result, requested)
	}
}

func TestT2AuthoritativeEffortDowngradePersistsAndLogs(t *testing.T) {
	var logs []string
	manager, _ := newFakeManager(t, "claude-effort", Options{
		RSSSampleInterval: time.Hour,
		Provider:          ProviderConfig{ID: "claude"},
		Logf: func(message string, fields map[string]any) {
			if message == "model selection downgraded" {
				logs = append(logs, fmt.Sprintf("%s original=%v applied=%v reason=%v", message, fields["original"], fields["applied"], fields["reason"]))
			}
		},
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := manager.NewSession(ctx, SessionOptions{TabID: "known-downgrade-tab", ChatID: "known-downgrade-chat"})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	bridge := manager.bridgeForSession(session.SessionID, SessionOptions{SessionID: session.SessionID})
	if bridge == nil {
		t.Fatal("session bridge missing")
	}
	bridge.mu.Lock()
	bridge.axisEffortsByModel["haiku"] = []string{}
	bridge.mu.Unlock()

	result, err := manager.SetModel(ctx, session.SessionID, "haiku[high]")
	if err != nil {
		t.Fatalf("set unsupported known effort: %v", err)
	}
	if result["currentModelId"] != "haiku" || result["appliedModelId"] != "haiku" || result["modelWritebackReason"] != "unsupported-effort" {
		t.Fatalf("known downgrade result = %#v", result)
	}
	if got := nativeBindingModel(t, manager, "known-downgrade-tab", "known-downgrade-chat", "claude"); got != "haiku" {
		t.Fatalf("native persisted downgrade = %q, want haiku", got)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "original=haiku[high]") || !strings.Contains(logs[0], "applied=haiku") || !strings.Contains(logs[0], "reason=unsupported-effort") {
		t.Fatalf("downgrade logs = %#v", logs)
	}
}

func TestT3AdapterSideModelChangeCapturesStoredControls(t *testing.T) {
	manager, _ := newFakeManager(t, "claude-effort", Options{
		RSSSampleInterval: time.Hour,
		Provider:          ProviderConfig{ID: "claude"},
	})
	t.Cleanup(func() { manager.Reset() })
	refreshes := make(chan map[string]any, 1)
	manager.SetSessionRefreshFunc(func(payload map[string]any) { refreshes <- payload })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := manager.NewSession(ctx, SessionOptions{TabID: "adapter-model-tab", ChatID: "adapter-model-chat"})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	bridge := manager.bridgeForSession(session.SessionID, SessionOptions{SessionID: session.SessionID})
	if bridge == nil {
		t.Fatal("session bridge missing")
	}
	bridge.applyConfigOptionsForSession(session.SessionID, fakeClaudeEffortConfigOptions("claude-fable-5[1m]", "default", "high"), true, false)

	var evt map[string]any
	select {
	case evt = <-refreshes:
	case <-time.After(2 * time.Second):
		t.Fatal("adapter model correction did not reach the immediate refresh coordinator hook")
	}
	if evt["action"] != "session-refresh" || evt["modelId"] != "claude-fable-5[1m][high]" || evt["tabId"] != "adapter-model-tab" || evt["chatId"] != "adapter-model-chat" {
		t.Fatalf("adapter capture event = %#v", evt)
	}
	if got := nativeBindingModel(t, manager, "adapter-model-tab", "adapter-model-chat", "claude"); got != "claude-fable-5[1m][high]" {
		t.Fatalf("adapter-side stored model = %q", got)
	}
	controls, err := bridge.ensureSessionControls(ctx, session.SessionID, "claude-fable-5[1m][high]", "")
	if err != nil {
		t.Fatalf("ensure captured model: %v", err)
	}
	if controls.AppliedModelID != "claude-fable-5[1m][high]" || controls.CurrentModelID != "claude-fable-5[1m][high]" {
		t.Fatalf("ensure after adapter capture = %#v", controls)
	}
}

func TestT5LiteralBracketedModelRoundTripsAfterResumeDiscovery(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "cold-roundtrip-calls.log")
	t.Setenv("WORKASS_FAKE_ACP_CONFIG_LOG", logPath)
	manager, events := newFakeManager(t, "claude-cold-effort", Options{
		RSSSampleInterval: time.Hour,
		Provider:          ProviderConfig{ID: "claude"},
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := manager.NewSession(ctx, SessionOptions{TabID: "literal-roundtrip-tab", ChatID: "literal-roundtrip-chat"})
	if err != nil {
		t.Fatalf("new cold session: %v", err)
	}
	requested := "claude-fable-5[1m][high]"
	if result, err := manager.SetModel(ctx, session.SessionID, requested); err != nil || result["currentModelId"] != requested {
		t.Fatalf("cold preserved selection result=%#v err=%v", result, err)
	}
	bridge := manager.bridgeForSession(session.SessionID, SessionOptions{SessionID: session.SessionID})
	if bridge == nil {
		t.Fatal("session bridge missing")
	}
	bridge.applyConfigOptionsForSession(session.SessionID, fakeClaudeEffortConfigOptions("claude-fable-5[1m]", "default", "default"), false, true)

	job, err := manager.StartJob(ctx, JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, TabID: "literal-roundtrip-tab", ChatID: "literal-roundtrip-chat",
		ProviderID: "claude", ModelID: requested, Prompt: "round trip literal bracketed model",
	})
	if err != nil {
		t.Fatalf("start literal roundtrip turn: %v", err)
	}
	end := events.waitJobEnd(t, jobID(job), 4*time.Second)
	assertJobStatus(t, end, "done", 0, "end_turn")
	result := jobFromEnd(end)["result"].(string)
	if !strings.Contains(result, `model "claude-fable-5[1m][high]"`) {
		t.Fatalf("runtime banner did not preserve literal composite: %q", result)
	}
	calls := readConfigCalls(t, logPath)
	if !containsString(calls, "model=claude-fable-5[1m]") || !containsString(calls, "effort=high") || containsString(calls, "model=claude-fable-5[1m][high]") {
		t.Fatalf("literal composite routing calls = %#v", calls)
	}
}

func TestT6NewSessionConvergesNativeBindingModelWithoutRendererReapply(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "native-startup-controls.log")
	t.Setenv("WORKASS_FAKE_ACP_CONFIG_LOG", logPath)
	stateDir := t.TempDir()
	manager, _ := newFakeManager(t, "claude-cold-effort", Options{
		StateDir:          stateDir,
		RSSSampleInterval: time.Hour,
		Provider:          ProviderConfig{ID: "claude"},
	})
	t.Cleanup(func() { manager.Reset() })
	requested := "claude-fable-5[1m][xhigh]"
	if err := manager.nativeSessions.put(nativeSessionBinding{
		TabID: "native-controls-tab", ChatID: "native-controls-chat", ProviderID: "claude",
		SessionID: "native-controls-provider-session", ModelID: requested,
		HistoryHash: historyDigest(nil), HistoryVersion: nativeHistoryDigestVersion, ResumeSafe: true,
	}); err != nil {
		t.Fatalf("seed native binding: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := manager.NewSession(ctx, SessionOptions{
		TabID: "native-controls-tab", ChatID: "native-controls-chat", ProviderID: "claude",
	})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if stringPointer(info.CurrentModelID) != requested {
		t.Fatalf("startup current model = %q, want durable %q", stringPointer(info.CurrentModelID), requested)
	}
	live, ok := manager.LiveSession(info.SessionID)
	if !ok {
		t.Fatal("startup session is not live")
	}
	if stringPointer(live.Info.CurrentModelID) != requested {
		t.Fatalf("live session current model = %q, want durable %q", stringPointer(live.Info.CurrentModelID), requested)
	}
	if got := nativeBindingModel(t, manager, "native-controls-tab", "native-controls-chat", "claude"); got != requested {
		t.Fatalf("native binding model = %q, want %q", got, requested)
	}
	calls := readConfigCalls(t, logPath)
	if !containsString(calls, "model=claude-fable-5[1m]") {
		t.Fatalf("startup did not apply durable model selection, calls = %#v", calls)
	}
	if containsString(calls, "model=claude-fable-5[1m][xhigh]") {
		t.Fatalf("startup sent composite to model axis, calls = %#v", calls)
	}
}

func nativeBindingModel(t *testing.T, manager *Manager, tabID, chatID, providerID string) string {
	t.Helper()
	if manager.nativeSessions == nil {
		t.Fatal("native session ledger missing")
	}
	binding, ok := manager.nativeSessions.get(tabID, chatID, providerID)
	if !ok {
		t.Fatalf("native binding missing for tab=%s chat=%s provider=%s", tabID, chatID, providerID)
	}
	return binding.ModelID
}

func TestSplitEffortSuffix(t *testing.T) {
	efforts := []string{"low", "medium", "high", "xhigh", "max"}
	cases := []struct {
		id         string
		wantBase   string
		wantEffort string
		wantSplit  bool
	}{
		{"opus[1m][high]", "opus[1m]", "high", true},
		{"claude-fable-5[1m][max]", "claude-fable-5[1m]", "max", true},
		{"opus[1m]", "opus[1m]", "", false},               // context variant, not an effort
		{"sonnet", "sonnet", "", false},                   // bare model
		{"opus[1m][turbo]", "opus[1m][turbo]", "", false}, // unknown suffix stays whole
	}
	for _, tc := range cases {
		base, effort, split := splitEffortSuffix(tc.id, efforts)
		if base != tc.wantBase || effort != tc.wantEffort || split != tc.wantSplit {
			t.Fatalf("splitEffortSuffix(%q) = (%q,%q,%v), want (%q,%q,%v)", tc.id, base, effort, split, tc.wantBase, tc.wantEffort, tc.wantSplit)
		}
	}
	// With no known effort vocabulary (Codex/mock) the id is always left whole.
	if base, effort, split := splitEffortSuffix("gpt-5[high]", nil); base != "gpt-5[high]" || effort != "" || split {
		t.Fatalf("splitEffortSuffix with no efforts mutated id: (%q,%q,%v)", base, effort, split)
	}
}

func readConfigCalls(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config-call log: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func TestFakeInitTimeout(t *testing.T) {
	manager, _ := newFakeManager(t, "init-hang", Options{InitTimeout: 40 * time.Millisecond})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := manager.InitializeBridge(ctx, "hang-tab")
	if err == nil || !strings.Contains(err.Error(), "ACP timeout: initialize") {
		t.Fatalf("initialize error = %v", err)
	}
}

func TestFakeStderrTailCapturedOnCrash(t *testing.T) {
	var logsMu sync.Mutex
	var logs []map[string]any
	manager, _ := newFakeManager(t, "crash-stderr", Options{
		InitTimeout:     500 * time.Millisecond,
		StderrTailBytes: 24,
		Logf: func(message string, fields map[string]any) {
			logsMu.Lock()
			defer logsMu.Unlock()
			cp := map[string]any{"message": message}
			for k, v := range fields {
				cp[k] = v
			}
			logs = append(logs, cp)
		},
	})
	t.Cleanup(func() { manager.Reset() })
	_, _ = manager.InitializeBridge(context.Background(), "crash-tab")

	deadline := time.After(2 * time.Second)
	for {
		logsMu.Lock()
		for _, entry := range logs {
			if entry["message"] == "acp engine exited" && entry["stderrTail"] != nil {
				tail := entry["stderrTail"].(string)
				logsMu.Unlock()
				if len(tail) > 24 {
					t.Fatalf("stderr tail length = %d tail=%q", len(tail), tail)
				}
				if !strings.HasSuffix("abcdefghijklmnopqrstuvwxyz0123456789", tail) {
					t.Fatalf("stderr tail = %q", tail)
				}
				return
			}
		}
		logsMu.Unlock()
		select {
		case <-deadline:
			t.Fatalf("no stderr tail log: %#v", logs)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestFakePromptSerializationQueuesPerBridge(t *testing.T) {
	manager, _ := newFakeManager(t, "serialize", Options{})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	first, err := manager.NewSession(ctx, SessionOptions{TabID: "serial-tab"})
	if err != nil {
		t.Fatalf("new first session: %v", err)
	}
	second, err := manager.NewSession(ctx, SessionOptions{TabID: "serial-tab"})
	if err != nil {
		t.Fatalf("new second session: %v", err)
	}
	bridge := manager.BridgeForKey("serial-tab")

	errCh := make(chan error, 2)
	go func() {
		_, err := bridge.Prompt(ctx, first.SessionID, "one")
		errCh <- err
	}()
	go func() {
		_, err := bridge.Prompt(ctx, second.SessionID, "two")
		errCh <- err
	}()
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("serialized prompt %d failed: %v", i+1, err)
		}
	}
}

func TestFakeHistoryReplayExactlyOnce(t *testing.T) {
	manager, events := newFakeManager(t, "echo-prompt", Options{})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "history-tab")
	history := []any{map[string]any{"role": "user", "content": "earlier request", "at": "2026-07-10T00:00:00Z"}}

	first, err := manager.StartJob(context.Background(), JobStartOptions{Kind: "app-chat", SessionID: session.SessionID, TabID: "history-tab", Prompt: "first live request", History: history})
	if err != nil {
		t.Fatalf("first job: %v", err)
	}
	firstEnd := events.waitJobEnd(t, jobID(first), 2*time.Second)
	if result := jobFromEnd(firstEnd)["result"].(string); !strings.Contains(result, "User: earlier request") || !strings.Contains(result, "User request:\nfirst live request") {
		t.Fatalf("first result did not replay history once: %q", result)
	}

	second, err := manager.StartJob(context.Background(), JobStartOptions{Kind: "app-chat", SessionID: session.SessionID, TabID: "history-tab", Prompt: "second live request", History: history})
	if err != nil {
		t.Fatalf("second job: %v", err)
	}
	secondEnd := events.waitJobEnd(t, jobID(second), 2*time.Second)
	if result := jobFromEnd(secondEnd)["result"].(string); strings.Contains(result, "User: earlier request") ||
		!strings.Contains(result, `Active Workass runtime for this turn: provider "custom"`) ||
		!strings.Contains(result, "User request:\nsecond live request") {
		t.Fatalf("second result should not replay history: %q", result)
	}
}

func TestLifecyclePinnedNeverReapedTinyTTL(t *testing.T) {
	manager, events := newFakeManager(t, "slow-prompt", Options{
		HibernateTTL:           20 * time.Millisecond,
		LifecycleCheckInterval: 10 * time.Millisecond,
		RSSSampleInterval:      time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "pinned-tab")

	job := startAppChatJob(t, manager, session.SessionID, "pinned-tab", "slow pinned turn")
	_ = waitProcState(t, manager, StateActive, 500*time.Millisecond)
	time.Sleep(70 * time.Millisecond)
	if procHasState(manager, StateHibernated) {
		t.Fatalf("active prompt was hibernated under tiny TTL: %#v", manager.Processes())
	}
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 2*time.Second), "done", 0, "end_turn")
}

func TestLifecycleIdleReapFires(t *testing.T) {
	manager, events := newFakeManager(t, "echo-prompt", Options{
		HibernateTTL:           25 * time.Millisecond,
		LifecycleCheckInterval: 10 * time.Millisecond,
		RSSSampleInterval:      time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "idle-reap-tab")
	job := startAppChatJob(t, manager, session.SessionID, "idle-reap-tab", "finish and idle")
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 2*time.Second), "done", 0, "end_turn")
	_ = waitProcState(t, manager, StateHibernated, time.Second)
}

func TestWorkspaceMoveInvalidatesEveryBindingAndFreshSessionReplaysCanonicalHistory(t *testing.T) {
	manager, events := newFakeManager(t, "echo-prompt", Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	oldCWD := t.TempDir()
	targetCWD := t.TempDir()
	tabID, chatID := "workspace-move-tab", "workspace-move-chat"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	old, err := manager.NewSession(ctx, SessionOptions{TabID: tabID, ChatID: chatID, CWD: oldCWD})
	if err != nil {
		t.Fatalf("new old-cwd session: %v", err)
	}
	// A hibernated binding from another provider must not survive a workspace
	// move and reappear on a later provider switch.
	other := nativeSessionBinding{
		TabID: tabID, ChatID: chatID, ProviderID: "other-provider", SessionID: "other-native-session", CWD: oldCWD,
		HistoryHash: historyDigest(nil), HistoryVersion: nativeHistoryDigestVersion, Generation: 1, ResumeSafe: true,
	}
	if err := manager.nativeSessions.put(other); err != nil {
		t.Fatalf("seed other-provider binding: %v", err)
	}
	commits := 0
	committed, err := manager.InvalidateChatWorkspace(ctx, old.SessionID, SessionOptions{
		TabID: tabID, ChatID: chatID, CWD: targetCWD,
	}, func() error { commits++; return nil })
	if err != nil || !committed || commits != 1 {
		t.Fatalf("invalidate workspace committed=%v commits=%d err=%v", committed, commits, err)
	}
	if _, ok := manager.LiveSession(old.SessionID); ok {
		t.Fatalf("old live session survived workspace invalidation")
	}
	if _, ok := manager.nativeSessions.get(tabID, chatID, old.ProviderID); ok {
		t.Fatalf("current-provider native binding survived workspace invalidation")
	}
	if _, ok := manager.nativeSessions.get(tabID, chatID, other.ProviderID); ok {
		t.Fatalf("other-provider native binding survived workspace invalidation")
	}
	if _, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: old.SessionID, TabID: tabID, ChatID: chatID, CWD: oldCWD, Prompt: "stale",
	}); err == nil || !strings.Contains(err.Error(), "invalidada") {
		t.Fatalf("stale session start err=%v", err)
	}

	fresh, err := manager.NewSession(ctx, SessionOptions{TabID: tabID, ChatID: chatID, CWD: targetCWD, ProviderID: old.ProviderID})
	if err != nil {
		t.Fatalf("new target-cwd session: %v", err)
	}
	if fresh.SessionID == old.SessionID || !sameFilesystemPath(fresh.CWD, targetCWD) {
		t.Fatalf("fresh session=%+v old=%s target=%s", fresh, old.SessionID, targetCWD)
	}
	history := []any{map[string]any{"role": "user", "content": "canonical earlier turn", "at": "2026-07-13T00:00:00Z"}}
	job, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: fresh.SessionID, TabID: tabID, ChatID: chatID, CWD: targetCWD,
		Prompt: "run after move", History: history,
	})
	if err != nil {
		t.Fatalf("start after move: %v", err)
	}
	end := events.waitJobEnd(t, jobID(job), 2*time.Second)
	assertJobStatus(t, end, "done", 0, "end_turn")
	result := jobFromEnd(end)["result"].(string)
	if !strings.Contains(result, "User: canonical earlier turn") || !strings.Contains(result, "User request:\nrun after move") {
		t.Fatalf("fresh target session did not canonical-replay history: %q", result)
	}
}

func TestWorkspaceMoveRejectsActiveTurnBeforeCommit(t *testing.T) {
	manager, events := newFakeManager(t, "slow-prompt", Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	tabID, chatID := "workspace-active-tab", "workspace-active-chat"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: tabID, ChatID: chatID, CWD: t.TempDir()})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	job, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, TabID: tabID, ChatID: chatID, CWD: session.CWD, Prompt: "slow active",
	})
	if err != nil {
		t.Fatalf("start active job: %v", err)
	}
	commits := 0
	committed, moveErr := manager.InvalidateChatWorkspace(ctx, session.SessionID, SessionOptions{
		TabID: tabID, ChatID: chatID, CWD: t.TempDir(),
	}, func() error { commits++; return nil })
	if moveErr == nil || committed || commits != 0 || !strings.Contains(moveErr.Error(), "respuesta en curso") {
		t.Fatalf("active move committed=%v commits=%d err=%v", committed, commits, moveErr)
	}
	if live, ok := manager.LiveSession(session.SessionID); !ok || live.Info.SessionID != session.SessionID {
		t.Fatalf("active rejection disturbed live session: %+v ok=%v", live, ok)
	}
	manager.CancelJob(jobID(job))
	events.waitJobEnd(t, jobID(job), 2*time.Second)
}

func TestWorkspaceMoveWriteFailureKeepsLiveAndNativeBinding(t *testing.T) {
	manager, _ := newFakeManager(t, "echo-prompt", Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	tabID, chatID := "workspace-write-fail-tab", "workspace-write-fail-chat"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{
		TabID: tabID, ChatID: chatID, CWD: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	committed, moveErr := manager.InvalidateChatWorkspace(ctx, session.SessionID, SessionOptions{
		TabID: tabID, ChatID: chatID, CWD: t.TempDir(),
	}, func() error { return fmt.Errorf("forced durable write failure") })
	if moveErr == nil || committed || !strings.Contains(moveErr.Error(), "forced durable write failure") {
		t.Fatalf("write failure committed=%v err=%v", committed, moveErr)
	}
	if live, ok := manager.LiveSession(session.SessionID); !ok || live.Info.SessionID != session.SessionID {
		t.Fatalf("write failure disturbed live session: %+v ok=%v", live, ok)
	}
	if binding, ok := manager.nativeSessions.get(tabID, chatID, session.ProviderID); !ok || binding.SessionID != session.SessionID {
		t.Fatalf("write failure disturbed native binding: %+v ok=%v", binding, ok)
	}
}

func TestLifecycleTraceResurrectReplayExactlyOnce(t *testing.T) {
	manager, events := newFakeManager(t, "slow-prompt", Options{
		HibernateTTL:           80 * time.Millisecond,
		LifecycleCheckInterval: 10 * time.Millisecond,
		RSSSampleInterval:      time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "resurrect-tab")
	t.Log("trace lifecycle spawn -> warm")
	history := []any{map[string]any{"role": "user", "content": "earlier request", "at": "2026-07-10T00:00:00Z"}}

	first := startAppChatJobWithHistory(t, manager, session.SessionID, "resurrect-tab", "first live request", history)
	active := waitProcState(t, manager, StateActive, 500*time.Millisecond)
	t.Logf("trace lifecycle active(pinned) pid=%v rssKb=%v", active["pid"], active["rssKb"])
	firstEnd := events.waitJobEnd(t, jobID(first), 2*time.Second)
	assertJobStatus(t, firstEnd, "done", 0, "end_turn")
	if result := jobFromEnd(firstEnd)["result"].(string); !strings.Contains(result, "User: earlier request") || !strings.Contains(result, "User request:\nfirst live request") {
		t.Fatalf("first result did not replay history once: %q", result)
	}
	idle := waitProcState(t, manager, StateIdle, 500*time.Millisecond)
	t.Logf("trace lifecycle idle lastLine=%q", idle["lastLine"])
	hibernated := waitProcState(t, manager, StateHibernated, time.Second)
	t.Logf("trace lifecycle hibernated pid=%v", hibernated["pid"])

	second := startAppChatJobWithHistory(t, manager, session.SessionID, "resurrect-tab", "second live request", history)
	replaced := events.waitChannel(t, "chat:session-replaced", 2*time.Second).payload.(map[string]any)
	replacement := replacedSessionInfo(t, replaced["session"])
	if replaced["oldSessionId"] != session.SessionID || replacement.SessionID == "" || replacement.SessionID == session.SessionID {
		t.Fatalf("replacement payload = %#v", replaced)
	}
	t.Logf("trace lifecycle resurrect old=%s new=%s", session.SessionID, replacement.SessionID)
	active = waitProcState(t, manager, StateActive, 500*time.Millisecond)
	t.Logf("trace lifecycle active(pinned) resurrected pid=%v", active["pid"])
	secondEnd := events.waitJobEnd(t, jobID(second), 2*time.Second)
	assertJobStatus(t, secondEnd, "done", 0, "end_turn")
	if result := jobFromEnd(secondEnd)["result"].(string); !strings.Contains(result, "User: earlier request") || !strings.Contains(result, "User request:\nsecond live request") {
		t.Fatalf("resurrected result did not seed exactly once: %q", result)
	}

	third := startAppChatJobWithHistory(t, manager, replacement.SessionID, "resurrect-tab", "third live request", history)
	thirdEnd := events.waitJobEnd(t, jobID(third), 2*time.Second)
	assertJobStatus(t, thirdEnd, "done", 0, "end_turn")
	if result := jobFromEnd(thirdEnd)["result"].(string); strings.Contains(result, "User: earlier request") ||
		!strings.Contains(result, `Active Workass runtime for this turn: provider "custom"`) ||
		!strings.Contains(result, "User request:\nthird live request") {
		t.Fatalf("third result should not replay history again: %q", result)
	}
}

func TestFailedSpareWarmTripsCircuitBreakerInsteadOfRespawning(t *testing.T) {
	var mu sync.Mutex
	exits := 0
	blocks := 0
	blocked := make(chan struct{}, 1)
	manager, _ := newFakeManager(t, "crash-session-new", Options{
		SpareSessions:      2,
		SpareCheckInterval: 20 * time.Millisecond,
		InitTimeout:        150 * time.Millisecond,
		RSSSampleInterval:  time.Hour,
		Logf: func(message string, _ map[string]any) {
			mu.Lock()
			defer mu.Unlock()
			switch message {
			case "acp engine exited":
				exits++
			case "acp spare warming disabled":
				blocks++
				select {
				case blocked <- struct{}{}:
				default:
				}
			}
		},
	})
	t.Cleanup(func() { manager.Reset() })

	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("failed spare warm did not trip its circuit breaker")
	}
	// More than five retry intervals must pass without another process launch.
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	gotExits, gotBlocks := exits, blocks
	mu.Unlock()
	if gotExits != 1 || gotBlocks != 1 {
		t.Fatalf("failed spare launches=%d circuit logs=%d, want one bounded attempt", gotExits, gotBlocks)
	}
	manager.mu.Lock()
	providerID := manager.defaultProviderID
	isBlocked := manager.spareBlocked[providerID]
	warming := manager.spareWarming[providerID]
	spares := manager.spareCountLocked(providerID)
	manager.mu.Unlock()
	if !isBlocked || warming != 0 || spares != 0 {
		t.Fatalf("spare breaker blocked=%v warming=%d spares=%d", isBlocked, warming, spares)
	}
}

func TestLifecycleIdleCrashResurrectsOnNextUse(t *testing.T) {
	manager, events := newFakeManager(t, "echo-prompt", Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "idle-crash-tab")
	first := startAppChatJob(t, manager, session.SessionID, "idle-crash-tab", "first idle crash turn")
	assertJobStatus(t, events.waitJobEnd(t, jobID(first), 2*time.Second), "done", 0, "end_turn")
	_ = waitProcState(t, manager, StateIdle, 500*time.Millisecond)

	bridge := manager.bridgeForSession(session.SessionID, SessionOptions{SessionID: session.SessionID, TabID: "idle-crash-tab"})
	if bridge == nil {
		t.Fatalf("bridge missing before idle crash")
	}
	bridge.mu.Lock()
	child := bridge.child
	bridge.mu.Unlock()
	if child == nil || child.Process == nil {
		t.Fatalf("idle child missing")
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill idle child: %v", err)
	}
	_ = waitProcStatus(t, manager, "failed", time.Second)
	t.Log("trace idle crash closed bridge without mid-turn recovery")

	history := []any{map[string]any{"role": "user", "content": "pre-crash history", "at": "2026-07-10T00:00:00Z"}}
	second := startAppChatJobWithHistory(t, manager, session.SessionID, "idle-crash-tab", "after idle crash", history)
	replaced := events.waitChannel(t, "chat:session-replaced", 2*time.Second).payload.(map[string]any)
	replacement := replacedSessionInfo(t, replaced["session"])
	if replaced["oldSessionId"] != session.SessionID || replacement.SessionID == "" || replacement.SessionID == session.SessionID {
		t.Fatalf("idle crash replacement payload = %#v", replaced)
	}
	secondEnd := events.waitJobEnd(t, jobID(second), 2*time.Second)
	assertJobStatus(t, secondEnd, "done", 0, "end_turn")
	if result := jobFromEnd(secondEnd)["result"].(string); !strings.Contains(result, "User: pre-crash history") || !strings.Contains(result, "User request:\nafter idle crash") {
		t.Fatalf("idle crash resurrect result = %q", result)
	}
	events.expectNoChannel(t, "chat:engine-recovered", 100*time.Millisecond)
	t.Logf("trace idle crash resurrected old=%s new=%s", session.SessionID, replacement.SessionID)
}

func TestLifecycleRSSSampledForLiveChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := sampleProcessRSS(ctx, os.Getpid()); err != nil {
		t.Skipf("RSS sampling unavailable in this environment: %v", err)
	}
	manager, _ := newFakeManager(t, "echo-prompt", Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	_ = newFakeSession(t, manager, "rss-tab")

	manager.SampleRSS(ctx)
	proc := firstProc(t, manager)
	rss, _ := proc["rssKb"].(int)
	if rss <= 0 {
		t.Fatalf("rssKb = %v in proc %#v", proc["rssKb"], proc)
	}
}

func TestLifecycleRecycleAtNextIdle(t *testing.T) {
	manager, events := newFakeManager(t, "slow-prompt", Options{
		HibernateTTL:           time.Hour,
		LifecycleCheckInterval: time.Hour,
		RSSSampleInterval:      time.Hour,
		EngineMaxAge:           time.Millisecond,
	})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "recycle-tab")
	job := startAppChatJob(t, manager, session.SessionID, "recycle-tab", "slow recycle turn")
	_ = waitProcState(t, manager, StateActive, 500*time.Millisecond)

	time.Sleep(5 * time.Millisecond)
	manager.SweepLifecycle()
	if procHasState(manager, StateHibernated) {
		t.Fatalf("recycle hibernated mid-prompt: %#v", manager.Processes())
	}
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 2*time.Second), "done", 0, "end_turn")
	_ = waitProcState(t, manager, StateHibernated, time.Second)
}

func TestLifecycleRaceReapAbortedByArrivingPrompt(t *testing.T) {
	manager, events := newFakeManager(t, "slow-prompt", Options{
		HibernateTTL:           20 * time.Millisecond,
		LifecycleCheckInterval: time.Hour,
		RSSSampleInterval:      time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "race-tab")
	first := startAppChatJob(t, manager, session.SessionID, "race-tab", "first race turn")
	assertJobStatus(t, events.waitJobEnd(t, jobID(first), 2*time.Second), "done", 0, "end_turn")
	_ = waitProcState(t, manager, StateIdle, 500*time.Millisecond)
	time.Sleep(30 * time.Millisecond)

	bridge := manager.bridgeForSession(session.SessionID, SessionOptions{SessionID: session.SessionID})
	if bridge == nil {
		t.Fatalf("bridge missing")
	}
	bridge.mu.Lock()
	lastActivity := bridge.lastActivity
	bridge.mu.Unlock()

	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan bool, 1)
	go func() {
		done <- manager.hibernateBridgeIfEligible(bridge, hibernateReasonIdleTTL, manager.opts.HibernateTTL, lastActivity, time.Now(), func() {
			close(entered)
			<-release
		})
	}()
	<-entered
	second := startAppChatJob(t, manager, session.SessionID, "race-tab", "arriving prompt wins")
	t.Log("trace race prompt arrived while reap waited before write-lock recheck")
	close(release)
	if hibernated := <-done; hibernated {
		t.Fatalf("reaper hibernated after prompt arrived: %#v", manager.Processes())
	}
	t.Log("trace race reap ABORTED by arriving prompt")
	assertJobStatus(t, events.waitJobEnd(t, jobID(second), 2*time.Second), "done", 0, "end_turn")
}

func newFakeManager(t *testing.T, mode string, overrides Options) (*Manager, *eventCollector) {
	t.Helper()
	events := newEventCollector()
	root := repoRoot(t)
	opts := Options{
		RootDir:              root,
		StateDir:             filepath.Join(t.TempDir(), "state"),
		Provider:             ProviderConfig{Command: os.Args[0], Args: []string{"-test.run=TestFakeACPHelper", "--"}, CWD: root, Env: map[string]string{"WORKASS_FAKE_ACP": "1", "WORKASS_FAKE_ACP_MODE": mode}, Label: "Fake ACP"},
		Broadcast:            events.Broadcast,
		InitTimeout:          2 * time.Second,
		PermissionTimeout:    2 * time.Second,
		StdoutFlushInterval:  10 * time.Millisecond,
		ThoughtFlushInterval: 10 * time.Millisecond,
	}
	if overrides.InitTimeout != 0 {
		opts.InitTimeout = overrides.InitTimeout
	}
	if overrides.StateDir != "" {
		opts.StateDir = overrides.StateDir
	}
	if overrides.PermissionTimeout != 0 {
		opts.PermissionTimeout = overrides.PermissionTimeout
	}
	if overrides.StderrTailBytes != 0 {
		opts.StderrTailBytes = overrides.StderrTailBytes
	}
	if overrides.HibernateTTL != 0 {
		opts.HibernateTTL = overrides.HibernateTTL
	}
	if overrides.LifecycleCheckInterval != 0 {
		opts.LifecycleCheckInterval = overrides.LifecycleCheckInterval
	}
	if overrides.RSSSampleInterval != 0 {
		opts.RSSSampleInterval = overrides.RSSSampleInterval
	}
	if overrides.EngineMaxAge != 0 {
		opts.EngineMaxAge = overrides.EngineMaxAge
	}
	if overrides.EngineMaxRSSKB != 0 {
		opts.EngineMaxRSSKB = overrides.EngineMaxRSSKB
	}
	if overrides.SpareSessions != 0 {
		opts.SpareSessions = overrides.SpareSessions
	}
	if overrides.SpareTTL != 0 {
		opts.SpareTTL = overrides.SpareTTL
	}
	if overrides.SpareCheckInterval != 0 {
		opts.SpareCheckInterval = overrides.SpareCheckInterval
	}
	if overrides.CompactionEnabled {
		opts.CompactionEnabled = overrides.CompactionEnabled
	}
	if overrides.CompactionThresholdPct != 0 {
		opts.CompactionThresholdPct = overrides.CompactionThresholdPct
	}
	if overrides.CompactionKeepLastTurns != 0 {
		opts.CompactionKeepLastTurns = overrides.CompactionKeepLastTurns
	}
	if overrides.CrashRecoveryBackoff != 0 {
		opts.CrashRecoveryBackoff = overrides.CrashRecoveryBackoff
	}
	if overrides.CrashRecoveryWindow != 0 {
		opts.CrashRecoveryWindow = overrides.CrashRecoveryWindow
	}
	if overrides.ProviderDetectionRetryBackoffs != nil {
		opts.ProviderDetectionRetryBackoffs = append([]time.Duration(nil), overrides.ProviderDetectionRetryBackoffs...)
	}
	if overrides.Logf != nil {
		opts.Logf = overrides.Logf
	}
	if overrides.Provider.ID != "" {
		opts.Provider.ID = overrides.Provider.ID
	}
	if len(overrides.Provider.Env) > 0 {
		opts.Provider.Env = make(map[string]string, len(opts.Provider.Env)+len(overrides.Provider.Env))
		for key, value := range map[string]string{"WORKASS_FAKE_ACP": "1", "WORKASS_FAKE_ACP_MODE": mode} {
			opts.Provider.Env[key] = value
		}
		for key, value := range overrides.Provider.Env {
			opts.Provider.Env[key] = value
		}
	}
	return NewManager(opts), events
}

func newFakeSession(t *testing.T, manager *Manager, tabID string) SessionInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: tabID})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	return session
}

func newMockSession(t *testing.T, manager *Manager, tabID string) SessionInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: tabID})
	if err != nil {
		t.Fatalf("new mock session: %v", err)
	}
	return session
}

func startAppChatJob(t *testing.T, manager *Manager, sessionID, tabID, prompt string) map[string]any {
	t.Helper()
	return startAppChatJobWithHistory(t, manager, sessionID, tabID, prompt, nil)
}

func startAppChatJobWithHistory(t *testing.T, manager *Manager, sessionID, tabID, prompt string, history []any) map[string]any {
	t.Helper()
	job, err := manager.StartJob(context.Background(), JobStartOptions{Kind: "app-chat", SessionID: sessionID, ChatID: "chat-" + tabID, TabID: tabID, Prompt: prompt, History: history})
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	// Renderer fixture: desktop/renderer/app.js:4921-4923 reads the job:start reply id
	// and binds it to the assistant message created by app.js:4901-4903's chatId.
	if job["id"] == "" || job["kind"] != "app-chat" || job["status"] != "running" {
		t.Fatalf("job = %#v", job)
	}
	if job["chatId"] != "chat-"+tabID || job["tabId"] != tabID || job["sessionId"] != sessionID {
		t.Fatalf("job routing fields = %#v", job)
	}
	return job
}

func waitProcState(t *testing.T, manager *Manager, state EngineState, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, proc := range manager.Processes() {
			if proc["state"] == string(state) {
				return proc
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for proc state %s; processes=%#v", state, manager.Processes())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitProcStatus(t *testing.T, manager *Manager, status string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, proc := range manager.Processes() {
			if proc["status"] == status {
				return proc
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for proc status %s; processes=%#v", status, manager.Processes())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func procHasState(manager *Manager, state EngineState) bool {
	for _, proc := range manager.Processes() {
		if proc["state"] == string(state) {
			return true
		}
	}
	return false
}

func firstProc(t *testing.T, manager *Manager) map[string]any {
	t.Helper()
	processes := manager.Processes()
	if len(processes) == 0 {
		t.Fatalf("no processes")
	}
	return processes[0]
}

func replacedSessionInfo(t *testing.T, raw any) SessionInfo {
	t.Helper()
	switch x := raw.(type) {
	case SessionInfo:
		return x
	case map[string]any:
		return SessionInfo{SessionID: asString(x["sessionId"])}
	default:
		t.Fatalf("unexpected replacement session payload: %#v", raw)
		return SessionInfo{}
	}
}

func assertCatalogGroup(t *testing.T, groups []CatalogGroup, providerID, status string, wantModels bool) {
	t.Helper()
	group := findCatalogGroup(groups, providerID)
	if group == nil {
		t.Fatalf("catalog missing provider %s in %#v", providerID, groups)
	}
	if group.Status != status {
		t.Fatalf("catalog provider %s status=%s want %s group=%#v", providerID, group.Status, status, *group)
	}
	if wantModels && (len(group.Models) == 0 || len(group.Modes) == 0) {
		t.Fatalf("catalog provider %s missing models/modes: %#v", providerID, *group)
	}
	if !wantModels && status == providerStatusError && group.Error == "" {
		t.Fatalf("catalog provider %s missing error: %#v", providerID, *group)
	}
}

func assertCatalogEfforts(t *testing.T, group *CatalogGroup, modelID string, want []string) {
	t.Helper()
	if group == nil {
		t.Fatalf("catalog group missing for model %s", modelID)
	}
	model := findCatalogModel(group.Models, modelID)
	if model == nil {
		t.Fatalf("catalog model %s missing in %#v", modelID, group.Models)
	}
	if !stringSlicesEqual(model.Efforts, want) {
		t.Fatalf("catalog model %s efforts = %#v, want %#v in %#v", modelID, model.Efforts, want, group.Models)
	}
}

func findCatalogModel(models []Model, modelID string) *Model {
	for i := range models {
		if models[i].ModelID == modelID {
			return &models[i]
		}
	}
	return nil
}

func findCatalogGroup(groups []CatalogGroup, providerID string) *CatalogGroup {
	for i := range groups {
		if groups[i].ProviderID == providerID {
			return &groups[i]
		}
	}
	return nil
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringSliceContainsAll(values []string, want []string) bool {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		seen[value] = true
	}
	for _, value := range want {
		if !seen[value] {
			return false
		}
	}
	return true
}

func bridgeKey(bridge *Bridge) string {
	if bridge == nil {
		return "<nil>"
	}
	return bridge.Key()
}

func jobID(job map[string]any) string {
	return job["id"].(string)
}

func assertJobStatus(t *testing.T, endPayload map[string]any, status string, code int, stopReason string) {
	t.Helper()
	job := jobFromEnd(endPayload)
	if job["status"] != status || job["code"] != code {
		t.Fatalf("job status/code = %#v, want %s/%d", job, status, code)
	}
	if stopReason != "" && job["stopReason"] != stopReason {
		t.Fatalf("stopReason = %#v, want %q in job %#v", job["stopReason"], stopReason, job)
	}
}

func assertDataChunkShape(t *testing.T, payload map[string]any, id string) {
	t.Helper()
	// Renderer fixture: desktop/renderer/app.js:5206-5209 dispatches top-level
	// {type:"data", id, chunk, stream}; app.js:5538-5539 requires stream=="stdout";
	// app.js:4948-4953 appends chunk to the assistant message.
	if payload["type"] != "data" || payload["id"] != id || payload["stream"] != "stdout" {
		t.Fatalf("data chunk routing shape = %#v", payload)
	}
	if _, ok := payload["chunk"].(string); !ok {
		t.Fatalf("data chunk missing string chunk: %#v", payload)
	}
	if _, ok := payload["job"]; ok {
		t.Fatalf("data chunk should not nest job: %#v", payload)
	}
}

func jobFromEnd(payload map[string]any) map[string]any {
	job, _ := payload["job"].(map[string]any)
	return job
}

type collectedEvent struct {
	channel string
	payload any
}

type eventCollector struct {
	ch     chan collectedEvent
	mu     sync.Mutex
	events []collectedEvent
}

func newEventCollector() *eventCollector {
	return &eventCollector{ch: make(chan collectedEvent, 256)}
}

func (c *eventCollector) Broadcast(channel string, payload any) {
	ev := collectedEvent{channel: channel, payload: payload}
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
	c.ch <- ev
}

func (c *eventCollector) snapshot() []collectedEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]collectedEvent, len(c.events))
	copy(out, c.events)
	return out
}

func (c *eventCollector) waitChannel(t *testing.T, channel string, timeout time.Duration) collectedEvent {
	t.Helper()
	return c.waitFor(t, timeout, func(ev collectedEvent) bool { return ev.channel == channel })
}

func (c *eventCollector) waitJobType(t *testing.T, id, typ string, timeout time.Duration) map[string]any {
	t.Helper()
	ev := c.waitFor(t, timeout, func(ev collectedEvent) bool {
		if ev.channel != "job:event" {
			return false
		}
		payload, _ := ev.payload.(map[string]any)
		if payload["type"] != typ {
			return false
		}
		if typ == "start" || typ == "end" {
			job, _ := payload["job"].(map[string]any)
			return job["id"] == id
		}
		return payload["id"] == id
	})
	return ev.payload.(map[string]any)
}

func (c *eventCollector) waitJobEnd(t *testing.T, id string, timeout time.Duration) map[string]any {
	t.Helper()
	ev := c.waitFor(t, timeout, func(ev collectedEvent) bool {
		if ev.channel != "job:event" {
			return false
		}
		payload, _ := ev.payload.(map[string]any)
		if payload["type"] != "end" {
			return false
		}
		job, _ := payload["job"].(map[string]any)
		return job["id"] == id
	})
	return ev.payload.(map[string]any)
}

func (c *eventCollector) waitFor(t *testing.T, timeout time.Duration, pred func(collectedEvent) bool) collectedEvent {
	t.Helper()
	deadline := time.After(timeout)
	for {
		c.mu.Lock()
		for _, ev := range c.events {
			if pred(ev) {
				c.mu.Unlock()
				return ev
			}
		}
		c.mu.Unlock()
		select {
		case ev := <-c.ch:
			if pred(ev) {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for event; events=%#v", c.snapshot())
		}
	}
}

func (c *eventCollector) expectNoChannel(t *testing.T, channel string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-c.ch:
			if ev.channel == channel {
				t.Fatalf("unexpected %s event payload=%#v", channel, ev.payload)
			}
		case <-deadline:
			return
		}
	}
}

func (c *eventCollector) jobEvents(id, typ string) []map[string]any {
	var out []map[string]any
	for _, ev := range c.snapshot() {
		if ev.channel != "job:event" {
			continue
		}
		payload, _ := ev.payload.(map[string]any)
		if payload["type"] == typ && payload["id"] == id {
			out = append(out, payload)
		}
	}
	return out
}

func (c *eventCollector) jobEventKindsInOrder(id string, want []string) bool {
	idx := 0
	for _, ev := range c.snapshot() {
		if ev.channel != "job:event" {
			continue
		}
		payload, _ := ev.payload.(map[string]any)
		if payload["type"] == "start" {
			job, _ := payload["job"].(map[string]any)
			if job["id"] != id {
				continue
			}
		} else if payload["type"] == "end" {
			job, _ := payload["job"].(map[string]any)
			if job["id"] != id {
				continue
			}
		} else if payload["id"] != id && payload["type"] != "usage" {
			continue
		}
		if idx < len(want) && payload["type"] == want[idx] {
			idx++
		}
	}
	return idx == len(want)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	return root
}

func TestFakeACPHelper(t *testing.T) {
	if os.Getenv("WORKASS_FAKE_ACP") != "1" {
		return
	}
	runFakeACP(os.Getenv("WORKASS_FAKE_ACP_MODE"))
	os.Exit(0)
}

type fakeACP struct {
	mode     string
	mu       sync.Mutex
	writeMu  sync.Mutex
	sessions int
	pending  map[string]chan map[string]any
	active   bool
	steerCh  chan string
	cancelCh chan struct{}
	// The Claude effort fixture changes its config-option surface with the
	// selected model, matching the real adapter (Haiku has no effort option).
	currentModel      string
	resetCredits      int
	redeemedResetKeys map[string]bool
	backgroundFiles   []*os.File
}

func runFakeACP(mode string) {
	s := &fakeACP{mode: mode, pending: make(map[string]chan map[string]any), steerCh: make(chan string, 1), cancelCh: make(chan struct{}, 1), resetCredits: 1, redeemedResetKeys: make(map[string]bool)}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		s.accept(line)
	}
}

func (s *fakeACP) accept(line []byte) {
	var fields map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&fields); err != nil {
		return
	}
	rawID, hasID := fields["id"]
	var method string
	if raw := fields["method"]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &method)
	}
	if method != "" {
		recordFakeACPMethod(method)
	}
	if hasID && method == "" {
		ch := s.pending[string(rawID)]
		if ch != nil {
			ch <- decodeObject(fields["result"])
		}
		return
	}
	if !hasID || method == "" {
		if method == "session/cancel" {
			select {
			case s.cancelCh <- struct{}{}:
			default:
			}
		}
		return
	}
	params := decodeObject(fields["params"])
	go s.handleRequest(rawID, method, params)
}

func (s *fakeACP) handleRequest(id json.RawMessage, method string, params map[string]any) {
	switch method {
	case "initialize":
		switch s.mode {
		case "init-hang":
			select {}
		case "crash-stderr":
			_, _ = fmt.Fprint(os.Stderr, "abcdefghijklmnopqrstuvwxyz0123456789")
			os.Exit(7)
		case "auth-stderr":
			_, _ = fmt.Fprint(os.Stderr, "Unauthorized: login required; credential missing")
			os.Exit(1)
		case "flaky-init":
			stateFile := os.Getenv("WORKASS_FAKE_ACP_FLAKY_STATE")
			if stateFile == "" {
				_, _ = fmt.Fprint(os.Stderr, "WORKASS_FAKE_ACP_FLAKY_STATE is required")
				os.Exit(8)
			}
			if _, err := os.Stat(stateFile); os.IsNotExist(err) {
				_ = os.WriteFile(stateFile, []byte("failed-once\n"), 0o600)
				_, _ = fmt.Fprint(os.Stderr, "temporary initialize failure")
				os.Exit(8)
			}
		}
		imageSupport := strings.HasPrefix(s.mode, "image-") || strings.HasSuffix(s.mode, "-image")
		capabilities := map[string]any{
			"promptCapabilities": map[string]any{"image": imageSupport},
		}
		if strings.HasPrefix(s.mode, "codex-steer") {
			capabilities["_meta"] = map[string]any{
				"workassCodexSteerRequest": true,
				"workassCodexSteerReceipt": true,
			}
		}
		if strings.HasPrefix(s.mode, "claude-steer") {
			capabilities["_meta"] = map[string]any{
				"workassClaudeSteerRequest": true,
				"workassClaudeSteerReceipt": true,
			}
		}
		if strings.HasPrefix(s.mode, "claude-commands") {
			capabilities["_meta"] = map[string]any{"workassClaudeCommandCatalog": true}
		}
		if s.mode == "codex-plan-limits" {
			capabilities["_meta"] = map[string]any{
				"workassCodexRateLimitsRequest":     true,
				"workassCodexRateLimitResetRequest": true,
			}
		}
		if s.mode == "permission-reconcile-hang" {
			capabilities["_meta"] = map[string]any{"workassTurnReconcileRequest": true}
		}
		if s.mode == "claude-plan-limits" || s.mode == "claude-plan-limits-delay" || s.mode == "claude-plan-limits-hang" {
			capabilities["_meta"] = map[string]any{"workassClaudeUsageRequest": true}
		}
		s.respond(id, map[string]any{
			"protocolVersion":   1,
			"agentInfo":         map[string]any{"name": "Fake ACP", "version": "0.0.0"},
			"agentCapabilities": capabilities,
			"authMethods":       []any{},
		})
	case "session/new":
		if s.mode == "crash-session-new" {
			_, _ = fmt.Fprint(os.Stderr, "authenticated ACP exited while creating a session")
			os.Exit(9)
		}
		s.mu.Lock()
		s.sessions++
		sessionID := fmt.Sprintf("fake-session-%d-%d", os.Getpid(), s.sessions)
		s.mu.Unlock()
		configOptions := fakeConfigOptions("fake-model", "ask")
		result := map[string]any{"sessionId": sessionID, "configOptions": configOptions}
		if s.mode == "effort-catalog" {
			result["configOptions"] = fakePlainCatalogConfigOptions("m", "ask")
			result["models"] = map[string]any{"availableModels": fakeAvailableEffortModels(), "currentModelId": "m[medium]"}
		}
		if s.mode == "claude-effort" || s.mode == "claude-effort-default" {
			currentModel := "opus[1m]"
			if s.mode == "claude-effort-default" {
				currentModel = "default"
			}
			s.mu.Lock()
			s.currentModel = currentModel
			s.mu.Unlock()
			result["configOptions"] = fakeClaudeEffortConfigOptions(currentModel, "default", "default")
		}
		if s.mode == "claude-cold-effort" {
			s.mu.Lock()
			s.currentModel = "claude-fable-5[1m]"
			s.mu.Unlock()
			result["availableModels"] = fakeClaudeAvailableModels()
		}
		if s.mode == "codex-controls" {
			result["configOptions"] = fakeCodexControlsConfigOptions("gpt-5.6-sol", "agent", "xhigh")
			result["models"] = map[string]any{
				"availableModels": fakeCodexAvailableModels(),
				"currentModelId":  "gpt-5.6-sol[xhigh]",
			}
		}
		if strings.HasPrefix(s.mode, "claude-commands") {
			result["commandCatalog"] = fakeClaudeCommandCatalogResult(s.mode)
		}
		s.respond(id, result)
	case "session/set_config_option":
		recordFakeConfigCall(asString(params["configId"]), asString(params["value"]))
		if s.mode == "control-reject" {
			s.fail(id, -32602, "Invalid params")
			return
		}
		if s.mode == "codex-controls" {
			model, mode, effort := "gpt-5.6-sol", "agent", "xhigh"
			switch asString(params["configId"]) {
			case "model":
				model = asString(params["value"])
			case "mode":
				mode = asString(params["value"])
			case "reasoning_effort":
				effort = asString(params["value"])
			}
			s.respond(id, map[string]any{"configOptions": fakeCodexControlsConfigOptions(model, mode, effort)})
			return
		}
		if s.mode == "claude-effort" || s.mode == "claude-effort-default" {
			if asString(params["configId"]) == "effort" {
				s.mu.Lock()
				currentModel := s.currentModel
				s.mu.Unlock()
				if currentModel == "haiku" {
					s.fail(id, -32602, "Unknown config option: effort")
					return
				}
				s.respond(id, map[string]any{"configOptions": []any{fakeClaudeEffortOption(asString(params["value"]))}})
				return
			}
			model := asString(params["value"])
			s.mu.Lock()
			s.currentModel = model
			s.mu.Unlock()
			s.respond(id, map[string]any{"configOptions": fakeClaudeEffortConfigOptions(model, "default", "default")})
			return
		}
		if s.mode == "claude-cold-effort" {
			if asString(params["configId"]) == "effort" {
				s.respond(id, map[string]any{"configOptions": []any{fakeClaudeEffortOption(asString(params["value"]))}})
				return
			}
			model := asString(params["value"])
			if model == "workass-test-rejected-model" {
				s.fail(id, -32602, "unknown model")
				return
			}
			s.mu.Lock()
			s.currentModel = model
			s.mu.Unlock()
			s.respond(id, map[string]any{})
			return
		}
		s.respond(id, map[string]any{"configOptions": fakeConfigOptions(asString(params["value"]), asString(params["value"]))})
	case "session/close":
		s.respond(id, map[string]any{})
	case "session/prompt":
		s.handlePrompt(id, params)
	case "_workass/codex/steer":
		if !strings.HasPrefix(s.mode, "codex-steer") {
			s.fail(id, -32601, "fake method not found: "+method)
			return
		}
		if s.mode == "codex-steer-rejected" {
			s.fail(id, -32098, "native steer rejected")
			return
		}
		if s.mode == "codex-steer-next-turn" {
			s.respond(id, map[string]any{"disposition": "next-turn", "reason": "no-active-turn"})
			return
		}
		if s.mode == "codex-steer-nonsteerable" {
			s.respond(id, map[string]any{"disposition": "queue", "reason": "active-turn-not-steerable", "turnKind": "review"})
			return
		}
		clientUserMessageID := asString(params["clientUserMessageId"])
		if clientUserMessageID != "" {
			s.notify(asString(params["sessionId"]), map[string]any{
				"sessionUpdate":       "_workass_codex_steer_consumed",
				"clientUserMessageId": clientUserMessageID,
			})
			if s.mode == "codex-steer-duplicate-receipt" {
				s.notify(asString(params["sessionId"]), map[string]any{
					"sessionUpdate":       "_workass_codex_steer_consumed",
					"clientUserMessageId": clientUserMessageID,
				})
			}
		}
		if s.mode == "codex-steer-image" {
			s.steerCh <- fakePromptImageObservation(params["prompt"])
		} else {
			s.steerCh <- fakePromptText(params["prompt"])
		}
		s.respond(id, map[string]any{"turnId": "fake-active-turn"})
	case "_workass/claude/steer":
		if !strings.HasPrefix(s.mode, "claude-steer") {
			s.fail(id, -32601, "fake method not found: "+method)
			return
		}
		clientUserMessageID := asString(params["clientUserMessageId"])
		if clientUserMessageID != "" {
			s.notify(asString(params["sessionId"]), map[string]any{
				"sessionUpdate":       "_workass_claude_steer_consumed",
				"clientUserMessageId": clientUserMessageID,
			})
		}
		if s.mode == "claude-steer-image" {
			s.steerCh <- fakePromptImageObservation(params["prompt"])
		} else {
			s.steerCh <- fakePromptText(params["prompt"])
		}
		s.respond(id, map[string]any{"turnId": "fake-claude-steer"})
	case "_workass/codex/rate-limits":
		if s.mode != "codex-plan-limits" {
			s.fail(id, -32601, "fake method not found: "+method)
			return
		}
		s.mu.Lock()
		credits := s.resetCredits
		s.mu.Unlock()
		s.respond(id, fakeCodexRateLimitsWithCredits(credits))
	case "_workass/codex/rate-limit-reset/consume":
		if s.mode != "codex-plan-limits" {
			s.fail(id, -32601, "fake method not found: "+method)
			return
		}
		key := strings.TrimSpace(asString(params["idempotencyKey"]))
		if key == "" {
			s.fail(id, -32602, "idempotencyKey is required")
			return
		}
		s.mu.Lock()
		outcome := "noCredit"
		if strings.TrimSpace(asString(params["creditId"])) == "nothing-to-reset" {
			outcome = "nothingToReset"
		} else if s.redeemedResetKeys[key] {
			outcome = "alreadyRedeemed"
		} else if s.resetCredits > 0 {
			s.redeemedResetKeys[key] = true
			s.resetCredits--
			outcome = "reset"
		}
		credits := s.resetCredits
		s.mu.Unlock()
		s.respond(id, map[string]any{"outcome": outcome, "rateLimits": fakeCodexRateLimitsWithCredits(credits)})
	case "_workass/turn/reconcile":
		if s.mode != "permission-reconcile-hang" {
			s.fail(id, -32601, "fake method not found: "+method)
			return
		}
		select {}
	case "_workass/claude/usage":
		if s.mode == "claude-plan-limits-hang" {
			select {}
		}
		if s.mode == "claude-plan-limits-delay" {
			time.Sleep(150 * time.Millisecond)
		}
		if s.mode != "claude-plan-limits" && s.mode != "claude-plan-limits-delay" {
			s.fail(id, -32601, "fake method not found: "+method)
			return
		}
		s.respond(id, fakeClaudeStructuredUsage())
	default:
		s.fail(id, -32601, "fake method not found: "+method)
	}
}

func (s *fakeACP) handlePrompt(id json.RawMessage, params map[string]any) {
	sessionID := asString(params["sessionId"])
	if strings.HasPrefix(s.mode, "claude-commands") && strings.Contains(fakePromptText(params["prompt"]), "push commands") {
		// The host's commands_changed forward: a full-catalog replace pushed
		// between/inside turns as a _workass_claude_commands session update.
		s.notify(sessionID, map[string]any{
			"sessionUpdate": "_workass_claude_commands",
			"commandCatalog": map[string]any{
				"commands":    []any{map[string]any{"name": "changed-one", "description": "Pushed replacement", "argumentHint": ""}},
				"agents":      []any{},
				"outputStyle": "default", "availableOutputStyles": []any{"default"},
				"commandsTruncated": 0, "agentsTruncated": 0, "stylesTruncated": 0,
				"asOf": 1785000000001,
			},
		})
		s.notify(sessionID, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "commands pushed"}})
		s.respond(id, map[string]any{"stopReason": "end_turn"})
		return
	}
	if strings.Contains(fakePromptText(params["prompt"]), "[fake:claude-updates]") {
		s.notify(sessionID, map[string]any{
			"sessionUpdate": "_workass_claude_provider_session", "providerSessionId": sessionID + "-forked-1",
		})
		s.notify(sessionID, map[string]any{
			"sessionUpdate": "_workass_claude_turn_heartbeat",
			"elapsedMs":     float64(2000), "outputTokens": float64(345), "phase": "thinking",
		})
		s.notify(sessionID, map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": "updates sent"},
		})
		s.respond(id, map[string]any{"stopReason": "end_turn"})
		return
	}
	switch s.mode {
	case "image-echo":
		s.notify(sessionID, map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": fakePromptImageObservation(params["prompt"])},
		})
		s.respond(id, map[string]any{"stopReason": "end_turn"})
		return
	case "auth-on-prompt":
		s.fail(id, -32001, "Authentication required")
		return
	case "claude-plan-usage":
		s.notify(sessionID, fakeClaudeRateLimitUsageUpdate())
		s.notify(sessionID, fakeClaudeCostUsageUpdate())
		s.respond(id, map[string]any{"stopReason": "end_turn"})
		return
	case "codex-plan-usage":
		s.respond(id, fakeCodexPromptResult())
		return
	case "unknown-plan-usage":
		s.notify(sessionID, map[string]any{"sessionUpdate": "usage_update", "_meta": map[string]any{"weekly": map[string]any{"remaining": 42}, "credits": map[string]any{"used": 7, "size": 100}}})
		s.respond(id, map[string]any{"stopReason": "end_turn"})
		return
	case "huge-plan-usage":
		s.notify(sessionID, map[string]any{"sessionUpdate": "usage_update", "_meta": map[string]any{"blob": strings.Repeat("x", 9000)}})
		s.respond(id, map[string]any{"stopReason": "end_turn"})
		return
	case "secret-plan-usage":
		s.notify(sessionID, map[string]any{"sessionUpdate": "usage_update", "_meta": map[string]any{"api_key": "sk-live-secret", "nested": map[string]any{"safe": "api_key=sk-nested"}}})
		s.respond(id, map[string]any{"stopReason": "end_turn"})
		return
	case "codex-steer", "codex-steer-duplicate-receipt", "codex-steer-image":
		s.notify(sessionID, map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "waiting for acknowledged native steer"}})
		select {
		case steer := <-s.steerCh:
			s.notify(sessionID, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "codex steered: " + steer}})
		case <-time.After(time.Second):
			s.fail(id, -32098, "native steer request did not arrive")
			return
		}
		s.respond(id, map[string]any{"stopReason": "end_turn"})
		return
	case "claude-steer", "claude-steer-image":
		s.notify(sessionID, map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "waiting for acknowledged Claude steer"}})
		select {
		case steer := <-s.steerCh:
			s.notify(sessionID, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "claude steered: " + steer}})
		case <-s.cancelCh:
			s.respond(id, map[string]any{"stopReason": "cancelled"})
			return
		case <-time.After(time.Second):
			s.fail(id, -32098, "native Claude steer request did not arrive")
			return
		}
		s.respond(id, map[string]any{"stopReason": "end_turn"})
		return
	case "codex-steer-next-turn":
		s.notify(sessionID, map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "native turn already ended"}})
		select {
		case <-s.cancelCh:
			s.respond(id, map[string]any{"stopReason": "cancelled"})
		case <-time.After(time.Second):
			s.fail(id, -32098, "stale ACP prompt wrapper was not interrupted")
		}
		return
	case "codex-steer-nonsteerable":
		s.notify(sessionID, map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "review turn remains active"}})
		select {
		case <-s.cancelCh:
			s.respond(id, map[string]any{"stopReason": "cancelled"})
		case <-time.After(220 * time.Millisecond):
			s.respond(id, map[string]any{"stopReason": "end_turn"})
		}
		return
	case "interruptible-prompt", "codex-steer-rejected":
		s.notify(sessionID, map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "waiting for interrupt"}})
		select {
		case <-s.cancelCh:
			s.respond(id, map[string]any{"stopReason": "cancelled"})
		case <-time.After(3 * time.Second):
			s.respond(id, map[string]any{"stopReason": "end_turn"})
		}
		return
	case "claude-spawned-work-interrupt":
		taskID := "fake-bg-running"
		toolCallID := "fake-bg-tool"
		outputFile := filepath.Join(os.TempDir(), "claude-fake", sessionID, "tasks", taskID+".output")
		if err := os.MkdirAll(filepath.Dir(outputFile), 0o700); err != nil {
			s.fail(id, -32098, err.Error())
			return
		}
		f, err := os.OpenFile(outputFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			s.fail(id, -32098, err.Error())
			return
		}
		_, _ = f.WriteString("fake background work started\n")
		s.mu.Lock()
		s.backgroundFiles = append(s.backgroundFiles, f)
		s.mu.Unlock()
		s.notify(sessionID, map[string]any{
			"sessionUpdate": "tool_call", "toolCallId": toolCallID, "title": "Bash", "kind": "execute", "status": "in_progress",
			"rawInput": map[string]any{"command": "node fake-background-helper.mjs", "run_in_background": true},
			"_meta":    map[string]any{"claudeCode": map[string]any{"toolName": "Bash"}},
		})
		s.notify(sessionID, map[string]any{
			"sessionUpdate": "tool_call_update", "toolCallId": toolCallID, "title": "Bash", "kind": "execute", "status": "completed",
			"content": map[string]any{"type": "text", "text": fmt.Sprintf("Command running in background with ID: %s. Output is being written to: %s", taskID, outputFile)},
			"_meta":   map[string]any{"claudeCode": map[string]any{"toolName": "Bash"}},
		})
		s.notify(sessionID, map[string]any{
			"sessionUpdate": "_workass_claude_spawned_work",
			"event":         map[string]any{"type": "started", "taskId": taskID, "toolCallId": toolCallID, "description": "Fake background helper", "taskType": "bash"},
		})
		s.notify(sessionID, map[string]any{
			"sessionUpdate": "_workass_claude_spawned_work",
			"event":         map[string]any{"type": "snapshot", "tasks": []any{map[string]any{"taskId": taskID, "taskType": "bash", "description": "Fake background helper"}}},
		})
		select {
		case <-s.cancelCh:
			s.respond(id, map[string]any{"stopReason": "cancelled"})
		case <-time.After(3 * time.Second):
			s.respond(id, map[string]any{"stopReason": "end_turn"})
		}
		return
	}
	if s.mode == "slow-prompt" {
		time.Sleep(160 * time.Millisecond)
		s.notify(sessionID, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": fakePromptText(params["prompt"])}})
		s.respond(id, map[string]any{"stopReason": "end_turn"})
		return
	}
	if s.mode == "serialize" {
		s.mu.Lock()
		if s.active {
			s.mu.Unlock()
			s.fail(id, -32099, "concurrent prompt reached fake agent")
			return
		}
		s.active = true
		s.mu.Unlock()
		time.Sleep(80 * time.Millisecond)
		s.notify(sessionID, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "serialized ok"}})
		s.respond(id, map[string]any{"stopReason": "end_turn"})
		s.mu.Lock()
		s.active = false
		s.mu.Unlock()
		return
	}

	if s.mode == "permission" || s.mode == "permission-reconcile-hang" {
		result := s.serverRequest("perm-1", "session/request_permission", map[string]any{
			"sessionId": sessionID,
			"toolCall":  map[string]any{"title": "Run fake tool", "kind": "execute"},
			"options": []any{
				map[string]any{"optionId": "allow", "name": "Allow once", "kind": "allow_once"},
				map[string]any{"optionId": "deny", "name": "Deny", "kind": "reject_once"},
			},
		})
		outcome := mapFromAny(result["outcome"])
		optionID := asString(outcome["optionId"])
		if outcome["outcome"] == "cancelled" {
			optionID = "cancelled"
		}
		s.notify(sessionID, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "permission selected " + optionID}})
		s.respond(id, map[string]any{"stopReason": "end_turn"})
		return
	}
	if s.mode == "echo-prompt" || s.mode == "claude-cold-effort" {
		s.notify(sessionID, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": fakePromptText(params["prompt"])}})
		s.respond(id, map[string]any{"stopReason": "end_turn"})
		return
	}

	s.notify(sessionID, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "ok"}})
	s.respond(id, map[string]any{"stopReason": "end_turn"})
}

func fakeClaudeRateLimitUsageUpdate() map[string]any {
	return map[string]any{
		"sessionUpdate": "usage_update",
		"used":          28320,
		"size":          200000,
		"_meta": map[string]any{
			"_claude/rateLimit": map[string]any{
				"status":          "allowed",
				"utilization":     0.78,
				"resetsAt":        1783713600,
				"rateLimitType":   "five_hour",
				"overageStatus":   "allowed",
				"overageResetsAt": 1783708800,
				"isUsingOverage":  false,
			},
		},
	}
}

func fakeClaudeStructuredUsage() map[string]any {
	return map[string]any{
		"subscription_type":     "max",
		"rate_limits_available": true,
		"rate_limits": map[string]any{
			"five_hour":        map[string]any{"utilization": 37.5, "resets_at": "2026-07-13T20:00:00Z"},
			"seven_day":        map[string]any{"utilization": 78, "resets_at": "2026-07-15T18:00:00Z"},
			"seven_day_opus":   map[string]any{"utilization": 12.25, "resets_at": "2026-07-15T18:00:00Z"},
			"seven_day_sonnet": nil,
			"model_scoped": []any{
				map[string]any{"display_name": "Fable", "utilization": 4, "resets_at": "2026-07-15T18:00:00Z"},
			},
		},
	}
}

func fakeCodexRateLimits() map[string]any {
	return fakeCodexRateLimitsWithCredits(1)
}

func fakeCodexRateLimitsWithCredits(available int) map[string]any {
	var credits any
	if available > 0 {
		credits = []any{map[string]any{
			"id":          "RateLimitResetCredit_test",
			"resetType":   "codexRateLimits",
			"status":      "available",
			"grantedAt":   1783971000,
			"expiresAt":   1784577600,
			"title":       "Full reset (Weekly + 5 hr)",
			"description": "A free reset from Codex",
		}}
	} else {
		credits = []any{}
	}
	return map[string]any{
		"rateLimits": map[string]any{
			"limitId":   "codex",
			"limitName": "Codex",
			"primary": map[string]any{
				"usedPercent":        42,
				"windowDurationMins": 300,
				"resetsAt":           1783972800,
			},
			"secondary": map[string]any{
				"usedPercent":        67,
				"windowDurationMins": 10080,
				"resetsAt":           1784145600,
			},
		},
		"rateLimitResetCredits": map[string]any{
			"availableCount": available,
			"credits":        credits,
		},
	}
}

func fakeClaudeCostUsageUpdate() map[string]any {
	return map[string]any{
		"sessionUpdate": "usage_update",
		"used":          28320,
		"size":          1000000,
		"cost":          map[string]any{"amount": 0.0581616, "currency": "USD"},
	}
}

func fakeCodexPromptResult() map[string]any {
	return map[string]any{
		"stopReason": "end_turn",
		"usage": map[string]any{
			"totalTokens":      14609,
			"inputTokens":      4616,
			"cachedReadTokens": 9984,
			"outputTokens":     9,
			"thoughtTokens":    0,
		},
		"_meta": map[string]any{
			"quota": map[string]any{
				"token_count": map[string]any{
					"totalTokens":           14609,
					"inputTokens":           4616,
					"cachedInputTokens":     9984,
					"outputTokens":          9,
					"reasoningOutputTokens": 0,
				},
				"model_usage": []any{
					map[string]any{
						"model": "gpt-5.6-sol",
						"token_count": map[string]any{
							"totalTokens":           14609,
							"inputTokens":           4616,
							"cachedInputTokens":     9984,
							"outputTokens":          9,
							"reasoningOutputTokens": 0,
						},
					},
				},
			},
		},
	}
}

func fakePromptText(raw any) string {
	blocks, _ := raw.([]any)
	var parts []string
	for _, block := range blocks {
		m := mapFromAny(block)
		if asString(m["type"]) == "text" {
			parts = append(parts, asString(m["text"]))
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func fakePromptImageObservation(raw any) string {
	blocks, _ := raw.([]any)
	text := fakePromptText(raw)
	count, mimeType, data := 0, "", ""
	for _, block := range blocks {
		item := mapFromAny(block)
		if asString(item["type"]) != "image" {
			continue
		}
		count++
		if mimeType == "" {
			mimeType, data = asString(item["mimeType"]), asString(item["data"])
		}
	}
	return fmt.Sprintf("images=%d mime=%s bytes=%d attachment-notice=%t", count, mimeType, len(data), strings.Contains(text, "Workass attachment context"))
}

func fakeConfigOptions(model, mode string) []any {
	return []any{
		map[string]any{"id": "model", "category": "model", "currentValue": model, "options": []any{map[string]any{"value": "fake-model", "name": "Fake model"}}},
		map[string]any{"id": "mode", "category": "mode", "currentValue": mode, "options": []any{
			map[string]any{"value": "ask", "name": "Ask"},
			map[string]any{"value": "bypass", "name": "Bypass"},
		}},
	}
}

func fakePlainCatalogConfigOptions(model, mode string) []any {
	return []any{
		map[string]any{"id": "model", "category": "model", "currentValue": model, "options": []any{
			map[string]any{"value": "m", "name": "M"},
			map[string]any{"value": "plain", "name": "Plain"},
		}},
		map[string]any{"id": "mode", "category": "mode", "currentValue": mode, "options": []any{map[string]any{"value": "ask", "name": "Ask"}}},
	}
}

// fakeClaudeCommandCatalogResult mirrors the native Claude host's clamped
// commandCatalog field on session open replies. The "-overflow" mode is a
// SKEWED host: over the §2 entry caps with over-long fields, carrying its own
// nonzero agentsTruncated, so the daemon's defensive re-clamp has work to do.
func fakeClaudeCommandCatalogResult(mode string) map[string]any {
	if mode == "claude-commands-overflow" {
		commands := make([]any, 0, 600)
		for i := 0; i < 600; i++ {
			commands = append(commands, map[string]any{
				"name":         fmt.Sprintf("overflow-command-%d", i),
				"description":  strings.Repeat("d", 300),
				"argumentHint": strings.Repeat("h", 120),
				"aliases":      []any{"a0", "a1", "a2", "a3", "a4", "a5"},
			})
		}
		return map[string]any{
			"commands":    commands,
			"agents":      []any{map[string]any{"name": strings.Repeat("n", 90), "description": "over-limit host", "model": "sonnet"}},
			"outputStyle": "default", "availableOutputStyles": []any{"default"},
			"commandsTruncated": 0, "agentsTruncated": 2, "stylesTruncated": 0,
			"asOf": 1785000000000,
		}
	}
	return map[string]any{
		"commands": []any{
			map[string]any{"name": "review", "description": "Review a PR", "argumentHint": "<pr>", "aliases": []any{"cr"}},
			map[string]any{"name": "deploy", "description": "Deploy with token=sk-fake-cmd-secret inside", "argumentHint": ""},
		},
		"agents": []any{
			map[string]any{"name": "Explore", "description": "Fast explorer", "model": "sonnet"},
		},
		"outputStyle": "default", "availableOutputStyles": []any{"default", "explanatory"},
		"commandsTruncated": 0, "agentsTruncated": 0, "stylesTruncated": 0,
		"asOf": 1785000000000,
	}
}

// fakeClaudeEffortConfigOptions mirrors the native Claude host's SDK projection
// shape: reasoning effort is a SEPARATE thought_level config option, not baked into
// bracketed model ids the way Codex does it.
func fakeClaudeEffortConfigOptions(model, mode, effort string) []any {
	options := []any{
		map[string]any{"id": "model", "category": "model", "currentValue": model, "options": []any{
			map[string]any{"value": "default", "name": "Default (recommended)", "description": "Opus 4.8 with 1M context · Best for everyday, complex tasks"},
			map[string]any{"value": "opus[1m]", "name": "Opus", "description": "Opus 4.8 with 1M context · Best for everyday, complex tasks"},
			map[string]any{"value": "claude-fable-5[1m]", "name": "Fable", "description": "Fable 5"},
			map[string]any{"value": "sonnet", "name": "Sonnet", "description": "Sonnet 5"},
			map[string]any{"value": "haiku", "name": "Haiku", "description": "Haiku 4.5"},
		}},
		map[string]any{"id": "mode", "category": "mode", "currentValue": mode, "options": []any{map[string]any{"value": "default", "name": "Manual"}}},
	}
	if model != "haiku" {
		options = append(options, fakeClaudeEffortOption(effort))
	}
	return options
}

func fakeClaudeEffortOption(current string) map[string]any {
	return map[string]any{"id": "effort", "category": "thought_level", "name": "Effort", "currentValue": current, "options": []any{
		map[string]any{"value": "default", "name": "Default"},
		map[string]any{"value": "low", "name": "Low"},
		map[string]any{"value": "medium", "name": "Medium"},
		map[string]any{"value": "high", "name": "High"},
		map[string]any{"value": "xhigh", "name": "Xhigh"},
		map[string]any{"value": "max", "name": "Max"},
	}}
}

func fakeClaudeAvailableModels() []any {
	return []any{
		map[string]any{"id": "default", "name": "Default", "description": "Use the default model"},
		map[string]any{"id": "opus[1m]", "name": "Opus", "description": "Opus 4.8 with 1M context"},
		map[string]any{"id": "claude-fable-5[1m]", "name": "Fable", "description": "Fable 5"},
		map[string]any{"id": "sonnet", "name": "Sonnet", "description": "Sonnet 5"},
	}
}

func fakeCodexControlsConfigOptions(model, mode, effort string) []any {
	return []any{
		map[string]any{"id": "model", "category": "model", "currentValue": model, "options": []any{
			map[string]any{"value": "gpt-5.6-sol", "name": "GPT-5.6 Sol"},
			map[string]any{"value": "gpt-5.6-luna", "name": "GPT-5.6 Luna"},
		}},
		map[string]any{"id": "mode", "category": "mode", "currentValue": mode, "options": []any{
			map[string]any{"value": "read-only", "name": "Read only"},
			map[string]any{"value": "agent", "name": "Agent"},
			map[string]any{"value": "agent-full-access", "name": "Full access"},
		}},
		map[string]any{"id": "reasoning_effort", "category": "thought_level", "currentValue": effort, "options": []any{
			map[string]any{"value": "low", "name": "Low"},
			map[string]any{"value": "high", "name": "High"},
			map[string]any{"value": "xhigh", "name": "Xhigh"},
			map[string]any{"value": "max", "name": "Max"},
		}},
	}
}

func fakeCodexAvailableModels() []any {
	models := []any{}
	for _, model := range []string{"gpt-5.6-sol", "gpt-5.6-luna"} {
		for _, effort := range []string{"low", "high", "xhigh", "max"} {
			models = append(models, map[string]any{
				"id":   model + "[" + effort + "]",
				"name": model + " (" + effort + ")",
			})
		}
	}
	return models
}

func recordFakeConfigCall(configID, value string) {
	logPath := strings.TrimSpace(os.Getenv("WORKASS_FAKE_ACP_CONFIG_LOG"))
	if logPath == "" {
		return
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(configID + "=" + value + "\n")
}

func recordFakeACPMethod(method string) {
	logPath := strings.TrimSpace(os.Getenv("WORKASS_FAKE_ACP_METHOD_LOG"))
	if logPath == "" {
		return
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(method + "\n")
}

func fakeAvailableEffortModels() []any {
	return []any{
		map[string]any{"id": "m[low]", "name": "M (low)"},
		map[string]any{"id": "m[medium]", "name": "M (medium)"},
		map[string]any{"id": "m[high]", "name": "M (high)"},
		map[string]any{"id": "plain", "name": "Plain"},
	}
}

func (s *fakeACP) serverRequest(id, method string, params any) map[string]any {
	rawID, _ := json.Marshal(id)
	key := string(rawID)
	ch := make(chan map[string]any, 1)
	s.mu.Lock()
	s.pending[key] = ch
	s.mu.Unlock()
	s.write(rpcCall{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	select {
	case result := <-ch:
		return result
	case <-time.After(2 * time.Second):
		return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
	}
}

func (s *fakeACP) notify(sessionID string, update any) {
	s.write(rpcCall{JSONRPC: "2.0", Method: "session/update", Params: map[string]any{"sessionId": sessionID, "update": update}})
}

func (s *fakeACP) respond(id json.RawMessage, result any) {
	s.write(rpcCall{JSONRPC: "2.0", ID: json.RawMessage(id), Result: result})
}

func (s *fakeACP) fail(id json.RawMessage, code int, message string) {
	s.write(rpcCall{JSONRPC: "2.0", ID: json.RawMessage(id), Error: rpcError{Code: code, Message: message}})
}

func (s *fakeACP) write(v any) {
	data, _ := json.Marshal(v)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, _ = os.Stdout.Write(append(data, '\n'))
}

// Subagent tracking: the daemon must forward the parent→child linkage the Claude
// adapter stamps on nested tool events (_meta.claudeCode.parentToolUseId), and
// label each child with the spawning call's title. Drives emitToolEvent directly
// so the mapping is asserted deterministically without a live adapter.
func TestEmitToolEventForwardsSubagentLinkage(t *testing.T) {
	var events []map[string]any
	mgr := NewManager(Options{Broadcast: func(channel string, payload any) {
		if channel != "job:event" {
			return
		}
		if m, ok := payload.(map[string]any); ok {
			events = append(events, m)
		}
	}})
	t.Cleanup(func() { mgr.Reset() })
	b := &Bridge{providerID: "claude-agent-acp", manager: mgr}
	job := &Job{ID: "job-1"}

	// The spawning Task tool call runs on the main thread — no parent id.
	b.emitToolEvent(job, "tool_call", map[string]any{
		"toolCallId": "task-1", "title": "acp-wire-truth", "kind": "other", "status": "in_progress",
	}, "")
	// A tool call made INSIDE that subagent carries the parent id in _meta, which
	// handleNotification extracts and passes through.
	b.emitToolEvent(job, "tool_call", map[string]any{
		"toolCallId": "read-1", "title": "Read", "kind": "read", "status": "completed",
	}, "task-1")

	if len(events) != 2 {
		t.Fatalf("emitted %d job:events, want 2", len(events))
	}
	ev0 := mapFromAny(events[0]["event"])
	if _, ok := ev0["subagentId"]; ok {
		t.Fatalf("main-thread Task call must NOT carry subagentId: %#v", ev0)
	}
	ev1 := mapFromAny(events[1]["event"])
	if got := asString(ev1["subagentId"]); got != "task-1" {
		t.Fatalf("child subagentId = %q, want task-1", got)
	}
	if got := asString(ev1["subagentLabel"]); got != "acp-wire-truth" {
		t.Fatalf("child subagentLabel = %q, want acp-wire-truth (spawning Task's title)", got)
	}
	if got := asString(ev1["subagentProvider"]); got != "claude" {
		t.Fatalf("child subagentProvider = %q, want claude", got)
	}
}

func TestEmitToolEventPreservesVisibleRasterResults(t *testing.T) {
	var events []map[string]any
	mgr := NewManager(Options{Broadcast: func(channel string, payload any) {
		if channel == "job:event" {
			events = append(events, mapFromAny(payload))
		}
	}})
	t.Cleanup(func() { mgr.Reset() })
	b := &Bridge{providerID: "codex", manager: mgr}
	job := &Job{ID: "image-job"}
	content := []any{
		map[string]any{"type": "text", "text": "comparison ready"},
		map[string]any{"type": "image", "mimeType": "image/png", "data": "cG5n", "name": "Option A"},
		map[string]any{"type": "wrapper", "content": []any{
			map[string]any{"type": "image", "data": "data:image/webp;base64,d2VicA==", "alt": "Option B"},
		}},
		map[string]any{"type": "image", "mimeType": "image/svg+xml", "data": "PHN2Zz4="},
	}
	b.emitToolEvent(job, "tool_call_update", map[string]any{
		"toolCallId": "mock-sheet", "title": "Render mocks", "status": "completed", "content": content,
	}, "")

	if len(events) != 1 {
		t.Fatalf("emitted events = %d, want 1", len(events))
	}
	event := mapFromAny(events[0]["event"])
	if got := asString(event["output"]); got != "comparison ready" {
		t.Fatalf("tool text output = %q", got)
	}
	images, _ := event["images"].([]any)
	if len(images) != 2 {
		t.Fatalf("tool images = %#v, want two safe raster images", images)
	}
	first, second := mapFromAny(images[0]), mapFromAny(images[1])
	if first["mimeType"] != "image/png" || first["data"] != "cG5n" || first["name"] != "Option A" {
		t.Fatalf("first tool image = %#v", first)
	}
	if second["mimeType"] != "image/webp" || second["data"] != "d2VicA==" || second["name"] != "Option B" {
		t.Fatalf("nested data-url tool image = %#v", second)
	}
}

func TestToolResultImagesAreBounded(t *testing.T) {
	content := make([]any, 0, maxToolResultImages+2)
	for index := 0; index < maxToolResultImages+2; index++ {
		content = append(content, map[string]any{"type": "image", "mimeType": "image/png", "data": fmt.Sprintf("aW1hZ2Ut%d", index)})
	}
	if got := len(toolImagesFromContent(content)); got != maxToolResultImages {
		t.Fatalf("tool image count = %d, want cap %d", got, maxToolResultImages)
	}
	if got := toolImagesFromContent(map[string]any{
		"type": "image", "mimeType": "image/png", "data": strings.Repeat("a", maxToolResultImageBytes+1),
	}); len(got) != 0 {
		t.Fatalf("oversized image was accepted: %#v", got)
	}
}

func TestMetaParentToolUseID(t *testing.T) {
	onUpdate := metaParentToolUseID(
		mapFromAny(map[string]any{"claudeCode": map[string]any{"parentToolUseId": "T9"}}),
		map[string]any{},
	)
	if onUpdate != "T9" {
		t.Fatalf("_meta on update: got %q, want T9", onUpdate)
	}
	onParams := metaParentToolUseID(
		map[string]any{},
		mapFromAny(map[string]any{"claudeCode": map[string]any{"parentToolUseId": "P5"}}),
	)
	if onParams != "P5" {
		t.Fatalf("_meta on params: got %q, want P5", onParams)
	}
	if none := metaParentToolUseID(map[string]any{}, map[string]any{}); none != "" {
		t.Fatalf("no _meta (main thread): got %q, want empty", none)
	}
}

func TestBrandForProvider(t *testing.T) {
	cases := map[string]string{
		"claude-agent-acp": "claude", "anthropic": "claude", "opus": "claude",
		"codex-acp": "gpt", "gpt-5": "gpt", "openai": "gpt",
		"workass.mock": "", "cognition.devin": "",
	}
	for in, want := range cases {
		if got := brandForProvider(in); got != want {
			t.Fatalf("brandForProvider(%q) = %q, want %q", in, got, want)
		}
	}
}

// A durable id that cannot be applied (placeholder, unknown, adapter-rejected)
// must never prevent the session from opening: startup control application is
// best-effort and degrades to the adapter default with the store untouched.
func TestNewSessionOpensWhenStoredStartupModelIsUnappliable(t *testing.T) {
	stateDir := t.TempDir()
	manager, _ := newFakeManager(t, "claude-cold-effort", Options{
		StateDir:          stateDir,
		RSSSampleInterval: time.Hour,
		Provider:          ProviderConfig{ID: "claude"},
	})
	t.Cleanup(func() { manager.Reset() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := manager.NewSession(ctx, SessionOptions{
		TabID: "unappliable-tab", ChatID: "unappliable-chat", ProviderID: "claude",
		ModelID: "workass-test-rejected-model",
	})
	if err != nil {
		t.Fatalf("session must open despite unappliable startup model: %v", err)
	}
	if _, ok := manager.LiveSession(info.SessionID); !ok {
		t.Fatal("session is not live after best-effort startup controls")
	}
}
