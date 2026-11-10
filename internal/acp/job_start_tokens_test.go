package acp

import (
	"os"
	"path/filepath"
	"testing"

	"workass/internal/toolcli"
)

func jobStartTestManager(t *testing.T, stateDir, origin string) *Manager {
	t.Helper()
	m := NewManager(Options{
		StateDir: stateDir, SpawnedWorkReconcileInterval: 3600e9,
		WorkassToolsOrigin: origin, WorkassToolsCAFile: filepath.Join(stateDir, "ca.pem"),
	})
	t.Cleanup(func() { m.Reset() })
	return m
}

// A timer holds its credential for months: it must survive a daemon restart,
// follow the daemon to a new endpoint, and end when its chat is deleted.
func TestJobStartCredentialOutlivesARestartAndEndsWithItsChat(t *testing.T) {
	stateDir := t.TempDir()
	first := jobStartTestManager(t, stateDir, "https://tools.localhost:18001")
	binding, replayed, err := first.IssueJobStartToken("tab-a", "chat-a", "wa:m-x/chat/chat-a", "systemd tiering", "op-1")
	if err != nil || replayed {
		t.Fatalf("issue: %v replayed=%v", err, replayed)
	}
	config, err := toolcli.ReadConfig(binding.File)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := first.JobStartTokenFor(config.Credential); !ok {
		t.Fatal("the issued credential is not recognised")
	}
	if _, ok := first.RunTokenFor(config.Credential); ok {
		t.Fatal("a job-start credential must not pass as a one-shot run credential")
	}
	if _, _, ok := first.ownerBinding(config.Credential, "chat-a", "tab-a"); ok {
		t.Fatal("a job-start credential must not authenticate as its chat")
	}
	first.Reset()

	second := jobStartTestManager(t, stateDir, "https://tools.localhost:18002")
	if _, ok := second.JobStartTokenFor(config.Credential); !ok {
		t.Fatal("the credential did not survive a restart")
	}
	moved, err := toolcli.ReadConfig(binding.File)
	if err != nil || moved.Endpoint != "https://tools.localhost:18002/workass/tools" || moved.Credential != config.Credential {
		t.Fatalf("the file did not follow the daemon to its new endpoint: %#v %v", moved, err)
	}
	if listed := second.ListJobStartTokens("tab-a", "chat-a"); len(listed) != 1 || listed[0].ID != binding.ID {
		t.Fatalf("list = %#v", listed)
	}
	if _, err := second.RevokeJobStartToken("tab-b", "chat-b", binding.ID); err == nil {
		t.Fatal("another chat revoked this chat's credential")
	}
	second.RevokeJobStartTokensForChat("tab-a", "chat-a")
	if _, ok := second.JobStartTokenFor(config.Credential); ok {
		t.Fatal("a deleted chat's credential still works")
	}
	if _, err := os.Stat(filepath.Dir(binding.File)); !os.IsNotExist(err) {
		t.Fatal("a deleted chat's credential file is still there")
	}
}
