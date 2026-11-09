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
//
// That API rate-limits per account, so the last good read is a cache:
//
//   - it lives on the provider's plan-usage snapshot (ReadAt dates it) and is
//     kept across restarts with it (plan_usage_store.go);
//   - a read younger than accountResetsFreshFor is served as is. Grants are
//     issued weekly or by promotion and expire over days; what changes within
//     hours is whether one can be used now, and that flips when a usage window
//     reaches its limit, which the bars show. So the cache is good for 30
//     minutes, and for 5 while a window of that provider is at its limit;
//   - concurrent asks share one read;
//   - a failed read backs off (accountResetBackoff), and the cache keeps being
//     served. A 429 waits out Retry-After, else 2, 4, 8... up to 30 minutes;
//     that backoff also holds the provider's native plan-usage reads, which
//     hit the same limit. An owner's retry skips other backoffs, never a 429's.

const (
	// accountResetSourceAccount marks reset summaries read here, so a
	// withdrawn consent drops exactly them.
	accountResetSourceAccount = "account"
	accountResetTimeout       = 30 * time.Second
	accountResetsFreshFor     = 30 * time.Minute
	accountResetsFreshAtLimit = 5 * time.Minute

	accountResetRetryAfterMin = 30 * time.Second
	accountResetRetryAfterMax = 6 * time.Hour
	accountResetRateLimitBase = 2 * time.Minute
	accountResetRateLimitMax  = 30 * time.Minute
	accountResetTransientBase = time.Minute
	accountResetTransientMax  = 15 * time.Minute
	accountResetActionBackoff = 10 * time.Minute
)

// cliVersionObserver is an account reset source that wants the installed CLI
// version, told whenever the manager detects it.
type cliVersionObserver interface {
	ObserveCLIVersion(version string)
}

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
// as is, so it is written for the owner, not for a log. retryAfter is the
// wait a rate-limited answer asked for, when it named one.
type accountResetError struct {
	code       string
	message    string
	retryAfter time.Duration
}

func (e *accountResetError) Error() string { return e.message }

const (
	accountResetNeedsConsent = "consent"
	accountResetNoLogin      = "no-login"
	accountResetExpired      = "expired"
	accountResetKeychain     = "keychain"
	accountResetAuth         = "auth"
	accountResetRateLimited  = "rate-limited"
	accountResetUnavailable  = "unavailable"
)

func newAccountResetError(code, message string) error {
	return &accountResetError{code: code, message: message}
}

// accountResetBackoff is a provider's wait after a failed read. It is
// persisted, so a restart does not ask again early.
type accountResetBackoff struct {
	RetryAt  time.Time `json:"retryAt"`
	Failures int       `json:"failures"`
	Reason   string    `json:"reason"`
	Message  string    `json:"message,omitempty"`
}

type accountResetFlight struct {
	done chan struct{}
}

type accountResetState struct {
	mu      sync.Mutex
	consent func(providerID string) bool
	sources map[string]accountResetSource
	backoff map[string]accountResetBackoff
	flights map[string]*accountResetFlight
}

// accountResetsAnswer is what accounts:resets-refresh replies: the cache's
// date, and while backing off when the next read happens and why.
type accountResetsAnswer struct {
	ReadAt  string
	RetryAt string
	Reason  string
	Message string
	// Read is true when this call read the account API itself.
	Read bool
}

