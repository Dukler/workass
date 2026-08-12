package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

const legacyChatMigrationVersion uint32 = 2
const legacyChatCutoverVersion uint32 = 5

var errActorChatNotFound = errors.New("durable chat actor does not exist")

const legacyChatCutoverReceiptFilename = "provider-chat-cutover-v5.json"
const previousLegacyChatCutoverReceiptFilename = "provider-chat-cutover-v4.json"
const olderLegacyChatCutoverReceiptFilename = "provider-chat-cutover-v3.json"

type legacyChatCutoverReceipt struct {
	Version     uint32   `json:"version"`
	Complete    bool     `json:"complete"`
	CompletedAt string   `json:"completedAt"`
	ChatIDs     []string `json:"chatIds"`
}

// completeLegacyChatCutover is the only runtime allowed to consume the old
// renderer mirror/archive as chat recovery authority. It eagerly migrates the
// complete legacy inventory, durably verifies every actor, and writes one
// global receipt last. After that receipt exists, lazy migration is forbidden:
// an unknown nonempty mirror row is corruption, never a fallback source.
func (r *providerChatRuntime) completeLegacyChatCutover() error {
	if r == nil || r.sessions == nil {
		return errors.New("legacy chat cutover is unavailable")
	}
	receiptPath := filepath.Join(r.stateDir, legacyChatCutoverReceiptFilename)
	if receipt, ok, err := readLegacyChatCutoverReceipt(receiptPath); err != nil {
		return err
	} else if ok {
		if receipt.Version != legacyChatCutoverVersion || !receipt.Complete {
			return fmt.Errorf("unsupported or incomplete provider chat cutover receipt v%d", receipt.Version)
		}
		seenReceipt := make(map[string]struct{}, len(receipt.ChatIDs))
		for _, chatID := range receipt.ChatIDs {
			chatID = strings.TrimSpace(chatID)
			if chatID == "" {
				return errors.New("provider chat cutover receipt contains an empty chat id")
			}
			if _, duplicate := seenReceipt[chatID]; duplicate {
				return fmt.Errorf("provider chat cutover receipt contains duplicate chatId %q", chatID)
			}
			seenReceipt[chatID] = struct{}{}
			r.mu.Lock()
			_, known := r.known[chatID]
			r.mu.Unlock()
			if !known {
				return fmt.Errorf("provider chat cutover receipt references missing actor %q", chatID)
			}
		}
		// ChatIDs is the immutable inventory consumed by the one-time migration,
		// not a permanent actor index. Actor-native chats created after cutover
		// live only in the actor store and must never mutate this legacy receipt.
		return r.sessions.ActivateActorCutover()
	}
	previousPath := filepath.Join(r.stateDir, previousLegacyChatCutoverReceiptFilename)
	if previous, ok, err := readLegacyChatCutoverReceipt(previousPath); err != nil {
		return err
	} else if ok {
		if previous.Version != 4 || !previous.Complete {
			return fmt.Errorf("unsupported or incomplete previous provider chat cutover receipt v%d", previous.Version)
		}
		if err := r.upgradeLegacyBackground(previous.ChatIDs); err != nil {
			return fmt.Errorf("upgrade actor-owned background work: %w", err)
		}
		known, err := r.knownChatIDs()
		if err != nil {
			return err
		}
		receipt := legacyChatCutoverReceipt{
			Version: legacyChatCutoverVersion, Complete: true,
			CompletedAt: time.Now().UTC().Format(time.RFC3339Nano), ChatIDs: known,
		}
		if err := writeLegacyChatCutoverReceipt(receiptPath, receipt); err != nil {
			return fmt.Errorf("commit provider chat cutover upgrade: %w", err)
		}
		if err := os.Remove(previousPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove superseded provider chat cutover receipt: %w", err)
		}
		if err := removeLegacyObligationStore(r.stateDir); err != nil {
			return err
		}
		return r.sessions.ActivateActorCutover()
	}
	olderPath := filepath.Join(r.stateDir, olderLegacyChatCutoverReceiptFilename)
	if older, ok, err := readLegacyChatCutoverReceipt(olderPath); err != nil {
		return err
	} else if ok {
		if older.Version != 3 || !older.Complete {
			return fmt.Errorf("unsupported or incomplete older provider chat cutover receipt v%d", older.Version)
		}
		if err := r.upgradeLegacyObligations(older.ChatIDs); err != nil {
			return fmt.Errorf("upgrade actor-owned obligations: %w", err)
		}
		if err := r.upgradeLegacyBackground(older.ChatIDs); err != nil {
			return fmt.Errorf("upgrade actor-owned background work: %w", err)
		}
		known, err := r.knownChatIDs()
		if err != nil {
			return err
		}
		receipt := legacyChatCutoverReceipt{
			Version: legacyChatCutoverVersion, Complete: true,
			CompletedAt: time.Now().UTC().Format(time.RFC3339Nano), ChatIDs: known,
		}
		if err := writeLegacyChatCutoverReceipt(receiptPath, receipt); err != nil {
			return fmt.Errorf("commit provider chat cutover upgrade: %w", err)
		}
		if err := os.Remove(olderPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove superseded provider chat cutover receipt: %w", err)
		}
		if err := removeLegacyObligationStore(r.stateDir); err != nil {
			return err
		}
		return r.sessions.ActivateActorCutover()
	}

	snapshot, ok := r.sessions.Get().(map[string]any)
	if !ok || snapshot == nil {
		if loadErr := r.sessions.LoadError(); loadErr != nil {
			return fmt.Errorf("legacy session snapshot is unavailable: %w", loadErr)
		}
		snapshot = map[string]any{"chats": []any{}}
	}
	seen := make(map[string]struct{})
	for _, raw := range anySlice(snapshot["chats"]) {
		source := mapFromAnyMain(raw)
		chatID := strings.TrimSpace(fieldString(source, "chatId"))
		if chatID == "" {
			return errors.New("legacy chat cutover found a row without immutable chatId")
		}
		if _, duplicate := seen[chatID]; duplicate {
			return fmt.Errorf("legacy chat cutover found duplicate chatId %q", chatID)
		}
		seen[chatID] = struct{}{}
		actor, err := r.actorFromLegacy(chatID, source)
		if err != nil {
			return err
		}
		state := actor.engine.Snapshot()
		if !state.Initialized || state.ChatID != chatID {
			return fmt.Errorf("legacy chat %q failed actor readback", chatID)
		}
		if err := r.adoptLegacyProviderBindings(actor, source); err != nil {
			return fmt.Errorf("migrate legacy provider lanes for chat %q: %w", chatID, err)
		}
		if err := r.migrateLegacyBackground(actor.engine); err != nil {
			return fmt.Errorf("migrate legacy background work for chat %q: %w", chatID, err)
		}
	}
	// The native binding ledger may contain a chat that the old renderer mirror
	// omitted (stale save, partial deletion, or an interrupted prior fallback).
	// Inventory it explicitly and quarantine that one chat. Ignoring it would
	// lose an exact provider thread; guessing it into another row would corrupt
	// ownership. Unrelated mirror chats remain usable.
	nativeInventory, err := r.manager.LegacyProviderChatInventory()
	if err != nil {
		return err
	}
	for _, item := range nativeInventory {
		if _, exists := seen[item.ChatID]; exists {
			continue
		}
		tabID := strings.TrimSpace(item.TabID)
		if tabID == "" {
			digest := sha256.Sum256([]byte(item.ChatID))
			tabID = "quarantined-native-" + hex.EncodeToString(digest[:8])
		}
		source := map[string]any{
			"id": tabID, "chatId": item.ChatID, "title": "Quarantined recovered chat",
			"titleLocked": true, "providerId": item.ProviderID, "cwd": item.CWD,
			"messages": []any{}, "queue": []any{},
		}
		actor, actorErr := r.actorFromLegacy(item.ChatID, source)
		if actorErr != nil {
			return fmt.Errorf("inventory orphan native chat %q: %w", item.ChatID, actorErr)
		}
		if actorErr = actor.engine.Apply(chat.QuarantineLegacyMigration{Error: providercontract.ErrorNativeIdentityConflict}); actorErr != nil {
			return fmt.Errorf("quarantine orphan native chat %q: %w", item.ChatID, actorErr)
		}
		seen[item.ChatID] = struct{}{}
	}
	ids, err := r.knownChatIDs()
	if err != nil {
		return err
	}
	sort.Strings(ids)
	receipt := legacyChatCutoverReceipt{
		Version: legacyChatCutoverVersion, Complete: true,
		CompletedAt: time.Now().UTC().Format(time.RFC3339Nano), ChatIDs: ids,
	}
	if err := writeLegacyChatCutoverReceipt(receiptPath, receipt); err != nil {
		return fmt.Errorf("commit provider chat cutover receipt: %w", err)
	}
	if err := removeLegacyObligationStore(r.stateDir); err != nil {
		return err
	}
	return r.sessions.ActivateActorCutover()
}

