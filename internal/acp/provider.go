package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	providercontract "workass/internal/provider"
)

const (
	providerStatusInactive   = "inactive"
	providerStatusReady      = "ready"
	providerStatusError      = "error"
	providerStatusNotFound   = "not-found"
	providerStatusNeedsLogin = "needs-login"
	defaultProbeTimeout      = 10 * time.Second
	devinProbeTimeout        = 60 * time.Second
	frontierProbeTimeout     = 30 * time.Second

	localLMStudioProviderID = "local-lmstudio"
	localOllamaProviderID   = "local-ollama"
	localOMLXProviderID     = "local-omlx"
	workassAgentBinEnv      = "WORKASS_AGENT_BIN"
	workassAgentDevGoRunEnv = "WORKASS_AGENT_DEV_GO_RUN"
	defaultOMLXModelsURL    = "http://127.0.0.1:8000/v1/models"
	maxOMLXSettingsBytes    = 1024 * 1024
	maxOMLXBootstrapBytes   = 16 * 1024
)

var defaultQwenModelEndpoints = []string{
	"http://127.0.0.1:1234/v1/models",
	"http://127.0.0.1:11434/v1/models",
	defaultOMLXModelsURL,
}

type localModelServer struct {
	ProviderID string
	Name       string
	Endpoint   string
	OMLX       bool
}

type providerRuntime struct {
	Config     ProviderConfig
	Status     string
	LatencyMs  *int64
	Error      string
	FixHint    string
	Models     []Model
	Modes      []Mode
	AgentName  string
	Probed     bool
	CLIVersion *CLIVersion
}

type CatalogGroup struct {
	ProviderID        string            `json:"providerId"`
	ProviderName      string            `json:"providerName"`
	Models            []Model           `json:"models"`
	Modes             []Mode            `json:"modes"`
	PermissionIntents map[string]string `json:"permissionIntents"`
	AssistantBrand    string            `json:"assistantBrand,omitempty"`
	Status            string            `json:"status,omitempty"`
	LatencyMs         *int64            `json:"latencyMs,omitempty"`
	Error             string            `json:"error,omitempty"`
	FixHint           string            `json:"fixHint,omitempty"`
	Badge             string            `json:"badge,omitempty"`
}

// Provider is the small provider-specific seam used before any ACP/native
// handshake starts.  A provider owns how its user-installed executable is
// found; the manager owns the common detection, launch, update, and catalog
// lifecycle around it.  Keeping executable discovery here means every later
// operation uses the same absolute path that was found from the daemon's PATH
// (or from the provider's explicit override).
type Provider interface {
	ID() string
	ResolveExecutable(ProviderConfig) (string, error)
}

type cliProvider struct {
	id             string
	defaultCommand string
	pathEnv        []string
	pathNames      []string
	knownPaths     func() []string
}

func (p cliProvider) ID() string { return p.id }

func (p cliProvider) ResolveExecutable(cfg ProviderConfig) (string, error) {
	for _, envName := range p.pathEnv {
		if explicit := strings.TrimSpace(os.Getenv(envName)); explicit != "" {
			return resolveProviderExecutablePath(explicit, envName)
		}
	}

	command := strings.TrimSpace(cfg.Command)
	if command != "" && command != p.defaultCommand {
		return resolveProviderExecutablePath(command, "provider command")
	}
	// ResolvedCommand is discovery cache, not an explicit user selection. Prefer
	// the current stable PATH entry so a newly installed vendor CLI replaces an
	// older cached executable without requiring a fresh chat or providers.json
	// edit. Never promote a terminal-injection shim: those paths live below a
	// temporary directory and can outlive neither the terminal nor an app update.
	for _, name := range p.pathNames {
		if resolved, err := resolveProviderExecutableOnPATH(name); err == nil && !isTransientProviderShim(resolved) {
			return resolved, nil
		}
	}
	// A valid cached path remains the fallback when the provider is intentionally
	// installed outside PATH. The manager persists any newer PATH resolution.
	if cached := strings.TrimSpace(cfg.ResolvedCommand); cached != "" {
		if resolved, err := resolveProviderExecutablePath(cached, "resolved provider command"); err == nil && !isTransientProviderShim(resolved) {
			return resolved, nil
		}
	}
	if p.knownPaths != nil {
		for _, candidate := range p.knownPaths() {
			if resolved, err := resolveProviderExecutablePath(candidate, "provider install path"); err == nil {
				return resolved, nil
			}
		}
	}
	return "", fmt.Errorf("%s not found on PATH", p.id)
}

func providerForID(id string) (Provider, bool) {
	registration, ok := providerRegistrationForID(id)
	if !ok || registration.Discovery == nil {
		return nil, false
	}
	provider := *registration.Discovery
	return provider, true
}

func isTransientProviderShim(path string) bool {
	candidate, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil || candidate == "" {
		return false
	}
	// Terminal launchers may prepend short-lived CLI wrapper directories to
	// PATH. Treat that shape as discovery noise only when it is also rooted in
	// the operating system's temporary directory.
	clean := strings.ToLower(filepath.ToSlash(filepath.Clean(candidate)))
	if !strings.Contains(clean, "cli-shims/") {
		return false
	}
	tempRoot, err := filepath.Abs(strings.TrimSpace(os.TempDir()))
	if err != nil || tempRoot == "" {
		return false
	}
	rel, err := filepath.Rel(tempRoot, candidate)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

func resolveProviderExecutableOnPATH(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || filepath.IsAbs(name) || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("provider PATH name is invalid: %q", name)
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, name)
		if !executableFile(candidate) || isTransientProviderShim(candidate) {
			continue
		}
		resolved, err := filepath.Abs(candidate)
		if err != nil {
			return candidate, nil
		}
		return resolved, nil
	}
	return "", fmt.Errorf("provider PATH %q was not found", name)
}

func resolveProviderExecutable(cfg ProviderConfig) (string, error) {
	if provider, ok := providerForID(cfg.ID); ok {
		return provider.ResolveExecutable(cfg)
	}
	command := strings.TrimSpace(firstNonEmpty(cfg.ResolvedCommand, cfg.Command))
	if command == "" {
		return "", fmt.Errorf("provider %q has no executable command", cfg.ID)
	}
	return resolveProviderExecutablePath(command, "provider command")
}

func resolveProviderExecutablePath(command, label string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", fmt.Errorf("%s is empty", label)
	}
	if filepath.IsAbs(command) || strings.ContainsAny(command, `/\\`) {
		if executableFile(command) {
			return command, nil
		}
		return "", fmt.Errorf("%s points to an unusable executable: %s", label, command)
	}
	resolved, err := exec.LookPath(command)
	if err != nil {
		return "", fmt.Errorf("%s %q was not found on PATH", label, command)
	}
	if !executableFile(resolved) {
		return "", fmt.Errorf("%s resolved to an unusable executable: %s", label, resolved)
	}
	return resolved, nil
}

// BuiltInProviderConfigs returns the daemon's default ACP registry. Only the
// deterministic mock is enabled by default for local daemon development.
func BuiltInProviderConfigs(rootDir string) []ProviderConfig {
	configs := make([]ProviderConfig, 0, len(providerRegistrationOrder))
	for _, id := range providerRegistrationOrder {
		registration, ok := providerRegistrationForID(id)
		if !ok {
			continue
		}
		config := builtInProviderConfig(registration, rootDir)
		if registration.FixtureOnly {
			config.Args = []string{filepath.Join("desktop", "acp", "mock-server.mjs")}
		}
		configs = append(configs, config)
	}
	return configs
}

func LoadProviderConfigs(filePath, rootDir string) ([]ProviderConfig, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return BuiltInProviderConfigs(rootDir), nil
		}
		return nil, err
	}
	providers, err := decodeProviderConfigs(data)
	if err != nil {
		return nil, err
	}
	if len(providers) == 0 {
		return BuiltInProviderConfigs(rootDir), nil
	}
	return NormalizeProviderCacheConfigs(providers, rootDir), nil
}

func decodeProviderConfigs(data []byte) ([]ProviderConfig, error) {
	var rawItems []json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&rawItems); err != nil {
		return nil, err
	}
	providers := make([]ProviderConfig, 0, len(rawItems))
	for _, raw := range rawItems {
		var provider ProviderConfig
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&provider); err != nil {
			return nil, err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err == nil {
			_, provider.enabledSet = fields["enabled"]
			// enabled=false is also persisted for transient detection and
			// needs-login states. Only disabledByUser carries durable user
			// intent; promoting a disabled runtime state here makes the next
			// startup skip that provider forever.
		}
		providers = append(providers, provider)
	}
	return providers, nil
}

func SaveProviderConfigs(filePath string, providers []ProviderConfig) error {
	if strings.TrimSpace(filePath) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(providersForJSON(providers), "", "  ")
	if err != nil {
		return err
	}
	tmp := filePath + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filePath)
}

func providersForJSON(providers []ProviderConfig) []ProviderConfig {
	out := make([]ProviderConfig, 0, len(providers))
	for _, provider := range providers {
		provider.CWD = ""
		provider.Label = ""
		if provider.Env != nil && len(provider.Env) == 0 {
			provider.Env = nil
		}
		if provider.AutoEnv != nil && len(provider.AutoEnv) == 0 {
			provider.AutoEnv = nil
		}
		if provider.Args == nil {
			provider.Args = []string{}
		}
		out = append(out, provider)
	}
	return out
}

