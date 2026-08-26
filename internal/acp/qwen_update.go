package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

const (
	qwenPackageName                 = "@qwen-code/qwen-code"
	qwenRegistryMetadataMaxBytes    = 8 * 1024 * 1024
	qwenStandaloneVersionProbeLimit = 32
	qwenStandaloneUpdateScript      = `import { pathToFileURL } from "node:url"; const { performStandaloneUpdate } = await import(pathToFileURL(process.argv[1]).href); await performStandaloneUpdate(process.argv[2], process.argv[3]);`
)

var qwenStandaloneSemverRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?$`)

type qwenStandaloneManifest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Target  string `json:"target"`
}

type qwenStandaloneInstall struct {
	Root         string
	Node         string
	UpdateModule string
	Target       string
	Version      string
}

// resolveQwenUpdateCommand keeps the updater provider-owned while pinning a
// standalone update to the release Workass actually checked. Qwen's public
// `update` command re-reads npm's latest tag; that tag can lead the standalone
// release assets and make a valid Workass target fail with a 403/404. Current
// standalone bundles export their verified, rollback-capable updater, so invoke
// that official implementation with the exact compatible target. npm and other
// install forms retain Qwen's public update command and package-manager advice.
func resolveQwenUpdateCommand(resolvedCLI, target string, fallback ProviderUpdateCommand) ProviderUpdateCommand {
	target, valid := normalizeQwenStandaloneVersion(target)
	if !valid {
		return fallback
	}
	install, ok := qwenStandaloneInstallForCLI(resolvedCLI)
	if !ok {
		return fallback
	}
	return ProviderUpdateCommand{
		Command: install.Node,
		Args: []string{
			"--input-type=module",
			"--eval",
			qwenStandaloneUpdateScript,
			install.UpdateModule,
			install.Root,
			target,
		},
	}
}

func qwenStandaloneInstallForCLI(resolvedCLI string) (qwenStandaloneInstall, bool) {
	for _, root := range qwenStandaloneRoots(resolvedCLI) {
		manifest, ok := readQwenStandaloneManifest(filepath.Join(root, "manifest.json"))
		if !ok || manifest.Name != qwenPackageName {
			continue
		}
		version, valid := normalizeQwenStandaloneVersion(manifest.Version)
		if !valid {
			continue
		}
		target := strings.TrimSpace(manifest.Target)
		if target == "" {
			target, valid = qwenStandaloneTargetForRuntime()
		}
		if !validQwenStandaloneTarget(target) {
			continue
		}
		node := filepath.Join(root, "node", "bin", "node")
		if runtime.GOOS == "windows" {
			node += ".exe"
		}
		modules, err := filepath.Glob(filepath.Join(root, "lib", "chunks", "standalone-update-*.js"))
		if err != nil || len(modules) != 1 || !regularFile(node) || !regularFile(modules[0]) {
			continue
		}
		return qwenStandaloneInstall{
			Root:         root,
			Node:         node,
			UpdateModule: modules[0],
			Target:       target,
			Version:      version,
		}, true
	}
	return qwenStandaloneInstall{}, false
}

func readQwenStandaloneManifest(path string) (qwenStandaloneManifest, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 64*1024 {
		return qwenStandaloneManifest{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return qwenStandaloneManifest{}, false
	}
	var manifest qwenStandaloneManifest
	if json.Unmarshal(raw, &manifest) != nil {
		return qwenStandaloneManifest{}, false
	}
	return manifest, true
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

func qwenStandaloneTargetForRuntime() (string, bool) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return "darwin-arm64", true
	case "darwin/amd64":
		return "darwin-x64", true
	case "linux/arm64":
		return "linux-arm64", true
	case "linux/amd64":
		return "linux-x64", true
	case "windows/amd64":
		return "win-x64", true
	default:
		return "", false
	}
}

func validQwenStandaloneTarget(target string) bool {
	switch strings.TrimSpace(target) {
	case "darwin-arm64", "darwin-x64", "linux-arm64", "linux-x64", "win-x64":
		return true
	default:
		return false
	}
}

func normalizeQwenStandaloneVersion(raw string) (string, bool) {
	version := strings.TrimPrefix(strings.TrimSpace(raw), "v")
	return version, qwenStandaloneSemverRE.MatchString(version)
}

func resolveQwenLatestVersion(ctx context.Context, candidate providerUpdateCandidate, registryLatest string) (string, error) {
	install, ok := qwenStandaloneInstallForCLI(candidate.resolvedCLI)
	if !ok {
		return registryLatest, nil
	}
	return latestQwenStandaloneVersion(ctx, http.DefaultClient, candidate, install, registryLatest)
}

func latestQwenStandaloneVersion(ctx context.Context, client *http.Client, candidate providerUpdateCandidate, install qwenStandaloneInstall, registryLatest string) (string, error) {
	registryLatest, valid := normalizeQwenStandaloneVersion(registryLatest)
	if !valid {
		return "", fmt.Errorf("Qwen registry version is not a standalone version")
	}
	comparison, comparable := compareLenientSemver(candidate.installed, registryLatest)
	if !comparable || comparison <= 0 {
		return registryLatest, nil
	}
	assetBase := strings.TrimSpace(candidate.spec.AssetSource)
	if assetBase == "" {
		return "", fmt.Errorf("Qwen standalone release source is unavailable")
	}
	available, err := qwenStandaloneAssetAvailable(ctx, client, assetBase, registryLatest, install.Target)
	if err != nil {
		return "", err
	}
	if available {
		return registryLatest, nil
	}

	versions, err := qwenRegistryVersions(ctx, client, candidate.spec.Source)
	if err != nil {
		return "", err
	}
	candidates := make([]string, 0, len(versions))
	for raw := range versions {
		version, valid := normalizeQwenStandaloneVersion(raw)
		if !valid || version == registryLatest {
			continue
		}
		newerThanInstalled, comparable := compareLenientSemver(candidate.installed, version)
		if !comparable || newerThanInstalled <= 0 {
			continue
		}
		noNewerThanRegistry, comparable := compareLenientSemver(version, registryLatest)
		if !comparable || noNewerThanRegistry < 0 {
			continue
		}
		candidates = append(candidates, version)
	}
	sort.Slice(candidates, func(i, j int) bool {
		comparison, _ := compareLenientSemver(candidates[j], candidates[i])
		return comparison > 0
	})
	if len(candidates) > qwenStandaloneVersionProbeLimit {
		candidates = candidates[:qwenStandaloneVersionProbeLimit]
	}
	for _, version := range candidates {
		available, err := qwenStandaloneAssetAvailable(ctx, client, assetBase, version, install.Target)
		if err != nil {
			return "", err
		}
		if available {
			return version, nil
		}
	}
	// npm can publish before the standalone channel. The installed version is
	// the authoritative no-update result until a compatible asset appears.
	return candidate.installed, nil
}

func qwenRegistryVersions(ctx context.Context, client *http.Client, latestSource string) (map[string]json.RawMessage, error) {
	metadataSource, err := qwenRegistryMetadataSource(latestSource)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataSource, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.npm.install-v1+json")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("Qwen registry metadata status %d", res.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, qwenRegistryMetadataMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > qwenRegistryMetadataMaxBytes {
		return nil, fmt.Errorf("Qwen registry metadata exceeds size limit")
	}
	var body struct {
		Versions map[string]json.RawMessage `json:"versions"`
	}
	if json.Unmarshal(raw, &body) != nil || len(body.Versions) == 0 {
		return nil, fmt.Errorf("Qwen registry metadata has no versions")
	}
	return body.Versions, nil
}

func qwenRegistryMetadataSource(latestSource string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(latestSource))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("Qwen registry source is invalid")
	}
	path := strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(path, "/latest") {
		return "", fmt.Errorf("Qwen registry source has no latest endpoint")
	}
	parsed.Path = strings.TrimSuffix(path, "/latest")
	parsed.RawPath = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func qwenStandaloneAssetAvailable(ctx context.Context, client *http.Client, base, version, target string) (bool, error) {
	version, valid := normalizeQwenStandaloneVersion(version)
	if !valid || !validQwenStandaloneTarget(target) {
		return false, fmt.Errorf("Qwen standalone release coordinates are invalid")
	}
	extension := ".tar.gz"
	if strings.HasPrefix(target, "win-") {
		extension = ".zip"
	}
	assetURL := strings.TrimRight(strings.TrimSpace(base), "/") + "/v" + version + "/qwen-code-" + target + extension
	available, retryWithGet, err := probeQwenStandaloneAsset(ctx, client, http.MethodHead, assetURL)
	if err != nil || !retryWithGet {
		return available, err
	}
	return probeQwenStandaloneAssetWithRange(ctx, client, assetURL)
}

func probeQwenStandaloneAsset(ctx context.Context, client *http.Client, method, assetURL string) (available, retryWithGet bool, err error) {
	req, err := http.NewRequestWithContext(ctx, method, assetURL, nil)
	if err != nil {
		return false, false, err
	}
	res, err := client.Do(req)
	if err != nil {
		return false, false, err
	}
	res.Body.Close()
	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return true, false, nil
	case res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusGone:
		return false, false, nil
	case res.StatusCode == http.StatusMethodNotAllowed || res.StatusCode == http.StatusNotImplemented:
		return false, true, nil
	default:
		return false, false, fmt.Errorf("Qwen standalone release status %d", res.StatusCode)
	}
}

func probeQwenStandaloneAssetWithRange(ctx context.Context, client *http.Client, assetURL string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Range", "bytes=0-0")
	res, err := client.Do(req)
	if err != nil {
		return false, err
	}
	res.Body.Close()
	switch {
	case res.StatusCode == http.StatusOK || res.StatusCode == http.StatusPartialContent:
		return true, nil
	case res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusGone:
		return false, nil
	default:
		return false, fmt.Errorf("Qwen standalone release status %d", res.StatusCode)
	}
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
