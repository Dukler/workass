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
	SteerAnchor          *SteerAnchor
	SteerBoundary        string
	SteerContinuationID  string
	SteerContinuationFor string
	TurnRootID           string
	TurnTerminal         *bool
	TurnStartedAt        int64
	Interrupted          bool
	RetryPrompt          string
	Terminal             *provider.TerminalEvent
	Legacy               bool
	// ContextExcluded marks a visible row copied by an explicit user fork. It
	// has no child-lane ownership and is never imported into the fresh provider
	// thread; unlike Legacy, this exclusion is an intentional product action.
	ContextExcluded bool
	Timeline        []TimelineEntry
	Permission      *provider.PermissionEvent
}

type SteerAnchor struct {
	AssistantMessageID string
	ContentOffset      int
	ResultOffset       int
	EventCount         int
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
	CoverageExcluded   CoverageStatus = "excluded"
)

type CoverageRecord struct {
	Sequence   uint64
	EventID    string
	Status     CoverageStatus
	DeliveryID provider.OperationID
}

type LaneState struct {
	Identity             provider.LaneIdentity
	Owner                provider.AttachmentOwner
	CWD                  string
	ModelID              string
	ModeID               string
	Thread               provider.ThreadRef
	Phase                LanePhase
	CoveredThrough       uint64
	Coverage             map[uint64]CoverageRecord
	ConnectionGeneration uint64
	LastEventSequence    uint64
	CreateGeneration     uint64
	Context              provider.ContextCapabilities
	Delivery             provider.DeliveryCapabilities
	PendingImport        *PendingImport
	LastError            provider.ErrorKind
	Attachment           *provider.LaneAttachmentSnapshot
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
	Revision     uint64
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
	Digest   string
	Revision uint64
	CWD      string
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

func validateProviderActivityOwner(state State, owner ProviderActivityOwner) error {
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
	return nil
}

type ToolState struct {
	Owner ProviderActivityOwner
	Event provider.ToolEvent
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
	EffectDeleteChat        EffectKind = "delete_chat"
)

// OutboxEntry is the durable write-ahead record for one external effect. An
// executor must claim Pending before making a provider call. On restart,
// Dispatched delivery/import effects reconcile; they are never blindly resent.
type OutboxEntry struct {
	ID               string
	Kind             EffectKind
	Status           OutboxStatus
	LaneID           provider.LaneID
	OperationID      provider.OperationID
	Owner            provider.AttachmentOwner
	CWD              string
	ModelID          string
	ModeID           string
	Input            *QueueEntry
	Generation       uint64
	Reconcile        bool
	From             uint64
	To               uint64
	Thread           provider.ThreadRef
	Turn             provider.TurnRef
	RequestID        string
	OptionID         string
	ChatID           string
	TabID            string
	Background       *BackgroundAction
	TurnSequence     int
	ObservedAtUnixMS int64
	Result           json.RawMessage
	Batch            *ContextBatch
	LastError        provider.ErrorKind
}

type State struct {
	ChatID   string
	Revision uint64
	// Initialized distinguishes a genuinely new actor-owned chat from an empty
	// pre-cutover sidecar that still requires legacy reconciliation. Migration
	// is evidence about the old mirror only; it must not be forged for new chats.
	Initialized bool
	// CreationOperationID and CreationDigest are the immutable receipt for the
	// one actor-native chat creation. They make a lost create reply replay-safe:
	// the same request returns the existing ChatID, while the same id with
	// different content fails closed. Legacy-migrated chats deliberately leave
	// both fields empty because their authority is the migration receipt.
	CreationOperationID provider.OperationID
	CreationDigest      string
	// Deleted is a durable tombstone. A migrated chat can never be resurrected
	// from a stale renderer mirror or legacy JSONL after this bit commits.
	Deleted             bool
	DeletionOperationID provider.OperationID
	Presentation        PresentationState
	Migration           MigrationState
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
	QueueMutationReceipts          map[provider.OperationID]QueueMutationReceipt
	PresentationMutationReceipts   map[provider.OperationID]PresentationMutationReceipt
	RuntimeControlMutationReceipts map[provider.OperationID]RuntimeControlMutationReceipt
	WorkspaceMutationReceipts      map[provider.OperationID]WorkspaceMutationReceipt
	LaneSelectionMutationReceipts  map[provider.OperationID]LaneSelectionMutationReceipt
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
		QueueMutationReceipts:          make(map[provider.OperationID]QueueMutationReceipt),
		PresentationMutationReceipts:   make(map[provider.OperationID]PresentationMutationReceipt),
		RuntimeControlMutationReceipts: make(map[provider.OperationID]RuntimeControlMutationReceipt),
		WorkspaceMutationReceipts:      make(map[provider.OperationID]WorkspaceMutationReceipt),
		LaneSelectionMutationReceipts:  make(map[provider.OperationID]LaneSelectionMutationReceipt),
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
		out.WorkspaceMutationReceipts[operationID] = receipt
	}
	out.LaneSelectionMutationReceipts = make(map[provider.OperationID]LaneSelectionMutationReceipt, len(s.LaneSelectionMutationReceipts))
	for operationID, receipt := range s.LaneSelectionMutationReceipts {
		out.LaneSelectionMutationReceipts[operationID] = receipt
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
		entry.Result = append(json.RawMessage(nil), entry.Result...)
		out.Outbox[i] = entry
	}
	out.Lanes = make(map[provider.LaneID]LaneState, len(s.Lanes))
	for id, lane := range s.Lanes {
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
		value.Event.Attachments = append([]provider.Attachment(nil), value.Event.Attachments...)
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
	if s.Lanes == nil || s.Operations == nil || s.QueueMutationReceipts == nil || s.PresentationMutationReceipts == nil || s.RuntimeControlMutationReceipts == nil || s.WorkspaceMutationReceipts == nil || s.LaneSelectionMutationReceipts == nil || s.Tools == nil || s.Plans == nil || s.Permissions == nil || s.Background == nil || s.Usage == nil || s.Compactions == nil || s.Transport == nil {
		return errors.New("chat state maps are not initialized")
	}
	if (s.CreationOperationID == "") != (strings.TrimSpace(s.CreationDigest) == "") {
		return errors.New("chat creation receipt is incomplete")
	}
	for operationID, receipt := range s.QueueMutationReceipts {
		if operationID == "" || strings.TrimSpace(receipt.Digest) == "" || receipt.Revision == 0 || receipt.Revision > s.Presentation.AgentQueueRevision {
			return errors.New("chat queue mutation receipt is invalid")
		}
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
	}
	for operationID, receipt := range s.LaneSelectionMutationReceipts {
		if operationID == "" || strings.TrimSpace(receipt.Digest) == "" || receipt.LaneID == "" || receipt.Revision > s.Revision {
			return errors.New("chat lane-selection mutation receipt is invalid")
		}
		if _, exists := s.Lanes[receipt.LaneID]; !exists {
			return errors.New("chat lane-selection mutation receipt references an unknown lane")
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
	if s.Migration.Complete && (s.Migration.Version == 0 || strings.TrimSpace(s.Migration.Digest) == "") {
		return errors.New("completed chat migration is missing version or digest")
	}
	if s.Migration.BlockedError != "" && !s.Migration.Complete {
		return errors.New("incomplete chat migration cannot be quarantined")
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
		} else if event.Legacy {
			if event.LaneID != "" || event.ProviderID != "" {
				return errors.New("legacy ledger event guessed provider attribution")
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
			case CoverageNativeSeen, CoverageImported, CoverageExcluded:
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
	for _, effect := range s.Outbox {
		if strings.TrimSpace(effect.ID) == "" || effect.Kind == "" || effect.Status == "" {
			return errors.New("chat outbox contains an incomplete effect")
		}
		if _, duplicate := seenEffects[effect.ID]; duplicate {
			return errors.New("chat outbox contains duplicate effect id")
		}
		seenEffects[effect.ID] = struct{}{}
		if effect.Kind == EffectDeleteChat {
			if !s.Deleted || strings.TrimSpace(effect.ChatID) != s.ChatID || effect.LaneID != "" {
				return errors.New("delete-chat outbox effect lost its tombstone identity")
			}
		} else if effect.Kind == EffectCheckpointRestore {
			if effect.LaneID != "" || effect.OperationID == "" || effect.TurnSequence <= 0 || effect.ObservedAtUnixMS <= 0 {
				return errors.New("checkpoint-restore outbox effect lost its immutable request")
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
		case EffectCreateLane, EffectResumeLane, EffectImportContext, EffectStartTurn, EffectReconcileTurn, EffectSteerTurn, EffectCancelTurn, EffectPermission, EffectBackground, EffectCheckpointRestore, EffectDeleteChat:
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
	return nil
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