func NormalizeProviderConfigs(providers []ProviderConfig, rootDir string) []ProviderConfig {
	builtIns := BuiltInProviderConfigs(rootDir)
	byID := make(map[string]ProviderConfig, len(builtIns))
	for _, provider := range builtIns {
		byID[provider.ID] = provider
	}
	seen := map[string]struct{}{}
	out := make([]ProviderConfig, 0, len(providers))
	for _, raw := range providers {
		id := normalizeProviderID(raw.ID)
		if id == "" {
			id = normalizeProviderID(raw.Name)
		}
		if id == "" {
			continue
		}
		base := ProviderConfig{ID: id}
		if builtIn, ok := byID[id]; ok {
			base = builtIn
		}
		merged := mergeProviderConfig(base, raw, rootDir)
		if _, ok := seen[merged.ID]; ok {
			for i := range out {
				if out[i].ID == merged.ID {
					out[i] = merged
					break
				}
			}
			continue
		}
		seen[merged.ID] = struct{}{}
		out = append(out, merged)
	}
	return out
}

func NormalizeProviderCacheConfigs(providers []ProviderConfig, rootDir string) []ProviderConfig {
	normalized := NormalizeProviderConfigs(providers, rootDir)
	seen := make(map[string]bool, len(normalized))
	for _, provider := range normalized {
		seen[provider.ID] = true
	}
	for _, builtIn := range BuiltInProviderConfigs(rootDir) {
		if !seen[builtIn.ID] {
			normalized = append(normalized, builtIn)
		}
	}
	return normalized
}

func mergeProviderConfig(base, raw ProviderConfig, rootDir string) ProviderConfig {
	provider := base
	if id := normalizeProviderID(raw.ID); id != "" {
		provider.ID = id
	}
	if name := strings.TrimSpace(raw.Name); name != "" {
		provider.Name = name
	}
	if command := strings.TrimSpace(raw.Command); command != "" {
		provider.Command = command
	}
	if raw.Args != nil {
		provider.Args = append([]string(nil), raw.Args...)
	}
	if raw.Env != nil {
		provider.Env = copyStringMap(raw.Env)
	}
	if raw.AutoEnv != nil {
		provider.AutoEnv = copyStringMap(raw.AutoEnv)
	}
	if raw.enabledSet || raw.Enabled {
		provider.Enabled = raw.Enabled
	}
	if badge := strings.TrimSpace(raw.Badge); badge != "" {
		provider.Badge = badge
	}
	provider.Detected = raw.Detected
	provider.DetectedAt = strings.TrimSpace(raw.DetectedAt)
	provider.ResolvedCommand = strings.TrimSpace(raw.ResolvedCommand)
	provider.DisabledByUser = raw.DisabledByUser
	provider.NeedsLogin = raw.NeedsLogin
	provider.LaunchSanitizationRevision = raw.LaunchSanitizationRevision
	provider.enabledSet = raw.enabledSet
	if cwd := strings.TrimSpace(raw.CWD); cwd != "" {
		provider.CWD = cwd
	}
	if label := strings.TrimSpace(raw.Label); label != "" {
		provider.Label = label
	}
	return normalizeProviderConfig(provider, rootDir, provider.ID)
}

func (o Options) withProviderDefaults() Options {
	singleProvider := o.Provider
	singleProviderSet := providerHasLaunch(singleProvider)
	if len(o.Providers) == 0 {
		o.Providers = BuiltInProviderConfigs(o.RootDir)
		if singleProviderSet {
			custom := normalizeProviderConfig(singleProvider, o.RootDir, "custom")
			custom.Enabled = true
			o.Providers = upsertProvider(o.Providers, custom)
			if o.DefaultProviderID == "" {
				o.DefaultProviderID = custom.ID
			}
		}
	} else {
		o.Providers = NormalizeProviderConfigs(o.Providers, o.RootDir)
	}
	if o.DefaultProviderID == "" {
		o.DefaultProviderID = defaultProviderID(o.Providers)
	}
	if cfg, ok := providerFromSlice(o.Providers, o.DefaultProviderID); ok {
		o.Provider = cfg
	} else if len(o.Providers) > 0 {
		o.Provider = o.Providers[0]
		o.DefaultProviderID = o.Provider.ID
	}
	return o
}

func providerHasLaunch(provider ProviderConfig) bool {
	return strings.TrimSpace(provider.ID) != "" ||
		strings.TrimSpace(provider.Name) != "" ||
		strings.TrimSpace(provider.Command) != "" ||
		provider.Args != nil ||
		provider.Env != nil ||
		strings.TrimSpace(provider.CWD) != "" ||
		strings.TrimSpace(provider.Label) != "" ||
		provider.Enabled ||
		strings.TrimSpace(provider.Badge) != ""
}

func normalizeProviderConfig(provider ProviderConfig, rootDir, fallbackID string) ProviderConfig {
	provider.ID = normalizeProviderID(firstNonEmpty(provider.ID, fallbackID))
	if provider.ID == "" {
		provider.ID = "custom"
	}
	provider.Name = strings.TrimSpace(firstNonEmpty(provider.Name, provider.Label, provider.ID))
	provider.Label = strings.TrimSpace(firstNonEmpty(provider.Label, provider.Name))
	provider.Command = strings.TrimSpace(provider.Command)
	if provider.Args == nil {
		provider.Args = []string{}
	} else {
		provider.Args = append([]string(nil), provider.Args...)
	}
	if provider.Env != nil {
		provider.Env = copyStringMap(provider.Env)
	}
	if provider.AutoEnv != nil {
		provider.AutoEnv = copyStringMap(provider.AutoEnv)
	}
	provider.ResolvedCommand = strings.TrimSpace(provider.ResolvedCommand)
	provider.DetectedAt = strings.TrimSpace(provider.DetectedAt)
	if strings.TrimSpace(provider.CWD) == "" {
		provider.CWD = rootDir
	}
	return provider
}

func normalizeProviderID(raw string) string {
	return string(providercontract.NormalizeID(raw))
}

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[strings.TrimSpace(k)] = v
	}
	return out
}

func redactedStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		key := strings.TrimSpace(k)
		if secretKeyRE.MatchString(key) {
			out[key] = "[redacted]"
			continue
		}
		out[key] = redactSensitiveText(v)
	}
	return out
}

func effectiveProviderEnv(provider ProviderConfig) map[string]string {
	env := copyStringMap(provider.AutoEnv)
	for k, v := range provider.Env {
		env[strings.TrimSpace(k)] = v
	}
	if len(env) == 0 {
		return nil
	}
	return env
}

func launchCommand(provider ProviderConfig) string {
	if command := strings.TrimSpace(provider.ResolvedCommand); command != "" {
		return command
	}
	return strings.TrimSpace(provider.Command)
}

func launchProviderConfig(provider ProviderConfig) ProviderConfig {
	provider.Command = launchCommand(provider)
	provider.Env = effectiveProviderEnv(provider)
	return provider
}

func fileExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func upsertProvider(providers []ProviderConfig, provider ProviderConfig) []ProviderConfig {
	for i := range providers {
		if providers[i].ID == provider.ID {
			providers[i] = provider
			return providers
		}
	}
	return append(providers, provider)
}

func providerFromSlice(providers []ProviderConfig, id string) (ProviderConfig, bool) {
	id = normalizeProviderID(id)
	for _, provider := range providers {
		if provider.ID == id {
			return provider, true
		}
	}
	return ProviderConfig{}, false
}

func defaultProviderID(providers []ProviderConfig) string {
	for _, provider := range providers {
		if provider.Enabled && providerIsFixture(provider.ID) {
			return provider.ID
		}
	}
	for _, provider := range providers {
		if provider.Enabled {
			return provider.ID
		}
	}
	if len(providers) > 0 {
		return providers[0].ID
	}
	return defaultFixtureProviderID()
}

func (m *Manager) initProviders(opts Options) {
	m.providers = make(map[string]*providerRuntime, len(opts.Providers))
	m.providerOrder = make([]string, 0, len(opts.Providers))
	for _, provider := range opts.Providers {
		provider = normalizeProviderConfig(provider, opts.RootDir, provider.ID)
		status := providerStatusInactive
		probed := false
		if provider.NeedsLogin && !provider.DisabledByUser {
			provider.Enabled = false
			status = providerStatusNeedsLogin
			probed = true
		}
		m.providers[provider.ID] = &providerRuntime{Config: provider, Status: status, Probed: probed}
		m.providerOrder = append(m.providerOrder, provider.ID)
	}
	m.defaultProviderID = normalizeProviderID(opts.DefaultProviderID)
	if m.defaultProviderID == "" {
		m.defaultProviderID = defaultProviderID(opts.Providers)
	}
	m.providerConfigFile = opts.ProviderConfigFile
	if len(m.providers) == 0 {
		for _, provider := range BuiltInProviderConfigs(opts.RootDir) {
			status := providerStatusInactive
			m.providers[provider.ID] = &providerRuntime{Config: provider, Status: status}
			m.providerOrder = append(m.providerOrder, provider.ID)
		}
		m.defaultProviderID = defaultFixtureProviderID()
	}
}

func (m *Manager) providerConfig(providerID string) (ProviderConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.providerConfigLocked(providerID)
}

