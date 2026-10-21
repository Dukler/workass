package main

import "encoding/json"

// A message count alone does not bound an image-rich history response. Count
// each image occurrence at its expanded size before reading any sidecar bytes.
// Pages remain contiguous suffixes, so the existing stable-message pagination
// can retrieve every omitted row. One indivisible row may exceed this target.
const actorHistoryPageBytes = 8 << 20

// boundedActorHistory bounds a reply that carries every image inline.
func boundedActorHistory(messages []any, stateDir string) ([]any, error) {
	return actorHistorySuffixWithinBytes(messages, stateDir, actorHistoryPageBytes)
}

// boundedArchiveHistory bounds a projected history reply by what it carries.
// Every archive and session projection sends tool media by reference
// (deferArchiveToolImages), and a message's own images too when the reader
// asked for deferImages; those bytes are read one image at a time later.
// Counting them anyway cut each page of a screenshot-heavy chat to a single
// row: a 225 MB chat with 29,000 tool screenshots (17 GB counted, almost none
// of it sent) took a round trip per message to reach its first one.
func boundedArchiveHistory(messages []any, stateDir string, deferMessageImages bool) ([]any, error) {
	return actorHistorySuffixWithin(messages, actorHistoryPageBytes, func(message any, imageSizes map[string]int64) (int64, error) {
		return archiveReplyBytes(message, stateDir, imageSizes, deferMessageImages)
	})
}

func actorHistorySuffixWithinBytes(messages []any, stateDir string, budget int64) ([]any, error) {
	return actorHistorySuffixWithin(messages, budget, func(message any, imageSizes map[string]int64) (int64, error) {
		return actorHistoryExpandedBytes(message, stateDir, imageSizes)
	})
}

func actorHistorySuffixWithin(messages []any, budget int64, sizeOf func(any, map[string]int64) (int64, error)) ([]any, error) {
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
		size, err := sizeOf(messages[index], imageSizes)
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
	inlined, err := inlinedImageBytes(value, stateDir, imageSizes)
	if err != nil {
		return 0, err
	}
	return int64(len(encoded)) + inlined, nil
}

// archiveReplyBytes is one projected message's size in a reply that defers
// tool media, and its own images when deferMessageImages: a deferred image
// travels as its reference, which the encoded message already counts. The
// skipped paths are exactly the ones deferArchiveToolImages and
// deferArchiveMessageImages rewrite; every other reference is inlined by
// rehydrateExternalSessionImages and counted at its sidecar size.
func archiveReplyBytes(value any, stateDir string, imageSizes map[string]int64, deferMessageImages bool) (int64, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	size := int64(len(encoded))
	message, isMessage := value.(map[string]any)
	if !isMessage {
		inlined, err := inlinedImageBytes(value, stateDir, imageSizes)
		return size + inlined, err
	}
	count := func(child any) error {
		inlined, err := inlinedImageBytes(child, stateDir, imageSizes)
		size += inlined
		return err
	}
	for key, child := range message {
		switch key {
		case "images":
			if deferMessageImages {
				continue
			}
		case "events":
			for _, rawEvent := range anySlice(child) {
				event, isEvent := rawEvent.(map[string]any)
				if !isEvent {
					if err := count(rawEvent); err != nil {
						return 0, err
					}
					continue
				}
				tool := fieldString(event, "kind") == "tool"
				for eventKey, eventChild := range event {
					if tool && eventKey == "images" {
						continue
					}
					if err := count(eventChild); err != nil {
						return 0, err
					}
				}
			}
			continue
		}
		if err := count(child); err != nil {
			return 0, err
		}
	}
	return size, nil
}

// inlinedImageBytes adds, for every image reference under value, the bytes
// rehydration will put in its place.
func inlinedImageBytes(value any, stateDir string, imageSizes map[string]int64) (int64, error) {
	var size int64
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
