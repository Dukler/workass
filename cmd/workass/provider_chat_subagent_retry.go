package main

import (
	"context"
	"encoding/json"
	"strings"

	"workass/internal/acp"
	"workass/internal/chat"
)

// maxRetryLineage bounds the walk back through retries of retries.
const maxRetryLineage = 64

// seedSubagentRetrySpec lets a retry reach a subagent spawned before the
// manager kept spawn specs (bug-20261118-afbfeed6): such a run, orphaned by a
// daemon update, exists only as an actor row, and its spawn request is in the
// actor's completed outbox. Best effort: when nothing is found, the retry
// answers as it always did.
func (r *providerChatRuntime) seedSubagentRetrySpec(ctx context.Context, ownerKey, tabID, chatID, id string) {
	if r == nil || r.manager == nil || strings.TrimSpace(id) == "" {
		return
	}
	_, state, err := r.exactActor(tabID, chatID)
	if err != nil {
		return
	}
	opts, ok := actorSpawnIntentForSubagent(state, id)
	if !ok {
		return
	}
	inheritActorSelection(&opts, state, id)
	opts.OwnerKey, opts.ParentChatID, opts.ParentTabID = ownerKey, chatID, tabID
	_ = r.manager.SeedSubagentRetrySpec(ctx, opts, id)
}

// inheritActorSelection fills what the spawn request left to inheritance from
// the chat's own durable selection. Right after a daemon replacement the chat
// may have no live session yet, and the manager's defaults come only from one.
// The provider the run actually used wins over the chat's.
func inheritActorSelection(opts *acp.SubagentSpawnOptions, state chat.State, id string) {
	chatProvider := strings.TrimSpace(string(state.Presentation.ProviderID))
	if strings.TrimSpace(opts.ProviderID) == "" {
		if row, ok := state.Background[strings.TrimSpace(id)]; ok && strings.TrimSpace(row.Event.ProviderID) != "" {
			opts.ProviderID = strings.TrimSpace(row.Event.ProviderID)
		} else {
			opts.ProviderID = chatProvider
		}
	}
	if strings.EqualFold(strings.TrimSpace(opts.ProviderID), chatProvider) {
		if strings.TrimSpace(opts.ModelID) == "" {
			opts.ModelID = state.Presentation.CurrentModelID
		}
		if intent := strings.ToLower(strings.TrimSpace(opts.PermissionIntent)); strings.TrimSpace(opts.ModeID) == "" && (intent == "" || intent == "inherit") {
			opts.ModeID = state.Presentation.CurrentModeID
		}
	}
	if cwd := strings.TrimSpace(opts.CWD); (cwd == "" || strings.EqualFold(cwd, "inherit")) && state.Presentation.CWD != nil {
		opts.CWD = *state.Presentation.CWD
	}
}

// actorSpawnIntentForSubagent rebuilds the prompt a run was started with: its
// spawn request plus the guidance of every retry between that spawn and it, in
// the order RetrySubagent appended them.
func actorSpawnIntentForSubagent(state chat.State, id string) (acp.SubagentSpawnOptions, bool) {
	guidance := []string{}
	seen := map[string]bool{}
	for step := 0; step < maxRetryLineage; step++ {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return acp.SubagentSpawnOptions{}, false
		}
		seen[id] = true
		action, ok := actorBackgroundActionThatStarted(state, id)
		if !ok {
			return acp.SubagentSpawnOptions{}, false
		}
		if action.Retry != nil {
			if extra := strings.TrimSpace(action.Retry.Message); extra != "" {
				guidance = append([]string{extra}, guidance...)
			}
			id = action.Retry.WorkID
			continue
		}
		spawn := action.Spawn
		if spawn == nil || strings.TrimSpace(spawn.OriginMachine) != "" {
			// A hosted run belongs to the machine that asked; it has no spec here.
			return acp.SubagentSpawnOptions{}, false
		}
		prompt := strings.TrimSpace(spawn.Prompt)
		for _, extra := range guidance {
			prompt += "\n\nRetry guidance from the coordinator:\n" + extra
		}
		return acp.SubagentSpawnOptions{
			Prompt: prompt, Label: spawn.Label, ProviderID: spawn.ProviderID, ModelID: spawn.ModelID,
			Effort: spawn.Effort, ModeID: spawn.ModeID, CWD: spawn.CWD, PermissionIntent: spawn.PermissionIntent,
			Fork: spawn.Fork,
		}, true
	}
	return acp.SubagentSpawnOptions{}, false
}

func actorBackgroundActionThatStarted(state chat.State, id string) (chat.BackgroundAction, bool) {
	for _, entry := range state.Outbox {
		if entry.Kind != chat.EffectBackground || entry.Status != chat.OutboxCompleted || entry.Background == nil {
			continue
		}
		if entry.Background.Kind != chat.BackgroundSpawnAgent && entry.Background.Kind != chat.BackgroundRetryAgent {
			continue
		}
		var result struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(entry.Result, &result) == nil && strings.TrimSpace(result.ID) == id {
			return entry.Background.Clone(), true
		}
	}
	return chat.BackgroundAction{}, false
}
