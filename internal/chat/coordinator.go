package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"workass/internal/provider"
)

// DefinitionResolver is the narrow registry surface consumed by the chat
// coordinator. provider.Registry satisfies it directly; tests and embedders can
// supply an immutable view without exposing provider configuration mutation.
type DefinitionResolver interface {
	Resolve(provider.ID) (provider.Definition, bool)
}

// LifecycleReceipt describes a provider-neutral transition that was already
// committed to the durable actor. It is a presentation hook only: observers
// may publish frozen UI notifications, but cannot accept provider work or
// mutate chat ownership.
type LifecycleReceipt struct {
	Kind         string
	ChatID       string
	LaneID       provider.LaneID
	OperationID  provider.OperationID
	Thread       provider.ThreadRef
	Turn         provider.TurnRef
	Terminal     bool
	Status       string
	TurnSequence int
	Result       json.RawMessage
}

const (
	LifecycleHostRecoveryResumed = "host_recovery_resumed"
	LifecycleTurnReconciled      = "turn_reconciled"
	LifecycleTurnAdmissionFailed = "turn_admission_failed"
	LifecycleCheckpointRestored  = "checkpoint_restored"
)

// Coordinator is the only executor for a chat engine's durable outbox. It owns
// disposable lane attachments, while Engine remains the sole owner of durable
// chat state. External calls happen only after ClaimNext persisted dispatch.
type Coordinator struct {
	engine   *Engine
	registry DefinitionResolver
	ctx      context.Context

	mu                        sync.Mutex
	lanes                     map[provider.LaneID]provider.Lane
	generations               map[provider.LaneID]uint64
	cancel                    context.CancelFunc
	eventsWG                  sync.WaitGroup
	drainMu                   sync.Mutex
	claimMu                   sync.Mutex
	replyAdmissionHolds       int
	effectWake                chan struct{}
	effectWG                  sync.WaitGroup
	chatCleanup               func(context.Context, string, string, provider.OperationID) error
	backgroundExecutor        func(context.Context, BackgroundAction) (json.RawMessage, error)
	checkpointRestoreExecutor func(context.Context, string, int, json.RawMessage, string, provider.OperationID) (json.RawMessage, error)
	lifecycleObserver         func(LifecycleReceipt)
}

// BeginReplyAdmission fences the generic durable-effect claim boundary while
// a caller commits input whose receipt has not crossed its reply boundary yet.
// It is transient by design: a process restart drops the fence and recovery
// drains the already-durable input. The returned release is idempotent.
func (c *Coordinator) BeginReplyAdmission() func() {
	if c == nil {
		return func() {}
	}
	c.claimMu.Lock()
	c.replyAdmissionHolds++
	c.claimMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.claimMu.Lock()
			if c.replyAdmissionHolds > 0 {
				c.replyAdmissionHolds--
			}
			ready := c.replyAdmissionHolds == 0
			c.claimMu.Unlock()
			if ready {
				c.Wake()
			}
		})
	}
}

func NewCoordinator(engine *Engine, registry DefinitionResolver) (*Coordinator, error) {
	if engine == nil {
		return nil, errors.New("chat coordinator requires an engine")
	}
	if registry == nil {
		return nil, errors.New("chat coordinator requires a provider registry")
	}
	ctx, cancel := context.WithCancel(context.Background())
	coordinator := &Coordinator{
		engine: engine, registry: registry, lanes: make(map[provider.LaneID]provider.Lane),
		generations: make(map[provider.LaneID]uint64), ctx: ctx, cancel: cancel,
		effectWake: make(chan struct{}, 1),
	}
	coordinator.effectWG.Add(1)
	go coordinator.runEffectWake()
	return coordinator, nil
}

