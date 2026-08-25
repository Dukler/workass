package acp

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	providercontract "workass/internal/provider"
)

const (
	ProtocolVersion = 1

	defaultInitTimeout              = 60 * time.Second
	defaultPromptReconcileInterval  = 10 * time.Second
	defaultPromptReconcileTimeout   = 5 * time.Second
	defaultPromptTerminalGrace      = 3 * time.Second
	defaultStdoutFlush              = 16 * time.Millisecond
	defaultThoughtFlush             = 24 * time.Millisecond
	defaultStderrTailBytes          = 16 * 1024
	defaultHibernateTTL             = 20 * time.Minute
	defaultRSSSampleInterval        = 30 * time.Second
	defaultEngineMaxAge             = 12 * time.Hour
	defaultEngineMaxRSSKB           = 4 * 1024 * 1024
	defaultSpareTTL                 = 5 * time.Minute
	defaultSpareCheck               = 2500 * time.Millisecond
	defaultProviderUpdateInterval   = time.Hour
	defaultPlanUsageRefreshInterval = 5 * time.Minute
	defaultSpawnedWorkReconcile     = 2 * time.Second
	defaultProviderUpdateTimeout    = 5 * time.Second
	defaultProviderUpdateRunTimeout = 10 * time.Minute
	defaultCompactionThresholdPct   = 80
	defaultCompactionKeepLastTurns  = 4
)

var (
	secretTextRE                          = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+|((?:api[_-]?key|token|secret|password|credential)\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;}]+)`)
	secretKeyRE                           = regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password|credential|bearer)`)
	defaultProviderDetectionRetryBackoffs = []time.Duration{5 * time.Minute, 15 * time.Minute, 30 * time.Minute}
	defaultProviderUpdateRetryBackoffs    = []time.Duration{5 * time.Minute, 15 * time.Minute, 30 * time.Minute}
)

type EngineState string

const (
	StateWarm       EngineState = "warm"
	StateActive     EngineState = "active"
	StateIdle       EngineState = "idle"
	StateHibernated EngineState = "hibernated"
)

// ProviderConfig describes the ACP subprocess to launch.
type ProviderConfig struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Command         string            `json:"command"`
	Args            []string          `json:"args"`
	Env             map[string]string `json:"env,omitempty"`
	AutoEnv         map[string]string `json:"autoEnv,omitempty"`
	Enabled         bool              `json:"enabled"`
	Badge           string            `json:"badge,omitempty"`
	Detected        bool              `json:"detected,omitempty"`
	DetectedAt      string            `json:"detectedAt,omitempty"`
	ResolvedCommand string            `json:"resolvedCommand,omitempty"`
	DisabledByUser  bool              `json:"disabledByUser,omitempty"`
	// NeedsLogin is durable provider readiness, not Workass-owned credentials.
	// It suppresses automatic probing/spawning until an explicit re-enable or a
	// successful explicit provider probe proves the vendor CLI is authenticated.
	NeedsLogin bool   `json:"needsLogin,omitempty"`
	FixHint    string `json:"-"`
	CWD        string `json:"-"`
	Label      string `json:"-"`
	enabledSet bool   `json:"-"`
}

