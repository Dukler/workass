package main

import (
	"context"
	"testing"
	"time"

	"workass/internal/acp"
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

// An agent's message with no delivery steers into a running local turn, as the
// person's own Enter does (owner 2026-11-19); images and idle chats stay auto.
// A send for another machine is forwarded with auto set explicitly.
func TestAgentSendSteersARunningChatByDefault(t *testing.T) {
	if got := defaultAgentSendDelivery(map[string]any{}, true); got != "steer" {
		t.Fatalf("running chat = %q", got)
	}
	if got := defaultAgentSendDelivery(map[string]any{}, false); got != "auto" {
		t.Fatalf("idle chat = %q", got)
	}
	if got := defaultAgentSendDelivery(map[string]any{"attachments": []any{"a.png"}}, true); got != "auto" {
		t.Fatalf("with images = %q", got)
	}
}

// With "Continuar solo" on, a usage-limit stop schedules "continue" for the
// reset by itself (owner 2026-11-19: the limit can hit while nobody is there).
func TestUsageLimitStopSchedulesContinueForTheReset(t *testing.T) {
	now := time.Date(2026, 11, 19, 3, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	newRuntime := func(t *testing.T) *providerChatRuntime {
		scheduler, err := scheduledmessage.New(scheduledmessage.Options{StateDir: t.TempDir(), Now: clock, Send: func(context.Context, scheduledmessage.Entry, string) error { return nil }})
		if err != nil {
			t.Fatal(err)
		}
		return &providerChatRuntime{scheduled: scheduler}
	}
	stop := acp.UsageLimitStop{TabID: "tab", ChatID: "chat", AssistantMessageID: "a1", ProviderID: "claude",
		Limit: providercontract.UsageLimit{ResetsAt: "2026-11-19T05:00:00Z", Label: "Claude Max"}}

	r := newRuntime(t)
	if err := r.ScheduleLimitContinue(nil, stop, clock, func(time.Duration) {}); err != nil {
		t.Fatal(err)
	}
	entries := r.scheduled.List("tab", "chat")
	if len(entries) != 1 || entries[0].Purpose != scheduledmessage.PurposeLimitContinue || entries[0].ForMessageID != "a1" ||
		!entries[0].Condition.At.Equal(time.Date(2026, 11, 19, 5, 0, 20, 0, time.UTC)) || scheduledmessage.Text(entries[0]) != "continue" {
		t.Fatalf("scheduled = %+v", entries)
	}
	// A repeat of the same stop is the same entry, never a second "continue".
	if err := r.ScheduleLimitContinue(nil, stop, clock, func(time.Duration) {}); err != nil || len(r.scheduled.List("tab", "chat")) != 1 {
		t.Fatalf("repeat: %v %d", err, len(r.scheduled.List("tab", "chat")))
	}

	// No time from the provider and no plan usage to ask: nothing.
	unknown := stop
	unknown.Limit.ResetsAt = ""
	r = newRuntime(t)
	if err := r.ScheduleLimitContinue(nil, unknown, clock, func(time.Duration) {}); err != nil || len(r.scheduled.List("tab", "chat")) != 0 {
		t.Fatalf("unknown reset: err=%v entries=%d", err, len(r.scheduled.List("tab", "chat")))
	}
	// A reset already past schedules nothing either.
	past := stop
	past.Limit.ResetsAt = "2026-11-19T02:00:00Z"
	r = newRuntime(t)
	if err := r.ScheduleLimitContinue(nil, past, clock, func(time.Duration) {}); err != nil || len(r.scheduled.List("tab", "chat")) != 0 {
		t.Fatalf("past reset scheduled: %v", err)
	}
}
