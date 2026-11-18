package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
	"workass/internal/scheduledmessage"
	"workass/internal/wire"
)

// Continuar on a reply a subscription's usage limit stopped (owner
// 2026-11-18, mock desktop/docs/mocks/limit-resume/option-a-v2.html). It can
// be pressed at any time: before the limit comes back it schedules "continue"
// for the moment it does; after, it sends it at once. The scheduled message is
// durable (scheduledmessage), shows in Automatizaciones, and drops itself if
// anything was written in the chat since.
//
//   - chat:limit-continue {tabId, chatId, messageId} → {ok, sent} or
//     {ok, scheduled: {id, at}}
//   - chat:limit-continue-cancel {tabId, chatId, id} → {ok}
//   - chat:limit-continue-status {tabId, chatId} → {scheduled: {id, at, messageId} | null}
const limitContinueText = "continue"

// A reset is sent a little after the provider's time, so the first request
// does not land a second before the window turns over.
const limitContinueSlack = 20 * time.Second

func limitContinueOperationID(messageID string) providercontract.OperationID {
	return providercontract.OperationID("limit-continue-" + messageID)
}

func registerLimitContinueHandlers(hub *wire.Hub, manager *acp.Manager, providerChats *providerChatRuntime, control *chatControlCoordinator) int {
	hub.Register("chat:limit-continue", func(args []any) (any, error) {
		arg := firstMapArg(args)
		return providerChats.LimitContinue(context.Background(), manager, fieldString(arg, "tabId"), fieldString(arg, "chatId"), fieldString(arg, "messageId"), time.Now(),
			func(tabID, chatID string) { control.refresh(tabID, chatID, false) })
	})
	hub.Register("chat:limit-continue-cancel", func(args []any) (any, error) {
		arg := firstMapArg(args)
		return providerChats.LimitContinueCancel(fieldString(arg, "tabId"), fieldString(arg, "chatId"), fieldString(arg, "id"))
	})
	hub.Register("chat:limit-continue-status", func(args []any) (any, error) {
		arg := firstMapArg(args)
		return providerChats.LimitContinueStatus(fieldString(arg, "tabId"), fieldString(arg, "chatId"))
	})
	return 3
}

func (r *providerChatRuntime) limitScheduler() (*scheduledmessage.Scheduler, error) {
	if r == nil || r.scheduled == nil {
		return nil, errors.New(scheduledMessageUnavailable)
	}
	return r.scheduled, nil
}

