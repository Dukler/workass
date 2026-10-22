package chat

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workass/internal/provider"
)

// Engine is the single serialization boundary for one chat. Apply only commits
// commands. External effects become executable exclusively through ClaimNext,
// after their Pending -> Dispatched transition is durably stored.
type Engine struct {
	mu    sync.Mutex
	state State
	// validated: state is exactly a state State.Validate accepted, because it
	// was installed from a validated transition and committed state is never
	// changed in place. The next transition then skips revalidating it. A
	// journaled provider event validates only what it changed, so it clears
	// this.
	validated           bool
	store               StateStore
	cancelRouting       atomic.Pointer[cancelRoutingSnapshot]
	pendingSteerSettled chan struct{}
	// retired: Retire took this actor out of memory. Its state stays
	// readable for a caller that still holds it, but it commits nothing: the
	// store is the authority until the actor is opened again.
	retired bool
}

// ErrActorRetired is returned by a commit on an engine that Retire took out of
// memory. The caller opens the actor again and retries.
var ErrActorRetired = errors.New("chat actor was closed to free memory; open it again")

// PendingSteerSettled returns a signal for the current steer receipt boundary.
// A nil channel means the actor can admit a new steer now. Callers recheck the
// actor state after the signal because another caller may have admitted first.
func (e *Engine) PendingSteerSettled() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pendingSteerSettled
}

func NewEngine(chatID string) (*Engine, error) {
	state, err := NewState(chatID)
	if err != nil {
		return nil, err
	}
	engine := &Engine{}
	engine.installCommittedState(state)
	return engine, nil
}

func NewDurableEngine(chatID string, store StateStore) (*Engine, error) {
	if store == nil {
		return nil, &storeError{"chat engine requires a durable state store"}
	}
	state, ok, err := store.Load(chatID)
	if err != nil {
		return nil, err
	}
	if !ok {
		state, err = NewState(chatID)
		if err != nil {
			return nil, err
		}
		if err := store.Save(state); err != nil {
			return nil, err
		}
	}
	if ok && recoveryIsNoop(state) {
		// Restart recovery has nothing to settle: opening the actor commits
		// nothing and leaves its revision alone, so opening it at boot or on
		// first use is the same transition. A store whose checkpoint is not
		// the opened state (journal records replayed onto it, rows from before
		// incremental checkpoints) still checkpoints it, at the same revision.
		checkpointer, isCheckpointer := store.(loadCheckpointer)
		if isCheckpointer && checkpointer.needsCheckpointAfterLoad() {
			if err := store.Save(state); err != nil {
				return nil, err
			}
		}
		engine := &Engine{store: store}
		engine.installCommittedState(state)
		// The SQLite load validated this exact state (no journal replayed onto
		// it), or Save just did; a file journal may have been replayed after
		// the load's validation, so the first command validates it.
		engine.validated = isCheckpointer
		return engine, nil
	}
	next, _, err := Reduce(state, RecoverOutbox{})
	if err != nil {
		return nil, err
	}
	engine := &Engine{store: store}
	if err := engine.saveReduced(next); err != nil {
		return nil, err
	}
	engine.installCommittedState(next)
	engine.validated = true
	return engine, nil
}

// loadCheckpointer is a store that can tell, right after Load, that its
// persisted checkpoint is not the loaded state (journal records were replayed
// onto it, or its rows predate the current checkpoint format).
type loadCheckpointer interface {
	needsCheckpointAfterLoad() bool
}

// retireCheckpointer is a store that folds journal records into a checkpoint
// before its actor leaves memory, so the next open reads one checkpoint.
type retireCheckpointer interface {
	checkpointForRetire(State) error
}

// Retire takes a quiescent actor out of memory. Under the engine lock, so no
// commit can interleave, it checks Quiescent and asks ready whether the state
// may be left closed; if so it folds any journal into a checkpoint, stops
// accepting commits (ErrActorRetired), and returns the summary every read
// surface uses until the actor is opened again. ready sees the state
// read-only.
func (e *Engine) Retire(ready func(*State) bool) (ActorSummary, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.retired {
		return ActorSummary{}, false, ErrActorRetired
	}
	if ok, _ := Quiescent(e.state); !ok || (ready != nil && !ready(&e.state)) {
		return ActorSummary{}, false, nil
	}
	if checkpointer, isCheckpointer := e.store.(retireCheckpointer); isCheckpointer {
		if err := checkpointer.checkpointForRetire(e.state); err != nil {
			return ActorSummary{}, false, err
		}
	}
	e.retired = true
	return summarizeResident(&e.state), true, nil
}