// Options configures a bridge manager. Tests can shorten timeouts here.
type Options struct {
	Provider           ProviderConfig
	Providers          []ProviderConfig
	DefaultProviderID  string
	ProviderConfigFile string
	RootDir            string
	StateDir           string
	// MachineID is the immutable identity of the daemon that owns provider-native
	// threads. Production always supplies it; isolated callers get a deterministic
	// state-directory scope rather than an invented cross-machine identity.
	MachineID string
	// RuntimeProfile is set explicitly by the Workass daemon. Empty is kept
	// unfiltered for package/test callers; the production binary always passes
	// "prod", while isolated development passes "dev" or "test".
	RuntimeProfile string
	// WorkassMCPBaseURL is the daemon-owned HTTPS origin that serves the
	// stateless 2026-07-28 Workass MCP endpoints.
	WorkassMCPBaseURL string
	// WorkassMCPCACertFile is the public certificate providers add to their TLS
	// trust store for the daemon's self-signed, pinned Workass identity. It is
	// never the private key and verification is never disabled.
	WorkassMCPCACertFile string
	// WorkassMCPStdioCommand is the absolute daemon executable used to expose
	// those same endpoints to ACP agents that negotiate MCP over stdio. ACP
	// requires every agent to support stdio; HTTP remains capability-gated.
	WorkassMCPStdioCommand string
	LocalModelEndpoints    []string
	// OMLXSettingsFile overrides the provider-owned oMLX settings location.
	// Production leaves it empty and follows oMLX's own base-path resolution;
	// tests use an isolated file. Workass reads the API key only at probe/launch
	// time and never copies it into ProviderConfig or providers.json.
	OMLXSettingsFile               string
	ProviderDetectionRetryBackoffs []time.Duration
	Version                        string
	InitTimeout                    time.Duration
	PermissionTimeout              time.Duration
	PromptReconcileInterval        time.Duration
	PromptReconcileTimeout         time.Duration
	PromptTerminalGrace            time.Duration
	StdoutFlushInterval            time.Duration
	ThoughtFlushInterval           time.Duration
	StderrTailBytes                int
	HibernateTTL                   time.Duration
	LifecycleCheckInterval         time.Duration
	RSSSampleInterval              time.Duration
	EngineMaxAge                   time.Duration
	EngineMaxRSSKB                 int
	SpareSessions                  int
	// DeferProviderStartup keeps provider-owned session work, including spare
	// prewarming, stopped until the daemon has published its MCP listener.
	// Normal embedders retain the historical eager behavior.
	DeferProviderStartup         bool
	SpareTTL                     time.Duration
	SpareCheckInterval           time.Duration
	ProviderUpdateInterval       time.Duration
	PlanUsageRefreshInterval     time.Duration
	SpawnedWorkReconcileInterval time.Duration
	SpawnedWorkPIDProbe          func(paths []string) (map[string][]int, bool)
	SpawnedWorkListenProbe       func(pids []int) (map[int]bool, bool)
	SpawnedWorkSignal            func(pid int, force bool) bool
	ProviderUpdateRetryBackoffs  []time.Duration
	ProviderUpdateTimeout        time.Duration
	ProviderUpdateSources        map[string]string
	ProviderUpdateRunTimeout     time.Duration
	ProviderUpdateCommands       map[string]ProviderUpdateCommand
	CompactionEnabled            bool
	CompactionThresholdPct       int
	CompactionKeepLastTurns      int
	Broadcast                    func(channel string, payload any)
	Logf                         func(message string, fields map[string]any)
}

