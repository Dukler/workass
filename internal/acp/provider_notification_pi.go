package acp

import "strings"

type piProviderNotificationStrategy struct{}

func (piProviderNotificationStrategy) Decode(update, _ map[string]any) (providerNotification, bool) {
	if strings.TrimSpace(asString(update["sessionUpdate"])) != "_workass_pi_steer_consumed" {
		return providerNotification{}, false
	}
	id := strings.TrimSpace(asString(update["clientUserMessageId"]))
	if id == "" {
		return providerNotification{}, false
	}
	return providerNotification{Kind: providerNotificationSteerConsumed,
		SteerConsumed: &providerSteerConsumptionUpdate{ClientUserMessageID: id}}, true
}

func (piProviderNotificationStrategy) ToolParentID(map[string]any, map[string]any) string { return "" }
