package acp

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAgentMCPIsInjectedIntoSpareSessionsWithOpaqueOwnerKey(t *testing.T) {
	command := filepath.Join(t.TempDir(), "workass")
	servers := agentMCPServers(Options{
		AgentMCPCommand:     command,
		AgentMCPControlFile: "/tmp/workass-agent-control.json",
	}, SessionOptions{Spare: true, AgentOwnerKey: "owner-spare-1"})
	if len(servers) != 1 {
		t.Fatalf("agent MCP servers = %#v, want one for spare session", servers)
	}
	server := mapFromAny(servers[0])
	joined := fmt.Sprint(server["args"])
	if server["name"] != "workass-agent" || server["command"] != command ||
		!strings.Contains(joined, "--owner-key owner-spare-1") {
		t.Fatalf("agent MCP descriptor = %#v", server)
	}
	if got := agentMCPServers(Options{AgentMCPCommand: command}, SessionOptions{Ephemeral: true, AgentOwnerKey: "probe"}); len(got) != 0 {
		t.Fatalf("catalog probe received agent MCP: %#v", got)
	}
}

func TestSpareAdoptionRebindsInjectedAgentOwnerToRealChat(t *testing.T) {
	manager, _ := newFakeManager(t, "echo-prompt", Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	manager.opts.AgentMCPCommand = filepath.Join(t.TempDir(), "workass")
	manager.opts.AgentMCPControlFile = filepath.Join(t.TempDir(), "agent-control.json")
	manager.mu.Lock()
	providerID := manager.defaultProviderID
	gen := manager.spareGen
	manager.mu.Unlock()
	manager.warmOneSpare(providerID, gen, 1)
	manager.mu.Lock()
	if len(manager.spareSessions) != 1 {
		manager.mu.Unlock()
		t.Fatalf("spare sessions = %#v", manager.spareSessions)
	}
	ownerKey := manager.spareSessions[0].agentOwnerKey
	manager.mu.Unlock()
	if ownerKey == "" {
		t.Fatal("prewarmed session has no owner key")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{ChatID: "chat-adopted", TabID: "tab-adopted", ProviderID: providerID})
	if err != nil {
		t.Fatalf("adopt spare: %v", err)
	}
	manager.mu.Lock()
	binding := manager.agentOwners[ownerKey]
	boundKey := manager.agentOwnerBySession[session.SessionID]
	manager.mu.Unlock()
	if boundKey != ownerKey || binding.ChatID != "chat-adopted" || binding.TabID != "tab-adopted" {
		t.Fatalf("adopted owner key=%q binding=%#v want=%q", boundKey, binding, ownerKey)
	}
}

func TestEnvironmentBriefAdvertisesAgentCatalogAndSpawnTools(t *testing.T) {
	manager := NewManager(Options{AgentMCPCommand: filepath.Join(t.TempDir(), "workass")})
	t.Cleanup(func() { manager.Reset() })
	brief := manager.buildEnvironmentBrief(false)
	for _, want := range []string{
		"workass-agent MCP server",
		"list/read/create/rename/configure/focus/delete exact chats",
		"provider/model/effort/permission catalog",
		"tab_id + chat_id",
		"never infer the active tab",
		"host workspace artifacts",
		"workass_host_artifact",
		"use its returned markdown",
		"natural ![label](path)",
		"Never expose a raw local filesystem path",
		"durable inline chat images",
		"use workass_spawn_subagent for delegated agent work",
		"do not launch untracked detached agents or shells",
		"For every ACP provider",
		"workass_register_external_work in the same turn",
	} {
		if !strings.Contains(brief, want) {
			t.Fatalf("environment brief missing %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, "In Claude-provider chats") {
		t.Fatalf("environment brief still teaches a Claude-only external-work law:\n%s", brief)
	}
}

func TestEnvironmentBriefKeepsVerificationReceiptsInternal(t *testing.T) {
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	brief := manager.buildEnvironmentBrief(false)
	for _, want := range []string{"preserves command and tool output in internal event history", "Do not repeat raw command output", "name failures or skipped checks", "only when the user explicitly asks"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("environment brief missing internal receipt policy %q:\n%s", want, brief)
		}
	}
}

func TestAgentOwnerCapabilityCannotBeRetargetedToAnotherChat(t *testing.T) {
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	manager.mu.Lock()
	manager.bindAgentOwnerLocked("owner-exact", "chat-a", "tab-a")
	manager.mu.Unlock()
	if !manager.ValidateAgentOwner("owner-exact", "chat-a", "tab-a") {
		t.Fatal("exact owner binding was rejected")
	}
	for _, target := range [][2]string{{"chat-b", "tab-a"}, {"chat-a", "tab-b"}, {"", ""}} {
		if manager.ValidateAgentOwner("owner-exact", target[0], target[1]) {
			t.Fatalf("owner capability retargeted to chat=%q tab=%q", target[0], target[1])
		}
	}
}
