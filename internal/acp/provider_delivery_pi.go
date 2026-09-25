package acp

import (
	"context"
	"strings"
	"time"

	providercontract "workass/internal/provider"
)

// Native Pi admission is the SDK steer() completion, never a followUp(), a
// concurrent prompt(), or merely a write to the host's stdin.
type piDeliveryStrategy struct{}

func (piDeliveryStrategy) Capabilities(b *Bridge) providercontract.DeliveryCapabilities {
	return providercontract.DeliveryCapabilities{
		LiveSteer:               b != nil && b.hasProviderCapability("workassPiSteerRequest"),
		SteerConsumptionReceipt: b != nil && b.hasProviderCapability("workassPiSteerReceipt"),
	}
}

func (piDeliveryStrategy) AssistantPhase(update map[string]any) string {
	return standardAssistantPhase(update)
}

func (s piDeliveryStrategy) Steer(b *Bridge, request providerSteerRequest) providerSteerOutcome {
	if !s.Capabilities(b).LiveSteer {
		return providerSteerOutcome{unsupported: true, strategy: "unsupported", errorText: "El host Pi activo no admite steering nativo."}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := b.request(ctx, "_workass/pi/steer", map[string]any{
		"sessionId": request.sessionID, "prompt": request.prompt, "clientUserMessageId": request.clientUserMessageID,
	}, 15*time.Second)
	if err != nil {
		if providerRequestTimedOut(err) {
			return providerSteerOutcome{strategy: "uncertain", errorText: "Pi no confirmo el steer a tiempo; no se reenvio para evitar duplicarlo."}
		}
		return providerSteerOutcome{strategy: "rejected", errorText: "Pi rechazo el steer; no se encolo ni se interrumpio el turno."}
	}
	if turnID := strings.TrimSpace(asString(result["turnId"])); turnID != "" {
		return providerSteerOutcome{ok: true, live: true, strategy: "pi-live", turnID: turnID,
			receipt: request.clientUserMessageID != "" && b.hasProviderCapability("workassPiSteerReceipt")}
	}
	return providerSteerOutcome{strategy: "uncertain", errorText: "Pi no confirmo la identidad del turno; no se reenvio el steer."}
}
