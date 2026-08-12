package acp

import (
	"context"
	"errors"
	"strings"
)

type planUsageRead struct {
	metaKey string
	result  map[string]any
}

type providerPlanUsageStrategy interface {
	Supported(*Bridge) bool
	Refresh(context.Context, *Bridge, string) (planUsageRead, error)
	SupportsReset(*Bridge) bool
	ConsumeReset(context.Context, *Bridge, string, string) (map[string]any, planUsageRead, error)
}

type unsupportedPlanUsageStrategy struct{}
type codexPlanUsageStrategy struct{}
type claudePlanUsageStrategy struct{}

func (unsupportedPlanUsageStrategy) Supported(*Bridge) bool { return false }

func (unsupportedPlanUsageStrategy) Refresh(context.Context, *Bridge, string) (planUsageRead, error) {
	return planUsageRead{}, nil
}

func (unsupportedPlanUsageStrategy) SupportsReset(*Bridge) bool { return false }

func (unsupportedPlanUsageStrategy) ConsumeReset(context.Context, *Bridge, string, string) (map[string]any, planUsageRead, error) {
	return nil, planUsageRead{}, errors.New("this provider does not expose earned rate-limit resets")
}

func (codexPlanUsageStrategy) Supported(bridge *Bridge) bool {
	return bridge != nil && bridge.hasProviderCapability("workassCodexRateLimitsRequest")
}

func (codexPlanUsageStrategy) Refresh(ctx context.Context, bridge *Bridge, _ string) (planUsageRead, error) {
	result, err := bridge.request(ctx, "_workass/codex/rate-limits", map[string]any{}, planUsageRefreshTimeout)
	return planUsageRead{metaKey: "workass.codex/rateLimits", result: result}, err
}

func (codexPlanUsageStrategy) SupportsReset(bridge *Bridge) bool {
	return bridge != nil && bridge.hasProviderCapability("workassCodexRateLimitResetRequest")
}

func (codexPlanUsageStrategy) ConsumeReset(
	ctx context.Context,
	bridge *Bridge,
	idempotencyKey string,
	creditID string,
) (map[string]any, planUsageRead, error) {
	params := map[string]any{"idempotencyKey": strings.TrimSpace(idempotencyKey)}
	if creditID = strings.TrimSpace(creditID); creditID != "" {
		params["creditId"] = creditID
	}
	result, err := bridge.request(ctx, "_workass/codex/rate-limit-reset/consume", params, planUsageRefreshTimeout)
	if err != nil {
		return nil, planUsageRead{}, err
	}
	rateLimits := mapFromAny(result["rateLimits"])
	if len(rateLimits) == 0 {
		return nil, planUsageRead{}, errors.New("Codex reset response omitted the refreshed rate limits")
	}
	return result, planUsageRead{metaKey: "workass.codex/rateLimits", result: rateLimits}, nil
}

func (claudePlanUsageStrategy) Supported(bridge *Bridge) bool {
	return bridge != nil && bridge.hasProviderCapability("workassClaudeUsageRequest")
}

func (claudePlanUsageStrategy) Refresh(ctx context.Context, bridge *Bridge, sessionID string) (planUsageRead, error) {
	result, err := bridge.request(
		ctx,
		"_workass/claude/usage",
		map[string]any{"sessionId": strings.TrimSpace(sessionID)},
		planUsageRefreshTimeout,
	)
	return planUsageRead{metaKey: "workass.claude/usage", result: result}, err
}

func (claudePlanUsageStrategy) SupportsReset(*Bridge) bool { return false }

func (claudePlanUsageStrategy) ConsumeReset(context.Context, *Bridge, string, string) (map[string]any, planUsageRead, error) {
	return nil, planUsageRead{}, errors.New("this provider does not expose earned rate-limit resets")
}
