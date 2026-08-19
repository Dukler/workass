package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"workass/internal/provider"
)

type LanePhase string

const (
	LaneAbsent      LanePhase = "absent"
	LaneCreating    LanePhase = "creating"
	LaneDetached    LanePhase = "detached"
	LaneResuming    LanePhase = "resuming"
	LaneReady       LanePhase = "ready"
	LaneImporting   LanePhase = "importing"
	LaneRunning     LanePhase = "running"
	LaneReconciling LanePhase = "reconciling"
	LaneBlocked     LanePhase = "blocked"
	LaneBroken      LanePhase = "broken"
)

type LedgerEvent struct {
	EventID              string
	MessageID            string
	Sequence             uint64
	Role                 string
	Text                 string
	Result               string
	Status               string
	At                   string
	Attachments          []provider.Attachment
	LaneID               provider.LaneID
	ProviderID           provider.ID
	ModelID              string
	OperationID          provider.OperationID
	QueueID              string
	NativeTurnID         string
	TerminalState        string
	SteerState           string
	SteerBoundary        string
	SteerContinuationID  string
	SteerContinuationFor string
	TurnRootID           string
	TurnTerminal         *bool
	TurnStartedAt        int64
	Interrupted          bool
	RetryPrompt          string
	Terminal             *provider.TerminalEvent
	// ContextExcluded marks a visible row copied by an explicit user fork. It
	// has no child-lane ownership and is never imported into the fresh provider
	// thread.
	ContextExcluded bool
	Timeline        []TimelineEntry
	Permission      *provider.PermissionEvent
}

// TimelineEntry is the renderer-visible, provider-neutral event sequence owned
// by one assistant row. At is a UTF-16 content offset because the frozen
// renderer contract indexes JavaScript strings. Exactly one typed payload must
// match Kind.
type TimelineEntry struct {
	Key        string
	At         int
	Kind       provider.EventKind
	Thinking   *provider.ThinkingEvent
	Tool       *provider.ToolEvent
	Plan       *provider.PlanEvent
	Compaction *provider.CompactionEvent
	Restored   *provider.CheckpointRestoredEvent
}

type ContextBatch struct {
	ProjectionVersion uint32
	EventIDs          []string
	Digest            string
	Messages          []provider.ContextMessage
}

type PendingImport struct {
	OperationID provider.OperationID
	From        uint64
	To          uint64
	Batch       ContextBatch
}

type CoverageStatus string

const (
	CoverageNativeSeen CoverageStatus = "native_seen"
	CoverageImported   CoverageStatus = "imported"
	CoverageSeeded     CoverageStatus = "initial_seed"
	CoverageExcluded   CoverageStatus = "excluded"
)

type CoverageRecord struct {
	Sequence   uint64
	EventID    string
	Status     CoverageStatus
	DeliveryID provider.OperationID
}

type LaneState struct {
	Identity provider.LaneIdentity
	Owner    provider.AttachmentOwner
	CWD      string
	ModelID  string
	ModeID   string
	Thread   provider.ThreadRef
	// Provision is a provider-returned candidate that has not crossed the
	// provider's durable creation boundary. It is never an established ThreadRef
	// and may only coexist with a zero Thread.
	Provision *provider.ThreadRef
	// CreateAfterCandidateAbsence is durable actor proof that this deferred
	// lane has never acquired provider-native coverage. It is the only state in
	// which an exact candidate reported missing may be followed by session/new.
	CreateAfterCandidateAbsence bool
	// InitialSeedPending is true only while this lane has never consumed a real
	// provider input. It allows an absent lane to receive the existing Workass
	// ledger once as part of its first sampling turn. Once input is consumed it
	// can never become true again; later coverage gaps require ContextStrategy.
	InitialSeedPending   bool
	Phase                LanePhase
	CoveredThrough       uint64
	Coverage             map[uint64]CoverageRecord
	ConnectionGeneration uint64
	LastEventSequence    uint64
	CreateGeneration     uint64
	Context              provider.ContextCapabilities
	Delivery             provider.DeliveryCapabilities
	Creation             provider.CreationCapabilities
	PendingImport        *PendingImport
	LastError            provider.ErrorKind
	Attachment           *provider.LaneAttachmentSnapshot
}

// CreationFailedBeforeEstablishment identifies a lane whose create attempt
// ended before Workass acquired either an immutable ThreadRef or a provisional
// provider candidate. No provider input can have been dispatched through such
// a lane, so a later explicit select or submit may start a fresh create
// generation. The failed outbox receipt remains immutable audit evidence.
func (l LaneState) CreationFailedBeforeEstablishment() bool {
	if !l.Thread.IsZero() || l.Provision != nil || l.LastError == "" {
		return false
	}
	switch l.Phase {
	case LaneAbsent, LaneBlocked, LaneBroken:
		return true
	default:
		return false
	}
}

type QueueEntry struct {
	OperationID  provider.OperationID
	LaneID       provider.LaneID
	Text         string
	Attachments  []provider.Attachment
	ModelID      string
	ModeID       string
	Permission   string
	Presentation provider.TurnPresentation
	// InitialSeedFrom/Through/Digest freeze the one-time seed range without
	// duplicating transcript bodies in actor state. The executable effect
	// deterministically rebuilds the bounded batch from the immutable ledger.
	InitialSeedFrom    uint64
	InitialSeedThrough uint64
	InitialSeedDigest  string
	Revision           uint64
}

// StagedQueueEntry is a visible follow-up that has not yet been promoted to a
// provider operation. Its target provider/control snapshot is immutable from
// admission; promotion resolves that snapshot to an exact LaneID once and then
// moves it into Queue.
type StagedQueueEntry struct {
	ID               string
	Text             string
	Source           string
	Delivery         string
	QueuedAt         string
	Attachments      []provider.Attachment
	AttachmentNames  []string
	AttachmentState  string
	AttachmentError  string
	TargetProviderID provider.ID
	ModelID          string
	ModeID           string
	Permission       string
}

// QueueMutationReceipt makes renderer FIFO writes exactly-once across a lost
// reply or reconnect. The actor stores the canonical payload digest and the
// revision produced by that immutable request id; retrying the same request
// returns the same state, while reusing an id for different bytes fails closed.
type QueueMutationReceipt struct {
	Digest   string
	Revision uint64
}

// QueueControlState is the durable dispatch gate for work that is already in
// either queue. Explicit cancellation closes the gate before the provider is
// contacted, so a terminal event cannot make Stop look like it failed by
// immediately starting the next row. Resuming is revision-fenced: a delayed
// click from an earlier cancellation can never release a newer paused queue.
type QueueControlState struct {
	Paused         bool
	Revision       uint64
	ResumeReceipts map[provider.OperationID]QueueResumeReceipt
}

type QueueResumeReceipt struct {
	PauseRevision uint64
}

type PresentationMutationReceipt struct {
	Digest   string
	Revision uint64
}

type RuntimeControlMutationReceipt struct {
	Digest   string
	Revision uint64
}

// WorkspaceMutationReceipt records the durable result of one workspace epoch
// change.  The target cwd and revision are part of the receipt so a lost reply
// can be returned byte-for-byte from actor state even after a later operation
// has moved the chat again.  Reusing the operation id with another digest is a
// fail-closed protocol violation.
type WorkspaceMutationReceipt struct {
	Digest             string
	Revision           uint64
	CWD                string
	DetachOperationIDs []provider.OperationID
}

// LaneSelectionMutationReceipt records the durable result of one provider
// lane-selection request.  Selection is a compound user action: its provider
// controls and desired lane must commit together, and a lost wire reply must
// be replayable without resolving or spawning the provider a second time.
type LaneSelectionMutationReceipt struct {
	Digest   string
	Revision uint64
	LaneID   provider.LaneID
}

