package acp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	providercontract "workass/internal/provider"
)

// providerRegistration is the single daemon-side definition of a provider.
// Ordinary ACP providers fill identity/default/discovery parameters and inherit
// every semantic strategy. A native integration overrides only the cohesive
// adapter facets it actually changes.
type providerRegistration struct {
	ID               string
	Name             string
	DefaultCommand   string
	DefaultArgs      []string
	Badge            string
	EnabledByDefault func(rootDir string) bool

	Adapter   providerAdapter
	Discovery *cliProvider
	Detection providerDetectionStrategy

	ProbeTimeout   time.Duration
	FixtureOnly    bool
	Local          bool
	Authentication providercontract.AuthenticationStrategy
	Native         *frontierNativeSpec
	Update         providerUpdateRegistration
}

type providerUpdateRegistration struct {
	Source         string
	Command        ProviderUpdateCommand
	Hint           string
	ResolveCommand func(resolvedCLI string, fallback ProviderUpdateCommand) ProviderUpdateCommand
}

type vendorCLIAuthenticationStrategy struct {
	loginHint string
}

func (strategy vendorCLIAuthenticationStrategy) IsAuthenticationFailure(err error) bool {
	if err == nil {
		return false
	}
	if providercontract.ErrorIs(err, providercontract.ErrorAuthenticationRequired) {
		return true
	}
	return isAuthShapedError(redactSensitiveText(err.Error()))
}

func (strategy vendorCLIAuthenticationStrategy) LoginHint() string {
	return strings.TrimSpace(strategy.loginHint)
}

// noProviderAuthenticationStrategy keeps Authentication mandatory on every
// provider definition without teaching providers that do not own an external
// login flow to classify credential-shaped prose as an auth failure.
type noProviderAuthenticationStrategy struct{}

func (noProviderAuthenticationStrategy) IsAuthenticationFailure(error) bool { return false }
func (noProviderAuthenticationStrategy) LoginHint() string                  { return "" }

type providerDetectionStrategy interface {
	Prepare(context.Context, *Manager, ProviderConfig) (command string, args []string, env map[string]string, inactive string, err error)
}

type mockDetectionStrategy struct{}

func (mockDetectionStrategy) Prepare(_ context.Context, manager *Manager, cfg ProviderConfig) (string, []string, map[string]string, string, error) {
	mockPath := filepath.Join(manager.opts.RootDir, "desktop", "acp", "mock-server.mjs")
	if !fileExists(mockPath) {
		return "", nil, nil, "", fmt.Errorf("mock ACP fixture not found: %s", mockPath)
	}
	node, err := resolveBinary(firstNonEmpty(cfg.Command, "node"), []string{"node.exe", "node"}, nil)
	return node, append([]string(nil), cfg.Args...), nil, "", err
}

type cliDetectionStrategy struct{}

func (cliDetectionStrategy) Prepare(_ context.Context, _ *Manager, cfg ProviderConfig) (string, []string, map[string]string, string, error) {
	resolved, err := resolveProviderExecutable(cfg)
	return resolved, append([]string(nil), cfg.Args...), nil, "", err
}

type qwenDetectionStrategy struct{}

func (qwenDetectionStrategy) Prepare(ctx context.Context, manager *Manager, cfg ProviderConfig) (string, []string, map[string]string, string, error) {
	resolved, err := resolveProviderExecutable(cfg)
	if err != nil {
		return "", nil, nil, "", err
	}
	model, baseURL, err := manager.detectQwenLocalModel(ctx)
	if err != nil {
		return resolved, append([]string(nil), cfg.Args...), nil, err.Error(), nil
	}
	return resolved, append([]string(nil), cfg.Args...), map[string]string{
		"OPENAI_BASE_URL": baseURL,
		"OPENAI_API_KEY":  "local",
		"OPENAI_MODEL":    model,
	}, "", nil
}

type nativeDetectionStrategy struct{}

func (nativeDetectionStrategy) Prepare(_ context.Context, _ *Manager, cfg ProviderConfig) (string, []string, map[string]string, string, error) {
	launch, err := resolveFrontierNativeLaunch(cfg)
	if err != nil {
		return "", nil, nil, "", err
	}
	return launch.Command, launch.Args, nil, "", nil
}

type localModelDetectionStrategy struct{}

