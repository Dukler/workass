package acp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func isOfficialNativeCommand(provider ProviderConfig, expected string) bool {
	command := strings.TrimSpace(launchCommand(provider))
	if command == "" {
		return false
	}
	base := strings.ToLower(filepath.Base(command))
	base = strings.TrimSuffix(base, ".exe")
	base = strings.TrimSuffix(base, ".cmd")
	return base == strings.ToLower(strings.TrimSpace(expected))
}

// claudeNativeHostLaunch replaces the official Claude executable launch with
// Workass's small Agent SDK host. The executable itself is passed to the SDK;
// no Zed ACP adapter is started or discovered here.
func claudeNativeHostLaunch(provider ProviderConfig, opts Options, daemonExecutable string) (ProviderConfig, error) {
	root := strings.TrimSpace(opts.RootDir)
	platform := runtime.GOOS + "-" + runtime.GOARCH
	daemonDir := filepath.Dir(strings.TrimSpace(daemonExecutable))

	host, err := firstNativeFile(strings.TrimSpace(os.Getenv("WORKASS_CLAUDE_HOST")), []string{
		filepath.Join(daemonDir, "frontier-hosts", platform, "claude-native-host.mjs"),
		filepath.Join(daemonDir, "frontier-hosts", "claude-native-host.mjs"),
		filepath.Join(root, "scripts", "claude-native-host.mjs"),
	})
	if err != nil {
		return ProviderConfig{}, fmt.Errorf("Claude Agent SDK host: %w", err)
	}
	sdkModule, err := firstNativeFile(strings.TrimSpace(firstNonEmpty(provider.Env["WORKASS_CLAUDE_SDK_MODULE"], os.Getenv("WORKASS_CLAUDE_SDK_MODULE"))), []string{
		filepath.Join(root, "dist-bin", "frontier-hosts", platform, "node_modules", "@anthropic-ai", "claude-agent-sdk", "sdk.mjs"),
		filepath.Join(daemonDir, "frontier-hosts", platform, "node_modules", "@anthropic-ai", "claude-agent-sdk", "sdk.mjs"),
		filepath.Join(daemonDir, "frontier-hosts", "node_modules", "@anthropic-ai", "claude-agent-sdk", "sdk.mjs"),
	})
	if err != nil {
		return ProviderConfig{}, fmt.Errorf("official Claude Agent SDK module: %w", err)
	}
	node, err := resolveNativeNode(daemonDir, platform)
	if err != nil {
		return ProviderConfig{}, err
	}

	env := copyStringMap(provider.Env)
	if env == nil {
		env = make(map[string]string)
	}
	env["WORKASS_CLAUDE_EXECUTABLE"] = launchCommand(provider)
	// Claude Code's own forked subagent (Agent tool, subagent_type "fork": the
	// child inherits the whole conversation) is gated off for SDK hosts. The
	// user turned it on (2026-10-16), knowing the gate also removes
	// run_in_background and runs every Agent spawn in the background, as
	// interactive Claude Code does. Fresh stays the default per spawn; any
	// explicit setting (provider env or the daemon environment, e.g. 0) wins.
	if _, set := env["CLAUDE_CODE_FORK_SUBAGENT"]; !set && os.Getenv("CLAUDE_CODE_FORK_SUBAGENT") == "" {
		env["CLAUDE_CODE_FORK_SUBAGENT"] = "1"
	}
	env["WORKASS_CLAUDE_SDK_MODULE"] = sdkModule
	provider.Command = node
	provider.ResolvedCommand = ""
	provider.Args = []string{host}
	provider.Env = env
	return provider, nil
}

