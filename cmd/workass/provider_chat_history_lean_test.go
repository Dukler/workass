package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
	"workass/internal/wire"
)

func leanHistoryFixture(t *testing.T, timeline []chat.TimelineEntry) (*providerChatRuntime, *wire.Hub) {
	t.Helper()
	stateDir := t.TempDir()
	engine, err := chat.NewEngine("lean-history-chat")
	if err != nil {
		t.Fatal(err)
	}
	messages := make([]chat.LedgerEvent, 0, 60)
	for index := 0; index < 29; index++ {
		messages = append(messages,
			chat.LedgerEvent{EventID: fmt.Sprintf("user-%d", index), MessageID: fmt.Sprintf("user-%d", index), OperationID: "fixture", Role: "user", Text: "question", Status: "done"},
			chat.LedgerEvent{EventID: fmt.Sprintf("reply-%d", index), MessageID: fmt.Sprintf("reply-%d", index), OperationID: "fixture", Role: "assistant", Text: "answer", Status: "done"},
		)
	}
	messages = append(messages,
		chat.LedgerEvent{EventID: "user-tools", MessageID: "user-tools", OperationID: "fixture", Role: "user", Text: "run the tools", Status: "done"},
		chat.LedgerEvent{EventID: "reply-tools", MessageID: "reply-tools", OperationID: "fixture", Role: "assistant", Text: "done", Status: "done", Timeline: timeline},
	)
	if err := engine.Apply(chat.InitializeFork{Presentation: chat.PresentationState{TabID: "lean-history-tab", Title: "Tools"},
		SourceChatID: "source", OperationID: "create-lean-history", Digest: "fixture", Messages: messages}); err != nil {
		t.Fatal(err)
	}
	runtime := &providerChatRuntime{manager: &acp.Manager{}, sessions: sharedSessionStore(stateDir), stateDir: stateDir,
		actors: map[string]*providerChatActor{"lean-history-chat": {engine: engine}}, known: map[string]struct{}{"lean-history-chat": {}}}
	hub := wire.NewHub()
	registerArchiveHandlers(hub, nil, runtime)
	return runtime, hub
}

func toolEntry(key string, tool providercontract.ToolEvent) chat.TimelineEntry {
	tool.ToolCallID = "call-" + key
	if tool.Status == "" {
		tool.Status = "completed"
	}
	return chat.TimelineEntry{Key: key, Kind: providercontract.EventToolUpdate, Tool: &tool}
}

func toolEventsByKey(t *testing.T, reply any, messageID string) map[string]map[string]any {
	t.Helper()
	for _, raw := range anySlice(reply) {
		message := mapFromAnyMain(raw)
		if fieldString(message, "id") != messageID {
			continue
		}
		events := map[string]map[string]any{}
		for _, rawEvent := range anySlice(message["events"]) {
			event := mapFromAnyMain(rawEvent)
			events[fieldString(event, "key")] = event
		}
		return events
	}
	t.Fatalf("message %q missing from reply", messageID)
	return nil
}

func readToolDetail(t *testing.T, hub *wire.Hub, key, field string) string {
	t.Helper()
	var text strings.Builder
	offset := 0
	for {
		reply, err := hub.Invoke("chat:archive-load", []any{"lean-history-tab", map[string]any{
			"tail": 1, "toolDetail": map[string]any{"messageId": "reply-tools", "key": key, "field": field, "offset": offset},
		}})
		if err != nil {
			t.Fatalf("tool detail %s/%s at %d: %v", key, field, offset, err)
		}
		chunk := mapFromAnyMain(reply)
		part, _ := chunk["text"].(string)
		if len(part) > toolDetailChunkBytes || !utf8.ValidString(part) {
			t.Fatalf("tool detail chunk is unbounded or splits a rune: %d bytes", len(part))
		}
		if intValue(chunk["offset"]) != offset {
			t.Fatalf("tool detail answered offset %v for %d", chunk["offset"], offset)
		}
		text.WriteString(part)
		next, total := intValue(chunk["next"]), intValue(chunk["total"])
		if next == total {
			return text.String()
		}
		if next <= offset {
			t.Fatal("tool detail did not advance")
		}
		offset = next
	}
}