func (r *providerChatRuntime) upgradeLegacyObligations(chatIDs []string) error {
	seen := make(map[string]struct{}, len(chatIDs))
	for _, chatID := range chatIDs {
		chatID = strings.TrimSpace(chatID)
		if chatID == "" {
			return errors.New("previous cutover receipt contains an empty chat id")
		}
		if _, duplicate := seen[chatID]; duplicate {
			return fmt.Errorf("previous cutover receipt contains duplicate chatId %q", chatID)
		}
		seen[chatID] = struct{}{}
		engine, err := chat.NewDurableEngine(chatID, chat.FileStore{Path: providerChatStatePath(r.stateDir, chatID)})
		if err != nil {
			return err
		}
		state := engine.Snapshot()
		obligation, err := legacyObligationFor(r.stateDir, state.Presentation.TabID, chatID)
		if err != nil {
			return err
		}
		if err := engine.Apply(chat.MigrateLegacyObligation{Obligation: obligation}); err != nil {
			return err
		}
		if !engine.Snapshot().Migration.LegacyObligationMigrated {
			return fmt.Errorf("chat %q did not durably acknowledge obligation migration", chatID)
		}
	}
	return nil
}

func (r *providerChatRuntime) upgradeLegacyBackground(chatIDs []string) error {
	seen := make(map[string]struct{}, len(chatIDs))
	for _, chatID := range chatIDs {
		chatID = strings.TrimSpace(chatID)
		if chatID == "" {
			return errors.New("previous cutover receipt contains an empty chat id")
		}
		if _, duplicate := seen[chatID]; duplicate {
			return fmt.Errorf("previous cutover receipt contains duplicate chatId %q", chatID)
		}
		seen[chatID] = struct{}{}
		engine, err := chat.NewDurableEngine(chatID, chat.FileStore{Path: providerChatStatePath(r.stateDir, chatID)})
		if err != nil {
			return err
		}
		if err := r.migrateLegacyBackground(engine); err != nil {
			return err
		}
	}
	return nil
}

