package acp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	providercontract "workass/internal/provider"
)

type Manager struct {
	opts Options

	mu                      sync.Mutex
	stream                  streamStats
	bridges                 map[string]*Bridge
	sessionBridge           map[string]*Bridge
	sessionProvider         map[string]string
	chatProviders           map[string]string
	nativeSessions          *nativeSessionLedger
	agentOwners             map[string]agentOwnerBinding
	agentOwnerBySession     map[string]string
	providers               map[string]*providerRuntime
	providerOrder           []string
	providerRegistry        *providercontract.Registry
	providerRegistryErr     error
	providerLaneMu          sync.RWMutex
	providerLanesBySession  map[string]*managerLane
	providerLanesByJob      map[string]*managerLane
	providerLaneManagedJobs map[string]struct{}
	// providerLaneClosedJobs retains only immutable ids, never lane/bridge
	// pointers. Late provider callbacks must remain fail-closed for the daemon
	// lifetime, but terminal tombstones must not pin every historical lane.
	providerLaneClosedJobs map[string]struct{}
	// providerPublicationMu spans normalized observation, durable ACK, and the
	// frozen Broadcast call. It is deliberately non-reentrant: a publication
	// callback must not synchronously re-enter Manager.emit, or it could publish
	// a later sequence ahead of the callback that owns this boundary.
	providerPublicationMu       sync.Mutex
	providerAdmissionMu         sync.Mutex
	providerAdmissions          map[string]map[string]any
	providerAdmissionOrder      []string
	providerAttachmentMu        sync.RWMutex
	providerAttachmentResolver  func(context.Context, providercontract.Attachment) (any, error)
	providerAttachmentPersister func([]any) ([]providercontract.Attachment, error)
	defaultProviderID           string
	providerConfigFile          string
	latestProvidersList         any
	hasLatestProvidersList      bool
	latestChatCatalog           any
	hasLatestChatCatalog        bool
	latestProviderUpdates       any
	hasProviderUpdates          bool
	latestAppUpdate             any
	hasAppUpdate                bool
	planUsageByProvider         map[string]PlanUsageSnapshot
	jobs                        map[string]*Job
	finishedJobIDs              map[string]struct{}
	finishedJobOrder            []string
	permissions                 map[string]*permissionResolver
	subagents                   map[string]*SubagentRun
	modelScores                 ModelScores
	providerUpdateRuns          map[string]*providerUpdateRun
	providerUpdateFailures      map[string]providerUpdateFailure
	providerUpdateRechecks      map[string]string
	providerUpdateLastKnown     map[string]ProviderUpdate
	providerUpdateProgress      map[string]ProviderUpdateProgress
	spareSessions               []spareRecord
	spareWarming                map[string]int
	spareBlocked                map[string]bool
	providerStartupReady        bool
	usageBySession              map[string]contextUsage
	// commandCatalogs is the memory-only Claude command-catalog cache, keyed by
	// tab/chat (commandCatalogKey). Never persisted; see command_catalog.go.
	commandCatalogs map[string]*commandCatalogEntry
	spareSeq        int64
	spareGen        int64
	bridgeSeq       int64
	jobSeq          int64
	permSeq         int64
	subagentSeq     int64
	agentOwnerSeq   int64

	jobMu                 sync.Mutex
	jobEndMu              sync.RWMutex
	jobEndFunc            func(tabID, chatID string)
	sessionRefreshMu      sync.RWMutex
	sessionRefreshFunc    func(payload map[string]any)
	updateMu              sync.Mutex
	updateCheckMu         sync.Mutex
	updateCheckRunning    bool
	updateCheckCancel     context.CancelFunc
	updateCheckWG         sync.WaitGroup
	planUsageRefreshMu    sync.Mutex
	planUsageRefreshes    map[string]*planUsageRefreshRun
	planUsageRefreshWG    sync.WaitGroup
	receiptMu             sync.Mutex
	harnessTurns          *harnessTurnStore
	spawnedWorkMu         sync.Mutex
	spawnedWorkCommitMu   sync.Mutex
	spawnedWork           map[string]*spawnedWorkRecord
	spawnedCandidates     map[string]spawnedWorkCandidate
	spawnedWorkObserverMu sync.RWMutex
	spawnedWorkObserver   func(string, string, []SpawnedWorkItem) (SpawnedWorkActorProjection, error)
	resetMu               sync.Mutex
	jobWG                 sync.WaitGroup
	resetting             bool
	loopStop              chan struct{}
	loopStopOnce          sync.Once
	loopWG                sync.WaitGroup
	// updateGateMu is the admission barrier for an app-owned release handoff.
	// A release may begin only after every foreground turn and tracked unit of
	// work is terminal. Once BeginUpdateDrain succeeds, no new work can enter
	// while the shell shuts this daemon down and swaps the complete release.
	updateGateMu     sync.Mutex
	updateDraining   bool
	updateAdmissions int
	// Serializes the short turn-boundary claim for one exact logical chat.
	// Workspace rebinds hold this lock while invalidating/recreating the session;
	// job:start holds it until the running job is registered. Different chats do
	// not block one another.
	chatLifecycle sync.Map // map[tabID+NUL+chatID]*sync.Mutex
	// Old ids invalidated by an explicit workspace move must never be selected
	// for exact native-thread resume in the workspace the provider originally
	// owned.
	workspaceInvalidatedSessions sync.Map // map[sessionID]struct{}

	envMu             sync.Mutex
	envBySession      map[string]*chatEnvTracker
	envByChatID       map[string]*chatEnvTracker
	envByTabID        map[string]*chatEnvTracker
	chatEnvObserverMu sync.RWMutex
	chatEnvObserver   func(ChatEnvPayload) error
	chatEnvRestorerMu sync.RWMutex
	chatEnvRestorer   func(string, string) error

	checkpointMu sync.Mutex
}

type spareRecord struct {
	info          SessionInfo
	bridgeKey     string
	providerID    string
	createdAt     time.Time
	agentOwnerKey string
}

type agentOwnerBinding struct {
	ChatID string
	TabID  string
}

type contextUsage struct {
	Used             int
	Size             int
	UsedPct          int
	InputTokens      any
	OutputTokens     any
	CachedReadTokens any
	UpdatedAt        time.Time
}

type providerSnapshotEvent struct {
	channel string
	payload any
}

type permissionRequest struct {
	JobID             string
	SessionID         string
	ToolCall          map[string]any
	Options           []any
	FallbackOptionID  string
	PermissionTimeout time.Duration
	// A spawned subagent's request. Its card already attaches to the PARENT
	// chat's turn (JobID is the visible job), which is right for a permission the
	// user must grant — and wrong for a question, which belongs to the agent that
	// spawned it. See subagentQuestionOptionID.
	Subagent bool
}

// Returned to a subagent that tried to ask the user: the host turns it into a
// hand-back so the question travels up in the subagent's result, where the
// parent can answer it or ask the user itself.
const subagentQuestionOptionID = "question-subagent"

type permissionResolver struct {
	id        string
	jobID     string
	sessionID string
	payload   map[string]any
	ch        chan string
	once      sync.Once
	timerMu   sync.Mutex
	timer     *time.Timer
}

func NewManager(opts Options) *Manager {
	opts = opts.withDefaults()
	m := &Manager{
		opts:                    opts,
		bridges:                 make(map[string]*Bridge),
		sessionBridge:           make(map[string]*Bridge),
		sessionProvider:         make(map[string]string),
		chatProviders:           make(map[string]string),
		nativeSessions:          newNativeSessionLedger(opts.StateDir, opts.MachineID),
		agentOwners:             make(map[string]agentOwnerBinding),
		agentOwnerBySession:     make(map[string]string),
		providerLanesBySession:  make(map[string]*managerLane),
		providerLanesByJob:      make(map[string]*managerLane),
		providerLaneManagedJobs: make(map[string]struct{}),
		providerLaneClosedJobs:  make(map[string]struct{}),
		providerAdmissions:      make(map[string]map[string]any),
		jobs:                    make(map[string]*Job),
		finishedJobIDs:          make(map[string]struct{}),
		permissions:             make(map[string]*permissionResolver),
		subagents:               make(map[string]*SubagentRun),
		modelScores:             make(ModelScores),
		providerUpdateRuns:      make(map[string]*providerUpdateRun),
		providerUpdateFailures:  make(map[string]providerUpdateFailure),
		providerUpdateRechecks:  make(map[string]string),
		providerUpdateLastKnown: make(map[string]ProviderUpdate),
		providerUpdateProgress:  make(map[string]ProviderUpdateProgress),
		spareWarming:            make(map[string]int),
		spareBlocked:            make(map[string]bool),
		providerStartupReady:    !opts.DeferProviderStartup,
		usageBySession:          make(map[string]contextUsage),
		planUsageByProvider:     make(map[string]PlanUsageSnapshot),
		planUsageRefreshes:      make(map[string]*planUsageRefreshRun),
		harnessTurns:            newHarnessTurnStore(),
		spawnedWork:             make(map[string]*spawnedWorkRecord),
		spawnedCandidates:       make(map[string]spawnedWorkCandidate),
		commandCatalogs:         make(map[string]*commandCatalogEntry),
		loopStop:                make(chan struct{}),
		envBySession:            make(map[string]*chatEnvTracker),
		envByChatID:             make(map[string]*chatEnvTracker),
		envByTabID:              make(map[string]*chatEnvTracker),
	}
	if m.nativeSessions != nil && m.nativeSessions.loadErr != nil {
		m.opts.Logf("native session ledger unreadable; native resume disabled", map[string]any{"error": m.nativeSessions.loadErr.Error()})
	}
	m.initProviders(opts)
	m.initProviderRegistry()
	m.initializePersistedProviderAuthenticationState()
	m.emitMockAppUpdateFromEnv()
	m.loadSpawnedWorkSnapshots()
	m.startManagerLoop(m.lifecycleLoop)
	m.startManagerLoop(m.rssLoop)
	m.startManagerLoop(m.providerUpdateLoop)
	m.startManagerLoop(m.planUsageLoop)
	m.startManagerLoop(m.spawnedWorkLoop)
	m.startManagerLoop(m.mcpFanoutLoop)
	if opts.SpareSessions > 0 && m.providerStartupReady {
		m.WarmSpareSessions()
		m.startManagerLoop(m.spareLoop)
	}
	return m
}

// StartProviderStartup releases provider-owned startup work after the daemon's
// own HTTPS/MCP listener is ready. It is deliberately idempotent so failure
// cleanup and supervised handoffs can safely call the release boundary once.
func (m *Manager) StartProviderStartup() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.providerStartupReady {
		m.mu.Unlock()
		return
	}
	m.providerStartupReady = true
	startSpareLoop := m.opts.SpareSessions > 0
	m.mu.Unlock()
	if startSpareLoop {
		m.WarmSpareSessions()
		m.startManagerLoop(m.spareLoop)
	}
}

func (m *Manager) startManagerLoop(run func()) {
	if m == nil || run == nil {
		return
	}
	m.loopWG.Add(1)
	go func() {
		defer m.loopWG.Done()
		run()
	}()
}

func (m *Manager) SetJobEndFunc(fn func(tabID, chatID string)) {
	if m == nil {
		return
	}
	m.jobEndMu.Lock()
	m.jobEndFunc = fn
	m.jobEndMu.Unlock()
}

// SetSessionRefreshFunc lets the daemon route manager-owned mirror mutations
// through its shared hydration coordinator. Package-local manager tests and
// embedders that do not install the hook retain the original direct event.
func (m *Manager) SetSessionRefreshFunc(fn func(payload map[string]any)) {
	if m == nil {
		return
	}
	m.sessionRefreshMu.Lock()
	m.sessionRefreshFunc = fn
	m.sessionRefreshMu.Unlock()
}

// SetChatEnvObserver installs the actor ingress for actor-managed chats. The
// observer must durably commit the typed Entorno snapshot before returning;
// Manager then publishes no chat:env event for that snapshot. Standalone ACP
// fixtures retain direct publication when no daemon actor runtime is installed.
func (m *Manager) SetChatEnvObserver(fn func(ChatEnvPayload) error) {
	if m == nil {
		return
	}
	m.chatEnvObserverMu.Lock()
	m.chatEnvObserver = fn
	m.chatEnvObserverMu.Unlock()
}

// SetChatEnvRestorer lets the actor runtime rehydrate a previously committed
// Git baseline before Manager initializes a disposable provider attachment.
// It is deliberately a narrow reference restore, not a session/chat lookup.
func (m *Manager) SetChatEnvRestorer(fn func(string, string) error) {
	if m == nil {
		return
	}
	m.chatEnvRestorerMu.Lock()
	m.chatEnvRestorer = fn
	m.chatEnvRestorerMu.Unlock()
}

func (m *Manager) restoreActorChatEnvReference(chatID, tabID string, actorManaged bool) {
	if m == nil || !actorManaged {
		return
	}
	m.chatEnvRestorerMu.RLock()
	restorer := m.chatEnvRestorer
	m.chatEnvRestorerMu.RUnlock()
	if restorer == nil {
		return
	}
	if err := restorer(strings.TrimSpace(chatID), strings.TrimSpace(tabID)); err != nil {
		m.opts.Logf("actor-managed chat:env reference restore failed", map[string]any{
			"chatId": chatID, "tabId": tabID, "error": redactSensitiveText(err.Error()),
		})
	}
}

func (m *Manager) observeChatEnv(payload ChatEnvPayload, actorManaged bool) bool {
	if m == nil || !actorManaged {
		return false
	}
	m.chatEnvObserverMu.RLock()
	observer := m.chatEnvObserver
	m.chatEnvObserverMu.RUnlock()
	if observer == nil {
		m.opts.Logf("actor-managed chat:env suppressed: no actor observer", map[string]any{
			"chatId": payload.ChatID, "tabId": payload.TabID,
		})
		return true
	}
	if err := observer(payload); err != nil {
		m.opts.Logf("actor-managed chat:env rejected before frozen-wire publication", map[string]any{
			"chatId": payload.ChatID, "tabId": payload.TabID, "error": redactSensitiveText(err.Error()),
		})
	}
	return true
}

func (m *Manager) requestSessionRefresh(payload map[string]any) {
	if m == nil {
		return
	}
	m.sessionRefreshMu.RLock()
	fn := m.sessionRefreshFunc
	m.sessionRefreshMu.RUnlock()
	if fn != nil {
		fn(payload)
		return
	}
	m.emit("agent:apply", payload)
}

func (m *Manager) notifyJobEnd(tabID, chatID string) {
	m.jobEndMu.RLock()
	fn := m.jobEndFunc
	m.jobEndMu.RUnlock()
	if fn != nil {
		fn(tabID, chatID)
	}
}

func (m *Manager) BridgeForKey(key string) *Bridge {
	return m.getBridge(SessionOptions{BridgeKey: key})
}

func (m *Manager) StateDir() string {
	return m.opts.StateDir
}

func (m *Manager) InitializeBridge(ctx context.Context, key string) (InitResult, error) {
	bridge := m.BridgeForKey(key)
	if bridge == nil {
		return InitResult{}, errors.New("no enabled ACP provider")
	}
	return bridge.Initialize(ctx)
}

