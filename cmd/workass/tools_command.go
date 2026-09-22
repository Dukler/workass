package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func resolveWorkassToolsCommand(daemonExecutable, explicit string, prod bool, _ string) (string, error) {
	if !prod && strings.TrimSpace(explicit) != "" {
		return existingRegularExecutable(explicit)
	}
	if strings.TrimSpace(daemonExecutable) == "" || !filepath.IsAbs(daemonExecutable) {
		return "", errors.New("daemon executable path is unavailable")
	}
	// The running daemon already contains the tools client. A quarantined or
	// absent compatibility helper must not disable provider tools.
	return filepath.Clean(daemonExecutable), nil
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