func (m *Manager) providerConfigLocked(providerID string) (ProviderConfig, error) {
	id := normalizeProviderID(providerID)
	if id == "" {
		id = m.defaultProviderID
	}
	if _, err := m.ProviderDefinition(id); err != nil {
		return ProviderConfig{}, fmt.Errorf("provider definition is unavailable: %w", err)
	}
	runtime := m.providers[id]
	if runtime == nil {
		return ProviderConfig{}, fmt.Errorf("unknown ACP provider: %s", id)
	}
	if runtime.Config.NeedsLogin {
		return ProviderConfig{}, m.providerNeedsLoginError(id)
	}
	if !runtime.Config.Enabled {
		return ProviderConfig{}, fmt.Errorf("ACP provider disabled: %s", id)
	}
	if strings.TrimSpace(runtime.Config.Command) == "" {
		return ProviderConfig{}, fmt.Errorf("ACP provider %q has no command", id)
	}
	return runtime.Config, nil
}

func (m *Manager) optionsForProviderLocked(providerID string) (Options, error) {
	opts := m.opts
	cfg, err := m.providerConfigLocked(providerID)
	if err != nil {
		return Options{}, err
	}
	opts.Provider = cfg
	opts.Providers = []ProviderConfig{cfg}
	opts.DefaultProviderID = cfg.ID
	return opts, nil
}

func (m *Manager) resolveSessionProviderLocked(opts SessionOptions) (string, error) {
	if id := normalizeProviderID(opts.ProviderID); id != "" {
		if m.isProductionRuntime() && providerIsFixture(id) {
			return "", errors.New("development fixture provider is unavailable in production")
		}
		bound := m.boundProviderForChatLocked(opts)
		if m.isProductionRuntime() && providerIsFixture(bound) {
			bound = ""
		}
		if !opts.ProviderLaneManaged && bound != "" && bound != id {
			return "", fmt.Errorf("chat is already bound to ACP provider %q", bound)
		}
		if _, err := m.providerConfigLocked(id); err != nil {
			return "", err
		}
		return id, nil
	}
	if id := normalizeProviderID(m.sessionProvider[opts.SessionID]); id != "" {
		if !m.isProductionRuntime() || !providerIsFixture(id) {
			if _, err := m.providerConfigLocked(id); err != nil {
				return "", err
			}
			return id, nil
		}
	}
	if id := m.boundProviderForChatLocked(opts); id != "" {
		if !m.isProductionRuntime() || !providerIsFixture(id) {
			if _, err := m.providerConfigLocked(id); err != nil {
				return "", err
			}
			return id, nil
		}
	}
	if !m.isProductionRuntime() || !providerIsFixture(m.defaultProviderID) {
		if _, err := m.providerConfigLocked(m.defaultProviderID); err == nil {
			return m.defaultProviderID, nil
		}
	}
	for _, id := range m.providerOrder {
		if m.isProductionRuntime() && providerIsFixture(id) {
			continue
		}
		if _, err := m.providerConfigLocked(id); err == nil {
			return id, nil
		}
	}
	return "", errors.New("no enabled ACP providers")
}

func (m *Manager) boundProviderForChatLocked(opts SessionOptions) string {
	for _, key := range chatProviderKeys(opts) {
		if id := normalizeProviderID(m.chatProviders[key]); id != "" {
			return id
		}
	}
	return ""
}

func (m *Manager) bindChatProviderLocked(opts SessionOptions, providerID string) {
	providerID = normalizeProviderID(providerID)
	if providerID == "" {
		return
	}
	for _, key := range chatProviderKeys(opts) {
		if key != "" {
			m.chatProviders[key] = providerID
		}
	}
}

func chatProviderKeys(opts SessionOptions) []string {
	var keys []string
	if chatID := strings.TrimSpace(opts.ChatID); chatID != "" {
		keys = append(keys, "chat:"+chatID)
	}
	if tabID := strings.TrimSpace(opts.TabID); tabID != "" {
		keys = append(keys, "tab:"+tabID)
	}
	return keys
}

func (m *Manager) enabledProviderIDsLocked() []string {
	out := make([]string, 0, len(m.providerOrder))
	for _, id := range m.providerOrder {
		if runtime := m.providers[id]; runtime != nil && runtime.Config.Enabled && !runtime.Config.NeedsLogin {
			if _, err := m.ProviderDefinition(id); err != nil {
				continue
			}
			out = append(out, id)
		}
	}
	return out
}

func (m *Manager) providerSnapshot(id string) (ProviderConfig, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	runtime := m.providers[normalizeProviderID(id)]
	if runtime == nil {
		return ProviderConfig{}, false
	}
	return runtime.Config, true
}

func (m *Manager) ProvidersList() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]map[string]any, 0, len(m.providerOrder))
	for _, id := range m.providerOrder {
		if m.isProductionRuntime() && providerIsFixture(id) {
			continue
		}
		runtime := m.providers[id]
		if runtime == nil {
			continue
		}
		status := runtime.Status
		if status == "" {
			status = providerStatusInactive
		}
		item := map[string]any{
			"id":      runtime.Config.ID,
			"name":    runtime.Config.Name,
			"enabled": runtime.Config.Enabled,
			"status":  status,
		}
		if runtime.Error != "" {
			item["message"] = runtime.Error
			item["error"] = runtime.Error
		}
		if runtime.LatencyMs != nil {
			item["latencyMs"] = *runtime.LatencyMs
		}
		if runtime.Config.Detected {
			item["detected"] = true
		}
		if runtime.Config.DetectedAt != "" {
			item["detectedAt"] = runtime.Config.DetectedAt
		}
		if runtime.Config.ResolvedCommand != "" {
			item["resolvedCommand"] = runtime.Config.ResolvedCommand
		}
		if runtime.Config.NeedsLogin {
			item["needsLogin"] = true
		}
		if runtime.Config.Badge != "" {
			item["badge"] = runtime.Config.Badge
		}
		if brand := providerAdapterForID(id).model.AssistantBrand; brand != "" {
			item["assistantBrand"] = brand
		}
		if hint := firstNonEmpty(runtime.FixHint, runtime.Config.FixHint); hint != "" {
			item["fixHint"] = hint
		}
		if runtime.CLIVersion != nil {
			item["cliVersion"] = runtime.CLIVersion
		}
		if len(runtime.Config.AutoEnv) > 0 {
			item["autoEnv"] = redactedStringMap(runtime.Config.AutoEnv)
		}
		out = append(out, item)
	}
	return out
}

// initializePersistedProviderAuthenticationState resolves durable needs-login
// hints through the registered definition after the registry exists. Runtime
// code never falls back to provider registration metadata.
func (m *Manager) initializePersistedProviderAuthenticationState() {
	for _, id := range m.providerOrder {
		m.mu.Lock()
		runtime := m.providers[id]
		needsLogin := runtime != nil && runtime.Config.NeedsLogin && !runtime.Config.DisabledByUser
		m.mu.Unlock()
		if !needsLogin {
			continue
		}
		strategy, err := m.providerAuthenticationStrategy(id)
		if err != nil {
			continue
		}
		m.mu.Lock()
		if runtime = m.providers[id]; runtime != nil && runtime.Config.NeedsLogin && !runtime.Config.DisabledByUser {
			runtime.FixHint = strings.TrimSpace(strategy.LoginHint())
			runtime.Config.FixHint = runtime.FixHint
		}
		m.mu.Unlock()
	}
}

// markProviderNeedsLogin applies the one definition-owned authentication
// policy. A policy lookup error is returned so callers fail closed instead of
// continuing into retry, replacement, prompt, or recovery behavior.
func (m *Manager) markProviderNeedsLogin(ctx context.Context, providerID string, cause error) (string, error) {
	id := normalizeProviderID(providerID)
	message := ""
	if cause != nil {
		message = redactSensitiveText(cause.Error())
	}
	strategy, err := m.providerAuthenticationStrategy(id)
	if err != nil {
		return "", fmt.Errorf("provider authentication policy is unavailable: %w", err)
	}
	if !strategy.IsAuthenticationFailure(cause) {
		return "", nil
	}
	hint := strings.TrimSpace(strategy.LoginHint())
	if hint == "" {
		return "", fmt.Errorf("provider authentication policy %q has no external login hint", id)
	}
	m.mu.Lock()
	runtime := m.providers[id]
	if runtime == nil {
		m.mu.Unlock()
		return "", fmt.Errorf("provider authentication policy references unknown provider %q", id)
	}
	policyRevision := providerLaunchSanitizationRevision(id)
	revisionChanged := policyRevision > runtime.Config.LaunchSanitizationRevision
	if revisionChanged {
		runtime.Config.LaunchSanitizationRevision = policyRevision
	}
	if runtime.Config.NeedsLogin && runtime.Status == providerStatusNeedsLogin && !runtime.Config.Enabled {
		providers := m.providerRecordsLocked()
		filePath := m.providerConfigFile
		m.mu.Unlock()
		if revisionChanged {
			if err := SaveProviderConfigs(filePath, providers); err != nil && m.opts.Logf != nil {
				m.opts.Logf("provider needs-login revision persist failed", map[string]any{"provider": id, "error": redactSensitiveText(err.Error())})
			}
		}
		return hint, nil
	}
	runtime.Status = providerStatusNeedsLogin
	runtime.Error = message
	runtime.FixHint = hint
	runtime.Config.Enabled = false
	runtime.Config.DisabledByUser = false
	runtime.Config.NeedsLogin = true
	runtime.Config.Detected = true
	runtime.Config.FixHint = hint
	runtime.Probed = true
	runtime.Models = nil
	runtime.Modes = nil
	runtime.AgentName = ""
	m.spareBlocked[id] = true
	providers := m.providerRecordsLocked()
	filePath := m.providerConfigFile
	m.mu.Unlock()
	if err := SaveProviderConfigs(filePath, providers); err != nil && m.opts.Logf != nil {
		m.opts.Logf("provider needs-login persist failed", map[string]any{"provider": id, "error": redactSensitiveText(err.Error())})
	}
	list := m.ProvidersList()
	m.emit("providers:list", list)
	m.EmitCatalog(ctx)
	if m.opts.Logf != nil {
		m.opts.Logf("provider requires login", map[string]any{"provider": id, "status": providerStatusNeedsLogin})
	}
	return hint, nil
}

