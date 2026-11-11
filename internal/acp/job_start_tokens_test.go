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

// The in-app updater boots a test daemon on a clone of the state. The clone's
// token index still names the live credential files by absolute path, so that
// daemon must leave every file outside its own state directory alone: a timer
// holding the live file would otherwise be pointed at the test daemon's
// temporary certificate and state, and every call after the update would fail.
func TestJobStartCredentialFileOutsideThisStateIsNeverRewritten(t *testing.T) {
	liveState := t.TempDir()
	live := jobStartTestManager(t, liveState, "https://tools.localhost:18001")
	binding, _, err := live.IssueJobStartToken("tab-a", "chat-a", "wa:m-x/chat/chat-a", "daily builder", "op-1")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(binding.File)
	if err != nil {
		t.Fatal(err)
	}
	live.Reset()

	cloneState := t.TempDir()
	index := filepath.Join("tool-contexts", "job-start-tokens.json")
	data, err := os.ReadFile(filepath.Join(liveState, index))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cloneState, "tool-contexts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cloneState, index), data, 0o600); err != nil {
		t.Fatal(err)
	}
	clone := jobStartTestManager(t, cloneState, "https://tools.localhost:61331")
	config, err := toolcli.ReadConfig(binding.File)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := clone.JobStartTokenFor(config.Credential); !ok {
		t.Fatal("the clone lost the credential")
	}
	if _, replayed, err := clone.IssueJobStartToken("tab-a", "chat-a", "wa:m-x/chat/chat-a", "daily builder", "op-1"); err != nil || !replayed {
		t.Fatalf("replay: %v replayed=%v", err, replayed)
	}
	after, err := os.ReadFile(binding.File)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("a daemon on another state directory rewrote the live credential file:\nbefore %s\nafter  %s", before, after)
	}
	// Deleted by hand, the replay must not recreate it from the clone either.
	if err := os.Remove(binding.File); err != nil {
		t.Fatal(err)
	}
	if _, _, err := clone.IssueJobStartToken("tab-a", "chat-a", "wa:m-x/chat/chat-a", "daily builder", "op-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(binding.File); !os.IsNotExist(err) {
		t.Fatal("the clone wrote a credential file into the live state directory")
	}
}

// A file the buggy updater already pointed at a vanished temporary directory is
// repaired by the live daemon's next boot.
func TestJobStartCredentialFileDamagedByAnUpdateIsRepairedAtBoot(t *testing.T) {
	stateDir := t.TempDir()
	first := jobStartTestManager(t, stateDir, "https://tools.localhost:18001")
	binding, _, err := first.IssueJobStartToken("tab-a", "chat-a", "wa:m-x/chat/chat-a", "daily builder", "op-1")
	if err != nil {
		t.Fatal(err)
	}
	config, err := toolcli.ReadConfig(binding.File)
	if err != nil {
		t.Fatal(err)
	}
	first.Reset()
	damaged := config
	damaged.CAFile, damaged.StateDir = "/var/folders/x/workass-preflight-1/data/state/daemon-cert.pem", "/var/folders/x/workass-preflight-1/data/state"
	if err := toolcli.WriteConfig(binding.File, damaged); err != nil {
		t.Fatal(err)
	}
	second := jobStartTestManager(t, stateDir, "https://tools.localhost:18001")
	_ = second
	fixed, err := toolcli.ReadConfig(binding.File)
	if err != nil || fixed.CAFile != filepath.Join(stateDir, "ca.pem") || fixed.StateDir != stateDir {
		t.Fatalf("not repaired: %#v %v", fixed, err)
	}
}
