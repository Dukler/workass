package acp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	providercontract "workass/internal/provider"
)

// providerAdapter is the only daemon-side registration point for provider
// semantic differences. Chat, lifecycle, persistence, and renderer code consume
// these strategies and never branch on provider branding.
type providerAdapter struct {
	delivery      providerDeliveryStrategy
	context       providerContextPolicy
	creation      providercontract.CreationCapabilities
	input         providerInputReceiptPolicy
	planUsage     providerPlanUsageStrategy
	commands      providerCommandCatalogStrategy
	notifications providerNotificationStrategy
	spawnedWork   providerSpawnedWorkStrategy
	model         providerModelPolicy
	catalog       providerCatalogStrategy
	permission    providerPermissionPolicy
	launch        providerLaunchStrategy
	// subagentEnvironment contains only provider-owned launch settings. The
	// generic bridge adds its provider-neutral owner marker separately.
	subagentEnvironment map[string]string
}

// providerInputReceiptPolicy identifies the transport boundary that proves a
// prompt reached its provider thread. Standard ACP providers prove consumption
// with the first prompt-scoped protocol activity (or the terminal prompt
// reply). Native hosts expose their stronger provider-specific receipt update.
// Chat code consumes the same typed input receipt in either case.
type providerInputReceiptPolicy interface {
	StandardACPActivity() bool
}

type standardACPInputReceiptPolicy struct{}
type explicitHostInputReceiptPolicy struct{}

func (standardACPInputReceiptPolicy) StandardACPActivity() bool  { return true }
func (explicitHostInputReceiptPolicy) StandardACPActivity() bool { return false }

type providerModelPolicy struct {
	SeparateEffortAxis    bool
	SyntheticDefaultAlias bool
	AssistantBrand        string
	InspectAllEfforts     func(ProviderConfig) bool
}

type providerLaunchStrategy interface {
	Prepare(ProviderConfig, Options) (ProviderConfig, error)
	EnvironmentPolicy() providerEnvironmentPolicy
}

// providerEnvironmentPolicy describes how Workass constructs a provider's
// subprocess environment. It is owned by the provider launch adapter so the
// process bridge never branches on provider identity.
type providerEnvironmentPolicy struct {
	blockedKeys          []string
	sanitizationRevision int
}

func (policy providerEnvironmentPolicy) allows(key string) bool {
	key = strings.TrimSpace(key)
	for _, blocked := range policy.blockedKeys {
		if strings.EqualFold(key, strings.TrimSpace(blocked)) {
			return false
		}
	}
	return true
}

// startupRecoveryRevision is opt-in. A positive revision grants a provider
// with a legacy needs-login record exactly one startup probe after its launch
// environment boundary changes. Increment it only when the sanitized child
// environment can invalidate a readiness result produced by an older policy.
func (policy providerEnvironmentPolicy) startupRecoveryRevision() int {
	if policy.sanitizationRevision < 1 {
		return 0
	}
	return policy.sanitizationRevision
}

func providerLaunchSanitizationRevision(providerID string) int {
	return providerAdapterForID(providerID).launch.EnvironmentPolicy().startupRecoveryRevision()
}

type standardACPLaunchStrategy struct {
	environment providerEnvironmentPolicy
}

func (strategy standardACPLaunchStrategy) EnvironmentPolicy() providerEnvironmentPolicy {
	return strategy.environment
}

func (standardACPLaunchStrategy) Prepare(config ProviderConfig, opts Options) (ProviderConfig, error) {
	return launchProviderConfig(config), nil
}

// omlxAwareACPLaunchStrategy keeps oMLX's provider-owned API key ephemeral.
// Detection persists only the harmless "local" placeholder; immediately before
// an oMLX-backed Qwen or workass-agent child starts, this adapter replaces the
// placeholder in the child-only environment.
type omlxAwareACPLaunchStrategy struct {
	environment providerEnvironmentPolicy
}

func (strategy omlxAwareACPLaunchStrategy) EnvironmentPolicy() providerEnvironmentPolicy {
	return strategy.environment
}

func (strategy omlxAwareACPLaunchStrategy) Prepare(config ProviderConfig, opts Options) (ProviderConfig, error) {
	config = launchProviderConfig(config)
	if providerLaunchUsesOMLX(config, opts) {
		apiKey, err := omlxAPIKey(opts)
		if err != nil {
			return ProviderConfig{}, fmt.Errorf("prepare oMLX authentication: %w", err)
		}
		currentKey := strings.TrimSpace(config.Env["OPENAI_API_KEY"])
		if apiKey != "" && (currentKey == "" || currentKey == "local") {
			if config.Env == nil {
				config.Env = make(map[string]string)
			}
			config.Env["OPENAI_API_KEY"] = apiKey
		}
	}
	return (standardACPLaunchStrategy{environment: strategy.environment}).Prepare(config, opts)
}

func providerLaunchUsesOMLX(config ProviderConfig, opts Options) bool {
	if normalizeProviderID(config.ID) == localOMLXProviderID {
		return true
	}
	return openAIBaseURLUsesOMLX(config.Env["OPENAI_BASE_URL"], opts)
}