func (m *Manager) NewSession(ctx context.Context, opts SessionOptions) (SessionInfo, error) {
	unlockLifecycle := func() {}
	if strings.TrimSpace(opts.TabID) != "" && strings.TrimSpace(opts.ChatID) != "" && !opts.Spare && !opts.Ephemeral {
		unlockLifecycle = m.lockChatLifecycle(opts.TabID, opts.ChatID)
	}
	defer unlockLifecycle()
	m.mu.Lock()
	if m.resetting {
		m.mu.Unlock()
		return SessionInfo{}, errors.New("ACP manager is resetting")
	}
	if opts.AgentOwnerKey == "" && !opts.Ephemeral {
		opts.AgentOwnerKey = m.newAgentOwnerKeyLocked()
	}
	providerID, err := m.resolveSessionProviderLocked(opts)
	if err != nil {
		m.mu.Unlock()
		return SessionInfo{}, err
	}
	opts.ProviderID = providerID
	m.mu.Unlock()
	opts = m.withNativeSessionControls(opts)

	unlockNative := func() {}
	if m.nativeSessions != nil && m.nativeSessions.enabledFor(opts) {
		unlockNative = m.nativeSessions.lock(opts.TabID, opts.ChatID, providerID, opts.CWD)
	}
	defer unlockNative()
	if opts.ProviderLaneCreate && m.nativeSessions != nil && m.nativeSessions.enabledFor(opts) {
		if _, exists := m.nativeSessions.getForWorkspace(opts.TabID, opts.ChatID, providerID, opts.CWD); exists {
			return SessionInfo{}, nativeLaneError(providercontract.ErrorNativeIdentityConflict, "an established provider lane cannot enter session/new", nil)
		}
	}

	info, bindingFound, restoreErr := m.tryRestoreNativeSession(ctx, opts)
	if bindingFound && restoreErr == nil {
		info, err = m.applySessionStartupControls(ctx, opts, info)
		if err != nil {
			m.CloseSession(context.Background(), info.SessionID)
			return SessionInfo{}, err
		}
		m.mu.Lock()
		m.bindChatProviderLocked(opts, providerID)
		m.bindAgentOwnerLocked(opts.AgentOwnerKey, opts.ChatID, opts.TabID)
		m.mu.Unlock()
		m.initChatEnvForSession(ctx, opts, info)
		m.WarmSpareSessions()
		return info, nil
	}
	if bindingFound && restoreErr != nil {
		if stale := m.getBridge(opts); stale != nil {
			stale.Close(false, restoreErr)
		}
		if hint, policyErr := m.markProviderNeedsLogin(ctx, providerID, restoreErr); policyErr != nil {
			return SessionInfo{}, policyErr
		} else if hint != "" {
			return SessionInfo{}, providerAuthenticationFailureError(providerID, restoreErr, hint)
		}
		return SessionInfo{}, restoreErr
	}
	if !opts.ProviderLaneCreate {
		if info, ok := m.adoptSpareSession(opts); ok {
			info, err = m.applySessionStartupControls(ctx, opts, info)
			if err != nil {
				m.CloseSession(context.Background(), info.SessionID)
				return SessionInfo{}, err
			}
			if err := m.rememberNewNativeSession(opts, info); err != nil {
				m.CloseSession(context.Background(), info.SessionID)
				return SessionInfo{}, err
			}
			m.mu.Lock()
			m.bindChatProviderLocked(opts, providerID)
			m.bindAgentOwnerLocked(m.agentOwnerBySession[info.SessionID], opts.ChatID, opts.TabID)
			m.mu.Unlock()
			m.initChatEnvForSession(ctx, opts, info)
			m.WarmSpareSessions()
			return info, nil
		}
	}
	bridge := m.getBridge(opts)
	if bridge == nil {
		if _, configErr := m.providerConfig(providerID); configErr != nil {
			return SessionInfo{}, configErr
		}
		return SessionInfo{}, fmt.Errorf("ACP provider bridge is unavailable: %s", providerID)
	}
	info, err = bridge.NewSession(ctx, opts)
	if err == nil {
		if !opts.Spare {
			info, err = m.applySessionStartupControls(ctx, opts, info)
			if err != nil {
				bridge.CloseSession(context.Background(), info.SessionID)
				return SessionInfo{}, err
			}
			if persistErr := m.rememberNewNativeSession(opts, info); persistErr != nil {
				bridge.CloseSession(context.Background(), info.SessionID)
				return SessionInfo{}, persistErr
			}
			m.mu.Lock()
			m.bindChatProviderLocked(opts, providerID)
			m.bindAgentOwnerLocked(opts.AgentOwnerKey, opts.ChatID, opts.TabID)
			m.mu.Unlock()
			m.initChatEnvForSession(ctx, opts, info)
			m.WarmSpareSessions()
		}
		return info, nil
	}
	hint, policyErr := m.markProviderNeedsLogin(ctx, providerID, err)
	if policyErr != nil {
		bridge.Close(false, err)
		return SessionInfo{}, policyErr
	}
	if hint != "" {
		bridge.Close(false, err)
		return SessionInfo{}, providerAuthenticationFailureError(providerID, err, hint)
	}
	if opts.ProviderLaneCreate {
		bridge.Close(false, err)
		return SessionInfo{}, nativeLaneError(
			providercontract.ErrorAcceptanceAmbiguous,
			"provider thread creation did not produce a durable acceptance receipt; Workass will not issue another create",
			err,
		)
	}
	bridge.Close(false, err)
	bridge = m.replaceBridge(opts)
	if bridge == nil {
		if _, configErr := m.providerConfig(providerID); configErr != nil {
			return SessionInfo{}, configErr
		}
		return SessionInfo{}, fmt.Errorf("ACP provider became unavailable after session failure: %s", providerID)
	}
	info, err = bridge.NewSession(ctx, opts)
	if err == nil && !opts.Spare {
		info, err = m.applySessionStartupControls(ctx, opts, info)
		if err != nil {
			bridge.CloseSession(context.Background(), info.SessionID)
			return SessionInfo{}, err
		}
		if persistErr := m.rememberNewNativeSession(opts, info); persistErr != nil {
			bridge.CloseSession(context.Background(), info.SessionID)
			return SessionInfo{}, persistErr
		}
		m.mu.Lock()
		m.bindChatProviderLocked(opts, providerID)
		m.bindAgentOwnerLocked(opts.AgentOwnerKey, opts.ChatID, opts.TabID)
		m.mu.Unlock()
		m.initChatEnvForSession(ctx, opts, info)
		m.WarmSpareSessions()
	}
	return info, err
}

func (m *Manager) withNativeSessionControls(opts SessionOptions) SessionOptions {
	if m == nil || m.nativeSessions == nil || !m.nativeSessions.enabledFor(opts) {
		return opts
	}
	binding, ok := m.nativeSessions.getForWorkspace(opts.TabID, opts.ChatID, opts.ProviderID, opts.CWD)
	if !ok {
		return opts
	}
	if strings.TrimSpace(opts.ModelID) == "" {
		opts.ModelID = binding.ModelID
	}
	if strings.TrimSpace(opts.ModeID) == "" {
		opts.ModeID = binding.ModeID
	}
	return opts
}

// applySessionStartupControls is BEST-EFFORT by design: a stored selection
// that cannot be applied (unknown id, adapter rejection, fixture in prod)
// must degrade to the adapter default with a log line, never prevent the
// session from opening — a chat on the wrong model is recoverable, a chat
// that cannot open a session is not. The store is never written here, so a
// skipped apply cannot degrade the durable selection (fix-2 invariant).
func (m *Manager) applySessionStartupControls(ctx context.Context, opts SessionOptions, info SessionInfo) (SessionInfo, error) {
	skip := func(reason string, err error) (SessionInfo, error) {
		errMsg := ""
		if err != nil {
			errMsg = redactSensitiveText(err.Error())
		}
		m.opts.Logf("startup control apply skipped", map[string]any{
			"tabID": opts.TabID, "providerID": info.ProviderID, "reason": reason, "error": errMsg,
		})
		// A chat silently running on the wrong model violates the receipts
		// law — the log line above was the only witness (2026-07-27: fable[max]
		// configured, opus applied, nobody knew). Additive action; renderers
		// that predate it ignore it.
		m.requestSessionRefresh(map[string]any{
			"action": "session-controls-skipped",
			"tabId":  opts.TabID, "chatId": opts.ChatID, "sessionId": info.SessionID,
			"providerId":       info.ProviderID,
			"requestedModelId": strings.TrimSpace(opts.ModelID),
			"requestedModeId":  strings.TrimSpace(opts.ModeID),
			"reason":           reason, "error": errMsg,
		})
		return info, nil
	}
	modelID := strings.TrimSpace(opts.ModelID)
	modeID := strings.TrimSpace(opts.ModeID)
	if modelID == "" && modeID == "" {
		return info, nil
	}
	if modelID != "" {
		if err := m.validateProductionModelSelection(info.ProviderID, modelID); err != nil {
			return skip("production-visibility", err)
		}
	}
	bridge := m.bridgeForSession(info.SessionID, SessionOptions{
		TabID: opts.TabID, ChatID: opts.ChatID, SessionID: info.SessionID, ProviderID: info.ProviderID,
	})
	if bridge == nil {
		return skip("no-bridge", nil)
	}
	controls, err := bridge.ensureSessionControls(ctx, info.SessionID, modelID, modeID)
	if err != nil {
		hint, policyErr := m.markProviderNeedsLogin(ctx, info.ProviderID, err)
		if policyErr != nil {
			return info, policyErr
		}
		if hint != "" {
			return info, providerAuthenticationFailureError(info.ProviderID, err, hint)
		}
		return skip("apply-failed", err)
	}
	if modelID != "" && controls.CurrentModelID != "" {
		current := controls.CurrentModelID
		bridge.rememberDurableModelSelection(info.SessionID, current)
		info.CurrentModelID = &current
		if m.nativeSessions != nil {
			m.nativeSessions.updateControls(opts.TabID, opts.ChatID, info.ProviderID, info.SessionID, current, "")
		}
	}
	if live, ok := bridge.liveSession(info.SessionID); ok {
		info.Models = live.Info.Models
		info.Modes = live.Info.Modes
		info.CurrentModeID = live.Info.CurrentModeID
		info.ImageSupport = live.Info.ImageSupport
		if info.CurrentModelID == nil {
			info.CurrentModelID = live.Info.CurrentModelID
		}
	}
	if modeID != "" && info.CurrentModeID == nil {
		currentMode := modeID
		info.CurrentModeID = &currentMode
	}
	if modeID != "" && m.nativeSessions != nil {
		m.nativeSessions.updateControls(opts.TabID, opts.ChatID, info.ProviderID, info.SessionID, "", modeID)
	}
	return info, nil
}

