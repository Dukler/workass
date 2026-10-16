package acp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/toolcli"
)

func chatTokenTestOptions(stateDir string) Options {
	return Options{
		StateDir:                     stateDir,
		Provider:                     ProviderConfig{ID: "opencode"},
		WorkassToolsOrigin:           "https://tools.localhost:8788",
		WorkassToolsCommand:          filepath.Join(stateDir, "workass"),
		WorkassToolsCAFile:           filepath.Join(stateDir, "ca.pem"),
		SpawnedWorkReconcileInterval: time.Hour,
	}
}

func newChatTokenTestManager(t *testing.T, stateDir string) *Manager {
	manager := NewManager(chatTokenTestOptions(stateDir))
	t.Cleanup(func() { manager.Reset() })
	return manager
}

func writeChatToolContextDir(t *testing.T, dir, credential string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	config := toolcli.Config{Endpoint: "https://tools.localhost:8788/workass/tools", Credential: credential, ChatID: "chat-a", TabID: "tab-a"}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "context.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func mustNot(t *testing.T, condition bool, message string) {
	t.Helper()
	if condition {
		t.Fatal(message)
	}
}

// TestChatToolTokenOutlivesEngineSessionAndRestart walks the exact casualty
// sequence from the idle-stop bug: Workass stops the engine under a chat whose
// helpers carry WORKASS_TOOL_CONTEXT, forgets the session owner, and the daemon
// restarts. The token never loses its authority here, for its own chat only.
func TestChatToolTokenOutlivesEngineSessionAndRestart(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	manager := newChatTokenTestManager(t, stateDir)
	opts := chatTokenTestOptions(stateDir)
	bridge := newBridge("chat-token", opts, manager)
	manager.mu.Lock()
	owner := manager.newAgentOwnerKeyLocked()
	manager.bindAgentOwnerLocked(owner, "chat-a", "tab-a")
	manager.agentOwnerBySession["session-a"] = owner
	manager.mu.Unlock()
	prepared, err := bridge.prepareNativeInstructions(opts.Provider)
	if err != nil {
		t.Fatal(err)
	}
	boundDir := bridge.nativeInstructionsDir
	if err := bridge.bindNativeToolContext(owner, "chat-a", "tab-a"); err != nil {
		t.Fatal(err)
	}
	config, err := toolcli.ReadConfig(prepared.Env["WORKASS_TOOL_CONTEXT"])
	if err != nil {
		t.Fatal(err)
	}
	token := config.Credential
	if !strings.HasPrefix(token, "wct_") || config.ChatID != "chat-a" || config.TabID != "tab-a" {
		t.Fatal("bindNativeToolContext did not write the chat's tool token")
	}
	manager.mu.Lock()
	persisted := manager.chatToolTokenLocked("chat-a", "tab-a")
	manager.mu.Unlock()
	if persisted != token {
		t.Fatal("context token is not the chat's minted token")
	}
	if !manager.ValidateAgentOwner(token, "chat-a", "tab-a") {
		t.Fatal("freshly bound chat token rejected")
	}
	mustNot(t, manager.ValidateAgentOwner(token, "chat-b", "tab-a"), "chat token accepted for another chat")
	mustNot(t, manager.ValidateAgentOwner(token, "chat-a", "tab-b"), "chat token accepted for another tab")
	// The idle stop removes instructions and guide; the helper context stays.
	bridge.Close(true, nil)
	if _, err := os.Stat(prepared.Env["WORKASS_INSTRUCTIONS_FILE"]); !os.IsNotExist(err) {
		t.Fatal("engine stop leaked its instructions")
	}
	if !manager.ValidateAgentOwner(token, "chat-a", "tab-a") {
		t.Fatal("chat token rejected after the engine stopped")
	}
	manager.forgetSession("session-a", nil)
	mustNot(t, manager.ValidateAgentOwner(owner, "chat-a", "tab-a"), "session owner survived forgetSession")
	if !manager.ValidateAgentOwner(token, "chat-a", "tab-a") {
		t.Fatal("chat token rejected after forgetSession")
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "tool-bootstrap", "restart-prune"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "tool-bootstrap", "restart-prune", "context.json"), []byte("garbage"), 0600); err != nil {
		t.Fatal(err)
	}
	restored := newChatTokenTestManager(t, stateDir)
	if _, err := os.Stat(filepath.Join(stateDir, "tool-bootstrap", "restart-prune")); !os.IsNotExist(err) {
		t.Fatal("startup prune kept a dead credential directory")
	}
	if _, err := toolcli.ReadConfig(filepath.Join(boundDir, "context.json")); err != nil {
		t.Fatal("startup prune removed the live chat token directory", err)
	}
	if !restored.ValidateAgentOwner(token, "chat-a", "tab-a") {
		t.Fatal("chat token did not survive a fresh manager on the same StateDir")
	}
}

