package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

// workGroupTimeline is a settled turn the renderer draws with its calls
// folded: thoughts, main-thread calls at three text offsets (two with named
// images, one with an unnamed one), a subagent with its header and its own
// calls, a call that published a page, and the final answer.
func workGroupTimeline(t *testing.T, stateDir, label string, text string) []chat.TimelineEntry {
	t.Helper()
	tool := func(key string, at int, event providercontract.ToolEvent) chat.TimelineEntry {
		if event.ToolCallID == "" {
			event.ToolCallID = key
		}
		if event.Status == "" {
			event.Status = "completed"
		}
		return chat.TimelineEntry{Key: "timeline:tool:" + key, At: at, Kind: providercontract.EventToolUpdate, Tool: &event}
	}
	image := func(name, seed string) []providercontract.Attachment {
		return []providercontract.Attachment{{ID: "img-" + seed, Name: name, MIMEType: "image/png", Ref: providerSessionImageRefPrefix + lazyImageRef(t, stateDir, label+seed)}}
	}
	var out []chat.TimelineEntry
	out = append(out, chat.TimelineEntry{Key: "thinking-1", At: 0, Kind: providercontract.EventThinkingUpdate, Thinking: &providercontract.ThinkingEvent{Text: "**Plan** look first."}})
	for index := 0; index < 6; index++ {
		out = append(out, tool(fmt.Sprintf("%s-read-%d", label, index), 4, providercontract.ToolEvent{
			ToolKind: "read", Title: fmt.Sprintf("Read file %d", index), Input: strings.Repeat("i", 300), Output: strings.Repeat("o", 9000+index),
			StartedAtUnixMS: int64(1_000 + index), EndedAtUnixMS: int64(2_000 + index),
		}))
	}
	out = append(out, tool(label+"-shot-1", 4, providercontract.ToolEvent{ToolKind: "execute", Title: "Screenshot", Output: "ok", Attachments: image("shot-1.png", "a")}))
	out = append(out, tool(label+"-shot-2", 4, providercontract.ToolEvent{ToolKind: "execute", Title: "Screenshot", Output: "ok", Attachments: image("shot-2.png", "b")}))
	out = append(out, chat.TimelineEntry{Key: "thinking-2", At: 9, Kind: providercontract.EventThinkingUpdate, Thinking: &providercontract.ThinkingEvent{Text: "Then the page."}})
	out = append(out, tool(label+"-bare", 9, providercontract.ToolEvent{ToolKind: "execute", Title: "View image", Attachments: image("", "c"), Status: "failed"}))
	out = append(out, tool(label+"-page", 9, providercontract.ToolEvent{ToolKind: "other", Title: "mcp__workass__workass_render_html", Output: `{"htmlRender":{"id":"p1"}}`}))
	out = append(out, tool(label+"-agent", 9, providercontract.ToolEvent{ToolKind: "agent", Title: "Review lane", SubagentHeader: true, SubagentID: label + "-agent", Output: "done"}))
	for index := 0; index < 4; index++ {
		out = append(out, tool(fmt.Sprintf("%s-child-%d", label, index), 9, providercontract.ToolEvent{
			ToolKind: "execute", Title: "child step", SubagentID: label + "-agent", Output: strings.Repeat("c", 5000),
		}))
	}
	for index := 0; index < 3; index++ {
		out = append(out, tool(fmt.Sprintf("%s-edit-%d", label, index), len(text), providercontract.ToolEvent{ToolKind: "edit", Title: "Edit", Input: "x", Output: "y", StartedAtUnixMS: 5_000, EndedAtUnixMS: 9_000}))
	}
	return out
}

func workGroupTestChat(t *testing.T, stateDir string, backend chatActorBackend, chatID string) {
	lazyTestChat(t, stateDir, backend, chatID, 9, func(state *chat.State) {
		for index := range state.Ledger {
			row := &state.Ledger[index]
			if row.Role != "assistant" {
				continue
			}
			row.Text = "Looked. Then wrote the page.\n\nDone."
			row.Timeline = workGroupTimeline(t, stateDir, fmt.Sprintf("%s-%d", chatID, index), row.Text)
		}
	})
}

var workGroupReadOptions = archiveReadOptions{deferMessageImages: true, leanTools: true, leanTitles: true, workGroups: true}

