package acp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestBuiltInFrontierProvidersUseOfficialNativeCommands(t *testing.T) {
	providers := BuiltInProviderConfigs(repoRoot(t))
	claude, ok := providerFromSlice(providers, "claude")
	if !ok {
		t.Fatal("Claude provider missing")
	}
	if claude.Command != "claude" || claude.Name != "Claude Code" || claude.Badge != "native" {
		t.Fatalf("Claude provider = %#v", claude)
	}
	codex, ok := providerFromSlice(providers, "codex")
	if !ok {
		t.Fatal("Codex provider missing")
	}
	if codex.Command != "codex" || codex.Name != "Codex" || codex.Badge != "native" {
		t.Fatalf("Codex provider = %#v", codex)
	}
}

func TestBuiltInOpenCodeProviderDefaultsToOxAlphaFree(t *testing.T) {
	providers := BuiltInProviderConfigs(repoRoot(t))
	opencode, ok := providerFromSlice(providers, "opencode")
	if !ok {
		t.Fatal("OpenCode provider missing")
	}
	if opencode.Command != "opencode" || opencode.Name != "OpenCode" || opencode.Badge != "agent" || strings.Join(opencode.Args, " ") != "acp" {
		t.Fatalf("OpenCode provider = %#v", opencode)
	}

	executable := filepath.Join(t.TempDir(), executableName("opencode"))
	writeExecutable(t, executable, nativeNoopScript())
	t.Setenv("WORKASS_OPENCODE", "")
	resolved, args, env, inactive, err := (opencodeDetectionStrategy{}).Prepare(context.Background(), nil, ProviderConfig{
		ID: "opencode", Command: executable, Args: []string{"acp"}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("prepare OpenCode: %v", err)
	}
	if resolved != executable || strings.Join(args, " ") != "acp" || inactive != "" {
		t.Fatalf("OpenCode launch = command %q args %#v inactive %q", resolved, args, inactive)
	}
	if env["OPENCODE_CONFIG_CONTENT"] != opencodeOxAlphaConfigContent || !strings.Contains(env["OPENCODE_CONFIG_CONTENT"], "opencode/x-preview-f-free") {
		t.Fatalf("OpenCode default config = %#v", env)
	}
}

func TestResolveFrontierNativeLaunchUsesOfficialCLIsAndIgnoresZedAdapters(t *testing.T) {
	pathDir := t.TempDir()
	claude := filepath.Join(pathDir, executableName("claude"))
	codex := filepath.Join(pathDir, executableName("codex"))
	writeExecutable(t, claude, nativeNoopScript())
	writeExecutable(t, codex, nativeNoopScript())
	writeExecutable(t, filepath.Join(pathDir, executableName("claude-agent-acp")), nativeNoopScript())
	writeExecutable(t, filepath.Join(pathDir, executableName("codex-acp")), nativeNoopScript())
	t.Setenv("PATH", pathDir)
	t.Setenv("WORKASS_CLAUDE_CODE", "")
	t.Setenv("WORKASS_CODEX", "")

	claudeLaunch, err := resolveFrontierNativeLaunchWithExecutable(ProviderConfig{ID: "claude", Command: "claude"})
	if err != nil {
		t.Fatalf("resolve Claude: %v", err)
	}
	if claudeLaunch.Command != claude || len(claudeLaunch.Args) != 0 {
		t.Fatalf("Claude launch = %#v", claudeLaunch)
	}

	codexLaunch, err := resolveFrontierNativeLaunchWithExecutable(ProviderConfig{ID: "codex", Command: "codex"})
	if err != nil {
		t.Fatalf("resolve Codex: %v", err)
	}
	if codexLaunch.Command != codex || len(codexLaunch.Args) != 0 {
		t.Fatalf("Codex launch = %#v", codexLaunch)
	}
	if strings.Contains(claudeLaunch.Command, "acp") || strings.Contains(codexLaunch.Command, "acp") {
		t.Fatalf("Zed adapter leaked into native launches: Claude=%#v Codex=%#v", claudeLaunch, codexLaunch)
	}
}

func TestResolveFrontierNativeLaunchHonorsExplicitOfficialCLIOverrides(t *testing.T) {
	claude := filepath.Join(t.TempDir(), executableName("claude-custom"))
	codex := filepath.Join(t.TempDir(), executableName("codex-custom"))
	writeExecutable(t, claude, nativeNoopScript())
	writeExecutable(t, codex, nativeNoopScript())
	t.Setenv("WORKASS_CLAUDE_CODE", claude)
	t.Setenv("WORKASS_CODEX", codex)

	gotClaude, err := resolveFrontierNativeLaunchWithExecutable(ProviderConfig{ID: "claude", Command: "claude"})
	if err != nil || gotClaude.Command != claude {
		t.Fatalf("Claude override = %#v err=%v", gotClaude, err)
	}
	gotCodex, err := resolveFrontierNativeLaunchWithExecutable(ProviderConfig{ID: "codex", Command: "codex"})
	if err != nil || gotCodex.Command != codex {
		t.Fatalf("Codex override = %#v err=%v", gotCodex, err)
	}
}

func TestClaudeBridgeLaunchUsesWorkassSDKHostAndOfficialExecutable(t *testing.T) {
	root := t.TempDir()
	host := filepath.Join(root, "scripts", "claude-native-host.mjs")
	sdk := filepath.Join(root, "dist-bin", "frontier-hosts", runtime.GOOS+"-"+runtime.GOARCH, "node_modules", "@anthropic-ai", "claude-agent-sdk", "sdk.mjs")
	if err := os.MkdirAll(filepath.Dir(host), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(sdk), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, host, "// fixture host\n")
	writeFile(t, sdk, "export const query = () => {};\n")
	node := filepath.Join(t.TempDir(), executableName("node"))
	writeExecutable(t, node, nativeNoopScript())
	t.Setenv("WORKASS_NODE", node)
	t.Setenv("WORKASS_CLAUDE_HOST", "")
	t.Setenv("WORKASS_CLAUDE_SDK_MODULE", "")

	claude := filepath.Join(t.TempDir(), executableName("claude"))
	writeExecutable(t, claude, nativeNoopScript())
	launch, err := claudeNativeHostLaunch(ProviderConfig{
		ID: "claude", Command: claude, CWD: root, Env: map[string]string{"FIXTURE": "yes"},
	}, Options{RootDir: root, WorkassMCPCACertFile: "/workass/public-mcp-ca.pem"}, filepath.Join(t.TempDir(), "workass"))
	if err != nil {
		t.Fatalf("resolve Claude SDK host: %v", err)
	}
	if launch.Command != node || len(launch.Args) != 1 || launch.Args[0] != host {
		t.Fatalf("host launch = %#v", launch)
	}
	if launch.Env["WORKASS_CLAUDE_EXECUTABLE"] != claude || launch.Env["WORKASS_CLAUDE_SDK_MODULE"] != sdk ||
		launch.Env["FIXTURE"] != "yes" || launch.Env["NODE_EXTRA_CA_CERTS"] != "/workass/public-mcp-ca.pem" {
		t.Fatalf("host env = %#v", launch.Env)
	}
}

func TestClaudeNativeTurnRecoversTransientOAuthRefreshWithoutFalseAssistantAnswer(t *testing.T) {
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
	t.Setenv("WORKASS_CLAUDE_FIXTURE_TRANSIENT_OAUTH_RESULT", "1")
	t.Setenv("WORKASS_CLAUDE_SESSION_ID", "fixture-oauth-recovery-session")

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

	session, err := manager.NewSession(context.Background(), SessionOptions{
		TabID: "claude-oauth-tab", ChatID: "claude-oauth-chat", ProviderID: "claude", CWD: root,
	})
	if err != nil {
		t.Fatalf("new Claude OAuth recovery session: %v", err)
	}
	job, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, TabID: "claude-oauth-tab", ChatID: "claude-oauth-chat",
		ProviderID: "claude", Prompt: "recover the untouched logical turn",
	})
	if err != nil {
		t.Fatalf("start Claude OAuth recovery turn: %v", err)
	}
	end := events.waitJobEnd(t, jobID(job), 5*time.Second)
	endJob := jobFromEnd(end)
	if endJob["status"] != "done" {
		t.Fatalf("Claude OAuth recovery status = %#v", endJob)
	}
	result := fmt.Sprint(endJob["result"])
	if !strings.Contains(result, "Fixture answer") || strings.Contains(result, "OAuth session expired and could not be refreshed") {
		t.Fatalf("Claude OAuth recovery result did not replace the false auth answer: %q", result)
	}
	provider := assertProviderListItem(t, manager.ProvidersList(), "claude", providerStatusReady, true)
	if provider["enabled"] != true {
		t.Fatalf("Claude provider was disabled after recovered refresh: %#v", provider)
	}
}

