package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/httpserve"
	"workass/internal/wire"
)

// A just-sent image is readable as soon as a client can see it. The first
// send of a chat waits in the actor's Queue while its lane is created (a
// resumed Claude session: 2.5 s), yet the job:start receipt already names its
// image (userImages) and the archive tail already draws it as the running
// turn's user row. 2026-11-12, tab-1794240790756-85: the phone read the image
// 68 ms after job:start and got "archive image does not belong to this chat";
// the desktop's URL read and its socket fallback failed the same way, and
// neither client read it again.
func TestJustSentImageIsReadableBeforeItsLaneAttaches(t *testing.T) {
	root, stateDir, renderer := repoRoot(t), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><body></body>"), 0o644); err != nil {
		t.Fatal(err)
	}
	hub := wire.NewHub()
	sessionState := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir,
		Provider: acp.ProviderConfig{ID: "pi", Command: "node", Args: []string{filepath.Join(root, "scripts", "pi-native-host.mjs")}, CWD: root, Enabled: true,
			Env: map[string]string{"WORKASS_PI_SDK_MODULE": filepath.Join(root, "desktop", "acp", "mock-pi-sdk.mjs"), "WORKASS_PI_FIXTURE_DIR": stateDir}},
		DefaultProviderID: "pi", Broadcast: daemonEventBroadcaster(sessionState, hub.Broadcast),
	})
	providerChats := newProviderChatRuntime(manager, sessionState, stateDir, hub.Broadcast)
	t.Cleanup(func() { _ = providerChats.Close(context.Background()); manager.Reset() })
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})
	client := dialTestWSHandler(t, httpserve.New(renderer, hub, nil), "/")
	defer client.conn.Close()
	images := newChatImageHTTPHandler(providerChats)
	const tabID, chatID = "just-sent-tab", "just-sent-chat"
	// Wider than a phone thumbnail, so the {width} read makes a rendition.
	png := testPNG(t, 1000, 600)
	createWireActorChat(t, client, 1, tabID, chatID, "pi")

	// The phone's send: the job carries the image's bytes.
	client.invoke(t, 2, "job:start", map[string]any{
		"kind": "app-chat", "tabId": tabID, "chatId": chatID, "providerId": "pi", "prompt": "look",
		"images":      []any{map[string]any{"mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(png), "name": "pegada.png"}},
		"operationId": "u-just-sent", "userMessageId": "u-just-sent", "assistantMessageId": "a-just-sent", "history": []any{},
	})
	reply := client.waitReply(t, 2, 10*time.Second)
	if reply.Error != nil {
		t.Fatalf("job:start: %s", *reply.Error)
	}
	receipt := mapFromAnyMain(reply.Result)
	userImages := anySlice(receipt["userImages"])
	if len(userImages) != 1 {
		t.Fatalf("receipt userImages = %#v, want the sent image by reference", receipt["userImages"])
	}
	ref := fieldString(mapFromAnyMain(userImages[0]), "deferredImageRef")
	actor, _, err := providerChats.actorByTab(tabID)
	if err != nil {
		t.Fatal(err)
	}
	if state := actor.engine.ReadSnapshot(); state.Foreground == nil && len(state.Queue) == 1 {
		t.Logf("reading while the send still waits for its lane")
	}

	// Desktop: GET /workass/chat-image/<tab>/<digest> (the key is the hub's to check).
	digest := strings.TrimPrefix(ref, sessionImageDirname+"/")
	recorder := httptest.NewRecorder()
	images.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://daemon.test"+httpserve.ChatImagePathPrefix+tabID+"/"+digest+"?k=key", nil))
	if recorder.Code != http.StatusOK || !bytes.Equal(recorder.Body.Bytes(), png) {
		t.Fatalf("desktop read of the just-sent image = %d (%d bytes), want 200 with the image", recorder.Code, recorder.Body.Len())
	}
	// Phone: chat:archive-load <tab> {imageRef}.
	client.invoke(t, 3, "chat:archive-load", tabID, map[string]any{"imageRef": ref})
	read := client.waitReply(t, 3, 5*time.Second)
	if read.Error != nil {
		t.Fatalf("phone read of the just-sent image: %s", *read.Error)
	}
	if data := fieldString(mapFromAnyMain(anySlice(read.Result)[0]), "data"); data != base64.StdEncoding.EncodeToString(png) {
		t.Fatalf("phone read returned %d base64 bytes, want the image", len(data))
	}
	// Phone thumbnail: {imageRef, width} answers a rendition (300 rounds up
	// to 320), as a data URL, from the cache the desktop's ?w= uses.
	client.invoke(t, 4, "chat:archive-load", tabID, map[string]any{"imageRef": ref, "width": 300})
	thumb := client.waitReply(t, 4, 5*time.Second)
	if thumb.Error != nil {
		t.Fatalf("phone thumbnail read: %s", *thumb.Error)
	}
	url := fieldString(mapFromAnyMain(anySlice(thumb.Result)[0]), "data")
	encoded, ok := strings.CutPrefix(url, "data:image/png;base64,")
	if !ok {
		t.Fatalf("phone thumbnail = %.40q, want a PNG data URL", url)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width != 320 || len(raw) >= len(png) {
		t.Fatalf("phone thumbnail = %dx%d, %d bytes (err %v); want 320 wide and smaller than the %d-byte image", config.Width, config.Height, len(raw), err, len(png))
	}
	client.waitJobEvent(t, fieldString(receipt, "id"), "end", 10*time.Second)
}
