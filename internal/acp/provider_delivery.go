package acp

import (
	"context"
	"errors"
	"strings"
	"time"

	providercontract "workass/internal/provider"
)

type providerSteerRequest struct {
	sessionID           string
	prompt              []any
	clientUserMessageID string
}

type providerSteerOutcome struct {
	ok          bool
	live        bool
	queued      bool
	unsupported bool
	receipt     bool
	strategy    string
	turnID      string
	reason      string
	errorText   string
}

func (o providerSteerOutcome) payload() map[string]any {
	payload := map[string]any{
		"ok":     o.ok,
		"live":   o.live,
		"queued": o.queued,
	}
	if o.unsupported {
		payload["unsupported"] = true
	}
	if o.receipt {
		payload["receipt"] = true
	}
	if o.strategy != "" {
		payload["strategy"] = o.strategy
	}
	if o.turnID != "" {
		payload["turnId"] = o.turnID
	}
	if o.reason != "" {
		payload["reason"] = o.reason
	}
	if o.errorText != "" {
		payload["error"] = o.errorText
	}
	return payload
}

type providerDeliveryStrategy interface {
	Capabilities(*Bridge) providercontract.DeliveryCapabilities
	Steer(*Bridge, providerSteerRequest) providerSteerOutcome
	AssistantPhase(map[string]any) string
}

type genericACPDeliveryStrategy struct{}
type codexDeliveryStrategy struct{}
type claudeDeliveryStrategy struct{}

func standardACPSteerCapabilities(b *Bridge) (providercontract.DeliveryCapabilities, bool) {
	if b == nil || !b.hasProviderCapability("steerNotification", "sessionSteer") {
		return providercontract.DeliveryCapabilities{}, false
	}
	return providercontract.DeliveryCapabilities{LiveSteer: true}, true
}

func (genericACPDeliveryStrategy) Capabilities(b *Bridge) providercontract.DeliveryCapabilities {
	capabilities, _ := standardACPSteerCapabilities(b)
	return capabilities
}

func (codexDeliveryStrategy) Capabilities(b *Bridge) providercontract.DeliveryCapabilities {
	if b == nil || !b.hasProviderCapability("workassCodexSteerRequest") {
		return providercontract.DeliveryCapabilities{}
	}
	return providercontract.DeliveryCapabilities{
		LiveSteer:               true,
		SteerConsumptionReceipt: b.hasProviderCapability("workassCodexSteerReceipt"),
	}
}

func (claudeDeliveryStrategy) Capabilities(b *Bridge) providercontract.DeliveryCapabilities {
	if b == nil || !b.hasProviderCapability("workassClaudeSteerRequest") {
		return providercontract.DeliveryCapabilities{}
	}
	return providercontract.DeliveryCapabilities{
		LiveSteer:               true,
		SteerConsumptionReceipt: b.hasProviderCapability("workassClaudeSteerReceipt"),
	}
}

func standardAssistantPhase(update map[string]any) string {
	meta := mapFromAny(update["_meta"])
	switch strings.TrimSpace(asString(meta["workassAssistantPhase"])) {
	case "commentary":
		return "commentary"
	case "final_answer":
		return "final_answer"
	default:
		return ""
	}
}

func (genericACPDeliveryStrategy) AssistantPhase(update map[string]any) string {
	return standardAssistantPhase(update)
}

func (claudeDeliveryStrategy) AssistantPhase(update map[string]any) string {
	return standardAssistantPhase(update)
}

func (codexDeliveryStrategy) AssistantPhase(update map[string]any) string {
	if phase := standardAssistantPhase(update); phase != "" {
		return phase
	}
	meta := mapFromAny(update["_meta"])
	codexMeta := mapFromAny(meta["codex"])
	switch strings.TrimSpace(asString(codexMeta["phase"])) {
	case "commentary":
		return "commentary"
	case "final_answer":
		return "final_answer"
	default:
		return ""
	}
}

func tryStandardACPSteer(b *Bridge, request providerSteerRequest) (providerSteerOutcome, bool) {
	if capabilities, supported := standardACPSteerCapabilities(b); !supported || !capabilities.LiveSteer {
		return providerSteerOutcome{}, false
	}
	if !b.notify("_session/steer", map[string]any{"sessionId": request.sessionID, "prompt": request.prompt}) {
		return providerSteerOutcome{
			queued: false, strategy: "generic-live", errorText: "No pude enviar el steer al agente ACP.",
		}, true
	}
	return providerSteerOutcome{ok: true, live: true, queued: false, strategy: "generic-live"}, true
}

func (genericACPDeliveryStrategy) Steer(b *Bridge, request providerSteerRequest) providerSteerOutcome {
	if outcome, handled := tryStandardACPSteer(b, request); handled {
		return outcome
	}
	return providerSteerOutcome{
		queued: false, unsupported: true, strategy: "unsupported",
		errorText: "El agente ACP no anuncio steering mid-turn estandar.",
	}
}

