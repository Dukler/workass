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
	// The Agent SDK labels every process it starts "sdk-ts" unless told
	// otherwise, and Anthropic's 5-hour meter charges that label about 1.7x
	// to 3x more than the interactive CLI for the same tokens (measured by
	// t3code, pingdotgg/t3code#24649). Workass names itself instead; the
	// requests still carry agent-sdk/<version> and the client-app below, so
	// nothing about what is calling is hidden. A provider setting wins.
	if _, set := env["CLAUDE_CODE_ENTRYPOINT"]; !set {
		env["CLAUDE_CODE_ENTRYPOINT"] = claudeWorkassEntrypoint
	}
	if _, set := env["CLAUDE_AGENT_SDK_CLIENT_APP"]; !set {
		env["CLAUDE_AGENT_SDK_CLIENT_APP"] = claudeWorkassEntrypoint
	}
	// Claude Code turns its Artifact tools off only for the sdk-* labels. Under
	// any other name it loads Artifact, ArtifactComments and ArtifactData into
	// every request (about 26k more characters of tool definitions), which
	// Workass chats never had. Off unless the provider or the daemon's own
	// environment says otherwise.
	if _, set := env["CLAUDE_CODE_ARTIFACT"]; !set && os.Getenv("CLAUDE_CODE_ARTIFACT") == "" {
		env["CLAUDE_CODE_ARTIFACT"] = "0"
	}
	env["WORKASS_CLAUDE_SDK_MODULE"] = sdkModule
	provider.Command = node
	provider.ResolvedCommand = ""
	provider.Args = []string{host}
	provider.Env = env
	return provider, nil
}

// claudeWorkassEntrypoint is how Workass's Claude processes name themselves
// (CLAUDE_CODE_ENTRYPOINT and CLAUDE_AGENT_SDK_CLIENT_APP).
const claudeWorkassEntrypoint = "workass"

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
