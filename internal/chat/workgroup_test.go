package chat

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"workass/internal/provider"
)

// workGroupTestRow is a settled row whose payloads hold everything the skim
// must step over: escaped quotes and backslashes, a raw {"Key": inside a
// string, unicode, HTML-escaped bytes and JSON in JSON.
func workGroupTestRow(calls int, payload int) LedgerEvent {
	tricky := `say "hi" \ {"Key":"x","At":1} <&> ñ ` + "\n\t" + `{\"Key\":\"y\"} \\"` + strings.Repeat("z", payload)
	row := LedgerEvent{EventID: "e", MessageID: "m", Role: "assistant", Status: "done", Text: "Did it.\n\nDone."}
	row.Timeline = append(row.Timeline, TimelineEntry{Key: "th", Kind: provider.EventThinkingUpdate, Thinking: &provider.ThinkingEvent{Text: tricky}})
	for index := 0; index < calls; index++ {
		tool := &provider.ToolEvent{ToolCallID: fmt.Sprintf("c%d", index), ToolKind: "execute", Title: tricky, Status: "completed", Input: tricky, Output: tricky, StartedAtUnixMS: int64(10 + index), EndedAtUnixMS: int64(20 + index)}
		switch index % 7 {
		case 1:
			tool.SubagentID = "agent"
		case 2:
			tool.Attachments = []provider.Attachment{{ID: "a", Name: "shot.png", MIMEType: "image/png", Ref: "session-image:abc"}}
		case 3:
			tool.Status = "failed"
		}
		row.Timeline = append(row.Timeline, TimelineEntry{Key: fmt.Sprintf("k%d", index), At: index % 3 * 4, Kind: provider.EventToolUpdate, Tool: tool})
	}
	row.Timeline = append(row.Timeline,
		TimelineEntry{Key: "agent", At: 4, Kind: provider.EventToolUpdate, Tool: &provider.ToolEvent{ToolCallID: "agent", ToolKind: "agent", Title: "Review", SubagentHeader: true, SubagentID: "agent", Output: tricky}},
		TimelineEntry{Key: "page", At: 8, Kind: provider.EventToolUpdate, Tool: &provider.ToolEvent{ToolCallID: "page", Title: "mcp__workass__workass_render_html", Output: `{"htmlRender":{"id":"p"}}`}},
		TimelineEntry{Key: "not-page", At: 8, Kind: provider.EventToolUpdate, Tool: &provider.ToolEvent{ToolCallID: "np", Title: "talks about htmlRender", Output: "no"}},
		TimelineEntry{Key: "result", At: 15, Kind: provider.EventAssistantChunk, Result: &AssistantResultSpan{Start: 0, End: 4}},
	)
	return row
}

func TestDecodeLedgerEventForWorkGroupReducesLikeAWholeDecode(t *testing.T) {
	for _, shape := range []struct {
		name           string
		calls, payload int
	}{
		{"few calls", 5, 10},
		{"small", 40, 10},
		{"parallel", 400, 4000},
	} {
		t.Run(shape.name, func(t *testing.T) {
			row := workGroupTestRow(shape.calls, shape.payload)
			raw, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			if shape.name == "parallel" && len(raw) < skimParallelBytes {
				t.Fatalf("row is %d bytes, under the parallel threshold", len(raw))
			}
			var whole LedgerEvent
			if err := json.Unmarshal(raw, &whole); err != nil {
				t.Fatal(err)
			}
			skimmed, err := DecodeLedgerEventForWorkGroup(raw)
			if err != nil {
				t.Fatal(err)
			}
			wantRow, wantFolds := ReduceLedgerEventForWorkGroup(whole)
			gotRow, gotFolds := ReduceLedgerEventForWorkGroup(skimmed)
			if wantFolds != gotFolds {
				t.Fatalf("folds %v, whole decode %v", gotFolds, wantFolds)
			}
			if !wantFolds {
				if mustMarshal(t, skimmed) != mustMarshal(t, whole) {
					t.Fatal("a row that does not fold decoded differently")
				}
				return
			}
			if mustMarshal(t, gotRow) != mustMarshal(t, wantRow) {
				t.Fatalf("reduced rows differ\n%s\n%s", mustMarshal(t, gotRow), mustMarshal(t, wantRow))
			}
			// Kept calls and other entries are whole; no shell carries a payload.
			for _, item := range gotRow.Items {
				if item.Entry != nil && item.Entry.Tool != nil && item.Entry.Tool.ToolCallID == "agent" && item.Entry.Tool.Output == "" {
					t.Fatal("a subagent header lost its payload")
				}
			}
			if gotRow.Work.Calls == 0 || gotRow.Work.SubagentCalls == 0 || gotRow.Work.Failed == 0 {
				t.Fatalf("summary %+v", gotRow.Work)
			}
			// The stored timeline reads back entry by entry.
			stored, err := ParseStoredTimeline(raw)
			if err != nil || stored.Len() != len(whole.Timeline) {
				t.Fatalf("stored timeline %d entries (%v), want %d", stored.Len(), err, len(whole.Timeline))
			}
			for index := range whole.Timeline {
				entry, err := stored.Entry(index)
				if err != nil || mustMarshal(t, entry) != mustMarshal(t, whole.Timeline[index]) {
					t.Fatalf("stored entry %d differs (%v)", index, err)
				}
			}
		})
	}
}

func TestWorkGroupClassifiesLikeTheRenderer(t *testing.T) {
	parents := map[string]struct{}{"task": {}}
	for _, test := range []struct {
		call WorkGroupCall
		want WorkGroupFold
	}{
		{WorkGroupCall{ID: "a"}, WorkGroupFolded},
		{WorkGroupCall{ID: "a", Media: true}, WorkGroupMediaStub},
		{WorkGroupCall{ID: "a", Page: true}, WorkGroupKeep},
		{WorkGroupCall{ID: "a", Header: true, SubagentID: "a"}, WorkGroupKeep},
		{WorkGroupCall{ID: "a", ToolKind: "agent"}, WorkGroupKeep},
		{WorkGroupCall{ID: "task"}, WorkGroupKeep},
		{WorkGroupCall{ID: "x", SubagentID: "x"}, WorkGroupKeep},
		{WorkGroupCall{ID: "c", SubagentID: "task"}, WorkGroupChild},
		{WorkGroupCall{ID: "c", SubagentID: "task", Media: true}, WorkGroupChild},
	} {
		if got := ClassifyWorkGroupCall(test.call, parents); got != test.want {
			t.Fatalf("%+v: %v, want %v", test.call, got, test.want)
		}
	}
}

func mustMarshal(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
