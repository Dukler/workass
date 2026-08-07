package acp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	maxConcurrentSubagentsPerTurn = 8
	maxConcurrentSubagentsGlobal  = 16
	maxCompletedSubagentHistory   = 128
)

// SubagentSpawnOptions is the provider-neutral contract used by the injected
// workass-agent MCP server. ParentChatID/ParentTabID identify the calling ACP
// session without exposing a transient session id before session/new returns.
type SubagentSpawnOptions struct {
	OwnerKey         string `json:"ownerKey,omitempty"`
	ParentChatID     string `json:"parentChatId,omitempty"`
	ParentTabID      string `json:"parentTabId,omitempty"`
	RootJobIDHint    string `json:"rootJobIdHint,omitempty"`
	Prompt           string `json:"prompt"`
	Label            string `json:"label,omitempty"`
	ProviderID       string `json:"providerId,omitempty"`
	ModelID          string `json:"modelId,omitempty"`
	Effort           string `json:"effort,omitempty"`
	ModeID           string `json:"modeId,omitempty"`
	CWD              string `json:"cwd,omitempty"`
	Profile          string `json:"profile,omitempty"`
	PermissionIntent string `json:"permissionIntent,omitempty"`
	RetryOf          string `json:"retryOf,omitempty"`
}

type subagentFollowup struct {
	ID   int64
	Text string
}

// SubagentAttention is a durable-in-memory notification for the coordinator.
// It is separate from Phase because a fast user decision may resolve the live
// wait before the parent model receives its next tool result.
type SubagentAttention struct {
	Kind        string `json:"kind"`
	Sequence    int64  `json:"sequence"`
	Message     string `json:"message"`
	Active      bool   `json:"active"`
	RequestedAt string `json:"requestedAt"`
	ResolvedAt  string `json:"resolvedAt,omitempty"`
	// Who can say yes: "parent" when this chat's own mode already permits the
	// action, "human" when only the card on screen can grant it. Without this a
	// parent's only rational move is to keep waiting, which is exactly wrong
	// when the answer has to come from someone who may not be at the machine.
	// Denying is always the parent's to do, in either case.
	GrantableBy string `json:"grantableBy,omitempty"`
}

func (m *Manager) newAgentOwnerKeyLocked() string {
	m.agentOwnerSeq++
	return fmt.Sprintf("agent-owner-%d-%d", time.Now().UnixNano(), m.agentOwnerSeq)
}

func (m *Manager) bindAgentOwnerLocked(ownerKey, chatID, tabID string) {
	ownerKey = strings.TrimSpace(ownerKey)
	if ownerKey == "" {
		return
	}
	m.agentOwners[ownerKey] = agentOwnerBinding{ChatID: strings.TrimSpace(chatID), TabID: strings.TrimSpace(tabID)}
}

func (m *Manager) ownerBinding(ownerKey, chatID, tabID string) (string, string, bool) {
	ownerKey = strings.TrimSpace(ownerKey)
	if ownerKey == "" {
		return "", "", false
	}
	m.mu.Lock()
	binding, ok := m.agentOwners[ownerKey]
	m.mu.Unlock()
	if !ok {
		return "", "", false
	}
	return binding.ChatID, binding.TabID, true
}

// ValidateAgentOwner authenticates the opaque capability injected into one ACP
// session. Chat-control tools may target other conversations, but only a real
// Workass-owned session may call them; the caller identity itself must match
// exactly and can never be retargeted by tool arguments.
func (m *Manager) ValidateAgentOwner(ownerKey, chatID, tabID string) bool {
	boundChat, boundTab, ok := m.ownerBinding(ownerKey, chatID, tabID)
	return ok && boundChat == strings.TrimSpace(chatID) && boundTab == strings.TrimSpace(tabID)
}

// SubagentRun is a snapshot-safe record. cancel is daemon-private and never
// crosses a log/UI/API boundary.
type SubagentRun struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Status     string `json:"status"`
	ProviderID string `json:"providerId"`
	ModelID    string `json:"modelId,omitempty"`
	Effort     string `json:"effort,omitempty"`
	// ModelLabel is the friendly model+effort combo for the Turnos chip
	// ("Opus4.8-xhigh"): the catalog display name with spaces stripped, joined to
	// effort with a dash. Resolved once at spawn from the provider catalog.
	ModelLabel string `json:"modelLabel,omitempty"`
	ModeID     string `json:"modeId,omitempty"`
	CWD        string `json:"cwd"`
	Profile    string `json:"profile,omitempty"`
	RetryOf    string `json:"retryOf,omitempty"`
	Phase      string `json:"phase"`
	// NeedsAttention is true when the coordinator must stop waiting and surface
	// a child-owned decision to the user. Permission requests are the first
	// attention kind; Phase and LatestActivity retain the exact bounded reason.
	NeedsAttention   bool               `json:"needsAttention,omitempty"`
	Attention        *SubagentAttention `json:"attention,omitempty"`
	LatestActivity   string             `json:"latestActivity,omitempty"`
	LastActivityAt   string             `json:"lastActivityAt,omitempty"`
	ProgressSequence int64              `json:"progressSequence"`
	ElapsedMs        int64              `json:"elapsedMs"`
	// ParentJobID is the immediate spawning job and is the authorization
	// boundary for list/wait/cancel until a run is adopted by its owning chat.
	// RootJobID is the visible Workass turn used only for Turnos event routing
	// and explicit teardown.
	ParentJobID                string `json:"parentJobId"`
	RootJobID                  string `json:"rootJobId"`
	Adopted                    bool   `json:"adopted,omitempty"`
	AdoptedAt                  string `json:"adoptedAt,omitempty"`
	ParentSessionID            string `json:"parentSessionId,omitempty"`
	RootSessionID              string `json:"rootSessionId,omitempty"`
	SessionID                  string `json:"sessionId,omitempty"`
	JobID                      string `json:"jobId,omitempty"`
	StartedAt                  string `json:"startedAt"`
	FinishedAt                 string `json:"finishedAt,omitempty"`
	Result                     string `json:"result,omitempty"`
	Error                      string `json:"error,omitempty"`
	StopReason                 string `json:"stopReason,omitempty"`
	ResultTruncated            bool   `json:"resultTruncated,omitempty"`
	ErrorTruncated             bool   `json:"errorTruncated,omitempty"`
	ReceiptID                  string `json:"receiptId,omitempty"`
	cancelled                  bool
	cancel                     context.CancelFunc
	done                       chan struct{}
	prompt                     string
	parentChatID               string
	parentTabID                string
	followups                  []subagentFollowup
	followupSeq                int64
	acceptingMessages          bool
	receiptCommitted           bool
	attentionDeliveredSequence int64
}

// AgentCatalog exposes the exact daemon-owned provider/model/effort/mode
// registry to an agent. No model ids are guessed from config files.
func (m *Manager) AgentCatalog(ctx context.Context, ownerKey, parentChatID, parentTabID string) (map[string]any, error) {
	parent := m.runningJobForOwner(ownerKey, parentChatID, parentTabID)
	if parent == nil {
		return nil, errors.New("no running Workass turn owns this agent catalog request")
	}
	return m.agentCatalogV2(ctx, parent), nil
}

