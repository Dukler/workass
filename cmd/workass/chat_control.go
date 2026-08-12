package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"workass/internal/acp"
	"workass/internal/chat"
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
	// The durable actor pair is the identity fence. Manager capabilities are
	// disposable execution state and must not be consulted for a stale pair.
	if _, _, err := c.providerChats.exactActor(parentTabID, parentChatID); err != nil {
		return err
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

func requiredChatOperation(params map[string]any, method string) (providercontract.OperationID, error) {
	operationID, err := providercontract.ValidateOperationID(fieldString(params, "operation_id"))
	if err != nil {
		return "", fmt.Errorf("chat.%s requires a valid caller-stable operation_id", method)
	}
	return operationID, nil
}

// validateMutationTarget is the fail-first boundary for agent chat control.
// It validates the stable logical operation and the durable actor identity
// before catalog lookup, foreground inspection, cancellation, or provider
// execution. The actor-owned mutation methods retain responsibility for
// receipt readback and concurrent revision checks after this boundary.
func (c *chatControlCoordinator) validateMutationTarget(tabID, chatID string, operationID providercontract.OperationID, method string) error {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if tabID == "" || chatID == "" {
		return errors.New("exact tab_id and chat_id are required; call workass_list_chats first")
	}
	if providercontract.NormalizeOperationID(string(operationID)) == "" {
		return fmt.Errorf("chat.%s requires a stable operation_id", method)
	}
	if c == nil || c.providerChats == nil {
		return errors.New("Workass chat actor is unavailable")
	}
	if _, _, err := c.providerChats.exactActor(tabID, chatID); err != nil {
		return err
	}
	return nil
}

func (c *chatControlCoordinator) validateMutation(params map[string]any, method string) (string, string, providercontract.OperationID, error) {
	tabID, chatID, err := exactAgentTarget(params)
	if err != nil {
		return "", "", "", err
	}
	operationID, err := requiredChatOperation(params, method)
	if err != nil {
		return "", "", "", err
	}
	if err := c.validateMutationTarget(tabID, chatID, operationID, method); err != nil {
		return "", "", "", err
	}
	return tabID, chatID, operationID, nil
}

func cancelReceiptForOperation(state chat.State, operationID providercontract.OperationID) (chat.OutboxEntry, bool) {
	for _, entry := range state.Outbox {
		if entry.Kind == chat.EffectCancelTurn && entry.OperationID == operationID {
			return entry, true
		}
	}
	return chat.OutboxEntry{}, false
}

func cancelResultFromReceipt(entry chat.OutboxEntry) (acp.JobCancelResult, bool) {
	switch entry.Status {
	case chat.OutboxAccepted, chat.OutboxConsumed, chat.OutboxCompleted:
		return acp.JobCancelResult{Cancelled: true, Reason: "cancelled"}, true
	case chat.OutboxAmbiguous:
		return acp.JobCancelResult{Cancelled: false, Reason: "uncertain"}, true
	case chat.OutboxFailed:
		return acp.JobCancelResult{Cancelled: false, Reason: "not-owned"}, true
	default:
		return acp.JobCancelResult{Cancelled: false, Reason: "pending"}, false
	}
}

func cancelResultFromActorReceipt(receipt chat.CancelMutationReceipt) acp.JobCancelResult {
	return acp.JobCancelResult{Cancelled: receipt.Cancelled, Reason: receipt.Reason}
}

// cancelChatTurn is the actor-backed chat.cancel boundary. The caller's
// operation id is the durable CancelTurn identity; native job ids are only
// receipts. A terminal receipt is read back without re-draining the provider,
// and reusing that operation for a different foreground turn is rejected.
func (r *providerChatRuntime) cancelChatTurn(ctx context.Context, tabID, chatID string, operationID providercontract.OperationID) (string, acp.JobCancelResult, bool, error) {
	operationID = providercontract.NormalizeOperationID(string(operationID))
	if operationID == "" {
		return "", acp.JobCancelResult{}, true, errors.New("chat.cancel requires a stable operation_id")
	}
	actor, _, err := r.exactActor(tabID, chatID)
	if err != nil {
		return "", acp.JobCancelResult{}, false, err
	}
	actor.mu.Lock()
	defer actor.mu.Unlock()

	state := actor.engine.Snapshot()
	if receipt, ok := state.CancelMutationReceipts[operationID]; ok {
		// A terminal actor receipt wins over current foreground state. This is the
		// idempotency fence for an idle cancel that was acknowledged before a
		// later foreground turn began.
		return "", cancelResultFromActorReceipt(receipt), true, nil
	}
	receipt, hasReceipt := cancelReceiptForOperation(state, operationID)
	if hasReceipt {
		jobID := strings.TrimSpace(receipt.Turn.NativeID)
		if jobID == "" {
			return "", acp.JobCancelResult{}, true, errors.New("chat cancel receipt is missing its native turn id")
		}
		if state.Foreground != nil && state.Foreground.Turn != receipt.Turn {
			return "", acp.JobCancelResult{}, true, errors.New("chat cancel operation id was reused for a different foreground turn")
		}
		if result, done := cancelResultFromReceipt(receipt); done {
			return jobID, result, true, nil
		}
		if state.PendingCancel == nil || state.PendingCancel.OperationID != operationID || state.PendingCancel.Turn != receipt.Turn {
			return jobID, acp.JobCancelResult{Cancelled: false, Reason: "pending"}, true, nil
		}
		if err := actor.coordinator.Drain(ctx); err != nil {
			return jobID, acp.JobCancelResult{}, true, err
		}
		state = actor.engine.Snapshot()
		receipt, ok := cancelReceiptForOperation(state, operationID)
		if !ok {
			return jobID, acp.JobCancelResult{}, true, errors.New("chat cancel receipt disappeared from actor state")
		}
		if result, done := cancelResultFromReceipt(receipt); done {
			return jobID, result, true, nil
		}
		return jobID, acp.JobCancelResult{Cancelled: false, Reason: "pending"}, true, nil
	}
	if _, used := state.Operations[operationID]; used {
		return "", acp.JobCancelResult{}, true, errors.New("chat cancel operation id was reused for a different chat action")
	}
	if state.Foreground == nil {
		if err := actor.engine.Apply(chat.RecordCancelReceipt{
			OperationID: operationID, Cancelled: false, Reason: "idle",
		}); err != nil {
			return "", acp.JobCancelResult{}, true, err
		}
		state = actor.engine.Snapshot()
		idleReceipt, ok := state.CancelMutationReceipts[operationID]
		if !ok {
			return "", acp.JobCancelResult{}, true, errors.New("idle cancel did not produce an actor receipt")
		}
		return "", cancelResultFromActorReceipt(idleReceipt), true, nil
	}
	jobID := strings.TrimSpace(state.Foreground.Turn.NativeID)
	if jobID == "" {
		return "", acp.JobCancelResult{}, true, errors.New("chat foreground turn is not yet cancellable")
	}
	if err := actor.engine.Apply(chat.CancelTurn{OperationID: operationID}); err != nil {
		return jobID, acp.JobCancelResult{}, true, err
	}
	if err := actor.coordinator.Drain(ctx); err != nil {
		return jobID, acp.JobCancelResult{}, true, err
	}
	state = actor.engine.Snapshot()
	receipt, ok := cancelReceiptForOperation(state, operationID)
	if !ok {
		return jobID, acp.JobCancelResult{}, true, errors.New("chat cancel did not produce an actor receipt")
	}
	if result, done := cancelResultFromReceipt(receipt); done {
		return jobID, result, true, nil
	}
	return jobID, acp.JobCancelResult{Cancelled: false, Reason: "pending"}, true, nil
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
	operationID, err := requiredChatOperation(params, "create")
	if err != nil {
		return nil, err
	}
	if err := c.validateMutationTarget(parentTabID, parentChatID, operationID, "create"); err != nil {
		return nil, err
	}
	focus, _ := boolField(params, "focus")
	intentDigest, err := headlessCreateIntentDigest(parentTabID, parentChatID, params)
	if err != nil {
		return nil, err
	}
	if existing, found, err := c.providerChats.existingHeadlessCreate(operationID, intentDigest, focus); found {
		if err != nil {
			return nil, err
		}
		c.refresh(fieldString(existing, "tabId"), fieldString(existing, "chatId"), focus)
		return existing, nil
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
	created, err := c.providerChats.CreateChatWithIntentDigest(fieldString(params, "title"), cwd, controls, focus, operationID, intentDigest)
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

func headlessCreateIntentDigest(parentTabID, parentChatID string, params map[string]any) (string, error) {
	cwd := strings.TrimSpace(fieldString(params, "cwd"))
	if cwd == "" || cwd == "inherit" {
		cwd = "inherit"
	}
	focus, _ := boolField(params, "focus")
	payload := struct {
		Version          string `json:"version"`
		ParentTabID      string `json:"parentTabId"`
		ParentChatID     string `json:"parentChatId"`
		Title            string `json:"title"`
		CWD              string `json:"cwd"`
		ProviderID       string `json:"providerId"`
		ModelID          string `json:"modelId"`
		Effort           string `json:"effort"`
		ModeID           string `json:"modeId"`
		PermissionIntent string `json:"permissionIntent"`
		Focus            bool   `json:"focus"`
	}{
		Version: "headless-create-v1", ParentTabID: strings.TrimSpace(parentTabID), ParentChatID: strings.TrimSpace(parentChatID),
		Title: normalizedHeadlessChatTitle(fieldString(params, "title")), CWD: cwd,
		ProviderID: fieldString(params, "provider_id"), ModelID: fieldString(params, "model_id"), Effort: fieldString(params, "effort"),
		ModeID: fieldString(params, "mode_id"), PermissionIntent: fieldString(params, "permission_intent"), Focus: focus,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return fmt.Sprintf("headless-create-v1:%x", digest[:]), nil
}

func (c *chatControlCoordinator) rename(params map[string]any) (map[string]any, error) {
	tabID, chatID, operationID, err := c.validateMutation(params, "rename")
	if err != nil {
		return nil, err
	}
	if err := c.providerChats.RenameChat(tabID, chatID, fieldString(params, "title"), operationID); err != nil {
		return nil, err
	}
	c.refresh(tabID, chatID, false)
	return map[string]any{"ok": true, "tabId": tabID, "chatId": chatID, "title": fieldString(params, "title")}, nil
}

func (c *chatControlCoordinator) configure(ctx context.Context, params map[string]any) (map[string]any, error) {
	tabID, chatID, operationID, err := c.validateMutation(params, "configure")
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
	tabID, chatID, operationID, err := c.validateMutation(params, "focus")
	if err != nil {
		return nil, err
	}
	if err := c.providerChats.FocusChat(tabID, chatID, operationID); err != nil {
		return nil, err
	}
	c.refresh(tabID, chatID, true)
	return map[string]any{"ok": true, "tabId": tabID, "chatId": chatID, "focused": true}, nil
}

func (c *chatControlCoordinator) delete(params map[string]any) (map[string]any, error) {
	tabID, chatID, operationID, err := c.validateMutation(params, "delete")
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
	if err := c.providerChats.DeleteChat(context.Background(), tabID, chatID, operationID, force); err != nil {
		return nil, err
	}
	c.refresh(tabID, chatID, false)
	return map[string]any{"ok": true, "tabId": tabID, "chatId": chatID, "deleted": true}, nil
}

func (c *chatControlCoordinator) send(params map[string]any) (map[string]any, error) {
	tabID, chatID, operationID, err := c.validateMutation(params, "send")
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
		operationID,
		fieldString(params, "message"), delivery,
	)
	if err != nil {
		return nil, err
	}
	c.refresh(tabID, chatID, false)
	return receipt, nil
}

func (c *chatControlCoordinator) cancel(params map[string]any) (map[string]any, error) {
	tabID, chatID, operationID, err := c.validateMutation(params, "cancel")
	if err != nil {
		return nil, err
	}
	jobID, result, handled, cancelErr := c.providerChats.cancelChatTurn(context.Background(), tabID, chatID, operationID)
	if cancelErr != nil {
		return nil, cancelErr
	}
	if !handled {
		return nil, errors.New("running chat cancellation is not actor-owned")
	}
	ok := result.Cancelled
	response := map[string]any{"ok": ok, "tabId": tabID, "chatId": chatID, "cancelled": ok, "operationId": string(operationID)}
	if jobID != "" {
		response["jobId"] = jobID
	}
	if result.Reason != "" {
		response["reason"] = result.Reason
	}
	return response, nil
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
