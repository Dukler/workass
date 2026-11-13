package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"runtime"
	"sync"

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

// size is the length of the page's reply, raw() without building it.
func (p archivePage) size() int {
	size := 2 + max(0, len(p.encoded)-1)
	for _, row := range p.encoded {
		size += len(row)
	}
	return size
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
		data, err := finishArchiveRow(messages[index:index+1], stateDir, opts, resolved)
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

// finishArchiveRow finishes one projected row for the wire, in place (image
// sizes, deferred media, the reader's options, rehydrated sidecars), and
// returns its encoding. finishArchivePage bounds pages by these bytes, and a
// closed actor's row selection (closedHistoryRows) sizes rows with it too, so
// both paths keep exactly the same rows.
func finishArchiveRow(row []any, stateDir string, opts []archiveReadOptions, resolved map[string]string) ([]byte, error) {
	if err := finishArchiveRowValue(row, stateDir, opts, resolved); err != nil {
		return nil, err
	}
	return marshalArchiveRow(row[0])
}

// marshalParallelEvents: a row with more events than this has them encoded
// in parallel.
const marshalParallelEvents = 2048

// marshalArchiveRow is json.Marshal(row), byte for byte, with a large row's
// events encoded on every core: encoding the 13.9 MB row of an older
// renderer's page took 40 ms on one.
func marshalArchiveRow(row any) ([]byte, error) {
	message, ok := row.(map[string]any)
	events, isSlice := message["events"].([]any)
	if !ok || !isSlice || len(events) < marshalParallelEvents {
		return json.Marshal(row)
	}
	workers := max(2, min(8, runtime.GOMAXPROCS(0)))
	chunks := make([][]byte, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	per := (len(events) + workers - 1) / workers
	for worker := 0; worker < workers; worker++ {
		from, to := worker*per, min(len(events), (worker+1)*per)
		if from >= to {
			continue
		}
		wg.Add(1)
		go func(worker, from, to int) {
			defer wg.Done()
			chunks[worker], errs[worker] = json.Marshal(events[from:to])
		}(worker, from, to)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	// The row with a placeholder where its events go, then the events,
	// joined as one array encodes them.
	var sentinel [16]byte
	_, _ = rand.Read(sentinel[:])
	placeholder := "\x00workass-events-" + hex.EncodeToString(sentinel[:])
	shell := make(map[string]any, len(message))
	for key, value := range message {
		shell[key] = value
	}
	shell["events"] = placeholder
	head, err := json.Marshal(shell)
	if err != nil {
		return nil, err
	}
	quoted, err := json.Marshal(placeholder)
	if err != nil {
		return nil, err
	}
	at := bytes.Index(head, quoted)
	if at < 0 || bytes.Count(head, quoted) != 1 {
		return json.Marshal(row)
	}
	size := len(head) - len(quoted) + 2
	for _, chunk := range chunks {
		size += len(chunk)
	}
	out := make([]byte, 0, size+rawReplySpare)
	out = append(out, head[:at]...)
	out = append(out, '[')
	first := true
	for _, chunk := range chunks {
		if len(chunk) <= 2 {
			continue
		}
		if !first {
			out = append(out, ',')
		}
		out = append(out, chunk[1:len(chunk)-1]...)
		first = false
	}
	out = append(out, ']')
	return append(out, head[at+len(quoted):]...), nil
}

// finishArchiveRowValue is finishArchiveRow without the encoding.
func finishArchiveRowValue(row []any, stateDir string, opts []archiveReadOptions, resolved map[string]string) error {
	prefetchSessionImageSizes(row, stateDir)
	annotateSessionImageSizes(row, stateDir)
	sessionImageSizesOnDisk.flush(stateDir)
	deferArchiveToolImages(row)
	for _, opt := range opts {
		opt.apply(row)
	}
	return rehydrateExternalSessionImagesShared(row, stateDir, resolved)
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
					file, err := locateSessionImage(ref, stateDir)
					if err != nil {
						return err
					}
					bytes = file.logicalSize()
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