// SpawnSubagent starts one explicitly coordinated ACP session/engine and
// returns immediately. Status/wait/cancel are separate operations so a parent
// can fan out sequential MCP calls without relying on parallel tool dispatch.
func (m *Manager) SpawnSubagent(ctx context.Context, opts SubagentSpawnOptions) (SubagentRun, error) {
	if err := m.beginWorkAdmission(); err != nil {
		return SubagentRun{}, err
	}
	defer m.endWorkAdmission()
	prompt := strings.TrimSpace(opts.Prompt)
	if prompt == "" {
		return SubagentRun{}, errors.New("subagent prompt is required")
	}
	ownerChatID, ownerTabID, parent, ownerOK := m.subagentOwnerContext(opts.OwnerKey, opts.ParentChatID, opts.ParentTabID)
	if !ownerOK {
		return SubagentRun{}, errors.New("no running Workass turn owns this subagent request")
	}
	defaults := parent
	if defaults == nil {
		defaults = m.subagentParentDefaults(ownerChatID, ownerTabID)
	}

	providerID, modelID, effort, modeID, err := m.resolveSubagentSelection(ctx, defaults, opts)
	if err != nil {
		return SubagentRun{}, err
	}
	modelLabel := subagentModelLabel(m.subagentModelName(ctx, providerID, modelID), modelID, effort)
	cwd := strings.TrimSpace(opts.CWD)
	if cwd == "" || strings.EqualFold(cwd, "inherit") {
		cwd = strings.TrimSpace(defaults.CWD)
	}
	if cwd == "" {
		return SubagentRun{}, errors.New("subagent cwd is required because the owning chat has no readable working directory")
	}
	if !filepath.IsAbs(cwd) && m.opts.RootDir != "" {
		cwd = filepath.Join(m.opts.RootDir, cwd)
	}
	info, statErr := os.Stat(cwd)
	if statErr != nil || !info.IsDir() {
		return SubagentRun{}, fmt.Errorf("subagent cwd is not a readable directory: %s", cwd)
	}
	label := strings.TrimSpace(opts.Label)
	if label == "" {
		label = compactText(prompt, 80)
	}
	if label == "" {
		label = "Subagent"
	}

	now := time.Now().UTC()
	m.mu.Lock()
	activeParent := parent != nil && m.runningOwnerJobLocked(parent, ownerChatID, ownerTabID)
	parentJobID := ""
	rootJobID := strings.TrimSpace(opts.RootJobIDHint)
	parentSessionID := defaults.SessionID
	rootSessionID := firstNonEmpty(defaults.VisibleSessionID, defaults.SessionID)
	adopted := !activeParent
	adoptedAt := ""
	if activeParent {
		parentJobID = parent.ID
		rootJobID = firstNonEmpty(parent.VisibleJobID, parent.ID)
		parentSessionID = parent.SessionID
		rootSessionID = firstNonEmpty(parent.VisibleSessionID, parent.SessionID)
	} else {
		adoptedAt = now.Format(time.RFC3339Nano)
	}
	running := 0
	runningGlobal := 0
	for _, run := range m.subagents {
		if run != nil && run.Status == "running" {
			runningGlobal++
			sameScope := rootJobID != "" && run.RootJobID == rootJobID
			if parentJobID == "" {
				sameScope = run.parentChatID == ownerChatID && run.parentTabID == ownerTabID
			}
			if sameScope {
				running++
			}
		}
	}
	if running >= maxConcurrentSubagentsPerTurn {
		m.mu.Unlock()
		return SubagentRun{}, fmt.Errorf("subagent fan-out limit reached (%d running for this turn)", maxConcurrentSubagentsPerTurn)
	}
	if runningGlobal >= maxConcurrentSubagentsGlobal {
		m.mu.Unlock()
		return SubagentRun{}, fmt.Errorf("global subagent fan-out limit reached (%d running)", maxConcurrentSubagentsGlobal)
	}
	m.pruneSubagentsLocked()
	m.subagentSeq++
	id := fmt.Sprintf("wa-subagent-%d-%d", now.UnixMilli(), m.subagentSeq)
	runCtx, cancel := context.WithCancel(context.Background())
	run := &SubagentRun{
		ID: id, Label: label, Status: "running", ProviderID: providerID,
		ModelID: modelID, Effort: effort, ModelLabel: modelLabel, ModeID: modeID, CWD: cwd,
		Profile: strings.TrimSpace(opts.Profile), Phase: "starting", LatestActivity: "Starting subagent",
		RetryOf:     strings.TrimSpace(opts.RetryOf),
		ParentJobID: parentJobID, RootJobID: rootJobID,
		Adopted: adopted, AdoptedAt: adoptedAt,
		ParentSessionID: parentSessionID, RootSessionID: rootSessionID,
		StartedAt: now.Format(time.RFC3339Nano), LastActivityAt: now.Format(time.RFC3339Nano),
		ProgressSequence: 1, cancel: cancel, done: make(chan struct{}), prompt: prompt,
		parentChatID: ownerChatID, parentTabID: ownerTabID,
		acceptingMessages: true,
	}
	m.subagents[id] = run
	snapshot := copySubagentRun(run)
	m.mu.Unlock()

	m.registerSubagentSpawnedWork(ownerTabID, ownerChatID, snapshot)
	brand := brandForProvider(providerID)
	if rootJobID != "" {
		m.emitSubagentHeader(rootJobID, id, label, brand, modelLabel, "in_progress", "")
	}
	go m.runSubagent(runCtx, run, prompt, brand)
	return snapshot, nil
}

// subagentChatIDPrefix marks a chat that belongs to a delegated child rather
// than to a person. Nothing renames it: other code already matches on the
// literal, and this only gives that literal a name.
const subagentChatIDPrefix = "subagent:"

