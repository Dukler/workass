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

const actorStateEnvelopeV21 = 21

// UpgradeActorStoreV22 makes the v21 presentation-origin contract explicit
// before any chat actor or provider runtime exists. In v21 an empty origin was
// defined as human. V22 stores that value directly and rejects missing origins
// in all live state.
func UpgradeActorStoreV22(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return errors.New("actor-store v22 upgrade requires storage")
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
		if envelope.Version == currentStateEnvelopeVersion {
			continue
		}
		if envelope.Version != actorStateEnvelopeV21 {
			return fmt.Errorf("actor state %q schema v%d is unsupported", entry.Name(), envelope.Version)
		}
		state := envelope.State
		normalizeStateMaps(&state)
		chatID := strings.TrimSpace(state.ChatID)
		if chatID == "" {
			return fmt.Errorf("actor state %q has no immutable chat id", entry.Name())
		}
		upgradeV21PresentationOrigins(&state)
		if err := state.Validate(); err != nil {
			return fmt.Errorf("upgrade actor %q to schema v22: %w", chatID, err)
		}
		if err := saveStateEnvelope(path, state, currentStateEnvelopeVersion); err != nil {
			return fmt.Errorf("commit actor %q schema v22: %w", chatID, err)
		}
		loaded, found, err := (FileStore{Path: path}).Load(chatID)
		if err != nil || !found || loaded.ChatID != chatID {
			return fmt.Errorf("verify actor %q schema v22 receipt", chatID)
		}
	}
	return nil
}

func upgradeV21PresentationOrigins(state *State) {
	for index := range state.Queue {
		upgradeV21Presentation(&state.Queue[index].Presentation)
	}
	if state.Foreground != nil {
		upgradeV21Presentation(&state.Foreground.Input.Presentation)
	}
	if state.PendingSteer != nil {
		upgradeV21Presentation(&state.PendingSteer.Presentation)
	}
	for index := range state.Outbox {
		entry := &state.Outbox[index]
		if entry.Input == nil || strings.TrimSpace(entry.Input.Presentation.Origin) != "" {
			continue
		}
		if entry.Kind == EffectSteerTurn && state.PendingSteer != nil &&
			state.PendingSteer.OperationID == entry.OperationID {
			entry.Input.Presentation = state.PendingSteer.Presentation
		}
		upgradeV21Presentation(&entry.Input.Presentation)
	}
}

func upgradeV21Presentation(presentation *provider.TurnPresentation) {
	if presentation != nil && strings.TrimSpace(presentation.Origin) == "" {
		presentation.Origin = "human"
	}
}