// Tool payloads were 95% of a large chat's history bytes. A renderer that asks
// for leanTools receives a bounded head of each large committed payload plus its
// size, and reads the exact payload back in bounded chunks; one that does not
// ask keeps the byte-identical full rows.
func TestLeanHistoryDefersLargeToolPayloadsAndServesThemBack(t *testing.T) {
	big := strings.Repeat("line of command output ñ\n", 2000)                       // ~50 KB, multibyte runes
	huge := strings.Repeat("ünïcödé payload that crosses chunk boundaries ", 60000) // ~3 MB
	timeline := []chat.TimelineEntry{
		toolEntry("big", providercontract.ToolEvent{Title: "go test", Command: "go test ./...", Output: big}),
		toolEntry("small", providercontract.ToolEvent{Title: "ls", Output: "a\nb\n"}),
		toolEntry("input", providercontract.ToolEvent{Title: "Write", Input: big, Output: "ok"}),
		toolEntry("huge", providercontract.ToolEvent{Title: "cat", Output: huge}),
		toolEntry("header", providercontract.ToolEvent{Title: "Explore", SubagentID: "sub", SubagentHeader: true, Output: big}),
	}
	runtime, hub := leanHistoryFixture(t, timeline)

	full, err := hub.Invoke("chat:archive-load", []any{"lean-history-tab", map[string]any{"tail": 10, "deferImages": true}})
	if err != nil {
		t.Fatal(err)
	}
	full = decodedWireReply(t, full)
	legacy, _, err := runtime.ProjectRecentArchiveByTab("lean-history-tab", 10, archiveReadOptions{deferMessageImages: true})
	if err != nil {
		t.Fatal(err)
	}
	fullJSON, _ := json.Marshal(full)
	legacyJSON, _ := json.Marshal(legacy)
	if string(fullJSON) != string(legacyJSON) || !strings.Contains(string(fullJSON), "crosses chunk boundaries") {
		t.Fatal("a renderer that did not ask for lean tools lost its full payloads")
	}
	if events := toolEventsByKey(t, full, "reply-tools"); events["big"]["output"] != big || events["big"]["outputDeferred"] != nil {
		t.Fatal("full read marked a payload deferred")
	}

	lean, err := hub.Invoke("chat:archive-load", []any{"lean-history-tab", map[string]any{"tail": 10, "deferImages": true, "leanTools": true}})
	if err != nil {
		t.Fatal(err)
	}
	lean = decodedWireReply(t, lean)
	leanJSON, _ := json.Marshal(lean)
	if len(leanJSON)*20 > len(fullJSON) {
		t.Fatalf("lean read is not lean: %d bytes vs %d", len(leanJSON), len(fullJSON))
	}
	events := toolEventsByKey(t, lean, "reply-tools")
	for _, key := range []string{"big", "huge"} {
		head, _ := events[key]["output"].(string)
		if events[key]["outputDeferred"] != true || len(head) == 0 || len(head) > leanToolPayloadHead || !utf8.ValidString(head) {
			t.Fatalf("%s output was not deferred to a bounded head: %#v", key, events[key]["outputDeferred"])
		}
	}
	if intValue(events["big"]["outputBytes"]) != len(big) || intValue(events["huge"]["outputBytes"]) != len(huge) {
		t.Fatal("deferred payload lost its size")
	}
	if head, _ := events["big"]["output"].(string); !strings.HasPrefix(big, head) || !strings.HasSuffix(head, "\n") {
		t.Fatal("deferred head is not a whole-line prefix of the payload")
	}
	if events["small"]["output"] != "a\nb\n" || events["small"]["outputDeferred"] != nil {
		t.Fatal("a small payload was deferred")
	}
	if events["input"]["inputDeferred"] != true || events["input"]["output"] != "ok" {
		t.Fatal("a large tool input was not deferred")
	}
	if events["header"]["output"] != big || events["header"]["outputDeferred"] != nil {
		t.Fatal("a subagent header summary was deferred; the rail prints it unopened")
	}

	if got := readToolDetail(t, hub, "big", "output"); got != big {
		t.Fatal("tool detail did not return the exact output")
	}
	if got := readToolDetail(t, hub, "huge", "output"); got != huge {
		t.Fatal("chunked tool detail did not reassemble the exact output")
	}
	if got := readToolDetail(t, hub, "input", "input"); got != big {
		t.Fatal("tool detail did not return the exact input")
	}
	if _, err := hub.Invoke("chat:archive-load", []any{"lean-history-tab", map[string]any{
		"tail": 1, "toolDetail": map[string]any{"messageId": "reply-tools", "key": "missing"},
	}}); err == nil {
		t.Fatal("an unknown tool key was answered")
	}

	// Pages and the session projection honor the same option; the canonical
	// actor is untouched by any lean read.
	before := runtime.actors["lean-history-chat"].engine.Snapshot()
	page, found, err := runtime.ProjectArchivePageBeforeByTab("lean-history-tab", "user-tools", 40, archiveReadOptions{leanTools: true})
	if err != nil || !found || len(page) != 40 {
		t.Fatalf("lean page: rows=%d found=%v err=%v", len(page), found, err)
	}
	session, err := runtime.ProjectSession(archiveReadOptions{leanTools: true})
	if err != nil {
		t.Fatal(err)
	}
	projected := mapFromAnyMain(anySlice(session["chats"])[0])
	if sessionEvents := toolEventsByKey(t, projected["messages"], "reply-tools"); sessionEvents["big"]["outputDeferred"] != true {
		t.Fatal("lean session projection kept the large output")
	}
	plain, err := runtime.ProjectSession()
	if err != nil {
		t.Fatal(err)
	}
	if plainEvents := toolEventsByKey(t, mapFromAnyMain(anySlice(plain["chats"])[0])["messages"], "reply-tools"); plainEvents["big"]["output"] != big {
		t.Fatal("plain session projection lost the full output")
	}
	if !reflect.DeepEqual(before, runtime.actors["lean-history-chat"].engine.Snapshot()) {
		t.Fatal("a lean read mutated the authoritative actor")
	}
}

