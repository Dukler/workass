package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Nothing here reads a real login or reaches a real account API: every
// source is a fake or points at a loopback httptest server.

type fakeAccountResetSource struct {
	mu       sync.Mutex
	status   accountResetStatus
	statusOK error
	// errs answer the next reads in order before status does.
	errs []error
	// hold, when set, keeps every read waiting until it is closed.
	hold     chan struct{}
	claim    accountResetClaim
	claimErr error
	reads    int
	claims   []string
}

func (f *fakeAccountResetSource) ConsentNote() string { return "nota" }

func (f *fakeAccountResetSource) Status(context.Context) (accountResetStatus, error) {
	f.mu.Lock()
	f.reads++
	hold := f.hold
	var err error
	if len(f.errs) > 0 {
		err, f.errs = f.errs[0], f.errs[1:]
	}
	f.mu.Unlock()
	if hold != nil {
		<-hold
	}
	if err != nil {
		return accountResetStatus{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, f.statusOK
}

func (f *fakeAccountResetSource) Claim(_ context.Context, creditID, requestID string) (accountResetClaim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims = append(f.claims, creditID+"|"+requestID)
	return f.claim, f.claimErr
}

func (f *fakeAccountResetSource) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads, len(f.claims)
}

func oneUsableGrant() *RateLimitResetCreditsSummary {
	left, total, usable := int64(1), int64(1), true
	return &RateLimitResetCreditsSummary{AvailableCount: 1, Credits: []RateLimitResetCredit{{
		ID: "grant_full", ResetType: "full", Status: "available", Remaining: &left, Total: &total, UsableNow: &usable,
		Refills: []string{"five_hour", "seven_day"},
	}}}
}

func newAccountResetTestManager(t *testing.T, stateDir string) (*Manager, *fakeAccountResetSource) {
	t.Helper()
	manager := NewManager(Options{StateDir: stateDir, Providers: []ProviderConfig{{ID: "acct", Name: "Acct", Command: "acct", Enabled: true}}})
	t.Cleanup(func() { manager.Reset() })
	source := &fakeAccountResetSource{status: accountResetStatus{Credits: oneUsableGrant()}}
	manager.setAccountResetSourceForTest("acct", source)
	manager.SetAccountResetConsent(func(id string) bool { return id == "acct" })
	return manager, source
}

func rateLimited(retryAfter time.Duration) error {
	return &accountResetError{code: accountResetRateLimited, message: "limit", retryAfter: retryAfter}
}

func TestAccountResetsNeverReadWithoutConsent(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	source := &fakeAccountResetSource{status: accountResetStatus{Credits: oneUsableGrant()}}
	manager.setAccountResetSourceForTest("acct", source)

	if _, err := manager.RefreshAccountResets(context.Background(), "acct", false); err == nil {
		t.Fatal("a refresh without any consent hook must be refused")
	}
	manager.SetAccountResetConsent(func(string) bool { return false })
	_, err := manager.RefreshAccountResets(context.Background(), "acct", true)
	var refusal *accountResetError
	if !errors.As(err, &refusal) || refusal.code != accountResetNeedsConsent {
		t.Fatalf("refresh without consent = %v, want the consent refusal", err)
	}
	if _, err := manager.ConsumeRateLimitResetCredit(context.Background(), "acct", "", "request-1", "grant_full"); err == nil {
		t.Fatal("a claim without consent must be refused")
	}
	manager.refreshConsentedAccountResets()
	time.Sleep(20 * time.Millisecond)
	if reads, claims := source.counts(); reads != 0 || claims != 0 {
		t.Fatalf("source touched without consent: %d reads, %d claims", reads, claims)
	}
}

func TestAccountResetsRecordOnThePlanUsageSnapshotAndForgetOnWithdrawal(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	used, limit := 0.0, 20.0
	source := &fakeAccountResetSource{status: accountResetStatus{
		Credits:       oneUsableGrant(),
		ExtraUsage:    &PlanUsageExtraUsage{Enabled: true, MonthlyLimit: &limit, Used: &used, Currency: "USD"},
		ExtraUsageSet: true,
	}}
	manager.setAccountResetSourceForTest("acct", source)
	var consented atomic.Bool
	consented.Store(true)
	manager.SetAccountResetConsent(func(id string) bool { return id == "acct" && consented.Load() })

	reply, err := manager.RefreshAccountResets(context.Background(), "acct", false)
	if err != nil || reply["ok"] != true || reply["readAt"] == nil || reply["retryAt"] != nil {
		t.Fatalf("refresh = %#v, %v", reply, err)
	}
	snapshot := manager.planUsageByProvider["acct"]
	if snapshot.RateLimitResetCredits == nil || snapshot.RateLimitResetCredits.Source != accountResetSourceAccount ||
		len(snapshot.RateLimitResetCredits.Credits) != 1 || snapshot.RateLimitResetCredits.ReadAt != reply["readAt"] {
		t.Fatalf("snapshot resets = %#v", snapshot.RateLimitResetCredits)
	}
	if snapshot.ExtraUsage == nil || *snapshot.ExtraUsage.MonthlyLimit != 20 {
		t.Fatalf("snapshot extra usage = %#v", snapshot.ExtraUsage)
	}

	consented.Store(false)
	if _, err := manager.RefreshAccountResets(context.Background(), "acct", false); err == nil {
		t.Fatal("refresh after withdrawal must be refused")
	}
	if got := manager.planUsageByProvider["acct"].RateLimitResetCredits; got != nil {
		t.Fatalf("withdrawn consent left resets behind: %#v", got)
	}
	if reads, _ := source.counts(); reads != 1 {
		t.Fatalf("source reads = %d, want 1 (none after withdrawal)", reads)
	}
}

func TestAccountResetsServeAFreshCacheWithoutAsking(t *testing.T) {
	t.Parallel()
	manager, source := newAccountResetTestManager(t, "")
	first, _ := manager.RefreshAccountResets(context.Background(), "acct", false)
	for i := 0; i < 5; i++ {
		again, err := manager.RefreshAccountResets(context.Background(), "acct", false)
		if err != nil || again["readAt"] != first["readAt"] {
			t.Fatalf("cached answer %d = %#v, %v", i, again, err)
		}
	}
	manager.refreshConsentedAccountResets()
	time.Sleep(20 * time.Millisecond)
	if reads, _ := source.counts(); reads != 1 {
		t.Fatalf("reads = %d, want 1 while the cache is fresh", reads)
	}
	// The owner's retry reads again; a stale cache is read again too.
	if _, err := manager.RefreshAccountResets(context.Background(), "acct", true); err != nil {
		t.Fatal(err)
	}
	if reads, _ := source.counts(); reads != 1 {
		t.Fatalf("a fresh cache is the answer even to a retry: reads = %d", reads)
	}
	ageAccountResetCache(manager, "acct", accountResetsFreshFor+time.Minute)
	if _, err := manager.RefreshAccountResets(context.Background(), "acct", false); err != nil {
		t.Fatal(err)
	}
	if reads, _ := source.counts(); reads != 2 {
		t.Fatalf("reads = %d, want 2 once the cache went stale", reads)
	}
}

func ageAccountResetCache(manager *Manager, providerID string, by time.Duration) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	snapshot := manager.planUsageByProvider[providerID]
	credits := *snapshot.RateLimitResetCredits
	readAt, _ := time.Parse(time.RFC3339Nano, credits.ReadAt)
	credits.ReadAt = readAt.Add(-by).UTC().Format(time.RFC3339Nano)
	snapshot.RateLimitResetCredits = &credits
	manager.planUsageByProvider[providerID] = snapshot
}