// CancelMutationReceipt records a terminal chat.cancel result that did not
// have a provider turn to address. It is deliberately provider-neutral: an
// idle result has no lane, native turn, or external effect receipt to retain.
type CancelMutationReceipt struct {
	Cancelled bool
	Reason    string
}

// AgentWaitObservationReceipt is the durable idempotency fence for an agent
// wait/read operation. The request's exact target and timeout intent live only
// in the bounded digest; raw subagent output, tails, prompts, and credentials
// never enter actor state.
type AgentWaitObservationReceipt struct {
	Digest string
}

type ForegroundStatus string

const (
	ForegroundDispatching ForegroundStatus = "dispatching"
	ForegroundRunning     ForegroundStatus = "running"
	ForegroundReconciling ForegroundStatus = "reconciling"
	ForegroundUncertain   ForegroundStatus = "uncertain"
)

type ForegroundTurn struct {
	OperationID               provider.OperationID
	LaneID                    provider.LaneID
	Input                     QueueEntry
	Turn                      provider.TurnRef
	Status                    ForegroundStatus
	UserConsumed              bool
	StartedAt                 string
	RootAssistantMessageID    string
	CurrentAssistantMessageID string
	// AssistantDraft is read-only schema-v1 compatibility. New events are split
	// by provider phase into Content and Result so final answers render once.
	AssistantDraft       string
	AssistantContent     string
	AssistantResult      string
	TypedAssistantPhases bool
	AssistantAttachments []provider.Attachment
	Timeline             []TimelineEntry
	Permission           *provider.PermissionEvent
}

type SteerStatus string

const (
	SteerDispatching SteerStatus = "dispatching"
	SteerAccepted    SteerStatus = "accepted"
	SteerUncertain   SteerStatus = "uncertain"
)

type PendingSteer struct {
	OperationID      provider.OperationID
	LaneID           provider.LaneID
	Turn             provider.TurnRef
	Text             string
	Attachments      []provider.Attachment
	Presentation     provider.TurnPresentation
	Status           SteerStatus
	AwaitConsumption bool
	Interrupted      bool
}

type PendingCancel struct {
	OperationID provider.OperationID
	LaneID      provider.LaneID
	Turn        provider.TurnRef
}

// Provider-owned side activities remain tied to the lane/turn that emitted
// them even when the user selects another provider. These are authoritative
// chat state, not renderer-local cards.
type ProviderActivityOwner struct {
	LaneID               provider.LaneID
	OperationID          provider.OperationID
	TurnID               string
	ConnectionGeneration uint64
}

// ResolveProviderActivityOwner resolves a provider activity origin against
// the actor's current foreground turn or immutable historical ledger. Empty
// arguments are treated as omitted origin fields so callers can resolve a
// provider-supplied partial origin, but the returned owner is always complete.
// The returned owner itself is still subject to the empty-native historical
// rule enforced by validateProviderActivityOwner.
func (s State) ResolveProviderActivityOwner(laneID provider.LaneID, operationID provider.OperationID, turnID string) (ProviderActivityOwner, bool) {
	laneID = provider.LaneID(strings.TrimSpace(string(laneID)))
	operationID = provider.NormalizeOperationID(string(operationID))
	turnID = strings.TrimSpace(turnID)

	type candidateKey struct {
		laneID      provider.LaneID
		operationID provider.OperationID
		turnID      string
	}
	candidates := make(map[candidateKey]ProviderActivityOwner)
	add := func(candidate ProviderActivityOwner) {
		candidate.LaneID = provider.LaneID(strings.TrimSpace(string(candidate.LaneID)))
		candidate.OperationID = provider.NormalizeOperationID(string(candidate.OperationID))
		candidate.TurnID = strings.TrimSpace(candidate.TurnID)
		if candidate.LaneID == "" || candidate.OperationID == "" {
			return
		}
		if laneID != "" && candidate.LaneID != laneID || operationID != "" && candidate.OperationID != operationID || turnID != "" && candidate.TurnID != turnID {
			return
		}
		lane, ok := s.Lanes[candidate.LaneID]
		if !ok {
			return
		}
		candidate.ConnectionGeneration = lane.ConnectionGeneration
		candidates[candidateKey{candidate.LaneID, candidate.OperationID, candidate.TurnID}] = candidate
	}

	if foreground := s.Foreground; foreground != nil {
		add(ProviderActivityOwner{
			LaneID: foreground.LaneID, OperationID: foreground.OperationID,
			TurnID: foreground.Turn.NativeID,
		})
	}
	for _, event := range s.Ledger {
		if !providerActivityLedgerOwnsLane(s, laneID, event) {
			continue
		}
		candidateLaneID := event.LaneID
		if candidateLaneID == "" {
			candidateLaneID = laneID
		}
		add(ProviderActivityOwner{
			LaneID: candidateLaneID, OperationID: event.OperationID,
			TurnID: event.NativeTurnID,
		})
	}

	var resolved ProviderActivityOwner
	for _, candidate := range candidates {
		if !providerActivityOwnerMatches(s, candidate) {
			continue
		}
		if resolved.LaneID != "" {
			// A partial provider origin is usable only when it identifies one
			// exact actor turn. Multiple candidates are ambiguous, even when
			// they happen to share a lane.
			return ProviderActivityOwner{}, false
		}
		resolved = candidate
	}
	return resolved, resolved.LaneID != ""
}

func validateProviderActivityOwner(state State, owner ProviderActivityOwner) error {
	owner.LaneID = provider.LaneID(strings.TrimSpace(string(owner.LaneID)))
	owner.OperationID = provider.NormalizeOperationID(string(owner.OperationID))
	owner.TurnID = strings.TrimSpace(owner.TurnID)
	if owner.LaneID == "" {
		return errors.New("provider activity is missing lane ownership")
	}
	lane, ok := state.Lanes[owner.LaneID]
	if !ok {
		return errors.New("provider activity belongs to an unknown lane")
	}
	if owner.ConnectionGeneration == 0 || owner.ConnectionGeneration > lane.ConnectionGeneration {
		return errors.New("provider activity has an invalid connection generation")
	}
	if owner.OperationID == "" {
		return errors.New("provider activity is missing operation ownership")
	}
	if !providerActivityOwnerMatches(state, owner) {
		return errors.New("provider activity does not match an exact current or historical actor turn")
	}
	return nil
}

func providerActivityOwnerMatches(state State, owner ProviderActivityOwner) bool {
	laneID := provider.LaneID(strings.TrimSpace(string(owner.LaneID)))
	operationID := provider.NormalizeOperationID(string(owner.OperationID))
	turnID := strings.TrimSpace(owner.TurnID)
	if laneID == "" || operationID == "" {
		return false
	}
	if _, ok := state.Lanes[laneID]; !ok {
		return false
	}

	if foreground := state.Foreground; foreground != nil &&
		foreground.LaneID == laneID && provider.NormalizeOperationID(string(foreground.OperationID)) == operationID {
		// A live turn with no native identity is not an ownership proof. It
		// becomes eligible only after admission supplies the exact native id.
		return turnID != "" && strings.TrimSpace(foreground.Turn.NativeID) == turnID
	}

	found := false
	knownNativeTurn := false
	matchedNativeTurn := false
	for _, event := range state.Ledger {
		if !providerActivityLedgerOwnsLane(state, laneID, event) ||
			provider.NormalizeOperationID(string(event.OperationID)) != operationID {
			continue
		}
		found = true
		eventTurnID := strings.TrimSpace(event.NativeTurnID)
		if eventTurnID != "" {
			knownNativeTurn = true
		}
		if turnID != "" && eventTurnID == turnID {
			matchedNativeTurn = true
		}
	}
	if turnID != "" {
		return matchedNativeTurn
	}
	// An empty native turn is valid only for a historical operation for which
	// the actor has no native turn id at all. It must never wildcard through a
	// row that does know its native turn.
	return found && !knownNativeTurn
}

