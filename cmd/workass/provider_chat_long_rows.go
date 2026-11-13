package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"workass/internal/wire"
)

// Long rows: one reply of hundreds of kilobytes, sent in parts.
//
// The oldest page of the c94k chat is one settled reply: 868 KB of text
// (2,404 inline images by path) and 1,649 image stubs, 1.7 MB for one row
// that a reader arriving from below sees the end of. A history read with
// {workGroups, longRows} sends such a row (settled, its text over
// longRowMinBytes) as its last part only:
//
//	content: the text from contentFrom on (a paragraph boundary outside any
//	code fence), events: its events from there on, contentFrom: where that
//	is (UTF-16, like every event's at), contentLength: the whole text's.
//
// The renderer reads the rest, a part at a time, as the reader scrolls up to
// it: archive-load(tab, {tail: 1, contentHead: {messageId, before, all?},
// ...the read's options}) answers {messageId, from, to, content, events}:
// the text [from, to) with its events, to == before, one part (all: every
// part before it). Parts are cut once, when the row is read, and kept with
// the chat's packs per chat revision (provider_chat_reply_memo.go).

const (
	longRowMinBytes  = 192 << 10
	longRowTailBytes = 24 << 10
	longRowPartBytes = 256 << 10
)

type longRowPart struct {
	From    int             `json:"from"`
	To      int             `json:"to"`
	Content string          `json:"content"`
	Events  json.RawMessage `json:"events"`
}

func longRowsRead(opts []archiveReadOptions) bool {
	long, groups := false, false
	for _, opt := range opts {
		long = long || opt.longRows
		groups = groups || opt.workGroups
	}
	return long && groups
}

// longRowCuts are byte offsets where the text may be cut: after a blank line,
// outside a fenced code block.
func longRowCuts(text string) []int {
	var cuts []int
	fenced := false
	lineStart := 0
	for lineStart < len(text) {
		end := strings.IndexByte(text[lineStart:], '\n')
		if end < 0 {
			break
		}
		line := text[lineStart : lineStart+end]
		trimmed := strings.TrimLeft(line, " ")
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
		}
		next := lineStart + end + 1
		if !fenced && strings.TrimSpace(line) == "" && next < len(text) && next > 0 {
			cuts = append(cuts, next)
		}
		lineStart = next
	}
	return cuts
}