func (m *Manager) lockChatLifecycle(tabID, chatID string) func() {
	key := strings.TrimSpace(tabID) + "\x00" + strings.TrimSpace(chatID)
	raw, _ := m.chatLifecycle.LoadOrStore(key, &sync.Mutex{})
	mu := raw.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// InvalidateChatWorkspace moves one initialized logical chat to a different
// workspace at an idle turn boundary. Provider-native threads commonly retain
// their original cwd even when resume accepts another one, so the current
// implementation closes the old epoch before a new lane is created at the
// target cwd. Workass transcript text is never replayed into that lane.
//
// commitTarget runs while the exact chat lifecycle lock is held and before the
// old sessions are invalidated. It durably records the requested workspace; a
// crash after that commit therefore cannot resume execution under the old
// directory.
func (m *Manager) InvalidateChatWorkspace(
	ctx context.Context,
	oldSessionID string,
	opts SessionOptions,
	commitTarget func() error,
) (bool, error) {
	oldSessionID = strings.TrimSpace(oldSessionID)
	opts.TabID = strings.TrimSpace(opts.TabID)
	opts.ChatID = strings.TrimSpace(opts.ChatID)
	opts.CWD = strings.TrimSpace(opts.CWD)
	if opts.TabID == "" || opts.ChatID == "" || opts.CWD == "" {
		return false, errors.New("workspace rebind requires tabId, chatId, and cwd")
	}
	unlock := m.lockChatLifecycle(opts.TabID, opts.ChatID)
	defer unlock()

	liveSessions := m.liveSessionsForChat(opts.TabID, opts.ChatID)
	if oldSessionID != "" {
		owned := false
		for _, live := range liveSessions {
			if live.Info.SessionID == oldSessionID {
				owned = true
				break
			}
		}
		if !owned && m.nativeSessions != nil {
			owned = m.nativeSessions.ownsSession(opts.TabID, opts.ChatID, oldSessionID)
		}
		if !owned {
			return false, errors.New("La sesión ACP ya no pertenece a esta conversación; recargá antes de moverla.")
		}
	} else if len(liveSessions) > 0 {
		return false, errors.New("La conversación todavía tiene una sesión ACP; recargá antes de moverla.")
	}
	m.mu.Lock()
	for _, job := range m.jobs {
		if job != nil && job.Status == "running" && job.TabID == opts.TabID && job.ChatID == opts.ChatID {
			m.mu.Unlock()
			return false, errors.New("No se puede mover la conversación mientras hay una respuesta en curso.")
		}
	}
	m.mu.Unlock()
	if commitTarget != nil {
		if err := commitTarget(); err != nil {
			return false, fmt.Errorf("persist target workspace: %w", err)
		}
	}
	for _, live := range liveSessions {
		m.workspaceInvalidatedSessions.Store(live.Info.SessionID, struct{}{})
		m.CloseSession(ctx, live.Info.SessionID)
	}
	if oldSessionID != "" {
		m.workspaceInvalidatedSessions.Store(oldSessionID, struct{}{})
	}
	return true, nil
}

func (m *Manager) liveSessionsForChat(tabID, chatID string) []LiveSession {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	m.mu.Lock()
	bindings := make(map[string]*Bridge, len(m.sessionBridge))
	for sessionID, bridge := range m.sessionBridge {
		bindings[sessionID] = bridge
	}
	m.mu.Unlock()
	out := make([]LiveSession, 0)
	for sessionID, bridge := range bindings {
		live, ok := bridge.liveSession(sessionID)
		if ok && live.TabID == tabID && live.ChatID == chatID {
			out = append(out, live)
		}
	}
	return out
}

func (m *Manager) ForkSession(ctx context.Context, opts ForkOptions) (SessionInfo, error) {
	source := SessionOptions{TabID: opts.TabID, ChatID: opts.ChatID}
	m.mu.Lock()
	providerID := m.boundProviderForChatLocked(source)
	m.mu.Unlock()
	if providerID == "" {
		return SessionInfo{}, fmt.Errorf("source chat has no bound ACP provider: %s", opts.TabID)
	}
	return m.NewSession(ctx, SessionOptions{
		CWD:        opts.CWD,
		TabID:      opts.NewTabID,
		ChatID:     opts.NewChatID,
		ProviderID: providerID,
	})
}

func (m *Manager) CloseSession(ctx context.Context, sessionID string) bool {
	bridge := m.bridgeForSession(sessionID, SessionOptions{SessionID: sessionID})
	if bridge == nil {
		if lane := m.providerLaneForSessionID(sessionID); lane != nil {
			lane.attachmentClosed()
		}
		return false
	}
	closed := bridge.CloseSession(ctx, sessionID)
	if closed {
		if lane := m.providerLaneForSessionID(sessionID); lane != nil {
			lane.attachmentClosed()
		}
		m.forgetChatEnvSession(sessionID)
	}
	return closed
}

// CloseSessionAndForget is the explicit chat-close operation. Lifecycle reap,
// compaction, and provider handover keep the durable native binding; deleting a
// chat removes it so a recycled tab id can never reopen the old provider thread.
func (m *Manager) CloseSessionAndForget(ctx context.Context, sessionID string) bool {
	live, hasLive := m.LiveSession(sessionID)
	if hasLive {
		m.cancelAndDrainSubagentsForChat(live.ChatID, live.TabID, 5*time.Second)
	}
	closed := m.CloseSession(ctx, sessionID)
	if closed && hasLive && m.nativeSessions != nil {
		m.nativeSessions.delete(live.TabID, live.ChatID, live.Info.ProviderID, sessionID)
	}
	return closed
}

// ForgetChat is the exact logical-chat delete boundary. It closes every live
// provider bridge and removes every hibernated native provider binding for the
// immutable tab+conversation pair; deleting only the currently live session
// would let an older provider thread reappear later.
func (m *Manager) ForgetChat(ctx context.Context, tabID, chatID string, operationID providercontract.OperationID) bool {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	operationID = providercontract.NormalizeOperationID(string(operationID))
	if tabID == "" || chatID == "" || operationID == "" {
		return false
	}
	m.cancelAndDrainSubagentsForChat(chatID, tabID, 5*time.Second)
	m.DropSpawnedWorkForChat(tabID, chatID)
	changed := false
	for _, live := range m.LiveSessions() {
		if live.TabID == tabID && live.ChatID == chatID {
			changed = m.CloseSession(ctx, live.Info.SessionID) || changed
		}
	}
	if m.nativeSessions != nil {
		m.nativeSessions.deleteChat(tabID, chatID)
		changed = true
	}
	m.mu.Lock()
	for _, key := range chatProviderKeys(SessionOptions{TabID: tabID, ChatID: chatID}) {
		delete(m.chatProviders, key)
	}
	m.forgetCommandCatalogChatLocked(tabID)
	m.mu.Unlock()
	return changed
}

func (m *Manager) Reset() bool {
	m.resetMu.Lock()
	defer m.resetMu.Unlock()
	m.loopStopOnce.Do(func() { close(m.loopStop) })
	m.loopWG.Wait()
	m.mu.Lock()
	m.resetting = true
	m.mu.Unlock()
	m.stopScheduledProviderUpdateCheck()
	m.stopPlanUsageRefreshes()
	m.cancelAllSubagents(5 * time.Second)
	m.killAllProviderUpdates()
	m.mu.Lock()
	bridges := make([]*Bridge, 0, len(m.bridges))
	for _, bridge := range m.bridges {
		bridges = append(bridges, bridge)
	}
	m.bridges = make(map[string]*Bridge)
	m.sessionBridge = make(map[string]*Bridge)
	m.sessionProvider = make(map[string]string)
	m.chatProviders = make(map[string]string)
	m.agentOwners = make(map[string]agentOwnerBinding)
	m.agentOwnerBySession = make(map[string]string)
	m.permissions = make(map[string]*permissionResolver)
	m.subagents = make(map[string]*SubagentRun)
	m.spareGen++
	m.spareSessions = nil
	m.spareWarming = make(map[string]int)
	m.usageBySession = make(map[string]contextUsage)
	m.planUsageByProvider = make(map[string]PlanUsageSnapshot)
	m.commandCatalogs = make(map[string]*commandCatalogEntry)
	m.mu.Unlock()
	m.spawnedWorkMu.Lock()
	m.spawnedWork = make(map[string]*spawnedWorkRecord)
	m.spawnedCandidates = make(map[string]spawnedWorkCandidate)
	m.spawnedWorkMu.Unlock()
	m.providerLaneMu.Lock()
	m.providerLanesBySession = make(map[string]*managerLane)
	m.providerLanesByJob = make(map[string]*managerLane)
	m.providerLaneManagedJobs = make(map[string]struct{})
	m.providerLaneClosedJobs = make(map[string]struct{})
	m.providerLaneMu.Unlock()
	m.resetChatEnv()
	for _, bridge := range bridges {
		bridge.Close(true, errors.New("ACP session reset"))
	}
	// Bridge close unblocks active session/prompt calls. Wait until every app-chat
	// worker has finished its durability-first native cursor/receipt cleanup before
	// returning; otherwise a test teardown or daemon handoff can remove StateDir
	// while a late worker recreates provider-lanes.json behind it.
	m.jobWG.Wait()
	m.mu.Lock()
	m.resetting = false
	m.mu.Unlock()
	return true
}

func (m *Manager) SetModel(ctx context.Context, sessionID, modelID string) (map[string]any, error) {
	bridge := m.bridgeForSession(sessionID, SessionOptions{SessionID: sessionID})
	if bridge == nil {
		return nil, errors.New("ACP session not found")
	}
	if err := m.validateProductionModelSelection(bridge.ProviderID(), modelID); err != nil {
		return nil, err
	}
	result, err := bridge.SetModel(ctx, sessionID, modelID)
	if err == nil && m.nativeSessions != nil {
		if live, ok := bridge.liveSession(sessionID); ok {
			persistedModelID := durableModelIDFromSetResult(result, modelID)
			if persistedModelID != "" {
				m.nativeSessions.updateControls(live.TabID, live.ChatID, live.Info.ProviderID, sessionID, persistedModelID, "")
			}
		}
	}
	return result, err
}

func durableModelIDFromSetResult(result map[string]any, fallback string) string {
	modelID := strings.TrimSpace(asString(result["currentModelId"]))
	if modelID != "" {
		return modelID
	}
	return strings.TrimSpace(fallback)
}

func appliedModelIDFromSetResult(result map[string]any, fallback string) string {
	modelID := strings.TrimSpace(asString(result["appliedModelId"]))
	if modelID != "" {
		return modelID
	}
	modelID = strings.TrimSpace(asString(result["currentModelId"]))
	if modelID != "" {
		return modelID
	}
	return strings.TrimSpace(fallback)
}

func (m *Manager) SetMode(ctx context.Context, sessionID, modeID string) (map[string]any, error) {
	bridge := m.bridgeForSession(sessionID, SessionOptions{SessionID: sessionID})
	if bridge == nil {
		return nil, errors.New("ACP session not found")
	}
	result, err := bridge.SetMode(ctx, sessionID, modeID)
	if err == nil && m.nativeSessions != nil {
		if live, ok := bridge.liveSession(sessionID); ok {
			m.nativeSessions.updateControls(live.TabID, live.ChatID, live.Info.ProviderID, sessionID, "", modeID)
		}
	}
	return result, err
}

func (m *Manager) captureAdapterModelSelection(bridge *Bridge, sessionID, modelID string) {
	modelID = strings.TrimSpace(modelID)
	if bridge == nil || modelID == "" {
		return
	}
	live, ok := bridge.liveSession(sessionID)
	if !ok {
		return
	}
	if m.nativeSessions != nil {
		m.nativeSessions.updateControls(live.TabID, live.ChatID, live.Info.ProviderID, sessionID, modelID, "")
	}
	m.opts.Logf("captured adapter model selection", map[string]any{
		"tabID": live.TabID, "chatID": live.ChatID, "providerID": live.Info.ProviderID, "sessionID": sessionID, "modelID": modelID,
	})
	m.requestSessionRefresh(map[string]any{
		"action": "session-refresh", "tabId": live.TabID, "chatId": live.ChatID, "sessionId": sessionID,
		"providerId": live.Info.ProviderID, "modelId": modelID, "currentModelId": modelID,
	})
}

func (m *Manager) Steer(sessionID, promptText string, images []any, clientUserMessageID string) map[string]any {
	bridge := m.bridgeForSession(sessionID, SessionOptions{SessionID: sessionID})
	if bridge == nil {
		return map[string]any{"ok": false, "queued": false, "error": "La sesion ACP expiro. Cerra y reabri la pestana del chat."}
	}
	return bridge.Steer(sessionID, promptText, images, clientUserMessageID)
}

// ErrChatBusy is returned before JobStartOptions.BeforeStart runs. Callers that
// advertise a durable busy-start capability may therefore transfer the prompt
// into FIFO without first manufacturing a failed canonical transcript turn.
var ErrChatBusy = errors.New("Ya hay una respuesta en curso en esta conversación.")

// ErrUpdateDraining is deliberately distinct from ErrChatBusy: the renderer
// may retry a busy chat into its durable FIFO, but it must never enqueue a new
// turn after the daemon has granted an atomic app-update handoff.
var ErrUpdateDraining = errors.New("Workass is preparing a verified app update; try again after it restarts.")

func (m *Manager) beginWorkAdmission() error {
	m.updateGateMu.Lock()
	defer m.updateGateMu.Unlock()
	if m.updateDraining {
		return ErrUpdateDraining
	}
	m.updateAdmissions++
	return nil
}

func (m *Manager) endWorkAdmission() {
	m.updateGateMu.Lock()
	if m.updateAdmissions > 0 {
		m.updateAdmissions--
	}
	m.updateGateMu.Unlock()
}

func (m *Manager) StartJob(ctx context.Context, opts JobStartOptions) (map[string]any, error) {
	if err := m.beginWorkAdmission(); err != nil {
		return nil, err
	}
	defer m.endWorkAdmission()
	if opts.Kind != "app-chat" {
		return nil, errors.New("not implemented until P2")
	}
	if opts.SessionID == "" {
		return nil, errors.New("Falta la sesión ACP del chat (abrí una pestaña).")
	}
	unlockLifecycle := m.lockChatLifecycle(opts.TabID, opts.ChatID)
	defer unlockLifecycle()
	if _, invalidated := m.workspaceInvalidatedSessions.Load(opts.SessionID); invalidated {
		return nil, errors.New("La sesión ACP fue invalidada al mover el chat; reintentá con el estado actualizado.")
	}
	// A connection id is never authority to retarget a turn. Reject a caller
	// that presents another chat's still-live attachment before bridge-key
	// resolution can select the requested chat's process instead.
	if live, ok := m.LiveSession(opts.SessionID); ok && !liveSessionMatchesOptions(live, SessionOptions{TabID: opts.TabID, ChatID: opts.ChatID}) {
		m.opts.Logf("rejected cross-chat ACP session binding", map[string]any{
			"requestedTabId": opts.TabID, "requestedChatId": opts.ChatID,
			"boundTabId": live.TabID, "boundChatId": live.ChatID,
		})
		return nil, errors.New("La sesión ACP pertenece a otra conversación; se rechazó para proteger el contexto.")
	}
	bridge := m.bridgeForSession(opts.SessionID, SessionOptions{TabID: opts.TabID, ChatID: opts.ChatID, SessionID: opts.SessionID})
	if bridge == nil {
		return nil, errors.New("La sesión ACP expiró. Cerrá y reabrí la pestaña del chat.")
	}
	if live, ok := bridge.liveSession(opts.SessionID); ok && !liveSessionMatchesOptions(live, SessionOptions{TabID: opts.TabID, ChatID: opts.ChatID}) {
		m.opts.Logf("rejected cross-chat ACP session binding", map[string]any{
			"requestedTabId": opts.TabID, "requestedChatId": opts.ChatID,
			"boundTabId": live.TabID, "boundChatId": live.ChatID,
		})
		return nil, errors.New("La sesión ACP pertenece a otra conversación; se rechazó para proteger el contexto.")
	}
	providerID := bridge.ProviderID()
	if _, err := m.providerConfig(providerID); err != nil {
		return nil, err
	}
	targetProviderID := providerID
	if requested := normalizeProviderID(opts.ProviderID); requested != "" {
		targetProviderID = requested
	}
	startupControls := m.withNativeSessionControls(SessionOptions{
		TabID: opts.TabID, ChatID: opts.ChatID, SessionID: opts.SessionID, ProviderID: targetProviderID,
		ModelID: opts.ModelID, ModeID: opts.ModeID,
	})
	opts.ModelID = startupControls.ModelID
	opts.ModeID = startupControls.ModeID
	if err := m.validateProductionModelSelection(targetProviderID, opts.ModelID); err != nil {
		return nil, err
	}
	liveSession := bridge.hasLiveSession(opts.SessionID)
	// The Manager is the provider transport, not a chat recovery coordinator.
	// Actor-managed admissions remain durable across the narrow race where a
	// host dies after selection; every other caller must attach/resume first.
	if !liveSession && !opts.ProviderLaneManaged {
		return nil, errors.New("La conexión ACP no está activa; el actor durable debe reanudar exactamente el hilo nativo antes de enviar.")
	}
	if _, running := m.RunningJobForChat(opts.TabID, opts.ChatID); running {
		return nil, ErrChatBusy
	}
	if existing := bridge.jobForSession(opts.SessionID); liveSession && existing != nil && existing.Status == "running" {
		return nil, ErrChatBusy
	}
	// Switching providers is a lane transaction, not a session replacement.
	// Until the lane coordinator can create/restore the target provider lane and
	// verify a non-sampling context import, fail before detaching the active lane.
	if opts.ProviderID != "" && opts.ProviderID != providerID {
		return nil, &providercontract.Error{
			Kind:      providercontract.ErrorUnsupportedCapability,
			Operation: providercontract.OperationID("switch-provider-lane"),
			Message:   "provider switching is blocked until the target lane can import context without transcript replay",
		}
	}
	// Carry the resolved provider into the durable job input. Once a host exits,
	// sessionProvider is intentionally gone; recovery must use the immutable lane
	// identity captured at admission rather than guess the default provider.
	opts.ProviderID = providerID

	now := time.Now().UTC()
	m.mu.Lock()
	m.jobSeq++
	id := fmt.Sprintf("app-chat-%d-%d", now.UnixMilli(), m.jobSeq)
	m.mu.Unlock()

	mode := strings.TrimSpace(opts.PermissionMode)
	if mode == "" {
		mode = "ask"
	}
	title := strings.TrimSpace(opts.Title)
	if title == "" {
		title = "Chat Devin · ACP"
	}
	cwd := strings.TrimSpace(opts.CWD)
	if cwd == "" {
		cwd = m.opts.RootDir
	}
	// Prepare the daemon-owned transcript only after the lifecycle lock has
	// rejected a concurrent start. Keep the callback out of the retained job
	// snapshot so it cannot hold renderer request state for the turn lifetime.
	if opts.BeforeStart != nil {
		beforeStart := opts.BeforeStart
		opts.BeforeStart = nil
		if err := beforeStart(&opts); err != nil {
			return nil, err
		}
	}
	opts.PromptText = strings.TrimSpace(RedactSensitiveText(opts.PromptText))
	job := &Job{
		ID:             id,
		Kind:           "app-chat",
		Key:            nil,
		Title:          title,
		Status:         "running",
		StartedAt:      now.Format(time.RFC3339Nano),
		PermissionMode: mode,
		ChatID:         opts.ChatID,
		TabID:          opts.TabID,
		SessionID:      opts.SessionID,
		ProviderID:     providerID,
		CWD:            cwd,
		startOpts:      opts,
	}
	job.touchActivity()
	// The actor must own the immutable provider turn before any manager state or
	// frozen event can expose it. This callback persists the admission receipt;
	// if it fails, no job is registered and no provider prompt is started.
	public := job.Public()
	if commitAdmission := opts.CommitAdmission; commitAdmission != nil {
		opts.CommitAdmission = nil
		job.startOpts.CommitAdmission = nil
		if err := commitAdmission(public); err != nil {
			return nil, err
		}
	}

	m.mu.Lock()
	if m.resetting {
		m.mu.Unlock()
		return nil, errors.New("ACP manager is resetting")
	}
	m.jobs[id] = job
	m.jobWG.Add(1)
	m.mu.Unlock()
	m.bindChatEnvToJob(opts.SessionID, opts.ChatID, opts.TabID, cwd)
	if liveSession {
		bridge.setJobForSession(opts.SessionID, job)
	}
	// Actor operation identity must own the provider lane before the frozen
	// start event is published. Fast providers can request permission or emit
	// output immediately; binding after publication would leave those events
	// briefly keyed by the unrelated renderer message id.
	if operationID := providercontract.NormalizeOperationID(opts.OperationID); operationID != "" {
		if lane := m.providerLaneForSession(opts.SessionID); lane != nil {
			m.bindProviderLaneJob(lane, job.ID, operationID)
		}
	}
	m.beginChatTurnCheckpoint(context.Background(), job)
	// Snapshot the public start state before the worker can mutate the job. The
	// old order launched runAppChatJob and then called job.Public(), racing fast
	// providers that completed while the start reply was still being built.
	m.emit("job:event", map[string]any{"type": "start", "job": public})

	go m.runAppChatJob(ctx, bridge, job, opts)
	return public, nil
}

func (m *Manager) runAppChatJob(ctx context.Context, bridge *Bridge, job *Job, opts JobStartOptions) {
	defer m.jobWG.Done()
	activeBridge := bridge
	defer func() {
		m.adoptSubagentsForParent(firstNonEmpty(job.VisibleJobID, job.ID))
		activeBridge.flushJobBuffers(job)
		// ACP agents naturally author either structured image blocks or ordinary
		// Markdown image links. Normalize the latter once at the terminal boundary
		// so every provider gets durable Workass media without a provider-specific
		// prompt/tool contract.
		job.addAssistantImages(ResolveAssistantMarkdownImages(job.Result, job.CWD))
		m.mu.Lock()
		if rec := m.jobs[job.ID]; rec != nil {
			rec.Status = job.Status
		}
		// A turn that failed while the manager is tearing its bridges down was
		// ended by the daemon (restart/handoff), not by the agent. Reset() closes
		// every bridge, so the in-flight prompt returns an error and the job is
		// finalized here, before the process exits — which is why the next boot's
		// orphan sweep never sees it: nothing is left in a running state to sweep.
		if job.Status == "failed" && m.resetting && job.StopReason != "cancelled" {
			job.Interrupted = true
			if job.StopReason == "" {
				job.StopReason = "daemon-restart"
			}
		}
		m.rememberFinishedJobLocked(job.ID)
		delete(m.jobs, job.ID)
		m.mu.Unlock()
		m.refreshChatEnvAfterJob(context.Background(), job)
		actorRecovery := job.actorRecoveryPending.Load()
		if job.Status == "done" && !actorRecovery {
			m.maybeCompactAfterTurn(context.Background(), activeBridge, job)
		}
		if !actorRecovery {
			m.classifyDispositionForJob(job)
			m.emit("job:event", map[string]any{"type": "end", "job": job.Public()})
		}
		activeBridge.clearJobForSession(job.SessionID, job)
		if !actorRecovery {
			m.notifyJobEnd(job.TabID, job.ChatID)
		}
	}()

	promptText := strings.TrimSpace(opts.Prompt)
	if promptText == "" {
		promptText = strings.TrimSpace(opts.Message)
	}
	if !activeBridge.hasLiveSession(job.SessionID) {
		job.CrashInterrupted = true
		job.StopReason = "engine-crash"
		if opts.ProviderLaneManaged {
			job.actorRecoveryPending.Store(true)
			if lane := m.providerLaneForSessionID(job.SessionID); lane != nil {
				lane.attachmentClosed()
			}
		}
		job.Status = "failed"
		job.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		return
	}
	// The renderer sends the selected controls on every turn. Reconcile them
	// against THIS chat's bridge immediately before prompting so a reconnect,
	// adapter-side reset, or another tab's lifecycle can never silently fall back
	// to the provider default (notably Codex "guardian").
	requestedModelID := strings.TrimSpace(opts.ModelID)
	controlResult, controlsErr := activeBridge.ensureSessionControls(ctx, job.SessionID, opts.ModelID, opts.ModeID)
	if controlsErr != nil {
		// A stale or temporarily rejected adapter control is not a prompt
		// failure. Continue with the adapter's authoritative current controls;
		// the explicit renderer selection remains durable and will be retried on
		// the next turn. This is the same recoverability rule used during
		// session startup and prevents one bad set_config_option from bricking a
		// chat forever.
		hint, policyErr := m.markProviderNeedsLogin(ctx, job.ProviderID, controlsErr)
		if policyErr != nil || hint != "" {
			displayErr := policyErr
			if displayErr == nil {
				displayErr = providerAuthenticationFailureError(job.ProviderID, controlsErr, hint)
			}
			job.Error += "\n[acp error] " + redactSensitiveText(displayErr.Error()) + "\n"
			job.Result = cleanDraft(firstNonEmpty(m.outputForJob(job), job.Error))
			code := 1
			job.Code = &code
			job.Status = "failed"
			job.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
			return
		}
		m.opts.Logf("turn control restore skipped", map[string]any{
			"tabID": job.TabID, "providerID": job.ProviderID,
			"error": redactSensitiveText(controlsErr.Error()),
		})
	}
	if controlResult.CurrentModelID != "" {
		if controlsErr == nil && m.nativeSessions != nil {
			m.nativeSessions.updateControls(job.TabID, job.ChatID, job.ProviderID, job.SessionID, controlResult.CurrentModelID, "")
		}
		if controlsErr == nil && requestedModelID != "" && controlResult.CurrentModelID != requestedModelID {
			m.requestSessionRefresh(map[string]any{
				"action": "session-refresh", "tabId": job.TabID, "chatId": job.ChatID, "sessionId": job.SessionID,
				"providerId": job.ProviderID, "modelId": controlResult.CurrentModelID, "currentModelId": controlResult.CurrentModelID,
			})
		}
	}
	if controlResult.AppliedModelID != "" {
		opts.ModelID = controlResult.AppliedModelID
	}
	if controlResult.CurrentModeID != "" {
		opts.ModeID = controlResult.CurrentModeID
	}
	if activeBridge.markSeeded(job.SessionID) {
		promptText = m.buildAppChatPrompt(opts, promptText)
	} else {
		promptText = buildUserRequestBlock(promptText, opts.HumanAuthored)
	}
	// The selected provider/model can change at any turn boundary. Agents do not
	// reliably know the host application's exact selection from their vendor
	// system prompt, so Workass supplies the authoritative runtime identity on
	// EVERY turn (not only the seed prompt).
	//
	// Sending it only when it changes was tried and reverted: compaction is
	// provider-owned for claude and codex, so the daemon cannot see the model
	// forget, and "only on change" degrades to "once, ever" the first time a
	// conversation compacts.
	promptText = buildTurnRuntimeIdentity(activeBridge, job.ProviderID, opts.ModelID) + promptText
	operationID := strings.TrimSpace(firstNonEmpty(opts.OperationID, opts.UserMessageID, job.ID))
	res, err := activeBridge.promptForJob(ctx, job.SessionID, job, operationID, promptText, opts.Images)
	if err != nil {
		displayError := redactSensitiveText(err.Error())
		hint, policyErr := m.markProviderNeedsLogin(ctx, job.ProviderID, err)
		if policyErr != nil {
			displayError = redactSensitiveText(policyErr.Error())
		} else if hint != "" {
			displayError = providerAuthenticationFailureError(job.ProviderID, err, hint).Error()
		}
		if job.CrashInterrupted {
			if job.StopReason == "" {
				job.StopReason = "engine-crash"
			}
			job.Error += "\n[acp error] El motor ACP se cerró durante el turno.\n"
		} else {
			job.Error += "\n[acp error] " + displayError + "\n"
			m.emit("job:event", map[string]any{"type": "data", "id": job.ID, "stream": "system", "chunk": "\n[acp error] " + displayError + "\n"})
		}
		job.Result = cleanDraft(firstNonEmpty(m.outputForJob(job), job.Error))
		code := 1
		if m.jobCancelled(job) {
			code = 130
			job.StopReason = "cancelled"
		}
		job.Code = &code
		job.Status = "failed"
		job.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		return
	}
	job.StopReason = res.StopReason
	job.Result = cleanDraft(m.outputForJob(job))
	code := 0
	if m.jobCancelled(job) || job.StopReason == "cancelled" {
		code = 130
		job.Status = "failed"
		job.StopReason = "cancelled"
	} else {
		job.Status = "done"
	}
	job.Code = &code
	job.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
}

func cleanDraft(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSpace(text)
	if strings.HasPrefix(strings.ToLower(text), "```") {
		if idx := strings.Index(text, "\n"); idx >= 0 {
			text = text[idx+1:]
		}
	}
	text = strings.TrimSuffix(text, "```")
	return strings.TrimSpace(text)
}

func (m *Manager) buildAppChatPrompt(opts JobStartOptions, userText string) string {
	// Workass history is a UI/audit projection. It is never replayed into a
	// provider thread. Established lanes resume their native context; a new lane
	// starts empty unless its ContextStrategy performs a verified non-sampling
	// import before this function is reached.
	return m.buildEnvironmentBrief(false) + buildUserRequestBlock(userText, opts.HumanAuthored)
}

const perTurnLanguageRule = "Response language for this turn: use the language of the current human-authored user request below. Workass internal notices, maintenance or wake messages, restored transcripts, internal summaries, tool output, UI labels, locale, and previous assistant messages are context only and never language preferences. Generated Workass tool-card text embedded in the request is quoted evidence, not human-authored language selection; when a card is followed by human prose, use the language of that prose. If the current request is a Workass-generated notice, continue in the language of the most recent human-authored user message unless that user explicitly requested another language.\n"

const perTurnHostUIRule = "Host UI rule: never use OS accessibility or GUI automation—including macOS osascript, System Events, AppleScript GUI scripting, or synthetic keyboard or mouse input—to control Workass or show results. Use Workass control, browser, and shell diagnostic surfaces instead. If those surfaces cannot perform the operation, report the limitation instead of requesting Accessibility access.\n"

func buildUserRequestBlock(userText string, humanAuthored bool) string {
	// The host UI rule stays on every turn deliberately: host_ui_contract_test
	// pins it AFTER the session is seeded, because the seed alone was judged
	// insufficient. It is duplicated with the seed on purpose.
	//
	// The language rule is repeated even for human turns. Workass tool cards can
	// be serialized ahead of the person's prose in the same transport message;
	// without an adjacent boundary their localized label can incorrectly win.
	if humanAuthored {
		if card, request, ok := splitWorkassToolCardPrefix(userText); ok {
			return perTurnLanguageRule + perTurnHostUIRule +
				"Workass tool-card evidence (not a language preference):\n<workass_tool_card>\n" + card +
				"\n</workass_tool_card>\n\nUser request:\n" + request
		}
	}
	return perTurnLanguageRule + perTurnHostUIRule + "User request:\n" + userText
}

// splitWorkassToolCardPrefix recognizes only the concrete serialized card
// shape emitted by Workass: a localized label, an MCP tool identifier, two
// duration rows, result text, then a blank line and the person's prose. It
// preserves both parts verbatim while giving the model an unambiguous language
// boundary. Ordinary messages that merely mention MCP are never rewritten.
func splitWorkassToolCardPrefix(text string) (card, request string, ok bool) {
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	if len(lines) < 7 ||
		(!strings.HasPrefix(strings.TrimSpace(lines[1]), "mcp.") && !strings.HasPrefix(strings.TrimSpace(lines[1]), "mcp__")) {
		return "", "", false
	}
	for _, duration := range lines[2:4] {
		if _, err := time.ParseDuration(strings.TrimSpace(duration)); err != nil {
			return "", "", false
		}
	}
	blank := -1
	for i := 4; i < len(lines) && i < 12; i++ {
		if strings.TrimSpace(lines[i]) == "" {
			blank = i
			break
		}
	}
	if blank < 5 {
		return "", "", false
	}
	human := strings.Join(lines[blank+1:], "\n")
	if strings.TrimSpace(human) == "" {
		return "", "", false
	}
	return strings.Join(lines[:blank], "\n"), human, true
}

func buildTurnRuntimeIdentity(bridge *Bridge, providerID, selectedModelID string) string {
	providerID = normalizeProviderID(providerID)
	providerName := ""
	modelID := strings.TrimSpace(selectedModelID)
	modelName := ""
	if bridge != nil {
		bridge.mu.Lock()
		if providerID == "" {
			providerID = bridge.providerID
		}
		providerName = strings.TrimSpace(bridge.providerName)
		if modelID == "" {
			if current := bridge.currentModelSelectionLocked(); current != nil {
				modelID = strings.TrimSpace(*current)
			}
		}
		for _, model := range bridge.models {
			if model.ModelID == modelID {
				modelName = strings.TrimSpace(model.Name)
				break
			}
		}
		bridge.mu.Unlock()
	}
	if providerID == "" {
		providerID = "unknown"
	}
	if providerName == "" || strings.EqualFold(providerName, providerID) {
		providerName = providerID
	}
	if modelID == "" {
		modelID = "unknown"
	}
	if modelName == "" || strings.EqualFold(modelName, modelID) {
		modelName = modelID
	}
	return fmt.Sprintf(
		"Active Workass runtime for this turn: provider %q (%s); model %q (%s). When asked what model or agent you are, answer with this exact Workass runtime identity instead of guessing from prior messages or a generic model family.\n\n",
		providerID, providerName, modelID, modelName,
	)
}

// buildEnvironmentBrief describes the session it is actually being sent to.
// forSubagent matters because a delegated child does not get the browser
// server: telling it otherwise costs a fruitless tool hunt on claude and an
// call to a tool that is not there on codex — the same defect as the
// notify/show line deleted in 153a406d, introduced by the same commit.
func (m *Manager) buildEnvironmentBrief(forSubagent bool) string {
	stateDir := strings.TrimSpace(m.opts.StateDir)
	if abs, err := filepath.Abs(stateDir); err == nil {
		stateDir = abs
	}
	archivePath := filepath.Join(stateDir, "chat-archive", "<chatId>.jsonl")
	browserLine := ""
	if strings.TrimSpace(m.opts.WorkassMCPBaseURL) != "" && !forSubagent {
		browserLine = "Browser tools: the visible Workass browser is available through the workass-browser MCP server; use those tools instead of opening another browser process.\n"
	}
	agentLine := ""
	if strings.TrimSpace(m.opts.WorkassMCPBaseURL) != "" {
		agentLine = "Workass control tools: the workass-agent MCP server can list/read/create/rename/configure/focus/delete exact chats; send or steer messages; cancel turns; inspect the real scored provider/model/effort/permission catalog; orchestrate tracked subagents with progress, follow-ups, retry, cancellation, and durable receipts; and host workspace artifacts with workass_host_artifact. Use exact tab_id + chat_id pairs from workass_list_chats; never infer the active tab or guess model ids.\n" +
			"Artifact delivery: ordinary local raster image Markdown is adapted by Workass into durable inline chat images, so use natural ![label](path) when the user asks to see images. The bytes are captured when the message arrives, and ONLY for files that resolve inside the chat's working directory: a path outside it, /tmp included, is left as plain text and the user sees nothing. For those, and for non-image files or when a stable hosted URL is needed, call workass_host_artifact and use its returned markdown in the response. Never expose a raw local filesystem path as an ordinary link.\n" +
			"Background work: use workass_spawn_subagent for delegated agent work; do not launch untracked detached agents or shells. For every ACP provider, if external work must outlive the ACP engine, call workass_register_external_work in the same turn and ensure its returned done_file is written, or explicitly settle it with workass_settle_external_work.\n"
	}
	return "Workass context: you are running inside workass; the user sees chat, panels, and canvas from the controller device.\n" +
		"Do not open OS windows just to show results.\n" +
		perTurnHostUIRule +
		browserLine +
		agentLine +
		"Language rule: reply in the language of the current human-authored user request; restored or internal text never selects it.\n" +
		"Verification receipts: Workass preserves command and tool output in internal event history and profile logs. Do not repeat raw command output or exhaustive modified-file manifests in final responses. Summarize relevant outcomes, name failures or skipped checks, and include detailed output only when the user explicitly asks for it.\n" +
		"Chat transcripts live at " + archivePath + ".\n" +
		"Each line is one JSON message {role, content, status, at}; to read another conversation the user references, read that file.\n\n"
}

func safeArchiveName(tabID string) string {
	var b strings.Builder
	for _, r := range tabID {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

const finishedJobReceiptLimit = 256

type JobCancelResult struct {
	Cancelled bool   `json:"cancelled"`
	Reason    string `json:"reason"`
}

func (m *Manager) rememberFinishedJobLocked(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	if _, exists := m.finishedJobIDs[id]; exists {
		return
	}
	m.finishedJobIDs[id] = struct{}{}
	m.finishedJobOrder = append(m.finishedJobOrder, id)
	if len(m.finishedJobOrder) <= finishedJobReceiptLimit {
		return
	}
	drop := m.finishedJobOrder[0]
	m.finishedJobOrder = append([]string(nil), m.finishedJobOrder[1:]...)
	delete(m.finishedJobIDs, drop)
}

func (m *Manager) CancelJobResult(id string) JobCancelResult {
	id = strings.TrimSpace(id)
	m.mu.Lock()
	job := m.jobs[id]
	if job == nil {
		_, idle := m.finishedJobIDs[id]
		m.mu.Unlock()
		if idle {
			return JobCancelResult{Reason: "idle"}
		}
		return JobCancelResult{Reason: "unknown"}
	}
	if job.Status != "running" {
		m.mu.Unlock()
		return JobCancelResult{Reason: "idle"}
	}
	job.cancelled = true
	m.mu.Unlock()
	m.cancelPermissionsForSession(job.SessionID)
	bridge := m.bridgeForJob(job)
	if bridge == nil {
		return JobCancelResult{Reason: "unknown"}
	}
	bridge.notify("session/cancel", map[string]any{"sessionId": job.SessionID})
	return JobCancelResult{Cancelled: true, Reason: "cancelled"}
}

func (m *Manager) CancelJob(id string) bool {
	return m.CancelJobResult(id).Cancelled
}

// JobChatID is a read-only address fence. Actor-aware handlers use it to
// distinguish an addressless internal job from a logical chat whose
// mutation must never fall back around the actor.
func (m *Manager) JobChatID(id string) string {
	id = strings.TrimSpace(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	if job := m.jobs[id]; job != nil {
		return strings.TrimSpace(job.ChatID)
	}
	return ""
}

// RunningJobForChat returns the exact active app-chat job, if any. It is used by
// the daemon-owned agent chat controller to serialize its durable FIFO with UI
// turns; matching one identity field is never sufficient.
func (m *Manager) RunningJobForChat(tabID, chatID string) (map[string]any, bool) {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if tabID == "" || chatID == "" {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, job := range m.jobs {
		if job == nil || job.Status != "running" || job.TabID != tabID || job.ChatID != chatID {
			continue
		}
		return job.Public(), true
	}
	return nil, false
}

type ChatRuntimeDigest struct {
	RunningJobID         string
	PendingPermissionIDs []string
}

// ChatRuntimeDigests snapshots only reconciliation identities. It deliberately
// excludes prompts, output, media, and tool bodies so the daemon heartbeat can
// remain cheap and bounded even when the canonical session mirror is large.
func (m *Manager) ChatRuntimeDigests() map[string]ChatRuntimeDigest {
	out := make(map[string]ChatRuntimeDigest)
	jobKeys := make(map[string]string)
	m.mu.Lock()
	for id, job := range m.jobs {
		if job == nil || job.Status != "running" || job.TabID == "" || job.ChatID == "" {
			continue
		}
		key := job.TabID + "\x00" + job.ChatID
		out[key] = ChatRuntimeDigest{RunningJobID: id, PendingPermissionIDs: []string{}}
		jobKeys[id] = key
	}
	for id, permission := range m.permissions {
		if permission == nil {
			continue
		}
		key := jobKeys[permission.jobID]
		if key == "" {
			continue
		}
		item := out[key]
		item.PendingPermissionIDs = append(item.PendingPermissionIDs, id)
		out[key] = item
	}
	m.mu.Unlock()
	for key, item := range out {
		sort.Strings(item.PendingPermissionIDs)
		out[key] = item
	}
	return out
}

func (m *Manager) jobCancelled(job *Job) bool {
	if job == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return job.cancelled
}

func (m *Manager) PermissionDecide(id, optionID string) bool {
	if id == "" {
		return false
	}
	m.mu.Lock()
	rec := m.permissions[id]
	m.mu.Unlock()
	if rec != nil {
		rec.finish(optionID)
	}
	return true
}

// PermissionChatID is the read-only counterpart to JobChatID. A non-empty
// value means the request is actor-owned and may not use manager mutation as a
// recovery fallback.
func (m *Manager) PermissionChatID(id string) string {
	id = strings.TrimSpace(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec := m.permissions[id]; rec != nil {
		return strings.TrimSpace(asString(rec.payload["chatId"]))
	}
	return ""
}

// PendingPermissions returns reconnect-safe copies of unresolved permission
// requests for the current controller snapshot.
func (m *Manager) PendingPermissions() []any {
	m.mu.Lock()
	ids := make([]string, 0, len(m.permissions))
	for id := range m.permissions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]any, 0, len(ids))
	for _, id := range ids {
		if rec := m.permissions[id]; rec != nil && rec.payload != nil {
			out = append(out, copyPermissionPayload(rec.payload))
		}
	}
	m.mu.Unlock()
	return out
}

func (m *Manager) requestPermission(req permissionRequest) string {
	m.mu.Lock()
	m.permSeq++
	id := fmt.Sprintf("perm-%d-%d", time.Now().UnixMilli(), m.permSeq)
	payload := map[string]any{
		"id":        id,
		"jobId":     nullableString(req.JobID),
		"sessionId": nullableString(req.SessionID),
		"title":     firstNonEmpty(asString(req.ToolCall["title"]), asString(req.ToolCall["kind"]), "acción"),
		"kind":      nullableString(asString(req.ToolCall["kind"])),
		"options":   permissionOptions(req.Options),
	}
	for _, job := range m.jobs {
		if job == nil || (job.ID != req.JobID && job.VisibleJobID != req.JobID) {
			continue
		}
		payload["tabId"] = nullableString(job.TabID)
		payload["chatId"] = nullableString(job.ChatID)
		break
	}
	// A question is not a permission: AskUserQuestion reaches us down this same
	// channel because the SDK has no other one (see claude-native-host.mjs), and
	// dropping its rawInput here is what rendered "Quiere ejecutar
	// AskUserQuestion" with no question in it. Additive field: renderers that
	// don't know it keep showing the old card.
	question := permissionQuestion(req.ToolCall["rawInput"])
	if question != nil {
		payload["question"] = question
	}
	// A subagent's question never reaches the user's screen: it goes back to the
	// agent that spawned it, which holds the chat and can answer or ask on its
	// behalf. Parking it here would hang a background lane on a human, on a card
	// rendered in someone else's turn.
	if question != nil && req.Subagent {
		m.mu.Unlock()
		return subagentQuestionOptionID
	}
	// Plan mode is a ceremony, not a capability: it ends by asking a human to
	// approve ExitPlanMode. A parent that spawns a child with permission_intent
	// "read" is asking for analysis, and gets it by the child staying in plan
	// mode and answering in prose — so keep planning, which grants nothing and
	// is the outcome the narrowing was chosen for. Only ever reached by a child
	// that is IN plan mode, which is exactly the deliberately narrowed one.
	if req.Subagent && isExitPlanModeToolCall(req.ToolCall) {
		if optionID, ok := permissionOptionForDecision(req.Options, subagentPermissionDeny); ok {
			m.mu.Unlock()
			return optionID
		}
	}
	rec := &permissionResolver{id: id, jobID: req.JobID, sessionID: req.SessionID, payload: payload, ch: make(chan string, 1)}
	m.permissions[id] = rec
	m.mu.Unlock()

	// A card WAITS — Workass arms no clock of its own. Every deadline we invented
	// here expired something a person was still reading: applied to a question it
	// answered "no answer" for a user who had merely stepped away, and applied to
	// a permission it denied a tool the user was about to allow. Whatever timeout
	// the origin harness enforces is the only one (user 2026-07-25). Nothing is
	// left dangling: cancelling the turn or closing the session settles every
	// pending request through cancelPermissionsForSession, and the card stays on
	// screen meanwhile. A caller that wants a deadline back sets PermissionTimeout
	// explicitly; a question is exempt even then.
	timeout := req.PermissionTimeout
	if timeout <= 0 {
		timeout = m.opts.PermissionTimeout
	}
	if question == nil && timeout > 0 {
		timer := time.AfterFunc(timeout, func() {
			rec.finish(req.FallbackOptionID)
		})
		rec.timerMu.Lock()
		rec.timer = timer
		rec.timerMu.Unlock()
	}

	m.emit("chat:permission-request", payload)
	optionID := <-rec.ch
	m.mu.Lock()
	if m.permissions[id] == rec {
		delete(m.permissions, id)
	}
	m.mu.Unlock()
	m.emit("chat:permission-resolved", map[string]any{
		"id": id, "jobId": nullableString(req.JobID), "sessionId": nullableString(req.SessionID),
		"optionId": nullableString(optionID), "resolvedAt": time.Now().UTC().Format(time.RFC3339Nano),
	})
	return optionID
}

func copyPermissionPayload(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		if key == "options" {
			options := []any{}
			for _, raw := range anySlice(value) {
				option := mapFromAny(raw)
				copyOption := make(map[string]any, len(option))
				for optionKey, optionValue := range option {
					copyOption[optionKey] = optionValue
				}
				options = append(options, copyOption)
			}
			out[key] = options
			continue
		}
		out[key] = value
	}
	return out
}

// permissionQuestion extracts a model-authored question from a permission
// request's rawInput. Bounded and redacted on the way out: this text is written
// by the model and lands straight in the transcript, so it can neither carry a
// secret it scraped nor blow up a card with an unbounded paragraph.
func permissionQuestion(raw any) map[string]any {
	fields := mapFromAny(raw)
	if len(fields) == 0 {
		return nil
	}
	question := strings.TrimSpace(asString(fields["question"]))
	if question == "" {
		return nil
	}
	options := []any{}
	for _, entry := range anySlice(fields["options"]) {
		option := mapFromAny(entry)
		label := strings.TrimSpace(asString(option["label"]))
		if label == "" {
			continue
		}
		options = append(options, map[string]any{
			"label":       clipPermissionText(label, 120),
			"description": clipPermissionText(strings.TrimSpace(asString(option["description"])), 240),
		})
		if len(options) >= 4 {
			break
		}
	}
	if len(options) == 0 {
		return nil
	}
	multiSelect, _ := fields["multiSelect"].(bool)
	return map[string]any{
		"question":    clipPermissionText(question, 400),
		"header":      clipPermissionText(strings.TrimSpace(asString(fields["header"])), 40),
		"options":     options,
		"multiSelect": multiSelect,
	}
}

func clipPermissionText(s string, max int) string {
	s = redactSensitiveText(s)
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return strings.TrimSpace(string(runes[:max])) + "…"
}

func permissionOptions(options []any) []any {
	out := make([]any, 0, len(options))
	for _, raw := range options {
		opt := mapFromAny(raw)
		optionID := opt["optionId"]
		name := firstNonEmpty(asString(opt["name"]), asString(opt["kind"]), asString(opt["optionId"]))
		out = append(out, map[string]any{"optionId": optionID, "name": name, "kind": firstNonEmpty(asString(opt["kind"]), "")})
	}
	return out
}

func (r *permissionResolver) finish(optionID string) {
	r.once.Do(func() {
		r.timerMu.Lock()
		if r.timer != nil {
			r.timer.Stop()
		}
		r.timerMu.Unlock()
		r.ch <- optionID
	})
}

func (m *Manager) cancelPermissionsForSession(sessionID string) {
	if sessionID == "" {
		return
	}
	m.mu.Lock()
	var recs []*permissionResolver
	for id, rec := range m.permissions {
		if rec.sessionID == sessionID {
			delete(m.permissions, id)
			recs = append(recs, rec)
		}
	}
	m.mu.Unlock()
	for _, rec := range recs {
		rec.finish("")
	}
}

func (m *Manager) emit(channel string, payload any) {
	if m == nil {
		return
	}
	// Keep normalized observation, its durable actor ACK, and frozen publication
	// in one non-reentrant critical section. Without this boundary a later
	// provider callback can receive its ACK and enter Broadcast while an earlier
	// callback is still finishing its own publication.
	m.providerPublicationMu.Lock()
	defer m.providerPublicationMu.Unlock()
	if err := m.observeProviderLaneEvent(channel, payload); err != nil {
		m.opts.Logf("provider event rejected before frozen-wire publication", map[string]any{
			"channel": channel, "error": redactSensitiveText(err.Error()),
		})
		if lane := m.providerLaneForFrozenPayload(mapFromAny(payload)); lane != nil {
			lane.rejectFrozenProtocol(err)
		}
		return
	}
	m.cacheProviderSnapshotEvent(channel, payload)
	m.opts.Broadcast(channel, payload)
}

func (m *Manager) clearProviderSnapshots() {
	m.mu.Lock()
	m.latestProvidersList = nil
	m.hasLatestProvidersList = false
	m.latestChatCatalog = nil
	m.hasLatestChatCatalog = false
	m.latestProviderUpdates = nil
	m.hasProviderUpdates = false
	m.mu.Unlock()
}

func (m *Manager) cacheProviderSnapshotEvent(channel string, payload any) {
	switch channel {
	case "providers:list", "chat:catalog", "chat:plan-usage", "providers:updates", "providers:update-progress", "app:update":
	default:
		return
	}
	m.mu.Lock()
	switch channel {
	case "providers:list":
		m.latestProvidersList = payload
		m.hasLatestProvidersList = true
	case "chat:catalog":
		m.latestChatCatalog = payload
		m.hasLatestChatCatalog = true
	case "providers:updates":
		m.latestProviderUpdates = payload
		m.hasProviderUpdates = true
	case "providers:update-progress":
		if progress, ok := payload.(ProviderUpdateProgress); ok && progress.ProviderID != "" {
			m.providerUpdateProgress[progress.ProviderID] = progress
		}
	case "app:update":
		m.latestAppUpdate = payload
		m.hasAppUpdate = true
	case "chat:plan-usage":
		if snapshot, ok := payload.(PlanUsageSnapshot); ok && snapshot.ProviderID != "" {
			m.planUsageByProvider[snapshot.ProviderID] = snapshot
		}
	}
	m.mu.Unlock()
}

func (m *Manager) providerSnapshotEvents() []providerSnapshotEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	events := make([]providerSnapshotEvent, 0, 4+len(m.planUsageByProvider)+len(m.providerUpdateProgress))
	if m.hasLatestProvidersList {
		events = append(events, providerSnapshotEvent{channel: "providers:list", payload: m.latestProvidersList})
	}
	if m.hasLatestChatCatalog {
		events = append(events, providerSnapshotEvent{channel: "chat:catalog", payload: m.latestChatCatalog})
	}
	if m.hasProviderUpdates {
		events = append(events, providerSnapshotEvent{channel: "providers:updates", payload: m.latestProviderUpdates})
	}
	seenProgress := make(map[string]struct{}, len(m.providerUpdateProgress))
	for _, id := range m.providerOrder {
		if progress, ok := m.providerUpdateProgress[id]; ok {
			events = append(events, providerSnapshotEvent{channel: "providers:update-progress", payload: progress})
			seenProgress[id] = struct{}{}
		}
	}
	var extraProgress []string
	for id := range m.providerUpdateProgress {
		if _, ok := seenProgress[id]; !ok {
			extraProgress = append(extraProgress, id)
		}
	}
	sort.Strings(extraProgress)
	for _, id := range extraProgress {
		events = append(events, providerSnapshotEvent{channel: "providers:update-progress", payload: m.providerUpdateProgress[id]})
	}
	if m.hasAppUpdate {
		events = append(events, providerSnapshotEvent{channel: "app:update", payload: m.latestAppUpdate})
	}
	seen := make(map[string]struct{}, len(m.planUsageByProvider))
	for _, id := range m.providerOrder {
		if snapshot, ok := m.planUsageByProvider[id]; ok {
			events = append(events, providerSnapshotEvent{channel: "chat:plan-usage", payload: snapshot})
			seen[id] = struct{}{}
		}
	}
	var extra []string
	for id := range m.planUsageByProvider {
		if _, ok := seen[id]; !ok {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	for _, id := range extra {
		events = append(events, providerSnapshotEvent{channel: "chat:plan-usage", payload: m.planUsageByProvider[id]})
	}
	return events
}

// PublishProviderSnapshots seeds a newly ready wire client with the current
// provider projections. If detection is in flight, DetectProviders has cleared
// this cache and the client will receive the eventual broadcast instead.
func (m *Manager) PublishProviderSnapshots(send func(channel string, payload any) error) {
	if send == nil {
		return
	}
	for _, event := range m.providerSnapshotEvents() {
		if err := send(event.channel, event.payload); err != nil {
			return
		}
	}
}

// LiveSessions snapshots the live ACP bridge bindings for this daemon process.
// session:get overlays them at read time; durable provider-native ids remain in
// the separate daemon-only ledger and never enter the renderer mirror.
func (m *Manager) LiveSessions() []LiveSession {
	m.mu.Lock()
	active := make(map[string]bool, len(m.jobs))
	for _, job := range m.jobs {
		if job != nil && job.Status == "running" && job.SessionID != "" {
			active[job.SessionID] = true
		}
	}
	bindings := make(map[string]*Bridge, len(m.sessionBridge))
	ids := make([]string, 0, len(m.sessionBridge))
	for sessionID, bridge := range m.sessionBridge {
		bindings[sessionID] = bridge
		ids = append(ids, sessionID)
	}
	m.mu.Unlock()

	sort.Slice(ids, func(i, j int) bool {
		if active[ids[i]] != active[ids[j]] {
			return active[ids[i]]
		}
		return ids[i] < ids[j]
	})
	out := make([]LiveSession, 0, len(ids))
	seenTabs := make(map[string]bool)
	for _, sessionID := range ids {
		binding, ok := bindings[sessionID].liveSession(sessionID)
		if !ok || binding.TabID == "" || seenTabs[binding.TabID] {
			continue
		}
		seenTabs[binding.TabID] = true
		out = append(out, binding)
	}
	return out
}

// LiveSessionByID returns the exact disposable bridge attachment for one
// provider-native session. Unlike LiveSessions it does not collapse several
// provider lanes that currently share one renderer tab; actor projection must
// overlay only the lane selected by authoritative chat state.
func (m *Manager) LiveSessionByID(sessionID string) (LiveSession, bool) {
	if m == nil || strings.TrimSpace(sessionID) == "" {
		return LiveSession{}, false
	}
	m.mu.Lock()
	bridge := m.sessionBridge[strings.TrimSpace(sessionID)]
	m.mu.Unlock()
	if bridge == nil {
		return LiveSession{}, false
	}
	return bridge.liveSession(strings.TrimSpace(sessionID))
}

// LiveSession returns the runtime binding for one ACP session without exposing
// manager internals to the wire/session persistence layer.
func (m *Manager) LiveSession(sessionID string) (LiveSession, bool) {
	if strings.TrimSpace(sessionID) == "" {
		return LiveSession{}, false
	}
	m.mu.Lock()
	bridge := m.sessionBridge[sessionID]
	m.mu.Unlock()
	if bridge == nil {
		return LiveSession{}, false
	}
	return bridge.liveSession(sessionID)
}

func (m *Manager) getBridge(opts SessionOptions) *Bridge {
	m.mu.Lock()
	defer m.mu.Unlock()
	providerID, err := m.resolveSessionProviderLocked(opts)
	if err != nil {
		return nil
	}
	opts.ProviderID = providerID
	providerOptions, err := m.optionsForProviderLocked(opts.ProviderID)
	if err != nil {
		return nil
	}
	key := m.normalizeBridgeKeyLocked(opts)
	bridge := m.bridges[key]
	if bridge == nil || bridge.Closed() || bridge.Hibernated() {
		bridge = newBridge(key, providerOptions, m)
		m.bridges[key] = bridge
	}
	return bridge
}

func (m *Manager) replaceBridge(opts SessionOptions) *Bridge {
	m.mu.Lock()
	defer m.mu.Unlock()
	providerID, err := m.resolveSessionProviderLocked(opts)
	if err != nil {
		return nil
	}
	opts.ProviderID = providerID
	providerOptions, err := m.optionsForProviderLocked(opts.ProviderID)
	if err != nil {
		return nil
	}
	key := m.normalizeBridgeKeyLocked(opts)
	bridge := newBridge(key, providerOptions, m)
	m.bridges[key] = bridge
	return bridge
}

func (m *Manager) normalizeBridgeKeyLocked(opts SessionOptions) string {
	providerID := normalizeProviderID(opts.ProviderID)
	if providerID == "" {
		providerID, _ = m.resolveSessionProviderLocked(opts)
	}
	if providerID == "" {
		providerID = m.defaultProviderID
	}
	for _, raw := range []string{opts.BridgeKey, opts.TabID, opts.ChatID, opts.SessionID} {
		raw = strings.TrimSpace(raw)
		if raw != "" && raw != "default" {
			if len(raw) > 120 {
				raw = raw[:120]
			}
			return providerID + ":" + raw
		}
	}
	m.bridgeSeq++
	return fmt.Sprintf("%s:unscoped-%d-%d", providerID, time.Now().UnixMilli(), m.bridgeSeq)
}

func (m *Manager) bridgeForSession(sessionID string, fallback SessionOptions) *Bridge {
	m.mu.Lock()
	fallback = m.disambiguateSessionFallbackLocked(sessionID, fallback)
	keyedBridge := m.bridgeForFallbackLocked(fallback)
	bridge := m.sessionBridge[sessionID]
	m.mu.Unlock()
	if keyedBridge != nil {
		if live, ok := keyedBridge.liveSession(sessionID); ok && liveSessionMatchesOptions(live, fallback) {
			return keyedBridge
		}
	}
	if bridge != nil && !bridge.Closed() && !bridge.Hibernated() {
		if live, ok := bridge.liveSession(sessionID); ok && liveSessionMatchesOptions(live, fallback) {
			return bridge
		}
	}
	if keyedBridge != nil && !keyedBridge.Closed() && !keyedBridge.Hibernated() {
		return keyedBridge
	}
	if fallback.BridgeKey != "" || fallback.TabID != "" || fallback.ChatID != "" {
		return m.getBridge(fallback)
	}
	if bridge != nil && !bridge.Closed() {
		return bridge
	}
	return nil
}

func liveSessionMatchesOptions(live LiveSession, opts SessionOptions) bool {
	if tabID := strings.TrimSpace(opts.TabID); tabID != "" && live.TabID != tabID {
		return false
	}
	if chatID := strings.TrimSpace(opts.ChatID); chatID != "" && live.ChatID != chatID {
		return false
	}
	if providerID := normalizeProviderID(opts.ProviderID); providerID != "" && normalizeProviderID(live.Info.ProviderID) != providerID {
		return false
	}
	return true
}

func (m *Manager) disambiguateSessionFallbackLocked(sessionID string, fallback SessionOptions) SessionOptions {
	if fallback.SessionID == "" {
		fallback.SessionID = sessionID
	}
	if fallback.ProviderID != "" {
		return fallback
	}
	if bound := m.boundProviderForChatLocked(fallback); bound != "" {
		fallback.ProviderID = bound
		return fallback
	}
	if providerID := normalizeProviderID(m.sessionProvider[sessionID]); providerID != "" {
		fallback.ProviderID = providerID
		return fallback
	}
	fallback.ProviderID = m.defaultProviderID
	return fallback
}

func (m *Manager) bridgeForFallbackLocked(fallback SessionOptions) *Bridge {
	if fallback.BridgeKey == "" && fallback.TabID == "" && fallback.ChatID == "" {
		return nil
	}
	key := m.normalizeBridgeKeyLocked(fallback)
	return m.bridges[key]
}

func (m *Manager) bridgeForJob(job *Job) *Bridge {
	fallback := SessionOptions{
		SessionID:  job.SessionID,
		TabID:      job.TabID,
		ChatID:     job.ChatID,
		ProviderID: job.ProviderID,
	}
	bridge := m.bridgeForSession(job.SessionID, fallback)
	if bridge != nil && bridge.jobForSession(job.SessionID) == job {
		return bridge
	}

	m.mu.Lock()
	bridges := make([]*Bridge, 0, len(m.bridges))
	for _, candidate := range m.bridges {
		bridges = append(bridges, candidate)
	}
	m.mu.Unlock()
	for _, candidate := range bridges {
		if candidate == nil || candidate.Closed() || candidate.Hibernated() {
			continue
		}
		if candidate.jobForSession(job.SessionID) == job {
			return candidate
		}
	}
	return bridge
}

func (m *Manager) rememberSession(sessionID string, bridge *Bridge, ownerKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.sessionBridge[sessionID]; existing != nil && existing != bridge {
		// A hibernated or closed engine keeps its session mapping so lookups can
		// still resolve the chat, but its replacement bridge re-attaching the
		// same provider-native session must take ownership, not collide.
		if !existing.Closed() && !existing.Hibernated() {
			return fmt.Errorf("ACP session id collision: %q is already attached to another chat", sessionID)
		}
		existing.releaseSession(sessionID)
	}
	m.sessionBridge[sessionID] = bridge
	m.sessionProvider[sessionID] = bridge.ProviderID()
	if ownerKey != "" {
		m.agentOwnerBySession[sessionID] = ownerKey
	}
	return nil
}

func (m *Manager) forgetSession(sessionID string, bridge *Bridge) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessionBridge[sessionID] == bridge {
		delete(m.sessionBridge, sessionID)
		delete(m.sessionProvider, sessionID)
	}
	if ownerKey := m.agentOwnerBySession[sessionID]; ownerKey != "" {
		delete(m.agentOwnerBySession, sessionID)
		delete(m.agentOwners, ownerKey)
	}
	delete(m.usageBySession, sessionID)
	m.forgetCommandCatalogSessionLocked(sessionID)
}

// provisionAgentOwner makes the per-session bearer capability valid before
// the provider opens its MCP connection during session/new or session/resume.
// The old stdio helper did not need this ordering because it deferred the
// callback until a tool call. Stateless MCP authenticates discovery itself, so
// the binding must already exist when the provider negotiates the session.
func (m *Manager) provisionAgentOwner(opts SessionOptions) func() {
	ownerKey := strings.TrimSpace(opts.AgentOwnerKey)
	if m == nil || opts.Ephemeral || ownerKey == "" {
		return func() {}
	}
	m.mu.Lock()
	m.bindAgentOwnerLocked(ownerKey, opts.ChatID, opts.TabID)
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, attachedOwner := range m.agentOwnerBySession {
			if attachedOwner == ownerKey {
				return
			}
		}
		binding, ok := m.agentOwners[ownerKey]
		if ok && binding.ChatID == strings.TrimSpace(opts.ChatID) && binding.TabID == strings.TrimSpace(opts.TabID) {
			delete(m.agentOwners, ownerKey)
		}
	}
}

func (b *Bridge) NewSession(ctx context.Context, opts SessionOptions) (SessionInfo, error) {
	if _, err := b.Initialize(ctx); err != nil {
		return SessionInfo{}, err
	}
	releaseOwner := b.manager.provisionAgentOwner(opts)
	cwd := b.sessionCWD(opts.CWD)
	res, err := b.request(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": sessionMCPServers(b.opts, opts)}, b.opts.InitTimeout)
	if err != nil {
		releaseOwner()
		return SessionInfo{}, b.withStderrTail(err)
	}
	sessionID := asString(res["sessionId"])
	if sessionID == "" {
		releaseOwner()
		return SessionInfo{}, errors.New("ACP session/new returned no sessionId")
	}
	info, err := b.attachSession(sessionID, cwd, opts, res, "session-new")
	if err != nil {
		releaseOwner()
	}
	return info, err
}

// RestoreSession attaches a new adapter host to the exact provider-native
// thread saved for the lane. Established lanes use session/resume exclusively:
// session/load may hydrate a transcript, but it is not proof that the same
// native lineage was resumed and therefore cannot be a recovery fallback.
func (b *Bridge) RestoreSession(ctx context.Context, binding nativeSessionBinding, opts SessionOptions) (SessionInfo, string, error) {
	if _, err := b.Initialize(ctx); err != nil {
		return SessionInfo{}, "", err
	}
	cwd := b.sessionCWD(firstNonEmpty(opts.CWD, binding.CWD))
	// A fork moved the conversation under ProviderSessionID; that is the id
	// whose transcript carries the post-fork turns.
	resumeID := firstNonEmpty(binding.ProviderSessionID, binding.SessionID)
	params := map[string]any{
		"sessionId":  resumeID,
		"cwd":        cwd,
		"mcpServers": sessionMCPServers(b.opts, opts),
	}
	if !b.supportsSessionResume() {
		return SessionInfo{}, "", errors.New("ACP provider does not support exact session/resume")
	}
	releaseOwner := b.manager.provisionAgentOwner(opts)
	const method = "session/resume"
	res, err := b.request(ctx, method, params, b.opts.InitTimeout)
	if err != nil {
		releaseOwner()
		return SessionInfo{}, method, b.withStderrTail(err)
	}
	info, attachErr := b.attachSession(resumeID, cwd, opts, res, method)
	if attachErr != nil {
		releaseOwner()
		return SessionInfo{}, method, attachErr
	}
	return info, method, nil
}

func (b *Bridge) sessionCWD(raw string) string {
	cwd := strings.TrimSpace(raw)
	if cwd == "" {
		cwd = b.opts.RootDir
	}
	if !filepath.IsAbs(cwd) && b.opts.RootDir != "" {
		cwd = filepath.Join(b.opts.RootDir, cwd)
	}
	return cwd
}

func (b *Bridge) supportsSessionResume() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	caps := mapFromAny(b.agentCaps["sessionCapabilities"])
	raw, ok := caps["resume"]
	if !ok || raw == nil {
		return false
	}
	_, ok = raw.(map[string]any)
	return ok
}

func (b *Bridge) attachSession(sessionID, cwd string, opts SessionOptions, res map[string]any, reason string) (SessionInfo, error) {
	if !opts.Ephemeral {
		if err := b.manager.rememberSession(sessionID, b, opts.AgentOwnerKey); err != nil {
			return SessionInfo{}, err
		}
	}
	b.mu.Lock()
	b.spare = opts.Spare
	b.tabID = opts.TabID
	b.chatID = opts.ChatID
	b.cwd = cwd
	b.agentOwnerKey = opts.AgentOwnerKey
	b.state = StateWarm
	b.pinned = false
	b.lastActivity = time.Now()
	b.sessions[sessionID] = struct{}{}
	b.jobsBySession[sessionID] = nil
	b.mu.Unlock()
	b.applyConfigOptionsForSession(sessionID, res["configOptions"], false, false)
	b.applyAvailableModels(res["availableModels"])
	b.applySessionModels(res["models"])
	commandCatalog := b.applyCommandCatalog(sessionID, res["commandCatalog"])
	commandCatalogSupported := b.supportsProviderCommandCatalog()
	b.manager.bridgeChanged(b, reason)

	b.mu.Lock()
	currentModelID := b.currentModelSelectionLocked()
	realmMeta := mapFromAny(mapFromAny(res["_meta"])["workassProviderRealm"])
	info := SessionInfo{
		SessionID:               sessionID,
		CWD:                     cwd,
		Agent:                   b.agentName,
		ProviderID:              b.providerID,
		ProviderName:            b.providerName,
		ProviderAccountScope:    strings.TrimSpace(asString(realmMeta["accountScope"])),
		ProviderInstallScope:    strings.TrimSpace(asString(realmMeta["installScope"])),
		ProviderRealmVerified:   boolFromMap(realmMeta, "verified"),
		Models:                  append([]Model(nil), b.models...),
		CurrentModelID:          currentModelID,
		Modes:                   append([]Mode(nil), b.modes...),
		CurrentModeID:           copyStringPtr(b.currentMode),
		ImageSupport:            b.imageSupport,
		CommandCatalogSupported: commandCatalogSupported,
		CommandCatalog:          commandCatalog,
	}
	b.mu.Unlock()
	if !opts.Ephemeral {
		b.manager.schedulePlanUsageRefresh(b, sessionID)
	}
	return info, nil
}

func browserMCPServers(options Options, session SessionOptions) []any {
	if session.Ephemeral || session.Spare {
		return []any{}
	}
	// A delegated child does not drive the user's browser: it was spawned to do
	// a job and report back. Attaching the server anyway cost every one of its
	// requests the whole browser tool list for a child that cannot use it.
	if strings.HasPrefix(strings.TrimSpace(session.ChatID), subagentChatIDPrefix) {
		return []any{}
	}
	baseURL := strings.TrimRight(strings.TrimSpace(options.WorkassMCPBaseURL), "/")
	ownerKey := strings.TrimSpace(session.AgentOwnerKey)
	if !strings.HasPrefix(baseURL, "https://") || ownerKey == "" {
		return []any{}
	}
	return []any{map[string]any{
		"name": "workass-browser",
		"type": "http",
		"url":  baseURL + "/workass/mcp/browser",
		"headers": map[string]string{
			"Authorization":     "Bearer " + ownerKey,
			"X-Workass-Chat-ID": strings.TrimSpace(session.ChatID),
			"X-Workass-Tab-ID":  strings.TrimSpace(session.TabID),
		},
	}}
}

func agentMCPServers(options Options, session SessionOptions) []any {
	if session.Ephemeral {
		return []any{}
	}
	baseURL := strings.TrimRight(strings.TrimSpace(options.WorkassMCPBaseURL), "/")
	ownerKey := strings.TrimSpace(session.AgentOwnerKey)
	if !strings.HasPrefix(baseURL, "https://") || ownerKey == "" {
		return []any{}
	}
	return []any{map[string]any{
		"name": "workass-agent",
		"type": "http",
		"url":  baseURL + "/workass/mcp/agent",
		"headers": map[string]string{
			"Authorization":     "Bearer " + ownerKey,
			"X-Workass-Chat-ID": strings.TrimSpace(session.ChatID),
			"X-Workass-Tab-ID":  strings.TrimSpace(session.TabID),
		},
	}}
}

func sessionMCPServers(options Options, session SessionOptions) []any {
	servers := browserMCPServers(options, session)
	return append(servers, agentMCPServers(options, session)...)
}

func (b *Bridge) liveSession(sessionID string) (LiveSession, bool) {
	if b == nil || sessionID == "" || b.Closed() || b.Hibernated() {
		return LiveSession{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.sessions[sessionID]; !ok {
		return LiveSession{}, false
	}
	currentModelID := b.currentModelSelectionLocked()
	if desired := strings.TrimSpace(b.durableModelSelection[sessionID]); desired != "" {
		currentModelID = &desired
	}
	return LiveSession{
		TabID:  b.tabID,
		ChatID: b.chatID,
		Info: SessionInfo{
			SessionID:      sessionID,
			CWD:            b.cwd,
			Agent:          b.agentName,
			ProviderID:     b.providerID,
			ProviderName:   b.providerName,
			Models:         append([]Model(nil), b.models...),
			CurrentModelID: currentModelID,
			Modes:          append([]Mode(nil), b.modes...),
			CurrentModeID:  copyStringPtr(b.currentMode),
			ImageSupport:   b.imageSupport,
		},
	}, true
}

// releaseSession removes a session from this bridge's internal ownership maps
// without touching manager state or the engine process. Used when a
// successor bridge takes over a session that this (hibernated or closed)
// bridge still holds, so a later attachment cannot double-own it.
func (b *Bridge) releaseSession(sessionID string) {
	b.mu.Lock()
	delete(b.sessions, sessionID)
	delete(b.seededSessions, sessionID)
	delete(b.jobsBySession, sessionID)
	delete(b.durableModelSelection, sessionID)
	b.mu.Unlock()
}

func (b *Bridge) CloseSession(ctx context.Context, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	b.manager.cancelPermissionsForSession(sessionID)
	b.mu.Lock()
	delete(b.sessions, sessionID)
	delete(b.seededSessions, sessionID)
	delete(b.jobsBySession, sessionID)
	delete(b.durableModelSelection, sessionID)
	remaining := len(b.sessions)
	b.mu.Unlock()
	b.manager.forgetSession(sessionID, b)
	if !b.Closed() && !b.Hibernated() {
		_, _ = b.request(ctx, "session/close", map[string]any{"sessionId": sessionID}, 8*time.Second)
	}
	if remaining == 0 {
		b.Close(true, errors.New("ACP session closed"))
	}
	return true
}

type modelWriteResolution struct {
	requested      string
	modelValue     string
	baseModelID    string
	effort         string
	separateAxis   bool
	axisKnown      bool
	variantMatched bool
}

func (b *Bridge) SetModel(ctx context.Context, sessionID, modelID string) (map[string]any, error) {
	if _, err := b.Initialize(ctx); err != nil {
		return nil, err
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil, errors.New("model id is required")
	}
	// Exact adapter ids always win because Claude has literal bracketed model ids
	// (for example opus[1m]). Otherwise resolve a canonical trailing effort as
	// either a separate adapter axis, a direct model-id variant, or a stale effort
	// on a model that may no longer support one.
	b.mu.Lock()
	resolution := b.resolveModelWriteLocked(modelID)
	effortConfigID := firstNonEmpty(b.effortConfigID, "effort")
	b.mu.Unlock()
	finishWrite := b.beginWorkassModelWrite(sessionID)
	defer finishWrite()
	res, err := b.request(ctx, "session/set_config_option", map[string]any{"sessionId": sessionID, "configId": "model", "value": resolution.modelValue}, 15*time.Second)
	if err != nil {
		return nil, err
	}
	// A successful model write is authoritative even when an adapter omits the
	// configOptions echo. Seed the bridge with that applied value before parsing
	// any returned model-specific option surface.
	b.mu.Lock()
	b.currentModel = stringValuePtr(resolution.modelValue)
	b.mu.Unlock()
	b.applyConfigOptionsForSession(sessionID, res["configOptions"], true, true)

	if resolution.separateAxis && resolution.effort != "" {
		b.mu.Lock()
		levels := append([]string(nil), b.axisEffortsByModel[resolution.baseModelID]...)
		effortConfigID = firstNonEmpty(b.effortConfigID, effortConfigID)
		supportedEffort := matchingStringFold(levels, resolution.effort)
		b.mu.Unlock()
		if supportedEffort != "" {
			effortRes, effortErr := b.request(ctx, "session/set_config_option", map[string]any{"sessionId": sessionID, "configId": effortConfigID, "value": supportedEffort}, 15*time.Second)
			if effortErr != nil {
				return nil, effortErr
			}
			b.applyConfigOptionsForSession(sessionID, effortRes["configOptions"], true, true)
			// Some adapters acknowledge without echoing options. Preserve the exact
			// successful effort write in that case.
			b.mu.Lock()
			b.currentModel = stringValuePtr(resolution.baseModelID)
			b.currentEffort = stringValuePtr(supportedEffort)
			b.efforts = append([]string(nil), b.axisEffortsByModel[resolution.baseModelID]...)
			b.mu.Unlock()
		}
	}

	b.mu.Lock()
	currentKey := resolution.modelValue
	if resolution.separateAxis {
		currentKey = resolution.baseModelID
		b.currentModel = stringValuePtr(resolution.baseModelID)
	}
	if levels, known := b.axisEffortsByModel[currentKey]; known {
		b.efforts = append([]string(nil), levels...)
		if len(levels) == 0 {
			b.currentEffort = nil
		}
	} else {
		b.efforts = nil
		b.currentEffort = nil
	}
	current := b.currentModelSelectionLocked()
	b.mu.Unlock()
	appliedModelID := currentKey
	if current != nil && strings.TrimSpace(*current) != "" {
		appliedModelID = strings.TrimSpace(*current)
	}
	b.rememberWorkassModelWrite(sessionID, appliedModelID)
	durableModelID, reason := b.durableModelWriteback(modelID, appliedModelID, resolution)
	b.rememberDurableModelSelection(sessionID, durableModelID)
	result := map[string]any{"currentModelId": durableModelID, "appliedModelId": appliedModelID}
	if reason != "" {
		result["modelWritebackReason"] = reason
	}
	return result, nil
}

func (b *Bridge) beginWorkassModelWrite(sessionID string) func() {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return func() {}
	}
	b.mu.Lock()
	if b.modelWriteInFlight == nil {
		b.modelWriteInFlight = make(map[string]int)
	}
	b.modelWriteInFlight[sessionID]++
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.modelWriteInFlight[sessionID] <= 1 {
			delete(b.modelWriteInFlight, sessionID)
		} else {
			b.modelWriteInFlight[sessionID]--
		}
	}
}

func (b *Bridge) rememberWorkassModelWrite(sessionID, appliedModelID string) {
	sessionID = strings.TrimSpace(sessionID)
	appliedModelID = strings.TrimSpace(appliedModelID)
	if sessionID == "" || appliedModelID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lastWorkassModelWrite == nil {
		b.lastWorkassModelWrite = make(map[string]string)
	}
	b.lastWorkassModelWrite[sessionID] = appliedModelID
}

func (b *Bridge) rememberDurableModelSelection(sessionID, modelID string) {
	sessionID = strings.TrimSpace(sessionID)
	modelID = strings.TrimSpace(modelID)
	if sessionID == "" || modelID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.durableModelSelection == nil {
		b.durableModelSelection = make(map[string]string)
	}
	b.durableModelSelection[sessionID] = modelID
}

func (b *Bridge) durableModelWriteback(requested, applied string, resolution modelWriteResolution) (string, string) {
	requested = strings.TrimSpace(requested)
	applied = strings.TrimSpace(applied)
	if requested == "" {
		return applied, ""
	}
	if applied == "" || applied == requested || resolution.effort == "" || resolution.baseModelID == "" {
		return firstNonEmpty(applied, requested), ""
	}
	b.mu.Lock()
	axisLevels, axisKnown := b.axisEffortsByModel[resolution.baseModelID]
	variantMatched := matchingStringFold(b.variantEffortsByModel[resolution.baseModelID], resolution.effort) != ""
	b.mu.Unlock()
	if !axisKnown && !variantMatched {
		return requested, "effort-axis-undiscovered"
	}
	if axisKnown && matchingStringFold(axisLevels, resolution.effort) == "" && !variantMatched {
		reason := "unsupported-effort"
		b.opts.Logf("model selection downgraded", map[string]any{
			"original": requested, "applied": applied, "reason": reason,
		})
		return applied, reason
	}
	return applied, ""
}

func (b *Bridge) SetMode(ctx context.Context, sessionID, modeID string) (map[string]any, error) {
	if _, err := b.Initialize(ctx); err != nil {
		return nil, err
	}
	b.mu.Lock()
	modeConfigID := firstNonEmpty(b.modeConfigID, "mode")
	b.mu.Unlock()
	res, err := b.request(ctx, "session/set_config_option", map[string]any{"sessionId": sessionID, "configId": modeConfigID, "value": modeID}, 15*time.Second)
	if err != nil {
		return nil, err
	}
	b.applyConfigOptionsForSession(sessionID, res["configOptions"], false, true)
	b.mu.Lock()
	b.currentMode = &modeID
	b.mu.Unlock()
	return map[string]any{"currentModeId": modeID}, nil
}

type sessionControlResult struct {
	CurrentModelID string
	AppliedModelID string
	CurrentModeID  string
}

func (b *Bridge) ensureSessionControls(ctx context.Context, sessionID, modelID, modeID string) (sessionControlResult, error) {
	modelID = strings.TrimSpace(modelID)
	modeID = strings.TrimSpace(modeID)
	b.mu.Lock()
	currentModel := ""
	currentMode := ""
	modes := append([]Mode(nil), b.modes...)
	if selected := b.currentModelSelectionLocked(); selected != nil {
		currentModel = strings.TrimSpace(*selected)
	}
	if b.currentMode != nil {
		currentMode = strings.TrimSpace(*b.currentMode)
	}
	b.mu.Unlock()
	result := sessionControlResult{CurrentModelID: currentModel, AppliedModelID: currentModel, CurrentModeID: currentMode}
	var controlErrs []error
	if modelID != "" && modelID != currentModel {
		setResult, err := b.SetModel(ctx, sessionID, modelID)
		if err != nil {
			controlErrs = append(controlErrs, fmt.Errorf("model control: %w", err))
			result = b.currentSessionControlResult()
		} else {
			result.CurrentModelID = durableModelIDFromSetResult(setResult, modelID)
			result.AppliedModelID = appliedModelIDFromSetResult(setResult, result.CurrentModelID)
		}
	}
	resolvedMode := compatibleModeID(modeID, modes, currentMode)
	if resolvedMode != "" && resolvedMode != currentMode {
		if _, err := b.SetMode(ctx, sessionID, resolvedMode); err != nil {
			controlErrs = append(controlErrs, fmt.Errorf("mode control: %w", err))
		} else {
			result.CurrentModeID = resolvedMode
		}
	}
	return result, errors.Join(controlErrs...)
}

func (b *Bridge) currentSessionControlResult() sessionControlResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	currentModel := ""
	currentMode := ""
	if selected := b.currentModelSelectionLocked(); selected != nil {
		currentModel = strings.TrimSpace(*selected)
	}
	if b.currentMode != nil {
		currentMode = strings.TrimSpace(*b.currentMode)
	}
	return sessionControlResult{CurrentModelID: currentModel, AppliedModelID: currentModel, CurrentModeID: currentMode}
}

// resolveModelWriteLocked decides how a Workass selection maps onto the
// adapter's model axis. Caller must hold b.mu.
func (b *Bridge) resolveModelWriteLocked(modelID string) modelWriteResolution {
	modelID = strings.TrimSpace(modelID)
	out := modelWriteResolution{requested: modelID, modelValue: modelID, baseModelID: modelID}
	for _, model := range b.models {
		if strings.TrimSpace(model.ModelID) == modelID {
			return out
		}
	}
	match := effortModelIDPattern.FindStringSubmatch(modelID)
	if match == nil {
		return out
	}
	base := strings.TrimSpace(match[1])
	canonical, canonicalOK := canonicalEffortKey(match[2])
	if base == "" || !canonicalOK {
		return out
	}
	out.baseModelID = base
	out.effort = canonical
	var baseModel *Model
	for i := range b.models {
		if strings.TrimSpace(b.models[i].ModelID) == base {
			baseModel = &b.models[i]
			break
		}
	}
	if baseModel == nil {
		// A restore-time apply can race catalog discovery, so the composite's
		// base row may not be in b.models yet (prod 2026-07-27: a binding's
		// claude-fable-5[1m][max] went to the adapter raw and was refused).
		// Claude and Codex own a separate effort axis, so trust the canonical
		// split and let the adapter judge the base id — apply is best-effort,
		// and the write's configOptions echo completes discovery.
		if providerAdapterForID(b.providerID).model.SeparateEffortAxis {
			out.modelValue = base
			out.separateAxis = true
		}
		return out
	}
	axisLevels, axisKnown := b.axisEffortsByModel[base]
	out.axisKnown = axisKnown
	if axisEffort := matchingStringFold(axisLevels, canonical); axisEffort != "" {
		out.modelValue = base
		out.effort = axisEffort
		out.separateAxis = true
		return out
	}
	separateAxisProvider := providerAdapterForID(b.providerID).model.SeparateEffortAxis
	if !separateAxisProvider {
		for _, levels := range b.axisEffortsByModel {
			if len(levels) > 0 {
				separateAxisProvider = true
				break
			}
		}
	}
	// A catalog effort without a matching separate-axis capability came from
	// direct model-id variants. Preserve the composite byte-for-byte only for an
	// adapter that has never exposed a separate effort axis. Codex and Claude
	// also advertise composite catalog rows, but their config write contract is
	// base model first followed by the provider's effort option.
	if variantEffort := matchingStringFold(b.variantEffortsByModel[base], canonical); variantEffort != "" && !separateAxisProvider {
		out.effort = variantEffort
		out.variantMatched = true
		return out
	}
	if !separateAxisProvider {
		// A custom adapter that has never exposed an effort axis owns its model ids
		// byte-for-byte. Preserve unknown canonical brackets for compatibility.
		return out
	}
	// The base exists but does not currently advertise this effort. Switch the
	// valid base first: its returned configOptions may discover an effort axis, or
	// may authoritatively confirm that this is a stale unsupported suffix.
	out.modelValue = base
	out.separateAxis = true
	return out
}

func matchingStringFold(values []string, requested string) string {
	requested = strings.TrimSpace(requested)
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), requested) {
			return value
		}
	}
	return ""
}