// Summary is the resident actor's ActorSummary.
func (e *Engine) Summary() ActorSummary {
	e.mu.Lock()
	defer e.mu.Unlock()
	return summarizeResident(&e.state)
}

type storeError struct{ message string }

func (e *storeError) Error() string { return e.message }

func (e *Engine) Apply(command Command) error {
	return e.ApplyPrepared(command, nil)
}

// ApplyPrepared validates a command against the exact locked actor state,
// performs local durable preparation, and only then publishes the new actor
// snapshot. Preparation is reserved for content-addressed attachment writes;
// provider calls and other externally visible effects must remain in the
// durable outbox and execute through Coordinator after this commit.
func (e *Engine) ApplyPrepared(command Command, prepare func() error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.retired {
		return ErrActorRetired
	}
	if admission, ok := command.(TurnAdmitted); ok && prepare == nil && !e.state.Deleted {
		// StartTurn commits admission before publishing provider output, then
		// returns the same receipt to the coordinator. That second observation
		// is already durable. Match the reducer's exact no-op before cloning or
		// checkpointing the whole history on every turn.
		foreground := e.state.Foreground
		if foreground != nil && foreground.OperationID == admission.OperationID &&
			foreground.Status == ForegroundRunning && foreground.Turn == admission.Turn &&
			admission.Accepted && !admission.Ambiguous {
			return nil
		}
	}
	if observation, ok := command.(ReconcileObligation); ok && prepare == nil && !e.state.Deleted {
		// A periodic observation has no operation receipt to commit. Run the
		// obligation-only reducer on an isolated status copy first: unchanged
		// evidence must not clone, validate or rewrite the entire transcript.
		probe := State{Foreground: e.state.Foreground, Obligation: e.state.Obligation.Clone()}
		if err := reduceReconcileObligation(&probe, observation); err != nil {
			return err
		}
		if probe.Obligation == nil || *probe.Obligation == *e.state.Obligation {
			return nil
		}
	}
	if save, ok := command.(SaveRuntimeControls); ok && prepare == nil && !e.state.Deleted {
		// An exact replay only reads its receipt; persisting it again advanced
		// the actor revision on every retry.
		if receipt, exists := e.state.RuntimeControlMutationReceipts[provider.NormalizeOperationID(string(save.OperationID))]; exists &&
			receipt.Digest == strings.TrimSpace(save.Digest) {
			return nil
		}
	}
	if update, ok := command.(UpdateEnvironment); ok && prepare == nil && !e.state.Deleted && environmentUpdateUnchanged(e.state, update) {
		// Re-observing the same Entorno (every provider session bind, and
		// turns that change no file) would only advance an unread counter.
		return nil
	}
	if prepare == nil && !e.state.Deleted && externalMutationReplayUnchanged(e.state, command) {
		// Re-recording a journaled external mutation, or a receipt it already
		// holds, is accepted and changes nothing but the revision. Opening a
		// chat re-hosts its visualizations and a browser retry re-records its
		// operation; neither may rewrite the whole actor.
		return nil
	}
	providerStore, hasProviderStore := e.store.(providerEventStateStore)
	providerEvent, isProviderEvent := command.(ProviderEventReceived)
	useProviderEventJournal := prepare == nil && hasProviderStore && isProviderEvent && journalableProviderEvent(providerEvent.Event.Kind)
	var (
		next State
		err  error
		undo = func() {}
	)
	if useProviderEventJournal {
		// The journaled reducer shares unchanged structure with the committed
		// state; stop vouching for it before running.
		e.validated = false
		next, undo, err = reduceJournaledProviderEventSharingTools(e.state, providerEvent)
	} else {
		next, _, err = reduce(e.state, command, e.validated)
	}
	if err != nil {
		return err
	}
	if prepare != nil {
		if err := prepare(); err != nil {
			undo()
			return err
		}
	}
	if e.store != nil {
		var persistErr error
		if hasProviderStore && isProviderEvent {
			persistErr = providerStore.commitProviderEvent(e.state.Revision, next, providerEvent)
		} else if prepare == nil {
			persistErr = e.persistCommand(next, command)
		} else {
			persistErr = e.saveReduced(next)
		}
		if persistErr != nil {
			undo()
			return persistErr
		}
	}
	e.installCommittedState(next)
	e.validated = !useProviderEventJournal
	return nil
}

