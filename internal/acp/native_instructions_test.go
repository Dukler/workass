package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	providercontract "workass/internal/provider"
	"workass/internal/toolcli"
)

func TestNativeInstructionsBindStableContextWithoutChangingUserConfig(t *testing.T) {
	root := t.TempDir()
	opts := Options{StateDir: root, Provider: ProviderConfig{ID: "opencode"}, WorkassToolsOrigin: "https://tools.localhost:8788", WorkassToolsCommand: filepath.Join(root, "workass"), WorkassToolsCAFile: filepath.Join(root, "ca.pem")}
	m := NewManager(opts)
	t.Cleanup(func() { m.Reset() })
	b := newBridge("native-test", opts, m)
	original := ProviderConfig{Env: map[string]string{"OPENCODE_CONFIG_CONTENT": `{"instructions":["user.md"],"model":"user-model","permission":{"edit":"ask"}}`}}
	prepared, err := b.prepareNativeInstructions(original)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(b.nativeInstructionsDir) })
	if original.Env["WORKASS_TOOL_CONTEXT"] != "" {
		t.Fatal("mutated user environment")
	}
	var config map[string]any
	json.Unmarshal([]byte(prepared.Env["OPENCODE_CONFIG_CONTENT"]), &config)
	if config["model"] != "user-model" || len(config["instructions"].([]any)) != 2 || config["permission"].(map[string]any)["edit"] != "ask" {
		t.Fatal("lost user settings")
	}
	m.mu.Lock()
	owner := m.newAgentOwnerKeyLocked()
	m.bindAgentOwnerLocked(owner, "chat", "tab")
	m.mu.Unlock()
	if err := b.bindNativeToolContext(owner, "chat", "tab"); err != nil {
		t.Fatal(err)
	}
	contextPath := prepared.Env["WORKASS_TOOL_CONTEXT"]
	first, err := toolcli.ReadConfig(contextPath)
	if err != nil || first.ChatID != "chat" {
		t.Fatal("missing owner context", err)
	}
	stat, _ := os.Stat(contextPath)
	if stat.Mode().Perm()&0077 != 0 {
		t.Fatal("context not private")
	}
	instructions, _ := os.ReadFile(prepared.Env["WORKASS_INSTRUCTIONS_FILE"])
	if strings.Contains(string(instructions), owner) || strings.Contains(string(instructions), contextPath) {
		t.Fatal("embedded session binding in instructions")
	}
	m.mu.Lock()
	m.bindAgentOwnerLocked(owner, "adopted", "adopted-tab")
	m.mu.Unlock()
	if err := b.bindNativeToolContext(owner, "chat", "tab"); err == nil {
		t.Fatal("accepted stale authority")
	}
	if err := b.bindNativeToolContext(owner, "adopted", "adopted-tab"); err != nil {
		t.Fatal(err)
	}
	second, err := toolcli.ReadConfig(contextPath)
	if err != nil || second.ChatID != "adopted" || m.ValidateAgentOwner(owner, "chat", "tab") {
		t.Fatal("did not refresh stable path")
	}
	b.Close(true, nil)
	if _, err := os.Stat(contextPath); !os.IsNotExist(err) {
		t.Fatal("retained context after close")
	}
}

func TestNativeChatPromptPreservesHistoryWithoutBoilerplate(t *testing.T) {
	opts := JobStartOptions{HumanAuthored: true}
	for _, seed := range []bool{false, true} {
		if got := nativeChatPrompt(opts, "hello", seed); got != "hello" {
			t.Fatal(got)
		}
	}
	opts.InitialContextSeed = []providercontract.ContextMessage{{Role: "user", Text: "prior request"}}
	got := nativeChatPrompt(opts, "hello", true)
	if !strings.Contains(got, "prior request") || !strings.HasSuffix(got, "User request:\nhello") || strings.Contains(got, "Workass tools for this turn") {
		t.Fatal(got)
	}
	if got := nativeChatPrompt(opts, "next", false); got != "next" {
		t.Fatal("repeated history", got)
	}
	opts.ContextDelta = opts.InitialContextSeed
	if got := nativeChatPrompt(opts, "next", false); !strings.Contains(got, "Missing Workass conversation") {
		t.Fatal("lost exact resume delta", got)
	}
}

func TestNativeInstructionConfigMergeRejectsMalformedAndDeduplicates(t *testing.T) {
	for _, raw := range []string{`null`, `broken`, `{"instructions":"wrong"}`, `{"instructions":[1]}`} {
		if _, err := appendInstructionFile(raw, "/fixture/instructions.md"); err == nil {
			t.Fatal("accepted malformed config")
		}
	}
	one, err := appendInstructionFile(`{"instructions":["user.md"]}`, "/fixture/instructions.md")
	if err != nil {
		t.Fatal(err)
	}
	two, err := appendInstructionFile(one, "/fixture/instructions.md")
	if err != nil || two != one {
		t.Fatal("duplicated instruction file")
	}
}

func TestCatalogProbeDoesNotReceiveOwnerInstructions(t *testing.T) {
	root := t.TempDir()
	opts := Options{StateDir: root, Provider: ProviderConfig{ID: "opencode"}, WorkassToolsOrigin: "https://tools.localhost:8788"}
	m := NewManager(opts)
	t.Cleanup(func() { m.Reset() })
	b := newBridge("probe", opts, m)
	b.catalogProbe = true
	config, err := b.prepareNativeInstructions(ProviderConfig{})
	if err != nil || b.usesNativeInstructions() || config.Env["WORKASS_TOOL_CONTEXT"] != "" {
		t.Fatal("catalog probe required a chat owner", err)
	}
}