func (o Options) withDefaults() Options {
	if o.RootDir == "" {
		if cwd, err := os.Getwd(); err == nil {
			o.RootDir = cwd
		}
	}
	// Durable state is opt-in at the manager boundary. The daemon always passes
	// its explicit --state-dir, while unit tests and embedded callers commonly
	// construct Options{} in the repository cwd. Defaulting those callers to
	// <root>/state let tests write fake native-session bindings into the user's
	// real ledger. An empty StateDir now means ephemeral/no durable recovery.
	o = o.withProviderDefaults()
	if o.ProviderDetectionRetryBackoffs == nil {
		o.ProviderDetectionRetryBackoffs = append([]time.Duration(nil), defaultProviderDetectionRetryBackoffs...)
	} else {
		o.ProviderDetectionRetryBackoffs = positiveDurations(o.ProviderDetectionRetryBackoffs)
	}
	if o.Version == "" {
		o.Version = "0.0.1-dev"
	}
	if o.InitTimeout <= 0 {
		o.InitTimeout = defaultInitTimeout
	}
	// PermissionTimeout has no default on purpose: a card is answered by a
	// person, and Workass does not put its own clock on one. Zero means "no
	// deadline here" — whatever the origin harness enforces is the only one
	// (user 2026-07-25). A caller may still set it explicitly.
	if o.PromptReconcileInterval <= 0 {
		o.PromptReconcileInterval = defaultPromptReconcileInterval
	}
	if o.PromptReconcileTimeout <= 0 {
		o.PromptReconcileTimeout = defaultPromptReconcileTimeout
	}
	if o.PromptTerminalGrace <= 0 {
		o.PromptTerminalGrace = defaultPromptTerminalGrace
	}
	if o.StdoutFlushInterval <= 0 {
		o.StdoutFlushInterval = defaultStdoutFlush
	}
	if o.ThoughtFlushInterval <= 0 {
		o.ThoughtFlushInterval = defaultThoughtFlush
	}
	if o.StderrTailBytes <= 0 {
		o.StderrTailBytes = defaultStderrTailBytes
	}
	if o.HibernateTTL <= 0 {
		o.HibernateTTL = defaultHibernateTTL
	}
	if o.LifecycleCheckInterval <= 0 {
		o.LifecycleCheckInterval = defaultLifecycleCheckInterval(o.HibernateTTL)
	}
	if o.RSSSampleInterval <= 0 {
		o.RSSSampleInterval = defaultRSSSampleInterval
	}
	if o.EngineMaxAge <= 0 {
		o.EngineMaxAge = defaultEngineMaxAge
	}
	if o.EngineMaxRSSKB <= 0 {
		o.EngineMaxRSSKB = defaultEngineMaxRSSKB
	}
	if o.SpareTTL <= 0 {
		o.SpareTTL = defaultSpareTTL
	}
	if o.SpareCheckInterval <= 0 {
		o.SpareCheckInterval = defaultSpareCheck
	}
	if o.ProviderUpdateInterval <= 0 {
		o.ProviderUpdateInterval = defaultProviderUpdateInterval
	}
	if o.PlanUsageRefreshInterval <= 0 {
		o.PlanUsageRefreshInterval = defaultPlanUsageRefreshInterval
	}
	if o.SpawnedWorkReconcileInterval <= 0 {
		o.SpawnedWorkReconcileInterval = defaultSpawnedWorkReconcile
	}
	if o.SpawnedWorkPIDProbe == nil {
		o.SpawnedWorkPIDProbe = spawnedWorkPIDsForOutputs
	}
	if o.SpawnedWorkListenProbe == nil {
		o.SpawnedWorkListenProbe = spawnedWorkListeningPIDs
	}
	if o.SpawnedWorkSignal == nil {
		o.SpawnedWorkSignal = spawnedWorkSignalPID
	}
	if o.ProviderUpdateRetryBackoffs == nil {
		o.ProviderUpdateRetryBackoffs = append([]time.Duration(nil), defaultProviderUpdateRetryBackoffs...)
	} else {
		o.ProviderUpdateRetryBackoffs = positiveDurations(o.ProviderUpdateRetryBackoffs)
	}
	if o.ProviderUpdateTimeout <= 0 {
		o.ProviderUpdateTimeout = defaultProviderUpdateTimeout
	}
	if o.ProviderUpdateSources == nil {
		o.ProviderUpdateSources = defaultProviderUpdateSources()
	} else {
		o.ProviderUpdateSources = copyStringMap(o.ProviderUpdateSources)
	}
	if o.ProviderUpdateRunTimeout <= 0 {
		o.ProviderUpdateRunTimeout = defaultProviderUpdateRunTimeout
	}
	if o.ProviderUpdateCommands == nil {
		o.ProviderUpdateCommands = defaultProviderUpdateCommands()
	} else {
		o.ProviderUpdateCommands = copyProviderUpdateCommands(o.ProviderUpdateCommands)
	}
	if o.CompactionThresholdPct <= 0 {
		o.CompactionThresholdPct = defaultCompactionThresholdPct
	}
	if o.CompactionThresholdPct > 100 {
		o.CompactionThresholdPct = 100
	}
	if o.CompactionKeepLastTurns < 0 {
		o.CompactionKeepLastTurns = 0
	}
	if o.CompactionKeepLastTurns == 0 {
		o.CompactionKeepLastTurns = defaultCompactionKeepLastTurns
	}
	if o.SpareSessions < 0 {
		o.SpareSessions = 0
	}
	if o.SpareSessions > 4 {
		o.SpareSessions = 4
	}
	if o.Broadcast == nil {
		o.Broadcast = func(string, any) {}
	}
	if o.Logf == nil {
		o.Logf = func(string, map[string]any) {}
	}
	return o
}

func positiveDurations(values []time.Duration) []time.Duration {
	out := make([]time.Duration, 0, len(values))
	for _, value := range values {
		if value > 0 {
			out = append(out, value)
		}
	}
	return out
}

func defaultLifecycleCheckInterval(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return 30 * time.Second
	}
	interval := ttl / 4
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	return interval
}

