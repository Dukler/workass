package chat

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"

	"workass/internal/provider"
)

type Command interface {
	chatCommand()
}

type SelectLane struct {
	Identity provider.LaneIdentity
	Owner    provider.AttachmentOwner
	CWD      string
	ModelID  string
	ModeID   string
	Creation provider.CreationCapabilities
}

func (SelectLane) chatCommand() {}

// BindEstablishedLane records an exact daemon-owned native thread as detached.
// It never creates, resumes, replays, or guesses provider state. Selecting the
// lane afterwards performs the ordinary exact-resume transition.
type BindEstablishedLane struct {
	Identity provider.LaneIdentity
	Thread   provider.ThreadRef
	Owner    provider.AttachmentOwner
	CWD      string
	ModelID  string
	ModeID   string
	Context  provider.ContextCapabilities
	Creation provider.CreationCapabilities
}

func (BindEstablishedLane) chatCommand() {}

type LaneOpened struct {
	// LaneID is the requested (possibly provisional) lane id used by the durable
	// create effect. Identity is the adapter-attested canonical identity. They
	// may differ only on the first successful create, before a native thread has
	// ever been established.
	LaneID               provider.LaneID
	Identity             provider.LaneIdentity
	Thread               provider.ThreadRef
	ConnectionGeneration uint64
	Context              provider.ContextCapabilities
	Delivery             provider.DeliveryCapabilities
	Attachment           *provider.LaneAttachmentSnapshot
	Reconciled           bool
}

func (LaneOpened) chatCommand() {}

// LaneProvisioned records a deferred provider's exact creation candidate
// without establishing it as ThreadRef. Only a matching input-consumption
// receipt may promote this candidate into the immutable provider lane.
type LaneProvisioned struct {
	LaneID                  provider.LaneID
	Identity                provider.LaneIdentity
	Candidate               provider.ThreadRef
	ConnectionGeneration    uint64
	Context                 provider.ContextCapabilities
	Delivery                provider.DeliveryCapabilities
	Creation                provider.CreationCapabilities
	Attachment              *provider.LaneAttachmentSnapshot
	Reconciled              bool
	PreviousCandidateAbsent bool
}

func (LaneProvisioned) chatCommand() {}

type LaneOpenFailed struct {
	LaneID    provider.LaneID
	Kind      provider.ErrorKind
	Ambiguous bool
}

func (LaneOpenFailed) chatCommand() {}

type LineageAdvanced struct {
	LaneID               provider.LaneID
	ConnectionGeneration uint64
	From                 provider.ThreadRef
	To                   provider.ThreadRef
}

func (LineageAdvanced) chatCommand() {}

type RetryLane struct {
	LaneID provider.LaneID
}

func (RetryLane) chatCommand() {}

type Submit struct {
	OperationID  provider.OperationID
	LaneID       provider.LaneID
	Text         string
	Attachments  []provider.Attachment
	ModelID      string
	ModeID       string
	Permission   string
	Presentation provider.TurnPresentation
}

func (Submit) chatCommand() {}

type Steer struct {
	OperationID  provider.OperationID
	Text         string
	Attachments  []provider.Attachment
	Presentation provider.TurnPresentation
}

func (Steer) chatCommand() {}

// PrepareQueuedTurn binds daemon-owned public row identities to an actor-owned
// FIFO operation immediately before provider dispatch. It never creates a new
// operation and is legal only while the start effect is still pending.
type PrepareQueuedTurn struct {
	OperationID  provider.OperationID
	ModelID      string
	ModeID       string
	Permission   string
	Presentation provider.TurnPresentation
}

func (PrepareQueuedTurn) chatCommand() {}

type SteerAdmitted struct {
	OperationID      provider.OperationID
	Accepted         bool
	Consumed         bool
	AwaitConsumption bool
	Interrupted      bool
}

func (SteerAdmitted) chatCommand() {}

type SteerFailed struct {
	OperationID provider.OperationID
	Kind        provider.ErrorKind
	Unsupported bool
	Ambiguous   bool
}

func (SteerFailed) chatCommand() {}

type CancelTurn struct{ OperationID provider.OperationID }

func (CancelTurn) chatCommand() {}

// RecordCancelReceipt commits a terminal chat.cancel result that did not have
// a provider turn to address. The command intentionally produces no provider
// effect; the actor operation reservation is the durable idempotency boundary.
type RecordCancelReceipt struct {
	OperationID provider.OperationID
	Cancelled   bool
	Reason      string
}

func (RecordCancelReceipt) chatCommand() {}

// RecordAgentWaitObservation reserves one exact agent wait/read request in
// the actor before the transient Manager is allowed to observe it. Only the
// request digest is durable; the executor's output remains ephemeral and is
// reconciled through the existing actor background snapshot ingress.
type RecordAgentWaitObservation struct {
	OperationID provider.OperationID
	TabID       string
	Digest      string
}

func (RecordAgentWaitObservation) chatCommand() {}

type CancelAcknowledged struct{ OperationID provider.OperationID }

func (CancelAcknowledged) chatCommand() {}

type CancelFailed struct {
	OperationID provider.OperationID
	Kind        provider.ErrorKind
}

func (CancelFailed) chatCommand() {}

type ResolvePermission struct {
	OperationID provider.OperationID
	RequestID   string
	OptionID    string
}

func (ResolvePermission) chatCommand() {}

type PermissionDecided struct {
	OperationID provider.OperationID
	RequestID   string
	OptionID    string
	Accepted    bool
	Ambiguous   bool
}

func (PermissionDecided) chatCommand() {}

type ContextImported struct {
	LaneID      provider.LaneID
	OperationID provider.OperationID
	From        uint64
	To          uint64
	Digest      string
	Found       bool
	Confirmed   bool
	Ambiguous   bool
	Reconciled  bool
}

func (ContextImported) chatCommand() {}

type TurnAdmitted struct {
	OperationID provider.OperationID
	Turn        provider.TurnRef
	Accepted    bool
	Ambiguous   bool
}

func (TurnAdmitted) chatCommand() {}

type TurnAdmissionFailed struct {
	OperationID provider.OperationID
	Kind        provider.ErrorKind
}

func (TurnAdmissionFailed) chatCommand() {}

type TurnReconciled struct {
	OperationID provider.OperationID
	Turn        provider.TurnRef
	Found       bool
	Consumed    bool
	Terminal    bool
	Status      string
}

func (TurnReconciled) chatCommand() {}

type InputConsumed struct {
	OperationID provider.OperationID
	Thread      *provider.ThreadRef
}

func (InputConsumed) chatCommand() {}

type TurnCompleted struct {
	OperationID provider.OperationID
	Assistant   string
}

func (TurnCompleted) chatCommand() {}

type TurnTerminated struct {
	OperationID      provider.OperationID
	Status           string
	StopReason       string
	Assistant        string
	ObservedAtUnixMS int64
	Terminal         *provider.TerminalEvent
}

func (TurnTerminated) chatCommand() {}

// ReconcileObligation applies current executor evidence to actor-owned request
// state. It is safe to repeat: evidence can only move a phantom working/parked
// record to stalled, never recreate or silently close it.
type ReconcileObligation struct {
	ObservedAt   string
	LiveEvidence bool
	HarnessQuiet bool
	DaemonBoot   bool
}

func (ReconcileObligation) chatCommand() {}

// ProviderEventReceived is the sole adapter-to-chat ingress. Connection
// generation and adapter sequence are checked before any payload can mutate
// ownership or visible history.
type ProviderEventReceived struct {
	ConnectionGeneration uint64
	Event                provider.Event
}

func (ProviderEventReceived) chatCommand() {}

type HostLost struct {
	LaneID               provider.LaneID
	ConnectionGeneration uint64
}

func (HostLost) chatCommand() {}

// DetachTarget is the exact disposable attachment that a close request is
// authorized to detach. It is also embedded in a workspace transaction so the
// reducer can validate the complete detach group before changing any actor
// state. The operation id is derived from this immutable target, because the
// frozen app-chat:close-session wire command has no operation-id field of its
// own.
type DetachTarget struct {
	OperationID          provider.OperationID
	LaneID               provider.LaneID
	Owner                provider.AttachmentOwner
	ConnectionID         string
	ConnectionGeneration uint64
}

func (DetachTarget) chatCommand() {}

// DetachLane is the compatibility name for the standalone actor command. A
// target is deliberately the same immutable typed value whether it is
// journaled by close-session or by ChangeWorkspace.
type DetachLane = DetachTarget

// DetachLaneFailed is the provider-side failure receipt for a durable detach
// effect. Ambiguous delivery never becomes an automatic retry: the exact
// attachment remains fenced by its generation and the operation is surfaced
// as uncertain in durable state.
type DetachLaneFailed struct {
	OperationID          provider.OperationID
	LaneID               provider.LaneID
	ConnectionID         string
	ConnectionGeneration uint64
	Kind                 provider.ErrorKind
	Ambiguous            bool
}

func (DetachLaneFailed) chatCommand() {}

// RecordExternalMutation is the provider-neutral actor ingress for a mutation
// whose executor lives outside the daemon process. Only the immutable target
// metadata and a safe request digest cross the actor boundary; raw request
// arguments remain in the short-lived executor call and are never persisted.
type RecordExternalMutation struct {
	OperationID provider.OperationID
	Kind        string
	Method      string
	TabID       string
	Digest      string
}

func (RecordExternalMutation) chatCommand() {}

// ExternalMutationReceipt is the only command that can advance an external
// mutation after its durable dispatch claim. A dispatched mutation that has no
// receipt is marked ambiguous and is never returned to Pending automatically.
type ExternalMutationReceipt struct {
	OperationID provider.OperationID
	Kind        string
	Method      string
	TabID       string
	Digest      string
	Failed      bool
	Ambiguous   bool
	ErrorKind   provider.ErrorKind
}

func (ExternalMutationReceipt) chatCommand() {}

type LaneProtocolFailed struct {
	LaneID               provider.LaneID
	ConnectionGeneration uint64
}

func (LaneProtocolFailed) chatCommand() {}

type Effect interface {
	chatEffect()
}

type CreateLaneEffect struct {
	Identity                    provider.LaneIdentity
	Owner                       provider.AttachmentOwner
	CWD                         string
	ModelID                     string
	ModeID                      string
	Generation                  uint64
	Reconcile                   bool
	CreateAfterCandidateAbsence bool
}

func (CreateLaneEffect) chatEffect() {}

type ResumeLaneEffect struct {
	Identity   provider.LaneIdentity
	Thread     provider.ThreadRef
	Owner      provider.AttachmentOwner
	CWD        string
	ModelID    string
	ModeID     string
	Generation uint64
}

func (ResumeLaneEffect) chatEffect() {}

type ImportContextEffect struct {
	LaneID      provider.LaneID
	OperationID provider.OperationID
	From        uint64
	To          uint64
	Batch       ContextBatch
	Reconcile   bool
}

func (ImportContextEffect) chatEffect() {}

type StartTurnEffect struct {
	LaneID provider.LaneID
	Input  QueueEntry
	Seed   ContextBatch
}

func (StartTurnEffect) chatEffect() {}

type ReconcileTurnEffect struct {
	LaneID      provider.LaneID
	OperationID provider.OperationID
	Turn        provider.TurnRef
}

func (ReconcileTurnEffect) chatEffect() {}

type SteerTurnEffect struct {
	LaneID      provider.LaneID
	OperationID provider.OperationID
	Turn        provider.TurnRef
	Text        string
	Attachments []provider.Attachment
}

func (SteerTurnEffect) chatEffect() {}

type CancelTurnEffect struct {
	LaneID      provider.LaneID
	OperationID provider.OperationID
	Turn        provider.TurnRef
}

func (CancelTurnEffect) chatEffect() {}

type ResolvePermissionEffect struct {
	LaneID      provider.LaneID
	OperationID provider.OperationID
	RequestID   string
	OptionID    string
}

func (ResolvePermissionEffect) chatEffect() {}

type DetachLaneEffect struct {
	OperationID          provider.OperationID
	LaneID               provider.LaneID
	Owner                provider.AttachmentOwner
	ConnectionID         string
	ConnectionGeneration uint64
}

func (DetachLaneEffect) chatEffect() {}

type ExternalMutationEffect struct {
	OperationID provider.OperationID
	ChatID      string
	Kind        string
	Method      string
	TabID       string
	Digest      string
}

func (ExternalMutationEffect) chatEffect() {}

// RestoreCheckpoint is a destructive chat-scoped action. Its stable operation
// id is committed before filesystem mutation, so a lost reply can return the
// original receipt and a crash after dispatch fails closed instead of running
// the restore a second time.
type RestoreCheckpoint struct {
	OperationID      provider.OperationID
	TurnSequence     int
	ObservedAtUnixMS int64
	Checkpoint       json.RawMessage
	CheckpointDigest string
}

func (RestoreCheckpoint) chatCommand() {}

type CheckpointRestored struct {
	OperationID  provider.OperationID
	TurnSequence int
	Result       json.RawMessage
}

func (CheckpointRestored) chatCommand() {}

type CheckpointRestoreFailed struct {
	OperationID provider.OperationID
	Kind        provider.ErrorKind
	Ambiguous   bool
}

func (CheckpointRestoreFailed) chatCommand() {}

type RestoreCheckpointEffect struct {
	OperationID      provider.OperationID
	TurnSequence     int
	ObservedAtUnixMS int64
	Checkpoint       json.RawMessage
	CheckpointDigest string
}

func (RestoreCheckpointEffect) chatEffect() {}

// DeleteChatEffect is the durable, idempotent cleanup of all disposable
// provider attachments and exact native bindings owned by one tombstoned chat.
// It deliberately has no LaneID: deletion covers every provider lane.
type DeleteChatEffect struct {
	OperationID provider.OperationID
	ChatID      string
	TabID       string
}

func (DeleteChatEffect) chatEffect() {}

type ChatDeletionCompleted struct {
	OperationID provider.OperationID
	ChatID      string
}

func (ChatDeletionCompleted) chatCommand() {}

type ClaimEffect struct {
	EffectID string
}

func (ClaimEffect) chatCommand() {}

type RecoverOutbox struct{}

func (RecoverOutbox) chatCommand() {}

func Reduce(current State, command Command) (State, []Effect, error) {
	if err := current.Validate(); err != nil {
		return current, nil, fmt.Errorf("invalid current chat state: %w", err)
	}
	if current.Deleted {
		switch command.(type) {
		case DeleteChat, RecoverOutbox, ClaimEffect, ChatDeletionCompleted:
		default:
			return current, nil, errors.New("chat was durably deleted")
		}
	}
	next := current.Clone()
	var (
		effects []Effect
		err     error
	)
	switch command := command.(type) {
	case InitializeChat:
		err = reduceInitializeChat(&next, command)
	case InitializeFork:
		err = reduceInitializeFork(&next, command)
	case UpdatePresentation:
		err = reduceUpdatePresentation(&next, command)
	case SavePresentation:
		err = reduceSavePresentation(&next, command)
	case ChangeWorkspace:
		effects, err = reduceChangeWorkspace(&next, command)
	case UpdateEnvironment:
		err = reduceUpdateEnvironment(&next, command)
	case ReplaceStagedQueue:
		err = reduceReplaceStagedQueue(&next, command)
	case UpdateRuntimeControls:
		err = reduceUpdateRuntimeControls(&next, command)
	case SaveRuntimeControls:
		err = reduceSaveRuntimeControls(&next, command)
	case CommitLaneSelection:
		effects, err = reduceCommitLaneSelection(&next, command)
	case PromoteStagedQueue:
		effects, err = reducePromoteStagedQueue(&next, command)
	case ResumeQueue:
		effects, err = reduceResumeQueue(&next, command)
	case DeleteChat:
		effects, err = reduceDeleteChat(&next, command)
	case ChatDeletionCompleted:
		err = reduceChatDeletionCompleted(&next, command)
	case AttachTab:
		err = reduceAttachTab(&next, command)
	case SelectLane:
		effects, err = reduceSelectLane(&next, command)
	case BindEstablishedLane:
		err = reduceBindEstablishedLane(&next, command)
	case LaneOpened:
		effects, err = reduceLaneOpened(&next, command)
	case LaneProvisioned:
		effects, err = reduceLaneProvisioned(&next, command)
	case LaneOpenFailed:
		err = reduceLaneOpenFailed(&next, command)
	case LineageAdvanced:
		err = reduceLineageAdvanced(&next, command)
	case RetryLane:
		effects, err = reduceRetryLane(&next, command)
	case Submit:
		effects, err = reduceSubmit(&next, command)
	case Steer:
		effects, err = reduceSteer(&next, command)
	case PrepareQueuedTurn:
		err = reducePrepareQueuedTurn(&next, command)
	case SteerAdmitted:
		err = reduceSteerAdmitted(&next, command)
	case SteerFailed:
		effects, err = reduceSteerFailed(&next, command)
	case CancelTurn:
		effects, err = reduceCancelTurn(&next, command)
	case RecordCancelReceipt:
		err = reduceRecordCancelReceipt(&next, command)
	case RecordAgentWaitObservation:
		err = reduceRecordAgentWaitObservation(&next, command)
	case CancelAcknowledged:
		err = reduceCancelAcknowledged(&next, command)
	case CancelFailed:
		err = reduceCancelFailed(&next, command)
	case ResolvePermission:
		effects, err = reduceResolvePermission(&next, command)
	case PermissionDecided:
		err = reducePermissionDecided(&next, command)
	case RestoreCheckpoint:
		effects, err = reduceRestoreCheckpoint(&next, command)
	case CheckpointRestored:
		err = reduceCheckpointRestored(&next, command)
	case CheckpointRestoreFailed:
		err = reduceCheckpointRestoreFailed(&next, command)
	case RequestBackgroundAction:
		effects, err = reduceRequestBackgroundAction(&next, command)
	case BackgroundActionCompleted:
		err = reduceBackgroundActionCompleted(&next, command)
	case BackgroundActionFailed:
		err = reduceBackgroundActionFailed(&next, command)
	case ReconcileBackgroundSnapshot:
		err = reduceReconcileBackgroundSnapshot(&next, command)
	case ContextImported:
		effects, err = reduceContextImported(&next, command)
	case TurnAdmitted:
		err = reduceTurnAdmitted(&next, command)
	case TurnAdmissionFailed:
		effects, err = reduceTurnAdmissionFailed(&next, command)
	case TurnReconciled:
		effects, err = reduceTurnReconciled(&next, command)
	case InputConsumed:
		err = reduceInputConsumed(&next, command)
	case TurnCompleted:
		effects, err = reduceTurnCompleted(&next, command)
	case TurnTerminated:
		effects, err = reduceTurnTerminated(&next, command)
	case ReconcileObligation:
		err = reduceReconcileObligation(&next, command)
	case ProviderEventReceived:
		effects, err = reduceProviderEvent(&next, command)
	case HostLost:
		effects, err = reduceHostLost(&next, command)
	case DetachLane:
		effects, err = reduceDetachLane(&next, command)
	case DetachLaneFailed:
		err = reduceDetachLaneFailed(&next, command)
	case RecordExternalMutation:
		effects, err = reduceRecordExternalMutation(&next, command)
	case ExternalMutationReceipt:
		err = reduceExternalMutationReceipt(&next, command)
	case LaneProtocolFailed:
		err = reduceLaneProtocolFailed(&next, command)
	case ClaimEffect:
		effects, err = reduceClaimEffect(&next, command)
	case RecoverOutbox:
		effects, err = reduceRecoverOutbox(&next)
	default:
		err = fmt.Errorf("unsupported chat command %T", command)
	}
	if err != nil {
		return current, nil, err
	}
	if err := recordEffects(&next, effects); err != nil {
		return current, nil, err
	}
	next.Revision++
	if err := next.Validate(); err != nil {
		return current, nil, fmt.Errorf("chat transition produced invalid state: %w", err)
	}
	return next, effects, nil
}

func reduceInitializeChat(state *State, command InitializeChat) error {
	if state.Initialized {
		return errors.New("chat actor is already initialized")
	}
	if len(state.Ledger) != 0 || len(state.Lanes) != 0 ||
		len(state.Queue) != 0 || len(state.StagedQueue) != 0 || len(state.Outbox) != 0 ||
		len(state.Operations) != 0 || state.Foreground != nil || state.PendingSteer != nil || state.PendingCancel != nil {
		return errors.New("new chat initialization conflicts with existing actor state")
	}
	presentation := command.Presentation.Clone()
	if strings.TrimSpace(presentation.TabID) == "" {
		return errors.New("new chat initialization requires tab identity")
	}
	if err := presentation.Validate(); err != nil {
		return err
	}
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	digest := strings.TrimSpace(command.Digest)
	if operationID == "" || digest == "" {
		return errors.New("new chat initialization requires immutable operation identity and digest")
	}
	state.Presentation = presentation
	state.CreationOperationID = operationID
	state.CreationDigest = digest
	state.Initialized = true
	return nil
}

func reduceInitializeFork(state *State, command InitializeFork) error {
	if state.Initialized || len(state.Ledger) != 0 || len(state.Lanes) != 0 ||
		len(state.Queue) != 0 || len(state.StagedQueue) != 0 || len(state.Outbox) != 0 || len(state.Operations) != 0 {
		return errors.New("fork initialization conflicts with existing actor state")
	}
	sourceChatID := strings.TrimSpace(command.SourceChatID)
	if sourceChatID == "" || sourceChatID == state.ChatID {
		return errors.New("fork initialization requires a distinct source chat")
	}
	presentation := command.Presentation.Clone()
	if strings.TrimSpace(presentation.TabID) == "" {
		return errors.New("fork initialization requires tab identity")
	}
	if err := presentation.Validate(); err != nil {
		return err
	}
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	digest := strings.TrimSpace(command.Digest)
	if operationID == "" || digest == "" {
		return errors.New("fork initialization requires immutable operation identity and digest")
	}
	operationIDs := make(map[provider.OperationID]provider.OperationID)
	ledger := make([]LedgerEvent, 0, len(command.Messages))
	seenMessages := make(map[string]struct{}, len(command.Messages))
	for index, source := range command.Messages {
		if strings.TrimSpace(source.MessageID) == "" || source.OperationID == "" || (source.Role != "user" && source.Role != "assistant") {
			return fmt.Errorf("fork source message %d is incomplete", index)
		}
		if _, duplicate := seenMessages[source.MessageID]; duplicate {
			return errors.New("fork source contains duplicate message identity")
		}
		seenMessages[source.MessageID] = struct{}{}
		operationID := operationIDs[source.OperationID]
		if operationID == "" {
			operationID = provider.OperationID(fmt.Sprintf("fork-visible:%s:%s", state.ChatID, source.OperationID))
			operationIDs[source.OperationID] = operationID
		}
		event := source
		event.EventID = fmt.Sprintf("fork:%s:%d:%s", state.ChatID, index+1, source.EventID)
		event.Sequence = uint64(index + 1)
		event.OperationID = operationID
		event.LaneID = ""
		event.NativeTurnID = ""
		event.ContextExcluded = true
		event.Attachments = append([]provider.Attachment(nil), source.Attachments...)
		event.Timeline = cloneTimeline(source.Timeline)
		event.Permission = clonePermission(source.Permission)
		ledger = append(ledger, event)
	}
	state.Presentation = presentation
	state.Ledger = ledger
	state.ContextFloor = uint64(len(ledger))
	state.CreationOperationID = operationID
	state.CreationDigest = digest
	for _, operationID := range operationIDs {
		state.Operations[operationID] = struct{}{}
	}
	state.Initialized = true
	return nil
}

func reduceUpdatePresentation(state *State, command UpdatePresentation) error {
	return applyPresentation(state, command.Presentation)
}

func reduceSavePresentation(state *State, command SavePresentation) error {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	digest := strings.TrimSpace(command.Digest)
	if operationID == "" || digest == "" {
		return errors.New("presentation save requires immutable operation identity and digest")
	}
	if receipt, exists := state.PresentationMutationReceipts[operationID]; exists {
		if receipt.Digest != digest {
			return errors.New("presentation operation id was reused for different content")
		}
		return nil
	}
	if err := applyPresentation(state, command.Presentation); err != nil {
		return err
	}
	state.PresentationMutationReceipts[operationID] = PresentationMutationReceipt{Digest: digest, Revision: state.Presentation.PresentationRevision}
	return nil
}

