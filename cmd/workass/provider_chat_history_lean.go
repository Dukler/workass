package main

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Tool payloads dominate transcript bytes: in a 3,000-message chat they were
// 95% of every history page (a ten-row read was 330 KB, the whole history
// 148 MiB), while the renderer shows a tool's output only after its row is
// opened. A renderer that asks for leanTools receives each large payload as a
// bounded head plus its full size, and reads the rest through the same
// authenticated archive channel when a row opens (after t3code's wire
// projection, which omits command output and serves detail on demand). A
// renderer that does not ask keeps the byte-identical full rows.
const (
	leanToolPayloadThreshold = 4 << 10
	leanToolPayloadHead      = 1 << 10
	toolDetailChunkBytes     = 1 << 20
	// The tool whose call the renderer finds in a title to show the page it
	// published (desktop/renderer2/src/markdown/html-render.ts).
	htmlRenderToolName = "workass_render_html"
)

// leanArchiveToolPayloads trims committed rows only. A running or pending row
// is the live foreground owner: its tools are still changing on the event
// stream and are never addressable by a committed message id. A subagent
// header keeps its summary, which the rail prints without an open gesture.
func leanArchiveToolPayloads(messages []any, payloads, titles bool) {
	fields := make([]string, 0, 3)
	if payloads {
		fields = append(fields, "output", "input")
	}
	if titles {
		fields = append(fields, "title")
	}
	for _, raw := range messages {
		message := mapFromAnyMain(raw)
		switch fieldString(message, "status") {
		case "running", "pending":
			continue
		}
		for _, rawEvent := range anySlice(message["events"]) {
			event := mapFromAnyMain(rawEvent)
			if fieldString(event, "kind") != "tool" || event["subagentHeader"] == true {
				continue
			}
			for _, field := range fields {
				value, ok := event[field].(string)
				if !ok || len(value) <= leanToolPayloadThreshold {
					continue
				}
				// The renderer finds a page the agent published by the call in
				// the command or title, wherever it sits in a long one.
				if field == "title" && strings.Contains(value, htmlRenderToolName) {
					continue
				}
				event[field] = leanToolHead(value)
				event[field+"Deferred"] = true
				event[field+"Bytes"] = len(value)
			}
		}
	}
}

// toolSummaryOutputHead is how much of a settled call's output a summary
// keeps: the row is clickable while it has any, and opening it reads the rest.
const toolSummaryOutputHead = 160

// leanArchiveToolSummaries is the {toolSummaries} read (a chat's first
// screen). A settled turn's calls are folded into its Worked row, and a call
// row, once that is open, draws its action, its command or path and, when
// opened itself, its output: never its input. So a summary drops every input
// and keeps only the head of each output; both stay readable through the
// toolDetail read, which a row already uses when it opens (2026-10-25: tool
// payloads were 40 of the 56 KB of a big chat's first screen). The calls that
// show something outside the fold keep everything: a page the agent published
// is found in its output, and a subagent header prints its summary.
func leanArchiveToolSummaries(messages []any) {
	for _, raw := range messages {
		message := mapFromAnyMain(raw)
		switch fieldString(message, "status") {
		case "running", "pending":
			continue
		}
		for _, rawEvent := range anySlice(message["events"]) {
			event := mapFromAnyMain(rawEvent)
			if fieldString(event, "kind") != "tool" || event["subagentHeader"] == true {
				continue
			}
			if strings.Contains(fieldString(event, "title"), htmlRenderToolName) || strings.Contains(fieldString(event, "command"), htmlRenderToolName) ||
				strings.Contains(fieldString(event, "input"), htmlRenderToolName) {
				continue
			}
			if value, ok := event["input"].(string); ok && value != "" {
				deferToolSummaryField(event, "input", value, 0)
			}
			if value, ok := event["output"].(string); ok && len(value) > toolSummaryOutputHead {
				deferToolSummaryField(event, "output", value, toolSummaryOutputHead)
			}
		}
	}
}

func deferToolSummaryField(event map[string]any, field, value string, head int) {
	if event[field+"Deferred"] != true {
		event[field+"Bytes"] = len(value)
	}
	event[field] = toolPayloadHead(value, head)
	event[field+"Deferred"] = true
}

// toolPayloadHead is value's first limit bytes, never splitting a rune.
func toolPayloadHead(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	head := value[:limit]
	for !utf8.ValidString(head) && len(head) > 0 {
		head = head[:len(head)-1]
	}
	return head
}

// leanToolHead keeps whole lines when it can, and never splits a rune.
func leanToolHead(value string) string {
	head := value[:leanToolPayloadHead]
	for !utf8.ValidString(head) && len(head) > 0 {
		head = head[:len(head)-1]
	}
	if cut := strings.LastIndexByte(head, '\n'); cut > leanToolPayloadHead/2 {
		head = head[:cut+1]
	}
	return head
}

type toolDetailRequest struct {
	messageID string
	key       string
	field     string
	offset    int
}

func parseToolDetailRequest(raw any) (toolDetailRequest, error) {
	options := mapFromAnyMain(raw)
	request := toolDetailRequest{
		messageID: strings.TrimSpace(fieldString(options, "messageId")),
		key:       strings.TrimSpace(fieldString(options, "key")),
		field:     strings.TrimSpace(fieldString(options, "field")),
		offset:    intValue(options["offset"]),
	}
	if request.field == "" {
		request.field = "output"
	}
	if request.messageID == "" || request.key == "" {
		return toolDetailRequest{}, errors.New("tool detail requires a message id and tool key")
	}
	if request.field != "output" && request.field != "input" && request.field != "title" {
		return toolDetailRequest{}, fmt.Errorf("unknown tool detail field %q", request.field)
	}
	if request.offset < 0 {
		return toolDetailRequest{}, errors.New("tool detail offset must not be negative")
	}
	return request, nil
}

// ProjectToolDetailByTab returns one bounded chunk of a committed tool
// payload: {text, offset, next, total}. Chunks end on rune boundaries, so a
// reader that concatenates them from offset 0 until next == total holds the
// exact stored payload. No reply approaches the frame limit, whatever the
// payload's size.
func (r *providerChatRuntime) ProjectToolDetailByTab(tabID string, request toolDetailRequest) (map[string]any, bool, error) {
	chatID, found, err := r.chatByTab(tabID)
	if err != nil || !found {
		return nil, found, err
	}
	event, found, err := r.ledgerMessage(chatID, request.messageID)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	for _, entry := range event.Timeline {
		if entry.Tool == nil || entry.Key != request.key {
			continue
		}
		value := entry.Tool.Output
		switch request.field {
		case "input":
			value = entry.Tool.Input
		case "title":
			value = entry.Tool.Title
		}
		if request.offset > len(value) {
			return nil, false, errors.New("tool detail offset is past the payload")
		}
		end := min(len(value), request.offset+toolDetailChunkBytes)
		for end < len(value) && end > request.offset && !utf8.RuneStart(value[end]) {
			end--
		}
		return map[string]any{
			"text": value[request.offset:end], "offset": request.offset, "next": end, "total": len(value),
		}, true, nil
	}
	return nil, false, nil
}