// SessionOptions are the app-chat session creation inputs used by the wire handler.
type SessionOptions struct {
	CWD            string
	WorkspaceEpoch providercontract.WorkspaceEpoch
	BridgeKey      string
	TabID          string
	ChatID         string
	SessionID      string
	// OperationID is the stable Workass user-action identity for a provider
	// lane selection. It is separate from SessionID, which names a disposable
	// transport attachment.
	OperationID   providercontract.OperationID
	ProviderID    string
	ModelID       string
	ModeID        string
	AgentOwnerKey string
	Spare         bool
	Ephemeral     bool
	// ProviderLaneManaged means provider selection is owned by the durable chat
	// actor. A chat may retain multiple provider lanes, so a single-provider
	// chat binding must not reject an explicit lane selection.
	ProviderLaneManaged bool
	// ProviderLaneCreate is the fail-closed create path used by the durable
	// provider-neutral coordinator. It forbids spare adoption, exact-resume of a
	// concurrently established binding, and any second session/new retry.
	ProviderLaneCreate bool
	// ProviderLaneVerifyCandidate is set while executing a persisted create
	// effect for a deferred-creation provider. It permits only exact resume of
	// the stored candidate.
	ProviderLaneVerifyCandidate bool
	// ProviderLaneCreateAfterCandidateAbsence is actor-owned proof that the lane
	// has no provider-native coverage. Only this proof plus authoritative
	// NativeThreadMissing permits removal of the candidate and one session/new.
	ProviderLaneCreateAfterCandidateAbsence bool
}

type ForkOptions struct {
	TabID     string
	NewTabID  string
	ChatID    string
	NewChatID string
	CWD       string
}

// SessionInfo mirrors the AcpSessionInfo wire contract.
type SessionInfo struct {
	SessionID               string               `json:"sessionId"`
	CWD                     string               `json:"cwd"`
	Agent                   string               `json:"agent"`
	ProviderID              string               `json:"providerId,omitempty"`
	ProviderName            string               `json:"providerName,omitempty"`
	ProviderAccountScope    string               `json:"-"`
	ProviderInstallScope    string               `json:"-"`
	ProviderRealmVerified   bool                 `json:"-"`
	Models                  []Model              `json:"models"`
	CurrentModelID          *string              `json:"currentModelId"`
	Modes                   []Mode               `json:"modes"`
	CurrentModeID           *string              `json:"currentModeId"`
	ImageSupport            bool                 `json:"imageSupport"`
	PlanUsageSupported      bool                 `json:"planUsageSupported"`
	PlanUsageResetSupported bool                 `json:"planUsageResetSupported"`
	DeliveryCapabilities    DeliveryCapabilities `json:"deliveryCapabilities"`
	CommandCatalogSupported bool                 `json:"-"`
	// CommandCatalog is the additive Claude commands surface
	// (docs/specs/claude-commands-surface.md). Absent = UNKNOWN (old host or
	// non-claude provider). The actor persists the normalized snapshot.
	CommandCatalog *CommandCatalog `json:"commandCatalog,omitempty"`
}

// DeliveryCapabilities is the additive frozen-wire projection of the provider
// contract's typed capability snapshot. It stays separate from the durable Go
// actor shape so renderer JSON naming cannot silently change actor storage.
type DeliveryCapabilities struct {
	StableInputIdentity     bool `json:"stableInputIdentity"`
	LiveSteer               bool `json:"liveSteer"`
	SteerConsumptionReceipt bool `json:"steerConsumptionReceipt"`
	ConsumptionReceipt      bool `json:"consumptionReceipt"`
	TurnReadback            bool `json:"turnReadback"`
}

func DeliveryCapabilitiesForWire(capabilities providercontract.DeliveryCapabilities) DeliveryCapabilities {
	return DeliveryCapabilities{
		StableInputIdentity:     capabilities.StableInputIdentity,
		LiveSteer:               capabilities.LiveSteer,
		SteerConsumptionReceipt: capabilities.SteerConsumptionReceipt,
		ConsumptionReceipt:      capabilities.ConsumptionReceipt,
		TurnReadback:            capabilities.TurnReadback,
	}
}

