package acp

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Claude's earned resets ("cedar_ember" in Claude Code 2.1.x) live on the
// claude.ai OAuth API. Claude Code reads them with
// GET /api/oauth/usage?cedar_ember=1&skip_spend=1 and spends one with
// POST /api/organizations/{organizationUuid}/reset_rate_limits. Workass makes
// the same two calls with the OAuth access token Claude Code stored (macOS
// Keychain item "Claude Code-credentials", else <config>/.credentials.json),
// only after the owner's consent (account_resets.go). It never refreshes the
// token: a refresh rotates it and could sign Claude Code out, so an expired
// token is answered with "open Claude Code".

const (
	claudeOAuthAPIBase            = "https://api.anthropic.com"
	claudeOAuthBetaHeader         = "oauth-2025-04-20"
	claudeResetProgram            = "cedar_ember"
	claudeResetStatusPath         = "/api/oauth/usage?cedar_ember=1&skip_spend=1"
	claudeCredentialService       = "Claude Code-credentials"
	claudeUsageSettingsURL        = "https://claude.ai/settings/usage"
	claudeAccountResponseLimit    = 1 << 20
	claudeKeychainReadTimeout     = 60 * time.Second
	claudeCredentialExpirySlack   = 30 * time.Second
	claudeKeychainItemMissingCode = 44
	claudeKeychainLockedCode      = 36
)

var (
	// The same shapes Claude Code checks before it sends a claim.
	claudeResetGrantIDPattern   = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
	claudeResetRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	claudeOrganizationPattern   = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)
	// The windows a grant may refill or be blocked by; they double as the
	// plan-usage entry ids the native usage read reports.
	claudeResetWindowIDs = []string{
		"five_hour", "seven_day", "seven_day_overage_included", "seven_day_opus",
		"seven_day_sonnet", "seven_day_cowork", "seven_day_omelette", "seven_day_oauth_apps",
	}
	errClaudeKeychainItemMissing = errors.New("keychain item missing")
)

type claudeOAuthCredential struct {
	AccessToken string
	ExpiresAt   time.Time
	Scopes      []string
}

type claudeAccountResetSource struct {
	// Every hook is nil in production; tests replace them so no real login
	// or account API is ever touched.
	readCredential   func(context.Context) (claudeOAuthCredential, error)
	readOrganization func() (string, error)
	client           *http.Client
	baseURL          string
	now              func() time.Time
	goos             string

	mu     sync.Mutex
	cached *claudeOAuthCredential
}

func newClaudeAccountResetSource() *claudeAccountResetSource {
	return &claudeAccountResetSource{}
}

func (s *claudeAccountResetSource) platform() string {
	if s.goos != "" {
		return s.goos
	}
	return runtime.GOOS
}

func (s *claudeAccountResetSource) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *claudeAccountResetSource) ConsentNote() string {
	if s.platform() == "darwin" {
		return "macOS puede pedirte permiso una vez para leer el llavero."
	}
	return "Workass lee el archivo de credenciales de Claude Code."
}

func (s *claudeAccountResetSource) Status(ctx context.Context) (accountResetStatus, error) {
	code, body, err := s.call(ctx, http.MethodGet, claudeResetStatusPath, nil)
	if err != nil {
		return accountResetStatus{}, err
	}
	if err := claudeAccountHTTPError(code); err != nil {
		return accountResetStatus{}, err
	}
	payload, err := decodeClaudeAccountBody(body)
	if err != nil {
		return accountResetStatus{}, err
	}
	return claudeAccountResetStatusFrom(payload), nil
}

func (s *claudeAccountResetSource) Claim(ctx context.Context, creditID, requestID string) (accountResetClaim, error) {
	creditID = strings.TrimSpace(creditID)
	requestID = strings.TrimSpace(requestID)
	if !claudeResetGrantIDPattern.MatchString(creditID) {
		return accountResetClaim{}, errors.New("ese reset no tiene un identificador válido")
	}
	if !claudeResetRequestIDPattern.MatchString(requestID) {
		return accountResetClaim{}, errors.New("el pedido no tiene un identificador válido")
	}
	readOrganization := s.readOrganization
	if readOrganization == nil {
		readOrganization = readClaudeOrganizationUUID
	}
	organization, err := readOrganization()
	if err != nil {
		return accountResetClaim{}, err
	}
	payload, err := json.Marshal(map[string]any{
		"program": claudeResetProgram, "grant_id": creditID, "request_id": requestID,
	})
	if err != nil {
		return accountResetClaim{}, err
	}
	path := "/api/organizations/" + url.PathEscape(organization) + "/reset_rate_limits"
	code, body, err := s.call(ctx, http.MethodPost, path, payload)
	if err != nil {
		return accountResetClaim{}, err
	}
	if err := claudeAccountHTTPError(code); err != nil {
		return accountResetClaim{}, err
	}
	answer, err := decodeClaudeAccountBody(body)
	if err != nil {
		return accountResetClaim{}, err
	}
	return claudeAccountResetClaimFrom(answer)
}