// claudeIdentityEnv is set on every Claude process Workass starts (chats,
// spares, subagents, session forks, fork runs, chat titles) unless the
// provider's own environment sets the same name. It is applied by the Claude
// launch policy (mergedEnv), so no launch path can miss it.
//
//   - CLAUDE_CODE_ENTRYPOINT: the Agent SDK labels its processes "sdk-ts" and
//     `claude -p` labels itself "sdk-cli". t3code measured Anthropic's 5-hour
//     meter charging those labels about 1.6x to 1.7x more than the
//     interactive CLI for the same tokens (pingdotgg/t3code#24649; the weekly
//     meter was unaffected), and fixed it by naming their app. Workass does
//     the same. The User-Agent still carries agent-sdk/<version> and the
//     client-app tag, so the request says what is calling.
//   - CLAUDE_AGENT_SDK_CLIENT_APP: Claude Code's own field for the app on top
//     of the SDK (x-client-app header, client-app/ in the User-Agent).
//   - CLAUDE_CODE_ARTIFACT=0: Claude Code turns its Artifact tools off only
//     for the sdk-* labels; under any other name Artifact, ArtifactComments
//     and ArtifactData add about 26k characters of tool definitions to every
//     request. A daemon environment value is kept (it is not a host-session
//     variable).
//
// Under a non-SDK label Claude Code 2.1.322 also adds its claude-code-guide
// agent and an AskUserQuestion preview paragraph, records the label in the
// session transcript (so a terminal `claude -c` or /resume in the same folder
// lists Workass sessions), and makes background memory consolidation
// (autoDream) eligible; the host turns that one off (claude-native-host.mjs).
var claudeIdentityEnv = map[string]string{
	"CLAUDE_CODE_ENTRYPOINT":      "workass",
	"CLAUDE_AGENT_SDK_CLIENT_APP": "workass",
	"CLAUDE_CODE_ARTIFACT":        "0",
}

// claudeHostSessionEnv is what a Claude Code session exports to the processes it
// starts. A daemon started from inside one (a dev rig run from a chat) would
// hand them to every Claude it launches, and that Claude would act as part of a
// session it does not belong to: its id, its messaging socket, its effort, its
// label. They are dropped from the daemon's environment; a provider setting for
// the same name still passes.
var claudeHostSessionEnv = []string{
	"CLAUDECODE",
	"CLAUDE_CODE_ENTRYPOINT",
	"CLAUDE_CODE_SESSION_ID",
	"CLAUDE_CODE_HOST_SESSION_ID",
	"CLAUDE_CODE_CHILD_SESSION",
	"CLAUDE_CODE_SESSION_ATTENDED",
	"CLAUDE_CODE_MESSAGING_SOCKET",
	"CLAUDE_CODE_MESSAGING_TOKEN",
	"CLAUDE_CODE_EXECPATH",
	"CLAUDE_CODE_SSE_PORT",
	"CLAUDE_CODE_DESKTOP_APP_VERSION",
	"CLAUDE_EFFORT",
	"CLAUDE_PID",
	"CLAUDE_AGENT_SDK_VERSION",
	"CLAUDE_AGENT_SDK_CLIENT_APP",
	"CLAUDE_PLUGIN_ROOT",
	"CLAUDE_PLUGIN_DATA",
}

func firstNativeFile(explicit string, candidates []string) (string, error) {
	if explicit != "" {
		if fileExists(explicit) {
			return explicit, nil
		}
		return "", fmt.Errorf("configured path does not exist: %s", explicit)
	}
	for _, candidate := range candidates {
		if fileExists(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("not found in the Workass source or packaged runtime")
}

func resolveNativeNode(daemonDir, platform string) (string, error) {
	if explicit := strings.TrimSpace(os.Getenv("WORKASS_NODE")); explicit != "" {
		return resolveFrontierNativeCommand(explicit, "WORKASS_NODE")
	}
	name := "node"
	if runtime.GOOS == "windows" {
		name = "node.exe"
	}
	for _, candidate := range []string{
		filepath.Join(daemonDir, "node", platform, "bin", name),
		filepath.Join(daemonDir, "node", platform, name),
	} {
		if executableFile(candidate) {
			return candidate, nil
		}
	}
	if resolved, err := exec.LookPath(name); err == nil && executableFile(resolved) {
		return resolved, nil
	}
	return "", errorsNewNativeNode(name)
}

func errorsNewNativeNode(name string) error {
	return fmt.Errorf("portable Node runtime %s was not found", name)
}
