package main

import (
	"testing"

	"workass/internal/chat"
	providercontract "workass/internal/provider"
	"workass/internal/scheduledmessage"
)

func limitedRow(id string) chat.LedgerEvent {
	return chat.LedgerEvent{MessageID: id, Role: "assistant", Status: "failed",
		Terminal: &providercontract.TerminalEvent{Status: "failed", UsageLimit: &providercontract.UsageLimit{Label: "Claude Max"}}}
}

// The Continuar line, and a scheduled "continue", belong to the chat's latest
// reply only: anything the person wrote, or any reply, after it moves the chat
// on; Workass's own wake-up notices do not (owner 2026-11-18).
func TestLimitedReplyIsCurrentUntilTheChatMovesOn(t *testing.T) {
	ledger := []chat.LedgerEvent{{MessageID: "u1", Role: "user"}, limitedRow("a1")}
	if row, current := limitedReply(ledger, "a1"); row == nil || !current {
		t.Fatal("the latest limited reply is current")
	}
	withNotice := append(append([]chat.LedgerEvent(nil), ledger...), chat.LedgerEvent{MessageID: "n1", Role: "user", NoticeKind: "subagent"})
	if _, current := limitedReply(withNotice, "a1"); !current {
		t.Fatal("a wake-up notice does not move the chat on")
	}
	for _, later := range []chat.LedgerEvent{{MessageID: "u2", Role: "user"}, {MessageID: "a2", Role: "assistant"}} {
		moved := append(append([]chat.LedgerEvent(nil), ledger...), later)
		if row, current := limitedReply(moved, "a1"); row == nil || current {
			t.Fatalf("%s after it moves the chat on", later.Role)
		}
		if !limitContinueObsolete(chat.State{Ledger: moved}, scheduledmessage.Entry{ForMessageID: "a1"}) {
			t.Fatal("a scheduled continue for a chat that moved on is obsolete")
		}
	}
	if row, _ := limitedReply([]chat.LedgerEvent{{MessageID: "a1", Role: "assistant"}}, "a1"); row != nil {
		t.Fatal("a reply no limit stopped has no Continuar")
	}
	if limitContinueObsolete(chat.State{Ledger: ledger}, scheduledmessage.Entry{ForMessageID: "a1"}) {
		t.Fatal("still current: not obsolete")
	}
}
