// Package headlessupdate gives a headless Workass daemon the same owned,
// exact-machine update surface the packaged Electron shells already expose —
// without a shell, without root, and without replacing the operator's manual
// activation decision.
//
// The Electron updater (desktop/shell/update-manager.js) owns macOS and
// Windows, where an app bundle and a visible window exist. A Linux daemon
// installed as a `systemd --user` unit has neither, so its own activation
// mechanics live here, in Go, beside the binary being replaced. The wire and
// MCP contract is unchanged in shape on purpose: the same
// update.targets/update.status/update.apply methods, the same
// `update <machineId> from <a> to <b>` authorization text, and the same
// operation_id replay receipts.
package headlessupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultFeedRoot mirrors DEFAULT_FEED_ROOT in desktop/shell/update-manager.js:
// one immutable GitHub release, read over HTTPS, discovered through the stable
// /releases/latest/download/ redirect.
const DefaultFeedRoot = "https://github.com/Dukler/workass/releases/latest/download/"

const (
	maxManifestBytes = 256 * 1024
	maxUpdateBytes   = int64(4) << 30
	feedTimeout      = 30 * time.Second
	downloadTimeout  = 30 * time.Minute
)

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// ValidVersion reports whether value is one strict X.Y.Z release version.
func ValidVersion(value string) bool { return versionPattern.MatchString(strings.TrimSpace(value)) }

// CompareVersions orders two strict X.Y.Z versions.
func CompareVersions(left, right string) (int, error) {
	a, aok := parseVersion(left)
	b, bok := parseVersion(right)
	if !aok || !bok {
		return 0, errors.New("Workass release versions must use X.Y.Z")
	}
	for index := 0; index < 3; index++ {
		if a[index] != b[index] {
			if a[index] < b[index] {
				return -1, nil
			}
			return 1, nil
		}
	}
	return 0, nil
}

func parseVersion(value string) ([3]uint64, bool) {
	var out [3]uint64
	fields := versionPattern.FindStringSubmatch(strings.TrimSpace(value))
	if fields == nil {
		return out, false
	}
	for index := 0; index < 3; index++ {
		parsed, err := strconv.ParseUint(fields[index+1], 10, 64)
		if err != nil {
			return out, false
		}
		out[index] = parsed
	}
	return out, true
}

// Artifact is one immutable release file bound by exact size and digest.
type Artifact struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Manifest is the platform release feed. It is the same schemaVersion 1
// document the Mac and Windows feeds use; `headless` marks the artifact as a
// daemon tree rather than an app bundle, exactly as `portable` marks the
// Windows zip.
type Manifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	Product       string `json:"product"`
	Version       string `json:"version"`
	Platform      string `json:"platform"`
	Arch          string `json:"arch"`
	Headless      bool   `json:"headless"`
	Service       string `json:"service,omitempty"`
	Artifacts     struct {
		Update Artifact `json:"update"`
	} `json:"artifacts"`
}

// FeedName is the per-platform manifest file name. It matches
// releaseFeedName() in the Electron updater so one release carries one
// manifest per target and no client has to guess.
func FeedName(platform, arch string) string {
	return fmt.Sprintf("workass-%s-%s-release.json", platform, arch)
}

// FeedURL resolves the manifest URL for this platform, honouring an explicit
// override so tests and a private mirror never reach GitHub.
func FeedURL(override, platform, arch string) (string, error) {
	trimmed := strings.TrimSpace(override)
	if trimmed == "" {
		return DefaultFeedRoot + FeedName(platform, arch), nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return "", errors.New("update feed override must be one absolute http(s) URL")
	}
	return trimmed, nil
}

// ValidateManifest rejects anything that is not this exact platform's release.
func ValidateManifest(raw []byte, platform, arch string) (*Manifest, error) {
	var manifest Manifest
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	if err := decoder.Decode(&manifest); err != nil {
		return nil, errors.New("unsupported Workass release manifest")
	}
	if manifest.SchemaVersion != 1 || manifest.Product != "Workass" {
		return nil, errors.New("unsupported Workass release manifest")
	}
	if !ValidVersion(manifest.Version) {
		return nil, errors.New("release manifest has an invalid version")
	}
	if manifest.Platform != platform || manifest.Arch != arch {
		return nil, fmt.Errorf("release manifest targets %s-%s, not %s-%s", manifest.Platform, manifest.Arch, platform, arch)
	}
	if !manifest.Headless {
		return nil, errors.New("headless Workass updates require a headless release")
	}
	artifact := manifest.Artifacts.Update
	if !sha256Pattern.MatchString(strings.ToLower(artifact.SHA256)) {
		return nil, errors.New("release manifest has no valid update checksum")
	}
	if artifact.Size <= 0 || artifact.Size > maxUpdateBytes {
		return nil, errors.New("release manifest has an invalid update size")
	}
	if strings.TrimSpace(artifact.Name) == "" || strings.TrimSpace(artifact.URL) == "" {
		return nil, errors.New("release manifest has no update artifact")
	}
	return &manifest, nil
}

// fetch reads a bounded HTTPS body. Updater traffic always revalidates: the
// /releases/latest/ redirect intentionally moves as releases are published,
// and a cached redirect would pin a daemon to a stale manifest forever.
func fetch(client *http.Client, target string, maxBytes int64) ([]byte, error) {
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, errors.New("invalid update URL")
	}
	if request.URL.Scheme != "https" && request.URL.Scheme != "http" {
		return nil, errors.New("updates require HTTPS")
	}
	request.Header.Set("user-agent", "Workass-Updater/1")
	request.Header.Set("cache-control", "no-cache, no-store")
	request.Header.Set("pragma", "no-cache")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
	}()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("update server returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, errors.New("update response is too large")
	}
	return body, nil
}

// downloadArtifact streams the release archive to writer and returns its exact
// digest. Size and digest are checked by the caller against the manifest.
func downloadArtifact(client *http.Client, artifact Artifact, writer io.Writer) (string, int64, error) {
	request, err := http.NewRequest(http.MethodGet, artifact.URL, nil)
	if err != nil {
		return "", 0, errors.New("invalid update artifact URL")
	}
	if request.URL.Scheme != "https" && request.URL.Scheme != "http" {
		return "", 0, errors.New("updates require HTTPS")
	}
	request.Header.Set("user-agent", "Workass-Updater/1")
	request.Header.Set("cache-control", "no-cache, no-store")
	response, err := client.Do(request)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", 0, fmt.Errorf("update download returned HTTP %d", response.StatusCode)
	}
	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(writer, digest), io.LimitReader(response.Body, artifact.Size+1))
	if err != nil {
		return "", written, err
	}
	if written != artifact.Size {
		return "", written, fmt.Errorf("update download is %d bytes, not the %d the manifest binds", written, artifact.Size)
	}
	return hex.EncodeToString(digest.Sum(nil)), written, nil
}
