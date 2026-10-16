package acp

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// The vector comes from stableMessageUUID in scripts/claude-native-host.mjs;
// a drift here would cut a forked subsession at the wrong message.
func TestClaudeUserMessageUUIDsMatchTheHost(t *testing.T) {
	got := claudeUserMessageUUIDs("op:turn-7", "2f1c6a52-6b4e-4f8e-9a51-1d2a3b4c5d6e", "2f1c6a52-6b4e-4f8e-9a51-1d2a3b4c5d6e", "")
	if len(got) != 1 || got[0] != "af332bec-0487-5f4c-a98a-4e2da38883d7" {
		t.Fatalf("claudeUserMessageUUIDs = %v", got)
	}
	if claudeUserMessageUUIDs(" ", "s") != nil {
		t.Fatal("an empty operation id must produce no candidates")
	}
}

func TestNativeSessionForkSupportIsClaudeOnly(t *testing.T) {
	if !NativeSessionForkSupported("claude") {
		t.Fatal("claude must support native session forks")
	}
	if NativeSessionForkSupported("codex") {
		t.Fatal("codex has no native session fork")
	}
}

// A forked subagent's header carries its fork mark into the typed event, so
// the parent transcript keeps ⑂ on the child's row without a live work item.
func TestSubagentHeaderCarriesTheForkMark(t *testing.T) {
	forked := providerToolEvent(map[string]any{
		"kind": "tool", "toolKind": "agent", "id": "wa-subagent-1", "subagentId": "wa-subagent-1",
		"subagentHeader": true, "subagentForked": true,
	}, nil)
	if forked == nil || !forked.SubagentForked || !forked.SubagentHeader {
		t.Fatalf("forked header = %#v", forked)
	}
	fresh := providerToolEvent(map[string]any{"kind": "tool", "id": "wa-subagent-2", "subagentId": "wa-subagent-2", "subagentHeader": true}, nil)
	if fresh == nil || fresh.SubagentForked {
		t.Fatalf("a fresh header claimed a fork: %#v", fresh)
	}
}

// Review wa-subagent-1792180415563-2, suggestion 6: the pinned native fork is
// bound as the child's committed lane, selection then resumes it exactly
// (Established, never session/new), replays are idempotent, and a provider
// without the native host refuses instead of handing out a fresh session.
func TestForkedSubsessionBindsItsPinnedForkAndSelectionResumesIt(t *testing.T) {
	root, stateDir := repoRoot(t), t.TempDir()
	manager := NewManager(Options{
		RootDir: root, StateDir: stateDir, MachineID: "fork-machine",
		Providers: []ProviderConfig{{
			ID: "claude", Name: "Claude fixture", Command: "node",
			Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Enabled: true,
		}},
		DefaultProviderID: "claude", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	if err := manager.nativeSessions.put(nativeSessionBinding{
		TabID: "parent-tab", ChatID: "parent-chat", ProviderID: "claude", SessionID: "0b9c9a3e-1f6f-4d55-9a07-3c2c9a8b0001",
		ThreadCommitted: true, CWD: root, WorkspaceEpoch: string(WorkspaceEpochForRevision("parent-chat", root, 0)),
	}); err != nil {
		t.Fatal(err)
	}
	child := SessionOptions{
		TabID: "child-tab", ChatID: "child-chat", ProviderID: "claude", CWD: root,
		WorkspaceEpoch: WorkspaceEpochForRevision("child-chat", root, 0),
	}
	request := SubsessionForkRequest{SourceChatID: "parent-chat", SourceSessionID: "0b9c9a3e-1f6f-4d55-9a07-3c2c9a8b0001", Child: child}
	const forked = "7a1c2b3d-4e5f-4061-8a9b-0c1d2e3f4a5b"
	if err := manager.BindForkedSubsessionLane(request, forked); err != nil {
		t.Fatalf("bind pinned fork: %v", err)
	}
	if err := manager.BindForkedSubsessionLane(request, forked); err != nil {
		t.Fatalf("a replay must re-bind the same fork: %v", err)
	}
	if err := manager.BindForkedSubsessionLane(request, "9f8e7d6c-5b4a-4321-8fed-cba987654321"); err == nil {
		t.Fatal("a different native session replaced the child's fork")
	}
	selection, err := manager.ResolveProviderLaneSelection(context.Background(), child)
	if err != nil {
		t.Fatal(err)
	}
	if !selection.Established || selection.Thread.HeadID != forked || selection.Thread.RootID != forked {
		t.Fatalf("selection = established %v thread %#v; want an exact resume of the fork", selection.Established, selection.Thread)
	}
	parent, _ := manager.nativeSessions.getForOptions(SessionOptions{TabID: "parent-tab", ChatID: "parent-chat", ProviderID: "claude", CWD: root, WorkspaceEpoch: WorkspaceEpochForRevision("parent-chat", root, 0)})
	if selection.Identity.Realm.AccountScope != parent.AccountScope || selection.Identity.Realm.InstallScope != parent.InstallScope {
		t.Fatalf("the child's realm %#v differs from its parent's binding", selection.Identity.Realm)
	}
	if _, err := manager.ForkSubsessionSession(context.Background(), SubsessionForkRequest{
		SourceChatID: "parent-chat", SourceSessionID: "0b9c9a3e-1f6f-4d55-9a07-3c2c9a8b0001",
		Child: SessionOptions{TabID: "other-tab", ChatID: "other-chat", ProviderID: "claude", CWD: root},
	}); err == nil {
		t.Fatal("a launch without the Agent SDK host produced a fork")
	}
	if err := manager.BindForkedSubsessionLane(SubsessionForkRequest{
		SourceChatID: "parent-chat", SourceSessionID: "0b9c9a3e-1f6f-4d55-9a07-3c2c9a8b0001",
		Child: SessionOptions{TabID: "elsewhere-tab", ChatID: "elsewhere-chat", ProviderID: "claude", CWD: t.TempDir()},
	}, forked); err == nil {
		t.Fatal("a forked subsession left its parent's workspace")
	}
}