func applyPresentation(state *State, source PresentationState) error {
	incoming := source.Clone()
	if err := incoming.Validate(); err != nil {
		return err
	}
	if incoming.PresentationRevision != state.Presentation.PresentationRevision {
		return errors.New("renderer chat presentation changed in another controller; reload before saving")
	}
	// A frozen renderer snapshot is a presentation command, never a replacement
	// ChatState. Copy only fields the renderer is allowed to author. Provider,
	// workspace, usage, plan, and runtime-control fields retain actor ownership
	// even when a stale or hostile client includes different values.
	presentation := state.Presentation.Clone()
	if strings.TrimSpace(incoming.TabID) == "" {
		return errors.New("renderer presentation is missing tab identity")
	}
	if strings.TrimSpace(incoming.TabID) != strings.TrimSpace(presentation.TabID) {
		return errors.New("renderer presentation cannot change tab attachment")
	}
	presentation.Title = incoming.Title
	presentation.TitleLocked = incoming.TitleLocked
	presentation.Group = incoming.Group
	presentation.Draft = incoming.Draft
	presentation.Unread = incoming.Unread
	presentation.Settled = incoming.Settled
	presentation.SettledAt = incoming.SettledAt
	presentation.Pane = incoming.Pane
	if !samePresentationFields(presentation, state.Presentation) {
		presentation.PresentationRevision++
	}
	state.Presentation = presentation
	return nil
}

func samePresentationFields(left, right PresentationState) bool {
	return left.Title == right.Title && left.TitleLocked == right.TitleLocked &&
		sameOptionalString(left.Group, right.Group) && left.Draft == right.Draft &&
		left.Unread == right.Unread && left.Settled == right.Settled && left.SettledAt == right.SettledAt &&
		sameOptionalString(left.Pane, right.Pane)
}

func sameOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func reduceChangeWorkspace(state *State, command ChangeWorkspace) ([]Effect, error) {
	if !state.Initialized {
		return nil, errors.New("workspace change requires an initialized chat actor")
	}
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	digest := strings.TrimSpace(command.Digest)
	if operationID == "" || digest == "" {
		return nil, errors.New("workspace change requires immutable operation identity and digest")
	}
	targetOperationIDs, err := workspaceDetachOperationIDs(command.DetachTargets)
	if err != nil {
		return nil, err
	}
	if receipt, exists := state.WorkspaceMutationReceipts[operationID]; exists {
		if receipt.Digest != digest || !sameOperationIDSequence(receipt.DetachOperationIDs, targetOperationIDs) {
			return nil, errors.New("workspace operation id was reused for different content")
		}
		if err := validateWorkspaceDetachReceiptTargets(*state, command.DetachTargets); err != nil {
			return nil, err
		}
		return nil, nil
	}
	_, targetOperationIDs, effects, err := validateWorkspaceDetachTargets(*state, command.DetachTargets)
	if err != nil {
		return nil, err
	}
	if command.ExpectedRevision != state.Presentation.WorkspaceRevision {
		return nil, errors.New("workspace changed in another controller; reload before moving")
	}
	cwd := strings.TrimSpace(command.CWD)
	if cwd == "" {
		return nil, errors.New("workspace change requires a concrete cwd")
	}
	if state.Foreground != nil || state.PendingSteer != nil || state.PendingCancel != nil {
		return nil, errors.New("workspace cannot change while a foreground operation is active")
	}
	// All target validation, including operation-id, lane, owner, connection,
	// and generation fencing, completed above before any workspace field is
	// touched. The effects are returned to Reduce so recordEffects persists the
	// entire detach group in this same actor/store transition.
	for _, detachOperationID := range targetOperationIDs {
		state.Operations[detachOperationID] = struct{}{}
	}
	if state.Presentation.CWD != nil && strings.TrimSpace(*state.Presentation.CWD) == cwd {
		state.WorkspaceMutationReceipts[operationID] = WorkspaceMutationReceipt{
			Digest: digest, Revision: state.Presentation.WorkspaceRevision, CWD: cwd,
			DetachOperationIDs: append([]provider.OperationID(nil), targetOperationIDs...),
		}
		return effects, nil
	}
	state.Presentation.CWD = &cwd
	state.Presentation.WorkspaceRevision++
	// The historical active lane belongs to the previous workspace epoch. Keep
	// its immutable record, but no command may target it implicitly after the
	// workspace commit.
	state.ActiveLaneID = ""
	state.DesiredLaneID = ""
	state.WorkspaceMutationReceipts[operationID] = WorkspaceMutationReceipt{
		Digest: digest, Revision: state.Presentation.WorkspaceRevision, CWD: cwd,
		DetachOperationIDs: append([]provider.OperationID(nil), targetOperationIDs...),
	}
	return effects, nil
}

func workspaceDetachOperationIDs(targets []DetachTarget) ([]provider.OperationID, error) {
	operationIDs := make([]provider.OperationID, 0, len(targets))
	seen := make(map[provider.OperationID]struct{}, len(targets))
	for _, target := range targets {
		operationID := provider.NormalizeOperationID(string(target.OperationID))
		if operationID == "" || operationID != target.OperationID {
			return nil, errors.New("workspace detach group contains an invalid operation identity")
		}
		if _, duplicate := seen[operationID]; duplicate {
			return nil, errors.New("workspace detach group contains duplicate operation identities")
		}
		seen[operationID] = struct{}{}
		operationIDs = append(operationIDs, operationID)
	}
	return operationIDs, nil
}

func validateWorkspaceDetachTargets(state State, rawTargets []DetachTarget) ([]DetachTarget, []provider.OperationID, []Effect, error) {
	targets := make([]DetachTarget, 0, len(rawTargets))
	operationIDs := make([]provider.OperationID, 0, len(rawTargets))
	effects := make([]Effect, 0, len(rawTargets))
	seen := make(map[provider.OperationID]struct{}, len(rawTargets))
	for _, rawTarget := range rawTargets {
		target := rawTarget
		target.OperationID = provider.NormalizeOperationID(string(target.OperationID))
		target.LaneID = provider.LaneID(strings.TrimSpace(string(target.LaneID)))
		target.Owner.TabID = strings.TrimSpace(target.Owner.TabID)
		target.Owner.AgentOwnerKey = strings.TrimSpace(target.Owner.AgentOwnerKey)
		target.ConnectionID = strings.TrimSpace(target.ConnectionID)
		if target.OperationID == "" || target.LaneID == "" || target.ConnectionID == "" || target.ConnectionGeneration == 0 {
			return nil, nil, nil, errors.New("workspace detach target requires immutable operation, lane, connection, and generation")
		}
		expectedOperationID := DetachOperationID(state.ChatID, target.LaneID, target.ConnectionID, target.ConnectionGeneration)
		if target.OperationID != expectedOperationID {
			return nil, nil, nil, errors.New("workspace detach operation identity does not match its exact target")
		}
		if _, duplicate := seen[target.OperationID]; duplicate {
			return nil, nil, nil, errors.New("workspace detach group contains duplicate operation identities")
		}
		seen[target.OperationID] = struct{}{}
		if _, reserved := state.Operations[target.OperationID]; reserved {
			return nil, nil, nil, errors.New("workspace detach operation id conflicts with another actor operation")
		}
		for _, entry := range state.Outbox {
			if entry.OperationID == target.OperationID {
				return nil, nil, nil, errors.New("workspace detach operation id conflicts with an existing durable effect")
			}
		}
		lane, ok := state.Lanes[target.LaneID]
		if !ok || lane.Identity.ID != target.LaneID || lane.Identity.ChatID != state.ChatID {
			return nil, nil, nil, errors.New("workspace detach target lane is unknown")
		}
		if lane.Attachment == nil || lane.ConnectionGeneration != target.ConnectionGeneration ||
			strings.TrimSpace(lane.Attachment.ConnectionID) != target.ConnectionID {
			return nil, nil, nil, errors.New("workspace detach target attachment changed")
		}
		if target.Owner != lane.Owner || target.Owner.TabID == "" {
			return nil, nil, nil, errors.New("workspace detach target owner changed")
		}
		targets = append(targets, target)
		operationIDs = append(operationIDs, target.OperationID)
		effects = append(effects, DetachLaneEffect{
			OperationID: target.OperationID, LaneID: target.LaneID, Owner: target.Owner,
			ConnectionID: target.ConnectionID, ConnectionGeneration: target.ConnectionGeneration,
		})
	}
	return targets, operationIDs, effects, nil
}

func validateWorkspaceDetachReceiptTargets(state State, rawTargets []DetachTarget) error {
	for _, rawTarget := range rawTargets {
		target := rawTarget
		target.OperationID = provider.NormalizeOperationID(string(target.OperationID))
		target.LaneID = provider.LaneID(strings.TrimSpace(string(target.LaneID)))
		target.Owner.TabID = strings.TrimSpace(target.Owner.TabID)
		target.Owner.AgentOwnerKey = strings.TrimSpace(target.Owner.AgentOwnerKey)
		target.ConnectionID = strings.TrimSpace(target.ConnectionID)
		if target.OperationID == "" || target.LaneID == "" || target.ConnectionID == "" || target.ConnectionGeneration == 0 {
			return errors.New("workspace detach retry contains an invalid immutable target")
		}
		if target.OperationID != DetachOperationID(state.ChatID, target.LaneID, target.ConnectionID, target.ConnectionGeneration) {
			return errors.New("workspace detach retry operation identity does not match its exact target")
		}
		entry, ok := detachEntryForOperation(state, target.OperationID)
		if !ok || entry.LaneID != target.LaneID || entry.Owner != target.Owner ||
			entry.ConnectionID != target.ConnectionID || entry.Generation != target.ConnectionGeneration {
			return errors.New("workspace detach retry changed its durable target")
		}
	}
	return nil
}

func sameOperationIDSequence(left, right []provider.OperationID) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func reduceUpdateEnvironment(state *State, command UpdateEnvironment) error {
	if !state.Initialized || state.Deleted {
		return errors.New("environment update requires an active chat actor")
	}
	tabID := strings.TrimSpace(command.ExpectedTabID)
	if tabID == "" || tabID != strings.TrimSpace(state.Presentation.TabID) {
		return errors.New("environment update tab attachment is stale")
	}
	if len(command.Payload) == 0 || !json.Valid(command.Payload) {
		return errors.New("environment update requires a valid payload")
	}
	if len(command.Checkpoints) == 0 || !json.Valid(command.Checkpoints) {
		return errors.New("environment update requires a valid checkpoint list")
	}
	if len(command.Reference) > 0 && !json.Valid(command.Reference) {
		return errors.New("environment update requires a valid manager reference")
	}
	cwd := strings.TrimSpace(command.CWD)
	state.Environment.Revision++
	state.Environment.TabID = tabID
	state.Environment.CWD = cwd
	state.Environment.Payload = append(json.RawMessage(nil), command.Payload...)
	state.Environment.Checkpoints = append(json.RawMessage(nil), command.Checkpoints...)
	state.Environment.Reference = append(json.RawMessage(nil), command.Reference...)
	return nil
}

func reduceReplaceStagedQueue(state *State, command ReplaceStagedQueue) error {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	digest := strings.TrimSpace(command.Digest)
	if operationID == "" || digest == "" {
		return errors.New("staged queue replacement requires immutable operation identity and digest")
	}
	if receipt, exists := state.QueueMutationReceipts[operationID]; exists {
		if receipt.Digest != digest {
			return errors.New("staged queue operation id was reused for different content")
		}
		return nil
	}
	if command.ExpectedRevision != state.Presentation.AgentQueueRevision {
		return errors.New("staged queue revision is stale")
	}
	entries := cloneStagedQueue(command.Entries)
	if err := normalizeStagedQueue(entries); err != nil {
		return err
	}
	// Provider/control targeting is captured when the row first enters the
	// actor. Later renderer edits may change text or attachment readiness, but
	// cannot silently retarget an already queued message to another lane.
	existing := make(map[string]StagedQueueEntry, len(state.StagedQueue))
	for _, entry := range state.StagedQueue {
		existing[entry.ID] = entry
	}
	for index := range entries {
		if previous, ok := existing[entries[index].ID]; ok {
			entries[index].TargetProviderID = previous.TargetProviderID
			entries[index].ModelID = previous.ModelID
			entries[index].ModeID = previous.ModeID
			entries[index].Permission = previous.Permission
		}
	}
	state.StagedQueue = entries
	state.Presentation.AgentQueueRevision++
	state.QueueMutationReceipts[operationID] = QueueMutationReceipt{Digest: digest, Revision: state.Presentation.AgentQueueRevision}
	return nil
}

func reduceUpdateRuntimeControls(state *State, command UpdateRuntimeControls) error {
	if !state.Initialized {
		return errors.New("runtime controls require an initialized chat actor")
	}
	if command.RequireRevision && command.ExpectedRevision != state.Presentation.RuntimeControlRevision {
		return errors.New("runtime controls changed in another controller; reload before updating")
	}
	presentation := state.Presentation.Clone()
	changed := false
	if providerID := provider.NormalizeID(string(command.ProviderID)); providerID != "" && providerID != presentation.ProviderID {
		presentation.ProviderID = providerID
		changed = true
	}
	modelID := strings.TrimSpace(command.ModelID)
	if command.ReplaceModelID && modelID != presentation.CurrentModelID {
		presentation.CurrentModelID = modelID
		changed = true
	} else if !command.ReplaceModelID && modelID != "" && modelID != presentation.CurrentModelID {
		presentation.CurrentModelID = modelID
		changed = true
	}
	modeID := strings.TrimSpace(command.ModeID)
	if command.ReplaceModeID && modeID != presentation.CurrentModeID {
		presentation.CurrentModeID = modeID
		changed = true
	} else if !command.ReplaceModeID && modeID != "" && modeID != presentation.CurrentModeID {
		presentation.CurrentModeID = modeID
		changed = true
	}
	if command.ReplaceModelControls && !bytes.Equal(command.ModelControls, presentation.ModelControls) {
		if len(command.ModelControls) != 0 && !json.Valid(command.ModelControls) {
			return errors.New("runtime model controls are not valid JSON")
		}
		presentation.ModelControls = append(json.RawMessage(nil), command.ModelControls...)
		changed = true
	}
	if changed {
		presentation.RuntimeControlRevision++
		state.Presentation = presentation
	}
	selectedLaneID := state.DesiredLaneID
	if selectedLaneID == "" {
		selectedLaneID = state.ActiveLaneID
	}
	if lane, ok := state.Lanes[selectedLaneID]; ok &&
		(command.ProviderID == "" || lane.Identity.Realm.ProviderID == provider.NormalizeID(string(command.ProviderID))) {
		if command.ReplaceModelID || modelID != "" {
			lane.ModelID = modelID
		}
		if command.ReplaceModeID || modeID != "" {
			lane.ModeID = modeID
		}
		state.Lanes[selectedLaneID] = lane
	}
	return nil
}

func reduceSaveRuntimeControls(state *State, command SaveRuntimeControls) error {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	digest := strings.TrimSpace(command.Digest)
	if operationID == "" || digest == "" {
		return errors.New("runtime-control save requires immutable operation identity and digest")
	}
	if receipt, exists := state.RuntimeControlMutationReceipts[operationID]; exists {
		if receipt.Digest != digest {
			return errors.New("runtime-control operation id was reused for different content")
		}
		return nil
	}
	update := command.Update
	update.RequireRevision = true
	if err := reduceUpdateRuntimeControls(state, update); err != nil {
		return err
	}
	state.RuntimeControlMutationReceipts[operationID] = RuntimeControlMutationReceipt{
		Digest: digest, Revision: state.Presentation.RuntimeControlRevision,
	}
	return nil
}

func reduceCommitLaneSelection(state *State, command CommitLaneSelection) ([]Effect, error) {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	digest := strings.TrimSpace(command.Digest)
	if operationID == "" || digest == "" {
		return nil, errors.New("lane selection requires immutable operation identity and digest")
	}
	if receipt, exists := state.LaneSelectionMutationReceipts[operationID]; exists {
		if receipt.Digest != digest {
			return nil, errors.New("lane-selection operation id was reused for different content")
		}
		return nil, nil
	}
	identity := command.Identity.Normalize()
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if identity.ChatID != state.ChatID {
		return nil, errors.New("selected lane belongs to another chat")
	}
	if command.Established {
		thread := command.Thread.Normalize()
		if err := thread.Validate(identity.Realm.ProviderID); err != nil {
			return nil, err
		}
		if err := reduceBindEstablishedLane(state, BindEstablishedLane{
			Identity: identity, Thread: thread, Owner: command.Owner, CWD: command.CWD,
			ModelID: command.ModelID, ModeID: command.ModeID, Context: command.Context, Creation: command.Creation,
		}); err != nil {
			return nil, err
		}
	}
	effects, err := reduceSelectLane(state, SelectLane{
		Identity: identity, Owner: command.Owner, CWD: command.CWD,
		ModelID: command.ModelID, ModeID: command.ModeID, Creation: command.Creation,
	})
	if err != nil {
		return nil, err
	}
	update := command.Update
	update.ProviderID = identity.Realm.ProviderID
	update.ModelID = command.ModelID
	update.ModeID = command.ModeID
	update.ReplaceModelID = true
	update.ReplaceModeID = true
	update.RequireRevision = true
	if err := reduceUpdateRuntimeControls(state, update); err != nil {
		return nil, err
	}
	state.LaneSelectionMutationReceipts[operationID] = LaneSelectionMutationReceipt{
		Digest: digest, Revision: state.Revision + 1, LaneID: identity.ID,
	}
	return effects, nil
}

func reducePromoteStagedQueue(state *State, command PromoteStagedQueue) ([]Effect, error) {
	queueID := strings.TrimSpace(command.QueueID)
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	if queueID == "" || operationID == "" {
		return nil, errors.New("staged queue promotion requires queue and operation identities")
	}
	if _, exists := state.Operations[operationID]; exists {
		return nil, errors.New("staged queue operation id already belongs to this chat")
	}
	index := -1
	for candidate := range state.StagedQueue {
		if state.StagedQueue[candidate].ID == queueID {
			index = candidate
			break
		}
	}
	if index < 0 {
		return nil, errors.New("staged queue row is no longer owned by this chat")
	}
	staged := state.StagedQueue[index]
	target := command.LaneID
	if target == "" {
		target = state.DesiredLaneID
	}
	lane, ok := state.Lanes[target]
	if !ok || target == "" {
		return nil, errors.New("staged queue promotion requires a selected target lane")
	}
	if staged.TargetProviderID != "" && lane.Identity.Realm.ProviderID != staged.TargetProviderID {
		return nil, errors.New("staged queue target provider changed after admission")
	}
	presentation, err := normalizeTurnPresentation(command.Presentation)
	if err != nil {
		return nil, err
	}
	presentation.QueueID = queueID
	state.StagedQueue = append(state.StagedQueue[:index:index], state.StagedQueue[index+1:]...)
	state.Operations[operationID] = struct{}{}
	state.Queue = append(state.Queue, QueueEntry{
		OperationID: operationID, LaneID: target, Text: staged.Text,
		Attachments:  append([]provider.Attachment(nil), staged.Attachments...),
		ModelID:      firstNonEmptyString(strings.TrimSpace(command.ModelID), staged.ModelID),
		ModeID:       firstNonEmptyString(strings.TrimSpace(command.ModeID), staged.ModeID),
		Permission:   firstNonEmptyString(strings.TrimSpace(command.Permission), staged.Permission),
		Presentation: presentation, Revision: state.Revision + 1,
	})
	state.Presentation.AgentQueueRevision++
	return drive(state)
}

func reduceResumeQueue(state *State, command ResumeQueue) ([]Effect, error) {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	if operationID == "" || command.ExpectedRevision == 0 {
		return nil, errors.New("queue resume requires operation identity and pause revision")
	}
	if receipt, exists := state.QueueControl.ResumeReceipts[operationID]; exists {
		if receipt.PauseRevision != command.ExpectedRevision {
			return nil, errors.New("queue resume operation id was reused for another pause boundary")
		}
		return nil, nil
	}
	if _, exists := state.Operations[operationID]; exists {
		return nil, errors.New("queue resume operation id already belongs to another chat action")
	}
	if command.ExpectedRevision != state.QueueControl.Revision {
		return nil, errors.New("queue pause revision changed before resume")
	}
	state.Operations[operationID] = struct{}{}
	state.QueueControl.ResumeReceipts[operationID] = QueueResumeReceipt{PauseRevision: command.ExpectedRevision}
	state.QueueControl.Paused = false
	return drive(state)
}

func reduceDeleteChat(state *State, command DeleteChat) ([]Effect, error) {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	if operationID == "" {
		return nil, errors.New("chat deletion requires a stable operation id")
	}
	if state.Deleted {
		if state.DeletionOperationID != operationID {
			return nil, errors.New("chat was already deleted by another operation")
		}
		return nil, nil
	}
	if !state.Initialized {
		return nil, errors.New("chat deletion requires a completed initialization or migration")
	}
	if !command.Force && (state.Foreground != nil || state.PendingSteer != nil || state.PendingCancel != nil) {
		return nil, errors.New("chat has active provider work")
	}
	state.Deleted = true
	state.DeletionOperationID = operationID
	state.StagedQueue = nil
	state.Queue = nil
	state.Foreground = nil
	state.PendingSteer = nil
	state.PendingCancel = nil
	state.Outbox = nil
	state.Tools = make(map[string]ToolState)
	state.Plans = make(map[provider.OperationID]PlanState)
	state.Permissions = make(map[string]PermissionState)
	state.Background = make(map[string]BackgroundState)
	for id, lane := range state.Lanes {
		lane.Phase = LaneDetached
		lane.PendingImport = nil
		state.Lanes[id] = lane
	}
	state.ActiveLaneID = ""
	state.DesiredLaneID = ""
	return []Effect{DeleteChatEffect{
		OperationID: operationID, ChatID: state.ChatID, TabID: state.Presentation.TabID,
	}}, nil
}

func reduceChatDeletionCompleted(state *State, command ChatDeletionCompleted) error {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	if !state.Deleted || strings.TrimSpace(command.ChatID) != state.ChatID || operationID != state.DeletionOperationID {
		return errors.New("chat cleanup receipt does not match the durable tombstone")
	}
	if !updateOutbox(state, deleteChatEffectID(state.ChatID), OutboxCompleted, "") {
		return errors.New("chat cleanup receipt has no durable delete effect")
	}
	return nil
}

func reduceAttachTab(state *State, command AttachTab) error {
	if !state.Initialized || state.Deleted {
		return errors.New("tab attachment requires an active initialized chat")
	}
	tabID := strings.TrimSpace(command.TabID)
	if tabID == "" {
		return errors.New("tab attachment requires tab id")
	}
	if state.Foreground != nil || state.PendingSteer != nil || state.PendingCancel != nil {
		return errors.New("tab attachment cannot change during foreground work")
	}
	state.Presentation.TabID = tabID
	for id, lane := range state.Lanes {
		lane.Owner.TabID = tabID
		state.Lanes[id] = lane
	}
	return nil
}