func TestForgetChatRevokesTokenAndRemovesItsContextDirectories(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	manager := newChatTokenTestManager(t, stateDir)
	opts := chatTokenTestOptions(stateDir)
	live := newBridge("live-instruction", opts, manager)
	gone := newBridge("closed-instruction", opts, manager)
	manager.mu.Lock()
	owner := manager.newAgentOwnerKeyLocked()
	manager.bindAgentOwnerLocked(owner, "chat-a", "tab-a")
	manager.mu.Unlock()
	livePrepared, err := live.prepareNativeInstructions(opts.Provider)
	if err != nil {
		t.Fatal(err)
	}
	if err := live.bindNativeToolContext(owner, "chat-a", "tab-a"); err != nil {
		t.Fatal(err)
	}
	token, err := toolcli.ReadConfig(livePrepared.Env["WORKASS_TOOL_CONTEXT"])
	if err != nil {
		t.Fatal(err)
	}
	closedPrepared, err := gone.prepareNativeInstructions(opts.Provider)
	if err != nil {
		t.Fatal(err)
	}
	if err := gone.bindNativeToolContext(owner, "chat-a", "tab-a"); err != nil {
		t.Fatal(err)
	}
	gone.Close(true, nil)
	if _, err := os.Stat(closedPrepared.Env["WORKASS_TOOL_CONTEXT"]); err != nil {
		t.Fatal("engine close removed the helper context", err)
	}
	ghost := filepath.Join(stateDir, "tool-bootstrap", "helper-ghost")
	writeChatToolContextDir(t, ghost, token.Credential)
	manager.mu.Lock()
	manager.bridges["live-instruction"] = live
	manager.mu.Unlock()
	manager.ForgetChat(context.Background(), "tab-a", "chat-a", "chat-token-forget-test-operation")
	mustNot(t, manager.ValidateAgentOwner(token.Credential, "chat-a", "tab-a"), "ForgetChat left the chat token valid")
	if _, err := os.Stat(ghost); !os.IsNotExist(err) {
		t.Fatal("ForgetChat kept a helper context of the deleted chat")
	}
	if _, err := toolcli.ReadConfig(filepath.Join(live.nativeInstructionsDir, "context.json")); err != nil {
		t.Fatal("ForgetChat removed the live bridge's instruction directory", err)
	}
	if _, err := os.Stat(filepath.Dir(closedPrepared.Env["WORKASS_TOOL_CONTEXT"])); !os.IsNotExist(err) {
		t.Fatal("ForgetChat kept the closed engine's context directory")
	}
}