// saveReduced checkpoints a state reduce has just validated. Both stores
// validate on Save to refuse any caller's invalid state; for the engine's own
// transition that is the same pure check run an instant earlier on the same
// value. Other stores (wrappers in tests) keep their Save.
func (e *Engine) saveReduced(next State) error {
	switch store := e.store.(type) {
	case *SQLStore:
		return store.saveValidated(next)
	case FileStore:
		return store.saveValidated(next)
	}
	return e.store.Save(next)
}

// ClaimNext persists the Pending -> Dispatched transition before returning an
// external effect. Callers must execute only claimed effects, never the effects
// initially reported by Apply.
func (e *Engine) ClaimNext() (Effect, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.retired {
		return nil, false, nil
	}
	for _, entry := range e.state.Outbox {
		if entry.Status != OutboxPending || !outboxEntryExecutable(e.state, entry) {
			continue
		}
		next, effects, err := reduce(e.state, ClaimEffect{EffectID: entry.ID}, e.validated)
		if err != nil {
			return nil, false, err
		}
		if len(effects) != 1 {
			return nil, false, &storeError{"claim did not produce exactly one provider effect"}
		}
		if e.store != nil {
			if err := e.persistCommand(next, ClaimEffect{EffectID: entry.ID}); err != nil {
				return nil, false, err
			}
		}
		e.installCommittedState(next)
		e.validated = true
		return effects[0], true, nil
	}
	return nil, false, nil
}

// ClaimEffect claims one exact durable effect without draining older effects
// from the chat outbox. Close requests use this narrow boundary so a detach
// cannot accidentally dispatch an unrelated queued provider operation first.
// A false result means the requested effect is not currently pending and
// executable; callers must not attempt the provider call themselves.
func (e *Engine) ClaimEffect(effectID string) (Effect, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.retired {
		return nil, false, nil
	}
	effectID = strings.TrimSpace(effectID)
	for index := range e.state.Outbox {
		entry := e.state.Outbox[index]
		if entry.ID != effectID || entry.Status != OutboxPending || !outboxEntryDirectClaimable(e.state, entry) {
			continue
		}
		next, effects, err := reduce(e.state, ClaimEffect{EffectID: effectID}, e.validated)
		if err != nil {
			return nil, false, err
		}
		if len(effects) != 1 {
			return nil, false, &storeError{"claim did not produce exactly one provider effect"}
		}
		if e.store != nil {
			if err := e.persistCommand(next, ClaimEffect{EffectID: effectID}); err != nil {
				return nil, false, err
			}
		}
		e.installCommittedState(next)
		e.validated = true
		return effects[0], true, nil
	}
	return nil, false, nil
}

// outboxEntryExecutable encodes effect dependencies in one place. Persistence
// order alone is insufficient after restart: an older Pending prompt can
// precede the newly required exact-resume receipt in the file. The lane phase
// makes resume executable first without rewriting either immutable outbox id.
func outboxEntryExecutable(state State, entry OutboxEntry) bool {
	if entry.Kind == EffectDeleteChat {
		return state.Deleted && strings.TrimSpace(entry.ChatID) == state.ChatID
	}
	if entry.Kind == EffectBackground {
		return !state.Deleted && entry.Background != nil && entry.Background.OperationID == entry.OperationID
	}
	if entry.Kind == EffectCheckpointRestore {
		return !state.Deleted && entry.OperationID != "" && entry.TurnSequence > 0
	}
	// External browser mutations are claimed by their exact MCP actor boundary,
	// never by the provider coordinator's generic drain. Their dedicated executor
	// lock serializes claim/dispatch/readback while the actor state lock is held
	// only for the durable reducer transitions.
	if entry.Kind == EffectExternalMutation {
		return false
	}
	lane, ok := state.Lanes[entry.LaneID]
	if !ok {
		return false
	}
	switch entry.Kind {
	case EffectCreateLane:
		return lane.Phase == LaneCreating
	case EffectResumeLane:
		return lane.Phase == LaneResuming
	case EffectImportContext:
		return lane.Phase == LaneImporting
	case EffectStartTurn:
		laneCanDispatch := lane.Phase == LaneRunning ||
			(lane.Phase == LaneCreating && lane.Provision != nil && lane.Creation.DeferredUntilInput)
		attachedForDispatch := lane.Provision == nil || lane.Attachment != nil
		return laneCanDispatch && attachedForDispatch && state.Foreground != nil &&
			state.Foreground.LaneID == entry.LaneID && state.Foreground.OperationID == entry.OperationID &&
			state.Foreground.Status == ForegroundDispatching
	case EffectSteerTurn:
		laneCanSteer := lane.Phase == LaneRunning ||
			(lane.Phase == LaneCreating && lane.Provision != nil && lane.Creation.DeferredUntilInput && lane.Thread.IsZero() && lane.Attachment != nil)
		pending := state.PendingSteerFor(entry.OperationID)
		return laneCanSteer && pending != nil && pending.LaneID == entry.LaneID && pending.Status == SteerDispatching
	case EffectCancelTurn:
		return state.PendingCancel != nil && state.PendingCancel.LaneID == entry.LaneID &&
			state.PendingCancel.OperationID == entry.OperationID
	case EffectPermission:
		permission, exists := state.Permissions[entry.RequestID]
		return exists && permission.Owner.LaneID == entry.LaneID
	case EffectDetachLane:
		return lane.ConnectionGeneration == entry.Generation && lane.Attachment != nil &&
			strings.TrimSpace(lane.Attachment.ConnectionID) == strings.TrimSpace(entry.ConnectionID) &&
			lane.Owner == entry.Owner
	default:
		return false
	}
}