func providerActivityLedgerOwnsLane(state State, laneID provider.LaneID, event LedgerEvent) bool {
	laneID = provider.LaneID(strings.TrimSpace(string(laneID)))
	if event.ContextExcluded {
		return false
	}
	if laneID == "" {
		return event.LaneID != ""
	}
	if event.LaneID == laneID {
		return true
	}
	return false
}

type ToolState struct {
	Owner ProviderActivityOwner
}

type PlanState struct {
	Owner ProviderActivityOwner
	Event provider.PlanEvent
}

type PermissionState struct {
	Owner ProviderActivityOwner
	Event provider.PermissionEvent
}

type BackgroundState struct {
	Owner ProviderActivityOwner
	Event provider.BackgroundEvent
}

type CompactionState struct {
	Owner ProviderActivityOwner
	Event provider.CompactionEvent
}

// EnvironmentState is the actor-owned, chat-scoped Entorno projection.  The
// payload/checkpoint bytes are deliberately opaque to the chat reducer: Git
// inspection and checkpoint execution belong to the ACP/filesystem adapter,
// while the actor owns the durable association with this exact ChatID/tab.
// Keeping the observed data in the actor means a renderer reconnect or daemon
// restart can rebuild the rail without consulting a disposable manager cache.
type EnvironmentState struct {
	Revision    uint64
	TabID       string
	CWD         string
	Payload     json.RawMessage
	Checkpoints json.RawMessage
	Reference   json.RawMessage
}

func (e EnvironmentState) Clone() EnvironmentState {
	e.Payload = append(json.RawMessage(nil), e.Payload...)
	e.Checkpoints = append(json.RawMessage(nil), e.Checkpoints...)
	e.Reference = append(json.RawMessage(nil), e.Reference...)
	return e
}

func (e EnvironmentState) Validate() error {
	if len(e.Payload) > 0 && !json.Valid(e.Payload) {
		return errors.New("chat environment payload is invalid json")
	}
	if len(e.Checkpoints) > 0 && !json.Valid(e.Checkpoints) {
		return errors.New("chat environment checkpoints are invalid json")
	}
	if len(e.Reference) > 0 && !json.Valid(e.Reference) {
		return errors.New("chat environment reference is invalid json")
	}
	return nil
}

// CheckpointRestoreTarget returns the exact checkpoint object from the
// actor-owned environment observation. Callers must pass its returned bytes
// and digest into RestoreCheckpoint; the manager's mutable checkpoint cache is
// not a valid source for an already-observed actor command.
func (s State) CheckpointRestoreTarget(turnSequence int) (json.RawMessage, string, error) {
	return checkpointRestoreTarget(s.Environment.Checkpoints, turnSequence)
}

// ObligationState is the actor-owned answer to "what does this chat still owe
// the user?". It is deliberately chat-scoped rather than turn-scoped: one
// request may park and resume across several provider turns. Executor liveness
// and provider stop reasons are evidence supplied to the reducer, never a
// second persisted state machine.
type ObligationState struct {
	State       string
	Source      string
	Note        string
	OpenedAt    string
	UpdatedAt   string
	PromptID    string
	ParkedSince string
}

func (o *ObligationState) Clone() *ObligationState {
	if o == nil {
		return nil
	}
	copy := *o
	return &copy
}

func (o ObligationState) Validate() error {
	switch strings.TrimSpace(o.State) {
	case "working", "parked", "needs_input", "done", "stalled":
	default:
		return errors.New("chat obligation has invalid state")
	}
	if strings.TrimSpace(o.OpenedAt) == "" || strings.TrimSpace(o.UpdatedAt) == "" {
		return errors.New("chat obligation is missing durable timestamps")
	}
	if o.State == "parked" && strings.TrimSpace(o.ParkedSince) == "" {
		return errors.New("parked chat obligation is missing parked timestamp")
	}
	return nil
}

type OutboxStatus string

const (
	OutboxPending    OutboxStatus = "pending"
	OutboxDispatched OutboxStatus = "dispatched"
	OutboxAccepted   OutboxStatus = "accepted"
	OutboxConsumed   OutboxStatus = "consumed"
	OutboxAmbiguous  OutboxStatus = "ambiguous"
	OutboxCompleted  OutboxStatus = "completed"
	OutboxFailed     OutboxStatus = "failed"
)

type EffectKind string

const (
	EffectCreateLane        EffectKind = "create_lane"
	EffectResumeLane        EffectKind = "resume_lane"
	EffectImportContext     EffectKind = "import_context"
	EffectStartTurn         EffectKind = "start_turn"
	EffectReconcileTurn     EffectKind = "reconcile_turn"
	EffectSteerTurn         EffectKind = "steer_turn"
	EffectCancelTurn        EffectKind = "cancel_turn"
	EffectPermission        EffectKind = "resolve_permission"
	EffectBackground        EffectKind = "background_action"
	EffectCheckpointRestore EffectKind = "checkpoint_restore"
	EffectDetachLane        EffectKind = "detach_lane"
	EffectExternalMutation  EffectKind = "external_mutation"
	EffectDeleteChat        EffectKind = "delete_chat"
)

// OutboxEntry is the durable write-ahead record for one external effect. An
// executor must claim Pending before making a provider call. On restart,
// Dispatched delivery/import effects reconcile; they are never blindly resent.
type OutboxEntry struct {
	ID                          string
	Kind                        EffectKind
	Status                      OutboxStatus
	LaneID                      provider.LaneID
	OperationID                 provider.OperationID
	Owner                       provider.AttachmentOwner
	ConnectionID                string
	MutationKind                string
	MutationMethod              string
	MutationDigest              string
	CWD                         string
	ModelID                     string
	ModeID                      string
	Input                       *QueueEntry
	Generation                  uint64
	Reconcile                   bool
	CreateAfterCandidateAbsence bool
	From                        uint64
	To                          uint64
	Thread                      provider.ThreadRef
	Turn                        provider.TurnRef
	RequestID                   string
	OptionID                    string
	ChatID                      string
	TabID                       string
	Background                  *BackgroundAction
	TurnSequence                int
	ObservedAtUnixMS            int64
	Checkpoint                  json.RawMessage
	CheckpointDigest            string
	Result                      json.RawMessage
	Batch                       *ContextBatch
	LastError                   provider.ErrorKind
}

