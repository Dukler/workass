package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

// A long settled reply comes as its last part; its earlier parts, read with
// {contentHead}, put it back together exactly: the same text, the same events
// at the same places.
func TestLongRowsComeInPartsThatMakeTheWholeReply(t *testing.T) {
	stateDir := t.TempDir()
	backend, err := openChatActorBackend(stateDir, true, "test", t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.close()
	var text strings.Builder
	for index := 0; text.Len() < 400<<10; index++ {
		fmt.Fprintf(&text, "Paragraph %d — ünïcödé 😀 ![shot](/tmp/shot-%d.png)\n", index, index)
		if index%50 == 7 {
			text.WriteString("```\ncode\n\nstill code\n```\n")
		}
		text.WriteString("\n")
	}
	full := text.String()
	length := utf16Len(full)
	lazyTestChat(t, stateDir, backend, "long", 4, func(state *chat.State) {
		row := &state.Ledger[3]
		row.Role, row.Status, row.Text = "assistant", "done", full
		row.Timeline = nil
		for index := 0; index < 40; index++ {
			at := length * index / 40
			row.Timeline = append(row.Timeline, chat.TimelineEntry{Key: fmt.Sprintf("timeline:tool:t%d", index), At: at, Kind: providercontract.EventToolUpdate,
				Tool: &providercontract.ToolEvent{ToolCallID: fmt.Sprintf("t%d", index), ToolKind: "read", Title: "Read", Status: "completed", Output: "ok",
					Attachments: []providercontract.Attachment{{ID: fmt.Sprintf("img-%d", index), Name: "shot.png", MIMEType: "image/png", Ref: providerSessionImageRefPrefix + lazyImageRef(t, stateDir, fmt.Sprintf("long-%d", index))}}}})
		}
	})
	runtime := startLazyTestRuntime(t, stateDir, backend, "tab-none")
	defer runtime.Close(context.Background())
	defer workGroupPackStores.Delete(stateDir)
	whole := archiveReadOptions{deferMessageImages: true, leanTools: true, leanTitles: true, workGroups: true}
	parted := whole
	parted.longRows = true
	read := func(opts archiveReadOptions) map[string]any {
		raw, found, err := runtime.ProjectRecentArchiveByTabRaw("tab-long", 1, opts)
		if err != nil || !found {
			t.Fatal(found, err)
		}
		var rows []map[string]any
		if err := json.Unmarshal(raw, &rows); err != nil || len(rows) != 1 {
			t.Fatal(err, len(rows))
		}
		return rows[0]
	}
	want := read(whole)
	tail := read(parted)
	from := intValue(tail["contentFrom"])
	if from <= 0 || intValue(tail["contentLength"]) != length || len(tail["content"].(string)) > 2*longRowTailBytes {
		t.Fatalf("tail from %d length %v content %d", from, tail["contentLength"], len(tail["content"].(string)))
	}
	// Part by part, as a reader scrolling up reads it.
	content, events := tail["content"].(string), anySlice(tail["events"])
	parts := 0
	for before := from; before > 0; parts++ {
		reply, found, err := runtime.ContentHeadByTab("tab-long", contentHeadRequest{messageID: fieldString(tail, "id"), before: before}, []archiveReadOptions{parted})
		if err != nil || !found {
			t.Fatal(found, err)
		}
		var head map[string]any
		if err := json.Unmarshal(mustJSONBytes(t, reply), &head); err != nil {
			t.Fatal(err)
		}
		if intValue(head["to"]) != before || intValue(head["from"]) >= before {
			t.Fatalf("part %v", head["from"])
		}
		content = head["content"].(string) + content
		events = append(anySlice(head["events"]), events...)
		before = intValue(head["from"])
	}
	if parts < 2 {
		t.Fatalf("%d parts", parts)
	}
	if w := want["content"].(string); content != w {
		i := 0
		for i < len(content) && i < len(w) && content[i] == w[i] {
			i++
		}
		t.Logf("differ at %d: got %q want %q", i, content[max(0, i-20):min(len(content), i+40)], w[max(0, i-20):min(len(w), i+40)])
		t.Fatalf("the parts are not the whole text: %d vs %d (full %d), prefix ok %v", len(content), len(want["content"].(string)), len(full), strings.HasSuffix(want["content"].(string), content))
	}
	// The renderer restores what each by-reference image leaves out.
	compacted := 0
	for _, raw := range events {
		for _, rawImage := range anySlice(mapFromAnyMain(raw)["images"]) {
			image := mapFromAnyMain(rawImage)
			if _, ok := image["data"]; !ok {
				image["data"] = ""
				compacted++
			}
			if _, ok := image["mimeType"]; !ok {
				image["mimeType"] = "image/png"
			}
		}
	}
	if compacted == 0 {
		t.Fatal("no image was sent compact")
	}
	if string(mustJSONBytes(t, events)) != string(mustJSONBytes(t, want["events"])) {
		t.Fatal("the parts' events are not the whole reply's")
	}
	// All at once, as find and copy read it.
	reply, _, err := runtime.ContentHeadByTab("tab-long", contentHeadRequest{messageID: fieldString(tail, "id"), before: from, all: true}, []archiveReadOptions{parted})
	if err != nil {
		t.Fatal(err)
	}
	var head map[string]any
	_ = json.Unmarshal(mustJSONBytes(t, reply), &head)
	if intValue(head["from"]) != 0 || head["content"].(string)+tail["content"].(string) != full {
		t.Fatal("the whole head is not the text before the last part")
	}
	// Every part in one read, framed from the kept parts.
	var all struct {
		MessageID string `json:"messageId"`
		Parts     []struct {
			From    int    `json:"from"`
			To      int    `json:"to"`
			Content string `json:"content"`
		} `json:"parts"`
	}
	partsReply, _, err := runtime.ContentHeadByTab("tab-long", contentHeadRequest{messageID: fieldString(tail, "id"), before: from, parts: true}, []archiveReadOptions{parted})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(mustJSONBytes(t, partsReply), &all); err != nil {
		t.Fatal(err)
	}
	joined, at := "", 0
	for _, part := range all.Parts {
		if part.From != at {
			t.Fatalf("parts not contiguous at %d", part.From)
		}
		joined += part.Content
		at = part.To
	}
	if at != from || joined+tail["content"].(string) != full {
		t.Fatal("the parts read at once are not the head")
	}
	// A cut never falls inside a code fence.
	for _, cut := range longRowBounds(full) {
		if strings.Count(full[:cut], "```")%2 != 0 {
			t.Fatalf("cut at %d is inside a fence", cut)
		}
	}
	// Without the option nothing changes.
	if _, split := want["contentFrom"]; split {
		t.Fatal("a read without longRows was split")
	}
}

// A large row encoded in parallel is json.Marshal's encoding, byte for byte.
func TestMarshalArchiveRowIsMarshal(t *testing.T) {
	events := make([]any, 0, 5000)
	for index := 0; index < 5000; index++ {
		events = append(events, map[string]any{"kind": "tool", "key": fmt.Sprintf("k%d", index), "title": "<b>&é😀", "at": index})
	}
	row := map[string]any{"id": "m", "role": "assistant", "content": "x<y", "events": events, "zeta": 1}
	want, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	got, err := marshalArchiveRow(row)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatal("parallel encoding differs")
	}
}
