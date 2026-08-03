package acp

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAcceptedImageReachesGenericProviderWithExplicitAttachmentContextOnEveryTurn(t *testing.T) {
	manager, events := newFakeManager(t, "image-echo", Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tabID, chatID := "image-input-tab", "image-input-chat"
	session, err := manager.NewSession(ctx, SessionOptions{TabID: tabID, ChatID: chatID})
	if err != nil {
		t.Fatalf("new image-capable session: %v", err)
	}
	if !session.ImageSupport {
		t.Fatal("image-capable fixture was not surfaced to Workass")
	}

	image := []any{map[string]any{
		"mimeType": "image/png",
		"data":     "iVBORw0KGgo=",
		"name":     "attached-proof.png",
	}}
	for index, prompt := range []string{"first image turn", "resumed image turn"} {
		job, startErr := manager.StartJob(ctx, JobStartOptions{
			Kind: "app-chat", SessionID: session.SessionID, TabID: tabID, ChatID: chatID,
			ProviderID: session.ProviderID, Prompt: prompt, Images: image,
		})
		if startErr != nil {
			t.Fatalf("start image turn %d: %v", index+1, startErr)
		}
		end := events.waitJobEnd(t, jobID(job), 3*time.Second)
		assertJobStatus(t, end, "done", 0, "end_turn")
		result := asString(jobFromEnd(end)["result"])
		if result != "images=1 mime=image/png bytes=12 attachment-notice=true" {
			t.Fatalf("image turn %d provider observation = %q", index+1, result)
		}
	}
}

func TestAcceptedImageNeverSilentlyDegradesToTextForUnsupportedProvider(t *testing.T) {
	manager, events := newFakeManager(t, "echo-prompt", Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: "no-image-tab", ChatID: "no-image-chat"})
	if err != nil {
		t.Fatalf("new non-image session: %v", err)
	}
	job, err := manager.StartJob(ctx, JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, TabID: "no-image-tab", ChatID: "no-image-chat",
		ProviderID: session.ProviderID, Prompt: "must not become text only",
		Images: []any{map[string]any{"mimeType": "image/png", "data": "iVBORw0KGgo="}},
	})
	if err != nil {
		t.Fatalf("start unsupported image turn: %v", err)
	}
	end := events.waitJobEnd(t, jobID(job), 3*time.Second)
	assertJobStatus(t, end, "failed", 1, "")
	if errText := asString(jobFromEnd(end)["error"]); !strings.Contains(strings.ToLower(errText), "does not support image input") {
		t.Fatalf("unsupported image failure = %q", errText)
	}
}

func TestAttachedImageAndContextSurviveNativeSteeringForEveryFrontierProvider(t *testing.T) {
	for _, providerID := range []string{"codex", "claude"} {
		t.Run(providerID, func(t *testing.T) {
			mode := providerID + "-steer-image"
			manager, events := newFakeManager(t, mode, Options{
				RSSSampleInterval: time.Hour,
				Provider:          ProviderConfig{ID: providerID},
			})
			t.Cleanup(func() { manager.Reset() })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			tabID, chatID := providerID+"-image-steer-tab", providerID+"-image-steer-chat"
			session, err := manager.NewSession(ctx, SessionOptions{TabID: tabID, ChatID: chatID})
			if err != nil {
				t.Fatalf("new %s steer session: %v", providerID, err)
			}
			job, err := manager.StartJob(ctx, JobStartOptions{
				Kind: "app-chat", SessionID: session.SessionID, TabID: tabID, ChatID: chatID,
				ProviderID: providerID, Prompt: "base turn",
			})
			if err != nil {
				t.Fatalf("start %s steer turn: %v", providerID, err)
			}
			_ = events.waitJobType(t, jobID(job), "acp", 2*time.Second)
			result := manager.Steer(session.SessionID, "inspect this steer image", []any{
				map[string]any{"mimeType": "image/jpeg", "data": "/9j/2Q=="},
			}, "image-steer-receipt")
			if result["ok"] != true || result["live"] != true {
				t.Fatalf("%s image steer result = %#v", providerID, result)
			}
			end := events.waitJobEnd(t, jobID(job), 2*time.Second)
			assertJobStatus(t, end, "done", 0, "end_turn")
			observed := asString(jobFromEnd(end)["result"])
			if !strings.Contains(observed, "images=1 mime=image/jpeg bytes=8 attachment-notice=true") {
				t.Fatalf("%s provider image steer observation = %q", providerID, observed)
			}
		})
	}
}