type State struct {
	ChatID   string
	Revision uint64
	// Initialized distinguishes a committed chat from an empty actor file left
	// by a create attempt that never reached its durable transition.
	Initialized bool
	// CreationOperationID and CreationDigest are the immutable receipt for the
	// one actor-native chat creation. They make a lost create reply replay-safe:
	// the same request returns the existing ChatID, while the same id with
	// different content fails closed. Chats created before this receipt existed
	// deliberately leave both fields empty.
	CreationOperationID provider.OperationID
	CreationDigest      string
	// Deleted is a durable tombstone. A deleted chat can never be recreated
	// from a stale renderer mirror after this bit commits.
	Deleted             bool
	DeletionOperationID provider.OperationID
	Presentation        PresentationState
	// Environment is the durable actor projection for the Entorno rail. The
	// manager may observe Git and execute filesystem operations, but it is not
	// allowed to be the post-cutover authority for this chat-scoped state.
	Environment EnvironmentState
	// ContextFloor is the contiguous visible prefix explicitly excluded from
	// every lane created in this chat (currently only actor-native forks).
	ContextFloor                   uint64
	Ledger                         []LedgerEvent
	Lanes                          map[provider.LaneID]LaneState
	ActiveLaneID                   provider.LaneID
	DesiredLaneID                  provider.LaneID
	StagedQueue                    []StagedQueueEntry
	QueueControl                   QueueControlState
	QueueMutationReceipts          map[provider.OperationID]QueueMutationReceipt
	PresentationMutationReceipts   map[provider.OperationID]PresentationMutationReceipt
	RuntimeControlMutationReceipts map[provider.OperationID]RuntimeControlMutationReceipt
	WorkspaceMutationReceipts      map[provider.OperationID]WorkspaceMutationReceipt
	LaneSelectionMutationReceipts  map[provider.OperationID]LaneSelectionMutationReceipt
	CancelMutationReceipts         map[provider.OperationID]CancelMutationReceipt
	AgentWaitObservationReceipts   map[provider.OperationID]AgentWaitObservationReceipt
	Queue                          []QueueEntry
	Foreground                     *ForegroundTurn
	PendingSteer                   *PendingSteer
	PendingCancel                  *PendingCancel
	Operations                     map[provider.OperationID]struct{}
	Outbox                         []OutboxEntry
	Tools                          map[string]ToolState
	Plans                          map[provider.OperationID]PlanState
	Permissions                    map[string]PermissionState
	Background                     map[string]BackgroundState
	Usage                          map[provider.LaneID]provider.UsageEvent
	Compactions                    map[provider.LaneID]CompactionState
	Transport                      map[provider.LaneID]provider.TransportHealthEvent
	Obligation                     *ObligationState
}

func NewState(chatID string) (State, error) {
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return State{}, errors.New("chat state requires chat id")
	}
	return State{
		ChatID:                         chatID,
		Lanes:                          make(map[provider.LaneID]LaneState),
		Operations:                     make(map[provider.OperationID]struct{}),
		QueueControl:                   QueueControlState{ResumeReceipts: make(map[provider.OperationID]QueueResumeReceipt)},
		QueueMutationReceipts:          make(map[provider.OperationID]QueueMutationReceipt),
		PresentationMutationReceipts:   make(map[provider.OperationID]PresentationMutationReceipt),
		RuntimeControlMutationReceipts: make(map[provider.OperationID]RuntimeControlMutationReceipt),
		WorkspaceMutationReceipts:      make(map[provider.OperationID]WorkspaceMutationReceipt),
		LaneSelectionMutationReceipts:  make(map[provider.OperationID]LaneSelectionMutationReceipt),
		CancelMutationReceipts:         make(map[provider.OperationID]CancelMutationReceipt),
		AgentWaitObservationReceipts:   make(map[provider.OperationID]AgentWaitObservationReceipt),
		Tools:                          make(map[string]ToolState),
		Plans:                          make(map[provider.OperationID]PlanState),
		Permissions:                    make(map[string]PermissionState),
		Background:                     make(map[string]BackgroundState),
		Usage:                          make(map[provider.LaneID]provider.UsageEvent),
		Compactions:                    make(map[provider.LaneID]CompactionState),
		Transport:                      make(map[provider.LaneID]provider.TransportHealthEvent),
	}, nil
}

func (s State) Clone() State {
	out := s
	out.Presentation = s.Presentation.Clone()
	out.Environment = s.Environment.Clone()
	out.Obligation = s.Obligation.Clone()
	out.Ledger = make([]LedgerEvent, len(s.Ledger))
	for i, event := range s.Ledger {
		event.Attachments = append([]provider.Attachment(nil), event.Attachments...)
		if event.TurnTerminal != nil {
			terminal := *event.TurnTerminal
			event.TurnTerminal = &terminal
		}
		event.Timeline = cloneTimeline(event.Timeline)
		event.Permission = clonePermission(event.Permission)
		event.Terminal = cloneTerminal(event.Terminal)
		out.Ledger[i] = event
	}
	out.Queue = cloneQueue(s.Queue)
	out.StagedQueue = cloneStagedQueue(s.StagedQueue)
	out.QueueControl.ResumeReceipts = make(map[provider.OperationID]QueueResumeReceipt, len(s.QueueControl.ResumeReceipts))
	for operationID, receipt := range s.QueueControl.ResumeReceipts {
		out.QueueControl.ResumeReceipts[operationID] = receipt
	}
	out.QueueMutationReceipts = make(map[provider.OperationID]QueueMutationReceipt, len(s.QueueMutationReceipts))
	for operationID, receipt := range s.QueueMutationReceipts {
		out.QueueMutationReceipts[operationID] = receipt
	}
	out.PresentationMutationReceipts = make(map[provider.OperationID]PresentationMutationReceipt, len(s.PresentationMutationReceipts))
	for operationID, receipt := range s.PresentationMutationReceipts {
		out.PresentationMutationReceipts[operationID] = receipt
	}
	out.RuntimeControlMutationReceipts = make(map[provider.OperationID]RuntimeControlMutationReceipt, len(s.RuntimeControlMutationReceipts))
	for operationID, receipt := range s.RuntimeControlMutationReceipts {
		out.RuntimeControlMutationReceipts[operationID] = receipt
	}
	out.WorkspaceMutationReceipts = make(map[provider.OperationID]WorkspaceMutationReceipt, len(s.WorkspaceMutationReceipts))
	for operationID, receipt := range s.WorkspaceMutationReceipts {
		receipt.DetachOperationIDs = append([]provider.OperationID(nil), receipt.DetachOperationIDs...)
		out.WorkspaceMutationReceipts[operationID] = receipt
	}
	out.LaneSelectionMutationReceipts = make(map[provider.OperationID]LaneSelectionMutationReceipt, len(s.LaneSelectionMutationReceipts))
	for operationID, receipt := range s.LaneSelectionMutationReceipts {
		out.LaneSelectionMutationReceipts[operationID] = receipt
	}
	out.CancelMutationReceipts = make(map[provider.OperationID]CancelMutationReceipt, len(s.CancelMutationReceipts))
	for operationID, receipt := range s.CancelMutationReceipts {
		out.CancelMutationReceipts[operationID] = receipt
	}
	out.AgentWaitObservationReceipts = make(map[provider.OperationID]AgentWaitObservationReceipt, len(s.AgentWaitObservationReceipts))
	for operationID, receipt := range s.AgentWaitObservationReceipts {
		out.AgentWaitObservationReceipts[operationID] = receipt
	}
	out.Outbox = make([]OutboxEntry, len(s.Outbox))
	for i, entry := range s.Outbox {
		if entry.Input != nil {
			input := *entry.Input
			input.Attachments = append([]provider.Attachment(nil), entry.Input.Attachments...)
			entry.Input = &input
		}
		if entry.Batch != nil {
			batch := cloneContextBatch(*entry.Batch)
			entry.Batch = &batch
		}
		if entry.Background != nil {
			action := entry.Background.Clone()
			entry.Background = &action
		}
		entry.Checkpoint = append(json.RawMessage(nil), entry.Checkpoint...)
		entry.Result = append(json.RawMessage(nil), entry.Result...)
		out.Outbox[i] = entry
	}
	out.Lanes = make(map[provider.LaneID]LaneState, len(s.Lanes))
	for id, lane := range s.Lanes {
		if lane.Provision != nil {
			provision := lane.Provision.Normalize()
			lane.Provision = &provision
		}
		if lane.PendingImport != nil {
			pending := *lane.PendingImport
			pending.Batch = cloneContextBatch(lane.PendingImport.Batch)
			lane.PendingImport = &pending
		}
		lane.Coverage = make(map[uint64]CoverageRecord, len(lane.Coverage))
		for sequence, record := range s.Lanes[id].Coverage {
			lane.Coverage[sequence] = record
		}
		if lane.Attachment != nil {
			attachment := lane.Attachment.Clone()
			lane.Attachment = &attachment
		}
		out.Lanes[id] = lane
	}
	out.Operations = make(map[provider.OperationID]struct{}, len(s.Operations))
	for operationID := range s.Operations {
		out.Operations[operationID] = struct{}{}
	}
	out.Tools = make(map[string]ToolState, len(s.Tools))
	for id, value := range s.Tools {
		out.Tools[id] = value
	}
	out.Plans = make(map[provider.OperationID]PlanState, len(s.Plans))
	for id, value := range s.Plans {
		value.Event.Entries = append([]provider.PlanEntry(nil), value.Event.Entries...)
		out.Plans[id] = value
	}
	out.Permissions = make(map[string]PermissionState, len(s.Permissions))
	for id, value := range s.Permissions {
		cloned := clonePermission(&value.Event)
		value.Event = *cloned
		out.Permissions[id] = value
	}
	out.Background = make(map[string]BackgroundState, len(s.Background))
	for id, value := range s.Background {
		if value.Event.ExitCode != nil {
			exitCode := *value.Event.ExitCode
			value.Event.ExitCode = &exitCode
		}
		out.Background[id] = value
	}
	out.Usage = make(map[provider.LaneID]provider.UsageEvent, len(s.Usage))
	for id, value := range s.Usage {
		out.Usage[id] = value
	}
	out.Compactions = make(map[provider.LaneID]CompactionState, len(s.Compactions))
	for id, value := range s.Compactions {
		out.Compactions[id] = value
	}
	out.Transport = make(map[provider.LaneID]provider.TransportHealthEvent, len(s.Transport))
	for id, value := range s.Transport {
		out.Transport[id] = value
	}
	if s.Foreground != nil {
		foreground := *s.Foreground
		foreground.Input.Attachments = append([]provider.Attachment(nil), s.Foreground.Input.Attachments...)
		foreground.AssistantAttachments = append([]provider.Attachment(nil), s.Foreground.AssistantAttachments...)
		foreground.Timeline = cloneTimeline(s.Foreground.Timeline)
		foreground.Permission = clonePermission(s.Foreground.Permission)
		out.Foreground = &foreground
	}
	if s.PendingSteer != nil {
		steer := *s.PendingSteer
		steer.Attachments = append([]provider.Attachment(nil), s.PendingSteer.Attachments...)
		out.PendingSteer = &steer
	}
	if s.PendingCancel != nil {
		cancel := *s.PendingCancel
		out.PendingCancel = &cancel
	}
	return out
}

