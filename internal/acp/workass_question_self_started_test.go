package acp

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	providercontract "workass/internal/provider"
)

// A question asked in a turn Claude started by itself is owned by that turn, as
// one asked in a human turn is. Ending the turn must release the asking CLI and
// settle the card: nothing else would, because a question has no clock of its own.
func TestAQuestionAskedInASelfStartedTurnEndsWithIt(t *testing.T) {
	for name, end := range map[string]func(*Manager, *Bridge, string){
		"harness reports the end": func(manager *Manager, bridge *Bridge, sessionID string) {
			manager.endAdoptedHarnessTurn(bridge, sessionID, nil)
		},
		"engine dies mid-turn": func(manager *Manager, bridge *Bridge, sessionID string) {
			manager.abandonAdoptedHarnessTurn(bridge, sessionID)
		},
	} {
		t.Run(name, func(t *testing.T) {
			events := newEventCollector()
			manager := NewManager(Options{StateDir: t.TempDir(), Broadcast: events.Broadcast})
			t.Cleanup(func() { manager.Reset() })
			const ownerKey, chatID, tabID, sessionID = "owner-self", "chat-self", "tab-self", "session-self"
			bridge := &Bridge{manager: manager, jobsBySession: make(map[string]*Job), sessions: make(map[string]struct{})}
			manager.mu.Lock()
			manager.agentOwners[ownerKey] = agentOwnerBinding{ChatID: chatID, TabID: tabID}
			manager.agentOwnerBySession[sessionID] = ownerKey
			manager.mu.Unlock()
			manager.adoptHarnessTurn(bridge, tabID, chatID, sessionID, "notification-1")
			job := bridge.jobForSession(sessionID)
			if job == nil || !job.harnessTurn {
				t.Fatalf("self-started turn was not adopted: %#v", job)
			}

			done := make(chan *providercontract.QuestionAnswer, 1)
			go func() {
				answer, err := manager.AskAgentQuestion(context.Background(), ownerKey, chatID, tabID, testWorkassQuestion(), 0)
				if err != nil {
					t.Errorf("the running self-started turn refused its question: %v", err)
				}
				done <- answer
			}()
			request := events.waitFor(t, 2*time.Second, func(event collectedEvent) bool {
				return event.channel == "chat:permission-request" && asString(mapFromAny(event.payload)["jobId"]) == job.ID
			})

			end(manager, bridge, sessionID)

			select {
			case answer := <-done:
				if answer == nil || answer.Status != "cancelled" || answer.Reason != "provider_turn_ended" {
					t.Fatalf("question result = %#v", answer)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the asking CLI is still waiting on a turn that ended")
			}
			if pending := manager.PendingPermissions(); len(pending) != 0 {
				t.Fatalf("a finished turn left its card pending: %#v", pending)
			}
			// The card settles before the turn's end, while the chat still owns it.
			resolvedAt, endAt := -1, -1
			for index, event := range events.snapshot() {
				payload := mapFromAny(event.payload)
				if event.channel == "chat:permission-resolved" && asString(payload["id"]) == asString(mapFromAny(request.payload)["id"]) {
					resolvedAt = index
				}
				if event.channel == "job:event" && asString(payload["type"]) == "end" {
					endAt = index
				}
			}
			if resolvedAt < 0 || endAt < 0 || resolvedAt > endAt {
				t.Fatalf("the card must settle before the turn ends: resolved at %d, end at %d", resolvedAt, endAt)
			}
			manager.mu.Lock()
			_, stillRunning := manager.jobs[job.ID]
			manager.mu.Unlock()
			if stillRunning {
				t.Fatal("the ended job is still registered")
			}
		})
	}
}

// The same, over the real Claude host and the fixture SDK: Claude starts a turn by
// itself, a detached helper asks in it, and the turn ends with the card still open.
func TestAQuestionInARealSelfStartedTurnIsReleasedWhenTheTurnEnds(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("node not available for Claude native-host test: %v", err)
	}
	claude := filepath.Join(t.TempDir(), executableName("claude"))
	writeExecutable(t, claude, nativeNoopScript())
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "claude", Name: "Claude Code", Command: claude, Enabled: true, Badge: "native", CWD: root,
			Env: map[string]string{
				"WORKASS_CLAUDE_SDK_MODULE": filepath.Join(root, "desktop", "acp", "mock-claude-agent-sdk.mjs"),
				"WORKASS_CLAUDE_SESSION_ID": "fixture-self-started-question",
			},
		}},
		DefaultProviderID:  "claude",
		ProviderConfigFile: filepath.Join(t.TempDir(), "providers.json"),
		Broadcast:          events.Broadcast,
		InitTimeout:        5 * time.Second,
		RSSSampleInterval:  time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	const tabID, chatID = "real-self-tab", "real-self-chat"
	session, err := manager.NewSession(context.Background(), SessionOptions{TabID: tabID, ChatID: chatID, ProviderID: "claude", CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	human, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, TabID: tabID, ChatID: chatID, ProviderID: "claude",
		Prompt: "[fixture:harness-hold=800] start the background work",
	})
	if err != nil {
		t.Fatal(err)
	}
	events.waitJobEnd(t, jobID(human), 10*time.Second)
	started := events.waitFor(t, 10*time.Second, func(event collectedEvent) bool {
		if event.channel != "job:event" || asString(mapFromAny(event.payload)["type"]) != "start" {
			return false
		}
		return strings.HasPrefix(asString(mapFromAny(mapFromAny(event.payload)["job"])["id"]), "harness-turn-")
	})
	selfStarted := asString(mapFromAny(mapFromAny(started.payload)["job"])["id"])
	manager.mu.Lock()
	ownerKey := manager.agentOwnerBySession[session.SessionID]
	manager.mu.Unlock()

	done := make(chan *providercontract.QuestionAnswer, 1)
	go func() {
		answer, err := manager.AskAgentQuestion(context.Background(), ownerKey, chatID, tabID, testWorkassQuestion(), 0)
		if err != nil {
			t.Errorf("the running self-started turn refused its question: %v", err)
		}
		done <- answer
	}()
	events.waitFor(t, 2*time.Second, func(event collectedEvent) bool {
		return event.channel == "chat:permission-request" && asString(mapFromAny(event.payload)["jobId"]) == selfStarted
	})
	events.waitJobEnd(t, selfStarted, 10*time.Second)
	select {
	case answer := <-done:
		if answer == nil || answer.Status != "cancelled" || answer.Reason != "provider_turn_ended" {
			t.Fatalf("question result = %#v", answer)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the asking CLI is still waiting on a turn that ended")
	}
	if pending := manager.PendingPermissions(); len(pending) != 0 {
		t.Fatalf("a finished turn left its card pending: %#v", pending)
	}
}
