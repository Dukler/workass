package acp

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
)

type providerPlanUsageStrategy interface {
	Normalize(map[string]any) planUsageCapture
	Supported(*Bridge) bool
	Refresh(context.Context, *Bridge, string) (planUsageCapture, error)
	SupportsReset(*Bridge) bool
	ConsumeReset(context.Context, *Bridge, string, string) (map[string]any, planUsageCapture, error)
}

type unsupportedPlanUsageStrategy struct{}
type codexPlanUsageStrategy struct{}
type claudePlanUsageStrategy struct{}

func (unsupportedPlanUsageStrategy) Normalize(carrier map[string]any) planUsageCapture {
	return appendStandardACPPlanUsage(planUsageRawCapture(carrier), carrier)
}

func (unsupportedPlanUsageStrategy) Supported(*Bridge) bool { return false }

func (unsupportedPlanUsageStrategy) Refresh(context.Context, *Bridge, string) (planUsageCapture, error) {
	return planUsageCapture{}, nil
}

func (unsupportedPlanUsageStrategy) SupportsReset(*Bridge) bool { return false }

func (unsupportedPlanUsageStrategy) ConsumeReset(context.Context, *Bridge, string, string) (map[string]any, planUsageCapture, error) {
	return nil, planUsageCapture{}, errors.New("this provider does not expose earned rate-limit resets")
}

func (codexPlanUsageStrategy) Normalize(carrier map[string]any) planUsageCapture {
	out := planUsageRawCapture(carrier)
	meta := mapFromAny(carrier["_meta"])
	out.entries = append(out.entries, codexRateLimitPlanUsage(meta)...)
	out.resetCredits, out.resetCreditsSet = codexRateLimitResetCredits(meta)
	if entry, ok := codexQuotaPlanUsage(meta); ok {
		out.entries = append(out.entries, entry)
	}
	return appendStandardACPPlanUsage(out, carrier)
}

func (codexPlanUsageStrategy) Supported(bridge *Bridge) bool {
	return bridge != nil && bridge.hasProviderCapability("workassCodexRateLimitsRequest")
}

func (strategy codexPlanUsageStrategy) Refresh(ctx context.Context, bridge *Bridge, _ string) (planUsageCapture, error) {
	result, err := bridge.request(ctx, "_workass/codex/rate-limits", map[string]any{}, planUsageRefreshTimeout)
	if err != nil {
		return planUsageCapture{}, err
	}
	return strategy.Normalize(map[string]any{
		"_meta": map[string]any{"workass.codex/rateLimits": result},
	}), nil
}

func (codexPlanUsageStrategy) SupportsReset(bridge *Bridge) bool {
	return bridge != nil && bridge.hasProviderCapability("workassCodexRateLimitResetRequest")
}

func (codexPlanUsageStrategy) ConsumeReset(
	ctx context.Context,
	bridge *Bridge,
	idempotencyKey string,
	creditID string,
) (map[string]any, planUsageCapture, error) {
	params := map[string]any{"idempotencyKey": strings.TrimSpace(idempotencyKey)}
	if creditID = strings.TrimSpace(creditID); creditID != "" {
		params["creditId"] = creditID
	}
	result, err := bridge.request(ctx, "_workass/codex/rate-limit-reset/consume", params, planUsageRefreshTimeout)
	if err != nil {
		return nil, planUsageCapture{}, err
	}
	rateLimits := mapFromAny(result["rateLimits"])
	if len(rateLimits) == 0 {
		return nil, planUsageCapture{}, errors.New("Codex reset response omitted the refreshed rate limits")
	}
	return result, (codexPlanUsageStrategy{}).Normalize(map[string]any{
		"_meta": map[string]any{"workass.codex/rateLimits": rateLimits},
	}), nil
}

func (claudePlanUsageStrategy) Normalize(carrier map[string]any) planUsageCapture {
	out := planUsageRawCapture(carrier)
	meta := mapFromAny(carrier["_meta"])
	out.entries = append(out.entries, claudeRateLimitPlanUsage(meta)...)
	out.entries = append(out.entries, claudeStructuredPlanUsage(meta)...)
	return appendStandardACPPlanUsage(out, carrier)
}

func (claudePlanUsageStrategy) Supported(bridge *Bridge) bool {
	return bridge != nil && bridge.hasProviderCapability("workassClaudeUsageRequest")
}

func (strategy claudePlanUsageStrategy) Refresh(ctx context.Context, bridge *Bridge, sessionID string) (planUsageCapture, error) {
	result, err := bridge.request(
		ctx,
		"_workass/claude/usage",
		map[string]any{"sessionId": strings.TrimSpace(sessionID)},
		planUsageRefreshTimeout,
	)
	if err != nil {
		return planUsageCapture{}, err
	}
	return strategy.Normalize(map[string]any{
		"_meta": map[string]any{"workass.claude/usage": result},
	}), nil
}

func (claudePlanUsageStrategy) SupportsReset(*Bridge) bool { return false }

