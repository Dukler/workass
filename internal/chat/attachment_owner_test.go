package chat

import (
	"testing"

	"workass/internal/provider"
)

func TestAttachmentRefOwnershipFindsRowAndToolMedia(t *testing.T) {
	engine, err := NewEngine("owner-chat")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(InitializeFork{
		Presentation: PresentationState{TabID: "owner-tab", Title: "Owner"},
		SourceChatID: "source", OperationID: "create-owner", Digest: "fixture",
		Messages: []LedgerEvent{{
			EventID: "row", MessageID: "assistant-row", OperationID: "turn", Role: "assistant", Text: "done", Status: "done",
			Attachments: []provider.Attachment{{ID: "own", MIMEType: "image/png", Ref: "workass-session-image:images/own"}},
			Timeline: []TimelineEntry{{Key: "tool", Kind: provider.EventToolUpdate, Tool: &provider.ToolEvent{
				ToolCallID: "call", Title: "Screenshot",
				Attachments: []provider.Attachment{{ID: "shot", MIMEType: "image/png", Ref: "workass-session-image:images/shot"}},
			}}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"workass-session-image:images/own", "workass-session-image:images/shot"} {
		if got := engine.AttachmentRefOwnership(ref); !got.Owned || got.Deleted {
			t.Fatalf("%s: %#v, want owned", ref, got)
		}
	}
	if got := engine.AttachmentRefOwnership("workass-session-image:images/other"); got.Owned {
		t.Fatalf("another chat's image reads as owned: %#v", got)
	}
}
