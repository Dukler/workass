package acp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type piPermissionPolicy struct{}

// Pi has no built-in read-only/approval modes. Its extension guards remain
// authoritative; never map a read/edit request to unrestricted native defaults.
func (piPermissionPolicy) Candidates(intent string) []string {
	if intent == "full" {
		return []string{"native"}
	}
	return nil
}
func (piPermissionPolicy) Intent(modeID string) string {
	if modeID == "native" {
		return "full"
	}
	return ""
}

// piNativeHostLaunch connects the Workass host to the user's installed Pi.
// Workass does not package Pi's engine, SDK, or extensions.
func piNativeHostLaunch(provider ProviderConfig, opts Options, daemonExecutable string) (ProviderConfig, error) {
	root, platform := strings.TrimSpace(opts.RootDir), runtime.GOOS+"-"+runtime.GOARCH
	daemonDir := filepath.Dir(strings.TrimSpace(daemonExecutable))
	installed, err := resolveInstalledPiExecutable(provider)
	if err != nil {
		return ProviderConfig{}, err
	}
	host, err := firstNativeFile(strings.TrimSpace(os.Getenv("WORKASS_PI_HOST")), []string{filepath.Join(daemonDir, "frontier-hosts", platform, "pi-native-host.mjs"), filepath.Join(daemonDir, "frontier-hosts", "pi-native-host.mjs"), filepath.Join(root, "scripts", "pi-native-host.mjs")})
	if err != nil {
		return ProviderConfig{}, fmt.Errorf("Pi native host: %w", err)
	}
	node, err := resolveNativeNode(daemonDir, platform)
	if err != nil {
		return ProviderConfig{}, err
	}
	env := piNativeHostEnvironment(provider.Env, installed, runtime.GOOS)
	provider.Command, provider.ResolvedCommand, provider.Args, provider.Env = node, "", []string{host}, env
	return provider, nil
}

func piNativeHostEnvironment(configured map[string]string, installed, goos string) map[string]string {
	env := copyStringMap(configured)
	if env == nil {
		env = map[string]string{}
	}
	// Node reads this at startup. Trust enterprise roots installed in Windows
	// without disabling certificate verification or modifying the user's profile.
	if goos == "windows" {
		env["NODE_USE_SYSTEM_CA"] = "1"
	}
	env["WORKASS_PI_EXECUTABLE"] = installed
	return env
}

func resolveInstalledPiExecutable(provider ProviderConfig) (string, error) {
	if explicit := strings.TrimSpace(os.Getenv("WORKASS_PI")); explicit != "" {
		return resolveFrontierNativeCommand(explicit, "WORKASS_PI")
	}
	command := strings.TrimSpace(provider.Command)
	if command != "" && command != "pi" {
		return resolveFrontierNativeCommand(command, "provider command")
	}
	if cached := strings.TrimSpace(provider.ResolvedCommand); cached != "" {
		if resolved, err := resolveFrontierNativeCommand(cached, "resolved Pi command"); err == nil {
			return resolved, nil
		}
	}
	for _, name := range []string{"pi", "pi.exe", "pi.cmd"} {
		if resolved, err := exec.LookPath(name); err == nil && executableFile(resolved) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("Pi installed executable: pi was not found on PATH (set WORKASS_PI to the existing install)")
}