func (localModelDetectionStrategy) Prepare(ctx context.Context, manager *Manager, cfg ProviderConfig) (string, []string, map[string]string, string, error) {
	models, baseURL, err := manager.detectLocalModelServer(ctx, cfg.ID)
	if err != nil {
		return "", nil, nil, err.Error(), nil
	}
	launch, err := resolveWorkassAgentLaunch(manager.opts.RootDir)
	if err != nil {
		return "", nil, nil, "", &providerDetectionStatusError{status: providerStatusError, message: err.Error()}
	}
	env := map[string]string{
		"OPENAI_BASE_URL": baseURL,
		"OPENAI_API_KEY":  "local",
		"OPENAI_MODEL":    models[0].ModelID,
	}
	if stateDir := strings.TrimSpace(manager.opts.StateDir); stateDir != "" {
		env["WORKASS_AGENT_SESSION_STORE"] = filepath.Join(stateDir, "provider-native", normalizeProviderID(cfg.ID)+"-sessions.json")
	}
	return launch.Command, launch.Args, env, "", nil
}

func devinKnownPaths() []string {
	home, _ := os.UserHomeDir()
	var known []string
	if home != "" {
		known = append(known,
			filepath.Join(home, "AppData", "Local", "Programs", "Devin", "resources", "app", "extensions", "windsurf", "devin", "bin", "devin.exe"),
			filepath.Join(home, "AppData", "Local", "devin", "cli", "bin", "devin.exe"),
			filepath.Join(home, ".local", "bin", "devin"),
		)
	}
	if runtime.GOOS != "windows" {
		known = append(known, "/opt/homebrew/bin/devin", "/usr/local/bin/devin")
	}
	return known
}

var providerRegistrationOrder = []string{
	"mock", "devin", "qwen", "claude", "codex",
	localLMStudioProviderID, localOllamaProviderID, "custom",
}

var providerRegistrations = map[string]providerRegistration{
	"mock": {
		ID: "mock", Name: "Workass Mock ACP", DefaultCommand: "node", Badge: "dev",
		EnabledByDefault: func(rootDir string) bool {
			return fileExists(filepath.Join(rootDir, "desktop", "acp", "mock-server.mjs"))
		},
		Detection: mockDetectionStrategy{}, FixtureOnly: true,
		Adapter: providerAdapter{launch: mockACPLaunchStrategy{}},
	},
	"devin": {
		ID: "devin", Name: "Devin ACP", DefaultCommand: "devin", DefaultArgs: []string{"acp"}, Badge: "agent",
		Discovery: &cliProvider{id: "devin", defaultCommand: "devin", pathEnv: []string{"WORKASS_DEVIN", "ASSISTANT_DEVIN"}, pathNames: []string{"devin.exe", "devin"}, knownPaths: devinKnownPaths},
		Detection: cliDetectionStrategy{}, ProbeTimeout: devinProbeTimeout,
		Authentication: vendorCLIAuthenticationStrategy{loginHint: "Ejecuta `devin auth login`"},
	},
	"qwen": {
		ID: "qwen", Name: "Qwen Code ACP", DefaultCommand: "qwen", DefaultArgs: []string{"--acp"}, Badge: "agent",
		Discovery: &cliProvider{id: "qwen", defaultCommand: "qwen", pathEnv: []string{"WORKASS_QWEN", "ASSISTANT_QWEN"}, pathNames: []string{"qwen.cmd", "qwen.exe", "qwen"}},
		Detection: qwenDetectionStrategy{},
		Adapter:   providerAdapter{permission: qwenProviderPermissionPolicy{}},
		Update: providerUpdateRegistration{
			Source:  "https://registry.npmjs.org/@qwen-code/qwen-code/latest",
			Command: ProviderUpdateCommand{Command: "qwen", Args: []string{"update"}},
			Hint:    "qwen update", ResolveCommand: resolveQwenUpdateCommand,
		},
	},
	"claude": {
		ID: "claude", Name: "Claude Code", DefaultCommand: "claude", Badge: "native",
		Discovery: &cliProvider{id: "claude", defaultCommand: "claude", pathEnv: []string{"WORKASS_CLAUDE_CODE"}, pathNames: []string{"claude", "claude.exe", "claude.cmd"}},
		Detection: nativeDetectionStrategy{}, ProbeTimeout: frontierProbeTimeout,
		Authentication: vendorCLIAuthenticationStrategy{loginHint: "Ejecuta `claude auth login`"},
		Native:         &frontierNativeSpec{ProviderID: "claude", DefaultCommand: "claude", OverrideEnv: "WORKASS_CLAUDE_CODE", PathNames: []string{"claude", "claude.exe", "claude.cmd"}},
		Adapter: providerAdapter{
			creation:  providercontract.CreationCapabilities{DeferredUntilInput: true},
			input:     explicitHostInputReceiptPolicy{},
			delivery:  claudeDeliveryStrategy{},
			planUsage: claudePlanUsageStrategy{},
			commands:  capabilityCommandCatalogStrategy{capability: "workassClaudeCommandCatalog"},
			model: providerModelPolicy{
				SeparateEffortAxis: true, SyntheticDefaultAlias: true, AssistantBrand: "claude",
				InspectAllEfforts: func(ProviderConfig) bool { return true },
			},
			catalog:  claudeProviderCatalogStrategy{},
			launch:   nativeHostLaunchStrategy{command: "claude", prepare: claudeNativeHostLaunch},
			features: providerFeaturePolicy{NativeSpawnedWork: true},
			context:  staticProviderContextPolicy{capabilities: exactNativeContextCapabilities()},
		},
		Update: providerUpdateRegistration{Source: "https://registry.npmjs.org/@anthropic-ai/claude-code/latest", Command: ProviderUpdateCommand{Command: "claude", Args: []string{"update"}}, Hint: "claude update"},
	},
	"codex": {
		ID: "codex", Name: "Codex", DefaultCommand: "codex", Badge: "native",
		Discovery: &cliProvider{id: "codex", defaultCommand: "codex", pathEnv: []string{"WORKASS_CODEX"}, pathNames: []string{"codex", "codex.exe", "codex.cmd"}},
		Detection: nativeDetectionStrategy{}, ProbeTimeout: frontierProbeTimeout,
		Authentication: vendorCLIAuthenticationStrategy{loginHint: "Ejecuta `codex login`"},
		Native:         &frontierNativeSpec{ProviderID: "codex", DefaultCommand: "codex", OverrideEnv: "WORKASS_CODEX", PathNames: []string{"codex", "codex.exe", "codex.cmd"}},
		Adapter: providerAdapter{
			creation:  providercontract.CreationCapabilities{DeferredUntilInput: true},
			input:     explicitHostInputReceiptPolicy{},
			delivery:  codexDeliveryStrategy{},
			planUsage: codexPlanUsageStrategy{},
			commands:  unsupportedCommandCatalogStrategy{},
			model: providerModelPolicy{
				SeparateEffortAxis: true, AssistantBrand: "gpt",
				InspectAllEfforts: func(config ProviderConfig) bool { return isOfficialNativeCommand(config, "codex") },
			},
			launch:  nativeHostLaunchStrategy{command: "codex", prepare: codexNativeHostLaunch},
			context: staticProviderContextPolicy{capabilities: exactNativeContextCapabilities()},
		},
		Update: providerUpdateRegistration{Source: "https://registry.npmjs.org/@openai/codex/latest", Command: ProviderUpdateCommand{Command: "codex", Args: []string{"update"}}, Hint: "codex update"},
	},
	localLMStudioProviderID: {
		ID: localLMStudioProviderID, Name: "LM Studio (local)", DefaultCommand: "workass-agent", Badge: "native",
		Detection: localModelDetectionStrategy{}, Local: true,
	},
	localOllamaProviderID: {
		ID: localOllamaProviderID, Name: "Ollama (local)", DefaultCommand: "workass-agent", Badge: "native",
		Detection: localModelDetectionStrategy{}, Local: true,
	},
	"custom": {ID: "custom", Name: "Custom ACP", Badge: "custom"},
}