func TestAccountResetsAreFreshForLessWhileAWindowIsAtItsLimit(t *testing.T) {
	t.Parallel()
	manager, _ := newAccountResetTestManager(t, "")
	now := time.Now()
	if got := manager.accountResetsFreshFor("acct", now); got != accountResetsFreshFor {
		t.Fatalf("fresh for = %s, want %s", got, accountResetsFreshFor)
	}
	full, half := 100.0, 50.0
	manager.recordNormalizedPlanUsageCapture("", "acct", planUsageCapture{entries: []PlanUsageEntry{
		{Kind: "rate-limit", ID: "five_hour", UsedPercent: &full, ResetsAt: now.Add(time.Hour).UTC().Format(time.RFC3339)},
		{Kind: "rate-limit", ID: "seven_day", UsedPercent: &half},
	}})
	if got := manager.accountResetsFreshFor("acct", now); got != accountResetsFreshAtLimit {
		t.Fatalf("fresh for at a limit = %s, want %s", got, accountResetsFreshAtLimit)
	}
	manager.recordNormalizedPlanUsageCapture("", "acct", planUsageCapture{entries: []PlanUsageEntry{
		{Kind: "rate-limit", ID: "five_hour", UsedPercent: &full, ResetsAt: now.Add(-time.Minute).UTC().Format(time.RFC3339)},
	}})
	if got := manager.accountResetsFreshFor("acct", now); got != accountResetsFreshFor {
		t.Fatalf("a passed limit is not a limit: fresh for = %s", got)
	}
}

func TestAccountResetsCollapseConcurrentAsksIntoOneRead(t *testing.T) {
	t.Parallel()
	manager, source := newAccountResetTestManager(t, "")
	source.hold = make(chan struct{})
	var wg sync.WaitGroup
	replies := make([]map[string]any, 6)
	for i := range replies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			replies[i], _ = manager.RefreshAccountResets(context.Background(), "acct", i%2 == 0)
		}(i)
	}
	deadline := time.Now().Add(2 * time.Second)
	for reads, _ := source.counts(); reads == 0; reads, _ = source.counts() {
		if time.Now().After(deadline) {
			t.Fatal("no read started")
		}
		time.Sleep(2 * time.Millisecond)
	}
	manager.refreshConsentedAccountResets()
	time.Sleep(30 * time.Millisecond)
	close(source.hold)
	wg.Wait()
	if reads, _ := source.counts(); reads != 1 {
		t.Fatalf("concurrent asks made %d reads, want 1", reads)
	}
	for i, reply := range replies {
		if reply["readAt"] == nil || reply["readAt"] != replies[0]["readAt"] {
			t.Fatalf("reply %d = %#v, want the shared read", i, reply)
		}
	}
}

