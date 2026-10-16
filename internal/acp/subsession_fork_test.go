package acp

import "testing"

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