// call sends one request with the stored token. A 401 re-reads the stored
// login once, in case Claude Code renewed it since; it never renews it.
func (s *claudeAccountResetSource) call(ctx context.Context, method, path string, payload []byte) (int, []byte, error) {
	base := strings.TrimRight(firstNonEmpty(s.baseURL, claudeOAuthAPIBase), "/")
	client := s.client
	if client == nil {
		client = &http.Client{Timeout: 25 * time.Second}
	}
	credential, err := s.credential(ctx, false)
	if err != nil {
		return 0, nil, err
	}
	for attempt := 0; ; attempt++ {
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}
		request, err := http.NewRequestWithContext(ctx, method, base+path, body)
		if err != nil {
			return 0, nil, err
		}
		request.Header.Set("Authorization", "Bearer "+credential.AccessToken)
		request.Header.Set("anthropic-beta", claudeOAuthBetaHeader)
		request.Header.Set("Accept", "application/json")
		if payload != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		response, err := client.Do(request)
		if err != nil {
			if ctx.Err() != nil {
				return 0, nil, newAccountResetError(accountResetUnavailable, "Claude tardó demasiado en responder. Probá de nuevo.")
			}
			return 0, nil, newAccountResetError(accountResetUnavailable, "No se pudo conectar con Claude. Revisá tu conexión.")
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, claudeAccountResponseLimit))
		response.Body.Close()
		if readErr != nil {
			return 0, nil, newAccountResetError(accountResetUnavailable, "La respuesta de Claude se cortó. Probá de nuevo.")
		}
		if response.StatusCode != http.StatusUnauthorized || attempt > 0 {
			return response.StatusCode, data, nil
		}
		s.forgetCredential()
		renewed, err := s.credential(ctx, true)
		if err != nil {
			return 0, nil, err
		}
		if renewed.AccessToken == credential.AccessToken {
			return response.StatusCode, data, nil
		}
		credential = renewed
	}
}

func (s *claudeAccountResetSource) forgetCredential() {
	s.mu.Lock()
	s.cached = nil
	s.mu.Unlock()
}

func (s *claudeAccountResetSource) expired(credential claudeOAuthCredential) bool {
	return !credential.ExpiresAt.IsZero() && !s.clock().Add(claudeCredentialExpirySlack).Before(credential.ExpiresAt)
}

// credential returns the stored token, read once and kept in memory until it
// expires, so the Keychain is not asked on every read.
func (s *claudeAccountResetSource) credential(ctx context.Context, fresh bool) (claudeOAuthCredential, error) {
	s.mu.Lock()
	if !fresh && s.cached != nil && !s.expired(*s.cached) {
		credential := *s.cached
		s.mu.Unlock()
		return credential, nil
	}
	s.mu.Unlock()
	read := s.readCredential
	if read == nil {
		read = s.readStoredCredential
	}
	credential, err := read(ctx)
	if err != nil {
		return claudeOAuthCredential{}, err
	}
	if strings.TrimSpace(credential.AccessToken) == "" {
		return claudeOAuthCredential{}, newAccountResetError(accountResetNoLogin, "No encontré tu login de Claude Code. Abrí Claude Code e iniciá sesión.")
	}
	if s.expired(credential) {
		s.forgetCredential()
		return claudeOAuthCredential{}, newAccountResetError(accountResetExpired, "Tu sesión de Claude Code venció. Abrí Claude Code para renovar la sesión.")
	}
	if len(credential.Scopes) > 0 && !stringInList(credential.Scopes, "user:profile") {
		return claudeOAuthCredential{}, newAccountResetError(accountResetAuth, "Tu login de Claude Code no deja leer el uso. Volvé a iniciar sesión en Claude Code.")
	}
	s.mu.Lock()
	s.cached = &credential
	s.mu.Unlock()
	return credential, nil
}

