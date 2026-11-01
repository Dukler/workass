package acp

import (
	"context"
	"strings"
	"testing"
	"time"
)

func startSelfStartedTurn(t *testing.T, manager *Manager, session subagentParentSession) (*Bridge, *Job) {
	t.Helper()
	manager.mu.Lock()
	bridge := manager.sessionBridge[session.Info.SessionID]
	manager.mu.Unlock()
	if bridge == nil {
		t.Fatal("no live bridge for the parent session")
	}
	observeHarnessFixture(manager, bridge, session.TabID, session.ChatID, session.Info.SessionID, map[string]any{
		"phase": harnessTurnPhaseStarted, "promptId": "wake-1", "humanAuthored": false,
	})
	job := bridge.jobForSession(session.Info.SessionID)
	if job == nil || !job.harnessTurn {
		t.Fatalf("self-started turn was not adopted: %#v", job)
	}
	return bridge, job
}

// Found live on v0.1.259: a child spawned while the chat was woken by a
// background task listed as running, but every later message, wait and cancel
// from its own parent answered "subagent not found for this owner". Only a human
// turn's end adopted its children; a self-started turn's end left them bound to
// a job that no longer existed.
func TestChildOfASelfStartedTurnStaysReachableAfterTheTurnEnds(t *testing.T) {
	t.Parallel()
	ends := map[string]func(*Manager, *Bridge, subagentParentSession){
		"turn-ended": func(manager *Manager, bridge *Bridge, session subagentParentSession) {
			observeHarnessFixture(manager, bridge, session.TabID, session.ChatID, session.Info.SessionID,
				harnessEndedUpdate("wake-1", []any{}, []any{}, "completed"))
		},
		"engine-died": func(manager *Manager, bridge *Bridge, session subagentParentSession) {
			manager.abandonAdoptedHarnessTurn(bridge, session.Info.SessionID)
		},
	}
	for name, end := range ends {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			manager, _, session, ownerKey, root, _ := newSubagentLifecycleFixture(t, "self-started-"+name)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			bridge, turn := startSelfStartedTurn(t, manager, session)
			run, err := manager.SpawnSubagent(ctx, SubagentSpawnOptions{
				OwnerKey: ownerKey, ParentChatID: session.ChatID, ParentTabID: session.TabID,
				Prompt: "[mock:active-without-terminal] child of a wake", Label: "wake-child",
				ProviderID: session.Info.ProviderID, ModelID: "mock-deterministic", Effort: "high", ModeID: "ask", CWD: root,
			})
			if err != nil {
				t.Fatalf("spawn from the self-started turn: %v", err)
			}
			if run.ParentJobID != turn.ID || run.Adopted {
				t.Fatalf("child of a running self-started turn = %#v, want bound to %s", run, turn.ID)
			}

			end(manager, bridge, session)
			if bridge.jobForSession(session.Info.SessionID) != nil {
				t.Fatal("the self-started turn did not end")
			}

			if result, err := manager.MessageSubagent(ownerKey, session.ChatID, session.TabID, run.ID, "note, no turn running"); err != nil || result["ok"] != true {
				t.Fatalf("message with no turn running = %#v err=%v", result, err)
			}
			next := startSubagentParent(t, manager, session, root, "[mock:active-without-terminal] the next human turn")
			if result, err := manager.MessageSubagent(ownerKey, session.ChatID, session.TabID, run.ID, "note, from a later turn"); err != nil || result["ok"] != true {
				t.Fatalf("message from a later turn = %#v err=%v", result, err)
			}
			listed := manager.ListSubagents(ownerKey, session.ChatID, session.TabID)
			if len(listed) != 1 || listed[0].ID != run.ID || listed[0].ParentJobID != asString(next["id"]) {
				t.Fatalf("list from a later turn = %#v, want the child rebound to %v", listed, next["id"])
			}
			// The child is still running, so this wait times out; it must not refuse the owner.
			if _, err := manager.WaitSubagent(ctx, ownerKey, session.ChatID, session.TabID, run.ID, 10*time.Millisecond); err == nil || strings.Contains(err.Error(), "not found") {
				t.Fatalf("wait from a later turn: %v, want a timeout on a found child", err)
			}
			if !manager.CancelSubagent(ownerKey, session.ChatID, session.TabID, run.ID) {
				t.Fatal("the parent could not cancel its own child")
			}
		})
	}
}

// The same report's visible clue. A self-started turn's job carried none of the
// chat's selection, so its children fell to the defaults: effort "high" instead of
// the chat's, the first catalog model when none was named, and a refused spawn
// when no cwd was named.
func TestChildOfASelfStartedTurnInheritsTheChatsSelection(t *testing.T) {
	t.Parallel()
	manager, _, session, ownerKey, root, _ := newSubagentLifecycleFixture(t, "self-started-selection")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	manager.mu.Lock()
	bridge := manager.sessionBridge[session.Info.SessionID]
	manager.mu.Unlock()
	if bridge == nil {
		t.Fatal("no live bridge for the parent session")
	}
	bridge.mu.Lock()
	bridge.durableModelSelection[session.Info.SessionID] = "mock-deterministic[low]"
	bridge.mu.Unlock()

	startSelfStartedTurn(t, manager, session)
	run, err := manager.SpawnSubagent(ctx, SubagentSpawnOptions{
		OwnerKey: ownerKey, ParentChatID: session.ChatID, ParentTabID: session.TabID,
		Prompt: "[mock:active-without-terminal] child of a wake", Label: "wake-child",
		ProviderID: session.Info.ProviderID, ModelID: "mock-deterministic",
	})
	if err != nil {
		t.Fatalf("spawn naming no cwd or effort: %v", err)
	}
	if run.CWD != root || run.Effort != "low" || run.ModelID != "mock-deterministic" {
		t.Fatalf("child = cwd %q effort %q model %q, want the chat's cwd %q and effort low", run.CWD, run.Effort, run.ModelID, root)
	}
	if run.ModeID != "ask" {
		t.Fatalf("child mode = %q, want the chat's", run.ModeID)
	}
}