func cloneTimeline(entries []TimelineEntry) []TimelineEntry {
	out := make([]TimelineEntry, len(entries))
	for index, entry := range entries {
		if entry.Thinking != nil {
			value := *entry.Thinking
			entry.Thinking = &value
		}
		if entry.Tool != nil {
			value := *entry.Tool
			value.Attachments = append([]provider.Attachment(nil), entry.Tool.Attachments...)
			entry.Tool = &value
		}
		if entry.Plan != nil {
			value := *entry.Plan
			value.Entries = append([]provider.PlanEntry(nil), entry.Plan.Entries...)
			entry.Plan = &value
		}
		if entry.Compaction != nil {
			value := *entry.Compaction
			entry.Compaction = &value
		}
		if entry.Restored != nil {
			value := *entry.Restored
			entry.Restored = &value
		}
		out[index] = entry
	}
	return out
}

func clonePermission(permission *provider.PermissionEvent) *provider.PermissionEvent {
	if permission == nil {
		return nil
	}
	out := *permission
	out.Options = append([]string(nil), permission.Options...)
	out.OptionDetails = append([]provider.PermissionOption(nil), permission.OptionDetails...)
	if permission.Question != nil {
		question := *permission.Question
		question.Options = append([]provider.PermissionQuestionOption(nil), permission.Question.Options...)
		out.Question = &question
	}
	return &out
}

func cloneTerminal(terminal *provider.TerminalEvent) *provider.TerminalEvent {
	if terminal == nil {
		return nil
	}
	out := *terminal
	if terminal.Code != nil {
		code := *terminal.Code
		out.Code = &code
	}
	out.ConsumedSteerIDs = append([]provider.OperationID(nil), terminal.ConsumedSteerIDs...)
	out.Attachments = append([]provider.Attachment(nil), terminal.Attachments...)
	return &out
}

func cloneContextBatch(batch ContextBatch) ContextBatch {
	batch.EventIDs = append([]string(nil), batch.EventIDs...)
	messages := batch.Messages
	batch.Messages = make([]provider.ContextMessage, len(messages))
	for i, message := range messages {
		message.Attachments = append([]provider.Attachment(nil), message.Attachments...)
		batch.Messages[i] = message
	}
	return batch
}

func cloneQueue(queue []QueueEntry) []QueueEntry {
	out := make([]QueueEntry, len(queue))
	for i, entry := range queue {
		entry.Attachments = append([]provider.Attachment(nil), entry.Attachments...)
		out[i] = entry
	}
	return out
}

func cloneStagedQueue(queue []StagedQueueEntry) []StagedQueueEntry {
	out := make([]StagedQueueEntry, len(queue))
	for i, entry := range queue {
		entry.Attachments = append([]provider.Attachment(nil), entry.Attachments...)
		entry.AttachmentNames = append([]string(nil), entry.AttachmentNames...)
		out[i] = entry
	}
	return out
}

func (s State) LedgerHead() uint64 {
	if len(s.Ledger) == 0 {
		return 0
	}
	return s.Ledger[len(s.Ledger)-1].Sequence
}

