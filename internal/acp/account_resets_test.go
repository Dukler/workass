package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
	claim    accountResetClaim
	claimErr error
	reads    int
	claims   []string
}

func (f *fakeAccountResetSource) ConsentNote() string { return "nota" }

func (f *fakeAccountResetSource) Status(context.Context) (accountResetStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
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

func TestAccountResetsNeverReadWithoutConsent(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	source := &fakeAccountResetSource{status: accountResetStatus{Credits: oneUsableGrant()}}
	manager.setAccountResetSourceForTest("acct", source)

	if _, err := manager.RefreshAccountResets(context.Background(), "acct"); err == nil {
		t.Fatal("a refresh without any consent hook must be refused")
	}
	manager.SetAccountResetConsent(func(string) bool { return false })
	_, err := manager.RefreshAccountResets(context.Background(), "acct")
	var refusal *accountResetError
	if !errors.As(err, &refusal) || refusal.code != accountResetNeedsConsent {
		t.Fatalf("refresh without consent = %v, want the consent refusal", err)
	}
	if _, err := manager.ConsumeRateLimitResetCredit(context.Background(), "acct", "", "request-1", "grant_full"); err == nil {
		t.Fatal("a claim without consent must be refused")
	}
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

	reply, err := manager.RefreshAccountResets(context.Background(), "acct")
	if err != nil || reply["ok"] != true {
		t.Fatalf("refresh = %#v, %v", reply, err)
	}
	snapshot := manager.planUsageByProvider["acct"]
	if snapshot.RateLimitResetCredits == nil || snapshot.RateLimitResetCredits.Source != accountResetSourceAccount || len(snapshot.RateLimitResetCredits.Credits) != 1 {
		t.Fatalf("snapshot resets = %#v", snapshot.RateLimitResetCredits)
	}
	if snapshot.ExtraUsage == nil || *snapshot.ExtraUsage.MonthlyLimit != 20 {
		t.Fatalf("snapshot extra usage = %#v", snapshot.ExtraUsage)
	}

	consented.Store(false)
	if _, err := manager.RefreshAccountResets(context.Background(), "acct"); err == nil {
		t.Fatal("refresh after withdrawal must be refused")
	}
	if got := manager.planUsageByProvider["acct"].RateLimitResetCredits; got != nil {
		t.Fatalf("withdrawn consent left resets behind: %#v", got)
	}
	if reads, _ := source.counts(); reads != 1 {
		t.Fatalf("source reads = %d, want 1 (none after withdrawal)", reads)
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

func TestAccountResetBackgroundRefreshIsSpacedAndConsentBound(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{Providers: []ProviderConfig{{ID: "acct", Name: "Acct", Command: "acct", Enabled: true}}})
	t.Cleanup(func() { manager.Reset() })
	source := &fakeAccountResetSource{status: accountResetStatus{Credits: oneUsableGrant()}}
	manager.setAccountResetSourceForTest("acct", source)

	manager.refreshConsentedAccountResets()
	time.Sleep(20 * time.Millisecond)
	if reads, _ := source.counts(); reads != 0 {
		t.Fatalf("background read without consent: %d", reads)
	}
	manager.SetAccountResetConsent(func(string) bool { return true })
	manager.refreshConsentedAccountResets()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if reads, _ := source.counts(); reads == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background read never ran")
		}
		time.Sleep(5 * time.Millisecond)
	}
	manager.refreshConsentedAccountResets()
	time.Sleep(20 * time.Millisecond)
	if reads, _ := source.counts(); reads != 1 {
		t.Fatalf("background reads = %d, want 1 inside %s", reads, accountResetBackgroundEvery)
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
	for code, want := range map[int]string{
		http.StatusTooManyRequests:     accountResetRateLimited,
		http.StatusForbidden:           accountResetAuth,
		http.StatusInternalServerError: accountResetUnavailable,
	} {
		err := claudeAccountHTTPError(code)
		var refusal *accountResetError
		if !errors.As(err, &refusal) || refusal.code != want {
			t.Fatalf("HTTP %d = %v, want %s", code, err, want)
		}
	}
	if claudeAccountHTTPError(http.StatusOK) != nil {
		t.Fatal("200 is not an error")
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