func utf16Len(text string) int {
	n := 0
	for _, r := range text {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// longRowBounds are the parts' byte bounds: [0, b1), [b1, b2), ... the last
// starting at about longRowTailBytes from the end.
func longRowBounds(text string) []int {
	cuts := longRowCuts(text)
	tail := -1
	for index := len(cuts) - 1; index >= 0; index-- {
		if cuts[index] <= len(text)-longRowTailBytes {
			tail = cuts[index]
			break
		}
	}
	if tail <= 0 {
		return nil
	}
	bounds := []int{0}
	for _, cut := range cuts {
		if cut >= tail {
			break
		}
		if cut-bounds[len(bounds)-1] >= longRowPartBytes && tail-cut >= longRowPartBytes/4 {
			bounds = append(bounds, cut)
		}
	}
	return append(bounds, tail, len(text))
}

// splitLongRow is a finished row as a long-rows read sends it, and its
// parts before the last; ok is false when it is not a long row.
func splitLongRow(message map[string]any) (map[string]any, []longRowPart, bool, error) {
	if fieldString(message, "role") != "assistant" {
		return nil, nil, false, nil
	}
	switch fieldString(message, "status") {
	case "running", "pending", "":
		return nil, nil, false, nil
	}
	text, ok := message["content"].(string)
	if !ok || len(text) < longRowMinBytes || !utf8.ValidString(text) {
		return nil, nil, false, nil
	}
	bounds := longRowBounds(text)
	if len(bounds) < 3 {
		return nil, nil, false, nil
	}
	at16 := make([]int, len(bounds))
	for index := range bounds {
		if index == 0 {
			continue
		}
		at16[index] = at16[index-1] + utf16Len(text[bounds[index-1]:bounds[index]])
	}
	events := anySlice(message["events"])
	compactLongRowImages(events)
	parts := make([][]any, len(bounds)-1)
	for _, raw := range events {
		at := intValue(mapFromAnyMain(raw)["at"])
		part := len(parts) - 1
		for part > 0 && at < at16[part] {
			part--
		}
		parts[part] = append(parts[part], raw)
	}
	out := make([]longRowPart, 0, len(parts)-1)
	for index := 0; index < len(parts)-1; index++ {
		eventsJSON, err := json.Marshal(nonNilSlice(parts[index]))
		if err != nil {
			return nil, nil, false, err
		}
		out = append(out, longRowPart{From: at16[index], To: at16[index+1], Content: text[bounds[index]:bounds[index+1]], Events: eventsJSON})
	}
	last := len(parts) - 1
	tail := make(map[string]any, len(message)+2)
	for key, value := range message {
		tail[key] = value
	}
	tail["content"] = text[bounds[last]:]
	tail["events"] = nonNilSlice(parts[last])
	tail["contentFrom"] = at16[last]
	tail["contentLength"] = at16[len(at16)-1]
	return tail, out, true, nil
}

// compactLongRowImages leaves out, of each image a long row's events carry by
// reference, what every such image has: an empty "data" and the PNG type
// (the renderer restores both: chat/long-row.ts expandLongRowImages). The
// row's 2,356 images were 75 KB of them.
func compactLongRowImages(events []any) {
	for _, raw := range events {
		for _, rawImage := range anySlice(mapFromAnyMain(raw)["images"]) {
			image := mapFromAnyMain(rawImage)
			if fieldString(image, "deferredImageRef") == "" {
				continue
			}
			if data, ok := image["data"].(string); ok && data == "" {
				delete(image, "data")
			}
			if image["mimeType"] == "image/png" {
				delete(image, "mimeType")
			}
		}
	}
}

func nonNilSlice(values []any) []any {
	if values == nil {
		return []any{}
	}
	return values
}

// splitLongRows replaces, in a finished page, each long row by its last part
// and keeps its other parts for {contentHead} reads.
func (r *providerChatRuntime) splitLongRows(chatID string, revision uint64, known bool, page archivePage, opts []archiveReadOptions) archivePage {
	if !known || !longRowsRead(opts) || len(page.encoded) != len(page.messages) {
		return page
	}
	var messages []any
	var encoded [][]byte
	for index, raw := range page.messages {
		if len(page.encoded[index]) < longRowMinBytes {
			continue
		}
		tail, parts, ok, err := splitLongRow(mapFromAnyMain(raw))
		if err != nil || !ok {
			continue
		}
		data, err := json.Marshal(tail)
		if err != nil {
			continue
		}
		if messages == nil {
			messages = append([]any(nil), page.messages...)
			encoded = append([][]byte(nil), page.encoded...)
		}
		messages[index], encoded[index] = tail, data
		r.keepLongRowParts(chatID, fieldString(tail, "id"), revision, opts, parts)
	}
	if messages == nil {
		return page
	}
	return archivePage{messages: messages, encoded: encoded}
}

// A long row's parts are kept as their {contentHead} replies, each
// compressed once at the best level (a part is read many times and is kept
// before any reader asks), in memory and with the chat's packs on disk, with
// an index of where each part starts and ends. A part read is then the kept
// segment framed as it is, in this process or a later one.

type longRowIndex struct {
	revision uint64
	bounds   [][2]int // [from, to] of each part, in order
}

var longRowIndexes = struct {
	sync.Mutex
	rows map[pageMemoKey]longRowIndex
}{rows: map[pageMemoKey]longRowIndex{}}

func longRowKey(chatID, messageID string, opts []archiveReadOptions) pageMemoKey {
	return pageMemoKey{chatID: chatID, before: "long:" + messageID, opts: singleReadOption(opts)}
}

func longRowPartKey(row pageMemoKey, index int) pageMemoKey {
	row.before += "#" + strconv.Itoa(index)
	return row
}

func (r *providerChatRuntime) keepLongRowParts(chatID, messageID string, revision uint64, opts []archiveReadOptions, parts []longRowPart) {
	key := longRowKey(chatID, messageID, opts)
	index := longRowIndex{revision: revision}
	for _, part := range parts {
		index.bounds = append(index.bounds, [2]int{part.From, part.To})
	}
	longRowIndexes.Lock()
	if len(longRowIndexes.rows) >= 16 {
		for other := range longRowIndexes.rows {
			delete(longRowIndexes.rows, other)
			break
		}
	}
	longRowIndexes.rows[key] = index
	longRowIndexes.Unlock()
	store := r.workGroupPacks()
	// Newest first: the part a reader scrolls to first is ready first. Off
	// the reply that read the row.
	go func() {
		// Every part at once, each on its own core: a reader may ask for
		// them all as soon as the last part lands.
		var wg sync.WaitGroup
		for at := range parts {
			wg.Add(1)
			go func(at int) {
				defer wg.Done()
				encoded, err := encodeLongRowPart(messageID, parts[at])
				if err != nil {
					return
				}
				if deflated := wire.DeflateSegment(encoded); deflated != nil {
					store.keepPageKept(longRowPartKey(key, at), revision, keptReply{deflated: deflated, size: len(encoded)})
				}
			}(at)
		}
		wg.Wait()
		bounds, err := json.Marshal(index.bounds)
		if err == nil {
			store.keepPage(key, revision, bounds)
		}
	}()
}

func (r *providerChatRuntime) longRowIndex(chatID, messageID string, revision uint64, opts []archiveReadOptions) (longRowIndex, bool) {
	key := longRowKey(chatID, messageID, opts)
	longRowIndexes.Lock()
	kept, ok := longRowIndexes.rows[key]
	longRowIndexes.Unlock()
	if ok && kept.revision == revision {
		return kept, true
	}
	stored, ok := r.workGroupPacks().keptPage(key, revision)
	if !ok {
		return longRowIndex{}, false
	}
	raw, err := stored.plain()
	if err != nil {
		return longRowIndex{}, false
	}
	index := longRowIndex{revision: revision}
	if json.Unmarshal(raw, &index.bounds) != nil || len(index.bounds) == 0 {
		return longRowIndex{}, false
	}
	longRowIndexes.Lock()
	longRowIndexes.rows[key] = index
	longRowIndexes.Unlock()
	return index, true
}

// longRowPartReply is a part's kept reply; it waits briefly for one being
// kept right now.
func (r *providerChatRuntime) longRowPartReply(chatID, messageID string, revision uint64, at int, opts []archiveReadOptions) (keptReply, bool) {
	key := longRowPartKey(longRowKey(chatID, messageID, opts), at)
	for wait := 0; wait < 100; wait++ {
		if kept, ok := r.workGroupPacks().keptPage(key, revision); ok {
			return kept, true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return keptReply{}, false
}

func encodeLongRowPart(messageID string, part longRowPart) ([]byte, error) {
	return json.Marshal(map[string]any{
		"messageId": messageID, "from": part.From, "to": part.To, "content": part.Content, "events": part.Events,
	})
}

type contentHeadRequest struct {
	messageID string
	before    int
	all       bool
	// parts (additive): every part up to before, each as its own reply,
	// in one reply {messageId, parts: [...]}, framed from the kept parts.
	parts bool
}

func parseContentHeadRequest(raw any) (contentHeadRequest, error) {
	options := mapFromAnyMain(raw)
	request := contentHeadRequest{messageID: strings.TrimSpace(fieldString(options, "messageId")), before: intValue(options["before"]), all: options["all"] == true, parts: options["parts"] == true}
	if request.messageID == "" || request.before <= 0 {
		return contentHeadRequest{}, errors.New("content head requires a message id and where it ends")
	}
	return request, nil
}

// ContentHeadByTab is a {contentHead} read: the part of a long row ending at
// request.before (every part before it with all), from the parts kept when the
// row was read, or, when they are gone, from the row read again.
func (r *providerChatRuntime) ContentHeadByTab(tabID string, request contentHeadRequest, opts []archiveReadOptions) (any, bool, error) {
	chatID, found, err := r.chatByTab(tabID)
	if err != nil || !found {
		return nil, found, err
	}
	revision, known := r.chatRevision(chatID)
	if !known {
		return nil, false, nil
	}
	index, ok := r.longRowIndex(chatID, request.messageID, revision, opts)
	if !ok {
		// Read the row again: its page splits it and keeps its parts.
		next, found, err := r.nextMessageID(chatID, request.messageID)
		if err != nil || !found {
			return nil, found, err
		}
		if next != "" {
			_, found, err = r.archivePageBeforeByChat(chatID, next, 1, opts...)
		} else {
			_, found, err = r.archivePageByChat(chatID, actorHistoryTail, 1, false, opts...)
		}
		if err != nil || !found {
			return nil, found, err
		}
		if index, ok = r.longRowIndex(chatID, request.messageID, revision, opts); !ok {
			return nil, false, fmt.Errorf("message %s is not a long row at this revision", request.messageID)
		}
	}
	end := -1
	for at, bound := range index.bounds {
		if bound[1] == request.before {
			end = at
		}
	}
	if end < 0 {
		return nil, false, fmt.Errorf("no part of %s ends at %d", request.messageID, request.before)
	}
	if request.parts {
		// Every part up to it, as kept: no part is decoded or encoded again.
		id, _ := json.Marshal(request.messageID)
		segments := []wire.SegmentedPart{{Literal: append(append([]byte(`{"messageId":`), id...), `,"parts":[`...)}}
		for at := 0; at <= end; at++ {
			kept, ok := r.longRowPartReply(chatID, request.messageID, revision, at, opts)
			if !ok {
				return nil, false, fmt.Errorf("part %d of %s is not kept", at, request.messageID)
			}
			if at > 0 {
				segments = append(segments, wire.SegmentedPart{Literal: []byte{','}})
			}
			segments = append(segments, kept.result().Parts...)
		}
		segments = append(segments, wire.SegmentedPart{Literal: []byte(`]}`)})
		return wire.SegmentedResult{Parts: segments}, true, nil
	}
	if !request.all {
		kept, ok := r.longRowPartReply(chatID, request.messageID, revision, end, opts)
		if !ok {
			return nil, false, fmt.Errorf("part %d of %s is not kept", end, request.messageID)
		}
		return kept.result(), true, nil
	}
	// Every part up to it, as one reply.
	var content strings.Builder
	var events bytes.Buffer
	events.WriteByte('[')
	for at := 0; at <= end; at++ {
		kept, ok := r.longRowPartReply(chatID, request.messageID, revision, at, opts)
		if !ok {
			return nil, false, fmt.Errorf("part %d of %s is not kept", at, request.messageID)
		}
		raw, err := kept.plain()
		if err != nil {
			return nil, false, err
		}
		var part longRowPart
		if err := json.Unmarshal(raw, &part); err != nil {
			return nil, false, err
		}
		content.WriteString(part.Content)
		inner := bytes.TrimSpace(part.Events)
		if len(inner) >= 2 {
			inner = inner[1 : len(inner)-1]
		}
		if len(inner) > 0 {
			if events.Len() > 1 {
				events.WriteByte(',')
			}
			events.Write(inner)
		}
	}
	events.WriteByte(']')
	reply := map[string]any{
		"messageId": request.messageID, "from": index.bounds[0][0], "to": index.bounds[end][1],
		"content": content.String(), "events": json.RawMessage(events.Bytes()),
	}
	encoded, err := json.Marshal(reply)
	if err != nil {
		return nil, false, err
	}
	return wire.EncodedResult(encoded), true, nil
}

func (r *providerChatRuntime) nextMessageID(chatID, messageID string) (string, bool, error) {
	if _, closed := r.closedSummary(chatID); closed && r.ledgers != nil {
		return r.ledgers.NextMessageID(chatID, messageID)
	}
	actor, err := r.actor(chatID)
	if err != nil {
		return "", false, err
	}
	next, found := actor.engine.NextMessageID(messageID)
	return next, found, nil
}
