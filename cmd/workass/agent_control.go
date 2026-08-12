package main

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"workass/internal/acp"
	"workass/internal/artifacthost"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

type agentControlHandler struct {
	manager   *acp.Manager
	chats     *chatControlCoordinator
	artifacts *artifacthost.Registry
}

type agentControlRequest struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

func newAgentControlHandler(manager *acp.Manager, broadcast func(string, any), chats *chatControlCoordinator) (*agentControlHandler, error) {
	if manager == nil {
		return nil, errors.New("agent control requires an ACP manager")
	}
	if chats == nil || chats.providerChats == nil {
		return nil, errors.New("agent control requires the singleton durable chat actor runtime")
	}
	handler := &agentControlHandler{manager: manager, chats: chats}
	chats.resumeQueues()
	return handler, nil
}

func (h *agentControlHandler) call(r *http.Request, request agentControlRequest) (any, error) {
	params := request.Params
	chatID := fieldString(params, "parent_chat_id")
	tabID := fieldString(params, "parent_tab_id")
	ownerKey := fieldString(params, "owner_key")
	switch request.Method {
	case "chat.list", "chat.read", "chat.create", "chat.rename", "chat.configure", "chat.focus", "chat.delete", "chat.send", "chat.cancel":
		if err := h.chats.authorize(ownerKey, chatID, tabID); err != nil {
			return nil, err
		}
		switch request.Method {
		case "chat.list":
			return h.chats.list()
		case "chat.read":
			return h.chats.read(params)
		case "chat.create":
			return h.chats.create(r.Context(), tabID, chatID, params)
		case "chat.rename":
			return h.chats.rename(params)
		case "chat.configure":
			return h.chats.configure(r.Context(), params)
		case "chat.focus":
			return h.chats.focus(params)
		case "chat.delete":
			return h.chats.delete(params)
		case "chat.send":
			return h.chats.send(params)
		case "chat.cancel":
			return h.chats.cancel(params)
		}
		return nil, errors.New("unknown chat control method")
	case "agent.catalog":
		return h.manager.AgentCatalog(r.Context(), ownerKey, chatID, tabID)
	case "agent.spawn":
		if !h.manager.ValidateAgentOwner(ownerKey, chatID, tabID) {
			return nil, errors.New("no running Workass turn owns this subagent request")
		}
		if h.chats == nil || h.chats.providerChats == nil {
			return nil, errors.New("subagent spawn requires the durable chat actor")
		}
		return h.chats.providerChats.RunBackgroundAction(r.Context(), tabID, chatID, chat.BackgroundAction{
			Kind: chat.BackgroundSpawnAgent, OperationID: providercontract.NormalizeOperationID(fieldString(params, "operation_id")),
			Spawn: &chat.SpawnAgentAction{
				Prompt: fieldString(params, "prompt"), Label: fieldString(params, "label"),
				ProviderID: fieldString(params, "provider_id"), ModelID: fieldString(params, "model_id"),
				Effort: fieldString(params, "effort"), ModeID: fieldString(params, "mode_id"),
				CWD: fieldString(params, "cwd"), Profile: fieldString(params, "profile"),
				PermissionIntent: fieldString(params, "permission_intent"),
			},
		})
	case "agent.list":
		if h.chats == nil || h.chats.providerChats == nil {
			return nil, errors.New("agent list requires the durable chat actor")
		}
		subagents, err := h.chats.providerChats.ListSubagents(ownerKey, tabID, chatID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"subagents": subagents}, nil
	case "agent.wait":
		id := fieldString(params, "id")
		if id == "" {
			return nil, errors.New("subagent id is required")
		}
		timeoutMS := intField(params, "timeout_ms")
		if timeoutMS != 0 && (timeoutMS < 1000 || timeoutMS > 3600000) {
			return nil, errors.New("subagent wait timeout_ms must be between 1000 and 3600000")
		}
		if h.chats == nil || h.chats.providerChats == nil {
			return nil, errors.New("agent wait requires the durable chat actor")
		}
		timeout := time.Duration(timeoutMS) * time.Millisecond
		return h.chats.providerChats.WaitSubagent(r.Context(), ownerKey, tabID, chatID, id, timeout)
	case "agent.wait_many":
		ids := stringSliceField(params["ids"])
		if len(ids) == 0 {
			return nil, errors.New("at least one subagent id is required")
		}
		timeoutMS := intField(params, "timeout_ms")
		if timeoutMS != 0 && (timeoutMS < 1000 || timeoutMS > 3600000) {
			return nil, errors.New("subagent wait timeout_ms must be between 1000 and 3600000")
		}
		if h.chats == nil || h.chats.providerChats == nil {
			return nil, errors.New("agent wait requires the durable chat actor")
		}
		return h.chats.providerChats.WaitSubagents(r.Context(), ownerKey, tabID, chatID, ids, fieldString(params, "return_when"), time.Duration(timeoutMS)*time.Millisecond)
	case "agent.message":
		if !h.manager.ValidateAgentOwner(ownerKey, chatID, tabID) {
			return nil, errors.New("no running Workass turn owns this subagent request")
		}
		return h.runBackgroundAction(r, tabID, chatID, chat.BackgroundAction{
			Kind: chat.BackgroundMessageAgent, OperationID: providercontract.NormalizeOperationID(fieldString(params, "operation_id")),
			Message: &chat.MessageAgentAction{WorkID: fieldString(params, "id"), Message: fieldString(params, "message")},
		})
	case "agent.retry":
		if !h.manager.ValidateAgentOwner(ownerKey, chatID, tabID) {
			return nil, errors.New("no running Workass turn owns this subagent request")
		}
		return h.runBackgroundAction(r, tabID, chatID, chat.BackgroundAction{
			Kind: chat.BackgroundRetryAgent, OperationID: providercontract.NormalizeOperationID(fieldString(params, "operation_id")),
			Retry: &chat.RetryAgentAction{WorkID: fieldString(params, "id"), Message: fieldString(params, "message")},
		})
	case "agent.receipts":
		if h.chats == nil || h.chats.providerChats == nil {
			return nil, errors.New("agent receipts require the durable chat actor")
		}
		receipts, err := h.chats.providerChats.ListSubagentReceipts(ownerKey, tabID, chatID, intField(params, "limit"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"receipts": receipts}, nil
	case "spawned_work.list":
		tailChars := intField(params, "tail_chars")
		if tailChars < 0 || tailChars > 12000 {
			return nil, errors.New("spawned work tail_chars must be between 0 and 12000")
		}
		if h.chats == nil || h.chats.providerChats == nil {
			return nil, errors.New("spawned work list requires the durable chat actor")
		}
		items, err := h.chats.providerChats.ListSpawnedWorkForOwner(
			ownerKey, chatID, tabID, fieldString(params, "chat_id"), fieldString(params, "tab_id"), tailChars,
		)
		if err != nil {
			return nil, err
		}
		return map[string]any{"items": items}, nil
	case "spawned_work.receipts":
		if h.chats == nil || h.chats.providerChats == nil {
			return nil, errors.New("spawned work receipts require the durable chat actor")
		}
		receipts, err := h.chats.providerChats.ListSpawnedWorkReceipts(
			ownerKey, chatID, tabID, fieldString(params, "chat_id"), fieldString(params, "tab_id"), intField(params, "limit"),
		)
		if err != nil {
			return nil, err
		}
		return map[string]any{"receipts": receipts}, nil
	case "external.register":
		var pid *int
		if value, ok := intFieldPresent(params, "pid"); ok {
			if value <= 1 {
				return nil, errors.New("external work pid must be greater than 1")
			}
			pid = &value
		}
		targetTab, targetChat := firstNonEmptyString(fieldString(params, "tab_id"), tabID), firstNonEmptyString(fieldString(params, "chat_id"), chatID)
		if targetTab != tabID || targetChat != chatID {
			return nil, errors.New("external work must remain in the exact owning Workass chat")
		}
		if !h.manager.ValidateAgentOwner(ownerKey, targetChat, targetTab) {
			return nil, errors.New("no running Workass turn owns this external work request")
		}
		return h.runBackgroundAction(r, tabID, chatID, chat.BackgroundAction{
			Kind: chat.BackgroundRegisterExternal, OperationID: providercontract.NormalizeOperationID(fieldString(params, "operation_id")),
			Register: &chat.RegisterExternalAction{
				Label: fieldString(params, "label"), Role: fieldString(params, "role"), PID: pid,
				OutputFile: fieldString(params, "output_file"), DoneFile: fieldString(params, "done_file"),
			},
		})
	case "obligation.get":
		obligationTab := firstNonEmptyString(fieldString(params, "tab_id"), tabID)
		obligationChat := firstNonEmptyString(fieldString(params, "chat_id"), chatID)
		if !h.manager.ValidateAgentOwner(ownerKey, obligationChat, obligationTab) {
			return nil, errors.New("no running Workass turn owns this obligation request")
		}
		if h.chats == nil || h.chats.providerChats == nil {
			return nil, errors.New("obligation read requires the durable chat actor")
		}
		obligation, err := h.chats.providerChats.Obligation(obligationTab, obligationChat)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "obligation": obligation}, nil
	case "external.settle":
		var exitCode *int
		if value, ok := intFieldPresent(params, "exit_code"); ok {
			exitCode = &value
		}
		targetTab, targetChat := firstNonEmptyString(fieldString(params, "tab_id"), tabID), firstNonEmptyString(fieldString(params, "chat_id"), chatID)
		if targetTab != tabID || targetChat != chatID {
			return nil, errors.New("external work must remain in the exact owning Workass chat")
		}
		if !h.manager.ValidateAgentOwner(ownerKey, targetChat, targetTab) {
			return nil, errors.New("no running Workass turn owns this external work request")
		}
		return h.runBackgroundAction(r, tabID, chatID, chat.BackgroundAction{
			Kind: chat.BackgroundSettleExternal, OperationID: providercontract.NormalizeOperationID(fieldString(params, "operation_id")),
			Settle: &chat.SettleExternalAction{
				WorkID: fieldString(params, "work_id"), Status: fieldString(params, "status"),
				ExitCode: exitCode, Summary: fieldString(params, "summary"),
			},
		})
	case "artifact.host", "html.host":
		if h.artifacts == nil {
			return nil, errors.New("Workass artifact hosting is unavailable")
		}
		if h.chats == nil || h.chats.providerChats == nil {
			return nil, errors.New("artifact hosting requires the durable chat actor")
		}
		cwd, err := h.chats.providerChats.AgentOwnerCWD(ownerKey, tabID, chatID)
		if err != nil {
			return nil, err
		}
		return h.artifacts.Register(artifacthost.RegisterOptions{
			BaseDir: cwd, SourcePath: fieldString(params, "source_path"),
			Entry: fieldString(params, "entry"), Label: fieldString(params, "name"),
		})
	case "agent.cancel":
		id := fieldString(params, "id")
		if id == "" {
			return nil, errors.New("subagent id is required")
		}
		if !h.manager.ValidateAgentOwner(ownerKey, chatID, tabID) {
			return nil, errors.New("no running Workass turn owns this subagent request")
		}
		return h.runBackgroundAction(r, tabID, chatID, chat.BackgroundAction{
			Kind: chat.BackgroundCancelAgent, OperationID: providercontract.NormalizeOperationID(fieldString(params, "operation_id")),
			Cancel: &chat.CancelAgentAction{WorkID: id},
		})
	case "agent.decide_permission":
		if !h.manager.ValidateAgentOwner(ownerKey, chatID, tabID) {
			return nil, errors.New("no running Workass turn owns this subagent request")
		}
		return h.runBackgroundAction(r, tabID, chatID, chat.BackgroundAction{
			Kind: chat.BackgroundAgentPermission, OperationID: providercontract.NormalizeOperationID(fieldString(params, "operation_id")),
			Permission: &chat.AgentPermissionAction{WorkID: fieldString(params, "subagent_id"), Decision: fieldString(params, "decision")},
		})
	default:
		return nil, errors.New("unknown agent control method")
	}
}

func (h *agentControlHandler) runBackgroundAction(r *http.Request, tabID, chatID string, action chat.BackgroundAction) (any, error) {
	if h == nil || h.chats == nil || h.chats.providerChats == nil {
		return nil, errors.New("background mutation requires the durable chat actor")
	}
	return h.chats.providerChats.RunBackgroundAction(r.Context(), tabID, chatID, action)
}

func stringSliceField(raw any) []string {
	values, _ := raw.([]any)
	if values == nil {
		if stringsList, ok := raw.([]string); ok {
			return append([]string(nil), stringsList...)
		}
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if text := strings.TrimSpace(toString(value)); text != "" {
			out = append(out, text)
		}
	}
	return out
}

func localRemoteAddr(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	return ip != nil && ip.IsLoopback()
}
