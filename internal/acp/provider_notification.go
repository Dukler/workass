package acp

import (
	"errors"
	"strings"
)

type providerNotificationKind uint8

const (
	providerNotificationUnknown providerNotificationKind = iota
	providerNotificationSpawnedWork
	providerNotificationHarnessTurn
	providerNotificationCommandCatalog
	providerNotificationLineage
	providerNotificationHeartbeat
	providerNotificationSteerConsumed
)

// providerNotification is the provider-neutral union emitted by a registered
// notification decoder. Vendor method names, metadata keys, and loose payload
// maps stop before this type reaches the generic bridge dispatcher.
type providerNotification struct {
	Kind           providerNotificationKind
	SpawnedWork    *providerSpawnedWorkUpdate
	HarnessTurn    *providerHarnessTurnUpdate
	CommandCatalog *CommandCatalog
	CatalogSet     bool
	Lineage        *providerLineageUpdate
	Heartbeat      *providerHeartbeatUpdate
	SteerConsumed  *providerSteerConsumptionUpdate
}

type providerLineageUpdate struct {
	PreviousThreadID string
	ThreadID         string
	Generation       uint64
	Proof            string
}

type providerHeartbeatRetry struct {
	Code       int
	CodeSet    bool
	Attempt    int
	AttemptSet bool
}

type providerHeartbeatUpdate struct {
	ElapsedMS       int
	ElapsedMSSet    bool
	OutputTokens    int
	OutputTokensSet bool
	Phase           string
	PhaseSet        bool
	ToolName        string
	ToolNameSet     bool
	Retry           *providerHeartbeatRetry
}

type providerSteerConsumptionUpdate struct {
	ClientUserMessageID string
}

type providerHarnessTurnUpdate struct {
	Phase         string
	PromptID      string
	HumanAuthored bool
	Evidence      *harnessTurnEvidence
}

type providerNotificationStrategy interface {
	Decode(update, params map[string]any) (providerNotification, bool)
	ToolParentID(updateMeta, paramsMeta map[string]any) string
}

type unsupportedProviderNotificationStrategy struct{}

func (unsupportedProviderNotificationStrategy) Decode(map[string]any, map[string]any) (providerNotification, bool) {
	return providerNotification{}, false
}

func (unsupportedProviderNotificationStrategy) ToolParentID(map[string]any, map[string]any) string {
	return ""
}

func (b *Bridge) handleProviderNotification(sessionID string, job *Job, notification providerNotification) {
	switch notification.Kind {
	case providerNotificationSpawnedWork:
		if notification.SpawnedWork == nil || !b.manager.acceptsProviderSpawnedWork(b.ProviderID()) {
			return
		}
		tabID, chatID := b.chatIdentity()
		b.manager.observeProviderSpawnedWork(tabID, chatID, sessionID, b.ProviderID(), *notification.SpawnedWork)
	case providerNotificationHarnessTurn:
		if notification.HarnessTurn == nil {
			return
		}
		tabID, chatID := b.chatIdentity()
		b.manager.observeHarnessTurn(b, tabID, chatID, sessionID, *notification.HarnessTurn)
	case providerNotificationCommandCatalog:
		if !notification.CatalogSet {
			return
		}
		tabID, chatID := b.chatIdentity()
		b.manager.storeCommandCatalog(tabID, chatID, sessionID, notification.CommandCatalog)
	case providerNotificationLineage:
		lineage := notification.Lineage
		if lineage == nil || strings.TrimSpace(lineage.ThreadID) == "" {
			return
		}
		tabID, chatID := b.chatIdentity()
		if lane := b.manager.providerLaneForSessionID(sessionID); lane != nil {
			if err := lane.advanceLineage(lineage.PreviousThreadID, lineage.ThreadID, lineage.Generation, lineage.Proof); err != nil {
				b.opts.Logf("provider lineage rejected", map[string]any{"providerId": b.ProviderID(), "error": err.Error()})
				go b.Close(false, err)
			}
		} else if strings.HasPrefix(chatID, subagentChatIDPrefix) && b.manager.nativeSessions != nil {
			b.manager.nativeSessions.adoptProviderSession(
				tabID, chatID, b.ProviderID(), sessionID,
				lineage.PreviousThreadID, lineage.ThreadID, lineage.Generation, lineage.Proof,
			)
		} else {
			err := errors.New("chat-scoped provider lineage has no authoritative actor lane")
			b.opts.Logf("provider lineage rejected", map[string]any{"providerId": b.ProviderID(), "error": err.Error()})
			go b.Close(false, err)
		}
	case providerNotificationHeartbeat:
		if notification.Heartbeat == nil || job == nil || job.internal {
			return
		}
		pulse := notification.Heartbeat
		event := map[string]any{"kind": "turn-heartbeat"}
		if pulse.ElapsedMSSet {
			event["elapsedMs"] = pulse.ElapsedMS
		} else {
			event["elapsedMs"] = nil
		}
		if pulse.OutputTokensSet {
			event["outputTokens"] = pulse.OutputTokens
		} else {
			event["outputTokens"] = nil
		}
		if pulse.PhaseSet {
			event["phase"] = pulse.Phase
		} else {
			event["phase"] = nil
		}
		if pulse.ToolNameSet {
			event["toolName"] = pulse.ToolName
		} else {
			event["toolName"] = nil
		}
		if pulse.Retry == nil {
			event["retry"] = nil
		} else {
			retry := map[string]any{}
			if pulse.Retry.CodeSet {
				retry["code"] = pulse.Retry.Code
			}
			if pulse.Retry.AttemptSet {
				retry["attempt"] = pulse.Retry.Attempt
			}
			event["retry"] = retry
		}
		b.manager.emit("job:event", map[string]any{"type": "acp", "id": job.ID, "event": event})
	case providerNotificationSteerConsumed:
		if notification.SteerConsumed == nil || job == nil || job.internal {
			return
		}
		clientUserMessageID := strings.TrimSpace(notification.SteerConsumed.ClientUserMessageID)
		if job.markSteerConsumed(clientUserMessageID) {
			b.manager.emit("job:event", map[string]any{
				"type": "acp", "id": job.ID,
				"event": map[string]any{"kind": "steer-consumed", "clientUserMessageId": clientUserMessageID},
			})
		}
	}
}