func (r *providerChatRuntime) migrateLegacyBackground(engine *chat.Engine) error {
	if r == nil || r.manager == nil || engine == nil {
		return errors.New("legacy background migration is unavailable")
	}
	state := engine.Snapshot()
	tabID := strings.TrimSpace(state.Presentation.TabID)
	if tabID == "" || strings.TrimSpace(state.ChatID) == "" {
		return errors.New("legacy background migration requires exact chat attachment identity")
	}
	items := r.manager.ListSpawnedWork(tabID, state.ChatID)
	background := make([]chat.BackgroundState, 0, len(items))
	for _, item := range items {
		workID := firstNonEmptyString(strings.TrimSpace(item.ID), strings.TrimSpace(item.TaskID))
		if workID == "" {
			return errors.New("legacy background snapshot contains an item without identity")
		}
		owner, ok := exactBackgroundOwner(state, item)
		if !ok {
			owner, ok = legacyBackgroundOwner(state, item, workID)
		}
		if !ok {
			return fmt.Errorf("legacy background work %q has ambiguous provider ownership", workID)
		}
		if owner.ConnectionGeneration == 0 {
			owner.ConnectionGeneration = 1
		}
		background = append(background, chat.BackgroundState{Owner: owner, Event: backgroundEvent(item, workID)})
	}
	if err := engine.Apply(chat.MigrateLegacyBackground{Items: background}); err != nil {
		return err
	}
	if !engine.Snapshot().Migration.LegacyBackgroundMigrated {
		return fmt.Errorf("chat %q did not durably acknowledge background migration", state.ChatID)
	}
	return nil
}

// legacyBackgroundOwner is migration-only. Runtime snapshots are forbidden
// from calling it: after the cutover receipt every work row must carry exact
// lane/operation ownership or be rejected.
func legacyBackgroundOwner(state chat.State, item acp.SpawnedWorkItem, workID string) (chat.ProviderActivityOwner, bool) {
	providerID := providercontract.NormalizeID(item.ProviderID)
	var candidate chat.ProviderActivityOwner
	for laneID, lane := range state.Lanes {
		if providerID != "" && lane.Identity.Realm.ProviderID != providerID {
			continue
		}
		if candidate.LaneID != "" {
			return chat.ProviderActivityOwner{}, false
		}
		generation := lane.ConnectionGeneration
		if generation == 0 {
			generation = 1
		}
		candidate = chat.ProviderActivityOwner{
			LaneID: laneID, OperationID: providercontract.OperationID("migrated-background:" + workID),
			TurnID: strings.TrimSpace(item.OriginTurnID), ConnectionGeneration: generation,
		}
	}
	return candidate, candidate.LaneID != ""
}

type legacyObligationRecord struct {
	TabID       string `json:"tabId"`
	ChatID      string `json:"chatId"`
	State       string `json:"state"`
	Source      string `json:"source,omitempty"`
	Note        string `json:"note,omitempty"`
	OpenedAt    string `json:"openedAt"`
	UpdatedAt   string `json:"updatedAt"`
	PromptID    string `json:"promptId,omitempty"`
	ParkedSince string `json:"parkedSince,omitempty"`
}

type legacyObligationSnapshot struct {
	Open []legacyObligationRecord `json:"open"`
}

func legacyObligationFor(stateDir, tabID, chatID string) (*chat.ObligationState, error) {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if tabID == "" || chatID == "" {
		return nil, nil
	}
	path := filepath.Join(stateDir, "obligations", tabID+".json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snapshot legacyObligationSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil, fmt.Errorf("decode legacy obligation snapshot %q: %w", tabID, err)
	}
	var result *chat.ObligationState
	for _, record := range snapshot.Open {
		if strings.TrimSpace(record.TabID) != tabID || strings.TrimSpace(record.ChatID) != chatID {
			continue
		}
		if result != nil {
			return nil, fmt.Errorf("legacy obligation snapshot contains duplicate chatId %q", chatID)
		}
		candidate := &chat.ObligationState{
			State: strings.TrimSpace(record.State), Source: strings.TrimSpace(record.Source),
			Note: redactedSessionString(record.Note), OpenedAt: strings.TrimSpace(record.OpenedAt),
			UpdatedAt: strings.TrimSpace(record.UpdatedAt), PromptID: strings.TrimSpace(record.PromptID),
			ParkedSince: strings.TrimSpace(record.ParkedSince),
		}
		if err := candidate.Validate(); err != nil {
			return nil, fmt.Errorf("legacy obligation for chat %q: %w", chatID, err)
		}
		result = candidate
	}
	return result, nil
}

func removeLegacyObligationStore(stateDir string) error {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		return errors.New("cannot remove legacy obligation store without state directory")
	}
	path := filepath.Join(stateDir, "obligations")
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove migrated legacy obligation store: %w", err)
	}
	return syncDirectory(stateDir)
}

