//go:build review

// Independent-review reproduction (b8b18322..fe7003e5). Run with:
//   go test -tags review -count=1 -run TestReview ./cmd/workass/

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/httpserve"
	"workass/internal/wire"
)

// Review: a pending steer's own image, sent by reference (deferImages, as the
// renderer now asks on every history read and on session:get), must be
// readable back through {imageRef}. ProjectToolImageByTab only admits refs it
// finds in the ledger or the foreground turn, not in a pending steer.
func TestReviewPendingSteerDeferredImageIsReadable(t *testing.T) {
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
	const tabID, chatID = "review-steer-image-tab", "review-steer-image-chat"
	pngData := func(shade uint8) string {
		img := image.NewRGBA(image.Rect(0, 0, 3, 2))
		img.Set(0, 0, color.RGBA{R: shade, A: 255})
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatal(err)
		}
		return base64.StdEncoding.EncodeToString(buf.Bytes())
	}
	first := map[string]any{"mimeType": "image/png", "data": pngData(10), "name": "first.png"}
	steered := map[string]any{"mimeType": "image/png", "data": pngData(200), "name": "steered.png"}
	createWireActorChat(t, client, 100, tabID, chatID, "pi")
	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": tabID, "chatId": chatID, "providerId": "pi"})
	sessionReply := client.waitReply(t, 1, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("Pi session: %s", *sessionReply.Error)
	}
	sessionID := fieldString(mapFromAnyMain(sessionReply.Result), "sessionId")
	client.invoke(t, 2, "job:start", map[string]any{
		"kind": "app-chat", "tabId": tabID, "chatId": chatID, "sessionId": sessionID,
		"providerId": "pi", "prompt": "[fixture:wait-file] inspect initial image", "images": []any{first},
		"operationId": "review-image-turn", "userMessageId": "review-image-user", "assistantMessageId": "review-image-assistant",
	})
	start := client.waitReply(t, 2, 5*time.Second)
	if start.Error != nil {
		t.Fatalf("start: %s", *start.Error)
	}
	jobID := fieldString(mapFromAnyMain(start.Result), "id")
	startEvent := waitProviderAttachedJobStartEvent(t, client, jobID, "", 5*time.Second)
	sessionID = fieldString(mapFromAnyMain(startEvent["job"]), "sessionId")
	client.waitJobEvent(t, jobID, "acp", 5*time.Second)
	client.invoke(t, 3, "app-chat:steer", map[string]any{
		"sessionId": sessionID, "prompt": "inspect steered image", "images": []any{steered},
		"clientUserMessageId": "review-image-steer", "continuationAssistantMessageId": "review-image-continuation",
		"boundary": map[string]any{"assistantMessageId": "review-image-assistant", "deferUntilConsumed": true},
	})
	if steer := client.waitReply(t, 3, 5*time.Second); steer.Error != nil {
		t.Fatalf("steer: %s", *steer.Error)
	}
	// The renderer's recent read: {tail, deferImages, leanTools}.
	client.invoke(t, 4, "chat:archive-load", tabID, map[string]any{"tail": 10, "deferImages": true, "leanTools": true})
	recent := client.waitReply(t, 4, 5*time.Second)
	if recent.Error != nil {
		t.Fatalf("recent read: %s", *recent.Error)
	}
	refs := map[string]string{}
	for _, raw := range anySlice(recent.Result) {
		row := mapFromAnyMain(raw)
		for _, rawImage := range anySlice(row["images"]) {
			refs[fieldString(row, "id")] = fieldString(mapFromAnyMain(rawImage), "deferredImageRef")
		}
	}
	steerRef := refs["review-image-steer"]
	if steerRef == "" || refs["review-image-user"] == steerRef {
		t.Fatalf("pending steer image was not deferred under its own ref: %#v", refs)
	}
	id := 10
	for rowID, ref := range refs {
		id++
		client.invoke(t, id, "chat:archive-load", tabID, map[string]any{"imageRef": ref})
		reply := client.waitReply(t, id, 5*time.Second)
		if reply.Error != nil {
			t.Errorf("%s deferred image cannot be read back: %s", rowID, *reply.Error)
			continue
		}
		t.Logf("%s deferred image read back ok", rowID)
	}
	if err := os.WriteFile(filepath.Join(stateDir, sessionID+".release"), []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	client.waitJobEvent(t, jobID, "end", 5*time.Second)
}
