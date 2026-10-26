package acp

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ImageCloneSourceField names, on a provider image map handed to the
// attachment persister, local files that may hold exactly that image (the file
// a "View image" call read). The image store clones its raw form from one that
// verifies byte for byte. It is a hint only, set on copies that go to the
// persister and never on the maps the wire carries; staging removes it before
// anything is recorded.
const ImageCloneSourceField = "_workassCloneSource"

const maxImageCloneSourceHints = 8

// withToolImageCloneSources returns images with the absolute paths named in a
// tool's input attached as clone-source hints. The input maps are not changed.
func withToolImageCloneSources(images []any, input string) []any {
	if len(images) == 0 {
		return images
	}
	paths := ToolInputImagePaths(input)
	if len(paths) == 0 {
		return images
	}
	hints := make([]any, len(paths))
	for index, path := range paths {
		hints[index] = path
	}
	out := make([]any, len(images))
	for index, raw := range images {
		image, ok := raw.(map[string]any)
		if !ok {
			out[index] = raw
			continue
		}
		copied := make(map[string]any, len(image)+1)
		for key, value := range image {
			copied[key] = value
		}
		copied[ImageCloneSourceField] = hints
		out[index] = copied
	}
	return out
}

// toolInputPathField finds path-like fields in tool input JSON that the
// compact form truncated beyond parsing.
var toolInputPathField = regexp.MustCompile(`"(?:path|file_path|filePath|image_path|imagePath)"\s*:\s*"((?:[^"\\]|\\.)*)"`)

// ToolInputImagePaths returns the absolute paths a tool's input names: the
// "path" of Codex's view_image, the "file_path" of Claude's Read, and any
// other absolute string in the arguments.
func ToolInputImagePaths(input string) []string {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil
	}
	var paths []string
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 4096 || !filepath.IsAbs(value) || len(paths) >= maxImageCloneSourceHints {
			return
		}
		for _, existing := range paths {
			if existing == value {
				return
			}
		}
		paths = append(paths, value)
	}
	var decoded any
	if err := json.Unmarshal([]byte(input), &decoded); err == nil {
		var visit func(any, int)
		visit = func(value any, depth int) {
			if depth > 6 {
				return
			}
			switch item := value.(type) {
			case string:
				add(item)
			case map[string]any:
				keys := make([]string, 0, len(item))
				for key := range item {
					keys = append(keys, key)
				}
				sort.Strings(keys)
				for _, key := range keys {
					visit(item[key], depth+1)
				}
			case []any:
				for _, child := range item {
					visit(child, depth+1)
				}
			}
		}
		visit(decoded, 0)
		return paths
	}
	for _, match := range toolInputPathField.FindAllStringSubmatch(input, maxImageCloneSourceHints) {
		if value, err := strconv.Unquote(`"` + match[1] + `"`); err == nil {
			add(value)
		}
	}
	return paths
}
