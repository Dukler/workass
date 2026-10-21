package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
	"workass/internal/wire"
)

func sizedPNGBase64(t *testing.T, width, height int) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// writeSessionImageSidecar stores data the way the session image store does
// and returns its durable reference.
func writeSessionImageSidecar(t *testing.T, stateDir, data string) string {
	t.Helper()
	name := sessionImageName(data)
	if err := os.MkdirAll(filepath.Join(stateDir, sessionImageDirname), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, sessionImageDirname, name), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return sessionImageDirname + "/" + name
}

func TestAttachmentWithoutImageSizeEncodesByteIdentically(t *testing.T) {
	attachment := providercontract.Attachment{
		ID: "image-0123456789abcdef", Name: "shot.png", MIMEType: "image/png",
		Digest: "0123456789abcdef", Size: 12, Ref: "workass-session-image:images/0123456789abcdef",
	}
	raw, err := json.Marshal(attachment)
	if err != nil {
		t.Fatal(err)
	}
	// The pre-size golden: existing rows, SQLite message diffs and the file
	// journal must not churn.
	const golden = `{"ID":"image-0123456789abcdef","Name":"shot.png","MIMEType":"image/png","Digest":"0123456789abcdef","Size":12,"Source":"","Ref":"workass-session-image:images/0123456789abcdef"}`
	if string(raw) != golden {
		t.Fatalf("size-less attachment encoding changed:\n got %s\nwant %s", raw, golden)
	}
	attachment.Width, attachment.Height = 1440, 900
	sized, err := json.Marshal(attachment)
	if err != nil {
		t.Fatal(err)
	}
	if want := golden[:len(golden)-1] + `,"Width":1440,"Height":900}`; string(sized) != want {
		t.Fatalf("sized attachment = %s, want %s", sized, want)
	}
	var decoded providercontract.Attachment
	if err := json.Unmarshal([]byte(golden), &decoded); err != nil || decoded.Width != 0 || decoded.Height != 0 {
		t.Fatalf("older row decoded with a size: %#v, %v", decoded, err)
	}
}

func TestPlanProviderAttachmentsRecordsImageSizeOutsideIdentity(t *testing.T) {
	stateDir := t.TempDir()
	sessions := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	data := sizedPNGBase64(t, 40, 30)
	inline := []any{steerRegressionImage(data, "shot.png")}
	plan, err := sessions.PlanProviderAttachments(inline)
	if err != nil {
		t.Fatal(err)
	}
	planned := plan.Attachments[0]
	if planned.Width != 40 || planned.Height != 30 {
		t.Fatalf("planned attachment size = %d x %d, want 40 x 30", planned.Width, planned.Height)
	}
	// The steering identity recomputes the same attachment, size included.
	pure, err := steerAttachmentInputIdentity(inline)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pure, plan.Attachments) {
		t.Fatalf("steering identity differs from the planned attachment: pure=%#v planned=%#v", pure, plan.Attachments)
	}
	if err := plan.Materialize(); err != nil {
		t.Fatal(err)
	}
	// A ref-only retry without a store cannot read the header; the size never
	// takes part in the identity comparison.
	refImage := map[string]any{
		"mimeType": planned.MIMEType, "name": planned.Name,
		sessionImageDataRefField: sessionImageDirname + "/" + planned.Digest, "digest": planned.Digest, "size": planned.Size,
	}
	storeless, err := steerAttachmentInputIdentity([]any{refImage})
	if err != nil {
		t.Fatal(err)
	}
	if storeless[0].Width != 0 || !sameSteerAttachments(storeless, plan.Attachments) {
		t.Fatalf("storeless ref identity = %#v, want an unsized match", storeless)
	}
	withStore, err := steerAttachmentInputIdentity([]any{refImage}, sessions)
	if err != nil {
		t.Fatal(err)
	}
	if withStore[0].Width != 40 || withStore[0].Height != 30 || !sameSteerAttachments(withStore, plan.Attachments) {
		t.Fatalf("stored ref identity = %#v", withStore)
	}
	// An attachment recorded before sizes existed still matches.
	older := providercontract.WithoutImageSize(plan.Attachments)
	if !sameSteerAttachments(older, plan.Attachments) {
		t.Fatal("an unsized attachment no longer matches its sized replay")
	}
	// A planned attachment reusing an existing sidecar reads its header.
	replanned, err := sessions.PlanProviderAttachments([]any{refImage})
	if err != nil || replanned.Attachments[0].Width != 40 || replanned.Attachments[0].Height != 30 {
		t.Fatalf("ref plan = %#v, %v", replanned.Attachments, err)
	}
}

