package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	claudeNativeReleaseSource = "https://downloads.claude.ai/claude-code-releases"
	claudeSettingsMaxBytes    = 4 * 1024 * 1024
)

// resolveClaudeLatestVersion answers what `claude update` would actually install.
// A native install follows its own release channel, which trails npm's latest
// tag by hours: comparing against npm offered a version the updater then reported
// as "up to date", so the update failed verification until the channel caught up.
// npm and other install forms keep the registry answer.
func resolveClaudeLatestVersion(ctx context.Context, candidate providerUpdateCandidate, registryLatest string) (string, error) {
	if !claudeNativeInstallForCLI(candidate.resolvedCLI) {
		return registryLatest, nil
	}
	return latestClaudeNativeVersion(ctx, http.DefaultClient, candidate.spec.AssetSource, claudeReleaseChannel())
}

// claudeNativeInstallForCLI recognizes the installer's layout from the executable
// itself, so it answers for the same install the updater will run from: a launcher
// in <prefix>/bin beside <prefix>/share/claude/versions. On Windows the launcher is
// a copy rather than a symlink, and an npm prefix of ~/.local resolves into
// node_modules, which is not a native install even with versions left beside it.
func claudeNativeInstallForCLI(resolvedCLI string) bool {
	resolvedCLI = strings.TrimSpace(resolvedCLI)
	if resolvedCLI == "" {
		return false
	}
	real, err := filepath.EvalSymlinks(resolvedCLI)
	if err != nil {
		return false
	}
	if strings.Contains(filepath.ToSlash(real), "/node_modules/") {
		return false
	}
	if versions := filepath.Dir(real); filepath.Base(versions) == "versions" && filepath.Base(filepath.Dir(versions)) == "claude" {
		return true
	}
	return directoryExists(filepath.Join(filepath.Dir(filepath.Dir(resolvedCLI)), "share", "claude", "versions"))
}

// claudeReleaseChannel reads the channel the updater follows. Anything but an
// explicit "stable" is the updater's default.
func claudeReleaseChannel() string {
	dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "latest"
		}
		dir = filepath.Join(home, ".claude")
	}
	path := filepath.Join(dir, "settings.json")
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > claudeSettingsMaxBytes {
		return "latest"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "latest"
	}
	var settings struct {
		AutoUpdatesChannel string `json:"autoUpdatesChannel"`
	}
	if json.Unmarshal(raw, &settings) != nil || settings.AutoUpdatesChannel != "stable" {
		return "latest"
	}
	return "stable"
}

func latestClaudeNativeVersion(ctx context.Context, client *http.Client, source, channel string) (string, error) {
	source = strings.TrimRight(strings.TrimSpace(source), "/")
	if source == "" {
		return "", fmt.Errorf("Claude native release source is unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source+"/"+channel, nil)
	if err != nil {
		return "", err
	}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", fmt.Errorf("Claude native release status %d", res.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 128))
	if err != nil {
		return "", err
	}
	version := parseSemverToken(string(raw))
	if version == "" {
		return "", fmt.Errorf("Claude native release version not comparable: %q", strings.TrimSpace(string(raw)))
	}
	return version, nil
}
