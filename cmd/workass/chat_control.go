package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"workass/internal/acp"
	providercontract "workass/internal/provider"
)

// chatControlCoordinator is the daemon-owned parity surface between the visible
// React controls and the injected workass-agent MCP server. It never addresses
// an implicit active tab: every read/mutation carries the immutable pair
// (tabId, chatId), and every chat-scoped operation enters the durable actor.
type chatControlCoordinator struct {
	manager       *acp.Manager
	refreshes     *sessionRefreshCoordinator
	providerChats *providerChatRuntime
}

type resolvedChatControls struct {
	ProviderID string
	ModelID    string
	BaseModel  string
	Effort     string
	ModeID     string
}

func newChatControlCoordinator(manager *acp.Manager, broadcast func(string, any), runtime *providerChatRuntime) *chatControlCoordinator {
	managerRefreshEnabled := broadcast != nil
	if broadcast == nil {
		broadcast = func(string, any) {}
	}
	coordinator := &chatControlCoordinator{
		manager:   manager,
		refreshes: newSessionRefreshCoordinator(broadcast),
	}
	coordinator.providerChats = runtime
	if manager != nil {
		if managerRefreshEnabled {
			manager.SetSessionRefreshFunc(func(payload map[string]any) {
				// Adapter corrections carry the authoritative controls. Commit
				// them before the immediate global invalidation so hydration
				// observes the corrected visible selection.
				if coordinator.providerChats != nil {
					if err := coordinator.providerChats.ApplySessionRefresh(payload); err != nil {
						// A refresh for a chat that is not actor-owned has no safe
						// generation to publish. Do not fall back to the retired
						// session mirror: the actor is the only authority here.
						return
					}
				}
				coordinator.refresh(fieldString(payload, "tabId"), fieldString(payload, "chatId"), false)
			})
		}
	}
	return coordinator
}

func (c *chatControlCoordinator) resumeQueues() {
	if c != nil && c.providerChats != nil {
		_ = c.providerChats.ResumeActors()
	}
}

func (c *chatControlCoordinator) authorize(ownerKey, parentChatID, parentTabID string) error {
	if c == nil || c.manager == nil || c.providerChats == nil {
		return errors.New("Workass chat control is unavailable")
	}
	if !c.manager.ValidateAgentOwner(ownerKey, parentChatID, parentTabID) {
		return errors.New("Workass chat control caller is not an owned ACP session")
	}
	return nil
}

func (c *chatControlCoordinator) list() (map[string]any, error) {
	if c == nil || c.providerChats == nil {
		return nil, errors.New("Workass chat actor is unavailable")
	}
	chats, err := c.providerChats.ListChats()
	if err != nil {
		return nil, err
	}
	return map[string]any{"chats": chats}, nil
}

func exactAgentTarget(params map[string]any) (string, string, error) {
	tabID, chatID := fieldString(params, "tab_id"), fieldString(params, "chat_id")
	if tabID == "" || chatID == "" {
		return "", "", errors.New("exact tab_id and chat_id are required; call workass_list_chats first")
	}
	return tabID, chatID, nil
}

func (c *chatControlCoordinator) read(params map[string]any) (map[string]any, error) {
	tabID, chatID, err := exactAgentTarget(params)
	if err != nil {
		return nil, err
	}
	limit := intField(params, "limit")
	if limit < 0 || limit > 200 {
		return nil, errors.New("limit must be between 1 and 200")
	}
	includeEvents, _ := boolField(params, "include_events")
	result, err := c.providerChats.ReadChat(tabID, chatID, limit, includeEvents)
	if err != nil {
		return nil, err
	}
	jobID, running, err := c.providerChats.Foreground(tabID, chatID)
	if err != nil {
		return nil, err
	}
	result["running"] = running
	if running {
		result["jobId"] = jobID
	}
	return result, nil
}

