package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

// screenshotHistory is a chat whose every row carries tool screenshots (and,
// when ownImages, screenshots of its own), all naming one ~1 MiB sidecar.
func screenshotHistory(t *testing.T, stateDir, ref string, rows, imagesPerRow int, ownImages bool) *providerChatRuntime {
	t.Helper()
	engine, err := chat.NewEngine("screenshot-chat")
	if err != nil {
		t.Fatal(err)
	}
	events := make([]chat.LedgerEvent, rows)
	for index := range events {
		timeline := make([]chat.TimelineEntry, imagesPerRow)
		for tool := range timeline {
			timeline[tool] = chat.TimelineEntry{Key: fmt.Sprintf("tool-%d-%d", index, tool), Kind: providercontract.EventToolUpdate,
				Tool: &providercontract.ToolEvent{ToolCallID: fmt.Sprintf("call-%d-%d", index, tool), Title: "Screenshot",
					Attachments: []providercontract.Attachment{{ID: fmt.Sprintf("shot-%d-%d", index, tool), MIMEType: "image/png", Ref: providerSessionImageRefPrefix + ref}}}}
		}
		events[index] = chat.LedgerEvent{
			EventID: fmt.Sprintf("event-%02d", index), MessageID: fmt.Sprintf("message-%02d", index),
			OperationID: "fixture", Role: "assistant", Text: "looked at the page", Status: "done", Timeline: timeline,
		}
		if ownImages {
			for image := 0; image < imagesPerRow; image++ {
				events[index].Attachments = append(events[index].Attachments, providercontract.Attachment{
					ID: fmt.Sprintf("own-%d-%d", index, image), MIMEType: "image/png", Ref: providerSessionImageRefPrefix + ref,
				})
			}
		}
	}
	if err := engine.Apply(chat.InitializeFork{
		Presentation: chat.PresentationState{TabID: "screenshot-tab", Title: "Screenshots"},
		SourceChatID: "source", OperationID: "create-screenshots", Digest: "fixture", Messages: events,
	}); err != nil {
		t.Fatal(err)
	}
	return &providerChatRuntime{manager: &acp.Manager{}, sessions: sharedSessionStore(stateDir), stateDir: stateDir,
		actors: map[string]*providerChatActor{"screenshot-chat": {engine: engine}}, known: map[string]struct{}{"screenshot-chat": {}}}
}

// The page budget counted every tool screenshot at its full sidecar size,
// although archive pages send tool media by reference. A 225 MB chat with
// 29,000 tool screenshots (17 GB counted) came back one row per page, so
// reaching its first message took a round trip per message.
func TestArchivePagesCountDeferredToolMediaAsReferences(t *testing.T) {
	stateDir, ref, _ := imageReadFixture(t)
	// Each row names ten ~1 MiB screenshots: over the 8 MiB page budget alone.
	runtime := screenshotHistory(t, stateDir, ref, 40, 10, false)
	recent, found, err := runtime.ProjectRecentArchiveByTab("screenshot-tab", 20, archiveReadOptions{deferMessageImages: true, leanTools: true})
	if err != nil || !found || len(recent) != 20 {
		t.Fatalf("recent page = rows:%d found:%v err:%v, want 20 rows", len(recent), found, err)
	}
	older, found, err := runtime.ProjectArchivePageBeforeByTab("screenshot-tab", fieldString(mapFromAnyMain(recent[0]), "id"), 20, archiveReadOptions{deferMessageImages: true, leanTools: true})
	if err != nil || !found || len(older) != 20 {
		t.Fatalf("older page = rows:%d found:%v err:%v, want 20 rows", len(older), found, err)
	}
	for _, page := range [][]any{recent, older} {
		encoded, err := json.Marshal(page)
		if err != nil || len(encoded) >= actorHistoryPageBytes {
			t.Fatalf("page carries %d bytes (err=%v), over its budget", len(encoded), err)
		}
		for _, row := range page {
			for _, rawEvent := range anySlice(mapFromAnyMain(row)["events"]) {
				for _, rawImage := range anySlice(mapFromAnyMain(rawEvent)["images"]) {
					image := mapFromAnyMain(rawImage)
					if fieldString(image, "deferredImageRef") != ref || fieldString(image, "data") != "" {
						t.Fatalf("tool image was not sent by reference: %#v", image)
					}
				}
			}
		}
	}
	// The session projection's tail is bounded the same way.
	projection, err := runtime.ProjectSession(archiveReadOptions{deferMessageImages: true, leanTools: true})
	if err != nil {
		t.Fatal(err)
	}
	if rows := anySlice(mapFromAnyMain(anySlice(projection["chats"])[0])["messages"]); len(rows) < 20 {
		t.Fatalf("session tail kept %d rows of a reference-only history", len(rows))
	}
}

// A message's own images count when the reply inlines them, and do not when
// the reader asked for them by reference.
func TestArchivePagesCountMessageImagesOnlyWhenInlined(t *testing.T) {
	stateDir, ref, data := imageReadFixture(t)
	runtime := screenshotHistory(t, stateDir, ref, 12, 10, true)
	deferred, _, err := runtime.ProjectRecentArchiveByTab("screenshot-tab", 12, archiveReadOptions{deferMessageImages: true})
	if err != nil || len(deferred) != 12 {
		t.Fatalf("deferred page = rows:%d err:%v, want 12", len(deferred), err)
	}
	inline, _, err := runtime.ProjectRecentArchiveByTab("screenshot-tab", 12)
	if err != nil || len(inline) != 1 {
		t.Fatalf("inline page = rows:%d err:%v, want the one row its bytes allow", len(inline), err)
	}
	for _, image := range anySlice(mapFromAnyMain(inline[0])["images"]) {
		if mapFromAnyMain(image)["data"] != data {
			t.Fatal("an inline reply lost its image bytes")
		}
	}
}

// A deferred image is never read by the page that names it, so a sidecar
// that is missing on disk must not make the whole history page fail.
func TestArchivePageSurvivesAMissingDeferredSidecar(t *testing.T) {
	stateDir, _, _ := imageReadFixture(t)
	missing := sessionImageDirname + "/" + strings.Repeat("0", 64)
	runtime := screenshotHistory(t, stateDir, missing, 3, 2, false)
	page, found, err := runtime.ProjectRecentArchiveByTab("screenshot-tab", 3, archiveReadOptions{deferMessageImages: true, leanTools: true})
	if err != nil || !found || len(page) != 3 {
		t.Fatalf("page with a missing deferred sidecar = rows:%d found:%v err:%v", len(page), found, err)
	}
}

// Image reads check ownership in place instead of cloning the actor.
func TestToolImageReadChecksOwnershipWithoutCopyingTheChat(t *testing.T) {
	stateDir, ref, data := imageReadFixture(t)
	runtime := screenshotHistory(t, stateDir, ref, 2, 1, false)
	image, found, err := runtime.ProjectToolImageByTab("screenshot-tab", ref)
	if err != nil || !found || fieldString(mapFromAnyMain(image[0]), "data") != data {
		t.Fatalf("owned image read: found=%v err=%v", found, err)
	}
	if _, _, err := runtime.ProjectToolImageByTab("screenshot-tab", sessionImageDirname+"/"+strings.Repeat("1", 64)); err == nil {
		t.Fatal("an image this chat does not own was read")
	}
}