func (m *Manager) runSubagent(runCtx context.Context, run *SubagentRun, prompt, brand string) {
	defer run.cancel()
	providerID, modelID, effort, modeID, cwd := run.ProviderID, run.ModelID, run.Effort, run.ModeID, run.CWD
	rootJobID, rootSessionID, id, label := run.RootJobID, run.RootSessionID, run.ID, run.Label
	m.updateSubagentActivity(id, "initializing", "Initializing ACP session")

	childChatID := subagentChatIDPrefix + id
	childTabID := childChatID
	info, err := m.NewSession(runCtx, SessionOptions{
		CWD: cwd, BridgeKey: childChatID, ChatID: childChatID, TabID: childTabID,
		ProviderID: providerID,
	})
	if err != nil {
		m.finishSubagent(run, nil, err)
		return
	}
	bridge := m.bridgeForSession(info.SessionID, SessionOptions{SessionID: info.SessionID})
	if bridge == nil {
		_ = m.CloseSession(context.Background(), info.SessionID)
		m.finishSubagent(run, nil, errors.New("subagent ACP session was not registered"))
		return
	}

	m.mu.Lock()
	run.SessionID = info.SessionID
	m.mu.Unlock()
	m.updateSubagentActivity(id, "configuring", "Applying model and permission controls")
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		m.CloseSession(closeCtx, info.SessionID)
		m.mu.Lock()
		if m.bridges[bridge.key] == bridge {
			delete(m.bridges, bridge.key)
		}
		for _, key := range chatProviderKeys(SessionOptions{ChatID: childChatID, TabID: childTabID}) {
			delete(m.chatProviders, key)
		}
		m.mu.Unlock()
	}()

	selectedModel := modelID
	if effort != "" {
		selectedModel = modelID + "[" + effort + "]"
	}
	if _, err := bridge.ensureSessionControls(runCtx, info.SessionID, selectedModel, modeID); err != nil {
		m.finishSubagent(run, nil, err)
		return
	}

	now := time.Now().UTC()
	m.mu.Lock()
	m.jobSeq++
	jobID := fmt.Sprintf("subagent-job-%d-%d", now.UnixMilli(), m.jobSeq)
	job := &Job{
		ID: jobID, Kind: "subagent", Title: label, Status: "running",
		StartedAt: now.Format(time.RFC3339Nano), PermissionMode: modeID,
		ChatID: childChatID, TabID: childTabID, SessionID: info.SessionID,
		ProviderID: providerID, CWD: cwd,
		VisibleJobID: rootJobID, VisibleSessionID: rootSessionID,
		SubagentID: id, SubagentLabel: label, SubagentProvider: brand, SubagentModel: run.ModelLabel,
		startOpts: JobStartOptions{Kind: "subagent", Prompt: prompt, ProviderID: providerID, ModelID: selectedModel, ModeID: modeID, CWD: cwd},
	}
	if rootJobID == "" {
		job.suppressVisible.Store(true)
	}
	m.jobs[jobID] = job
	run.JobID = jobID
	m.mu.Unlock()
	m.updateSubagentActivity(id, "working", "Running delegated task")
	bridge.setJobForSession(info.SessionID, job)
	defer func() {
		bridge.flushJobBuffers(job)
		bridge.clearJobForSession(info.SessionID, job)
		m.mu.Lock()
		delete(m.jobs, jobID)
		m.mu.Unlock()
	}()
	defer m.cancelAndDrainSubagentsForOwner(jobID, 5*time.Second)

	nextPrompt := buildTurnRuntimeIdentity(bridge, providerID, selectedModel) +
		m.buildEnvironmentBrief(true) + "Subagent task:\n" + prompt
	var result PromptResult
	for {
		var promptErr error
		result, promptErr = bridge.promptForJob(runCtx, info.SessionID, job, nextPrompt, nil)
		followup, hasFollowup := m.popSubagentFollowupOrSeal(id)
		if hasFollowup {
			m.updateSubagentActivity(id, "working", "Applying coordinator follow-up")
			// Same session, same engine: the identity line and the environment
			// brief were sent with the original task and are still in context.
			// Resending them bought nothing and cost their full size on every
			// follow-up.
			nextPrompt = "Coordinator follow-up for the same delegated task:\n" + followup.Text
			continue
		}
		if promptErr != nil {
			m.finishSubagent(run, job, promptErr)
			return
		}
		break
	}
	job.StopReason = result.StopReason
	job.Result = cleanDraft(m.outputForJob(job))
	job.Status = "done"
	job.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	m.finishSubagent(run, job, nil)
}

func (m *Manager) popSubagentFollowupOrSeal(id string) (subagentFollowup, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run := m.subagents[id]
	if run == nil || len(run.followups) == 0 {
		if run != nil {
			run.acceptingMessages = false
		}
		return subagentFollowup{}, false
	}
	followup := run.followups[0]
	run.followups = run.followups[1:]
	return followup, true
}

func (m *Manager) removeQueuedSubagentFollowup(id string, followupID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run := m.subagents[id]
	if run == nil {
		return
	}
	for i, followup := range run.followups {
		if followup.ID == followupID {
			run.followups = append(run.followups[:i], run.followups[i+1:]...)
			return
		}
	}
}

func (m *Manager) updateSubagentActivity(id, phase, activity string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	changed := false
	tabID, chatID := "", ""
	mirrorPhase, mirrorActivity := "", ""
	m.mu.Lock()
	run := m.subagents[id]
	if run != nil && run.Status == "running" {
		if phase != "" {
			run.Phase = phase
		}
		if activity != "" {
			run.LatestActivity = compactText(activity, 240)
		}
		run.LastActivityAt = now
		run.ProgressSequence++
		tabID, chatID = run.parentTabID, run.parentChatID
		mirrorPhase, mirrorActivity = run.Phase, run.LatestActivity
		changed = true
	}
	m.mu.Unlock()
	if changed {
		m.updateSubagentSpawnedWork(tabID, chatID, id, mirrorPhase, mirrorActivity)
	}
}

func (m *Manager) updateSubagentActivityForJob(job *Job, phase, activity string) {
	if job == nil || job.SubagentID == "" {
		return
	}
	m.updateSubagentActivity(job.SubagentID, phase, activity)
}

func (m *Manager) notifySubagentPermissionForJob(job *Job, title string) {
	if job == nil || job.SubagentID == "" {
		return
	}
	title = compactText(redactSensitiveText(firstNonEmpty(strings.TrimSpace(title), "action")), 180)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	changed := false
	tabID, chatID := "", ""
	mirrorPhase, mirrorActivity := "", ""

	// Who may grant this is resolved BEFORE the lock below, never inside it: the
	// mode lookup reaches bridgeForSession, which takes this same mutex, and a
	// Go mutex is not reentrant — doing it in one block deadlocks the daemon the
	// first time a subagent asks for anything with its parent job still live.
	m.mu.Lock()
	parent := (*Job)(nil)
	if existing := m.subagents[job.SubagentID]; existing != nil {
		parent = m.jobs[existing.ParentJobID]
	}
	m.mu.Unlock()
	grantableBy := "human"
	if _, allowed := m.parentMayGrantSubagentPermission(parent); allowed {
		grantableBy = "parent"
	}

	m.mu.Lock()
	run := m.subagents[job.SubagentID]
	if run != nil && run.Status == "running" {
		sequence := int64(1)
		if run.Attention != nil {
			sequence = run.Attention.Sequence + 1
		}
		message := "Permission required: " + title
		if grantableBy == "parent" {
			message += " — answer it with workass_decide_subagent_permission"
		} else {
			message += " — only the user can grant this; deny it or report to them rather than waiting"
		}
		run.Attention = &SubagentAttention{
			Kind: "permission", Sequence: sequence, Message: message,
			Active: true, RequestedAt: now, GrantableBy: grantableBy,
		}
		run.Phase = "waiting_permission"
		run.LatestActivity = message
		run.LastActivityAt = now
		run.ProgressSequence++
		tabID, chatID = run.parentTabID, run.parentChatID
		mirrorPhase, mirrorActivity = run.Phase, run.LatestActivity
		changed = true
	}
	m.mu.Unlock()
	if changed {
		m.updateSubagentSpawnedWork(tabID, chatID, job.SubagentID, mirrorPhase, mirrorActivity)
	}
}