func (c *chatControlCoordinator) create(ctx context.Context, parentTabID, parentChatID string, params map[string]any) (map[string]any, error) {
	operationID := providercontract.NormalizeOperationID(fieldString(params, "operation_id"))
	if operationID == "" {
		return nil, errors.New("chat.create requires a stable operation_id")
	}
	parent, err := c.providerChats.ReadChat(parentTabID, parentChatID, 1, false)
	if err != nil {
		return nil, errors.New("calling chat no longer exists")
	}
	controls, err := c.resolveControls(ctx, parent, params)
	if err != nil {
		return nil, err
	}
	cwd := fieldString(params, "cwd")
	if cwd == "" || cwd == "inherit" {
		cwd = fieldString(parent, "cwd")
	}
	if err := validateChatCWD(cwd); err != nil {
		return nil, err
	}
	focus, _ := boolField(params, "focus")
	created, err := c.providerChats.CreateChat(fieldString(params, "title"), cwd, controls, focus, operationID)
	if err != nil {
		return nil, err
	}
	if _, err := c.providerChats.ReadChat(fieldString(created, "tabId"), fieldString(created, "chatId"), 1, false); err != nil {
		return nil, fmt.Errorf("created chat is not addressable: %w", err)
	}
	c.refresh(fieldString(created, "tabId"), fieldString(created, "chatId"), focus)
	created["providerId"], created["modelId"], created["effort"], created["modeId"] = controls.ProviderID, controls.BaseModel, controls.Effort, controls.ModeID
	created["resolvedModelId"] = controls.ModelID
	return created, nil
}

func (c *chatControlCoordinator) rename(params map[string]any) (map[string]any, error) {
	tabID, chatID, err := exactAgentTarget(params)
	if err != nil {
		return nil, err
	}
	operationID := providercontract.NormalizeOperationID(fieldString(params, "operation_id"))
	if operationID == "" {
		return nil, errors.New("chat.rename requires a stable operation_id")
	}
	if err := c.providerChats.RenameChat(tabID, chatID, fieldString(params, "title"), operationID); err != nil {
		return nil, err
	}
	c.refresh(tabID, chatID, false)
	return map[string]any{"ok": true, "tabId": tabID, "chatId": chatID, "title": fieldString(params, "title")}, nil
}

func (c *chatControlCoordinator) configure(ctx context.Context, params map[string]any) (map[string]any, error) {
	tabID, chatID, err := exactAgentTarget(params)
	if err != nil {
		return nil, err
	}
	current, err := c.providerChats.ReadChat(tabID, chatID, 1, false)
	if err != nil {
		return nil, err
	}
	controls, err := c.resolveControls(ctx, current, params)
	if err != nil {
		return nil, err
	}
	cwd := fieldString(params, "cwd")
	if cwd == "inherit" {
		cwd = fieldString(current, "cwd")
	}
	if cwd == "" {
		cwd = fieldString(current, "cwd")
	}
	if err := validateChatCWD(cwd); err != nil {
		return nil, err
	}
	jobID, running, err := c.providerChats.Foreground(tabID, chatID)
	if err != nil {
		return nil, err
	}
	providerChanged := controls.ProviderID != "" && controls.ProviderID != fieldString(current, "providerId")
	cwdChanged := cwd != fieldString(current, "cwd")
	if running && (providerChanged || cwdChanged) {
		return nil, fmt.Errorf("cannot change provider or cwd while job %s is running", jobID)
	}
	operationID := providercontract.NormalizeOperationID(fieldString(params, "operation_id"))
	if operationID == "" {
		return nil, errors.New("chat.configure requires a stable operation_id")
	}
	if err := c.providerChats.ConfigureChat(ctx, tabID, chatID, cwd, controls, operationID); err != nil {
		return nil, err
	}
	c.refresh(tabID, chatID, false)
	return map[string]any{
		"ok": true, "tabId": tabID, "chatId": chatID, "cwd": cwd,
		"providerId": controls.ProviderID, "modelId": controls.BaseModel, "effort": controls.Effort,
		"resolvedModelId": controls.ModelID, "modeId": controls.ModeID,
	}, nil
}

func (c *chatControlCoordinator) focus(params map[string]any) (map[string]any, error) {
	tabID, chatID, err := exactAgentTarget(params)
	if err != nil {
		return nil, err
	}
	operationID := providercontract.NormalizeOperationID(fieldString(params, "operation_id"))
	if operationID == "" {
		return nil, errors.New("chat.focus requires a stable operation_id")
	}
	if err := c.providerChats.FocusChat(tabID, chatID, operationID); err != nil {
		return nil, err
	}
	c.refresh(tabID, chatID, true)
	return map[string]any{"ok": true, "tabId": tabID, "chatId": chatID, "focused": true}, nil
}