func TestPruneToolBootstrapRemovesContextsWithoutLiveTokens(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	manager := newChatTokenTestManager(t, stateDir)
	bootstrap := filepath.Join(stateDir, "tool-bootstrap")
	keep := filepath.Join(bootstrap, "waker-keep")
	neverBound := filepath.Join(bootstrap, "never-bound")
	garbage := filepath.Join(bootstrap, "garbage")
	staleOwner := filepath.Join(bootstrap, "dead-engine")
	manager.mu.Lock()
	token := manager.chatToolTokenLocked("chat-a", "tab-a")
	manager.mu.Unlock()
	if token == "" {
		t.Fatal("no chat token was minted")
	}
	writeChatToolContextDir(t, keep, token)
	if err := os.MkdirAll(neverBound, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(neverBound, "context.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(garbage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(garbage, "context.json"), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	writeChatToolContextDir(t, staleOwner, "dead engine owner key")
	stray := filepath.Join(bootstrap, "stray-file")
	if err := os.WriteFile(stray, []byte("junk"), 0600); err != nil {
		t.Fatal(err)
	}
	manager.pruneToolBootstrap()
	if _, err := os.Stat(filepath.Join(keep, "context.json")); err != nil {
		t.Fatal("prune removed the live chat token directory", err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatal("prune removed a stray file inside tool-bootstrap", err)
	}
	for _, removed := range []string{neverBound, garbage, staleOwner} {
		if _, err := os.Stat(removed); !os.IsNotExist(err) {
			t.Fatalf("prune kept %q without a live credential", removed)
		}
	}
}

func TestWorkassQuestionJobLockedAcceptsChatTokenForItsChat(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	manager.mu.Lock()
	token := manager.chatToolTokenLocked("chat-q", "tab-q")
	job := &Job{ID: "job-q", Status: "running", ChatID: "chat-q", TabID: "tab-q", SessionID: "session-q"}
	manager.jobs[job.ID] = job
	manager.mu.Unlock()
	if token == "" {
		t.Fatal("no chat token was minted")
	}
	if err := manager.ValidateWorkassQuestionCaller(token, "chat-q", "tab-q"); err != nil {
		t.Fatalf("chat token rejected for its own running turn: %v", err)
	}
	manager.mu.Lock()
	got, err := manager.workassQuestionJobLocked(token, "chat-q", "tab-q")
	manager.mu.Unlock()
	if err != nil || got != job {
		t.Fatalf("chat token did not resolve the running job: job=%#v err=%v", got, err)
	}
	manager.mu.Lock()
	extra := &Job{ID: "job-q2", Status: "running", ChatID: "chat-q", TabID: "tab-q", SessionID: "session-q2"}
	manager.jobs[extra.ID] = extra
	manager.mu.Unlock()
	if err := manager.ValidateWorkassQuestionCaller(token, "chat-q", "tab-q"); err == nil {
		t.Fatal("chat token owned two running turns at once")
	}
	manager.mu.Lock()
	delete(manager.jobs, extra.ID)
	job.SubagentID = "child-q"
	manager.mu.Unlock()
	mustNot(t, manager.ValidateWorkassQuestionCaller(token, "chat-q", "tab-q") == nil, "subagent turn accepted a chat token question")
	manager.mu.Lock()
	job.SubagentID = ""
	job.Status = "done"
	manager.mu.Unlock()
	if err := manager.ValidateWorkassQuestionCaller(token, "chat-q", "tab-q"); err == nil {
		t.Fatal("a chat token asked while its chat had no running turn")
	}
	manager.mu.Lock()
	job.Status = "running"
	other := manager.chatToolTokenLocked("chat-other", "tab-other")
	owner := manager.newAgentOwnerKeyLocked()
	manager.bindAgentOwnerLocked(owner, "chat-q", "tab-q")
	manager.agentOwnerBySession["session-q"] = owner
	manager.mu.Unlock()
	if err := manager.ValidateWorkassQuestionCaller(other, "chat-q", "tab-q"); err == nil {
		t.Fatal("another chat's token resolved a running turn it does not own")
	}
	// The original session-owner path must be unchanged, including its exact
	// session-equality fence.
	if err := manager.ValidateWorkassQuestionCaller(owner, "chat-q", "tab-q"); err != nil {
		t.Fatalf("session owner lost its running turn: %v", err)
	}
}
