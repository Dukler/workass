package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"workass/internal/acp"
)

// chatControlCoordinator is the daemon-owned parity surface between the visible
// React controls and the injected workass-agent MCP server. It never addresses
// an implicit active tab: every read/mutation carries the immutable pair
// (tabId, chatId), and the session store remains the canonical UI authority.
type chatControlCoordinator struct {
	manager   *acp.Manager
	state     *sessionStore
	refreshes *sessionRefreshCoordinator

	mu           sync.Mutex
	draining     map[string]bool
	drainPending map[string]bool
	drainTimers  map[string]*time.Timer
	drainBackoff map[string]time.Duration

	queueStartTimeout       time.Duration
	queueRetryBase          time.Duration
	rendererRecheckBase     time.Duration
	rendererRecheckMax      time.Duration
	rendererAdoptionAge     time.Duration
	startQueuedTurnOverride func(context.Context, string, string, map[string]any) error
}

const (
	agentQueueStartTimeout      = 2 * time.Minute
	agentQueueRetryBase         = time.Second
	rendererQueueRecheckBase    = time.Second
	rendererQueueRecheckMax     = 30 * time.Second
	rendererQueueAdoptionAge    = 60 * time.Second
	agentQueueStartAttemptLimit = 3
)

type resolvedChatControls struct {
	ProviderID string
	ModelID    string
	BaseModel  string
	Effort     string
	ModeID     string
}

func newChatControlCoordinator(manager *acp.Manager, state *sessionStore, broadcast func(string, any)) *chatControlCoordinator {
	managerRefreshEnabled := broadcast != nil
	if broadcast == nil {
		broadcast = func(string, any) {}
	}
	coordinator := &chatControlCoordinator{
		manager: manager, state: state,
		refreshes: newSessionRefreshCoordinator(broadcast),
		draining:  map[string]bool{}, drainPending: map[string]bool{},
		drainTimers: map[string]*time.Timer{}, drainBackoff: map[string]time.Duration{},
		queueStartTimeout: agentQueueStartTimeout, queueRetryBase: agentQueueRetryBase,
		rendererRecheckBase: rendererQueueRecheckBase, rendererRecheckMax: rendererQueueRecheckMax,
		rendererAdoptionAge: rendererQueueAdoptionAge,
	}
	if state != nil {
		state.SetQueueWakeFunc(coordinator.wakeDrain)
	}
	if manager != nil {
		manager.SetJobEndFunc(coordinator.wakeDrain)
		if managerRefreshEnabled {
			manager.SetSessionRefreshFunc(func(payload map[string]any) {
				// Adapter corrections carry the authoritative controls. Commit
				// them before the immediate global invalidation so hydration
				// observes the corrected visible selection.
				persistAgentApplyControls(state, payload)
				coordinator.refreshes.Request(
					fieldString(payload, "tabId"), fieldString(payload, "chatId"),
					state.RefreshGeneration(), refreshImmediate,
				)
			})
		}
	}
	return coordinator
}

func (c *chatControlCoordinator) resumeQueues() {
	for _, target := range c.state.AgentQueueTargets() {
		c.scheduleDrain(target[0], target[1])
	}
}

func (c *chatControlCoordinator) authorize(ownerKey, parentChatID, parentTabID string) error {
	if c == nil || c.manager == nil || c.state == nil {
		return errors.New("Workass chat control is unavailable")
	}
	if !c.manager.ValidateAgentOwner(ownerKey, parentChatID, parentTabID) {
		return errors.New("Workass chat control caller is not an owned ACP session")
	}
	return nil
}

func (c *chatControlCoordinator) list() map[string]any {
	chats := c.state.AgentChatList()
	for _, chat := range chats {
		job, running := c.manager.RunningJobForChat(fieldString(chat, "tabId"), fieldString(chat, "chatId"))
		chat["running"] = running
		if running {
			chat["jobId"] = fieldString(job, "id")
		}
	}
	return map[string]any{"chats": chats}
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
	result, err := c.state.AgentReadChat(tabID, chatID, limit, includeEvents)
	if err != nil {
		return nil, err
	}
	job, running := c.manager.RunningJobForChat(tabID, chatID)
	result["running"] = running
	if running {
		result["jobId"] = fieldString(job, "id")
	}
	return result, nil
}