func TestAccountResetsBackOffOnA429AndKeepServingTheCache(t *testing.T) {
	t.Parallel()
	manager, source := newAccountResetTestManager(t, "")
	cached, _ := manager.RefreshAccountResets(context.Background(), "acct", false)
	ageAccountResetCache(manager, "acct", accountResetsFreshFor+time.Minute)
	staleReadAt := manager.planUsageByProvider["acct"].RateLimitResetCredits.ReadAt
	source.errs = []error{rateLimited(90 * time.Second)}

	started := time.Now()
	reply, err := manager.RefreshAccountResets(context.Background(), "acct", false)
	if err != nil || reply["reason"] != accountResetRateLimited || reply["readAt"] != staleReadAt || reply["message"] != nil {
		t.Fatalf("429 reply = %#v, %v (first read %#v)", reply, err, cached)
	}
	retryAt, _ := time.Parse(time.RFC3339, reply["retryAt"].(string))
	if wait := retryAt.Sub(started); wait < 85*time.Second || wait > 95*time.Second {
		t.Fatalf("Retry-After 90s waits %s", wait)
	}
	if credits := manager.planUsageByProvider["acct"].RateLimitResetCredits; credits == nil || len(credits.Credits) != 1 {
		t.Fatalf("a 429 dropped the cached resets: %#v", credits)
	}
	// Nobody asks again before Retry-After: not the page, not its retry, not
	// the unattended read, not the provider's own plan-usage reads.
	for _, force := range []bool{false, true} {
		again, _ := manager.RefreshAccountResets(context.Background(), "acct", force)
		if again["retryAt"] != reply["retryAt"] {
			t.Fatalf("ask during backoff (force=%v) = %#v", force, again)
		}
	}
	manager.refreshConsentedAccountResets()
	time.Sleep(20 * time.Millisecond)
	if reads, _ := source.counts(); reads != 2 {
		t.Fatalf("reads = %d, want 2 (the first and the 429)", reads)
	}
	if wait := manager.planUsageReadWait("acct", time.Now()); wait < 80*time.Second {
		t.Fatalf("native plan-usage reads wait %s, want the 429's backoff", wait)
	}
}

func TestAccountResetBackoffSchedule(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	var previous accountResetBackoff
	var waits []time.Duration
	for i := 0; i < 6; i++ {
		previous = nextAccountResetBackoff(previous, rateLimited(0), now)
		waits = append(waits, previous.RetryAt.Sub(now))
	}
	want := []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("429 without Retry-After waits %v, want %v", waits, want)
		}
	}
	for retryAfter, want := range map[time.Duration]time.Duration{
		5 * time.Second: accountResetRetryAfterMin, 10 * time.Minute: 10 * time.Minute, 48 * time.Hour: accountResetRetryAfterMax,
	} {
		if got := nextAccountResetBackoff(accountResetBackoff{}, rateLimited(retryAfter), now).RetryAt.Sub(now); got != want {
			t.Fatalf("Retry-After %s waits %s, want %s", retryAfter, got, want)
		}
	}
	transient := nextAccountResetBackoff(accountResetBackoff{}, newAccountResetError(accountResetUnavailable, "down"), now)
	if transient.RetryAt.Sub(now) != time.Minute || transient.Message != "down" {
		t.Fatalf("transient backoff = %#v", transient)
	}
	action := nextAccountResetBackoff(transient, newAccountResetError(accountResetExpired, "Abrí Claude Code"), now)
	if action.RetryAt.Sub(now) != accountResetActionBackoff || action.Failures != 1 || action.Reason != accountResetExpired {
		t.Fatalf("owner-action backoff = %#v", action)
	}
}

func TestAnOwnersRetrySkipsABackoffButNeverA429(t *testing.T) {
	t.Parallel()
	manager, source := newAccountResetTestManager(t, "")
	source.errs = []error{newAccountResetError(accountResetExpired, "Abrí Claude Code para renovar la sesión.")}
	reply, err := manager.RefreshAccountResets(context.Background(), "acct", false)
	if err != nil || reply["reason"] != accountResetExpired || reply["message"] == nil || reply["readAt"] != nil {
		t.Fatalf("expired reply = %#v, %v", reply, err)
	}
	if again, _ := manager.RefreshAccountResets(context.Background(), "acct", false); again["reason"] != accountResetExpired {
		t.Fatalf("an unattended ask during the backoff read again: %#v", again)
	}
	retried, _ := manager.RefreshAccountResets(context.Background(), "acct", true)
	if retried["reason"] != nil || retried["readAt"] == nil {
		t.Fatalf("the owner's retry after renewing = %#v", retried)
	}
	if reads, _ := source.counts(); reads != 2 {
		t.Fatalf("reads = %d, want 2", reads)
	}
}

