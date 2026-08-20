package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"workass/internal/durablefs"
	"workass/internal/provider"
)

type StateStore interface {
	Load(chatID string) (State, bool, error)
	Save(State) error
}

type FileStore struct{ Path string }

const currentStateEnvelopeVersion = 22

type stateEnvelope struct {
	Version int   `json:"v"`
	State   State `json:"state"`
}

// DiscoverFileStates enumerates durable chat actors without consulting the
// renderer mirror. The directory is actor storage, so any unreadable or
// duplicate state fails closed instead of silently falling back to UI mirror
// history. Callers may then open exact actors by ChatID through FileStore.
func DiscoverFileStates(dir string) ([]State, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("chat state directory is empty")
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	states := make([]State, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read chat actor %q: %w", entry.Name(), err)
		}
		var header struct {
			State struct {
				ChatID string `json:"ChatID"`
			} `json:"state"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return nil, fmt.Errorf("decode chat actor %q: %w", entry.Name(), err)
		}
		chatID := strings.TrimSpace(header.State.ChatID)
		if chatID == "" {
			return nil, fmt.Errorf("chat actor %q has no immutable chat id", entry.Name())
		}
		if _, duplicate := seen[chatID]; duplicate {
			return nil, fmt.Errorf("duplicate durable chat actor %q", chatID)
		}
		state, ok, err := (FileStore{Path: path}).Load(chatID)
		if err != nil {
			return nil, fmt.Errorf("load chat actor %q: %w", chatID, err)
		}
		if !ok {
			return nil, fmt.Errorf("chat actor %q disappeared during discovery", chatID)
		}
		seen[chatID] = struct{}{}
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].ChatID < states[j].ChatID })
	return states, nil
}

func (s FileStore) Load(chatID string) (State, bool, error) {
	path := strings.TrimSpace(s.Path)
	if path == "" {
		return State{}, false, errors.New("chat state store path is empty")
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, err
	}
	var envelope stateEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return State{}, false, fmt.Errorf("decode chat state: %w", err)
	}
	if envelope.Version != currentStateEnvelopeVersion {
		return State{}, false, fmt.Errorf("unsupported chat state version %d", envelope.Version)
	}
	if strings.TrimSpace(envelope.State.ChatID) != strings.TrimSpace(chatID) {
		return State{}, false, errors.New("chat state file belongs to another chat")
	}
	normalizeStateMaps(&envelope.State)
	if err := envelope.State.Validate(); err != nil {
		return State{}, false, fmt.Errorf("validate chat state: %w", err)
	}
	return envelope.State, true, nil
}

func normalizeStateMaps(state *State) {
	if state.Lanes == nil {
		state.Lanes = make(map[provider.LaneID]LaneState)
	}
	if state.Operations == nil {
		state.Operations = make(map[provider.OperationID]struct{})
	}
	if state.QueueControl.ResumeReceipts == nil {
		state.QueueControl.ResumeReceipts = make(map[provider.OperationID]QueueResumeReceipt)
	}
	if state.QueueMutationReceipts == nil {
		state.QueueMutationReceipts = make(map[provider.OperationID]QueueMutationReceipt)
	}
	if state.PresentationMutationReceipts == nil {
		state.PresentationMutationReceipts = make(map[provider.OperationID]PresentationMutationReceipt)
	}
	if state.RuntimeControlMutationReceipts == nil {
		state.RuntimeControlMutationReceipts = make(map[provider.OperationID]RuntimeControlMutationReceipt)
	}
	if state.WorkspaceMutationReceipts == nil {
		state.WorkspaceMutationReceipts = make(map[provider.OperationID]WorkspaceMutationReceipt)
	}
	if state.LaneSelectionMutationReceipts == nil {
		state.LaneSelectionMutationReceipts = make(map[provider.OperationID]LaneSelectionMutationReceipt)
	}
	if state.AgentWaitObservationReceipts == nil {
		state.AgentWaitObservationReceipts = make(map[provider.OperationID]AgentWaitObservationReceipt)
	}
	normalizeCancelMutationReceipts(state)
	if state.Tools == nil {
		state.Tools = make(map[string]ToolState)
	}
	if state.Plans == nil {
		state.Plans = make(map[provider.OperationID]PlanState)
	}
	if state.Permissions == nil {
		state.Permissions = make(map[string]PermissionState)
	}
	if state.Background == nil {
		state.Background = make(map[string]BackgroundState)
	}
	if state.Usage == nil {
		state.Usage = make(map[provider.LaneID]provider.UsageEvent)
	}
	if state.Compactions == nil {
		state.Compactions = make(map[provider.LaneID]CompactionState)
	}
	if state.Transport == nil {
		state.Transport = make(map[provider.LaneID]provider.TransportHealthEvent)
	}
}

func normalizeCancelMutationReceipts(state *State) {
	if state.CancelMutationReceipts == nil {
		state.CancelMutationReceipts = make(map[provider.OperationID]CancelMutationReceipt)
	}
}

func (s FileStore) Save(state State) error {
	return saveStateEnvelope(s.Path, state, currentStateEnvelopeVersion)
}

func saveStateEnvelope(path string, state State, version int) error {
	if err := state.Validate(); err != nil {
		return err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("chat state store path is empty")
	}
	raw, err := json.Marshal(stateEnvelope{Version: version, State: state})
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".chat-state-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return durablefs.SyncDirectory(dir)
}
