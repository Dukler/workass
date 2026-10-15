package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

func TestRuntimeBackgroundOwnerRequiresExactOrigin(t *testing.T) {
	state, err := chat.NewState("chat")
	if err != nil {
		t.Fatal(err)
	}
	apply := func(command chat.Command) {
		t.Helper()
		next, _, reduceErr := chat.Reduce(state, command)
		if reduceErr != nil {
			t.Fatalf("apply %T: %v", command, reduceErr)
		}
		state = next
	}
	realm := providercontract.Realm{
		ProviderID: "codex", MachineID: "machine", AccountScope: "account", InstallScope: "install", Verified: true,
	}
	identity := providercontract.LaneIdentity{ChatID: "chat", WorkspaceEpoch: "workspace-1", Realm: realm}.Normalize()
	apply(chat.InitializeChat{Presentation: chat.PresentationState{TabID: "tab"}, OperationID: "create:background", Digest: "create-background"})
	apply(chat.SelectLane{Identity: identity, Owner: providercontract.AttachmentOwner{TabID: "tab"}})
	apply(chat.LaneOpened{
		LaneID: identity.ID, Thread: providercontract.ThreadRef{ProviderID: "codex", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1, Context: providercontract.ContextCapabilities{ExactResume: true},
	})
	apply(chat.Submit{OperationID: "turn-op", Text: "run it", Presentation: providercontract.TurnPresentation{Origin: "human"}})
	apply(chat.TurnAdmitted{OperationID: "turn-op", Accepted: true, Turn: providercontract.TurnRef{OperationID: "turn-op", NativeID: "native-turn"}})

	if owner, ok := exactBackgroundOwner(state, acp.SpawnedWorkItem{ID: "work", ProviderID: "codex"}); ok {
		t.Fatalf("ownerless live item guessed historical owner %#v", owner)
	}
	item := acp.SpawnedWorkItem{ID: "work", ProviderID: "codex", OriginOperationID: "turn-op"}
	owner, ok := exactBackgroundOwner(state, item)
	if !ok || owner.LaneID != identity.ID || owner.OperationID != "turn-op" || owner.TurnID != "native-turn" {
		t.Fatalf("exact operation owner = %#v, %v", owner, ok)
	}
	item.OriginTurnID = "another-turn"
	if owner, ok := exactBackgroundOwner(state, item); ok {
		t.Fatalf("mismatched operation/turn origin was accepted: %#v", owner)
	}
	child := acp.SpawnedWorkItem{ID: "child", Kind: "subagent", ProviderID: "claude", AssistantBrand: "Claude",
		OriginOperationID: "turn-op", OriginTurnID: "native-turn"}
	childOwner, ok := exactBackgroundOwner(state, child)
	if !ok || childOwner != owner {
		t.Fatal("cross-provider child lost exact parent origin")
	}
	childEvent := backgroundEvent(child, child.ID)
	backgroundChild := chat.BackgroundState{Owner: childOwner, Event: childEvent}
	if actorSubagentRun(state, backgroundChild).ProviderID != "claude" || actorBackgroundWorkItem(state, backgroundChild).AssistantBrand != "Claude" {
		t.Fatal("child projection borrowed parent provider")
	}
	child.OriginTurnID = "wrong-turn"
	if _, ok := exactBackgroundOwner(state, child); ok {
		t.Fatal("cross-provider exemption bypassed exact turn ownership")
	}
	child.OriginTurnID, child.Kind = "native-turn", "native-agent"
	if _, ok := exactBackgroundOwner(state, child); ok {
		t.Fatal("native background work bypassed origin provider fence")
	}

	valid := acp.SpawnedWorkItem{ID: "owned", TaskID: "owned", TabID: "tab", ChatID: "chat", ProviderID: "codex", Status: "running", OriginOperationID: "turn-op"}
	orphan := acp.SpawnedWorkItem{ID: "orphan", TaskID: "orphan", TabID: "tab", ChatID: "chat", ProviderID: "codex", Status: "running"}
	background, accepted := actorOwnedBackgroundSnapshot(state, []acp.SpawnedWorkItem{orphan, valid})
	if len(background) != 1 || background[0].Event.WorkID != "owned" || len(accepted) != 1 || accepted[0].ID != "owned" {
		t.Fatalf("actor-owned projection = background:%#v accepted:%#v", background, accepted)
	}

}

func TestNativeCodexBackgroundLifecycleIsOwnedByChatActor(t *testing.T) {
	t.Parallel()
	root, stateDir := repoRoot(t), t.TempDir()
	releaseFile := filepath.Join(stateDir, "release-child")
	args, err := json.Marshal([]string{filepath.Join(root, "desktop", "acp", "mock-codex-app-server.mjs")})
	if err != nil {
		t.Fatal(err)
	}
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev", DefaultProviderID: "codex", InitTimeout: 10 * time.Second, RSSSampleInterval: time.Hour,
		Provider: acp.ProviderConfig{ID: "codex", Command: "node", Args: []string{filepath.Join(root, "scripts", "codex-native-host.mjs")}, CWD: root, Enabled: true,
			Env: map[string]string{"WORKASS_CODEX_EXECUTABLE": "node", "WORKASS_CODEX_APP_SERVER_ARGS": string(args), "WORKASS_CODEX_FIXTURE_THREAD_ID": "actor-background-parent", "WORKASS_CODEX_FIXTURE_CHILD_RELEASE_FILE": releaseFile}},
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := runtime.CreateRendererChat(map[string]any{"tabId": "bg-tab", "chatId": "bg-chat", "operationId": "bg-create", "cwd": root, "providerId": "codex"}); err != nil {
		t.Fatal(err)
	}
	info, err := runtime.Select(ctx, acp.SessionOptions{TabID: "bg-tab", ChatID: "bg-chat", ProviderID: "codex", CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Start(ctx, map[string]any{"kind": "app-chat", "tabId": "bg-tab", "chatId": "bg-chat", "sessionId": info.SessionID, "operationId": "bg-turn", "userMessageId": "bg-user", "assistantMessageId": "bg-assistant", "prompt": "[fixture:native-background]"}, "human"); err != nil {
		t.Fatal(err)
	}
	waitProviderChatIdle(t, runtime, "bg-chat", 5*time.Second)
	state, _ := runtime.Snapshot("bg-chat")
	if len(state.Background) != 1 {
		t.Fatalf("actor lost child after parent completion: %#v", state.Background)
	}
	var workID string
	var owner chat.ProviderActivityOwner
	for id, item := range state.Background {
		workID, owner = id, item.Owner
		if item.Event.Status != "running" || owner.OperationID != "bg-turn" || item.Event.ModelLabel != "gpt-fixture-mini" {
			t.Fatalf("incorrect actor-owned child: %#v", item)
		}
	}
	if err := os.WriteFile(releaseFile, []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RefreshPlanUsageSession(ctx, acp.SessionOptions{SessionID: info.SessionID, TabID: "bg-tab", ChatID: "bg-chat", ProviderID: "codex"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state, _ = runtime.Snapshot("bg-chat")
		if state.Background[workID].Event.Status == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	child := state.Background[workID]
	if child.Event.Status != "failed" || child.Event.Summary != "Background review failed" || child.Owner != owner || state.Foreground != nil {
		t.Fatalf("late child result changed parent/ownership or was lost: child=%#v foreground=%#v", child, state.Foreground)
	}
	if err := runtime.Close(ctx); err != nil {
		t.Fatal(err)
	}
	manager.Reset()
	restoredManager := acp.NewManager(acp.Options{RootDir: root, StateDir: stateDir, RuntimeProfile: "dev"})
	t.Cleanup(func() { restoredManager.Reset() })
	restored := newTestProviderChatRuntime(t, restoredManager, sharedSessionStore(stateDir), stateDir)
	replay, _ := restored.Snapshot("bg-chat")
	if replay.Background[workID].Event.Status != "failed" || replay.Background[workID].Owner != owner {
		t.Fatalf("restart lost child receipt: %#v", replay.Background)
	}
}

// A Claude Code Agent-tool subagent launched in a turn and finishing after the
// turn ended (it wakes the idle session) must keep its entry: same kind, same
// title, same start time (DepthVision wake test, 2026-10-13). It used to come
// back as kind=background, title=raw id, started=finished.
func TestClaudeBackgroundAgentKeepsItsEntryWhenItFinishesAfterTheTurn(t *testing.T) {
	t.Parallel()
	root, stateDir := repoRoot(t), t.TempDir()
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev", DefaultProviderID: "claude", InitTimeout: 10 * time.Second,
		RSSSampleInterval: time.Hour, SpawnedWorkReconcileInterval: time.Hour,
		Provider: acp.ProviderConfig{ID: "claude", Command: "node", Args: []string{filepath.Join(root, "scripts", "claude-native-host.mjs")}, CWD: root, Enabled: true,
			Env: map[string]string{
				"WORKASS_CLAUDE_SDK_MODULE": filepath.Join(root, "desktop", "acp", "mock-claude-agent-sdk.mjs"),
				"WORKASS_CLAUDE_EXECUTABLE": "/fixture/claude",
				"WORKASS_CLAUDE_SESSION_ID": "fixture-bg-wake-session",
			}},
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := runtime.CreateRendererChat(map[string]any{"tabId": "wake-tab", "chatId": "wake-chat", "operationId": "wake-create", "cwd": root, "providerId": "claude"}); err != nil {
		t.Fatal(err)
	}
	info, err := runtime.Select(ctx, acp.SessionOptions{TabID: "wake-tab", ChatID: "wake-chat", ProviderID: "claude", CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Start(ctx, map[string]any{"kind": "app-chat", "tabId": "wake-tab", "chatId": "wake-chat", "sessionId": info.SessionID, "operationId": "wake-turn", "userMessageId": "wake-user", "assistantMessageId": "wake-assistant", "prompt": "[fixture:bg-agent-wake]"}, "human"); err != nil {
		t.Fatal(err)
	}
	var running *acp.SpawnedWorkItem
	var saved providercontract.BackgroundEvent
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, item := range manager.ListSpawnedWork("wake-tab", "wake-chat") {
			if item.TaskID == "a-bg-wake" && item.Status == "running" && running == nil {
				copy := item
				running = &copy
			}
		}
		state, _ := runtime.Snapshot("wake-chat")
		if saved = state.Background["a-bg-wake"].Event; saved.Status != "" && saved.Status != "running" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if running == nil || running.Kind != "agent" || running.Label != "Wake test agent" {
		t.Fatalf("running agent entry = %#v", running)
	}
	// Finished entries leave the live list; the chat actor keeps them.
	if saved.Status == "running" || saved.Kind != "agent" || saved.Title != "Wake test agent" || saved.StartedAt != running.StartedAt || saved.StartedAt == saved.FinishedAt {
		t.Fatalf("finishing replaced the entry instead of updating it:\nrunning: %#v\nsaved: %#v", running, saved)
	}
}

// The DepthVision wake test replayed end to end: every entry keeps its kind,
// title and start time through the wakes that settle it.
func TestClaudeWakeTestEntriesKeepTheirIdentityUntilTheyFinish(t *testing.T) {
	t.Parallel()
	root, stateDir := repoRoot(t), t.TempDir()
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev", DefaultProviderID: "claude", InitTimeout: 10 * time.Second,
		RSSSampleInterval: time.Hour, SpawnedWorkReconcileInterval: time.Hour,
		Provider: acp.ProviderConfig{ID: "claude", Command: "node", Args: []string{filepath.Join(root, "scripts", "claude-native-host.mjs")}, CWD: root, Enabled: true,
			Env: map[string]string{
				"WORKASS_CLAUDE_SDK_MODULE": filepath.Join(root, "desktop", "acp", "mock-claude-agent-sdk.mjs"),
				"WORKASS_CLAUDE_EXECUTABLE": "/fixture/claude",
				"WORKASS_CLAUDE_SESSION_ID": "fixture-bg-wake-full-session",
			}},
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := runtime.CreateRendererChat(map[string]any{"tabId": "full-tab", "chatId": "full-chat", "operationId": "full-create", "cwd": root, "providerId": "claude"}); err != nil {
		t.Fatal(err)
	}
	info, err := runtime.Select(ctx, acp.SessionOptions{TabID: "full-tab", ChatID: "full-chat", ProviderID: "claude", CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Start(ctx, map[string]any{"kind": "app-chat", "tabId": "full-tab", "chatId": "full-chat", "sessionId": info.SessionID, "operationId": "full-turn", "userMessageId": "full-user", "assistantMessageId": "full-assistant", "prompt": "[fixture:bg-wake-full]"}, "human"); err != nil {
		t.Fatal(err)
	}
	want := map[string]struct{ kind, label string }{
		"b-wake-shell":   {"bash", "Wake test: shell task"},
		"a-wake-agent":   {"agent", "Wake test: agent"},
		"b-wake-monitor": {"monitor", "Wake test: watcher"},
		"b-wake-child":   {"bash", "Sleep 40 seconds"},
	}
	first := map[string]acp.SpawnedWorkItem{}
	agentAfterInterim := ""
	var state chat.State
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, item := range manager.ListSpawnedWork("full-tab", "full-chat") {
			if _, ok := first[item.TaskID]; !ok {
				first[item.TaskID] = item
			}
			if item.TaskID == "a-wake-agent" && strings.HasPrefix(item.Summary, "Stopped with background work") && agentAfterInterim == "" {
				agentAfterInterim = item.Status
			}
		}
		state, _ = runtime.Snapshot("full-chat")
		if state.Background["a-wake-agent"].Event.Summary == "agent wake test done" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if agentAfterInterim != "running" {
		t.Fatalf("an agent that stopped with its own work still running was shown as %q, not still running", agentAfterInterim)
	}
	for id, expected := range want {
		saved := state.Background[id].Event
		seen := first[id]
		if saved.Kind != expected.kind || saved.Title != expected.label || saved.StartedAt == "" || saved.StartedAt != seen.StartedAt || saved.Status == "running" {
			t.Fatalf("%s lost its identity:\nfirst seen: %#v\nsaved: %#v", id, seen, saved)
		}
	}
	if state.Background["b-wake-child"].Event.ParentTaskID != "a-wake-agent" {
		t.Fatalf("the agent's own command lost its parent: %#v", state.Background["b-wake-child"].Event)
	}
	if agent := state.Background["a-wake-agent"].Event; agent.FinishedAt < state.Background["b-wake-child"].Event.FinishedAt {
		t.Fatalf("the agent settled before its own work finished: agent %s, child %s", agent.FinishedAt, state.Background["b-wake-child"].Event.FinishedAt)
	}
}

// A Claude agent still running when the daemon is killed cannot survive it:
// every provider process ends with the daemon. Reloaded as "running", it had
// nothing left to end it while its chat kept working (observed 2026-10-14
// after the 0.1.214 install). The restarted daemon must settle it.
func TestClaudeAgentRunningAtDaemonKillIsOrphanedOnRestart(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	newManager := func(stateDir string) *acp.Manager {
		return acp.NewManager(acp.Options{
			RootDir: root, StateDir: stateDir, RuntimeProfile: "dev", DefaultProviderID: "claude", InitTimeout: 10 * time.Second,
			RSSSampleInterval: time.Hour, SpawnedWorkReconcileInterval: time.Hour,
			Provider: acp.ProviderConfig{ID: "claude", Command: "node", Args: []string{filepath.Join(root, "scripts", "claude-native-host.mjs")}, CWD: root, Enabled: true,
				Env: map[string]string{
					"WORKASS_CLAUDE_SDK_MODULE": filepath.Join(root, "desktop", "acp", "mock-claude-agent-sdk.mjs"),
					"WORKASS_CLAUDE_EXECUTABLE": "/fixture/claude",
					"WORKASS_CLAUDE_SESSION_ID": "fixture-stranded-session",
				}},
		})
	}
	firstDir := t.TempDir()
	first := newManager(firstDir)
	t.Cleanup(func() { first.Reset() })
	runtime := newTestProviderChatRuntime(t, first, sharedSessionStore(firstDir), firstDir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := runtime.CreateRendererChat(map[string]any{"tabId": "stranded-tab", "chatId": "stranded-chat", "operationId": "stranded-create", "cwd": root, "providerId": "claude"}); err != nil {
		t.Fatal(err)
	}
	info, err := runtime.Select(ctx, acp.SessionOptions{TabID: "stranded-tab", ChatID: "stranded-chat", ProviderID: "claude", CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Start(ctx, map[string]any{"kind": "app-chat", "tabId": "stranded-tab", "chatId": "stranded-chat", "sessionId": info.SessionID, "operationId": "stranded-turn", "userMessageId": "stranded-user", "assistantMessageId": "stranded-assistant", "prompt": "[fixture:stranded-agent]"}, "human"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		state, _ := runtime.Snapshot("stranded-chat")
		if state.Background["a-stranded"].Event.Status == "running" {
			if _, busy := first.RunningJobForChat("stranded-tab", "stranded-chat"); !busy {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent never ran in a finished turn: %#v", state.Background)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The daemon is killed here: what is on disk at this instant is all the
	// next daemon gets. Copy it rather than shutting down cleanly.
	secondDir := t.TempDir()
	if err := os.CopyFS(secondDir, os.DirFS(firstDir)); err != nil {
		t.Fatal(err)
	}
	second := newManager(secondDir)
	t.Cleanup(func() { second.Reset() })
	restarted := newTestProviderChatRuntime(t, second, sharedSessionStore(secondDir), secondDir)
	state, ok := restarted.Snapshot("stranded-chat")
	if !ok {
		t.Fatal("the restarted daemon lost the chat")
	}
	if got := state.Background["a-stranded"].Event.Status; got != "orphaned" {
		t.Fatalf("an agent whose Claude process died with the daemon reads %q after restart", got)
	}
}

// A subagent's tool calls reach the chat filed under its Agent call, so the
// transcript (which shows only the main agent's own calls and images) leaves
// them to the subagent's row. Unattributed, a labeling run filled the chat
// with 1,200 image reads (DepthVision speed, 2026-10-15).
func TestClaudeSubagentToolCallsReachTheChatUnderTheirAgent(t *testing.T) {
	t.Parallel()
	root, stateDir := repoRoot(t), t.TempDir()
	var mu sync.Mutex
	var reads []map[string]any
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev", DefaultProviderID: "claude", InitTimeout: 10 * time.Second,
		RSSSampleInterval: time.Hour, SpawnedWorkReconcileInterval: time.Hour,
		Provider: acp.ProviderConfig{ID: "claude", Command: "node", Args: []string{filepath.Join(root, "scripts", "claude-native-host.mjs")}, CWD: root, Enabled: true,
			Env: map[string]string{
				"WORKASS_CLAUDE_SDK_MODULE": filepath.Join(root, "desktop", "acp", "mock-claude-agent-sdk.mjs"),
				"WORKASS_CLAUDE_EXECUTABLE": "/fixture/claude",
				"WORKASS_CLAUDE_SESSION_ID": "fixture-subagent-reads-session",
			}},
		Broadcast: func(channel string, payload any) {
			if channel != "job:event" {
				return
			}
			event := mapFromAnyMain(mapFromAnyMain(payload)["event"])
			if fieldString(event, "id") == "toolu_sub_read" {
				mu.Lock()
				reads = append(reads, event)
				mu.Unlock()
			}
		},
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := runtime.CreateRendererChat(map[string]any{"tabId": "sub-tab", "chatId": "sub-chat", "operationId": "sub-create", "cwd": root, "providerId": "claude"}); err != nil {
		t.Fatal(err)
	}
	info, err := runtime.Select(ctx, acp.SessionOptions{TabID: "sub-tab", ChatID: "sub-chat", ProviderID: "claude", CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Start(ctx, map[string]any{"kind": "app-chat", "tabId": "sub-tab", "chatId": "sub-chat", "sessionId": info.SessionID, "operationId": "sub-turn", "userMessageId": "sub-user", "assistantMessageId": "sub-assistant", "prompt": "[fixture:subagent-reads]"}, "human"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		mu.Lock()
		got := append([]map[string]any(nil), reads...)
		mu.Unlock()
		if len(got) > 0 {
			for _, event := range got {
				if fieldString(event, "subagentId") != "toolu_sub_agent" {
					t.Fatalf("a subagent's read reached the chat as the main agent's own: %#v", event)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the subagent's read never reached the chat")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