func (a accountResetsAnswer) reply(providerID string) map[string]any {
	reply := map[string]any{"ok": true, "providerId": providerID}
	for key, value := range map[string]string{"readAt": a.ReadAt, "retryAt": a.RetryAt, "reason": a.Reason, "message": a.Message} {
		if value != "" {
			reply[key] = value
		}
	}
	return reply
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

// accountResetRateLimitedUntil is when a provider's 429 backoff ends (zero
// when there is none). Native plan-usage reads wait for it too.
func (m *Manager) accountResetRateLimitedUntil(providerID string) time.Time {
	m.accountResets.mu.Lock()
	defer m.accountResets.mu.Unlock()
	if backoff, ok := m.accountResets.backoff[normalizeProviderID(providerID)]; ok && backoff.Reason == accountResetRateLimited {
		return backoff.RetryAt
	}
	return time.Time{}
}

func (m *Manager) accountResetBackoffs(now time.Time) map[string]accountResetBackoff {
	m.accountResets.mu.Lock()
	defer m.accountResets.mu.Unlock()
	out := map[string]accountResetBackoff{}
	for id, backoff := range m.accountResets.backoff {
		if backoff.RetryAt.After(now) {
			out[id] = backoff
		}
	}
	return out
}

// RefreshAccountResets answers accounts:resets-refresh: one provider's resets
// from its account API, through the cache. It refuses before touching any
// login unless the owner consented, and the refusal also drops resets read
// under a consent since withdrawn. A failed read is not an error: the reply
// says when the next read happens and why, and the cache stays in place.
// force is the owner's retry: it skips a backoff, except a 429's.
func (m *Manager) RefreshAccountResets(ctx context.Context, providerID string, force bool) (map[string]any, error) {
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
	answer := m.accountResetsRead(ctx, providerID, source, force, false)
	return answer.reply(providerID), nil
}

func (m *Manager) cachedAccountResets(providerID string) *RateLimitResetCreditsSummary {
	m.mu.Lock()
	defer m.mu.Unlock()
	if credits := m.planUsageByProvider[providerID].RateLimitResetCredits; credits != nil && credits.Source == accountResetSourceAccount {
		copied := *credits
		copied.Credits = append([]RateLimitResetCredit(nil), credits.Credits...)
		return &copied
	}
	return nil
}

// accountResetsFreshFor is how long a read of this provider's resets is
// served as is: 30 minutes, 5 while one of its usage windows is at its limit.
func (m *Manager) accountResetsFreshFor(providerID string, now time.Time) time.Duration {
	m.mu.Lock()
	entries := append([]PlanUsageEntry(nil), m.planUsageByProvider[providerID].Entries...)
	m.mu.Unlock()
	for _, entry := range entries {
		if entry.Kind != "rate-limit" || entry.UsedPercent == nil || *entry.UsedPercent < 100 {
			continue
		}
		if resets, err := time.Parse(time.RFC3339, entry.ResetsAt); err != nil || resets.After(now) {
			return accountResetsFreshAtLimit
		}
	}
	return accountResetsFreshFor
}

// accountResetsRead serves the cache when it is fresh or while a backoff
// holds, and otherwise reads the account API once, however many ask.
// afterClaim reads even a fresh cache (a claim just changed it).
func (m *Manager) accountResetsRead(ctx context.Context, providerID string, source accountResetSource, force, afterClaim bool) accountResetsAnswer {
	for {
		now := time.Now()
		cached := m.cachedAccountResets(providerID)
		answer := accountResetsAnswer{}
		fresh := false
		if cached != nil {
			answer.ReadAt = cached.ReadAt
			if readAt, err := time.Parse(time.RFC3339Nano, cached.ReadAt); err == nil {
				fresh = now.Sub(readAt) < m.accountResetsFreshFor(providerID, now)
			}
		}
		m.accountResets.mu.Lock()
		backoff, backingOff := m.accountResets.backoff[providerID]
		backingOff = backingOff && backoff.RetryAt.After(now)
		if backingOff && (!force || backoff.Reason == accountResetRateLimited) {
			m.accountResets.mu.Unlock()
			answer.RetryAt = backoff.RetryAt.UTC().Format(time.RFC3339)
			answer.Reason, answer.Message = backoff.Reason, backoff.Message
			return answer
		}
		if flight := m.accountResets.flights[providerID]; flight != nil {
			m.accountResets.mu.Unlock()
			select {
			case <-flight.done:
			case <-ctx.Done():
				return answer
			}
			// The shared read is done: answer from what it left, without
			// starting another.
			force, afterClaim = false, false
			continue
		}
		if !afterClaim && fresh {
			m.accountResets.mu.Unlock()
			return answer
		}
		flight := &accountResetFlight{done: make(chan struct{})}
		if m.accountResets.flights == nil {
			m.accountResets.flights = make(map[string]*accountResetFlight)
		}
		m.accountResets.flights[providerID] = flight
		m.accountResets.mu.Unlock()

		answer = m.readAccountResetsNow(providerID, source, backoff)

		m.accountResets.mu.Lock()
		delete(m.accountResets.flights, providerID)
		m.accountResets.mu.Unlock()
		close(flight.done)
		return answer
	}
}

// readAccountResetsNow asks the account API. The read outlives any one
// caller (others may be waiting on it), bounded by accountResetTimeout.
func (m *Manager) readAccountResetsNow(providerID string, source accountResetSource, previous accountResetBackoff) accountResetsAnswer {
	ctx, cancel := context.WithTimeout(context.Background(), accountResetTimeout)
	defer cancel()
	started := time.Now()
	status, err := source.Status(ctx)
	if err != nil {
		backoff := nextAccountResetBackoff(previous, err, time.Now())
		m.accountResets.mu.Lock()
		if m.accountResets.backoff == nil {
			m.accountResets.backoff = make(map[string]accountResetBackoff)
		}
		m.accountResets.backoff[providerID] = backoff
		m.accountResets.mu.Unlock()
		m.persistPlanUsage()
		answer := accountResetsAnswer{RetryAt: backoff.RetryAt.UTC().Format(time.RFC3339), Reason: backoff.Reason, Message: backoff.Message}
		if cached := m.cachedAccountResets(providerID); cached != nil {
			answer.ReadAt = cached.ReadAt
		}
		return answer
	}
	// The same read reported the usage windows: it counts as the provider's
	// plan-usage read for the spacing.
	m.notePlanUsageRead(providerID, started)
	readAt := time.Now().UTC().Format(time.RFC3339Nano)
	credits := &RateLimitResetCreditsSummary{Credits: []RateLimitResetCredit{}}
	if status.Credits != nil {
		copied := *status.Credits
		copied.Credits = append([]RateLimitResetCredit{}, status.Credits.Credits...)
		credits = &copied
	}
	credits.Source = accountResetSourceAccount
	credits.ReadAt = readAt
	m.accountResets.mu.Lock()
	delete(m.accountResets.backoff, providerID)
	m.accountResets.mu.Unlock()
	m.recordNormalizedPlanUsageCapture("", providerID, planUsageCapture{
		entries:         status.Entries,
		resetCredits:    credits,
		resetCreditsSet: true,
		extraUsage:      status.ExtraUsage,
		extraUsageSet:   status.ExtraUsageSet,
	})
	return accountResetsAnswer{ReadAt: readAt, Read: true}
}

// nextAccountResetBackoff is the wait after a failed read: a 429 honours
// Retry-After (30 s to 6 h), else doubles from 2 to 30 minutes; a network or
// server failure doubles from 1 to 15 minutes; a failure the owner must fix
// (login, Keychain, session) waits 10 minutes before an unattended retry.
func nextAccountResetBackoff(previous accountResetBackoff, err error, now time.Time) accountResetBackoff {
	reason, message, retryAfter := accountResetUnavailable, err.Error(), time.Duration(0)
	var failure *accountResetError
	if errors.As(err, &failure) {
		reason, retryAfter = failure.code, failure.retryAfter
	}
	failures := 1
	if previous.Reason == reason {
		failures = previous.Failures + 1
	}
	doubling := func(base, ceiling time.Duration) time.Duration {
		wait := base
		for i := 1; i < failures && wait < ceiling; i++ {
			wait *= 2
		}
		return min(wait, ceiling)
	}
	var wait time.Duration
	switch reason {
	case accountResetRateLimited:
		if retryAfter > 0 {
			wait = max(accountResetRetryAfterMin, min(retryAfter, accountResetRetryAfterMax))
		} else {
			wait = doubling(accountResetRateLimitBase, accountResetRateLimitMax)
		}
		message = ""
	case accountResetUnavailable:
		wait = doubling(accountResetTransientBase, accountResetTransientMax)
	default:
		wait = accountResetActionBackoff
	}
	return accountResetBackoff{RetryAt: now.Add(wait), Failures: failures, Reason: reason, Message: message}
}

// forgetAccountResets drops resets this file recorded, leaving resets a
// provider session reported alone.
func (m *Manager) forgetAccountResets(providerID string) {
	m.accountResets.mu.Lock()
	delete(m.accountResets.backoff, providerID)
	m.accountResets.mu.Unlock()
	if m.cachedAccountResets(providerID) == nil {
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
	// its usage: read them again, unless a 429 backoff holds, and then mark the
	// spent reset in the cache from the claim's own answer.
	if claim.Outcome == "reset" || claim.Outcome == "alreadyRedeemed" {
		if answer := m.accountResetsRead(ctx, providerID, source, true, true); !answer.Read {
			m.markAccountResetSpent(providerID, creditID, claim.Remaining)
		}
	}
	m.mu.Lock()
	result["planUsage"] = m.planUsageByProvider[providerID]
	m.mu.Unlock()
	return result, nil
}

func (m *Manager) markAccountResetSpent(providerID, creditID string, remaining *int64) {
	cached := m.cachedAccountResets(providerID)
	if cached == nil {
		return
	}
	for i, credit := range cached.Credits {
		if credit.ID != creditID {
			continue
		}
		left := int64(0)
		if remaining != nil {
			left = *remaining
		} else if credit.Remaining != nil && *credit.Remaining > 0 {
			left = *credit.Remaining - 1
		}
		usable := false
		credit.Remaining, credit.UsableNow = &left, &usable
		if left == 0 {
			credit.Status = "used"
		}
		cached.Credits[i] = credit
	}
	var available int64
	for _, credit := range cached.Credits {
		if credit.Status == "available" && credit.Remaining != nil {
			available += *credit.Remaining
		}
	}
	cached.AvailableCount = available
	m.recordNormalizedPlanUsageCapture("", providerID, planUsageCapture{resetCredits: cached, resetCreditsSet: true})
}

// refreshConsentedAccountResets keeps consented providers' resets current
// with the Cuentas page closed. It goes through the same cache, spacing and
// backoff as the page, so it reads only what has gone stale.
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
	for _, id := range ids {
		source := m.accountResetSourceFor(id)
		if source == nil || !m.accountResetConsented(id) {
			continue
		}
		go func(providerID string, source accountResetSource) {
			answer := m.accountResetsRead(context.Background(), providerID, source, false, false)
			if answer.Read || answer.Reason == "" || m.opts.Logf == nil {
				return
			}
			m.opts.Logf("account resets refresh waits", map[string]any{
				"providerId": providerID, "reason": answer.Reason, "retryAt": answer.RetryAt,
			})
		}(id, source)
	}
}
