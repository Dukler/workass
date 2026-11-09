package main

import (
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
)

func TestAutomationItemsCarryCronsAndRedactText(t *testing.T) {
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	items := automationItems(nil, []acp.ChatSessionCron{{
		TabID: "tab-1", ChatID: "chat-1", ID: "cron-1", Schedule: "0 9 * * *", Recurring: true,
		Prompt: "deploy with password=hunter2hunter2 " + strings.Repeat("x", 600), ReportedAt: at,
	}})
	if len(items) != 1 {
		t.Fatalf("items = %#v", items)
	}
	item := items[0]
	if item.Kind != "session-cron" || item.Schedule != "0 9 * * *" || !item.Recurring || item.ReportedAt != "2026-11-10T09:00:00Z" {
		t.Fatalf("item = %#v", item)
	}
	if strings.Contains(item.Text, "hunter2hunter2") {
		t.Fatalf("secret reached the list: %q", item.Text)
	}
	if n := len([]rune(item.Text)); n > automationTextMax+1 {
		t.Fatalf("text is %d runes, want at most %d", n, automationTextMax+1)
	}
	if got := automationItems(nil, nil); got == nil || len(got) != 0 {
		t.Fatalf("an empty list must be [], not null: %#v", got)
	}
}
