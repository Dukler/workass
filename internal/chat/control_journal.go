package chat

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
)

// The send path journals its commands instead of checkpointing the actor.
//
// One job:start commits four commands before the prompt reaches the
// provider: the lane selection, the submitted input, the claim of its start
// effect and the provider's admission. Each was a whole-actor checkpoint: on
// a long chat the head alone is megabytes (its outbox, background and lane
// receipts), and every checkpoint re-encoded and rewrote it, about 35-45 ms
// each on a 62k-item chat, so a send spent ~190 ms writing the same head
// four times. These commands now go to the actor's ordered journal, as the
// cancellation commands already do (cancel_journal.go): a small record of
// the command, committed in its own transaction before the command is
// acknowledged, exactly when the checkpoint would have been. Load replays it
// through the same reducer, and a record that cannot be journaled (no
// checkpoint yet, journal budget spent, a ledger change on SQLite, a command
// that would not replay identically) is a checkpoint as before.
//
// Replay must rebuild exactly the state that was acknowledged, so only
// commands whose transition is a pure function of the state and the command
// are eligible: the reducer reads no clock and no randomness on their paths.
// Submit is journaled only with its StartedAt stamped (beginForegroundObligation
// would otherwise read the clock) and TurnAdmitted only for an accepted,
// unambiguous admission (the ambiguous path terminalizes the turn with the
// current time). The record is decoded again before it is written and must
// equal the command, so a field JSON cannot carry falls back to a checkpoint.
const controlJournalVersion = 3

type controlJournalCommand struct {
	ChatID    string               `json:"chatId"`
	Selection *CommitLaneSelection `json:"selection,omitempty"`
	Submit    *Submit              `json:"submit,omitempty"`
	Claim     *ClaimEffect         `json:"claim,omitempty"`
	Admitted  *TurnAdmitted        `json:"admitted,omitempty"`
}

func (c controlJournalCommand) command() (Command, error) {
	var command Command
	count := 0
	set := func(value Command) { command = value; count++ }
	if c.Selection != nil {
		selection := *c.Selection
		if string(selection.Update.ModelControls) == "null" {
			// A nil json.RawMessage encodes as null and decodes as the
			// four bytes "null"; the selection path never sets it.
			selection.Update.ModelControls = nil
		}
		set(selection)
	}
	if c.Submit != nil {
		set(*c.Submit)
	}
	if c.Claim != nil {
		set(*c.Claim)
	}
	if c.Admitted != nil {
		set(*c.Admitted)
	}
	if count != 1 || strings.TrimSpace(c.ChatID) == "" {
		return nil, errors.New("control journal requires one command and its chat owner")
	}
	return command, nil
}

// controlJournalCommandFor wraps an eligible command; ok is false for any
// command this journal does not carry.
func controlJournalCommandFor(state State, command Command) (controlJournalCommand, bool) {
	out := controlJournalCommand{ChatID: state.ChatID}
	if !controlCommandEligible(state, command) {
		return out, false
	}
	switch value := command.(type) {
	case CommitLaneSelection:
		out.Selection = &value
	case Submit:
		out.Submit = &value
	case ClaimEffect:
		out.Claim = &value
	case TurnAdmitted:
		out.Admitted = &value
	default:
		return out, false
	}
	return out, true
}

// controlCommandEligible reports whether command, applied to state, is a
// send-path transition the journal may carry. state is the state the command
// applies to (the replay checks the same predicate against the same state).
func controlCommandEligible(state State, command Command) bool {
	if state.Deleted || strings.TrimSpace(state.ChatID) == "" {
		return false
	}
	switch value := command.(type) {
	case CommitLaneSelection:
		return true
	case Submit:
		return strings.TrimSpace(value.Presentation.StartedAt) != ""
	case ClaimEffect:
		effectID := strings.TrimSpace(value.EffectID)
		for _, entry := range state.Outbox {
			if entry.ID == effectID {
				return entry.Kind == EffectStartTurn
			}
		}
		return false
	case TurnAdmitted:
		return value.Accepted && !value.Ambiguous
	}
	return false
}

type controlCommandStateStore interface {
	// commitControlCommand journals command, whose transition from the
	// state at baseRevision produced next. journaled is false when the
	// caller must checkpoint next instead.
	commitControlCommand(base State, next State, command Command) (journaled bool, err error)
}

func (s FileStore) commitControlCommand(base State, next State, command Command) (bool, error) {
	return commitControlCommandViaJournal(s, base, next, command)
}

func (s *SQLStore) commitControlCommand(base State, next State, command Command) (bool, error) {
	if strings.TrimSpace(next.ChatID) != s.chatID || s.chatID == "" {
		return false, errors.New("chat state store belongs to another chat")
	}
	if s.ledgerChanged(next) {
		// The message rows must equal the actor for readers that never
		// open it: a ledger change is a checkpoint.
		return false, nil
	}
	return commitControlCommandViaJournal(s, base, next, command)
}

func commitControlCommandViaJournal(s journalStateStore, base State, next State, command Command) (bool, error) {
	control, ok := controlJournalCommandFor(base, command)
	if !ok || next.Revision == 0 || base.Revision != next.Revision-1 || next.ChatID != base.ChatID {
		return false, nil
	}
	record := providerEventJournalRecord{
		Version: controlJournalVersion, BaseRevision: base.Revision, Revision: next.Revision, Control: &control,
	}
	payload, err := encodeProviderEventJournalPayload(record)
	if err != nil {
		// An oversized record is still a valid command: checkpoint it.
		return false, nil
	}
	if !controlJournalRoundTrips(payload, command) {
		return false, nil
	}
	return s.appendJournalRecord(record, payload, false)
}

// controlJournalRoundTrips proves replay will reduce exactly command.
func controlJournalRoundTrips(payload []byte, command Command) bool {
	var decoded providerEventJournalRecord
	if err := json.Unmarshal(payload, &decoded); err != nil || decoded.Control == nil {
		return false
	}
	replayed, err := decoded.Control.command()
	if err != nil {
		return false
	}
	return reflect.DeepEqual(replayed, command)
}

// replayControlCommand applies one journaled control command during Load.
func replayControlCommand(state State, control *controlJournalCommand) (State, error) {
	command, err := control.command()
	if err != nil {
		return State{}, err
	}
	if control.ChatID != state.ChatID || !controlCommandEligible(state, command) {
		return State{}, errors.New("control journal changed chat or command ownership")
	}
	state, _, err = Reduce(state, command)
	return state, err
}

var (
	_ controlCommandStateStore = (*SQLStore)(nil)
	_ controlCommandStateStore = FileStore{}
)
