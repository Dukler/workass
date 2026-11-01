package chat

import "strings"

// UserMessageRecord is what this chat's own durable state says about one message
// that entered it as input. It is read from the daemon's record and never from
// anything a caller or a provider asserts about the message.
type UserMessageRecord struct {
	MessageID string
	Text      string
	At        string
	// Origin is human, agent or internal, or empty when the row predates the
	// origin being recorded. Empty is unknown, never human.
	Origin string
	Notice bool
	// Copied marks a row a fork copied from another chat. The person wrote it
	// to that chat, not this one.
	Copied bool
	// Withdrawn marks input stopped before it ever reached the agent.
	Withdrawn bool
}

// UserMessageRecord finds one input of this chat by its stable message id: a row
// of the transcript, the running turn's own input before its row is written, or a
// steer still waiting to be consumed. Input only queued has not reached the agent
// yet and is not found.
func (s State) UserMessageRecord(messageID string) (UserMessageRecord, bool) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" || s.Deleted {
		return UserMessageRecord{}, false
	}
	for index := len(s.Ledger) - 1; index >= 0; index-- {
		event := s.Ledger[index]
		if event.MessageID != messageID || event.Role != "user" {
			continue
		}
		return UserMessageRecord{
			MessageID: messageID, Text: event.Text, At: event.At, Origin: event.Origin,
			Notice: strings.TrimSpace(event.NoticeKind) != "", Copied: event.Inherited || event.ContextExcluded,
			Withdrawn: event.TerminalState == "cancelled_before_admission",
		}, true
	}
	if foreground := s.Foreground; foreground != nil && !foreground.UserConsumed &&
		strings.TrimSpace(foreground.Input.Presentation.UserMessageID) == messageID {
		presentation := foreground.Input.Presentation
		return UserMessageRecord{
			MessageID: messageID, Text: foreground.Input.Text, At: foreground.StartedAt, Origin: presentation.Origin,
			Notice: strings.TrimSpace(presentation.NoticeKind) != "",
		}, true
	}
	for pending := s.PendingSteer; pending != nil; pending = pending.Next {
		if strings.TrimSpace(pending.Presentation.UserMessageID) != messageID {
			continue
		}
		return UserMessageRecord{
			MessageID: messageID, Text: pending.Text, At: pending.Presentation.StartedAt, Origin: pending.Presentation.Origin,
			Notice: strings.TrimSpace(pending.Presentation.NoticeKind) != "",
		}, true
	}
	return UserMessageRecord{}, false
}