// SetChatCleanup installs the daemon-owned, idempotent native-binding cleanup
// boundary used by the durable chat tombstone effect. It is configured before
// Wake and is intentionally separate from provider adapters because deletion
// covers every lane in the chat.
func (c *Coordinator) SetChatCleanup(cleanup func(context.Context, string, string, provider.OperationID) error) error {
	if c == nil || cleanup == nil {
		return errors.New("chat coordinator requires a cleanup executor")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.chatCleanup != nil {
		return errors.New("chat cleanup executor is already configured")
	}
	c.chatCleanup = cleanup
	return nil
}

// SetBackgroundExecutor installs the one daemon runtime boundary for all
// actor-journaled background mutations. Authentication is checked before the
// command is admitted; no credential is ever written into actor state.
func (c *Coordinator) SetBackgroundExecutor(execute func(context.Context, BackgroundAction) (json.RawMessage, error)) error {
	if c == nil || execute == nil {
		return errors.New("chat coordinator requires a background executor")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.backgroundExecutor != nil {
		return errors.New("chat background executor is already configured")
	}
	c.backgroundExecutor = execute
	return nil
}

func (c *Coordinator) SetCheckpointRestoreExecutor(execute func(context.Context, string, int, json.RawMessage, string, provider.OperationID) (json.RawMessage, error)) error {
	if c == nil || execute == nil {
		return errors.New("chat coordinator requires a checkpoint-restore executor")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.checkpointRestoreExecutor != nil {
		return errors.New("checkpoint-restore executor is already configured")
	}
	c.checkpointRestoreExecutor = execute
	return nil
}

func (c *Coordinator) SetLifecycleObserver(observe func(LifecycleReceipt)) error {
	if c == nil || observe == nil {
		return errors.New("chat coordinator requires a lifecycle observer")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lifecycleObserver != nil {
		return errors.New("chat lifecycle observer is already configured")
	}
	c.lifecycleObserver = observe
	return nil
}

func (c *Coordinator) publishLifecycle(receipt LifecycleReceipt) {
	c.mu.Lock()
	observe := c.lifecycleObserver
	c.mu.Unlock()
	if observe != nil {
		observe(receipt)
	}
}

// ExecuteNext claims and executes at most one durable effect. true means an
// effect was claimed, even if its provider receipt failed closed.
func (c *Coordinator) ExecuteNext(ctx context.Context) (bool, error) {
	c.claimMu.Lock()
	if c.replyAdmissionHolds > 0 {
		c.claimMu.Unlock()
		return false, nil
	}
	effect, ok, err := c.engine.ClaimNext()
	c.claimMu.Unlock()
	if err != nil || !ok {
		return ok, err
	}
	switch effect := effect.(type) {
	case CreateLaneEffect:
		definition, err := c.definition(effect.Identity.Realm.ProviderID)
		if err != nil {
			return true, c.applyLaneFailure(effect.Identity.ID, err)
		}
		lane, thread, err := definition.Runtime.Create(ctx, provider.CreateLaneRequest{
			Identity: effect.Identity, Owner: effect.Owner, CWD: effect.CWD, ModelID: effect.ModelID, ModeID: effect.ModeID,
			Reconcile: effect.Reconcile, CreateAfterCandidateAbsence: effect.CreateAfterCandidateAbsence,
		})
		if err != nil {
			return true, c.applyLaneFailure(effect.Identity.ID, err)
		}
		canonical := lane.Identity().Normalize()
		c.attachLane(lane, effect.Generation)
		attachment := laneAttachmentSnapshot(lane)
		creationReceipt, deferred := lane.(provider.ThreadCreationReceipt)
		if deferred && !creationReceipt.ThreadCreationCommitted() {
			err = c.engine.Apply(LaneProvisioned{
				LaneID: effect.Identity.ID, Identity: canonical, Candidate: thread, ConnectionGeneration: effect.Generation,
				Context: lane.Context().Capabilities(), Delivery: lane.Delivery().Capabilities(),
				Creation: provider.CreationCapabilities{DeferredUntilInput: true}, Attachment: attachment,
				Reconciled: effect.Reconcile, PreviousCandidateAbsent: creationReceipt.PreviousCandidateAbsent(),
			})
		} else {
			err = c.engine.Apply(LaneOpened{
				LaneID: effect.Identity.ID, Identity: canonical, Thread: thread, ConnectionGeneration: effect.Generation,
				Context: lane.Context().Capabilities(), Delivery: lane.Delivery().Capabilities(), Attachment: attachment, Reconciled: effect.Reconcile,
			})
		}
		if err != nil {
			c.discardLane(lane)
			return true, err
		}
		c.startLaneEvents(lane, effect.Generation)
		return true, nil
	case ResumeLaneEffect:
		beforeResume := c.engine.Snapshot()
		recoveringForeground := beforeResume.Foreground != nil &&
			beforeResume.Foreground.LaneID == effect.Identity.ID &&
			beforeResume.Foreground.Status == ForegroundReconciling
		recoveryOperationID := provider.OperationID("")
		recoveryTurn := provider.TurnRef{}
		if recoveringForeground {
			recoveryOperationID = beforeResume.Foreground.OperationID
			recoveryTurn = beforeResume.Foreground.Turn
		}
		definition, err := c.definition(effect.Identity.Realm.ProviderID)
		if err != nil {
			return true, c.applyLaneFailure(effect.Identity.ID, err)
		}
		lane, err := definition.Runtime.Resume(ctx, provider.ResumeLaneRequest{
			Identity: effect.Identity, Thread: effect.Thread, Owner: effect.Owner, CWD: effect.CWD,
			ModelID: effect.ModelID, ModeID: effect.ModeID,
		})
		if err != nil {
			return true, c.applyLaneFailure(effect.Identity.ID, err)
		}
		if !lane.Thread().Equal(effect.Thread) || lane.Identity() != effect.Identity {
			_ = lane.Detach(context.Background())
			return true, c.applyLaneFailure(effect.Identity.ID, &provider.Error{Kind: provider.ErrorNativeIdentityConflict, Message: "provider exact resume changed lane identity"})
		}
		c.attachLane(lane, effect.Generation)
		attachment := laneAttachmentSnapshot(lane)
		if err := c.engine.Apply(LaneOpened{
			LaneID: effect.Identity.ID, Identity: effect.Identity, Thread: effect.Thread, ConnectionGeneration: effect.Generation,
			Context: lane.Context().Capabilities(), Delivery: lane.Delivery().Capabilities(), Attachment: attachment,
		}); err != nil {
			c.discardLane(lane)
			return true, err
		}
		c.startLaneEvents(lane, effect.Generation)
		if recoveringForeground {
			c.publishLifecycle(LifecycleReceipt{
				Kind: LifecycleHostRecoveryResumed, ChatID: effect.Identity.ChatID, LaneID: effect.Identity.ID,
				OperationID: recoveryOperationID, Thread: effect.Thread, Turn: recoveryTurn,
			})
		}
		return true, nil
	case ImportContextEffect:
		lane, err := c.lane(effect.LaneID)
		if err != nil {
			return true, err
		}
		request := provider.ContextImportRequest{
			OperationID: effect.OperationID, From: effect.From, To: effect.To,
			Digest: effect.Batch.Digest, Messages: append([]provider.ContextMessage(nil), effect.Batch.Messages...),
		}
		var receipt provider.ContextImportReceipt
		if effect.Reconcile {
			receipt, err = lane.Context().ReconcileImport(ctx, request)
		} else {
			receipt, err = lane.Context().Import(ctx, request)
		}
		if err != nil {
			return true, c.engine.Apply(ContextImported{
				LaneID: effect.LaneID, OperationID: effect.OperationID, From: effect.From, To: effect.To,
				Digest: effect.Batch.Digest, Ambiguous: provider.ErrorIs(err, provider.ErrorAcceptanceAmbiguous),
				Reconciled: effect.Reconcile,
			})
		}
		return true, c.engine.Apply(ContextImported{
			LaneID: effect.LaneID, OperationID: receipt.OperationID, From: receipt.From, To: receipt.To,
			Digest: receipt.Digest, Found: receipt.Found, Confirmed: receipt.Confirmed,
			Ambiguous: receipt.Ambiguous, Reconciled: effect.Reconcile,
		})
	case StartTurnEffect:
		lane, err := c.lane(effect.LaneID)
		if err != nil {
			return true, err
		}
		admission, err := lane.Delivery().StartTurn(ctx, provider.TurnInput{
			OperationID: effect.Input.OperationID, Text: effect.Input.Text,
			Attachments:    append([]provider.Attachment(nil), effect.Input.Attachments...),
			InitialContext: append([]provider.ContextMessage(nil), effect.Seed.Messages...),
			ModelID:        effect.Input.ModelID, ModeID: effect.Input.ModeID, Permission: effect.Input.Permission,
			Presentation: effect.Input.Presentation,
			CommitAdmission: func(admission provider.TurnAdmission) error {
				return c.engine.Apply(TurnAdmitted{
					OperationID: effect.Input.OperationID, Turn: admission.Turn,
					Accepted: admission.Accepted, Ambiguous: !admission.Accepted,
				})
			},
		})
		if err != nil {
			if turnCancelledBeforeAdmission(c.engine.Snapshot(), effect.Input.OperationID) {
				return true, nil
			}
			if provider.ErrorIs(err, provider.ErrorAcceptanceAmbiguous) {
				return true, c.engine.Apply(TurnAdmitted{OperationID: effect.Input.OperationID, Ambiguous: true})
			}
			applyErr := c.engine.Apply(TurnAdmissionFailed{OperationID: effect.Input.OperationID, Kind: providerErrorKind(err)})
			if applyErr == nil {
				c.publishLifecycle(LifecycleReceipt{
					Kind: LifecycleTurnAdmissionFailed, ChatID: c.engine.Snapshot().ChatID,
					LaneID: effect.LaneID, OperationID: effect.Input.OperationID, Terminal: true, Status: "failed",
				})
			}
			return true, applyErr
		}
		if err := c.engine.Apply(TurnAdmitted{
			OperationID: effect.Input.OperationID, Turn: admission.Turn, Accepted: admission.Accepted,
			Ambiguous: !admission.Accepted,
		}); err != nil {
			if turnCancelledBeforeAdmission(c.engine.Snapshot(), effect.Input.OperationID) {
				return true, nil
			}
			return true, err
		}
		if admission.Consumed {
			return true, c.engine.Apply(InputConsumed{OperationID: effect.Input.OperationID})
		}
		return true, nil
	case ReconcileTurnEffect:
		lane, err := c.lane(effect.LaneID)
		if err != nil {
			return true, err
		}
		result, err := lane.Delivery().Reconcile(ctx, provider.ReconcileRequest{OperationID: effect.OperationID, Turn: effect.Turn})
		if err != nil {
			// Readback failure is not permission to leave a permanent spinner or
			// resend. Mark the exact operation uncertain and the lane blocked; an
			// explicit later reconciliation may resolve it, but ordinary admission
			// cannot pass this boundary.
			applyErr := c.engine.Apply(TurnReconciled{
				OperationID: effect.OperationID, Turn: effect.Turn, Found: false,
			})
			c.publishLifecycle(LifecycleReceipt{
				Kind: LifecycleTurnReconciled, ChatID: lane.Identity().ChatID, LaneID: effect.LaneID,
				OperationID: effect.OperationID, Turn: effect.Turn, Status: "uncertain",
			})
			if applyErr != nil {
				return true, errors.Join(err, applyErr)
			}
			return true, err
		}
		turn := result.Turn
		if turn.OperationID == "" {
			turn.OperationID = effect.OperationID
		}
		if turn.NativeID == "" {
			turn.NativeID = effect.Turn.NativeID
		}
		command := TurnReconciled{
			OperationID: effect.OperationID, Turn: turn, Found: result.Found,
			Consumed: result.Consumed, Terminal: result.Terminal, Status: result.State,
		}
		if err := c.engine.Apply(command); err != nil {
			return true, err
		}
		c.publishLifecycle(LifecycleReceipt{
			Kind: LifecycleTurnReconciled, ChatID: lane.Identity().ChatID, LaneID: effect.LaneID,
			OperationID: effect.OperationID, Turn: turn, Terminal: result.Terminal, Status: result.State,
		})
		return true, nil
	case SteerTurnEffect:
		return true, c.executeSteer(ctx, effect)
	case CancelTurnEffect:
		return true, c.executeCancel(ctx, effect)
	case ResolvePermissionEffect:
		lane, err := c.lane(effect.LaneID)
		if err != nil {
			return true, err
		}
		receipt, err := lane.Delivery().ResolvePermission(ctx, provider.PermissionDecision{
			OperationID: effect.OperationID, RequestID: effect.RequestID, OptionID: effect.OptionID,
		})
		if err != nil {
			return true, c.engine.Apply(PermissionDecided{
				OperationID: effect.OperationID, RequestID: effect.RequestID, OptionID: effect.OptionID,
				Ambiguous: provider.ErrorIs(err, provider.ErrorAcceptanceAmbiguous) || receipt.Ambiguous,
			})
		}
		return true, c.engine.Apply(PermissionDecided{
			OperationID: receipt.OperationID, RequestID: receipt.RequestID, OptionID: receipt.OptionID,
			Accepted: receipt.Accepted, Ambiguous: receipt.Ambiguous,
		})
	case DetachLaneEffect:
		return true, c.executeDetach(ctx, effect)
	case DeleteChatEffect:
		c.mu.Lock()
		cleanup := c.chatCleanup
		c.mu.Unlock()
		if cleanup == nil {
			return true, errors.New("durable chat deletion has no cleanup executor")
		}
		if err := cleanup(ctx, effect.TabID, effect.ChatID, effect.OperationID); err != nil {
			return true, err
		}
		return true, c.engine.Apply(ChatDeletionCompleted{OperationID: effect.OperationID, ChatID: effect.ChatID})
	case BackgroundActionEffect:
		c.mu.Lock()
		execute := c.backgroundExecutor
		c.mu.Unlock()
		if execute == nil {
			return true, c.engine.Apply(BackgroundActionFailed{
				OperationID: effect.Action.OperationID, Kind: provider.ErrorProtocolViolation,
			})
		}
		result, err := execute(ctx, effect.Action.Clone())
		if err != nil {
			return true, c.engine.Apply(BackgroundActionFailed{
				OperationID: effect.Action.OperationID, Kind: providerErrorKind(err),
				Ambiguous: provider.ErrorIs(err, provider.ErrorAcceptanceAmbiguous),
			})
		}
		return true, c.engine.Apply(BackgroundActionCompleted{
			OperationID: effect.Action.OperationID, Result: append(json.RawMessage(nil), result...),
		})
	case RestoreCheckpointEffect:
		c.mu.Lock()
		execute := c.checkpointRestoreExecutor
		c.mu.Unlock()
		if execute == nil {
			return true, c.engine.Apply(CheckpointRestoreFailed{
				OperationID: effect.OperationID, Kind: provider.ErrorProtocolViolation,
			})
		}
		result, err := execute(ctx, c.engine.Snapshot().ChatID, effect.TurnSequence,
			append(json.RawMessage(nil), effect.Checkpoint...), effect.CheckpointDigest, effect.OperationID)
		if err != nil {
			// The executor may have restored an earlier repository before a later
			// repository failed. Without authoritative readback, any dispatched
			// failure is acceptance-ambiguous and must never be repeated.
			applyErr := c.engine.Apply(CheckpointRestoreFailed{
				OperationID: effect.OperationID, Kind: providerErrorKind(err), Ambiguous: true,
			})
			return true, errors.Join(err, applyErr)
		}
		command := CheckpointRestored{
			OperationID: effect.OperationID, TurnSequence: effect.TurnSequence,
			Result: append(json.RawMessage(nil), result...),
		}
		if err := c.engine.Apply(command); err != nil {
			return true, err
		}
		c.publishLifecycle(LifecycleReceipt{
			Kind: LifecycleCheckpointRestored, ChatID: c.engine.Snapshot().ChatID,
			OperationID: effect.OperationID, TurnSequence: effect.TurnSequence,
			Result: append(json.RawMessage(nil), result...),
		})
		return true, nil
	default:
		return true, fmt.Errorf("chat coordinator cannot execute effect %T", effect)
	}
}

func turnCancelledBeforeAdmission(state State, operationID provider.OperationID) bool {
	operationID = provider.NormalizeOperationID(string(operationID))
	for _, event := range state.Ledger {
		if event.OperationID == operationID && event.TerminalState == "cancelled_before_admission" {
			return true
		}
	}
	return false
}

// ExecuteSteer claims one exact live direction without waiting behind unrelated
// actor work in the ordinary outbox drain. The steer remains durable before the
// provider call, and its acknowledgement still settles the same immutable
// operation; only dispatch priority changes.
func (c *Coordinator) ExecuteSteer(ctx context.Context, operationID provider.OperationID) (bool, error) {
	if c == nil {
		return false, errors.New("chat coordinator is unavailable")
	}
	operationID = provider.NormalizeOperationID(string(operationID))
	effect, ok, err := c.engine.ClaimEffect(steerEffectID(operationID))
	if err != nil || !ok {
		return ok, err
	}
	steer, ok := effect.(SteerTurnEffect)
	if !ok {
		return true, fmt.Errorf("steer operation claimed non-steer effect %T", effect)
	}
	return true, c.executeSteer(ctx, steer)
}

func (c *Coordinator) executeSteer(ctx context.Context, effect SteerTurnEffect) error {
	lane, err := c.lane(effect.LaneID)
	if err != nil {
		return c.engine.Apply(SteerFailed{OperationID: effect.OperationID, Kind: providerErrorKind(err)})
	}
	receipt, err := lane.Delivery().Steer(ctx, provider.SteerInput{
		OperationID: effect.OperationID, Turn: effect.Turn, Text: effect.Text,
		Attachments: append([]provider.Attachment(nil), effect.Attachments...),
	})
	if err != nil {
		return c.engine.Apply(SteerFailed{
			OperationID: effect.OperationID, Kind: providerErrorKind(err),
			Unsupported: provider.ErrorIs(err, provider.ErrorUnsupportedCapability),
			Ambiguous:   provider.ErrorIs(err, provider.ErrorAcceptanceAmbiguous) || receipt.Ambiguous,
		})
	}
	if !receipt.Accepted {
		return c.engine.Apply(SteerFailed{OperationID: effect.OperationID, Kind: provider.ErrorAdmissionRejected})
	}
	return c.engine.Apply(SteerAdmitted{
		OperationID: effect.OperationID, Accepted: receipt.Accepted, Consumed: receipt.Consumed,
		AwaitConsumption: receipt.AwaitConsumption,
	})
}

// ExecuteCancel claims and executes one exact cancellation without waiting for
// the ordinary outbox drain. In particular, a provider's slow live-steer
// acknowledgement must never hold Stop behind drainMu. ClaimEffect preserves
// the same durable-before-external-call rule while allowing the cancellation
// notification to preempt that unrelated in-flight effect.
func (c *Coordinator) ExecuteCancel(ctx context.Context, operationID provider.OperationID) (bool, error) {
	if c == nil {
		return false, errors.New("chat coordinator is unavailable")
	}
	effect, ok, err := c.engine.ClaimEffect(cancelEffectID(provider.NormalizeOperationID(string(operationID))))
	if err != nil || !ok {
		return ok, err
	}
	cancel, ok := effect.(CancelTurnEffect)
	if !ok {
		return true, fmt.Errorf("cancel operation claimed non-cancel effect %T", effect)
	}
	return true, c.executeCancel(ctx, cancel)
}

func (c *Coordinator) executeCancel(ctx context.Context, effect CancelTurnEffect) error {
	lane, err := c.lane(effect.LaneID)
	if err != nil {
		// ClaimEffect already made this cancellation durable and dispatched. A
		// detached/resuming lane is a definite local rejection, not permission to
		// leave the exact Stop operation permanently pending. Settle the claimed
		// effect just like every provider-side cancellation failure; later exact
		// resume/readback remains responsible for the foreground turn itself.
		return c.engine.Apply(CancelFailed{OperationID: effect.OperationID, Kind: providerErrorKind(err)})
	}
	if err := lane.Delivery().Cancel(ctx, effect.Turn); err != nil {
		return c.engine.Apply(CancelFailed{OperationID: effect.OperationID, Kind: providerErrorKind(err)})
	}
	return c.engine.Apply(CancelAcknowledged{OperationID: effect.OperationID})
}

// ExecuteDetach claims and executes one exact detach effect without draining
// older outbox entries. The frozen close handler uses this targeted path so a
// close request cannot dispatch unrelated queued provider work.
func (c *Coordinator) ExecuteDetach(ctx context.Context, operationID provider.OperationID) (bool, error) {
	if c == nil {
		return false, errors.New("chat coordinator is unavailable")
	}
	// Serialize the targeted claim with the generic outbox drain. A wake that
	// was already queued may otherwise claim this detach first; returning while
	// that local execution is still in flight would expose a durable Dispatched
	// receipt even though the exact provider attachment has not finished
	// closing. Holding drainMu means either this call owns the claim, or it waits
	// for the in-process owner to durably settle it before reporting readback.
	c.drainMu.Lock()
	defer c.drainMu.Unlock()
	effect, ok, err := c.engine.ClaimEffect(string(provider.NormalizeOperationID(string(operationID))))
	if err != nil || !ok {
		return ok, err
	}
	detach, ok := effect.(DetachLaneEffect)
	if !ok {
		return true, fmt.Errorf("close operation claimed non-detach effect %T", effect)
	}
	return true, c.executeDetach(ctx, detach)
}

func (c *Coordinator) executeDetach(ctx context.Context, effect DetachLaneEffect) error {
	c.mu.Lock()
	lane := c.lanes[effect.LaneID]
	generation := c.generations[effect.LaneID]
	c.mu.Unlock()
	if lane == nil || generation != effect.ConnectionGeneration || lane.Identity() != laneIdentityForEffect(c.engine.Snapshot(), effect.LaneID) {
		return c.failDetach(effect, provider.ErrorNativeIdentityConflict, false, "provider lane attachment changed before detach")
	}
	if attachment := laneAttachmentSnapshot(lane); attachment != nil && strings.TrimSpace(attachment.ConnectionID) != strings.TrimSpace(effect.ConnectionID) {
		return c.failDetach(effect, provider.ErrorNativeIdentityConflict, false, "provider lane connection changed before detach")
	}
	if err := lane.Detach(ctx); err != nil {
		state := c.engine.Snapshot()
		if detachReceiptForGeneration(state, effect.LaneID, effect.ConnectionGeneration) {
			return nil
		}
		// ClaimEffect durably moved this non-idempotent detach to Dispatched
		// before the provider call. Without a same-generation HostLost or
		// LaneDetached receipt, no provider error can prove the call did not
		// escape. Preserve the original error for diagnostics, but settle the
		// actor operation as acceptance-ambiguous so recovery can never replay it.
		return errors.Join(err, c.failDetach(effect, provider.ErrorAcceptanceAmbiguous, true, "provider lane detach failed without a durable receipt"))
	}
	// A compliant lane publishes LaneDetached and the event forwarder commits
	// HostLost before Detach returns. A small compatibility lane may only close
	// its transport; settle the same exact generation synchronously in that case.
	if !detachReceiptForGeneration(c.engine.Snapshot(), effect.LaneID, effect.ConnectionGeneration) {
		if err := c.engine.Apply(HostLost{LaneID: effect.LaneID, ConnectionGeneration: effect.ConnectionGeneration}); err != nil {
			return err
		}
	}
	return nil
}

func (c *Coordinator) failDetach(effect DetachLaneEffect, kind provider.ErrorKind, ambiguous bool, message string) error {
	if kind == "" {
		kind = provider.ErrorNativeIdentityConflict
	}
	if err := c.engine.Apply(DetachLaneFailed{
		OperationID: effect.OperationID, LaneID: effect.LaneID, ConnectionID: effect.ConnectionID,
		ConnectionGeneration: effect.ConnectionGeneration, Kind: kind, Ambiguous: ambiguous,
	}); err != nil {
		return errors.Join(errors.New(message), err)
	}
	return errors.New(message)
}

func laneIdentityForEffect(state State, laneID provider.LaneID) provider.LaneIdentity {
	if lane, ok := state.Lanes[laneID]; ok {
		return lane.Identity
	}
	return provider.LaneIdentity{}
}

func laneAttachmentSnapshot(lane provider.Lane) *provider.LaneAttachmentSnapshot {
	source, ok := lane.(provider.LaneAttachmentSource)
	if !ok {
		return nil
	}
	snapshot := source.AttachmentSnapshot().Clone()
	return &snapshot
}

// Drain executes all currently executable outbox effects. It stops when the
// reducer reaches a provider/user boundary or on the first failure.
func (c *Coordinator) Drain(ctx context.Context) error {
	c.drainMu.Lock()
	defer c.drainMu.Unlock()
	for {
		executed, err := c.ExecuteNext(ctx)
		if err != nil {
			return err
		}
		if !executed {
			return nil
		}
	}
}

func (c *Coordinator) runEffectWake() {
	defer c.effectWG.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.effectWake:
			// Provider receipts can make the next queued operation executable.
			// Run it outside the event-forwarder goroutine: managerLane delivery
			// backpressures until that same forwarder durably acknowledges start,
			// so synchronous execution here would deadlock.
			_ = c.Drain(c.ctx)
		}
	}
}

func (c *Coordinator) wakeEffects() {
	select {
	case c.effectWake <- struct{}{}:
	default:
	}
}

// Wake schedules recovery/execution of already-durable effects. It never
// creates an effect and is safe to call after opening an actor from disk.
func (c *Coordinator) Wake() {
	if c != nil {
		c.wakeEffects()
	}
}

func (c *Coordinator) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	lanes := make([]provider.Lane, 0, len(c.lanes))
	for _, lane := range c.lanes {
		lanes = append(lanes, lane)
	}
	c.mu.Unlock()
	var errs []error
	for _, lane := range lanes {
		if err := lane.Detach(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	// Detach while the event forwarders are still alive. A provider lane is
	// allowed to backpressure rather than lose normalized events, and its final
	// detached event therefore needs a live durable consumer. Cancellation comes
	// only after every lane has been asked to close its stream.
	if c.cancel != nil {
		c.cancel()
	}
	// Cancellation only requests that event forwarders stop. Wait for the
	// durable receipt before returning so no already-selected provider event can
	// write chat state after its owner has reported itself closed.
	c.eventsWG.Wait()
	c.effectWG.Wait()
	c.mu.Lock()
	c.lanes = make(map[provider.LaneID]provider.Lane)
	c.generations = make(map[provider.LaneID]uint64)
	c.mu.Unlock()
	return errors.Join(errs...)
}

func (c *Coordinator) definition(id provider.ID) (provider.Definition, error) {
	definition, ok := c.registry.Resolve(id)
	if !ok {
		return provider.Definition{}, &provider.Error{Kind: provider.ErrorProviderUnavailable, Message: fmt.Sprintf("provider %q is not registered", id)}
	}
	return definition, nil
}

func (c *Coordinator) lane(id provider.LaneID) (provider.Lane, error) {
	c.mu.Lock()
	lane := c.lanes[id]
	c.mu.Unlock()
	if lane == nil {
		return nil, &provider.Error{Kind: provider.ErrorTransientTransport, Message: "provider lane is not attached"}
	}
	return lane, nil
}

func (c *Coordinator) attachLane(lane provider.Lane, generation uint64) {
	id := lane.Identity().ID
	c.mu.Lock()
	previous := c.lanes[id]
	c.lanes[id] = lane
	c.generations[id] = generation
	c.mu.Unlock()
	if previous != nil && previous != lane {
		_ = previous.Detach(context.Background())
	}
}

func (c *Coordinator) startLaneEvents(lane provider.Lane, generation uint64) {
	if durable, ok := lane.(provider.DurableEventDelivery); ok {
		durable.RequireDurableEventCommits()
	}
	c.eventsWG.Add(1)
	go func() {
		defer c.eventsWG.Done()
		c.forwardEvents(lane, generation)
	}()
}

func (c *Coordinator) discardLane(lane provider.Lane) {
	if lane == nil {
		return
	}
	c.mu.Lock()
	id := lane.Identity().ID
	if c.lanes[id] == lane {
		delete(c.lanes, id)
		delete(c.generations, id)
	}
	c.mu.Unlock()
	_ = lane.Detach(context.Background())
}

func (c *Coordinator) forwardEvents(lane provider.Lane, generation uint64) {
	for {
		var event provider.Event
		var ok bool
		select {
		case <-c.ctx.Done():
			return
		case event, ok = <-lane.Events():
			if !ok {
				// A normal detach is durably observed above before the adapter closes
				// this stream. A close while the same generation is still registered
				// is therefore host loss, not a successful quiet shutdown.
				c.mu.Lock()
				current := c.lanes[lane.Identity().ID] == lane && c.generations[lane.Identity().ID] == generation
				if current {
					delete(c.lanes, lane.Identity().ID)
					delete(c.generations, lane.Identity().ID)
				}
				c.mu.Unlock()
				if current {
					_ = c.engine.Apply(LaneProtocolFailed{LaneID: lane.Identity().ID, ConnectionGeneration: generation})
					c.wakeEffects()
				}
				return
			}
		}
		c.mu.Lock()
		current := c.lanes[lane.Identity().ID] == lane && c.generations[lane.Identity().ID] == generation
		c.mu.Unlock()
		if !current {
			acknowledgeProviderEvent(lane, event.Identity.Sequence, errors.New("provider event belongs to a stale attachment"))
			return
		}
		if err := c.engine.Apply(ProviderEventReceived{ConnectionGeneration: generation, Event: event}); err != nil {
			acknowledgeProviderEvent(lane, event.Identity.Sequence, err)
			// The authoritative state rejected malformed, duplicate, or stale
			// adapter output. Break the attachment in durable state and detach it;
			// continuing after a protocol violation could mutate ownership out of
			// order.
			_ = c.engine.Apply(LaneProtocolFailed{LaneID: lane.Identity().ID, ConnectionGeneration: generation})
			c.discardLane(lane)
			return
		}
		acknowledgeProviderEvent(lane, event.Identity.Sequence, nil)
		c.wakeEffects()
		if event.Kind == provider.EventLaneDetached {
			// The adapter has already destroyed this disposable attachment. Remove
			// only the coordinator reference; calling Detach again after an exact
			// resume can close the new host that inherited the same immutable
			// native thread id.
			c.mu.Lock()
			if c.lanes[lane.Identity().ID] == lane && c.generations[lane.Identity().ID] == generation {
				delete(c.lanes, lane.Identity().ID)
				delete(c.generations, lane.Identity().ID)
			}
			c.mu.Unlock()
			return
		}
	}
}

func acknowledgeProviderEvent(lane provider.Lane, sequence uint64, err error) {
	if durable, ok := lane.(provider.DurableEventDelivery); ok {
		durable.AcknowledgeDurableEvent(sequence, err)
	}
}

func (c *Coordinator) applyLaneFailure(laneID provider.LaneID, err error) error {
	before := c.engine.Snapshot()
	operationID := provider.OperationID("")
	if before.Foreground != nil && before.Foreground.LaneID == laneID && before.Foreground.Status == ForegroundDispatching {
		operationID = before.Foreground.OperationID
	} else {
		for _, queued := range before.Queue {
			if queued.LaneID == laneID && strings.TrimSpace(queued.Presentation.QueueID) == "" {
				operationID = queued.OperationID
				break
			}
		}
	}
	kind := providerErrorKind(err)
	applyErr := c.engine.Apply(LaneOpenFailed{
		LaneID: laneID, Kind: kind, Ambiguous: provider.ErrorIs(err, provider.ErrorAcceptanceAmbiguous),
	})
	if applyErr != nil {
		return errors.Join(err, applyErr)
	}
	if operationID != "" && !provider.ErrorIs(err, provider.ErrorAcceptanceAmbiguous) {
		c.publishLifecycle(LifecycleReceipt{
			Kind: LifecycleTurnAdmissionFailed, ChatID: before.ChatID,
			LaneID: laneID, OperationID: operationID, Terminal: true, Status: "failed",
		})
	}
	return err
}

func providerErrorKind(err error) provider.ErrorKind {
	var typed *provider.Error
	if errors.As(err, &typed) && typed.Kind != "" {
		return typed.Kind
	}
	return provider.ErrorTransientTransport
}