// LimitContinueCancel drops a scheduled Continuar.
func (r *providerChatRuntime) LimitContinueCancel(tabID, chatID, id string) (any, error) {
	scheduler, err := r.limitScheduler()
	if err != nil {
		return nil, err
	}
	if tabID == "" || chatID == "" || id == "" {
		return nil, errors.New("tabId, chatId and id are required")
	}
	if _, err := scheduler.Cancel(tabID, chatID, id); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// LimitContinueStatus is the chat's scheduled Continuar, or null.
func (r *providerChatRuntime) LimitContinueStatus(tabID, chatID string) (any, error) {
	scheduler, err := r.limitScheduler()
	if err != nil {
		return nil, err
	}
	return map[string]any{"scheduled": pendingLimitContinue(scheduler, tabID, chatID)}, nil
}

func pendingLimitContinue(scheduler *scheduledmessage.Scheduler, tabID, chatID string) any {
	for _, entry := range scheduler.List(tabID, chatID) {
		if entry.Purpose == scheduledmessage.PurposeLimitContinue && entry.Status == scheduledmessage.StatusPending {
			return map[string]any{"id": entry.ID, "at": entry.Condition.At.UTC().Format(time.RFC3339), "messageId": entry.ForMessageID}
		}
	}
	return nil
}

// LimitContinue sends "continue" now, or schedules it for the reset.
func (r *providerChatRuntime) LimitContinue(ctx context.Context, manager *acp.Manager, tabID, chatID, messageID string, now time.Time, refresh func(tabID, chatID string)) (any, error) {
	if tabID == "" || chatID == "" || messageID == "" {
		return nil, errors.New("tabId, chatId and messageId are required")
	}
	scheduler, err := r.limitScheduler()
	if err != nil {
		return nil, err
	}
	actor, err := r.actor(chatID)
	if err != nil {
		return nil, err
	}
	state := actor.engine.ReadSnapshot()
	if strings.TrimSpace(state.Presentation.TabID) != tabID {
		return nil, errors.New("chat was deleted or moved")
	}
	row, current := limitedReply(state.Ledger, messageID)
	if row == nil {
		return nil, errors.New("this reply was not stopped by a usage limit")
	}
	if !current {
		return nil, errors.New("the chat has moved on since this reply")
	}
	if pending := pendingLimitContinue(scheduler, tabID, chatID); pending != nil {
		return map[string]any{"ok": true, "scheduled": pending}, nil
	}
	resetsAt := limitResetTime(row, manager, now)
	if resetsAt.IsZero() || !resetsAt.After(now) {
		if _, err := r.QueueAgentMessageWithAttachments(ctx, tabID, chatID, limitContinueOperationID(messageID), limitContinueText, "auto", nil); err != nil {
			return nil, err
		}
		if refresh != nil {
			refresh(tabID, chatID)
		}
		return map[string]any{"ok": true, "sent": true}, nil
	}
	at := resetsAt.Add(limitContinueSlack).UTC()
	entry, err := scheduler.Schedule(tabID, chatID, scheduledmessage.Request{
		OperationID: string(limitContinueOperationID(messageID)),
		Message:     limitContinueText,
		Field:       "at",
		Value:       at.Format(time.RFC3339),
		Condition:   scheduledmessage.Condition{Kind: "at", At: at},
		Purpose:     scheduledmessage.PurposeLimitContinue, ForMessageID: messageID,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "scheduled": map[string]any{"id": entry.ID, "at": at.Format(time.RFC3339), "messageId": messageID}}, nil
}

// limitedReply finds the reply a usage limit stopped, and whether it is still
// where the chat stands: nothing the person wrote and no reply came after it.
// Wake-up notices do not count; they are Workass's, not the person's.
func limitedReply(ledger []chat.LedgerEvent, messageID string) (*chat.LedgerEvent, bool) {
	for index := len(ledger) - 1; index >= 0; index-- {
		row := &ledger[index]
		if row.MessageID != messageID {
			continue
		}
		if row.Role != "assistant" || row.Terminal == nil || row.Terminal.UsageLimit == nil {
			return nil, false
		}
		for _, later := range ledger[index+1:] {
			if later.Role == "assistant" || (later.Role == "user" && later.NoticeKind == "") {
				return row, false
			}
		}
		return row, true
	}
	return nil, false
}

// When the subscription is back: the provider's own time, else the account's
// plan usage as it reads now (Codex reports no time with the failure).
func limitResetTime(row *chat.LedgerEvent, manager *acp.Manager, now time.Time) time.Time {
	if at, err := time.Parse(time.RFC3339, row.Terminal.UsageLimit.ResetsAt); err == nil {
		return at
	}
	if manager == nil {
		return time.Time{}
	}
	if at, err := time.Parse(time.RFC3339, manager.UsageLimitReset(string(row.ProviderID), now)); err == nil {
		return at
	}
	return time.Time{}
}

// limitContinueObsolete checks, when its time comes, that a scheduled
// Continuar is still wanted.
func limitContinueObsolete(state chat.State, e scheduledmessage.Entry) bool {
	_, current := limitedReply(state.Ledger, e.ForMessageID)
	return !current
}

// cancelPendingLimitContinue drops the chat's scheduled Continuar, if any.
func cancelPendingLimitContinue(scheduler *scheduledmessage.Scheduler, tabID, chatID string) {
	if scheduler == nil {
		return
	}
	for _, entry := range scheduler.List(tabID, chatID) {
		if entry.Purpose == scheduledmessage.PurposeLimitContinue && entry.Status == scheduledmessage.StatusPending {
			_, _ = scheduler.Cancel(tabID, chatID, entry.ID)
		}
	}
}
