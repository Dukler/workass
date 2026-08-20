package acp

import "strings"

type codexProviderNotificationStrategy struct{}

func (codexProviderNotificationStrategy) Decode(update, _ map[string]any) (providerNotification, bool) {
	if strings.TrimSpace(asString(update["sessionUpdate"])) != "_workass_codex_steer_consumed" {
		return providerNotification{}, false
	}
	decoded := providerSteerConsumptionUpdate{ClientUserMessageID: strings.TrimSpace(asString(update["clientUserMessageId"]))}
	return providerNotification{Kind: providerNotificationSteerConsumed, SteerConsumed: &decoded}, true
}

func (codexProviderNotificationStrategy) ToolParentID(map[string]any, map[string]any) string {
	return ""
}