func TestAccountResetCacheAndBackoffSurviveARestart(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	manager, source := newAccountResetTestManager(t, stateDir)
	used := 42.0
	manager.recordNormalizedPlanUsageCapture("", "acct", planUsageCapture{
		entries: []PlanUsageEntry{{Kind: "rate-limit", ID: "five_hour", UsedPercent: &used}},
		account: &PlanUsageAccount{Email: "vos@ejemplo.com", Plan: "Max", Billing: "subscription"},
		raw:     map[string]any{"diagnostic": "kept out of the file"},
	})
	if _, err := manager.RefreshAccountResets(context.Background(), "acct", false); err != nil {
		t.Fatal(err)
	}
	ageAccountResetCache(manager, "acct", accountResetsFreshFor+time.Minute)
	source.errs = []error{rateLimited(10 * time.Minute)}
	if reply, _ := manager.RefreshAccountResets(context.Background(), "acct", false); reply["reason"] != accountResetRateLimited {
		t.Fatalf("429 reply = %#v", reply)
	}
	before := manager.planUsageByProvider["acct"]

	data, err := os.ReadFile(filepath.Join(stateDir, planUsageStoreName))
	if err != nil {
		t.Fatalf("plan usage store: %v", err)
	}
	if strings.Contains(string(data), "kept out of the file") || strings.Contains(string(data), "token") {
		t.Fatalf("store keeps raw or secret-shaped data: %s", data)
	}
	if info, err := os.Stat(filepath.Join(stateDir, planUsageStoreName)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("store mode = %v, %v", info.Mode().Perm(), err)
	}

	restarted := NewManager(Options{StateDir: stateDir, Providers: []ProviderConfig{{ID: "acct", Name: "Acct", Command: "acct", Enabled: true}}})
	t.Cleanup(func() { restarted.Reset() })
	after := restarted.planUsageByProvider["acct"]
	if after.CapturedAt != before.CapturedAt || after.Account == nil || after.Account.Email != "vos@ejemplo.com" ||
		len(after.Entries) != 1 || after.Raw != nil || after.RateLimitResetCredits == nil ||
		after.RateLimitResetCredits.ReadAt != before.RateLimitResetCredits.ReadAt {
		t.Fatalf("restored snapshot = %#v, want %#v without raw", after, before)
	}
	fresh := &fakeAccountResetSource{status: accountResetStatus{Credits: oneUsableGrant()}}
	restarted.setAccountResetSourceForTest("acct", fresh)
	restarted.SetAccountResetConsent(func(string) bool { return true })
	reply, _ := restarted.RefreshAccountResets(context.Background(), "acct", true)
	if reply["reason"] != accountResetRateLimited || reply["readAt"] != before.RateLimitResetCredits.ReadAt {
		t.Fatalf("after a restart the 429's wait still holds and the cache answers: %#v", reply)
	}
	if reads, _ := fresh.counts(); reads != 0 {
		t.Fatalf("a restart asked the account API during a 429 backoff: %d reads", reads)
	}
	if wait := restarted.planUsageReadWait("acct", time.Now()); wait < 9*time.Minute {
		t.Fatalf("restored native wait = %s", wait)
	}
}

