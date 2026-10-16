package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"workass/internal/acp"
	providercontract "workass/internal/provider"
	"workass/internal/scheduledmessage"
)

const scheduledMessageUnavailable = "Workass scheduled messages are unavailable"

// scheduledSendOperationID is the chat.send operation id of one scheduled
// message. It is derived from the scheduled message id alone, so a crash
// between queueing the message and saving the result cannot deliver it twice.
func scheduledSendOperationID(id string) providercontract.OperationID {
	return providercontract.OperationID("scheduled-send-" + id)
}

// attachScheduledMessages builds the daemon's scheduled-message store under
// stateDir and hands it to the chat runtime, which cancels a deleted chat's
// pending scheduled messages. The caller runs it with Run(ctx).
func (c *chatControlCoordinator) attachScheduledMessages(stateDir string, opts scheduledmessage.Options) (*scheduledmessage.Scheduler, error) {
	if c == nil || c.providerChats == nil {
		return nil, errors.New(scheduledMessageUnavailable)
	}
	opts.StateDir = stateDir
	opts.Send = c.deliverScheduledMessage
	scheduler, err := scheduledmessage.New(opts)
	if err != nil {
		return nil, err
	}
	c.providerChats.scheduled = scheduler
	return scheduler, nil
}

// deliverScheduledMessage is the scheduler's Sender. It queues the text into
// the entry's own chat exactly like chat.send with delivery auto and returns as
// soon as the durable queue holds it.
func (c *chatControlCoordinator) deliverScheduledMessage(ctx context.Context, e scheduledmessage.Entry, text string) error {
	runtime := c.providerChats
	actor, err := runtime.actor(e.ChatID)
	if err != nil {
		if errors.Is(err, errActorChatNotFound) {
			return scheduledSendError(scheduledmessage.ErrChatGone, err)
		}
		return scheduledSendError(nil, err)
	}
	state := actor.engine.Snapshot()
	if state.Deleted || strings.TrimSpace(state.Presentation.TabID) != strings.TrimSpace(e.TabID) {
		return scheduledSendError(scheduledmessage.ErrChatGone, errors.New("chat was deleted or moved"))
	}
	operationID := scheduledSendOperationID(e.ID)
	// A restart between queueing and saving can change the text (late marker)
	// of an already queued message; the queue would then refuse the changed
	// retry, so a known operation id is the delivery receipt itself.
	if _, exists, lookupErr := agentSendExistingOperation(state, operationID); lookupErr == nil && exists {
		return nil
	}
	if _, err := runtime.QueueAgentMessageWithAttachments(ctx, e.TabID, e.ChatID, operationID, text, "auto", nil); err != nil {
		return scheduledSendError(nil, err)
	}
	c.refresh(e.TabID, e.ChatID, false)
	return nil
}

// scheduledSendError redacts a delivery error; sentinel, when set, stays
// matchable with errors.Is.
func scheduledSendError(sentinel, err error) error {
	text := acp.RedactSensitiveText(err.Error())
	if sentinel != nil {
		return fmt.Errorf("%w: %s", sentinel, text)
	}
	return errors.New(text)
}

// scheduledMessageCall serves schedule.create, schedule.list and
// schedule.cancel. The target is always the caller's own chat; a scheduled
// message is never routed to another chat or machine.
func (h *agentControlHandler) scheduledMessageCall(method string, params map[string]any, tabID, chatID string) (any, error) {
	if h == nil || h.chats == nil || h.chats.providerChats == nil || h.chats.providerChats.scheduled == nil {
		return nil, errors.New(scheduledMessageUnavailable)
	}
	if target := fieldString(params, "tab_id"); target != "" && target != tabID {
		return nil, errors.New("scheduled messages can only target the calling chat; tab_id must be omitted or be this chat's tab")
	}
	if target := fieldString(params, "chat_id"); target != "" && target != chatID {
		return nil, errors.New("scheduled messages can only target the calling chat; chat_id must be omitted or be this chat")
	}
	if fieldString(params, "machine_id") != "" {
		return nil, errors.New("scheduled messages are local to this machine's chat; machine_id is not accepted")
	}
	scheduler := h.chats.providerChats.scheduled
	switch method {
	case "schedule.create":
		req, err := scheduledmessage.ParseRequest(params)
		if err != nil {
			return nil, err
		}
		entry, err := scheduler.Schedule(tabID, chatID, req)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "scheduledMessage": scheduledMessageView(entry)}, nil
	case "schedule.list":
		entries := scheduler.List(tabID, chatID)
		items := make([]any, len(entries))
		for i, entry := range entries {
			items[i] = scheduledMessageView(entry)
		}
		return map[string]any{"ok": true, "scheduledMessages": items}, nil
	case "schedule.cancel":
		id := fieldString(params, "scheduled_message_id")
		if id == "" {
			return nil, errors.New("scheduled_message_id is required")
		}
		entry, err := scheduler.Cancel(tabID, chatID, id)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "scheduledMessage": scheduledMessageView(entry)}, nil
	}
	return nil, errors.New("unknown scheduled message method")
}

// scheduledMessageView is the agent-facing projection: the condition as a
// sentence, the times, never the stored message or request hash.
func scheduledMessageView(e scheduledmessage.Entry) map[string]any {
	view := map[string]any{
		"id":        e.ID,
		"status":    e.Status,
		"summary":   scheduledSummary(e),
		"createdAt": e.CreatedAt.UTC().Format(time.RFC3339),
	}
	if e.Condition.Kind == "at" {
		view["fireAt"] = e.Condition.At.UTC().Format(time.RFC3339)
	} else if !e.ExpiresAt.IsZero() {
		view["expiresAt"] = e.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if e.SettledAt != nil {
		view["settledAt"] = e.SettledAt.UTC().Format(time.RFC3339)
	}
	if e.Late {
		view["late"] = true
	}
	if e.Error != "" {
		view["error"] = e.Error
	}
	return view
}

func scheduledSummary(e scheduledmessage.Entry) string {
	switch e.Condition.Kind {
	case "at":
		return "at " + e.Condition.At.UTC().Format(time.RFC3339)
	case "tcp":
		return "when " + e.Condition.Addr + " accepts connections"
	case "file":
		return "when " + e.Condition.Path + " exists"
	case "pid":
		return fmt.Sprintf("when pid %d exits", e.Condition.PID)
	}
	return e.Condition.Kind
}
