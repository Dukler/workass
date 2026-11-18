package acp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bug-20261118-afbfeed6: a daemon update replaced the process, so the
// in-memory subagent registry was empty, and workass_retry_subagent refused
// every subagent the update had orphaned with "subagent not found for this
// owner" while workass_list_subagents still showed them as failed.
func TestRetrySubagentAfterDaemonRestartRebuildsFromDurableSpec(t *testing.T) {
	t.Parallel()
	first, _, session, ownerKey, root, stateDir := newSubagentLifecycleFixture(t, "retry-restart")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	original, err := first.SpawnSubagent(ctx, SubagentSpawnOptions{
		OwnerKey: ownerKey, ParentChatID: session.ChatID, ParentTabID: session.TabID,
		Prompt: "survive the update", Label: "orphaned-by-update",
		ProviderID: session.Info.ProviderID, ModelID: "mock-deterministic", Effort: "high", ModeID: "ask", CWD: root,
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	// The spec is durable from the spawn on: a run the restart kills mid-flight
	// never reaches a terminal write, so nothing written later could serve it.
	if spec, ok := first.loadSubagentRetrySpec(session.ChatID, session.TabID, original.ID); !ok || spec.Prompt != "survive the update" {
		t.Fatalf("spec right after spawn = %#v ok=%v", spec, ok)
	}
	waitForSubagentTerminal(t, first, ownerKey, session, original.ID, 10*time.Second)
	first.Reset()

	// A new process over the same state directory: the daemon after an update.
	second := NewManager(Options{
		RootDir: root, StateDir: stateDir,
		Provider:            ProviderConfig{Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Label: "Workass Mock ACP"},
		Broadcast:           newEventCollector().Broadcast,
		PermissionTimeout:   8 * time.Second,
		StdoutFlushInterval: 10 * time.Millisecond, ThoughtFlushInterval: 10 * time.Millisecond,
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { second.Reset() })

	// The permission ceiling reads the stored mode when the registry has no run.
	if provider, mode, ok := second.StoredSubagentPermissionMode(session.ChatID, session.TabID, original.ID); !ok || mode != "ask" || provider != original.ProviderID {
		t.Fatalf("stored mode = %q %q %v", provider, mode, ok)
	}
	if _, _, ok := second.StoredSubagentPermissionMode("chat-intruder", "tab-intruder", original.ID); ok {
		t.Fatal("another chat read this chat's stored mode")
	}

	// Another chat never reaches this chat's spec.
	_, err = second.WithActorOwner("chat-intruder", "tab-intruder", func(key string) (any, error) {
		return second.RetrySubagent(ctx, key, "chat-intruder", "tab-intruder", original.ID, "")
	})
	if err == nil || !strings.Contains(err.Error(), "not found for this owner") {
		t.Fatalf("other chat's retry err = %v", err)
	}

	// The production path: the chat actor's background action, owned by the
	// chat with no live turn and no provider session yet.
	var retried, done SubagentRun
	_, err = second.WithActorOwner(session.ChatID, session.TabID, func(key string) (any, error) {
		var retryErr error
		retried, retryErr = second.RetrySubagent(ctx, key, session.ChatID, session.TabID, original.ID, "pick up where the update cut you off")
		if retryErr != nil {
			return nil, retryErr
		}
		done, retryErr = second.WaitSubagent(ctx, key, session.ChatID, session.TabID, retried.ID, 10*time.Second)
		return nil, retryErr
	})
	if err != nil {
		t.Fatalf("retry after restart: %v", err)
	}
	if retried.RetryOf != original.ID || retried.Label != "orphaned-by-update" || retried.ModelID != "mock-deterministic" ||
		retried.Effort != "high" || retried.ModeID != "ask" || retried.CWD != original.CWD {
		t.Fatalf("retry after restart = %#v, original %#v", retried, original)
	}
	second.mu.Lock()
	prompt := second.subagents[retried.ID].prompt
	second.mu.Unlock()
	if !strings.HasPrefix(prompt, "survive the update") || !strings.Contains(prompt, "pick up where the update cut you off") {
		t.Fatalf("retried prompt = %q", prompt)
	}
	if done.Status != "done" {
		t.Fatalf("retried run = %#v", done)
	}

	// A deleted chat takes its stored prompts with it.
	second.ForgetChat(ctx, session.TabID, session.ChatID, "forget-retry-restart")
	if _, ok := second.loadSubagentRetrySpec(session.ChatID, session.TabID, original.ID); ok {
		t.Fatal("spec survived the chat's deletion")
	}
}
