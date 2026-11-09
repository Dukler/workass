package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"workass/internal/acp"
	"workass/internal/agenttext"
)

// Chat titles written by an agent (Settings · General, 2026-11-08).
//
// A chat is first named from its first message, as before. With the setting
// on, the app then asks here for a better name: the chosen agent reads that
// message once, with no tools, no project, no settings or hooks and no saved
// session, and answers a few words. The title only replaces the first-message
// one if the chat still has it, so a name the human typed always wins. Sending
// never waits for this; a failure leaves the first-message title.
//
// The request names the agent and model. The app sends the choice stored in
// its own settings, so a chat that lives on another machine is named with the
// same choice by that machine. A request without them uses this machine's
// stored setting, and only while it is on.
//
//	app-chat:auto-title [{tabId, chatId, text, expectTitle, providerId?, model?}] → {started, reason?}

const (
	chatTitleModePrompt = "prompt"
	chatTitleModeAgent  = "agent"
	chatTitleTimeout    = 45 * time.Second
	// chatTitleSettleWait bounds how long a generated title waits for the
	// app's save of the first-message title, which is debounced and can trail
	// the request by a few seconds while the turn runs.
	chatTitleSettleWait = 15 * time.Second
	chatTitleInputMax   = 2000
	chatTitleMaxRunes   = 60
)

// chatTitleSettings is the stored "chatTitles" setting.
type chatTitleSettings struct {
	Mode       string
	ProviderID string
	Model      string
}

// The agents that can name a chat, and how each is asked once. Only Claude
// Code today; another provider is added here with its own one-shot flags.
var chatTitleProviders = map[string]func(command, model, system string) []string{
	"claude": func(command, model, system string) []string {
		return []string{command, "-p", "--model", model, "--no-session-persistence", "--tools", "",
			"--setting-sources", "", "--strict-mcp-config", "--disable-slash-commands",
			"--output-format", "text", "--system-prompt", system}
	},
}

func defaultChatTitleSettings() map[string]any {
	return map[string]any{"mode": chatTitleModePrompt, "providerId": "claude", "model": "haiku"}
}

func normalizeChatTitles(value any) map[string]any {
	out := defaultChatTitleSettings()
	raw := mapFromAnyMain(value)
	if mode := strings.TrimSpace(toString(raw["mode"])); mode == chatTitleModeAgent || mode == chatTitleModePrompt {
		out["mode"] = mode
	}
	if provider := strings.TrimSpace(toString(raw["providerId"])); provider != "" {
		out["providerId"] = provider
	}
	if model := strings.TrimSpace(toString(raw["model"])); model != "" && len(model) <= 120 {
		out["model"] = model
	}
	return out
}

// chatTitleModelPattern keeps a requested model a plain name, so it can never
// read as another command-line flag.
var chatTitleModelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:\[\]-]{0,119}$`)

// chatTitleRequest is one app-chat:auto-title call.
type chatTitleRequest struct {
	TabID, ChatID, Text, Expect string
	// ProviderID and Model are the app's stored choice; both empty means
	// "use this machine's setting".
	ProviderID, Model string
}

func chatTitleSettingsFrom(settings map[string]any) chatTitleSettings {
	raw := normalizeChatTitles(settings["chatTitles"])
	return chatTitleSettings{Mode: toString(raw["mode"]), ProviderID: toString(raw["providerId"]), Model: toString(raw["model"])}
}

// cleanGeneratedTitle keeps the first line of a model's answer as a title:
// no surrounding quotes or markdown, no trailing period, at most 60 characters
// cut at a word.
func cleanGeneratedTitle(answer string) string {
	line := ""
	for _, candidate := range strings.Split(strings.ReplaceAll(answer, "\r", ""), "\n") {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			line = candidate
			break
		}
	}
	line = strings.TrimLeft(line, "#*-> \t")
	lower := strings.ToLower(line)
	for _, prefix := range []string{"title:", "título:", "titulo:"} {
		if strings.HasPrefix(lower, prefix) {
			line = strings.TrimSpace(line[len(prefix):])
			break
		}
	}
	line = strings.Trim(line, "\"'`“”‘’«»*_ \t")
	line = strings.TrimRight(line, ".。!¡ ")
	line = strings.Join(strings.Fields(line), " ")
	if utf8.RuneCountInString(line) > chatTitleMaxRunes {
		runes := []rune(line)[:chatTitleMaxRunes]
		cut := string(runes)
		if space := strings.LastIndexFunc(cut, unicode.IsSpace); space > chatTitleMaxRunes/2 {
			cut = cut[:space]
		}
		line = strings.TrimSpace(cut) + "…"
	}
	return line
}

