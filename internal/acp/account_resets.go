package acp

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"time"
)

// Some providers keep their earned rate-limit resets on their account API
// rather than behind their CLI, so no provider session can read or claim
// them. Workass reaches that API with the login the provider's own CLI
// stored, and only for a provider the owner consented to on the Cuentas page
// (the settings key accountResetConsent, checked through
// SetAccountResetConsent before any login is touched). Workass never
// refreshes or rotates that login: a rotation could sign the CLI out.
//
// The provider-specific half is an accountResetSource registered on the
// provider's adapter; this file is the neutral half the daemon calls.

const (
	// accountResetSourceAccount marks reset summaries read here, so a
	// withdrawn consent drops exactly them.
	accountResetSourceAccount = "account"
	// accountResetBackgroundEvery spaces the unattended reads that keep the
	// left panel's dot current while the Cuentas page is closed.
	accountResetBackgroundEvery = 15 * time.Minute
	accountResetTimeout         = 30 * time.Second
)

type accountResetSource interface {
	// ConsentNote is the one line shown under the consent button: what the
	// operating system will ask, if anything.
	ConsentNote() string
	Status(ctx context.Context) (accountResetStatus, error)
	// Claim spends one use of creditID. requestID is the caller's idempotency
	// key, reused on a retry of the same click.
	Claim(ctx context.Context, creditID, requestID string) (accountResetClaim, error)
}

type accountResetStatus struct {
	Credits    *RateLimitResetCreditsSummary
	Entries    []PlanUsageEntry
	ExtraUsage *PlanUsageExtraUsage
	// ExtraUsageSet distinguishes "the API said nothing about it" from "the
	// API said there is none".
	ExtraUsageSet bool
}

type accountResetClaim struct {
	// Outcome is one of app-chat:use-rate-limit-reset's outcomes: reset,
	// alreadyRedeemed, nothingToReset or noCredit.
	Outcome string
	// Refilled lists the windows the claim refilled, as plan-usage entry ids.
	Refilled  []string
	Remaining *int64
	// Message says, in plain words, why nothing was reset.
	Message string
}

// accountResetError is a failure the owner can act on. Its message is shown
// as is, so it is written for the owner, not for a log.
type accountResetError struct {
	code    string
	message string
}

func (e *accountResetError) Error() string { return e.message }

const (
	accountResetNeedsConsent = "consent"
	accountResetNoLogin      = "no-login"
	accountResetExpired      = "expired"
	accountResetAuth         = "auth"
	accountResetRateLimited  = "rate-limited"
	accountResetUnavailable  = "unavailable"
)

func newAccountResetError(code, message string) error {
	return &accountResetError{code: code, message: message}
}

type accountResetState struct {
	mu       sync.Mutex
	consent  func(providerID string) bool
	sources  map[string]accountResetSource
	lastRead map[string]time.Time
	inFlight map[string]bool
}

// providerAccountListFields are the providers:list fields the Cuentas page
// reads: accountUsage (the adapter reads plan usage for the account),
// accountResetConsent (resets need the owner's consent to read the login,
// with the line shown under that button) and localEndpoint (host:port of a
// model server on this machine).
func providerAccountListFields(providerID string, localServers []localModelServer) map[string]any {
	fields := map[string]any{}
	adapter := providerAdapterForID(providerID)
	if _, unsupported := adapter.planUsage.(unsupportedPlanUsageStrategy); !unsupported {
		fields["accountUsage"] = true
	}
	if adapter.accountResets != nil {
		fields["accountResetConsent"] = map[string]any{"note": adapter.accountResets.ConsentNote()}
	}
	if providerIsLocal(providerID) {
		for _, server := range localServers {
			if server.ProviderID != normalizeProviderID(providerID) {
				continue
			}
			if parsed, err := url.Parse(server.Endpoint); err == nil && parsed.Host != "" {
				fields["localEndpoint"] = parsed.Host
			}
		}
	}
	return fields
}

// SetAccountResetConsent hands the manager the owner's consent as the
// app-settings blob records it. Until it is set no login is ever read.
func (m *Manager) SetAccountResetConsent(consent func(providerID string) bool) {
	m.accountResets.mu.Lock()
	m.accountResets.consent = consent
	m.accountResets.mu.Unlock()
}

// setAccountResetSourceForTest replaces one provider's source, so tests never
// read a real login or reach a real account API.
func (m *Manager) setAccountResetSourceForTest(providerID string, source accountResetSource) {
	m.accountResets.mu.Lock()
	defer m.accountResets.mu.Unlock()
	if m.accountResets.sources == nil {
		m.accountResets.sources = make(map[string]accountResetSource)
	}
	m.accountResets.sources[normalizeProviderID(providerID)] = source
}

func (m *Manager) accountResetSourceFor(providerID string) accountResetSource {
	providerID = normalizeProviderID(providerID)
	if providerID == "" {
		return nil
	}
	m.accountResets.mu.Lock()
	source, overridden := m.accountResets.sources[providerID]
	m.accountResets.mu.Unlock()
	if overridden {
		return source
	}
	return providerAdapterForID(providerID).accountResets
}

func (m *Manager) accountResetConsented(providerID string) bool {
	m.accountResets.mu.Lock()
	consent := m.accountResets.consent
	m.accountResets.mu.Unlock()
	return consent != nil && consent(normalizeProviderID(providerID))
}