// CommandCatalog mirrors the commandCatalog wire object of
// docs/specs/claude-commands-surface.md §2: one shape on every hop (host open
// reply, _workass_claude_commands update, chat:commands event, chat:commands-get
// reply). Memory-only everywhere — never written into session-state.json or
// chat archives. A nil list marshals as null (host did not report that axis —
// UNKNOWN); an empty list is proven empty. Never collapse the two.
type CommandCatalog struct {
	Commands              []CommandCatalogCommand `json:"commands"`
	Agents                []CommandCatalogAgent   `json:"agents"`
	OutputStyle           string                  `json:"outputStyle,omitempty"`
	AvailableOutputStyles []string                `json:"availableOutputStyles"`
	CommandsTruncated     int                     `json:"commandsTruncated"`
	AgentsTruncated       int                     `json:"agentsTruncated"`
	StylesTruncated       int                     `json:"stylesTruncated"`
	AsOf                  int64                   `json:"asOf"`
}

// CommandCatalogCommand mirrors the SDK's SlashCommand (name has no leading
// slash; aliases are display-only — a pick always sends the canonical name).
type CommandCatalogCommand struct {
	Name         string   `json:"name"`
	Description  string   `json:"description,omitempty"`
	ArgumentHint string   `json:"argumentHint,omitempty"`
	Aliases      []string `json:"aliases,omitempty"`
}

// CommandCatalogAgent mirrors the SDK's AgentInfo (model absent = inherits the
// parent's model).
type CommandCatalogAgent struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Model       string `json:"model,omitempty"`
}

// LiveSession is the transient daemon-owned bridge binding returned alongside
// a persisted renderer snapshot. It is never written to session-state.json;
// durable provider-native ids live in the daemon-only native-session ledger.
type LiveSession struct {
	TabID  string      `json:"tabId"`
	ChatID string      `json:"chatId,omitempty"`
	Info   SessionInfo `json:"info"`
}

type Model struct {
	ModelID string   `json:"modelId"`
	Name    string   `json:"name"`
	Efforts []string `json:"efforts,omitempty"`
}

type Mode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type InitResult struct {
	ProtocolVersion int
	AgentName       string
	Raw             map[string]any
}

type PromptResult struct {
	StopReason string
	Raw        map[string]any
	Output     string
}

type JobStartOptions struct {
	// JobID is supplied only by the durable provider-lane actor. It projects the
	// immutable ChatID+OperationID pair onto the frozen public job namespace so
	// admission can be acknowledged before any provider work begins.
	JobID          string
	Kind           string
	Key            any
	Title          string
	PermissionMode string
	ChatID         string
	TabID          string
	SessionID      string
	CWD            string
	Prompt         string
	Message        string
	// InitialContextSeed is actor-authored and appears only on the first real
	// sampling input of a provider lane that has never consumed input.
	InitialContextSeed []providercontract.ContextMessage
	ContextSize        int
	Images             []any
	ModelID            string
	ModeID             string
	ProviderID         string
	// DeliveryCapabilities is the exact typed snapshot of the lane that owns
	// this turn. It is projected additively on the existing job payload so a
	// session attachment cannot lose live-steer authority between admission and
	// the later provider start event.
	DeliveryCapabilities *providercontract.DeliveryCapabilities
	// HumanAuthored separates a request from a resumption. Both can arrive
	// through the same queue, so only the caller knows which this is; getting it
	// wrong would either lose the user's request or invent a new one.
	HumanAuthored bool
	// ProviderLaneManaged fences crash/session recovery. The durable chat actor
	// owns exact resume and readback for these jobs; Manager must never launch
	// any recovery/replacement path for them.
	ProviderLaneManaged bool
	// OperationID is the actor-owned, immutable delivery identity. It is
	// deliberately distinct from UserMessageID, which identifies the visible
	// renderer row. Provider readback, normalized events, and native stable-input
	// receipts use OperationID; the frozen wire projection keeps UserMessageID.
	OperationID        string
	UserMessageID      string
	AssistantMessageID string
	QueueID            string
	PromptText         string
	// BeforeStart runs synchronously under the manager's per-chat lifecycle lock
	// after the atomic busy gate and before the job becomes visible. The daemon
	// uses it to durably prepare canonical turn rows without creating a false
	// failed turn when another start already owns the chat. It is host-internal
	// and never crosses ACP or the frozen wire shape.
	BeforeStart func(*JobStartOptions) error
	// CommitAdmission runs after the public job id is fixed but before the job is
	// registered, published, checkpointed, or sent to the provider. Actor-backed
	// callers use it to durably bind the native turn identity. A failure aborts
	// the start without exposing a job that the actor does not own.
	CommitAdmission func(map[string]any) error
}

