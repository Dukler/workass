package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"workass/internal/provider"
)

// StoredLane is the provider-neutral part of one exact durable lane record.
// The v19 -> v20 actor-store upgrade consumes these records before the chat
// runtime exists; it never invokes a provider or changes a native thread.
type StoredLane struct {
	Identity provider.LaneIdentity
	Thread   provider.ThreadRef
	Owner    provider.AttachmentOwner
	CWD      string
	ModelID  string
	ModeID   string
	Context  provider.ContextCapabilities
}

type v19StateProbe struct {
	Cutover struct {
		Complete     bool               `json:"Complete"`
		BlockedError provider.ErrorKind `json:"BlockedError"`
	} `json:"Migration"`
	Presentation struct {
		Usage json.RawMessage `json:"LegacyUsage"`
	} `json:"Presentation"`
	Ledger []struct {
		Unattributed bool            `json:"Legacy"`
		SteerAnchor  json.RawMessage `json:"SteerAnchor"`
	} `json:"Ledger"`
}

// UpgradeActorStoreV20 performs the one supported actor file-format evolution.
// Every file is replaced atomically, so a crash can leave a mixed v19/v20
// directory and the next boot simply continues. No migration command or
// compatibility state enters the live reducer.
func UpgradeActorStoreV20(dir string, resolve func(string) ([]StoredLane, error)) error {
	dir = strings.TrimSpace(dir)
	if dir == "" || resolve == nil {
		return errors.New("actor-store v20 upgrade requires storage and lane resolver")
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
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read actor state %q: %w", entry.Name(), err)
		}
		var envelope stateEnvelope
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return fmt.Errorf("decode actor state %q: %w", entry.Name(), err)
		}
		switch envelope.Version {
		case currentStateEnvelopeVersion:
			continue
		case 19:
		default:
			return fmt.Errorf("actor state %q requires unsupported bridge from schema v%d", entry.Name(), envelope.Version)
		}
		chatID := strings.TrimSpace(envelope.State.ChatID)
		if chatID == "" {
			return fmt.Errorf("actor state %q has no immutable chat id", entry.Name())
		}
		var probe struct {
			State v19StateProbe `json:"state"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return fmt.Errorf("decode actor state v19 evidence %q: %w", chatID, err)
		}
		normalizeStateMaps(&envelope.State)
		lanes, err := resolve(chatID)
		if err != nil {
			return fmt.Errorf("resolve stored lanes for actor %q: %w", chatID, err)
		}
		if err := upgradeActorStateV20(&envelope.State, probe.State, lanes); err != nil {
			return fmt.Errorf("upgrade actor %q to schema v20: %w", chatID, err)
		}
		if err := (FileStore{Path: path}).Save(envelope.State); err != nil {
			return fmt.Errorf("commit actor %q schema v20: %w", chatID, err)
		}
	}
	return nil
}

func upgradeActorStateV20(state *State, probe v19StateProbe, stored []StoredLane) error {
	if state == nil {
		return errors.New("schema v19 actor is missing")
	}
	if !state.Initialized {
		if err := validateEmptyUninitializedV19Actor(*state, probe); err != nil {
			return fmt.Errorf("schema v19 uninitialized actor contains durable chat state: %w", err)
		}
		return state.Validate()
	}
	for _, row := range probe.Ledger {
		anchor := strings.TrimSpace(string(row.SteerAnchor))
		if anchor != "" && anchor != "null" {
			return errors.New("schema v19 actor contains unsupported offset-based steering state")
		}
	}
	if !probe.Cutover.Complete {
		if probe.Cutover.BlockedError != "" {
			return errors.New("schema v19 actor has an invalid incomplete cutover marker")
		}
		for _, row := range probe.Ledger {
			if row.Unattributed {
				return errors.New("actor-native state contains an unattributed transcript row")
			}
		}
		providerID := provider.NormalizeID(string(state.Presentation.ProviderID))
		if providerID == "" {
			if lane, ok := state.Lanes[state.ActiveLaneID]; ok {
				providerID = lane.Identity.Realm.ProviderID
			}
		}
		if err := promoteV19Usage(&state.Presentation, probe.Presentation.Usage, providerID); err != nil {
			return err
		}
		return state.Validate()
	}
	if len(probe.Ledger) != len(state.Ledger) {
		return errors.New("schema v19 transcript evidence does not match the actor ledger")
	}
	if state.Foreground != nil || state.PendingSteer != nil || state.PendingCancel != nil || len(state.Queue) != 0 || len(state.StagedQueue) != 0 {
		return errors.New("schema v19 conversion cannot cross a pending chat operation")
	}
	for _, entry := range state.Outbox {
		if entry.Status != OutboxCompleted && entry.Status != OutboxFailed {
			return errors.New("schema v19 conversion cannot cross an unresolved provider effect")
		}
	}

	storedByID := make(map[provider.LaneID]StoredLane, len(stored))
	for _, record := range stored {
		record.Identity = record.Identity.Normalize()
		record.Thread = record.Thread.Normalize()
		if err := record.Identity.Validate(); err != nil {
			return err
		}
		if record.Identity.ChatID != state.ChatID {
			return errors.New("stored lane belongs to another chat")
		}
		if err := record.Thread.Validate(record.Identity.Realm.ProviderID); err != nil {
			return err
		}
		if !record.Context.ExactResume {
			return errors.New("stored lane cannot resume its exact native thread")
		}
		if strings.TrimSpace(record.Owner.TabID) != strings.TrimSpace(state.Presentation.TabID) {
			return errors.New("stored lane belongs to another tab attachment")
		}
		if _, duplicate := storedByID[record.Identity.ID]; duplicate {
			return errors.New("stored lane inventory contains a duplicate identity")
		}
		storedByID[record.Identity.ID] = record
	}
	for laneID, lane := range state.Lanes {
		record, exists := storedByID[laneID]
		if !exists || lane.Identity.Normalize() != record.Identity || !lane.Thread.Normalize().Equal(record.Thread) {
			return errors.New("actor lane does not match the canonical lane store")
		}
	}
	for laneID, record := range storedByID {
		lane, exists := state.Lanes[laneID]
		if !exists {
			lane = LaneState{Identity: record.Identity, Thread: record.Thread, Phase: LaneDetached, Coverage: make(map[uint64]CoverageRecord)}
		}
		if lane.Phase != LaneDetached && lane.Phase != LaneBlocked && lane.Phase != LaneAbsent {
			return errors.New("schema v19 lane still has live runtime state")
		}
		if lane.PendingImport != nil || lane.Attachment != nil {
			return errors.New("schema v19 lane still has an unresolved attachment")
		}
		lane.Identity = record.Identity
		lane.Thread = record.Thread
		lane.Owner = record.Owner
		lane.CWD = strings.TrimSpace(record.CWD)
		lane.ModelID = strings.TrimSpace(record.ModelID)
		lane.ModeID = strings.TrimSpace(record.ModeID)
		lane.Context = record.Context
		lane.Phase = LaneDetached
		lane.LastError = ""
		lane.PendingImport = nil
		lane.Attachment = nil
		if lane.Coverage == nil {
			lane.Coverage = make(map[uint64]CoverageRecord)
		}
		state.Lanes[laneID] = lane
	}

	selected, ok, err := selectV20StoredLane(*state, storedByID)
	if err != nil {
		return err
	}
	if !ok {
		if len(state.Ledger) != 0 {
			return errors.New("nonempty actor has no exact stored provider lane")
		}
		state.ActiveLaneID = ""
		state.DesiredLaneID = ""
		return state.Validate()
	}
	lane := state.Lanes[selected.Identity.ID]
	lane.Coverage = make(map[uint64]CoverageRecord, len(state.Ledger))
	for index := range state.Ledger {
		event := &state.Ledger[index]
		if index >= len(probe.Ledger) || !probe.Ledger[index].Unattributed {
			return errors.New("schema v19 migrated transcript contains an attributed row")
		}
		event.LaneID = selected.Identity.ID
		event.ProviderID = selected.Identity.Realm.ProviderID
		status := CoverageNativeSeen
		if event.ContextExcluded {
			status = CoverageExcluded
		}
		sequence := uint64(index + 1)
		lane.Coverage[sequence] = CoverageRecord{
			Sequence: sequence, EventID: event.EventID, Status: status, DeliveryID: event.OperationID,
		}
	}
	lane.CoveredThrough = uint64(len(state.Ledger))
	state.Lanes[selected.Identity.ID] = lane
	state.ActiveLaneID = selected.Identity.ID
	state.DesiredLaneID = selected.Identity.ID
	state.Presentation.ProviderID = selected.Identity.Realm.ProviderID
	if selected.ModelID != "" {
		state.Presentation.CurrentModelID = selected.ModelID
	}
	if selected.ModeID != "" {
		state.Presentation.CurrentModeID = selected.ModeID
	}
	if err := promoteV19Usage(&state.Presentation, probe.Presentation.Usage, selected.Identity.Realm.ProviderID); err != nil {
		return err
	}
	return state.Validate()
}

func validateEmptyUninitializedV19Actor(state State, probe v19StateProbe) error {
	if state.Initialized {
		return errors.New("actor is initialized")
	}
	if state.Deleted || state.CreationOperationID != "" || strings.TrimSpace(state.CreationDigest) != "" || state.DeletionOperationID != "" {
		return errors.New("lifecycle receipt is present")
	}
	presentation := state.Presentation
	presentation.ModelControls = canonicalEmptyV19JSON(presentation.ModelControls)
	presentation.ContextUsageByProvider = canonicalEmptyV19JSON(presentation.ContextUsageByProvider)
	if len(presentation.PlanLatest) == 0 {
		presentation.PlanLatest = nil
	}
	if !reflect.DeepEqual(presentation, PresentationState{}) {
		return errors.New("presentation is present")
	}
	environment := state.Environment
	environment.Payload = canonicalEmptyV19JSON(environment.Payload)
	environment.Checkpoints = canonicalEmptyV19JSON(environment.Checkpoints)
	environment.Reference = canonicalEmptyV19JSON(environment.Reference)
	if !reflect.DeepEqual(environment, EnvironmentState{}) {
		return errors.New("environment is present")
	}
	if state.ContextFloor != 0 || len(state.Ledger) != 0 || len(state.Lanes) != 0 || state.ActiveLaneID != "" || state.DesiredLaneID != "" {
		return errors.New("ledger or lane state is present")
	}
	if len(state.StagedQueue) != 0 || len(state.Queue) != 0 || state.Foreground != nil || state.PendingSteer != nil || state.PendingCancel != nil {
		return errors.New("pending chat work is present")
	}
	if len(state.Operations) != 0 || len(state.Outbox) != 0 || len(state.Tools) != 0 || len(state.Plans) != 0 ||
		len(state.Permissions) != 0 || len(state.Background) != 0 || len(state.Usage) != 0 ||
		len(state.Compactions) != 0 || len(state.Transport) != 0 || state.Obligation != nil {
		return errors.New("durable provider work is present")
	}
	if len(state.QueueMutationReceipts) != 0 || len(state.PresentationMutationReceipts) != 0 ||
		len(state.RuntimeControlMutationReceipts) != 0 || len(state.WorkspaceMutationReceipts) != 0 ||
		len(state.LaneSelectionMutationReceipts) != 0 || len(state.CancelMutationReceipts) != 0 ||
		len(state.AgentWaitObservationReceipts) != 0 {
		return errors.New("mutation receipt is present")
	}
	usage := strings.TrimSpace(string(probe.Presentation.Usage))
	if probe.Cutover.Complete || probe.Cutover.BlockedError != "" || (usage != "" && usage != "null") || len(probe.Ledger) != 0 {
		return errors.New("v19 conversion evidence is present")
	}
	return nil
}

func canonicalEmptyV19JSON(raw json.RawMessage) json.RawMessage {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return nil
	}
	return raw
}

func selectV20StoredLane(state State, stored map[provider.LaneID]StoredLane) (StoredLane, bool, error) {
	for _, laneID := range []provider.LaneID{state.DesiredLaneID, state.ActiveLaneID} {
		if record, ok := stored[laneID]; ok {
			return record, true, nil
		}
	}
	providerID := provider.NormalizeID(string(state.Presentation.ProviderID))
	var selected StoredLane
	for _, record := range stored {
		if record.Identity.Realm.ProviderID != providerID {
			continue
		}
		if selected.Identity.ID != "" {
			return StoredLane{}, false, errors.New("multiple stored lanes match the selected provider")
		}
		selected = record
	}
	if selected.Identity.ID != "" {
		return selected, true, nil
	}
	if len(stored) == 1 {
		for _, record := range stored {
			return record, true, nil
		}
	}
	if len(stored) == 0 {
		return StoredLane{}, false, nil
	}
	return StoredLane{}, false, errors.New("actor provider selection does not identify one exact stored lane")
}

func promoteV19Usage(presentation *PresentationState, raw json.RawMessage, providerID provider.ID) error {
	if presentation == nil || len(raw) == 0 || string(raw) == "null" || providerID == "" {
		return nil
	}
	if !json.Valid(raw) {
		return errors.New("schema v19 usage snapshot is invalid")
	}
	values := make(map[string]json.RawMessage)
	if len(presentation.ContextUsageByProvider) > 0 && string(presentation.ContextUsageByProvider) != "null" {
		if err := json.Unmarshal(presentation.ContextUsageByProvider, &values); err != nil {
			return err
		}
	}
	key := string(providerID)
	if _, exists := values[key]; !exists {
		values[key] = append(json.RawMessage(nil), raw...)
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return err
	}
	presentation.ContextUsageByProvider = encoded
	return nil
}
