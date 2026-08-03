package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workass/internal/acp"
	"workass/internal/artifacthost"
)

const agentControlPath = "/workass/agent-control"

type agentControlHandler struct {
	manager   *acp.Manager
	chats     *chatControlCoordinator
	state     *sessionStore
	artifacts *artifacthost.Registry
	token     string
}

type agentControlRequest struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

func newAgentControlHandler(manager *acp.Manager, state *sessionStore, broadcast func(string, any), url, descriptorPath string, coordinators ...*chatControlCoordinator) (*agentControlHandler, error) {
	if manager == nil {
		return nil, errors.New("agent control requires an ACP manager")
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	descriptor := browserControlDescriptor{Version: 1, URL: strings.TrimSpace(url), Token: token}
	data, err := json.Marshal(descriptor)
	if err != nil {
		return nil, err
	}
	if err := writeAgentControlDescriptor(descriptorPath, data); err != nil {
		return nil, err
	}
	var chats *chatControlCoordinator
	if len(coordinators) > 0 {
		chats = coordinators[0]
	}
	if chats == nil {
		chats = newChatControlCoordinator(manager, state, broadcast)
	}
	handler := &agentControlHandler{manager: manager, chats: chats, state: state, token: token}
	chats.resumeQueues()
	return handler, nil
}

func writeAgentControlDescriptor(path string, data []byte) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("agent control descriptor path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".agent-control-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Windows os.Rename does not replace an existing file. The descriptor is a
	// transient daemon-owned capability and is rewritten on every healthy boot.
	_ = os.Remove(path)
	return os.Rename(tmpName, path)
}

func (h *agentControlHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL == nil || r.URL.Path != agentControlPath || r.Method != http.MethodPost || !localRemoteAddr(r.RemoteAddr) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if len(provided) != len(h.token) || subtle.ConstantTimeCompare([]byte(provided), []byte(h.token)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(browserControlReply{Error: "unauthorized"})
		return
	}
	var request agentControlRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 4*1024*1024))
	dec.UseNumber()
	if err := dec.Decode(&request); err != nil {
		_ = json.NewEncoder(w).Encode(browserControlReply{Error: "invalid agent control request"})
		return
	}
	result, err := h.call(r, request)
	if err != nil {
		_ = json.NewEncoder(w).Encode(browserControlReply{Error: acp.RedactSensitiveText(err.Error())})
		return
	}
	_ = json.NewEncoder(w).Encode(browserControlReply{Result: redactSessionValue(result)})
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
			return h.chats.list(), nil
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
		rootJobIDHint := ""
		if h.state != nil {
			rootJobIDHint = h.state.MostRecentVisibleAssistantJobID(tabID, chatID)
		}
		return h.manager.SpawnSubagent(r.Context(), acp.SubagentSpawnOptions{
			OwnerKey:         ownerKey,
			ParentChatID:     chatID,
			ParentTabID:      tabID,
			RootJobIDHint:    rootJobIDHint,
			Prompt:           fieldString(params, "prompt"),
			Label:            fieldString(params, "label"),
			ProviderID:       fieldString(params, "provider_id"),
			ModelID:          fieldString(params, "model_id"),
			Effort:           fieldString(params, "effort"),
			ModeID:           fieldString(params, "mode_id"),
			CWD:              fieldString(params, "cwd"),
			Profile:          fieldString(params, "profile"),
			PermissionIntent: fieldString(params, "permission_intent"),
		})
	case "agent.list":
		if !h.manager.ValidateAgentOwner(ownerKey, chatID, tabID) {
			return nil, errors.New("no running Workass turn owns this subagent request")
		}
		return map[string]any{"subagents": h.manager.ListSubagents(ownerKey, chatID, tabID)}, nil
	case "agent.wait":
		id := fieldString(params, "id")
		if id == "" {
			return nil, errors.New("subagent id is required")
		}
		timeoutMS := intField(params, "timeout_ms")
		if timeoutMS != 0 && (timeoutMS < 1000 || timeoutMS > 3600000) {
			return nil, errors.New("subagent wait timeout_ms must be between 1000 and 3600000")
		}
		if !h.manager.ValidateAgentOwner(ownerKey, chatID, tabID) {
			return nil, errors.New("no running Workass turn owns this subagent request")
		}
		timeout := time.Duration(timeoutMS) * time.Millisecond
		return h.manager.WaitSubagent(r.Context(), ownerKey, chatID, tabID, id, timeout)
	case "agent.wait_many":
		ids := stringSliceField(params["ids"])
		if len(ids) == 0 {
			return nil, errors.New("at least one subagent id is required")
		}
		timeoutMS := intField(params, "timeout_ms")
		if timeoutMS != 0 && (timeoutMS < 1000 || timeoutMS > 3600000) {
			return nil, errors.New("subagent wait timeout_ms must be between 1000 and 3600000")
		}
		if !h.manager.ValidateAgentOwner(ownerKey, chatID, tabID) {
			return nil, errors.New("no running Workass turn owns this subagent request")
		}
		return h.manager.WaitSubagents(r.Context(), ownerKey, chatID, tabID, ids, fieldString(params, "return_when"), time.Duration(timeoutMS)*time.Millisecond)
	case "agent.message":
		if !h.manager.ValidateAgentOwner(ownerKey, chatID, tabID) {
			return nil, errors.New("no running Workass turn owns this subagent request")
		}
		return h.manager.MessageSubagent(ownerKey, chatID, tabID, fieldString(params, "id"), fieldString(params, "message"))
	case "agent.retry":
		if !h.manager.ValidateAgentOwner(ownerKey, chatID, tabID) {
			return nil, errors.New("no running Workass turn owns this subagent request")
		}
		return h.manager.RetrySubagent(r.Context(), ownerKey, chatID, tabID, fieldString(params, "id"), fieldString(params, "message"))
	case "agent.receipts":
		if !h.manager.ValidateAgentOwner(ownerKey, chatID, tabID) {
			return nil, errors.New("no running Workass turn owns this subagent request")
		}
		return map[string]any{"receipts": h.manager.ListSubagentReceipts(ownerKey, chatID, tabID, intField(params, "limit"))}, nil
	case "spawned_work.list":
		tailChars := intField(params, "tail_chars")
		if tailChars < 0 || tailChars > 12000 {
			return nil, errors.New("spawned work tail_chars must be between 0 and 12000")
		}
		items, err := h.manager.ListSpawnedWorkForOwner(
			ownerKey, chatID, tabID, fieldString(params, "chat_id"), fieldString(params, "tab_id"), tailChars,
		)
		if err != nil {
			return nil, err
		}
		return map[string]any{"items": items}, nil
	case "spawned_work.receipts":
		receipts, err := h.manager.ListSpawnedWorkReceipts(
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
		return h.manager.RegisterExternalWork(acp.ExternalWorkRegistrationOptions{
			OwnerKey: ownerKey, ParentChatID: chatID, ParentTabID: tabID,
			TabID: fieldString(params, "tab_id"), ChatID: fieldString(params, "chat_id"),
			Label: fieldString(params, "label"), Role: fieldString(params, "role"), PID: pid,
			OutputFile: fieldString(params, "output_file"), DoneFile: fieldString(params, "done_file"),
		})
	case "obligation.get":
		obligationTab := firstNonEmptyString(fieldString(params, "tab_id"), tabID)
		obligationChat := firstNonEmptyString(fieldString(params, "chat_id"), chatID)
		if !h.manager.ValidateAgentOwner(ownerKey, obligationChat, obligationTab) {
			return nil, errors.New("no running Workass turn owns this obligation request")
		}
		return map[string]any{"ok": true, "obligation": h.manager.ObligationFor(obligationTab, obligationChat)}, nil
	case "external.settle":
		var exitCode *int
		if value, ok := intFieldPresent(params, "exit_code"); ok {
			exitCode = &value
		}
		return h.manager.SettleExternalWork(acp.ExternalWorkSettleOptions{
			OwnerKey: ownerKey, ParentChatID: chatID, ParentTabID: tabID,
			TabID: fieldString(params, "tab_id"), ChatID: fieldString(params, "chat_id"),
			WorkID: fieldString(params, "work_id"), Status: fieldString(params, "status"),
			ExitCode: exitCode, Summary: fieldString(params, "summary"),
		})
	case "artifact.host", "html.host":
		if h.artifacts == nil {
			return nil, errors.New("Workass artifact hosting is unavailable")
		}
		cwd, err := h.manager.AgentOwnerCWD(ownerKey, chatID, tabID)
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
		if h.manager.CancelSubagent(ownerKey, chatID, tabID, id) {
			return map[string]any{"ok": true}, nil
		}
		// A bare false reads as "cancellation is broken" when the usual cause is
		// that the run is gone: the registry is in memory, so a daemon restart
		// takes every running subagent with it and leaves no receipt either.
		return map[string]any{"ok": false, "reason": "no addressable running subagent with that id in this chat; it may have already ended, or been lost to a daemon restart"}, nil
	case "agent.decide_permission":
		if !h.manager.ValidateAgentOwner(ownerKey, chatID, tabID) {
			return nil, errors.New("no running Workass turn owns this subagent request")
		}
		return h.manager.DecideSubagentPermission(ownerKey, chatID, tabID,
			fieldString(params, "subagent_id"), fieldString(params, "decision"))
	default:
		return nil, errors.New("unknown agent control method")
	}
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
