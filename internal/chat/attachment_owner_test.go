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

// A just-sent image is read the moment its row is shown. A direct send waits
// in Queue while its lane is created or resumed, and PresentationState draws
// it as the running turn's user row from the first read; its image must be
// readable from then on, not only once the lane attaches (2026-11-12: the
// phone's read 68 ms after job:start and the desktop's both failed, and
// neither read it again). Queued and staged rows show their images too.
func TestAttachmentRefOwnershipCoversInputBeforeTheForeground(t *testing.T) {
	sent := provider.Attachment{ID: "sent", MIMEType: "image/jpeg", Ref: "workass-session-image:images/sent"}
	queued := provider.Attachment{ID: "queued", MIMEType: "image/png", Ref: "workass-session-image:images/queued"}
	staged := provider.Attachment{ID: "staged", MIMEType: "image/png", Ref: "workass-session-image:images/staged"}
	for _, phase := range []LanePhase{LaneCreating, LaneResuming, LaneImporting, LaneDetached} {
		t.Run(string(phase), func(t *testing.T) {
			state, _ := NewState("sent-image-chat")
			state.Initialized = true
			state.Lanes["lane"] = LaneState{Phase: phase}
			state.Queue = []QueueEntry{
				{OperationID: "send", LaneID: "lane", Text: "look", Attachments: []provider.Attachment{sent}, Presentation: provider.TurnPresentation{
					UserMessageID: "user", AssistantMessageID: "assistant",
				}},
				{OperationID: "later", LaneID: "lane", Attachments: []provider.Attachment{queued}, Presentation: provider.TurnPresentation{QueueID: "q-later"}},
			}
			state.StagedQueue = []StagedQueueEntry{{ID: "q-staged", Text: "next", Attachments: []provider.Attachment{staged}}}
			engine := &Engine{state: state}
			if shown := state.PresentationState().Foreground; (phase != LaneDetached) != (shown != nil && shown.Input.OperationID == "send") {
				t.Fatalf("lane %s: presented foreground %#v", phase, shown)
			}
			for _, attachment := range []provider.Attachment{sent, queued, staged} {
				if got := engine.AttachmentRefOwnership(attachment.Ref); !got.Owned || got.Deleted {
					t.Fatalf("lane %s: %s = %#v, want owned", phase, attachment.ID, got)
				}
			}
			if got := engine.AttachmentRefOwnership("workass-session-image:images/other"); got.Owned {
				t.Fatalf("another chat's image reads as owned: %#v", got)
			}
		})
	}
}
