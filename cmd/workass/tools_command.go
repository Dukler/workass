package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func resolveWorkassToolsCommand(daemonExecutable, explicit string, prod bool, goos string) (string, error) {
	if !prod && explicit != "" {
		command, err := existingRegularExecutable(explicit)
		if err != nil {
			return "", err
		}
		if daemonExecutable != "" && filepath.Clean(command) == filepath.Clean(daemonExecutable) {
			return "", errors.New("tools executable must not be the daemon executable")
		}
		return command, nil
	}
	if daemonExecutable == "" || !filepath.IsAbs(daemonExecutable) {
		return "", errors.New("daemon executable path is unavailable")
	}
	name := "workass-tools"
	if goos == "windows" {
		name += ".exe"
	}
	return existingRegularExecutable(filepath.Join(filepath.Dir(daemonExecutable), name))
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
