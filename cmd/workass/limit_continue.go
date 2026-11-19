package main

import (
	"errors"
	"log"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
	"workass/internal/scheduledmessage"
)

// "continue" for a chat a subscription's usage limit stopped (owner
// 2026-11-18/19). An opt-in setting with no chat UI ("it could happen while
// I'm AFK"; the button and the chat's line were removed): with Ajustes ·
// General · limitContinue on, the daemon schedules "continue" for the moment
// that subscription is back, as soon as the limit stops the turn. The
// scheduled message is durable (scheduledmessage), shows in Automatizaciones
// (where it can be cancelled), and drops itself if anything was written in
// the chat since.
const limitContinueText = "continue"

// A reset is sent a little after the provider's time, so the first request
// does not land a second before the window turns over.
const limitContinueSlack = 20 * time.Second

func limitContinueOperationID(messageID string) providercontract.OperationID {
	return providercontract.OperationID("limit-continue-" + messageID)
}

func (r *providerChatRuntime) limitScheduler() (*scheduledmessage.Scheduler, error) {
	if r == nil || r.scheduled == nil {
		return nil, errors.New(scheduledMessageUnavailable)
	}
	return r.scheduled, nil
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

// limitContinueRetry is how long to wait for the plan usage to say when a
// subscription is back, when the provider did not (Codex).
const limitContinueRetry = 2 * time.Minute

// ScheduleLimitContinue is the opt-in automatic Continuar: the reply's
// "continue" is scheduled for when its subscription is back. It needs no
// ledger row yet (it runs as the turn fails); delivery checks the row is
// still the chat's latest. A reset already past, or none known even after the
// plan usage refreshed, schedules nothing: sending into a limit only fails.
func (r *providerChatRuntime) ScheduleLimitContinue(manager *acp.Manager, stop acp.UsageLimitStop, now func() time.Time, wait func(time.Duration)) error {
	scheduler, err := r.limitScheduler()
	if err != nil {
		return err
	}
	if stop.TabID == "" || stop.ChatID == "" || stop.AssistantMessageID == "" {
		return errors.New("a usage-limit stop needs its chat and reply")
	}
	resetsAt, _ := time.Parse(time.RFC3339, stop.Limit.ResetsAt)
	if resetsAt.IsZero() && manager != nil {
		for attempt := 0; attempt < 2 && resetsAt.IsZero(); attempt++ {
			if attempt > 0 {
				wait(limitContinueRetry)
			}
			resetsAt, _ = time.Parse(time.RFC3339, manager.UsageLimitReset(stop.ProviderID, now()))
		}
	}
	if resetsAt.IsZero() || !resetsAt.After(now()) {
		return nil
	}
	at := resetsAt.Add(limitContinueSlack).UTC()
	_, err = scheduler.Schedule(stop.TabID, stop.ChatID, scheduledmessage.Request{
		OperationID: string(limitContinueOperationID(stop.AssistantMessageID)),
		Message:     limitContinueText,
		Field:       "at",
		Value:       at.Format(time.RFC3339),
		Condition:   scheduledmessage.Condition{Kind: "at", At: at},
		Purpose:     scheduledmessage.PurposeLimitContinue, ForMessageID: stop.AssistantMessageID,
	})
	return err
}

// watchUsageLimitStops wires the opt-in setting to the manager's stops.
func watchUsageLimitStops(manager *acp.Manager, chats *providerChatRuntime, settings func() map[string]any) {
	if manager == nil || chats == nil {
		return
	}
	manager.SetUsageLimitObserver(func(stop acp.UsageLimitStop) {
		if settings == nil || settings()["limitContinue"] != true {
			return
		}
		if err := chats.ScheduleLimitContinue(manager, stop, time.Now, time.Sleep); err != nil {
			log.Printf("[workass] usage-limit continue not scheduled: %s", acp.RedactSensitiveText(err.Error()))
		}
	})
}