func TestPlanProviderAttachmentsLeavesUnreadableImagesUnsized(t *testing.T) {
	stateDir := t.TempDir()
	sessions := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	svg := base64.StdEncoding.EncodeToString([]byte(`<svg xmlns="http://www.w3.org/2000/svg" width="64" height="48"/>`))
	plan, err := sessions.PlanProviderAttachments([]any{
		map[string]any{"mimeType": "image/svg+xml", "name": "chart.svg", "data": svg},
		map[string]any{"mimeType": "image/png", "name": "damaged.png", "data": base64.StdEncoding.EncodeToString([]byte("not a png"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, attachment := range plan.Attachments {
		if attachment.Width != 0 || attachment.Height != 0 {
			t.Fatalf("%s gained a size: %d x %d", attachment.Name, attachment.Width, attachment.Height)
		}
	}
	images, err := projectionAttachments(plan.Attachments)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range images {
		image := mapFromAnyMain(raw)
		if _, ok := image["width"]; ok {
			t.Fatalf("unsized image projected a width: %#v", image)
		}
	}
}

func TestSessionImageSizeReadsDataURLSidecars(t *testing.T) {
	stateDir := t.TempDir()
	ref := writeSessionImageSidecar(t, stateDir, "data:image/png;base64,"+sizedPNGBase64(t, 7, 5))
	if width, height := sessionImageRefSize(ref, stateDir); width != 7 || height != 5 {
		t.Fatalf("data URL sidecar = %d x %d", width, height)
	}
	if width, height := sessionImageRefSize(sessionImageDirname+"/"+fmt.Sprintf("%064x", 0), stateDir); width != 0 || height != 0 {
		t.Fatalf("missing sidecar = %d x %d", width, height)
	}
}

func TestStagedQueueDigestIgnoresImageSize(t *testing.T) {
	attachment := providercontract.Attachment{ID: "image-0123456789abcdef", Name: "shot.png", MIMEType: "image/png", Digest: "0123456789abcdef", Size: 12, Ref: "ref"}
	unsized := []chat.StagedQueueEntry{{ID: "row", Text: "look", Attachments: []providercontract.Attachment{attachment}}, {ID: "plain", Text: "no images"}}
	// The receipt digest an older build recorded for this request.
	raw, err := json.Marshal(unsized)
	if err != nil {
		t.Fatal(err)
	}
	recorded := fmt.Sprintf("%x", sha256.Sum256(raw))
	sized := []chat.StagedQueueEntry{unsized[0], unsized[1]}
	sized[0].Attachments = []providercontract.Attachment{attachment}
	sized[0].Attachments[0].Width, sized[0].Attachments[0].Height = 1440, 900
	for name, entries := range map[string][]chat.StagedQueueEntry{"unsized": unsized, "sized": sized} {
		digest, err := stagedQueueRequestDigest(entries)
		if err != nil || digest != recorded {
			t.Fatalf("%s digest = %s, want the recorded %s (%v)", name, digest, recorded, err)
		}
	}
	if sized[0].Attachments[0].Width != 1440 {
		t.Fatal("digesting cleared the caller's attachment size")
	}
}

// Every archive form keeps width/height on deferred images: new rows carry the
// recorded size, older rows gain it from their sidecar header, and an
// unreadable image stays unsized.
func TestArchiveImagesKeepPixelSizeAfterDeferral(t *testing.T) {
	stateDir := t.TempDir()
	userRef := writeSessionImageSidecar(t, stateDir, sizedPNGBase64(t, 1440, 900))
	toolRef := writeSessionImageSidecar(t, stateDir, sizedPNGBase64(t, 800, 1100))
	brokenRef := writeSessionImageSidecar(t, stateDir, base64.StdEncoding.EncodeToString([]byte("<svg/>")))
	engine, err := chat.NewEngine("sized-image-chat")
	if err != nil {
		t.Fatal(err)
	}
	attach := func(id, ref string, width, height int) providercontract.Attachment {
		return providercontract.Attachment{ID: id, Name: id, MIMEType: "image/png", Ref: providerSessionImageRefPrefix + ref, Width: width, Height: height}
	}
	timeline := []chat.TimelineEntry{{Key: "tool-1", Kind: providercontract.EventToolUpdate,
		Tool: &providercontract.ToolEvent{ToolCallID: "call-1", Title: "Capture", Attachments: []providercontract.Attachment{attach("tool", toolRef, 0, 0)}}}}
	if err := engine.Apply(chat.InitializeFork{Presentation: chat.PresentationState{TabID: "sized-image-tab", Title: "Sizes"},
		SourceChatID: "source", OperationID: "create-sized-images", Digest: "fixture", Messages: []chat.LedgerEvent{
			{EventID: "ask", MessageID: "user-ask", OperationID: "turn", Role: "user", Text: "two screenshots", Status: "done",
				Attachments: []providercontract.Attachment{attach("shot", userRef, 1440, 900), attach("broken", brokenRef, 0, 0)}},
			{EventID: "reply", MessageID: "assistant-reply", OperationID: "turn", Role: "assistant", Text: "captured", Status: "done", Timeline: timeline},
			{EventID: "later", MessageID: "assistant-later", OperationID: "turn-2", Role: "assistant", Text: "later", Status: "done"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &providerChatRuntime{manager: &acp.Manager{}, sessions: sharedSessionStore(stateDir), stateDir: stateDir,
		actors: map[string]*providerChatActor{"sized-image-chat": {engine: engine}}, known: map[string]struct{}{"sized-image-chat": {}}}
	check := func(form string, rows []any) {
		t.Helper()
		byID := map[string]map[string]any{}
		for _, raw := range rows {
			row := mapFromAnyMain(raw)
			byID[fieldString(row, "id")] = row
		}
		images := anySlice(byID["user-ask"]["images"])
		if len(images) != 2 {
			t.Fatalf("%s: user images = %#v", form, byID["user-ask"]["images"])
		}
		shot, broken := mapFromAnyMain(images[0]), mapFromAnyMain(images[1])
		if fieldString(shot, "deferredImageRef") != userRef || intValue(shot["width"]) != 1440 || intValue(shot["height"]) != 900 {
			t.Fatalf("%s: recorded size lost in deferral: %#v", form, shot)
		}
		if _, ok := broken["width"]; ok || fieldString(broken, "deferredImageRef") != brokenRef {
			t.Fatalf("%s: unreadable image gained a size: %#v", form, broken)
		}
		event := mapFromAnyMain(anySlice(byID["assistant-reply"]["events"])[0])
		tool := mapFromAnyMain(anySlice(event["images"])[0])
		if fieldString(tool, "deferredImageRef") != toolRef || intValue(tool["width"]) != 800 || intValue(tool["height"]) != 1100 {
			t.Fatalf("%s: older tool image did not gain its sidecar size: %#v", form, tool)
		}
	}
	hub := wire.NewHub()
	registerArchiveHandlers(hub, nil, runtime)
	recent, err := hub.Invoke("chat:archive-load", []any{"sized-image-tab", map[string]any{"tail": 10, "deferImages": true}})
	if err != nil {
		t.Fatal(err)
	}
	check("recent", anySlice(recent))
	page, found, err := runtime.ProjectArchivePageBeforeByTab("sized-image-tab", "assistant-later", 10, archiveReadOptions{deferMessageImages: true})
	if err != nil || !found {
		t.Fatalf("page: found=%v err=%v", found, err)
	}
	check("page", page)
	full, found, err := runtime.projectArchiveByTab("sized-image-tab", actorHistoryFull, sessionProjectionMessageTail, archiveReadOptions{deferMessageImages: true})
	if err != nil || !found {
		t.Fatalf("full: found=%v err=%v", found, err)
	}
	check("full", full)
	root, err := runtime.ProjectSession(archiveReadOptions{deferMessageImages: true})
	if err != nil {
		t.Fatal(err)
	}
	chats := anySlice(root["chats"])
	if len(chats) != 1 {
		t.Fatalf("session:get chats = %d", len(chats))
	}
	check("session", anySlice(mapFromAnyMain(chats[0])["messages"]))
	// An inline (non-deferred) read keeps its bytes and gains the size too.
	inline, err := hub.Invoke("chat:archive-load", []any{"sized-image-tab", map[string]any{"tail": 10}})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range anySlice(inline) {
		row := mapFromAnyMain(raw)
		if fieldString(row, "id") != "user-ask" {
			continue
		}
		shot := mapFromAnyMain(anySlice(row["images"])[0])
		if fieldString(shot, "data") == "" || intValue(shot["width"]) != 1440 {
			t.Fatalf("inline read: %#v", shot)
		}
	}
	// The durable ledger was never rewritten by the projection pass.
	state := engine.Snapshot()
	if got := state.Ledger[1].Timeline[0].Tool.Attachments[0].Width; got != 0 {
		t.Fatalf("projection wrote a size into actor state: %d", got)
	}
}