func openAIBaseURLUsesOMLX(rawBaseURL string, opts Options) bool {
	baseURL := strings.TrimRight(strings.TrimSpace(rawBaseURL), "/")
	if baseURL == "" {
		return false
	}
	for _, server := range localModelServers(opts.LocalModelEndpoints) {
		if server.OMLX && baseURL == strings.TrimRight(openAIBaseURLFromModelsEndpoint(server.Endpoint), "/") {
			return true
		}
	}
	return false
}

const maxNodeExtraCABytes = 4 * 1024 * 1024

type mockACPLaunchStrategy struct{}

func (mockACPLaunchStrategy) EnvironmentPolicy() providerEnvironmentPolicy {
	return providerEnvironmentPolicy{}
}

func (mockACPLaunchStrategy) Prepare(config ProviderConfig, opts Options) (ProviderConfig, error) {
	config = launchProviderConfig(config)
	if strings.TrimSpace(config.Env["WORKASS_MOCK_ACP_SESSION_STORE"]) == "" && strings.TrimSpace(opts.StateDir) != "" {
		if config.Env == nil {
			config.Env = make(map[string]string)
		}
		config.Env["WORKASS_MOCK_ACP_SESSION_STORE"] = filepath.Join(opts.StateDir, "mock-acp-sessions-"+normalizeProviderID(config.ID)+".json")
	}
	return config, nil
}

type nativeHostLaunchStrategy struct {
	command string
	prepare func(ProviderConfig, Options, string) (ProviderConfig, error)
}

func (nativeHostLaunchStrategy) EnvironmentPolicy() providerEnvironmentPolicy {
	return providerEnvironmentPolicy{}
}

func (strategy nativeHostLaunchStrategy) Prepare(config ProviderConfig, opts Options) (ProviderConfig, error) {
	config = launchProviderConfig(config)
	if !isOfficialNativeCommand(config, strategy.command) {
		return config, nil
	}
	executable, _ := os.Executable()
	return strategy.prepare(config, opts, executable)
}

type providerContextPolicy interface {
	Capabilities() providercontract.ContextCapabilities
}

type staticProviderContextPolicy struct {
	capabilities providercontract.ContextCapabilities
}

func (p staticProviderContextPolicy) Capabilities() providercontract.ContextCapabilities {
	return p.capabilities
}

var genericACPProviderAdapter = providerAdapter{
	delivery:      genericACPDeliveryStrategy{},
	input:         standardACPInputReceiptPolicy{},
	planUsage:     unsupportedPlanUsageStrategy{},
	commands:      unsupportedCommandCatalogStrategy{},
	notifications: unsupportedProviderNotificationStrategy{},
	spawnedWork:   unsupportedProviderSpawnedWorkStrategy{},
	catalog:       genericProviderCatalogStrategy{},
	permission:    genericProviderPermissionPolicy{},
	launch:        standardACPLaunchStrategy{},
	context: staticProviderContextPolicy{capabilities: providercontract.ContextCapabilities{
		ExactResume: true,
		ImportMode:  providercontract.ContextImportUnsupported,
	}},
}

func (adapter providerAdapter) negotiatedCreationCapabilities(bridge *Bridge) providercontract.CreationCapabilities {
	capabilities := adapter.creation
	if adapter.input == nil || !adapter.input.StandardACPActivity() || bridge == nil {
		return capabilities
	}
	attachment, ok := bridge.exactSessionAttachment()
	if ok && attachment.method == exactSessionLoad {
		// A load-only ACP server may not expose session/new through session/load
		// until the first prompt activity. Keep that exact id provisional; a
		// resume-capable server's session/new receipt remains immediately durable.
		capabilities.DeferredUntilInput = true
	}
	return capabilities
}

func providerAdapterWithDefaults(adapter providerAdapter) providerAdapter {
	if adapter.delivery == nil {
		adapter.delivery = genericACPProviderAdapter.delivery
	}
	if adapter.context == nil {
		adapter.context = genericACPProviderAdapter.context
	}
	if adapter.input == nil {
		adapter.input = genericACPProviderAdapter.input
	}
	if adapter.planUsage == nil {
		adapter.planUsage = genericACPProviderAdapter.planUsage
	}
	if adapter.commands == nil {
		adapter.commands = genericACPProviderAdapter.commands
	}
	if adapter.notifications == nil {
		adapter.notifications = genericACPProviderAdapter.notifications
	}
	if adapter.spawnedWork == nil {
		adapter.spawnedWork = genericACPProviderAdapter.spawnedWork
	}
	if adapter.catalog == nil {
		adapter.catalog = genericACPProviderAdapter.catalog
	}
	if adapter.permission == nil {
		adapter.permission = genericACPProviderAdapter.permission
	}
	if adapter.launch == nil {
		adapter.launch = genericACPProviderAdapter.launch
	}
	return adapter
}
func (b *Bridge) hasProviderCapability(names ...string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, name := range names {
		if boolMapField(b.agentMeta, name) || boolMapField(b.agentCaps, name) {
			return true
		}
	}
	return false
}