func (codexDeliveryStrategy) Steer(b *Bridge, request providerSteerRequest) providerSteerOutcome {
	if b.hasProviderCapability("workassCodexSteerRequest") {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		params := map[string]any{"sessionId": request.sessionID, "prompt": request.prompt}
		if request.clientUserMessageID != "" {
			params["clientUserMessageId"] = request.clientUserMessageID
		}
		result, err := b.request(ctx, "_workass/codex/steer", params, 15*time.Second)
		if err == nil {
			switch strings.TrimSpace(asString(result["disposition"])) {
			case "rejected", "queue", "next-turn":
				// queue/next-turn are accepted only as old-host rejection shapes.
				// They never authorize Workass to interrupt the active turn or
				// convert an explicit live-steer intent into FIFO work.
				return providerSteerOutcome{
					queued: false, strategy: "rejected", reason: asString(result["reason"]),
					errorText: "Codex no acepto el steer; la indicacion sigue en el composer.",
				}
			}
		}
		turnID := strings.TrimSpace(asString(result["turnId"]))
		if err == nil && turnID != "" {
			return providerSteerOutcome{
				ok: true, live: true, queued: false, strategy: "codex-live", turnID: turnID,
				receipt: request.clientUserMessageID != "" && b.hasProviderCapability("workassCodexSteerReceipt"),
			}
		}
		if err == nil {
			b.opts.Logf("native Codex steer acknowledgement omitted turn identity", map[string]any{"key": b.key})
			return providerSteerOutcome{
				queued: false, strategy: "uncertain",
				receipt:   request.clientUserMessageID != "" && b.hasProviderCapability("workassCodexSteerReceipt"),
				errorText: "Codex no devolvio la identidad del turno que acepto el steer; no lo reenvie para evitar duplicarlo.",
			}
		}
		if providerRequestTimedOut(err) {
			b.opts.Logf("native Codex steer acknowledgement timed out", map[string]any{"key": b.key, "error": err.Error()})
			return providerSteerOutcome{
				queued: false, strategy: "uncertain",
				receipt:   request.clientUserMessageID != "" && b.hasProviderCapability("workassCodexSteerReceipt"),
				errorText: "Codex no confirmo el steer a tiempo; no lo reenvie para evitar duplicarlo.",
			}
		}
		b.opts.Logf("native Codex steer rejected", map[string]any{"key": b.key, "error": err.Error()})
		return providerSteerOutcome{
			queued: false, strategy: "rejected",
			errorText: "Codex rechazo el steer; la indicacion sigue en el composer.",
		}
	}
	return providerSteerOutcome{
		queued: false, unsupported: true, strategy: "unsupported",
		errorText: "El adapter Codex activo no expone turn/steer; la indicacion sigue en el composer.",
	}
}

func (claudeDeliveryStrategy) Steer(b *Bridge, request providerSteerRequest) providerSteerOutcome {
	if b.hasProviderCapability("workassClaudeSteerRequest") {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		params := map[string]any{"sessionId": request.sessionID, "prompt": request.prompt}
		if request.clientUserMessageID != "" {
			params["clientUserMessageId"] = request.clientUserMessageID
		}
		result, err := b.request(ctx, "_workass/claude/steer", params, 15*time.Second)
		turnID := strings.TrimSpace(asString(result["turnId"]))
		if err == nil && turnID != "" {
			return providerSteerOutcome{
				ok: true, live: true, queued: false, strategy: "claude-live", turnID: turnID,
				receipt: request.clientUserMessageID != "" && b.hasProviderCapability("workassClaudeSteerReceipt"),
			}
		}
		if err == nil {
			b.opts.Logf("native Claude steer acknowledgement omitted turn identity", map[string]any{"key": b.key})
			return providerSteerOutcome{
				queued: false, strategy: "uncertain",
				receipt:   request.clientUserMessageID != "" && b.hasProviderCapability("workassClaudeSteerReceipt"),
				errorText: "Claude no devolvio la identidad del turno que acepto el steer; no lo reenvie para evitar duplicarlo.",
			}
		}
		if providerRequestTimedOut(err) {
			b.opts.Logf("native Claude steer acknowledgement timed out", map[string]any{"key": b.key, "error": err.Error()})
			return providerSteerOutcome{
				queued: false, strategy: "uncertain",
				receipt:   request.clientUserMessageID != "" && b.hasProviderCapability("workassClaudeSteerReceipt"),
				errorText: "Claude no confirmo el steer a tiempo; no lo reenvie para evitar duplicarlo.",
			}
		}
		b.opts.Logf("native Claude steer rejected", map[string]any{"key": b.key, "error": err.Error()})
		return providerSteerOutcome{
			queued: false, strategy: "rejected",
			errorText: "Claude rechazo el steer; la indicacion sigue en el composer.",
		}
	}
	return providerSteerOutcome{
		queued: false, unsupported: true, strategy: "unsupported",
		errorText: "El adapter Claude activo no expone live steering; la indicacion sigue en el composer.",
	}
}

func providerRequestTimedOut(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "ACP timeout:") || errors.Is(err, context.DeadlineExceeded))
}