func stringValuePtr(value string) *string {
	out := value
	return &out
}

// currentModelSelectionLocked returns the Workass UI/persistence selection.
// Adapters expose model + effort as separate options, but the frozen renderer
// contract represents them as base[effort]. Caller must hold b.mu.
func (b *Bridge) currentModelSelectionLocked() *string {
	if b.currentModel == nil {
		return nil
	}
	modelID := b.canonicalProviderModelIDLocked(*b.currentModel)
	if modelID == "" {
		return nil
	}
	if _, _, split := splitEffortSuffix(modelID, b.efforts); split {
		return &modelID
	}
	if b.currentEffort != nil {
		effort := strings.TrimSpace(*b.currentEffort)
		for _, known := range b.efforts {
			if strings.EqualFold(known, effort) {
				selection := modelID + "[" + known + "]"
				return &selection
			}
		}
	}
	return &modelID
}

func (b *Bridge) Prompt(ctx context.Context, sessionID, promptText string) (PromptResult, error) {
	return b.promptForJob(ctx, sessionID, nil, "", promptText, nil)
}

func (b *Bridge) promptSystem(ctx context.Context, sessionID, promptText string) (PromptResult, error) {
	job := &Job{Status: "running", SessionID: sessionID, internal: true}
	job.touchActivity()
	b.setJobForSession(sessionID, job)
	defer func() {
		b.flushJobBuffers(job)
		b.clearJobForSession(sessionID, job)
	}()
	return b.promptForJob(ctx, sessionID, job, "", promptText, nil)
}

