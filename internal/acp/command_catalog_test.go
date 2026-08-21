package acp

// Claude commands surface (docs/specs/claude-commands-surface.md §8): daemon
// verification against the bridge_test fake — SessionInfo.CommandCatalog on
// open, the chat:commands emit, chat:commands-get across warm/hibernated/
// unknown, the non-claude gate, the defensive re-clamp of a skewed host, and
// the D7 slash-prefix guard on the image preamble.

import (
	"context"
	"strings"
	"testing"
	"time"
)

func waitChatCommands(t *testing.T, events *eventCollector, pred func(payload map[string]any, catalog *CommandCatalog) bool) map[string]any {
	t.Helper()
	ev := events.waitFor(t, 2*time.Second, func(ev collectedEvent) bool {
		if ev.channel != "chat:commands" {
			return false
		}
		payload, _ := ev.payload.(map[string]any)
		catalog, _ := payload["commandCatalog"].(*CommandCatalog)
		return pred(payload, catalog)
	})
	return ev.payload.(map[string]any)
}

func TestClaudeCommandCatalogRidesOpenReplyEmitsEventAndAnswersCommandsGet(t *testing.T) {
	t.Parallel()
	manager, events := newFakeManager(t, "claude-commands", Options{
		RSSSampleInterval: time.Hour,
		Provider:          ProviderConfig{ID: "claude"},
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tabID, chatID := "cmd-tab", "cmd-chat"
	session, err := manager.NewSession(ctx, SessionOptions{TabID: tabID, ChatID: chatID})
	if err != nil {
		t.Fatalf("new claude-commands session: %v", err)
	}

	catalog := session.CommandCatalog
	if catalog == nil {
		t.Fatal("open reply dropped the command catalog")
	}
	if len(catalog.Commands) != 2 || catalog.Commands[0].Name != "review" || catalog.Commands[1].Name != "deploy" {
		t.Fatalf("open catalog commands = %#v", catalog.Commands)
	}
	if len(catalog.Commands[0].Aliases) != 1 || catalog.Commands[0].Aliases[0] != "cr" {
		t.Fatalf("open catalog aliases = %#v", catalog.Commands[0].Aliases)
	}
	if len(catalog.Agents) != 1 || catalog.Agents[0].Model != "sonnet" {
		t.Fatalf("open catalog agents = %#v", catalog.Agents)
	}
	if catalog.OutputStyle != "default" || len(catalog.AvailableOutputStyles) != 2 {
		t.Fatalf("open catalog styles = %q %#v", catalog.OutputStyle, catalog.AvailableOutputStyles)
	}
	if catalog.AsOf != 1785000000000 {
		t.Fatalf("open catalog asOf = %d", catalog.AsOf)
	}
	// D2 redaction: catalog strings pass the redaction prefilter before any
	// emit or reply.
	if desc := catalog.Commands[1].Description; strings.Contains(desc, "sk-fake-cmd-secret") || !strings.Contains(desc, "token=[redacted]") {
		t.Fatalf("catalog description leaked a secret: %q", desc)
	}

	payload := waitChatCommands(t, events, func(payload map[string]any, catalog *CommandCatalog) bool {
		return catalog != nil && len(catalog.Commands) == 2
	})
	if payload["tabId"] != tabID || payload["chatId"] != chatID || payload["sessionId"] != session.SessionID {
		t.Fatalf("chat:commands identity = %#v", payload)
	}

	reply := manager.ChatCommands(tabID, chatID)
	if reply["supported"] != true || reply["live"] != true {
		t.Fatalf("warm chat:commands-get = %#v", reply)
	}
	got, _ := reply["commandCatalog"].(*CommandCatalog)
	if got == nil || len(got.Commands) != 2 || got.Commands[0].Name != "review" {
		t.Fatalf("warm chat:commands-get catalog = %#v", reply["commandCatalog"])
	}

	// D3: a mid-session _workass_claude_commands update replaces the cached
	// catalog wholesale and re-emits chat:commands — no live-job requirement.
	job, err := manager.StartJob(ctx, JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, TabID: tabID, ChatID: chatID,
		ProviderID: session.ProviderID, Prompt: "push commands please",
	})
	if err != nil {
		t.Fatalf("start push-commands turn: %v", err)
	}
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 3*time.Second), "done", 0, "end_turn")
	waitChatCommands(t, events, func(payload map[string]any, catalog *CommandCatalog) bool {
		return catalog != nil && len(catalog.Commands) == 1 && catalog.Commands[0].Name == "changed-one"
	})
	reply = manager.ChatCommands(tabID, chatID)
	got, _ = reply["commandCatalog"].(*CommandCatalog)
	if got == nil || len(got.Commands) != 1 || got.Commands[0].Name != "changed-one" {
		t.Fatalf("post-push chat:commands-get catalog = %#v", reply["commandCatalog"])
	}
	if got.AsOf != 1785000000001 {
		t.Fatalf("post-push catalog asOf = %d", got.AsOf)
	}
}

