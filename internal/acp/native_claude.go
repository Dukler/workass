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
	env["WORKASS_CLAUDE_SDK_MODULE"] = sdkModule
	if caFile := strings.TrimSpace(opts.WorkassMCPCACertFile); caFile != "" {
		// Claude Code/Bun treats NODE_EXTRA_CA_CERTS as additional trust
		// material. Verification remains enabled and the daemon private key is
		// never exposed to the provider process.
		env["NODE_EXTRA_CA_CERTS"] = caFile
	}
	provider.Command = node
	provider.ResolvedCommand = ""
	provider.Args = []string{host}
	provider.Env = env
	return provider, nil
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