func providerAuthenticationFailureError(providerID string, cause error, hint string) error {
	message := redactSensitiveText(errString(cause))
	if message == "" {
		message = fmt.Sprintf("ACP provider needs login: %s", normalizeProviderID(providerID))
	}
	if hint = strings.TrimSpace(hint); hint != "" && !strings.Contains(message, hint) {
		message += ". " + hint
	}
	return &providercontract.Error{Kind: providercontract.ErrorAuthenticationRequired, Message: message}
}
func (m *Manager) ToggleProvider(ctx context.Context, id string, enabled bool) ([]map[string]any, error) {
	id = normalizeProviderID(id)
	if id == "" {
		return nil, errors.New("missing provider id")
	}
	m.mu.Lock()
	runtime := m.providers[id]
	if runtime == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("unknown ACP provider: %s", id)
	}
	runtime.Config.Enabled = enabled
	runtime.Probed = false
	runtime.Models = nil
	runtime.Modes = nil
	runtime.AgentName = ""
	runtime.Error = ""
	runtime.FixHint = ""
	runtime.LatencyMs = nil
	if enabled {
		runtime.Config.NeedsLogin = false
		runtime.Status = providerStatusInactive
		runtime.Config.DisabledByUser = false
	} else {
		runtime.Status = providerStatusInactive
		runtime.Config.DisabledByUser = true
	}
	delete(m.spareBlocked, id)
	providers := m.providerRecordsLocked()
	filePath := m.providerConfigFile
	if !enabled {
		m.removeSpareProviderLocked(id)
	}
	m.mu.Unlock()

	if err := SaveProviderConfigs(filePath, providers); err != nil {
		return nil, err
	}
	m.EmitCatalog(ctx)
	list := m.ProvidersList()
	m.cacheProviderSnapshotEvent("providers:list", list)
	return list, nil
}

func (m *Manager) providerRecordsLocked() []ProviderConfig {
	out := make([]ProviderConfig, 0, len(m.providerOrder))
	for _, id := range m.providerOrder {
		if runtime := m.providers[id]; runtime != nil {
			out = append(out, runtime.Config)
		}
	}
	return out
}

func (m *Manager) removeSpareProviderLocked(providerID string) {
	out := m.spareSessions[:0]
	for _, rec := range m.spareSessions {
		if rec.providerID != providerID {
			out = append(out, rec)
		}
	}
	m.spareSessions = out
	delete(m.spareWarming, providerID)
}

type DetectOptions struct {
	ProviderID string
}

type providerDetectionResult struct {
	ProviderID       string            `json:"provider"`
	Label            string            `json:"label,omitempty"`
	OK               bool              `json:"ok"`
	Status           string            `json:"status"`
	Message          string            `json:"message,omitempty"`
	Error            string            `json:"error,omitempty"`
	FixHint          string            `json:"fixHint,omitempty"`
	LatencyMs        *int64            `json:"latencyMs,omitempty"`
	ResolvedCommand  string            `json:"resolvedCommand,omitempty"`
	AutoEnv          map[string]string `json:"autoEnv,omitempty"`
	Models           []Model           `json:"models,omitempty"`
	Modes            []Mode            `json:"modes,omitempty"`
	AgentName        string            `json:"agentName,omitempty"`
	ProtocolVersion  int               `json:"protocolVersion,omitempty"`
	CLIVersion       *CLIVersion       `json:"cliVersion,omitempty"`
	Detected         bool              `json:"detected,omitempty"`
	ExplicitDisabled bool              `json:"explicitDisabled,omitempty"`
	Terminal         bool              `json:"-"`
	autoEnv          map[string]string
	args             []string
	config           ProviderConfig
}

type providerDetectionStatusError struct {
	status  string
	message string
}

type providerDetectionIntent uint8

const (
	providerDetectionExplicit providerDetectionIntent = iota
	providerDetectionStartup
)

func (e *providerDetectionStatusError) Error() string {
	return e.message
}

func (m *Manager) DetectProviders(ctx context.Context, opts DetectOptions) map[string]any {
	pass := m.runProviderDetectionPass(ctx, []string{opts.ProviderID}, true, providerDetectionExplicit)
	return map[string]any{"ok": true, "detected": pass.detected, "results": pass.results, "providers": pass.providers}
}

type providerDetectionPass struct {
	results   []providerDetectionResult
	detected  []providerDetectionResult
	providers []map[string]any
}

func (m *Manager) runProviderDetectionPass(ctx context.Context, providerIDs []string, resetSnapshots bool, intent providerDetectionIntent) providerDetectionPass {
	if ctx == nil {
		ctx = context.Background()
	}
	if resetSnapshots {
		m.clearProviderSnapshots()
	}
	candidates := m.providerDetectionCandidates(intent, providerIDs...)
	results := make([]providerDetectionResult, len(candidates))
	var wg sync.WaitGroup
	for i, cfg := range candidates {
		i, cfg := i, cfg
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = m.detectProvider(ctx, cfg)
		}()
	}
	wg.Wait()

	m.applyDetectionResults(results)
	list := m.ProvidersList()
	m.emit("providers:list", list)
	m.EmitCatalog(ctx)
	m.scheduleProviderUpdateCheck()
	detected := make([]providerDetectionResult, 0, len(results))
	for _, result := range results {
		if result.OK {
			detected = append(detected, result)
		}
	}
	return providerDetectionPass{results: results, detected: detected, providers: list}
}

func (m *Manager) StartProviderDetection(ctx context.Context) {
	go func() {
		pass := m.runProviderDetectionPass(ctx, nil, true, providerDetectionStartup)
		m.retryStartupProviderDetection(ctx, retryableProviderDetectionIDs(pass.results))
	}()
}

func (m *Manager) retryStartupProviderDetection(ctx context.Context, providerIDs []string) {
	pending := normalizeProviderIDList(providerIDs)
	if len(pending) == 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	backoffs := append([]time.Duration(nil), m.opts.ProviderDetectionRetryBackoffs...)
	for attempt, delay := range backoffs {
		if len(pending) == 0 {
			return
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-timer.C:
		}
		m.opts.Logf("provider detection retry", map[string]any{
			"attempt":   attempt + 1,
			"delayMs":   delay.Milliseconds(),
			"providers": append([]string(nil), pending...),
		})
		pass := m.runProviderDetectionPass(ctx, pending, false, providerDetectionStartup)
		pending = retryableProviderDetectionIDs(pass.results)
	}
}

func retryableProviderDetectionIDs(results []providerDetectionResult) []string {
	ids := make([]string, 0, len(results))
	for _, result := range results {
		if result.Status == providerStatusError && !result.ExplicitDisabled && !result.Terminal {
			ids = append(ids, result.ProviderID)
		}
	}
	return normalizeProviderIDList(ids)
}

