package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const qwenPackageName = "@qwen-code/qwen-code"

type qwenStandaloneManifest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// resolveQwenUpdateCommand keeps the updater provider-owned while working
// around the standalone launcher's extra cli-entry.js layer. Qwen's update
// command identifies standalone installs only when process.argv[1] is its
// bundled lib/cli.js; launching the public shim leaves argv pointing at
// cli-entry.js, so current releases merely print reinstall instructions and
// exit zero. Invoke the same official CLI with its bundled Node when the
// resolved executable belongs to a validated standalone layout. Other install
// forms retain Qwen's public update command and its package-manager guidance.
func resolveQwenUpdateCommand(resolvedCLI string, fallback ProviderUpdateCommand) ProviderUpdateCommand {
	for _, root := range qwenStandaloneRoots(resolvedCLI) {
		manifestBytes, err := os.ReadFile(filepath.Join(root, "manifest.json"))
		if err != nil || len(manifestBytes) > 64*1024 {
			continue
		}
		var manifest qwenStandaloneManifest
		if json.Unmarshal(manifestBytes, &manifest) != nil || manifest.Name != qwenPackageName || strings.TrimSpace(manifest.Version) == "" {
			continue
		}
		cli := filepath.Join(root, "lib", "cli.js")
		node := filepath.Join(root, "node", "bin", "node")
		if runtime.GOOS == "windows" {
			node += ".exe"
		}
		if !regularFile(cli) || !regularFile(node) {
			continue
		}
		return ProviderUpdateCommand{Command: node, Args: []string{cli, "update"}}
	}
	return fallback
}

func qwenStandaloneRoots(resolvedCLI string) []string {
	paths := []string{strings.TrimSpace(resolvedCLI)}
	if real, err := filepath.EvalSymlinks(strings.TrimSpace(resolvedCLI)); err == nil {
		paths = append(paths, real)
	}
	seen := make(map[string]bool)
	roots := make([]string, 0, 4)
	for _, path := range paths {
		if path == "" {
			continue
		}
		prefix := filepath.Dir(filepath.Dir(path))
		for _, root := range []string{prefix, filepath.Join(prefix, "lib", "qwen-code")} {
			root = filepath.Clean(root)
			if seen[root] {
				continue
			}
			seen[root] = true
			roots = append(roots, root)
		}
	}
	return roots
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
