package main

import (
	"errors"
	"strings"
	"time"

	"workass/internal/acp"
	"workass/internal/scheduledmessage"
	"workass/internal/wire"
)

// Automatizaciones (owner 2026-11-09, mock desktop/docs/mocks/scheduled
// option E): what is scheduled on this machine, for the Trabajos tab's
// Programado section (one chat) and the global page (every chat, and every
// paired machine through its own daemon).
//   - automations:list replies {items}: each pending scheduled message
//     (kind "message") and each cron a provider session last reported (kind
//     "session-cron", Claude's CronCreate, ScheduleWakeup and /loop).
//   - automations:cancel {tabId, chatId, id} cancels a pending scheduled
//     message. A session cron lives in its session; only that agent can drop it.

const automationTextMax = 400

type automationItem struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	TabID     string `json:"tabId"`
	ChatID    string `json:"chatId"`
	Text      string `json:"text"`
	CreatedAt string `json:"createdAt,omitempty"`
	// message: "at" | "tcp" | "file" | "pid" and its one value.
	Condition string `json:"condition,omitempty"`
	At        string `json:"at,omitempty"`
	Addr      string `json:"addr,omitempty"`
	Path      string `json:"path,omitempty"`
	PID       int    `json:"pid,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
	Late      bool   `json:"late,omitempty"`
	// session-cron
	Schedule   string `json:"schedule,omitempty"`
	Recurring  bool   `json:"recurring,omitempty"`
	ReportedAt string `json:"reportedAt,omitempty"`
}

func automationText(text string) string {
	text = strings.TrimSpace(acp.RedactSensitiveText(text))
	if runes := []rune(text); len(runes) > automationTextMax {
		text = string(runes[:automationTextMax]) + "…"
	}
	return text
}

func automationTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func automationItems(scheduled *scheduledmessage.Scheduler, crons []acp.ChatSessionCron) []automationItem {
	items := []automationItem{}
	if scheduled != nil {
		for _, e := range scheduled.Pending() {
			items = append(items, automationItem{
				Kind: "message", ID: e.ID, TabID: e.TabID, ChatID: e.ChatID,
				Text: automationText(e.Message), CreatedAt: automationTime(e.CreatedAt),
				Condition: e.Condition.Kind, At: automationTime(e.Condition.At), Addr: e.Condition.Addr,
				Path: e.Condition.Path, PID: e.Condition.PID, ExpiresAt: automationTime(e.ExpiresAt), Late: e.Late,
			})
		}
	}
	for _, cron := range crons {
		items = append(items, automationItem{
			Kind: "session-cron", ID: cron.ID, TabID: cron.TabID, ChatID: cron.ChatID,
			Text: automationText(cron.Prompt), Schedule: cron.Schedule, Recurring: cron.Recurring,
			ReportedAt: automationTime(cron.ReportedAt),
		})
	}
	return items
}

func registerAutomationHandlers(hub *wire.Hub, manager *acp.Manager, chats *providerChatRuntime) int {
	scheduler := func() *scheduledmessage.Scheduler {
		if chats == nil {
			return nil
		}
		return chats.scheduled
	}
	hub.Register("automations:list", func(args []any) (any, error) {
		var crons []acp.ChatSessionCron
		if manager != nil {
			crons = manager.SessionCrons()
		}
		return map[string]any{"items": automationItems(scheduler(), crons)}, nil
	})
	hub.Register("automations:cancel", func(args []any) (any, error) {
		arg := firstMapArg(args)
		tabID, chatID, id := fieldString(arg, "tabId"), fieldString(arg, "chatId"), fieldString(arg, "id")
		s := scheduler()
		if s == nil {
			return nil, errors.New(scheduledMessageUnavailable)
		}
		if tabID == "" || chatID == "" || id == "" {
			return nil, errors.New("tabId, chatId and id are required")
		}
		entry, err := s.Cancel(tabID, chatID, id)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "id": entry.ID, "status": entry.Status}, nil
	})
	return 2
}
