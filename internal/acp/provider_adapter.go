package acp

import (
	"os"
	"path/filepath"
	"strings"

	providercontract "workass/internal/provider"
)

// providerAdapter is the only daemon-side registration point for provider
// semantic differences. Chat, lifecycle, persistence, and renderer code consume
// these strategies and never branch on provider branding.
type providerAdapter struct {
	delivery   providerDeliveryStrategy
	context    providerContextPolicy
	creation   providercontract.CreationCapabilities
	input      providerInputReceiptPolicy
	planUsage  providerPlanUsageStrategy
	commands   providerCommandCatalogStrategy
	model      providerModelPolicy
	catalog    providerCatalogStrategy
	permission providerPermissionPolicy
	launch     providerLaunchStrategy
	features   providerFeaturePolicy
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

type providerFeaturePolicy struct {
	NativeSpawnedWork bool
}

type providerLaunchStrategy interface {
	Prepare(ProviderConfig, Options) (ProviderConfig, error)
}

type standardACPLaunchStrategy struct{}

func (standardACPLaunchStrategy) Prepare(config ProviderConfig, _ Options) (ProviderConfig, error) {
	return launchProviderConfig(config), nil
}

type mockACPLaunchStrategy struct{}

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
	delivery:   genericACPDeliveryStrategy{},
	input:      standardACPInputReceiptPolicy{},
	planUsage:  unsupportedPlanUsageStrategy{},
	commands:   unsupportedCommandCatalogStrategy{},
	catalog:    genericProviderCatalogStrategy{},
	permission: genericProviderPermissionPolicy{},
	launch:     standardACPLaunchStrategy{},
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