func normalizeStagedQueue(entries []StagedQueueEntry) error {
	seen := make(map[string]struct{}, len(entries))
	for index := range entries {
		entry := &entries[index]
		entry.ID = strings.TrimSpace(entry.ID)
		entry.Text = strings.TrimSpace(entry.Text)
		entry.Source = strings.ToLower(strings.TrimSpace(entry.Source))
		entry.Delivery = strings.ToLower(strings.TrimSpace(entry.Delivery))
		entry.TargetProviderID = provider.NormalizeID(string(entry.TargetProviderID))
		if entry.ID == "" || entry.Text == "" && len(entry.Attachments) == 0 {
			return errors.New("staged queue entry is missing identity or content")
		}
		if _, duplicate := seen[entry.ID]; duplicate {
			return errors.New("staged queue contains duplicate identity")
		}
		seen[entry.ID] = struct{}{}
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
	return nil
}

func reduceBindEstablishedLane(state *State, command BindEstablishedLane) error {
	identity := command.Identity.Normalize()
	if err := identity.Validate(); err != nil {
		return err
	}
	if identity.ChatID != state.ChatID {
		return errors.New("established provider lane belongs to another chat")
	}
	thread := command.Thread.Normalize()
	if err := thread.Validate(identity.Realm.ProviderID); err != nil {
		return err
	}
	if !command.Context.ExactResume {
		return errors.New("established provider lane does not support exact native-thread resume")
	}
	lane, exists := state.Lanes[identity.ID]
	if exists {
		existing := lane
		if existing.Identity != identity || !existing.Thread.Equal(thread) {
			return errors.New("established provider lane conflicts with durable chat ownership")
		}
		existing.Owner = command.Owner
		existing.CWD = firstNonEmptyString(existing.CWD, command.CWD)
		existing.ModelID = firstNonEmptyString(command.ModelID, existing.ModelID)
		existing.ModeID = firstNonEmptyString(command.ModeID, existing.ModeID)
		// An established actor lane may already contain capabilities negotiated
		// with the exact live native thread. Read-only binding resolution carries
		// only the provider's conservative static baseline, so re-adoption must
		// never downgrade negotiated import/readback support.
		if existing.Context == (provider.ContextCapabilities{}) {
			existing.Context = command.Context
		}
		existing.Creation = command.Creation
		lane = existing
	} else {
		lane = LaneState{
			Identity: identity, Owner: command.Owner, CWD: strings.TrimSpace(command.CWD),
			ModelID: strings.TrimSpace(command.ModelID), ModeID: strings.TrimSpace(command.ModeID),
			Thread: thread, Phase: LaneDetached, Coverage: make(map[uint64]CoverageRecord),
			Context: command.Context, Creation: command.Creation,
		}
	}
	state.Lanes[identity.ID] = lane
	return nil
}

func reduceLineageAdvanced(state *State, command LineageAdvanced) error {
	lane, ok := state.Lanes[command.LaneID]
	if !ok {
		return errors.New("lineage event belongs to an unknown lane")
	}
	if command.ConnectionGeneration != lane.ConnectionGeneration {
		return errors.New("lineage event belongs to a stale host connection")
	}
	if !lane.Context.VerifiedLineage {
		return errors.New("provider did not negotiate verified lineage events")
	}
	if !lane.Thread.Equal(command.From) {
		return errors.New("lineage event does not start at the lane's current native head")
	}
	if !command.From.CanAdvanceTo(command.To) {
		return errors.New("provider lineage event does not prove a monotonic same-lineage head advance")
	}
	lane.Thread = command.To.Normalize()
	state.Lanes[command.LaneID] = lane
	return nil
}

func reduceSelectLane(state *State, command SelectLane) ([]Effect, error) {
	identity := command.Identity.Normalize()
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if identity.ChatID != state.ChatID {
		return nil, errors.New("selected lane belongs to another chat")
	}
	if pendingDetachForLane(*state, identity.ID) {
		return nil, errors.New("provider lane has a detach operation in flight")
	}
	if existing, ok := state.Lanes[identity.ID]; ok {
		if existing.Identity != identity {
			return nil, errors.New("selected lane id collides with different immutable identity")
		}
		if cwd := strings.TrimSpace(command.CWD); cwd != "" && existing.CWD != "" && existing.CWD != cwd {
			return nil, errors.New("selected lane workspace changed without a new workspace epoch")
		}
		if strings.TrimSpace(command.Owner.TabID) != "" {
			owner := command.Owner
			owner.TabID = strings.TrimSpace(owner.TabID)
			owner.AgentOwnerKey = strings.TrimSpace(owner.AgentOwnerKey)
			if owner.AgentOwnerKey == "" {
				owner.AgentOwnerKey = existing.Owner.AgentOwnerKey
			}
			existing.Owner = owner
		}
		if existing.CWD == "" {
			existing.CWD = strings.TrimSpace(command.CWD)
		}
		if modelID := strings.TrimSpace(command.ModelID); modelID != "" {
			existing.ModelID = modelID
		}
		if modeID := strings.TrimSpace(command.ModeID); modeID != "" {
			existing.ModeID = modeID
		}
		if existing.Provision != nil && existing.Creation != command.Creation {
			return nil, errors.New("selected lane changed deferred creation semantics while provisional")
		}
		existing.Creation = command.Creation
		if existing.CreationFailedBeforeEstablishment() {
			// Selection is explicit user intent. A lane with no ThreadRef and no
			// provider candidate is still absent, even when an older build labeled
			// the failed create blocked or broken.
			existing.Phase = LaneAbsent
		}
		// Releases before the initial-seed law may already have created an exact
		// target thread and then blocked it solely because context import was
		// unsupported. If that lane provably never consumed provider input, it is
		// still safe to use that same untouched thread for the one-time seed.
		if !existing.InitialSeedPending && laneCanUseInitialSeed(*state, existing) {
			existing.InitialSeedPending = true
			existing.LastError = ""
			switch {
			case existing.Thread.IsZero():
				existing.Phase = LaneAbsent
			case existing.Attachment != nil:
				existing.Phase = LaneReady
			default:
				existing.Phase = LaneDetached
			}
		}
		state.Lanes[identity.ID] = existing
	} else {
		lane := LaneState{
			Identity: identity, Owner: command.Owner, CWD: strings.TrimSpace(command.CWD),
			ModelID: strings.TrimSpace(command.ModelID), ModeID: strings.TrimSpace(command.ModeID),
			Phase: LaneAbsent, Coverage: make(map[uint64]CoverageRecord), Creation: command.Creation,
			InitialSeedPending:          true,
			CreateAfterCandidateAbsence: command.Creation.DeferredUntilInput,
		}
		for sequence := uint64(1); sequence <= state.ContextFloor; sequence++ {
			lane.Coverage[sequence] = CoverageRecord{
				Sequence: sequence, EventID: state.Ledger[sequence-1].EventID, Status: CoverageExcluded,
			}
			lane.CoveredThrough = sequence
		}
		state.Lanes[identity.ID] = lane
	}
	state.DesiredLaneID = identity.ID
	if state.Foreground != nil {
		return nil, nil
	}
	return drive(state)
}

func laneCanUseInitialSeed(state State, lane LaneState) bool {
	if lane.Phase != LaneBlocked || lane.LastError != provider.ErrorUnsupportedCapability || lane.PendingImport != nil {
		return false
	}
	for _, record := range lane.Coverage {
		if record.Status == CoverageNativeSeen || record.Status == CoverageImported || record.Status == CoverageSeeded {
			return false
		}
	}
	for _, event := range state.Ledger {
		if event.LaneID == lane.Identity.ID {
			return false
		}
	}
	if state.Foreground != nil && state.Foreground.LaneID == lane.Identity.ID ||
		state.PendingSteer != nil && state.PendingSteer.LaneID == lane.Identity.ID {
		return false
	}
	for _, entry := range state.Outbox {
		if entry.LaneID != lane.Identity.ID {
			continue
		}
		switch entry.Kind {
		case EffectStartTurn, EffectSteerTurn:
			if entry.Status != OutboxFailed {
				return false
			}
		}
	}
	return true
}

func reduceLaneOpened(state *State, command LaneOpened) ([]Effect, error) {
	lane, ok := state.Lanes[command.LaneID]
	if !ok {
		return nil, errors.New("opened lane is unknown")
	}
	openingPhase := lane.Phase
	reconciledCreate := openingPhase == LaneBlocked && command.Reconciled && lane.Thread.IsZero() && outboxHas(state, createEffectID(command.LaneID, lane.CreateGeneration), OutboxAmbiguous)
	if openingPhase != LaneCreating && openingPhase != LaneResuming && !reconciledCreate {
		return nil, fmt.Errorf("lane opened from illegal phase %q", lane.Phase)
	}
	canonical := command.Identity.Normalize()
	if canonical.ID == "" {
		canonical = lane.Identity
	}
	openedLaneID := command.LaneID
	if canonical != lane.Identity {
		var err error
		openedLaneID, lane, err = canonicalizeCreatingLane(state, command.LaneID, lane, canonical, openingPhase, reconciledCreate)
		if err != nil {
			return nil, err
		}
	}
	thread := command.Thread.Normalize()
	if err := thread.Validate(canonical.Realm.ProviderID); err != nil {
		return nil, err
	}
	if !lane.Thread.IsZero() && !lane.Thread.Equal(thread) {
		return nil, errors.New("resume returned a replacement native thread")
	}
	if lane.Provision != nil && !lane.Provision.Equal(thread) {
		return nil, errors.New("provider creation committed a different native candidate")
	}
	if !command.Context.ExactResume {
		lane.Phase = LaneBroken
		lane.LastError = provider.ErrorUnsupportedCapability
		state.Lanes[command.LaneID] = lane
		return nil, nil
	}
	lane.Thread = thread
	lane.Provision = nil
	lane.CreateAfterCandidateAbsence = false
	if lane.Coverage == nil {
		lane.Coverage = make(map[uint64]CoverageRecord)
	}
	lane.Context = command.Context
	lane.Delivery = command.Delivery
	lane.ConnectionGeneration = command.ConnectionGeneration
	if lane.ConnectionGeneration == 0 {
		lane.ConnectionGeneration = 1
	}
	lane.LastEventSequence = 0
	if command.Attachment != nil {
		attachment := command.Attachment.Clone()
		lane.Attachment = &attachment
	} else {
		lane.Attachment = nil
	}
	lane.Phase = LaneReady
	lane.LastError = ""
	state.Lanes[openedLaneID] = lane
	if openingPhase == LaneCreating || reconciledCreate {
		updateOutbox(state, createEffectID(command.LaneID, lane.CreateGeneration), OutboxCompleted, "")
	} else {
		updateOutbox(state, resumeEffectID(openedLaneID, command.ConnectionGeneration), OutboxCompleted, "")
	}
	if lane.PendingImport != nil {
		// A restarted actor must attach the exact native thread before it can
		// read back the already-dispatched import operation. Keep that immutable
		// batch and operation id; the recovered outbox chooses readback vs send.
		lane.Phase = LaneImporting
		state.Lanes[openedLaneID] = lane
		return nil, nil
	}
	if state.Foreground != nil && state.Foreground.LaneID == openedLaneID && state.Foreground.Status == ForegroundReconciling {
		rearmTurnReconcileOutbox(state, state.Foreground.OperationID, state.Foreground.Turn)
		lane.Phase = LaneReconciling
		state.Lanes[openedLaneID] = lane
		return []Effect{ReconcileTurnEffect{
			LaneID: openedLaneID, OperationID: state.Foreground.OperationID, Turn: state.Foreground.Turn,
		}}, nil
	}
	if state.Foreground != nil && state.Foreground.LaneID == openedLaneID && state.Foreground.Status == ForegroundDispatching {
		// The user input is still only Pending in the durable outbox. Restoring
		// the disposable attachment makes that already-recorded effect executable;
		// it does not create or resend any additional input.
		lane.Phase = LaneRunning
		state.Lanes[openedLaneID] = lane
		return nil, nil
	}
	return drive(state)
}

func reduceLaneProvisioned(state *State, command LaneProvisioned) ([]Effect, error) {
	lane, ok := state.Lanes[command.LaneID]
	if !ok {
		return nil, errors.New("provisioned lane is unknown")
	}
	if lane.Phase != LaneCreating {
		return nil, fmt.Errorf("lane provisioned from illegal phase %q", lane.Phase)
	}
	if !lane.Thread.IsZero() {
		return nil, errors.New("established lane cannot accept a creation candidate")
	}
	canonical := command.Identity.Normalize()
	if canonical.ID == "" {
		canonical = lane.Identity
	}
	openedLaneID := command.LaneID
	if canonical != lane.Identity {
		var err error
		openedLaneID, lane, err = canonicalizeCreatingLane(state, command.LaneID, lane, canonical, LaneCreating, false)
		if err != nil {
			return nil, err
		}
	}
	candidate := command.Candidate.Normalize()
	if err := candidate.Validate(canonical.Realm.ProviderID); err != nil {
		return nil, err
	}
	if !command.Creation.DeferredUntilInput {
		return nil, errors.New("provider returned a provisional candidate without deferred creation semantics")
	}
	if !command.Delivery.StableInputIdentity || !command.Delivery.ConsumptionReceipt {
		return nil, errors.New("deferred provider candidate lacks a stable input consumption receipt")
	}
	if !command.Context.ExactResume {
		return nil, errors.New("provider candidate cannot become a durable lane without exact resume")
	}
	if lane.Provision != nil && !lane.Provision.Equal(candidate) && !command.PreviousCandidateAbsent {
		return nil, errors.New("provider changed an uncommitted candidate without proving native absence")
	}
	if command.PreviousCandidateAbsent && (!command.Reconciled || !lane.CreateAfterCandidateAbsence) {
		return nil, errors.New("provider changed a candidate without actor proof that the lane was empty")
	}
	lane.Provision = &candidate
	lane.Context = command.Context
	lane.Delivery = command.Delivery
	lane.Creation = command.Creation
	lane.ConnectionGeneration = command.ConnectionGeneration
	if lane.ConnectionGeneration == 0 {
		lane.ConnectionGeneration = 1
	}
	lane.LastEventSequence = 0
	if command.Attachment != nil {
		attachment := command.Attachment.Clone()
		lane.Attachment = &attachment
	} else {
		lane.Attachment = nil
	}
	lane.Phase = LaneCreating
	lane.LastError = ""
	state.Lanes[openedLaneID] = lane
	if !updateOutbox(state, createEffectID(command.LaneID, lane.CreateGeneration), OutboxCompleted, "") {
		return nil, errors.New("provider candidate has no durable create operation")
	}

	if state.Foreground != nil {
		if state.Foreground.LaneID != openedLaneID {
			return nil, errors.New("provider candidate conflicts with another foreground owner")
		}
		if command.PreviousCandidateAbsent {
			state.Foreground.Status = ForegroundDispatching
			state.Foreground.Turn = provider.TurnRef{}
			state.Foreground.UserConsumed = false
			if !updateOutbox(state, startTurnEffectID(state.Foreground.OperationID), OutboxPending, "") {
				return nil, errors.New("new provider candidate lost its durable prompt operation")
			}
		}
		return nil, nil
	}
	if len(state.Queue) == 0 || state.Queue[0].LaneID != openedLaneID {
		return nil, nil
	}
	return beginQueuedForeground(state, openedLaneID, lane, true)
}

// canonicalizeCreatingLane is the sole identity-rekey transition. Realm
// attestation may become available only in the provider's session/new reply,
// so a proposal is allowed to become canonical exactly once and only while it
// has no native thread, events, foreground ownership, or side activities.
func canonicalizeCreatingLane(
	state *State,
	requestedID provider.LaneID,
	lane LaneState,
	canonical provider.LaneIdentity,
	openingPhase LanePhase,
	reconciledCreate bool,
) (provider.LaneID, LaneState, error) {
	if openingPhase != LaneCreating && !reconciledCreate || !lane.Thread.IsZero() {
		return "", lane, errors.New("established lane identity cannot be canonicalized")
	}
	if err := canonical.Validate(); err != nil {
		return "", lane, err
	}
	proposed := lane.Identity.Normalize()
	if canonical.ChatID != proposed.ChatID || canonical.Realm.ProviderID != proposed.Realm.ProviderID ||
		canonical.Realm.MachineID != proposed.Realm.MachineID || canonical.WorkspaceEpoch != proposed.WorkspaceEpoch {
		return "", lane, errors.New("provider creation changed chat, provider, machine, or workspace identity")
	}
	if canonical.ID != requestedID {
		if _, exists := state.Lanes[canonical.ID]; exists {
			return "", lane, errors.New("canonical provider realm collides with an existing chat lane")
		}
	}
	if state.Foreground != nil || state.PendingSteer != nil || state.PendingCancel != nil {
		return "", lane, errors.New("creating lane unexpectedly owns foreground control state")
	}
	for _, value := range state.Tools {
		if value.Owner.LaneID == requestedID {
			return "", lane, errors.New("creating lane already owns a tool")
		}
	}
	for _, value := range state.Plans {
		if value.Owner.LaneID == requestedID {
			return "", lane, errors.New("creating lane already owns a plan")
		}
	}
	for _, value := range state.Permissions {
		if value.Owner.LaneID == requestedID {
			return "", lane, errors.New("creating lane already owns a permission")
		}
	}
	for _, value := range state.Background {
		if value.Owner.LaneID == requestedID {
			return "", lane, errors.New("creating lane already owns background work")
		}
	}
	if _, ok := state.Usage[requestedID]; ok {
		return "", lane, errors.New("creating lane already owns usage")
	}
	if _, ok := state.Compactions[requestedID]; ok {
		return "", lane, errors.New("creating lane already owns compaction state")
	}
	if _, ok := state.Transport[requestedID]; ok {
		return "", lane, errors.New("creating lane already owns transport state")
	}

	delete(state.Lanes, requestedID)
	lane.Identity = canonical
	state.Lanes[canonical.ID] = lane
	if state.ActiveLaneID == requestedID {
		state.ActiveLaneID = canonical.ID
	}
	if state.DesiredLaneID == requestedID {
		state.DesiredLaneID = canonical.ID
	}
	for i := range state.Queue {
		if state.Queue[i].LaneID == requestedID {
			state.Queue[i].LaneID = canonical.ID
		}
	}
	for i := range state.Outbox {
		if state.Outbox[i].LaneID == requestedID {
			state.Outbox[i].LaneID = canonical.ID
		}
	}
	for operationID, receipt := range state.LaneSelectionMutationReceipts {
		if receipt.LaneID == requestedID {
			receipt.LaneID = canonical.ID
			state.LaneSelectionMutationReceipts[operationID] = receipt
		}
	}
	return canonical.ID, lane, nil
}

func reduceLaneOpenFailed(state *State, command LaneOpenFailed) error {
	lane, ok := state.Lanes[command.LaneID]
	if !ok {
		return errors.New("failed lane is unknown")
	}
	if lane.Phase != LaneCreating && lane.Phase != LaneResuming && lane.Phase != LaneReconciling {
		return fmt.Errorf("lane open failed from illegal phase %q", lane.Phase)
	}
	if lane.Thread.IsZero() && lane.Provision == nil {
		// session/new must return its native id before any prompt can be sent.
		// Without a ThreadRef or provisional candidate there is no established
		// lane to break and no input whose delivery could be duplicated. Preserve
		// the failed receipt, remain absent, and wait for explicit user intent.
		lane.Phase = LaneAbsent
	} else if command.Ambiguous {
		lane.Phase = LaneBlocked
	} else {
		lane.Phase = LaneBroken
	}
	lane.LastError = command.Kind
	state.Lanes[command.LaneID] = lane
	if lane.Thread.IsZero() {
		status := OutboxFailed
		if command.Ambiguous {
			status = OutboxAmbiguous
		}
		updateOutbox(state, createEffectID(command.LaneID, lane.CreateGeneration), status, command.Kind)
	} else {
		status := OutboxFailed
		if command.Ambiguous {
			status = OutboxAmbiguous
		}
		updateOutbox(state, resumeEffectID(command.LaneID, lane.ConnectionGeneration), status, command.Kind)
	}
	return nil
}

func reduceRetryLane(state *State, command RetryLane) ([]Effect, error) {
	lane, ok := state.Lanes[command.LaneID]
	if !ok {
		return nil, errors.New("retry lane is unknown")
	}
	retryingUnestablishedCreate := lane.CreationFailedBeforeEstablishment()
	if lane.Phase != LaneBroken && lane.Phase != LaneDetached && lane.Phase != LaneBlocked && !retryingUnestablishedCreate {
		return nil, fmt.Errorf("lane cannot retry from phase %q", lane.Phase)
	}
	if lane.PendingImport != nil {
		if !lane.Context.ImportReadback || !lane.Context.IdempotentImport {
			return nil, errors.New("context import cannot be reconciled safely")
		}
		found := false
		for index := range state.Outbox {
			entry := &state.Outbox[index]
			if entry.Kind != EffectImportContext || entry.OperationID != lane.PendingImport.OperationID || entry.Status != OutboxAmbiguous {
				continue
			}
			entry.Status = OutboxPending
			entry.Reconcile = true
			entry.LastError = ""
			found = true
			break
		}
		if !found {
			return nil, errors.New("ambiguous context import lost its durable operation")
		}
		lane.LastError = ""
		if lane.Phase == LaneDetached {
			lane.Phase = LaneResuming
			lane.ConnectionGeneration++
			state.Lanes[command.LaneID] = lane
			return []Effect{ResumeLaneEffect{
				Identity: lane.Identity, Thread: lane.Thread, Owner: lane.Owner, CWD: lane.CWD,
				ModelID: lane.ModelID, ModeID: lane.ModeID, Generation: lane.ConnectionGeneration,
			}}, nil
		}
		lane.Phase = LaneImporting
		state.Lanes[command.LaneID] = lane
		return nil, nil
	}
	if lane.Provision != nil {
		if !lane.Thread.IsZero() || !lane.Creation.DeferredUntilInput {
			return nil, errors.New("provisional lane retry has contradictory thread ownership")
		}
		for i := len(state.Outbox) - 1; i >= 0; i-- {
			entry := &state.Outbox[i]
			if entry.Kind != EffectCreateLane || entry.LaneID != command.LaneID ||
				(entry.Status != OutboxAmbiguous && entry.Status != OutboxFailed) {
				continue
			}
			entry.Status = OutboxPending
			entry.Reconcile = true
			entry.LastError = ""
			lane.Phase = LaneCreating
			lane.LastError = ""
			state.Lanes[command.LaneID] = lane
			return nil, nil
		}
		lane.CreateGeneration++
		if lane.CreateGeneration <= lane.ConnectionGeneration {
			lane.CreateGeneration = lane.ConnectionGeneration + 1
		}
		lane.ConnectionGeneration = lane.CreateGeneration
		lane.Phase = LaneCreating
		lane.LastError = ""
		state.Lanes[command.LaneID] = lane
		return []Effect{CreateLaneEffect{
			Identity: lane.Identity, Owner: lane.Owner, CWD: lane.CWD, ModelID: lane.ModelID, ModeID: lane.ModeID,
			Generation: lane.CreateGeneration, Reconcile: true,
			CreateAfterCandidateAbsence: lane.CreateAfterCandidateAbsence,
		}}, nil
	}
	if lane.Thread.IsZero() {
		if !retryingUnestablishedCreate {
			lane.Phase = LaneBroken
			lane.LastError = provider.ErrorProtocolViolation
			state.Lanes[command.LaneID] = lane
			return nil, nil
		}
		lane.Phase = LaneCreating
		lane.CreateGeneration++
		if lane.CreateGeneration <= lane.ConnectionGeneration {
			lane.CreateGeneration = lane.ConnectionGeneration + 1
		}
		lane.LastError = ""
		state.Lanes[command.LaneID] = lane
		return []Effect{CreateLaneEffect{
			Identity: lane.Identity, Owner: lane.Owner, CWD: lane.CWD, ModelID: lane.ModelID, ModeID: lane.ModeID,
			Generation: lane.CreateGeneration, CreateAfterCandidateAbsence: lane.CreateAfterCandidateAbsence,
		}}, nil
	}
	lane.Phase = LaneResuming
	lane.LastError = ""
	lane.ConnectionGeneration++
	state.Lanes[command.LaneID] = lane
	return []Effect{ResumeLaneEffect{
		Identity: lane.Identity, Thread: lane.Thread, Owner: lane.Owner, CWD: lane.CWD, ModelID: lane.ModelID, ModeID: lane.ModeID,
		Generation: lane.ConnectionGeneration,
	}}, nil
}

func reduceSubmit(state *State, command Submit) ([]Effect, error) {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	if operationID == "" {
		return nil, errors.New("submit requires operation id")
	}
	if _, exists := state.Operations[operationID]; exists {
		return nil, errors.New("operation id already belongs to this chat")
	}
	if strings.TrimSpace(command.Text) == "" && len(command.Attachments) == 0 {
		return nil, errors.New("submit requires text or attachments")
	}
	target := command.LaneID
	if target == "" {
		target = state.DesiredLaneID
	}
	if target == "" {
		target = state.ActiveLaneID
	}
	lane, ok := state.Lanes[target]
	if !ok || target == "" {
		return nil, errors.New("submit requires a selected target lane")
	}
	if err := recoverLegacyUnconsumedSteerCoverage(state, target); err != nil {
		return nil, err
	}
	lane = state.Lanes[target]
	if lane.CreationFailedBeforeEstablishment() {
		// Submit is also explicit intent. Reopen only a create that never acquired
		// native identity; established and provisional lanes retain their exact
		// resume/reconciliation boundaries.
		lane.Phase = LaneAbsent
		state.Lanes[target] = lane
	}
	presentation, err := normalizeTurnPresentation(command.Presentation)
	if err != nil {
		return nil, err
	}
	state.Operations[operationID] = struct{}{}
	state.Queue = append(state.Queue, QueueEntry{
		OperationID:  operationID,
		LaneID:       target,
		Text:         strings.TrimSpace(command.Text),
		Attachments:  append([]provider.Attachment(nil), command.Attachments...),
		ModelID:      strings.TrimSpace(command.ModelID),
		ModeID:       strings.TrimSpace(command.ModeID),
		Permission:   strings.TrimSpace(command.Permission),
		Presentation: presentation,
		Revision:     state.Revision + 1,
	})
	if presentation.QueueID != "" {
		state.Presentation.AgentQueueRevision++
	}
	return drive(state)
}

// Releases before unconsumed steering rows received an explicit excluded
// coverage record can leave an established lane blocked behind its own
// ambiguity-fenced input. That input must remain visible and must never be
// replayed, but it is also not cross-provider context to import. A later
// explicit submit may therefore advance only a contiguous tail of exact
// same-lane, terminal-unconsumed steer rows whose durable steer effects remain
// fenced as acceptance-ambiguous.
func recoverLegacyUnconsumedSteerCoverage(state *State, laneID provider.LaneID) error {
	lane, ok := state.Lanes[laneID]
	if !ok || lane.Phase != LaneBlocked || lane.LastError != provider.ErrorUnsupportedCapability ||
		lane.Thread.IsZero() || lane.PendingImport != nil || state.Foreground != nil || state.PendingSteer != nil ||
		lane.CoveredThrough >= state.LedgerHead() {
		return nil
	}
	from := lane.CoveredThrough + 1
	for sequence := from; sequence <= state.LedgerHead(); sequence++ {
		event := state.Ledger[sequence-1]
		if event.Sequence != sequence || event.LaneID != laneID || event.Role != "user" ||
			event.TerminalState != "unconsumed" ||
			(event.SteerState != "accepted" && event.SteerState != "uncertain") ||
			event.OperationID == "" || !ambiguousSteerOutboxHas(state, event.OperationID) {
			return nil
		}
	}
	for sequence := from; sequence <= state.LedgerHead(); sequence++ {
		event := state.Ledger[sequence-1]
		if err := setCoverage(&lane, state, sequence, CoverageExcluded, event.OperationID); err != nil {
			return err
		}
	}
	lane.LastError = ""
	if lane.Attachment != nil {
		lane.Phase = LaneReady
	} else {
		lane.Phase = LaneDetached
	}
	state.Lanes[laneID] = lane
	return nil
}

func ambiguousSteerOutboxHas(state *State, operationID provider.OperationID) bool {
	for i := range state.Outbox {
		entry := state.Outbox[i]
		if entry.ID == steerEffectID(operationID) && entry.Kind == EffectSteerTurn &&
			entry.Status == OutboxAmbiguous && entry.LastError == provider.ErrorAcceptanceAmbiguous {
			return true
		}
	}
	return false
}

func reduceSteer(state *State, command Steer) ([]Effect, error) {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	if operationID == "" {
		return nil, errors.New("steer requires operation id")
	}
	if _, exists := state.Operations[operationID]; exists {
		return nil, errors.New("operation id already belongs to this chat")
	}
	if strings.TrimSpace(command.Text) == "" && len(command.Attachments) == 0 {
		return nil, errors.New("steer requires text or attachments")
	}
	if state.Foreground == nil || state.Foreground.Status != ForegroundRunning || state.Foreground.Turn.NativeID == "" {
		return nil, errors.New("steer requires a running foreground turn")
	}
	if state.PendingSteer != nil {
		return nil, errors.New("another steer is awaiting a provider receipt")
	}
	presentation, err := normalizeTurnPresentation(command.Presentation)
	if err != nil {
		return nil, err
	}
	state.Operations[operationID] = struct{}{}
	state.PendingSteer = &PendingSteer{
		OperationID: operationID, LaneID: state.Foreground.LaneID, Turn: state.Foreground.Turn,
		Text: strings.TrimSpace(command.Text), Attachments: append([]provider.Attachment(nil), command.Attachments...),
		Presentation: presentation, Status: SteerDispatching,
	}
	return []Effect{SteerTurnEffect{
		LaneID: state.Foreground.LaneID, OperationID: operationID, Turn: state.Foreground.Turn,
		Text: strings.TrimSpace(command.Text), Attachments: append([]provider.Attachment(nil), command.Attachments...),
	}}, nil
}

func reduceSteerAdmitted(state *State, command SteerAdmitted) error {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	if state.PendingSteer == nil || state.PendingSteer.OperationID != operationID {
		if outboxHas(state, steerEffectID(operationID), OutboxConsumed) || outboxHas(state, steerEffectID(operationID), OutboxCompleted) || outboxHas(state, steerEffectID(operationID), OutboxAmbiguous) {
			return nil
		}
		return errors.New("steer admission does not match pending steer")
	}
	if !command.Accepted {
		return errors.New("rejected steer requires an explicit failure transition")
	}
	state.PendingSteer.Status = SteerAccepted
	state.PendingSteer.AwaitConsumption = command.AwaitConsumption
	state.PendingSteer.Interrupted = command.Interrupted
	updateOutbox(state, steerEffectID(operationID), OutboxAccepted, "")
	if command.Consumed {
		return reduceInputConsumed(state, InputConsumed{OperationID: operationID})
	}
	return nil
}

func reduceSteerFailed(state *State, command SteerFailed) ([]Effect, error) {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	if state.PendingSteer == nil || state.PendingSteer.OperationID != operationID {
		// An urgent explicit cancellation may terminate the foreground while a
		// provider steer acknowledgement is still in flight. The terminal reducer
		// has already preserved that input exactly once as unconsumed/uncertain;
		// a late failure is never permission to replay or move it to FIFO.
		if outboxHas(state, steerEffectID(operationID), OutboxAmbiguous) || outboxHas(state, steerEffectID(operationID), OutboxCompleted) {
			return nil, nil
		}
		return nil, errors.New("steer failure does not match pending steer")
	}
	pending := state.PendingSteer
	kind := command.Kind
	if kind == "" {
		kind = provider.ErrorAdmissionRejected
	}
	if command.Ambiguous {
		pending.Status = SteerUncertain
		state.PendingSteer = pending
		updateOutbox(state, steerEffectID(operationID), OutboxAmbiguous, provider.ErrorAcceptanceAmbiguous)
		return nil, nil
	}
	state.PendingSteer = nil
	// Any definite non-consumption transfers the same immutable operation into
	// the FIFO. Acceptance ambiguity is handled above and never resends. The
	// provider adapter owns that classification; chat logic never parses text.
	foregroundInput := state.Foreground.Input
	state.Queue = append([]QueueEntry{{
		OperationID: operationID, LaneID: pending.LaneID, Text: pending.Text,
		Attachments: append([]provider.Attachment(nil), pending.Attachments...),
		ModelID:     foregroundInput.ModelID, ModeID: foregroundInput.ModeID, Permission: foregroundInput.Permission,
		Presentation: pending.Presentation, Revision: state.Revision + 1,
	}}, state.Queue...)
	state.Presentation.AgentQueueRevision++
	updateOutbox(state, steerEffectID(operationID), OutboxCompleted, kind)
	return nil, nil
}

func normalizeTurnPresentation(presentation provider.TurnPresentation) (provider.TurnPresentation, error) {
	presentation.UserMessageID = strings.TrimSpace(presentation.UserMessageID)
	presentation.AssistantMessageID = strings.TrimSpace(presentation.AssistantMessageID)
	presentation.QueueID = strings.TrimSpace(presentation.QueueID)
	presentation.PromptText = strings.TrimSpace(presentation.PromptText)
	presentation.Title = strings.TrimSpace(presentation.Title)
	presentation.Origin = strings.ToLower(strings.TrimSpace(presentation.Origin))
	presentation.StartedAt = strings.TrimSpace(presentation.StartedAt)
	switch presentation.Origin {
	case "", "human", "agent", "internal":
		return presentation, nil
	default:
		return provider.TurnPresentation{}, errors.New("turn presentation origin is invalid")
	}
}

func reducePrepareQueuedTurn(state *State, command PrepareQueuedTurn) error {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	if operationID == "" {
		return errors.New("queued turn preparation requires an operation id")
	}
	presentation, err := normalizeTurnPresentation(command.Presentation)
	if err != nil {
		return err
	}
	apply := func(input *QueueEntry) {
		input.ModelID = firstNonEmptyString(strings.TrimSpace(command.ModelID), input.ModelID)
		input.ModeID = firstNonEmptyString(strings.TrimSpace(command.ModeID), input.ModeID)
		input.Permission = firstNonEmptyString(strings.TrimSpace(command.Permission), input.Permission)
		input.Presentation = presentation
	}
	if state.Foreground != nil && state.Foreground.OperationID == operationID {
		if state.Foreground.Status != ForegroundDispatching {
			return errors.New("queued turn was already admitted before public ownership was prepared")
		}
		apply(&state.Foreground.Input)
		for index := range state.Outbox {
			entry := &state.Outbox[index]
			if entry.Kind != EffectStartTurn || entry.OperationID != operationID {
				continue
			}
			if entry.Status != OutboxPending || entry.Input == nil {
				return errors.New("queued turn provider effect was already dispatched")
			}
			copyInput := state.Foreground.Input
			copyInput.Attachments = append([]provider.Attachment(nil), state.Foreground.Input.Attachments...)
			entry.Input = &copyInput
			return nil
		}
		return errors.New("queued foreground has no durable start effect")
	}
	for index := range state.Queue {
		if state.Queue[index].OperationID == operationID {
			apply(&state.Queue[index])
			return nil
		}
	}
	return errors.New("queued turn operation is not owned by this chat")
}

func reduceCancelTurn(state *State, command CancelTurn) ([]Effect, error) {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	if operationID == "" {
		return nil, errors.New("cancel requires operation id")
	}
	if _, exists := state.Operations[operationID]; exists {
		return nil, errors.New("operation id already belongs to this chat")
	}
	if state.Foreground == nil || state.Foreground.Turn.NativeID == "" {
		return nil, errors.New("cancel requires a provider-owned foreground turn")
	}
	if state.PendingCancel != nil {
		return nil, errors.New("foreground turn already has a pending cancellation")
	}
	state.Operations[operationID] = struct{}{}
	state.QueueControl.Paused = true
	state.QueueControl.Revision++
	state.PendingCancel = &PendingCancel{
		OperationID: operationID, LaneID: state.Foreground.LaneID, Turn: state.Foreground.Turn,
	}
	return []Effect{CancelTurnEffect{
		LaneID: state.Foreground.LaneID, OperationID: operationID, Turn: state.Foreground.Turn,
	}}, nil
}

func reduceRecordCancelReceipt(state *State, command RecordCancelReceipt) error {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	reason := strings.ToLower(strings.TrimSpace(command.Reason))
	if operationID == "" {
		return errors.New("cancel receipt requires operation id")
	}
	if receipt, exists := state.CancelMutationReceipts[operationID]; exists {
		if receipt.Cancelled != command.Cancelled || receipt.Reason != reason {
			return errors.New("cancel receipt operation was reused for a different result")
		}
		return nil
	}
	if reason == "" {
		return errors.New("cancel receipt requires a reason")
	}
	if command.Cancelled || reason != "idle" {
		return errors.New("cancel receipt may only commit the provider-neutral idle result")
	}
	if _, exists := state.Operations[operationID]; exists {
		return errors.New("cancel receipt operation id conflicts with another actor operation")
	}
	state.Operations[operationID] = struct{}{}
	state.CancelMutationReceipts[operationID] = CancelMutationReceipt{Cancelled: false, Reason: reason}
	return nil
}

func reduceRecordAgentWaitObservation(state *State, command RecordAgentWaitObservation) error {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	tabID := strings.TrimSpace(command.TabID)
	digest := strings.ToLower(strings.TrimSpace(command.Digest))
	if operationID == "" || len(operationID) > 256 || tabID == "" || len(tabID) > 256 || !validAgentWaitObservationDigest(digest) {
		return errors.New("agent wait observation requires bounded operation, tab, and digest identity")
	}
	if !state.Initialized {
		return errors.New("agent wait observation requires an initialized chat")
	}
	if tabID != strings.TrimSpace(state.Presentation.TabID) {
		return errors.New("agent wait observation tab attachment is stale")
	}
	if receipt, exists := state.AgentWaitObservationReceipts[operationID]; exists {
		if receipt.Digest != digest {
			return errors.New("agent wait operation id was reused for a different request")
		}
		return nil
	}
	if _, exists := state.Operations[operationID]; exists {
		return errors.New("agent wait operation id conflicts with another actor operation")
	}
	state.Operations[operationID] = struct{}{}
	state.AgentWaitObservationReceipts[operationID] = AgentWaitObservationReceipt{Digest: digest}
	return nil
}

func reduceCancelAcknowledged(state *State, command CancelAcknowledged) error {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	if state.PendingCancel == nil || state.PendingCancel.OperationID != operationID {
		if outboxHas(state, cancelEffectID(operationID), OutboxCompleted) {
			return nil
		}
		return errors.New("cancel receipt does not match pending cancellation")
	}
	updateOutbox(state, cancelEffectID(operationID), OutboxAccepted, "")
	return nil
}

func reduceCancelFailed(state *State, command CancelFailed) error {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	if state.PendingCancel == nil || state.PendingCancel.OperationID != operationID {
		if outboxHas(state, cancelEffectID(operationID), OutboxCompleted) {
			return nil
		}
		return errors.New("cancel failure does not match pending cancellation")
	}
	state.PendingCancel = nil
	kind := command.Kind
	if kind == "" {
		kind = provider.ErrorAdmissionRejected
	}
	updateOutbox(state, cancelEffectID(operationID), OutboxFailed, kind)
	return nil
}

func reduceResolvePermission(state *State, command ResolvePermission) ([]Effect, error) {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	requestID, optionID := strings.TrimSpace(command.RequestID), strings.TrimSpace(command.OptionID)
	if operationID == "" || requestID == "" || optionID == "" {
		return nil, errors.New("permission decision requires operation, request, and option identity")
	}
	if _, exists := state.Operations[operationID]; exists {
		return nil, errors.New("operation id already belongs to this chat")
	}
	permission, ok := state.Permissions[requestID]
	if !ok || permission.Event.Status != "pending" {
		return nil, errors.New("permission request is not pending")
	}
	if len(permission.Event.Options) > 0 {
		found := false
		for _, candidate := range permission.Event.Options {
			if candidate == optionID {
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("permission option does not belong to the request")
		}
	}
	state.Operations[operationID] = struct{}{}
	permission.Event.Status = "resolving"
	state.Permissions[requestID] = permission
	return []Effect{ResolvePermissionEffect{
		LaneID: permission.Owner.LaneID, OperationID: operationID, RequestID: requestID, OptionID: optionID,
	}}, nil
}

func reducePermissionDecided(state *State, command PermissionDecided) error {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	requestID, optionID := strings.TrimSpace(command.RequestID), strings.TrimSpace(command.OptionID)
	permission, ok := state.Permissions[requestID]
	if !ok {
		return errors.New("permission receipt belongs to an unknown request")
	}
	if command.Ambiguous {
		permission.Event.Status = "uncertain"
		state.Permissions[requestID] = permission
		updateOutbox(state, permissionEffectID(operationID), OutboxAmbiguous, provider.ErrorAcceptanceAmbiguous)
		return nil
	}
	if !command.Accepted {
		permission.Event.Status = "failed"
		state.Permissions[requestID] = permission
		updateOutbox(state, permissionEffectID(operationID), OutboxFailed, provider.ErrorAdmissionRejected)
		return nil
	}
	permission.Event.Status = "resolved"
	permission.Event.Options = append([]string(nil), permission.Event.Options...)
	state.Permissions[requestID] = permission
	for index := range state.Outbox {
		if state.Outbox[index].Kind == EffectPermission && state.Outbox[index].OperationID == operationID && state.Outbox[index].RequestID == requestID && state.Outbox[index].OptionID == optionID {
			state.Outbox[index].Status = OutboxCompleted
			state.Outbox[index].LastError = ""
			return nil
		}
	}
	return errors.New("permission receipt has no durable decision effect")
}

func reduceContextImported(state *State, command ContextImported) ([]Effect, error) {
	lane, ok := state.Lanes[command.LaneID]
	reconcilingAmbiguous := lane.Phase == LaneBlocked && lane.PendingImport != nil && outboxHas(state, string(command.OperationID), OutboxAmbiguous)
	if !ok || lane.PendingImport == nil || (lane.Phase != LaneImporting && !reconcilingAmbiguous) {
		return nil, errors.New("context import does not match a pending lane import")
	}
	pending := lane.PendingImport
	if command.OperationID != pending.OperationID || command.From != pending.From || command.To != pending.To || command.Digest != pending.Batch.Digest {
		return nil, errors.New("context import receipt identity does not match")
	}
	if command.Confirmed && !command.Found {
		return nil, errors.New("context import receipt confirmed an operation it did not find")
	}
	if command.Ambiguous {
		lane.Phase = LaneBlocked
		lane.LastError = provider.ErrorAcceptanceAmbiguous
		state.Lanes[command.LaneID] = lane
		updateOutbox(state, string(command.OperationID), OutboxAmbiguous, provider.ErrorAcceptanceAmbiguous)
		return nil, nil
	}
	if command.Reconciled && !command.Found {
		if !lane.Context.ImportReadback || !lane.Context.IdempotentImport {
			return nil, errors.New("provider reported an absent import without negotiated idempotent readback")
		}
		for index := range state.Outbox {
			entry := &state.Outbox[index]
			if entry.Kind != EffectImportContext || entry.OperationID != command.OperationID {
				continue
			}
			entry.Status = OutboxPending
			entry.Reconcile = false
			entry.LastError = ""
			lane.Phase = LaneImporting
			lane.LastError = ""
			state.Lanes[command.LaneID] = lane
			return nil, nil
		}
		return nil, errors.New("reconciled context import has no durable outbox entry")
	}
	if !command.Confirmed {
		lane.Phase = LaneBlocked
		lane.LastError = provider.ErrorAdmissionRejected
		lane.PendingImport = nil
		state.Lanes[command.LaneID] = lane
		updateOutbox(state, string(command.OperationID), OutboxFailed, provider.ErrorAdmissionRejected)
		return nil, nil
	}
	for _, message := range pending.Batch.Messages {
		if err := setCoverage(&lane, state, message.LedgerSequence, CoverageImported, command.OperationID); err != nil {
			return nil, err
		}
	}
	lane.InitialSeedPending = false
	lane.PendingImport = nil
	lane.Phase = LaneReady
	lane.LastError = ""
	state.Lanes[command.LaneID] = lane
	updateOutbox(state, string(command.OperationID), OutboxCompleted, "")
	return drive(state)
}

func reduceTurnAdmitted(state *State, command TurnAdmitted) error {
	if state.Foreground == nil || state.Foreground.OperationID != command.OperationID {
		return errors.New("turn admission does not match foreground operation")
	}
	if state.Foreground.Status == ForegroundRunning && state.Foreground.Turn == command.Turn && command.Accepted && !command.Ambiguous {
		return nil
	}
	lane := state.Lanes[state.Foreground.LaneID]
	if command.Ambiguous {
		state.Foreground.Status = ForegroundUncertain
		lane.Phase = LaneBlocked
		lane.LastError = provider.ErrorAcceptanceAmbiguous
		state.Lanes[state.Foreground.LaneID] = lane
		updateOutbox(state, startTurnEffectID(command.OperationID), OutboxAmbiguous, provider.ErrorAcceptanceAmbiguous)
		return nil
	}
	if !command.Accepted {
		return errors.New("rejected turn admission requires an explicit failure transition")
	}
	state.Foreground.Turn = command.Turn
	state.Foreground.Status = ForegroundRunning
	if lane.Provision != nil {
		lane.Phase = LaneCreating
	} else {
		lane.Phase = LaneRunning
	}
	lane.LastError = ""
	state.Lanes[state.Foreground.LaneID] = lane
	updateOutbox(state, startTurnEffectID(command.OperationID), OutboxAccepted, "")
	return nil
}

func reduceTurnAdmissionFailed(state *State, command TurnAdmissionFailed) ([]Effect, error) {
	if state.Foreground == nil || state.Foreground.OperationID != command.OperationID {
		return nil, errors.New("turn admission failure does not match foreground operation")
	}
	kind := command.Kind
	if kind == "" {
		kind = provider.ErrorAdmissionRejected
	}
	laneID := state.Foreground.LaneID
	lane := state.Lanes[laneID]
	lane.Phase = LaneBlocked
	lane.LastError = kind
	state.Lanes[laneID] = lane
	updateOutbox(state, startTurnEffectID(command.OperationID), OutboxFailed, kind)
	if err := appendForegroundAdmissionFailure(state, kind); err != nil {
		return nil, err
	}
	settleForegroundObligation(state, "failed", &provider.TerminalEvent{
		Status: "failed", Error: string(kind), DispositionState: "needs_input", DispositionSource: "inferred",
	}, 0)
	state.Foreground = nil
	return drive(state)
}

func reduceTurnReconciled(state *State, command TurnReconciled) ([]Effect, error) {
	if state.Foreground == nil || state.Foreground.OperationID != command.OperationID || state.Foreground.Status != ForegroundReconciling {
		return nil, errors.New("turn reconciliation does not match a reconciling foreground operation")
	}
	laneID := state.Foreground.LaneID
	lane := state.Lanes[laneID]
	if !command.Found {
		state.Foreground.Status = ForegroundUncertain
		lane.Phase = LaneBlocked
		lane.LastError = provider.ErrorProtocolViolation
		state.Lanes[laneID] = lane
		updateOutbox(state, reconcileTurnEffectID(command.OperationID), OutboxFailed, provider.ErrorProtocolViolation)
		return nil, nil
	}
	if command.Turn.OperationID != command.OperationID || strings.TrimSpace(command.Turn.NativeID) == "" {
		return nil, errors.New("turn reconciliation changed the native turn owner")
	}
	state.Foreground.Turn = command.Turn
	state.Foreground.Status = ForegroundRunning
	lane.Phase = LaneRunning
	lane.LastError = ""
	state.Lanes[laneID] = lane
	updateOutbox(state, reconcileTurnEffectID(command.OperationID), OutboxCompleted, "")
	if command.Consumed {
		if err := reduceInputConsumed(state, InputConsumed{OperationID: command.OperationID}); err != nil {
			return nil, err
		}
	}
	if command.Terminal {
		status := strings.TrimSpace(command.Status)
		if status == "" {
			status = "completed"
		}
		terminal := &provider.TerminalEvent{Status: status}
		switch strings.ToLower(status) {
		case "completed", "done":
			terminal.Status = "completed"
		case "cancelled", "canceled":
			terminal.Status = "cancelled"
			terminal.StopReason = "cancelled"
		case "failed", "error":
			terminal.Status = "failed"
		case "interrupted":
			// This command is reachable only after an accepted turn lost its
			// disposable host and exact-resumed for operation readback. The native
			// provider has proved the input existed and was interrupted; represent
			// that terminal outcome honestly and never resend it.
			terminal.Status = "failed"
			terminal.StopReason = "engine-crash"
			terminal.Interrupted = true
			terminal.CrashInterrupted = true
		default:
			return nil, fmt.Errorf("unknown reconciled provider terminal status %q", command.Status)
		}
		return reduceTurnTerminated(state, TurnTerminated{
			OperationID: command.OperationID, Status: terminal.Status, StopReason: terminal.StopReason, Terminal: terminal,
		})
	}
	return nil, nil
}

func reduceInputConsumed(state *State, command InputConsumed) error {
	if state.PendingSteer != nil && state.PendingSteer.OperationID == command.OperationID {
		if command.Thread != nil {
			return errors.New("steer consumption cannot establish a provider thread")
		}
		pending := state.PendingSteer
		if state.Foreground == nil || state.Foreground.LaneID != pending.LaneID || state.Foreground.Turn != pending.Turn {
			return errors.New("consumed steer lost its foreground turn owner")
		}
		// A provider cannot consume a later steer before it owns the original
		// input. Preserve chronology even when the two receipts cross goroutines.
		if !state.Foreground.UserConsumed {
			if err := reduceInputConsumed(state, InputConsumed{OperationID: state.Foreground.OperationID}); err != nil {
				return err
			}
		}
		if shouldPersistForegroundSegment(state.Foreground) {
			if _, err := appendForegroundAssistantSegment(state, "done", false, 0); err != nil {
				return err
			}
		}
		if _, err := appendConsumedSteer(state, pending); err != nil {
			return err
		}
		continuationID := strings.TrimSpace(pending.Presentation.AssistantMessageID)
		if continuationID == "" {
			continuationID = state.Foreground.RootAssistantMessageID + "~after~" + string(pending.OperationID)
		}
		state.Foreground.CurrentAssistantMessageID = continuationID
		state.Foreground.AssistantDraft = ""
		state.Foreground.AssistantContent = ""
		state.Foreground.AssistantResult = ""
		state.Foreground.AssistantAttachments = nil
		state.Foreground.Timeline = nil
		state.Foreground.Permission = nil
		state.PendingSteer = nil
		updateOutbox(state, steerEffectID(command.OperationID), OutboxConsumed, "")
		return nil
	}
	if state.Foreground == nil || state.Foreground.OperationID != command.OperationID {
		return errors.New("input consumption does not match foreground operation")
	}
	lane := state.Lanes[state.Foreground.LaneID]
	if command.Thread != nil {
		thread := command.Thread.Normalize()
		if err := thread.Validate(lane.Identity.Realm.ProviderID); err != nil {
			return err
		}
		if lane.Provision == nil || !lane.Provision.Equal(thread) || !lane.Creation.DeferredUntilInput || !lane.Thread.IsZero() {
			return errors.New("input consumption does not match the lane's deferred creation candidate")
		}
		lane.Thread = thread
		lane.Provision = nil
		lane.CreateAfterCandidateAbsence = false
		lane.Phase = LaneRunning
		lane.LastError = ""
		state.Lanes[state.Foreground.LaneID] = lane
	} else if lane.Provision != nil {
		return errors.New("deferred provider input was consumed without a durable thread receipt")
	}
	if lane.InitialSeedPending {
		if err := commitInitialSeedCoverage(state, &lane, state.Foreground.Input); err != nil {
			return err
		}
		lane.InitialSeedPending = false
		state.Lanes[state.Foreground.LaneID] = lane
	}
	if state.Foreground.UserConsumed {
		return nil
	}
	if _, err := appendForegroundUser(state); err != nil {
		return err
	}
	updateOutbox(state, startTurnEffectID(command.OperationID), OutboxConsumed, "")
	return nil
}

func commitInitialSeedCoverage(state *State, lane *LaneState, input QueueEntry) error {
	if state == nil || lane == nil {
		return errors.New("initial context seed lost its lane state")
	}
	if input.InitialSeedDigest == "" {
		if input.InitialSeedFrom != 0 || input.InitialSeedThrough != 0 {
			return errors.New("initial context seed lost its immutable digest")
		}
		return nil
	}
	if input.InitialSeedFrom != lane.CoveredThrough || input.InitialSeedThrough <= input.InitialSeedFrom {
		return errors.New("initial context seed no longer starts at the lane coverage frontier")
	}
	batch, err := buildInitialSeedBatch(*state, input.InitialSeedFrom, input.InitialSeedThrough)
	if err != nil {
		return err
	}
	if batch.Digest != input.InitialSeedDigest {
		return errors.New("initial context seed changed identity before consumption")
	}
	included := make(map[uint64]struct{}, len(batch.Messages))
	for _, message := range batch.Messages {
		included[message.LedgerSequence] = struct{}{}
	}
	for sequence := input.InitialSeedFrom + 1; sequence <= input.InitialSeedThrough; sequence++ {
		if _, exists := lane.Coverage[sequence]; exists {
			continue
		}
		status := CoverageExcluded
		if _, ok := included[sequence]; ok {
			status = CoverageSeeded
		}
		if err := setCoverage(lane, state, sequence, status, input.OperationID); err != nil {
			return err
		}
	}
	return nil
}

func reduceTurnCompleted(state *State, command TurnCompleted) ([]Effect, error) {
	return reduceTurnTerminated(state, TurnTerminated{
		OperationID: command.OperationID, Status: "completed", Assistant: command.Assistant,
	})
}

func reduceTurnTerminated(state *State, command TurnTerminated) ([]Effect, error) {
	if state.Foreground == nil || state.Foreground.OperationID != command.OperationID {
		return nil, errors.New("turn completion does not match foreground operation")
	}
	terminal := cloneTerminal(command.Terminal)
	if terminal == nil {
		terminal = &provider.TerminalEvent{Status: command.Status, StopReason: command.StopReason}
	}
	if terminal.Status == "" {
		terminal.Status = command.Status
	}
	if terminal.StopReason == "" {
		terminal.StopReason = command.StopReason
	}
	status := strings.ToLower(strings.TrimSpace(command.Status))
	if status == "" {
		status = strings.ToLower(strings.TrimSpace(terminal.Status))
	}
	if terminal.Code != nil && *terminal.Code == 130 || strings.EqualFold(strings.TrimSpace(terminal.StopReason), "cancelled") {
		status = "cancelled"
	}
	switch status {
	case "completed", "done", "cancelled", "failed":
	default:
		return nil, fmt.Errorf("unknown provider terminal status %q", command.Status)
	}
	if status == "done" {
		status = "completed"
	}
	terminal.Status = status
	for _, operationID := range terminal.ConsumedSteerIDs {
		if err := consumeTerminalSteerReceipt(state, operationID); err != nil {
			return nil, err
		}
	}
	if !state.Foreground.UserConsumed {
		lane := state.Lanes[state.Foreground.LaneID]
		if lane.Provision == nil {
			if err := reduceInputConsumed(state, InputConsumed{OperationID: command.OperationID}); err != nil {
				return nil, err
			}
		} else if _, err := appendForegroundUser(state); err != nil {
			return nil, err
		}
	}
	if terminal.Result != "" && !state.Foreground.TypedAssistantPhases {
		// Phase-less ACP output is only a compatibility stream. The provider's
		// terminal whole-turn result is canonical and may replace a partial stream.
		state.Foreground.AssistantContent = terminal.Result
		state.Foreground.AssistantResult = ""
	} else if terminal.Result != "" && state.Foreground.AssistantContent == "" && state.Foreground.AssistantResult == "" {
		state.Foreground.AssistantResult = terminal.Result
	}
	if state.Foreground.AssistantContent == "" && state.Foreground.AssistantResult == "" {
		state.Foreground.AssistantContent = firstNonEmptyString(command.Assistant, state.Foreground.AssistantDraft)
	}
	if status == "failed" && state.Foreground.AssistantContent == "" && state.Foreground.AssistantResult == "" {
		if strings.TrimSpace(terminal.Error) != "" {
			state.Foreground.AssistantContent = "Error: " + strings.TrimSpace(terminal.Error)
		} else {
			state.Foreground.AssistantContent = "La tarea falló."
		}
	}
	state.Foreground.AssistantAttachments = mergeProviderAttachments(state.Foreground.AssistantAttachments, terminal.Attachments)
	settleForegroundActivities(state, status, terminal.FinishedAt, command.ObservedAtUnixMS)
	if _, err := appendForegroundAssistantSegment(state, status, true, command.ObservedAtUnixMS); err != nil {
		return nil, err
	}
	assistant := &state.Ledger[len(state.Ledger)-1]
	assistant.Terminal = cloneTerminal(terminal)
	assistant.Interrupted = terminal.Interrupted
	if terminal.Interrupted {
		assistant.RetryPrompt = state.Foreground.Input.Text
	}
	if strings.TrimSpace(terminal.FinishedAt) != "" {
		assistant.At = strings.TrimSpace(terminal.FinishedAt)
	}
	laneID := state.Foreground.LaneID
	lane := state.Lanes[laneID]
	lane.Phase = LaneReady
	lane.LastError = ""
	state.Lanes[laneID] = lane
	updateOutbox(state, startTurnEffectID(command.OperationID), OutboxCompleted, "")
	for index := range state.Outbox {
		entry := &state.Outbox[index]
		if entry.Kind == EffectSteerTurn && entry.Turn == state.Foreground.Turn && entry.Status == OutboxConsumed {
			entry.Status = OutboxCompleted
			entry.LastError = ""
		}
	}
	if state.PendingSteer != nil && state.PendingSteer.Turn == state.Foreground.Turn {
		pending := state.PendingSteer
		if err := appendUnconsumedSteerAtTerminal(state, pending); err != nil {
			return nil, err
		}
		updateOutbox(state, steerEffectID(pending.OperationID), OutboxAmbiguous, provider.ErrorAcceptanceAmbiguous)
		state.PendingSteer = nil
	}
	if state.PendingCancel != nil && state.PendingCancel.Turn == state.Foreground.Turn {
		updateOutbox(state, cancelEffectID(state.PendingCancel.OperationID), OutboxCompleted, "")
		state.PendingCancel = nil
	}
	settleForegroundObligation(state, status, terminal, command.ObservedAtUnixMS)
	state.Foreground = nil
	return drive(state)
}

func shouldPersistForegroundSegment(foreground *ForegroundTurn) bool {
	if foreground == nil {
		return false
	}
	if foreground.CurrentAssistantMessageID == foreground.RootAssistantMessageID {
		return true
	}
	return foreground.AssistantDraft != "" || foreground.AssistantContent != "" || foreground.AssistantResult != "" ||
		len(foreground.AssistantAttachments) != 0 || len(foreground.Timeline) != 0 || foreground.Permission != nil
}

func settleForegroundObligation(state *State, status string, terminal *provider.TerminalEvent, observedAtUnixMS int64) {
	if state == nil || state.Obligation == nil {
		return
	}
	status = strings.ToLower(strings.TrimSpace(status))
	reason := ""
	if terminal != nil {
		reason = strings.ToLower(strings.TrimSpace(terminal.StopReason))
	}
	if status == "cancelled" || reason == "cancelled" || terminalCodeIs(terminal, 130) {
		state.Obligation = nil
		return
	}
	// A daemon-owned interruption says nothing about whether the user's request
	// was fulfilled. Preserve the pre-crash obligation exactly; restart
	// reconciliation may later supply authoritative evidence.
	if terminal != nil && terminal.Interrupted && reason == "daemon-restart" {
		return
	}
	finishedAt := ""
	if terminal != nil {
		finishedAt = strings.TrimSpace(terminal.FinishedAt)
	}
	if finishedAt == "" && observedAtUnixMS > 0 {
		finishedAt = time.UnixMilli(observedAtUnixMS).UTC().Format(time.RFC3339Nano)
	}
	if finishedAt == "" && state.Foreground != nil {
		finishedAt = strings.TrimSpace(state.Foreground.StartedAt)
	}
	if finishedAt == "" {
		finishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	disposition, source, note := "", "", ""
	if terminal != nil {
		disposition = strings.ToLower(strings.TrimSpace(terminal.DispositionState))
		source = strings.TrimSpace(terminal.DispositionSource)
		note = strings.TrimSpace(terminal.DispositionNote)
	}
	if hasUnresolvedPermission(state) {
		disposition, source = "needs_input", "inferred"
	}
	hasBackground := hasLiveBackgroundWork(state)
	switch disposition {
	case "deferred":
		return
	case "needs_input":
		state.Obligation.State = "needs_input"
		state.Obligation.ParkedSince = ""
	case "parked":
		state.Obligation.State = "parked"
		state.Obligation.ParkedSince = finishedAt
	case "done":
		if hasBackground {
			state.Obligation.State = "parked"
			state.Obligation.ParkedSince = finishedAt
			source = "inferred"
		} else {
			state.Obligation.State = "done"
			state.Obligation.ParkedSince = ""
		}
	case "cancelled":
		state.Obligation = nil
		return
	case "":
		switch {
		case status == "failed":
			state.Obligation.State = "needs_input"
			state.Obligation.ParkedSince = ""
		case hasBackground:
			state.Obligation.State = "parked"
			state.Obligation.ParkedSince = finishedAt
		default:
			state.Obligation.State = "done"
			state.Obligation.ParkedSince = ""
		}
		source = "inferred"
	default:
		state.Obligation.State = "needs_input"
		state.Obligation.ParkedSince = ""
		source = "inferred"
		note = "Provider returned an unknown request disposition."
	}
	state.Obligation.Source = source
	state.Obligation.Note = note
	state.Obligation.UpdatedAt = finishedAt
}

func terminalCodeIs(terminal *provider.TerminalEvent, value int) bool {
	return terminal != nil && terminal.Code != nil && *terminal.Code == value
}

func hasUnresolvedPermission(state *State) bool {
	if state == nil {
		return false
	}
	for _, permission := range state.Permissions {
		if strings.TrimSpace(permission.Event.ResolvedOptionID) == "" && !strings.EqualFold(strings.TrimSpace(permission.Event.Status), "resolved") {
			return true
		}
	}
	return false
}

func hasLiveBackgroundWork(state *State) bool {
	if state == nil {
		return false
	}
	for _, background := range state.Background {
		if strings.EqualFold(strings.TrimSpace(background.Event.Status), "running") && !strings.EqualFold(strings.TrimSpace(background.Event.Role), "service") {
			return true
		}
	}
	return false
}

func reduceReconcileObligation(state *State, command ReconcileObligation) error {
	if state == nil || state.Obligation == nil || state.Foreground != nil {
		return nil
	}
	obligation := state.Obligation
	if obligation.State != "working" && obligation.State != "parked" {
		return nil
	}
	if command.LiveEvidence {
		if obligation.State == "working" {
			obligation.State = "parked"
			obligation.ParkedSince = firstNonEmptyString(strings.TrimSpace(obligation.ParkedSince), strings.TrimSpace(command.ObservedAt))
			obligation.UpdatedAt = firstNonEmptyString(strings.TrimSpace(command.ObservedAt), obligation.UpdatedAt)
			obligation.Source = "inferred"
		}
		return nil
	}
	observedAt := strings.TrimSpace(command.ObservedAt)
	if observedAt == "" {
		observedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	shouldStall := obligation.State == "working" && command.DaemonBoot
	if obligation.State == "parked" {
		shouldStall = command.HarnessQuiet
		if !shouldStall {
			parkedAt, err := time.Parse(time.RFC3339Nano, obligation.ParkedSince)
			observed, observedErr := time.Parse(time.RFC3339Nano, observedAt)
			shouldStall = err != nil || observedErr == nil && observed.Sub(parkedAt) >= 90*time.Minute
		}
	}
	if !shouldStall {
		return nil
	}
	previousState := obligation.State
	obligation.State = "stalled"
	obligation.Source = "inferred"
	obligation.ParkedSince = ""
	obligation.UpdatedAt = observedAt
	if command.DaemonBoot && previousState == "working" {
		obligation.Note = "This turn was still running when the daemon restarted, so it never finished."
	} else if command.HarnessQuiet {
		obligation.Note = "Nothing is running and nothing is scheduled to wake this chat."
	} else {
		obligation.Note = "The work this chat parked on is no longer running."
	}
	return nil
}

func consumeTerminalSteerReceipt(state *State, operationID provider.OperationID) error {
	operationID = provider.NormalizeOperationID(string(operationID))
	if operationID == "" {
		return errors.New("terminal steer receipt is missing operation identity")
	}
	if state.PendingSteer != nil && state.PendingSteer.OperationID == operationID {
		return reduceInputConsumed(state, InputConsumed{OperationID: operationID})
	}
	for _, event := range state.Ledger {
		if event.OperationID == operationID && event.Role == "user" && event.SteerState == "applied" {
			return nil
		}
	}
	return errors.New("terminal steer receipt does not match a pending or consumed steer")
}

func appendUnconsumedSteerAtTerminal(state *State, pending *PendingSteer) error {
	if state == nil || state.Foreground == nil || pending == nil {
		return errors.New("terminal steer settlement has no foreground owner")
	}
	messageID := strings.TrimSpace(pending.Presentation.UserMessageID)
	if messageID == "" {
		messageID = fmt.Sprintf("message:%s:user", pending.OperationID)
	}
	steerState := "uncertain"
	if pending.Status == SteerAccepted {
		steerState = "accepted"
	}
	lane := state.Lanes[pending.LaneID]
	event := LedgerEvent{
		EventID: fmt.Sprintf("event:%s:user", pending.OperationID), MessageID: messageID,
		Sequence: state.LedgerHead() + 1, Role: "user", Text: pending.Text, Status: "done",
		At: pending.Presentation.StartedAt, Attachments: append([]provider.Attachment(nil), pending.Attachments...),
		LaneID: pending.LaneID, ProviderID: lane.Identity.Realm.ProviderID, ModelID: state.Foreground.Input.ModelID,
		OperationID: pending.OperationID, QueueID: strings.TrimSpace(pending.Presentation.QueueID),
		NativeTurnID: pending.Turn.NativeID, TerminalState: "unconsumed",
		SteerState: steerState, TurnRootID: state.Foreground.RootAssistantMessageID,
	}
	state.Ledger = append(state.Ledger, event)
	// No consumption receipt means this exact input remains ambiguity-fenced and
	// is never replayed. Mark it excluded for its own lane so the next ordinary
	// turn does not mistake it for a cross-provider import gap and brick a
	// provider that correctly lacks non-sampling context import.
	return markLedgerExcluded(state, event, pending.OperationID)
}

func settleForegroundActivities(state *State, status, finishedAt string, observedAtUnixMS int64) {
	if state == nil || state.Foreground == nil {
		return
	}
	endedAt := observedAtUnixMS
	if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(finishedAt)); err == nil {
		endedAt = parsed.UnixMilli()
	}
	toolStatus := "completed"
	if status == "failed" {
		toolStatus = "failed"
	} else if status == "cancelled" {
		toolStatus = "cancelled"
	}
	activeTool := func(value string) bool {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "", "in_progress", "pending", "running":
			return true
		default:
			return false
		}
	}
	for index := range state.Foreground.Timeline {
		entry := &state.Foreground.Timeline[index]
		if entry.Kind != provider.EventToolUpdate || entry.Tool == nil || !activeTool(entry.Tool.Status) {
			continue
		}
		entry.Tool.Status = toolStatus
		if entry.Tool.EndedAtUnixMS <= 0 {
			entry.Tool.EndedAtUnixMS = endedAt
		}
	}
	for id, permission := range state.Permissions {
		if permission.Owner.LaneID != state.Foreground.LaneID || permission.Owner.TurnID != state.Foreground.Turn.NativeID {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(permission.Event.Status), "resolved") {
			continue
		}
		delete(state.Permissions, id)
	}
	if state.Foreground.Permission != nil && !strings.EqualFold(strings.TrimSpace(state.Foreground.Permission.Status), "resolved") {
		state.Foreground.Permission = nil
	}
}

func reduceProviderEvent(state *State, command ProviderEventReceived) ([]Effect, error) {
	event := command.Event
	if err := event.Validate(); err != nil {
		return nil, err
	}
	if event.Identity.ChatID != state.ChatID {
		return nil, errors.New("provider event belongs to another chat")
	}
	lane, ok := state.Lanes[event.Identity.LaneID]
	if !ok {
		return nil, errors.New("provider event belongs to an unknown lane")
	}
	if command.ConnectionGeneration == 0 || command.ConnectionGeneration != lane.ConnectionGeneration {
		// A detach executor may use HostLost as the synchronous fallback when a
		// provider closes without publishing its receipt. If the provider then
		// delivers the matching LaneDetached event, acknowledge that exact old
		// generation without touching a newer attachment. Other stale events stay
		// hard failures.
		if event.Kind == provider.EventLaneDetached && detachReceiptForGeneration(*state, event.Identity.LaneID, command.ConnectionGeneration) {
			return nil, nil
		}
		return nil, errors.New("provider event belongs to a stale host connection")
	}
	expectedSequence := lane.LastEventSequence + 1
	if event.Identity.Sequence != expectedSequence {
		return nil, fmt.Errorf("provider event sequence is not contiguous: got %d, want %d", event.Identity.Sequence, expectedSequence)
	}
	lane.LastEventSequence = event.Identity.Sequence
	state.Lanes[event.Identity.LaneID] = lane
	owner := ProviderActivityOwner{
		LaneID: event.Identity.LaneID, OperationID: event.Identity.OperationID,
		TurnID: event.Identity.TurnID, ConnectionGeneration: command.ConnectionGeneration,
	}
	switch event.Kind {
	case provider.EventLaneAttached:
		expectedThread := lane.Thread
		if lane.Provision != nil {
			expectedThread = *lane.Provision
		}
		if event.Thread == nil || !expectedThread.Equal(*event.Thread) {
			return nil, errors.New("attached provider event changed the lane thread")
		}
		return nil, nil
	case provider.EventLaneDetached:
		return reduceHostLost(state, HostLost{LaneID: event.Identity.LaneID, ConnectionGeneration: command.ConnectionGeneration})
	case provider.EventLaneCapabilities:
		attachment := event.Attachment.Clone()
		lane.Attachment = &attachment
		state.Lanes[event.Identity.LaneID] = lane
		return nil, nil
	case provider.EventLineageAdvanced:
		return nil, reduceLineageAdvanced(state, LineageAdvanced{
			LaneID: event.Identity.LaneID, ConnectionGeneration: command.ConnectionGeneration,
			From: lane.Thread, To: *event.Thread,
		})
	case provider.EventTurnAdmitted:
		admission := event.Admission
		return nil, reduceTurnAdmitted(state, TurnAdmitted{
			OperationID: admission.Turn.OperationID, Turn: admission.Turn,
			Accepted: admission.Accepted, Ambiguous: !admission.Accepted,
		})
	case provider.EventInputConsumed:
		operationID := provider.NormalizeOperationID(string(event.Input.OperationID))
		if operationID == "" {
			operationID = event.Identity.OperationID
		}
		if event.Identity.OperationID != "" && operationID != event.Identity.OperationID {
			return nil, errors.New("input-consumed event changed operation ownership")
		}
		return nil, reduceInputConsumed(state, InputConsumed{OperationID: operationID, Thread: event.Input.Thread})
	case provider.EventAssistantChunk:
		if state.Foreground == nil || state.Foreground.OperationID != event.Identity.OperationID || state.Foreground.LaneID != event.Identity.LaneID {
			return nil, errors.New("assistant event does not match the foreground owner")
		}
		switch event.Assistant.Phase {
		case provider.AssistantPhaseFinal:
			state.Foreground.AssistantResult += event.Assistant.Text
		case provider.AssistantPhaseContent, provider.AssistantPhaseCommentary, "":
			state.Foreground.AssistantContent += event.Assistant.Text
		default:
			return nil, fmt.Errorf("assistant event has unknown phase %q", event.Assistant.Phase)
		}
		state.Foreground.TypedAssistantPhases = state.Foreground.TypedAssistantPhases || event.Assistant.TypedPhase
		return nil, nil
	case provider.EventAssistantMedia:
		if state.Foreground == nil || state.Foreground.OperationID != event.Identity.OperationID || state.Foreground.LaneID != event.Identity.LaneID {
			return nil, errors.New("assistant media does not match the foreground owner")
		}
		state.Foreground.AssistantAttachments = mergeProviderAttachments(state.Foreground.AssistantAttachments, event.Media.Attachments)
		return nil, nil
	case provider.EventThinkingUpdate:
		if err := requireForegroundEventOwner(state, event); err != nil {
			return nil, err
		}
		upsertThinkingTimeline(state.Foreground, event.Thinking, event.Identity.ObservedAtUnixMS)
		return nil, nil
	case provider.EventToolUpdate:
		id := strings.TrimSpace(event.Tool.ToolCallID)
		if id == "" {
			return nil, errors.New("tool event is missing its provider identity")
		}
		tool := cloneToolEvent(*event.Tool)
		if err := requireForegroundEventOwner(state, event); err != nil {
			return nil, err
		}
		upsertToolTimeline(state.Foreground, &tool, event.Identity.ObservedAtUnixMS)
		// The foreground timeline and then the immutable ledger own the complete
		// tool payload. This side index exists only to correlate later background
		// work by tool-call id, so storing the payload here duplicated large command
		// output in every actor write and made streaming/control latency grow with
		// chat age.
		state.Tools[id] = ToolState{Owner: owner}
		return nil, nil
	case provider.EventPlanUpdate:
		if owner.OperationID == "" {
			return nil, errors.New("plan event is missing operation ownership")
		}
		plan := *event.Plan
		plan.Entries = append([]provider.PlanEntry(nil), event.Plan.Entries...)
		if err := requireForegroundEventOwner(state, event); err != nil {
			return nil, err
		}
		upsertPlanTimeline(state.Foreground, &plan)
		state.Plans[owner.OperationID] = PlanState{Owner: owner, Event: plan}
		state.Presentation.PlanLatest = append([]provider.PlanEntry(nil), plan.Entries...)
		state.Presentation.PlanLatestMessageID = state.Foreground.CurrentAssistantMessageID
		return nil, nil
	case provider.EventPermissionRequested, provider.EventPermissionResolved:
		id := strings.TrimSpace(event.Permission.RequestID)
		if id == "" {
			return nil, errors.New("permission event is missing its provider identity")
		}
		permission := *clonePermission(event.Permission)
		if existing, exists := state.Permissions[id]; exists {
			if existing.Owner.LaneID != owner.LaneID || existing.Owner.TurnID != owner.TurnID {
				return nil, errors.New("permission event changed its provider turn owner")
			}
			owner = existing.Owner
		}
		state.Permissions[id] = PermissionState{Owner: owner, Event: permission}
		if state.Foreground != nil && state.Foreground.LaneID == owner.LaneID && state.Foreground.Turn.NativeID == owner.TurnID {
			state.Foreground.Permission = clonePermission(&permission)
		}
		if event.Kind == provider.EventPermissionResolved {
			for index := range state.Outbox {
				entry := &state.Outbox[index]
				if entry.Kind == EffectPermission && entry.RequestID == id && entry.Status != OutboxFailed && entry.Status != OutboxAmbiguous {
					entry.Status = OutboxCompleted
					entry.LastError = ""
				}
			}
		}
		return nil, nil
	case provider.EventUsageUpdated:
		usage := *event.Usage
		usage.ObservedAtUnixMS = event.Identity.ObservedAtUnixMS
		state.Usage[event.Identity.LaneID] = usage
		return nil, nil
	case provider.EventCompactionStarted, provider.EventCompactionCheckpoint:
		if event.Compaction.Coverage > state.LedgerHead() {
			return nil, errors.New("provider compaction checkpoint is ahead of the visible ledger")
		}
		compaction := *event.Compaction
		if state.Foreground != nil && state.Foreground.LaneID == owner.LaneID && state.Foreground.Turn.NativeID == owner.TurnID {
			upsertCompactionTimeline(state.Foreground, event.Kind, &compaction)
		}
		state.Compactions[event.Identity.LaneID] = CompactionState{Owner: owner, Event: compaction}
		return nil, nil
	case provider.EventBackgroundWork:
		id := strings.TrimSpace(event.Background.WorkID)
		if id == "" {
			return nil, errors.New("background event is missing its provider identity")
		}
		state.Background[id] = BackgroundState{Owner: owner, Event: *event.Background}
		return nil, nil
	case provider.EventTurnTerminal:
		return reduceTurnTerminated(state, TurnTerminated{
			OperationID: event.Identity.OperationID, Status: event.Terminal.Status,
			StopReason: event.Terminal.StopReason, ObservedAtUnixMS: event.Identity.ObservedAtUnixMS,
			Terminal: cloneTerminal(event.Terminal),
		})
	case provider.EventTransportHealth:
		state.Transport[event.Identity.LaneID] = *event.Health
		if event.Health.Error == provider.ErrorProtocolViolation || strings.EqualFold(strings.TrimSpace(event.Health.State), "protocol_failed") {
			lane.Phase = LaneBroken
			lane.LastError = provider.ErrorProtocolViolation
			lane.Attachment = nil
			state.Lanes[event.Identity.LaneID] = lane
			if state.Foreground != nil && state.Foreground.LaneID == event.Identity.LaneID {
				state.Foreground.Status = ForegroundUncertain
				for i := range state.Outbox {
					entry := &state.Outbox[i]
					if entry.OperationID == state.Foreground.OperationID && entry.Status != OutboxCompleted && entry.Status != OutboxFailed {
						entry.Status = OutboxAmbiguous
						entry.LastError = provider.ErrorProtocolViolation
					}
				}
			}
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported normalized provider event %q", event.Kind)
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func pendingDetachForLane(state State, laneID provider.LaneID) bool {
	for _, entry := range state.Outbox {
		if entry.Kind == EffectDetachLane && entry.LaneID == laneID &&
			(entry.Status == OutboxPending || entry.Status == OutboxDispatched) {
			return true
		}
	}
	return false
}

func detachReceiptForGeneration(state State, laneID provider.LaneID, generation uint64) bool {
	if laneID == "" || generation == 0 {
		return false
	}
	for _, entry := range state.Outbox {
		if entry.Kind == EffectDetachLane && entry.LaneID == laneID && entry.Generation == generation &&
			entry.Status == OutboxCompleted {
			return true
		}
	}
	return false
}

func detachEntryForOperation(state State, operationID provider.OperationID) (*OutboxEntry, bool) {
	operationID = provider.NormalizeOperationID(string(operationID))
	for index := range state.Outbox {
		entry := &state.Outbox[index]
		if entry.Kind == EffectDetachLane && entry.OperationID == operationID {
			return entry, true
		}
	}
	return nil, false
}

func reduceDetachLane(state *State, command DetachLane) ([]Effect, error) {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	laneID := provider.LaneID(strings.TrimSpace(string(command.LaneID)))
	connectionID := strings.TrimSpace(command.ConnectionID)
	if operationID == "" || laneID == "" || connectionID == "" || command.ConnectionGeneration == 0 {
		return nil, errors.New("lane detach requires immutable operation, lane, connection, and generation")
	}
	expectedOperationID := DetachOperationID(state.ChatID, laneID, connectionID, command.ConnectionGeneration)
	if operationID != expectedOperationID {
		return nil, errors.New("lane detach operation identity does not match its exact target")
	}
	if existing, ok := detachEntryForOperation(*state, operationID); ok {
		if existing.LaneID != laneID || existing.ConnectionID != connectionID ||
			existing.Generation != command.ConnectionGeneration || existing.Owner != command.Owner {
			return nil, errors.New("lane detach operation was reused for a different attachment")
		}
		return nil, nil
	}
	if _, exists := state.Operations[operationID]; exists {
		return nil, errors.New("lane detach operation id conflicts with another actor operation")
	}
	lane, ok := state.Lanes[laneID]
	if !ok {
		return nil, errors.New("lane detach target is unknown")
	}
	if lane.ConnectionGeneration != command.ConnectionGeneration || lane.Attachment == nil ||
		strings.TrimSpace(lane.Attachment.ConnectionID) != connectionID {
		return nil, errors.New("lane detach target changed")
	}
	if command.Owner != lane.Owner {
		return nil, errors.New("lane detach owner changed")
	}
	if state.Operations == nil {
		state.Operations = make(map[provider.OperationID]struct{})
	}
	state.Operations[operationID] = struct{}{}
	return []Effect{DetachLaneEffect{
		OperationID: operationID, LaneID: laneID, Owner: lane.Owner,
		ConnectionID: connectionID, ConnectionGeneration: command.ConnectionGeneration,
	}}, nil
}

func reduceDetachLaneFailed(state *State, command DetachLaneFailed) error {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	entry, ok := detachEntryForOperation(*state, operationID)
	if !ok {
		return errors.New("lane detach failure has no durable operation")
	}
	if entry.LaneID != command.LaneID || entry.ConnectionID != strings.TrimSpace(command.ConnectionID) ||
		entry.Generation != command.ConnectionGeneration {
		return errors.New("lane detach failure changed its immutable target")
	}
	if entry.Status == OutboxPending {
		return errors.New("lane detach failure arrived before durable dispatch")
	}
	if entry.Status == OutboxCompleted || entry.Status == OutboxFailed || entry.Status == OutboxAmbiguous {
		return nil
	}
	kind := command.Kind
	if kind == "" {
		kind = provider.ErrorTransientTransport
	}
	entry.LastError = kind
	if command.Ambiguous {
		entry.Status = OutboxAmbiguous
	} else {
		entry.Status = OutboxFailed
	}
	return nil
}

func externalMutationEntryForOperation(state State, operationID provider.OperationID) *OutboxEntry {
	operationID = provider.NormalizeOperationID(string(operationID))
	for index := range state.Outbox {
		entry := &state.Outbox[index]
		if entry.Kind == EffectExternalMutation && entry.OperationID == operationID {
			return entry
		}
	}
	return nil
}

func normalizeExternalMutationFields(command RecordExternalMutation) (provider.OperationID, string, string, string, string, error) {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	kind := strings.TrimSpace(command.Kind)
	method := strings.TrimSpace(command.Method)
	tabID := strings.TrimSpace(command.TabID)
	digest := strings.ToLower(strings.TrimSpace(command.Digest))
	if operationID == "" || len(operationID) > 256 || kind == "" || len(kind) > 128 || method == "" || len(method) > 128 || tabID == "" || len(tabID) > 256 {
		return "", "", "", "", "", errors.New("external mutation is missing bounded immutable identity")
	}
	if digest == "" || len(digest) > 256 {
		return "", "", "", "", "", errors.New("external mutation requires a bounded request digest")
	}
	for _, char := range digest {
		if char < 0x21 || char > 0x7e {
			return "", "", "", "", "", errors.New("external mutation request digest is invalid")
		}
	}
	return operationID, kind, method, tabID, digest, nil
}

func reduceRecordExternalMutation(state *State, command RecordExternalMutation) ([]Effect, error) {
	if !state.Initialized || state.Deleted {
		return nil, errors.New("external mutation requires an active initialized chat")
	}
	operationID, kind, method, tabID, digest, err := normalizeExternalMutationFields(command)
	if err != nil {
		return nil, err
	}
	if tabID != strings.TrimSpace(state.Presentation.TabID) {
		return nil, errors.New("external mutation tab attachment is stale")
	}
	if existing := externalMutationEntryForOperation(*state, operationID); existing != nil {
		if existing.ChatID != state.ChatID || existing.MutationKind != kind || existing.MutationMethod != method ||
			existing.TabID != tabID || existing.MutationDigest != digest {
			return nil, errors.New("external mutation operation was reused with a different request")
		}
		return nil, nil
	}
	if _, exists := state.Operations[operationID]; exists {
		return nil, errors.New("external mutation operation id conflicts with another actor operation")
	}
	state.Operations[operationID] = struct{}{}
	return []Effect{ExternalMutationEffect{
		OperationID: operationID, ChatID: state.ChatID, Kind: kind, Method: method, TabID: tabID, Digest: digest,
	}}, nil
}

func reduceExternalMutationReceipt(state *State, command ExternalMutationReceipt) error {
	record := RecordExternalMutation{
		OperationID: command.OperationID, Kind: command.Kind, Method: command.Method,
		TabID: command.TabID, Digest: command.Digest,
	}
	operationID, kind, method, tabID, digest, err := normalizeExternalMutationFields(record)
	if err != nil {
		return err
	}
	if command.Ambiguous && command.Failed {
		return errors.New("external mutation receipt cannot be both failed and ambiguous")
	}
	entry := externalMutationEntryForOperation(*state, operationID)
	if entry == nil {
		return errors.New("external mutation receipt has no durable operation")
	}
	if entry.ChatID != state.ChatID || entry.MutationKind != kind || entry.MutationMethod != method ||
		entry.TabID != tabID || entry.MutationDigest != digest {
		return errors.New("external mutation receipt changed its immutable request")
	}
	if command.Ambiguous {
		if entry.Status == OutboxPending {
			return errors.New("external mutation receipt arrived before durable dispatch")
		}
		if entry.Status == OutboxCompleted || entry.Status == OutboxFailed {
			return errors.New("external mutation receipt conflicts with a terminal result")
		}
		entry.Status = OutboxAmbiguous
		entry.LastError = provider.ErrorAcceptanceAmbiguous
		return nil
	}
	if entry.Status == OutboxPending {
		return errors.New("external mutation receipt arrived before durable dispatch")
	}
	if entry.Status == OutboxCompleted {
		if command.Failed {
			return errors.New("external mutation receipt conflicts with a completed result")
		}
		return nil
	}
	if entry.Status == OutboxFailed {
		if !command.Failed {
			return errors.New("external mutation receipt conflicts with a failed result")
		}
		return nil
	}
	if entry.Status != OutboxDispatched && entry.Status != OutboxAmbiguous {
		return fmt.Errorf("external mutation receipt cannot advance outbox status %q", entry.Status)
	}
	if command.Failed {
		kind := command.ErrorKind
		if kind == "" {
			kind = provider.ErrorAdmissionRejected
		}
		entry.Status = OutboxFailed
		entry.LastError = kind
		return nil
	}
	entry.Status = OutboxCompleted
	entry.LastError = ""
	return nil
}

func reduceHostLost(state *State, command HostLost) ([]Effect, error) {
	lane, ok := state.Lanes[command.LaneID]
	if !ok {
		return nil, errors.New("lost host lane is unknown")
	}
	if command.ConnectionGeneration != lane.ConnectionGeneration {
		return nil, nil
	}
	for index := range state.Outbox {
		entry := &state.Outbox[index]
		if entry.Kind == EffectDetachLane && entry.LaneID == command.LaneID && entry.Generation == command.ConnectionGeneration {
			entry.Status = OutboxCompleted
			entry.LastError = ""
		}
	}
	lane.Attachment = nil
	if lane.Provision != nil {
		if !lane.Thread.IsZero() || !lane.Creation.DeferredUntilInput {
			return nil, errors.New("lost provisional lane has contradictory thread ownership")
		}
		if state.Foreground != nil && state.Foreground.LaneID == command.LaneID && state.Foreground.Turn.NativeID != "" {
			state.Foreground.Status = ForegroundReconciling
		}
		lane.CreateGeneration++
		if lane.CreateGeneration <= lane.ConnectionGeneration {
			lane.CreateGeneration = lane.ConnectionGeneration + 1
		}
		lane.ConnectionGeneration = lane.CreateGeneration
		lane.Phase = LaneCreating
		lane.LastError = ""
		state.Lanes[command.LaneID] = lane
		return []Effect{CreateLaneEffect{
			Identity: lane.Identity, Owner: lane.Owner, CWD: lane.CWD,
			ModelID: lane.ModelID, ModeID: lane.ModeID, Generation: lane.CreateGeneration, Reconcile: true,
			CreateAfterCandidateAbsence: lane.CreateAfterCandidateAbsence,
		}}, nil
	}
	if lane.Thread.IsZero() {
		lane.Phase = LaneBroken
		lane.LastError = provider.ErrorProtocolViolation
		state.Lanes[command.LaneID] = lane
		return nil, nil
	}
	lane.ConnectionGeneration++
	if state.Foreground != nil && state.Foreground.LaneID == command.LaneID {
		state.Foreground.Status = ForegroundReconciling
		lane.Phase = LaneResuming
	} else {
		lane.Phase = LaneDetached
	}
	state.Lanes[command.LaneID] = lane
	if state.Foreground != nil && state.Foreground.LaneID == command.LaneID {
		return []Effect{ResumeLaneEffect{
			Identity: lane.Identity, Thread: lane.Thread, Owner: lane.Owner, CWD: lane.CWD, ModelID: lane.ModelID, ModeID: lane.ModeID,
			Generation: lane.ConnectionGeneration,
		}}, nil
	}
	return nil, nil
}

func reduceLaneProtocolFailed(state *State, command LaneProtocolFailed) error {
	lane, ok := state.Lanes[command.LaneID]
	if !ok {
		return errors.New("protocol failure belongs to an unknown lane")
	}
	if command.ConnectionGeneration != lane.ConnectionGeneration {
		return nil
	}
	lane.Phase = LaneBroken
	lane.LastError = provider.ErrorProtocolViolation
	state.Lanes[command.LaneID] = lane
	if state.Foreground != nil && state.Foreground.LaneID == command.LaneID {
		state.Foreground.Status = ForegroundUncertain
		updateOutbox(state, startTurnEffectID(state.Foreground.OperationID), OutboxAmbiguous, provider.ErrorProtocolViolation)
	}
	return nil
}

func drive(state *State) ([]Effect, error) {
	if state.Foreground != nil {
		return nil, nil
	}
	if state.QueueControl.Paused && len(state.Queue) > 0 {
		return nil, nil
	}
	var target provider.LaneID
	if len(state.Queue) > 0 {
		target = state.Queue[0].LaneID
	} else {
		target = state.DesiredLaneID
	}
	if target == "" {
		return nil, nil
	}
	lane, ok := state.Lanes[target]
	if !ok {
		return nil, errors.New("drive target lane does not exist")
	}
	switch lane.Phase {
	case LaneAbsent:
		if !lane.Thread.IsZero() {
			return nil, errors.New("established lane attempted create transition")
		}
		if lane.Creation.DeferredUntilInput && len(state.Queue) == 0 {
			return nil, nil
		}
		lane.Phase = LaneCreating
		lane.CreateGeneration++
		lane.LastError = ""
		state.Lanes[target] = lane
		return []Effect{CreateLaneEffect{
			Identity: lane.Identity, Owner: lane.Owner, CWD: lane.CWD, ModelID: lane.ModelID, ModeID: lane.ModeID,
			Generation: lane.CreateGeneration, Reconcile: lane.Provision != nil,
			CreateAfterCandidateAbsence: lane.CreateAfterCandidateAbsence,
		}}, nil
	case LaneDetached:
		if lane.Thread.IsZero() {
			return nil, errors.New("detached lane has no native thread")
		}
		lane.Phase = LaneResuming
		lane.ConnectionGeneration++
		state.Lanes[target] = lane
		return []Effect{ResumeLaneEffect{
			Identity: lane.Identity, Thread: lane.Thread, Owner: lane.Owner, CWD: lane.CWD, ModelID: lane.ModelID, ModeID: lane.ModeID,
			Generation: lane.ConnectionGeneration,
		}}, nil
	case LaneReady:
		if lane.CoveredThrough < state.LedgerHead() {
			if lane.InitialSeedPending && lane.Context.ImportMode != provider.ContextImportNonSampling {
				if len(state.Queue) == 0 || state.Queue[0].LaneID != target {
					return nil, nil
				}
				return beginQueuedForeground(state, target, lane, false)
			}
			if lane.Context.ImportMode != provider.ContextImportNonSampling {
				lane.Phase = LaneBlocked
				lane.LastError = provider.ErrorUnsupportedCapability
				state.Lanes[target] = lane
				return nil, nil
			}
			batch, from, to, buildErr := buildContextBatch(*state, lane.CoveredThrough, lane.Context)
			if buildErr != nil {
				lane.Phase = LaneBlocked
				lane.LastError = provider.ErrorContextLimitReached
				state.Lanes[target] = lane
				return nil, nil
			}
			operationID := provider.OperationID(fmt.Sprintf("context-import:%s:v%d:%d:%d:%s", target, batch.ProjectionVersion, from, to, batch.Digest[:16]))
			lane.Phase = LaneImporting
			lane.PendingImport = &PendingImport{OperationID: operationID, From: from, To: to, Batch: batch}
			state.Lanes[target] = lane
			return []Effect{ImportContextEffect{LaneID: target, OperationID: operationID, From: from, To: to, Batch: batch}}, nil
		}
		state.ActiveLaneID = target
		if len(state.Queue) == 0 {
			return nil, nil
		}
		return beginQueuedForeground(state, target, lane, false)
	case LaneCreating:
		if lane.Provision != nil && lane.Creation.DeferredUntilInput && len(state.Queue) > 0 && state.Queue[0].LaneID == target {
			return beginQueuedForeground(state, target, lane, true)
		}
		return nil, nil
	case LaneResuming, LaneImporting, LaneRunning, LaneReconciling, LaneBlocked, LaneBroken:
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown lane phase %q", lane.Phase)
	}
}

func beginQueuedForeground(state *State, target provider.LaneID, lane LaneState, keepCreating bool) ([]Effect, error) {
	if state == nil || state.Foreground != nil || len(state.Queue) == 0 || state.Queue[0].LaneID != target {
		return nil, errors.New("queued foreground start lost its lane or FIFO owner")
	}
	input := state.Queue[0]
	seed := ContextBatch{}
	if lane.InitialSeedPending && lane.CoveredThrough < state.LedgerHead() {
		var err error
		seed, err = buildInitialSeedBatch(*state, lane.CoveredThrough, state.LedgerHead())
		if err != nil {
			return nil, err
		}
		input.InitialSeedFrom = lane.CoveredThrough
		input.InitialSeedThrough = state.LedgerHead()
		input.InitialSeedDigest = seed.Digest
	}
	state.Queue = append([]QueueEntry(nil), state.Queue[1:]...)
	rootAssistantID := strings.TrimSpace(input.Presentation.AssistantMessageID)
	if rootAssistantID == "" {
		rootAssistantID = fmt.Sprintf("message:%s:assistant", input.OperationID)
	}
	if planEntriesAllCompleted(state.Presentation.PlanLatest) {
		state.Presentation.PlanLatest = nil
		state.Presentation.PlanLatestMessageID = rootAssistantID
	}
	state.Foreground = &ForegroundTurn{
		OperationID: input.OperationID, LaneID: target, Input: input, Status: ForegroundDispatching,
		StartedAt: input.Presentation.StartedAt, RootAssistantMessageID: rootAssistantID,
		CurrentAssistantMessageID: rootAssistantID,
	}
	beginForegroundObligation(state, input)
	state.ActiveLaneID = target
	if keepCreating {
		lane.Phase = LaneCreating
	} else {
		lane.Phase = LaneRunning
	}
	state.Lanes[target] = lane
	return []Effect{StartTurnEffect{LaneID: target, Input: input, Seed: seed}}, nil
}

func planEntriesAllCompleted(entries []provider.PlanEntry) bool {
	if len(entries) == 0 {
		return false
	}
	for _, entry := range entries {
		if strings.TrimSpace(entry.Status) != "completed" {
			return false
		}
	}
	return true
}

func beginForegroundObligation(state *State, input QueueEntry) {
	if state == nil {
		return
	}
	startedAt := strings.TrimSpace(input.Presentation.StartedAt)
	if startedAt == "" {
		startedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	origin := strings.ToLower(strings.TrimSpace(input.Presentation.Origin))
	if origin == "" || origin == "human" || state.Obligation == nil {
		promptID := strings.TrimSpace(input.Presentation.UserMessageID)
		if promptID == "" {
			promptID = string(input.OperationID)
		}
		state.Obligation = &ObligationState{
			State: "working", OpenedAt: startedAt, UpdatedAt: startedAt, PromptID: promptID,
		}
	} else {
		state.Obligation.State = "working"
		state.Obligation.Source = ""
		state.Obligation.Note = ""
		state.Obligation.UpdatedAt = startedAt
		state.Obligation.ParkedSince = ""
	}
	if state.Presentation.Settled != "" || state.Presentation.SettledAt != 0 {
		state.Presentation.Settled = ""
		state.Presentation.SettledAt = 0
		state.Presentation.PresentationRevision++
	}
}

func appendForegroundUser(state *State) (LedgerEvent, error) {
	if state == nil || state.Foreground == nil {
		return LedgerEvent{}, errors.New("foreground user has no turn owner")
	}
	foreground := state.Foreground
	messageID := strings.TrimSpace(foreground.Input.Presentation.UserMessageID)
	if messageID == "" {
		messageID = fmt.Sprintf("message:%s:user", foreground.OperationID)
	}
	lane := state.Lanes[foreground.LaneID]
	event := LedgerEvent{
		EventID: fmt.Sprintf("event:%s:user", foreground.OperationID), MessageID: messageID,
		Sequence: state.LedgerHead() + 1, Role: "user", Text: foreground.Input.Text, Status: "done",
		At: foreground.StartedAt, Attachments: append([]provider.Attachment(nil), foreground.Input.Attachments...),
		LaneID: foreground.LaneID, ProviderID: lane.Identity.Realm.ProviderID, ModelID: foreground.Input.ModelID,
		OperationID: foreground.OperationID, QueueID: strings.TrimSpace(foreground.Input.Presentation.QueueID),
		NativeTurnID: foreground.Turn.NativeID, TerminalState: "consumed",
	}
	state.Ledger = append(state.Ledger, event)
	foreground.UserConsumed = true
	if err := markLedgerNativeSeen(state, event, foreground.OperationID); err != nil {
		return LedgerEvent{}, err
	}
	return event, nil
}

// appendForegroundAdmissionFailure materializes the optimistic public turn in
// the authoritative ledger after the provider has definitively rejected
// admission. The provider did not consume either row, so the origin lane marks
// them excluded instead of claiming native coverage. A different provider may
// still import the visible semantic failure through its own verified context
// transaction.
func appendForegroundAdmissionFailure(state *State, kind provider.ErrorKind) error {
	if state == nil || state.Foreground == nil {
		return errors.New("admission failure has no foreground owner")
	}
	foreground := state.Foreground
	lane, ok := state.Lanes[foreground.LaneID]
	if !ok {
		return errors.New("admission failure belongs to an unknown lane")
	}
	userID := strings.TrimSpace(foreground.Input.Presentation.UserMessageID)
	if userID == "" {
		userID = fmt.Sprintf("message:%s:user", foreground.OperationID)
	}
	user := LedgerEvent{
		EventID: fmt.Sprintf("event:%s:user", foreground.OperationID), MessageID: userID,
		Sequence: state.LedgerHead() + 1, Role: "user", Text: foreground.Input.Text, Status: "done",
		At: foreground.StartedAt, Attachments: append([]provider.Attachment(nil), foreground.Input.Attachments...),
		LaneID: foreground.LaneID, ProviderID: lane.Identity.Realm.ProviderID, ModelID: foreground.Input.ModelID,
		OperationID: foreground.OperationID, QueueID: strings.TrimSpace(foreground.Input.Presentation.QueueID),
		TerminalState: "admission_rejected",
	}
	state.Ledger = append(state.Ledger, user)
	if err := markLedgerExcluded(state, user, foreground.OperationID); err != nil {
		return err
	}

	assistantID := strings.TrimSpace(foreground.CurrentAssistantMessageID)
	if assistantID == "" {
		assistantID = strings.TrimSpace(foreground.Input.Presentation.AssistantMessageID)
	}
	if assistantID == "" {
		assistantID = fmt.Sprintf("message:%s:assistant", foreground.OperationID)
	}
	errorText := "The provider could not start this turn."
	if kind != "" {
		errorText += " " + string(kind) + "."
	}
	terminal := &provider.TerminalEvent{Status: "failed", Error: string(kind), Interrupted: true}
	assistant := LedgerEvent{
		EventID: fmt.Sprintf("event:%s:assistant:%s", foreground.OperationID, assistantID), MessageID: assistantID,
		Sequence: state.LedgerHead() + 1, Role: "assistant", Text: errorText, Status: "failed",
		At: foreground.StartedAt, LaneID: foreground.LaneID, ProviderID: lane.Identity.Realm.ProviderID,
		ModelID: foreground.Input.ModelID, OperationID: foreground.OperationID, TerminalState: "failed",
		Interrupted: true, RetryPrompt: foreground.Input.Text, Terminal: terminal,
	}
	state.Ledger = append(state.Ledger, assistant)
	return markLedgerExcluded(state, assistant, foreground.OperationID)
}

func appendConsumedSteer(state *State, pending *PendingSteer) (LedgerEvent, error) {
	if state == nil || state.Foreground == nil || pending == nil {
		return LedgerEvent{}, errors.New("consumed steer has no turn owner")
	}
	messageID := strings.TrimSpace(pending.Presentation.UserMessageID)
	if messageID == "" {
		messageID = fmt.Sprintf("message:%s:user", pending.OperationID)
	}
	lane := state.Lanes[pending.LaneID]
	event := LedgerEvent{
		EventID: fmt.Sprintf("event:%s:user", pending.OperationID), MessageID: messageID,
		Sequence: state.LedgerHead() + 1, Role: "user", Text: pending.Text, Status: "done",
		At: pending.Presentation.StartedAt, Attachments: append([]provider.Attachment(nil), pending.Attachments...),
		LaneID: pending.LaneID, ProviderID: lane.Identity.Realm.ProviderID, ModelID: state.Foreground.Input.ModelID,
		OperationID: pending.OperationID, QueueID: strings.TrimSpace(pending.Presentation.QueueID),
		NativeTurnID: pending.Turn.NativeID, TerminalState: "consumed",
		SteerState: "applied", TurnRootID: state.Foreground.RootAssistantMessageID,
	}
	state.Ledger = append(state.Ledger, event)
	if err := markLedgerNativeSeen(state, event, pending.OperationID); err != nil {
		return LedgerEvent{}, err
	}
	return event, nil
}

func appendForegroundAssistantSegment(state *State, terminalState string, terminal bool, observedAtUnixMS int64) (LedgerEvent, error) {
	if state == nil || state.Foreground == nil {
		return LedgerEvent{}, errors.New("assistant segment has no foreground owner")
	}
	foreground := state.Foreground
	messageID := strings.TrimSpace(foreground.CurrentAssistantMessageID)
	if messageID == "" {
		return LedgerEvent{}, errors.New("assistant segment is missing its public identity")
	}
	lane := state.Lanes[foreground.LaneID]
	status := "done"
	switch strings.ToLower(strings.TrimSpace(terminalState)) {
	case "cancelled":
		status = "cancelled"
	case "failed":
		status = "failed"
	}
	segmented := !terminal || messageID != foreground.RootAssistantMessageID
	if !segmented {
		for _, prior := range state.Ledger {
			if prior.Role == "assistant" && prior.TurnRootID == foreground.RootAssistantMessageID {
				segmented = true
				break
			}
		}
	}
	event := LedgerEvent{
		EventID: fmt.Sprintf("event:%s:assistant:%s", foreground.OperationID, messageID), MessageID: messageID,
		Sequence: state.LedgerHead() + 1, Role: "assistant", Text: foreground.AssistantContent,
		Result: foreground.AssistantResult, Status: status, Attachments: append([]provider.Attachment(nil), foreground.AssistantAttachments...),
		LaneID: foreground.LaneID, ProviderID: lane.Identity.Realm.ProviderID, ModelID: foreground.Input.ModelID,
		OperationID: foreground.OperationID, NativeTurnID: foreground.Turn.NativeID, TerminalState: terminalState,
		Timeline: cloneTimeline(foreground.Timeline),
	}
	if terminal {
		event.At = unixMSRFC3339(observedAtUnixMS)
		event.Permission = clonePermission(foreground.Permission)
	}
	if segmented {
		event.TurnRootID = foreground.RootAssistantMessageID
		terminalCopy := terminal
		event.TurnTerminal = &terminalCopy
	}
	state.Ledger = append(state.Ledger, event)
	if err := markLedgerNativeSeen(state, event, foreground.OperationID); err != nil {
		return LedgerEvent{}, err
	}
	return event, nil
}

func markLedgerNativeSeen(state *State, event LedgerEvent, operationID provider.OperationID) error {
	lane := state.Lanes[event.LaneID]
	if err := setCoverage(&lane, state, event.Sequence, CoverageNativeSeen, operationID); err != nil {
		return err
	}
	state.Lanes[event.LaneID] = lane
	return nil
}

func markLedgerExcluded(state *State, event LedgerEvent, operationID provider.OperationID) error {
	lane := state.Lanes[event.LaneID]
	if err := setCoverage(&lane, state, event.Sequence, CoverageExcluded, operationID); err != nil {
		return err
	}
	state.Lanes[event.LaneID] = lane
	return nil
}

func unixMSRFC3339(value int64) string {
	if value <= 0 {
		return ""
	}
	return time.UnixMilli(value).UTC().Format(time.RFC3339Nano)
}

func foregroundUTF16Offset(foreground *ForegroundTurn) int {
	if foreground == nil {
		return 0
	}
	return len(utf16.Encode([]rune(foreground.AssistantContent)))
}

func requireForegroundEventOwner(state *State, event provider.Event) error {
	if state == nil || state.Foreground == nil || state.Foreground.LaneID != event.Identity.LaneID {
		return errors.New("provider timeline event does not match the foreground lane")
	}
	if event.Identity.OperationID != "" && state.Foreground.OperationID != event.Identity.OperationID {
		return errors.New("provider timeline event changed operation ownership")
	}
	if event.Identity.TurnID != "" && state.Foreground.Turn.NativeID != "" && state.Foreground.Turn.NativeID != event.Identity.TurnID {
		return errors.New("provider timeline event changed native turn ownership")
	}
	return nil
}

func mergeProviderAttachments(current, incoming []provider.Attachment) []provider.Attachment {
	out := append([]provider.Attachment(nil), current...)
	seen := make(map[string]struct{}, len(out))
	key := func(attachment provider.Attachment) string {
		return firstNonEmptyString(strings.TrimSpace(attachment.Ref), strings.TrimSpace(attachment.Digest), strings.TrimSpace(attachment.ID))
	}
	for _, attachment := range out {
		if id := key(attachment); id != "" {
			seen[id] = struct{}{}
		}
	}
	for _, attachment := range incoming {
		id := key(attachment)
		if id != "" {
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
		}
		out = append(out, attachment)
	}
	return out
}

func timelineIndex(entries []TimelineEntry, key string) int {
	for index := range entries {
		if entries[index].Key == key {
			return index
		}
	}
	return -1
}

func upsertThinkingTimeline(foreground *ForegroundTurn, incoming *provider.ThinkingEvent, observedAtUnixMS int64) {
	if foreground == nil || incoming == nil {
		return
	}
	text := strings.TrimSpace(incoming.Text)
	if text == "" {
		return
	}
	key := "timeline:thinking:" + foreground.CurrentAssistantMessageID
	index := timelineIndex(foreground.Timeline, key)
	if index < 0 {
		value := *incoming
		value.Text = text
		foreground.Timeline = append(foreground.Timeline, TimelineEntry{
			Key: key, At: foregroundUTF16Offset(foreground), Kind: provider.EventThinkingUpdate, Thinking: &value,
		})
		return
	}
	existing := foreground.Timeline[index].Thinking
	if existing == nil {
		return
	}
	for _, line := range strings.Split(existing.Text, "\n") {
		if line == text {
			return
		}
	}
	if existing.Text == "" {
		existing.Text = text
	} else {
		existing.Text += "\n" + text
	}
}

func upsertPlanTimeline(foreground *ForegroundTurn, incoming *provider.PlanEvent) {
	if foreground == nil || incoming == nil {
		return
	}
	value := *incoming
	value.Entries = append([]provider.PlanEntry(nil), incoming.Entries...)
	key := "timeline:plan:" + foreground.CurrentAssistantMessageID
	index := timelineIndex(foreground.Timeline, key)
	if index < 0 {
		foreground.Timeline = append(foreground.Timeline, TimelineEntry{
			Key: key, At: foregroundUTF16Offset(foreground), Kind: provider.EventPlanUpdate, Plan: &value,
		})
		return
	}
	foreground.Timeline[index].Plan = &value
}

func cloneToolEvent(in provider.ToolEvent) provider.ToolEvent {
	in.Attachments = append([]provider.Attachment(nil), in.Attachments...)
	return in
}

func terminalToolStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "done", "completed", "complete", "failed", "error", "cancelled", "canceled", "rejected":
		return true
	default:
		return false
	}
}

func mergeToolEvent(current, incoming provider.ToolEvent) provider.ToolEvent {
	out := cloneToolEvent(current)
	assign := func(target *string, value string) {
		if value != "" {
			*target = value
		}
	}
	assign(&out.ToolCallID, incoming.ToolCallID)
	assign(&out.ToolKind, incoming.ToolKind)
	assign(&out.Title, incoming.Title)
	assign(&out.Status, incoming.Status)
	assign(&out.Command, incoming.Command)
	assign(&out.TerminalID, incoming.TerminalID)
	assign(&out.Input, incoming.Input)
	assign(&out.Output, incoming.Output)
	assign(&out.Location, incoming.Location)
	assign(&out.SubagentID, incoming.SubagentID)
	assign(&out.SubagentLabel, incoming.SubagentLabel)
	assign(&out.SubagentProvider, incoming.SubagentProvider)
	assign(&out.SubagentModel, incoming.SubagentModel)
	if incoming.SubagentHeader {
		out.SubagentHeader = true
	}
	out.Attachments = mergeProviderAttachments(out.Attachments, incoming.Attachments)
	if out.StartedAtUnixMS == 0 {
		out.StartedAtUnixMS = incoming.StartedAtUnixMS
	}
	if incoming.EndedAtUnixMS > 0 {
		out.EndedAtUnixMS = incoming.EndedAtUnixMS
	}
	return out
}

func upsertToolTimeline(foreground *ForegroundTurn, incoming *provider.ToolEvent, observedAtUnixMS int64) {
	if foreground == nil || incoming == nil {
		return
	}
	value := cloneToolEvent(*incoming)
	if value.StartedAtUnixMS <= 0 {
		value.StartedAtUnixMS = observedAtUnixMS
	}
	if terminalToolStatus(value.Status) && value.EndedAtUnixMS <= 0 {
		value.EndedAtUnixMS = observedAtUnixMS
	}
	key := "timeline:tool:" + strings.TrimSpace(value.ToolCallID)
	index := timelineIndex(foreground.Timeline, key)
	if index < 0 {
		foreground.Timeline = append(foreground.Timeline, TimelineEntry{
			Key: key, At: foregroundUTF16Offset(foreground), Kind: provider.EventToolUpdate, Tool: &value,
		})
		return
	}
	merged := mergeToolEvent(*foreground.Timeline[index].Tool, value)
	if terminalToolStatus(merged.Status) && merged.EndedAtUnixMS <= 0 {
		merged.EndedAtUnixMS = observedAtUnixMS
	}
	foreground.Timeline[index].Tool = &merged
}

func upsertCompactionTimeline(foreground *ForegroundTurn, kind provider.EventKind, incoming *provider.CompactionEvent) {
	if foreground == nil || incoming == nil || kind != provider.EventCompactionCheckpoint {
		return
	}
	value := *incoming
	key := "timeline:compaction:" + foreground.CurrentAssistantMessageID
	index := timelineIndex(foreground.Timeline, key)
	if index < 0 {
		foreground.Timeline = append(foreground.Timeline, TimelineEntry{
			Key: key, At: foregroundUTF16Offset(foreground), Kind: kind, Compaction: &value,
		})
		return
	}
	foreground.Timeline[index].Kind = kind
	foreground.Timeline[index].Compaction = &value
}

func reduceRestoreCheckpoint(state *State, command RestoreCheckpoint) ([]Effect, error) {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	checkpoint := append(json.RawMessage(nil), command.Checkpoint...)
	checkpointDigest := strings.ToLower(strings.TrimSpace(command.CheckpointDigest))
	if operationID == "" || command.TurnSequence <= 0 || command.ObservedAtUnixMS <= 0 || len(checkpoint) == 0 || checkpointDigest == "" {
		return nil, errors.New("checkpoint restore requires stable operation, turn, and observed time")
	}
	for i := range state.Outbox {
		entry := state.Outbox[i]
		if entry.OperationID != operationID {
			continue
		}
		if entry.Kind != EffectCheckpointRestore || entry.TurnSequence != command.TurnSequence ||
			entry.CheckpointDigest != checkpointDigest || !bytes.Equal(entry.Checkpoint, checkpoint) {
			return nil, errors.New("checkpoint restore operation conflicts with its durable request")
		}
		return nil, nil
	}
	if _, exists := state.Operations[operationID]; exists {
		return nil, errors.New("checkpoint restore operation conflicts with another actor command")
	}
	if state.Foreground != nil {
		return nil, errors.New("checkpoint restore is unavailable while the chat has foreground work")
	}
	foundAssistant := false
	for i := len(state.Ledger) - 1; i >= 0; i-- {
		if state.Ledger[i].Role == "assistant" {
			foundAssistant = true
			break
		}
	}
	if !foundAssistant {
		return nil, errors.New("checkpoint restore requires a completed assistant turn")
	}
	observedCheckpoint, observedDigest, err := checkpointRestoreTarget(state.Environment.Checkpoints, command.TurnSequence)
	if err != nil {
		return nil, err
	}
	if observedDigest != checkpointDigest || !bytes.Equal(observedCheckpoint, checkpoint) {
		return nil, errors.New("checkpoint restore target is stale or was not observed by the actor")
	}
	state.Operations[operationID] = struct{}{}
	return []Effect{RestoreCheckpointEffect{
		OperationID: operationID, TurnSequence: command.TurnSequence, ObservedAtUnixMS: command.ObservedAtUnixMS,
		Checkpoint: checkpoint, CheckpointDigest: checkpointDigest,
	}}, nil
}

// checkpointRestoreTarget returns the exact JSON object observed in the
// actor-owned checkpoint list. The raw object, rather than a re-marshaled
// manager struct, is the immutable filesystem target for a restore effect.
func checkpointRestoreTarget(checkpoints json.RawMessage, turnSequence int) (json.RawMessage, string, error) {
	if turnSequence <= 0 || len(checkpoints) == 0 || !json.Valid(checkpoints) {
		return nil, "", errors.New("checkpoint restore requires an actor-observed checkpoint")
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(checkpoints, &entries); err != nil {
		return nil, "", errors.New("actor checkpoint observation is not a JSON list")
	}
	var target json.RawMessage
	for _, entry := range entries {
		var identity struct {
			TurnSequence int `json:"turnSeq"`
		}
		if err := json.Unmarshal(entry, &identity); err != nil {
			return nil, "", errors.New("actor checkpoint observation contains invalid JSON")
		}
		if identity.TurnSequence != turnSequence {
			continue
		}
		if len(target) != 0 {
			return nil, "", errors.New("actor checkpoint observation has colliding turn sequence")
		}
		target = append(json.RawMessage(nil), entry...)
	}
	if len(target) == 0 {
		return nil, "", fmt.Errorf("checkpoint turn %d was not observed by the actor", turnSequence)
	}
	sum := sha256.Sum256(target)
	return target, hex.EncodeToString(sum[:]), nil
}

func validateCheckpointRestorePayload(payload json.RawMessage, turnSequence int, digest string) error {
	if len(payload) == 0 || !json.Valid(payload) || turnSequence <= 0 {
		return errors.New("checkpoint restore payload is invalid")
	}
	var identity struct {
		TurnSequence int `json:"turnSeq"`
	}
	if err := json.Unmarshal(payload, &identity); err != nil || identity.TurnSequence != turnSequence {
		return errors.New("checkpoint restore payload has the wrong turn sequence")
	}
	digest = strings.ToLower(strings.TrimSpace(digest))
	if len(digest) != sha256.Size*2 {
		return errors.New("checkpoint restore payload digest is invalid")
	}
	sum := sha256.Sum256(payload)
	if hex.EncodeToString(sum[:]) != digest {
		return errors.New("checkpoint restore payload digest does not match its bytes")
	}
	return nil
}

func reduceCheckpointRestored(state *State, command CheckpointRestored) error {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	for i := range state.Outbox {
		entry := &state.Outbox[i]
		if entry.Kind != EffectCheckpointRestore || entry.OperationID != operationID {
			continue
		}
		if command.TurnSequence != entry.TurnSequence {
			return errors.New("checkpoint restore receipt changed its immutable turn")
		}
		if entry.Status == OutboxCompleted {
			if len(command.Result) > 0 && !bytes.Equal(command.Result, entry.Result) {
				return errors.New("checkpoint restore receipt conflicts with its durable result")
			}
			return nil
		}
		if entry.Status != OutboxDispatched {
			return fmt.Errorf("checkpoint restore cannot complete from %q", entry.Status)
		}
		if len(command.Result) == 0 || !json.Valid(command.Result) {
			return errors.New("checkpoint restore receipt is not valid json")
		}
		var target *LedgerEvent
		for index := len(state.Ledger) - 1; index >= 0; index-- {
			if state.Ledger[index].Role == "assistant" {
				target = &state.Ledger[index]
				break
			}
		}
		if target == nil {
			return errors.New("checkpoint restore lost its completed assistant owner")
		}
		key := "timeline:checkpoint-restore:" + string(operationID)
		if timelineIndex(target.Timeline, key) < 0 {
			target.Timeline = append(target.Timeline, TimelineEntry{
				Key: key, At: len(utf16.Encode([]rune(target.Text))), Kind: provider.EventCheckpointRestored,
				Restored: &provider.CheckpointRestoredEvent{TurnSequence: command.TurnSequence},
			})
		}
		entry.Status = OutboxCompleted
		entry.LastError = ""
		entry.Result = append(json.RawMessage(nil), command.Result...)
		return nil
	}
	return errors.New("checkpoint restore receipt has no durable operation")
}

func reduceCheckpointRestoreFailed(state *State, command CheckpointRestoreFailed) error {
	operationID := provider.NormalizeOperationID(string(command.OperationID))
	kind := command.Kind
	if kind == "" {
		kind = provider.ErrorAdmissionRejected
	}
	for i := range state.Outbox {
		entry := &state.Outbox[i]
		if entry.Kind != EffectCheckpointRestore || entry.OperationID != operationID {
			continue
		}
		if entry.Status == OutboxCompleted {
			return nil
		}
		entry.LastError = kind
		if command.Ambiguous {
			entry.Status = OutboxAmbiguous
		} else {
			entry.Status = OutboxFailed
		}
		return nil
	}
	return errors.New("checkpoint restore failure has no durable operation")
}

const semanticProjectionVersion uint32 = 1

const (
	initialSeedMaxEvents = 512
	initialSeedMaxBytes  = 120000
)

// buildInitialSeedBatch projects the newest bounded tail of an immutable
// Workass ledger range for the first real input of a never-used provider lane.
// The range identity covers every event through `through`; events omitted by
// the byte/event bound are later recorded as excluded rather than falsely
// claiming the provider saw them.
func buildInitialSeedBatch(state State, from, through uint64) (ContextBatch, error) {
	if from > through || through > state.LedgerHead() {
		return ContextBatch{}, errors.New("initial context seed range is outside the semantic ledger")
	}
	messages := make([]provider.ContextMessage, 0, initialSeedMaxEvents)
	eventIDs := make([]string, 0, initialSeedMaxEvents)
	for sequence := through; sequence > from && len(messages) < initialSeedMaxEvents; sequence-- {
		event := state.Ledger[sequence-1]
		if event.ContextExcluded {
			continue
		}
		message := contextMessageForLedgerEvent(event)
		candidateMessages := append([]provider.ContextMessage{message}, messages...)
		candidateIDs := append([]string{event.EventID}, eventIDs...)
		payload := struct {
			Version  uint32
			From     uint64
			Through  uint64
			EventIDs []string
			Messages []provider.ContextMessage
		}{semanticProjectionVersion, from, through, candidateIDs, candidateMessages}
		raw, err := json.Marshal(payload)
		if err != nil {
			return ContextBatch{}, err
		}
		if len(raw) > initialSeedMaxBytes {
			continue
		}
		messages = candidateMessages
		eventIDs = candidateIDs
	}
	payload := struct {
		Version  uint32
		From     uint64
		Through  uint64
		EventIDs []string
		Messages []provider.ContextMessage
	}{semanticProjectionVersion, from, through, eventIDs, messages}
	raw, err := json.Marshal(payload)
	if err != nil {
		return ContextBatch{}, err
	}
	digest := sha256.Sum256(raw)
	return ContextBatch{
		ProjectionVersion: semanticProjectionVersion,
		EventIDs:          eventIDs,
		Digest:            hex.EncodeToString(digest[:]),
		Messages:          messages,
	}, nil
}

func contextMessageForLedgerEvent(event LedgerEvent) provider.ContextMessage {
	return provider.ContextMessage{
		EventID: event.EventID, LedgerSequence: event.Sequence, Role: event.Role, Text: event.Text, Result: event.Result,
		Attachments:  append([]provider.Attachment(nil), event.Attachments...),
		SourceLaneID: event.LaneID, SourceProvider: event.ProviderID, SourceModelID: event.ModelID,
		OperationID: event.OperationID, NativeTurnID: event.NativeTurnID, TerminalStatus: event.TerminalState,
		Inert: true,
	}
}

func buildContextBatch(state State, coveredThrough uint64, capabilities provider.ContextCapabilities) (ContextBatch, uint64, uint64, error) {
	if capabilities.MaxImportEvents <= 0 || capabilities.MaxImportBytes <= 0 {
		return ContextBatch{}, 0, 0, errors.New("provider did not advertise bounded context-import limits")
	}
	from := coveredThrough
	if from >= state.LedgerHead() {
		return ContextBatch{}, 0, 0, errors.New("context import has no uncovered ledger events")
	}
	messages := make([]provider.ContextMessage, 0, capabilities.MaxImportEvents)
	eventIDs := make([]string, 0, capabilities.MaxImportEvents)
	to := from
	for sequence := from + 1; sequence <= state.LedgerHead() && len(messages) < capabilities.MaxImportEvents; sequence++ {
		event := state.Ledger[sequence-1]
		message := contextMessageForLedgerEvent(event)
		candidate := append(append([]provider.ContextMessage(nil), messages...), message)
		raw, err := json.Marshal(candidate)
		if err != nil {
			return ContextBatch{}, 0, 0, err
		}
		if len(raw) > capabilities.MaxImportBytes {
			if len(messages) == 0 {
				return ContextBatch{}, 0, 0, errors.New("one semantic ledger event exceeds the provider import budget")
			}
			break
		}
		messages = candidate
		eventIDs = append(eventIDs, event.EventID)
		to = sequence
	}
	payload := struct {
		Version  uint32
		EventIDs []string
		Messages []provider.ContextMessage
	}{Version: semanticProjectionVersion, EventIDs: eventIDs, Messages: messages}
	raw, err := json.Marshal(payload)
	if err != nil {
		return ContextBatch{}, 0, 0, err
	}
	sum := sha256.Sum256(raw)
	return ContextBatch{
		ProjectionVersion: semanticProjectionVersion,
		EventIDs:          eventIDs,
		Digest:            hex.EncodeToString(sum[:]),
		Messages:          messages,
	}, from, to, nil
}

func setCoverage(lane *LaneState, state *State, sequence uint64, status CoverageStatus, deliveryID provider.OperationID) error {
	if sequence == 0 || sequence > state.LedgerHead() {
		return fmt.Errorf("coverage receipt references ledger sequence %d beyond head %d", sequence, state.LedgerHead())
	}
	if lane.Coverage == nil {
		lane.Coverage = make(map[uint64]CoverageRecord)
	}
	record := CoverageRecord{
		Sequence: sequence, EventID: state.Ledger[sequence-1].EventID, Status: status, DeliveryID: deliveryID,
	}
	if previous, exists := lane.Coverage[sequence]; exists && previous != record {
		return errors.New("coverage receipt conflicts with an existing immutable classification")
	}
	lane.Coverage[sequence] = record
	for next := lane.CoveredThrough + 1; ; next++ {
		if _, ok := lane.Coverage[next]; !ok {
			break
		}
		lane.CoveredThrough = next
	}
	return nil
}

func createEffectID(laneID provider.LaneID, generation uint64) string {
	return fmt.Sprintf("lane-create:%s:%d", laneID, generation)
}

func resumeEffectID(laneID provider.LaneID, generation uint64) string {
	return fmt.Sprintf("lane-resume:%s:%d", laneID, generation)
}

func startTurnEffectID(operationID provider.OperationID) string {
	return "turn-start:" + string(operationID)
}

func reconcileTurnEffectID(operationID provider.OperationID) string {
	return "turn-reconcile:" + string(operationID)
}

func steerEffectID(operationID provider.OperationID) string {
	return "turn-steer:" + string(operationID)
}

func cancelEffectID(operationID provider.OperationID) string {
	return "turn-cancel:" + string(operationID)
}

func permissionEffectID(operationID provider.OperationID) string {
	return "permission-resolve:" + string(operationID)
}

func backgroundEffectID(operationID provider.OperationID) string {
	return "background:" + string(provider.NormalizeOperationID(string(operationID)))
}

func deleteChatEffectID(chatID string) string {
	return "chat-delete:" + strings.TrimSpace(chatID)
}

func checkpointRestoreEffectID(operationID provider.OperationID) string {
	return "checkpoint-restore:" + string(provider.NormalizeOperationID(string(operationID)))
}

func externalMutationEffectID(operationID provider.OperationID) string {
	return "external-mutation:" + string(provider.NormalizeOperationID(string(operationID)))
}

func outboxEntryForEffect(effect Effect) (OutboxEntry, bool, error) {
	switch effect := effect.(type) {
	case CreateLaneEffect:
		return OutboxEntry{
			ID: createEffectID(effect.Identity.ID, effect.Generation), Kind: EffectCreateLane, Status: OutboxPending,
			LaneID: effect.Identity.ID, OperationID: provider.OperationID(createEffectID(effect.Identity.ID, effect.Generation)),
			Owner: effect.Owner, CWD: effect.CWD, ModelID: effect.ModelID, ModeID: effect.ModeID,
			Generation: effect.Generation, Reconcile: effect.Reconcile,
			CreateAfterCandidateAbsence: effect.CreateAfterCandidateAbsence,
		}, true, nil
	case ResumeLaneEffect:
		return OutboxEntry{
			ID: resumeEffectID(effect.Identity.ID, effect.Generation), Kind: EffectResumeLane, Status: OutboxPending,
			LaneID: effect.Identity.ID, OperationID: provider.OperationID(resumeEffectID(effect.Identity.ID, effect.Generation)),
			Owner: effect.Owner, CWD: effect.CWD, ModelID: effect.ModelID, ModeID: effect.ModeID,
			Generation: effect.Generation, Thread: effect.Thread,
		}, true, nil
	case ImportContextEffect:
		batch := cloneContextBatch(effect.Batch)
		return OutboxEntry{
			ID: string(effect.OperationID), Kind: EffectImportContext, Status: OutboxPending,
			LaneID: effect.LaneID, OperationID: effect.OperationID, From: effect.From, To: effect.To, Batch: &batch,
			Reconcile: effect.Reconcile,
		}, true, nil
	case StartTurnEffect:
		input := effect.Input
		input.Attachments = append([]provider.Attachment(nil), effect.Input.Attachments...)
		if input.InitialSeedDigest != "" {
			if effect.Seed.Digest != input.InitialSeedDigest || input.InitialSeedThrough <= input.InitialSeedFrom {
				return OutboxEntry{}, false, errors.New("start-turn effect lost its immutable initial context seed")
			}
		} else if effect.Seed.Digest != "" || input.InitialSeedFrom != 0 || input.InitialSeedThrough != 0 {
			return OutboxEntry{}, false, errors.New("start-turn effect has an incomplete initial context seed")
		}
		return OutboxEntry{
			ID: startTurnEffectID(effect.Input.OperationID), Kind: EffectStartTurn, Status: OutboxPending,
			LaneID: effect.LaneID, OperationID: effect.Input.OperationID, Input: &input,
		}, true, nil
	case ReconcileTurnEffect:
		return OutboxEntry{
			ID: reconcileTurnEffectID(effect.OperationID), Kind: EffectReconcileTurn, Status: OutboxPending,
			LaneID: effect.LaneID, OperationID: effect.OperationID, Turn: effect.Turn,
		}, true, nil
	case SteerTurnEffect:
		input := QueueEntry{
			OperationID: effect.OperationID, LaneID: effect.LaneID, Text: effect.Text,
			Attachments: append([]provider.Attachment(nil), effect.Attachments...),
		}
		return OutboxEntry{
			ID: steerEffectID(effect.OperationID), Kind: EffectSteerTurn, Status: OutboxPending,
			LaneID: effect.LaneID, OperationID: effect.OperationID, Input: &input, Turn: effect.Turn,
		}, true, nil
	case CancelTurnEffect:
		return OutboxEntry{
			ID: cancelEffectID(effect.OperationID), Kind: EffectCancelTurn, Status: OutboxPending,
			LaneID: effect.LaneID, OperationID: effect.OperationID, Turn: effect.Turn,
		}, true, nil
	case ResolvePermissionEffect:
		return OutboxEntry{
			ID: permissionEffectID(effect.OperationID), Kind: EffectPermission, Status: OutboxPending,
			LaneID: effect.LaneID, OperationID: effect.OperationID,
			RequestID: effect.RequestID, OptionID: effect.OptionID,
		}, true, nil
	case BackgroundActionEffect:
		action := effect.Action.Clone()
		return OutboxEntry{
			ID: backgroundEffectID(action.OperationID), Kind: EffectBackground, Status: OutboxPending,
			LaneID: action.Owner.LaneID, OperationID: action.OperationID, Background: &action,
		}, true, nil
	case RestoreCheckpointEffect:
		checkpoint := append(json.RawMessage(nil), effect.Checkpoint...)
		return OutboxEntry{
			ID: checkpointRestoreEffectID(effect.OperationID), Kind: EffectCheckpointRestore, Status: OutboxPending,
			OperationID: effect.OperationID, TurnSequence: effect.TurnSequence, ObservedAtUnixMS: effect.ObservedAtUnixMS,
			Checkpoint: checkpoint, CheckpointDigest: effect.CheckpointDigest,
		}, true, nil
	case DetachLaneEffect:
		return OutboxEntry{
			ID: detachEffectID(effect.OperationID), Kind: EffectDetachLane, Status: OutboxPending,
			LaneID: effect.LaneID, OperationID: effect.OperationID, Owner: effect.Owner,
			ConnectionID: strings.TrimSpace(effect.ConnectionID), Generation: effect.ConnectionGeneration,
		}, true, nil
	case ExternalMutationEffect:
		return OutboxEntry{
			ID: externalMutationEffectID(effect.OperationID), Kind: EffectExternalMutation, Status: OutboxPending,
			OperationID: effect.OperationID, ChatID: strings.TrimSpace(effect.ChatID),
			TabID: strings.TrimSpace(effect.TabID), MutationKind: strings.TrimSpace(effect.Kind),
			MutationMethod: strings.TrimSpace(effect.Method), MutationDigest: strings.TrimSpace(effect.Digest),
		}, true, nil
	case DeleteChatEffect:
		return OutboxEntry{
			ID: deleteChatEffectID(effect.ChatID), Kind: EffectDeleteChat, Status: OutboxPending,
			OperationID: effect.OperationID, ChatID: strings.TrimSpace(effect.ChatID), TabID: strings.TrimSpace(effect.TabID),
		}, true, nil
	default:
		return OutboxEntry{}, false, nil
	}
}

func recordEffects(state *State, effects []Effect) error {
	existing := make(map[string]struct{}, len(state.Outbox))
	for _, entry := range state.Outbox {
		existing[entry.ID] = struct{}{}
	}
	for _, effect := range effects {
		entry, durable, err := outboxEntryForEffect(effect)
		if err != nil {
			return err
		}
		if !durable {
			continue
		}
		if _, exists := existing[entry.ID]; exists {
			continue
		}
		state.Outbox = append(state.Outbox, entry)
		existing[entry.ID] = struct{}{}
	}
	return nil
}

func updateOutbox(state *State, effectID string, status OutboxStatus, kind provider.ErrorKind) bool {
	for i := range state.Outbox {
		if state.Outbox[i].ID != effectID {
			continue
		}
		state.Outbox[i].Status = status
		state.Outbox[i].LastError = kind
		return true
	}
	return false
}

func outboxHas(state *State, effectID string, status OutboxStatus) bool {
	for i := range state.Outbox {
		if state.Outbox[i].ID == effectID && state.Outbox[i].Status == status {
			return true
		}
	}
	return false
}

// Reconciliation is a pure, non-sampling read of one immutable provider
// operation. If a host/runtime version could not perform that read, an exact
// later reattachment may safely try it again. Re-arm the existing receipt
// instead of appending a duplicate effect or ever replaying the user input.
func rearmTurnReconcileOutbox(state *State, operationID provider.OperationID, turn provider.TurnRef) {
	effectID := reconcileTurnEffectID(operationID)
	for index := range state.Outbox {
		entry := &state.Outbox[index]
		if entry.ID != effectID || entry.Kind != EffectReconcileTurn || entry.OperationID != operationID || entry.Turn != turn {
			continue
		}
		switch entry.Status {
		case OutboxCompleted, OutboxFailed, OutboxAmbiguous:
			entry.Status = OutboxPending
			entry.LastError = ""
		}
		return
	}
}

func effectFromOutbox(state State, entry OutboxEntry) (Effect, error) {
	if entry.Kind == EffectDeleteChat {
		operationID := provider.NormalizeOperationID(string(entry.OperationID))
		if !state.Deleted || strings.TrimSpace(entry.ChatID) != state.ChatID || operationID == "" || operationID != state.DeletionOperationID {
			return nil, errors.New("delete-chat outbox entry lost its tombstone identity")
		}
		return DeleteChatEffect{OperationID: operationID, ChatID: entry.ChatID, TabID: entry.TabID}, nil
	}
	if entry.Kind == EffectBackground {
		if entry.Background == nil || entry.Background.OperationID != entry.OperationID {
			return nil, errors.New("background outbox entry lost its immutable action")
		}
		action := entry.Background.Clone()
		if err := action.Validate(state); err != nil {
			return nil, err
		}
		return BackgroundActionEffect{Action: action}, nil
	}
	if entry.Kind == EffectCheckpointRestore {
		if entry.OperationID == "" || entry.TurnSequence <= 0 || entry.ObservedAtUnixMS <= 0 ||
			validateCheckpointRestorePayload(entry.Checkpoint, entry.TurnSequence, entry.CheckpointDigest) != nil {
			return nil, errors.New("checkpoint-restore outbox entry lost its immutable request")
		}
		return RestoreCheckpointEffect{
			OperationID: entry.OperationID, TurnSequence: entry.TurnSequence, ObservedAtUnixMS: entry.ObservedAtUnixMS,
			Checkpoint: append(json.RawMessage(nil), entry.Checkpoint...), CheckpointDigest: entry.CheckpointDigest,
		}, nil
	}
	if entry.Kind == EffectExternalMutation {
		if !state.Initialized || state.Deleted || strings.TrimSpace(entry.ChatID) != state.ChatID ||
			entry.OperationID == "" || strings.TrimSpace(entry.MutationKind) == "" ||
			strings.TrimSpace(entry.MutationMethod) == "" || strings.TrimSpace(entry.TabID) == "" ||
			strings.TrimSpace(entry.MutationDigest) == "" {
			return nil, errors.New("external mutation outbox entry lost its immutable request")
		}
		return ExternalMutationEffect{
			OperationID: entry.OperationID, ChatID: entry.ChatID, Kind: entry.MutationKind,
			Method: entry.MutationMethod, TabID: entry.TabID, Digest: entry.MutationDigest,
		}, nil
	}
	if entry.Kind == EffectDetachLane {
		lane, ok := state.Lanes[entry.LaneID]
		if !ok || lane.ConnectionGeneration != entry.Generation || lane.Attachment == nil ||
			strings.TrimSpace(lane.Attachment.ConnectionID) != strings.TrimSpace(entry.ConnectionID) || lane.Owner != entry.Owner {
			return nil, errors.New("lane-detach outbox entry changed its exact attachment")
		}
		return DetachLaneEffect{
			OperationID: entry.OperationID, LaneID: entry.LaneID, Owner: entry.Owner,
			ConnectionID: entry.ConnectionID, ConnectionGeneration: entry.Generation,
		}, nil
	}
	lane, ok := state.Lanes[entry.LaneID]
	if !ok {
		return nil, errors.New("outbox lane no longer exists")
	}
	switch entry.Kind {
	case EffectCreateLane:
		if entry.CreateAfterCandidateAbsence != lane.CreateAfterCandidateAbsence {
			return nil, errors.New("create outbox lost its candidate-absence boundary")
		}
		return CreateLaneEffect{
			Identity: lane.Identity, Owner: entry.Owner, CWD: entry.CWD, ModelID: entry.ModelID, ModeID: entry.ModeID,
			Generation: entry.Generation, Reconcile: entry.Reconcile,
			CreateAfterCandidateAbsence: entry.CreateAfterCandidateAbsence,
		}, nil
	case EffectResumeLane:
		return ResumeLaneEffect{
			Identity: lane.Identity, Thread: entry.Thread, Owner: entry.Owner, CWD: entry.CWD, ModelID: entry.ModelID, ModeID: entry.ModeID,
			Generation: entry.Generation,
		}, nil
	case EffectImportContext:
		if entry.Batch == nil {
			return nil, errors.New("context-import outbox entry lost its immutable projection")
		}
		return ImportContextEffect{
			LaneID: entry.LaneID, OperationID: entry.OperationID, From: entry.From, To: entry.To,
			Batch: cloneContextBatch(*entry.Batch), Reconcile: entry.Reconcile,
		}, nil
	case EffectStartTurn:
		if entry.Input != nil && entry.Input.OperationID == entry.OperationID {
			seed := ContextBatch{}
			if entry.Input.InitialSeedDigest != "" {
				var err error
				seed, err = buildInitialSeedBatch(state, entry.Input.InitialSeedFrom, entry.Input.InitialSeedThrough)
				if err != nil {
					return nil, err
				}
				if seed.Digest != entry.Input.InitialSeedDigest {
					return nil, errors.New("start-turn outbox initial context seed changed identity")
				}
			}
			return StartTurnEffect{LaneID: entry.LaneID, Input: *entry.Input, Seed: seed}, nil
		}
		return nil, errors.New("start-turn outbox entry lost its immutable input")
	case EffectReconcileTurn:
		if state.Foreground == nil || state.Foreground.OperationID != entry.OperationID || entry.Turn.NativeID == "" {
			return nil, errors.New("reconcile outbox entry lost its foreground turn")
		}
		return ReconcileTurnEffect{LaneID: entry.LaneID, OperationID: entry.OperationID, Turn: entry.Turn}, nil
	case EffectSteerTurn:
		if entry.Input == nil || entry.Input.OperationID != entry.OperationID || entry.Turn.NativeID == "" {
			return nil, errors.New("steer outbox entry lost its immutable input or turn")
		}
		return SteerTurnEffect{
			LaneID: entry.LaneID, OperationID: entry.OperationID, Turn: entry.Turn,
			Text: entry.Input.Text, Attachments: append([]provider.Attachment(nil), entry.Input.Attachments...),
		}, nil
	case EffectCancelTurn:
		if entry.Turn.NativeID == "" {
			return nil, errors.New("cancel outbox entry lost its immutable turn")
		}
		return CancelTurnEffect{LaneID: entry.LaneID, OperationID: entry.OperationID, Turn: entry.Turn}, nil
	case EffectPermission:
		return ResolvePermissionEffect{
			LaneID: entry.LaneID, OperationID: entry.OperationID, RequestID: entry.RequestID, OptionID: entry.OptionID,
		}, nil
	default:
		return nil, fmt.Errorf("unknown outbox effect kind %q", entry.Kind)
	}
}

func reduceClaimEffect(state *State, command ClaimEffect) ([]Effect, error) {
	effectID := strings.TrimSpace(command.EffectID)
	for i := range state.Outbox {
		if state.Outbox[i].ID != effectID {
			continue
		}
		if state.Outbox[i].Status != OutboxPending {
			return nil, fmt.Errorf("outbox effect %q cannot dispatch from %q", effectID, state.Outbox[i].Status)
		}
		state.Outbox[i].Status = OutboxDispatched
		effect, err := effectFromOutbox(*state, state.Outbox[i])
		if err != nil {
			state.Outbox[i].Status = OutboxFailed
			state.Outbox[i].LastError = provider.ErrorProtocolViolation
			return nil, err
		}
		return []Effect{effect}, nil
	}
	return nil, fmt.Errorf("unknown outbox effect %q", effectID)
}

func pendingOutboxKind(state *State, laneID provider.LaneID, kind EffectKind) bool {
	for i := range state.Outbox {
		entry := state.Outbox[i]
		if entry.LaneID == laneID && entry.Kind == kind && entry.Status == OutboxPending {
			return true
		}
	}
	return false
}

func reduceRecoverOutbox(state *State) ([]Effect, error) {
	var effects []Effect
	for i := range state.Outbox {
		entry := &state.Outbox[i]
		switch entry.Status {
		case OutboxPending, OutboxCompleted, OutboxFailed, OutboxAmbiguous:
			continue
		case OutboxDispatched:
			switch entry.Kind {
			case EffectExternalMutation:
				// The browser call may already have escaped the daemon. Without its
				// exact shell receipt, replaying the mutation could duplicate the
				// visible side effect. Recovery therefore fails closed forever.
				entry.Status = OutboxAmbiguous
				entry.LastError = provider.ErrorAcceptanceAmbiguous
			case EffectBackground, EffectCheckpointRestore:
				entry.Status = OutboxAmbiguous
				entry.LastError = provider.ErrorAcceptanceAmbiguous
			case EffectDetachLane:
				if detachReceiptForGeneration(*state, entry.LaneID, entry.Generation) {
					entry.Status = OutboxCompleted
					entry.LastError = ""
				} else {
					// Detach has no provider-level idempotency/readback contract. A
					// crash after dispatch may have closed the transport even when the
					// saved attachment still looks unchanged, so it must never be sent
					// again automatically.
					entry.Status = OutboxAmbiguous
					entry.LastError = provider.ErrorAcceptanceAmbiguous
				}
			case EffectResumeLane, EffectReconcileTurn, EffectCancelTurn, EffectDeleteChat:
				// Exact resume and readback are non-creating operations. Returning
				// them to Pending lets the executor claim them again safely.
				entry.Status = OutboxPending
			case EffectCreateLane:
				// A provider create itself is not safe to repeat, but the daemon's
				// durable native binding is an authoritative receipt when it exists.
				// Reclaim this effect in reconcile-only mode: the runtime may exact-
				// resume that binding or fail ambiguous; it may never call create.
				entry.Status = OutboxPending
				entry.Reconcile = true
			case EffectImportContext:
				lane := state.Lanes[entry.LaneID]
				if lane.PendingImport != nil && lane.Context.ImportReadback && lane.Context.IdempotentImport {
					entry.Status = OutboxPending
					entry.Reconcile = true
					entry.LastError = ""
					continue
				}
				entry.Status = OutboxAmbiguous
				entry.LastError = provider.ErrorAcceptanceAmbiguous
				lane.Phase = LaneBlocked
				lane.LastError = provider.ErrorAcceptanceAmbiguous
				state.Lanes[entry.LaneID] = lane
			case EffectStartTurn:
				lane := state.Lanes[entry.LaneID]
				if lane.Provision != nil {
					if state.Foreground != nil && state.Foreground.OperationID == entry.OperationID && state.Foreground.Turn.NativeID == "" {
						entry.Status = OutboxPending
						entry.LastError = ""
						state.Foreground.Status = ForegroundDispatching
					} else {
						entry.Status = OutboxAmbiguous
						entry.LastError = provider.ErrorAcceptanceAmbiguous
						if state.Foreground != nil && state.Foreground.OperationID == entry.OperationID {
							state.Foreground.Status = ForegroundReconciling
						}
					}
					state.Lanes[entry.LaneID] = lane
					continue
				}
				entry.Status = OutboxAmbiguous
				entry.LastError = provider.ErrorAcceptanceAmbiguous
				lane = state.Lanes[entry.LaneID]
				lane.Phase = LaneBlocked
				lane.LastError = provider.ErrorAcceptanceAmbiguous
				state.Lanes[entry.LaneID] = lane
				if state.Foreground != nil && state.Foreground.OperationID == entry.OperationID {
					state.Foreground.Status = ForegroundUncertain
				}
			case EffectSteerTurn:
				entry.Status = OutboxAmbiguous
				entry.LastError = provider.ErrorAcceptanceAmbiguous
				if state.PendingSteer != nil && state.PendingSteer.OperationID == entry.OperationID {
					state.PendingSteer.Status = SteerUncertain
				}
			case EffectPermission:
				entry.Status = OutboxAmbiguous
				entry.LastError = provider.ErrorAcceptanceAmbiguous
				if permission, ok := state.Permissions[entry.RequestID]; ok {
					permission.Event.Status = "uncertain"
					state.Permissions[entry.RequestID] = permission
				}
			}
		case OutboxAccepted, OutboxConsumed:
			if entry.Kind == EffectCreateLane {
				entry.Status = OutboxPending
				entry.Reconcile = true
				entry.LastError = ""
				continue
			}
			if entry.Kind == EffectSteerTurn || entry.Kind == EffectPermission {
				entry.Status = OutboxAmbiguous
				entry.LastError = provider.ErrorAcceptanceAmbiguous
				if entry.Kind == EffectSteerTurn && state.PendingSteer != nil && state.PendingSteer.OperationID == entry.OperationID {
					state.PendingSteer.Status = SteerUncertain
				}
				continue
			}
			if entry.Kind == EffectCancelTurn {
				entry.Status = OutboxPending
				continue
			}
			if entry.Kind != EffectStartTurn || state.Foreground == nil || state.Foreground.OperationID != entry.OperationID || state.Foreground.Turn.NativeID == "" {
				entry.Status = OutboxAmbiguous
				entry.LastError = provider.ErrorAcceptanceAmbiguous
				continue
			}
			// Native acceptance is known, but every provider attachment died with
			// the daemon. Exact-resume the same lane first; LaneOpened will then
			// enqueue readback. Reconciliation can never run against a missing
			// disposable attachment.
			state.Foreground.Status = ForegroundReconciling
		default:
			return nil, fmt.Errorf("unknown outbox status %q", entry.Status)
		}
	}

	// Provider attachments are process-local. Durable lanes therefore never
	// remain Ready/Running merely because the daemon state file said so. Idle
	// lanes become Detached; lanes that own a pending or accepted foreground
	// operation schedule exact resume before delivery/readback can continue.
	for laneID, lane := range state.Lanes {
		if lane.Provision != nil {
			lane.Attachment = nil
			if lane.Phase == LaneBlocked || lane.Phase == LaneBroken {
				state.Lanes[laneID] = lane
				continue
			}
			if pendingOutboxKind(state, laneID, EffectCreateLane) {
				lane.Phase = LaneCreating
				state.Lanes[laneID] = lane
				continue
			}
			foreground := state.Foreground != nil && state.Foreground.LaneID == laneID
			queued := false
			for _, input := range state.Queue {
				if input.LaneID == laneID {
					queued = true
					break
				}
			}
			if !foreground && !queued {
				lane.Phase = LaneAbsent
				state.Lanes[laneID] = lane
				continue
			}
			lane.CreateGeneration++
			if lane.CreateGeneration <= lane.ConnectionGeneration {
				lane.CreateGeneration = lane.ConnectionGeneration + 1
			}
			lane.ConnectionGeneration = lane.CreateGeneration
			lane.Phase = LaneCreating
			state.Lanes[laneID] = lane
			effects = append(effects, CreateLaneEffect{
				Identity: lane.Identity, Owner: lane.Owner, CWD: lane.CWD,
				ModelID: lane.ModelID, ModeID: lane.ModeID, Generation: lane.CreateGeneration, Reconcile: true,
				CreateAfterCandidateAbsence: lane.CreateAfterCandidateAbsence,
			})
			continue
		}
		if lane.Thread.IsZero() || lane.Phase == LaneBlocked || lane.Phase == LaneBroken || lane.Phase == LaneCreating {
			continue
		}
		if pendingOutboxKind(state, laneID, EffectResumeLane) {
			lane.Phase = LaneResuming
			state.Lanes[laneID] = lane
			continue
		}
		foreground := state.Foreground != nil && state.Foreground.LaneID == laneID
		if foreground && (state.Foreground.Status == ForegroundDispatching || state.Foreground.Status == ForegroundReconciling) {
			lane.Phase = LaneResuming
			if !pendingOutboxKind(state, laneID, EffectResumeLane) {
				lane.ConnectionGeneration++
				effects = append(effects, ResumeLaneEffect{
					Identity: lane.Identity, Thread: lane.Thread, Owner: lane.Owner, CWD: lane.CWD,
					ModelID: lane.ModelID, ModeID: lane.ModeID, Generation: lane.ConnectionGeneration,
				})
			}
			state.Lanes[laneID] = lane
			continue
		}
		switch lane.Phase {
		case LaneReady, LaneRunning, LaneResuming, LaneReconciling, LaneImporting:
			lane.Phase = LaneDetached
			state.Lanes[laneID] = lane
		}
	}
	return effects, nil
}
