package main

import "encoding/json"

// A message count alone does not bound an image-rich history response. Count
// each image occurrence at its expanded size before reading any sidecar bytes.
// Pages remain contiguous suffixes, so the existing stable-message pagination
// can retrieve every omitted row. One indivisible row may exceed this target.
const actorHistoryPageBytes = 8 << 20

func boundedActorHistory(messages []any, stateDir string) ([]any, error) {
	return actorHistorySuffixWithinBytes(messages, stateDir, actorHistoryPageBytes)
}

func actorHistorySuffixWithinBytes(messages []any, stateDir string, budget int64) ([]any, error) {
	first := len(messages)
	used := int64(2) // JSON array brackets.
	imageSizes := make(map[string]int64)
	requiredFirst := len(messages) - 1
	for index, raw := range messages {
		status := fieldString(mapFromAnyMain(raw), "status")
		if status == "pending" || status == "running" {
			// Uncommitted foreground/steering owners cannot be retrieved by a
			// ledger cursor yet. Keep them together even above the page target.
			requiredFirst = min(requiredFirst, index)
		}
	}
	for index := len(messages) - 1; index >= 0; index-- {
		size, err := actorHistoryExpandedBytes(messages[index], stateDir, imageSizes)
		if err != nil {
			return nil, err
		}
		if index < requiredFirst && used+size+1 > budget {
			break
		}
		first = index
		used += size + 1
	}
	return messages[first:], nil
}

func actorHistoryExpandedBytes(value any, stateDir string, imageSizes map[string]int64) (int64, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	size := int64(len(encoded))
	var visit func(any) error
	visit = func(value any) error {
		switch item := value.(type) {
		case map[string]any:
			if ref := fieldString(item, sessionImageDataRefField); ref != "" {
				bytes, found := imageSizes[ref]
				if !found {
					_, _, info, err := externalSessionImageInfo(ref, stateDir)
					if err != nil {
						return err
					}
					bytes = info.Size()
					imageSizes[ref] = bytes
				}
				// Persisted image data is base64, which needs no JSON escaping.
				// Keeping the reference's existing size makes this conservative.
				size += bytes + int64(len(`,"data":""`))
			}
			for _, child := range item {
				if err := visit(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range item {
				if err := visit(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(value); err != nil {
		return 0, err
	}
	return size, nil
}
