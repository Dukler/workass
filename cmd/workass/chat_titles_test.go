package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"workass/internal/acp"
)

func TestCleanGeneratedTitleKeepsAShortPlainTitle(t *testing.T) {
	cases := map[string]string{
		"Fix scroll jumping and sparks\n":     "Fix scroll jumping and sparks",
		"\"Arreglar el salto del scroll.\"":   "Arreglar el salto del scroll",
		"Title: Settings revamp":              "Settings revamp",
		"Título: Rediseño de ajustes":         "Rediseño de ajustes",
		"\n\n## **Hammer icon**\nsecond line": "Hammer icon",
		"«Instalar 0.1.271»":                  "Instalar 0.1.271",
		strings.Repeat("palabra ", 20):        "palabra palabra palabra palabra palabra palabra palabra…",
	}
	for in, want := range cases {
		if got := cleanGeneratedTitle(in); got != want {
			t.Errorf("cleanGeneratedTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestChatTitleSettingDefaultsToTheFirstMessage(t *testing.T) {
	settings := normalizeSettings(nil)
	if got := chatTitleSettingsFrom(settings); got.Mode != chatTitleModePrompt || got.ProviderID != "" || got.Model != "" {
		t.Fatalf("default = %#v", got)
	}
	settings = normalizeSettings(map[string]any{"chatTitles": map[string]any{"mode": "agent", "providerId": "agent-a", "model": "small"}})
	if got := chatTitleSettingsFrom(settings); got.Mode != chatTitleModeAgent || got.Model != "small" || got.ProviderID != "agent-a" {
		t.Fatalf("agent setting = %#v", got)
	}
	settings = normalizeSettings(map[string]any{"chatTitles": map[string]any{"mode": "whatever"}})
	if got := chatTitleSettingsFrom(settings); got.Mode != chatTitleModePrompt {
		t.Fatalf("an unknown mode must fall back to the first message: %#v", got)
	}
}

// fakeOneShot stands in for the provider adapter's one-shot command: agent-a
// offers "small" and "large".
func fakeOneShot(providerID, model, system string) ([]string, string) {
	if providerID != "agent-a" {
		return nil, "this agent cannot answer one-shot prompts"
	}
	if model != "small" && model != "large" {
		return nil, "this agent does not offer that model here"
	}
	return []string{"/fake/agent", "--model", model, "--system", system}, ""
}

func newTitleTestChat(t *testing.T, title string) (*providerChatRuntime, string, string) {
	return newTitleTestChatLocked(t, title, true)
}

func newTitleTestChatLocked(t *testing.T, title string, locked bool) (*providerChatRuntime, string, string) {
	t.Helper()
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{StateDir: stateDir, RuntimeProfile: "dev"})
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	const tabID, chatID = "title-tab", "title-chat"
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "title-create",
		"title": title, "titleLocked": locked, "providerId": "mock", "currentModelId": "mock-deterministic",
	}); err != nil {
		t.Fatalf("create actor chat: %v", err)
	}
	return runtime, tabID, chatID
}

func TestAutoTitleReplacesOnlyTheFirstMessageTitle(t *testing.T) {
	runtime, tabID, chatID := newTitleTestChat(t, "when i scroll up everything…")
	outcome, err := runtime.AutoTitleChat(tabID, chatID, "when i scroll up everything…", "Fix scroll jumping")
	if err != nil || outcome != autoTitleApplied {
		t.Fatalf("auto title = %v, %v", outcome, err)
	}
	if state, _ := runtime.Snapshot(chatID); state.Presentation.Title != "Fix scroll jumping" {
		t.Fatalf("title = %q", state.Presentation.Title)
	}
	// The human renames the chat; a late generated title must not undo that.
	if err := runtime.RenameChat(tabID, chatID, "Mi nombre", "title-rename"); err != nil {
		t.Fatal(err)
	}
	outcome, err = runtime.AutoTitleChat(tabID, chatID, "Fix scroll jumping", "Another title")
	if err != nil || outcome != autoTitleKept {
		t.Fatalf("a renamed chat was retitled: %v, %v", outcome, err)
	}
	if state, _ := runtime.Snapshot(chatID); state.Presentation.Title != "Mi nombre" {
		t.Fatalf("title = %q", state.Presentation.Title)
	}
}