func (s State) Validate() error {
	if strings.TrimSpace(s.ChatID) == "" {
		return errors.New("chat state requires chat id")
	}
	if s.Lanes == nil || s.Operations == nil || s.QueueControl.ResumeReceipts == nil || s.QueueMutationReceipts == nil || s.PresentationMutationReceipts == nil || s.RuntimeControlMutationReceipts == nil || s.WorkspaceMutationReceipts == nil || s.LaneSelectionMutationReceipts == nil || s.CancelMutationReceipts == nil || s.AgentWaitObservationReceipts == nil || s.Tools == nil || s.Plans == nil || s.Permissions == nil || s.Background == nil || s.Usage == nil || s.Compactions == nil || s.Transport == nil {
		return errors.New("chat state maps are not initialized")
	}
	if s.Deleted {
		deletionOperationID := provider.NormalizeOperationID(string(s.DeletionOperationID))
		if deletionOperationID == "" || deletionOperationID != s.DeletionOperationID {
			return errors.New("deleted chat tombstone has an invalid deletion operation id")
		}
	} else if s.DeletionOperationID != "" {
		return errors.New("active chat has a deletion operation id")
	}
	if (s.CreationOperationID == "") != (strings.TrimSpace(s.CreationDigest) == "") {
		return errors.New("chat creation receipt is incomplete")
	}
	for operationID, receipt := range s.QueueMutationReceipts {
		if operationID == "" || strings.TrimSpace(receipt.Digest) == "" || receipt.Revision == 0 || receipt.Revision > s.Presentation.AgentQueueRevision {
			return errors.New("chat queue mutation receipt is invalid")
		}
	}
	for operationID, receipt := range s.QueueControl.ResumeReceipts {
		if operationID == "" || receipt.PauseRevision == 0 || receipt.PauseRevision > s.QueueControl.Revision {
			return errors.New("chat queue resume receipt is invalid")
		}
		if _, exists := s.Operations[operationID]; !exists {
			return errors.New("chat queue resume receipt lost its operation")
		}
	}
	if s.QueueControl.Paused && s.QueueControl.Revision == 0 {
		return errors.New("chat queue pause is missing its revision")
	}
	for operationID, receipt := range s.PresentationMutationReceipts {
		if operationID == "" || strings.TrimSpace(receipt.Digest) == "" || receipt.Revision > s.Presentation.PresentationRevision {
			return errors.New("chat presentation mutation receipt is invalid")
		}
	}
	for operationID, receipt := range s.RuntimeControlMutationReceipts {
		if operationID == "" || strings.TrimSpace(receipt.Digest) == "" || receipt.Revision > s.Presentation.RuntimeControlRevision {
			return errors.New("chat runtime-control mutation receipt is invalid")
		}
	}
	for operationID, receipt := range s.WorkspaceMutationReceipts {
		if operationID == "" || strings.TrimSpace(receipt.Digest) == "" || strings.TrimSpace(receipt.CWD) == "" || receipt.Revision > s.Presentation.WorkspaceRevision {
			return errors.New("chat workspace mutation receipt is invalid")
		}
		seenDetachOperations := make(map[provider.OperationID]struct{}, len(receipt.DetachOperationIDs))
		for _, rawDetachOperationID := range receipt.DetachOperationIDs {
			detachOperationID := provider.NormalizeOperationID(string(rawDetachOperationID))
			if detachOperationID == "" || detachOperationID != rawDetachOperationID {
				return errors.New("chat workspace mutation receipt has an invalid detach operation identity")
			}
			if _, duplicate := seenDetachOperations[detachOperationID]; duplicate {
				return errors.New("chat workspace mutation receipt contains duplicate detach operations")
			}
			seenDetachOperations[detachOperationID] = struct{}{}
			if !workspaceReceiptHasDetachEntry(s, detachOperationID) {
				return errors.New("chat workspace mutation receipt is missing a durable detach effect")
			}
		}
	}
	for operationID, receipt := range s.LaneSelectionMutationReceipts {
		if operationID == "" || strings.TrimSpace(receipt.Digest) == "" || receipt.LaneID == "" || receipt.Revision > s.Revision {
			return errors.New("chat lane-selection mutation receipt is invalid")
		}
		if _, exists := s.Lanes[receipt.LaneID]; !exists {
			return errors.New("chat lane-selection mutation receipt references an unknown lane")
		}
	}
	for operationID, receipt := range s.CancelMutationReceipts {
		if operationID == "" || receipt.Cancelled || strings.TrimSpace(receipt.Reason) != "idle" {
			return errors.New("chat cancel mutation receipt is invalid")
		}
		if _, exists := s.Operations[operationID]; !exists {
			return errors.New("chat cancel mutation receipt is not reserved")
		}
	}
	for operationID, receipt := range s.AgentWaitObservationReceipts {
		if operationID == "" || !validAgentWaitObservationDigest(receipt.Digest) {
			return errors.New("chat agent wait observation receipt is invalid")
		}
		if _, exists := s.Operations[operationID]; !exists {
			return errors.New("chat agent wait observation receipt is not reserved")
		}
	}
	if err := s.Presentation.Validate(); err != nil {
		return err
	}
	if err := s.Environment.Validate(); err != nil {
		return err
	}
	if s.Obligation != nil {
		if err := s.Obligation.Validate(); err != nil {
			return err
		}
	}
	if s.ContextFloor > s.LedgerHead() {
		return errors.New("chat context floor is ahead of the semantic ledger")
	}
	var sequence uint64
	for _, event := range s.Ledger {
		sequence++
		if event.Sequence != sequence {
			return fmt.Errorf("ledger sequence %d is not contiguous at %d", event.Sequence, sequence)
		}
		if strings.TrimSpace(event.EventID) == "" || strings.TrimSpace(event.MessageID) == "" || event.OperationID == "" {
			return errors.New("ledger event is missing message or operation identity")
		}
		if err := validateTimeline(event.Timeline); err != nil {
			return fmt.Errorf("ledger event %q timeline: %w", event.EventID, err)
		}
		if event.Permission != nil && strings.TrimSpace(event.Permission.RequestID) == "" {
			return errors.New("ledger permission is missing request identity")
		}
		if event.Role != "assistant" && (len(event.Timeline) > 0 || event.Permission != nil || event.TurnTerminal != nil) {
			return errors.New("non-assistant ledger row owns assistant-only state")
		}
		if event.Terminal != nil && event.Role != "assistant" {
			return errors.New("non-assistant ledger row owns terminal state")
		}
		if event.TurnTerminal != nil && strings.TrimSpace(event.TurnRootID) == "" {
			return errors.New("segmented assistant row is missing its turn root")
		}
		if event.SteerState != "" {
			if event.Role != "user" || strings.TrimSpace(event.TurnRootID) == "" {
				return errors.New("steering row is missing user or turn-root ownership")
			}
			switch event.SteerState {
			case "sending", "accepted", "applied", "uncertain":
			default:
				return errors.New("ledger steering row has an unknown state")
			}
		}
		if event.ContextExcluded {
			if event.LaneID != "" {
				return errors.New("context-excluded ledger event claims child lane ownership")
			}
		} else {
			if event.LaneID == "" || event.ProviderID == "" {
				return errors.New("ledger event is missing lane identity")
			}
			if _, ok := s.Lanes[event.LaneID]; !ok {
				return errors.New("ledger event belongs to an unknown lane")
			}
		}
	}
	for id, lane := range s.Lanes {
		if id == "" || lane.Identity.ID != id {
			return errors.New("lane map identity mismatch")
		}
		if lane.Identity.ChatID != s.ChatID {
			return errors.New("lane belongs to another chat")
		}
		if err := lane.Identity.Validate(); err != nil {
			return err
		}
		if lane.CoveredThrough > s.LedgerHead() {
			return errors.New("lane coverage is ahead of the chat ledger")
		}
		switch lane.Context.ImportMode {
		case provider.ContextImportUnsupported, "":
			if lane.Context.ImportReadback || lane.Context.IdempotentImport {
				return errors.New("unsupported context import advertises transactional guarantees")
			}
		case provider.ContextImportNonSampling:
			if !lane.Context.ImportReadback || !lane.Context.IdempotentImport || lane.Context.MaxImportEvents <= 0 || lane.Context.MaxImportBytes <= 0 {
				return errors.New("non-sampling context import lacks readback, idempotency, or bounded limits")
			}
		default:
			return fmt.Errorf("lane context has unknown import mode %q", lane.Context.ImportMode)
		}
		var covered uint64
		for sequence, record := range lane.Coverage {
			if sequence == 0 || sequence > s.LedgerHead() || record.Sequence != sequence {
				return errors.New("lane coverage record has an invalid sequence")
			}
			if s.Ledger[sequence-1].EventID != record.EventID {
				return errors.New("lane coverage record does not match the immutable ledger event")
			}
			switch record.Status {
			case CoverageNativeSeen, CoverageImported, CoverageSeeded, CoverageExcluded:
			default:
				return fmt.Errorf("lane coverage record has unknown status %q", record.Status)
			}
		}
		for sequence := uint64(1); ; sequence++ {
			if _, ok := lane.Coverage[sequence]; !ok {
				break
			}
			covered = sequence
		}
		if lane.CoveredThrough != covered {
			return errors.New("lane covered-through frontier hides an unresolved gap")
		}
		if !lane.Thread.IsZero() {
			if err := lane.Thread.Validate(lane.Identity.Realm.ProviderID); err != nil {
				return err
			}
		}
		if lane.Provision != nil {
			if !lane.Thread.IsZero() {
				return errors.New("provider lane cannot be provisional and established at once")
			}
			if !lane.Creation.DeferredUntilInput {
				return errors.New("provider lane has a provisional thread without deferred creation capability")
			}
			if err := lane.Provision.Validate(lane.Identity.Realm.ProviderID); err != nil {
				return err
			}
			if !lane.Delivery.StableInputIdentity || !lane.Delivery.ConsumptionReceipt {
				return errors.New("deferred provider lane lacks a stable input consumption receipt")
			}
			if lane.Phase != LaneAbsent && lane.Phase != LaneCreating && lane.Phase != LaneBlocked && lane.Phase != LaneBroken {
				return errors.New("provisional provider lane has an invalid phase")
			}
		}
		if lane.CreateAfterCandidateAbsence {
			if !lane.Thread.IsZero() || !lane.Creation.DeferredUntilInput {
				return errors.New("candidate absence may create only on an unestablished deferred lane")
			}
			for _, record := range lane.Coverage {
				if record.Status == CoverageNativeSeen || record.Status == CoverageImported || record.Status == CoverageSeeded {
					return errors.New("lane with provider-native coverage may not create after candidate absence")
				}
			}
		}
		if lane.InitialSeedPending {
			for _, record := range lane.Coverage {
				if record.Status == CoverageNativeSeen || record.Status == CoverageImported || record.Status == CoverageSeeded {
					return errors.New("lane with consumed provider context still has an initial seed pending")
				}
			}
		}
		if lane.Phase == LaneCreating && !lane.Thread.IsZero() {
			return errors.New("established lane cannot re-enter creating phase")
		}
	}
	validateOwner := func(owner ProviderActivityOwner) error { return validateProviderActivityOwner(s, owner) }
	for id, value := range s.Tools {
		if strings.TrimSpace(id) == "" {
			return errors.New("tool activity is missing its provider id")
		}
		if err := validateOwner(value.Owner); err != nil {
			return err
		}
		if value.Owner.OperationID == "" {
			return errors.New("background activity is missing immutable operation ownership")
		}
	}
	for operationID, value := range s.Plans {
		if operationID == "" || value.Owner.OperationID != operationID {
			return errors.New("plan activity operation ownership mismatch")
		}
		if err := validateOwner(value.Owner); err != nil {
			return err
		}
	}
	for id, value := range s.Permissions {
		if strings.TrimSpace(id) == "" {
			return errors.New("permission activity is missing its provider id")
		}
		if err := validateOwner(value.Owner); err != nil {
			return err
		}
	}
	for id, value := range s.Background {
		if strings.TrimSpace(id) == "" {
			return errors.New("background activity is missing its provider id")
		}
		if err := validateOwner(value.Owner); err != nil {
			return err
		}
	}
	for laneID := range s.Usage {
		if _, ok := s.Lanes[laneID]; !ok {
			return errors.New("usage activity belongs to an unknown lane")
		}
	}
	for laneID, value := range s.Compactions {
		if laneID != value.Owner.LaneID {
			return errors.New("compaction activity lane ownership mismatch")
		}
		if err := validateOwner(value.Owner); err != nil {
			return err
		}
	}
	for laneID := range s.Transport {
		if _, ok := s.Lanes[laneID]; !ok {
			return errors.New("transport health belongs to an unknown lane")
		}
	}
	if s.ActiveLaneID != "" {
		if _, ok := s.Lanes[s.ActiveLaneID]; !ok {
			return errors.New("active lane does not exist")
		}
	}
	seenStagedQueue := make(map[string]struct{}, len(s.StagedQueue))
	for _, entry := range s.StagedQueue {
		if strings.TrimSpace(entry.ID) == "" || strings.TrimSpace(entry.Text) == "" && len(entry.Attachments) == 0 {
			return errors.New("staged queue entry is missing identity or content")
		}
		if _, duplicate := seenStagedQueue[entry.ID]; duplicate {
			return errors.New("staged queue contains duplicate identity")
		}
		seenStagedQueue[entry.ID] = struct{}{}
		switch entry.Source {
		case "", "agent", "host":
		default:
			return errors.New("staged queue entry has invalid source")
		}
		switch entry.Delivery {
		case "", "auto", "queue", "steer":
		default:
			return errors.New("staged queue entry has invalid delivery")
		}
	}
	if s.DesiredLaneID != "" {
		if _, ok := s.Lanes[s.DesiredLaneID]; !ok {
			return errors.New("desired lane does not exist")
		}
	}
	if s.Foreground != nil {
		lane, ok := s.Lanes[s.Foreground.LaneID]
		if !ok {
			return errors.New("foreground lane does not exist")
		}
		validPhase := lane.Phase == LaneRunning && (s.Foreground.Status == ForegroundDispatching || s.Foreground.Status == ForegroundRunning)
		validPhase = validPhase || lane.Phase == LaneCreating && lane.Provision != nil &&
			(s.Foreground.Status == ForegroundDispatching || s.Foreground.Status == ForegroundRunning || s.Foreground.Status == ForegroundReconciling)
		// A persisted-but-not-yet-dispatched input survives a daemon restart. The
		// exact lane must attach before that Pending outbox entry becomes
		// executable, so dispatching may temporarily coexist with LaneResuming.
		validPhase = validPhase || lane.Phase == LaneResuming && s.Foreground.Status == ForegroundDispatching
		validPhase = validPhase || lane.Phase == LaneReconciling && s.Foreground.Status == ForegroundReconciling
		validPhase = validPhase || lane.Phase == LaneResuming && s.Foreground.Status == ForegroundReconciling
		validPhase = validPhase || (lane.Phase == LaneBlocked || lane.Phase == LaneBroken) && s.Foreground.Status == ForegroundUncertain
		if !validPhase {
			return fmt.Errorf("foreground turn has incompatible lane phase %q", lane.Phase)
		}
		if err := validateTimeline(s.Foreground.Timeline); err != nil {
			return fmt.Errorf("foreground timeline: %w", err)
		}
		if strings.TrimSpace(s.Foreground.RootAssistantMessageID) == "" || strings.TrimSpace(s.Foreground.CurrentAssistantMessageID) == "" {
			return errors.New("foreground turn is missing assistant segment identity")
		}
	}
	if s.PendingSteer != nil {
		if s.Foreground == nil || s.PendingSteer.LaneID != s.Foreground.LaneID || s.PendingSteer.Turn != s.Foreground.Turn {
			return errors.New("pending steer lost its foreground turn owner")
		}
		if s.PendingSteer.OperationID == "" || strings.TrimSpace(s.PendingSteer.Text) == "" && len(s.PendingSteer.Attachments) == 0 {
			return errors.New("pending steer is incomplete")
		}
		switch s.PendingSteer.Status {
		case SteerDispatching, SteerAccepted, SteerUncertain:
		default:
			return errors.New("pending steer has an unknown status")
		}
	}
	if s.PendingCancel != nil {
		if s.Foreground == nil || s.PendingCancel.LaneID != s.Foreground.LaneID || s.PendingCancel.Turn != s.Foreground.Turn || s.PendingCancel.OperationID == "" {
			return errors.New("pending cancellation lost its foreground turn owner")
		}
	}
	seenEffects := make(map[string]struct{}, len(s.Outbox))
	deleteEffectSeen := false
	for _, effect := range s.Outbox {
		if strings.TrimSpace(effect.ID) == "" || effect.Kind == "" || effect.Status == "" {
			return errors.New("chat outbox contains an incomplete effect")
		}
		if _, duplicate := seenEffects[effect.ID]; duplicate {
			return errors.New("chat outbox contains duplicate effect id")
		}
		seenEffects[effect.ID] = struct{}{}
		if effect.Kind == EffectDeleteChat {
			if deleteEffectSeen {
				return errors.New("chat outbox contains duplicate delete-chat effects")
			}
			deleteEffectSeen = true
			if !s.Deleted ||
				effect.ID != deleteChatEffectID(s.ChatID) ||
				effect.OperationID != s.DeletionOperationID ||
				effect.ChatID != s.ChatID ||
				effect.TabID != s.Presentation.TabID ||
				effect.LaneID != "" {
				return errors.New("delete-chat outbox effect lost its exact tombstone identity")
			}
		} else if effect.Kind == EffectCheckpointRestore {
			if effect.LaneID != "" || effect.OperationID == "" || effect.TurnSequence <= 0 || effect.ObservedAtUnixMS <= 0 ||
				validateCheckpointRestorePayload(effect.Checkpoint, effect.TurnSequence, effect.CheckpointDigest) != nil {
				return errors.New("checkpoint-restore outbox effect lost its immutable request")
			}
		} else if effect.Kind == EffectDetachLane {
			lane, ok := s.Lanes[effect.LaneID]
			if !ok || effect.OperationID == "" || strings.TrimSpace(effect.ConnectionID) == "" || effect.Generation == 0 {
				return errors.New("lane-detach outbox effect lost its immutable request")
			}
			if lane.Identity.ID != effect.LaneID || lane.Identity.ChatID != s.ChatID ||
				effect.Owner.TabID == "" ||
				effect.OperationID != DetachOperationID(s.ChatID, effect.LaneID, effect.ConnectionID, effect.Generation) ||
				effect.ID != detachEffectID(effect.OperationID) {
				return errors.New("lane-detach outbox effect changed its lane identity")
			}
			// A Pending effect must still fence the exact live attachment before the
			// coordinator is allowed to claim it. A Dispatched effect may instead
			// observe a changed attachment after a crash; recovery must be able to
			// convert that unknown-acceptance window to Ambiguous without resending.
			if effect.Status == OutboxPending {
				if effect.Owner != lane.Owner || lane.ConnectionGeneration != effect.Generation || lane.Attachment == nil ||
					strings.TrimSpace(lane.Attachment.ConnectionID) != strings.TrimSpace(effect.ConnectionID) {
					return errors.New("lane-detach outbox effect changed its exact attachment")
				}
			}
		} else if effect.Kind == EffectExternalMutation {
			if effect.LaneID != "" || strings.TrimSpace(effect.ChatID) != s.ChatID ||
				effect.OperationID == "" || strings.TrimSpace(effect.MutationKind) == "" ||
				strings.TrimSpace(effect.MutationMethod) == "" || strings.TrimSpace(effect.MutationDigest) == "" ||
				strings.TrimSpace(effect.TabID) == "" {
				return errors.New("external mutation outbox effect lost its immutable request")
			}
		} else {
			if effect.LaneID == "" {
				return errors.New("provider outbox effect is missing lane identity")
			}
			if _, ok := s.Lanes[effect.LaneID]; !ok {
				return errors.New("chat outbox effect belongs to an unknown lane")
			}
		}
		switch effect.Kind {
		case EffectCreateLane, EffectResumeLane, EffectImportContext, EffectStartTurn, EffectReconcileTurn, EffectSteerTurn, EffectCancelTurn, EffectPermission, EffectBackground, EffectCheckpointRestore, EffectDetachLane, EffectExternalMutation, EffectDeleteChat:
		default:
			return fmt.Errorf("chat outbox contains unknown effect kind %q", effect.Kind)
		}
		switch effect.Status {
		case OutboxPending, OutboxDispatched, OutboxAccepted, OutboxConsumed, OutboxAmbiguous, OutboxCompleted, OutboxFailed:
		default:
			return fmt.Errorf("chat outbox contains unknown status %q", effect.Status)
		}
		if effect.Kind == EffectStartTurn && effect.Input == nil {
			return errors.New("start-turn outbox effect is missing its immutable input")
		}
		if effect.Kind == EffectImportContext && effect.Batch == nil {
			return errors.New("context-import outbox effect is missing its immutable projection")
		}
		if effect.Kind == EffectSteerTurn && effect.Input == nil {
			return errors.New("steer outbox effect is missing its immutable input")
		}
		if effect.Kind == EffectPermission && (strings.TrimSpace(effect.RequestID) == "" || strings.TrimSpace(effect.OptionID) == "") {
			return errors.New("permission outbox effect is missing its immutable decision")
		}
		if effect.Kind == EffectBackground {
			if effect.Background == nil || effect.Background.OperationID != effect.OperationID {
				return errors.New("background outbox effect is missing its immutable action")
			}
			if err := effect.Background.Validate(s); err != nil {
				return fmt.Errorf("background outbox effect: %w", err)
			}
			if len(effect.Result) > 0 && !json.Valid(effect.Result) {
				return errors.New("background outbox receipt is not valid json")
			}
		}
		if effect.Kind == EffectCheckpointRestore && len(effect.Result) > 0 && !json.Valid(effect.Result) {
			return errors.New("checkpoint-restore outbox receipt is not valid json")
		}
	}
	if s.Deleted && !deleteEffectSeen {
		return errors.New("deleted chat tombstone has no durable delete-chat effect")
	}
	return nil
}