func TestPlanUsageReadsOfOneProviderAreSpaced(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{PlanUsageMinInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	now := time.Now()
	if wait := manager.planUsageReadWait("acct", now); wait != 0 {
		t.Fatalf("a first read waits %s", wait)
	}
	manager.notePlanUsageRead("acct", now)
	if wait := manager.planUsageReadWait("acct", now.Add(time.Minute)); wait != 59*time.Minute {
		t.Fatalf("a read a minute later waits %s, want 59m", wait)
	}
	if wait := manager.planUsageReadWait("other", now); wait != 0 {
		t.Fatalf("spacing leaked to another provider: %s", wait)
	}
	// Without a recorded read the spacing is off; a 429 still holds.
	off := NewManager(Options{PlanUsageMinInterval: -1})
	t.Cleanup(func() { off.Reset() })
	off.notePlanUsageRead("acct", now)
	if wait := off.planUsageReadWait("acct", now); wait != 0 {
		t.Fatalf("disabled spacing waits %s", wait)
	}
	if (Options{}).withDefaults().PlanUsageMinInterval != 2*time.Minute {
		t.Fatal("the default spacing is two minutes")
	}
}

func TestAccountResetClaimGoesToTheAccountSourceAndRereads(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	left := int64(0)
	source := &fakeAccountResetSource{
		status: accountResetStatus{Credits: &RateLimitResetCreditsSummary{Credits: []RateLimitResetCredit{}}},
		claim:  accountResetClaim{Outcome: "reset", Refilled: []string{"five_hour"}, Remaining: &left},
	}
	manager.setAccountResetSourceForTest("acct", source)
	manager.SetAccountResetConsent(func(string) bool { return true })

	result, err := manager.ConsumeRateLimitResetCredit(context.Background(), "acct", "", "request-1", "grant_full")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if result["outcome"] != "reset" || result["remaining"] != int64(0) {
		t.Fatalf("claim result = %#v", result)
	}
	if refilled, _ := result["refilled"].([]string); len(refilled) != 1 || refilled[0] != "five_hour" {
		t.Fatalf("claim refilled = %#v", result["refilled"])
	}
	if _, ok := result["planUsage"].(PlanUsageSnapshot); !ok {
		t.Fatalf("claim must return the re-read snapshot: %#v", result["planUsage"])
	}
	if reads, claims := source.counts(); reads != 1 || claims != 1 || source.claims[0] != "grant_full|request-1" {
		t.Fatalf("source calls: %d reads, claims %#v", reads, source.claims)
	}
}

func TestAClaimDuringA429MarksTheResetSpentInTheCache(t *testing.T) {
	t.Parallel()
	manager, source := newAccountResetTestManager(t, "")
	if _, err := manager.RefreshAccountResets(context.Background(), "acct", false); err != nil {
		t.Fatal(err)
	}
	ageAccountResetCache(manager, "acct", accountResetsFreshFor+time.Minute)
	source.errs = []error{rateLimited(time.Hour)}
	_, _ = manager.RefreshAccountResets(context.Background(), "acct", false)
	left := int64(0)
	source.claim = accountResetClaim{Outcome: "reset", Remaining: &left}

	result, err := manager.ConsumeRateLimitResetCredit(context.Background(), "acct", "", "request-2", "grant_full")
	if err != nil || result["outcome"] != "reset" {
		t.Fatalf("claim = %#v, %v", result, err)
	}
	if reads, _ := source.counts(); reads != 2 {
		t.Fatalf("a claim during a 429 read again: %d reads", reads)
	}
	credits := manager.planUsageByProvider["acct"].RateLimitResetCredits
	if credits == nil || credits.Credits[0].Status != "used" || *credits.Credits[0].UsableNow || credits.AvailableCount != 0 {
		t.Fatalf("spent reset in the cache = %#v", credits)
	}
}

func TestProviderAccountListFieldsAreNeutralFlags(t *testing.T) {
	t.Parallel()
	for _, id := range registeredProviderIDs() {
		fields := providerAccountListFields(id, localModelServers(nil))
		adapter := providerAdapterForID(id)
		if (adapter.accountResets != nil) != (fields["accountResetConsent"] != nil) {
			t.Fatalf("%s accountResetConsent = %#v", id, fields["accountResetConsent"])
		}
		if providerIsLocal(id) && fields["localEndpoint"] == nil {
			t.Fatalf("%s is local but names no endpoint", id)
		}
		if endpoint, _ := fields["localEndpoint"].(string); endpoint != "" && strings.Contains(endpoint, "/") {
			t.Fatalf("%s endpoint %q must be host:port", id, endpoint)
		}
	}
	if fields := providerAccountListFields("no-such-provider", nil); len(fields) != 0 {
		t.Fatalf("unregistered provider fields = %#v", fields)
	}
}

// ---- the Claude account API source, against a loopback server ----------

func newTestAccountSource(t *testing.T, handler http.HandlerFunc, credential func(context.Context) (claudeOAuthCredential, error)) (*claudeAccountResetSource, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	return &claudeAccountResetSource{
		readCredential:   credential,
		readOrganization: func() (string, error) { return "00000000-0000-4000-8000-000000000000", nil },
		client:           server.Client(),
		baseURL:          server.URL,
		now:              func() time.Time { return now },
		goos:             "darwin",
	}, server
}

func validCredential(token string) func(context.Context) (claudeOAuthCredential, error) {
	return func(context.Context) (claudeOAuthCredential, error) {
		return claudeOAuthCredential{AccessToken: token, ExpiresAt: time.Date(2026, 11, 9, 20, 0, 0, 0, time.UTC), Scopes: []string{"user:inference", "user:profile"}}, nil
	}
}

const testAccountUsageBody = `{
  "five_hour": {"utilization": 100, "resets_at": "2026-11-09T23:10:00Z"},
  "seven_day": {"utilization": 57, "resets_at": "2026-11-11T15:00:00Z"},
  "extra_usage": {"is_enabled": true, "monthly_limit": 2000, "used_credits": 1240, "utilization": 62, "currency": "usd"},
  "cedar_ember": {
    "eligible": true, "at_limit": true, "cooldown_until": null,
    "grants": [
      {"id": "five_hour_weekly", "label": "5-hour reset", "resets_total": 1, "resets_left": 1, "clears": ["five_hour"],
       "paused": false, "usable_now": true, "use_requires_limit": true, "type": "session", "ends_at": "2026-11-15T09:00:00Z"},
      {"id": "full_kept", "label": "full reset", "resets_total": 2, "resets_left": 1, "clears": ["five_hour", "seven_day", "bogus"],
       "paused": false, "usable_now": false, "use_requires_limit": true, "unavailable_reason": "blocked_seven_day", "type": "full"},
      {"id": "Bad ID!", "resets_left": 1},
      {"id": "spent", "label": "weekly reset", "resets_total": 1, "resets_left": 0, "clears": ["seven_day"], "type": "weekly"}
    ]
  }
}`

func TestClaudeAccountResetStatusMapsGrantsWindowsAndSpend(t *testing.T) {
	t.Parallel()
	var sawAuth, sawBeta, sawPath string
	source, _ := newTestAccountSource(t, func(w http.ResponseWriter, r *http.Request) {
		sawAuth, sawBeta, sawPath = r.Header.Get("Authorization"), r.Header.Get("anthropic-beta"), r.URL.RequestURI()
		_, _ = io.WriteString(w, testAccountUsageBody)
	}, validCredential("token-a"))

	status, err := source.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if sawAuth != "Bearer token-a" || sawBeta != claudeOAuthBetaHeader || sawPath != claudeResetStatusPath {
		t.Fatalf("request auth=%q beta=%q path=%q", sawAuth, sawBeta, sawPath)
	}
	credits := status.Credits
	if credits == nil || len(credits.Credits) != 3 || credits.AvailableCount != 2 {
		t.Fatalf("credits = %#v", credits)
	}
	usable := credits.Credits[0]
	if usable.ID != "five_hour_weekly" || usable.Status != "available" || !*usable.UsableNow || usable.UnavailableReason != "" ||
		strings.Join(usable.Refills, ",") != "five_hour" || usable.ExpiresAt != "2026-11-15T09:00:00Z" || usable.ResetType != "session" {
		t.Fatalf("usable grant = %#v", usable)
	}
	blocked := credits.Credits[1]
	if *blocked.UsableNow || blocked.UnavailableReason != "blocked:seven_day" || *blocked.Remaining != 1 || *blocked.Total != 2 ||
		strings.Join(blocked.Refills, ",") != "five_hour,seven_day" {
		t.Fatalf("blocked grant = %#v", blocked)
	}
	if spent := credits.Credits[2]; spent.Status != "used" || *spent.UsableNow {
		t.Fatalf("spent grant = %#v", spent)
	}
	if len(status.Entries) != 2 || status.Entries[0].ID != "five_hour" || *status.Entries[0].UsedPercent != 100 {
		t.Fatalf("entries = %#v", status.Entries)
	}
	extra := status.ExtraUsage
	if !status.ExtraUsageSet || extra == nil || !extra.Enabled || *extra.MonthlyLimit != 20 || *extra.Used != 12.4 || extra.Currency != "USD" {
		t.Fatalf("extra usage (cents to units) = %#v", extra)
	}
}

func TestClaudeAccountResetStatusIneligibleListsNothing(t *testing.T) {
	t.Parallel()
	credits := claudeResetCredits(map[string]any{"eligible": false, "grants": []any{map[string]any{"id": "x", "resets_left": json.Number("1")}}})
	if credits == nil || len(credits.Credits) != 0 || credits.AvailableCount != 0 {
		t.Fatalf("ineligible credits = %#v", credits)
	}
	if empty := claudeResetCredits(nil); empty == nil || empty.Credits == nil {
		t.Fatalf("absent block must still be an empty, known list: %#v", empty)
	}
}

func TestClaudeAccountResetNeverRefreshesAnExpiredToken(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	source, _ := newTestAccountSource(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}, func(context.Context) (claudeOAuthCredential, error) {
		return claudeOAuthCredential{AccessToken: "old", ExpiresAt: time.Date(2026, 11, 9, 11, 0, 0, 0, time.UTC)}, nil
	})
	_, err := source.Status(context.Background())
	var refusal *accountResetError
	if !errors.As(err, &refusal) || refusal.code != accountResetExpired || !strings.Contains(err.Error(), "Abrí Claude Code para renovar la sesión") {
		t.Fatalf("expired token = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("an expired token must never be sent")
	}
}

func TestClaudeAccountResetRereadsTheStoredLoginOnceAfter401(t *testing.T) {
	t.Parallel()
	var reads atomic.Int32
	tokens := []string{"stale", "renewed-by-claude-code"}
	source, _ := newTestAccountSource(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer renewed-by-claude-code" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"cedar_ember": {"eligible": true, "grants": []}}`)
	}, func(ctx context.Context) (claudeOAuthCredential, error) {
		index := int(reads.Add(1)) - 1
		if index >= len(tokens) {
			index = len(tokens) - 1
		}
		return validCredential(tokens[index])(ctx)
	})
	if _, err := source.Status(context.Background()); err != nil {
		t.Fatalf("status after a renewed login: %v", err)
	}
	if reads.Load() != 2 {
		t.Fatalf("stored login reads = %d, want 2", reads.Load())
	}
	// The same token twice is a real refusal, answered without a third read.
	source.readCredential = validCredential("stale")
	source.forgetCredential()
	_, err := source.Status(context.Background())
	var refusal *accountResetError
	if !errors.As(err, &refusal) || refusal.code != accountResetAuth {
		t.Fatalf("401 with an unchanged login = %v", err)
	}
}

