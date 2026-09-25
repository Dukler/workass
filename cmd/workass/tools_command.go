package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// windowsToolsLauncher is the release-relative Windows tools launcher. It runs
// the JavaScript client with the bundled OpenJS-signed Node executable.
var windowsToolsLauncher = struct{ launcher, client, node string }{
	launcher: filepath.Join("frontier-hosts", "windows-amd64", "workass-tools.cmd"),
	client:   filepath.Join("frontier-hosts", "windows-amd64", "workass-tools.mjs"),
	node:     filepath.Join("node", "windows-amd64", "node.exe"),
}

func resolveWorkassToolsCommand(daemonExecutable, explicit string, prod bool, goos string) (string, error) {
	if !prod && strings.TrimSpace(explicit) != "" {
		return existingRegularExecutable(explicit)
	}
	if strings.TrimSpace(daemonExecutable) == "" || !filepath.IsAbs(daemonExecutable) {
		return "", errors.New("daemon executable path is unavailable")
	}
	daemonExecutable = filepath.Clean(daemonExecutable)
	if goos == "windows" {
		if launcher, ok := windowsSignedRuntimeToolsLauncher(filepath.Dir(daemonExecutable)); ok {
			return launcher, nil
		}
	}
	// Older release layouts without the signed-runtime launcher keep the
	// in-process daemon entrypoint so provider tools stay available.
	return daemonExecutable, nil
}

func windowsSignedRuntimeToolsLauncher(root string) (string, bool) {
	launcher := filepath.Join(root, windowsToolsLauncher.launcher)
	for _, required := range []string{launcher, filepath.Join(root, windowsToolsLauncher.client), filepath.Join(root, windowsToolsLauncher.node)} {
		info, err := os.Stat(required)
		if err != nil || !info.Mode().IsRegular() {
			return "", false
		}
	}
	return launcher, true
}

func existingRegularExecutable(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("tools executable path must be absolute")
	}
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("tools executable %q is unavailable: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("tools executable %q is not a regular file", path)
	}
	return path, nil
}
