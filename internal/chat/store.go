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

type StateStore interface {
	Load(chatID string) (State, bool, error)
	Save(State) error
}

type FileStore struct{ Path string }

const currentStateEnvelopeVersion = 16

type stateEnvelope struct {
	Version int   `json:"v"`
	State   State `json:"state"`
}

// DiscoverFileStates enumerates durable chat actors without consulting the
// renderer mirror. The directory is actor storage, so any unreadable or
// duplicate state fails closed instead of silently falling back to legacy UI
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
	if envelope.Version < 1 || envelope.Version > currentStateEnvelopeVersion {
		return State{}, false, fmt.Errorf("unsupported chat state version %d", envelope.Version)
	}
	if strings.TrimSpace(envelope.State.ChatID) != strings.TrimSpace(chatID) {
		return State{}, false, errors.New("chat state file belongs to another chat")
	}
	normalizeStateMaps(&envelope.State)
	if envelope.Version == 1 {
		normalizeVersionOneState(&envelope.State)
	}
	if envelope.Version <= 2 {
		normalizeAssistantSegmentIdentity(&envelope.State)
	}
	if envelope.Version <= 3 {
		// Before schema v4 only migrated legacy chats were safe to project. Keep
		// empty/incomplete sidecars blocked; do not infer new-chat ownership.
		envelope.State.Initialized = envelope.State.Migration.Complete
	}
	if envelope.Version <= 10 {
		// Background rows before v11 were a passive manager mirror and could lose
		// their turn operation after the foreground job detached. Canonicalize a
		// stable migration operation from the immutable work id; all new rows must
		// arrive with their real actor owner before they are committed.
		for id, item := range envelope.State.Background {
			if item.Owner.OperationID == "" {
				item.Owner.OperationID = provider.OperationID("migrated-background:" + strings.TrimSpace(id))
				envelope.State.Background[id] = item
			}
		}
	}
	if envelope.Version <= 11 && envelope.State.QueueMutationReceipts == nil {
		envelope.State.QueueMutationReceipts = make(map[provider.OperationID]QueueMutationReceipt)
	}
	if envelope.Version <= 12 && envelope.State.PresentationMutationReceipts == nil {
		envelope.State.PresentationMutationReceipts = make(map[provider.OperationID]PresentationMutationReceipt)
	}
	if envelope.Version <= 13 && envelope.State.RuntimeControlMutationReceipts == nil {
		envelope.State.RuntimeControlMutationReceipts = make(map[provider.OperationID]RuntimeControlMutationReceipt)
	}
	if envelope.Version <= 14 && envelope.State.LaneSelectionMutationReceipts == nil {
		envelope.State.LaneSelectionMutationReceipts = make(map[provider.OperationID]LaneSelectionMutationReceipt)
	}
	if envelope.Version <= 15 && envelope.State.WorkspaceMutationReceipts == nil {
		envelope.State.WorkspaceMutationReceipts = make(map[provider.OperationID]WorkspaceMutationReceipt)
	}
	if err := envelope.State.Validate(); err != nil {
		return State{}, false, fmt.Errorf("validate chat state: %w", err)
	}
	return envelope.State, true, nil
}

// Version one was the provider-lane sidecar used while the actor did not yet
// own renderer projection. Its ledger rows did not persist public message ids
// or statuses, even though the immutable outbox already carried those ids.
// Recover only identities that are provable from the same operation; the
// deterministic fallback is an actor identity, not a guess from renderer text.
//
// Migration remains incomplete. The daemon must still reconcile the complete
// legacy mirror before lane selection, and a conflict there fails closed.
func normalizeVersionOneState(state *State) {
	presentations := make(map[provider.OperationID]provider.TurnPresentation)
	for _, entry := range state.Queue {
		if entry.OperationID != "" {
			presentations[entry.OperationID] = entry.Presentation
		}
	}
	if state.Foreground != nil && state.Foreground.OperationID != "" {
		presentations[state.Foreground.OperationID] = state.Foreground.Input.Presentation
	}
	for _, effect := range state.Outbox {
		if effect.Input != nil && effect.OperationID != "" {
			presentations[effect.OperationID] = effect.Input.Presentation
		}
	}
	for index := range state.Ledger {
		event := &state.Ledger[index]
		presentation := presentations[event.OperationID]
		if strings.TrimSpace(event.MessageID) == "" {
			switch strings.ToLower(strings.TrimSpace(event.Role)) {
			case "user":
				event.MessageID = strings.TrimSpace(presentation.UserMessageID)
			case "assistant":
				event.MessageID = strings.TrimSpace(presentation.AssistantMessageID)
			}
			if event.MessageID == "" {
				event.MessageID = fmt.Sprintf("message:%s:%s", event.OperationID, strings.ToLower(strings.TrimSpace(event.Role)))
			}
		}
		if strings.TrimSpace(event.Status) == "" {
			switch strings.ToLower(strings.TrimSpace(event.TerminalState)) {
			case "failed":
				event.Status = "failed"
			case "cancelled":
				event.Status = "cancelled"
			default:
				event.Status = "done"
			}
		}
	}
}

// Version three made chronological assistant segments authoritative. Earlier
// actor sidecars owned only one undifferentiated draft, so their exact public
// row identity must be derived from the immutable turn presentation before the
// stricter state validator runs. This does not inspect or match renderer text.
func normalizeAssistantSegmentIdentity(state *State) {
	if state == nil || state.Foreground == nil {
		return
	}
	foreground := state.Foreground
	rootID := strings.TrimSpace(foreground.RootAssistantMessageID)
	if rootID == "" {
		rootID = strings.TrimSpace(foreground.Input.Presentation.AssistantMessageID)
	}
	if rootID == "" {
		rootID = fmt.Sprintf("message:%s:assistant", foreground.OperationID)
	}
	foreground.RootAssistantMessageID = rootID
	if strings.TrimSpace(foreground.CurrentAssistantMessageID) == "" {
		foreground.CurrentAssistantMessageID = rootID
	}
	if foreground.StartedAt == "" {
		foreground.StartedAt = strings.TrimSpace(foreground.Input.Presentation.StartedAt)
	}
	if foreground.AssistantContent == "" && foreground.AssistantResult == "" && foreground.AssistantDraft != "" {
		foreground.AssistantContent = foreground.AssistantDraft
		foreground.AssistantDraft = ""
	}
}

func normalizeStateMaps(state *State) {
	if state.Lanes == nil {
		state.Lanes = make(map[provider.LaneID]LaneState)
	}
	if state.Operations == nil {
		state.Operations = make(map[provider.OperationID]struct{})
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

func (s FileStore) Save(state State) error {
	if err := state.Validate(); err != nil {
		return err
	}
	path := strings.TrimSpace(s.Path)
	if path == "" {
		return errors.New("chat state store path is empty")
	}
	raw, err := json.Marshal(stateEnvelope{Version: currentStateEnvelopeVersion, State: state})
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
	if dirHandle, err := os.Open(dir); err == nil {
		defer dirHandle.Close()
		if err := dirHandle.Sync(); err != nil {
			return err
		}
	}
	return nil
}
