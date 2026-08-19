package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	maxProjectIconBytes      = 256 * 1024
	maxProjectMetadataBytes  = 256 * 1024
	maxProjectIconSourceTags = 128
)

// Common project icon locations, in preference order. The list follows the
// favicon/app-icon paths used by contemporary web and desktop projects.
// desktop/assets is included so
// Workass itself and similarly-shaped Electron repositories resolve their real
// icon instead of a generic folder.
var projectIconCandidates = []string{
	"favicon.svg",
	"favicon.ico",
	"favicon.png",
	"public/favicon.svg",
	"public/favicon.ico",
	"public/favicon.png",
	"app/favicon.ico",
	"app/favicon.png",
	"app/icon.svg",
	"app/icon.png",
	"app/icon.ico",
	"src/favicon.ico",
	"src/favicon.svg",
	"src/app/favicon.ico",
	"src/app/icon.svg",
	"src/app/icon.png",
	"assets/icon.svg",
	"assets/icon.png",
	"assets/logo.svg",
	"assets/logo.png",
	"desktop/assets/icon.svg",
	"desktop/assets/icon.png",
	"desktop/assets/icon.ico",
	".idea/icon.svg",
}

var projectIconSourceFiles = []string{
	"index.html",
	"public/index.html",
	"app/routes/__root.tsx",
	"src/routes/__root.tsx",
	"app/root.tsx",
	"src/root.tsx",
	"src/index.html",
}

var (
	projectLinkTagRE  = regexp.MustCompile(`(?is)<link\b[^>]{0,1000}>`)
	projectLinkAttrRE = regexp.MustCompile(`(?i)([a-z_:][a-z0-9_.:-]*)\s*=\s*["']([^"']*)["']`)
)

type projectIconResult struct {
	Found    bool   `json:"found"`
	MIMEType string `json:"mimeType,omitempty"`
	Base64   string `json:"base64,omitempty"`
}

// projectIconForWorkspace resolves and reads one small image beneath cwd. It
// never returns a daemon-local path to the renderer. Missing, invalid, unsafe,
// or oversized candidates all degrade to Found=false so the UI can keep the
// generic folder fallback.
func projectIconForWorkspace(cwd string) projectIconResult {
	root, err := canonicalProjectRoot(cwd)
	if err != nil {
		return projectIconResult{}
	}

	candidates := make([]string, 0, len(projectIconCandidates)+8)
	candidates = append(candidates, configuredProjectIconCandidates(root)...)
	candidates = append(candidates, projectIconCandidates...)
	candidates = append(candidates, declaredProjectIconCandidates(root)...)

	seen := make(map[string]struct{}, len(candidates))
	for _, relativePath := range candidates {
		for _, candidate := range projectIconReferenceCandidates(relativePath) {
			key := strings.ToLower(filepath.Clean(candidate))
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			resolved, ok := resolveProjectIconCandidate(root, candidate)
			if !ok {
				continue
			}
			data, ok := readBoundedProjectFile(resolved, maxProjectIconBytes)
			if !ok {
				continue
			}
			mimeType := validProjectIconMIME(resolved, data)
			if mimeType == "" {
				continue
			}
			return projectIconResult{
				Found:    true,
				MIMEType: mimeType,
				Base64:   base64.StdEncoding.EncodeToString(data),
			}
		}
	}
	return projectIconResult{}
}

func canonicalProjectRoot(cwd string) (string, error) {
	clean := strings.TrimSpace(cwd)
	if clean == "" {
		return "", os.ErrNotExist
	}
	abs, err := filepath.Abs(clean)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(abs))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", os.ErrInvalid
	}
	return real, nil
}

func configuredProjectIconCandidates(root string) []string {
	var out []string
	for _, config := range []struct {
		name string
		pick func(map[string]any) string
	}{
		{name: "t3.json", pick: func(value map[string]any) string { return mapString(value, "iconPath") }},
	} {
		path, ok := resolveProjectMetadataFile(root, config.name)
		if !ok {
			continue
		}
		data, ok := readBoundedProjectFile(path, maxProjectMetadataBytes)
		if !ok {
			continue
		}
		var value map[string]any
		if json.Unmarshal(data, &value) != nil {
			continue
		}
		if candidate := strings.TrimSpace(config.pick(value)); candidate != "" {
			out = append(out, candidate)
		}
	}
	return out
}

func mapString(value map[string]any, key string) string {
	text, _ := value[key].(string)
	return strings.TrimSpace(text)
}

