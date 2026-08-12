package chat

import (
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
	e.mu.Lock()
	defer e.mu.Unlock()
	next, _, err := Reduce(e.state, command)
	if err != nil {
		return err
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
		return lane.Phase == LaneRunning && state.Foreground != nil &&
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
	default:
		return false
	}
}

func (e *Engine) Recover() error {
	return e.Apply(RecoverOutbox{})
}

func (e *Engine) Snapshot() State {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state.Clone()
}