func (claudePlanUsageStrategy) ConsumeReset(context.Context, *Bridge, string, string) (map[string]any, planUsageCapture, error) {
	return nil, planUsageCapture{}, errors.New("this provider does not expose earned rate-limit resets")
}

func planUsageRawCapture(carrier map[string]any) planUsageCapture {
	meta := mapFromAny(carrier["_meta"])
	if len(meta) == 0 {
		return planUsageCapture{}
	}
	return planUsageCapture{raw: meta}
}

func appendStandardACPPlanUsage(capture planUsageCapture, carrier map[string]any) planUsageCapture {
	if entry, ok := costPlanUsage(carrier); ok {
		capture.entries = append(capture.entries, entry)
	}
	return capture
}

func codexRateLimitResetCredits(meta map[string]any) (*RateLimitResetCreditsSummary, bool) {
	response := mapFromAny(meta["workass.codex/rateLimits"])
	if len(response) == 0 {
		return nil, false
	}
	raw, present := response["rateLimitResetCredits"]
	if !present || raw == nil {
		return nil, present
	}
	value := mapFromAny(raw)
	if len(value) == 0 {
		return nil, true
	}
	count, _ := int64FromAny(value["availableCount"])
	if count < 0 {
		count = 0
	}
	summary := &RateLimitResetCreditsSummary{AvailableCount: count}
	if rawCredits, detailsPresent := value["credits"]; detailsPresent && rawCredits != nil {
		summary.Credits = make([]RateLimitResetCredit, 0)
		for _, rawCredit := range anySlice(rawCredits) {
			credit := mapFromAny(rawCredit)
			if len(credit) == 0 {
				continue
			}
			summary.Credits = append(summary.Credits, RateLimitResetCredit{
				ID:          boundedPlanUsageText(credit["id"], 500),
				ResetType:   boundedPlanUsageText(credit["resetType"], 80),
				Status:      boundedPlanUsageText(credit["status"], 80),
				GrantedAt:   epochRFC3339(credit["grantedAt"]),
				ExpiresAt:   epochRFC3339(credit["expiresAt"]),
				Title:       boundedPlanUsageText(credit["title"], 200),
				Description: boundedPlanUsageText(credit["description"], 1000),
			})
		}
	}
	return summary, true
}

func boundedPlanUsageText(value any, maxRunes int) string {
	text := strings.TrimSpace(asString(value))
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes])
	}
	return text
}

func claudeRateLimitPlanUsage(meta map[string]any) []PlanUsageEntry {
	rateLimit := mapFromAny(meta["_claude/rateLimit"])
	if len(rateLimit) == 0 {
		return nil
	}
	entry := PlanUsageEntry{
		Kind:          "rate-limit",
		ID:            asString(rateLimit["rateLimitType"]),
		Status:        asString(rateLimit["status"]),
		ResetsAt:      epochRFC3339(rateLimit["resetsAt"]),
		OverageStatus: asString(rateLimit["overageStatus"]),
	}
	if utilization, ok := floatFromAny(rateLimit["utilization"]); ok {
		// SDKRateLimitInfo reports a 0..1 fraction; the structured /usage control
		// response below reports a 0..100 percentage.
		utilization *= 100
		entry.UsedPercent = &utilization
	}
	if v, ok := rateLimit["isUsingOverage"].(bool); ok {
		entry.IsUsingOverage = &v
	}
	return []PlanUsageEntry{entry}
}

func claudeStructuredPlanUsage(meta map[string]any) []PlanUsageEntry {
	usage := mapFromAny(meta["workass.claude/usage"])
	rateLimits := mapFromAny(usage["rate_limits"])
	if len(rateLimits) == 0 {
		return nil
	}
	keys := []string{"five_hour", "seven_day", "seven_day_oauth_apps", "seven_day_opus", "seven_day_sonnet"}
	out := make([]PlanUsageEntry, 0, len(keys))
	for _, key := range keys {
		window := mapFromAny(rateLimits[key])
		if len(window) == 0 {
			continue
		}
		entry := PlanUsageEntry{Kind: "rate-limit", ID: key, ResetsAt: rfc3339Timestamp(window["resets_at"])}
		if pct, ok := floatFromAny(window["utilization"]); ok {
			entry.UsedPercent = &pct
		}
		out = append(out, entry)
	}
	for _, raw := range anySlice(rateLimits["model_scoped"]) {
		window := mapFromAny(raw)
		name := strings.TrimSpace(asString(window["display_name"]))
		if len(window) == 0 || name == "" {
			continue
		}
		entry := PlanUsageEntry{
			Kind:      "rate-limit",
			ID:        "seven_day_model:" + strings.ToLower(name),
			LimitName: name,
			ResetsAt:  rfc3339Timestamp(window["resets_at"]),
		}
		if pct, ok := floatFromAny(window["utilization"]); ok {
			entry.UsedPercent = &pct
		}
		out = append(out, entry)
	}
	return out
}