func (s *claudeAccountResetSource) readStoredCredential(ctx context.Context) (claudeOAuthCredential, error) {
	if s.platform() == "darwin" {
		raw, err := readClaudeKeychainCredential(ctx)
		if err == nil {
			return parseClaudeOAuthCredential(raw)
		}
		if !errors.Is(err, errClaudeKeychainItemMissing) {
			return claudeOAuthCredential{}, err
		}
	}
	raw, err := os.ReadFile(filepath.Join(claudeConfigDir(), ".credentials.json"))
	if err != nil {
		return claudeOAuthCredential{}, newAccountResetError(accountResetNoLogin, "No encontré tu login de Claude Code. Abrí Claude Code e iniciá sesión.")
	}
	return parseClaudeOAuthCredential(raw)
}

// readClaudeKeychainCredential asks the security tool for the item exactly
// as Claude Code does (account = the login user), so macOS shows at most its
// own one-time permission prompt.
func readClaudeKeychainCredential(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, claudeKeychainReadTimeout)
	defer cancel()
	args := []string{"find-generic-password"}
	if name := claudeLoginUserName(); name != "" {
		args = append(args, "-a", name)
	}
	args = append(args, "-w", "-s", claudeCredentialService)
	command := managedCommandContext(ctx, "/usr/bin/security", args...)
	var stdout bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			switch exit.ExitCode() {
			case claudeKeychainItemMissingCode:
				return nil, errClaudeKeychainItemMissing
			case claudeKeychainLockedCode:
				return nil, newAccountResetError(accountResetAuth, "El llavero de macOS está bloqueado. Desbloquealo y probá de nuevo.")
			}
		}
		return nil, newAccountResetError(accountResetAuth, "macOS no dejó leer tu login de Claude Code.")
	}
	return bytes.TrimSpace(stdout.Bytes()), nil
}

func claudeLoginUserName() string {
	if current, err := user.Current(); err == nil && strings.TrimSpace(current.Username) != "" {
		return strings.TrimSpace(current.Username)
	}
	return strings.TrimSpace(os.Getenv("USER"))
}

