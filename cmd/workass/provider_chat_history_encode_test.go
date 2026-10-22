package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
	"workass/internal/wire"
)

// A history page is sized by the encoding it sends. The reply reuses that
// encoding instead of marshalling every row again, and it must be the exact
// bytes json.Marshal writes for the same rows.
func TestHistoryPagesSendTheEncodingThatSizedThem(t *testing.T) {
	stateDir, ref, _ := imageReadFixture(t)
	engine, err := chat.NewEngine("encoded-chat")
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("output <line> & more\n", 400)
	messages := make([]chat.LedgerEvent, 0, 40)
	for index := 0; index < 20; index++ {
		tool := providercontract.ToolEvent{ToolCallID: fmt.Sprintf("call-%d", index), Title: "Screenshot <a&b>", Status: "completed", Output: big,
			Attachments: []providercontract.Attachment{{ID: "shot", Name: "shot.png", MIMEType: "image/png", Ref: providerSessionImageRefPrefix + ref}}}
		messages = append(messages,
			chat.LedgerEvent{EventID: fmt.Sprintf("user-%d", index), MessageID: fmt.Sprintf("user-%d", index), OperationID: "fixture", Role: "user", Text: "look here", Status: "done",
				Attachments: []providercontract.Attachment{{ID: "att", Name: "player.png", MIMEType: "image/png", Source: "player.png", Ref: providerSessionImageRefPrefix + ref}}},
			chat.LedgerEvent{EventID: fmt.Sprintf("reply-%d", index), MessageID: fmt.Sprintf("reply-%d", index), OperationID: "fixture", Role: "assistant", Text: "done", Status: "done",
				Timeline: []chat.TimelineEntry{{Key: fmt.Sprintf("tool-%d", index), Kind: providercontract.EventToolUpdate, Tool: &tool}}},
		)
	}
	if err := engine.Apply(chat.InitializeFork{Presentation: chat.PresentationState{TabID: "encoded-tab", Title: "Encoded"},
		SourceChatID: "source", OperationID: "create-encoded", Digest: "fixture", Messages: messages}); err != nil {
		t.Fatal(err)
	}
	runtime := &providerChatRuntime{manager: &acp.Manager{}, sessions: sharedSessionStore(stateDir), stateDir: stateDir,
		actors: map[string]*providerChatActor{"encoded-chat": {engine: engine}}, known: map[string]struct{}{"encoded-chat": {}}}
	hub := wire.NewHub()
	registerArchiveHandlers(hub, nil, runtime)

	for _, options := range []archiveReadOptions{{}, {deferMessageImages: true}, {deferMessageImages: true, leanTools: true}} {
		wireOptions := map[string]any{"deferImages": options.deferMessageImages, "leanTools": options.leanTools}
		tail, found, err := runtime.ProjectRecentArchiveByTab("encoded-tab", 40, options)
		if err != nil || !found {
			t.Fatalf("tail: found=%v err=%v", found, err)
		}
		reply, err := hub.Invoke("chat:archive-load", []any{"encoded-tab", mergeOptions(wireOptions, map[string]any{"tail": 40})})
		if err != nil {
			t.Fatal(err)
		}
		assertSameEncoding(t, "tail", reply, tail)
		// With every image inline, the 8 MiB budget keeps only the newest rows.
		if !options.deferMessageImages && len(tail) >= 40 {
			t.Fatalf("inline images did not bound the page: %d rows", len(tail))
		}

		page, found, err := runtime.ProjectArchivePageBeforeByTab("encoded-tab", "reply-19", 30, options)
		if err != nil || !found || len(page) == 0 {
			t.Fatalf("page: rows=%d found=%v err=%v", len(page), found, err)
		}
		reply, err = hub.Invoke("chat:archive-load", []any{"encoded-tab", mergeOptions(wireOptions, map[string]any{"beforeMessageId": "reply-19", "limit": 30})})
		if err != nil {
			t.Fatal(err)
		}
		assertSameEncoding(t, "page", reply, page)
	}

	// session:get embeds the same row encodings in its root.
	root, err := runtime.ProjectSession(archiveReadOptions{deferMessageImages: true, leanTools: true})
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := runtime.ProjectSessionRaw(archiveReadOptions{deferMessageImages: true, leanTools: true})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("session:get bytes changed: %d vs %d bytes", len(got), len(want))
	}
}

func mergeOptions(base, extra map[string]any) map[string]any {
	merged := map[string]any{}
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range extra {
		merged[key] = value
	}
	return merged
}

func assertSameEncoding(t *testing.T, label string, reply any, rows []any) {
	t.Helper()
	raw, ok := reply.(wire.EncodedResult)
	if !ok {
		t.Fatalf("%s reply is not pre-encoded: %T", label, reply)
	}
	want, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(want) {
		t.Fatalf("%s reply differs from json.Marshal of its rows (%d vs %d bytes)", label, len(raw), len(want))
	}
}