// The live foreground owner is never trimmed: its rows are not addressable by
// a committed message id yet, and their tools are still changing.
func TestLeanHistoryLeavesLiveRowsWhole(t *testing.T) {
	big := strings.Repeat("x", leanToolPayloadThreshold+1)
	rows := []any{
		map[string]any{"id": "done", "status": "done", "events": []any{map[string]any{"kind": "tool", "key": "a", "output": big}}},
		map[string]any{"id": "live", "status": "running", "events": []any{map[string]any{"kind": "tool", "key": "b", "output": big}}},
	}
	leanArchiveToolPayloads(rows, true, false)
	if mapFromAnyMain(anySlice(mapFromAnyMain(rows[0])["events"])[0])["outputDeferred"] != true {
		t.Fatal("committed row was not trimmed")
	}
	if live := mapFromAnyMain(anySlice(mapFromAnyMain(rows[1])["events"])[0]); live["output"] != big || live["outputDeferred"] != nil {
		t.Fatal("live row was trimmed")
	}
}

func TestLedgerReaderPagesBySequenceAndReadsOneMessage(t *testing.T) {
	runtime, _ := leanHistoryFixture(t, []chat.TimelineEntry{toolEntry("big", providercontract.ToolEvent{Title: "x", Output: "y"})})
	engine := runtime.actors["lean-history-chat"].engine
	var reader chat.LedgerReader = engine
	newest, err := reader.ReadMessages(0, 5)
	if err != nil || len(newest.Events) != 5 || newest.LedgerCount != 60 || newest.Events[4].MessageID != "reply-tools" {
		t.Fatalf("newest page = %d rows, count %d, err %v", len(newest.Events), newest.LedgerCount, err)
	}
	older, err := reader.ReadMessages(uint64(newest.Start+1), 100)
	if err != nil || len(older.Events) != 55 || older.Start != 0 || older.End != newest.Start {
		t.Fatalf("older page = %d rows [%d,%d), err %v", len(older.Events), older.Start, older.End, err)
	}
	byCursor, err := reader.ReadLedgerPageBefore("user-tools", 3)
	if err != nil || len(byCursor.Events) != 3 || byCursor.Events[2].MessageID != "reply-28" {
		t.Fatalf("cursor page = %#v err %v", byCursor.Events, err)
	}
	if _, err := reader.ReadMessages(1000, 1); err == nil {
		t.Fatal("a page boundary past the ledger was accepted")
	}
	event, found := reader.ReadMessage("reply-tools")
	if !found || len(event.Timeline) != 1 || event.Timeline[0].Tool.Output != "y" {
		t.Fatal("one message read failed")
	}
	event.Timeline[0].Tool.Output = "mutated"
	if again, _ := reader.ReadMessage("reply-tools"); again.Timeline[0].Tool.Output != "y" {
		t.Fatal("one message read aliased actor state")
	}
	if _, found := reader.ReadMessage("missing"); found {
		t.Fatal("a missing message was found")
	}
}

// The active chat's session tail carried its screenshots inline (720 KiB of a
// lean session:get on the seeded chat). A renderer that asks for deferImages
// gets them by reference, as on archive-load; one that does not keeps them.
func TestSessionProjectionDefersMessageImagesOnlyWhenAsked(t *testing.T) {
	stateDir, ref, data := imageReadFixture(t)
	engine, err := chat.NewEngine("session-image-chat")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(chat.InitializeFork{Presentation: chat.PresentationState{TabID: "session-image-tab", Title: "Screenshots"},
		SourceChatID: "source", OperationID: "create-session-images", Digest: "fixture", Messages: []chat.LedgerEvent{{
			EventID: "reply", MessageID: "assistant-reply", OperationID: "turn", Role: "assistant", Text: "![shot](shot.png)", Status: "done",
			Attachments: []providercontract.Attachment{{ID: "shot", Name: "shot", MIMEType: "image/png", Source: "shot.png", Ref: providerSessionImageRefPrefix + ref}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &providerChatRuntime{manager: &acp.Manager{}, sessions: sharedSessionStore(stateDir), stateDir: stateDir,
		actors: map[string]*providerChatActor{"session-image-chat": {engine: engine}}, known: map[string]struct{}{"session-image-chat": {}}}
	firstImage := func(root map[string]any) map[string]any {
		row := mapFromAnyMain(anySlice(mapFromAnyMain(anySlice(root["chats"])[0])["messages"])[0])
		return mapFromAnyMain(anySlice(row["images"])[0])
	}
	inline, err := runtime.ProjectSession()
	if err != nil || fieldString(firstImage(inline), "data") != data {
		t.Fatalf("a renderer that did not ask lost its inline image: err=%v", err)
	}
	deferred, err := runtime.ProjectSession(archiveReadOptions{leanTools: true, deferMessageImages: true})
	if err != nil {
		t.Fatal(err)
	}
	if image := firstImage(deferred); fieldString(image, "deferredImageRef") != ref || fieldString(image, "data") != "" {
		t.Fatalf("session image was not deferred: %#v", image)
	}
}