func (r *providerChatRuntime) adoptLegacyProviderBindings(actor *providerChatActor, source map[string]any) error {
	if r == nil || r.manager == nil || actor == nil {
		return errors.New("legacy provider lane migration is unavailable")
	}
	state := actor.engine.Snapshot()
	history := make([]acp.LegacyCoverageMessage, 0, len(state.Ledger))
	for _, event := range state.Ledger {
		content := strings.TrimSpace(event.Text)
		if content == "" {
			continue
		}
		history = append(history, acp.LegacyCoverageMessage{
			ID: event.MessageID, Role: event.Role, Content: content,
		})
	}
	migrations, err := r.manager.LegacyProviderLaneMigrations(state.ChatID, history)
	if err != nil {
		var providerErr *providercontract.Error
		if errors.As(err, &providerErr) && providerErr.Kind != "" {
			return actor.engine.Apply(chat.QuarantineLegacyMigration{Error: providerErr.Kind})
		}
		return err
	}
	selectedIndexes, err := selectedLegacyLaneIndexes(source, migrations)
	if err != nil {
		return actor.engine.Apply(chat.QuarantineLegacyMigration{Error: providercontract.ErrorNativeIdentityConflict})
	}
	for index, migration := range migrations {
		coverage, err := legacyCoverageRecords(state, migration.CoveredMessages)
		if err != nil {
			return err
		}
		if selectedIndexes[index] && migration.BlockedError == "" {
			coverage = selectedLegacyCoverageRecords(state)
		}
		selection := migration.Selection
		if err := actor.engine.Apply(chat.AdoptLaneBinding{
			Identity: selection.Identity, Thread: selection.Thread, Owner: selection.Owner,
			CWD: selection.CWD, ModelID: selection.ModelID, ModeID: selection.ModeID,
			Context: selection.Context, Coverage: coverage,
			Selected: selectedIndexes[index], BlockedError: migration.BlockedError,
		}); err != nil {
			return err
		}
	}
	return nil
}

// selectedLegacyCoverageRecords records the one law the pre-actor product
// already guaranteed: the exact native session selected by an exact session id
// is the context owner of that chat's visible pre-cutover ledger. This is not a
// runtime history match and is never used for a second provider lane.
func selectedLegacyCoverageRecords(state chat.State) []chat.CoverageRecord {
	records := make([]chat.CoverageRecord, 0, len(state.Ledger))
	for _, event := range state.Ledger {
		status := chat.CoverageNativeSeen
		if event.ContextExcluded {
			status = chat.CoverageExcluded
		}
		records = append(records, chat.CoverageRecord{
			Sequence: event.Sequence, EventID: event.EventID, Status: status, DeliveryID: event.OperationID,
		})
	}
	return records
}

func legacyCoverageRecords(state chat.State, coveredMessages int) ([]chat.CoverageRecord, error) {
	if coveredMessages < 0 {
		return nil, errors.New("legacy provider coverage is negative")
	}
	remaining := coveredMessages
	records := make([]chat.CoverageRecord, 0, len(state.Ledger))
	for _, event := range state.Ledger {
		status := chat.CoverageExcluded
		if strings.TrimSpace(event.Text) != "" {
			if remaining == 0 {
				break
			}
			remaining--
			status = chat.CoverageNativeSeen
		}
		records = append(records, chat.CoverageRecord{
			Sequence: event.Sequence, EventID: event.EventID, Status: status, DeliveryID: event.OperationID,
		})
	}
	if remaining != 0 {
		return nil, errors.New("legacy native coverage exceeds the migrated semantic history")
	}
	return records, nil
}

func selectedLegacyLaneIndexes(source map[string]any, migrations []acp.LegacyProviderLaneMigration) (map[int]bool, error) {
	selected := make(map[int]bool)
	if len(migrations) == 0 {
		return selected, nil
	}
	sessionID := strings.TrimSpace(fieldString(source, "sessionId"))
	providerID := providercontract.NormalizeID(firstNonEmptyString(
		fieldString(source, "sessionProviderId"), fieldString(source, "providerId"),
	))
	cwd := strings.TrimSpace(fieldString(source, "cwd"))
	for index, migration := range migrations {
		selection := migration.Selection
		if sessionID != "" {
			if sessionID != selection.Thread.RootID && sessionID != selection.Thread.HeadID {
				continue
			}
			if providerID != "" && providerID != selection.Identity.Realm.ProviderID {
				continue
			}
			selected[index] = true
			continue
		}
		if providerID == "" || providerID != selection.Identity.Realm.ProviderID {
			continue
		}
		if cwd != "" && selection.CWD != "" && !sameLegacyFilesystemPath(cwd, selection.CWD) {
			continue
		}
		selected[index] = true
	}
	if len(selected) > 1 {
		return nil, errors.New("legacy session metadata selects multiple immutable provider lanes")
	}
	if sessionID != "" && len(selected) == 0 {
		return nil, errors.New("legacy session id does not match any durable provider lane")
	}
	return selected, nil
}

func sameLegacyFilesystemPath(left, right string) bool {
	canonical := func(value string) string {
		value = filepath.Clean(strings.TrimSpace(value))
		if absolute, err := filepath.Abs(value); err == nil {
			value = absolute
		}
		if resolved, err := filepath.EvalSymlinks(value); err == nil {
			value = resolved
		}
		return value
	}
	return canonical(left) == canonical(right)
}

func readLegacyChatCutoverReceipt(path string) (legacyChatCutoverReceipt, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return legacyChatCutoverReceipt{}, false, nil
	}
	if err != nil {
		return legacyChatCutoverReceipt{}, false, err
	}
	var receipt legacyChatCutoverReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return legacyChatCutoverReceipt{}, false, fmt.Errorf("decode provider chat cutover receipt: %w", err)
	}
	return receipt, true, nil
}