func resolveProjectMetadataFile(root, relativePath string) (string, bool) {
	clean := filepath.Clean(filepath.FromSlash(strings.TrimSpace(relativePath)))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	real, err := filepath.EvalSymlinks(filepath.Join(root, clean))
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	info, err := os.Stat(real)
	return real, err == nil && info.Mode().IsRegular() && info.Size() > 0 && info.Size() <= maxProjectMetadataBytes
}

func declaredProjectIconCandidates(root string) []string {
	var out []string
	for _, sourceName := range projectIconSourceFiles {
		path, ok := resolveProjectMetadataFile(root, sourceName)
		if !ok {
			continue
		}
		data, ok := readBoundedProjectFile(path, maxProjectMetadataBytes)
		if !ok {
			continue
		}
		if href := projectIconHref(data); href != "" {
			out = append(out, href)
		}
	}
	return out
}

func projectIconHref(source []byte) string {
	tags := projectLinkTagRE.FindAll(source, maxProjectIconSourceTags)
	for _, tag := range tags {
		attrs := make(map[string]string)
		for _, match := range projectLinkAttrRE.FindAllSubmatch(tag, -1) {
			if len(match) != 3 {
				continue
			}
			attrs[strings.ToLower(string(match[1]))] = strings.TrimSpace(string(match[2]))
		}
		rel := strings.ToLower(attrs["rel"])
		if !strings.Contains(rel, "icon") {
			continue
		}
		if href := strings.TrimSpace(attrs["href"]); href != "" {
			return href
		}
	}
	return ""
}

func projectIconReferenceCandidates(reference string) []string {
	clean := strings.TrimSpace(reference)
	if clean == "" || strings.HasPrefix(clean, "//") {
		return nil
	}
	if parsed, err := url.Parse(clean); err == nil {
		if parsed.Scheme != "" || parsed.Host != "" {
			return nil
		}
		clean = parsed.Path
	}
	decoded, err := url.PathUnescape(clean)
	if err != nil {
		return nil
	}
	clean = strings.TrimLeft(decoded, "/\\")
	if clean == "" {
		return nil
	}
	if filepath.Ext(clean) == "" {
		return []string{clean + ".png", clean + ".ico", clean + ".svg"}
	}
	// A root-relative browser favicon conventionally lives in public/. Try that
	// first, then the literal project-relative path.
	if strings.HasPrefix(strings.TrimSpace(reference), "/") {
		return []string{filepath.Join("public", filepath.FromSlash(clean)), filepath.FromSlash(clean)}
	}
	return []string{filepath.FromSlash(clean)}
}

func resolveProjectIconCandidate(root, relativePath string) (string, bool) {
	clean := filepath.Clean(filepath.FromSlash(strings.TrimSpace(relativePath)))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	candidate := filepath.Join(root, clean)
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	info, err := os.Stat(real)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxProjectIconBytes {
		return "", false
	}
	return real, true
}

func readBoundedProjectFile(path string, limit int64) ([]byte, bool) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || len(data) == 0 || int64(len(data)) > limit {
		return nil, false
	}
	return data, true
}

func validProjectIconMIME(path string, data []byte) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		if bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
			return "image/png"
		}
	case ".jpg", ".jpeg":
		if len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff {
			return "image/jpeg"
		}
	case ".gif":
		if bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")) {
			return "image/gif"
		}
	case ".webp":
		if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
			return "image/webp"
		}
	case ".ico":
		if len(data) >= 6 && bytes.Equal(data[:4], []byte{0, 0, 1, 0}) {
			return "image/x-icon"
		}
	case ".svg":
		if safeProjectSVG(data) {
			return "image/svg+xml"
		}
	}
	return ""
}

// SVGs render in an <img>, but keep the accepted subset inert as an additional
// boundary: no scripts, event handlers, foreign HTML, entities, imports, or
// external resource references. A rejected SVG simply yields the folder icon.
func safeProjectSVG(data []byte) bool {
	lower := strings.ToLower(strings.TrimSpace(string(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf}))))
	if !strings.Contains(lower, "<svg") {
		return false
	}
	for _, blocked := range []string{
		"<script", "<foreignobject", "<!doctype", "<!entity", "javascript:",
		" onload=", " onerror=", "@import", "url(http", "href=\"http", "href='http",
		"xlink:href=\"http", "xlink:href='http",
	} {
		if strings.Contains(lower, blocked) {
			return false
		}
	}
	return true
}
