package chat

import (
	"sort"
	"strings"
	"sync"
)

// Engine is the single serialization boundary for one chat. Apply only commits
// commands. External effects become executable exclusively through ClaimNext,
// after their Pending -> Dispatched transition is durably stored.
type Engine struct {
	mu    sync.Mutex
	state State
	store StateStore
}

func NewEngine(chatID string) (*Engine, error) {
	state, err := NewState(chatID)
	if err != nil {
		return nil, err
	}
	return &Engine{state: state}, nil
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
	next, _, err := Reduce(state, RecoverOutbox{})
	if err != nil {
		return nil, err
	}
	if err := store.Save(next); err != nil {
		return nil, err
	}
	return &Engine{state: next, store: store}, nil
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
	next, _, err := Reduce(e.state, command)
	if err != nil {
		return err
	}
	if prepare != nil {
		if err := prepare(); err != nil {
			return err
		}
	}
	if e.store != nil {
		if err := e.store.Save(next); err != nil {
			return err
		}
	}
	e.state = next
	return nil
}

// ClaimNext persists the Pending -> Dispatched transition before returning an
// external effect. Callers must execute only claimed effects, never the effects
// initially reported by Apply.
func (e *Engine) ClaimNext() (Effect, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, entry := range e.state.Outbox {
		if entry.Status != OutboxPending || !outboxEntryExecutable(e.state, entry) {
			continue
		}
		next, effects, err := Reduce(e.state, ClaimEffect{EffectID: entry.ID})
		if err != nil {
			return nil, false, err
		}
		if len(effects) != 1 {
			return nil, false, &storeError{"claim did not produce exactly one provider effect"}
		}
		if e.store != nil {
			if err := e.store.Save(next); err != nil {
				return nil, false, err
			}
		}
		e.state = next
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
	effectID = strings.TrimSpace(effectID)
	for index := range e.state.Outbox {
		entry := e.state.Outbox[index]
		if entry.ID != effectID || entry.Status != OutboxPending || !outboxEntryDirectClaimable(e.state, entry) {
			continue
		}
		next, effects, err := Reduce(e.state, ClaimEffect{EffectID: effectID})
		if err != nil {
			return nil, false, err
		}
		if len(effects) != 1 {
			return nil, false, &storeError{"claim did not produce exactly one provider effect"}
		}
		if e.store != nil {
			if err := e.store.Save(next); err != nil {
				return nil, false, err
			}
		}
		e.state = next
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
	// never by the provider coordinator's generic drain. The caller must retain
	// the actor lock across the claim and the transient browser dispatch.
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
	case EffectReconcileTurn:
		return lane.Phase == LaneReconciling && state.Foreground != nil &&
			state.Foreground.LaneID == entry.LaneID && state.Foreground.OperationID == entry.OperationID &&
			state.Foreground.Status == ForegroundReconciling
	case EffectSteerTurn:
		return lane.Phase == LaneRunning && state.PendingSteer != nil &&
			state.PendingSteer.LaneID == entry.LaneID && state.PendingSteer.OperationID == entry.OperationID &&
			state.PendingSteer.Status == SteerDispatching
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
	QueuePaused            bool
	QueuePauseRevision     uint64
	RuntimeControlRevision uint64
	ProviderID             string
	CurrentModelID         string
	CurrentModeID          string
	PendingPermissionIDs   []string
}

func (e *Engine) DigestSnapshot() DigestSnapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	state := &e.state
	digest := DigestSnapshot{
		Initialized:            state.Initialized,
		Deleted:                state.Deleted,
		ChatID:                 state.ChatID,
		TabID:                  state.Presentation.TabID,
		ActorRevision:          state.Revision,
		PresentationRevision:   state.Presentation.PresentationRevision,
		AgentQueueRevision:     state.Presentation.AgentQueueRevision,
		QueuePaused:            state.QueueControl.Paused,
		QueuePauseRevision:     state.QueueControl.Revision,
		RuntimeControlRevision: state.Presentation.RuntimeControlRevision,
		ProviderID:             string(state.Presentation.ProviderID),
		CurrentModelID:         state.Presentation.CurrentModelID,
		CurrentModeID:          state.Presentation.CurrentModeID,
		QueueLen:               len(state.StagedQueue) + len(state.Queue),
	}
	if len(state.StagedQueue) > 0 {
		digest.QueueHeadID = state.StagedQueue[0].ID
	} else if len(state.Queue) > 0 {
		digest.QueueHeadID = strings.TrimSpace(state.Queue[0].Presentation.QueueID)
		if digest.QueueHeadID == "" {
			digest.QueueHeadID = string(state.Queue[0].OperationID)
		}
	}

	seen := make(map[string]struct{}, len(state.Ledger)+4)
	addMessage := func(id string) {
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		digest.MessageCount++
		digest.LastMessageID = id
	}
	for _, event := range state.Ledger {
		addMessage(event.MessageID)
	}
	if foreground := state.Foreground; foreground != nil {
		digest.RunningJobID = foreground.Turn.NativeID
		if !foreground.UserConsumed {
			userID := strings.TrimSpace(foreground.Input.Presentation.UserMessageID)
			if userID == "" {
				userID = "message:" + string(foreground.OperationID) + ":user"
			}
			addMessage(userID)
		}
		addMessage(strings.TrimSpace(foreground.CurrentAssistantMessageID))
		if pending := state.PendingSteer; pending != nil {
			userID := strings.TrimSpace(pending.Presentation.UserMessageID)
			if userID == "" {
				userID = "message:" + string(pending.OperationID) + ":user"
			}
			addMessage(userID)
			continuationID := strings.TrimSpace(pending.Presentation.AssistantMessageID)
			if continuationID == "" {
				continuationID = foreground.RootAssistantMessageID + "~after~" + string(pending.OperationID)
			}
			addMessage(continuationID)
		}
	}
	for id, permission := range state.Permissions {
		if !strings.EqualFold(strings.TrimSpace(permission.Event.Status), "resolved") {
			digest.PendingPermissionIDs = append(digest.PendingPermissionIDs, id)
		}
	}
	sort.Strings(digest.PendingPermissionIDs)
	return digest
}