func (c *chatControlCoordinator) delete(params map[string]any) (map[string]any, error) {
	tabID, chatID, err := exactAgentTarget(params)
	if err != nil {
		return nil, err
	}
	force, _ := boolField(params, "force")
	if jobID, running, foregroundErr := c.providerChats.Foreground(tabID, chatID); foregroundErr != nil {
		return nil, foregroundErr
	} else if running {
		if !force {
			return nil, fmt.Errorf("chat has running job %s; pass force only when cancellation and deletion are intended", jobID)
		}
		if jobID == "" {
			return nil, errors.New("chat foreground turn is not yet cancellable")
		}
		if _, handled, cancelErr := c.providerChats.Cancel(context.Background(), jobID); cancelErr != nil || !handled {
			if cancelErr != nil {
				return nil, cancelErr
			}
			return nil, errors.New("running chat cancellation is not actor-owned")
		}
	}
	if err := c.providerChats.DeleteChat(context.Background(), tabID, chatID, providercontract.NormalizeOperationID(fieldString(params, "operation_id")), force); err != nil {
		return nil, err
	}
	c.refresh(tabID, chatID, false)
	return map[string]any{"ok": true, "tabId": tabID, "chatId": chatID, "deleted": true}, nil
}

func (c *chatControlCoordinator) send(params map[string]any) (map[string]any, error) {
	tabID, chatID, err := exactAgentTarget(params)
	if err != nil {
		return nil, err
	}
	delivery := fieldString(params, "delivery")
	if delivery == "" {
		delivery = "auto"
	}
	if delivery != "auto" && delivery != "queue" && delivery != "steer" {
		return nil, errors.New("delivery must be auto, queue, or steer")
	}
	_, running, err := c.providerChats.Foreground(tabID, chatID)
	if err != nil {
		return nil, err
	}
	if delivery == "steer" && !running {
		return nil, errors.New("cannot steer an idle chat; use auto or queue")
	}
	receipt, err := c.providerChats.QueueAgentMessage(
		context.Background(), tabID, chatID,
		providercontract.NormalizeOperationID(fieldString(params, "operation_id")),
		fieldString(params, "message"), delivery,
	)
	if err != nil {
		return nil, err
	}
	c.refresh(tabID, chatID, false)
	return receipt, nil
}

func (c *chatControlCoordinator) cancel(params map[string]any) (map[string]any, error) {
	tabID, chatID, err := exactAgentTarget(params)
	if err != nil {
		return nil, err
	}
	jobID, running, err := c.providerChats.Foreground(tabID, chatID)
	if err != nil {
		return nil, err
	}
	if !running {
		return map[string]any{"ok": false, "tabId": tabID, "chatId": chatID, "cancelled": false, "reason": "idle"}, nil
	}
	if jobID == "" {
		return nil, errors.New("chat foreground turn is not yet cancellable")
	}
	result, handled, cancelErr := c.providerChats.Cancel(context.Background(), jobID)
	if cancelErr != nil {
		return nil, cancelErr
	}
	if !handled {
		return nil, errors.New("running chat cancellation is not actor-owned")
	}
	ok := result.Cancelled
	return map[string]any{"ok": ok, "tabId": tabID, "chatId": chatID, "jobId": jobID, "cancelled": ok}, nil
}

func (c *chatControlCoordinator) refresh(tabID, chatID string, focus bool) {
	if c == nil || c.refreshes == nil {
		return
	}
	if c.providerChats == nil {
		return
	}
	state, ok := c.providerChats.Snapshot(chatID)
	if !ok {
		return
	}
	c.refreshes.Request(tabID, chatID, state.Revision, refreshImmediate)
}

