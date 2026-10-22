package main

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

// A tool title can carry a whole command or file: one message of a chat was
// 84 MB, 79 MB of it titles, while each row shows one line. A renderer that
// asks for leanTitles receives a bounded head plus the size and reads the
// whole title back when the row opens; one that asks only for leanTools, or
// for nothing, keeps every title whole.
func TestLeanTitlesDeferLongToolTitlesOnlyWhenAsked(t *testing.T) {
	long := "cat <<'EOF' > fixture.json\n" + strings.Repeat("{\"row\": \"ünïcödé\"}\n", 600) + "EOF"
	timeline := []chat.TimelineEntry{
		toolEntry("long", providercontract.ToolEvent{Title: long, Command: "cat > fixture.json", Output: "ok"}),
		toolEntry("short", providercontract.ToolEvent{Title: "Read", Output: "ok"}),
		toolEntry("header", providercontract.ToolEvent{Title: long, SubagentID: "sub", SubagentHeader: true, Output: "done"}),
		toolEntry("page", providercontract.ToolEvent{Title: long + " | workass-tools call workass_render_html", Output: "{}"}),
	}
	runtime, hub := leanHistoryFixture(t, timeline)

	tools, err := hub.Invoke("chat:archive-load", []any{"lean-history-tab", map[string]any{"tail": 10, "leanTools": true}})
	if err != nil {
		t.Fatal(err)
	}
	if events := toolEventsByKey(t, decodedWireReply(t, tools), "reply-tools"); events["long"]["title"] != long || events["long"]["titleDeferred"] != nil {
		t.Fatal("a renderer that asked only for leanTools lost a whole title")
	}

	lean, err := hub.Invoke("chat:archive-load", []any{"lean-history-tab", map[string]any{"tail": 10, "leanTools": true, "leanTitles": true}})
	if err != nil {
		t.Fatal(err)
	}
	events := toolEventsByKey(t, decodedWireReply(t, lean), "reply-tools")
	head, _ := events["long"]["title"].(string)
	if events["long"]["titleDeferred"] != true || intValue(events["long"]["titleBytes"]) != len(long) ||
		len(head) == 0 || len(head) > leanToolPayloadHead || !utf8.ValidString(head) || !strings.HasPrefix(long, head) {
		t.Fatalf("a long title was not sent as a bounded head: deferred=%v bytes=%v head=%d", events["long"]["titleDeferred"], events["long"]["titleBytes"], len(head))
	}
	if events["short"]["title"] != "Read" || events["short"]["titleDeferred"] != nil {
		t.Fatal("a short title was deferred")
	}
	if events["header"]["title"] != long || events["header"]["titleDeferred"] != nil {
		t.Fatal("a subagent header title was deferred; the rail prints it unopened")
	}
	if events["page"]["titleDeferred"] != nil {
		t.Fatal("a title that publishes a page was deferred; the renderer finds the page by its call")
	}
	if got := readToolDetail(t, hub, "long", "title"); got != long {
		t.Fatal("tool detail did not return the exact title")
	}

	// session:get honors the same option.
	raw, err := runtime.ProjectSessionRaw(archiveReadOptions{leanTools: true, leanTitles: true})
	if err != nil {
		t.Fatal(err)
	}
	var session map[string]any
	if err := json.Unmarshal(raw, &session); err != nil {
		t.Fatal(err)
	}
	projected := mapFromAnyMain(anySlice(session["chats"])[0])
	if sessionEvents := toolEventsByKey(t, projected["messages"], "reply-tools"); sessionEvents["long"]["titleDeferred"] != true {
		t.Fatal("the lean session projection kept the long title")
	}
}
