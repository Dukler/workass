package main

import (
	"encoding/json"

	"workass/internal/wire"
)

// A message count alone does not bound an image-rich history response. Count
// each image occurrence at its expanded size before reading any sidecar bytes.
// Pages remain contiguous suffixes, so the existing stable-message pagination
// can retrieve every omitted row. One indivisible row may exceed this target.
const actorHistoryPageBytes = 8 << 20

// boundedActorHistory bounds a reply that carries every image inline.
func boundedActorHistory(messages []any, stateDir string) ([]any, error) {
	return actorHistorySuffixWithinBytes(messages, stateDir, actorHistoryPageBytes)
}

// archivePage is a history reply finished for the wire: the rows exactly as
// they are sent, and each row's encoding.
type archivePage struct {
	messages []any
	encoded  [][]byte
}

// rawReplySpare is room after a page for the reply envelope and frame
// header, so the reply frame is written into the page's own buffer.
const rawReplySpare = 128

// raw is the page's reply: the row encodings joined into the array
// json.Marshal writes for the same rows, byte for byte.
func (p archivePage) raw() wire.EncodedResult {
	size := 2 + max(0, len(p.encoded)-1)
	for _, row := range p.encoded {
		size += len(row)
	}
	out := make([]byte, 0, size+rawReplySpare)
	out = append(out, '[')
	for index, row := range p.encoded {
		if index > 0 {
			out = append(out, ',')
		}
		out = append(out, row...)
	}
	return append(out, ']')
}

// rawMessages are the row encodings for a larger reply that embeds this page.
func (p archivePage) rawMessages() []any {
	rows := make([]any, len(p.encoded))
	for index, row := range p.encoded {
		rows[index] = json.RawMessage(row)
	}
	return rows
}

// finishArchivePage turns projected rows into the rows a reply sends (image
// sizes, deferred media, the reader's options, rehydrated sidecars) and
// encodes each row once. With a budget it works back from the newest row and
// keeps the contiguous suffix whose encodings fit, so the bytes that bound a
// page are the bytes it sends, and that encoding is the reply instead of a
// second marshal of every row. One indivisible row may exceed the budget, and
// uncommitted foreground rows stay together above it. budget <= 0 keeps every
// row.
func finishArchivePage(messages []any, stateDir string, opts []archiveReadOptions, budget int64) (archivePage, error) {
	requiredFirst := len(messages) - 1
	for index, raw := range messages {
		status := fieldString(mapFromAnyMain(raw), "status")
		if status == "pending" || status == "running" {
			// Uncommitted foreground/steering owners cannot be retrieved by a
			// ledger cursor yet. Keep them together even above the page target.
			requiredFirst = min(requiredFirst, index)
		}
	}
	resolved := make(map[string]string)
	encoded := make([][]byte, len(messages))
	first := len(messages)
	used := int64(2) // JSON array brackets.
	for index := len(messages) - 1; index >= 0; index-- {
		row := messages[index : index+1]
		annotateSessionImageSizes(row, stateDir)
		deferArchiveToolImages(row)
		for _, opt := range opts {
			opt.apply(row)
		}
		if err := rehydrateExternalSessionImagesShared(row, stateDir, resolved); err != nil {
			return archivePage{}, err
		}
		data, err := json.Marshal(messages[index])
		if err != nil {
			return archivePage{}, err
		}
		size := int64(len(data))
		if budget > 0 && index < requiredFirst && used+size+1 > budget {
			break
		}
		encoded[index] = data
		first = index
		used += size + 1
	}
	return archivePage{messages: messages[first:], encoded: encoded[first:]}, nil
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