func TestClaudeCommandCatalogSurvivesHibernationAsCachedSnapshot(t *testing.T) {
	t.Parallel()
	manager, events := newFakeManager(t, "claude-commands", Options{
		HibernateTTL:      20 * time.Millisecond,
		RSSSampleInterval: time.Hour,
		Provider:          ProviderConfig{ID: "claude"},
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tabID, chatID := "cmd-hib-tab", "cmd-hib-chat"
	session, err := manager.NewSession(ctx, SessionOptions{TabID: tabID, ChatID: chatID})
	if err != nil {
		t.Fatalf("new claude-commands session: %v", err)
	}
	if session.CommandCatalog == nil {
		t.Fatal("open reply dropped the command catalog")
	}
	job, err := manager.StartJob(ctx, JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, TabID: tabID, ChatID: chatID,
		ProviderID: session.ProviderID, Prompt: "one completed turn so the engine can idle",
	})
	if err != nil {
		t.Fatalf("start turn: %v", err)
	}
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 3*time.Second), "done", 0, "end_turn")
	_ = waitProcState(t, manager, StateHibernated, 2*time.Second)

	// D4/D6: hibernation kills the engine but keeps the cache — the chat still
	// answers with its snapshot, flagged live:false.
	reply := manager.ChatCommands(tabID, chatID)
	if reply["supported"] != true || reply["live"] != false {
		t.Fatalf("hibernated chat:commands-get = %#v", reply)
	}
	got, _ := reply["commandCatalog"].(*CommandCatalog)
	if got == nil || len(got.Commands) != 2 || got.Commands[0].Name != "review" {
		t.Fatalf("hibernated chat:commands-get catalog = %#v", reply["commandCatalog"])
	}
}

func TestChatCommandsGetIsUnsupportedForUnknownChatsAndUnadvertisedHosts(t *testing.T) {
	t.Parallel()
	// The fake in echo-prompt mode is a claude-id provider whose host never
	// advertises workassClaudeCommandCatalog — the "old host" skew: everything
	// stays UNKNOWN, nothing crashes.
	manager, _ := newFakeManager(t, "echo-prompt", Options{
		RSSSampleInterval: time.Hour,
		Provider:          ProviderConfig{ID: "claude"},
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: "old-host-tab", ChatID: "old-host-chat"})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if session.CommandCatalog != nil {
		t.Fatalf("old host invented a catalog: %#v", session.CommandCatalog)
	}
	reply := manager.ChatCommands("old-host-tab", "old-host-chat")
	if reply["supported"] != false || reply["live"] != false || reply["commandCatalog"] != nil {
		t.Fatalf("unadvertised host chat:commands-get = %#v", reply)
	}

	// A chat this daemon has never attached: UNKNOWN, not proven-empty.
	reply = manager.ChatCommands("tab-nunca-vista", "chat-nunca-vista")
	if reply["supported"] != false || reply["live"] != false || reply["commandCatalog"] != nil {
		t.Fatalf("unknown chat chat:commands-get = %#v", reply)
	}
}