func exactNativeContextCapabilities() providercontract.ContextCapabilities {
	return providercontract.ContextCapabilities{
		ExactResume: true, ImportMode: providercontract.ContextImportUnsupported,
		NativeCompaction: true, VerifiedLineage: true,
	}
}

func providerRegistrationForID(raw string) (providerRegistration, bool) {
	registration, ok := providerRegistrations[normalizeProviderID(raw)]
	return registration, ok
}

func providerAdapterForID(providerID string) providerAdapter {
	if registration, ok := providerRegistrationForID(providerID); ok {
		return providerAdapterWithDefaults(registration.Adapter)
	}
	return genericACPProviderAdapter
}

func builtInProviderConfig(registration providerRegistration, rootDir string) ProviderConfig {
	enabled := false
	if registration.EnabledByDefault != nil {
		enabled = registration.EnabledByDefault(rootDir)
	}
	args := append([]string(nil), registration.DefaultArgs...)
	if args == nil {
		args = []string{}
	}
	return ProviderConfig{
		ID: registration.ID, Name: registration.Name, Command: registration.DefaultCommand,
		Args: args, Enabled: enabled, Badge: registration.Badge, CWD: rootDir,
	}
}

func registeredProviderIDs() []string {
	return append([]string(nil), providerRegistrationOrder...)
}

func providerIsFixture(id string) bool {
	registration, ok := providerRegistrationForID(id)
	return ok && registration.FixtureOnly
}

func defaultFixtureProviderID() string {
	for _, id := range providerRegistrationOrder {
		if providerIsFixture(id) {
			return id
		}
	}
	return ""
}

func defaultNativeSpawnedWorkProviderID() string {
	for _, id := range providerRegistrationOrder {
		if providerAdapterForID(id).features.NativeSpawnedWork && !providerIsFixture(id) {
			return id
		}
	}
	return ""
}

func providerIsLocal(id string) bool {
	registration, ok := providerRegistrationForID(id)
	return ok && registration.Local
}
