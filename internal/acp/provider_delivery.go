package acp

import (
	"context"
	"errors"
	"strings"
	"time"
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
	interrupted bool
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
	if o.interrupted {
		payload["interrupted"] = true
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
	Steer(*Bridge, providerSteerRequest) providerSteerOutcome
	AssistantPhase(map[string]any) string
}

type genericACPDeliveryStrategy struct{}
type codexDeliveryStrategy struct{}
type claudeDeliveryStrategy struct{}

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
	if !b.hasProviderCapability("steerNotification", "sessionSteer") {
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
		queued: false, unsupported: true, strategy: "queue",
		errorText: "El agente ACP no anuncio steering mid-turn estandar.",
	}
}

func (codexDeliveryStrategy) Steer(b *Bridge, request providerSteerRequest) providerSteerOutcome {
	if outcome, handled := tryStandardACPSteer(b, request); handled {
		return outcome
	}
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
			case "queue":
				return providerSteerOutcome{
					queued: false, strategy: "queue", reason: asString(result["reason"]),
					errorText: "El turno activo de Codex no acepta steering; la indicacion se conservara para el siguiente turno.",
				}
			case "next-turn":
				interrupted := b.interruptForQueuedSteer(request.sessionID)
				strategy := "queue"
				if interrupted {
					strategy = "interrupt-queue"
				}
				return providerSteerOutcome{
					queued: false, interrupted: interrupted, strategy: strategy, reason: asString(result["reason"]),
					errorText: "El turno nativo ya termino; la indicacion se enviara como el siguiente turno.",
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
			err = errors.New("el adapter no devolvio el turnId que acepto el steer")
		}
		if providerRequestTimedOut(err) {
			b.opts.Logf("native Codex steer acknowledgement timed out", map[string]any{"key": b.key, "error": err.Error()})
			return providerSteerOutcome{
				queued: false, strategy: "uncertain",
				receipt:   request.clientUserMessageID != "" && b.hasProviderCapability("workassCodexSteerReceipt"),
				errorText: "Codex no confirmo el steer a tiempo; no lo reenvie para evitar duplicarlo.",
			}
		}
		b.opts.Logf("native Codex steer rejected; interrupting for queued follow-up", map[string]any{"key": b.key, "error": err.Error()})
	}
	if !b.interruptForQueuedSteer(request.sessionID) {
		return providerSteerOutcome{queued: false, errorText: "Codex no acepto steering nativo y tampoco pude interrumpir el turno."}
	}
	return providerSteerOutcome{
		queued: false, interrupted: true, unsupported: true, strategy: "interrupt-queue",
		errorText: "El adapter Codex activo no expone turn/steer; interrumpi el turno para enviar la indicacion inmediatamente despues.",
	}
}

func (claudeDeliveryStrategy) Steer(b *Bridge, request providerSteerRequest) providerSteerOutcome {
	if outcome, handled := tryStandardACPSteer(b, request); handled {
		return outcome
	}
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
			err = errors.New("el adapter no devolvio el turnId que acepto el steer")
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
			queued: false, strategy: "claude-live",
			errorText: "Claude no confirmo el steer nativo; no interrumpi el turno porque la entrega es incierta.",
		}
	}
	if !b.interruptForQueuedSteer(request.sessionID) {
		return providerSteerOutcome{queued: false, errorText: "No pude interrumpir el turno de Claude para redirigirlo."}
	}
	return providerSteerOutcome{
		queued: false, interrupted: true, unsupported: true, strategy: "interrupt-queue",
		errorText: "Claude fue interrumpido; la indicacion persistida se enviara como el siguiente turno.",
	}
}

func providerRequestTimedOut(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "ACP timeout:") || errors.Is(err, context.DeadlineExceeded))
}