func (b *Bridge) supportsPromptReconciliation() bool {
	return b.hasProviderCapability("workassTurnReconcileRequest")
}

func (b *Bridge) interruptForQueuedSteer(sessionID string) bool {
	return b.notify("session/cancel", map[string]any{"sessionId": sessionID})
}

func boolMapField(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	v, ok := m[key].(bool)
	return ok && v
}

const promptReconcileFailureLimit = 3

// requestPrompt waits for the ordinary ACP session/prompt response while a
// capability-gated watchdog asks the provider adapter to reconcile its native
// turn state after a quiet interval. Silence is never treated as completion:
// the adapter must consult its authoritative provider thread and report a
// terminal state. A terminal native turn should release the original prompt;
// if it does not, or the advertised liveness request repeatedly fails, the
// wedged bridge is recycled so the visible job cannot remain running forever.
func (b *Bridge) requestPrompt(ctx context.Context, sessionID string, job *Job, params map[string]any) (map[string]any, error) {
	if !b.supportsPromptReconciliation() {
		return b.request(ctx, "session/prompt", params, 0)
	}

	promptCtx, cancelPrompt := context.WithCancel(ctx)
	defer cancelPrompt()
	promptResult := make(chan rpcResult, 1)
	go func() {
		value, err := b.request(promptCtx, "session/prompt", params, 0)
		promptResult <- rpcResult{value: value, err: err}
	}()

	ticker := time.NewTicker(b.opts.PromptReconcileInterval)
	defer ticker.Stop()
	var terminalTimer *time.Timer
	var terminalDeadline <-chan time.Time
	defer func() {
		if terminalTimer != nil {
			terminalTimer.Stop()
		}
	}()
	failedChecks := 0

	closeWedgedBridge := func(reason string) {
		cause := errors.New(reason)
		b.opts.Logf("ACP prompt reconciliation recycled wedged bridge", map[string]any{
			"providerId": b.ProviderID(), "reason": reason,
		})
		b.manager.handleUnexpectedBridgeExit(b, cause)
	}

	for {
		select {
		case result := <-promptResult:
			return result.value, result.err
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-terminalDeadline:
			terminalDeadline = nil
			closeWedgedBridge("provider confirmed a terminal turn but the ACP prompt did not return")
		case <-ticker.C:
			if job != nil && (job.waitingPermission.Load() || job.inactiveFor(time.Now()) < b.opts.PromptReconcileInterval) {
				failedChecks = 0
				continue
			}
			checkCtx, cancelCheck := context.WithTimeout(ctx, b.opts.PromptReconcileTimeout)
			status, err := b.request(checkCtx, "_workass/turn/reconcile", map[string]any{
				"sessionId": sessionID,
			}, b.opts.PromptReconcileTimeout)
			cancelCheck()
			if err != nil {
				if job != nil && job.inactiveFor(time.Now()) < b.opts.PromptReconcileInterval {
					failedChecks = 0
					continue
				}
				failedChecks++
				b.opts.Logf("ACP prompt reconciliation check failed", map[string]any{
					"providerId": b.ProviderID(), "attempt": failedChecks, "error": err.Error(),
				})
				if failedChecks >= promptReconcileFailureLimit {
					closeWedgedBridge("provider turn status was unavailable after repeated reconciliation checks")
				}
				continue
			}
			failedChecks = 0
			terminal, _ := status["terminal"].(bool)
			if terminal && terminalTimer == nil {
				terminalTimer = time.NewTimer(b.opts.PromptTerminalGrace)
				terminalDeadline = terminalTimer.C
				b.opts.Logf("ACP prompt terminal state reconciled", map[string]any{
					"providerId": b.ProviderID(), "status": asString(status["status"]),
				})
			}
		}
	}
}