type Job struct {
	ID             string
	Kind           string
	Key            any
	Title          string
	Status         string
	StartedAt      string
	FinishedAt     string
	Code           *int
	PermissionMode string
	ChatID         string
	TabID          string
	SessionID      string
	ProviderID     string
	CWD            string
	Result         string
	Error          string
	StopReason     string
	// What this turn ended up saying about the user's REQUEST, after the clamp.
	// Distinct from StopReason, which only says how the model stopped talking:
	// a park and a finished request both end with end_turn.
	DispositionState  string
	DispositionSource string
	DispositionNote   string
	CrashInterrupted  bool
	// A turn the daemon itself ended (restart/handoff) is not an agent error.
	// The failure is real — the turn did stop — but the cause is ours, so the
	// sidebar must not report it as a chat that broke.
	Interrupted bool
	// A Workass-coordinated subagent owns a separate ACP session/engine, but its
	// visible activity belongs to the parent turn. VisibleJobID is that root
	// turn's job id; SubagentID is the stable renderer grouping key.
	VisibleJobID     string
	VisibleSessionID string
	SubagentID       string
	SubagentLabel    string
	SubagentProvider string
	// Friendly model+effort label for the Turnos chip, e.g. "Opus4.8-xhigh"
	// (display name, spaces stripped, joined to effort with a dash). Additive:
	// absent on older daemons → the renderer simply hides the chip.
	SubagentModel   string
	suppressVisible atomic.Bool
	// harnessTurn marks a job adopted for a turn the harness started on its
	// own. Such a job holds no session/prompt RPC, so the ordinary turn-end path
	// never runs for it and it is ended only by the host's turn-ended update.
	harnessTurn bool

	cancelled bool
	// admitting reserves the deterministic public id before the actor admission
	// callback commits. It is manager-private: cancellation may claim the exact
	// reservation, but read projections must not expose it as running until the
	// actor owns the native turn.
	admitting bool
	output    strings.Builder
	internal  bool
	// startOpts is the immutable admission snapshot. Runtime control
	// reconciliation belongs to the provider bridge/native binding; keeping the
	// admitted request immutable lets catalog and subagent readers safely inherit
	// it while the provider turn is running.
	startOpts JobStartOptions
	// actorRecoveryPending is set only when an actor-managed provider host dies.
	// The actor owns exact resume/readback; Manager only cleans up its
	// process-local job and never starts a recovery session.
	actorRecoveryPending  atomic.Bool
	inputDispatched       atomic.Bool
	inputDispatchBoundary chan struct{}
	inputDispatchOnce     sync.Once
	inputConsumed         atomic.Bool
	lastActivityNanos     atomic.Int64
	waitingPermission     atomic.Bool
	consumedSteerIDs      sync.Map // map[string]struct{}

	stdoutBuf   strings.Builder
	stdoutPhase string
	// When the current stdout batch began buffering: flush age is the delay the
	// daemon itself adds between the agent producing text and the renderer
	// seeing it.
	stdoutBufStartedAt time.Time
	thoughtBuf         string
	stdoutTimer        *time.Timer
	thinkTimer         *time.Timer
	// Guarded by Manager.jobMu. A local image Markdown token may span provider
	// chunks, so keep scanning until the newest token is syntactically complete.
	assistantMarkdownPending bool

	assistantImagesMu sync.Mutex
	assistantImages   []any
}

func (j *Job) touchActivity() {
	if j != nil {
		j.lastActivityNanos.Store(time.Now().UnixNano())
	}
}

func (j *Job) inactiveFor(now time.Time) time.Duration {
	if j == nil {
		return 0
	}
	nanos := j.lastActivityNanos.Load()
	if nanos <= 0 {
		return 0
	}
	return now.Sub(time.Unix(0, nanos))
}

func (j *Job) markSteerConsumed(clientUserMessageID string) bool {
	if j == nil {
		return false
	}
	clientUserMessageID = strings.TrimSpace(clientUserMessageID)
	if clientUserMessageID == "" {
		return false
	}
	_, loaded := j.consumedSteerIDs.LoadOrStore(clientUserMessageID, struct{}{})
	return !loaded
}