func (c *chatControlCoordinator) create(ctx context.Context, parentTabID, parentChatID string, params map[string]any) (map[string]any, error) {
	parent, err := c.state.AgentReadChat(parentTabID, parentChatID, 1, false)
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
	created, err := c.state.AgentCreateChat(fieldString(params, "title"), cwd, controls.ProviderID, controls.ModelID, controls.ModeID, focus)
	if err != nil {
		return nil, err
	}
	if err := c.state.AgentConfigureChat(
		fieldString(created, "tabId"), fieldString(created, "chatId"), cwd,
		controls.ProviderID, controls.ModelID, controls.BaseModel, controls.Effort, controls.ModeID,
	); err != nil {
		return nil, err
	}
	if _, err := c.state.AgentReadChat(fieldString(created, "tabId"), fieldString(created, "chatId"), 1, false); err != nil {
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
	if err := c.state.AgentRenameChat(tabID, chatID, fieldString(params, "title")); err != nil {
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
	current, err := c.state.AgentReadChat(tabID, chatID, 1, false)
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
	job, running := c.manager.RunningJobForChat(tabID, chatID)
	providerChanged := controls.ProviderID != "" && controls.ProviderID != fieldString(current, "providerId")
	cwdChanged := cwd != fieldString(current, "cwd")
	if running && (providerChanged || cwdChanged) {
		return nil, fmt.Errorf("cannot change provider or cwd while job %s is running", fieldString(job, "id"))
	}
	if err := c.state.AgentConfigureChat(tabID, chatID, cwd, controls.ProviderID, controls.ModelID, controls.BaseModel, controls.Effort, controls.ModeID); err != nil {
		return nil, err
	}
	for _, live := range c.manager.LiveSessions() {
		if live.TabID != tabID || live.ChatID != chatID {
			continue
		}
		if providerChanged || cwdChanged {
			c.manager.CloseSession(context.Background(), live.Info.SessionID)
			continue
		}
		if controls.ModelID != "" && stringPointerValue(live.Info.CurrentModelID) != controls.ModelID {
			if _, setErr := c.manager.SetModel(ctx, live.Info.SessionID, controls.ModelID); setErr != nil {
				return nil, setErr
			}
		}
		if controls.ModeID != "" && stringPointerValue(live.Info.CurrentModeID) != controls.ModeID {
			if _, setErr := c.manager.SetMode(ctx, live.Info.SessionID, controls.ModeID); setErr != nil {
				return nil, setErr
			}
		}
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
	if err := c.state.AgentFocusChat(tabID, chatID); err != nil {
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
	if job, running := c.manager.RunningJobForChat(tabID, chatID); running {
		if !force {
			return nil, fmt.Errorf("chat has running job %s; pass force only when cancellation and deletion are intended", fieldString(job, "id"))
		}
		c.manager.CancelJob(fieldString(job, "id"))
	}
	c.manager.ForgetChat(context.Background(), tabID, chatID)
	if err := c.state.AgentDeleteChat(tabID, chatID); err != nil {
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
	_, running := c.manager.RunningJobForChat(tabID, chatID)
	if delivery == "steer" && !running {
		return nil, errors.New("cannot steer an idle chat; use auto or queue")
	}
	receipt, err := c.state.AgentEnqueueChat(tabID, chatID, fieldString(params, "message"), delivery)
	if err != nil {
		return nil, err
	}
	queueID := fieldString(receipt, "queueId")
	c.refresh(tabID, chatID, false)
	if delivery == "steer" {
		live, ok := c.liveSession(tabID, chatID)
		if !ok {
			c.scheduleDrain(tabID, chatID)
			return nil, errors.New("running chat has no live ACP session; the message remains durably queued")
		}
		result := c.manager.Steer(live.Info.SessionID, fieldString(params, "message"), nil, "")
		strategy := fieldString(result, "strategy")
		if result["live"] == true || strategy == "uncertain" {
			if err := c.state.AgentCommitLiveSteer(tabID, chatID, queueID); err != nil {
				return nil, err
			}
			receipt["delivery"] = strategy
			receipt["acceptedLive"] = result["live"] == true
			delete(receipt, "position")
			c.refresh(tabID, chatID, false)
			return receipt, nil
		}
		receipt["delivery"] = strategy
		receipt["interrupted"] = result["interrupted"]
		receipt["steerError"] = result["error"]
	}
	c.scheduleDrain(tabID, chatID)
	return receipt, nil
}

func (c *chatControlCoordinator) cancel(params map[string]any) (map[string]any, error) {
	tabID, chatID, err := exactAgentTarget(params)
	if err != nil {
		return nil, err
	}
	job, running := c.manager.RunningJobForChat(tabID, chatID)
	if !running {
		return map[string]any{"ok": false, "tabId": tabID, "chatId": chatID, "cancelled": false, "reason": "idle"}, nil
	}
	jobID := fieldString(job, "id")
	ok := c.manager.CancelJob(jobID)
	return map[string]any{"ok": ok, "tabId": tabID, "chatId": chatID, "jobId": jobID, "cancelled": ok}, nil
}

func (c *chatControlCoordinator) refresh(tabID, chatID string, focus bool) {
	if c == nil || c.refreshes == nil {
		return
	}
	c.refreshes.Request(tabID, chatID, c.state.RefreshGeneration(), refreshImmediate)
}

func (c *chatControlCoordinator) refreshBackground(tabID, chatID string) {
	if c == nil || c.refreshes == nil {
		return
	}
	c.refreshes.Request(tabID, chatID, c.state.RefreshGeneration(), refreshBackground)
}

func (c *chatControlCoordinator) liveSession(tabID, chatID string) (acp.LiveSession, bool) {
	for _, live := range c.manager.LiveSessions() {
		if live.TabID == tabID && live.ChatID == chatID {
			return live, true
		}
	}
	return acp.LiveSession{}, false
}

func (c *chatControlCoordinator) scheduleDrain(tabID, chatID string) {
	key := tabID + "\x00" + chatID
	c.mu.Lock()
	if c.draining[key] {
		// A queue mutation can land while this chat's worker is blocked in a
		// provider startup. Remember that wake so the worker's eventual exit
		// cannot strand a newly deliverable row with no drainer.
		c.drainPending[key] = true
		c.mu.Unlock()
		return
	}
	c.draining[key] = true
	c.mu.Unlock()
	go func() {
		defer func() {
			c.mu.Lock()
			retry := c.drainPending[key]
			delete(c.drainPending, key)
			delete(c.draining, key)
			c.mu.Unlock()
			if retry {
				c.scheduleDrain(tabID, chatID)
			}
		}()
		if _, running := c.manager.RunningJobForChat(tabID, chatID); running {
			return
		}
		item, agentFirst, hasQueue := c.state.AgentQueueHead(tabID, chatID)
		if !hasQueue || item[queueParkedField] == true {
			return
		}
		if !agentFirst {
			var err error
			item, agentFirst, hasQueue, err = c.state.AgentAdoptRendererQueueHead(
				tabID, chatID, time.Now().UTC(), c.rendererAdoptionAge,
			)
			if err != nil {
				c.refreshBackground(tabID, chatID)
				return
			}
			if !hasQueue || item[queueParkedField] == true {
				return
			}
			if !agentFirst {
				c.scheduleRendererHeadRecheck(tabID, chatID)
				return
			}
		}

		timeout := c.queueStartTimeout
		if timeout <= 0 {
			timeout = agentQueueStartTimeout
		}
		retryBase := c.queueRetryBase
		if retryBase <= 0 {
			retryBase = agentQueueRetryBase
		}
		var startErr error
		for attempt := 0; attempt < agentQueueStartAttemptLimit; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			startErr = c.startQueuedTurnForDrain(ctx, tabID, chatID, item)
			cancel()
			if startErr == nil {
				c.refreshBackground(tabID, chatID)
				return
			}
			if errors.Is(startErr, acp.ErrChatBusy) {
				return
			}
			if attempt+1 < agentQueueStartAttemptLimit {
				time.Sleep(retryBase << attempt)
			}
		}
		if err := c.state.AgentParkQueuedTurn(tabID, chatID, fieldString(item, "id"), startErr.Error()); err != nil {
			c.refreshBackground(tabID, chatID)
			return
		}
		c.refreshBackground(tabID, chatID)
	}()
}

func (c *chatControlCoordinator) wakeDrain(tabID, chatID string) {
	key := tabID + "\x00" + chatID
	c.mu.Lock()
	if timer := c.drainTimers[key]; timer != nil {
		timer.Stop()
		delete(c.drainTimers, key)
	}
	delete(c.drainBackoff, key)
	c.mu.Unlock()
	c.scheduleDrain(tabID, chatID)
}

func (c *chatControlCoordinator) scheduleRendererHeadRecheck(tabID, chatID string) {
	key := tabID + "\x00" + chatID
	c.mu.Lock()
	if c.drainTimers[key] != nil {
		c.mu.Unlock()
		return
	}
	delay := c.drainBackoff[key]
	if delay <= 0 {
		delay = c.rendererRecheckBase
	}
	if delay <= 0 {
		delay = rendererQueueRecheckBase
	}
	maxDelay := c.rendererRecheckMax
	if maxDelay <= 0 {
		maxDelay = rendererQueueRecheckMax
	}
	next := delay * 2
	if next > maxDelay {
		next = maxDelay
	}
	c.drainBackoff[key] = next
	c.drainTimers[key] = time.AfterFunc(delay, func() {
		c.mu.Lock()
		delete(c.drainTimers, key)
		c.mu.Unlock()
		c.scheduleDrain(tabID, chatID)
	})
	c.mu.Unlock()
}

func (c *chatControlCoordinator) startQueuedTurnForDrain(ctx context.Context, tabID, chatID string, item map[string]any) error {
	if c.startQueuedTurnOverride != nil {
		return c.startQueuedTurnOverride(ctx, tabID, chatID, item)
	}
	return c.startQueuedTurn(ctx, tabID, chatID, item)
}

func (c *chatControlCoordinator) startQueuedTurn(ctx context.Context, tabID, chatID string, item map[string]any) error {
	chat, err := c.state.AgentReadChat(tabID, chatID, 1, false)
	if err != nil {
		return err
	}
	desiredProvider := fieldString(chat, "providerId")
	desiredModel := hydratableStoredModelID(fieldString(chat, "modelId"))
	desiredMode := fieldString(chat, "modeId")
	live, ok := c.liveSession(tabID, chatID)
	if !ok {
		info, newErr := c.manager.NewSession(ctx, acp.SessionOptions{
			TabID: tabID, ChatID: chatID, CWD: fieldString(chat, "cwd"),
			ProviderID: desiredProvider, ModelID: desiredModel, ModeID: desiredMode,
		})
		if newErr != nil {
			return newErr
		}
		live = acp.LiveSession{TabID: tabID, ChatID: chatID, Info: info}
	}
	if desiredProvider == "" {
		desiredProvider = live.Info.ProviderID
	}
	// Same-provider controls can be applied before the turn. Cross-provider
	// selection is intentionally left to StartJob's tested serial handover path.
	if desiredProvider == live.Info.ProviderID {
		if desiredModel != "" && desiredModel != stringPointerValue(live.Info.CurrentModelID) {
			if _, err := c.manager.SetModel(ctx, live.Info.SessionID, desiredModel); err != nil {
				return err
			}
		}
		if desiredMode != "" && desiredMode != stringPointerValue(live.Info.CurrentModeID) {
			if _, err := c.manager.SetMode(ctx, live.Info.SessionID, desiredMode); err != nil {
				return err
			}
		}
	}
	queuedOpts := map[string]any{
		"kind": "app-chat", "title": fieldString(chat, "title"), "tabId": tabID, "chatId": chatID,
		"sessionId": live.Info.SessionID, "cwd": fieldString(chat, "cwd"), "providerId": desiredProvider,
		"modelId": desiredModel, "modeId": desiredMode, "permissionMode": desiredMode,
		"prompt": fieldString(item, "text"), "images": cloneJSON(item["images"]), "queueId": fieldString(item, "id"),
	}
	jobOpts := parseJobStartOptions(queuedOpts)
	// A queued row records who put it there: "host" is the user's own message,
	// while "agent" covers agent-sent text (session_store.go:2712, :2804).
	// Only the former is a new request. Anything unrecognised is treated as a
	// resumption, which can under-count obligations but can never invent a
	// finished one.
	humanAuthored := fieldString(item, "source") == "host"
	jobOpts.HumanAuthored = humanAuthored
	prepared := false
	jobOpts.BeforeStart = func(target *acp.JobStartOptions) error {
		opts, prepareErr := c.state.AgentPrepareQueuedTurn(tabID, chatID, fieldString(item, "id"), live.Info.SessionID)
		if prepareErr != nil {
			return prepareErr
		}
		if desiredProvider != "" {
			opts["providerId"] = desiredProvider
		}
		resolved := parseJobStartOptions(opts)
		resolved.BeforeStart = nil
		// AgentPrepareQueuedTurn rebuilds the options wholesale, so the origin
		// has to be re-applied or every drained turn would look like a wake.
		resolved.HumanAuthored = humanAuthored
		*target = resolved
		prepared = true
		return nil
	}
	// The drain context bounds only cold session/control convergence. A queued
	// turn that has been admitted owns its normal independent lifetime; canceling
	// the startup timer after StartJob returns must not cancel the model turn.
	_, err = c.manager.StartJob(context.Background(), jobOpts)
	if err != nil && prepared {
		c.state.FailPreparedTurn(tabID, "Error: "+err.Error())
	}
	return err
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