func TestFrontierHostsPreferPackagedRuntimeOverMutableSourceTree(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude-native-host.mjs", "codex-native-host.mjs"} {
		writeFile(t, filepath.Join(root, "scripts", name), "// source host\n")
	}
	runtimeDir := t.TempDir()
	platform := runtime.GOOS + "-" + runtime.GOARCH
	packagedClaude := filepath.Join(runtimeDir, "frontier-hosts", platform, "claude-native-host.mjs")
	packagedCodex := filepath.Join(runtimeDir, "frontier-hosts", platform, "codex-native-host.mjs")
	if err := os.MkdirAll(filepath.Dir(packagedClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, packagedClaude, "// packaged Claude host\n")
	writeFile(t, packagedCodex, "// packaged Codex host\n")
	sdk := filepath.Join(runtimeDir, "frontier-hosts", platform, "node_modules", "@anthropic-ai", "claude-agent-sdk", "sdk.mjs")
	if err := os.MkdirAll(filepath.Dir(sdk), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, sdk, "export const query = () => {};\n")
	node := filepath.Join(t.TempDir(), executableName("node"))
	writeExecutable(t, node, nativeNoopScript())
	t.Setenv("WORKASS_NODE", node)
	t.Setenv("WORKASS_CLAUDE_HOST", "")
	t.Setenv("WORKASS_CODEX_HOST", "")
	t.Setenv("WORKASS_CLAUDE_SDK_MODULE", "")
	daemon := filepath.Join(runtimeDir, "workass")
	claudeExecutable := filepath.Join(t.TempDir(), executableName("claude"))
	codexExecutable := filepath.Join(t.TempDir(), executableName("codex"))
	writeExecutable(t, claudeExecutable, nativeNoopScript())
	writeExecutable(t, codexExecutable, nativeNoopScript())

	claude, err := claudeNativeHostLaunch(ProviderConfig{ID: "claude", Command: claudeExecutable}, Options{RootDir: root}, daemon)
	if err != nil {
		t.Fatalf("resolve packaged Claude host: %v", err)
	}
	codex, err := codexNativeHostLaunch(ProviderConfig{ID: "codex", Command: codexExecutable}, Options{RootDir: root}, daemon)
	if err != nil {
		t.Fatalf("resolve packaged Codex host: %v", err)
	}
	if len(claude.Args) != 1 || claude.Args[0] != packagedClaude {
		t.Fatalf("Claude host path = %#v, want %q", claude.Args, packagedClaude)
	}
	if len(codex.Args) != 1 || codex.Args[0] != packagedCodex {
		t.Fatalf("Codex host path = %#v, want %q", codex.Args, packagedCodex)
	}
}