func validAgentWaitObservationDigest(digest string) bool {
	if digest == "" || len(digest) > 256 {
		return false
	}
	for index := 0; index < len(digest); index++ {
		if digest[index] < 0x21 || digest[index] > 0x7e {
			return false
		}
	}
	return true
}

func workspaceReceiptHasDetachEntry(state State, operationID provider.OperationID) bool {
	for _, entry := range state.Outbox {
		if entry.Kind == EffectDetachLane && entry.OperationID == operationID {
			return true
		}
	}
	return false
}

func validateTimeline(entries []TimelineEntry) error {
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if strings.TrimSpace(entry.Key) == "" || entry.At < 0 {
			return errors.New("timeline entry is missing identity or has a negative offset")
		}
		if _, duplicate := seen[entry.Key]; duplicate {
			return errors.New("timeline contains duplicate identity")
		}
		seen[entry.Key] = struct{}{}
		payloads := 0
		for _, present := range []bool{entry.Thinking != nil, entry.Tool != nil, entry.Plan != nil, entry.Compaction != nil, entry.Restored != nil} {
			if present {
				payloads++
			}
		}
		if payloads != 1 {
			return errors.New("timeline entry does not have exactly one typed payload")
		}
		switch entry.Kind {
		case provider.EventThinkingUpdate:
			if entry.Thinking == nil {
				return errors.New("thinking timeline entry has wrong payload")
			}
		case provider.EventToolUpdate:
			if entry.Tool == nil || strings.TrimSpace(entry.Tool.ToolCallID) == "" {
				return errors.New("tool timeline entry has wrong payload")
			}
		case provider.EventPlanUpdate:
			if entry.Plan == nil {
				return errors.New("plan timeline entry has wrong payload")
			}
		case provider.EventCompactionStarted, provider.EventCompactionCheckpoint:
			if entry.Compaction == nil {
				return errors.New("compaction timeline entry has wrong payload")
			}
		case provider.EventCheckpointRestored:
			if entry.Restored == nil || entry.Restored.TurnSequence <= 0 {
				return errors.New("checkpoint restore timeline entry has wrong payload")
			}
		default:
			return errors.New("timeline entry has unsupported kind")
		}
	}
	return nil
}
