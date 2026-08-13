package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"workass/internal/acp"
)

func requireOnlyKeys(value map[string]any, allowed map[string]struct{}, label string) error {
	unknown := make([]string, 0)
	for key := range value {
		if _, ok := allowed[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("%s contains unsupported fields %q", label, unknown)
}

func optionalStringPointer(source map[string]any, key string) *string {
	value, exists := source[key]
	if !exists || value == nil {
		return nil
	}
	text := stringValue(value)
	return &text
}

func sameFilesystemPath(left, right string) bool {
	canonical := func(value string) string {
		value = filepath.Clean(strings.TrimSpace(value))
		if absolute, err := filepath.Abs(value); err == nil {
			value = absolute
		}
		if resolved, err := filepath.EvalSymlinks(value); err == nil {
			value = resolved
		}
		return value
	}
	return canonical(left) == canonical(right)
}

func canonicalModelControlKey(modelID string) string {
	baseModelID, _, split := acp.SplitCanonicalEffortSuffix(modelID)
	if split && baseModelID != "" {
		return baseModelID
	}
	return strings.TrimSpace(modelID)
}