func claudeConfigDir() string {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// parseClaudeOAuthCredential keeps only the access token, its expiry and its
// scopes. The refresh token is never kept.
func parseClaudeOAuthCredential(raw []byte) (claudeOAuthCredential, error) {
	raw = bytes.TrimSpace(raw)
	// Claude Code writes the item as hex (security -X); the tool prints it
	// back as text when it is printable and as hex when it is not.
	if len(raw) > 0 && raw[0] != '{' {
		if decoded, err := hex.DecodeString(string(raw)); err == nil {
			raw = bytes.TrimSpace(decoded)
		}
	}
	var stored struct {
		ClaudeAiOauth struct {
			AccessToken string   `json:"accessToken"`
			ExpiresAt   *float64 `json:"expiresAt"`
			Scopes      []string `json:"scopes"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return claudeOAuthCredential{}, newAccountResetError(accountResetNoLogin, "No pude leer tu login de Claude Code. Abrí Claude Code e iniciá sesión.")
	}
	credential := claudeOAuthCredential{
		AccessToken: strings.TrimSpace(stored.ClaudeAiOauth.AccessToken),
		Scopes:      stored.ClaudeAiOauth.Scopes,
	}
	if stored.ClaudeAiOauth.ExpiresAt != nil && *stored.ClaudeAiOauth.ExpiresAt > 0 {
		credential.ExpiresAt = time.UnixMilli(int64(*stored.ClaudeAiOauth.ExpiresAt))
	}
	return credential, nil
}

// readClaudeOrganizationUUID reads the organization Claude Code signed in to,
// which names the claim's URL.
func readClaudeOrganizationUUID() (string, error) {
	home, _ := os.UserHomeDir()
	candidates := []string{filepath.Join(home, ".claude.json")}
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		candidates = append([]string{filepath.Join(dir, ".claude.json")}, candidates...)
	}
	candidates = append(candidates, filepath.Join(claudeConfigDir(), ".config.json"))
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var config struct {
			OAuthAccount struct {
				OrganizationUUID string `json:"organizationUuid"`
			} `json:"oauthAccount"`
		}
		if json.Unmarshal(data, &config) != nil {
			continue
		}
		if organization := strings.TrimSpace(config.OAuthAccount.OrganizationUUID); claudeOrganizationPattern.MatchString(organization) {
			return organization, nil
		}
	}
	return "", newAccountResetError(accountResetNoLogin, "No encontré tu organización de Claude. Abrí Claude Code e iniciá sesión.")
}

func claudeAccountHTTPError(code int) error {
	switch {
	case code >= 200 && code < 300:
		return nil
	case code == http.StatusTooManyRequests:
		return newAccountResetError(accountResetRateLimited, "Claude está limitando los pedidos. Probá de nuevo en un minuto.")
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return newAccountResetError(accountResetAuth, "Claude no aceptó tu sesión. Abrí Claude Code para renovar la sesión.")
	default:
		return newAccountResetError(accountResetUnavailable, fmt.Sprintf("Claude respondió con un error (HTTP %d). Probá de nuevo más tarde.", code))
	}
}

func decodeClaudeAccountBody(body []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil || payload == nil {
		return nil, newAccountResetError(accountResetUnavailable, "Claude respondió algo que Workass no entiende.")
	}
	return payload, nil
}

// claudeAccountResetStatusFrom maps the usage body: its plan windows, its
// pay-as-you-go spend and its cedar_ember block of reset grants.
func claudeAccountResetStatusFrom(payload map[string]any) accountResetStatus {
	status := accountResetStatus{
		Entries: claudeStructuredPlanUsage(map[string]any{"workass.claude/usage": map[string]any{"rate_limits": payload}}),
		Credits: claudeResetCredits(payload["cedar_ember"]),
	}
	if raw, present := payload["extra_usage"]; present {
		status.ExtraUsage = claudeExtraUsage(raw)
		status.ExtraUsageSet = true
	}
	return status
}

func claudeResetCredits(raw any) *RateLimitResetCreditsSummary {
	summary := &RateLimitResetCreditsSummary{Credits: []RateLimitResetCredit{}}
	block := mapFromAny(raw)
	if len(block) == 0 {
		return summary
	}
	summary.CooldownUntil = rfc3339Timestamp(block["cooldown_until"])
	if eligible, _ := block["eligible"].(bool); !eligible {
		return summary
	}
	for _, rawGrant := range anySlice(block["grants"]) {
		grant := mapFromAny(rawGrant)
		id := strings.TrimSpace(asString(grant["id"]))
		left, ok := int64FromAny(grant["resets_left"])
		if !claudeResetGrantIDPattern.MatchString(id) || !ok || left < 0 {
			continue
		}
		total, ok := int64FromAny(grant["resets_total"])
		if !ok || total < left {
			total = left
		}
		paused, _ := grant["paused"].(bool)
		usable, _ := grant["usable_now"].(bool)
		requiresLimit := true
		if value, ok := grant["use_requires_limit"].(bool); ok {
			requiresLimit = value
		}
		status := "available"
		switch {
		case left == 0:
			status = "used"
		case paused:
			status = "paused"
		}
		usableNow := usable && status == "available"
		remaining, outOf := left, total
		credit := RateLimitResetCredit{
			ID:            id,
			ResetType:     boundedPlanUsageText(grant["type"], 40),
			Status:        status,
			GrantedAt:     rfc3339Timestamp(grant["starts_at"]),
			ExpiresAt:     rfc3339Timestamp(grant["ends_at"]),
			Title:         boundedPlanUsageText(grant["label"], 200),
			Remaining:     &remaining,
			Total:         &outOf,
			UsableNow:     &usableNow,
			RequiresLimit: requiresLimit,
			Refills:       claudeResetWindows(grant["clears"]),
		}
		if !usableNow && status == "available" {
			credit.UnavailableReason = claudeResetUnavailableReason(asString(grant["unavailable_reason"]), requiresLimit)
		}
		if status == "paused" {
			credit.UnavailableReason = "paused"
		}
		summary.Credits = append(summary.Credits, credit)
		if status == "available" {
			summary.AvailableCount += left
		}
	}
	return summary
}

// claudeResetUnavailableReason maps Claude's reason to the neutral codes the
// Cuentas page words: needs-limit, cooldown, blocked:<window>, cannot-clear,
// unavailable.
func claudeResetUnavailableReason(reason string, requiresLimit bool) string {
	switch strings.TrimSpace(reason) {
	case "needs_limit":
		return "needs-limit"
	case "cooldown":
		return "cooldown"
	case "blocked_five_hour":
		return "blocked:five_hour"
	case "blocked_seven_day":
		return "blocked:seven_day"
	case "blocked_opus", "blocked_sonnet", "blocked_other":
		return "blocked:other"
	case "cannot_clear":
		return "cannot-clear"
	case "":
		if requiresLimit {
			return "needs-limit"
		}
		return "unavailable"
	default:
		return "unavailable"
	}
}

func claudeResetWindows(raw any) []string {
	var out []string
	for _, item := range anySlice(raw) {
		window := strings.TrimSpace(asString(item))
		if stringInList(claudeResetWindowIDs, window) && !stringInList(out, window) {
			out = append(out, window)
		}
	}
	return out
}

func claudeAccountResetClaimFrom(answer map[string]any) (accountResetClaim, error) {
	claim := accountResetClaim{Refilled: claudeResetWindows(answer["cleared"])}
	if left, ok := int64FromAny(answer["resets_left"]); ok && left >= 0 {
		claim.Remaining = &left
	}
	reason := strings.TrimSpace(asString(answer["reason"]))
	switch result := strings.TrimSpace(asString(answer["result"])); result {
	case "reset":
		claim.Outcome = "reset"
	case "already_used":
		claim.Outcome = "alreadyRedeemed"
	case "not_limited":
		claim.Outcome = "nothingToReset"
		claim.Message = "Todavía no llegaste a un límite que este reset rellene. Queda guardado."
	case "cooldown":
		return accountResetClaim{}, newAccountResetError(accountResetRateLimited, "Hay que esperar un rato antes de usar otro reset.")
	case "ineligible", "unavailable":
		claim.Outcome = "noCredit"
		claim.Message = claudeResetClaimReasonText(reason)
	default:
		return accountResetClaim{}, newAccountResetError(accountResetUnavailable, "Claude no confirmó el reset. Mirá tus límites antes de reintentar.")
	}
	return claim, nil
}

func claudeResetClaimReasonText(reason string) string {
	switch reason {
	case "expired":
		return "Ese reset venció."
	case "paused":
		return "Ese reset está en pausa."
	case "not_next_grant":
		return "Primero tenés que usar otro de tus resets."
	case "unknown_grant", "no_grant":
		return "Claude ya no encuentra ese reset."
	case "cli_version":
		return "Claude pide una versión más nueva de Claude Code para este reset."
	case "reset_unconfirmed", "stamp_indeterminate":
		return "Claude no confirmó el reset. Mirá tus límites antes de reintentar."
	default:
		return "Claude dice que este reset no está disponible ahora."
	}
}

// claudeExtraUsage maps extra_usage, whose amounts are cents, to major units.
func claudeExtraUsage(raw any) *PlanUsageExtraUsage {
	value := mapFromAny(raw)
	if len(value) == 0 {
		return nil
	}
	enabled, _ := value["is_enabled"].(bool)
	out := &PlanUsageExtraUsage{Enabled: enabled, Currency: strings.ToUpper(boundedPlanUsageText(value["currency"], 8))}
	if cents, ok := floatFromAny(value["monthly_limit"]); ok && cents >= 0 {
		limit := cents / 100
		out.MonthlyLimit = &limit
	}
	if cents, ok := floatFromAny(value["used_credits"]); ok && cents >= 0 {
		used := cents / 100
		out.Used = &used
	}
	if percent, ok := floatFromAny(value["utilization"]); ok && percent >= 0 {
		out.Utilization = &percent
	}
	return out
}

// claudePlanUsageAccount reads the login behind a usage read: the SDK's
// AccountInfo the host attaches as workassAccount, and the plan the usage
// body names.
func claudePlanUsageAccount(usage map[string]any) *PlanUsageAccount {
	info := mapFromAny(usage["workassAccount"])
	subscription := strings.TrimSpace(firstNonEmpty(asString(usage["subscription_type"]), asString(info["subscriptionType"])))
	if len(info) == 0 && subscription == "" {
		return nil
	}
	account := &PlanUsageAccount{
		Email:        boundedPlanUsageText(info["email"], 200),
		Organization: boundedPlanUsageText(info["organization"], 200),
	}
	backend := strings.TrimSpace(asString(info["apiProvider"]))
	keySource := strings.TrimSpace(asString(info["apiKeySource"]))
	switch {
	case backend != "" && backend != "firstParty":
		account.Billing = "cloud"
		account.Backend = claudeBackendName(backend)
	case subscription != "":
		account.Billing = "subscription"
		account.Plan = planNameWords(subscription)
		account.ManageURL = claudeUsageSettingsURL
	case keySource != "" && keySource != "none":
		account.Billing = "api-key"
		account.KeySource = boundedPlanUsageText(keySource, 80)
	}
	return account
}

func claudeBackendName(backend string) string {
	switch backend {
	case "bedrock":
		return "Amazon Bedrock"
	case "vertex":
		return "Google Vertex AI"
	case "foundry":
		return "Microsoft Foundry"
	case "anthropicAws":
		return "Anthropic en AWS"
	case "anthropicGoogleCloud":
		return "Anthropic en Google Cloud"
	case "gateway":
		return "Gateway"
	default:
		return boundedPlanUsageText(backend, 60)
	}
}