func TestCodexBridgeLaunchUsesWorkassAppServerHostAndOfficialExecutable(t *testing.T) {
	root := t.TempDir()
	host := filepath.Join(root, "scripts", "codex-native-host.mjs")
	if err := os.MkdirAll(filepath.Dir(host), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, host, "// fixture host\n")
	node := filepath.Join(t.TempDir(), executableName("node"))
	writeExecutable(t, node, nativeNoopScript())
	t.Setenv("WORKASS_NODE", node)
	t.Setenv("WORKASS_CODEX_HOST", "")

	codex := filepath.Join(t.TempDir(), executableName("codex"))
	writeExecutable(t, codex, nativeNoopScript())
	launch, err := codexNativeHostLaunch(ProviderConfig{
		ID: "codex", Command: codex, CWD: root, Env: map[string]string{"FIXTURE": "yes"},
	}, Options{RootDir: root, WorkassMCPCACertFile: "/workass/public-mcp-ca.pem"}, filepath.Join(t.TempDir(), "workass"))
	if err != nil {
		t.Fatalf("resolve Codex app-server host: %v", err)
	}
	if launch.Command != node || len(launch.Args) != 1 || launch.Args[0] != host {
		t.Fatalf("host launch = %#v", launch)
	}
	if launch.Env["WORKASS_CODEX_EXECUTABLE"] != codex || launch.Env["FIXTURE"] != "yes" ||
		launch.Env["CODEX_CA_CERTIFICATE"] != "/workass/public-mcp-ca.pem" {
		t.Fatalf("host env = %#v", launch.Env)
	}
	if _, ok := launch.Env["SSL_CERT_FILE"]; ok {
		t.Fatalf("host env replaces native roots through SSL_CERT_FILE: %#v", launch.Env)
	}
}

func executableName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".cmd"
	}
	return base
}

func nativeNoopScript() string {
	if runtime.GOOS == "windows" {
		return "@echo off\r\nexit /b 0\r\n"
	}
	return "#!/bin/sh\nexit 0\n"
}