func normalizeProviderIDList(providerIDs []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(providerIDs))
	for _, providerID := range providerIDs {
		id := normalizeProviderID(providerID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func (m *Manager) providerDetectionCandidates(intent providerDetectionIntent, providerIDs ...string) []ProviderConfig {
	wants := make(map[string]bool)
	for _, providerID := range providerIDs {
		if id := normalizeProviderID(providerID); id != "" {
			wants[id] = true
		}
	}
	wantAll := len(wants) == 0
	m.mu.Lock()
	seen := map[string]bool{}
	out := make([]ProviderConfig, 0, len(m.providerOrder))
	recoveryIDs := make(map[string]bool)
	recoveryOrder := make([]string, 0, 1)
	for _, id := range m.providerOrder {
		if !wantAll && !wants[id] {
			continue
		}
		if !isAutoDetectableProviderID(id) {
			continue
		}
		if runtime := m.providers[id]; runtime != nil {
			if intent == providerDetectionStartup && runtime.Config.NeedsLogin {
				revision := providerLaunchSanitizationRevision(id)
				if revision < 1 || runtime.Config.LaunchSanitizationRevision >= revision {
					continue
				}
				// Claim the one-shot migration before launching. A failed persistence
				// blocks the probe, so a daemon crash or restart cannot turn a vendor
				// login failure into an automatic retry loop.
				runtime.Config.LaunchSanitizationRevision = revision
				recoveryIDs[id] = true
				recoveryOrder = append(recoveryOrder, id)
			}
			out = append(out, runtime.Config)
			seen[id] = true
		}
	}
	for _, server := range m.localModelServersLocked() {
		if seen[server.ProviderID] {
			continue
		}
		if !wantAll && !wants[server.ProviderID] {
			continue
		}
		out = append(out, m.localProviderConfigLocked(server))
		seen[server.ProviderID] = true
	}
	if len(recoveryIDs) == 0 {
		m.mu.Unlock()
		return out
	}
	providers := m.providerRecordsLocked()
	filePath := m.providerConfigFile
	m.mu.Unlock()
	if err := SaveProviderConfigs(filePath, providers); err != nil {
		if m.opts.Logf != nil {
			m.opts.Logf("provider startup recovery persist failed", map[string]any{
				"providers": recoveryOrder,
				"error":     redactSensitiveText(err.Error()),
			})
		}
		filtered := make([]ProviderConfig, 0, len(out)-len(recoveryIDs))
		for _, candidate := range out {
			if !recoveryIDs[normalizeProviderID(candidate.ID)] {
				filtered = append(filtered, candidate)
			}
		}
		return filtered
	}
	return out
}

func (m *Manager) detectProvider(parent context.Context, cfg ProviderConfig) providerDetectionResult {
	cfg = normalizeProviderConfig(cfg, m.opts.RootDir, cfg.ID)
	result := providerDetectionResult{
		ProviderID: cfg.ID,
		Label:      firstNonEmpty(cfg.Name, cfg.Label, cfg.ID),
		Status:     providerStatusInactive,
		config:     cfg,
	}
	authentication, policyErr := m.providerAuthenticationStrategy(cfg.ID)
	if policyErr != nil {
		result.Status = providerStatusError
		result.Message = redactSensitiveText(policyErr.Error())
		result.Error = result.Message
		result.Terminal = true
		m.logProviderDetection(result)
		return result
	}
	if cfg.DisabledByUser {
		result.Message = "disabled by user"
		result.ExplicitDisabled = true
		return result
	}

	resolved, args, autoEnv, inactive, err := m.prepareDetectedProvider(parent, cfg)
	if err != nil {
		result.Status = providerStatusNotFound
		var statusErr *providerDetectionStatusError
		if errors.As(err, &statusErr) && statusErr.status != "" {
			result.Status = statusErr.status
		}
		result.Message = err.Error()
		result.Error = result.Message
		m.logProviderDetection(result)
		return result
	}
	// Executable discovery is useful even when the provider cannot currently
	// serve models (for example, Qwen with no local model server). Preserve the
	// resolved path independently from readiness so Settings and later update
	// commands can still address the installed CLI.
	cfg.ResolvedCommand = resolved
	cfg.Args = append([]string(nil), args...)
	cfg.AutoEnv = autoEnv
	result.ResolvedCommand = resolved
	result.AutoEnv = redactedStringMap(autoEnv)
	result.autoEnv = copyStringMap(autoEnv)
	result.args = append([]string(nil), args...)
	result.config = cfg
	cliVersion := m.startInstalledCLIVersionCheck(parent, cfg.ID)
	if inactive != "" {
		result.Status = providerStatusInactive
		result.Message = inactive
		result.CLIVersion = collectInstalledCLIVersion(cliVersion)
		m.logProviderDetection(result)
		return result
	}
	timeout := providerProbeTimeout(cfg.ID)
	// initialize and session/new are separate ACP requests, each with the
	// provider's full per-request timeout. The enclosing detection context must
	// allow both stages to consume that budget.
	probeCtx, cancel := context.WithTimeout(parent, timeout*2)
	defer cancel()
	probeCfg := cfg
	probeCfg.Command = resolved
	probeCfg.Env = effectiveProviderEnv(cfg)
	probeCfg.Enabled = true

	start := time.Now()
	models, modes, agentName, err := m.probeProviderCatalogWithInitTimeout(probeCtx, probeCfg, timeout)
	latencyMs := time.Since(start).Milliseconds()
	result.LatencyMs = &latencyMs
	if err != nil {
		message := redactSensitiveText(err.Error())
		result.CLIVersion = collectInstalledCLIVersion(cliVersion)
		if authentication.IsAuthenticationFailure(err) {
			result.Status = providerStatusNeedsLogin
			result.FixHint = strings.TrimSpace(authentication.LoginHint())
			if result.FixHint == "" {
				result.Status = providerStatusError
				result.Terminal = true
				result.Message = fmt.Sprintf("provider authentication policy %q has no external login hint", cfg.ID)
				result.Error = result.Message
				m.logProviderDetection(result)
				return result
			}
			result.Detected = true
		} else {
			result.Status = providerStatusError
		}
		result.Message = message
		result.Error = message
		m.logProviderDetection(result)
		return result
	}
	result.OK = true
	result.Status = providerStatusReady
	result.Models = models
	result.Modes = modes
	result.AgentName = agentName
	result.Detected = true
	result.ProtocolVersion = ProtocolVersion
	result.CLIVersion = collectInstalledCLIVersion(cliVersion)
	m.logProviderDetection(result)
	return result
}

func providerProbeTimeout(providerID string) time.Duration {
	registration, ok := providerRegistrationForID(providerID)
	if ok && registration.ProbeTimeout > 0 {
		return registration.ProbeTimeout
	}
	return defaultProbeTimeout
}

func (m *Manager) prepareDetectedProvider(ctx context.Context, cfg ProviderConfig) (string, []string, map[string]string, string, error) {
	registration, ok := providerRegistrationForID(cfg.ID)
	if !ok || registration.Detection == nil {
		return "", nil, nil, "", fmt.Errorf("provider %q is not auto-detectable", cfg.ID)
	}
	return registration.Detection.Prepare(ctx, m, cfg)
}

func (m *Manager) applyDetectionResults(results []providerDetectionResult) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	m.mu.Lock()
	for _, result := range results {
		runtime := m.providers[result.ProviderID]
		if runtime == nil && isLocalProviderID(result.ProviderID) {
			cfg := result.config
			if cfg.ID == "" {
				if server, ok := m.localModelServerLocked(result.ProviderID); ok {
					cfg = m.localProviderConfigLocked(server)
				}
			}
			cfg = normalizeProviderConfig(cfg, m.opts.RootDir, result.ProviderID)
			runtime = &providerRuntime{Config: cfg, Status: providerStatusInactive}
			m.providers[cfg.ID] = runtime
			m.providerOrder = append(m.providerOrder, cfg.ID)
		}
		if runtime == nil {
			continue
		}
		if revision := providerLaunchSanitizationRevision(result.ProviderID); revision > runtime.Config.LaunchSanitizationRevision {
			runtime.Config.LaunchSanitizationRevision = revision
		}
		wasNeedsLogin := runtime.Config.NeedsLogin
		if runtime.Config.DisabledByUser || result.ExplicitDisabled {
			runtime.Config.Enabled = false
			runtime.Status = providerStatusInactive
			runtime.Error = firstNonEmpty(result.Message, "disabled by user")
			runtime.FixHint = ""
			runtime.LatencyMs = result.LatencyMs
			runtime.CLIVersion = nil
			continue
		}
		runtime.Status = result.Status
		runtime.Error = firstNonEmpty(result.Message, result.Error)
		runtime.FixHint = result.FixHint
		runtime.Config.FixHint = result.FixHint
		runtime.LatencyMs = result.LatencyMs
		runtime.CLIVersion = copyCLIVersion(result.CLIVersion)
		if result.ResolvedCommand != "" {
			runtime.Config.ResolvedCommand = result.ResolvedCommand
		}
		if result.args != nil {
			runtime.Config.Args = append([]string(nil), result.args...)
		}
		runtime.Config.AutoEnv = copyStringMap(result.autoEnv)
		if result.OK {
			delete(m.spareBlocked, result.ProviderID)
			runtime.Config.Enabled = true
			runtime.Config.DisabledByUser = false
			runtime.Config.NeedsLogin = false
			runtime.Config.Detected = true
			runtime.Config.DetectedAt = now
			runtime.Config.ResolvedCommand = result.ResolvedCommand
			if isLocalProviderID(result.ProviderID) && result.ResolvedCommand != "" {
				runtime.Config.Command = result.ResolvedCommand
			}
			runtime.Probed = true
			runtime.Status = providerStatusReady
			runtime.Error = ""
			runtime.FixHint = ""
			runtime.Config.FixHint = ""
			runtime.Models = append([]Model(nil), result.Models...)
			runtime.Modes = append([]Mode(nil), result.Modes...)
			runtime.AgentName = result.AgentName
			continue
		}
		if result.Status == providerStatusNeedsLogin {
			runtime.Config.Enabled = false
			runtime.Config.DisabledByUser = false
			runtime.Config.NeedsLogin = true
			runtime.Config.Detected = true
			runtime.Config.DetectedAt = now
			runtime.Config.ResolvedCommand = result.ResolvedCommand
			runtime.Probed = true
			runtime.Models = nil
			runtime.Modes = nil
			runtime.AgentName = ""
			m.spareBlocked[result.ProviderID] = true
			continue
		}
		// A failed explicit probe cannot silently erase a durable authentication
		// barrier. Only a successful probe or explicit re-enable may clear it.
		if wasNeedsLogin {
			runtime.Config.Enabled = false
			runtime.Config.NeedsLogin = true
			runtime.Status = providerStatusNeedsLogin
			runtime.FixHint = firstNonEmpty(runtime.FixHint, runtime.Config.FixHint)
			runtime.Config.FixHint = runtime.FixHint
			runtime.Probed = true
			runtime.Models = nil
			runtime.Modes = nil
			runtime.AgentName = ""
			m.spareBlocked[result.ProviderID] = true
			continue
		}
		if result.Status == providerStatusNotFound || result.Status == providerStatusInactive {
			runtime.Config.Enabled = false
			runtime.Config.Detected = false
			runtime.Config.DetectedAt = ""
			runtime.Config.ResolvedCommand = result.ResolvedCommand
			runtime.Config.AutoEnv = nil
			runtime.Config.FixHint = ""
			runtime.FixHint = ""
			runtime.Probed = false
			runtime.Models = nil
			runtime.Modes = nil
			runtime.AgentName = ""
			runtime.CLIVersion = nil
		}
		if result.Status == providerStatusError {
			// Enabled is runtime readiness, not durable user intent. Leaving a
			// previously detected provider enabled after its ACP probe fails lets
			// the warm-spare loop keep launching a broken executable. Detection
			// retries can promote it back to ready; DisabledByUser remains false.
			runtime.Config.Enabled = false
			runtime.FixHint = ""
			runtime.Config.FixHint = ""
			runtime.Probed = true
			runtime.Models = nil
			runtime.Modes = nil
			runtime.AgentName = ""
		}
	}
	providers := m.providerRecordsLocked()
	filePath := m.providerConfigFile
	m.mu.Unlock()
	err := SaveProviderConfigs(filePath, providers)
	if err != nil && m.opts.Logf != nil {
		m.opts.Logf("provider detection persist failed", map[string]any{"error": err.Error()})
	}
}

func isAutoDetectableProviderID(id string) bool {
	registration, ok := providerRegistrationForID(id)
	return ok && registration.Detection != nil
}

func isLocalProviderID(id string) bool {
	return providerIsLocal(id)
}

func (m *Manager) localModelServersLocked() []localModelServer {
	return localModelServers(m.opts.LocalModelEndpoints)
}

func localModelServers(configuredEndpoints []string) []localModelServer {
	endpoints := append([]string(nil), configuredEndpoints...)
	servers := []localModelServer{
		{ProviderID: localLMStudioProviderID, Name: "LM Studio (local)", Endpoint: defaultQwenModelEndpoints[0]},
		{ProviderID: localOllamaProviderID, Name: "Ollama (local)", Endpoint: defaultQwenModelEndpoints[1]},
		{ProviderID: localOMLXProviderID, Name: "oMLX (local)", Endpoint: defaultQwenModelEndpoints[2], OMLX: true},
	}
	if len(endpoints) == 0 {
		return servers
	}
	// An explicit endpoint list is a complete test/embedder override. Keeping
	// only its registered positions prevents a developer's real local servers
	// from leaking into an isolated detection run.
	if len(endpoints) < len(servers) {
		servers = servers[:len(endpoints)]
	}
	for i := range servers {
		if i < len(endpoints) && strings.TrimSpace(endpoints[i]) != "" {
			servers[i].Endpoint = strings.TrimSpace(endpoints[i])
		}
	}
	return servers
}

func (m *Manager) localModelServerLocked(providerID string) (localModelServer, bool) {
	providerID = normalizeProviderID(providerID)
	for _, server := range m.localModelServersLocked() {
		if server.ProviderID == providerID {
			return server, true
		}
	}
	return localModelServer{}, false
}

func (m *Manager) localProviderConfigLocked(server localModelServer) ProviderConfig {
	return normalizeProviderConfig(ProviderConfig{
		ID:      server.ProviderID,
		Name:    server.Name,
		Command: "workass-agent",
		Args:    []string{},
		Enabled: false,
		Badge:   "native",
		CWD:     m.opts.RootDir,
	}, m.opts.RootDir, server.ProviderID)
}

func (m *Manager) logProviderDetection(result providerDetectionResult) {
	if m.opts.Logf == nil {
		return
	}
	fields := map[string]any{
		"provider": result.ProviderID,
		"status":   result.Status,
		"ok":       result.OK,
	}
	if result.Message != "" {
		fields["message"] = result.Message
	}
	if result.LatencyMs != nil {
		fields["latencyMs"] = *result.LatencyMs
	}
	m.opts.Logf("provider detection", fields)
}

func resolveBinary(command string, names []string, known []string) (string, error) {
	command = strings.TrimSpace(command)
	if command != "" {
		if filepath.IsAbs(command) || strings.ContainsAny(command, `/\`) {
			if fileExists(command) {
				return command, nil
			}
		} else if resolved, err := exec.LookPath(command); err == nil {
			return resolved, nil
		}
	}
	for _, candidate := range known {
		if fileExists(candidate) {
			return candidate, nil
		}
	}
	for _, name := range names {
		if resolved, err := exec.LookPath(name); err == nil {
			return resolved, nil
		}
	}
	if command != "" {
		if resolved, err := exec.LookPath(command); err == nil {
			return resolved, nil
		}
	}
	if len(names) > 0 {
		return "", fmt.Errorf("%s not found on PATH", names[len(names)-1])
	}
	return "", errors.New("binary not found on PATH")
}

type agentLaunch struct {
	Command string
	Args    []string
}

// frontierNativeSpec describes the official provider-owned executable Workass
// integrates with. Claude is driven through Anthropic's Agent SDK host; Codex
// is driven through its app-server protocol. Neither path launches a Zed ACP
// compatibility package.
type frontierNativeSpec struct {
	ProviderID     string
	DefaultCommand string
	OverrideEnv    string
	PathNames      []string
}

func frontierNativeSpecForProvider(id string) (frontierNativeSpec, bool) {
	registration, ok := providerRegistrationForID(id)
	if !ok || registration.Native == nil {
		return frontierNativeSpec{}, false
	}
	return *registration.Native, true
}

func resolveFrontierNativeLaunch(cfg ProviderConfig) (agentLaunch, error) {
	return resolveFrontierNativeLaunchWithExecutable(cfg)
}

func resolveFrontierNativeLaunchWithExecutable(cfg ProviderConfig) (agentLaunch, error) {
	spec, ok := frontierNativeSpecForProvider(cfg.ID)
	if !ok {
		return agentLaunch{}, fmt.Errorf("provider %q is not a native frontier provider", cfg.ID)
	}
	resolved, err := resolveProviderExecutable(cfg)
	if err != nil {
		return agentLaunch{}, fmt.Errorf("official %s executable not found: checked %s override and PATH", spec.ProviderID, spec.OverrideEnv)
	}
	return agentLaunch{Command: resolved, Args: append([]string(nil), cfg.Args...)}, nil
}

func resolveFrontierNativeCommand(command, label string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", fmt.Errorf("%s is empty", label)
	}
	if filepath.IsAbs(command) || strings.ContainsAny(command, `/\`) {
		if executableFile(command) {
			return command, nil
		}
		return "", fmt.Errorf("%s points to an unusable official CLI: %s", label, command)
	}
	resolved, err := exec.LookPath(command)
	if err != nil {
		return "", fmt.Errorf("%s %q was not found on PATH", label, command)
	}
	if !executableFile(resolved) {
		return "", fmt.Errorf("%s resolved to an unusable official CLI: %s", label, resolved)
	}
	return resolved, nil
}

func isFrontierProviderID(id string) bool {
	_, ok := frontierNativeSpecForProvider(id)
	return ok
}

func (m *Manager) providerNeedsLoginError(id string) error {
	hint := ""
	if strategy, err := m.providerAuthenticationStrategy(id); err == nil {
		hint = strings.TrimSpace(strategy.LoginHint())
	} else {
		return fmt.Errorf("provider authentication policy is unavailable: %w", err)
	}
	message := fmt.Sprintf("ACP provider needs login: %s", normalizeProviderID(id))
	if hint != "" {
		message += ". " + hint
	}
	return &providercontract.Error{Kind: providercontract.ErrorAuthenticationRequired, Message: message}
}

func isAuthShapedError(text string) bool {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "authentication") ||
		strings.Contains(lower, "unauthenticated") ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "login") ||
		strings.Contains(lower, "log in") ||
		strings.Contains(lower, "logged out") ||
		strings.Contains(lower, "oauth") {
		return true
	}
	for _, phrase := range []string{
		"credential missing",
		"missing credential",
		"invalid credential",
		"expired credential",
		"credential required",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return containsDelimitedASCIIWord(lower, "auth")
}

func containsDelimitedASCIIWord(text, word string) bool {
	for offset := 0; offset < len(text); {
		relative := strings.Index(text[offset:], word)
		if relative < 0 {
			return false
		}
		start := offset + relative
		end := start + len(word)
		leftDelimited := start == 0 || !isASCIIWordByte(text[start-1])
		rightDelimited := end == len(text) || !isASCIIWordByte(text[end])
		if leftDelimited && rightDelimited {
			return true
		}
		offset = start + 1
	}
	return false
}

func isASCIIWordByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= '0' && value <= '9' || value == '_'
}

func resolveWorkassAgentLaunch(rootDir string) (agentLaunch, error) {
	executable, _ := os.Executable()
	return resolveWorkassAgentLaunchWithExecutable(rootDir, executable)
}

func resolveWorkassAgentLaunchWithExecutable(rootDir, daemonExecutable string) (agentLaunch, error) {
	if override := strings.TrimSpace(os.Getenv(workassAgentBinEnv)); override != "" {
		resolved, err := resolveExplicitWorkassAgentBinary(override)
		if err != nil {
			return agentLaunch{}, err
		}
		return agentLaunch{Command: resolved, Args: []string{}}, nil
	}

	for _, candidate := range workassAgentSiblingCandidates(daemonExecutable) {
		if executableFile(candidate) {
			return agentLaunch{Command: candidate, Args: []string{}}, nil
		}
	}

	if truthyEnv(os.Getenv(workassAgentDevGoRunEnv)) {
		if !looksLikeWorkassRepo(rootDir) {
			return agentLaunch{}, fmt.Errorf("%s=1 requires running from the workass repository root; could not find cmd/workass-agent/main.go under %s", workassAgentDevGoRunEnv, rootDir)
		}
		goBin, err := exec.LookPath("go")
		if err != nil {
			return agentLaunch{}, fmt.Errorf("%s=1 requested go run ./cmd/workass-agent, but go was not found on PATH", workassAgentDevGoRunEnv)
		}
		return agentLaunch{Command: goBin, Args: []string{"run", "./cmd/workass-agent"}}, nil
	}

	return agentLaunch{}, fmt.Errorf("workass-agent binary not found: set %s, place workass-agent next to the running workass daemon executable, or set %s=1 when running from the repository", workassAgentBinEnv, workassAgentDevGoRunEnv)
}

func resolveExplicitWorkassAgentBinary(command string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", fmt.Errorf("%s is empty", workassAgentBinEnv)
	}
	if filepath.IsAbs(command) || strings.ContainsAny(command, `/\`) {
		if executableFile(command) {
			return command, nil
		}
		return "", fmt.Errorf("%s points to an unusable workass-agent binary: %s", workassAgentBinEnv, command)
	}
	resolved, err := exec.LookPath(command)
	if err != nil {
		return "", fmt.Errorf("%s=%q was not found on PATH", workassAgentBinEnv, command)
	}
	if !executableFile(resolved) {
		return "", fmt.Errorf("%s resolved to an unusable workass-agent binary: %s", workassAgentBinEnv, resolved)
	}
	return resolved, nil
}

func workassAgentSiblingCandidates(daemonExecutable string) []string {
	daemonExecutable = strings.TrimSpace(daemonExecutable)
	if daemonExecutable == "" {
		return nil
	}
	dir := filepath.Dir(daemonExecutable)
	base := filepath.Base(daemonExecutable)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	suffix := executableSuffix()
	var candidates []string
	if strings.HasPrefix(stem, "workass") {
		candidates = append(candidates, filepath.Join(dir, "workass-agent"+strings.TrimPrefix(stem, "workass")+ext))
	}
	candidates = append(candidates,
		filepath.Join(dir, "workass-agent-"+runtime.GOOS+"-"+runtime.GOARCH+suffix),
		filepath.Join(dir, "workass-agent"+suffix),
	)
	return dedupeStrings(candidates)
}

func executableSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func executableFile(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0o111 != 0
}

func looksLikeWorkassRepo(rootDir string) bool {
	rootDir = strings.TrimSpace(rootDir)
	if rootDir == "" {
		return false
	}
	return fileExists(filepath.Join(rootDir, "go.mod")) && fileExists(filepath.Join(rootDir, "cmd", "workass-agent", "main.go"))
}

func truthyEnv(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, item := range in {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func (m *Manager) detectQwenLocalModel(ctx context.Context) (string, string, error) {
	servers := m.localModelServersLocked()
	type modelResult struct {
		modelID  string
		baseURL  string
		endpoint string
		err      error
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	ch := make(chan modelResult, len(servers))
	for _, server := range servers {
		server := server
		go func() {
			models, baseURL, err := m.queryOpenAIModelCatalog(ctx, server)
			modelID := ""
			if len(models) > 0 {
				modelID = models[0].ModelID
			}
			ch <- modelResult{modelID: modelID, baseURL: baseURL, endpoint: server.Endpoint, err: err}
		}()
	}
	var errs []string
	for range servers {
		select {
		case <-ctx.Done():
			if len(errs) == 0 {
				errs = append(errs, ctx.Err().Error())
			}
			return "", "", fmt.Errorf("no local OpenAI-compatible model server responded (%s)", strings.Join(errs, "; "))
		case result := <-ch:
			if result.err == nil && result.modelID != "" && result.baseURL != "" {
				return result.modelID, result.baseURL, nil
			}
			if result.err != nil {
				errs = append(errs, fmt.Sprintf("%s: %s", result.endpoint, result.err.Error()))
			}
		}
	}
	if len(errs) == 0 {
		errs = append(errs, "no models returned")
	}
	return "", "", fmt.Errorf("no local OpenAI-compatible model server responded (%s)", strings.Join(errs, "; "))
}

func (m *Manager) detectLocalModelServer(ctx context.Context, providerID string) ([]Model, string, error) {
	server, ok := m.localModelServerLocked(providerID)
	if !ok {
		return nil, "", fmt.Errorf("unknown local model server provider: %s", providerID)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	models, baseURL, err := m.queryOpenAIModelCatalog(ctx, server)
	if err != nil {
		return nil, "", fmt.Errorf("%s did not respond at %s (%s)", server.Name, server.Endpoint, err.Error())
	}
	if len(models) == 0 {
		return nil, "", fmt.Errorf("%s returned no models at %s", server.Name, server.Endpoint)
	}
	return models, baseURL, nil
}

func queryOpenAIModelCatalog(ctx context.Context, endpoint string) ([]Model, string, error) {
	return queryOpenAIModelCatalogWithAPIKey(ctx, endpoint, "")
}

func queryOpenAIModelCatalogWithAPIKey(ctx context.Context, endpoint, apiKey string) ([]Model, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", err
	}
	if apiKey = strings.TrimSpace(apiKey); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		return nil, "", err
	}
	models := make([]Model, 0, len(body.Data))
	seen := map[string]bool{}
	for _, model := range body.Data {
		if id := strings.TrimSpace(model.ID); id != "" {
			if seen[id] {
				continue
			}
			seen[id] = true
			models = append(models, Model{ModelID: id, Name: id})
		}
	}
	if len(models) == 0 {
		return nil, "", errors.New("no model id in /v1/models")
	}
	return normalizeCatalogModels(models), openAIBaseURLFromModelsEndpoint(endpoint), nil
}

func (m *Manager) queryOpenAIModelCatalog(ctx context.Context, server localModelServer) ([]Model, string, error) {
	apiKey := ""
	if server.OMLX {
		var err error
		apiKey, err = omlxAPIKey(m.opts)
		if err != nil {
			return nil, "", fmt.Errorf("read oMLX authentication settings: %w", err)
		}
	}
	return queryOpenAIModelCatalogWithAPIKey(ctx, server.Endpoint, apiKey)
}

func omlxAPIKey(opts Options) (string, error) {
	if apiKey := strings.TrimSpace(os.Getenv("OMLX_API_KEY")); apiKey != "" {
		return apiKey, nil
	}
	settingsFile, err := omlxSettingsFile(opts)
	if err != nil {
		return "", err
	}
	data, err := readBoundedProviderFile(settingsFile, maxOMLXSettingsBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	var settings struct {
		Auth struct {
			APIKey string `json:"api_key"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return "", fmt.Errorf("decode %s: %w", settingsFile, err)
	}
	return strings.TrimSpace(settings.Auth.APIKey), nil
}

func omlxSettingsFile(opts Options) (string, error) {
	if explicit := strings.TrimSpace(opts.OMLXSettingsFile); explicit != "" {
		return filepath.Abs(expandHomePath(explicit))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	basePath := strings.TrimSpace(os.Getenv("OMLX_BASE_PATH"))
	if basePath == "" && runtime.GOOS == "darwin" {
		bootstrap := filepath.Join(home, "Library", "Application Support", "oMLX", "base-path")
		if data, readErr := readBoundedProviderFile(bootstrap, maxOMLXBootstrapBytes); readErr == nil {
			basePath = strings.TrimSpace(string(data))
		}
	}
	if basePath == "" {
		basePath = filepath.Join(home, ".omlx")
	} else {
		basePath = expandHomePathWithHome(basePath, home)
	}
	basePath, err = filepath.Abs(basePath)
	if err != nil {
		return "", err
	}
	return filepath.Join(basePath, "settings.json"), nil
}

func expandHomePath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return expandHomePathWithHome(path, home)
}

func expandHomePathWithHome(path, home string) string {
	path = strings.TrimSpace(path)
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		return filepath.Join(home, path[2:])
	}
	return path
}

func readBoundedProviderFile(path string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("%s exceeds the %d-byte limit", path, maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%s exceeds the %d-byte limit", path, maxBytes)
	}
	return data, nil
}

func openAIBaseURLFromModelsEndpoint(endpoint string) string {
	baseURL := strings.TrimSpace(endpoint)
	baseURL = strings.TrimRight(baseURL, "/")
	baseURL = strings.TrimSuffix(baseURL, "/models")
	baseURL = strings.TrimRight(baseURL, "/")
	return baseURL
}

func (m *Manager) Catalog(ctx context.Context) map[string]any {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	ids := m.enabledProviderIDsLocked()
	defaultID := m.defaultProviderID
	m.mu.Unlock()

	groups := make([]CatalogGroup, 0, len(ids))
	for _, id := range ids {
		groups = append(groups, m.catalogGroup(ctx, id))
	}
	groups = m.userFacingCatalogGroups(groups)
	models, modes := []Model{}, []Mode{}
	var fallback *CatalogGroup
	for i := range groups {
		if groups[i].Status != providerStatusReady || len(groups[i].Models) == 0 {
			continue
		}
		if groups[i].ProviderID == defaultID {
			models = append([]Model(nil), groups[i].Models...)
			modes = append([]Mode(nil), groups[i].Modes...)
			fallback = nil
			break
		}
		if fallback == nil {
			fallback = &groups[i]
		}
	}
	if fallback != nil {
		models = append([]Model(nil), fallback.Models...)
		modes = append([]Mode(nil), fallback.Modes...)
	}
	return map[string]any{
		"models": models,
		"modes":  modes,
		"groups": groups,
	}
}

// CatalogSnapshotGroups returns the current visible catalog without probing or
// starting a provider. It is used by state:digest, whose five-second heartbeat
// must remain read-only.
func (m *Manager) CatalogSnapshotGroups() []CatalogGroup {
	m.mu.Lock()
	ids := m.enabledProviderIDsLocked()
	groups := make([]CatalogGroup, 0, len(ids))
	for _, id := range ids {
		runtime := m.providers[id]
		if runtime == nil {
			continue
		}
		groups = append(groups, runtime.catalogGroupLocked())
	}
	m.mu.Unlock()
	return m.userFacingCatalogGroups(groups)
}

func (m *Manager) EmitCatalog(ctx context.Context) {
	m.emit("chat:catalog", m.Catalog(ctx))
}

func (m *Manager) updateProviderCatalogFromBridge(b *Bridge, models []Model, modes []Mode) {
	b.mu.Lock()
	providerID := b.providerID
	agentName := b.agentName
	knownEffortModels := make(map[string]bool, len(b.axisEffortsByModel)+len(b.variantEffortsByModel))
	for modelID := range b.axisEffortsByModel {
		knownEffortModels[strings.TrimSpace(modelID)] = true
	}
	for modelID := range b.variantEffortsByModel {
		knownEffortModels[strings.TrimSpace(modelID)] = true
	}
	b.mu.Unlock()
	m.mu.Lock()
	if runtime := m.providers[providerID]; runtime != nil && runtime.Config.Enabled && !runtime.Config.NeedsLogin {
		runtime.Probed = true
		runtime.Status = providerStatusReady
		runtime.Error = ""
		incomingModels := normalizeProviderCatalogModels(providerID, append([]Model(nil), models...))
		runtime.Models, knownEffortModels = providerAdapterForID(providerID).catalog.Reconcile(runtime.Models, incomingModels, knownEffortModels)
		runtime.Modes = append([]Mode(nil), modes...)
		runtime.AgentName = agentName
	}
	m.mu.Unlock()
}

func (m *Manager) catalogGroup(ctx context.Context, id string) CatalogGroup {
	m.mu.Lock()
	runtime := m.providers[id]
	if runtime == nil {
		m.mu.Unlock()
		return CatalogGroup{ProviderID: id, ProviderName: id, Models: []Model{}, Modes: []Mode{}, Status: providerStatusError, Error: "unknown provider"}
	}
	if runtime.Probed {
		group := runtime.catalogGroupLocked()
		m.mu.Unlock()
		return group
	}
	cfg := runtime.Config
	runtime.Status = providerStatusInactive
	m.mu.Unlock()

	start := time.Now()
	models, modes, agentName, err := m.probeProviderCatalog(ctx, cfg)
	latencyMs := time.Since(start).Milliseconds()
	if err != nil {
		hint, policyErr := m.markProviderNeedsLogin(ctx, id, err)
		if policyErr != nil {
			err = policyErr
		} else if hint != "" {
			m.mu.Lock()
			runtime = m.providers[id]
			if runtime == nil {
				m.mu.Unlock()
				return CatalogGroup{ProviderID: id, ProviderName: id, Models: []Model{}, Modes: []Mode{}, Status: providerStatusError, Error: "unknown provider"}
			}
			group := runtime.catalogGroupLocked()
			m.mu.Unlock()
			return group
		}
	}

	m.mu.Lock()
	runtime = m.providers[id]
	if runtime == nil {
		m.mu.Unlock()
		return CatalogGroup{ProviderID: id, ProviderName: id, Models: []Model{}, Modes: []Mode{}, Status: providerStatusError, Error: "unknown provider"}
	}
	runtime.Probed = true
	runtime.LatencyMs = &latencyMs
	if err != nil {
		runtime.Status = providerStatusError
		runtime.Error = redactSensitiveText(err.Error())
		runtime.Models = nil
		runtime.Modes = nil
	} else {
		runtime.Status = providerStatusReady
		runtime.Error = ""
		runtime.Models = normalizeProviderCatalogModels(id, append([]Model(nil), models...))
		runtime.Modes = append([]Mode(nil), modes...)
		runtime.AgentName = agentName
	}
	group := runtime.catalogGroupLocked()
	m.mu.Unlock()
	return group
}

func (r *providerRuntime) catalogGroupLocked() CatalogGroup {
	status := r.Status
	if status == "" {
		status = providerStatusInactive
	}
	group := CatalogGroup{
		ProviderID:        r.Config.ID,
		ProviderName:      firstNonEmpty(r.Config.Name, r.AgentName, r.Config.Label, r.Config.ID),
		Models:            normalizeProviderCatalogModels(r.Config.ID, append([]Model(nil), r.Models...)),
		Modes:             append([]Mode(nil), r.Modes...),
		PermissionIntents: permissionIntentModes(r.Config.ID, r.Modes),
		AssistantBrand:    providerAdapterForID(r.Config.ID).model.AssistantBrand,
		Status:            status,
		LatencyMs:         r.LatencyMs,
		Error:             r.Error,
		FixHint:           firstNonEmpty(r.FixHint, r.Config.FixHint),
		Badge:             r.Config.Badge,
	}
	if group.Models == nil {
		group.Models = []Model{}
	}
	if group.Modes == nil {
		group.Modes = []Mode{}
	}
	return group
}

func (m *Manager) probeProviderCatalog(ctx context.Context, cfg ProviderConfig) ([]Model, []Mode, string, error) {
	return m.probeProviderCatalogWithInitTimeout(ctx, cfg, m.opts.InitTimeout)
}

func (m *Manager) probeProviderCatalogWithInitTimeout(ctx context.Context, cfg ProviderConfig, initTimeout time.Duration) ([]Model, []Mode, string, error) {
	cfg = normalizeProviderConfig(cfg, m.opts.RootDir, cfg.ID)
	cfg = launchProviderConfig(cfg)
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, nil, "", fmt.Errorf("ACP provider %q has no command", cfg.ID)
	}
	opts := m.opts
	opts.Provider = cfg
	opts.Providers = []ProviderConfig{cfg}
	opts.DefaultProviderID = cfg.ID
	if initTimeout > 0 {
		opts.InitTimeout = initTimeout
	}
	bridge := newBridge("catalog-"+cfg.ID, opts, m)
	timeout := opts.InitTimeout
	if timeout <= 0 {
		timeout = defaultInitTimeout
	}
	catalogCtx, cancel := context.WithTimeout(ctx, timeout*2)
	defer cancel()
	info, err := bridge.NewSession(catalogCtx, SessionOptions{BridgeKey: "catalog-" + cfg.ID, ProviderID: cfg.ID, Ephemeral: true})
	if err != nil {
		bridge.Close(true, err)
		return nil, nil, "", err
	}
	modelPolicy := providerAdapterForID(cfg.ID).model
	if modelPolicy.InspectAllEfforts != nil && modelPolicy.InspectAllEfforts(cfg) {
		m.probeFrontierModelEfforts(catalogCtx, bridge, info.SessionID, info.Models)
		bridge.mu.Lock()
		info.Models = append([]Model(nil), bridge.models...)
		bridge.mu.Unlock()
	}
	_ = bridge.CloseSession(context.Background(), info.SessionID)
	return info.Models, info.Modes, info.Agent, nil
}