type chatTitler struct {
	// command finds the executable of an enabled agent.
	command  func(providerID string) (string, bool)
	chats    *providerChatRuntime
	settings func() map[string]any
	logf     func(string, ...any)
	slots    chan struct{}
	// settlePoll is how often a generated title checks whether the
	// first-message title has landed.
	settlePoll time.Duration
	// run executes one command and returns its stdout; replaced in tests.
	run func(ctx context.Context, argv []string, stdin string) (string, error)
}

func newChatTitler(manager *acp.Manager, chats *providerChatRuntime, settings func() map[string]any, logf func(string, ...any)) *chatTitler {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &chatTitler{command: manager.ProviderCommand, chats: chats, settings: settings, logf: logf, slots: make(chan struct{}, 2), settlePoll: 300 * time.Millisecond, run: runChatTitleCommand}
}

// Start begins naming one chat in the background. It answers at once: false
// with a reason when the setting is off or the agent cannot name chats.
func (t *chatTitler) Start(req chatTitleRequest) (bool, string) {
	var cfg chatTitleSettings
	if req.ProviderID != "" || req.Model != "" {
		cfg = chatTitleSettings{Mode: chatTitleModeAgent, ProviderID: strings.TrimSpace(req.ProviderID), Model: strings.TrimSpace(req.Model)}
		if cfg.ProviderID == "" || !chatTitleModelPattern.MatchString(cfg.Model) {
			return false, "needs an agent and a model"
		}
	} else {
		cfg = chatTitleSettingsFrom(t.settings())
		if cfg.Mode != chatTitleModeAgent {
			return false, "off"
		}
	}
	if !chatTitleModelPattern.MatchString(cfg.Model) {
		return false, "needs an agent and a model"
	}
	tabID, chatID, text, expect := req.TabID, req.ChatID, req.Text, req.Expect
	build, ok := chatTitleProviders[cfg.ProviderID]
	if !ok {
		return false, "this agent cannot name chats"
	}
	command, ok := t.command(cfg.ProviderID)
	if !ok {
		return false, "agent not available"
	}
	text = strings.TrimSpace(acp.RedactSensitiveText(text))
	if text == "" || strings.TrimSpace(expect) == "" || strings.TrimSpace(tabID) == "" {
		return false, "nothing to name"
	}
	if runes := []rune(text); len(runes) > chatTitleInputMax {
		text = string(runes[:chatTitleInputMax])
	}
	argv := build(command, cfg.Model, agenttext.Get("chatTitle.system"))
	select {
	case t.slots <- struct{}{}:
	default:
		return false, "busy"
	}
	go func() {
		defer func() { <-t.slots }()
		ctx, cancel := context.WithTimeout(context.Background(), chatTitleTimeout)
		defer cancel()
		answer, err := t.run(ctx, argv, text)
		if err != nil {
			t.logf("[titles] %s could not name a chat: %v", cfg.ProviderID, err)
			return
		}
		title := cleanGeneratedTitle(answer)
		if title == "" {
			return
		}
		deadline := time.Now().Add(chatTitleSettleWait)
		for {
			outcome, err := t.chats.AutoTitleChat(tabID, chatID, expect, title)
			if err != nil {
				t.logf("[titles] applying a generated title: %v", err)
				return
			}
			if outcome != autoTitlePending || time.Now().After(deadline) {
				return
			}
			time.Sleep(t.settlePoll)
		}
	}()
	return true, ""
}

// runChatTitleCommand runs the agent once in an empty folder, so no project
// file or instruction file is read, with the message on stdin.
func runChatTitleCommand(ctx context.Context, argv []string, stdin string) (string, error) {
	if len(argv) == 0 {
		return "", errors.New("no command")
	}
	dir, err := os.MkdirTemp("", "workass-title-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(acp.RedactSensitiveText(stderr.String()))
		if len(detail) > 300 {
			detail = detail[:300]
		}
		return "", fmt.Errorf("%w: %s", err, detail)
	}
	return stdout.String(), nil
}