func TestClaudeAccountResetHTTPErrorsAreOwnerWords(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	for code, want := range map[int]string{
		http.StatusTooManyRequests:     accountResetRateLimited,
		http.StatusForbidden:           accountResetAuth,
		http.StatusInternalServerError: accountResetUnavailable,
	} {
		err := claudeAccountHTTPError(claudeAccountReply{code: code, header: http.Header{}}, now)
		var refusal *accountResetError
		if !errors.As(err, &refusal) || refusal.code != want {
			t.Fatalf("HTTP %d = %v, want %s", code, err, want)
		}
	}
	if claudeAccountHTTPError(claudeAccountReply{code: http.StatusOK}, now) != nil {
		t.Fatal("200 is not an error")
	}
	for header, want := range map[string]time.Duration{
		"120":                           2 * time.Minute,
		"Mon, 09 Nov 2026 12:05:00 GMT": 5 * time.Minute,
		"":                              0,
		"soon":                          0,
		"-3":                            0,
	} {
		err := claudeAccountHTTPError(claudeAccountReply{code: http.StatusTooManyRequests, header: http.Header{"Retry-After": {header}}}, now)
		var refusal *accountResetError
		if !errors.As(err, &refusal) || refusal.retryAfter != want {
			t.Fatalf("Retry-After %q = %#v, want %s", header, err, want)
		}
	}
}

