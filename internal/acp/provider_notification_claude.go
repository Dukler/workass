package acp

import (
	"strings"
	"time"
)

type claudeProviderNotificationStrategy struct{}

func (claudeProviderNotificationStrategy) Decode(update, _ map[string]any) (providerNotification, bool) {
	switch strings.TrimSpace(asString(update["sessionUpdate"])) {
	case "_workass_claude_spawned_work":
		decoded, ok := (claudeProviderSpawnedWorkStrategy{}).DecodeLifecycle(update["event"])
		if !ok {
			return providerNotification{Kind: providerNotificationSpawnedWork}, true
		}
		return providerNotification{Kind: providerNotificationSpawnedWork, SpawnedWork: &decoded}, true
	case "_workass_claude_turn":
		decoded := decodeClaudeHarnessTurn(update)
		return providerNotification{Kind: providerNotificationHarnessTurn, HarnessTurn: &decoded}, true
	case "_workass_claude_commands":
		return providerNotification{
			Kind: providerNotificationCommandCatalog, CommandCatalog: parseCommandCatalog(update["commandCatalog"]), CatalogSet: true,
		}, true
	case "_workass_claude_provider_session":
		decoded := providerLineageUpdate{
			PreviousThreadID: asString(update["previousProviderSessionId"]),
			ThreadID:         strings.TrimSpace(asString(update["providerSessionId"])),
			Generation:       uint64(numberOrZero(update["lineageGeneration"])),
			Proof:            asString(update["lineageProof"]),
		}
		return providerNotification{Kind: providerNotificationLineage, Lineage: &decoded}, true
	case "_workass_claude_turn_heartbeat":
		decoded := decodeClaudeHeartbeat(update)
		return providerNotification{Kind: providerNotificationHeartbeat, Heartbeat: &decoded}, true
	case "_workass_claude_steer_consumed":
		decoded := providerSteerConsumptionUpdate{ClientUserMessageID: strings.TrimSpace(asString(update["clientUserMessageId"]))}
		return providerNotification{Kind: providerNotificationSteerConsumed, SteerConsumed: &decoded}, true
	default:
		return providerNotification{}, false
	}
}

func (claudeProviderNotificationStrategy) ToolParentID(updateMeta, paramsMeta map[string]any) string {
	for _, meta := range []map[string]any{updateMeta, paramsMeta} {
		if id := asString(mapFromAny(meta["claudeCode"])["parentToolUseId"]); id != "" {
			return id
		}
	}
	return ""
}

func decodeClaudeHarnessTurn(update map[string]any) providerHarnessTurnUpdate {
	decoded := providerHarnessTurnUpdate{
		Phase:         asString(update["phase"]),
		PromptID:      asString(update["promptId"]),
		HumanAuthored: boolFromProviderValue(update["humanAuthored"]),
	}
	if decoded.Phase != harnessTurnPhaseEnded {
		return decoded
	}
	evidence := &harnessTurnEvidence{
		PromptID:       decoded.PromptID,
		TerminalReason: asString(update["terminalReason"]),
		StopReason:     asString(update["stopReason"]),
		OriginKind:     asString(update["originKind"]),
		HookEvidence:   boolFromProviderValue(update["harnessEvidence"]),
		At:             time.Now(),
	}
	if raw, ok := update["backgroundTasks"].([]any); ok {
		evidence.TasksKnown = true
		for _, item := range raw {
			task := mapFromAny(item)
			evidence.Tasks = append(evidence.Tasks, harnessBackgroundTask{
				ID: asString(task["id"]), Type: asString(task["type"]), Status: asString(task["status"]),
			})
		}
	}
	if raw, ok := update["sessionCrons"].([]any); ok {
		evidence.CronsKnown = true
		for _, item := range raw {
			cron := mapFromAny(item)
			evidence.Crons = append(evidence.Crons, harnessSessionCron{
				Schedule: asString(cron["schedule"]), Recurring: boolFromProviderValue(cron["recurring"]),
			})
		}
	}
	decoded.Evidence = evidence
	return decoded
}

func boolFromProviderValue(value any) bool {
	result, ok := value.(bool)
	return ok && result
}

func decodeClaudeHeartbeat(update map[string]any) providerHeartbeatUpdate {
	decoded := providerHeartbeatUpdate{}
	if _, ok := update["elapsedMs"]; ok {
		decoded.ElapsedMS, decoded.ElapsedMSSet = numberOrZero(update["elapsedMs"]), true
	}
	if _, ok := update["outputTokens"]; ok {
		decoded.OutputTokens, decoded.OutputTokensSet = numberOrZero(update["outputTokens"]), true
	}
	if _, ok := update["phase"]; ok {
		decoded.Phase, decoded.PhaseSet = asString(update["phase"]), true
	}
	if _, ok := update["toolName"]; ok {
		decoded.ToolName, decoded.ToolNameSet = asString(update["toolName"]), true
	}
	if retry, ok := update["retry"].(map[string]any); ok && retry != nil {
		decoded.Retry = &providerHeartbeatRetry{}
		if _, ok := retry["code"]; ok {
			decoded.Retry.Code, decoded.Retry.CodeSet = numberOrZero(retry["code"]), true
		}
		if _, ok := retry["attempt"]; ok {
			decoded.Retry.Attempt, decoded.Retry.AttemptSet = numberOrZero(retry["attempt"]), true
		}
	}
	return decoded
}