func (b *Bridge) Steer(sessionID, promptText string, images []any, clientUserMessageID string) map[string]any {
	job := b.jobForSession(sessionID)
	if job == nil || job.Status != "running" || job.internal {
		return map[string]any{"ok": false, "queued": false, "error": "No hay una respuesta en curso para steerear."}
	}
	prompt, promptErr := b.promptBlocks(promptText, images)
	if promptErr != nil {
		return map[string]any{"ok": false, "live": false, "queued": false, "error": promptErr.Error()}
	}
	request := providerSteerRequest{
		sessionID:           sessionID,
		prompt:              prompt,
		clientUserMessageID: strings.TrimSpace(clientUserMessageID),
	}
	return providerAdapterForID(b.ProviderID()).delivery.Steer(b, request).payload()
}
func (b *Bridge) promptForJob(ctx context.Context, sessionID string, job *Job, operationID, promptText string, images []any) (PromptResult, error) {
	b.mu.Lock()
	_, ok := b.sessions[sessionID]
	state := b.state
	b.mu.Unlock()
	if !ok || state == StateHibernated {
		return PromptResult{}, errors.New("La sesión ACP expiró. Cerrá y reabrí la pestaña del chat.")
	}
	b.promptMu.Lock()
	defer b.promptMu.Unlock()
	if job != nil && b.manager.jobCancelled(job) {
		return PromptResult{}, errors.New("Turno cancelado antes de enviarse al agente ACP.")
	}
	directJob := (*Job)(nil)
	if job == nil {
		directJob = &Job{Status: "running", SessionID: sessionID}
		b.setJobForSession(sessionID, directJob)
	}
	prompt, promptErr := b.promptBlocks(promptText, images)
	if promptErr != nil {
		return PromptResult{}, promptErr
	}
	dispatch, dispatchErr := b.manager.prepareNativeTurn(job, operationID, prompt)
	if dispatchErr != nil {
		return PromptResult{}, dispatchErr
	}
	params := map[string]any{
		"sessionId": sessionID,
		"prompt":    prompt,
	}
	if operationID = strings.TrimSpace(operationID); operationID != "" {
		params["clientUserMessageId"] = operationID
	}
	res, err := b.requestPrompt(ctx, sessionID, job, params)
	if directJob != nil {
		b.clearJobForSession(sessionID, directJob)
	}
	if err != nil {
		return PromptResult{}, err
	}
	if err := b.manager.finishNativeTurn(job, dispatch, res); err != nil {
		return PromptResult{}, err
	}
	b.manager.recordPlanUsageCapture(sessionID, b.ProviderID(), res)
	b.manager.schedulePlanUsageRefresh(b, sessionID)
	return PromptResult{StopReason: asString(res["stopReason"]), Raw: res, Output: b.manager.outputForJob(job)}, nil
}