func TestClaudeAccountResetStatusCarriesRetryAfterOnA429(t *testing.T) {
	t.Parallel()
	source, _ := newTestAccountSource(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "300")
		w.WriteHeader(http.StatusTooManyRequests)
	}, validCredential("token-a"))
	_, err := source.Status(context.Background())
	var refusal *accountResetError
	if !errors.As(err, &refusal) || refusal.code != accountResetRateLimited || refusal.retryAfter != 5*time.Minute {
		t.Fatalf("429 status = %#v", err)
	}
}

func TestClaudeAccountResetClaimSendsTheProgramAndMapsResults(t *testing.T) {
	t.Parallel()
	var body map[string]any
	var path string
	answer := `{"result": "reset", "resets_left": 0, "cleared": ["five_hour"], "weekly_resets_at": null}`
	source, _ := newTestAccountSource(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("claim request %s %q", r.Method, r.Header.Get("Content-Type"))
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.WriteString(w, answer)
	}, validCredential("token-a"))

	claim, err := source.Claim(context.Background(), "five_hour_weekly", "6f1c2a8e-0000-4000-8000-000000000001")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if path != "/api/organizations/00000000-0000-4000-8000-000000000000/reset_rate_limits" {
		t.Fatalf("claim path = %q", path)
	}
	if body["program"] != claudeResetProgram || body["grant_id"] != "five_hour_weekly" || body["request_id"] != "6f1c2a8e-0000-4000-8000-000000000001" {
		t.Fatalf("claim body = %#v", body)
	}
	if claim.Outcome != "reset" || *claim.Remaining != 0 || strings.Join(claim.Refilled, ",") != "five_hour" {
		t.Fatalf("claim = %#v", claim)
	}

	if _, err := source.Claim(context.Background(), "Bad ID!", "request"); err == nil {
		t.Fatal("a malformed grant id must not be sent")
	}
	if _, err := source.Claim(context.Background(), "ok", "bad request id with spaces"); err == nil {
		t.Fatal("a malformed request id must not be sent")
	}

	for result, want := range map[string]string{"already_used": "alreadyRedeemed", "not_limited": "nothingToReset", "ineligible": "noCredit", "unavailable": "noCredit"} {
		claim, err := claudeAccountResetClaimFrom(map[string]any{"result": result, "reason": "expired"})
		if err != nil || claim.Outcome != want {
			t.Fatalf("%s = %#v, %v; want %s", result, claim, err, want)
		}
	}
	if _, err := claudeAccountResetClaimFrom(map[string]any{"result": "cooldown"}); err == nil {
		t.Fatal("cooldown is a wait, not an outcome")
	}
	if _, err := claudeAccountResetClaimFrom(map[string]any{"result": "something-new"}); err == nil {
		t.Fatal("an unknown result must not read as success")
	}
}

