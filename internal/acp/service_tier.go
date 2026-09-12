package acp

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Service tiers are an advertised provider control, orthogonal to model and
// reasoning effort. Never downgrade an explicit selection after rejection.
func (m *Manager) applyServiceTier(ctx context.Context, sessionID, tier string) error {
	b := m.bridgeForSession(sessionID, SessionOptions{})
	if b == nil {
		return errors.New("service-tier session is not attached")
	}
	b.mu.Lock()
	configID := b.serviceTierConfigID
	tiers := append([]string(nil), b.serviceTiers...)
	b.mu.Unlock()
	tier = strings.TrimSpace(tier)
	if configID == "" {
		if tier != "" {
			return errors.New("provider does not advertise service-tier control")
		}
		return nil
	}
	if tier == "" {
		tier = "default"
	}
	supported := false
	for _, value := range tiers {
		if value == tier {
			supported = true
		}
	}
	if !supported {
		return errors.New("requested service tier is not advertised by this provider")
	}
	result, err := b.request(ctx, "session/set_config_option", map[string]any{"sessionId": sessionID, "configId": configID, "value": tier}, 15*time.Second)
	if err != nil {
		return err
	}
	options, _ := result["configOptions"].([]any)
	for _, value := range options {
		option := mapFromAny(value)
		if asString(option["id"]) == configID && asString(option["currentValue"]) == tier {
			return nil
		}
	}
	return errors.New("provider did not confirm the requested service tier")
}

func serviceTierValues(raw any) []string {
	var values []string
	switch list := raw.(type) {
	case []string:
		values = list
	case []any:
		for _, value := range list {
			if text, ok := value.(string); ok {
				values = append(values, text)
			}
		}
	}
	var out []string
	for _, value := range values {
		// These are the two public speed choices understood by the controller.
		if value == "default" || value == "fast" {
			out = appendMissingStrings(out, value)
		}
	}
	return out
}
