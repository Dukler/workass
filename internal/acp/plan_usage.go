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

// recordPlanUsageCapture is the raw provider-carrier entrypoint used by the
// transport boundary. It resolves the registered provider adapter first; only
// the adapter may inspect protocol metadata. The generic snapshot store below
// receives the resulting typed capture.
func (m *Manager) recordPlanUsageCapture(sessionID, providerID string, carrier map[string]any) (PlanUsageSnapshot, bool) {
	providerID = m.resolvePlanUsageProviderID(sessionID, providerID)
	capture := providerAdapterForID(providerID).planUsage.Normalize(carrier)
	return m.recordNormalizedPlanUsageCapture(sessionID, providerID, capture)
}

func (m *Manager) recordNormalizedPlanUsageCapture(sessionID, providerID string, capture planUsageCapture) (PlanUsageSnapshot, bool) {
	// Redaction and the raw-byte ceiling are enforced again at the generic
	// storage boundary so an adapter cannot accidentally expose unbounded or
	// secret-shaped diagnostic metadata.
	capture.raw = boundedPlanUsageRaw(redactPlanUsageValue(capture.raw))
	if len(capture.entries) == 0 && len(capture.raw) == 0 && !capture.resetCreditsSet {
		return PlanUsageSnapshot{}, false
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)

	providerID = m.resolvePlanUsageProviderID(sessionID, providerID)
	m.mu.Lock()
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

func (m *Manager) resolvePlanUsageProviderID(sessionID, providerID string) string {
	if providerID = normalizeProviderID(providerID); providerID != "" {
		return providerID
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if sessionID != "" {
		providerID = normalizeProviderID(m.sessionProvider[sessionID])
	}
	if providerID == "" {
		providerID = normalizeProviderID(m.defaultProviderID)
	}
	if providerID == "" {
		providerID = "unknown"
	}
	return providerID
}

type planUsageCapture struct {
	entries         []PlanUsageEntry
	raw             map[string]any
	resetCredits    *RateLimitResetCreditsSummary
	resetCreditsSet bool
}

func anySlice(v any) []any {
	items, _ := v.([]any)
	return items
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
	for {
		select {
		case <-m.loopStop:
			return
		case <-ticker.C:
			m.refreshLivePlanUsage()
		}
	}
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
	if m.isProductionRuntime() && providerIsFixture(providerID) {
		m.mu.Unlock()
		return errors.New("development fixture provider is unavailable in production")
	}
	if _, err := m.providerConfigLocked(providerID); err != nil {
		m.mu.Unlock()
		return err
	}
	providerOptions, err := m.optionsForProviderLocked(providerID)
	if err != nil {
		m.mu.Unlock()
		return err
	}
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
	return providerAdapterForID(b.ProviderID()).planUsage.Supported(b)
}

func (b *Bridge) supportsPlanUsageReset() bool {
	return providerAdapterForID(b.ProviderID()).planUsage.SupportsReset(b)
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
	if b == nil || !b.supportsPlanUsageReset() {
		m.mu.Lock()
		candidates := make([]*Bridge, 0, len(m.bridges))
		for _, candidate := range m.bridges {
			candidates = append(candidates, candidate)
		}
		m.mu.Unlock()
		for _, candidate := range candidates {
			if candidate != nil &&
				normalizeProviderID(candidate.ProviderID()) == providerID &&
				!candidate.Closed() && !candidate.Hibernated() && candidate.supportsPlanUsageReset() {
				b = candidate
				break
			}
		}
	}
	if b == nil {
		return nil, errors.New("no initialized Codex account bridge is available")
	}
	if !b.supportsPlanUsageReset() {
		return nil, errors.New("this provider does not expose earned rate-limit resets")
	}
	strategy := providerAdapterForID(providerID).planUsage
	result, capture, err := strategy.ConsumeReset(ctx, b, idempotencyKey, creditID)
	if err != nil {
		return nil, err
	}
	snapshot, ok := m.recordNormalizedPlanUsageCapture(sessionID, providerID, capture)
	if !ok {
		return nil, errors.New("Codex reset response contained no usable rate-limit snapshot")
	}
	result["planUsage"] = snapshot
	return result, nil
}

func (b *Bridge) refreshNativePlanUsage(ctx context.Context, sessionID string) error {
	providerID := b.ProviderID()
	strategy := providerAdapterForID(providerID).planUsage
	if !strategy.Supported(b) {
		return nil
	}
	capture, err := strategy.Refresh(ctx, b, sessionID)
	if err != nil {
		return err
	}
	b.manager.recordNormalizedPlanUsageCapture(sessionID, providerID, capture)
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