func codexRateLimitPlanUsage(meta map[string]any) []PlanUsageEntry {
	response := mapFromAny(meta["workass.codex/rateLimits"])
	if len(response) == 0 {
		return nil
	}
	buckets := mapFromAny(response["rateLimitsByLimitId"])
	if len(buckets) == 0 {
		if snapshot := mapFromAny(response["rateLimits"]); len(snapshot) > 0 {
			buckets = map[string]any{"": snapshot}
		}
	}
	keys := make([]string, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	multiple := len(keys) > 1
	var out []PlanUsageEntry
	for _, key := range keys {
		snapshot := mapFromAny(buckets[key])
		if len(snapshot) == 0 {
			continue
		}
		limitID := firstNonEmpty(asString(snapshot["limitId"]), key)
		limitName := firstNonEmpty(asString(snapshot["limitName"]), limitID)
		for _, windowName := range []string{"primary", "secondary"} {
			window := mapFromAny(snapshot[windowName])
			if len(window) == 0 {
				continue
			}
			entry := PlanUsageEntry{
				Kind:      "rate-limit",
				ID:        codexWindowID(limitID, windowName, window["windowDurationMins"], multiple),
				LimitName: limitName,
				ResetsAt:  epochRFC3339(window["resetsAt"]),
			}
			if pct, ok := floatFromAny(window["usedPercent"]); ok {
				entry.UsedPercent = &pct
			}
			if minutes, ok := int64FromAny(window["windowDurationMins"]); ok {
				entry.WindowMinutes = &minutes
			}
			if asString(snapshot["rateLimitReachedType"]) != "" {
				entry.Status = "rejected"
			}
			out = append(out, entry)
		}
	}
	return out
}

func codexWindowID(limitID, windowName string, duration any, multiple bool) string {
	base := ""
	if minutes, ok := int64FromAny(duration); ok {
		switch {
		case minutes == 300:
			base = "five_hour"
		case minutes >= 7*24*60:
			base = "seven_day"
		}
	}
	if base == "" {
		base = windowName
	}
	if multiple && limitID != "" {
		return limitID + ":" + base
	}
	return base
}

func costPlanUsage(carrier map[string]any) (PlanUsageEntry, bool) {
	cost := mapFromAny(carrier["cost"])
	if len(cost) == 0 {
		return PlanUsageEntry{}, false
	}
	entry := PlanUsageEntry{Kind: "cost"}
	if amount, ok := cost["amount"]; ok && amount != nil {
		entry.Amount = amount
	}
	if currency, ok := cost["currency"]; ok {
		entry.Currency = asString(currency)
	}
	return entry, true
}

func codexQuotaPlanUsage(meta map[string]any) (PlanUsageEntry, bool) {
	quota := mapFromAny(meta["quota"])
	if len(quota) == 0 {
		return PlanUsageEntry{}, false
	}
	entry := PlanUsageEntry{Kind: "quota"}
	for _, raw := range anySlice(quota["model_usage"]) {
		modelUsage := mapFromAny(raw)
		if len(modelUsage) == 0 {
			continue
		}
		row := map[string]any{}
		if model, ok := modelUsage["model"]; ok {
			row["model"] = asString(model)
		}
		copyTokenCountFields(row, mapFromAny(modelUsage["token_count"]))
		if len(row) > 0 {
			entry.PerModel = append(entry.PerModel, row)
		}
	}
	if len(entry.PerModel) == 0 {
		row := map[string]any{}
		copyTokenCountFields(row, mapFromAny(quota["token_count"]))
		if len(row) > 0 {
			entry.PerModel = append(entry.PerModel, row)
		}
	}
	return entry, true
}

func copyTokenCountFields(dst map[string]any, tokenCount map[string]any) {
	if len(tokenCount) == 0 {
		return
	}
	known := map[string]struct{}{}
	for _, key := range []string{
		"totalTokens",
		"inputTokens",
		"cachedInputTokens",
		"cachedReadTokens",
		"outputTokens",
		"reasoningOutputTokens",
		"thoughtTokens",
	} {
		known[key] = struct{}{}
		if value, ok := tokenCount[key]; ok {
			dst[key] = value
		}
	}
	var extra []string
	for key := range tokenCount {
		if _, ok := known[key]; !ok {
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)
	for _, key := range extra {
		dst[key] = tokenCount[key]
	}
}

func epochRFC3339(v any) string {
	var sec int64
	switch x := v.(type) {
	case json.Number:
		if n, err := x.Int64(); err == nil {
			sec = n
		} else if f, err := strconv.ParseFloat(x.String(), 64); err == nil {
			sec = int64(f)
		}
	case float64:
		sec = int64(x)
	case float32:
		sec = int64(x)
	case int:
		sec = int64(x)
	case int64:
		sec = x
	case int32:
		sec = int64(x)
	case string:
		if n, err := strconv.ParseInt(x, 10, 64); err == nil {
			sec = n
		} else if f, err := strconv.ParseFloat(x, 64); err == nil {
			sec = int64(f)
		}
	}
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

func rfc3339Timestamp(v any) string {
	if raw, ok := v.(string); ok {
		raw = strings.TrimSpace(raw)
		if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			return parsed.UTC().Format(time.RFC3339)
		}
	}
	return epochRFC3339(v)
}
