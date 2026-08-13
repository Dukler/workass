package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"workass/internal/provider"
)

const actorStateEnvelopeV20 = 20

// StoredLane is the provider-neutral part of one exact durable lane record.
// The actor-store upgrade consumes these records before the chat runtime
// exists; it never invokes a provider or changes a native thread reference.
type StoredLane struct {
	Identity    provider.LaneIdentity
	Thread      provider.ThreadRef
	Owner       provider.AttachmentOwner
	CWD         string
	ModelID     string
	ModeID      string
	Context     provider.ContextCapabilities
	Creation    provider.CreationCapabilities
	Established bool
}

// UpgradeActorStoreV21 adds the provider creation receipt to actor-owned
// lanes. It accepts only the immediately previous production schema and never
// contacts a provider or changes a provider-native thread reference.
func UpgradeActorStoreV21(dir string, resolve func(string) ([]StoredLane, error)) error {
	if resolve == nil {
		return errors.New("actor-store v21 upgrade requires a lane resolver")
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return errors.New("actor-store v21 upgrade requires storage")
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read actor state %q: %w", entry.Name(), readErr)
		}
		var envelope stateEnvelope
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return fmt.Errorf("decode actor state %q: %w", entry.Name(), err)
		}
		if envelope.Version == currentStateEnvelopeVersion {
			continue
		}
		if envelope.Version != actorStateEnvelopeV20 {
			return fmt.Errorf("actor state %q schema v%d is unsupported", entry.Name(), envelope.Version)
		}
		state := envelope.State
		normalizeStateMaps(&state)
		chatID := strings.TrimSpace(state.ChatID)
		if chatID == "" {
			return fmt.Errorf("actor state %q has no immutable chat id", entry.Name())
		}
		stored, resolveErr := resolve(chatID)
		if resolveErr != nil {
			return fmt.Errorf("resolve stored lanes for actor %q: %w", chatID, resolveErr)
		}
		storedByLane := make(map[provider.LaneID]StoredLane, len(stored))
		for _, record := range stored {
			identity := record.Identity.Normalize()
			if err := identity.Validate(); err != nil {
				return fmt.Errorf("actor %q stored lane is invalid: %w", chatID, err)
			}
			if identity.ChatID != chatID {
				return fmt.Errorf("actor %q stored lane belongs to another chat", chatID)
			}
			record.Identity = identity
			record.Thread = record.Thread.Normalize()
			if err := record.Thread.Validate(identity.Realm.ProviderID); err != nil {
				return fmt.Errorf("actor %q stored provider reference is invalid: %w", chatID, err)
			}
			if _, duplicate := storedByLane[identity.ID]; duplicate {
				return fmt.Errorf("actor %q stored lane inventory contains duplicate identity %q", chatID, identity.ID)
			}
			storedByLane[identity.ID] = record
		}
		matchedStored := make(map[provider.LaneID]struct{}, len(storedByLane))
		for laneID, lane := range state.Lanes {
			if lane.Provision != nil {
				return fmt.Errorf("actor %q schema v20 unexpectedly contains a provider candidate", chatID)
			}
			record, exists := storedByLane[laneID]
			if !exists {
				if !lane.Thread.IsZero() {
					return fmt.Errorf("actor %q lane %q has no exact provider-store owner", chatID, laneID)
				}
				state.Lanes[laneID] = lane
				continue
			}
			matchedStored[laneID] = struct{}{}
			if lane.Identity.Normalize() != record.Identity || !lane.Thread.Normalize().Equal(record.Thread) {
				return fmt.Errorf("actor %q lane %q disagrees with its exact provider-store reference", chatID, laneID)
			}
			lane.Creation = record.Creation
			if !record.Established {
				if !record.Creation.DeferredUntilInput {
					return fmt.Errorf("actor %q lane %q is provisional for an immediate-creation provider", chatID, laneID)
				}
				candidate := record.Thread
				lane.Thread = provider.ThreadRef{}
				lane.Provision = &candidate
				lane.CreateAfterCandidateAbsence = !laneHasNativeCoverage(lane)
				lane.Delivery.StableInputIdentity = true
				lane.Delivery.ConsumptionReceipt = true
				lane.Attachment = nil
				lane.Phase = LaneAbsent
				lane.LastError = ""
			}
			state.Lanes[laneID] = lane
		}
		for index := range state.Outbox {
			entry := &state.Outbox[index]
			if entry.Kind != EffectCreateLane {
				continue
			}
			lane, exists := state.Lanes[entry.LaneID]
			if !exists {
				return fmt.Errorf("actor %q create outbox belongs to an unknown lane %q", chatID, entry.LaneID)
			}
			entry.CreateAfterCandidateAbsence = lane.CreateAfterCandidateAbsence
		}
		for laneID, record := range storedByLane {
			if _, matched := matchedStored[laneID]; matched {
				continue
			}
			if !record.Established {
				return fmt.Errorf("actor %q provisional provider-store lane %q has no actor candidate", chatID, laneID)
			}
			state.Lanes[laneID] = LaneState{
				Identity: record.Identity, Owner: record.Owner, CWD: strings.TrimSpace(record.CWD),
				ModelID: strings.TrimSpace(record.ModelID), ModeID: strings.TrimSpace(record.ModeID),
				Thread: record.Thread, Phase: LaneDetached, Coverage: make(map[uint64]CoverageRecord),
				Context: record.Context, Creation: record.Creation,
			}
		}
		if err := state.Validate(); err != nil {
			return fmt.Errorf("upgrade actor %q to schema v21: %w", chatID, err)
		}
		if err := saveStateEnvelope(path, state, currentStateEnvelopeVersion); err != nil {
			return fmt.Errorf("commit actor %q schema v21: %w", chatID, err)
		}
		var receipt stateEnvelope
		committed, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(committed, &receipt) != nil || receipt.Version != currentStateEnvelopeVersion || receipt.State.ChatID != chatID {
			return fmt.Errorf("verify actor %q schema v21 receipt", chatID)
		}
	}
	return nil
}

func laneHasNativeCoverage(lane LaneState) bool {
	for _, record := range lane.Coverage {
		if record.Status == CoverageNativeSeen || record.Status == CoverageImported {
			return true
		}
	}
	return false
}