func TestClaudeAccountCredentialKeepsOnlyTheAccessToken(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"claudeAiOauth":{"accessToken":"at","refreshToken":"rt","expiresAt":1794231600000,"scopes":["user:profile"]}}`)
	credential, err := parseClaudeOAuthCredential(raw)
	if err != nil || credential.AccessToken != "at" || credential.ExpiresAt.UnixMilli() != 1794231600000 {
		t.Fatalf("credential = %#v, %v", credential, err)
	}
	hexed := []byte(strings.ToLower(strings.TrimSpace(hexString(raw))))
	fromHex, err := parseClaudeOAuthCredential(hexed)
	if err != nil || fromHex.AccessToken != "at" {
		t.Fatalf("hex credential = %#v, %v", fromHex, err)
	}
	if _, err := parseClaudeOAuthCredential([]byte("not json")); err == nil {
		t.Fatal("garbage must not parse")
	}
	missingScope := &claudeAccountResetSource{readCredential: func(context.Context) (claudeOAuthCredential, error) {
		return claudeOAuthCredential{AccessToken: "x", Scopes: []string{"user:inference"}}, nil
	}}
	if _, err := missingScope.credential(context.Background(), false); err == nil {
		t.Fatal("a login without the profile scope cannot read usage")
	}
}

func hexString(raw []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(raw)*2)
	for _, b := range raw {
		out = append(out, digits[b>>4], digits[b&15])
	}
	return string(out)
}

func TestPlanUsageAccountFromUsageReads(t *testing.T) {
	t.Parallel()
	claudeCapture := (claudePlanUsageStrategy{}).Normalize(map[string]any{"_meta": map[string]any{"workass.claude/usage": map[string]any{
		"subscription_type": "max",
		"rate_limits": map[string]any{
			"five_hour":   map[string]any{"utilization": 42.0, "resets_at": "2026-11-09T23:10:00Z"},
			"extra_usage": map[string]any{"is_enabled": false, "monthly_limit": nil, "used_credits": nil, "utilization": nil},
		},
		"workassAccount": map[string]any{"email": "vos@ejemplo.com", "organization": "Personal", "subscriptionType": "max"},
	}}})
	account := claudeCapture.account
	if account == nil || account.Email != "vos@ejemplo.com" || account.Organization != "Personal" || account.Plan != "Max" ||
		account.Billing != "subscription" || account.ManageURL != claudeUsageSettingsURL {
		t.Fatalf("subscription account = %#v", account)
	}
	if !claudeCapture.extraUsageSet || claudeCapture.extraUsage == nil || claudeCapture.extraUsage.Enabled || claudeCapture.extraUsage.MonthlyLimit != nil {
		t.Fatalf("disabled extra usage = %#v", claudeCapture.extraUsage)
	}
	keyed := claudePlanUsageAccount(map[string]any{"workassAccount": map[string]any{"apiKeySource": "ANTHROPIC_API_KEY"}})
	if keyed == nil || keyed.Billing != "api-key" || keyed.Plan != "" || keyed.KeySource != "ANTHROPIC_API_KEY" {
		t.Fatalf("api key account = %#v", keyed)
	}
	cloud := claudePlanUsageAccount(map[string]any{"workassAccount": map[string]any{"apiProvider": "bedrock"}})
	if cloud == nil || cloud.Billing != "cloud" || cloud.Backend != "Amazon Bedrock" {
		t.Fatalf("cloud account = %#v", cloud)
	}
	if claudePlanUsageAccount(map[string]any{}) != nil {
		t.Fatal("a read without account facts must not invent one")
	}

	codexCapture := (codexPlanUsageStrategy{}).Normalize(map[string]any{"_meta": map[string]any{"workass.codex/rateLimits": map[string]any{
		"rateLimits":     map[string]any{"planType": "plus", "primary": map[string]any{"usedPercent": 12.0, "windowDurationMins": 300.0}},
		"workassAccount": map[string]any{"account": map[string]any{"type": "chatgpt", "email": "vos@ejemplo.com", "planType": "pro", "workspaceName": nil}},
	}}})
	if got := codexCapture.account; got == nil || got.Email != "vos@ejemplo.com" || got.Plan != "ChatGPT Pro" || got.Billing != "subscription" {
		t.Fatalf("chatgpt account = %#v", got)
	}
	if got := codexPlanUsageAccount(map[string]any{"workassAccount": map[string]any{"account": map[string]any{"type": "apiKey"}}}); got == nil || got.Billing != "api-key" {
		t.Fatalf("codex api key account = %#v", got)
	}
}

func TestPlanUsageAccountMergeKeepsWhatALaterReadOmits(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	manager.recordNormalizedPlanUsageCapture("", "acct", planUsageCapture{account: &PlanUsageAccount{Email: "vos@ejemplo.com", Plan: "ChatGPT Pro", Billing: "subscription"}})
	merged, _ := manager.recordNormalizedPlanUsageCapture("", "acct", planUsageCapture{account: &PlanUsageAccount{Plan: "ChatGPT Plus", Billing: "subscription"}})
	if merged.Account.Email != "vos@ejemplo.com" || merged.Account.Plan != "ChatGPT Plus" {
		t.Fatalf("merged account = %#v", merged.Account)
	}
	switched, _ := manager.recordNormalizedPlanUsageCapture("", "acct", planUsageCapture{account: &PlanUsageAccount{Billing: "api-key"}})
	if switched.Account.Email != "" || switched.Account.Plan != "" {
		t.Fatalf("a different way of paying carried the old login: %#v", switched.Account)
	}
	kept, _ := manager.recordNormalizedPlanUsageCapture("", "acct", planUsageCapture{raw: map[string]any{"x": 1}})
	if kept.Account == nil || kept.Account.Billing != "api-key" {
		t.Fatalf("a read without account facts dropped the account: %#v", kept.Account)
	}
}
