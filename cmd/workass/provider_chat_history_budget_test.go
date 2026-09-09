package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

func TestActorHistoryBudgetCountsRepeatedImageOccurrencesBeforeHydration(t *testing.T) {
	root, ref, data := imageReadFixture(t)
	rows := []any{
		map[string]any{"id": "older", "images": repeatedImageProjection(ref)},
		map[string]any{"id": "newest", "images": []any{map[string]any{sessionImageDataRefField: ref}}},
	}
	page, err := boundedActorHistory(rows, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || fieldString(mapFromAnyMain(page[0]), "id") != "newest" {
		t.Fatal("expanded repeated image bytes did not bound the contiguous suffix")
	}
	// Bounding is metadata-only: no selected or omitted image has been hydrated.
	if _, ok := mapFromAnyMain(anySlice(mapFromAnyMain(page[0])["images"])[0])["data"]; ok {
		t.Fatal("budget pass read an image payload")
	}
	if err := rehydrateExternalSessionImages(page, root); err != nil {
		t.Fatal(err)
	}
	if mapFromAnyMain(anySlice(mapFromAnyMain(page[0])["images"])[0])["data"] != data {
		t.Fatal("selected image changed")
	}
	// A large, indivisible message must be returned when it is the next page,
	// rather than skipped forever or silently stripped of its images.
	page, err = boundedActorHistory(rows[:1], root)
	if err != nil || len(page) != 1 {
		t.Fatalf("large next row was skipped: rows=%d err=%v", len(page), err)
	}
	if err := rehydrateExternalSessionImages(page, root); err != nil {
		t.Fatal(err)
	}
	for _, image := range anySlice(mapFromAnyMain(page[0])["images"]) {
		if mapFromAnyMain(image)["data"] != data {
			t.Fatal("large row lost an image occurrence")
		}
	}
}

func TestImageRichHistoryStartupAndPagingPreserveEveryActorRow(t *testing.T) {
	stateDir, ref, data := imageReadFixture(t)
	engine, err := chat.NewEngine("image-budget-chat")
	if err != nil {
		t.Fatal(err)
	}
	// Sixty rows expand past the 256 MiB frozen transport ceiling, despite
	// sharing just one small immutable sidecar on disk. No giant buffer is
	// needed to construct the fixture or to exercise the corrected paging.
	events := make([]chat.LedgerEvent, 60)
	for index := range events {
		events[index] = chat.LedgerEvent{
			EventID: fmt.Sprintf("event-%02d", index), MessageID: fmt.Sprintf("message-%02d", index),
			OperationID: "fixture-input", Role: "assistant", Text: "mock image history", Status: "done",
		}
		for image := 0; image < 5; image++ {
			events[index].Attachments = append(events[index].Attachments, providercontract.Attachment{
				ID: fmt.Sprintf("image-%d", image), MIMEType: "image/png", Ref: providerSessionImageRefPrefix + ref,
			})
		}
	}
	if err := engine.Apply(chat.InitializeFork{
		Presentation: chat.PresentationState{TabID: "image-budget-tab", Title: "Image history"},
		SourceChatID: "mock-source", OperationID: "image-budget-create", Digest: "fixture", Messages: events,
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &providerChatRuntime{
		manager: &acp.Manager{}, sessions: sharedSessionStore(stateDir), stateDir: stateDir,
		actors: map[string]*providerChatActor{"image-budget-chat": {engine: engine}},
		known:  map[string]struct{}{"image-budget-chat": {}},
	}
	before := engine.Snapshot()
	projection, err := runtime.ProjectSession()
	if err != nil {
		t.Fatal(err)
	}
	projected := mapFromAnyMain(anySlice(projection["chats"])[0])
	if intValue(projected["messageCount"]) != 60 || projected["historyComplete"] != false {
		t.Fatal("startup lost the canonical history count")
	}
	encoded, err := json.Marshal(projection)
	if err != nil || len(encoded) > actorHistoryPageBytes {
		t.Fatalf("startup projection exceeded its image budget: bytes=%d err=%v", len(encoded), err)
	}
	page, found, err := runtime.ProjectRecentArchiveByTab("image-budget-tab", 60)
	if err != nil || !found || len(page) != 1 {
		t.Fatalf("recent page = rows:%d found:%v err:%v", len(page), found, err)
	}
	seen := make(map[string]bool)
	for len(page) > 0 {
		for _, raw := range page {
			row := mapFromAnyMain(raw)
			id := fieldString(row, "id")
			if seen[id] {
				t.Fatal("paging duplicated an actor row")
			}
			seen[id] = true
			images := anySlice(row["images"])
			if len(images) != 5 {
				t.Fatal("paging removed image occurrences")
			}
			for _, image := range images {
				if mapFromAnyMain(image)["data"] != data {
					t.Fatal("paging changed image bytes")
				}
			}
		}
		page, found, err = runtime.ProjectArchivePageBeforeByTab("image-budget-tab", fieldString(mapFromAnyMain(page[0]), "id"), 40)
		if err != nil || !found {
			t.Fatalf("older page: found=%v err=%v", found, err)
		}
	}
	if len(seen) != 60 || !reflect.DeepEqual(before, engine.Snapshot()) {
		t.Fatal("paging omitted history or mutated the authoritative actor")
	}
}

func TestActorHistoryBudgetKeepsUncommittedForegroundOwnersTogether(t *testing.T) {
	root, ref, _ := imageReadFixture(t)
	rows := []any{
		map[string]any{"id": "old", "status": "done", "content": "old history"},
		map[string]any{"id": "input", "status": "pending", "images": repeatedImageProjection(ref)},
		map[string]any{"id": "output", "status": "running", "content": "streaming"},
	}
	page, err := boundedActorHistory(rows, root)
	if err != nil || len(page) != 2 || fieldString(mapFromAnyMain(page[0]), "id") != "input" {
		t.Fatalf("uncommitted owner was dropped: rows=%d err=%v", len(page), err)
	}
}