func outboxEntryDirectClaimable(state State, entry OutboxEntry) bool {
	if entry.Kind == EffectExternalMutation {
		return !state.Deleted && state.Initialized &&
			strings.TrimSpace(entry.ChatID) == state.ChatID &&
			strings.TrimSpace(entry.TabID) != "" && entry.TabID == state.Presentation.TabID
	}
	return outboxEntryExecutable(state, entry)
}

func (e *Engine) Recover() error {
	return e.Apply(RecoverOutbox{})
}

func (e *Engine) Snapshot() State {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state.Clone()
}

// ReadSnapshot is Snapshot for a reader: every field is an isolated copy
// except the ledger rows, which are shared with the committed state instead of
// deep-copied (the bulk of a long chat; see State.cloneSharingLedger).
// Committed rows are never modified in place, so the rows can be read at any
// time, but the caller must treat them, and everything they reference, as
// read-only.
func (e *Engine) ReadSnapshot() State {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state.cloneSharingLedger()
}

// View runs read under the engine lock with the committed state itself, not
// a copy: Snapshot deep-copies the whole history, which on a long chat costs
// more than most reads that need a handful of fields. read may copy values
// out. It must not modify or keep anything reachable from the state (its
// maps, slices and pointers are the live committed state) and must not call
// back into this engine.
func (e *Engine) View(read func(State)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	read(e.state)
}

// HasCancellableJob locates Stop's owner without copying transcript history or
// taking the runtime's provider-attachment mutex. This is only a routing hint;
// cancellation revalidates the exact job under the owning actor's lock.
func (e *Engine) HasCancellableJob(jobID string) bool {
	routing := e.cancelRouting.Load()
	if routing == nil || jobID == "" {
		return false
	}
	_, found := routing.jobs[jobID]
	return found
}

// IdentitySnapshot is the constant-size identity needed to locate one actor by
// its renderer tab. It deliberately excludes ledger and provider-turn state.
type IdentitySnapshot struct {
	ChatID  string
	TabID   string
	Deleted bool
}

func (e *Engine) IdentitySnapshot() IdentitySnapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return IdentitySnapshot{
		ChatID: e.state.ChatID, TabID: e.state.Presentation.TabID, Deleted: e.state.Deleted,
	}
}

// ReadProjectionSnapshot is an isolated, bounded view for renderer reads. The
// ledger slice represents [LedgerOffset, LedgerCount); LedgerCount preserves
// exact history metadata without copying the omitted prefix. This has no write
// path and no authority over provider turns.
type ReadProjectionSnapshot struct {
	LastActivityAt int64
	State          State
	LedgerOffset   int
	LedgerCount    int
}

func (e *Engine) ReadProjectionSnapshot(ledgerTail int) ReadProjectionSnapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ledgerTail < 0 {
		ledgerTail = 0
	}
	ledgerCount := len(e.state.Ledger)
	ledgerOffset := ledgerCount - min(ledgerCount, ledgerTail)
	return ReadProjectionSnapshot{
		State: e.state.cloneReadProjection(ledgerOffset), LedgerOffset: ledgerOffset, LedgerCount: ledgerCount,
	}
}

