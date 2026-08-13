package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"workass/internal/chat"
)

// TestActorFileStoreRetainsContentAddressedProviderAttachmentAcrossRestart
// covers the canonical attachment contract end to end. Image bytes are
// accepted by the session boundary, the actor persists only an
// immutable content reference, and a restarted actor resolves that reference
// through the daemon-owned sidecar without embedding provider payload bytes in
// chat state.
func TestActorFileStoreRetainsContentAddressedProviderAttachmentAcrossRestart(t *testing.T) {
	stateDir := t.TempDir()
	sessions := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	imageData := base64.StdEncoding.EncodeToString([]byte("actor-owned-image"))
	attachments, err := sessions.PersistProviderAttachments([]any{map[string]any{
		"mimeType": "image/png", "name": "proof.png", "data": imageData,
	}})
	if err != nil {
		t.Fatalf("persist provider attachment: %v", err)
	}
	if len(attachments) != 1 || attachments[0].Ref == "" || attachments[0].Digest == "" {
		t.Fatalf("provider attachment receipt = %#v", attachments)
	}

	chatID := "actor-image-chat"
	actorPath := providerChatStatePath(stateDir, chatID)
	engine, err := chat.NewDurableEngine(chatID, chat.FileStore{Path: actorPath})
	if err != nil {
		t.Fatalf("create durable actor: %v", err)
	}
	if err := engine.Apply(chat.InitializeFork{
		Presentation: chat.PresentationState{TabID: "actor-image-tab", Title: "Images"},
		SourceChatID: "actor-image-source", OperationID: "actor-image-create", Digest: "actor-image-create-digest",
		Messages: []chat.LedgerEvent{{
			EventID: "source-image-user", MessageID: "image-user", OperationID: "image-op",
			Role: "user", Text: "inspect this", Status: "done", Attachments: attachments,
		}},
	}); err != nil {
		t.Fatalf("initialize attachment actor: %v", err)
	}

	actorBytes, err := os.ReadFile(actorPath)
	if err != nil {
		t.Fatalf("read actor state: %v", err)
	}
	if bytes.Contains(actorBytes, []byte(imageData)) {
		t.Fatal("actor state embedded provider attachment bytes")
	}
	if !bytes.Contains(actorBytes, []byte(attachments[0].Ref)) {
		t.Fatalf("actor state omitted durable attachment ref %q", attachments[0].Ref)
	}

	restarted, err := chat.NewDurableEngine(chatID, chat.FileStore{Path: actorPath})
	if err != nil {
		t.Fatalf("restart durable actor: %v", err)
	}
	state := restarted.Snapshot()
	if len(state.Ledger) != 1 || len(state.Ledger[0].Attachments) != 1 {
		t.Fatalf("restarted actor attachments = %#v", state.Ledger)
	}
	gotAttachment := state.Ledger[0].Attachments[0]
	if gotAttachment.Ref != attachments[0].Ref || gotAttachment.Digest != attachments[0].Digest {
		t.Fatalf("restarted actor changed attachment identity: got=%#v want=%#v", gotAttachment, attachments[0])
	}
	resolved, err := sessions.ResolveProviderAttachment(context.Background(), gotAttachment)
	if err != nil {
		t.Fatalf("resolve restarted actor attachment: %v", err)
	}
	resolvedImage, ok := resolved.(map[string]any)
	if !ok || resolvedImage["data"] != imageData || resolvedImage["mimeType"] != "image/png" || resolvedImage["name"] != "proof.png" {
		t.Fatalf("resolved actor attachment = %#v", resolved)
	}
}