func TestChatTitlerAsksTheAgentOnceInIsolationAndAppliesItsAnswer(t *testing.T) {
	runtime, tabID, chatID := newTitleTestChat(t, "arreglá el salto del…")
	settings := map[string]any{"chatTitles": map[string]any{"mode": "agent", "providerId": "agent-a", "model": "small"}}
	titler := &chatTitler{
		command:  fakeOneShot,
		chats:    runtime,
		settings: func() map[string]any { return settings },
		logf:     func(string, ...any) {},
		slots:    make(chan struct{}, 2),
	}
	titler.settlePoll = 10 * time.Millisecond
	var mu sync.Mutex
	var gotArgv []string
	var gotStdin string
	titler.run = func(ctx context.Context, argv []string, stdin string) (string, error) {
		mu.Lock()
		gotArgv, gotStdin = argv, stdin
		mu.Unlock()
		return "\"Arreglar el salto del scroll.\"\n", nil
	}
	started, reason := titler.Start(chatTitleRequest{
		TabID: tabID, ChatID: chatID, Expect: "arreglá el salto del…",
		Text: "arreglá el salto del scroll cuando subo, api key sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	})
	if !started {
		t.Fatalf("not started: %s", reason)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if state, _ := runtime.Snapshot(chatID); state.Presentation.Title == "Arreglar el salto del scroll" {
			break
		}
		if time.Now().After(deadline) {
			state, _ := runtime.Snapshot(chatID)
			t.Fatalf("title never applied: %q", state.Presentation.Title)
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if joined := strings.Join(gotArgv, " "); !strings.HasPrefix(joined, "/fake/agent --model small --system You name chat conversations.") {
		t.Errorf("argv = %v", gotArgv)
	}
	if strings.Contains(gotStdin, "sk-ant-api03") {
		t.Errorf("a secret reached the title agent: %q", gotStdin)
	}
}

func TestChatTitlerDoesNothingWhenTheSettingIsOff(t *testing.T) {
	titler := &chatTitler{
		command:  fakeOneShot,
		settings: func() map[string]any { return normalizeSettings(nil) },
		slots:    make(chan struct{}, 2),
		run: func(context.Context, []string, string) (string, error) {
			t.Fatal("the agent ran with the setting off")
			return "", nil
		},
	}
	if started, reason := titler.Start(chatTitleRequest{TabID: "t", ChatID: "c", Text: "hola", Expect: "hola"}); started || reason != "off" {
		t.Fatalf("Start = %v %q", started, reason)
	}
}

// The app sends its own choice, so a chat on a machine whose setting is off
// is still named, with the app's model, and a model that would read as a
// model the agent does not offer is refused before anything runs.
func TestChatTitlerUsesTheAgentAndModelTheAppSends(t *testing.T) {
	runtime, tabID, chatID := newTitleTestChat(t, "hola")
	ran := make(chan []string, 1)
	titler := &chatTitler{
		command:    fakeOneShot,
		chats:      runtime,
		settings:   func() map[string]any { return normalizeSettings(nil) },
		logf:       func(string, ...any) {},
		slots:      make(chan struct{}, 2),
		settlePoll: 10 * time.Millisecond,
		run: func(_ context.Context, argv []string, _ string) (string, error) {
			ran <- argv
			return "Saludo", nil
		},
	}
	if started, reason := titler.Start(chatTitleRequest{TabID: tabID, ChatID: chatID, Text: "hola", Expect: "hola", ProviderID: "agent-a", Model: "--dangerously-skip-permissions"}); started || reason == "" {
		t.Fatalf("a model the agent does not offer started: %v %q", started, reason)
	}
	if started, reason := titler.Start(chatTitleRequest{TabID: tabID, ChatID: chatID, Text: "hola", Expect: "hola", ProviderID: "agent-a", Model: "large"}); !started {
		t.Fatalf("not started: %s", reason)
	}
	select {
	case argv := <-ran:
		if !strings.Contains(strings.Join(argv, " "), "--model large") {
			t.Fatalf("argv = %v", argv)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the agent never ran")
	}
	deadline := time.Now().Add(5 * time.Second)
	for state, _ := runtime.Snapshot(chatID); state.Presentation.Title != "Saludo"; state, _ = runtime.Snapshot(chatID) {
		if time.Now().After(deadline) {
			t.Fatalf("title never applied: %q", state.Presentation.Title)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The app saves the first-message title on a debounce, so the generated title
// can be ready while the chat still has its placeholder name. It waits for the
// first-message title and then replaces it.
func TestChatTitlerWaitsForTheFirstMessageTitleToLand(t *testing.T) {
	runtime, tabID, chatID := newTitleTestChatLocked(t, "Chat 4", false)
	titler := &chatTitler{
		command:    fakeOneShot,
		chats:      runtime,
		settings:   func() map[string]any { return normalizeSettings(nil) },
		logf:       func(string, ...any) {},
		slots:      make(chan struct{}, 2),
		settlePoll: 10 * time.Millisecond,
		run:        func(context.Context, []string, string) (string, error) { return "Saludo inicial", nil },
	}
	if started, reason := titler.Start(chatTitleRequest{TabID: tabID, ChatID: chatID, Text: "hola, ¿cómo va?", Expect: "hola, ¿cómo va?", ProviderID: "agent-a", Model: "small"}); !started {
		t.Fatalf("not started: %s", reason)
	}
	time.Sleep(100 * time.Millisecond)
	if state, _ := runtime.Snapshot(chatID); state.Presentation.Title != "Chat 4" {
		t.Fatalf("the placeholder was replaced before the first-message title landed: %q", state.Presentation.Title)
	}
	state, _ := runtime.Snapshot(chatID)
	if _, err := runtime.SavePresentation(tabID, chatID, "first-message-title", state.Presentation.PresentationRevision, map[string]any{
		"tabId": tabID, "chatId": chatID, "title": "hola, ¿cómo va?", "titleLocked": true,
	}); err != nil {
		t.Fatalf("save first-message title: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for state, _ := runtime.Snapshot(chatID); state.Presentation.Title != "Saludo inicial"; state, _ = runtime.Snapshot(chatID) {
		if time.Now().After(deadline) {
			t.Fatalf("title never applied: %q", state.Presentation.Title)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