func writeLegacyChatCutoverReceipt(path string, receipt legacyChatCutoverReceipt) error {
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".provider-chat-cutover-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	removeTemp := true
	defer func() {
		_ = tmp.Close()
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	removeTemp = false
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (r *providerChatRuntime) migrateLegacyChatFromSource(chatID string, engine *chat.Engine, source map[string]any) error {
	if r == nil || r.sessions == nil || engine == nil {
		return errors.New("legacy chat migration is unavailable")
	}
	// The eager cutover always supplies the exact row it is migrating. Looking
	// it up again here would be a hidden lazy fallback after the global receipt.
	if source == nil {
		return errors.New("legacy chat migration requires an explicit source row")
	}
	if strings.TrimSpace(fieldString(source, "chatId")) != strings.TrimSpace(chatID) {
		return errors.New("legacy migration source belongs to another chat")
	}
	command, err := buildLegacyChatMigration(source, filepath.Dir(r.sessions.path), r.sessions)
	if err != nil {
		return err
	}
	if err := engine.Apply(command); err != nil {
		return fmt.Errorf("migrate legacy chat %q: %w", chatID, err)
	}
	obligation, err := legacyObligationFor(filepath.Dir(r.sessions.path), command.Presentation.TabID, chatID)
	if err != nil {
		return fmt.Errorf("read legacy obligation for chat %q: %w", chatID, err)
	}
	if err := engine.Apply(chat.MigrateLegacyObligation{Obligation: obligation}); err != nil {
		return fmt.Errorf("migrate legacy obligation for chat %q: %w", chatID, err)
	}
	readback := engine.Snapshot()
	if !readback.Initialized || !readback.Migration.Complete || !readback.Migration.LegacyObligationMigrated || readback.Migration.Version != command.Version || readback.Migration.Digest != command.Digest {
		return errors.New("legacy chat migration failed durable readback verification")
	}
	return nil
}

func buildLegacyChatMigration(source map[string]any, stateDir string, sessions *sessionStore) (chat.MigrateLegacyChat, error) {
	chatID := strings.TrimSpace(fieldString(source, "chatId"))
	tabID := strings.TrimSpace(fieldString(source, "id"))
	if chatID == "" || tabID == "" {
		return chat.MigrateLegacyChat{}, errors.New("legacy chat is missing immutable chat or tab identity")
	}
	presentation, err := presentationFromLegacyChat(source)
	if err != nil {
		return chat.MigrateLegacyChat{}, err
	}

	messageSources, err := completeLegacyMessages(source, stateDir)
	if err != nil {
		return chat.MigrateLegacyChat{}, err
	}
	messages := make([]chat.LegacyMessage, 0, len(messageSources))
	var currentOperation providercontract.OperationID
	for index, raw := range messageSources {
		message := mapFromAnyMain(cloneJSON(raw))
		messageID := strings.TrimSpace(fieldString(message, "id"))
		role := strings.ToLower(strings.TrimSpace(fieldString(message, "role")))
		if messageID == "" || (role != "user" && role != "assistant") {
			return chat.MigrateLegacyChat{}, fmt.Errorf("legacy message %d has no stable id or supported role", index)
		}
		if role == "user" {
			currentOperation = providercontract.NormalizeOperationID(messageID)
		}
		if currentOperation == "" {
			currentOperation = providercontract.OperationID("legacy-operation:" + messageID)
		}
		attachments, attachErr := sessions.PersistProviderAttachments(anySlice(message["images"]))
		if attachErr != nil {
			return chat.MigrateLegacyChat{}, fmt.Errorf("persist legacy message %q attachments: %w", messageID, attachErr)
		}
		// The actor stores only content-addressed image references. This walk also
		// covers tool-result media nested below the message event list.
		if err := makeSessionValueRefNative(message, stateDir); err != nil {
			return chat.MigrateLegacyChat{}, fmt.Errorf("externalize legacy message %q media: %w", messageID, err)
		}
		timeline, err := legacyTimeline(messageID, anySlice(message["events"]), sessions)
		if err != nil {
			return chat.MigrateLegacyChat{}, err
		}
		permission, err := legacyPermission(message["permission"])
		if err != nil {
			return chat.MigrateLegacyChat{}, fmt.Errorf("legacy message %q permission: %w", messageID, err)
		}
		if err := requireOnlyKeys(message, legacyMessageKeys, "legacy message "+messageID); err != nil {
			return chat.MigrateLegacyChat{}, err
		}
		status := strings.ToLower(strings.TrimSpace(fieldString(message, "status")))
		if status == "" {
			status = "done"
		}
		terminal := ""
		if role == "assistant" && status != "pending" && status != "running" {
			terminal = status
		}
		legacy := chat.LegacyMessage{
			MessageID: messageID, OperationID: currentOperation, Role: role,
			Text: fieldString(message, "content"), Result: fieldString(message, "result"),
			Status: status, At: fieldString(message, "at"), Attachments: attachments,
			TerminalState: terminal, NativeTurnID: fieldString(message, "jobId"), QueueID: fieldString(message, agentQueueMessageField),
			SteerState: fieldString(message, "steerState"), SteerBoundary: fieldString(message, "steerBoundary"),
			SteerContinuationID: fieldString(message, "steerContinuationId"), SteerContinuationFor: fieldString(message, "steerContinuationFor"),
			TurnRootID: fieldString(message, "turnRootId"), TurnStartedAt: int64(intValue(message["turnStartedAt"])),
			Interrupted: boolFieldValue(message, "interrupted"), RetryPrompt: fieldString(message, "retryPrompt"),
			Timeline: timeline, Permission: permission,
		}
		if value, present := boolField(message, "turnTerminal"); present {
			legacy.TurnTerminal = &value
		}
		if message["steerAnchor"] != nil {
			rawAnchor := mapFromAnyMain(message["steerAnchor"])
			if err := requireOnlyKeys(rawAnchor, map[string]struct{}{"assistantMessageId": {}, "contentOffset": {}, "resultOffset": {}, "eventCount": {}}, "legacy steer anchor"); err != nil {
				return chat.MigrateLegacyChat{}, err
			}
			legacy.SteerAnchor = &chat.SteerAnchor{
				AssistantMessageID: fieldString(rawAnchor, "assistantMessageId"), ContentOffset: intValue(rawAnchor["contentOffset"]),
				ResultOffset: intValue(rawAnchor["resultOffset"]), EventCount: intValue(rawAnchor["eventCount"]),
			}
		}
		messages = append(messages, legacy)
	}
	stagedQueue, err := stagedQueueFromLegacyChat(source, sessions)
	if err != nil {
		return chat.MigrateLegacyChat{}, err
	}
	command := chat.MigrateLegacyChat{
		Version: legacyChatMigrationVersion, Presentation: presentation, Messages: messages, StagedQueue: stagedQueue,
	}
	digestSource := struct {
		Version      uint32
		Presentation chat.PresentationState
		Messages     []chat.LegacyMessage
		StagedQueue  []chat.StagedQueueEntry
	}{command.Version, command.Presentation, command.Messages, command.StagedQueue}
	raw, err := json.Marshal(digestSource)
	if err != nil {
		return chat.MigrateLegacyChat{}, err
	}
	digest := sha256.Sum256(raw)
	command.Digest = hex.EncodeToString(digest[:])
	return command, nil
}

var legacyMessageKeys = map[string]struct{}{
	"id": {}, "role": {}, "content": {}, "result": {}, "status": {}, "at": {}, "events": {}, "images": {},
	"steerState": {}, "steerAnchor": {}, "steerBoundary": {}, "steerContinuationId": {}, "steerContinuationFor": {},
	"turnRootId": {}, "turnTerminal": {}, "permission": {}, "jobId": {}, "turnStartedAt": {}, "interrupted": {},
	"retryPrompt": {}, agentQueueMessageField: {},
}

func requireOnlyKeys(value map[string]any, allowed map[string]struct{}, label string) error {
	unknown := make([]string, 0)
	for key := range value {
		if _, ok := allowed[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("%s contains unsupported fields %q", label, unknown)
}

func legacyTimeline(messageID string, sources []any, sessions *sessionStore) ([]chat.TimelineEntry, error) {
	entries := make([]chat.TimelineEntry, 0, len(sources))
	for index, raw := range sources {
		source := mapFromAnyMain(raw)
		kind := strings.ToLower(strings.TrimSpace(fieldString(source, "kind")))
		key := strings.TrimSpace(fieldString(source, "key"))
		if key == "" {
			key = fmt.Sprintf("legacy-timeline:%s:%d", messageID, index+1)
		}
		entry := chat.TimelineEntry{Key: key, At: max(0, intValue(source["at"]))}
		switch kind {
		case "thinking":
			if err := requireOnlyKeys(source, map[string]struct{}{"key": {}, "at": {}, "kind": {}, "text": {}}, "legacy thinking event"); err != nil {
				return nil, err
			}
			entry.Kind = providercontract.EventThinkingUpdate
			entry.Thinking = &providercontract.ThinkingEvent{Text: fieldString(source, "text")}
		case "plan":
			if err := requireOnlyKeys(source, map[string]struct{}{"key": {}, "at": {}, "kind": {}, "entries": {}}, "legacy plan event"); err != nil {
				return nil, err
			}
			plan := &providercontract.PlanEvent{}
			for planIndex, rawPlan := range anySlice(source["entries"]) {
				item := mapFromAnyMain(rawPlan)
				if err := requireOnlyKeys(item, map[string]struct{}{"status": {}, "content": {}, "id": {}}, "legacy plan entry"); err != nil {
					return nil, err
				}
				id := fieldString(item, "id")
				if id == "" {
					id = fmt.Sprintf("legacy-plan:%s:%d:%d", messageID, index+1, planIndex+1)
				}
				plan.Entries = append(plan.Entries, providercontract.PlanEntry{ID: id, Status: fieldString(item, "status"), Text: fieldString(item, "content")})
			}
			entry.Kind, entry.Plan = providercontract.EventPlanUpdate, plan
		case "tool":
			allowed := map[string]struct{}{
				"key": {}, "at": {}, "kind": {}, "id": {}, "toolKind": {}, "title": {}, "status": {}, "command": {},
				"terminalId": {}, "input": {}, "output": {}, "location": {}, "images": {}, "startedAt": {}, "endedAt": {},
				"subagentId": {}, "subagentLabel": {}, "subagentProvider": {}, "subagentModel": {}, "subagentHeader": {},
			}
			if err := requireOnlyKeys(source, allowed, "legacy tool event"); err != nil {
				return nil, err
			}
			attachments, err := sessions.PersistProviderAttachments(anySlice(source["images"]))
			if err != nil {
				return nil, fmt.Errorf("persist legacy tool %q attachments: %w", key, err)
			}
			toolID := fieldString(source, "id")
			if toolID == "" {
				toolID = key
			}
			entry.Kind = providercontract.EventToolUpdate
			entry.Tool = &providercontract.ToolEvent{
				ToolCallID: toolID, ToolKind: fieldString(source, "toolKind"), Title: fieldString(source, "title"), Status: fieldString(source, "status"),
				Command: fieldString(source, "command"), TerminalID: fieldString(source, "terminalId"), Input: fieldString(source, "input"),
				Output: fieldString(source, "output"), Location: fieldString(source, "location"), Attachments: attachments,
				SubagentID: fieldString(source, "subagentId"), SubagentLabel: fieldString(source, "subagentLabel"),
				SubagentProvider: fieldString(source, "subagentProvider"), SubagentModel: fieldString(source, "subagentModel"),
				SubagentHeader: boolFieldValue(source, "subagentHeader"), StartedAtUnixMS: int64(intValue(source["startedAt"])), EndedAtUnixMS: int64(intValue(source["endedAt"])),
			}
		case "compaction":
			if err := requireOnlyKeys(source, map[string]struct{}{"key": {}, "at": {}, "kind": {}}, "legacy compaction event"); err != nil {
				return nil, err
			}
			entry.Kind = providercontract.EventCompactionCheckpoint
			entry.Compaction = &providercontract.CompactionEvent{}
		case "restored":
			if err := requireOnlyKeys(source, map[string]struct{}{"key": {}, "at": {}, "kind": {}, "turnSeq": {}}, "legacy checkpoint restore event"); err != nil {
				return nil, err
			}
			turnSequence := intValue(source["turnSeq"])
			if turnSequence <= 0 {
				return nil, errors.New("legacy checkpoint restore has no positive turn sequence")
			}
			entry.Kind = providercontract.EventCheckpointRestored
			entry.Restored = &providercontract.CheckpointRestoredEvent{TurnSequence: turnSequence}
		case "bgproc":
			// Background-process rows are explicitly transient renderer state and
			// therefore are not imported into the semantic actor ledger.
			continue
		default:
			return nil, fmt.Errorf("legacy timeline event %q has unsupported kind %q", key, kind)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func legacyPermission(raw any) (*providercontract.PermissionEvent, error) {
	if raw == nil {
		return nil, nil
	}
	source := mapFromAnyMain(raw)
	if source == nil {
		return nil, errors.New("permission is not an object")
	}
	if err := requireOnlyKeys(source, map[string]struct{}{"id": {}, "title": {}, "kind": {}, "options": {}, "resolved": {}, "question": {}}, "legacy permission"); err != nil {
		return nil, err
	}
	event := &providercontract.PermissionEvent{
		RequestID: fieldString(source, "id"), Title: fieldString(source, "title"), Kind: fieldString(source, "kind"), Status: "pending",
		ResolvedOptionID: fieldString(source, "resolved"),
	}
	if event.RequestID == "" {
		return nil, errors.New("permission has no stable request id")
	}
	if event.ResolvedOptionID != "" {
		event.Status = "resolved"
	}
	for _, rawOption := range anySlice(source["options"]) {
		option := mapFromAnyMain(rawOption)
		if err := requireOnlyKeys(option, map[string]struct{}{"optionId": {}, "name": {}, "kind": {}}, "legacy permission option"); err != nil {
			return nil, err
		}
		detail := providercontract.PermissionOption{ID: fieldString(option, "optionId"), Name: fieldString(option, "name"), Kind: fieldString(option, "kind")}
		if detail.ID == "" {
			return nil, errors.New("permission option has no stable id")
		}
		event.Options = append(event.Options, detail.ID)
		event.OptionDetails = append(event.OptionDetails, detail)
	}
	if rawQuestion := source["question"]; rawQuestion != nil {
		question := mapFromAnyMain(rawQuestion)
		if err := requireOnlyKeys(question, map[string]struct{}{"question": {}, "header": {}, "options": {}, "multiSelect": {}}, "legacy permission question"); err != nil {
			return nil, err
		}
		event.Question = &providercontract.PermissionQuestion{Question: fieldString(question, "question"), Header: fieldString(question, "header"), MultiSelect: boolFieldValue(question, "multiSelect")}
		for _, rawOption := range anySlice(question["options"]) {
			option := mapFromAnyMain(rawOption)
			if err := requireOnlyKeys(option, map[string]struct{}{"label": {}, "description": {}}, "legacy permission question option"); err != nil {
				return nil, err
			}
			event.Question.Options = append(event.Question.Options, providercontract.PermissionQuestionOption{Label: fieldString(option, "label"), Description: fieldString(option, "description")})
		}
	}
	return event, nil
}

func presentationFromLegacyChat(source map[string]any) (chat.PresentationState, error) {
	tabID := strings.TrimSpace(fieldString(source, "id"))
	if tabID == "" {
		return chat.PresentationState{}, errors.New("chat presentation is missing tab identity")
	}
	presentation := chat.PresentationState{
		TabID:                  tabID,
		Title:                  fieldString(source, "title"),
		TitleLocked:            boolFieldValue(source, "titleLocked"),
		Group:                  optionalStringPointer(source, "group"),
		CWD:                    optionalStringPointer(source, "cwd"),
		Draft:                  stringValue(source["draft"]),
		Unread:                 boolFieldValue(source, "unread"),
		Settled:                fieldString(source, "settled"),
		Pane:                   optionalStringPointer(source, "pane"),
		ProviderID:             providercontract.NormalizeID(fieldString(source, "providerId")),
		CurrentModelID:         hydratableStoredModelID(fieldString(source, "currentModelId")),
		CurrentModeID:          fieldString(source, "currentModeId"),
		WorkspaceRevision:      uint64(max(0, intValue(source[workspaceRevisionField]))),
		PresentationRevision:   uint64(max(0, intValue(source[presentationRevisionField]))),
		AgentQueueRevision:     uint64(max(0, intValue(source[agentQueueRevisionField]))),
		RuntimeControlRevision: uint64(max(0, intValue(source[runtimeControlRevisionField]))),
		PlanLatestMessageID:    fieldString(source, "planLatestMessageId"),
	}
	var err error
	if presentation.ModelControls, err = rawJSONValue(source["modelControls"]); err != nil {
		return chat.PresentationState{}, err
	}
	if presentation.ContextUsageByProvider, err = rawJSONValue(source["contextUsageByProvider"]); err != nil {
		return chat.PresentationState{}, err
	}
	if presentation.LegacyUsage, err = rawJSONValue(source["usage"]); err != nil {
		return chat.PresentationState{}, err
	}
	for index, raw := range anySlice(source["planLatest"]) {
		entry := mapFromAnyMain(raw)
		presentation.PlanLatest = append(presentation.PlanLatest, providercontract.PlanEntry{
			ID: fmt.Sprintf("legacy-plan:%d", index), Text: fieldString(entry, "content"), Status: fieldString(entry, "status"),
		})
	}
	if err := presentation.Validate(); err != nil {
		return chat.PresentationState{}, err
	}
	return presentation, nil
}

func stagedQueueFromLegacyChat(source map[string]any, sessions *sessionStore) ([]chat.StagedQueueEntry, error) {
	presentation, err := presentationFromLegacyChat(source)
	if err != nil {
		return nil, err
	}
	stagedQueue := make([]chat.StagedQueueEntry, 0, len(anySlice(source["queue"])))
	for index, raw := range anySlice(source["queue"]) {
		item := mapFromAnyMain(raw)
		id, textValue := fieldString(item, "id"), fieldString(item, "text")
		attachments, attachErr := sessions.PersistProviderAttachments(anySlice(item["images"]))
		if attachErr != nil {
			return nil, fmt.Errorf("persist legacy queue entry %d attachments: %w", index, attachErr)
		}
		if id == "" || strings.TrimSpace(textValue) == "" && len(attachments) == 0 {
			return nil, fmt.Errorf("legacy queue entry %d has no stable id or content", index)
		}
		names := make([]string, 0, len(anySlice(item["attachmentNames"])))
		for _, name := range anySlice(item["attachmentNames"]) {
			if value := strings.TrimSpace(stringValue(name)); value != "" {
				names = append(names, value)
			}
		}
		stagedQueue = append(stagedQueue, chat.StagedQueueEntry{
			ID: id, Text: textValue, Source: fieldString(item, "source"), Delivery: fieldString(item, "delivery"),
			QueuedAt: fieldString(item, "queuedAt"), Attachments: attachments, AttachmentNames: names,
			AttachmentState: fieldString(item, "attachmentState"), AttachmentError: fieldString(item, "attachmentError"),
			TargetProviderID: presentation.ProviderID, ModelID: presentation.CurrentModelID, ModeID: presentation.CurrentModeID,
		})
	}
	return stagedQueue, nil
}

// The inline mirror is only a retained window; the append-only archive holds
// older completed rows. Migration combines both by stable row id, never by
// content similarity. A truly pre-id row is ambiguous (two identical repeated
// messages are legal), so it quarantines instead of receiving a guessed id.
func completeLegacyMessages(source map[string]any, stateDir string) ([]any, error) {
	tabID := fieldString(source, "id")
	combined := make([]any, 0)
	position := make(map[string]int)
	appendSource := func(items []any, sourceName string) error {
		for index, raw := range items {
			message := mapFromAnyMain(cloneJSON(raw))
			id := strings.TrimSpace(fieldString(message, "id"))
			if id == "" {
				return fmt.Errorf("legacy %s message %d has no stable id", sourceName, index)
			}
			if existingIndex, duplicate := position[id]; duplicate {
				existing := mapFromAnyMain(combined[existingIndex])
				for _, key := range []string{"role", "content", "result"} {
					left, right := fieldString(existing, key), fieldString(message, key)
					if left != "" && right != "" && left != right {
						return fmt.Errorf("legacy message %q conflicts between archive and mirror", id)
					}
				}
				// The inline mirror is newer and may contain richer terminal/event
				// presentation for the same stable semantic row.
				if sourceName == "mirror" {
					combined[existingIndex] = message
				}
				continue
			}
			position[id] = len(combined)
			combined = append(combined, message)
		}
		return nil
	}
	if err := appendSource(loadChatArchive(stateDir, tabID), "archive"); err != nil {
		return nil, err
	}
	if err := appendSource(messageSlice(source), "mirror"); err != nil {
		return nil, err
	}
	return combined, nil
}

func optionalStringPointer(source map[string]any, key string) *string {
	value, exists := source[key]
	if !exists || value == nil {
		return nil
	}
	text := stringValue(value)
	return &text
}

func rawJSONValue(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return raw, nil
}
