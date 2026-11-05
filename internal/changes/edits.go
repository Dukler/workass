package changes

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
)

// IsEditTool reports whether a provider tool call changed files: the ACP
// kinds edit, delete and move. A failed call changed nothing.
func IsEditTool(toolKind, status string) bool {
	switch strings.ToLower(strings.TrimSpace(toolKind)) {
	case "edit", "delete", "move":
	default:
		return false
	}
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed", "error", "cancelled", "canceled":
		return false
	}
	return true
}

// inputPathPattern finds file paths in a tool input that was cut short and is
// no longer valid JSON (the daemon keeps 400 characters of it).
var inputPathPattern = regexp.MustCompile(`"(?:file_path|filePath|notebook_path|path|target_file)"\s*:\s*"((?:[^"\\]|\\.)+)"`)

// lineSuffix is the ":LINE" an ACP location carries after its path.
var lineSuffix = regexp.MustCompile(`:\d+$`)

// EditPaths returns the files one edit tool call names: its location and the
// paths in its input (Claude's file_path, Codex's changes[].path). Relative
// paths are resolved against cwd; anything that is not a local path (a URL,
// an empty string) is left out. The result has no duplicates.
func EditPaths(location, input, cwd string) []string {
	seen := map[string]struct{}{}
	var paths []string
	add := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" || strings.Contains(raw, "://") {
			return
		}
		raw = lineSuffix.ReplaceAllString(raw, "")
		if !filepath.IsAbs(raw) {
			if strings.TrimSpace(cwd) == "" {
				return
			}
			raw = filepath.Join(cwd, raw)
		}
		raw = filepath.Clean(raw)
		if _, ok := seen[raw]; ok {
			return
		}
		seen[raw] = struct{}{}
		paths = append(paths, raw)
	}
	add(location)
	input = strings.TrimSpace(input)
	if input == "" {
		return paths
	}
	var decoded any
	if json.Unmarshal([]byte(input), &decoded) == nil {
		collectInputPaths(decoded, add, 0)
		return paths
	}
	for _, match := range inputPathPattern.FindAllStringSubmatch(input, -1) {
		var unquoted string
		if json.Unmarshal([]byte(`"`+match[1]+`"`), &unquoted) == nil {
			add(unquoted)
		}
	}
	return paths
}

func collectInputPaths(value any, add func(string), depth int) {
	if depth > 3 {
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range []string{"file_path", "filePath", "notebook_path", "path", "target_file"} {
			if text, ok := typed[key].(string); ok {
				add(text)
			}
		}
		if changes, ok := typed["changes"].([]any); ok {
			for _, change := range changes {
				collectInputPaths(change, add, depth+1)
			}
		}
		if edits, ok := typed["edits"].([]any); ok {
			for _, edit := range edits {
				collectInputPaths(edit, add, depth+1)
			}
		}
	case []any:
		for _, item := range typed {
			collectInputPaths(item, add, depth+1)
		}
	}
}