func (m *Manager) outputForJob(job *Job) string {
	if job == nil {
		return ""
	}
	m.jobMu.Lock()
	defer m.jobMu.Unlock()
	return job.output.String()
}

func (b *Bridge) markSeeded(sessionID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.seededSessions[sessionID]; ok {
		return false
	}
	b.seededSessions[sessionID] = struct{}{}
	return true
}

func (b *Bridge) isSeeded(sessionID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.seededSessions[sessionID]
	return ok
}

func (b *Bridge) promptBlocks(promptText string, images []any) ([]any, error) {
	blocks := []any{}
	trailingNotice := ""
	if len(images) > 0 {
		b.mu.Lock()
		imageSupport := b.imageSupport
		b.mu.Unlock()
		if !imageSupport {
			return nil, errors.New("the selected agent does not support image input; the attachment was not sent as a text-only turn")
		}
		if len(images) > maxToolResultImages {
			return nil, fmt.Errorf("too many attached images: got %d, maximum is %d", len(images), maxToolResultImages)
		}
		total := 0
		for index, raw := range images {
			img := mapFromAny(raw)
			mimeType := strings.ToLower(strings.TrimSpace(firstNonEmpty(asString(img["mimeType"]), asString(img["mime_type"]))))
			data := strings.TrimSpace(asString(img["data"]))
			if strings.HasPrefix(data, "data:") {
				comma := strings.IndexByte(data, ',')
				if comma <= 5 || !strings.Contains(strings.ToLower(data[:comma]), ";base64") {
					return nil, fmt.Errorf("attached image %d has an invalid data URL", index+1)
				}
				if mimeType == "" {
					mimeType = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(data[:comma], "data:"), ";base64")))
				}
				data = strings.TrimSpace(data[comma+1:])
			}
			if !safeToolImageMIME(mimeType) || data == "" {
				return nil, fmt.Errorf("attached image %d is not a supported PNG, JPEG, WebP, or GIF raster", index+1)
			}
			if len(data) > maxToolResultImageBytes || total+len(data) > maxToolResultTotalBytes {
				return nil, fmt.Errorf("attached image %d exceeds Workass's bounded image payload", index+1)
			}
			total += len(data)
			blocks = append(blocks, map[string]any{"type": "image", "mimeType": mimeType, "data": data})
		}
		notice := fmt.Sprintf(
			"[Workass attachment context]\nThe current human-authored message includes %d attached image(s). Inspect every attached image directly before answering; do not claim that no image was provided. This internal notice does not set the response language.",
			len(blocks),
		)
		if strings.HasPrefix(strings.TrimSpace(promptText), "/") {
			// A slash command is recognized by the leading slash of the message
			// text; prefixing the notice would stop the CLI from seeing the
			// command (claude-commands-surface spec D7/§5). The notice trails
			// the prompt as its own text block instead.
			trailingNotice = notice
		} else {
			promptText = notice + "\n\n" + promptText
		}
	}
	if promptText != "" || len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": promptText})
	}
	if trailingNotice != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": trailingNotice})
	}
	return blocks, nil
}

