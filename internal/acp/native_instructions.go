package acp

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"workass/internal/agenttext"

	"workass/internal/toolcli"
)

// Native delivery is registered by adapters, never inferred in chat policy.
type nativeInstructionDelivery struct {
	HostEnvironment   string
	ConfigEnvironment string
}

var workassNativeInstructions = agenttext.Get("native.bootstrap")

// prepareNativeInstructions creates only process-owned files. User instruction,
// authentication, permission and project files are never written.
func (b *Bridge) prepareNativeInstructions(provider ProviderConfig) (ProviderConfig, error) {
	b.mu.Lock()
	probe := b.catalogProbe
	b.mu.Unlock()
	style := providerAdapterForID(b.providerID).instructions
	if probe || b.opts.WorkassToolsOrigin == "" {
		return provider, nil
	}
	native := style != (nativeInstructionDelivery{})
	// A custom command registered under a native provider name need not implement
	// our private host contract. Keep its existing prompt behavior.
	if style.HostEnvironment != "" && provider.Env[style.HostEnvironment] == "" {
		native = false
	}
	if !filepath.IsAbs(b.opts.WorkassToolsCommand) || !filepath.IsAbs(b.opts.WorkassToolsCAFile) {
		return provider, errors.New("Workass tools require absolute command and certificate paths")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.nativeInstructionsDir == "" {
		root := filepath.Join(b.opts.StateDir, "tool-bootstrap")
		if err := os.MkdirAll(root, 0700); err != nil {
			return provider, errors.New("cannot prepare Workass instruction directory")
		}
		dir, err := os.MkdirTemp(root, "bridge-")
		if err != nil {
			return provider, errors.New("cannot create Workass instruction directory")
		}
		absolute, err := filepath.Abs(dir)
		if err != nil {
			os.RemoveAll(dir)
			return provider, err
		}
		guide := b.manager.buildEnvironmentBrief(false)
		guide += agenttext.Get("native.guide")
		for name, body := range map[string]string{"instructions.md": workassNativeInstructions, "guide.md": guide, "context.json": "{}\n"} {
			if err := os.WriteFile(filepath.Join(absolute, name), []byte(body), 0600); err != nil {
				os.RemoveAll(dir)
				return provider, errors.New("cannot save Workass instruction files")
			}
		}
		b.nativeInstructionsDir = absolute
	}
	provider.Env = copyStringMap(provider.Env)
	if provider.Env == nil {
		provider.Env = map[string]string{}
	}
	provider.Env["WORKASS_TOOLS_COMMAND"] = b.opts.WorkassToolsCommand
	provider.Env["WORKASS_TOOL_CONTEXT"] = filepath.Join(b.nativeInstructionsDir, "context.json")
	provider.Env["WORKASS_INSTRUCTIONS_FILE"] = filepath.Join(b.nativeInstructionsDir, "instructions.md")
	provider.Env["WORKASS_TOOLS_GUIDE"] = filepath.Join(b.nativeInstructionsDir, "guide.md")
	if native && style.ConfigEnvironment != "" {
		raw, exists := provider.Env[style.ConfigEnvironment]
		if !exists {
			raw = os.Getenv(style.ConfigEnvironment)
		}
		merged, err := appendInstructionFile(raw, provider.Env["WORKASS_INSTRUCTIONS_FILE"])
		if err != nil {
			return provider, err
		}
		provider.Env[style.ConfigEnvironment] = merged
	}
	b.nativeInstructionsEnabled = native
	return provider, nil
}

func appendInstructionFile(raw, path string) (string, error) {
	config := map[string]any{}
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &config); err != nil || config == nil {
			return "", errors.New("cannot merge native instruction configuration")
		}
	}
	var instructions []any
	if existing, ok := config["instructions"]; ok {
		var valid bool
		instructions, valid = existing.([]any)
		if !valid {
			return "", errors.New("native instructions configuration must be an array")
		}
		for _, item := range instructions {
			if _, ok := item.(string); !ok {
				return "", errors.New("native instruction paths must be strings")
			}
			if item == path {
				data, _ := json.Marshal(config)
				return string(data), nil
			}
		}
	}
	config["instructions"] = append(instructions, path)
	data, err := json.Marshal(config)
	return string(data), err
}

func (b *Bridge) usesNativeInstructions() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.nativeInstructionsDir != "" && b.nativeInstructionsEnabled
}

func (b *Bridge) bindNativeToolContext(owner, chatID, tabID string) error {
	b.mu.Lock()
	dir := b.nativeInstructionsDir
	b.mu.Unlock()
	if dir == "" {
		return nil
	}
	m := b.manager
	m.mu.Lock()
	binding, ok := m.agentOwners[owner]
	m.mu.Unlock()
	if !ok || owner == "" || binding.ChatID != chatID || binding.TabID != tabID {
		return errors.New("Workass tools have no owner for this session")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("cannot prepare Workass tool context directory")
	}
	config := toolcli.Config{Endpoint: strings.TrimRight(b.opts.WorkassToolsOrigin, "/") + "/workass/tools", CAFile: b.opts.WorkassToolsCAFile, Credential: owner, ChatID: chatID, TabID: tabID}
	data, err := json.Marshal(config)
	if err != nil {
		return errors.New("cannot encode Workass tool context")
	}
	// The path inherited by shell tools stays stable across spare adoption. The
	// server still validates the exact owner/chat pair on every invocation.
	file, err := os.CreateTemp(dir, "context-*")
	if err != nil {
		return errors.New("cannot prepare Workass tool context")
	}
	name := file.Name()
	defer os.Remove(name)
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return errors.New("cannot save Workass tool context")
	}
	if err := os.Rename(name, filepath.Join(dir, "context.json")); err != nil {
		return errors.New("cannot replace Workass tool context")
	}
	return nil
}

func nativeUserRequestBlock(text string, human bool) string {
	if human {
		if card, request, ok := splitWorkassToolCardPrefix(text); ok {
			return agenttext.Get("request.toolcard.prefix") + card + agenttext.Get("request.toolcard.suffix") + request
		}
	}
	return text
}

func nativeChatPrompt(opts JobStartOptions, text string, seed bool) string {
	history := buildContextDeltaBlock(opts.ContextDelta)
	if history == "" && seed {
		history = buildInitialContextSeedBlock(opts.InitialContextSeed)
	}
	request := nativeUserRequestBlock(text, opts.HumanAuthored)
	if history != "" {
		return history + agenttext.Get("request.prefix") + request
	}
	return request
}

func (b *Bridge) removeNativeInstructions() {
	b.mu.Lock()
	dir := b.nativeInstructionsDir
	b.nativeInstructionsDir = ""
	b.nativeInstructionsEnabled = false
	b.mu.Unlock()
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
}
