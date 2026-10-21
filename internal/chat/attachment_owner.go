package chat

import "workass/internal/provider"

// AttachmentOwnership is what an archive image read needs to know about its
// chat: whether the chat is a tombstone, and whether it owns the reference.
type AttachmentOwnership struct {
	Deleted bool
	Owned   bool
}

// AttachmentRefOwnership reports whether ref names an attachment this chat
// owns: a ledger row's own attachment, a tool attachment in a row's timeline,
// or one of the foreground turn's. It reads under the engine lock and copies
// nothing. Image reads used Snapshot, which clones the whole actor: on a
// 225 MB chat each image read cost about 15 ms of cloning, and a reader
// scrolling through its tool screenshots queued thousands of them ahead of
// the history pages on the same lane.
func (e *Engine) AttachmentRefOwnership(ref string) AttachmentOwnership {
	e.mu.Lock()
	defer e.mu.Unlock()
	state := &e.state
	if state.Deleted {
		return AttachmentOwnership{Deleted: true}
	}
	for index := range state.Ledger {
		row := &state.Ledger[index]
		if timelineOwnsAttachment(row.Timeline, ref) || attachmentsOwn(row.Attachments, ref) {
			return AttachmentOwnership{Owned: true}
		}
	}
	if foreground := state.Foreground; foreground != nil {
		if timelineOwnsAttachment(foreground.Timeline, ref) || attachmentsOwn(foreground.Input.Attachments, ref) ||
			attachmentsOwn(foreground.AssistantAttachments, ref) {
			return AttachmentOwnership{Owned: true}
		}
	}
	return AttachmentOwnership{}
}

func timelineOwnsAttachment(timeline []TimelineEntry, ref string) bool {
	for index := range timeline {
		if tool := timeline[index].Tool; tool != nil && attachmentsOwn(tool.Attachments, ref) {
			return true
		}
	}
	return false
}

func attachmentsOwn(attachments []provider.Attachment, ref string) bool {
	for index := range attachments {
		if attachments[index].Ref == ref {
			return true
		}
	}
	return false
}