func (b *Bridge) applyConfigOptions(raw any, broadcast bool) {
	b.applyConfigOptionsForSession("", raw, broadcast, false)
}

// canonicalProviderModelIDLocked resolves a synthetic default alias only when
// the registered adapter declares that semantic. Caller must hold b.mu.
func (b *Bridge) canonicalProviderModelIDLocked(modelID string) string {
	modelID = strings.TrimSpace(modelID)
	if !providerAdapterForID(b.providerID).model.SyntheticDefaultAlias ||
		!strings.EqualFold(modelID, "default") ||
		strings.TrimSpace(b.syntheticDefaultModelAlias) == "" {
		return modelID
	}
	return strings.TrimSpace(b.syntheticDefaultModelAlias)
}

func (b *Bridge) applyConfigOptionsForSession(sessionID string, raw any, broadcast bool, workassWrite bool) {
	options, ok := raw.([]any)
	if !ok {
		return
	}
	var models []Model
	var modes []Mode
	var currentModel *string
	var currentMode *string
	var modeConfigID string
	var efforts []string
	var currentEffort *string
	var effortConfigID string
	modelSeen := false
	effortSeen := false
	for _, item := range options {
		opt := mapFromAny(item)
		id := asString(opt["id"])
		category := asString(opt["category"])
		values, _ := opt["options"].([]any)
		if id == "model" || category == "model" {
			modelSeen = true
			for _, rawValue := range values {
				value := mapFromAny(rawValue)
				modelID := asString(value["value"])
				if modelID != "" {
					models = append(models, providerCatalogModel(b.providerID, Model{
						ModelID: modelID,
						Name:    firstNonEmpty(asString(value["name"]), modelID),
					}, asString(value["description"])))
				}
			}
			if cur := asString(opt["currentValue"]); cur != "" {
				currentModel = &cur
			}
		}
		if id == "mode" || category == "mode" {
			if trimmed := strings.TrimSpace(id); trimmed != "" {
				modeConfigID = trimmed
			}
			for _, rawValue := range values {
				value := mapFromAny(rawValue)
				modeID := asString(value["value"])
				if modeID != "" {
					modes = append(modes, Mode{ID: modeID, Name: firstNonEmpty(asString(value["name"]), modeID)})
				}
			}
			if cur := asString(opt["currentValue"]); cur != "" {
				currentMode = &cur
			}
		}
		// Reasoning effort is a distinct axis (Claude's "thought_level"). Keep only
		// the canonical effort levels so the slider shares Codex's vocabulary; the
		// adapter's "default" pseudo-level is the no-explicit-effort state, which the
		// renderer already represents by the absence of an effort suffix.
		if id == "effort" || category == "thought_level" {
			effortSeen = true
			if trimmed := strings.TrimSpace(id); trimmed != "" {
				effortConfigID = trimmed
			}
			for _, rawValue := range values {
				value := mapFromAny(rawValue)
				if key, ok := canonicalEffortKey(asString(value["value"])); ok {
					efforts = appendMissingStrings(efforts, key)
				}
			}
			if cur, ok := canonicalEffortKey(asString(opt["currentValue"])); ok {
				currentEffort = &cur
			}
		}
	}
	syntheticAliasProvider := providerAdapterForID(b.providerID).model.SyntheticDefaultAlias
	syntheticDefaultAlias, syntheticDefaultAliasProven := "", false
	if syntheticAliasProvider && modelSeen {
		syntheticDefaultAlias, syntheticDefaultAliasProven = providerAdapterForID(b.providerID).catalog.ResolveSyntheticDefault(models)
	}
	changed := false
	b.mu.Lock()
	if b.axisEffortsByModel == nil {
		b.axisEffortsByModel = make(map[string][]string)
	}
	if b.variantEffortsByModel == nil {
		b.variantEffortsByModel = make(map[string][]string)
	}
	if syntheticAliasProvider && modelSeen {
		if syntheticDefaultAliasProven {
			b.syntheticDefaultModelAlias = syntheticDefaultAlias
		} else {
			b.syntheticDefaultModelAlias = ""
		}
	}
	effectiveModel := ""
	if currentModel != nil {
		effectiveModel = b.canonicalProviderModelIDLocked(*currentModel)
	} else if b.currentModel != nil {
		effectiveModel = b.canonicalProviderModelIDLocked(*b.currentModel)
	}
	if syntheticAliasProvider && strings.EqualFold(effectiveModel, "default") {
		// An unresolved synthetic alias is not an explicit model capability.
		// Keep the current selection literal, but never invent an effort owner.
		effectiveModel = ""
	}
	if models != nil {
		models = normalizeProviderCatalogModels(b.providerID, models)
		b.rememberVariantEffortsLocked(models)
		b.models = models
		changed = true
	}
	if modes != nil {
		b.modes = modes
		changed = true
	}
	if modeConfigID != "" {
		b.modeConfigID = modeConfigID
	}
	if effortSeen {
		if effortConfigID != "" {
			b.effortConfigID = effortConfigID
		}
		if effectiveModel != "" {
			previous, known := b.axisEffortsByModel[effectiveModel]
			if !known || !sameStringSlice(previous, efforts) {
				b.axisEffortsByModel[effectiveModel] = append([]string(nil), efforts...)
				changed = true
			}
		}
		if !sameStringSlice(b.efforts, efforts) {
			b.efforts = append([]string(nil), efforts...)
			changed = true
		}
		b.currentEffort = currentEffort
	} else if modelSeen {
		// A model-bearing configOptions list is the adapter's complete control
		// surface for its current model. If thought_level is absent, remember that
		// model as explicitly unsupported and clear any previous model's effort.
		if effectiveModel != "" {
			previous, known := b.axisEffortsByModel[effectiveModel]
			if !known || len(previous) != 0 {
				b.axisEffortsByModel[effectiveModel] = []string{}
				changed = true
			}
		}
		if len(b.efforts) != 0 {
			changed = true
		}
		b.efforts = nil
		b.currentEffort = nil
	}
	if currentModel != nil {
		b.currentModel = currentModel
	}
	if currentMode != nil {
		b.currentMode = currentMode
	}
	if b.refreshCatalogModelEffortsLocked() {
		changed = true
	}
	capturedModelID := ""
	if broadcast && !workassWrite && sessionID != "" && currentModel != nil {
		if currentSelection := b.currentModelSelectionLocked(); currentSelection != nil {
			selection := strings.TrimSpace(*currentSelection)
			inFlight := b.modelWriteInFlight[sessionID] > 0
			lastWorkassWrite := strings.TrimSpace(b.lastWorkassModelWrite[sessionID])
			if selection != "" && !inFlight && selection != lastWorkassWrite {
				capturedModelID = selection
				if b.durableModelSelection == nil {
					b.durableModelSelection = make(map[string]string)
				}
				b.durableModelSelection[sessionID] = selection
			}
		}
	}
	modelsCopy := append([]Model(nil), b.models...)
	modesCopy := append([]Mode(nil), b.modes...)
	b.mu.Unlock()
	if changed {
		b.manager.updateProviderCatalogFromBridge(b, modelsCopy, modesCopy)
	}
	if changed && broadcast {
		b.manager.EmitCatalog(context.Background())
	}
	if capturedModelID != "" {
		b.manager.captureAdapterModelSelection(b, sessionID, capturedModelID)
	}
}

func (b *Bridge) rememberVariantEffortsLocked(models []Model) {
	for _, model := range models {
		if len(model.Efforts) == 0 {
			continue
		}
		b.variantEffortsByModel[model.ModelID] = append([]string(nil), model.Efforts...)
	}
}

// refreshCatalogModelEffortsLocked rebuilds the renderer-facing effort list
// from its two distinct wire representations. Caller must hold b.mu.
func (b *Bridge) refreshCatalogModelEffortsLocked() bool {
	changed := false
	for i := range b.models {
		combined := append([]string(nil), b.variantEffortsByModel[b.models[i].ModelID]...)
		for _, effort := range b.axisEffortsByModel[b.models[i].ModelID] {
			combined = appendMissingStrings(combined, effort)
		}
		if !sameStringSlice(b.models[i].Efforts, combined) {
			b.models[i].Efforts = combined
			changed = true
		}
	}
	return changed
}

func (b *Bridge) applyAvailableModels(raw any) {
	models := modelsFromAvailableModelsForProvider(raw, b.providerID)
	if len(models) == 0 {
		return
	}
	hasEffortVariants := catalogModelsContainEffortVariants(models)
	models = normalizeProviderCatalogModels(b.providerID, models)
	if len(models) == 0 {
		return
	}
	changed := false
	b.mu.Lock()
	if b.axisEffortsByModel == nil {
		b.axisEffortsByModel = make(map[string][]string)
	}
	if b.variantEffortsByModel == nil {
		b.variantEffortsByModel = make(map[string][]string)
	}
	b.rememberVariantEffortsLocked(models)
	if hasEffortVariants || len(b.models) == 0 {
		b.models = models
		changed = true
	}
	if b.refreshCatalogModelEffortsLocked() {
		changed = true
	}
	modelsCopy := append([]Model(nil), b.models...)
	modesCopy := append([]Mode(nil), b.modes...)
	b.mu.Unlock()
	if changed {
		b.manager.updateProviderCatalogFromBridge(b, modelsCopy, modesCopy)
	}
}

func (b *Bridge) applySessionModels(raw any) {
	payload := mapFromAny(raw)
	if len(payload) == 0 {
		return
	}
	b.applyAvailableModels(payload["availableModels"])
	if current := asString(payload["currentModelId"]); current != "" {
		b.mu.Lock()
		b.currentModel = &current
		b.mu.Unlock()
	}
}

func copyStringPtr(v *string) *string {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}
