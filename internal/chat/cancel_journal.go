package chat

import (
	"errors"
	"fmt"
	"strings"
)

const cancelJournalVersion = 2

func (r providerEventJournalRecord) validateCommand() error {
	switch r.Version {
	case providerEventJournalVersion:
		if r.Cancel != nil || !providerEventJournalEligible(r.Command.Event.Kind) {
			return errors.New("provider event is not eligible for the durable stream journal")
		}
	case cancelJournalVersion:
		if r.Cancel == nil || r.Command.Event.Kind != "" {
			return errors.New("cancel journal record contains another command")
		}
		_, err := r.Cancel.command()
		return err
	default:
		return fmt.Errorf("unsupported provider event journal version %d", r.Version)
	}
	return nil
}

type cancelCommandStateStore interface {
	commitCancelCommand(baseRevision uint64, next State, command Command) error
}

// Cancellation uses the actor's existing ordered journal. These records contain
// only the intent/dispatch/receipt, never transcript bodies. Both reducer
// transitions still commit before the provider's cancellation can be sent.
type cancelJournalCommand struct {
	ChatID       string              `json:"chatId"`
	Intent       *CancelTurn         `json:"intent,omitempty"`
	Claim        *ClaimEffect        `json:"claim,omitempty"`
	Acknowledged *CancelAcknowledged `json:"acknowledged,omitempty"`
	Failed       *CancelFailed       `json:"failed,omitempty"`
	Pending      *CancelPendingTurn  `json:"pending,omitempty"`
}

func cancellationCommand(state State, command Command) bool {
	switch value := command.(type) {
	case CancelTurn, CancelAcknowledged, CancelFailed, CancelPendingTurn:
		return true
	case ClaimEffect:
		for _, entry := range state.Outbox {
			if entry.ID == strings.TrimSpace(value.EffectID) {
				return entry.Kind == EffectCancelTurn
			}
		}
	}
	return false
}

func (c cancelJournalCommand) command() (Command, error) {
	var command Command
	count := 0
	set := func(value Command) { command = value; count++ }
	if c.Intent != nil {
		set(*c.Intent)
	}
	if c.Claim != nil {
		set(*c.Claim)
	}
	if c.Acknowledged != nil {
		set(*c.Acknowledged)
	}
	if c.Failed != nil {
		set(*c.Failed)
	}
	if c.Pending != nil {
		set(*c.Pending)
	}
	if count != 1 || strings.TrimSpace(c.ChatID) == "" {
		return nil, errors.New("cancel journal requires one command and its chat owner")
	}
	return command, nil
}

func (s FileStore) commitCancelCommand(baseRevision uint64, next State, command Command) error {
	if !cancellationCommand(next, command) {
		return errors.New("non-cancel command cannot enter the cancellation journal")
	}
	cancel := &cancelJournalCommand{ChatID: next.ChatID}
	switch value := command.(type) {
	case CancelTurn:
		cancel.Intent = &value
	case ClaimEffect:
		cancel.Claim = &value
	case CancelAcknowledged:
		cancel.Acknowledged = &value
	case CancelFailed:
		cancel.Failed = &value
	case CancelPendingTurn:
		cancel.Pending = &value
	}
	frame, err := encodeProviderEventJournalFrame(providerEventJournalRecord{
		Version: cancelJournalVersion, BaseRevision: baseRevision, Revision: next.Revision, Cancel: cancel,
	})
	if err != nil {
		return err
	}
	// Like the native terminal receipt, Stop must not trigger a whole-history
	// checkpoint merely because the stream journal reached its normal threshold.
	appended, err := appendProviderEventJournalFrame(s.Path, frame, true)
	if err != nil || appended {
		return err
	}
	return s.Save(next)
}

func (e *Engine) persistCommand(next State, command Command) error {
	if e.store == nil {
		return nil
	}
	if store, ok := e.store.(cancelCommandStateStore); ok && cancellationCommand(e.state, command) {
		return store.commitCancelCommand(e.state.Revision, next, command)
	}
	return e.store.Save(next)
}
