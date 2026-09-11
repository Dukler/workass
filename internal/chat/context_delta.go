package chat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"workass/internal/provider"
)

// buildContextDelta includes only unseen semantic events. Unlike the initial
// seed, an established lane's delta must fit in full: silently dropping history
// here would turn a successful handoff into lost context.
func buildContextDelta(state State, lane LaneState) (ContextBatch, error) {
	batch := ContextBatch{ProjectionVersion: semanticProjectionVersion}
	for sequence := lane.CoveredThrough + 1; sequence <= state.LedgerHead(); sequence++ {
		event := state.Ledger[sequence-1]
		if _, covered := lane.Coverage[sequence]; covered || event.ContextExcluded {
			continue
		}
		batch.EventIDs = append(batch.EventIDs, event.EventID)
		batch.Messages = append(batch.Messages, contextMessageForLedgerEvent(event))
		if len(batch.Messages) > initialSeedMaxEvents {
			return ContextBatch{}, errors.New("missing history exceeds prompt event budget")
		}
	}
	digest, size, err := contextDeltaDigest(lane.CoveredThrough, state.LedgerHead(), batch)
	if err != nil {
		return ContextBatch{}, err
	}
	if size > initialSeedMaxBytes {
		return ContextBatch{}, errors.New("missing history exceeds prompt byte budget")
	}
	batch.Digest = digest
	return batch, nil
}

func contextDeltaDigest(from, through uint64, batch ContextBatch) (string, int, error) {
	raw, err := json.Marshal(struct {
		Version       uint32
		From, Through uint64
		EventIDs      []string
		Messages      []provider.ContextMessage
	}{batch.ProjectionVersion, from, through, batch.EventIDs, batch.Messages})
	if err != nil {
		return "", 0, err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), len(raw), nil
}

func contextDeltaBatch(input QueueEntry, batch ContextBatch) *ContextBatch {
	if input.DeltaDigest == "" {
		return nil
	}
	copy := cloneContextBatch(batch)
	return &copy
}

func validateContextDelta(input QueueEntry, batch *ContextBatch) error {
	if batch == nil || input.DeltaDigest == "" || input.DeltaThrough <= input.DeltaFrom || input.InitialSeedDigest != "" {
		return errors.New("context delta lost its immutable range or batch")
	}
	digest, size, err := contextDeltaDigest(input.DeltaFrom, input.DeltaThrough, *batch)
	if err != nil {
		return err
	}
	if digest != input.DeltaDigest || batch.Digest != digest || batch.ProjectionVersion != semanticProjectionVersion ||
		size > initialSeedMaxBytes || len(batch.Messages) > initialSeedMaxEvents || len(batch.Messages) != len(batch.EventIDs) {
		return errors.New("context delta changed identity or exceeded its budget")
	}
	last := input.DeltaFrom
	for index, message := range batch.Messages {
		if message.LedgerSequence <= last || message.LedgerSequence > input.DeltaThrough || message.EventID != batch.EventIDs[index] || !message.Inert {
			return errors.New("context delta has invalid semantic event order")
		}
		last = message.LedgerSequence
	}
	return nil
}

// The exact batch lives in the durable outbox, so a receipt never recomputes
// missing history against a newer coverage cursor. Uncertain delivery is fenced
// separately from confirmed consumption and cannot be replayed on a later send.
func commitContextDelta(state *State, lane *LaneState, input QueueEntry, status CoverageStatus) error {
	if input.DeltaDigest == "" {
		return nil
	}
	var batch *ContextBatch
	for _, entry := range state.Outbox {
		if entry.ID == startTurnEffectID(input.OperationID) && entry.LaneID == lane.Identity.ID {
			batch = entry.Batch
			break
		}
	}
	if err := validateContextDelta(input, batch); err != nil {
		return err
	}
	if input.DeltaThrough > state.LedgerHead() {
		return errors.New("context delta extends beyond the ledger")
	}
	included := make(map[uint64]string, len(batch.Messages))
	for _, message := range batch.Messages {
		included[message.LedgerSequence] = message.EventID
	}
	for sequence := input.DeltaFrom + 1; sequence <= input.DeltaThrough; sequence++ {
		if _, exists := lane.Coverage[sequence]; exists {
			continue
		}
		event := state.Ledger[sequence-1]
		coverage := CoverageExcluded
		if eventID, ok := included[sequence]; ok {
			if eventID != event.EventID {
				return errors.New("context delta event changed identity")
			}
			coverage = status
		} else if !event.ContextExcluded {
			return errors.New("context delta omitted unseen semantic history")
		}
		if err := setCoverage(lane, state, sequence, coverage, input.OperationID); err != nil {
			return err
		}
	}
	return nil
}