// ReadSessionProjectionSnapshot omits committed bodies for unselected idle
// chats at the copy boundary, rather than cloning their tool history only to
// discard it during serialization. Selection and foreground are tested under
// the same lock as the snapshot, so a newly running turn always carries history.
func (e *Engine) ReadSessionProjectionSnapshot(activeTabID string, ledgerTail int) ReadProjectionSnapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	ledgerCount := len(e.state.Ledger)
	if ledgerTail < 0 {
		ledgerTail = 0
	}
	ledgerOffset := ledgerCount - min(ledgerCount, ledgerTail)
	if e.state.Presentation.TabID != activeTabID && e.state.Foreground == nil {
		ledgerOffset = ledgerCount
	}
	snapshot := ReadProjectionSnapshot{
		State: e.state.cloneReadProjection(ledgerOffset), LedgerOffset: ledgerOffset, LedgerCount: ledgerCount,
	}
	// Metadata-only reads still need the lifecycle timestamp. Inspect scalar
	// timestamps directly; none of the ledger's bodies or tool payloads escape.
	if ledgerOffset == ledgerCount {
		// Keep the same timestamp window as the previous bounded tail. Legacy
		// undated history must not turn this cheap read into a whole-ledger scan.
		activityStart := ledgerCount - min(ledgerCount, ledgerTail)
		for i := ledgerCount - 1; i >= activityStart; i-- {
			if at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(e.state.Ledger[i].At)); err == nil {
				snapshot.LastActivityAt = at.UnixMilli()
				break
			}
		}
	}
	return snapshot
}

// ActivitySnapshot contains only the actor-owned state used by background-work
// and permission reads. It cannot be applied as an actor State, and copying it
// never traverses committed messages, tool history, coverage or receipts.
type ActivitySnapshot struct {
	ChatID      string
	TabID       string
	Deleted     bool
	Background  map[string]BackgroundState
	Permissions map[string]PermissionState
	Obligation  *ObligationState
	Lanes       map[provider.LaneID]ActivityLaneSnapshot
}

type ActivityLaneSnapshot struct {
	ProviderID provider.ID
	SessionID  string
}

func (e *Engine) ReadActivitySnapshot() ActivitySnapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return activitySnapshotOf(&e.state)
}

// LedgerPageSnapshot is one immutable page immediately before a stable message
// identity. It is a renderer read primitive only; actor persistence and provider
// delivery continue to consume the complete canonical State.
type LedgerPageSnapshot struct {
	Events      []LedgerEvent
	Start       int
	End         int
	LedgerCount int
	Deleted     bool
}

func (e *Engine) ReadLedgerPageBefore(beforeMessageID string, limit int) (LedgerPageSnapshot, error) {
	beforeMessageID = strings.TrimSpace(beforeMessageID)
	if beforeMessageID == "" {
		return LedgerPageSnapshot{}, errors.New("ledger page requires a stable before-message identity")
	}
	if limit < 1 {
		return LedgerPageSnapshot{}, errors.New("ledger page limit must be positive")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for index := len(e.state.Ledger) - 1; index >= 0; index-- {
		if e.state.Ledger[index].MessageID == beforeMessageID {
			return e.readMessagesLocked(uint64(index+1), limit)
		}
	}
	return LedgerPageSnapshot{}, errors.New("ledger page boundary does not exist")
}

// DigestSnapshot is the bounded, body-free view used by the five-second health
// heartbeat. Snapshot intentionally deep-clones the complete semantic ledger;
// doing that for every idle ping made large histories consume a full CPU core
// and several transient gigabytes merely to return counts and stable ids.
type DigestSnapshot struct {
	Initialized            bool
	Deleted                bool
	ChatID                 string
	TabID                  string
	ActorRevision          uint64
	PresentationRevision   uint64
	RunningJobID           string
	LastMessageID          string
	MessageCount           int
	QueueLen               int
	QueueHeadID            string
	AgentQueueRevision     uint64
	RuntimeControlRevision uint64
	ProviderID             string
	CurrentModelID         string
	CurrentModeID          string
	PendingPermissionIDs   []string
}

func (e *Engine) DigestSnapshot() DigestSnapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	view := e.state.PresentationState()
	lastMessageID := ""
	if count := len(view.Ledger); count > 0 {
		lastMessageID = view.Ledger[count-1].MessageID
	}
	return digestSnapshotOf(&view, len(view.Ledger), lastMessageID)
}
