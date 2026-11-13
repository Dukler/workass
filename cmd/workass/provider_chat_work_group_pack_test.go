package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"testing"
	"time"
)

type workGroupTestPage struct {
	Etag      string            `json:"etag"`
	Unchanged bool              `json:"unchanged"`
	Events    []json.RawMessage `json:"events"`
	MessageID string            `json:"messageId"`
	Next      int               `json:"next"`
	Offset    int               `json:"offset"`
	Total     int               `json:"total"`
}

func readWorkGroupPage(t *testing.T, runtime *providerChatRuntime, request workGroupRequest) workGroupTestPage {
	t.Helper()
	request.messageID = "wg-message-7"
	options := archiveReadOptions{deferMessageImages: true, leanTools: true, leanTitles: true, toolSummaries: true}
	reply, found, err := runtime.ProjectWorkGroupByTab("tab-wg", request, options)
	if err != nil || !found {
		t.Fatalf("work group page %+v: %v %v", request, found, err)
	}
	var page workGroupTestPage
	if err := json.Unmarshal(mustJSONBytes(t, reply), &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func readWorkGroupAll(t *testing.T, runtime *providerChatRuntime, compact bool) []json.RawMessage {
	t.Helper()
	var events []json.RawMessage
	for offset := 0; ; {
		page := readWorkGroupPage(t, runtime, workGroupRequest{offset: offset, compact: compact})
		if page.Offset != offset || page.Etag == "" {
			t.Fatalf("page %+v", page)
		}
		events = append(events, page.Events...)
		if page.Next >= page.Total {
			if len(events) != page.Total {
				t.Fatalf("read %d of %d", len(events), page.Total)
			}
			return events
		}
		if page.Next <= offset {
			t.Fatalf("page at %d did not advance", offset)
		}
		offset = page.Next
	}
}

// expandWorkGroupEvent mirrors desktop/renderer2 chat/work-group.ts.
func expandWorkGroupEvent(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	var event map[string]any
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatal(err)
	}
	if event["kind"] == "tool" {
		for _, field := range workGroupCompactNulls {
			if _, present := event[field]; !present {
				event[field] = nil
			}
		}
		if _, present := event["key"]; !present {
			event["key"] = "timeline:tool:" + fieldString(event, "id")
		}
		if event["titleIs"] == "command" {
			event["title"] = event["command"]
			delete(event, "titleIs")
		}
	}
	return mustJSONBytes(t, event)
}

func TestWorkGroupPacksPageFromDiskByWindowAndRebuildWhenStale(t *testing.T) {
	stateDir := t.TempDir()
	backend, err := openChatActorBackend(stateDir, true, "test", t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.close()
	workGroupTestChat(t, stateDir, backend, "wg")
	defer func(chunk, later, first int) {
		workGroupChunkBytes, workGroupLaterChunkBytes, workGroupFirstRange = chunk, later, first
	}(workGroupChunkBytes, workGroupLaterChunkBytes, workGroupFirstRange)
	workGroupChunkBytes, workGroupLaterChunkBytes, workGroupFirstRange = 700, 1500, 5
	runtime := startLazyTestRuntime(t, stateDir, backend, "tab-none")
	defer runtime.Close(context.Background())
	defer workGroupPackStores.Delete(stateDir)

	all := readWorkGroupAll(t, runtime, false)
	want := mustJSONBytes(t, all)
	if len(all) != 20 {
		t.Fatalf("%d events", len(all))
	}

	// A page with a limit holds whole chunks from its offset; one starting
	// inside a chunk holds only the calls it asks for.
	first := readWorkGroupPage(t, runtime, workGroupRequest{limit: 1})
	if first.Next <= 0 || first.Next >= first.Total || !bytes.Equal(mustJSONBytes(t, first.Events), mustJSONBytes(t, all[:first.Next])) {
		t.Fatalf("limit page next=%d of %d", first.Next, first.Total)
	}
	inner := readWorkGroupPage(t, runtime, workGroupRequest{offset: 3, limit: 1})
	if inner.Next <= 3 || !bytes.Equal(mustJSONBytes(t, inner.Events), mustJSONBytes(t, all[3:inner.Next])) {
		t.Fatalf("inner page next=%d", inner.Next)
	}
	// The same page asked with its etag is a receipt.
	again := readWorkGroupPage(t, runtime, workGroupRequest{limit: 1, ifNoneMatch: first.Etag})
	if !again.Unchanged || again.Etag != first.Etag || len(again.Events) != 0 {
		t.Fatalf("conditional page %+v", again)
	}

	// A compact page restores to exactly the same events.
	compact := readWorkGroupAll(t, runtime, true)
	if bytes.Equal(mustJSONBytes(t, compact), want) {
		t.Fatal("compact pages are not compact")
	}
	for index, raw := range compact {
		got := expandWorkGroupEvent(t, raw)
		var plain any
		_ = json.Unmarshal(all[index], &plain)
		if !bytes.Equal(got, mustJSONBytes(t, plain)) {
			t.Fatalf("compact event %d restores to\n%s\nnot\n%s", index, got, all[index])
		}
	}

	// A later process reads the pack from disk.
	store := runtime.workGroupPacks()
	path := store.packPath(workGroupPackKey{chatID: "wg", messageID: "wg-message-7", shape: "d1l1t1s1c0"})
	// The pack is written after its last page is published.
	var statErr error
	for wait := 0; wait < 400; wait++ {
		if _, statErr = os.Stat(path); statErr == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if statErr != nil {
		t.Fatalf("pack not on disk: %v", statErr)
	}
	workGroupPackStores.Delete(stateDir)
	if got := mustJSONBytes(t, readWorkGroupAll(t, runtime, false)); !bytes.Equal(got, want) {
		t.Fatal("a pack read from disk differs")
	}

	// A pack whose stamp is not the stored row's is built again.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	headerLen := binary.LittleEndian.Uint32(data[4:8])
	var header workGroupPackHeader
	if err := json.Unmarshal(data[8:8+headerLen], &header); err != nil {
		t.Fatal(err)
	}
	header.CRC, header.Revision = header.CRC+1, 0
	tampered, _ := json.Marshal(header)
	var out bytes.Buffer
	out.WriteString(workGroupPackMagic)
	_ = binary.Write(&out, binary.LittleEndian, uint32(len(tampered)))
	out.Write(tampered)
	out.Write(data[8+headerLen:])
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	workGroupPackStores.Delete(stateDir)
	if got := mustJSONBytes(t, readWorkGroupAll(t, runtime, false)); !bytes.Equal(got, want) {
		t.Fatal("a stale pack was read")
	}
	// The rebuilt pack is written back after its last page is published.
	var pack *workGroupPack
	for wait := 0; wait < 200; wait++ {
		if pack, _ = runtime.workGroupPacks().load(workGroupPackKey{chatID: "wg", messageID: "wg-message-7", shape: "d1l1t1s1c0"}); pack != nil && pack.crc == header.CRC-1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pack == nil || pack.crc != header.CRC-1 {
		t.Fatal("the rebuilt pack was not written back")
	}

	// Deleting the chat's packs leaves nothing on disk.
	runtime.workGroupPacks().forgetChat("wg")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("pack left behind: %v", err)
	}
}

// A large history page is kept per chat revision and read back, in this
// process or a later one, as exactly the bytes a read makes; a commit makes
// it read again.
func TestLargeHistoryPagesAreKeptPerRevision(t *testing.T) {
	stateDir := t.TempDir()
	backend, err := openChatActorBackend(stateDir, true, "test", t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.close()
	workGroupTestChat(t, stateDir, backend, "wg")
	defer func(min int) { pageMemoMinBytes = min }(pageMemoMinBytes)
	pageMemoMinBytes = 1
	runtime := startLazyTestRuntime(t, stateDir, backend, "tab-none")
	defer runtime.Close(context.Background())
	defer workGroupPackStores.Delete(stateDir)
	options := archiveReadOptions{deferMessageImages: true, leanTools: true, leanTitles: true}
	want, found, err := runtime.ProjectArchivePageBeforeByTabRaw("tab-wg", "wg-message-9", 4, options)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	want = append([]byte(nil), want...)
	reply := func() []byte {
		got, found, err := runtime.ArchivePageBeforeReply("tab-wg", "wg-message-9", 4, options)
		if err != nil || !found {
			t.Fatal(found, err)
		}
		return mustJSONBytes(t, got)
	}
	if got := reply(); !bytes.Equal(got, want) {
		t.Fatal("first read differs")
	}
	key := pageMemoKey{chatID: "wg", before: "wg-message-9", limit: 4, opts: options}
	revision, _ := runtime.chatRevision("wg")
	var kept bool
	for wait := 0; wait < 200 && !kept; wait++ {
		_, kept = runtime.workGroupPacks().keptPage(key, revision)
		time.Sleep(5 * time.Millisecond)
	}
	if !kept {
		t.Fatal("the page was not kept")
	}
	if got := reply(); !bytes.Equal(got, want) {
		t.Fatal("kept page differs")
	}
	workGroupPackStores.Delete(stateDir)
	if _, ok := runtime.workGroupPacks().keptPage(key, revision); !ok {
		t.Fatal("the page was not kept on disk")
	}
	if got := reply(); !bytes.Equal(got, want) {
		t.Fatal("page from disk differs")
	}
	if _, ok := runtime.workGroupPacks().keptPage(key, revision+1); ok {
		t.Fatal("a page was kept across a revision")
	}
}