// RefreshAccountResets reads one provider's resets from its account API and
// records them on that provider's plan-usage snapshot (chat:plan-usage). It
// refuses before touching any login unless the owner consented, and the
// refusal also drops resets read under a consent since withdrawn.
func (m *Manager) RefreshAccountResets(ctx context.Context, providerID string) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	providerID = normalizeProviderID(providerID)
	source := m.accountResetSourceFor(providerID)
	if source == nil {
		return nil, errors.New("this provider has no account resets")
	}
	if !m.accountResetConsented(providerID) {
		m.forgetAccountResets(providerID)
		return nil, newAccountResetError(accountResetNeedsConsent, "Workass no tiene tu permiso para usar este login.")
	}
	if err := m.readAccountResets(ctx, providerID, source); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "providerId": providerID}, nil
}

func (m *Manager) readAccountResets(ctx context.Context, providerID string, source accountResetSource) error {
	ctx, cancel := context.WithTimeout(ctx, accountResetTimeout)
	defer cancel()
	status, err := source.Status(ctx)
	m.accountResets.mu.Lock()
	if m.accountResets.lastRead == nil {
		m.accountResets.lastRead = make(map[string]time.Time)
	}
	m.accountResets.lastRead[providerID] = time.Now()
	m.accountResets.mu.Unlock()
	if err != nil {
		return err
	}
	credits := status.Credits
	if credits == nil {
		credits = &RateLimitResetCreditsSummary{Credits: []RateLimitResetCredit{}}
	}
	credits.Source = accountResetSourceAccount
	m.recordNormalizedPlanUsageCapture("", providerID, planUsageCapture{
		entries:         status.Entries,
		resetCredits:    credits,
		resetCreditsSet: true,
		extraUsage:      status.ExtraUsage,
		extraUsageSet:   status.ExtraUsageSet,
	})
	return nil
}

// forgetAccountResets drops resets this file recorded, leaving resets a
// provider session reported alone.
func (m *Manager) forgetAccountResets(providerID string) {
	m.mu.Lock()
	current := m.planUsageByProvider[providerID].RateLimitResetCredits
	m.mu.Unlock()
	if current == nil || current.Source != accountResetSourceAccount {
		return
	}
	m.recordNormalizedPlanUsageCapture("", providerID, planUsageCapture{resetCreditsSet: true})
}

func (m *Manager) claimAccountReset(ctx context.Context, providerID string, source accountResetSource, idempotencyKey, creditID string) (map[string]any, error) {
	if !m.accountResetConsented(providerID) {
		return nil, newAccountResetError(accountResetNeedsConsent, "Workass no tiene tu permiso para usar este login.")
	}
	if creditID == "" {
		return nil, errors.New("an account reset claim names the reset it spends")
	}
	claimCtx, cancel := context.WithTimeout(ctx, accountResetTimeout)
	defer cancel()
	claim, err := source.Claim(claimCtx, creditID, idempotencyKey)
	if err != nil {
		return nil, err
	}
	result := map[string]any{"outcome": claim.Outcome}
	if len(claim.Refilled) > 0 {
		result["refilled"] = claim.Refilled
	}
	if claim.Remaining != nil {
		result["remaining"] = *claim.Remaining
	}
	if claim.Message != "" {
		result["message"] = claim.Message
	}
	// The claim changed the account's resets and, when it refilled a window,
	// its usage. A failed re-read leaves the claim's answer standing.
	if err := m.readAccountResets(context.Background(), providerID, source); err == nil {
		m.mu.Lock()
		result["planUsage"] = m.planUsageByProvider[providerID]
		m.mu.Unlock()
	}
	return result, nil
}

// refreshConsentedAccountResets keeps consented providers' resets current
// without the Cuentas page open, at most once per accountResetBackgroundEvery.
func (m *Manager) refreshConsentedAccountResets() {
	m.mu.Lock()
	if m.resetting {
		m.mu.Unlock()
		return
	}
	ids := make([]string, 0, len(m.providerOrder))
	for _, id := range m.providerOrder {
		if runtime := m.providers[id]; runtime != nil && runtime.Config.Enabled && !runtime.Config.DisabledByUser {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	now := time.Now()
	for _, id := range ids {
		source := m.accountResetSourceFor(id)
		if source == nil || !m.accountResetConsented(id) {
			continue
		}
		m.accountResets.mu.Lock()
		last := m.accountResets.lastRead[id]
		if m.accountResets.inFlight[id] || (!last.IsZero() && now.Sub(last) < accountResetBackgroundEvery) {
			m.accountResets.mu.Unlock()
			continue
		}
		if m.accountResets.inFlight == nil {
			m.accountResets.inFlight = make(map[string]bool)
		}
		m.accountResets.inFlight[id] = true
		m.accountResets.mu.Unlock()
		go func(providerID string, source accountResetSource) {
			defer func() {
				m.accountResets.mu.Lock()
				delete(m.accountResets.inFlight, providerID)
				m.accountResets.mu.Unlock()
			}()
			if err := m.readAccountResets(context.Background(), providerID, source); err != nil && m.opts.Logf != nil {
				m.opts.Logf("account resets refresh failed", map[string]any{
					"providerId": providerID,
					"error":      redactSensitiveText(err.Error()),
				})
			}
		}(id, source)
	}
}
