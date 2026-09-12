package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