func (j *Job) claimInputConsumption() bool {
	return j != nil && j.inputConsumed.CompareAndSwap(false, true)
}

func (j *Job) markInputDispatched() {
	if j != nil {
		j.inputDispatched.Store(true)
		j.settleInputDispatch()
	}
}

func (j *Job) settleInputDispatch() {
	if j == nil || j.inputDispatchBoundary == nil {
		return
	}
	j.inputDispatchOnce.Do(func() { close(j.inputDispatchBoundary) })
}

func (j *Job) waitForInputDispatch() bool {
	if j == nil {
		return false
	}
	if j.inputDispatchBoundary != nil {
		<-j.inputDispatchBoundary
	}
	return j.inputWasDispatched()
}

func (j *Job) inputWasDispatched() bool {
	return j != nil && j.inputDispatched.Load()
}

func (j *Job) consumedSteerIDsSnapshot() []string {
	if j == nil {
		return []string{}
	}
	ids := make([]string, 0)
	j.consumedSteerIDs.Range(func(key, _ any) bool {
		if id, ok := key.(string); ok && id != "" {
			ids = append(ids, id)
		}
		return true
	})
	sort.Strings(ids)
	return ids
}

func (j *Job) Public() map[string]any {
	var code any
	if j.Code != nil {
		code = *j.Code
	}
	out := map[string]any{
		"id":                 j.ID,
		"kind":               j.Kind,
		"key":                j.Key,
		"title":              j.Title,
		"status":             j.Status,
		"startedAt":          j.StartedAt,
		"finishedAt":         nullableString(j.FinishedAt),
		"code":               code,
		"permissionMode":     j.PermissionMode,
		"chatId":             nullableString(j.ChatID),
		"tabId":              nullableString(j.TabID),
		"sessionId":          nullableString(j.SessionID),
		"providerId":         nullableString(j.ProviderID),
		"result":             nullableString(j.Result),
		"error":              nullableString(j.Error),
		"stopReason":         nullableString(j.StopReason),
		"crashInterrupted":   j.CrashInterrupted,
		"interrupted":        j.Interrupted,
		"consumedSteerIds":   j.consumedSteerIDsSnapshot(),
		"userMessageId":      nullableString(j.startOpts.UserMessageID),
		"assistantMessageId": nullableString(j.startOpts.AssistantMessageID),
		"promptText":         j.startOpts.PromptText,
	}
	if j.startOpts.DeliveryCapabilities != nil {
		out["deliveryCapabilities"] = DeliveryCapabilitiesForWire(*j.startOpts.DeliveryCapabilities)
	}
	if images := j.assistantImagesSnapshot(); len(images) > 0 {
		out["images"] = images
	}
	// Omitted while unknown, which is what keeps job:start payloads clean —
	// Public() feeds both start (manager.go:925) and end (manager.go:968), and
	// a starting turn has no verdict yet.
	if j.DispositionState != "" {
		disposition := map[string]any{"state": j.DispositionState, "source": j.DispositionSource}
		if strings.TrimSpace(j.DispositionNote) != "" {
			disposition["note"] = redactSensitiveText(j.DispositionNote)
		}
		out["disposition"] = disposition
	}
	return out
}

