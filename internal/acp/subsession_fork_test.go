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
