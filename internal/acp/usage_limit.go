package acp

import (
	"errors"
	"strings"
	"time"

	providercontract "workass/internal/provider"
)

// A turn the subscription's usage limit stopped (owner 2026-11-18). The
// native host marks the prompt's failure with error.data.workassUsageLimit
// (Claude: the blocking rate_limit_event or a 429 result; Codex:
// usageLimitExceeded). When the provider gave no reset time, the account's
// plan usage says when its exhausted window ends. The chat then offers
// Continuar, which sends "continue" now or once that time has passed.
func (m *Manager) usageLimitFromPromptError(bridge *Bridge, providerID string, err error) *providercontract.UsageLimit {
	var rpc *acpError
	if !errors.As(err, &rpc) {
		return nil
	}
	raw := mapFromAny(mapFromAny(rpc.Data)["workassUsageLimit"])
	if len(raw) == 0 {
		return nil
	}
	limit := &providercontract.UsageLimit{
		ResetsAt: normalizeResetTime(asString(raw["resetsAt"])),
		Kind:     strings.TrimSpace(asString(raw["kind"])),
	}
	providerName := ""
	if bridge != nil {
		bridge.mu.Lock()
		limit.AccountScope = bridge.accountScope
		providerName = bridge.providerName
		bridge.mu.Unlock()
	}
	m.mu.Lock()
	snapshot := m.planUsageByProvider[providerID]
	m.mu.Unlock()
	plan := ""
	if snapshot.Account != nil {
		plan = strings.TrimSpace(snapshot.Account.Plan)
	}
	limit.Label = usageLimitLabel(providerName, plan)
	if limit.ResetsAt == "" {
		limit.ResetsAt = exhaustedWindowReset(snapshot.Entries, time.Now())
	}
	return limit
}

// "Claude Max", "ChatGPT Pro": the plan names the provider when it already
// says whose it is.
func usageLimitLabel(providerName, plan string) string {
	providerName = strings.TrimSpace(providerName)
	switch {
	case plan == "":
		return providerName
	case providerName == "" || strings.HasPrefix(strings.ToLower(plan), strings.ToLower(providerName)) || strings.HasPrefix(plan, "ChatGPT"):
		return plan
	default:
		return providerName + " " + plan
	}
}

// The latest reset among the windows the account has used up: the limit is
// back only when every exhausted window is.
func exhaustedWindowReset(entries []PlanUsageEntry, now time.Time) string {
	var latest time.Time
	for _, entry := range entries {
		if entry.UsedPercent == nil || *entry.UsedPercent < 100 {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(entry.ResetsAt))
		if err != nil || !at.After(now) {
			continue
		}
		if at.After(latest) {
			latest = at
		}
	}
	if latest.IsZero() {
		return ""
	}
	return latest.UTC().Format(time.RFC3339)
}

func normalizeResetTime(value string) string {
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

func usageLimitWire(limit *providercontract.UsageLimit) map[string]any {
	out := map[string]any{"label": limit.Label}
	if limit.ResetsAt != "" {
		out["resetsAt"] = limit.ResetsAt
	}
	if limit.Kind != "" {
		out["kind"] = limit.Kind
	}
	if limit.AccountScope != "" {
		out["accountScope"] = limit.AccountScope
	}
	return out
}

func usageLimitFromWire(raw map[string]any) *providercontract.UsageLimit {
	if len(raw) == 0 {
		return nil
	}
	return &providercontract.UsageLimit{
		ResetsAt:     normalizeResetTime(asString(raw["resetsAt"])),
		Kind:         strings.TrimSpace(asString(raw["kind"])),
		Label:        strings.TrimSpace(asString(raw["label"])),
		AccountScope: strings.TrimSpace(asString(raw["accountScope"])),
	}
}

// UsageLimitReset is when the provider's account is back, read from its plan
// usage as it stands: the latest reset among its exhausted windows, or "".
func (m *Manager) UsageLimitReset(providerID string, now time.Time) string {
	m.mu.Lock()
	entries := append([]PlanUsageEntry(nil), m.planUsageByProvider[providerID].Entries...)
	m.mu.Unlock()
	return exhaustedWindowReset(entries, now)
}

// UsageLimitStop is a turn the subscription's usage limit stopped, for the
// daemon's opt-in "continue when it comes back" (Ajustes · General).
type UsageLimitStop struct {
	TabID, ChatID, AssistantMessageID, ProviderID string
	Limit                                         providercontract.UsageLimit
}

// SetUsageLimitObserver hands the manager the daemon's observer for usage-limit
// stops. It is called off the turn's goroutine and decides nothing here.
func (m *Manager) SetUsageLimitObserver(observe func(UsageLimitStop)) {
	if m == nil {
		return
	}
	m.usageLimitMu.Lock()
	m.usageLimitObserver = observe
	m.usageLimitMu.Unlock()
}

func (m *Manager) noteUsageLimitStop(job *Job) {
	if m == nil || job == nil || job.UsageLimit == nil {
		return
	}
	m.usageLimitMu.Lock()
	observe := m.usageLimitObserver
	m.usageLimitMu.Unlock()
	if observe == nil {
		return
	}
	stop := UsageLimitStop{TabID: job.TabID, ChatID: job.ChatID, AssistantMessageID: job.startOpts.AssistantMessageID,
		ProviderID: job.ProviderID, Limit: *job.UsageLimit}
	go observe(stop)
}