func (j *Job) addAssistantImages(images []any) []any {
	if j == nil || len(images) == 0 {
		return nil
	}
	j.assistantImagesMu.Lock()
	defer j.assistantImagesMu.Unlock()
	added := make([]any, 0, len(images))
	total := 0
	for _, raw := range j.assistantImages {
		total += len(asString(mapFromAny(raw)["data"]))
	}
	for _, raw := range images {
		if len(j.assistantImages) >= maxToolResultImages || total >= maxToolResultTotalBytes {
			break
		}
		image := mapFromAny(raw)
		mimeType := strings.ToLower(strings.TrimSpace(asString(image["mimeType"])))
		data := strings.TrimSpace(asString(image["data"]))
		source := strings.TrimSpace(asString(image["source"]))
		if !safeToolImageMIME(mimeType) || data == "" || len(data) > maxToolResultImageBytes || total+len(data) > maxToolResultTotalBytes {
			continue
		}
		duplicate := false
		for _, existingRaw := range j.assistantImages {
			existing := mapFromAny(existingRaw)
			existingSource := strings.TrimSpace(asString(existing["source"]))
			if (source != "" && source == existingSource) ||
				(source == "" && existingSource == "" && mimeType == asString(existing["mimeType"]) && data == asString(existing["data"])) {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		copyImage := map[string]any{"mimeType": mimeType, "data": data}
		if name := compactText(asString(image["name"]), 160); name != "" {
			copyImage["name"] = name
		}
		if source != "" {
			copyImage["source"] = source
		}
		j.assistantImages = append(j.assistantImages, copyImage)
		added = append(added, copyImage)
		total += len(data)
	}
	return added
}

func (j *Job) assistantImagesSnapshot() []any {
	if j == nil {
		return nil
	}
	j.assistantImagesMu.Lock()
	defer j.assistantImagesMu.Unlock()
	out := make([]any, 0, len(j.assistantImages))
	for _, raw := range j.assistantImages {
		image := mapFromAny(raw)
		copyImage := make(map[string]any, len(image))
		for key, value := range image {
			copyImage[key] = value
		}
		out = append(out, copyImage)
	}
	return out
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case nil:
		return ""
	default:
		return strings.TrimSpace(strings.Trim(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(toJSON(x)), "\n", " "), "\t", " "), `"`))
	}
}

func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// secretTriggerWords are the literals every secretTextRE alternative requires:
// "bearer\s+…" needs "bearer", and the key:value form needs one of
// api[_-]?key / token / secret / password / credential. "key" stands in for the
// api-key family, so the set over-accepts and never under-accepts.
var secretTriggerWords = [...]string{"bearer", "token", "secret", "password", "credential", "key"}

// mayContainSecret is a conservative prefilter for redactSensitiveText: false
// means secretTextRE provably cannot match, true means "run the regex". RE2 has
// no literal prefilter for this alternation, so it walked every byte of the
// multi-megabyte session mirror — 404 ms per session:save on the user's real
// state, all of it inside the invoke that also stalls later renderer frames.
func mayContainSecret(s string) bool {
	for i := 0; i < len(s); i++ {
		// Only ASCII letters fold into these cases: b/B, t/T, s/S, p/P, c/C,
		// k/K each differ solely in bit 0x20, and no other byte maps onto them.
		switch s[i] | 0x20 {
		case 'b':
			if hasFoldPrefix(s[i:], secretTriggerWords[0]) {
				return true
			}
		case 't':
			if hasFoldPrefix(s[i:], secretTriggerWords[1]) {
				return true
			}
		case 's':
			if hasFoldPrefix(s[i:], secretTriggerWords[2]) {
				return true
			}
		case 'p':
			if hasFoldPrefix(s[i:], secretTriggerWords[3]) {
				return true
			}
		case 'c':
			if hasFoldPrefix(s[i:], secretTriggerWords[4]) {
				return true
			}
		case 'k':
			if hasFoldPrefix(s[i:], secretTriggerWords[5]) {
				return true
			}
		}
	}
	return false
}

// hasFoldPrefix reports whether s starts with the all-lowercase ASCII word,
// compared case-insensitively and without allocating.
func hasFoldPrefix(s, word string) bool {
	if len(s) < len(word) {
		return false
	}
	for i := 0; i < len(word); i++ {
		if s[i]|0x20 != word[i] {
			return false
		}
	}
	return true
}

func redactSensitiveText(s string) string {
	if !mayContainSecret(s) {
		return s
	}
	return secretTextRE.ReplaceAllStringFunc(s, func(match string) string {
		lower := strings.ToLower(match)
		if strings.HasPrefix(lower, "bearer ") {
			return match[:strings.Index(strings.ToLower(match), "bearer ")+7] + "[redacted]"
		}
		if i := strings.IndexAny(match, ":="); i >= 0 {
			return match[:i+1] + "[redacted]"
		}
		return "[redacted]"
	})
}

func RedactSensitiveText(s string) string {
	return redactSensitiveText(s)
}

// MayContainSecret exposes the redaction prefilter so callers walking large
// trees can skip regex work on keys and strings that provably cannot match.
func MayContainSecret(s string) bool { return mayContainSecret(s) }
