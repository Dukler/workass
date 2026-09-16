package acp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// OMP's native default can be yolo; never label it as read-only.
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

// ompNativeHostLaunch loads OMP's SDK in Bun. Persisted "acp" arguments are
// deliberately not forwarded: the SDK owns the native agent/session engine.
func ompNativeHostLaunch(provider ProviderConfig, opts Options, daemonExecutable string) (ProviderConfig, error) {
	root := strings.TrimSpace(opts.RootDir)
	platform := runtime.GOOS + "-" + runtime.GOARCH
	daemonDir := filepath.Dir(strings.TrimSpace(daemonExecutable))
	host, err := firstNativeFile(strings.TrimSpace(os.Getenv("WORKASS_OMP_HOST")), []string{
		filepath.Join(daemonDir, "frontier-hosts", platform, "omp-native-host.mjs"),
		filepath.Join(daemonDir, "frontier-hosts", "omp-native-host.mjs"),
		filepath.Join(root, "scripts", "omp-native-host.mjs"),
	})
	if err != nil {
		return ProviderConfig{}, fmt.Errorf("OMP SDK host: %w", err)
	}
	sdk, err := firstNativeFile(strings.TrimSpace(firstNonEmpty(provider.Env["WORKASS_OMP_SDK_MODULE"], os.Getenv("WORKASS_OMP_SDK_MODULE"))), []string{
		filepath.Join(daemonDir, "frontier-hosts", platform, "node_modules", "@oh-my-pi", "pi-coding-agent", "src", "index.ts"),
		filepath.Join(daemonDir, "frontier-hosts", "node_modules", "@oh-my-pi", "pi-coding-agent", "src", "index.ts"),
		filepath.Join(root, "dist-bin", "frontier-hosts", platform, "node_modules", "@oh-my-pi", "pi-coding-agent", "src", "index.ts"),
	})
	if err != nil {
		return ProviderConfig{}, fmt.Errorf("OMP SDK module: %w", err)
	}
	bun, err := resolveOMPHostBun(daemonDir, root, platform)
	if err != nil {
		return ProviderConfig{}, err
	}
	env := copyStringMap(provider.Env)
	if env == nil {
		env = map[string]string{}
	}
	env["WORKASS_OMP_SDK_MODULE"] = sdk
	provider.Command = bun
	provider.ResolvedCommand = ""
	provider.Args = []string{host}
	provider.Env = env
	return provider, nil
}

func resolveOMPHostBun(daemonDir, root, platform string) (string, error) {
	if explicit := strings.TrimSpace(os.Getenv("WORKASS_BUN")); explicit != "" {
		return resolveFrontierNativeCommand(explicit, "WORKASS_BUN")
	}
	name := "bun"
	if runtime.GOOS == "windows" {
		name = "bun.exe"
	}
	for _, base := range []string{filepath.Join(daemonDir, "frontier-hosts", platform), filepath.Join(daemonDir, "frontier-hosts"), filepath.Join(root, "dist-bin", "frontier-hosts", platform)} {
		for _, candidate := range []string{filepath.Join(base, name), filepath.Join(base, "bun", name), filepath.Join(base, "bun", "bin", name)} {
			if executableFile(candidate) {
				return candidate, nil
			}
		}
	}
	if resolved, err := exec.LookPath(name); err == nil && executableFile(resolved) {
		return resolved, nil
	}
	return "", fmt.Errorf("OMP native SDK requires the staged Bun runtime")
}