func TestWorkGroupsFoldSettledCallsAndReadThemBack(t *testing.T) {
	stateDir := t.TempDir()
	backend, err := openChatActorBackend(stateDir, true, "test", t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.close()
	workGroupTestChat(t, stateDir, backend, "wg")
	firstScreen := firstScreenWarmSummaryOptions

	read := func(runtime *providerChatRuntime) map[string][]byte {
		out := map[string][]byte{}
		raw, found, err := runtime.ProjectRecentArchiveByTabRaw("tab-wg", 9, workGroupReadOptions)
		if err != nil || !found {
			t.Fatalf("tail: %v %v", found, err)
		}
		out["tail"] = append([]byte(nil), raw...)
		raw, found, err = runtime.ProjectLastExchangeByTabRaw("tab-wg", 9, firstScreen)
		if err != nil || !found {
			t.Fatalf("first screen: %v %v", found, err)
		}
		out["first"] = append([]byte(nil), raw...)
		raw, found, err = runtime.ProjectArchivePageBeforeByTabRaw("tab-wg", "wg-message-9", 8, workGroupReadOptions)
		if err != nil || !found {
			t.Fatalf("page: %v %v", found, err)
		}
		out["page"] = append([]byte(nil), raw...)
		raw, found, err = runtime.ProjectRecentArchiveByTabRaw("tab-wg", 9, archiveReadOptions{deferMessageImages: true, leanTools: true, leanTitles: true})
		if err != nil || !found {
			t.Fatalf("plain tail: %v %v", found, err)
		}
		out["plain"] = append([]byte(nil), raw...)
		return out
	}

	t.Setenv(eagerActorsEnv, "1")
	eager := startLazyTestRuntime(t, stateDir, backend, "tab-none")
	want := read(eager)
	// The rows a work group read leaves out, read back page by page, are
	// exactly the row's events as a read without work groups sends them.
	workGroupPageBytes, workGroupChunkBytes = 2<<10, 700
	defer func() { workGroupPageBytes, workGroupChunkBytes = 1<<20, 48<<10 }()
	wantGroup := readWorkGroup(t, eager, "wg-message-7")
	if err := eager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	t.Setenv(eagerActorsEnv, "")
	lazy := startLazyTestRuntime(t, stateDir, backend, "tab-none")
	defer lazy.Close(context.Background())
	got := read(lazy)
	for name := range want {
		if !bytes.Equal(got[name], want[name]) {
			t.Fatalf("%s: a closed chat's read differs from an open one's\nopen:   %s\nclosed: %s", name, want[name], got[name])
		}
	}
	if gotGroup := readWorkGroup(t, lazy, "wg-message-7"); !bytes.Equal(mustJSONBytes(t, gotGroup), mustJSONBytes(t, wantGroup)) {
		t.Fatal("a closed chat's work group differs from an open one's")
	}
	if open, _ := lazy.lazyActorCounts(); open != 0 {
		t.Fatalf("work group reads opened %d actors", open)
	}

	// Without the option nothing changes: no summary, every call whole.
	if bytes.Contains(got["plain"], []byte("workSummary")) || bytes.Contains(got["plain"], []byte("workStub")) {
		t.Fatal("a read without workGroups carries work groups")
	}
	var rows []map[string]any
	if err := json.Unmarshal(got["tail"], &rows); err != nil {
		t.Fatal(err)
	}
	var plain []map[string]any
	if err := json.Unmarshal(got["plain"], &plain); err != nil {
		t.Fatal(err)
	}
	message := rows[6]
	if fieldString(message, "id") != "wg-message-7" {
		t.Fatalf("row 7 is %q", fieldString(message, "id"))
	}
	summary := mapFromAnyMain(message["workSummary"])
	// 6 reads + 2 named shots + 1 unnamed + 3 edits; 4 subagent calls.
	if intValue(summary["tools"]) != 12 || intValue(summary["subagentCalls"]) != 4 || intValue(summary["failed"]) != 1 ||
		intValue(summary["startedAt"]) != 1_000 || intValue(summary["endedAt"]) != 9_000 || intValue(summary["events"]) != 20 {
		t.Fatalf("summary %v", summary)
	}
	var kinds, keys []string
	for _, raw := range anySlice(message["events"]) {
		event := mapFromAnyMain(raw)
		kind := fieldString(event, "kind")
		if kind == "tool" && event["workStub"] == true {
			kind = fmt.Sprintf("stub@%d/%d", intValue(event["at"]), len(anySlice(event["images"])))
			if fieldString(event, "title") != "" {
				kind += "+title"
			}
		}
		kinds = append(kinds, kind)
		keys = append(keys, fieldString(event, "key"))
	}
	// At 4 the reads' anchor and one stub holding the two named shots; at 9
	// the unnamed shot alone with its title, the page and the header whole;
	// an anchor for the edits at the end of the text.
	if strings.Join(kinds, ",") != "thinking,stub@4/0,stub@4/2,thinking,stub@9/1+title,tool,tool,stub@35/0" {
		t.Fatalf("events %v (%v)", kinds, keys)
	}
	// The group read back is the row's events, as the read without the option
	// sends them, under the group's options.
	wantEvents := plainEvents(t, lazy, "wg-message-7")
	if !bytes.Equal(mustJSONBytes(t, wantGroup), mustJSONBytes(t, wantEvents)) {
		t.Fatalf("work group differs from the row's events\n%s\n%s", mustJSONBytes(t, wantGroup), mustJSONBytes(t, wantEvents))
	}
	if len(plain[6]["events"].([]any)) != 20 {
		t.Fatal("the plain row lost events")
	}
}

func readWorkGroup(t *testing.T, runtime *providerChatRuntime, messageID string) []any {
	t.Helper()
	options := archiveReadOptions{deferMessageImages: true, leanTools: true, leanTitles: true, toolSummaries: true}
	var events []any
	offset, pages := 0, 0
	for {
		reply, found, err := runtime.ProjectWorkGroupByTab("tab-wg", workGroupRequest{messageID: messageID, offset: offset}, options)
		if err != nil || !found {
			t.Fatalf("work group page at %d: %v %v", offset, found, err)
		}
		var page map[string]any
		if err := json.Unmarshal(mustJSONBytes(t, reply), &page); err != nil {
			t.Fatal(err)
		}
		pages++
		events = append(events, anySlice(page["events"])...)
		next, total := intValue(page["next"]), intValue(page["total"])
		if next >= total {
			if len(events) != total {
				t.Fatalf("work group read %d of %d events", len(events), total)
			}
			break
		}
		if next <= offset {
			t.Fatalf("work group page at %d did not advance", offset)
		}
		offset = next
	}
	if pages < 2 {
		t.Fatalf("a %d-byte page budget read the group in %d page", workGroupPageBytes, pages)
	}
	return events
}

// plainEvents is the row's events from a read without work groups, under the
// work group page's options.
func plainEvents(t *testing.T, runtime *providerChatRuntime, messageID string) []any {
	t.Helper()
	raw, found, err := runtime.ProjectRecentArchiveByTabRaw("tab-wg", 9, archiveReadOptions{deferMessageImages: true, leanTools: true, leanTitles: true, toolSummaries: true})
	if err != nil || !found {
		t.Fatalf("plain read: %v %v", found, err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if fieldString(row, "id") == messageID {
			return anySlice(row["events"])
		}
	}
	t.Fatalf("no row %s", messageID)
	return nil
}

func mustJSONBytes(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestBriefSpawnedWorkKeepsWhatTheFirstPaintDraws(t *testing.T) {
	now, err := time.Parse(time.RFC3339, "2026-11-12T20:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	items := []map[string]any{
		{"id": "run-1", "kind": "agent", "toolCallId": "call-run", "status": "running", "summary": "busy"},
		{"id": "new-1", "kind": "bash", "status": "exited", "finishedAt": "2026-11-12T08:00:00Z", "summary": "today"},
		{"id": "old-agent", "kind": "agent", "toolCallId": "call-old", "status": "exited", "finishedAt": "2026-10-01T08:00:00Z", "summary": strings.Repeat("s", 5000)},
		{"id": "old-sub", "kind": "subagent", "toolCallId": "call-sub", "status": "failed", "finishedAt": "2026-10-01T08:00:00Z"},
		{"id": "old-bash", "kind": "bash", "status": "exited", "finishedAt": "2026-10-01T08:00:00Z"},
		{"id": "kept-agent", "kind": "agent", "toolCallId": "call-kept", "status": "exited", "finishedAt": "2026-10-01T08:00:00Z", "summary": "shown"},
	}
	reply := briefSpawnedWorkReply(items, map[string]struct{}{"call-kept": {}}, now)
	var ids []string
	for _, item := range reply["items"].([]map[string]any) {
		ids = append(ids, fieldString(item, "id"))
	}
	if strings.Join(ids, ",") != "run-1,new-1,kept-agent" {
		t.Fatalf("kept %v", ids)
	}
	if got := mustJSONBytes(t, reply["settled"]); string(got) != `[["call-old","exited"],["old-sub","failed"]]` {
		t.Fatalf("settled %s", got)
	}
	if reply["omitted"] != 3 || reply["brief"] != true {
		t.Fatalf("reply %v", reply)
	}
}
