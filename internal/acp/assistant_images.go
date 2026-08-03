package acp

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const maxAssistantImageFileBytes int64 = 6 * 1024 * 1024

type assistantMarkdownImageRef struct {
	name   string
	source string
}

// ResolveAssistantMarkdownImages adapts the ordinary image Markdown emitted by
// ACP agents into Workass's durable inline-media shape. The agent needs no
// Workass-specific tool or syntax: ![label](path) is enough. Only regular raster
// files whose resolved target remains inside the exact chat cwd are imported;
// remote URLs, SVG, symlink escapes, and oversized files are ignored.
func ResolveAssistantMarkdownImages(markdown, cwd string) []any {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" || !strings.Contains(markdown, "![") {
		return nil
	}
	root, err := filepath.Abs(cwd)
	if err != nil {
		return nil
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil
	}

	images := make([]any, 0, 2)
	seen := make(map[string]struct{})
	totalEncoded := 0
	for _, ref := range assistantMarkdownImageRefs(markdown) {
		if len(images) >= maxToolResultImages || totalEncoded >= maxToolResultTotalBytes {
			break
		}
		if _, duplicate := seen[ref.source]; duplicate {
			continue
		}
		seen[ref.source] = struct{}{}
		filename, ok := assistantImageFilename(root, ref.source)
		if !ok {
			continue
		}
		file, err := os.Open(filename)
		if err != nil {
			continue
		}
		info, statErr := file.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxAssistantImageFileBytes {
			_ = file.Close()
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(file, maxAssistantImageFileBytes+1))
		_ = file.Close()
		if readErr != nil || len(data) == 0 || int64(len(data)) > maxAssistantImageFileBytes {
			continue
		}
		mimeType := strings.ToLower(strings.TrimSpace(http.DetectContentType(data[:min(len(data), 512)])))
		if !safeToolImageMIME(mimeType) {
			continue
		}
		encoded := base64.StdEncoding.EncodeToString(data)
		if len(encoded) > maxToolResultImageBytes || totalEncoded+len(encoded) > maxToolResultTotalBytes {
			continue
		}
		name := compactText(ref.name, 160)
		if name == "" {
			name = compactText(filepath.Base(filename), 160)
		}
		image := map[string]any{
			"mimeType": mimeType,
			"data":     encoded,
			"source":   ref.source,
		}
		if name != "" {
			image["name"] = name
		}
		images = append(images, image)
		totalEncoded += len(encoded)
	}
	return images
}

func assistantMarkdownImageRefs(markdown string) []assistantMarkdownImageRef {
	refs := make([]assistantMarkdownImageRef, 0, 2)
	inFence := false
	fenceChar := byte(0)
	for _, line := range strings.Split(markdown, "\n") {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) >= 3 && (strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")) {
			marker := trimmed[0]
			if !inFence {
				inFence, fenceChar = true, marker
			} else if marker == fenceChar {
				inFence, fenceChar = false, 0
			}
			continue
		}
		if inFence {
			continue
		}
		for offset := 0; offset < len(line); {
			startRel := strings.Index(line[offset:], "![")
			if startRel < 0 {
				break
			}
			start := offset + startRel
			labelEndRel := strings.Index(line[start+2:], "](")
			if labelEndRel < 0 {
				break
			}
			labelEnd := start + 2 + labelEndRel
			targetStart := labelEnd + 2
			targetEnd := -1
			if targetStart < len(line) && line[targetStart] == '<' {
				if closeRel := strings.Index(line[targetStart+1:], ">)"); closeRel >= 0 {
					targetEnd = targetStart + 1 + closeRel + 1
				}
			} else if closeRel := strings.IndexByte(line[targetStart:], ')'); closeRel >= 0 {
				targetEnd = targetStart + closeRel
			}
			if targetEnd < 0 {
				offset = targetStart
				continue
			}
			rawTarget := strings.TrimSpace(line[targetStart:targetEnd])
			if strings.HasPrefix(rawTarget, "<") && strings.HasSuffix(rawTarget, ">") {
				rawTarget = strings.TrimSpace(rawTarget[1 : len(rawTarget)-1])
			}
			if rawTarget != "" && !insideInlineCode(line, start) {
				refs = append(refs, assistantMarkdownImageRef{
					name:   strings.TrimSpace(line[start+2 : labelEnd]),
					source: rawTarget,
				})
			}
			offset = targetEnd + 1
		}
	}
	return refs
}

// assistantMarkdownImagePending reports whether the newest natural image token
// is still split across provider chunks. Earlier complete tokens do not clear a
// later incomplete one. A completed but unsafe/missing target is still
// syntactically complete and will be retried once at the terminal boundary.
func assistantMarkdownImagePending(markdown string) bool {
	last := strings.LastIndex(markdown, "![")
	if last < 0 {
		return false
	}
	return len(assistantMarkdownImageRefs(markdown[last:])) == 0
}

func insideInlineCode(line string, index int) bool {
	inside := false
	for i := 0; i < index && i < len(line); i++ {
		if line[i] == '`' && (i == 0 || line[i-1] != '\\') {
			inside = !inside
		}
	}
	return inside
}

func assistantImageFilename(root, source string) (string, bool) {
	source = strings.TrimSpace(source)
	if source == "" || strings.ContainsRune(source, 0) {
		return "", false
	}
	var candidate string
	if filepath.IsAbs(source) {
		candidate = source
	} else if strings.HasPrefix(strings.ToLower(source), "file:") {
		parsed, err := url.Parse(source)
		if err != nil || (parsed.Host != "" && !strings.EqualFold(parsed.Host, "localhost")) {
			return "", false
		}
		candidate, err = url.PathUnescape(parsed.Path)
		if err != nil {
			return "", false
		}
		if runtime.GOOS == "windows" && strings.HasPrefix(candidate, "/") && len(candidate) >= 3 && candidate[2] == ':' {
			candidate = candidate[1:]
		}
	} else {
		if parsed, err := url.Parse(source); err == nil && parsed.Scheme != "" {
			return "", false
		}
		var err error
		candidate, err = url.PathUnescape(source)
		if err != nil {
			return "", false
		}
	}
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return resolved, true
}