// probeFrontierModelEfforts asks an already-open ephemeral catalog session for
// each advertised model's config surface. The direct Claude and Codex hosts
// expose effort as a model-specific option; without this metadata-only pass,
// Workass learns only the startup model's vocabulary. No provider prompt is
// sent and the original model is restored before close.
func (m *Manager) probeFrontierModelEfforts(ctx context.Context, bridge *Bridge, sessionID string, models []Model) {
	bridge.mu.Lock()
	restoreModel := ""
	if bridge.currentModel != nil {
		restoreModel = strings.TrimSpace(*bridge.currentModel)
	}
	bridge.mu.Unlock()

	inspect := func(modelID string) error {
		res, err := bridge.request(ctx, "session/set_config_option", map[string]any{
			"sessionId": sessionID,
			"configId":  "model",
			"value":     modelID,
		}, 15*time.Second)
		if err != nil {
			return err
		}
		bridge.mu.Lock()
		bridge.currentModel = stringValuePtr(modelID)
		bridge.mu.Unlock()
		bridge.applyConfigOptions(res["configOptions"], false)
		return nil
	}

	for _, model := range models {
		modelID := strings.TrimSpace(model.ModelID)
		if modelID == "" || modelID == restoreModel {
			continue
		}
		if err := inspect(modelID); err != nil && m.opts.Logf != nil {
			m.opts.Logf("frontier model effort probe failed", map[string]any{
				"providerId": bridge.ProviderID(),
				"modelId":    modelID,
				"error":      redactSensitiveText(err.Error()),
			})
		}
	}
	if restoreModel == "" {
		return
	}
	if err := inspect(restoreModel); err != nil && m.opts.Logf != nil {
		m.opts.Logf("frontier model effort probe restore failed", map[string]any{
			"providerId": bridge.ProviderID(),
			"modelId":    restoreModel,
			"error":      redactSensitiveText(err.Error()),
		})
	}
}
