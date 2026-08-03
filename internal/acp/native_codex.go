package acp

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// codexNativeHostLaunch runs the Workass protocol host while handing the
// official Codex executable to it for `codex app-server`. The returned process
// tree contains no ACP compatibility adapter.
func codexNativeHostLaunch(provider ProviderConfig, opts Options, daemonExecutable string) (ProviderConfig, error) {
	root := strings.TrimSpace(opts.RootDir)
	platform := runtime.GOOS + "-" + runtime.GOARCH
	daemonDir := filepath.Dir(strings.TrimSpace(daemonExecutable))
	host, err := firstNativeFile(strings.TrimSpace(os.Getenv("WORKASS_CODEX_HOST")), []string{
		filepath.Join(daemonDir, "frontier-hosts", platform, "codex-native-host.mjs"),
		filepath.Join(daemonDir, "frontier-hosts", "codex-native-host.mjs"),
		filepath.Join(root, "scripts", "codex-native-host.mjs"),
	})
	if err != nil {
		return ProviderConfig{}, fmt.Errorf("Codex app-server host: %w", err)
	}
	node, err := resolveNativeNode(daemonDir, platform)
	if err != nil {
		return ProviderConfig{}, err
	}
	env := copyStringMap(provider.Env)
	if env == nil {
		env = make(map[string]string)
	}
	env["WORKASS_CODEX_EXECUTABLE"] = launchCommand(provider)
	provider.Command = node
	provider.ResolvedCommand = ""
	provider.Args = []string{host}
	provider.Env = env
	return provider, nil
}