func (c *chatControlCoordinator) resolveControls(ctx context.Context, current, params map[string]any) (resolvedChatControls, error) {
	providerID := firstNonEmptyString(fieldString(params, "provider_id"), fieldString(current, "providerId"))
	catalog := c.manager.Catalog(ctx)
	groups, _ := catalog["groups"].([]acp.CatalogGroup)
	var group *acp.CatalogGroup
	for i := range groups {
		if groups[i].ProviderID == providerID {
			group = &groups[i]
			break
		}
	}
	if group == nil || group.Status != "ready" {
		return resolvedChatControls{}, fmt.Errorf("provider %q is not ready; call workass_agent_catalog", providerID)
	}
	requestedModel := firstNonEmptyString(fieldString(params, "model_id"), fieldString(current, "modelId"))
	baseModel, inheritedEffort := splitCatalogSelection(requestedModel, group.Models)
	if explicit := fieldString(params, "model_id"); explicit != "" {
		baseModel, inheritedEffort = splitCatalogSelection(explicit, group.Models)
	}
	var model *acp.Model
	for i := range group.Models {
		if group.Models[i].ModelID == baseModel {
			model = &group.Models[i]
			break
		}
	}
	if model == nil {
		return resolvedChatControls{}, fmt.Errorf("model %q is not available for provider %q; call workass_agent_catalog", requestedModel, providerID)
	}
	effort := firstNonEmptyString(fieldString(params, "effort"), inheritedEffort)
	if len(model.Efforts) > 0 {
		if effort == "" {
			effort = preferredChatEffort(model.Efforts)
		}
		if !containsString(model.Efforts, effort) {
			return resolvedChatControls{}, fmt.Errorf("effort %q is not available for model %q", effort, baseModel)
		}
	} else if effort != "" {
		return resolvedChatControls{}, fmt.Errorf("model %q does not expose effort controls", baseModel)
	}
	modeID := fieldString(params, "mode_id")
	intent := fieldString(params, "permission_intent")
	if modeID != "" && intent != "" {
		return resolvedChatControls{}, errors.New("choose mode_id or permission_intent, not both")
	}
	if intent != "" {
		if intent != "read" && intent != "edit" && intent != "full" {
			return resolvedChatControls{}, errors.New("permission_intent must be read, edit, or full")
		}
		modeID = acp.PermissionIntentModes(providerID, group.Modes)[intent]
		if modeID == "" {
			return resolvedChatControls{}, fmt.Errorf("provider %q does not expose a %s permission mode", providerID, intent)
		}
	}
	if modeID == "" {
		candidate := fieldString(current, "modeId")
		if containsMode(group.Modes, candidate) {
			modeID = candidate
		} else if inheritedIntent := acp.PermissionIntentForMode(fieldString(current, "providerId"), candidate); inheritedIntent != "" {
			modeID = acp.PermissionIntentModes(providerID, group.Modes)[inheritedIntent]
		}
		if modeID == "" && len(group.Modes) > 0 {
			modeID = group.Modes[0].ID
		}
	}
	if modeID != "" && !containsMode(group.Modes, modeID) {
		return resolvedChatControls{}, fmt.Errorf("mode %q is not available for provider %q", modeID, providerID)
	}
	resolvedModel := baseModel
	if effort != "" {
		resolvedModel += "[" + effort + "]"
	}
	return resolvedChatControls{ProviderID: providerID, ModelID: resolvedModel, BaseModel: baseModel, Effort: effort, ModeID: modeID}, nil
}

func splitCatalogSelection(selection string, models []acp.Model) (string, string) {
	selection = strings.TrimSpace(selection)
	for _, model := range models {
		if selection == model.ModelID {
			return model.ModelID, ""
		}
		for _, effort := range model.Efforts {
			if selection == model.ModelID+"["+effort+"]" {
				return model.ModelID, effort
			}
		}
	}
	return selection, ""
}

func preferredChatEffort(efforts []string) string {
	for _, effort := range efforts {
		if effort == "high" {
			return effort
		}
	}
	if len(efforts) > 0 {
		return efforts[len(efforts)-1]
	}
	return ""
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsMode(modes []acp.Mode, target string) bool {
	for _, mode := range modes {
		if mode.ID == target {
			return true
		}
	}
	return false
}

func validateChatCWD(cwd string) error {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return nil
	}
	if !filepath.IsAbs(cwd) {
		return errors.New("cwd must be an absolute server path")
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("cwd is not an accessible server directory: %s", cwd)
	}
	return nil
}
