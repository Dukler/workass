package acp

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A hosted Claude subagent whose login has expired must fail its row fast and
// name the reason, never sit at phase "working" until an operator cancels it
// (live bug, dukler-laptop 2026-10-31: `claude -p` printed "Failed to
// authenticate: OAuth session expired and could not be refreshed" in seconds,
// but the hosted row stayed on "Running delegated task" for minutes).
//
// The fixture SDK's WORKASS_CLAUDE_FIXTURE_PERSISTENT_OAUTH_RESULT makes EVERY
// attempt — including the host's own one-shot retry — fail with that exact
// text, which is what a durably-expired credential looks like (as opposed to
// WORKASS_CLAUDE_FIXTURE_TRANSIENT_OAUTH_RESULT's single blip, already covered
// by TestClaudeNativeTurnRecoversTransientOAuthRefreshWithoutFalseAssistantAnswer).
func TestHostedClaudeSubagentFailsFastOnPersistentAuthFailure(t *testing.T) {
	root := repoRoot(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not available for Claude native-host test: %v", err)
	}
	cliDir := t.TempDir()
	claude := filepath.Join(cliDir, executableName("claude"))
	writeExecutable(t, claude, nativeNoopScript())
	t.Setenv("WORKASS_NODE", node)
	t.Setenv("WORKASS_CLAUDE_HOST", filepath.Join(root, "scripts", "claude-native-host.mjs"))
	t.Setenv("WORKASS_CLAUDE_SDK_MODULE", filepath.Join(root, "desktop", "acp", "mock-claude-agent-sdk.mjs"))
	t.Setenv("WORKASS_CLAUDE_FIXTURE_PERSISTENT_OAUTH_RESULT", "1")
	t.Setenv("WORKASS_CLAUDE_SESSION_ID", "fixture-hosted-auth-failure-session")

	events := newEventCollector()
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "claude", Name: "Claude Code", Command: claude, Enabled: true, Badge: "native", CWD: root,
		}},
		DefaultProviderID:  "claude",
		ProviderConfigFile: filepath.Join(t.TempDir(), "providers.json"),
		Broadcast:          events.Broadcast,
		InitTimeout:        5 * time.Second,
		RSSSampleInterval:  time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	// HostedForOrigin is the exact shape of the live bug: a chat on another
	// machine asked this machine to run its delegated task.
	run, err := manager.SpawnSubagent(context.Background(), SubagentSpawnOptions{
		HostedForOrigin: true, OriginMachine: "m-laptop-under-test", OriginChatID: "chat-on-the-asking-machine",
		Prompt: "run the delegated task", ProviderID: "claude", CWD: root, PermissionIntent: "full",
	})
	if err != nil {
		t.Fatalf("spawn hosted Claude subagent: %v", err)
	}

	// The real symptom was "minutes, then a manual cancel". A fast failure must
	// settle in low single-digit seconds — two quick mock turns, not a hang.
	deadline := time.Now().Add(15 * time.Second)
	var status, phase, errText string
	for time.Now().Before(deadline) {
		manager.mu.Lock()
		if r := manager.subagents[run.ID]; r != nil {
			status, phase, errText = r.Status, r.Phase, r.Error
		}
		manager.mu.Unlock()
		if status != "" && status != "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status == "" || status == "running" {
		t.Fatalf("hosted Claude subagent hung instead of failing fast: status=%q phase=%q", status, phase)
	}
	if status != "failed" {
		t.Fatalf("hosted Claude subagent settled as %q, want failed (phase=%q err=%q)", status, phase, errText)
	}
	if !strings.Contains(errText, "OAuth session expired") {
		t.Fatalf("failure reason lost the auth cause: %q", errText)
	}
}

// A provider that hangs before producing even one message (a subprocess
// wedged in an auth-refresh call that never returns, rather than failing
// cleanly) must still fail the row, bounded by the host's no-signal watchdog.
// This is the structural gap behind the live bug: the Node host and the Go
// bridge both correctly impose no timeout on an ordinary slow-but-alive turn,
// but neither bounded a turn that produces ZERO signal at all, so a wedged
// subprocess could hang forever rather than merely run long.
func TestHostedClaudeSubagentFailsOnTotalProviderSilence(t *testing.T) {
	root := repoRoot(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not available for Claude native-host test: %v", err)
	}
	cliDir := t.TempDir()
	claude := filepath.Join(cliDir, executableName("claude"))
	writeExecutable(t, claude, nativeNoopScript())
	t.Setenv("WORKASS_NODE", node)
	t.Setenv("WORKASS_CLAUDE_HOST", filepath.Join(root, "scripts", "claude-native-host.mjs"))
	t.Setenv("WORKASS_CLAUDE_SDK_MODULE", filepath.Join(root, "desktop", "acp", "mock-claude-agent-sdk.mjs"))
	t.Setenv("WORKASS_CLAUDE_FIXTURE_SILENT_HANG", "1")
	// A short bound so the test proves the watchdog without waiting out the
	// host's real-world default (60s).
	t.Setenv("WORKASS_CLAUDE_FIRST_SIGNAL_TIMEOUT_MS", "300")
	t.Setenv("WORKASS_CLAUDE_SESSION_ID", "fixture-hosted-silent-hang-session")

	events := newEventCollector()
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "claude", Name: "Claude Code", Command: claude, Enabled: true, Badge: "native", CWD: root,
		}},
		DefaultProviderID:  "claude",
		ProviderConfigFile: filepath.Join(t.TempDir(), "providers.json"),
		Broadcast:          events.Broadcast,
		InitTimeout:        5 * time.Second,
		RSSSampleInterval:  time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	run, err := manager.SpawnSubagent(context.Background(), SubagentSpawnOptions{
		HostedForOrigin: true, OriginMachine: "m-laptop-under-test", OriginChatID: "chat-on-the-asking-machine",
		Prompt: "run the delegated task", ProviderID: "claude", CWD: root, PermissionIntent: "full",
	})
	if err != nil {
		t.Fatalf("spawn hosted Claude subagent: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	var status, phase, errText string
	for time.Now().Before(deadline) {
		manager.mu.Lock()
		if r := manager.subagents[run.ID]; r != nil {
			status, phase, errText = r.Status, r.Phase, r.Error
		}
		manager.mu.Unlock()
		if status != "" && status != "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status == "" || status == "running" {
		t.Fatalf("hosted Claude subagent hung on total provider silence instead of failing: status=%q phase=%q", status, phase)
	}
	if status != "failed" {
		t.Fatalf("hosted Claude subagent settled as %q, want failed (phase=%q err=%q)", status, phase, errText)
	}
	if !strings.Contains(errText, "no response") {
		t.Fatalf("failure reason lost the no-signal cause: %q", errText)
	}
}
