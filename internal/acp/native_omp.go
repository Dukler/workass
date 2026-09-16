package acp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type ompPermissionPolicy struct{}

func (ompPermissionPolicy) Candidates(intent string) []string {
	switch intent {
	case "read":
		return []string{"plan"}
	case "edit":
		return []string{"write"}
	case "full":
		return []string{"yolo"}
	}
	return nil
}
func (ompPermissionPolicy) Intent(modeID string) string {
	switch modeID {
	case "plan":
		return "read"
	case "write":
		return "edit"
	case "yolo":
		return "full"
	}
	return ""
}

// ompNativeHostLaunch connects the Workass host to the user's installed OMP.
// Workass does not package OMP's engine, SDK, or Bun runtime.
func ompNativeHostLaunch(provider ProviderConfig, opts Options, daemonExecutable string) (ProviderConfig, error) {
	root, platform := strings.TrimSpace(opts.RootDir), runtime.GOOS+"-"+runtime.GOARCH
	daemonDir := filepath.Dir(strings.TrimSpace(daemonExecutable))
	installed, err := resolveInstalledOMPExecutable(provider)
	if err != nil {
		return ProviderConfig{}, err
	}
	host, err := firstNativeFile(strings.TrimSpace(os.Getenv("WORKASS_OMP_HOST")), []string{filepath.Join(daemonDir, "frontier-hosts", platform, "omp-installed-host.mjs"), filepath.Join(daemonDir, "frontier-hosts", "omp-installed-host.mjs"), filepath.Join(root, "scripts", "omp-installed-host.mjs")})
	if err != nil {
		return ProviderConfig{}, fmt.Errorf("OMP native host: %w", err)
	}
	node, err := resolveNativeNode(daemonDir, platform)
	if err != nil {
		return ProviderConfig{}, err
	}
	env := copyStringMap(provider.Env)
	if env == nil {
		env = map[string]string{}
	}
	env["WORKASS_OMP_EXECUTABLE"] = installed
	provider.Command, provider.ResolvedCommand, provider.Args, provider.Env = node, "", []string{host}, env
	return provider, nil
}

func resolveInstalledOMPExecutable(provider ProviderConfig) (string, error) {
	if explicit := strings.TrimSpace(os.Getenv("WORKASS_OMP")); explicit != "" {
		return resolveFrontierNativeCommand(explicit, "WORKASS_OMP")
	}
	command := strings.TrimSpace(provider.Command)
	if command != "" && command != "omp" {
		return resolveFrontierNativeCommand(command, "provider command")
	}
	if cached := strings.TrimSpace(provider.ResolvedCommand); cached != "" {
		if resolved, err := resolveFrontierNativeCommand(cached, "resolved OMP command"); err == nil {
			return resolved, nil
		}
	}
	for _, name := range []string{"omp", "omp.exe", "omp.cmd"} {
		if resolved, err := exec.LookPath(name); err == nil && executableFile(resolved) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("OMP installed executable: omp was not found on PATH (set WORKASS_OMP to the existing install)")
}