func TestClaudeCommandCatalogIsGatedToTheClaudeProvider(t *testing.T) {
	t.Parallel()
	manager, events := newFakeManager(t, "claude-commands", Options{
		RSSSampleInterval: time.Hour,
		Provider:          ProviderConfig{ID: "codex"},
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tabID, chatID := "codex-cmd-tab", "codex-cmd-chat"
	session, err := manager.NewSession(ctx, SessionOptions{TabID: tabID, ChatID: chatID})
	if err != nil {
		t.Fatalf("new codex session: %v", err)
	}
	if session.CommandCatalog != nil {
		t.Fatalf("non-claude provider surfaced a catalog: %#v", session.CommandCatalog)
	}
	// The stray mid-session push must be ignored by the D3 gate too.
	job, err := manager.StartJob(ctx, JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, TabID: tabID, ChatID: chatID,
		ProviderID: session.ProviderID, Prompt: "push commands please",
	})
	if err != nil {
		t.Fatalf("start stray-push turn: %v", err)
	}
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 3*time.Second), "done", 0, "end_turn")
	events.expectNoChannel(t, "chat:commands", 250*time.Millisecond)
	reply := manager.ChatCommands(tabID, chatID)
	if reply["supported"] != false || reply["commandCatalog"] != nil {
		t.Fatalf("non-claude chat:commands-get = %#v", reply)
	}
}

func TestClaudeCommandCatalogReclampsASkewedHostPayload(t *testing.T) {
	t.Parallel()
	manager, _ := newFakeManager(t, "claude-commands-overflow", Options{
		RSSSampleInterval: time.Hour,
		Provider:          ProviderConfig{ID: "claude"},
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: "overflow-tab", ChatID: "overflow-chat"})
	if err != nil {
		t.Fatalf("new overflow session: %v", err)
	}
	catalog := session.CommandCatalog
	if catalog == nil {
		t.Fatal("overflow catalog missing")
	}
	if len(catalog.Commands) != 512 || catalog.CommandsTruncated != 88 {
		t.Fatalf("re-clamp kept %d commands, counted %d truncated", len(catalog.Commands), catalog.CommandsTruncated)
	}
	first := catalog.Commands[0]
	if len(first.Description) != 200 || len(first.ArgumentHint) != 80 || len(first.Aliases) != 4 {
		t.Fatalf("re-clamp clipped fields = desc:%d hint:%d aliases:%d", len(first.Description), len(first.ArgumentHint), len(first.Aliases))
	}
	if len(catalog.Agents) != 1 || len(catalog.Agents[0].Name) != 80 {
		t.Fatalf("re-clamp agents = %#v", catalog.Agents)
	}
	// The skewed host's own counter is evidence of ITS drops; the daemon adds
	// to it, never resets it.
	if catalog.AgentsTruncated != 2 {
		t.Fatalf("host-reported agentsTruncated lost: %d", catalog.AgentsTruncated)
	}
}

// D7/§5: a slash command is recognized by the leading slash of the message
// text, so with images attached the [Workass attachment context] notice must
// TRAIL a /command prompt as its own block — a prefix would stop the CLI from
// seeing the command. Plain prompts keep the notice as a prefix.
func TestPromptBlocksKeepsTheLeadingSlashAheadOfTheImageNotice(t *testing.T) {
	t.Parallel()
	b := &Bridge{imageSupport: true}
	image := []any{map[string]any{"mimeType": "image/png", "data": "iVBORw0KGgo="}}

	blocks, err := b.promptBlocks("/review 123", image)
	if err != nil {
		t.Fatalf("slash prompt blocks: %v", err)
	}
	if len(blocks) != 3 {
		t.Fatalf("slash prompt block count = %#v", blocks)
	}
	if text := asString(mapFromAny(blocks[1])["text"]); text != "/review 123" {
		t.Fatalf("slash prompt text block = %q", text)
	}
	if notice := asString(mapFromAny(blocks[2])["text"]); !strings.HasPrefix(notice, "[Workass attachment context]") {
		t.Fatalf("trailing notice block = %q", notice)
	}

	blocks, err = b.promptBlocks("  /usage", image)
	if err != nil {
		t.Fatalf("padded slash prompt blocks: %v", err)
	}
	if text := asString(mapFromAny(blocks[1])["text"]); text != "  /usage" {
		t.Fatalf("padded slash prompt text block = %q", text)
	}

	blocks, err = b.promptBlocks("plain message", image)
	if err != nil {
		t.Fatalf("plain prompt blocks: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("plain prompt block count = %#v", blocks)
	}
	text := asString(mapFromAny(blocks[1])["text"])
	if !strings.HasPrefix(text, "[Workass attachment context]") || !strings.HasSuffix(text, "plain message") {
		t.Fatalf("plain prompt kept notice prefix? %q", text)
	}
}