func (m *Manager) resolveSubagentPermissionForJob(job *Job) {
	if job == nil || job.SubagentID == "" {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	changed := false
	tabID, chatID := "", ""
	mirrorPhase, mirrorActivity := "", ""
	m.mu.Lock()
	run := m.subagents[job.SubagentID]
	if run != nil {
		if run.Attention != nil && run.Attention.Kind == "permission" && run.Attention.Active {
			run.Attention.Active = false
			run.Attention.ResolvedAt = now
		}
		if run.Status == "running" {
			run.Phase = "working"
			run.LatestActivity = "Permission resolved; continuing"
			run.LastActivityAt = now
			run.ProgressSequence++
			tabID, chatID = run.parentTabID, run.parentChatID
			mirrorPhase, mirrorActivity = run.Phase, run.LatestActivity
			changed = true
		}
	}
	m.mu.Unlock()
	if changed {
		m.updateSubagentSpawnedWork(tabID, chatID, job.SubagentID, mirrorPhase, mirrorActivity)
	}
}

// effectiveSubagentParentMode resolves the permission mode the parent is
// actually running with. Live adapter state is authoritative; the job request
// and durable native binding are recovery sources for cold/partial snapshots.
// Returning an empty string means inheritance is unknowable, not read-only.
func (m *Manager) effectiveSubagentParentMode(parent *Job) string {
	if m == nil || parent == nil {
		return ""
	}
	providerID := firstNonEmpty(strings.TrimSpace(parent.ProviderID), strings.TrimSpace(parent.startOpts.ProviderID))
	sessionID := strings.TrimSpace(parent.SessionID)
	if sessionID != "" {
		bridge := m.bridgeForSession(sessionID, SessionOptions{
			SessionID: sessionID, TabID: parent.TabID, ChatID: parent.ChatID, ProviderID: providerID,
		})
		if bridge != nil {
			if live, ok := bridge.liveSession(sessionID); ok {
				if modeID := stringPointerValue(live.Info.CurrentModeID); modeID != "" {
					return modeID
				}
			}
		}
	}
	if modeID := strings.TrimSpace(parent.startOpts.ModeID); modeID != "" {
		return modeID
	}
	controls := m.withNativeSessionControls(SessionOptions{
		SessionID: sessionID, TabID: parent.TabID, ChatID: parent.ChatID, ProviderID: providerID,
	})
	return strings.TrimSpace(controls.ModeID)
}

func (m *Manager) resolveSubagentSelection(ctx context.Context, parent *Job, opts SubagentSpawnOptions) (string, string, string, string, error) {
	providerID := normalizeProviderID(opts.ProviderID)
	recommended := recommendedSelection{}
	if profileID := strings.TrimSpace(opts.Profile); profileID != "" {
		if _, ok := recommendationProfileByID(profileID); !ok {
			return "", "", "", "", fmt.Errorf("unknown subagent profile %q; call workass_agent_catalog first", profileID)
		}
		avoid := ""
		if strings.EqualFold(profileID, "independent-review") {
			avoid = parent.ProviderID
		}
		var ok bool
		recommended, ok = m.recommendSubagentSelection(ctx, profileID, avoid)
		if !ok && providerID == "" && strings.TrimSpace(opts.ModelID) == "" {
			return "", "", "", "", fmt.Errorf("subagent profile %q has no scored model; score models in Settings or select provider_id/model_id explicitly", profileID)
		}
	}
	if providerID == "" {
		providerID = normalizeProviderID(recommended.ProviderID)
	}
	if providerID == "" {
		providerID = normalizeProviderID(parent.ProviderID)
	}
	if providerID == "" {
		return "", "", "", "", errors.New("subagent provider_id is required because the parent provider is unknown; call workass_agent_catalog first")
	}
	group := m.catalogGroup(ctx, providerID)
	if group.Status != providerStatusReady {
		return "", "", "", "", fmt.Errorf("provider %q is not ready: %s", providerID, firstNonEmpty(group.Error, group.Status))
	}
	requestedModel := strings.TrimSpace(opts.ModelID)
	if requestedModel == "" {
		if normalizeProviderID(recommended.ProviderID) == providerID {
			requestedModel = recommended.ModelID
		}
	}
	if requestedModel == "" && normalizeProviderID(parent.ProviderID) == providerID {
		requestedModel = strings.TrimSpace(parent.startOpts.ModelID)
	}
	if requestedModel == "" {
		return "", "", "", "", errors.New("subagent model_id is required because no parent or scored recommendation is available; call workass_agent_catalog first")
	}
	modelID := requestedModel
	var selected *Model
	for i := range group.Models {
		base, inheritedEffort, split := splitEffortSuffix(requestedModel, group.Models[i].Efforts)
		if group.Models[i].ModelID == modelID || (split && group.Models[i].ModelID == base) {
			selected = &group.Models[i]
			modelID = group.Models[i].ModelID
			if strings.TrimSpace(opts.Effort) == "" && split {
				opts.Effort = inheritedEffort
			}
			break
		}
	}
	if selected == nil {
		return "", "", "", "", fmt.Errorf("model %q is not available for provider %q; call workass_agent_catalog first", modelID, providerID)
	}
	if m.hidesFixtureModel(providerID, *selected) {
		return "", "", "", "", fmt.Errorf("model %q is a development fixture and is unavailable in production", modelID)
	}
	effort := strings.TrimSpace(opts.Effort)
	if effort == "" && recommended.ModelID == modelID && recommended.ProviderID == providerID {
		effort = recommended.Effort
	}
	if effort == "" && normalizeProviderID(parent.ProviderID) == providerID {
		if base, inherited, split := splitEffortSuffix(strings.TrimSpace(parent.startOpts.ModelID), selected.Efforts); split && base == modelID {
			effort = inherited
		}
	}
	if effort == "" && len(selected.Efforts) > 0 {
		effort = preferredAvailableEffort(selected.Efforts, "high")
	}
	if len(selected.Efforts) > 0 && effort == "" {
		return "", "", "", "", fmt.Errorf("effort is required for %s/%s; available: %s", providerID, modelID, strings.Join(selected.Efforts, ", "))
	}
	if effort != "" {
		matched := ""
		for _, candidate := range selected.Efforts {
			if strings.EqualFold(candidate, effort) {
				matched = candidate
				break
			}
		}
		if matched == "" {
			return "", "", "", "", fmt.Errorf("effort %q is not available for %s/%s", effort, providerID, modelID)
		}
		effort = matched
	}
	modeID := strings.TrimSpace(opts.ModeID)
	intent := strings.ToLower(strings.TrimSpace(opts.PermissionIntent))
	if intent == "" {
		intent = "inherit"
	}
	if intent != "" && intent != "inherit" && intent != "read" && intent != "edit" && intent != "full" {
		return "", "", "", "", fmt.Errorf("permission_intent %q is invalid; use inherit, read, edit, or full", opts.PermissionIntent)
	}
	if modeID == "" && intent != "" && intent != "inherit" {
		modeID = permissionIntentModes(providerID, group.Modes)[intent]
		if modeID == "" {
			return "", "", "", "", fmt.Errorf("provider %q has no mode for permission intent %q", providerID, intent)
		}
	}
	if modeID == "" && intent == "inherit" {
		parentModeID := m.effectiveSubagentParentMode(parent)
		if parentModeID == "" && len(group.Modes) > 0 {
			return "", "", "", "", errors.New("cannot inherit subagent permission because the parent has no effective mode; pass permission_intent or mode_id explicitly")
		}
		modeID = inheritedPermissionMode(parent.ProviderID, parentModeID, providerID, group.Modes)
		if modeID == "" && len(group.Modes) > 0 {
			return "", "", "", "", fmt.Errorf("cannot inherit parent permission mode %q from provider %q into provider %q; pass permission_intent or mode_id explicitly", parentModeID, parent.ProviderID, providerID)
		}
	}
	if len(group.Modes) > 0 && modeID == "" {
		return "", "", "", "", fmt.Errorf("mode_id is required for provider %q; call workass_agent_catalog first", providerID)
	}
	if modeID != "" {
		found := false
		for _, mode := range group.Modes {
			if mode.ID == modeID {
				found = true
				break
			}
		}
		if !found {
			return "", "", "", "", fmt.Errorf("permission/mode %q is not available for provider %q; call workass_agent_catalog first", modeID, providerID)
		}
	}
	return providerID, modelID, effort, modeID, nil
}

func (m *Manager) finishSubagent(run *SubagentRun, job *Job, runErr error) {
	finished := time.Now().UTC().Format(time.RFC3339Nano)
	status := "done"
	result := ""
	errorText := ""
	if job != nil {
		result = cleanDraft(m.outputForJob(job))
	}
	m.mu.Lock()
	cancelled := run.cancelled
	m.mu.Unlock()
	if job != nil && strings.EqualFold(job.StopReason, "cancelled") {
		cancelled = true
	}
	if cancelled {
		status = "cancelled"
	} else if runErr != nil {
		status = "failed"
		errorText = redactSensitiveText(runErr.Error())
	}
	m.mu.Lock()
	run.Status = status
	run.acceptingMessages = false
	run.Phase = status
	run.LatestActivity = map[string]string{"done": "Completed", "failed": "Failed", "cancelled": "Cancelled"}[status]
	run.LastActivityAt = finished
	run.ProgressSequence++
	run.FinishedAt = finished
	redactedResult := redactSensitiveText(result)
	run.ResultTruncated = len(redactedResult) > 12000
	run.ErrorTruncated = len(errorText) > 2000
	run.Result = compactText(redactedResult, 12000)
	run.Error = compactText(errorText, 2000)
	if job != nil {
		run.StopReason = job.StopReason
	}
	run.ReceiptID = run.ID
	done := run.done
	rootJobID := run.RootJobID
	parentChatID, parentTabID := run.parentChatID, run.parentTabID
	receipt := copySubagentRun(run)
	m.mu.Unlock()
	m.settleSubagentSpawnedWork(parentTabID, parentChatID, receipt)
	m.persistSubagentReceipt(parentChatID, parentTabID, receipt)
	m.mu.Lock()
	run.receiptCommitted = true
	m.mu.Unlock()
	if rootJobID != "" {
		headerStatus := "completed"
		if status == "failed" {
			headerStatus = "failed"
		} else if status == "cancelled" {
			headerStatus = "cancelled"
		}
		m.emitSubagentHeader(rootJobID, run.ID, run.Label, brandForProvider(run.ProviderID), run.ModelLabel, headerStatus, errorText)
	}
	if done != nil {
		close(done)
	}
}

func (m *Manager) emitSubagentHeader(parentJobID, id, label, provider, model, status, output string) {
	event := map[string]any{
		"kind": "tool", "toolKind": "agent", "id": id, "title": label,
		"status": status, "subagentId": id, "subagentLabel": label,
		"subagentProvider": provider, "subagentHeader": true,
	}
	if model != "" {
		event["subagentModel"] = model
	}
	if output != "" {
		event["output"] = compactText(redactSensitiveText(output), 600)
	}
	m.emit("job:event", map[string]any{"type": "acp", "id": parentJobID, "event": event})
}

// subagentModelName resolves a model's catalog display name (e.g. "Opus 4.8")
// for the friendly Turnos chip. Empty when the provider/model isn't in catalog.
func (m *Manager) subagentModelName(ctx context.Context, providerID, modelID string) string {
	group := m.catalogGroup(ctx, providerID)
	for i := range group.Models {
		if group.Models[i].ModelID == modelID {
			return strings.TrimSpace(group.Models[i].Name)
		}
	}
	return ""
}

// subagentModelLabel builds the "Opus4.8-xhigh" chip: prefer the display name
// (spaces stripped, original capitalization kept), fall back to the raw model
// id, and join a non-empty effort with a dash.
func subagentModelLabel(name, modelID, effort string) string {
	base := strings.ReplaceAll(strings.TrimSpace(name), " ", "")
	if base == "" {
		base = strings.TrimSpace(modelID)
	}
	if base == "" {
		return ""
	}
	if e := strings.TrimSpace(effort); e != "" {
		return base + "-" + e
	}
	return base
}

func (m *Manager) runningJobForOwner(ownerKey, chatID, tabID string) *Job {
	chatID, tabID, ok := m.subagentOwnerIdentity(ownerKey, chatID, tabID)
	if !ok {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runningJobForIdentityLocked(chatID, tabID)
}

func (m *Manager) subagentOwnerIdentity(ownerKey, chatID, tabID string) (string, string, bool) {
	boundChatID, boundTabID, ok := m.ownerBinding(ownerKey, chatID, tabID)
	if !ok {
		return "", "", false
	}
	chatID, tabID = strings.TrimSpace(chatID), strings.TrimSpace(tabID)
	if chatID != "" && chatID != boundChatID || tabID != "" && tabID != boundTabID {
		return "", "", false
	}
	return boundChatID, boundTabID, true
}

func (m *Manager) subagentOwnerContext(ownerKey, chatID, tabID string) (string, string, *Job, bool) {
	chatID, tabID, ok := m.subagentOwnerIdentity(ownerKey, chatID, tabID)
	if !ok {
		return "", "", nil, false
	}
	m.mu.Lock()
	parent := m.runningJobForIdentityLocked(chatID, tabID)
	m.mu.Unlock()
	return chatID, tabID, parent, true
}

func (m *Manager) runningJobForIdentityLocked(chatID, tabID string) *Job {
	for _, job := range m.jobs {
		if job == nil || job.Status != "running" {
			continue
		}
		if (chatID == "" || job.ChatID == chatID) && (tabID == "" || job.TabID == tabID) {
			return job
		}
	}
	return nil
}

func (m *Manager) runningOwnerJobLocked(parent *Job, chatID, tabID string) bool {
	if parent == nil || parent.Status != "running" || m.jobs[parent.ID] != parent {
		return false
	}
	return (chatID == "" || parent.ChatID == chatID) && (tabID == "" || parent.TabID == tabID)
}

func (m *Manager) subagentParentDefaults(chatID, tabID string) *Job {
	defaults := &Job{ChatID: chatID, TabID: tabID}
	for _, live := range m.LiveSessions() {
		if live.ChatID != chatID || live.TabID != tabID {
			continue
		}
		defaults.SessionID = live.Info.SessionID
		defaults.ProviderID = live.Info.ProviderID
		defaults.CWD = live.Info.CWD
		defaults.startOpts = JobStartOptions{
			ProviderID: live.Info.ProviderID,
			ModelID:    stringPointerValue(live.Info.CurrentModelID),
			ModeID:     stringPointerValue(live.Info.CurrentModeID),
			CWD:        live.Info.CWD,
		}
		break
	}
	return defaults
}

func stringPointerValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func sameSubagentOwner(run *SubagentRun, chatID, tabID string) bool {
	return run != nil && run.parentChatID == chatID && run.parentTabID == tabID
}

func (m *Manager) rebindAdoptedSubagentLocked(run *SubagentRun, parent *Job) {
	if run == nil || parent == nil {
		return
	}
	rootJobID := firstNonEmpty(parent.VisibleJobID, parent.ID)
	rootSessionID := firstNonEmpty(parent.VisibleSessionID, parent.SessionID)
	run.ParentJobID = parent.ID
	run.RootJobID = rootJobID
	run.ParentSessionID = parent.SessionID
	run.RootSessionID = rootSessionID
	if run.Attention != nil {
		run.attentionDeliveredSequence = 0
	}
	if job := m.jobs[run.JobID]; job != nil {
		job.VisibleJobID = rootJobID
		job.VisibleSessionID = rootSessionID
		job.suppressVisible.Store(rootJobID == "")
	}
}

func (m *Manager) addressSubagentLocked(run *SubagentRun, parent *Job, chatID, tabID string) bool {
	if run == nil {
		return false
	}
	if m.runningOwnerJobLocked(parent, chatID, tabID) {
		if run.ParentJobID == parent.ID {
			return true
		}
		if run.Adopted && sameSubagentOwner(run, chatID, tabID) {
			m.rebindAdoptedSubagentLocked(run, parent)
			return true
		}
		return false
	}
	return run.Adopted && sameSubagentOwner(run, chatID, tabID)
}

func (m *Manager) ListSubagents(ownerKey, parentChatID, parentTabID string) []SubagentRun {
	chatID, tabID, parent, ok := m.subagentOwnerContext(ownerKey, parentChatID, parentTabID)
	if !ok {
		return []SubagentRun{}
	}
	m.mu.Lock()
	out := make([]SubagentRun, 0, len(m.subagents))
	for _, run := range m.subagents {
		if !m.addressSubagentLocked(run, parent, chatID, tabID) {
			continue
		}
		out = append(out, copySubagentRun(run))
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt == out[j].StartedAt {
			return out[i].ID < out[j].ID
		}
		return out[i].StartedAt < out[j].StartedAt
	})
	m.noteSubagentResultsObserved(tabID, chatID, out)
	return out
}

// noteSubagentResultsObserved records that the owning coordinator has already
// read these children's terminal results, so their deferred wake is redundant.
// It deliberately runs with the manager lock released: cancelling a wake takes
// the spawned-work lock, and the two are never nested.
func (m *Manager) noteSubagentResultsObserved(tabID, chatID string, runs []SubagentRun) {
	for _, run := range runs {
		switch run.Status {
		case "", "running":
			continue
		}
		m.markSpawnedWorkWakeConsumed(tabID, chatID, run.ID)
	}
}

func (m *Manager) WaitSubagent(ctx context.Context, ownerKey, parentChatID, parentTabID, id string, timeout time.Duration) (SubagentRun, error) {
	chatID, tabID, parent, ok := m.subagentOwnerContext(ownerKey, parentChatID, parentTabID)
	if !ok {
		return SubagentRun{}, errors.New("no running Workass turn owns this subagent request")
	}
	id = strings.TrimSpace(id)
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	for {
		m.mu.Lock()
		run := m.subagents[id]
		if !m.addressSubagentLocked(run, parent, chatID, tabID) {
			m.mu.Unlock()
			return SubagentRun{}, errors.New("subagent not found for this owner")
		}
		snapshot := copySubagentRun(run)
		settled := subagentRunSettled(run)
		done := run.done
		if snapshot.NeedsAttention && run.Attention != nil && run.Attention.Sequence > run.attentionDeliveredSequence {
			run.attentionDeliveredSequence = run.Attention.Sequence
		}
		m.mu.Unlock()
		if settled {
			m.noteSubagentResultsObserved(tabID, chatID, []SubagentRun{snapshot})
		}
		if settled || snapshot.NeedsAttention {
			return snapshot, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return SubagentRun{}, errors.New("timed out waiting for subagent; it is still running")
		}
		poll := 50 * time.Millisecond
		if remaining < poll {
			poll = remaining
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return SubagentRun{}, ctx.Err()
		case <-done:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// WaitSubagents waits for the first or all selected children while always
// returning a useful snapshot on timeout. This is the fan-out primitive the
// parent agent needs to compose parallel work without serial one-child waits.
func (m *Manager) WaitSubagents(ctx context.Context, ownerKey, parentChatID, parentTabID string, ids []string, returnWhen string, timeout time.Duration) (map[string]any, error) {
	chatID, tabID, parent, ok := m.subagentOwnerContext(ownerKey, parentChatID, parentTabID)
	if !ok {
		return nil, errors.New("no running Workass turn owns this subagent request")
	}
	returnWhen = strings.ToLower(strings.TrimSpace(returnWhen))
	if returnWhen == "" {
		returnWhen = "first"
	}
	if returnWhen != "first" && returnWhen != "all" {
		return nil, errors.New("return_when must be first or all")
	}
	seen := map[string]bool{}
	cleanIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			cleanIDs = append(cleanIDs, id)
		}
	}
	if len(cleanIDs) == 0 {
		return nil, errors.New("at least one subagent id is required")
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	for {
		completed, running, err := m.subagentSnapshotsForOwner(parent, chatID, tabID, cleanIDs)
		if err != nil {
			return nil, err
		}
		attention := subagentsNeedingAttention(running)
		ready := len(attention) > 0 || returnWhen == "first" && len(completed) > 0 || returnWhen == "all" && len(running) == 0
		if ready {
			m.noteSubagentResultsObserved(tabID, chatID, completed)
			return map[string]any{
				"completed": completed, "running": running, "attention": attention,
				"needsAttention": len(attention) > 0, "timedOut": false,
			}, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			m.noteSubagentResultsObserved(tabID, chatID, completed)
			return map[string]any{
				"completed": completed, "running": running, "attention": attention,
				"needsAttention": len(attention) > 0, "timedOut": true,
			}, nil
		}
		wait := 50 * time.Millisecond
		if remaining < wait {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (m *Manager) subagentSnapshotsForOwner(parent *Job, chatID, tabID string, ids []string) ([]SubagentRun, []SubagentRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	completed := make([]SubagentRun, 0, len(ids))
	running := make([]SubagentRun, 0, len(ids))
	for _, id := range ids {
		run := m.subagents[id]
		if !m.addressSubagentLocked(run, parent, chatID, tabID) {
			return nil, nil, fmt.Errorf("subagent %q not found for this owner", id)
		}
		snapshot := copySubagentRun(run)
		if snapshot.NeedsAttention && run.Attention != nil && run.Attention.Sequence > run.attentionDeliveredSequence {
			run.attentionDeliveredSequence = run.Attention.Sequence
		}
		if !subagentRunSettled(run) {
			running = append(running, snapshot)
		} else {
			completed = append(completed, snapshot)
		}
	}
	return completed, running, nil
}

func subagentsNeedingAttention(running []SubagentRun) []SubagentRun {
	attention := make([]SubagentRun, 0, len(running))
	for _, run := range running {
		if run.NeedsAttention {
			attention = append(attention, run)
		}
	}
	return attention
}

func (m *Manager) subagentSnapshotsForParent(parentJobID string, ids []string) ([]SubagentRun, []SubagentRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	completed := make([]SubagentRun, 0, len(ids))
	running := make([]SubagentRun, 0, len(ids))
	for _, id := range ids {
		run := m.subagents[id]
		if run == nil || run.ParentJobID != parentJobID {
			return nil, nil, fmt.Errorf("subagent %q not found for this turn", id)
		}
		snapshot := copySubagentRun(run)
		if snapshot.NeedsAttention && run.Attention != nil && run.Attention.Sequence > run.attentionDeliveredSequence {
			run.attentionDeliveredSequence = run.Attention.Sequence
		}
		if !subagentRunSettled(run) {
			running = append(running, snapshot)
		} else {
			completed = append(completed, snapshot)
		}
	}
	return completed, running, nil
}

func subagentRunSettled(run *SubagentRun) bool {
	if run == nil || run.Status == "running" || !run.receiptCommitted {
		return false
	}
	if run.done == nil {
		return true
	}
	select {
	case <-run.done:
		return true
	default:
		return false
	}
}

// MessageSubagent queues the coordinator's direction before attempting live
// delivery. Native/live steering removes it from the FIFO only after an
// acknowledgement; Claude and unsupported agents consume it as the immediate
// next prompt after cancellation or natural completion.
func (m *Manager) MessageSubagent(ownerKey, parentChatID, parentTabID, id, message string) (map[string]any, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		return nil, errors.New("subagent message is required")
	}
	chatID, tabID, parent, ok := m.subagentOwnerContext(ownerKey, parentChatID, parentTabID)
	if !ok {
		return nil, errors.New("no running Workass turn owns this subagent request")
	}
	id = strings.TrimSpace(id)
	m.mu.Lock()
	run := m.subagents[id]
	if !m.addressSubagentLocked(run, parent, chatID, tabID) {
		m.mu.Unlock()
		return nil, errors.New("subagent not found for this owner")
	}
	if run.Status != "running" || !run.acceptingMessages {
		m.mu.Unlock()
		return nil, errors.New("subagent is no longer accepting messages; retry it to continue")
	}
	run.followupSeq++
	followup := subagentFollowup{ID: run.followupSeq, Text: message}
	run.followups = append(run.followups, followup)
	sessionID := run.SessionID
	run.LatestActivity = "Coordinator message queued"
	run.LastActivityAt = time.Now().UTC().Format(time.RFC3339Nano)
	run.ProgressSequence++
	m.mu.Unlock()

	if sessionID == "" {
		return map[string]any{"ok": true, "delivery": "queued", "subagentId": id}, nil
	}
	steer := m.Steer(sessionID, message, nil, "")
	strategy := asString(steer["strategy"])
	if steer["ok"] == true && steer["live"] == true {
		m.removeQueuedSubagentFollowup(id, followup.ID)
		m.updateSubagentActivity(id, "working", "Coordinator message delivered live")
		return map[string]any{"ok": true, "delivery": "live", "strategy": strategy, "subagentId": id}, nil
	}
	if strategy == "uncertain" {
		m.removeQueuedSubagentFollowup(id, followup.ID)
		return map[string]any{"ok": false, "delivery": "uncertain", "strategy": strategy, "subagentId": id, "error": steer["error"]}, nil
	}
	delivery := "followup"
	if steer["interrupted"] == true {
		delivery = "interrupt-followup"
	}
	return map[string]any{"ok": true, "delivery": delivery, "strategy": strategy, "subagentId": id}, nil
}

func (m *Manager) RetrySubagent(ctx context.Context, ownerKey, parentChatID, parentTabID, id, message string) (SubagentRun, error) {
	chatID, tabID, parent, ok := m.subagentOwnerContext(ownerKey, parentChatID, parentTabID)
	if !ok {
		return SubagentRun{}, errors.New("no running Workass turn owns this subagent request")
	}
	m.mu.Lock()
	original := m.subagents[strings.TrimSpace(id)]
	if !m.addressSubagentLocked(original, parent, chatID, tabID) {
		m.mu.Unlock()
		return SubagentRun{}, errors.New("subagent not found for this owner")
	}
	if original.Status == "running" {
		m.mu.Unlock()
		return SubagentRun{}, errors.New("subagent is still running")
	}
	prompt := original.prompt
	if extra := strings.TrimSpace(message); extra != "" {
		prompt += "\n\nRetry guidance from the coordinator:\n" + extra
	}
	opts := SubagentSpawnOptions{
		OwnerKey: ownerKey, ParentChatID: parentChatID, ParentTabID: parentTabID,
		Prompt: prompt, Label: original.Label, ProviderID: original.ProviderID,
		ModelID: original.ModelID, Effort: original.Effort, ModeID: original.ModeID,
		CWD: original.CWD, Profile: original.Profile, RetryOf: original.ID,
	}
	m.mu.Unlock()
	return m.SpawnSubagent(ctx, opts)
}

func (m *Manager) CancelSubagent(ownerKey, parentChatID, parentTabID, id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	chatID, tabID, parent, ok := m.subagentOwnerContext(ownerKey, parentChatID, parentTabID)
	if !ok {
		return false
	}
	m.mu.Lock()
	run := m.subagents[id]
	addressable := m.addressSubagentLocked(run, parent, chatID, tabID)
	ownerJobID := ""
	if addressable && m.runningOwnerJobLocked(parent, chatID, tabID) {
		ownerJobID = parent.ID
	}
	m.mu.Unlock()
	if !addressable {
		return false
	}
	return m.cancelSubagentByID(id, ownerJobID)
}

func (m *Manager) cancelSubagentByID(id, ownerJobID string) bool {
	m.mu.Lock()
	run := m.subagents[id]
	if run == nil || run.Status != "running" || (ownerJobID != "" && run.ParentJobID != ownerJobID) {
		m.mu.Unlock()
		return false
	}
	cancel := run.cancel
	sessionID := run.SessionID
	run.cancelled = true
	run.acceptingMessages = false
	m.mu.Unlock()
	if sessionID != "" {
		m.cancelPermissionsForSession(sessionID)
	}
	if cancel != nil {
		cancel()
	}
	if sessionID != "" {
		if bridge := m.bridgeForSession(sessionID, SessionOptions{SessionID: sessionID}); bridge != nil {
			bridge.notify("session/cancel", map[string]any{"sessionId": sessionID})
		}
	}
	return true
}

func (m *Manager) adoptSubagentsForParent(parentJobID string) {
	parentJobID = strings.TrimSpace(parentJobID)
	if parentJobID == "" {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, run := range m.subagents {
		if run == nil || run.RootJobID != parentJobID || run.Status != "running" {
			continue
		}
		run.Adopted = true
		if run.AdoptedAt == "" {
			run.AdoptedAt = now
		}
	}
}

func (m *Manager) cancelSubagentsForParent(parentJobID string) {
	m.mu.Lock()
	var ids []string
	for id, run := range m.subagents {
		if run != nil && run.RootJobID == parentJobID && run.Status == "running" {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.cancelSubagentByID(id, "")
	}
}

func (m *Manager) cancelAndDrainSubagentsForParent(parentJobID string, timeout time.Duration) {
	m.mu.Lock()
	var ids []string
	var done []<-chan struct{}
	for id, run := range m.subagents {
		if run != nil && run.RootJobID == parentJobID && run.Status == "running" {
			ids = append(ids, id)
			if run.done != nil {
				done = append(done, run.done)
			}
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.cancelSubagentByID(id, "")
	}
	deadline := time.Now().Add(timeout)
	for _, ch := range done {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			m.detachSubagentsFromParent(parentJobID)
			return
		}
		select {
		case <-ch:
		case <-time.After(remaining):
			m.detachSubagentsFromParent(parentJobID)
			return
		}
	}
}

func (m *Manager) cancelAndDrainSubagentsForOwner(ownerJobID string, timeout time.Duration) {
	if ownerJobID == "" {
		return
	}
	m.mu.Lock()
	var ids []string
	var done []<-chan struct{}
	for id, run := range m.subagents {
		if run != nil && run.ParentJobID == ownerJobID && run.Status == "running" {
			ids = append(ids, id)
			if run.done != nil {
				done = append(done, run.done)
			}
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.cancelSubagentByID(id, ownerJobID)
	}
	deadline := time.Now().Add(timeout)
	for _, ch := range done {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		select {
		case <-ch:
		case <-time.After(remaining):
			return
		}
	}
}

func (m *Manager) cancelAndDrainSubagentsForChat(chatID, tabID string, timeout time.Duration) {
	chatID, tabID = strings.TrimSpace(chatID), strings.TrimSpace(tabID)
	if chatID == "" || tabID == "" {
		return
	}
	m.mu.Lock()
	selected := map[string]bool{}
	ownerJobs := map[string]bool{}
	for id, run := range m.subagents {
		if run == nil || run.Status != "running" || !sameSubagentOwner(run, chatID, tabID) {
			continue
		}
		selected[id] = true
		if run.JobID != "" {
			ownerJobs[run.JobID] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for id, run := range m.subagents {
			if run == nil || run.Status != "running" || selected[id] || !ownerJobs[run.ParentJobID] {
				continue
			}
			selected[id] = true
			if run.JobID != "" {
				ownerJobs[run.JobID] = true
			}
			changed = true
		}
	}
	ids := make([]string, 0, len(selected))
	done := make([]<-chan struct{}, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
		if run := m.subagents[id]; run != nil && run.done != nil {
			done = append(done, run.done)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.cancelSubagentByID(id, "")
	}
	deadline := time.Now().Add(timeout)
	for _, ch := range done {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		select {
		case <-ch:
		case <-time.After(remaining):
			return
		}
	}
}

func (m *Manager) detachSubagentsFromParent(parentJobID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, run := range m.subagents {
		if run == nil || run.RootJobID != parentJobID || run.Status != "running" {
			continue
		}
		run.RootJobID = ""
		if job := m.jobs[run.JobID]; job != nil {
			job.suppressVisible.Store(true)
		}
	}
}

func (m *Manager) cancelAllSubagents(timeout time.Duration) {
	m.mu.Lock()
	parents := map[string]bool{}
	for _, run := range m.subagents {
		if run != nil && run.Status == "running" {
			parents[run.RootJobID] = true
		}
	}
	m.mu.Unlock()
	for parentJobID := range parents {
		m.cancelAndDrainSubagentsForParent(parentJobID, timeout)
	}
}

func (m *Manager) pruneSubagentsLocked() {
	if len(m.subagents) < maxCompletedSubagentHistory {
		return
	}
	completed := make([]*SubagentRun, 0, len(m.subagents))
	for _, run := range m.subagents {
		if run != nil && run.Status != "running" {
			completed = append(completed, run)
		}
	}
	sort.Slice(completed, func(i, j int) bool {
		if completed[i].FinishedAt == completed[j].FinishedAt {
			return completed[i].ID < completed[j].ID
		}
		return completed[i].FinishedAt < completed[j].FinishedAt
	})
	for len(m.subagents) >= maxCompletedSubagentHistory && len(completed) > 0 {
		delete(m.subagents, completed[0].ID)
		completed = completed[1:]
	}
}

func copySubagentRun(run *SubagentRun) SubagentRun {
	if run == nil {
		return SubagentRun{}
	}
	copy := *run
	if run.Attention != nil {
		attention := *run.Attention
		copy.Attention = &attention
		copy.NeedsAttention = attention.Active || attention.Sequence > run.attentionDeliveredSequence
	}
	copy.cancelled = false
	copy.cancel = nil
	copy.done = nil
	copy.prompt = ""
	copy.parentChatID = ""
	copy.parentTabID = ""
	copy.attentionDeliveredSequence = 0
	start, startErr := time.Parse(time.RFC3339Nano, copy.StartedAt)
	end := time.Now().UTC()
	if copy.FinishedAt != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, copy.FinishedAt); err == nil {
			end = parsed
		}
	}
	if startErr == nil && !end.Before(start) {
		copy.ElapsedMs = end.Sub(start).Milliseconds()
	}
	return copy
}
