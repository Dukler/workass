package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	planUsageRawLimitBytes   = 8 * 1024
	planUsageRefreshTimeout  = 20 * time.Second
	planUsageRefreshLogLabel = "provider plan usage refresh failed"
)

type PlanUsageSnapshot struct {
	ProviderID            string                        `json:"providerId"`
	CapturedAt            string                        `json:"capturedAt"`
	Entries               []PlanUsageEntry              `json:"entries,omitempty"`
	RateLimitResetCredits *RateLimitResetCreditsSummary `json:"rateLimitResetCredits,omitempty"`
	Raw                   map[string]any                `json:"raw,omitempty"`
}

type RateLimitResetCreditsSummary struct {
	AvailableCount int64                  `json:"availableCount"`
	Credits        []RateLimitResetCredit `json:"credits"`
}

type RateLimitResetCredit struct {
	ID          string `json:"id,omitempty"`
	ResetType   string `json:"resetType,omitempty"`
	Status      string `json:"status,omitempty"`
	GrantedAt   string `json:"grantedAt,omitempty"`
	ExpiresAt   string `json:"expiresAt,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

type PlanUsageEntry struct {
	Kind           string           `json:"kind"`
	ID             string           `json:"id,omitempty"`
	Status         string           `json:"status,omitempty"`
	ResetsAt       string           `json:"resetsAt,omitempty"`
	UsedPercent    *float64         `json:"usedPercent,omitempty"`
	WindowMinutes  *int64           `json:"windowMinutes,omitempty"`
	LimitName      string           `json:"limitName,omitempty"`
	OverageStatus  string           `json:"overageStatus,omitempty"`
	IsUsingOverage *bool            `json:"isUsingOverage,omitempty"`
	Amount         any              `json:"amount,omitempty"`
	Currency       string           `json:"currency,omitempty"`
	PerModel       []map[string]any `json:"perModel,omitempty"`
}

func (m *Manager) recordPlanUsageCapture(sessionID, providerID string, carrier map[string]any) (PlanUsageSnapshot, bool) {
	capture := extractPlanUsageCapture(carrier)
	if len(capture.entries) == 0 && len(capture.raw) == 0 && !capture.resetCreditsSet {
		return PlanUsageSnapshot{}, false
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)

	providerID = normalizeProviderID(providerID)
	m.mu.Lock()
	if providerID == "" && sessionID != "" {
		providerID = normalizeProviderID(m.sessionProvider[sessionID])
	}
	if providerID == "" {
		providerID = m.defaultProviderID
	}
	if providerID == "" {
		providerID = "unknown"
	}
	prev := m.planUsageByProvider[providerID]
	next := PlanUsageSnapshot{
		ProviderID:            providerID,
		CapturedAt:            now,
		Entries:               mergePlanUsageEntries(prev.Entries, capture.entries),
		RateLimitResetCredits: prev.RateLimitResetCredits,
		Raw:                   mergePlanUsageRaw(prev.Raw, capture.raw),
	}
	if capture.resetCreditsSet {
		next.RateLimitResetCredits = capture.resetCredits
	}
	m.planUsageByProvider[providerID] = next
	m.mu.Unlock()

	m.emit("chat:plan-usage", next)
	return next, true
}

type planUsageCapture struct {
	entries         []PlanUsageEntry
	raw             map[string]any
	resetCredits    *RateLimitResetCreditsSummary
	resetCreditsSet bool
}

func extractPlanUsageCapture(carrier map[string]any) planUsageCapture {
	if carrier == nil {
		return planUsageCapture{}
	}
	meta := mapFromAny(carrier["_meta"])
	out := planUsageCapture{}
	if len(meta) > 0 {
		out.raw = boundedPlanUsageRaw(redactPlanUsageValue(meta))
		out.entries = append(out.entries, claudeRateLimitPlanUsage(meta)...)
		out.entries = append(out.entries, claudeStructuredPlanUsage(meta)...)
		out.entries = append(out.entries, codexRateLimitPlanUsage(meta)...)
		out.resetCredits, out.resetCreditsSet = codexRateLimitResetCredits(meta)
		if entry, ok := codexQuotaPlanUsage(meta); ok {
			out.entries = append(out.entries, entry)
		}
	}
	if entry, ok := costPlanUsage(carrier); ok {
		out.entries = append(out.entries, entry)
	}
	return out
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

func anySlice(v any) []any {
	items, _ := v.([]any)
	return items
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

func floatFromAny(v any) (float64, bool) {
	var value float64
	switch x := v.(type) {
	case json.Number:
		parsed, err := x.Float64()
		if err != nil {
			return 0, false
		}
		value = parsed
	case float64:
		value = x
	case float32:
		value = float64(x)
	case int:
		value = float64(x)
	case int64:
		value = float64(x)
	case int32:
		value = float64(x)
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0, false
		}
		value = parsed
	default:
		return 0, false
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

func int64FromAny(v any) (int64, bool) {
	value, ok := floatFromAny(v)
	if !ok || value != math.Trunc(value) || value > math.MaxInt64 || value < math.MinInt64 {
		return 0, false
	}
	return int64(value), true
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

type planUsageRefreshRun struct {
	cancel  context.CancelFunc
	pending *planUsageRefreshTarget
}

type planUsageRefreshTarget struct {
	bridge    *Bridge
	sessionID string
}

func (m *Manager) planUsageLoop() {
	ticker := time.NewTicker(m.opts.PlanUsageRefreshInterval)
	defer ticker.Stop()
	m.runPlanUsageTicks(ticker.C)
}

func (m *Manager) runPlanUsageTicks(ticks <-chan time.Time) {
	for range ticks {
		m.refreshLivePlanUsage()
	}
}

// Refresh one initialized, non-hibernated session per provider. Plan usage is
// account-scoped, so querying multiple chats for the same provider would only
// duplicate work. Metadata reads do not update bridge activity and therefore do
// not keep an otherwise idle engine alive past the normal hibernation TTL.
func (m *Manager) refreshLivePlanUsage() {
	m.mu.Lock()
	if m.resetting {
		m.mu.Unlock()
		return
	}
	ids := make([]string, 0, len(m.sessionBridge))
	bridges := make(map[string]*Bridge, len(m.sessionBridge))
	for sessionID, bridge := range m.sessionBridge {
		ids = append(ids, sessionID)
		bridges[sessionID] = bridge
	}
	m.mu.Unlock()
	sort.Strings(ids)
	seen := make(map[string]struct{})
	for _, sessionID := range ids {
		bridge := bridges[sessionID]
		if bridge == nil || bridge.Closed() || bridge.Hibernated() {
			continue
		}
		live, ok := bridge.liveSession(sessionID)
		if !ok || !bridge.supportsNativePlanUsage() {
			continue
		}
		providerID := normalizeProviderID(live.Info.ProviderID)
		if _, exists := seen[providerID]; exists {
			continue
		}
		seen[providerID] = struct{}{}
		m.schedulePlanUsageRefresh(bridge, sessionID)
	}
}

// RefreshPlanUsageSession is the explicit active-chat metadata boundary. It
// reuses a live session when possible and otherwise resumes/creates that exact
// chat without issuing a provider prompt. A fresh/restored attach already
// schedules its native usage read; the live-session path schedules it here.
func (m *Manager) RefreshPlanUsageSession(ctx context.Context, opts SessionOptions) (SessionInfo, error) {
	if strings.TrimSpace(opts.TabID) == "" || strings.TrimSpace(opts.ChatID) == "" {
		return SessionInfo{}, errors.New("plan usage refresh requires tabId and chatId")
	}
	providerID := normalizeProviderID(opts.ProviderID)
	for _, live := range m.liveSessionsForChat(opts.TabID, opts.ChatID) {
		if providerID != "" && normalizeProviderID(live.Info.ProviderID) != providerID {
			continue
		}
		bridge := m.bridgeForSession(live.Info.SessionID, opts)
		if bridge != nil && bridge.supportsNativePlanUsage() {
			m.schedulePlanUsageRefresh(bridge, live.Info.SessionID)
		}
		return live.Info, nil
	}
	return m.NewSession(ctx, opts)
}

// RefreshProviderPlanUsage is the account-scoped metadata boundary used by the
// context popover. Unlike RefreshPlanUsageSession it never acquires a chat
// lifecycle lock, changes a chat binding, or sends session/prompt. It reuses any
// live session for the requested provider; a cold provider gets one disposable
// ephemeral session that is closed before this call returns.
func (m *Manager) RefreshProviderPlanUsage(ctx context.Context, providerID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	providerID = normalizeProviderID(providerID)
	if providerID == "" {
		return errors.New("plan usage refresh requires providerId")
	}

	m.mu.Lock()
	if m.resetting {
		m.mu.Unlock()
		return errors.New("ACP manager is resetting")
	}
	if m.isProductionRuntime() && providerID == "mock" {
		m.mu.Unlock()
		return errors.New("development fixture provider is unavailable in production")
	}
	if _, err := m.providerConfigLocked(providerID); err != nil {
		m.mu.Unlock()
		return err
	}
	providerOptions := m.optionsForProviderLocked(providerID)
	m.bridgeSeq++
	bridgeKey := fmt.Sprintf("plan-usage:%s:%d", providerID, m.bridgeSeq)
	m.mu.Unlock()

	if target, ok := m.liveProviderPlanUsageTarget(providerID); ok {
		return target.bridge.refreshNativePlanUsage(ctx, target.sessionID)
	}

	bridge := newBridge(bridgeKey, providerOptions, m)
	info, err := bridge.NewSession(ctx, SessionOptions{
		BridgeKey: bridgeKey, ProviderID: providerID, Ephemeral: true,
	})
	if err != nil {
		bridge.Close(false, err)
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		bridge.CloseSession(closeCtx, info.SessionID)
	}()
	if !bridge.supportsNativePlanUsage() {
		return fmt.Errorf("provider %q does not expose native plan usage", providerID)
	}
	return bridge.refreshNativePlanUsage(ctx, info.SessionID)
}

func (m *Manager) liveProviderPlanUsageTarget(providerID string) (planUsageRefreshTarget, bool) {
	m.mu.Lock()
	ids := make([]string, 0, len(m.sessionBridge))
	bridges := make(map[string]*Bridge, len(m.sessionBridge))
	for sessionID, bridge := range m.sessionBridge {
		ids = append(ids, sessionID)
		bridges[sessionID] = bridge
	}
	m.mu.Unlock()
	sort.Strings(ids)
	for _, sessionID := range ids {
		bridge := bridges[sessionID]
		live, ok := bridge.liveSession(sessionID)
		if !ok || normalizeProviderID(live.Info.ProviderID) != providerID || !bridge.supportsNativePlanUsage() {
			continue
		}
		return planUsageRefreshTarget{bridge: bridge, sessionID: sessionID}, true
	}
	return planUsageRefreshTarget{}, false
}

func (m *Manager) schedulePlanUsageRefresh(bridge *Bridge, sessionID string) {
	if bridge == nil || strings.TrimSpace(sessionID) == "" || !bridge.supportsNativePlanUsage() {
		return
	}
	m.mu.Lock()
	if m.resetting {
		m.mu.Unlock()
		return
	}

	providerID := normalizeProviderID(bridge.ProviderID())
	if providerID == "" {
		m.mu.Unlock()
		return
	}
	m.planUsageRefreshMu.Lock()
	if run := m.planUsageRefreshes[providerID]; run != nil {
		// Session attach and terminal-turn refreshes can overlap. Preserve one
		// trailing request so a slow attach-time read cannot swallow the required
		// post-turn refresh; newer requests replace older pending ones because plan
		// usage is provider-scoped and only the latest snapshot is retained.
		run.pending = &planUsageRefreshTarget{bridge: bridge, sessionID: sessionID}
		m.planUsageRefreshMu.Unlock()
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), planUsageRefreshTimeout)
	run := &planUsageRefreshRun{cancel: cancel}
	m.planUsageRefreshes[providerID] = run
	m.planUsageRefreshWG.Add(1)
	m.planUsageRefreshMu.Unlock()
	m.mu.Unlock()

	go func() {
		defer m.planUsageRefreshWG.Done()
		target := planUsageRefreshTarget{bridge: bridge, sessionID: sessionID}
		for {
			err := target.bridge.refreshNativePlanUsage(ctx, target.sessionID)
			cancel()
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				m.opts.Logf(planUsageRefreshLogLabel, map[string]any{
					"providerId": providerID,
					"error":      redactSensitiveText(err.Error()),
				})
			}

			m.mu.Lock()
			resetting := m.resetting
			m.mu.Unlock()
			m.planUsageRefreshMu.Lock()
			if m.planUsageRefreshes[providerID] != run || resetting || run.pending == nil {
				if m.planUsageRefreshes[providerID] == run {
					delete(m.planUsageRefreshes, providerID)
				}
				m.planUsageRefreshMu.Unlock()
				return
			}
			target = *run.pending
			run.pending = nil
			ctx, cancel = context.WithTimeout(context.Background(), planUsageRefreshTimeout)
			run.cancel = cancel
			m.planUsageRefreshMu.Unlock()
		}
	}()
}

func (m *Manager) stopPlanUsageRefreshes() {
	m.planUsageRefreshMu.Lock()
	for _, run := range m.planUsageRefreshes {
		run.cancel()
	}
	m.planUsageRefreshMu.Unlock()
	m.planUsageRefreshWG.Wait()
	m.planUsageRefreshMu.Lock()
	m.planUsageRefreshes = make(map[string]*planUsageRefreshRun)
	m.planUsageRefreshMu.Unlock()
}

func (b *Bridge) supportsNativePlanUsage() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch steeringProviderFamily(b.providerID) {
	case "codex":
		return boolMapField(b.agentMeta, "workassCodexRateLimitsRequest") ||
			boolMapField(b.agentCaps, "workassCodexRateLimitsRequest")
	case "claude":
		return boolMapField(b.agentMeta, "workassClaudeUsageRequest") ||
			boolMapField(b.agentCaps, "workassClaudeUsageRequest")
	default:
		return false
	}
}

func (b *Bridge) supportsCodexRateLimitReset() bool {
	if steeringProviderFamily(b.ProviderID()) != "codex" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return boolMapField(b.agentMeta, "workassCodexRateLimitResetRequest") ||
		boolMapField(b.agentCaps, "workassCodexRateLimitResetRequest")
}

// ConsumeRateLimitResetCredit spends one provider-issued Codex reset credit.
// The caller owns the idempotency key and must reuse it when retrying the same
// logical click. The adapter performs the required authoritative rate-limit
// refetch before replying; Workass records that snapshot before returning.
func (m *Manager) ConsumeRateLimitResetCredit(ctx context.Context, providerID, sessionID, idempotencyKey, creditID string) (map[string]any, error) {
	providerID = normalizeProviderID(providerID)
	sessionID = strings.TrimSpace(sessionID)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	creditID = strings.TrimSpace(creditID)
	if providerID != "codex" {
		return nil, errors.New("earned rate-limit resets require the Codex provider")
	}
	if idempotencyKey == "" || len(idempotencyKey) > 200 {
		return nil, errors.New("a bounded idempotency key is required")
	}
	if len(creditID) > 500 {
		return nil, errors.New("rate-limit reset credit id is too long")
	}
	var b *Bridge
	if sessionID != "" {
		b = m.bridgeForSession(sessionID, SessionOptions{SessionID: sessionID, ProviderID: providerID})
	}
	if b == nil || !b.supportsCodexRateLimitReset() {
		m.mu.Lock()
		candidates := make([]*Bridge, 0, len(m.bridges))
		for _, candidate := range m.bridges {
			candidates = append(candidates, candidate)
		}
		m.mu.Unlock()
		for _, candidate := range candidates {
			if candidate != nil && !candidate.Closed() && !candidate.Hibernated() && candidate.supportsCodexRateLimitReset() {
				b = candidate
				break
			}
		}
	}
	if b == nil {
		return nil, errors.New("no initialized Codex account bridge is available")
	}
	if !b.supportsCodexRateLimitReset() {
		return nil, errors.New("this provider does not expose earned rate-limit resets")
	}
	params := map[string]any{"idempotencyKey": idempotencyKey}
	if creditID != "" {
		params["creditId"] = creditID
	}
	result, err := b.request(ctx, "_workass/codex/rate-limit-reset/consume", params, planUsageRefreshTimeout)
	if err != nil {
		return nil, err
	}
	rateLimits := mapFromAny(result["rateLimits"])
	if len(rateLimits) == 0 {
		return nil, errors.New("Codex reset response omitted the refreshed rate limits")
	}
	snapshot, ok := m.recordPlanUsageCapture(sessionID, providerID, map[string]any{
		"_meta": map[string]any{"workass.codex/rateLimits": rateLimits},
	})
	if !ok {
		return nil, errors.New("Codex reset response contained no usable rate-limit snapshot")
	}
	result["planUsage"] = snapshot
	return result, nil
}

func (b *Bridge) refreshNativePlanUsage(ctx context.Context, sessionID string) error {
	providerID := b.ProviderID()
	var (
		result map[string]any
		err    error
		key    string
	)
	switch steeringProviderFamily(providerID) {
	case "codex":
		result, err = b.request(ctx, "_workass/codex/rate-limits", map[string]any{}, planUsageRefreshTimeout)
		key = "workass.codex/rateLimits"
	case "claude":
		result, err = b.request(ctx, "_workass/claude/usage", map[string]any{"sessionId": sessionID}, planUsageRefreshTimeout)
		key = "workass.claude/usage"
	default:
		return nil
	}
	if err != nil {
		return err
	}
	b.manager.recordPlanUsageCapture(sessionID, providerID, map[string]any{
		"_meta": map[string]any{key: result},
	})
	return nil
}

func mergePlanUsageEntries(existing, incoming []PlanUsageEntry) []PlanUsageEntry {
	if len(existing) == 0 {
		return append([]PlanUsageEntry(nil), incoming...)
	}
	out := append([]PlanUsageEntry(nil), existing...)
	for _, entry := range incoming {
		key := planUsageEntryKey(entry)
		replaced := false
		for i := range out {
			if planUsageEntryKey(out[i]) == key {
				out[i] = entry
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, entry)
		}
	}
	return out
}

func planUsageEntryKey(entry PlanUsageEntry) string {
	switch entry.Kind {
	case "rate-limit":
		return entry.Kind + ":" + entry.ID
	case "cost":
		return entry.Kind + ":" + entry.Currency
	default:
		if entry.ID != "" {
			return entry.Kind + ":" + entry.ID
		}
		return entry.Kind
	}
}

func mergePlanUsageRaw(existing, incoming map[string]any) map[string]any {
	if len(existing) == 0 && len(incoming) == 0 {
		return nil
	}
	if len(incoming) == 0 {
		return copyAnyMap(existing)
	}
	if planUsageRawTruncated(incoming) {
		return copyAnyMap(incoming)
	}
	merged := copyAnyMap(existing)
	if merged == nil {
		merged = map[string]any{}
	}
	for key, value := range incoming {
		merged[key] = value
	}
	return boundedPlanUsageRaw(merged)
}

func planUsageRawTruncated(raw map[string]any) bool {
	v, _ := raw["_truncated"].(bool)
	return v
}

func boundedPlanUsageRaw(raw any) map[string]any {
	m, _ := raw.(map[string]any)
	if len(m) == 0 {
		return nil
	}
	data, err := json.Marshal(m)
	if err != nil {
		return map[string]any{"_note": "raw _meta could not be encoded", "_truncated": true, "_limitBytes": planUsageRawLimitBytes}
	}
	if len(data) <= planUsageRawLimitBytes {
		return m
	}
	return map[string]any{
		"_note":       "raw _meta exceeded 8192 bytes and was dropped",
		"_truncated":  true,
		"_bytes":      len(data),
		"_limitBytes": planUsageRawLimitBytes,
	}
}

func redactPlanUsageValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for key, value := range x {
			if secretKeyRE.MatchString(key) {
				out[key] = "[redacted]"
				continue
			}
			out[key] = redactPlanUsageValue(value)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, value := range x {
			out[i] = redactPlanUsageValue(value)
		}
		return out
	case string:
		return redactSensitiveText(x)
	default:
		return x
	}
}

func copyAnyMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
